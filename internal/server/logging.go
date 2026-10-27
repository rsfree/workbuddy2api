// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// maxUserAgentLen 归档与面板展示保留的 UA 字节上限。UA 是客户端完全可控的
// 自由文本（浏览器动辄 150+ 字符，恶意客户端可以塞几 KB），落盘前必须截断，
// 否则一条请求就能把归档行撑大。截断只影响展示，不影响请求处理。
const maxUserAgentLen = 200

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatLogOut 聊天表格日志的输出目标。生产默认 os.Stdout；main 在启用管理面板时
// 经 SetChatLogOutput 注入 MultiWriter，把每行镜像进 /panel/api/logs 的环形缓冲，
// stdout 行为不变。需在开始服务前调用一次（无并发竞争窗口）。
var chatLogOut io.Writer = os.Stdout

// SetChatLogOutput 替换聊天表格日志输出目标（仅 main 启动期调用一次）。
func SetChatLogOutput(w io.Writer) { chatLogOut = w }

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start            time.Time
	model            string
	mode             string // "stream" | "sync"
	uid              string // 完整 uid，展示时只取前 8 位
	nick             string // 账号昵称（随选号同步），流水行经 logfmt.Label 拼成 "昵称(uid8)"
	ttfb             time.Duration
	toks             int // <0 表示 usage 缺失 → 显示 "-"
	status           int
	requestID        string
	outcome          string
	attempts         int
	credit           float64
	hasCredit        bool
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	// 缓存命中观测（usage.prompt_cache_hit_tokens / miss）：上游给到才有效。
	// 供 usage 桶命中率维度与 reqlog 逐次记录；缺失时 hasCache=false 不参与统计。
	cacheHit  int64
	cacheMiss int64
	hasCache  bool

	// egress 本次请求的出口标签（"154.40.45.140" / "proxy:host" / "direct"）。
	// 选号后由 upstream.EgressLabel 取值：与调用来源是**相反方向**，回答
	// 「这个请求从哪个 IP 出去」。空 = 未采集（非 chat 路径 / 上游不可用）。
	egress string
	// 调用来源（客户端 IP / User-Agent）。空 = 未采集（logging.request_client_info
	// 关闭，或非 chat 路径），展示层一律以 "-" 兜底。
	clientIP  string
	userAgent string
	// sticky 本轮粘性落点（reqlog.Sticky*）；空 = 无会话键，不参与换号率统计。
	sticky string
	// affinity 本轮前缀亲和落点（reqlog.Affinity*）；空 = 会话已有绑定（亲和不参与）
	// 或该请求没有可用的前缀指纹。
	affinity string
	// prefixBytes 本次请求「可共享前缀」的材料字节数（system + tools 名字/描述），
	// 由 session.ExtractKeysSized 一次解析顺带算出。0 = 无前缀成分。
	// 与 promptTokens 一起换算成「前缀占 prompt 的比例」，用来判断亲和到底有多少
	// 可省空间——比例很小的时候，亲和命中率再高也省不下钱（见 reqlog.CacheMetrics）。
	prefixBytes int
	// choiceNone 入站在声明 tool_choice=none（见 upstream.ToolChoiceNone）。
	// 这类请求必然被剥离 tools，prefix_bytes 与 prompt 的背离是设计使然，
	// 不该计入「前缀丢失」告警——否则每个纯文本会话都会刷一条 WARN。
	choiceNone bool
	// choiceRequired 入站在声明 tool_choice=required / any（见 upstream.ToolChoiceRequired）。
	choiceRequired bool
	// toolCalls 本回合实际产出的结构化 tool_calls 数（**含标记修复还原的**）。
	// 与 choiceRequired 组合成「上游是否真解析 tool_choice」的探针。
	toolCalls int
	// toolsDeclared / toolHistory 入站工具上下文（见 upstream.InspectTools）。
	// 两者 + toolCalls 构成「模型跳过工具」的三元判据，供用量桶长期统计。
	toolsDeclared int
	toolHistory   bool

	logged bool
}

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done 幂等落一行表格日志。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRowEx(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.nick, s.status, s.toks,
		s.requestID, s.outcome, s.attempts, s.credit, s.hasCredit, s.egress, s.clientIP, s.userAgent)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br                  *bufio.Reader
	start               time.Time
	ttfb                time.Duration
	seen                bool // 已见过首个 data 帧（TTFB 只记一次）
	promptTokens        int
	completionTokens    int
	totalTokens         int
	hasPromptTokens     bool
	hasCompletionTokens bool
	hasTotalTokens      bool
	// credit 上游末帧 usage.credit（本次真实扣费积分），供成本台账（NoteModelCost）。
	hasCredit  bool
	credit     float64
	errorFrame bool
	// cacheHit/cacheMiss 上游末帧 usage.prompt_cache_hit_tokens / miss_tokens，
	// 供用量桶的命中率维度与 reqlog 逐次记录（issue #92）。
	hasCacheHit  bool
	cacheHit     int
	cacheMiss    int
	hasCacheMiss bool
	// toolCalls 本回合出现过的结构化 tool_calls 数量（只计数，不保留内容）。
	// 与入站 tool_choice=required 一起构成「上游是否真解析了该字段」的探针：
	// 要求必调却一个都没有，说明字段被忽略或模型未遵从，两种都值得告警。
	toolCalls int
	pend      []byte // 已读未返回的行缓存
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.completionTokens, s.hasCompletionTokens }

