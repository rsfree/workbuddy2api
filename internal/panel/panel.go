// Package panel 内嵌式 Web 管理面板：账号池总览、单号运维（解冻/禁用/签到/
// 刷新余额/移除）、浏览器内 OAuth 添加账号（免重启热加载进池）、手动批量
// 签到/保活，以及运行日志环形缓冲（镜像 log 包与 chat 表格日志）。
//
// 设计约束：
//   - 前端 go:embed 单文件（index.html），无任何外部构建依赖，与二进制同体部署；
//   - 鉴权复用网关 api_key（Bearer），与 /v1/* 同一口径；api_key 为空 = 不鉴权
//     （仅本机/私网使用）。面板 HTML 本身无秘密，可匿名加载，密钥只发给 /panel/api/*；
//   - 不改写既有池语义：所有运维操作落到 pool 已有入口（Revive/Disable/Remove...），
//     添加账号走 auth.SaveAtomic + pool.Add，重启后与 auths/ 目录天然对齐。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// Config 面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler // 手动触发签到/保活；nil 时对应接口返回 501
	AuthDir   string               // OAuth 登录完成后凭证落盘目录
	APIKey    string               // 空 = 不鉴权（与主服务同语义）；与 Live 同时给出时 Live 优先
	RedisMode string               // "upstash" / "noop"，仅观测透出
	Version   string               // 面板版本号（展示用）

	// Live 运行期可变配置（在线改配置立即生效）。
	Live *livecfg.Holder

	// ConfigPath config.json 路径与加载器（配置页读写用）。
	// LoadConfig 返回解析后的配置对象（前端展示/校验用，具体类型由 main 注入的闭包决定）；
	// nil 时配置页返回 501。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig 校验并落盘配置，返回需要重启才能生效的字段列表；随后由 main 注入的
	// ApplyConfig 闭包完成热生效（池参数/排程/密钥/脱敏）。error 时配置不写盘。
	SaveConfig func(raw []byte) (restartRequired []string, err error)

	// StickyCount 返回粘性会话绑定数；nil 时报告 0。
	StickyCount func() int

	// Usage 逐请求用量记录器（nil = 用量接口返回 501）。
	Usage *usage.Recorder
	// RequestLog 请求指标与归档（nil = 对应接口返回 501）。
	RequestLog *reqlog.Recorder
	// CreditHistory 积分变动账本（nil = 积分历史接口返回 501）。只读展示：
	// 留痕由余额查询路径（upstream.Client 的余额观察者）写入。
	CreditHistory *credithist.Ledger

	// ProbeFile 模型输出上限探测结果文件（scripts/probe_max_tokens.py --panel-out
	// 写入；空或文件不存在 = model_probes 端点返回空集，面板不显示任何实测标注）。
	// 只读展示：网关不解析、不依赖其内容做任何路由/出站决策。
	ProbeFile string

	// CatalogFile 任务目录文件（taskcatalog.go）：记录上游下发过的任务码，用于
	// 「发现新任务」。空 = 纯内存（测试场景），不落盘。
	CatalogFile string
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)，
// 本 mux 的 pattern 均带 /panel 前缀（外层不做前缀剥离）。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	// logins 进行中的 OAuth 设备授权会话（state → 会话信息）。
	// poll 成功或超时（loginTTL）后剔除；面板常驻进程，容量天然有界。
	loginMu sync.Mutex
	logins  map[string]loginSession

	// taskMu/taskLocks 一键完成任务的 per-account 互斥：同一账号的任务动作
	// （单任务 / 全量）同时只允许一条在跑。重复点击直接返回 409"仍在执行"，
	// 而不是并发跑两遍浪费上游请求（动作虽幂等，expert 系每遍含 8 次真实对话）。
	// 不同账号之间不互斥（并行照旧）。TryLock 语义，锁条目常驻（账号数有界）。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex

	// 任务中心执行队列（taskcenter.go）。
	queueOnce sync.Once
	q         *queueState

	// 任务目录（taskcatalog.go）：持久化「见过哪些任务码」，用于发现新任务。
	catOnce sync.Once
	cat     *taskCatalog

	// 专家市场快照（autotask.go）：定向专家任务按名字匹配专家，一次快照多任务
	// 复用，避免逐任务重拉分页列表。uid → 快照，TTL 见 expertCacheTTL。
	expertMu    sync.Mutex
	expertCache map[string]expertCacheEntry

	// 容量分析扫描完整请求归档，低频读取并缓存；互斥同时合并相同/不同参数的并发计算。
	capacityMu    sync.Mutex
	capacityCache map[string]capacityCacheEntry
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// loginTTL 授权 URL 的最长有效期：超时的 state 直接回收，
// 防止"开了添加账号弹窗就走开"的会话永久滞留。
const loginTTL = 15 * time.Minute

// loginSession 进行中的 OAuth 会话：创建时刻 + realm（cn/global，用于落盘与端点切换）。
type loginSession struct {
	created time.Time
	realm   string // "cn" / "global"，缺省 cn
}

