// messages.go Anthropic Messages 兼容的搜索端点 POST /v1/search/messages。
//
// 存在理由：DSH 内置的搜索 provider（`@deepseek-ai/dsh-web-search-deepseek`）把
// 搜索请求硬编码发到 `${baseURL}/messages`，且要求响应是 Anthropic Messages 形态
// （含 `web_search_tool_result` block）。把该协议壳做在网关侧，客户端只需把搜索
// baseURL 指向 `<网关>/v1/search`，零代码改动即可用上本网关的搜索能力。
//
// 为什么不占用 `/v1/messages`：那是 Anthropic **模型对话**的标准路径，已由
// proto_chat.go 的对话前门承接（同一路径同时做搜索会让 claude 客户端拿到搜索结果）。
// 搜索挂在 `/v1/search/messages`，与 OpenAI 风格的 `/v1/search` 同前缀，
// 语义清晰且互不干扰。
//
// 与真实 Anthropic 行为的差异（有意为之）：本端点**不做模型调用**。真实 Anthropic
// 的 web_search 是 server tool，由模型决定何时搜、并生成带引用的自然语言回答；
// 这里直接执行一次搜索并返回结构化结果块——因为网关没有模型，客户端（DSH）只从
// 结果块里取 sources，不消费自然语言回答。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// messagesBodyLimit Messages 请求体上限：只含 query 与工具声明，1MB 富余。
const messagesBodyLimit = 1 << 20

// messagesQueryPrefix 辅助搜索模型收到的固定指令前缀（DSH/官方 CLI 的约定）。
// 它是给模型看的指令，不是搜索词的一部分，必须剥掉后再送去搜索。
const messagesQueryPrefix = "Perform a web search for the query:"

// messagesRequest Anthropic Messages 请求里本端点关心的字段。
type messagesRequest struct {
	Model    string             `json:"model"`
	Tools    []messagesTool     `json:"tools"`
	Messages []messagesTurnItem `json:"messages"`

	// 以下为网关扩展字段（Anthropic 协议本身没有，客户端可不传）：
	//
	// MaxResults 期望条数 1–10，缺省 5（上游默认）。上游最多给 10 条。
	MaxResults int `json:"max_results"`
	// AllowedDomains 严格域名白名单（后缀匹配：example.com 含 a.example.com）。
	// 上游的 site: 只是软加权，过滤在网关侧做，才能真正只留目标站。
	AllowedDomains []string `json:"allowed_domains"`
	// ExcludedDomains 严格域名黑名单，优先级高于白名单。
	ExcludedDomains []string `json:"excluded_domains"`
	// FetchContent 为 true 时对前 FetchMax 条结果抓正文（/agenttool/v1/webfetch），
	// 正文写进结果的 content 扩展字段。搜索给链接、抓取给全文。
	FetchContent bool `json:"fetch_content"`
	// FetchMax 抓正文的条数上限，缺省 3、封顶 5（每条都是一次真实上游抓取）。
	FetchMax int `json:"fetch_max"`
}

// messagesTool 工具声明。搜索工具两种形态都认：`{type:"web_search_20250305"}`
// 与 `{name:"web_search"}`（后者是部分客户端的简化形态）。
type messagesTool struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// messagesTurnItem 单条消息。content 既可能是字符串，也可能是 block 数组，
// 用 RawMessage 保留原始形态交给 extractMessagesQuery 处理。
type messagesTurnItem struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// messagesContentBlock Messages content 数组里的文本块。
type messagesContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// declaresWebSearch 判断请求是否声明了 web_search server tool。
// 判定口径与 DSH 内置 provider 的客户端实现一致：type 以 "web_search" 开头，
// 或 name 恰为 "web_search"。
func declaresWebSearch(tools []messagesTool) bool {
	for _, t := range tools {
		if strings.HasPrefix(t.Type, "web_search") || t.Name == "web_search" {
			return true
		}
	}
	return false
}