// ToolCalls 返回本回合观测到的结构化 tool_calls 数量。
func (s *chatStatsReader) ToolCalls() int { return s.toolCalls }

// Credit 返回末帧 usage.credit（本次真实扣费积分）与是否缺失。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasCredit }

// TotalTokens 返回末帧 usage.total_tokens 与是否缺失。
func (s *chatStatsReader) TotalTokens() (int, bool) { return s.totalTokens, s.hasTotalTokens }

// Usage 返回流式响应中已收到的 token usage 字段。
func (s *chatStatsReader) Usage() pool.TokenUsageDelta {
	return pool.TokenUsageDelta{
		HasPromptTokens:     s.hasPromptTokens,
		PromptTokens:        int64(s.promptTokens),
		HasCompletionTokens: s.hasCompletionTokens,
		CompletionTokens:    int64(s.completionTokens),
		HasTotalTokens:      s.hasTotalTokens,
		TotalTokens:         int64(s.totalTokens),
	}
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Error json.RawMessage `json:"error"`
		// Choices 只为数 tool_calls：与 usage 共用这一次 Unmarshal，不额外解析。
		// delta 覆盖流式分片，message 覆盖非标准的上游整条帧（两种形态都实测出现过）。
		Choices []struct {
			Delta struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
			Message struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens         *int     `json:"prompt_tokens"`
			CompletionTokens     *int     `json:"completion_tokens"`
			TotalTokens          *int     `json:"total_tokens"`
			Credit               *float64 `json:"credit"`
			PromptCacheHitTokens *int     `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTok   *int     `json:"prompt_cache_miss_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}
	// tool_calls 必须在 usage 判空之前数：绝大多数 tool_call 帧不带 usage，
	// 放到后面数会漏掉全部。
	for _, c := range chunk.Choices {
		s.toolCalls += len(c.Delta.ToolCalls) + len(c.Message.ToolCalls)
	}
	if chunk.Usage == nil {
		if len(chunk.Error) > 0 {
			s.errorFrame = true
		}
		return
	}
	if len(chunk.Error) > 0 {
		s.errorFrame = true
	}
	if chunk.Usage.PromptTokens != nil {
		s.hasPromptTokens = true
		s.promptTokens = *chunk.Usage.PromptTokens
	}
	if chunk.Usage.CompletionTokens != nil {
		s.hasCompletionTokens = true
		s.completionTokens = *chunk.Usage.CompletionTokens
	}
	if chunk.Usage.TotalTokens != nil {
		s.hasTotalTokens = true
		s.totalTokens = *chunk.Usage.TotalTokens
	}
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
	if chunk.Usage.PromptCacheHitTokens != nil {
		s.hasCacheHit = true
		s.cacheHit = *chunk.Usage.PromptCacheHitTokens
	}
	if chunk.Usage.PromptCacheMissTok != nil {
		s.hasCacheMiss = true
		s.cacheMiss = *chunk.Usage.PromptCacheMissTok
	}
}

// CacheTokens 返回末帧 usage 的缓存命中 / 未命中 token 数。miss 缺失时按
// prompt - hit 推导；对称地，miss 显式存在而 hit 为 0 / 缺失时按 prompt - miss
// 反推 hit（issue #170：上游在「接近全命中」时可能回 hit=0 + miss=3 这类组合，
// 不反推会被误判为整段未命中）。两个方向都推导不出时 ok=false（不参与命中率
// 统计）。
func (s *chatStatsReader) CacheTokens() (hit, miss int64, ok bool) {
	if !s.hasCacheHit && !s.hasCacheMiss {
		return 0, 0, false
	}
	hit = int64(s.cacheHit)
	miss = int64(s.cacheMiss)
	if !s.hasCacheMiss {
		if !s.hasPromptTokens || s.promptTokens < s.cacheHit {
			return hit, 0, true
		}
		miss = int64(s.promptTokens - s.cacheHit)
	} else if s.cacheHit == 0 && s.hasPromptTokens && s.promptTokens > s.cacheMiss {
		// hit 为 0 / 缺失但 miss 可信（prompt > miss，即确有命中量）→ 反推。
		// prompt <= miss 时保持 hit=0，那是真实的整段未命中。
		hit = int64(s.promptTokens - s.cacheMiss)
	}
	return hit, miss, true
}

// SawErrorFrame 报告流中是否透传过 SSE error 帧。
func (s *chatStatsReader) SawErrorFrame() bool { return s.errorFrame }

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// cacheTokensFromUsage 从非流式聚合响应的 usage 里提取缓存命中 / 未命中 token
// 数，与流式侧 CacheTokens 的推导口径对称：hit 不可得（缺失或 0）但 miss 显式
// 存在时，按 prompt - miss 反推 hit（issue #170）；prompt <= miss 时保持 hit=0
// （真实的整段未命中）。两个方向都不可得时 ok=false。
func cacheTokensFromUsage(usage map[string]any) (hit, miss int64, ok bool) {
	h, okH := upstream.UsageCacheHitTokens(usage)
	m, okM := upstream.UsageCacheMissTokens(usage)
	switch {
	case okH:
		return int64(h), int64(m), true
	case okM:
		if p, okP := upstream.UsagePromptTokens(usage); okP && p > m {
			return int64(p - m), int64(m), true
		}
		return 0, int64(m), true
	}
	return 0, 0, false
}

// usageDeltaFromResponse 从非流式聚合响应中提取明确存在的 token 字段。
func usageDeltaFromResponse(resp map[string]any) pool.TokenUsageDelta {
	delta := pool.TokenUsageDelta{}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return delta
	}
	read := func(key string) (int64, bool) {
		v, ok := u[key]
		if !ok {
			return 0, false
		}
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case float32:
			return int64(n), true
		case int:
			return int64(n), true
		case int64:
			return n, true
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		default:
			return 0, false
		}
	}
	if n, ok := read("prompt_tokens"); ok {
		delta.HasPromptTokens, delta.PromptTokens = true, n
	}
	if n, ok := read("completion_tokens"); ok {
		delta.HasCompletionTokens, delta.CompletionTokens = true, n
	}
	if n, ok := read("total_tokens"); ok {
		delta.HasTotalTokens, delta.TotalTokens = true, n
	}
	return delta
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
//
// 实现委托 logfmt.UID8，避免 "截 8 位" 的规则在 server 与 logfmt 两处各写一份而走样。
func uidPrefix(uid string) string {
	return logfmt.UID8(uid)
}