// New 构建面板。
func New(cfg Config) *Panel {
	if cfg.RedisMode == "" {
		cfg.RedisMode = "noop"
	}
	p := &Panel{
		cfg:     cfg,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logs:    NewRing(500),
		logins:  map[string]loginSession{},
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 与 chat 表格日志进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/mobile.css", p.mobileStyle)
	p.mux.HandleFunc("GET /panel/mobile.js", p.mobileScript)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/request_metrics", p.withAuth(p.requestMetrics))
	p.mux.HandleFunc("GET /panel/api/request_logs", p.withAuth(p.requestLogs))
	p.mux.HandleFunc("GET /panel/api/credit_history", p.withAuth(p.creditHistory))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAuth(p.loginPoll))
	p.mux.HandleFunc("GET /panel/api/login/regions", p.withAuth(p.loginRegions))
	p.mux.HandleFunc("POST /panel/api/import/cockpit", p.withAuth(p.importCockpit))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAuth(p.accountRevive))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/pause", p.withAuth(p.accountPause))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/resume", p.withAuth(p.accountResume))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	p.mux.HandleFunc("POST /panel/api/accounts/prune_banned", p.withAuth(p.accountPruneBanned))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.withAuth(p.accountTasks))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.withAuth(p.accountTaskAccept))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.withAuth(p.taskAcceptAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.withAuth(p.accountTaskClaim))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.accountTaskAuto))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.withAuth(p.accountTaskAutoAll))
	p.mux.HandleFunc("POST /panel/api/tasks/scan_all", p.withAuth(p.tasksScanAll))
	p.mux.HandleFunc("POST /panel/api/tasks/run_queue", p.withAuth(p.tasksRunQueue))
	p.mux.HandleFunc("GET /panel/api/tasks/queue", p.withAuth(p.tasksQueueStatus))
	p.mux.HandleFunc("GET /panel/api/tasks/catalog", p.withAuth(p.tasksCatalog))
	p.mux.HandleFunc("GET /panel/api/school/vouchers", p.withAuth(p.schoolVouchers))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAuth(p.travelAll))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAuth(p.activityAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	p.mux.HandleFunc("GET /panel/api/capacity", p.withAuth(p.capacityReport))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.usageSave))
	p.mux.HandleFunc("GET /panel/api/model_probes", p.withAuth(p.modelProbes))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
	p.mux.HandleFunc("GET /panel/api/compensation", p.withAuth(p.compensation))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/compensation", p.withAuth(p.accountCompensationClaim))
	p.mux.HandleFunc("POST /panel/api/compensation_all", p.withAuth(p.compensationAll))
}

// ServeHTTP 统一入口：先写安全响应头再分发，保证页面、静态资源、API
// 与 401 错误响应全都带上（API 也可能在浏览器里被直接打开）。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// withAuth 与 server 包同口径的 Bearer 鉴权（经 httpauth 常量时间比较）；
// api_key 为空时放行。密钥经 livecfg 快照读取：面板里改了 api_key，下一个请求
// 即用新值（无需重启）。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, p.apiKey()) {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")
			return
		}
		next(w, r)
	}
}

// apiKey 当前生效密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// expiringSoonWindow 返回调度器当前生效的快过期路由窗口；测试面板无调度器时返回 0。
func (p *Panel) expiringSoonWindow() time.Duration {
	if p.cfg.Scheduler == nil {
		return 0
	}
	return p.cfg.Scheduler.ExpiringSoonWindow()
}

// poolExpiryBuckets 把 upstream 的按日到期批次转换为 pool 结构。两个包字段同构，
// 手工转换是为了不让底层 pool 反向依赖 upstream。
func poolExpiryBuckets(in []upstream.ExpiryBucket) []pool.ExpiryBucket {
	if len(in) == 0 {
		return nil
	}
	out := make([]pool.ExpiryBucket, len(in))
	for i, b := range in {
		out[i] = pool.ExpiryBucket{Date: b.Date, ExpireAt: b.ExpireAt, Remain: b.Remain}
	}
	return out
}

// ---------------------------------------------------------------------------
// 只读接口
// ---------------------------------------------------------------------------