// extractMessagesQuery 从 Messages 请求里还原真实搜索词。
//
// 约定（DSH / 官方 CLI 相同）：请求里恰好一条 user 消息，文本为
// `Perform a web search for the query: <query>`。前缀必须剥掉，否则会把指令本身
// 当成搜索词送去上游。
//
// **只取最后一条带文本的消息**，不拼接整段历史。拼接会在客户端带多轮上下文时把
// 历史问句一起当搜索词送上游（且前缀因不在开头而剥不掉，最终搜的是一段垃圾）；
// 「最后一次提问」才是此处唯一正确的语义。
func extractMessagesQuery(items []messagesTurnItem) string {
	joined := ""
	for _, m := range items {
		if len(m.Content) == 0 {
			continue
		}
		// content 为纯字符串（Anthropic 允许的简写形态）。
		var plain string
		if err := json.Unmarshal(m.Content, &plain); err == nil {
			if t := strings.TrimSpace(plain); t != "" {
				joined = t
			}
			continue
		}
		var blocks []messagesContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue
		}
		var text []string
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				text = append(text, b.Text)
			}
		}
		// 同一条消息内的多个 text block 拼接（一条消息被拆块是合法的）。
		if t := strings.TrimSpace(strings.Join(text, "\n")); t != "" {
			joined = t
		}
	}
	// 前缀大小写不敏感地剥离：不同客户端的模板大小写可能有差异。
	if len(joined) >= len(messagesQueryPrefix) &&
		strings.EqualFold(joined[:len(messagesQueryPrefix)], messagesQueryPrefix) {
		joined = strings.TrimSpace(joined[len(messagesQueryPrefix):])
	}
	return joined
}

