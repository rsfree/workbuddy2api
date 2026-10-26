// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/bridge"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/tokencalib"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权（静态值；与 Live 同时给出时 Live 优先）
	MaxRotate int    // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// Panel 管理面板 handler（可选；nil = 不挂载）。挂载在 /panel/ 前缀下，
	// 面板自带 Bearer 鉴权（同一 api_key）与内嵌静态资源，主路由只做转发。
	Panel http.Handler

	// Live 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏开关时立即生效）。
	// nil 时回退静态字段（测试与裸用场景）。
	Live *livecfg.Holder

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// ModelsHidden 模型黑名单原始条目（config models.hidden；Live 为 nil 的
	// 裸用/测试路径经 loadLive() 归一化后使用）。
	ModelsHidden []string

	// ModelsExtra 静态补录的模型条目（config models.extra）。只影响 /v1/models
	// 的呈现，不参与可用性判断：上游没有路由时点名调用照样被上游拒绝。
	// 与 ModelsHidden 同源处理（Live 为 nil 时经 loadLive() 归一化）。
	ModelsExtra []livecfg.ExtraModel

	// TokenCalib /v1/messages/count_tokens 的输入 token 校准器（可选；nil =
	// 纯字符粗估）。协议前门每次对话回来都会用真实 usage.prompt_tokens 在线
	// 更新它，count_tokens 再拿学到的系数换算（见 internal/tokencalib）。
	TokenCalib *tokencalib.Calibrator

	// Baseline 会话基线缓存：记住「某批消息在上游的真实 prompt_tokens」，
	// count_tokens 用它做前缀匹配，回答「上一轮真数 + 本轮新增消息的估算」。
	// 可选（nil = 只用系数换算）。两者是互补关系，见 tokencalib/baseline.go。
	Baseline *tokencalib.Baseline

	// Usage 逐请求用量记录器（可选；nil = 不记录）。
	// 在 recordAttempt 这一唯一汇聚点调用，因此流式/非流式、成功/失败都会计入，
	// 且与 pool 的每账号累计器同源，两条口径不会漂移。
	Usage *usage.Recorder

	// RequestLog 请求指标与脱敏 JSONL 归档（可选；nil = 不记录）。
	RequestLog *reqlog.Recorder

	// RecordClientInfo 是否在请求日志里记录调用来源（客户端 IP / User-Agent）。
	// 来自 logging.request_client_info（缺省 true）；关闭时 reqlog 事件的来源字段
	// 保持为空，归档与面板都不出现来源信息。
	RecordClientInfo bool

	// Images 图像接口配置。nil = 用内置默认（启用；alpha / alpha-edit；420s；4 张）。
	// 为什么用指针而不是零值：false/0 是合法配置（关闭、不限），需要区分
	// 「没配置」与「显式关掉」。
	Images *ImagesConfig
}

// ImagesConfig /v1/images/* 的运行参数（来自 config images 段）。
type ImagesConfig struct {
	// Enabled 是否挂载 /v1/images/generations 与 /v1/images/edits。
	Enabled bool
	// DefaultModel 文生图默认模型（请求未写 model 时使用）。
	DefaultModel string
	// EditModel 图生图默认模型。
	EditModel string
	// FallbackModel 请求写了未知模型时的兜底（空 = 不兜底，交给上游判定）。
	FallbackModel string
	// Deadline 单次生图请求的总时限（跨账号轮转 + 重试）。
	Deadline time.Duration
	// MaxRefImages 单次请求的参考图上限（超出截尾）。
	MaxRefImages int
	// FetchSourceIP 下载 URL 参考图时绑定的本机源 IP（空 = 走默认路由）。
	// 多公网 IP 主机上，默认路由 IP 可能被图片 CDN（如腾讯云 COS）单独拉黑：
	// 此时指定一个可达的源 IP 即可，不影响生图请求本身的账号出口绑定。
	FetchSourceIP string
	// ListInModels 是否把图像模型附到 /v1/models 末尾（带 type/endpoint 字段）。
	ListInModels bool
}

// imagesCfg 返回归一化后的图像配置。
func (h *Handler) imagesCfg() ImagesConfig {
	var c ImagesConfig
	if h.cfg.Images != nil {
		c = *h.cfg.Images
	} else {
		// 未注入配置（测试/裸用）时的默认：启用图像接口，并把媒体模型列进
		// /v1/models（动态目录由 upstream.FetchModels 填充）。
		c.Enabled = true
		c.ListInModels = true
	}
	if c.DefaultModel == "" {
		c.DefaultModel = defaultImageModel
	}
	if c.EditModel == "" {
		c.EditModel = defaultImageEditModel
	}
	if c.Deadline <= 0 {
		c.Deadline = 420 * time.Second
	}
	if c.MaxRefImages <= 0 {
		c.MaxRefImages = 4
	}
	return c
}

// loadLive 返回当前运行期快照；Live 为 nil 时用静态字段合成。
func (h *Handler) loadLive() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey:           h.cfg.APIKey,
		SoftCooldown:     h.cfg.SoftCooldown,
		RecordClientInfo: h.cfg.RecordClientInfo,
		HiddenModels:     livecfg.NormalizeHiddenModels(h.cfg.ModelsHidden),
		ExtraModels:      livecfg.NormalizeExtraModels(h.cfg.ModelsExtra),
	}
}

// softCooldown 返回当前生效的软冷却基数（热改优先，<=0 回退默认）。
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return 600 * time.Second
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// wafIP WAF IP 级拦截状态机（fail-fast，wafip.go）：短窗多号 WAF 403 →
	// 激活期轮转遇 WAF 403 直接终止（不放大请求量）。进程内状态、重启清零。
	wafIP wafIPGate

	// videoMu/videoTasks 视频任务 → 出票账号的映射。上游按账号隔离任务，
	// 查询必须用提交时的同一个号；进程内状态，重启即失效（不落盘凭据）。
	videoMu    sync.Mutex
	videoTasks map[string]*videoTaskEntry
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	// 把基线的「新增段配对」接到系数校准器上：count_tokens 每次给出预测都会登记
	// （base + tail 粗估），下一次同一批 messages 的真实 usage 回来时，相减得到的
	// 真实增量就喂给 tail 比例。接在 NewHandler 而不是 main：任何构造路径
	//（含测试、面板内嵌启动）都不该漏这一步，漏了 tail 系数永远学不到。
	if cfg.Baseline != nil && cfg.TokenCalib != nil {
		cfg.Baseline.SetTailObserver(cfg.TokenCalib.ObserveTail)
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	// 另外两种对话协议的前门（见 proto_chat.go / bridge.go）：入口把请求翻成
	// chat 形态，出口把 chat 的输出翻回目标协议，中间复用同一条选号/轮转/记账
	// 链路。Claude Code 只说 /v1/messages，Codex 只说 /v1/responses。
	h.mux.HandleFunc("POST /v1/messages", h.withAuthProto(protoAnthropic, h.messagesChat))
	// count_tokens 不是模型调用（本地估算），但 Claude Code 会在压缩上下文前探它，
	// 缺这个路由会让客户端走 404 异常分支。见 proto_chat.go。
	h.mux.HandleFunc("POST /v1/messages/count_tokens", h.withAuthProto(protoAnthropic, h.countTokens))
	h.mux.HandleFunc("POST /v1/responses", h.withAuthProto(protoResponses, h.responsesChat))
	// 图像生成（OpenAI images 兼容形态，见 images.go）。两条路由共用同一条
	// 选号/租约/冷却/记账管道；edits 额外接受 multipart 上传。
	if h.imagesCfg().Enabled {
		h.mux.HandleFunc("POST /v1/images/generations", h.withAuth(h.imageGenerations))
		h.mux.HandleFunc("POST /v1/images/edits", h.withAuth(h.imageEdits))
	}
	// 联网搜索（Anthropic Messages 兼容形态，见 messages.go）。路径不占用
	// /v1/messages——那是 Anthropic 模型对话的标准路径，已由上面的对话前门承接；
	// 搜索挂在 /v1/search/messages，客户端把搜索 baseURL 指向 <网关>/v1/search。
	h.mux.HandleFunc("POST /v1/search/messages", h.withAuth(h.messages))
	// 网页正文抓取（JSON，见 fetch.go）：搜索给链接、抓取给全文，两者互补。
	h.mux.HandleFunc("POST /v1/fetch", h.withAuth(h.webFetch))
	// 视频生成（异步任务，见 videos.go）：提交拿 task_id，再用 GET 轮询成片。
	h.mux.HandleFunc("POST /v1/videos/generations", h.withAuth(h.videoGenerations))
	h.mux.HandleFunc("GET /v1/videos/{id}", h.withAuth(h.videoTask))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RequestLog != nil && h.tracedRequest(r) {
		trace := &requestTrace{id: reqlog.NewRequestID(), start: time.Now(), path: r.URL.Path}
		if isSearchPath(r.URL.Path) {
			// 搜索不是模型调用，但面板「模型」列空着无法区分这类流量；
			// 用产品名标注（与 chat 的 model 字段并列展示）。
			trace.model = searchToolLabel
		}
		if isFetchPath(r.URL.Path) {
			trace.model = fetchToolLabel
		}
		if isVideoPath(r.URL.Path) {
			trace.model = videoToolLabel // 提交成功后会被真实模型名覆盖
		}
		if label := protoModelLabel(r.URL.Path); label != "" {
			// count_tokens 没有模型调用，面板「模型」列空着看不出是什么流量。
			trace.model = label
		}
		if h.loadLive().RecordClientInfo {
			trace.captureClientInfo(r)
		}
		r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, trace))
		obs := &responseObserver{ResponseWriter: w}
		w.Header().Set("X-Request-Id", trace.id)
		h.cfg.RequestLog.Begin()
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			h.cfg.RequestLog.Record(trace.event(status))
		}()
		h.mux.ServeHTTP(obs, r)
		return
	}
	h.mux.ServeHTTP(w, r)
}