// overview 总览：池计数 + 每账号状态 + 面板元信息。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	// 用 WithPaused 而不是 CountsDetailed：后者把暂停号并进 disabled（/status 的
	// 「不可用」口径），而概况卡片要分开展示（issue #125）。
	total, healthy, cooling, disabled, paused, inFlightFull := p.cfg.Pool.CountsDetailedWithPaused()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	// accounts 只取一次快照：概览与「可剔除的封禁号」计数必须来自同一时刻。
	// 各自 List() 一次会在两次读锁之间被新到的 11140 插队，出现「已禁用 5 / 可剔除 6」
	// 这种自相矛盾的展示。计数口径与 Pool.Banned 一致（同用 IsUpstreamBannedReason）。
	accounts := p.cfg.Pool.List()
	banned := 0
	for _, st := range accounts {
		if st.Disabled && pool.IsUpstreamBannedReason(st.DisabledReason) {
			banned++
		}
	}
	// 两类企业相关凭证，展示口径不同（2026-10-23 实测修正后）：
	//   - enterprise_switch：JWT 带 token_source=enterprise_switch 但**未绑定租户**
	//     （enterprise_id 空）。它只是登录入口标记，实测仍正常计分 —— 前端**不渲染**
	//     徽标（一枚对"能否得分"零判断力的标签只会误导，2026-10-23 已撤）；
	//   - enterprise_session：真正绑定企业租户 / 企业版 → 行为事件被上游静默丢弃，
	//     任务动作会被拦截，前端标红「企业会话·不计分」。
	switchUIDs := make([]string, 0, 4)
	sessionUIDs := make([]string, 0, 4)
	for _, st := range accounts {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		switch {
		case a.EnterpriseSwitchOnly():
			switchUIDs = append(switchUIDs, st.UID)
		case a.IsEnterpriseSession() && !a.IsEnterprise():
			sessionUIDs = append(sessionUIDs, st.UID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         p.cfg.Version,
		"uptime_sec":      int(time.Since(p.started).Seconds()),
		"auth_required":   p.apiKey() != "",
		"redis_mode":      p.cfg.RedisMode,
		"sticky_sessions": sticky,
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		// banned 是 disabled 的子集：其中因上游 11140 等硬处置封停、面板可一键剔除的。
		// 前端据此禁用/标注「剔除封禁号」按钮；两类分开是因为人工禁用与 14017
		// 试用未激活都还能救，混在一起剔会误伤。
		"banned":         banned,
		"paused":         paused,
		"in_flight_full": inFlightFull,
		"accounts":       accounts,
		// 企业相关凭证清单（uid）+ 计数：只影响展示，不改变选号/执行口径。
		//   enterprise_switch  → 仅标记（实测可计分；前端已不渲染徽标）
		//   enterprise_session → 真拦截（不参与成长任务，红标）
		"enterprise_switch":        switchUIDs,
		"enterprise_switch_count":  len(switchUIDs),
		"enterprise_session":       sessionUIDs,
		"enterprise_session_count": len(sessionUIDs),
		// model_locks 模型级限流全清单（哪些模型不能用、锁了几个号、还要锁多久）：
		// 与 accounts 的账号池视图互补，前端「模型锁池」表直接渲染。无锁时为 null。
		"model_locks": p.cfg.Pool.ModelLockView(),
		// 到期提醒快照的节奏与年龄：界面轮询是 5 秒级，但卡片数据只随余额后台刷新
		// 更新（默认每 5 分钟）——把这两个数透出，前端才能如实标注「x 分钟前更新」。
		"balance_refresh_sec":  p.balanceRefreshSec(),
		"balance_refreshed_at": p.balanceRefreshedAt(),
	})
}

// balanceRefreshSec 余额后台刷新间隔（秒）；无调度器/未开启时为 0。
func (p *Panel) balanceRefreshSec() int {
	if p.cfg.Scheduler == nil {
		return 0
	}
	return int(p.cfg.Scheduler.BalanceInterval() / time.Second)
}

// balanceRefreshedAt 最近一次后台余额刷新完成时刻；无调度器或尚未跑过为零值。
func (p *Panel) balanceRefreshedAt() time.Time {
	if p.cfg.Scheduler == nil {
		return time.Time{}
	}
	return p.cfg.Scheduler.BalanceRefreshedAt()
}

// logsHandler 返回日志环形缓冲快照（时间升序，含频道标记 chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// requestMetrics 返回进程内请求指标、最近 100 条与归档状态。
func (p *Panel) requestMetrics(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.RequestLog.Snapshot())
}