type requestTraceKey struct{}

// requestTrace 在一次 chat 请求内共享标识与最终统计，ServeHTTP 出口统一记账。
type requestTrace struct {
	id    string
	start time.Time
	// path 实际请求路径（chat / search / search messages）。此前 Path 在 event() 里
	// 硬编码为 /v1/chat/completions，搜索接入后必须按真实路径记账。
	path string
	// account 本次请求实际使用的账号（搜索路径没有 chatStat，单独记录）。
	account string
	// model 非 chat 路径的模型标识（搜索写 "WorkBuddy搜索工具"）。chat 路径由
	// chatStat.model 覆盖，二者不会同时生效。
	model string
	stat  *chatStat
	// egress 非 chat 路径的出口标签；chat 路径由 chatStat.egress 覆盖。
	egress string
	// 调用来源，进入 handler 时一次性采集（见 ServeHTTP / captureClientInfo）。
	clientIP  string
	userAgent string
}

// captureClientInfo 采集调用来源（客户端 IP + 截断后的 UA）。开关关闭时保持空串：
// 来源信息比 token 计数敏感，是否落盘由 logging.request_client_info 决定。
func (t *requestTrace) captureClientInfo(r *http.Request) {
	if t == nil || r == nil {
		return
	}
	t.clientIP = clientIPForLog(r)
	t.userAgent = logfmt.Truncate(r.UserAgent(), maxUserAgentLen)
}