// tracedRequest 判断该请求是否需要请求日志记账（指标 + JSONL 归档）。
//
// 搜索与 chat 共用同一份账本：搜索同样消耗账号额度、同样会因上游/账号问题失败，
// 不记账等于在面板「请求记录」里完全看不见搜索流量（此前正是如此——只挂
// /v1/chat/completions 一个硬编码路径）。
func (h *Handler) tracedRequest(r *http.Request) bool {
	if r.Method == http.MethodGet && isVideoPollPath(r.URL.Path) {
		return true // 轮询也是真实上游调用，面板要能看见任务查询
	}
	if r.Method != http.MethodPost {
		return false
	}
	return r.URL.Path == "/v1/chat/completions" || isSearchPath(r.URL.Path) ||
		isFetchPath(r.URL.Path) || isImagePath(r.URL.Path) || isVideoPath(r.URL.Path) ||
		isProtoPath(r.URL.Path)
}

// isSearchPath 判断是否为搜索端点。
func isSearchPath(path string) bool {
	return path == "/v1/search/messages"
}

// isFetchPath 判断是否为网页抓取端点。
func isFetchPath(path string) bool {
	return path == "/v1/fetch"
}

// isVideoPath 判断是否为视频端点（提交或轮询）。
func isVideoPath(path string) bool {
	return path == "/v1/videos/generations" || isVideoPollPath(path)
}

// isVideoPollPath 判断是否为视频任务轮询（/v1/videos/{id}）。
func isVideoPollPath(path string) bool {
	return strings.HasPrefix(path, "/v1/videos/") && path != "/v1/videos/generations"
}