// requestLogs 从 JSONL 归档读取最近请求；limit 默认 200、最大 1000。
// 支持按 outcome/account/model/client_ip/user_agent 过滤（字符串字段为包含匹配）
// 与 from/to 时间区间（闭区间，unix 秒或 RFC3339）——面板「运行日志」的筛选框、
// 来源查询与「今天 / 自定义区间」都走这里。
func (p *Panel) requestLogs(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	q := r.URL.Query()
	rows, err := p.cfg.RequestLog.ReadArchive(limit, reqlog.Filter{
		Outcome:   q.Get("outcome"),
		Account:   q.Get("account"),
		Model:     q.Get("model"),
		ClientIP:  q.Get("client_ip"),
		UserAgent: q.Get("user_agent"),
		From:      parseTimeParam(q.Get("from")),
		To:        parseTimeParam(q.Get("to")),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 空结果回 []（而不是 JSON null）：前端把 null 与"归档关闭"混在一起会走错分支，
	// 显示成不满足筛选条件的最近请求。
	if rows == nil {
		rows = []reqlog.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "limit": limit})
}

// creditHistory 返回积分变动流水（新的在前），供「运行日志 → 积分历史」展示。
// limit 默认 200、上限 1000（与 requestLogs 同口径）；可带 uid 精确过滤。
// 账号昵称在读取时用池快照填充——账本只存 uid，上游改名后旧流水也跟着更新，
// 且留痕路径不必碰 auth 的昵称锁。
func (p *Panel) creditHistory(w http.ResponseWriter, r *http.Request) {
	if p.cfg.CreditHistory == nil {
		writeErr(w, http.StatusNotImplemented, "credit history not available")
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	uid := r.URL.Query().Get("uid")
	nicks := p.nicknames()
	// 有 uid 过滤时必须先全量取回（账本上限默认 2000 条）：先截断再过滤会让
	// 筛选后的条数看起来像历史缺失。无过滤时只取 limit 条，不为一次展示拷贝整个账本。
	readLimit := limit
	if uid != "" {
		readLimit = 0
	}
	all := p.cfg.CreditHistory.Read(readLimit)
	out := make([]map[string]any, 0, limit)
	for _, e := range all {
		if uid != "" && e.UID != uid {
			continue
		}
		out = append(out, map[string]any{
			"time":    e.Time,
			"uid":     e.UID,
			"account": nicks[e.UID],
			"delta":   e.Delta,
			"before":  e.Before,
			"after":   e.After,
		})
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "limit": limit})
}

// nicknames 池内 uid → 昵称映射（仅展示用，不含任何凭证）。
func (p *Panel) nicknames() map[string]string {
	nicks := map[string]string{}
	if p.cfg.Pool == nil {
		return nicks
	}
	for _, s := range p.cfg.Pool.List() {
		if s.Nickname != "" {
			nicks[s.UID] = s.Nickname
		}
	}
	return nicks
}

// models 实时查询上游模型列表与 reasoning 实际档位（直连上游，不读路由层 1h 缓存）：
// 回答"该模型到底支持哪几档思考"。顺带刷新 client 的 effort 降级能力缓存。
// 与 /v1/models 同口径的双域输出：CN 域模型加 "cn:" 前缀、global 域加 "global:" 前缀
// （gateway 路由协议，前端显示的 id 就是调用时要填的完整 model 值）。
// 各域独立探测、独立容错：某域无可用账号则整域跳过；两域全空时才报错
// （有错误明细回 502，一个账号都没有回 503）。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0)
	var fetchErrs []string
	hasAccounts := false

	// CN 域：有可用 CN 账号才查（此前无条件 Pool.Pick()+FetchModels——选中 global
	// 账号时打 CN 端点必然失败，混合池表现为偶发 502，纯 global 池必炸）。
	if uids := p.cfg.Pool.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		hasAccounts = true
		if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
			infos, err := p.cfg.Upstream.FetchModels(acct)
			if err != nil {
				fetchErrs = append(fetchErrs, "cn: "+err.Error())
			} else {
				for _, mi := range infos {
					out = append(out, panelModelEntry("cn", mi, mi.Efforts, mi.DefaultEffort, p.cfg.Upstream.HTTP))
				}
			}
		}
		out = appendPanelMediaModels(out, "cn", p.cfg.Upstream.MediaModels("cn"), p.cfg.Upstream.HTTP)
	}

	// global 域：路由开关开且有可用 global 账号才查（独立目录端点，FetchGlobalModelInfos；
	// Upstream.GlobalEnabled 是探测侧同一道闸，与 main 装配的 config global.enabled 一致）。
	if p.cfg.Upstream.GlobalEnabled {
		if uids := p.cfg.Pool.AvailableUIDsForRealm("global"); len(uids) > 0 {
			hasAccounts = true
			if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
				infos := p.cfg.Upstream.FetchGlobalModelInfos(acct)
				if len(infos) == 0 {
					fetchErrs = append(fetchErrs, "global: 上游未返回可用模型")
				} else {
					efforts, defaults := p.cfg.Upstream.GlobalEffortSnapshot()
					for _, mi := range infos {
						out = append(out, panelModelEntry("global", mi, efforts[mi.ID], defaults[mi.ID], p.cfg.Upstream.HTTP))
					}
				}
			}
			out = appendPanelMediaModels(out, "global", p.cfg.Upstream.MediaModels("global"), p.cfg.Upstream.HTTP)
		}
	}

	if hasAccounts {
		out = append(out, panelToolEntries()...)
	}
	if len(out) == 0 {
		if len(fetchErrs) > 0 {
			writeErr(w, http.StatusBadGateway, "fetch models: "+strings.Join(fetchErrs, "; "))
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "没有可用账号：请先在面板添加账号再查询")
		return
	}
	resp := map[string]any{"ok": true, "models": out}
	if len(fetchErrs) > 0 {
		resp["warnings"] = fetchErrs
	}
	writeJSON(w, http.StatusOK, resp)
}

// appendPanelMediaModels 把与聊天目录同次探测缓存的图像/视频模型附到面板目录。
// 面板此前只渲染 chat 条目，导致已支持的媒体能力在管理页完全不可见。
func appendPanelMediaModels(out []map[string]any, realm string, infos []upstream.ModelInfo, httpc *http.Client) []map[string]any {
	for _, mi := range infos {
		if mi.ID == "" || (mi.Type != "image" && mi.Type != "video") {
			continue
		}
		entry := panelModelEntry(realm, mi, nil, "", httpc)
		entry["type"] = mi.Type
		if mi.Type == "image" {
			entry["endpoint"] = "/v1/images/generations"
			entry["edits_endpoint"] = "/v1/images/edits"
		} else {
			entry["endpoint"] = "/v1/videos/generations"
			entry["poll_endpoint"] = "/v1/videos/{id}"
		}
		out = append(out, entry)
	}
	return out
}