// messagesSearchResultItem 响应里 `web_search_tool_result.content[]` 的单项。
// 注意：Anthropic 原生不带 snippet，摘要走 text 块的 citations，故这里只有 url/title。
type messagesSearchResultItem struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`

	// 以下为网关扩展字段（fetch_content=true 时才有）：Anthropic 原生 result 只
	// 有 url/title，多出的字段不影响既有客户端解析。
	Content       string `json:"content,omitempty"`
	ContentLength int    `json:"content_length,omitempty"`
	// ContentTruncated 正文超过单条上限被截断；或上游标记 truncated。
	ContentTruncated bool `json:"content_truncated,omitempty"`
	// ContentError 该条抓取失败的原因（不影响其它条）。
	ContentError string `json:"content_error,omitempty"`
}

// messagesToolResultBlock 搜索结果块。
type messagesToolResultBlock struct {
	Type      string                     `json:"type"`
	ToolUseID string                     `json:"tool_use_id"`
	Content   []messagesSearchResultItem `json:"content"`
}

// messagesCitation 引用条目（snippet 的唯一承载处）。
type messagesCitation struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	CitedText string `json:"cited_text"`
}

// messagesTextBlock 承载 citations 的文本块。
type messagesTextBlock struct {
	Type      string             `json:"type"`
	Text      string             `json:"text"`
	Citations []messagesCitation `json:"citations,omitempty"`
}

// messagesUsage Anthropic 响应必带的 usage。搜索不消耗模型 token，故为 0。
type messagesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// messagesResponse Anthropic Messages 响应。
type messagesResponse struct {
	ID           string        `json:"id"`
	Type         string        `json:"type"`
	Role         string        `json:"role"`
	Model        string        `json:"model"`
	Content      []any         `json:"content"`
	StopReason   string        `json:"stop_reason"`
	StopSequence any           `json:"stop_sequence"`
	Usage        messagesUsage `json:"usage"`
}

// buildMessagesSearchResponse 把上游搜索结果渲染成 Anthropic Messages 响应。
//
// 结构契约（DSH 内置 provider 的 mapAnthropicResponse 逐条要求）：
//   - 必须至少有一个 type="web_search_tool_result" 的 block，否则客户端报
//     WEB_PROVIDER_ERROR；
//   - 该 block 的 content[] 每项 type="web_search_result"，url 非空；
//   - snippet 只能从 text block 的 citations[].cited_text 取（web_search_result
//     本身不带摘要），故有摘要时追加一个 text block 承载引用。
func buildMessagesSearchResponse(query, model string, out *upstream.SearchOutcome, extras ...map[string]messagesFetchExtra) messagesResponse {
	// extras 可选：fetch_content=true 时传入的「url → 正文」映射。
	// 用可变参数是为了让不关心正文的调用方（以及既有测试）零改动。
	var fetched map[string]messagesFetchExtra
	if len(extras) > 0 {
		fetched = extras[0]
	}
	items := make([]messagesSearchResultItem, 0, len(out.Results))
	citations := make([]messagesCitation, 0, len(out.Results))
	for _, r := range out.Results {
		title := r.Title
		if title == "" {
			title = r.URL // 无标题时用 URL 兜底，客户端渲染 title ?? hostname 才不会空
		}
		item := messagesSearchResultItem{
			Type:  "web_search_result",
			URL:   r.URL,
			Title: title,
		}
		if ex, ok := fetched[r.URL]; ok {
			item.Content = ex.Content
			item.ContentLength = ex.ContentLength
			item.ContentTruncated = ex.Truncated
			item.ContentError = ex.Err
		}
		items = append(items, item)
		if r.Snippet != "" {
			citations = append(citations, messagesCitation{
				Type:      "web_search_result_location",
				URL:       r.URL,
				CitedText: r.Snippet,
			})
		}
	}
	if model == "" {
		model = "web-search"
	}
	// tool_use_id 与 msg id 每次调用都必须唯一：Anthropic 协议里它们标识的是
	// 「一次工具调用」，不是「一次查询的内容」。用 query 哈希会让同一 query 的
	// 重复搜索返回同一个 id（下游按 id 缓存/去重会互相覆盖），32 位哈希本身也可
	// 能碰撞。随机 16 hex 兼顾唯一性与可读性。
	callID := randomHex(16)
	content := []any{messagesToolResultBlock{
		Type:      "web_search_tool_result",
		ToolUseID: "srvtoolu_" + callID,
		Content:   items,
	}}
	if len(citations) > 0 {
		content = append(content, messagesTextBlock{Type: "text", Text: "", Citations: citations})
	}
	return messagesResponse{
		ID:           "msg_wb2api_" + callID,
		Type:         "message",
		Role:         "assistant",
		Model:        model,
		Content:      content,
		StopReason:   "end_turn",
		StopSequence: nil,
		Usage:        messagesUsage{InputTokens: 0, OutputTokens: 0},
	}
}

// randomHex 返回 n 字节随机数的 hex 串。熵源失败时回落时间+计数派生值：
// 仍保证非空且每次调用都不同（id 唯一性是这里的硬要求）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n*2, uint64(time.Now().UnixNano())^atomic.AddUint64(&idSeq, 1))
	}
	return hex.EncodeToString(b)
}

// idSeq 熵源故障时的兜底计数器（正常路径不使用）。
var idSeq uint64

// writeMessagesError 按 Anthropic 错误形态写出（与 OpenAI 形态分开，客户端各自解析）。
func writeMessagesError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	})
}

// messages 处理 POST /v1/search/messages（Anthropic Messages 兼容搜索端点）。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, messagesBodyLimit))
	if err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	var req messagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	// 只服务搜索：没有声明 web_search 的 Messages 请求属于模型对话，不该打到本端点
	// （打错端点时给明确错误，而不是静默把用户消息当搜索词）。
	if !declaresWebSearch(req.Tools) {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error",
			"request does not declare the web_search tool; this endpoint only serves web search, not chat")
		return
	}
	query := extractMessagesQuery(req.Messages)
	if len(query) < upstream.SearchMinQueryLen {
		writeMessagesError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("could not read a search query from the request (need at least %d characters)", upstream.SearchMinQueryLen))
		return
	}

	// 条数：缺省 5（上游默认），封顶 10（上游服务端上限，超了也不会多给）。
	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = upstream.SearchDefaultMaxResults
	}
	if maxResults > upstream.SearchMaxResultsCap {
		maxResults = upstream.SearchMaxResultsCap
	}

	allowed := normalizeDomainList(req.AllowedDomains)
	excluded := normalizeDomainList(req.ExcludedDomains)
	searchQuery := query
	upstreamN := maxResults
	if len(allowed) == 1 && !strings.Contains(strings.ToLower(searchQuery), "site:") {
		// site: 必须放在**查询开头**才生效：实测同一句 `智能体 落地`，
		// 后缀 `… site:mp.weixin.qq.com` 只回 1/5 公众号，前缀 `site:mp.weixin.qq.com …`
		// 回 5/5。上游把它当限定词而非布尔过滤，位置决定召回。
		// 仍是软加权（会混入 developers.weixin.qq.com 等同公司子域），
		// 故网关侧还要做严格后缀过滤。
		searchQuery = "site:" + allowed[0] + " " + searchQuery
	}
	if len(allowed) > 0 || len(excluded) > 0 || req.FetchContent {
		// 过滤/抓正文都要更多候选：只要 5 条时，过滤后可能只剩 0–2 条。
		upstreamN = upstream.SearchMaxResultsCap
	}

	meta := searchMetaFromRequest(r)
	out, fail := h.runSearch(r.Context(), searchQuery, upstreamN, meta)
	if fail != nil {
		// Anthropic 错误类型映射：限流用 rate_limit_error，其余用 api_error，
		// 与真实 Anthropic 的错误词表保持可识别。
		errType := "api_error"
		if fail.status == http.StatusTooManyRequests {
			errType = "rate_limit_error"
		} else if fail.status == http.StatusBadRequest {
			errType = "invalid_request_error"
		}
		msg := fail.msg
		if fail.hint != "" {
			msg = msg + " (" + fail.hint + ")"
		}
		writeMessagesError(w, fail.status, errType, msg)
		return
	}
	// 严格域名过滤在网关侧做：上游 site: 不保证「只返回该站」。
	if len(allowed) > 0 || len(excluded) > 0 {
		before := len(out.Results)
		out.Results = filterSearchResults(out.Results, allowed, excluded)
		if removed := before - len(out.Results); removed > 0 {
			w.Header().Set("X-WB2A-Search-Filtered", strconv.Itoa(removed))
		}
		if len(out.Results) == 0 && len(allowed) > 0 {
			// 明确告知「搜到了但都被域名过滤掉了」，避免客户端以为上游没结果。
			w.Header().Set("X-WB2A-Search-Warn", "no-results-after-domain-filter")
		}
	}
	// 过滤后可能超过调用方期望条数（上游取的是 10 条候选）。
	if len(out.Results) > maxResults {
		out.Results = out.Results[:maxResults]
	}
	w.Header().Set("X-WB2A-Search-Max", strconv.Itoa(maxResults))

	var extras map[string]messagesFetchExtra
	if req.FetchContent {
		fetchMax := req.FetchMax
		if fetchMax <= 0 {
			fetchMax = searchFetchDefault
		}
		if fetchMax > searchFetchMax {
			fetchMax = searchFetchMax
		}
		extras = h.fetchResultContents(r.Context(), meta, out.Results, fetchMax)
		if len(extras) > 0 {
			w.Header().Set("X-WB2A-Search-Fetched", strconv.Itoa(len(extras)))
		}
	}

	writeJSON(w, http.StatusOK, buildMessagesSearchResponse(query, req.Model, out, extras))
}

// ---------- 域名过滤与正文补全 ----------

const (
	// searchFetchDefault 默认抓几条正文。每条都是一次真实抓取，3 条是「够用且不慢」的折中。
	searchFetchDefault = 3
	// searchFetchMax 单次搜索最多抓 5 条正文（并发度也是 5）。
	searchFetchMax = 5
	// searchContentMaxChars 单条正文写回响应的字符上限。公众号长文 1–2 万字很常见，
	// 全量塞进搜索响应会让单次响应膨胀到几百 KB；需要全文请直接调 /v1/fetch。
	searchContentMaxChars = 20000
)

// normalizeDomainList 归一域名：小写、去 scheme/端口/路径、去空白与空项。
// 支持传 "mp.weixin.qq.com" 或 "https://mp.weixin.qq.com/s/xxx" 两种写法。
func normalizeDomainList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		d := strings.TrimSpace(strings.ToLower(raw))
		if d == "" {
			continue
		}
		if strings.HasPrefix(d, "http://") || strings.HasPrefix(d, "https://") {
			if u, err := url.Parse(d); err == nil && u.Hostname() != "" {
				d = u.Hostname()
			}
		}
		if i := strings.IndexAny(d, "/?#"); i >= 0 {
			d = d[:i]
		}
		if h, _, err := strings.Cut(d, ":"); err && h != "" {
			d = h
		}
		d = strings.Trim(d, ".")
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// hostMatchesDomain 后缀匹配：example.com 命中 example.com 与 a.example.com，
// 但不会命中 notexample.com（避免 "site:example.com" 被仿冒域绕过）。
func hostMatchesDomain(host, domain string) bool {
	host = strings.ToLower(strings.Trim(host, "."))
	if host == "" || domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// urlHost 取 URL 的主机名；解析失败返回空串。
func urlHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// filterSearchResults 按白/黑名单过滤结果。排除优先于允许。
// 无法解析出 host 的结果：有白名单时丢弃（无法证明它在白名单内），
// 只配黑名单时保留（没有证据表明它在黑名单内）。
func filterSearchResults(results []upstream.SearchResult, allowed, excluded []string) []upstream.SearchResult {
	if len(allowed) == 0 && len(excluded) == 0 {
		return results
	}
	out := make([]upstream.SearchResult, 0, len(results))
	for _, r := range results {
		host := urlHost(r.URL)
		if host == "" {
			if len(allowed) > 0 {
				continue
			}
			out = append(out, r)
			continue
		}
		if matchesAny(host, excluded) {
			continue
		}
		if len(allowed) > 0 && !matchesAny(host, allowed) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func matchesAny(host string, domains []string) bool {
	for _, d := range domains {
		if hostMatchesDomain(host, d) {
			return true
		}
	}
	return false
}

// messagesFetchExtra 单条结果的正文补全产物。
type messagesFetchExtra struct {
	Content       string
	ContentLength int
	Truncated     bool
	Err           string
}

// fetchResultContents 并发抓取前 limit 条结果的正文，返回 url → 正文映射。
//
// 抓取失败不失败整个搜索：错误写进该条的 content_error，其余结果照常返回。
// 并发度固定 5（等于 searchFetchMax），每条内部仍走账号轮转与在途租约。
func (h *Handler) fetchResultContents(ctx context.Context, meta upstream.ChatMeta, results []upstream.SearchResult, limit int) map[string]messagesFetchExtra {
	if limit <= 0 || len(results) == 0 {
		return nil
	}
	if limit > len(results) {
		limit = len(results)
	}
	out := make(map[string]messagesFetchExtra, limit)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, searchFetchMax)
	for _, r := range results[:limit] {
		u := strings.TrimSpace(r.URL)
		if u == "" {
			continue
		}
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// 语义：抓正文只要内容，不需要额外的提取指令。
			fetched, fail := h.runFetch(ctx, meta, target, "")
			mu.Lock()
			defer mu.Unlock()
			if fail != nil {
				out[target] = messagesFetchExtra{Err: fail.msg}
				return
			}
			content := fetched.Content
			truncated := fetched.Truncated
			if len(content) > searchContentMaxChars {
				content = content[:searchContentMaxChars]
				truncated = true
			}
			out[target] = messagesFetchExtra{
				Content:       content,
				ContentLength: fetched.ContentLength,
				Truncated:     truncated,
			}
		}(u)
	}
	wg.Wait()
	if len(out) == 0 {
		return nil
	}
	return out
}