// clientIPForLog 提取用于日志展示的客户端 IP。
//
// 与 upstream.ExtractClientIP 的差别：后者只认代理头（X-Forwarded-For 首段 →
// X-Real-IP），因为它的用途是把客户端 IP **透传给上游**，回落到网关自身地址会
// 污染上游风控判据；日志场景相反——直连（无反代）时 RemoteAddr 就是唯一线索，
// 必须回落，否则面板里所有来源都显示 "-"。代理头优先保证反代后拿到真实客户端。
func clientIPForLog(r *http.Request) string {
	if r == nil {
		return ""
	}
	if ip := upstream.ExtractClientIP(r); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// dashIfEmpty 空串统一显示 "-"（来源字段未采集时不留空白列）。
func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func requestTraceFrom(r *http.Request) *requestTrace {
	if r == nil {
		return nil
	}
	return requestTraceFromContext(r.Context())
}

// requestTraceFromContext 从 context 取 trace。搜索路径在 upstream 层之外拿不到
// *http.Request（runSearch 只持有 ctx），故单独提供 context 版本。
func requestTraceFromContext(ctx context.Context) *requestTrace {
	if ctx == nil {
		return nil
	}
	tr, _ := ctx.Value(requestTraceKey{}).(*requestTrace)
	return tr
}

func (t *requestTrace) event(status int) reqlog.Event {
	path := t.path
	if path == "" {
		path = "/v1/chat/completions"
	}
	e := reqlog.Event{
		Time:      t.start,
		RequestID: t.id,
		Path:      path,
		Status:    status,
	}
	if t.account != "" {
		e.Account = t.account
	}
	if t.model != "" {
		e.Model = t.model
	}
	duration := time.Since(t.start)
	e.DurationMs = duration.Milliseconds()
	if e.DurationMs < 1 {
		e.DurationMs = 1
	}
	if t.stat != nil {
		s := t.stat
		e.Account = logfmt.Label(s.uid, s.nick)
		// chat 路径以 stat 的模型为准（覆盖 trace.model）。
		e.Model = s.model
		e.Outcome = s.outcome
		e.TTFBMs = s.ttfb.Milliseconds()
		e.Attempts = s.attempts
		e.PromptTokens = s.promptTokens
		e.CompletionTokens = s.completionTokens
		e.TotalTokens = s.totalTokens
		e.Credit = s.credit
		e.HasCredit = s.hasCredit
		e.CacheHitTokens = s.cacheHit
		e.CacheMissTokens = s.cacheMiss
		e.Egress = s.egress
		e.Sticky = s.sticky
		e.Affinity = s.affinity
		e.PrefixBytes = s.prefixBytes
	}
	if e.Egress == "" {
		e.Egress = t.egress
	}
	e.ClientIP = t.clientIP
	e.UserAgent = t.userAgent
	if e.Outcome == "" {
		if status >= 200 && status < 300 {
			e.Outcome = reqlog.OutcomeSuccess
		} else {
			e.Outcome = reqlog.OutcomeHTTPError
		}
	}
	e.OK = status >= 200 && status < 300 && e.Outcome == reqlog.OutcomeSuccess
	if fired, msg := prefixLostWarn.note(e.Model, e.PrefixBytes, e.PromptTokens,
		t.stat != nil && t.stat.choiceNone); fired {
		log.Printf("%s", msg)
	}
	if t.stat != nil {
		if fired, msg := requiredIgnoredWarn.note(e.Model, t.stat.choiceRequired, t.stat.toolCalls); fired {
			log.Printf("%s", msg)
		}
	}
	return e
}

// responseToolCallCount 数一个非流式聚合响应里的结构化 tool_calls（各 choice 求和）。
// 兼容 message 与 delta 两种摆放（上游聚合帧两种形态都出现过）。
//
// 🔴 两个切片类型都要认：`upstream.Aggregate` 用 `[]map[string]any` 装 tool_calls
// （sse.go 里 `calls := make([]map[string]any, 0, ...)`），而从 JSON 反序列化来的
// 响应是 `[]any`。只断言 []any 会让网关自己聚合的响应恒数到 0——实测（2026-10-28）
// 表现为「客户端明明收到 1 个调用，跳过统计却记成 0 调用」，把正常轮次误判成跳过。
func responseToolCallCount(resp map[string]any) int {
	choices, _ := resp["choices"].([]any)
	n := 0
	for _, raw := range choices {
		ch, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"message", "delta"} {
			holder, ok := ch[key].(map[string]any)
			if !ok {
				continue
			}
			switch tcs := holder["tool_calls"].(type) {
			case []any:
				n += len(tcs)
			case []map[string]any:
				n += len(tcs)
			}
		}
	}
	return n
}