// panelToolEntries 搜索与网页抓取不是聊天模型，但同样是可调用能力；
// 用独立 type 透出，前端据此生成正确的调试请求，而不是伪装成 chat model。
func panelToolEntries() []map[string]any {
	return []map[string]any{
		{
			"id": "workbuddy:web-search", "name": "联网搜索", "type": "search",
			"description": "调用 WorkBuddy 搜索工具，返回 Anthropic Messages 兼容结果",
			"tags":        []string{"搜索", "工具"}, "endpoint": "/v1/search/messages",
		},
		{
			"id": "workbuddy:web-fetch", "name": "网页抓取", "type": "fetch",
			"description": "抓取指定网页并返回标题与 Markdown 正文",
			"tags":        []string{"网页", "抓取", "工具"}, "endpoint": "/v1/fetch",
		},
	}
}

// panelModelEntry 构造单个模型条目（两域共用）：id 带 realm 前缀（调用值即显示值），
// context_length / max_output_tokens 走四级查找链，effort 档位按 realm 域取
// EffortListing（远端权威 ∪ 静态兜底表）——与 /v1/models 同一口径，两侧不再漂移。
func panelModelEntry(realm string, mi upstream.ModelInfo, remoteEfforts []string, remoteDefault string, httpc *http.Client) map[string]any {
	entry := map[string]any{
		"id":                   realm + ":" + mi.ID,
		"type":                 "chat",
		"endpoint":             "/v1/chat/completions",
		"name":                 mi.Name,
		"default_effort":       mi.DefaultEffort,
		"supported_efforts":    mi.Efforts,
		"can_disable_thinking": mi.CanDisableThinking,
		"supports_reasoning":   mi.SupportsReasoning,
		"supports_images":      mi.SupportsImages,
		"credits":              mi.Credits,
		"description":          mi.Description,
		"tags":                 mi.Tags,
		"vendor":               mi.Vendor,
		"is_default":           mi.IsDefault,
		"supports_tool_call":   mi.SupportsToolCall,
		"only_reasoning":       mi.OnlyReasoning,
		"reasoning_effort":     mi.ReasoningEffort,
		"reasoning_summary":    mi.ReasoningSummary,
	}
	// 限时优惠（modelPromotions）：credits 是牌价，promo_* 是当前生效折扣
	//（WorkBuddy 客户端显示的就是这个生效价）。前端据此显示「生效价+标签+划线牌价」。
	if mi.PromoFactor != nil {
		entry["promo_factor"] = *mi.PromoFactor
		entry["promo_credits"] = mi.PromoCredits
	}
	if mi.PromoLabel != "" {
		entry["promo_label"] = mi.PromoLabel
	}
	if mi.PromoNote != "" {
		entry["promo_note"] = mi.PromoNote
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, httpc)
	if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, httpc); ok {
		entry["max_output_tokens"] = mo
	}
	if efforts, def := upstream.EffortListing(realm, mi.ID, remoteEfforts, remoteDefault); efforts != nil {
		entry["supported_efforts"] = efforts
		if def != "" {
			entry["default_effort"] = def
		}
	}
	return entry
}