// isImagePath 判断是否为图像端点。与 chat 一样进请求日志/指标账本：
// 生图同样消耗账号额度、同样会因上游/账号问题失败，不记账面板就完全看不见。
func isImagePath(path string) bool {
	return path == "/v1/images/generations" || path == "/v1/images/edits"
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	// cost_explore 探索台账（issue #136 §5 可观测性）：累计探索事件数 + 各
	// (域, 模型) 的最近探索时刻（键 "realm|model"）。与 accounts[].model_costs
	// 行对照即可读出「探索→毕业」全链路（单一事实来源，不做双表示）。零回归只增键。
	exploreEvents, exploreLast := h.cfg.Pool.CostExploreStatus()
	out := map[string]any{
		"accounts":       h.cfg.Pool.List(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
		// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		// model_locks 当前有未过期模型级限流的 (域, 模型) 全清单：账号池视图回答
		// 「哪些号不能用」，本键回答「哪些模型不能用、锁了几个号、还要锁多久」。
		// 与 ModelBlocked（请求失败时的单模型判定）互补；无锁时为 null。零回归只增键。
		"model_locks": h.cfg.Pool.ModelLockView(),
		// credit_floor 生效的积分保底值（0 = 关闭）。与 accounts[].credits +
		// model_costs 对照即可判定「某号为何对某模型不出票」。零值也显式写出
		// （运维口径：缺失会让人误以为没记录）。
		"credit_floor": h.cfg.Pool.CreditFloor(),
		// cost_explore 事件与 per-model 时间戳（时间值由 encoding/json 写 RFC3339）。
		"cost_explore": map[string]any{
			"events_total": exploreEvents,
			"per_model":    exploreLast,
		},
	}
	// token_calib 协议前门的输入 token 校准参数（按模型）：slope/intercept 就是
	// count_tokens 实际用的换算系数，配合 samples 判断「学到位了没」——
	// Claude Code 压缩时机偏早/偏晚时，先看这里的斜率是不是明显偏离 1。
	if h.cfg.TokenCalib != nil {
		out["token_calib"] = h.cfg.TokenCalib.Snapshot()
	}
	// token_baseline 会话基线的命中情况。hits 恒为 0 说明客户端每轮都在改
	// system/messages（前缀永远匹配不上），此时基线形同虚设、只剩系数换算。
	if h.cfg.Baseline != nil {
		out["token_baseline"] = h.cfg.Baseline.Snapshot()
	}
	writeJSON(w, http.StatusOK, out)
}

// countsMapFrom 把 CountsDetailed 五元组打包成 /status 的域分组建模。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	// dynamicModelsTTL 模型目录缓存时长。曾是 1h；缩到 10min 对齐「面板实时、
	// API 缓存」的漂移痛点（PR #38 报告）：目录新增模型时面板立即可见，公开
	// /v1/models 最多滞后一个 TTL。再短就不值得——每次失效都是 2 次上游探测。
	dynamicModelsTTL        = 10 * time.Minute
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：纯动态（缓存 10min），失败/无号返回空列表（无静态兜底——
// 拉不出目录即意味着上游不可用，假名单只会让客户端选到 11102 的模型）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	// 手动强制刷新：默认 10min TTL 内走缓存，?refresh=1 立即打上游重新探测
	//（chat + 媒体目录同一次刷新）。上游上新模型后不必等 TTL。
	if modelsRefreshRequested(r) {
		invalidateDynamicModelsCache()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelsRefreshRequested 解析 /v1/models?refresh=1|true|yes。
func modelsRefreshRequested(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("refresh"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// invalidateDynamicModelsCache 让下一次 fetchDynamicModels 重新探测上游。
// 只清 fetched/lastFail，不清 ids：探测失败时旧列表仍可继续服务。
func invalidateDynamicModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// fmtCreditsPrefix 从上游 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
// 上游格式不统一："x0.05 credits" / "x0.29" / "x0.00 credits" 等，
// 统一提取 x数字 部分，去 "credits" 后缀。
func fmtCreditsPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields 把上游模型对象全字段（ModelInfo）按「空值省略」写出规则
// 合入 /v1/models 条目：name/description/credits/tags/vendor/能力旗标/
// max_allowed_size/reasoning_effort/reasoning_summary。CN 动态分支与 global
// 探测命中分支共用（两域模型对象同构），保证输出字段集一致。
// 不覆盖 id/object/created/owned_by 及调用方先前写好的基础字段；上游未下发的
// 字段（零值）整体省略——不编造。
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		// 积分倍率前缀：从 "x0.05 credits" / "x0.29" 等格式提取纯数字，
		// 统一为 "[x0.05 credit]" 前缀拼入 description，方便下游面板直接展示。
		if mi.Credits != "" {
			entry["description"] = fmtCreditsPrefix(mi.Credits) + " " + mi.Description
		} else {
			entry["description"] = mi.Description // descriptionZh 中文描述
		}
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits // 积分倍率原文（如 "x0.05"），仅展示
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // 多模态能力透出
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
		if mi.CanDisableThinking {
			entry["can_disable_thinking"] = true
		}
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// modelList 模型列表：CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel
// 对称）；global.enabled=true 时追加 global: 前缀的国际版名单。
// 纯动态：动态拉取失败/无号 → 该域空列表，无静态兜底。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0)
	// models.hidden 黑名单：快照外提，避免逐条目重复 Load。
	hidden := h.loadLive().HiddenModels
	for _, mi := range h.fetchDynamicModels() {
		if _, ok := hidden["cn:"+mi.ID]; ok {
			continue
		}
		entry := map[string]any{
			"id":       "cn:" + mi.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "workbuddy",
		}
		// context_length / max_output_tokens 四级查找（upstream.model_catalog）：
		// 上游动态值（maxInputTokens/maxOutputTokens）权威 → 静态种子表 →
		// model.json 本地缓存 → models.dev 按需拉取（异步不阻塞本次响应，拉到后
		// 写 model.json 供下次命中）→ 1M 兜底 / max_output_tokens 省略。
		// 上游零值不再透出假 131072（误导 Codex/ZCode 等按 context_length 提前
		// 截断、白白丢上下文）。
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		// 上游模型对象全字段透出（name/描述/标签/倍率/能力旗标等，空值省略）。
		entry = applyModelInfoFields(entry, mi)
		// effort 能力透出——远端 supportedEfforts 权威，缺失落到 CN 静态兜底表
		// （客户端可发现档位，不再盲传）。无档位 → 省略字段。
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["reasoning_supported_efforts"] = efforts
			if def != "" {
				entry["reasoning_default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	// global 模型名单：仅 GlobalEnabled=true 时列出（逃生门）。
	// 名单 = 纯动态探测结果（fetchGlobalModels，失败/无号 → 空）。
	if h.cfg.GlobalEnabled {
		// global 域 effort 能力三级查找：探测下发桶（权威）→ 静态兜底表 → 省略。
		// 先 fetchGlobalModels（内部探测并落 effort 桶），再按 id 取快照。
		globalIDs, globalAccount := h.fetchGlobalModels()
		// 探测对象形态的全字段条目（与 fetchGlobalModels 共享同一次探测缓存）：
		// 命中 id 才透出富字段；窄表/失败 → nil，按裸 ID 条目输出（不编造字段）。
		// globalAccount 为 nil（无 global 号）时返回 nil，跳过富字段映射。
		globalInfos := map[string]upstream.ModelInfo{}
		for _, mi := range h.cfg.Upstream.FetchGlobalModelInfos(globalAccount) {
			globalInfos[mi.ID] = mi
		}
		globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
		for _, id := range globalIDs {
			if _, ok := hidden["global:"+id]; ok {
				continue
			}
			entry := map[string]any{
				"id":       "global:" + id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "workbuddy",
			}
			// context_length / max_output_tokens 四级查找（与 CN 动态分支同口径）。
			var remoteCtx, remoteOut int64
			if mi, ok := globalInfos[id]; ok {
				entry = applyModelInfoFields(entry, mi)
				remoteCtx, remoteOut = mi.ContextWindow, mi.MaxTokens
			}
			entry["context_length"] = upstream.ContextWindowListingV4(id, remoteCtx, h.cfg.Upstream.HTTP)
			if mo, ok := upstream.MaxOutputTokensListingV4(id, remoteOut, h.cfg.Upstream.HTTP); ok {
				entry["max_output_tokens"] = mo
			}
			if efforts, def := upstream.EffortListing("global", id, globalEfforts[id], globalDefaults[id]); efforts != nil {
				entry["reasoning_supported_efforts"] = efforts
				if def != "" {
					entry["reasoning_default_effort"] = def
				}
			}
			out = append(out, entry)
		}
	}
	// 媒体模型（图像 / 视频）：与 chat 目录同一次上游探测刷新（FetchModels
	// 内部收集 media），这里只负责组装 OpenAI 模型条目。两类模型都有专用端点：
	// 图像同步返回图片，视频是异步任务（提交后轮询）。
	if ic := h.imagesCfg(); ic.Enabled && ic.ListInModels && h.cfg.Upstream != nil {
		seen := make(map[string]bool, len(out))
		for _, m := range out {
			if id, ok := m["id"].(string); ok {
				seen[id] = true
			}
		}
		for _, mi := range h.cfg.Upstream.MediaModels("cn") {
			if mi.ID == "" || (mi.Type != "image" && mi.Type != "video") {
				continue
			}
			id := "cn:" + mi.ID
			if seen[id] {
				continue
			}
			if _, hide := hidden[id]; hide {
				continue
			}
			seen[id] = true
			entry := map[string]any{
				"id":       id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "workbuddy",
				"type":     mi.Type,
			}
			entry = applyModelInfoFields(entry, mi)
			switch mi.Type {
			case "image":
				entry["endpoint"] = "/v1/images/generations"
				entry["edits_endpoint"] = "/v1/images/edits"
			case "video":
				entry["endpoint"] = "/v1/videos/generations"
				entry["poll_endpoint"] = "/v1/videos/{id}"
			}
			out = append(out, entry)
		}
	}
	// models.extra 静态补录：上游账号目录里没有、但要对客户端可见的模型
	// （典型场景：产品已宣传但账号级路由未下发的生图模型）。只负责「列出来」，
	// 不承诺可用——上游缺路由时点名调用仍返错误；hidden 优先级更高。
	// 放最后：与上面两段共用同一份 out 去重，上游真实条目永远压过补录条目。
	if extras := h.loadLive().ExtraModels; len(extras) > 0 {
		seen := make(map[string]bool, len(out)+len(extras))
		for _, m := range out {
			if id, ok := m["id"].(string); ok {
				seen[id] = true
			}
		}
		for _, em := range extras {
			if _, hide := hidden[em.ID]; hide {
				continue
			}
			if seen[em.ID] {
				continue
			}
			seen[em.ID] = true
			entry := map[string]any{
				"id":       em.ID,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "workbuddy",
			}
			switch em.Type {
			case "image":
				entry["type"] = "image"
				entry["endpoint"] = "/v1/images/generations"
				entry["edits_endpoint"] = "/v1/images/edits"
			case "video":
				entry["type"] = "video"
				entry["endpoint"] = "/v1/videos/generations"
				entry["poll_endpoint"] = "/v1/videos/{id}"
			}
			if em.Name != "" {
				entry["name"] = em.Name
			}
			if len(em.Tags) > 0 {
				entry["tags"] = em.Tags
			}
			out = append(out, entry)
		}
	}
	return out
}

// fetchGlobalModels 返回 global 模型名单（纯动态探测结果）及被探测账号。
// 缓存/失败回落封在 upstream.FetchGlobalModels（内部 1h + 5min 负缓存）。
// 本方法只负责"何时探测"：池中无 global 账号 → 空名单 + nil 账号（零上游调用）。
// 返回的 acct 供调用方在同一账号上取富 ModelInfo（FetchGlobalModelInfos 与
// FetchGlobalModels 共享缓存，不会触发第二次上游探测）。
// GlobalEnabled=false 时 modelList 已不进入本分支（逃生门在调用方 gate）。
func (h *Handler) fetchGlobalModels() ([]string, *auth.Auth) {
	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "global")
	if acct == nil {
		return nil, nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct), acct
}

// fetchDynamicModels 从第一个可用 CN 账号拉模型列表（含 contextWindow/maxTokens），
// 缓存 10min。
// 选号与 /panel/api/models 完全同口径（AvailableUIDsForRealm("cn") 首个 + AuthByUID），
// 而非 Pool.Pick()：Pick 无 realm 过滤，混合池里可能选中 global 号去打 CN 端点，
// 表现为偶发失败/面板与 /v1/models 两套目录（PR #38 报告并给出的选号修复）。
// 缓存 + 5min 负缓存按既有语义**保留**（#38 原案整体删除缓存被拒）：公开端点逐请求
// 实时拉取 = 每次 2 个上游探测，客户端周期性刷新模型列表会持续打上游；上游故障时
// 无冷却窗口，客户端重试即放大请求量——负缓存正是为此设计（见 handler_test 吸收
// 上游 9832283 的注释）；且 cachedModelsSnapshot（gateway_hint 判定）依赖缓存写入。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	uids := h.cfg.Pool.AvailableUIDsForRealm("cn")
	if len(uids) == 0 {
		return nil
	}
	acct := h.cfg.Pool.AuthByUID(uids[0])
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError：NoteError 喂的是 chat
		// 熔断器，models 端点偶发 5xx 跨界惩罚 chat 通道健康的账号；
		// models 拉取失败 ≠ 账号 chat 不可用。
		// 只在真正打上游失败时记一行（负缓存期内前面已直接 return，不会刷屏）：
		// 此前这里静默，CN 域不可达时 /v1/models 只剩 global 模型却毫无线索。
		if err == nil {
			err = fmt.Errorf("models api returned empty list")
		}
		log.Printf("WARN: [server] /v1/models: CN 模型目录拉取失败（%s 内不再重试，cn: 模型暂不列出）: %v",
			modelsFetchFailCooldown, err)
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// cachedModelsSnapshot 只读模型目录缓存（TTL 内快照）；缓存冷/空 → nil。
// 不发起任何上游调用（hint 判定用：错误路径加一次 FetchModels 网络调用既拖慢
// 错误响应、又污染上游调用语义）。
func cachedModelsSnapshot() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.ids) == 0 || time.Since(dynamicModelsCache.fetched) >= dynamicModelsTTL {
		return nil
	}
	return dynamicModelsCache.ids
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 客户端 IP 提取（按请求传递到 ChatStream，不透传时 upstream 侧忽略）；
	// 消除早年共享字段方案的并发交叉污染（issue：ClientIP 竞态）。
	clientIP := upstream.ExtractClientIP(r)
	// 请求体无大小上限（max_body_mb 已移除，对齐上游）：完整读入，超限类问题交由
	// 上游自然返回错误（其响应经既有错误分类链路透出，信息量更大）。#41 的截断
	// 防御语义保留在读错误路径——移除预拦截后，截断只可能来自客户端自己断流，
	// 读 body 出错就地 400，不把半截 JSON 喂上游 unmarshal 报 unexpected EOF 冤枉罚号。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 输入 token 校准的「估算侧」：客户端原始 body 的字符粗估，真实侧是本次响应的
	// usage.prompt_tokens（在 recordAttempt 里配对）。两处刻意设计：
	//  1. 延迟到确认拿到 usage 之后再算：多一次 body unmarshal（220KB 约 1.2ms）
	//     不该挡在首字节前面，失败的轮转也不产生样本；
	//  2. 认 rawBody 而不是 body：后者会被下面的提示词 / 模型名改写覆盖，估算必须
	//     对应客户端原文（与 count_tokens 拿到的输入同源）。否则网关注入的 system
	//     会被算进估算侧，intercept 学不到「固定开销」，count_tokens 反而偏小。
	rawBody := body
	// 拿到真实 usage 后统一记账：一次 unmarshal 同时喂两个消费者——系数校准器
	// （整段估算 vs 真实值）与会话基线缓存（这批消息的标志序列 vs 真实值）。
	// 只记成功样本：调用点被 delta.HasPromptTokens 挡着，轮转失败/错误响应不产生
	// 数据；带图请求只进基线不进系数（图像 token 估算侧数不到，会抬高 slope）。
	// 显式入参而不是捕获 bareModel / reqHasImage：这两个变量在闭包之后才解析出来，
	// 靠顺序捕获会让将来重排代码时静默失效（与 recordAttempt 的 ttfb 同理）。
	recordTokenSample := func(model string, withImage bool, actual int) {
		if model == "" || actual <= 0 || (h.cfg.TokenCalib == nil && h.cfg.Baseline == nil) {
			return
		}
		var parsed map[string]any
		if err := json.Unmarshal(rawBody, &parsed); err != nil {
			return
		}
		key := calibKey(model)
		if h.cfg.Baseline != nil {
			if idx := bridge.IndexMessages(parsed); len(idx.Digests) > 0 {
				h.cfg.Baseline.Record(key, idx.Digests, actual)
			}
		}
		if h.cfg.TokenCalib != nil && !withImage {
			if est := bridge.EstimateInputTokens(parsed); est > 0 {
				h.cfg.TokenCalib.Observe(key, est, actual)
			}
		}
	}

	// 工具名名单：上游在没有 tools 的请求里会把工具调用吐成原生标记文本
	// （见 upstream/dsml.go），修复层用它做严格判定。名单来自「本请求声明的 tools」
	// 与「会话历史里出现过的工具名」两个来源——实测 tools 声明会在中转环节丢失，
	// 只认前者会让修复层在最需要它的场景下失效。
	// 在提示词/模型名改写之前取（那些改写不动 tools / messages[].tool_calls 子树）。
	declaredTools := upstream.ToolNameAllowlist(body)

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	realm, bareModel, realmExplicit := resolveModelDetail(peek.Model)
	// 别名归一（upstream.CanonicalModel）：官方平台点号名等外部写法 → 网关规范名。
	// 必须在一切按名查表之前——牌价、隐藏名单、选号、6004 模型级冷却键、
	// 成本观测、出站 body 重写全部由 bareModel 承载，别名不穿透到这些键里。
	if canon := upstream.CanonicalModel(bareModel); canon != bareModel {
		log.Printf("WARN: [server] model alias: %s → %s（客户端写法，全链路按规范名处理）", bareModel, canon)
		bareModel = canon
	}
	// 图像模型的 chat 前门：只支持 /v1/chat/completions 的客户端也能直接生图。
	// 把 messages 折成 prompt + 参考图，复用 /v1/images/* 的选号/冷却/记账管道，
	// 结果封成 chat.completion（stream=true 时回 SSE chunk）。
	if h.isImageModelName(bareModel) {
		// 黑名单优先级不变：隐藏模型不能借图像前门绕过。
		if _, hide := h.loadLive().HiddenModels[realm+":"+bareModel]; hide {
			writeOpenAIErrorHint(w, http.StatusBadRequest, "model_unavailable",
				"model is hidden by gateway configuration, try another model", "")
			return
		}
		h.imageChat(w, r, body, bareModel)
		return
	}
	modelRate := ""
	if h.cfg.Upstream != nil {
		modelRate = h.cfg.Upstream.ModelRate(realm, bareModel)
	}
	// 跨域回退（issue #135）：只有裸模型名 + 显式开启 pool.cross_domain_fallback 时生效。
	// 语义 = 「裸名不绑定单一域」：首选域（裸名默认 cn）选不到可用号（账号级冷却/熔断/
	// 在途占满，或该域账号对这个模型 11102 后进入模型级负缓存）时，改用另一域再选一次；
	// 显式 cn:/global: 前缀保持原语义，永不回退。
	crossRealm := !realmExplicit && h.cfg.Pool.CrossDomainFallback()

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		// 来源在 ServeHTTP 入口采集（此时才知道开关与请求头），此处转交给统计对象，
		// 让 stdout 流水行与归档事件共用同一份来源值，两处不会漂移。
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()

	// models.hidden 黑名单：点名调用直接拒绝，不打上游、不占选号。
	// 与 /v1/models 的列表过滤对称（只藏列表不够，知道名字仍可硬调）。
	if _, hide := h.loadLive().HiddenModels[realm+":"+bareModel]; hide {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "model_unavailable",
			"model is hidden by gateway configuration, try another model", "")
		st.status = http.StatusBadRequest
		st.outcome = reqlog.OutcomeHTTPError
		return
	}

	tried := map[string]bool{}
	var lastErr error
	// modelBlock 记录本次是否因「模型级冷却」而选不到号（下方 acct==nil 分支填充）。
	// 默认零值 Blocked=false = 按既有的"没有可用账号"口径报错。
	var modelBlock pool.ModelBlockStatus

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// ExtractKey 与粘性开关解耦（issue #35 侧）：关闭粘性时会话头族的聚合主键仍按
	// 会话级（RequestIDForKey(sessKey)），不悄悄退化成轮级——提取本身与粘性无关。
	// 一次解析同时取会话键与「共享前缀指纹」（前缀亲和用，见 session.PrefixKey）。
	sessKey, prefixKey, prefixBytes := session.ExtractKeysSized(body)
	// 前缀材料的字节数是「亲和值得投多少」的判断依据：prefix 只占 prompt 一小部分时，
	// 亲和命中率再高也省不下钱（可命中的 token 本来就那么多）。随事件落进日志，
	// 面板据此算 prefix_share_est。
	st.prefixBytes = prefixBytes
	// prevBound 是**本次请求之前**的绑定（只读快照，不续期）。必须在这里取：
	// ResolveForModel 可能已经把绑定换掉，事后看返回值分不清「命中同号」和
	// 「失效换号」——两者都返回一个 uid，而面板的换号率正是靠这个区分。
	prevBound := ""
	if h.cfg.Session != nil && sessKey != "" {
		if uid, ok := h.cfg.Session.Peek(sessKey); ok {
			prevBound = uid
		}
	}
	// 落点解析分三步，顺序不可交换：
	//   ① 已有绑定 → 复用（粘性，强）
	//   ② 无绑定 → 前缀亲和（弱，只给提示）
	//   ③ 仍无 → 按策略分配（加权随机走哈希并写绑定；其余策略交给 Pool 选号）
	//
	// 🔴 曾经写成 ①+③ 合成一步（直接 ResolveForModel），那会让亲和永远不生效：
	// ResolveForModel 对新会话**立刻**完成哈希分配并写入 entries，等它返回时
	// stickyUID 已非空，亲和分支永远进不去——线上表现为「亲和率恒为 0 而看不出
	// 任何异常」。所以这里必须先用 LookupForModel（只复用、不分配）腾出空档。
	//
	// 用规范名（realm + 别名归一后的 bareModel）查可用性：6004 模型级冷却按规范名
	// 记账，若拿别名/原名来查会漏判「绑定号对该模型正限额」。
	canonModel := realm + ":" + bareModel
	stickyUID := ""
	if h.cfg.Session != nil && sessKey != "" {
		if uid, ok := h.cfg.Session.LookupForModel(sessKey, canonModel); ok {
			stickyUID = uid
		}
	}

	// ② 前缀亲和：会话**还没有绑定**时，用「这段 system+tools 的共同前缀缓存热在
	// 哪个号」做弱提示。它是粘性的补充而不是替代——粒度为前缀而非会话，正好覆盖
	// 「一个客户端并行跑多个会话、每个会话键不同、同一段几万 token 的 system 提示词
	// 却在每个号上各冷启动一次」这种场景（线上换号请求命中 34.2% 而非 0 的成因）。
	//
	// 只把亲和号写进 stickyUID（复用既有 PickStickyForModel 的 busy/unusable 语义），
	// 不新建一条选号路径：少一条路径就少一处与限额/在途/realm 判定不一致的风险。
	affKey, affinityUID := "", ""
	if h.cfg.Session != nil && prefixKey != "" {
		// 键里带规范化模型：上游前缀缓存是账号级**且模型级**的，同一段 system 在
		// 不同模型上是两份缓存，混用一个键会把其中一份的落点带到另一份上。
		affKey = prefixKey + "\x00" + canonModel
		if stickyUID == "" {
			if uid, ok := h.cfg.Session.AffinityFor(affKey, canonModel); ok {
				affinityUID = uid
				stickyUID = uid
			}
		}
	}

	// ③ 真正的新会话才分配。加权随机策略下由 session 做双段哈希分配（并落绑定）；
	// 其余有序策略历来交给 Pool 选号（LookupForModel 只复用），保持一致。
	if stickyUID == "" && h.cfg.Session != nil && sessKey != "" &&
		h.cfg.Pool.StrategyForRealm(realm) == pool.WeightedRandom {
		if uid, ok := h.cfg.Session.ResolveForModel(sessKey, canonModel); ok {
			stickyUID = uid
		}
	}

	// 轮级聚合键：按 body 里最后一条 user 消息派生（同轮内所有上游调用同键，
	// 换 user 消息换键）。#170 起带会话键的客户端也统一走轮级（对齐官方桌面 CLI
	// 的 X-Conversation-Request-ID 轮级语义——TraceStartHook 每次 USER_PROMPT_SUBMIT
	// 清空重生成），故不再限 sessKey=="" 才计算；sessKey 由下方派生处以复合键方式
	// 入键（防不同会话同轮文本互撞）。
	// 必须在下方 prompt.Rewrite 之前取——改写会动 messages 内容，之后取会让键漂移。
	turnKey := session.TurnKey(body)

	// gateway_hint 判定所需的请求形态（image_url part）：在改写前取（与 turnKey
	// 同理）。11133「模型不支持图片」指向的前提。
	reqHasImage := hasImagePart(body)

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			// 亲和提示（affinityUID）不是真实会话绑定：此时 sessKey 可能压根没有
			// 绑定项，对 aff 号调 Unbind 是空操作但语义误导，且会把 stickyUID 抹掉
			// 导致后续「是不是亲和号」的判断失灵。只对真绑定解绑。
			if stickyUID != affinityUID {
				h.cfg.Session.Unbind(sessKey)
			}
			stickyUID = ""
		}
	}
	// stickyOverflow 标记「本轮粘性号只是在途满载、请求溢出到别的号」。
	// 溢出是临时压力，成功后台**不能**把会话重绑到溢出号：否则每一次满载都会
	// 把会话永久搬到另一个账号，而上游前缀缓存是账号级的，搬走即全冷——线上
	// 实测换号请求占 70%、吃掉 59% credit。绑定保留，压力一过立刻回热。
	//
	// 声明位置在选号循环**之前**：noteAffinity 闭包要读它。
	stickyOverflow := false

	// noteAffinity 在**真实成功**后登记「这段前缀的缓存已在这个号上热」。
	//
	// 写入门槛刻意很窄（两个条件缺一不可）：
	//   - acct.UID == affinityUID：给已有的亲和记录续期；
	//   - affinityUID == "" && prevBound == "":  会话此前无绑定，本次是「新会话首次
	//     落点」，由它建立亲和。
	// 其余情况（尤其「亲和号满载、本次溢出到别的号」）**不写**——写了会把整段
	// 前缀拖到一个临时接活的账号上，下次再搬一次，形成前缀级乒乓。这种漂移交给
	// TTL 兜底：亲和号长期接不到该前缀，20 分钟后记录过期并按新落点重建。
	noteAffinity := func(uid string) {
		if affKey == "" || h.cfg.Session == nil || stickyOverflow {
			return
		}
		if uid == affinityUID || (affinityUID == "" && prevBound == "") {
			h.cfg.Session.NoteAffinity(affKey, uid)
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}
	// ttfb 首 token 等待（仅流式有观测；非流式传 0 = 无观测，速率不扣减）。
	// 显式入参而不是读 st.ttfb：后者在流式分支里是**调用之后**才赋值的，
	// 靠顺序传递会让将来重排代码时静默把速率算回旧的错口径。
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, started time.Time, ttfb time.Duration) {
		st.attempts++
		if delta.HasPromptTokens {
			st.promptTokens = delta.PromptTokens
			// 输入 token 校准：把「客户端 body 的字符粗估」与「上游真实 prompt_tokens」
			// 配对喂给校准器。不另找采集点——recordAttempt 是 chat 链路 usage 的唯一
			// 汇聚点，/v1/messages、/v1/responses 复用 chatCompletions 后同样经过这里，
			// 一份统计覆盖全部入口，也不会因为某个协议走别的分支而漏采。
			// 只有真带 prompt_tokens 的那次尝试才算数：轮转失败的分支传的是空 delta，
			// 天然被 delta.HasPromptTokens 挡掉，错误流量不会污染系数。
			// 带图请求也排除：图像 token 会进上游 prompt_tokens，但字符估算侧一个都
			// 数不到，混进来会把 slope 抬高——遭殃的正是纯文本的 count_tokens。
			recordTokenSample(bareModel, reqHasImage, int(st.promptTokens))
		}
		if delta.HasCompletionTokens {
			st.completionTokens = delta.CompletionTokens
		}
		if delta.HasTotalTokens {
			st.totalTokens = delta.TotalTokens
		} else if delta.HasPromptTokens || delta.HasCompletionTokens {
			st.totalTokens = st.promptTokens + st.completionTokens
		}
		delta.Model = bareModel
		if delta.Model == "" {
			delta.Model = peek.Model
		}
		latency := time.Since(started)
		latencyMs := latency.Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		delta.HasLatencyMs = true
		delta.LatencyMs = latencyMs

		// HasCompletionTokens 是「本次有没有 token 观测」的独立判断，与速率怎么算
		// 无关，所以留在调用点；速率本身的边界（TTFB 缺失/超界）收在 tokensPerSecond。
		if delta.HasCompletionTokens {
			if tps, ok := tokensPerSecond(delta.CompletionTokens,
				time.Duration(latencyMs)*time.Millisecond, ttfb); ok {
				delta.HasTokensPerSecond = true
				delta.TokensPerSecond = tps
			}
		}
		h.cfg.Pool.RecordTokenUsage(uid, delta)

		// 用量时序记录。ok 以「上游是否给了 usage」判定：空 delta 意味着这次尝试
		// 没拿到任何 token 统计（传输错误 / >=400 / 解析失败），计为失败尝试。
		// 失败也计入请求数——否则重试放大在「用量」视图里看不见。
		if h.cfg.Usage != nil {
			realm := "cn"
			if a, ok := h.cfg.Pool.Status(uid); ok && a.Realm != "" {
				realm = a.Realm
			}
			h.cfg.Usage.Add(time.Now(), realm, uid, delta.Model, usage.Delta{
				PromptTokens:     delta.PromptTokens,
				HasPromptTokens:  delta.HasPromptTokens,
				CompletionTokens: delta.CompletionTokens,
				HasCompletion:    delta.HasCompletionTokens,
				TotalTokens:      delta.TotalTokens,
				HasTotal:         delta.HasTotalTokens,
				Credit:           credit,
				HasCredit:        hasCredit,
				HasCacheTokens:   st.hasCache,
				CacheHitTokens:   st.cacheHit,
				CacheMissTokens:  st.cacheMiss,
				ModelRate:        modelRate,
				LatencyMs:        delta.LatencyMs,
				HasLatency:       delta.HasLatencyMs,
				TokensPerSecond:  delta.TokensPerSecond,
				HasTPS:           delta.HasTokensPerSecond,
			}, delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens)
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - append：开头连续 system/developer 块后插自有提示词，既有消息逐字不动
	//     （客户端项目规范/工具约定与网关提示词并用，issue #129）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough / append 非降级期：透传客户端原始 system（append 则再插一条网关 system）。
	// 降级裁决：append 在降级期退化为 replace（Rewrite(Degraded)）——append 带
	// 指纹原文重试是确定性再撞墙，replace 是一次性最小抢救（issue #129 设计 §4）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "append" && h.cfg.PromptText != "" && !h.degrade.Active() {
		body = prompt.Append(body, h.cfg.PromptText)
	} else if (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}

	// 会话头族（issue #35）：后台按 X-Conversation-Request-ID（对话轮级）聚合请求，
	// 官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。此处**轮转
	// 循环外**生成一次，循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再
	// 碎片化（此前网关一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个
	// RequestID）。
	//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先，否则按
	//     粘性 key 进程内稳定生成；粘性 key 也空时走轮级兜底（TurnKey/TurnRequestID），
	//     无 user 消息时退化成本请求级随机——轮转内捕获一次即共享；
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	// ConversationID passes the client dialog id through. Clients that have none
	// (Anthropic / Claude Code traffic) fall back to the session key, otherwise
	// the upstream prompt_cache_key degrades into one constant per account and
	// every client on that account ends up sharing a single, constantly evicted
	// prefix-cache slot.
	convID := session.ResolveConversationID(body)
	if convID == "" {
		convID = sessKey
	}
	chatMeta := upstream.ChatMeta{ConversationID: convID}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		// 轮级复合键：sessKey 入键防跨会话同轮文本互撞（#170 统一轮级）。
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		// 无会话键客户端：纯轮级键（既有兜底语义不变，存量会话键值零漂移）。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		// 残留空态兜底（无 user 消息/无可签名内容）：会话级聚合，好于请求级随机。
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键也无轮级键：请求级随机（轮转内捕获一次即共享）。
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	// stickyBound 用**请求前**的绑定（prevBound）而不是 resolve 后的 stickyUID：
	// 后者在「失效换号」时已经变成新号，拿它比对会把换号误记成命中。
	stickyBound := prevBound
	for i := 0; i < h.cfg.MaxRotate; i++ {
		stickyOverflow = false
		st.sticky = ""
		st.affinity = ""
		// 选号：粘性号优先（PickStickyForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			var why pool.StickyPickResult
			acct, why = h.cfg.Pool.PickStickyForModel(stickyUID, bareModel)
			switch {
			case acct == nil && why == pool.StickyBusy:
				// 🔴 只是「在途名额占满」这一个临时原因：**不解绑**。
				// 解绑会把整段会话永久搬到另一个账号，而上游前缀缓存按账号隔离，
				// 搬走即全冷（实测命中率从 99.9% 掉到 0）。本次请求溢出到普通轮换，
				// 绑定留给下一个（通常是串行的）轮次继续命中。
				// 保留绑定的代价只是每轮多一次 PickByUID 读锁查询，可忽略。
				if affinityUID != "" {
					// 弱粘性（前缀亲和）没有会话绑定需要保全，也**不能**设
					// stickyOverflow：否则本次成功的号永不 Bind，该会话下次仍从亲和
					// 出发、又被判 busy，逐轮换号。让绑定落在真正服务的号上才收敛。
					// acct 保持 nil → 落到下面普通选号，本次由别的号服务。
					st.affinity = reqlog.AffinityMiss
				} else {
					stickyOverflow = true
					st.sticky = reqlog.StickyBusy
				}
			case acct == nil:
				// 持久不可用：账号不存在/冷却/封禁/该模型被限额/积分保底 → 解绑重分配。
				if affinityUID != "" {
					st.affinity = reqlog.AffinityMiss
				} else {
					st.sticky = reqlog.StickyMiss
				}
				unbindSticky()
			case realm != "" && acct.Realm() != realm && !crossRealm:
				// 跨域回退开启且请求为裸名时，粘性号可以落在任一域（回退成功后绑定的是另一域
				// 的账号，下一轮不该被 realm 校验立刻解绑）。
				unbindSticky()
				acct = nil
			}
		}
		if acct == nil {
			// 模型感知 + realm 感知选号：模型非空时启用 6004 模型级冷却豁免
			// （healthyForModel），realm 谓词过滤跨域账号。
			pickRealm := realm
			if crossRealm && h.cfg.Pool.ServableForModelRealm(bareModel, realm) == 0 &&
				h.cfg.Pool.ServableForModelRealm(bareModel, otherRealm(realm)) > 0 {
				// 首选域此刻没有真正可服务的号（11102 模型级负缓存 / 账号级冷却 /
				// 在途占满），而另一域有 → 直接换域选号（issue #135）。先看健康度再选，
				// 避免拿全冷却兜底捞回来的旧域账号白撞一次上游。
				pickRealm = otherRealm(realm)
			}
			acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, pickRealm)
			if acct == nil && crossRealm {
				// 该域选不到（并发抢名额/轮换耗尽/另一域刚冷却完）→ 另一域再试一次，
				// 保持尽力而为语义；tried 已记录失败号，轮转仍由 MaxRotate 封顶。
				acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, otherRealm(pickRealm))
			}
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			// 记下"是不是模型级阻塞"。选号返回 nil 有两种完全不同的成因：
			//   (a) 池子真的没有可用号（账号级冷却/在途占满/积分保底）；
			//   (b) 号都在，但每个号都对这个模型处于模型级冷却（11102/11158/6004）。
			// 两者此前都报 no_healthy_account，导致「模型不可用」被读成「号全挂了」。
			// 在这里取一次快照，供下方错误构造区分（issue #102 附带发现 1）。
			modelBlock = h.cfg.Pool.ModelBlockedForRealm(bareModel, realm)
			break
		}
		st.uid = acct.UID
		// 同步昵称：请求流水行只写 uid8 时无法直观看是哪一号，昵称随本次选号带入日志行。
		st.nick = acct.Nickname
		// 出口标签：本次请求真实从哪个出口出去（src 是入口，这是出口）。
		// 只读解析，不发请求；未启用出口策略时得到 "direct"。
		if h.cfg.Upstream != nil {
			st.egress = h.cfg.Upstream.EgressLabel(acct)
		}
		if crossRealm && h.cfg.Upstream != nil && acct.Realm() != realm {
			// 跨域回退命中：按实际服务域取积分倍率，用量/账本记的是真实域口径，
			// 而不是请求前缀声明的域。
			modelRate = h.cfg.Upstream.ModelRate(acct.Realm(), bareModel)
		}
		tried[acct.UID] = true

		// 前缀亲和归类：只在「会话此前无绑定」时统计——有绑定说明亲和根本没参与
		// 决策，算进去会让 affinity_hit_rate 被大量无关请求稀释。
		if affKey != "" && st.affinity == "" && prevBound == "" {
			switch {
			case affinityUID != "" && acct.UID == affinityUID:
				st.affinity = reqlog.AffinityHit
			case affinityUID != "":
				st.affinity = reqlog.AffinityMiss
			default:
				st.affinity = reqlog.AffinityNew
			}
		}

		// 粘性落点归类（面板换号率 / 请求日志排查用）。只在本轮尚未被
		// busy/miss 标记时归类：那两种情况已经定性，不需要"事后归因"。
		if sessKey != "" && st.sticky == "" {
			switch {
			case stickyBound != "" && acct.UID == stickyBound:
				st.sticky = reqlog.StickyHit
			case stickyBound == "":
				st.sticky = reqlog.StickyNew
			default:
				// 有绑定却没用上、且没被标 busy/miss（例如解绑后重新分配到同一个号
				// 之外的路径）——保守记为 miss，宁可让面板显得"换号多"也不要漏报。
				st.sticky = reqlog.StickyMiss
			}
		}

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			// 粘性号压线被并发抢走名额：对本次请求等价于「临时繁忙」，记 busy。
			// 亲和号被抢同理，但记在 affinity 计上（两条机制的指标不混）。
			switch {
			case affinityUID != "" && acct.UID == affinityUID:
				st.affinity = reqlog.AffinityMiss
			case stickyBound != "":
				st.sticky = reqlog.StickyBusy
			}
			if !rotateBackoff(i, r.Context()) {
				// 客户端已断连：换号重试无意义，终止轮转走末端错误透传。
				break
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				if !rotateBackoff(i, r.Context()) {
					break // ctx 取消：终止轮转（refresh 失败换号退避）
				}
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}

		// 客户端 IP 按请求传递（PassthroughIP 开启时注入；消除共享字段竞态）。
		attemptStarted := time.Now()
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析）。传输层错误（非 *Error）走抖动换号分支；防御分支
		// （terr 为 nil 但 status>=400，如 ErrNone 兜底）回落本地 Classify，双保险。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		// 出站链路故障（代理握手失败 / 本机源地址不可达）：**账号没有问题**。
		// 不罚号、不喂熔断/连败，只让出口自己进冷却（upstream 侧已记账），本轮换号
		// 继续——换号即换出口，是有意义的尝试。
		if uerr != nil && uerr.Kind == upstream.ErrEgress {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = http.StatusBadGateway
			lastErr = uerr
			log.Printf("WARN: [server] egress failure acct=%s: %v (account not penalized, rotating to another exit)",
				logfmt.Label(acct.UID, acct.Nickname), terr)
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}
		if uerr == nil && terr != nil {
			// 上游超时 / 停滞：**不换号、不罚号**。
			//
			// 超时不是账号的问题：同一份请求换到别的号，撞上的是同一个慢上游，
			// 只会把客户端拖到 MaxRotate × header_timeout（部署值 600s 时最坏
			// 约半小时），期间还给一串健康号喂连败计数。此前全仓没有任何超时
			// 识别，超时和"网络抖动"共用同一条换号路径。
			//
			// 判定三态：net.Error.Timeout()（ResponseHeaderTimeout / Client.Timeout）、
			// 显式 deadline（DeadlineExceeded / os.ErrDeadlineExceeded）、以及
			// **客户端仍在但 ctx 被取消**——那只能是我们自己的空闲看门狗掐的流，
			// 也就是上游停滞。客户端主动断连时 r.Context() 已取消，走下面的抖动分支。
			if isUpstreamTimeout(terr, r.Context().Err() != nil) {
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
				st.status = http.StatusServiceUnavailable
				lastErr = fmt.Errorf("%w: %v", errUpstreamTimeout, terr)
				log.Printf("WARN: [server] upstream timeout acct=%s: %v (rotation stopped, account not penalized)",
					logfmt.Label(acct.UID, acct.Nickname), terr)
				break
			}
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」，
			// 连败 N 次临时出池，单次/偶发不罚（NoteFailures 内部达阈才动作）。
			// 上游 client 已打 transport error 日志。
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（传输层错误换号退避）
			}
			continue
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 内容拦截误报（passthrough/append 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试（append
			// 降级重试同样退化为 replace——原文在场只会确定性再撞 400）。
			// 第二次仍被拦（用户内容本身触发审核）→ 回内容防火墙错误（见下分支）。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**——换任何账号都会撞同一
				// 审核，轮转纯属浪费时间。不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError）。
				// error-passthrough：message 装上游 body 原文（code/msg/requestId 原样），
				// 不再改写成网关固定文案——客户端必须看到真实错误才能排查。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
					msg = "content blocked by upstream content firewall"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与 WAF IP fail-fast 同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作，fail 只释放租约。
			// message 装上游 body 原文（含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 图片格式/数据无效：立即透传上游原文回客户端，不罚号不轮转。
			// 同一 body 换账号仍是同样的解析结果，轮转只会放大无效请求。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 请求体解析失败（11101）：与 11115 / 图片无效同一哲学——同一 body 换任何
			// 账号都是同样的解析结果，轮转只会放大无效请求（每号一次上游调用 +
			// rotateBackoff 占用在途名额）。更要紧的是：继续轮转后末端会落到
			// 「其余保持 503」，把确定失败的请求伪装成"账号不可用、稍后再试"，
			// 客户端于是对必然失败的请求无限重试。立即透传上游原文回 400。
			if kind == upstream.ErrBadParams {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "chat request body was rejected by upstream"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "bad_params", msg,
					h.hintOf(upstream.ErrBadParams, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 6021「queue.full.title」渠道级容量满载：**不罚号不轮转**，立即透传
			// 上游原文回客户端（503 + upstream_busy）。上游集群排队打满时换任何
			// 账号都撞同一堵墙——按限流处置会逐号软冷却，实测 2 秒绞杀整池健康号
			// （2026-10-15 实案），且换号白撞把无效请求放大 MaxRotate 倍。客户端
			// 按 SDK 退避重试即可；等待窗口过去渠道恢复即成功。
			if kind == upstream.ErrUpstreamBusy {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "upstream capacity is full (queue.full); retry after a short wait"
				}
				writeOpenAIErrorHint(w, http.StatusServiceUnavailable, "upstream_busy", msg,
					h.hintOf(upstream.ErrUpstreamBusy, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusServiceUnavailable
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
			// 要求原文全量）+ Kind/RetryAfter（末端映射与冷却时长共用）。
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			// WAF IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义）：
			// 该次 WAF 403 喂入 IP 级状态机，若激活（短窗多号命中，IP 被拦而非账号）
			// 则立即终止轮转——继续换号只会把请求放大 MaxRotate 倍打同一出口 IP，
			// 加重风控。账号级软冷却已在上方 applyErrorPolicy 照常记账。
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（分类错误换号退避）
			}
			continue
		}
		// 成功判定与粘性绑定一律**延后到这一跳真正成功之后**（见下方流式/非流式分支）：
		// 上游「200 已开流 + 一帧 error」是真实形态（6004 限流、内容拦截、审核），
		// 此前在读第一帧之前就 NoteSuccess + 清 11102 负缓存 + 绑粘性 → 被限流的号
		// 记成健康、粘性把会话钉死在它身上，后续每一轮都打同一个限流号。
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
			// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
			// 组装请求上下文做判定）。
			// errFrame：上游 error 帧原文（观察者旁路采集），用于流尾的账号处置。
			var errFrame string
			// 标记修复：上游在没有 tools 的请求里会把工具调用吐成原生标记文本，
			// 这里在透传前还原成 delta.tool_calls（见 upstream/dsml.go）。
			// 第二个参数为 true = 名单为空时启用弱判定——tools 声明在链路上丢失是
			// 实测最常见的泄漏成因，只做严格判定等于对主场景不设防。
			repair := upstream.NewMarkupRepair(declaredTools, true)
			sErr := upstream.StreamHint(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}), upstream.WithErrorFrameObserver(func(payload string) { errFrame = payload }),
				upstream.WithMarkupRepair(repair))
			switch {
			case upstream.IsEmptyStreamError(sErr):
				// 上游 200 但空流（0 有效帧）：StreamHint 已写 error 帧 + [DONE]
				// 兜底（HTTP 头已发出只能 200），但这是上游缺陷不是成功——日志/
				// 状态收敛到 502 观测，与非流式 Aggregate 空流→502 upstream_parse
				// 同语义（此前 `_ =` 吞错把失败流记成 200，运维看到假成功）。
				// 只认 IsEmptyStreamError：客户端断连的写失败不误标（人已走，
				// 502 观测没有意义）。
				st.status = http.StatusBadGateway
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (200+0 frames)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			case errFrame != "":
				// 上游以 error 帧报错（6004 限流 / 内容拦截 / 审核）：按帧内容分类并
				// 处置账号——**不记成功、不清 11102 负缓存、不绑粘性**。此前这些动作
				// 在流开始前就做了，于是一个正在限流的号被当成健康号，粘性还会把
				// 整个会话钉在它身上，后续每轮都失败。
				kind := upstream.FrameKind(errFrame)
				h.applyErrorPolicy(acct.UID, kind, errFrame, bareModel, nil)
				st.status = http.StatusServiceUnavailable
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream acct=%s model=%s: upstream error frame kind=%s payload=%s",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, kind, logfmt.Truncate(errFrame, 200))
			case sErr != nil:
				// 客户端写失败（断连）：上游帧无恙，账号健康——账号侧照常记成功
				// （与 default 同语义），请求日志归为 Interrupted（人已走，未完成）。
				st.outcome = reqlog.OutcomeInterrupted
				h.cfg.Pool.NoteSuccess(acct.UID)
				h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
				if sessKey != "" && h.cfg.Session != nil && !stickyOverflow {
					h.cfg.Session.Bind(sessKey, acct.UID)
				}
				noteAffinity(acct.UID)
			default:
				// 真成功：这一跳读完且上游没有报错，才记成功并让粘性跟上。
				st.outcome = reqlog.OutcomeSuccess
				h.cfg.Pool.NoteSuccess(acct.UID)
				// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
				// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
				h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
				// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
				// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
				// 但「粘性号只是满载」的溢出轮**不重绑**：那会把会话永久搬到一个临时接活的号上，
				// 下一次满载又搬一次，形成乒乓漂移（前缀缓存每跳全冷）。绑定留在原号等压力消退。
				if sessKey != "" && h.cfg.Session != nil && !stickyOverflow {
					h.cfg.Session.Bind(sessKey, acct.UID)
				}
				noteAffinity(acct.UID)
			}
			credit, hasCredit := stats.Credit()
			if hit, miss, ok := stats.CacheTokens(); ok {
				st.cacheHit, st.cacheMiss, st.hasCache = hit, miss, true
			}
			recordAttempt(acct.UID, stats.Usage(), credit, hasCredit, attemptStarted, stats.TTFB())
			// WARN 信号放 recordAttempt 之后：st.promptTokens 此时才是本次的观测值。
			if st.hasCache {
				cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, st.cacheHit, st.cacheMiss)
			}
			if n := repair.Converted(); n > 0 {
				// 修复命中：上游本来会把工具调用吐成正文标记，这里已还原成 tool_calls。
				// 打一行便于运维确认「这次不再空转」以及统计发生率。
				log.Printf("INFO: [server] stream acct=%s model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, n, len(declaredTools))
			} else if m := repair.Seen(); m > 0 {
				// 识别到标记却没还原：说明判定没过（工具名不合规 / 块内夹带正文 /
				// 参数畸形）。单独记一行，排障时能一眼区分「没识别到」与「识别到但拒绝」。
				log.Printf("WARN: [server] stream acct=%s model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, m, len(declaredTools))
			}
			st.ttfb = stats.TTFB()
			// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"），
			// 不写入零值——否则「没观测到 usage」被伪造成「测得 0 token」，
			// 与非流式走 completionTokens 返回 -1 的口径不一致。
			if toks, hasUsage := stats.Tokens(); hasUsage {
				st.toks = toks
			}
			// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
			// 供下次选号把免费/便宜的号排在前面。
			if hasCredit {
				st.credit = credit
				st.hasCredit = true
				if total, tok := stats.TotalTokens(); tok && total > 0 {
					h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
				}
			}
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			st.outcome = reqlog.OutcomeHTTPError
			return
		}
		// 标记修复（非流式）：把正文里的原生工具调用标记还原成 message.tool_calls。
		// 只在「没有结构化 tool_calls」且「finish_reason 为 stop」时生效（见 dsml.go）；
		// 名单为空时走弱判定（tools 声明在中转环节丢失是常态）。
		mrepair := upstream.RepairAggregatedResponse(resp, declaredTools, true)
		if n := mrepair.Converted(); n > 0 {
			log.Printf("INFO: [server] non-stream model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
				bareModel, n, len(declaredTools))
		} else if m := mrepair.Seen(); m > 0 {
			log.Printf("WARN: [server] non-stream model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
				bareModel, m, len(declaredTools))
		}
		credit, total, hasCredit := usageCreditTotal(resp)
		if usage, ok := resp["usage"].(map[string]any); ok {
			if hit, miss, okC := cacheTokensFromUsage(usage); okC {
				st.cacheHit, st.cacheMiss, st.hasCache = hit, miss, true
			}
		}
		recordAttempt(acct.UID, usageDeltaFromResponse(resp), credit, hasCredit, attemptStarted, 0)
		if st.hasCache {
			cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, st.cacheHit, st.cacheMiss)
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.outcome = reqlog.OutcomeSuccess
		st.toks = completionTokens(resp)
		// 非流式同理：聚合成功（无 error 帧、非空流）才算这一跳成功，事后才记成功/绑粘性。
		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		// 与非流式口径对齐：粘性号只是满载的溢出轮不重绑（否则乒乓漂移）。
		if sessKey != "" && h.cfg.Session != nil && !stickyOverflow {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		noteAffinity(acct.UID)
		// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
		if hasCredit {
			st.credit = credit
			st.hasCredit = true
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
		}
		return
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	// 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	// requestId 原样保留）。HTTP 状态码按 OpenAI 兼容口径映射类别：ErrSoftRate → 429
	// （限流语义、客户端应等待重试），其余保持 503。本地调度类错误（无可用账号/
	// 传输层抖动/非上游返回的 lastErr）→ 保留自有文案 no_healthy_account（本地错误
	// 没有上游原文可透传，不编造）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	// 上游超时：轮转已在传输层分支止损（见 isUpstreamTimeout），这里给一条**能区分**
	// 的文案，别混进"没有可用账号"——两者的排查方向完全不同。
	if errors.Is(lastErr, errUpstreamTimeout) {
		code = "upstream_timeout"
		msg = "upstream timed out: rotation stopped (another account would hit the same slow upstream), please retry later"
	}
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定；本地调度类
	// 错误（无上游原文）固定 no_healthy_account hint。
	hint := upstream.NoHealthyAccountHint()
	// upstreamMsgPassed 记录 error.message 是否已被上游原文占据：模型级阻塞分支
	// 据此决定要不要覆盖 msg（上游原文优先，含 requestId）。
	upstreamMsgPassed := false
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrWafBlock:
			if h.wafIP.active() {
				// IP 级拦截措辞（fail-fast 终止路径）：网关出口 IP 被 WAF 拦截、
				// 轮转已止损、窗口过后自动解除。客户端提前重试无意义（换号不换 IP）；
				// 有上游原文时原文优先（下方统一）。
				code = "waf_ip_blocked"
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		case upstream.ErrUpstreamBusy:
			// 6021 queue.full 渠道级容量满载（防御路径：正常已在轮转循环内
			// fail-fast 拦截）。503 + 可重试语义——上游原文（下方统一透传）
			// 含 requestId，客户端按 SDK 退避重试即可。
			code = "upstream_busy"
			msg = "upstream capacity is full (queue.full); retry after a short wait"
		case upstream.ErrEgress:
			// 全部账号的出口都不通（代理层故障 / 本机地址全不可用）：
			// 502 而非 503 —— 这是网关侧链路问题，不是「账号池暂时过载」，
			// 客户端按 5xx 退避重试即可，不必换模型。
			code = "egress_unavailable"
			status = http.StatusBadGateway
		case upstream.ErrModelBlocked:
			// 11102「该后端无此模型」：上游原文（下方统一透传）已经写清了原因，但
			// code 此前停在默认的 no_healthy_account —— 那是"服务端过载"的语义，
			// 客户端据此会不断重试（#81 里就是 503 → 客户端自动重试 5 次），而真正
			// 该做的是换个模型。用 400 + 明确的 code 把不可重试的性质讲清楚。
			code = "model_unavailable"
			status = http.StatusBadRequest
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
			upstreamMsgPassed = true
		}
	}
	// 模型级阻塞覆盖上面的通用文案（放在最后 = 优先级最高）。
	//
	// 为什么能覆盖 code 而不丢信息：modelBlock.Reason 就是 BlockModelBackoff 存下的
	// 上游原因，已在 hint 里原样带出，所以覆盖 message 不会丢掉上游信息，反而补上
	// 了上游不会告诉客户端的两件事——「有几个号被挡」和「最早什么时候解封」。
	//
	// 用 400 而非 503：这是"你选的这个模型当前不可用"，不是"服务端暂时过载"。
	// 换号重试必然同样失败，客户端不该按可重试错误处理。
	if modelBlock.Blocked {
		code = "model_unavailable"
		// 有上游原文时保留它（含 requestId，用户要拿去向上游反馈），只把"有几个号
		// 被挡、最早何时解封"这类本地调度信息放进 gateway_hint。
		if !upstreamMsgPassed {
			msg = "model is unavailable on every account (per-model cooldown), try another model"
		}
		hint = fmt.Sprintf("model_blocked: %d account(s) cooling down this model", modelBlock.Count)
		if !modelBlock.Until.IsZero() {
			hint += "; earliest unblock at " + modelBlock.Until.Format(time.RFC3339)
		}
		if s := strings.TrimSpace(modelBlock.Reason); s != "" {
			hint += "; upstream: " + s
		}
		status = http.StatusBadRequest
	}
	writeOpenAIErrorHint(w, status, code, msg, hint)
	st.status = status
	st.outcome = reqlog.OutcomeHTTPError
}