// responseObserver 捕获 handler 实际写出的 HTTP 状态，同时保留 Flusher/Unwrap，
// 避免破坏 SSE 逐帧刷新。
type responseObserver struct {
	http.ResponseWriter
	status int
}

func (o *responseObserver) WriteHeader(code int) {
	if o.status == 0 {
		o.status = code
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *responseObserver) Write(p []byte) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}
	return o.ResponseWriter.Write(p)
}

func (o *responseObserver) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *responseObserver) Unwrap() http.ResponseWriter { return o.ResponseWriter }

// 请求流水行的固定列宽（显示列宽，非字节）。取固定宽度而不是让内容自然长度撑开，
// 是为了让 stdout 里成百上千行能竖着扫——否则模型名长短不一、中文昵称按字节补空格
// 错位，根本没法用肉眼对齐着一列列看（这正是上一版 11 字节硬截断要解决的问题）。
const (
	// chatModelWidth 覆盖 realm 前缀 + 最长模型名："global:" (7) + "deepseek-v4.1-flash" (19) = 26。
	// 旧的 11 字节截断会把 "cn:deepseek-v4-flash" 切成 "cn:deepseek"，让人误以为是另一个模型。
	chatModelWidth = 26
	// chatAcctWidth 容纳 "昵称(uid8)"：中文昵称按 2 列/字算，5 字中文 + "(xxxxxxxx)" = 20 列。
	chatAcctWidth = 22
	chatTTFBWidth = 8
	chatTokWidth  = 6
	chatRateWidth = 11 // 形如 "183.6tok/s"
)

// logChatRow 打印一行请求级表格日志（输出 chatLogOut，无 log 时间戳前缀）。
//
// 参数：
//   - model：模型名（含 realm 前缀），超 chatModelWidth 截断（模型名是 ASCII，字节截即列宽）；
//   - uid/nick：完整 uid 与账号昵称，经 logfmt.Label 拼成 "昵称(uid8)" 展示——只有
//     uid8 时人眼无法判断是哪个号，要辨认必须再查 auths/，排障多一跳；
//   - toks<0 表示 usage 缺失，显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int) {
	logChatRowEx(ttfb, total, model, mode, uid, nick, status, toks, "", "", 0, 0, false, "", "", "")
}