// modelProbes 返回模型输出上限的探测结果（scripts/probe_max_tokens.py --panel-out
// 写入的契约文件），供前端在「模型与档位」的实测列做风险标注。
//
// 设计边界：纯只读透传——文件缺失/未配置返回空集（面板退化为无标注，与历史行为
// 一致），网关自身不解析字段语义、不据此做任何路由或出站决策；上游改了限制后
// 重跑一次工具、下次查询即刷新，无需重启网关。
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())
		return
	}
	var f struct {
		Version int                        `json:"version"`
		Probes  map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 手动复活：清禁用 + 冷却 + 熔断（运维口径无条件恢复）。
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Revive(uid)
	log.Printf("panel: revive uid=%s（人工清除禁用/冷却/熔断）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountDisable 人工禁用（不再参与选号，需面板 revive 或重登恢复）。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "manual disable (panel)")
	log.Printf("panel: disable uid=%s（人工禁用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountPause 暂停选号：账号退出选号候选，但**照常参与**签到 / 活跃上报 / 保活 /
// 余额刷新。与 disable 的区别：不写 reason、不清冷却域、不重置计数——账号是「临时
// 让位」而非「判死」，点「恢复选号」即可立刻回到池子（无需重登或解冻）。
func (p *Panel) accountPause(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Pause(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: pause uid=%s（暂停选号，保号任务照常）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountResume 解除暂停选号（幂等，对未暂停账号为空操作）。
func (p *Panel) accountResume(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Resume(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: resume uid=%s（恢复参与选号）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	checkinMsg := ""
	checkinDone := false
	// 企业版无签到体系（上游 400 code 10001「企业账号不支持该操作」）：不发起该调用，
	// 端点退化为「刷新额度」——企业额度走 get-enterprise-user-usage 口径（见 upstream）。
	// 前端对企业号不渲染「签到」按钮；此处是 API 侧防御（外部脚本/旧缓存前端仍可能调用）。
	if a.IsEnterprise() {
		checkinMsg = "企业账号无签到体系（已跳过签到，仅刷新额度）"
	} else if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error() // "今天已签到"等业务错误照常查余额
		// 幂等拒绝同样是「今日已签」，标记后按钮显示「已签」。
		if upstream.IsAlreadyCheckin(err) {
			p.cfg.Pool.NoteCheckinDone(uid)
			checkinDone = true
		}
	} else {
		p.cfg.Pool.NoteCheckinDone(uid)
		checkinDone = true
	}
	resp := map[string]any{"ok": true, "checkin_done": checkinDone}
	if a.IsEnterprise() {
		resp["enterprise"] = true
	}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	remain, total, expiring, earliestAt, earliestRemaining, buckets, err := p.cfg.Upstream.UserResourceSnapshot(a, p.expiringSoonWindow())
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	p.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{
		Remain:            remain,
		Total:             total,
		Expiring:          expiring,
		EarliestAt:        earliestAt,
		EarliestRemaining: earliestRemaining,
		ExpiryBuckets:     poolExpiryBuckets(buckets),
	})
	resp["credits"] = remain
	resp["credits_total"] = total
	log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", uid, checkinMsg, remain, total)
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：更新余额与到期快照，不触碰冷却状态。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	remain, total, expiring, earliestAt, earliestRemaining, buckets, err := p.cfg.Upstream.UserResourceSnapshot(a, p.expiringSoonWindow())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{
		Remain:            remain,
		Total:             total,
		Expiring:          expiring,
		EarliestAt:        earliestAt,
		EarliestRemaining: earliestRemaining,
		ExpiryBuckets:     poolExpiryBuckets(buckets),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": remain, "credits_total": total})
}

// accountRemove 移除账号：先出池（立即落盘 state），再删 auth 文件。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.Remove(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	fileMsg := ""
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
	}
	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth 文件删除失败: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})
		return
	}
	log.Printf("panel: remove uid=%s（已出池并删除凭证文件）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 批量剔除封禁号
// ---------------------------------------------------------------------------

// pruneBannedRequest 一键剔除「上游封禁」账号的请求体。
//
// dry_run 用指针而不是 bool：接口会删凭证，默认必须是安全的那一侧。空 body、
// 非法 JSON、字段缺省一律落到「只读预演」，只有显式 "dry_run": false 才真删。
//
// uids 是前端确认框里展示给运维的那批账号（预演结果原样回传）。带上它能把
// 「看到 3 个 → 点下去删了 99 个」这类 TOCTOU 差异压到零：真删时只动这批 uid，
// 期间新被封的号留到下一次剔除。为空时退化为「处理当前全部命中账号」，供
// curl / 脚本使用。
type pruneBannedRequest struct {
	DryRun *bool    `json:"dry_run"`
	UIDs   []string `json:"uids"`
}

// pruneBannedRow 预演/执行结果的单行（前端确认框直接渲染）。
type pruneBannedRow struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Reason   string `json:"reason"`
	Credits  int64  `json:"credits"`
}

// accountPruneBanned 一键剔除被封号（上游 11140 request illegal 等硬禁用）。
//
// 语义：
//   - 命中口径只有 pool.IsUpstreamBannedReason：人工禁用（manual disable）、
//     14017 试用未激活、积分/软冷却一律不动——那些都还能救；
//   - 出池走 Pool.Remove（与单号「移除」同一条路径：立即落盘 state，在途请求的
//     Release 对已删条目是 no-op）；
//   - 凭证文件**不删**，移到 <auth_dir 父目录>/auths-pruned/<YYYY-MM-DD>/。
//     被封号仍有申诉价值（上游解除后原凭证可再用），删了就真没了；该目录与
//     auth_dir 并列而非其子目录，确保 auth.LoadDir 的 auth_dir/workbuddy*.json
//     glob 永远扫不到归档，重启不会把刚剔掉的号又载回池子。
func (p *Panel) accountPruneBanned(w http.ResponseWriter, r *http.Request) {
	var req pruneBannedRequest
	// 解析失败按零值处理（= 预演）：这个接口的默认动作必须是不可逆操作的反面。
	_ = json.NewDecoder(r.Body).Decode(&req)
	dryRun := req.DryRun == nil || *req.DryRun

	banned := p.cfg.Pool.Banned()
	if len(req.UIDs) > 0 {
		want := make(map[string]bool, len(req.UIDs))
		for _, u := range req.UIDs {
			if u = strings.TrimSpace(u); u != "" {
				want[u] = true
			}
		}
		// 只认「既在确认清单里、此刻仍被封」的交集：确认后已被人手工解冻/移除的
		// 账号自动出列，不会因为清单过期而误删。
		sel := make([]pool.Status, 0, len(want))
		for _, st := range banned {
			if want[st.UID] {
				sel = append(sel, st)
			}
		}
		banned = sel
	}

	rows := make([]pruneBannedRow, 0, len(banned))
	var frozen int64
	for _, st := range banned {
		rows = append(rows, pruneBannedRow{
			UID:      st.UID,
			Nickname: st.Nickname,
			Reason:   st.DisabledReason,
			Credits:  st.Credits,
		})
		frozen += st.Credits
	}
	archiveDir := p.pruneArchiveDir()

	if dryRun {
		// 预演不写任何状态：只回清单，供前端弹确认框（账号数 / 冻结积分 / 归档位置）。
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"dry_run":        true,
			"count":          len(rows),
			"frozen_credits": frozen,
			"archive_dir":    archiveDir,
			"accounts":       rows,
		})
		return
	}

	removed := 0
	archived := 0
	failed := make([]map[string]string, 0)
	for _, st := range banned {
		a := p.cfg.Pool.Remove(st.UID)
		if a == nil {
			continue // 已被并发移除/剔除（另一页签、脚本），跳过而非报错
		}
		removed++
		if a.FilePath == "" {
			continue // 纯内存态账号：没有凭证文件可归档
		}
		if _, err := archiveAuthFile(a.FilePath, archiveDir); err != nil {
			// 出池已生效，只是归档失败（权限/磁盘）。如实报出来，凭证文件仍在原位，
			// 重启时会被 LoadDir 重新载回池子——运维需要知道这件事并手工处理。
			failed = append(failed, map[string]string{"uid": st.UID, "error": err.Error()})
			continue
		}
		archived++
	}
	log.Printf("panel: prune_banned 剔除 %d 个上游封禁号（归档凭证 %d，失败 %d，归档目录 %s）",
		removed, archived, len(failed), archiveDir)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"dry_run":     false,
		"removed":     removed,
		"archived":    archived,
		"failed":      failed,
		"archive_dir": archiveDir,
	})
}