// tokensPerSecond 计算吐字速率（token/s），返回 (速率, 是否有意义)。
//
// 分母用「生成耗时」= 端到端耗时 - 首 token 等待（TTFB），不是端到端耗时。
//
// 为什么要减：不减的话，首 token 等待越长、报告速率被压得越低。同一模型换个
// 上游或网络，TTFB 从 0.3s 涨到 2s，速率能凭空掉一半——读起来像"模型变慢了"，
// 其实只是排队久了。issue #34 报的正是这个，维护者也确认「速率计算时并没有减去
// 首token到达时间」。
//
// ttfb<=0 表示没有观测：非流式回复天然没有「首个 data 帧」（日志里那一列记的是
// "-"）。此时不猜、不扣——凭空假定一个 TTFB 会把分母推向零、把速率抬成虚高，
// 比不扣更糟。只有真测到才扣。
//
// 同理，ttfb 不小于总耗时时（时钟粒度、或 TTFB 落在计时终点之后）退回端到端耗时，
// 避免零/负分母。token 数为负哨兵值（-1 = 观测缺失）时返回 false。
//
// 扣除后不足 minGenWindow 也退回端到端（issue #127）：ttfb 量的是「首个 SSE 帧
// 到达」，当上游把整个响应攒到最后一次性下发（假流式/中间层攒批刷新——首帧与
// 末帧几乎同时到）时，total−ttfb 只剩几毫秒，拿它当分母会把几百 token 除成
// 上万 tok/s 的幻数。这种形态下「生成时长」根本不可测，诚实的分母只有端到端。
//
// 用量账本（handler）与控制台流水行（logging.go）都走这一个函数：两处各算一遍时
// 口径漂移过一次（流水行漏扣 TTFB、与面板数字对不上），共用是防再次分叉的唯一办法。
func tokensPerSecond(completionTokens int64, total, ttfb time.Duration) (float64, bool) {
	if completionTokens < 0 || total <= 0 {
		return 0, false
	}
	gen := total
	if ttfb > 0 {
		if g := total - ttfb; g >= minGenWindow {
			gen = g
		}
	}
	return float64(completionTokens) / gen.Seconds(), true
}