// logChatRowEx 是带请求 ID、结果、重试、积分、出口与调用来源字段的扩展流水行。旧调用
// 保持原格式；requestID 非空时才追加扩展字段；egress/clientIP/userAgent 全空时不追加尾段。
//
// egress 是**出**口（本次请求从哪个源 IP / 代理出去），clientIP 是**入**口（谁调进来）。
// 两者并排落一行，才能回答「出口隔离到底有没有生效」——此前只有 src，看到的是客户端
// IP，配没配出口在流水里根本看不出来，只能翻启动日志的 [egress] 段人工对账。
func logChatRowEx(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int,
	requestID, outcome string, attempts int, credit float64, hasCredit bool,
	egress, clientIP, userAgent string) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	model = logfmt.Pad(logfmt.Truncate(model, chatModelWidth), chatModelWidth)
	// 账号标签只补不截：超宽时宁可让该行变宽，也不丢昵称信息（昵称是排查的主线索）。
	acct := logfmt.Pad(logfmt.Label(uid, nick), chatAcctWidth)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		// 速率与用量账本走同一个函数（扣掉 TTFB）。此前这里自己除 total，漏扣首
		// token 等待，于是控制台流水行的 tok/s 与面板数字对不上（issue #34）——
		// 本函数本来就收到了 ttfb，只是没拿它算速率。
		if tps, ok := tokensPerSecond(int64(toks), total, ttfb); ok {
			tokpsField = fmt.Sprintf("%.1ftok/s", tps)
		} else {
			tokpsField = "0.0tok/s"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	extra := ""
	if requestID != "" {
		if outcome == "" {
			outcome = reqlog.OutcomeHTTPError
			if status >= 200 && status < 300 {
				outcome = reqlog.OutcomeSuccess
			}
		}
		creditField := "-"
		if hasCredit {
			creditField = fmt.Sprintf("%.4f", credit)
		}
		extra = fmt.Sprintf(" rid=%s | out=%s | try=%d | credit=%s |", requestID, outcome, attempts, creditField)
	}
	// 尾段：出口 + 调用来源。IP 用可解析的裸值（便于 grep），UA 用 ShortUA 压缩后的
	// 客户端标签并加引号（标签内可能含空格，如 `OpenAI/Python 1.30.0` 只会取到
	// OpenAI/Python）。三者都未采集时不追加，旧行格式保持不变。
	// egress 排在 src 之前：先看「从哪出去」，再看「谁调进来」。
	src := ""
	fields := ""
	if egress != "" {
		fields = "egress=" + egress + " "
	}
	if clientIP != "" || userAgent != "" {
		ua := "-"
		if s := logfmt.ShortUA(userAgent); s != "" {
			ua = `"` + s + `"`
		}
		fields += fmt.Sprintf("src=%s ua=%s", dashIfEmpty(clientIP), ua)
	}
	if fields = strings.TrimSpace(fields); fields != "" {
		src = " " + fields + " |"
	}
	fmt.Fprintf(chatLogOut, "| #%03d | %s | %s | %s | %d | %s | TTFB=%s | tok=%s | %s | total=%.1fs |%s%s\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		acct,
		logfmt.Pad(ttfbMS, chatTTFBWidth),
		logfmt.Pad(tokField, chatTokWidth),
		logfmt.Pad(tokpsField, chatRateWidth),
		total.Seconds(),
		extra,
		src,
	)
}

// cacheMissSignal 低命中率告警信号（issue #92 P1 轻量化）：同一模型此前观测到过
// 命中、本次大前缀（≥1000 tok）整段未命中（命中率 <10%）时打一条 WARN，每模型
// 10 分钟冷却。只报「本可命中却重算」的可行动信号——新模型/新会话的首请求天然
// 全 miss，不在此列。
type cacheMissSignal struct {
	mu      sync.Mutex
	everHit map[string]bool
	last    map[string]time.Time
}

var cacheMissWarn = &cacheMissSignal{everHit: map[string]bool{}, last: map[string]time.Time{}}

// cacheMissWarnEvery 冷却间隔；变量供测试缩短。
var cacheMissWarnEvery = 10 * time.Minute

// noteCacheTokens 记录一次观测：有命中 → 标记该模型可命中；整段未命中且此前
// 命中过 → 触发 WARN（冷却内不重复）。
func (c *cacheMissSignal) noteCacheTokens(model string, prompt, hit, miss int64) {
	if model == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if hit > 0 {
		c.everHit[model] = true
		return
	}
	// hit==0 才可能是整段重算。小前缀（<1000 tok）的 miss 无告警价值；上游未回
	// miss 字段时按 prompt 全量视为未命中。
	if prompt < 1000 {
		return
	}
	if miss <= 0 {
		miss = prompt
	}
	if !c.everHit[model] {
		return
	}
	now := time.Now()
	if t, ok := c.last[model]; ok && now.Sub(t) < cacheMissWarnEvery {
		return
	}
	c.last[model] = now
	log.Printf("WARN: [server] cache miss: model=%s 本次大前缀未命中（prompt=%d miss=%d，此前观测到过命中）；上游前缀缓存重算，费用会显著升高", model, prompt, miss)
}