// pruneArchiveDir 封禁号凭证的归档目录：<auth_dir 父目录>/auths-pruned/<日期>。
// auth_dir 为空（未配置）时返回空串，由 archiveAuthFile 就近推导。
func (p *Panel) pruneArchiveDir() string {
	dir := p.cfg.AuthDir
	if dir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dir), "auths-pruned", time.Now().Format("2006-01-02"))
}

// archiveAuthFile 把凭证文件移入归档目录（同分区 rename；跨分区退化为一读一写）。
func archiveAuthFile(src, dir string) (string, error) {
	if dir == "" {
		dir = filepath.Join(filepath.Dir(src), "auths-pruned", time.Now().Format("2006-01-02"))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, filepath.Base(src))
	if err := os.Rename(src, dst); err == nil {
		return dst, nil
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Remove(src); err != nil {
		return dst, err // 已复制但旧文件还在：报错让人工清理，不谎报成功
	}
	return dst, nil
}

// ---------------------------------------------------------------------------
// 批量任务
// ---------------------------------------------------------------------------

// checkinAll 手动触发全量签到（异步执行，进度看日志区/账号状态变化）。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunCheckinNow()
	log.Printf("panel: 手动全量签到已触发（含猫猫旅行）")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// travelAll 手动触发全量猫猫旅行巡检（异步执行）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunTravelNow()
	log.Printf("panel: 手动全量旅行巡检已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// activityAll 手动触发全量活跃上报（异步执行；点亮连登 + 解锁领养前置）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunActivityNow()
	log.Printf("panel: 手动全量活跃上报已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// keepaliveAll 手动触发全量 token 保活（异步执行）。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunKeepaliveNow()
	log.Printf("panel: 手动全量保活已触发")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// balanceAll 手动全量刷新余额：并发查上游、写回池内 credits（含解冻语义），
// 完成后返回——面板紧接着拉 overview 即是最新值。账号量小（个位数），
// 同步等待（上限受短 RPC 超时约束）比"触发后盲刷"体验更确定。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	p.cfg.Scheduler.RunBalanceRefreshNow()
	p.syncNicknames()
	log.Printf("panel: 手动全量余额刷新完成")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": p.cfg.Pool.List()})
}

// compensation 查询全部账号的补偿活动状态（**只读**，不发起任何领取）。
//
// 与 balanceAll 同构：同步等待而非「触发后盲刷」——账号量小（个位数），
// 面板拿到响应即可直接渲染横幅，不用再轮询。
func (p *Panel) compensation(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"accounts": p.cfg.Scheduler.CompensationStatusAll(),
	})
}

// accountCompensationClaim 单号领取补偿：查状态 → 待领才领（幂等）。
// 领完顺带刷一次余额，面板上的积分数字立即反映到账。
func (p *Panel) accountCompensationClaim(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	uid := r.PathValue("uid")
	res, err := p.cfg.Scheduler.ClaimCompensationFor(uid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if res.Claimed {
		p.refreshCreditsQuiet(uid)
	}
	// ok 反映「是否拿到确定结论」：error 非空即 false。注意前端 api() 只看
	// HTTP 状态码，这个字段是给日志/调用方看的，真正的分流依据是 result.error。
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.Error == "", "result": res})
}

// compensationAll 手动全量领取补偿（逐号查状态后领取；无待领的号静默跳过）。
func (p *Panel) compensationAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	results := p.cfg.Scheduler.ClaimCompensationNow()
	for _, res := range results {
		if res.Claimed {
			p.refreshCreditsQuiet(res.UID)
		}
	}
	summary := scheduler.SummarizeCompensation(results)
	log.Printf("panel: 手动全量补偿领取完成 合计=%d 领取=%d(+%dc) 跳过=%d 失败=%d",
		summary.Total, summary.Claimed, summary.Credit, summary.Skipped, summary.Errored)
	// ok=false 表示有账号没拿到确定结论；前端据此提示「查询失败」而不是
	// 「没有待领补偿」——两者语义完全相反，不能混。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      summary.OK(),
		"summary": summary,
		"results": results,
	})
}