// minGenWindow 可信生成窗口的下限：扣除 TTFB 后剩余窗口不足该值即视为「生成
// 时长不可测」，退回端到端耗时（见 tokensPerSecond 注释，issue #127）。
// 真流式下首帧到末帧通常铺满剩余窗口，200ms 远低于正常生成时长，不影响真实
// 快速输出（如缓存命中后的爆发）被如实报告。
const minGenWindow = 200 * time.Millisecond

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// usageCreditTotal 从聚合响应取 usage.credit 与 total_tokens（成本台账非流式入口）。
// 任一字段缺失/非法 → ok=false（不记录）。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		return 0, 0, false
	}
	c, _ := usage["credit"].(float64)
	t, _ := usage["total_tokens"].(float64)
	if t <= 0 {
		return 0, 0, false
	}
	return c, int(t), true
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2）：第 i 次轮转失败
// （continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，±25% 抖动），
// ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转（客户端已走，
// 换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；正常单号请求
// （首次成功）不经过本函数，零开销。
func rotateBackoff(i int, ctx context.Context) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// errUpstreamTimeout 上游超时的哨兵：末端出口据此给出与「没号可用」可区分的文案。
var errUpstreamTimeout = errors.New("upstream timeout")

// isUpstreamTimeout 判断这一跳的失败是否属于「上游超时 / 停滞」。超时不是账号的
// 问题，换号注定白换（同一份请求撞同一个慢上游），必须止损：不轮转、不罚号。
// 三态判定见传输层错误分支的注释。
func isUpstreamTimeout(err error, clientGone bool) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	return !clientGone && errors.Is(err, context.Canceled)
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify / ChatStreamContext 的 *Error 信封），
// 此处不再按原始 status 二次判断。仅在 chatCompletions 轮转循环内调用：内容拦截
// 会立即 400 返回，其余种类 continue 换号（continue 前由 rotateBackoff 退避）。
//
// 十条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 优先对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级豁免、
//     非 6004 走账号级，均不指数堆加）；无重置时间才走有界退避。冷却时长优先采信
//     Retry-After 头（uerr.RetryAfter，body 文案墙钟之外的头形态来源）。
//   - ErrWafBlock → 账号级软冷却：**不 Disable**——WAF 403 是 IP/指纹维频控信号，
//     罚过即走、到期自愈。时长优先 Retry-After 头；缺失按 wafCooldownBase(60s)
//     起 · softStreak 指数、封顶 soft_rate_max 的既有 CooldownSoftRate 有界退避。
//     基数经 jitterDur 抖动（防多账号同相位冷却到期再聚团）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号；passthrough 首遇触发降级重试，最终仍拦则回 400。
//   - ErrBadParams → 不罚账号，且与 ErrPromptTooLong/ErrImageInvalid 同待遇：
//     调用方在轮转循环内即刻 400 透传原文终止（换号必然同样失败）。
//   - ErrPromptTooLong → 11115：请求的问题不是账号的问题。零动作（不冷却/不熔断/
//     不 NoteError、不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrImageInvalid → 图片格式/数据无效：请求的问题不是账号的问题（同一 body
//     换任何号都会得到相同的解析错误）。零动作（不冷却/不熔断/不 NoteError、
//     不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102/11158 负缓存避让。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断；ErrClient
//     额外喂连败计数（NoteFailures，issue #114）：未知 4xx 连败 N 次临时出池。
//
// body 仅在 ErrSoftRate/ErrAccountFault 分支用于解析重置时间/分野；model 为请求
// 携带的模型名。uerr 是 ChatStreamContext 返回的分类信封（可携带 RetryAfter）；
// 零值/防御路径下为 nil，冷却时长回落既有计算。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 统一对齐上游重置时间：只要 body 带「将在 … 重置」，无论业务 code 是
		// 6004 还是 11140 rate-limiting 等形态，都精确冷却到该墙钟、绝不指数堆加。
		//   - 模型级（6004）→ CooldownSoftForModel：写 modelCooldowns[model]，切模型豁免。
		//   - 账号级（非 6004）→ CooldownSoftRate：写账号级 until，不产生模型豁免。
		modelRateLimited := upstream.IsModelRateLimit(body)
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			if modelRateLimited {
				h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), resetAt, model, "6004 model rate limit")
				return
			}
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), resetAt, "429 rate limit")
			return
		}
		// body 无重置文案但带 Retry-After 头 → 冷却到该时刻（不做指数堆加）。
		// 头优先于「有界退避」，但低于 body 重置文案（文案是上游更权威的口径）。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Now().Add(uerr.RetryAfter), "429 rate limit (retry-after)")
			if modelRateLimited {
				h.cfg.Pool.RecordModelRateLimitAudit(uid, model, "6004 model rate limit (reset unknown)")
			}
			return
		}
		// 无重置时间 → 账号级有界退避（soft_rate 基数起、softStreak 翻倍、封顶
		// soft_rate_max；已在冷却中的兜底探测不翻倍）。基数取 h.softCooldown()
		// （热改优先），管理面板改 soft_rate 后立即生效。
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Time{}, "429 rate limit")
		if modelRateLimited {
			h.cfg.Pool.RecordModelRateLimitAudit(uid, model, "6004 model rate limit (reset unknown)")
		}
	case upstream.ErrWafBlock:
		// WAF 403（无业务信封拦截形态）。软冷却复用 CooldownSoftRate 家族：基数
		// wafCooldownBase（60s，抖动后落 [45s,75s]）、softStreak 指数升级、封顶
		// soft_rate_max、冷却中兜底探测不翻倍——全部继承既有语义。
		// Retry-After 头优先（WAF 拦截页可能带该头）。不 Disable。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Now().Add(uerr.RetryAfter), "waf 403 block (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "waf 403 block")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按业务码分野（口径与 Classify 一致）：
		//   - 11140 + "request illegal" → 账号被上游限制（异常行为/风控）：硬禁用。
		//     解除只能由用户在 WorkBuddy 官方渠道申诉，重登无效（issue 1849 结论）。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
		// 注意 11128 的 msg 同为 "request illegal"，但那是通道校验失败，已在
		// Classify 分流到 ErrChannelRefused，走不到这里。
		if upstream.IsAccountRestricted(body) {
			h.cfg.Pool.Disable(uid, "account restricted by upstream (11140 request illegal); appeal in WorkBuddy (submit feedback) to lift the restriction")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), "account fault (14017)")
	case upstream.ErrChannelRefused:
		// 11128 通道校验失败（上游判定请求可能不是官方客户端发出，displayMsg 就是
		// 「请求被拦截，请重新发送」）。账号本身健康——官方客户端可正常使用同一账号，
		// 这不是账号级故障，**不罚账号**（无冷却/熔断/NoteFailures）。与
		// ErrContentBlocked 同待遇，由轮转继续尝试（换号不改变通道指纹，若全池同错
		// 会把上游原文透传给客户端，提示排查客户端指纹）。
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// 内容策略拦截（误报）：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 模式由 chatCompletions 内降级重试处理；custom 模式本不会到此分支。
	case upstream.ErrPromptTooLong:
		// 11115「prompt is too long」：请求的问题不是账号的问题（同一 body 换任何
		// 号都超限）。零动作（不冷却/不熔断/不 NoteError，同 ErrContentBlocked 待遇），
		// chatCompletions 已直接透传原文返回不轮转——该分支只为文档完备。
	case upstream.ErrImageInvalid:
		// 图片格式/数据无效：请求的问题不是账号的问题（同一 body 换任何号都会
		// 得到相同解析错误）。零动作，chatCompletions 已 fail-fast 透传。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关侧不再截断，均为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；chatCompletions
		// 已 fail-fast 400 透传原文、终止轮转——「换号可能有不同模型权限」属 11102
		// （ErrModelBlocked）的分类域，与本类无关。
	case upstream.ErrUpstreamBusy:
		// 6021「queue.full.title」渠道级容量满载：账号余额健康、未限流、session 未死，
		// 换号也撞同一堵墙。零动作（不冷却/不熔断/不 NoteError/不喂连败，同
		// ErrContentBlocked 待遇）；chatCompletions 已 fail-fast 503 透传原文、
		// 终止轮转——该分支只为文档完备。
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」/11158「该账号类型无此模型权限」：
		// (账号, 模型) 负缓存避让。复用 modelCooldowns 机制（与 6004 同域），
		// 选号侧 healthyForModel 对该账号自动避开该模型。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReasonFor(body))
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
		// ErrClient（未知 4xx）喂连败计数（issue #114）：连续 N 次该形态失败 →
		// 账号临时出池（NoteFailures 达阈降权），单次/偶发不罚（不误伤）。ErrNone
		// 到这里属防御路径（status>=400 但分类成功），语义不明不喂。
		if kind == upstream.ErrClient {
			h.cfg.Pool.NoteFailures(uid)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// wafCooldownBase WAF 403 软冷却基数（建议 60s 起；抖动 ±25% 后落 [45s,75s]，
// 实际进入 CooldownSoftRate 后再按 softStreak 指数、封顶 soft_rate_max）。
// 与 SoftCooldown 分流的原因：WAF 403 是 IP/指纹维频控，信号比 429「账号级限流」轻
// （账号本身健康），但比 404 重（带粘性会连环）；60s 级的快速避让已足够让频控窗口
// 滑过。抖动复用 backoff.go jitterDur（单一来源）。
const wafCooldownBase = 60 * time.Second

// writeOpenAIErrorHint 同 writeOpenAIError，另在 error 对象上附加
// error.gateway_hint（hint 为空串时不带字段——未覆盖形态不编造）。
// message 仍是上游原文透传（hint 只做并列补充，绝不替换/包装 message）。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":      msg,
			"type":         "api_error",
			"code":         code,
			"gateway_hint": hint,
		},
	})
}