// requiredIgnoredSignal「要求必调工具却没调用」告警。
//
// 客户端声明 tool_choice=required（Anthropic 是 any）意味着本回合**必须**产出工具
// 调用，下游拿到纯文本就只能报错或空转。2026-10-28 实测上游忽略该字段取值
// （deepseek 3/3、kimi 2/2 轮都没调），网关对此无能为力——但它一旦开始生效、
// 或者哪天变成硬报错，都需要第一时间知道。
//
// 这条 WARN 同时覆盖两种成因，无法区分也不必区分：上游不认字段，或模型没遵从。
// 两者的处置都是「查客户端为什么依赖 required」。
type requiredIgnoredSignal struct {
	mu   sync.Mutex
	last map[string]time.Time
}

var requiredIgnoredWarn = &requiredIgnoredSignal{last: map[string]time.Time{}}

// requiredIgnoredEvery 每模型冷却；变量供测试缩短。
var requiredIgnoredEvery = 10 * time.Minute

// note 记录一次观测；required=false 或确有调用时静默。
func (r *requiredIgnoredSignal) note(model string, required bool, toolCalls int) (bool, string) {
	if !required || model == "" || toolCalls > 0 {
		return false, ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if t, ok := r.last[model]; ok && now.Sub(t) < requiredIgnoredEvery {
		return false, ""
	}
	r.last[model] = now
	return true, fmt.Sprintf("WARN: [server] tool_choice ignored: model=%s 客户端要求 tool_choice=required（本回合必须调工具），但响应里 0 个 tool_calls —— "+
		"上游未实现该字段（实测 2026-10-28 双模型 confirmed）或模型未遵从；依赖 required 的客户端会拿到纯文本。改用「声明 tools + 让模型自主决定」，不要依赖 required", model)
}

// prefixLostSignal「可共享前缀没到上游」告警。
//
// 判据是**不可能成立的量级关系**：入站的可共享前缀（system 全文 + tools 名字/描述）
// 是 prompt 的一部分，折算成 token 后不可能显著超过整个 prompt。一旦超过，说明这段
// 前缀根本没被上游计入——要么网关剥离了（tool_choice=none 的纯文本优化，属预期），
// 要么客户端以为发了但其实没发。
//
// 为什么值得单独立信号：这类请求的粘性/亲和命中都是**空转**——账号选对了，可那段
// 前缀压根没进去，于是"命中率 0%"看上去像缓存故障。2026-10-28 就有一次单次
// 115,640 字节（≈29k token）却只计 130 token 的实案，当时只能靠拉 1000 条日志
// 反推。有了这条 WARN，同一个问题会在发生的那一秒就写在日志里。
//
// 阈值取 1.5 倍而不是 1 倍：bytes/4 对中文是**低估**（UTF-8 一个汉字 3 字节、约
// 1 token，实际 1.5~2 字节/token），留出余量避免中文长前缀误报；而这条路要抓的
// 背离通常在两个数量级以上。
type prefixLostSignal struct {
	mu   sync.Mutex
	last map[string]time.Time
}

var prefixLostWarn = &prefixLostSignal{last: map[string]time.Time{}}

// prefixLostWarnEvery 每模型冷却；变量供测试缩短。
var prefixLostWarnEvery = 10 * time.Minute

// note 记录一次观测；返回是否触发与告警文案（触发由调用方打日志，便于测试）。
//
// expectedStripped=true 表示客户端自己声明了 tool_choice=none，网关据此剥掉了
// tools——前缀"没到上游"是设计使然，跳过（否则每个纯文本会话都刷一条）。
func (p *prefixLostSignal) note(model string, prefixBytes int, prompt int64, expectedStripped bool) (bool, string) {
	if expectedStripped || model == "" || prefixBytes <= 0 || prompt <= 0 {
		return false, ""
	}
	// prefixTok 是前缀材料的保守 token 下界（bytes/4）。
	prefixTok := int64(prefixBytes) / 4
	if prefixTok*2 <= prompt*3 { // prefixTok <= prompt*1.5
		return false, ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if t, ok := p.last[model]; ok && now.Sub(t) < prefixLostWarnEvery {
		return false, ""
	}
	p.last[model] = now
	return true, fmt.Sprintf("WARN: [server] prefix lost: model=%s 入站可共享前缀 %d B（≈%d tok）但上游只计入 prompt=%d tok —— 该前缀没有到达上游，粘性/亲和命中的是空转。"+
		"客户端未声明 tool_choice=none（否则属预期剥离、不会告警），请查这条链路上谁吞掉了 tools/system",
		model, prefixBytes, prefixTok, prompt)
}