// b2i bool → int（汇总计数用）。
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// refreshCreditsQuiet 领取成功后刷新该号余额，失败不影响领取结果回传。
//
// 失败只记日志不返回：领取本身已经成功并落到上游，余额刷新失败只是展示滞后，
// 前端紧接着还会拉一次 overview。静默吞掉会让「积分没变」看起来像没领到。
func (p *Panel) refreshCreditsQuiet(uid string) {
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return
	}
	remain, total, expiring, earliestAt, earliestRemaining, err :=
		p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
	if err != nil {
		log.Printf("panel: compensation 后刷新余额失败 uid=%s: %v", uid, err)
		return
	}
	p.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
}

// syncNicknames 手动刷新时的昵称同步（issue #94：上游改名免重登）。
// 只在面板手动「刷新」路径调用——后台余额定时器不触发（用户明确要求资料接口
// 仅手动触达）。逐号拉 /console/account，只取 nickname（手机号等敏感字段在
// upstream.FetchAccountProfile 内即被丢弃）；单号失败静默跳过，不打断余额刷新
// 的既有结果。
func (p *Panel) syncNicknames() {
	type job struct {
		uid string
		a   *auth.Auth
	}
	var jobs []job
	for _, st := range p.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil && a.AccessTokenValue() != "" {
			jobs = append(jobs, job{uid: st.UID, a: a})
		}
	}
	if len(jobs) == 0 {
		return
	}
	var (
		mu      sync.Mutex
		updated int
		failed  int
		sem     = make(chan struct{}, 3)
		wg      sync.WaitGroup
	)
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			nick, err := p.cfg.Upstream.FetchAccountProfile(j.a)
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			if p.cfg.Pool.SetNickname(j.uid, nick) {
				mu.Lock()
				updated++
				mu.Unlock()
			}
		}(j)
	}
	wg.Wait()
	if updated > 0 || failed > 0 {
		log.Printf("panel: 昵称同步：更新 %d 个，失败 %d 个（未变化不计数）", updated, failed)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// usage 返回逐请求用量聚合。统计窗口三选一：
//   - from/to（unix 秒）：显式区间，用于「今天」与「自定义」——区间由浏览器按
//     本地时区算好再发，服务端时区与浏览器不一致时「今天」才不会被算错；
//   - hours：滚动窗口（默认 72，上限 1440=60 天），卡片汇总/按域/按账号/按模型/
//     时序**全部**按该窗口统计；显式 hours=0 表示全部历史（含 90 天前折叠出的日桶）；
//   - 都不给：等同于 hours=72（保持旧调用方行为）。
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	q := r.URL.Query()
	win := usage.Window{}
	win.From = parseTimeParam(q.Get("from"))
	win.To = parseTimeParam(q.Get("to"))
	if win.From.IsZero() && win.To.IsZero() {
		win.Hours = 72
		if v := q.Get("hours"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				win.Hours = n
			}
		}
		if win.Hours > 1440 {
			win.Hours = 1440
		}
	}
	// 昵称仅用于展示，取自池快照（不含任何凭证）。
	nicks := p.nicknames()
	var currentRate func(realm, model string) string
	if p.cfg.Upstream != nil {
		currentRate = p.cfg.Upstream.ModelRate
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.SnapshotWindow(win, nicks, currentRate))
}

// parseTimeParam 解析时间查询参数：unix 秒（前端默认）或 RFC3339（便于手工调
// 接口/写脚本）。空串与非法值都返回零值 = 该侧不设界，不报错——区间参数是可选
// 增强，拼错一个 from 不该让整页用量打不开。
func parseTimeParam(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}
		}
		// 兼容秒与毫秒（前端可能直接把 Date.now() 传上来）。
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", v, time.Local); err == nil {
		return t
	}
	return time.Time{}
}

// usageSave 立即把内存中的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// packages 返回全部账号的积分包构成，供「积分构成」视图对比。
//
// 逐个账号向上游查（并发有上限，避免瞬时打满上游限流），失败只在对应账号上
// 标 error，不影响其它账号——一个号 token 失效不该让整页空白。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID      string                   `json:"uid"`
		Nickname string                   `json:"nickname"`
		Realm    string                   `json:"realm"`
		Remain   int64                    `json:"remain"`
		Size     int64                    `json:"size"`
		Packages []upstream.CreditPackage `json:"packages"`
		Error    string                   `json:"error,omitempty"`
	}
	out := make([]row, len(accts))

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, s := range accts {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			it := row{UID: s.UID, Nickname: s.Nickname, Realm: s.Realm}
			a := p.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				it.Error = "account not loaded"
				out[i] = it
				return
			}
			packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
			if err != nil {
				it.Error = err.Error()
				out[i] = it
				return
			}
			it.Packages = packs
			it.Remain = remain
			it.Size = size
			out[i] = it
		}(i, s)
	}
	wg.Wait()

	// 余额降序：多的在前，便于和少的对比。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