// hasImagePart 报告聊天请求体是否携带多模态 image_url part（OpenAI 兼容形态
// messages[].content[] {type:"image_url"}）。畸形/其他形态一律 false（hint 侧
// 宁缺勿滥：判不出带图就不给「模型不支持图片」指向）。
func hasImagePart(body []byte) bool {
	var peek struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, m := range peek.Messages {
		for _, p := range m.Content {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// hintContext 组装 chatCompletions 的 gateway_hint 判定上下文：请求裸模型名 +
// 是否带图 + 模型目录 supports_images 声明（目录未收录 → ModelInCatalog=false，
// 不做「不支持」判定，防查不到误判）。仅错误路径调用（成功请求零开销）。
//
// 目录查询只读既有缓存快照（cachedModelsSnapshot），**不触发上游拉取**：错误路径
// 加一次 FetchModels 网络调用既拖慢错误响应、又污染上游调用语义（错误风暴时放大
// 请求量——与 WAF IP fail-fast 的「不放大请求量」哲学相悖）。缓存冷（最近 10min 未
// 拉过）→ ModelInCatalog=false，11133 退中性 hint（宁缺勿滥，不编造能力事实）。
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" {
		return ctx
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsImages
			return ctx
		}
	}
	return ctx
}

// hintOf 末端错误透传的统一 hint 入口：kind + 上游原文 + 请求上下文 →
// gateway_hint 文案（upstream.GatewayHint 单一事实来源）。uerr 为 nil 时回落
// body 原文判定（防御路径）。transport 层错误（lastErr 非 *upstream.Error 且
// 上游没回 body）→ 无 hint（不编造）。
func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}
