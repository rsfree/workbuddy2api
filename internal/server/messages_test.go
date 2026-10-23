package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newSearchUpstream 构造指向 httptest 假上游的 upstream.Client（CN 域）。
func newSearchUpstream(t *testing.T, h http.HandlerFunc) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
}

// searchOKBody 上游成功响应（扁平结构，非信封）。
const searchOKBody = `{"query":"golang","type":"text2text","provider":"0",
	"results":[{"title":"Go 官网","url":"https://go.dev","snippet":"摘要"}],
	"total_results":1,"response_time_ms":120}`

// messagesBody 构造一个声明了 web_search 的 Anthropic Messages 请求。
func messagesBody(text string, declareSearch bool) string {
	tools := `[{"type":"web_search_20250305","name":"web_search","max_uses":5}]`
	if !declareSearch {
		tools = `[]`
	}
	return `{"model":"deepseek-v4-flash","max_tokens":4096,` +
		`"tools":` + tools + `,` +
		`"messages":[{"role":"user","content":[{"type":"text","text":` + quoteJSON(text) + `}]}]}`
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func doMessages(t *testing.T, h *Handler, apiKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/search/messages", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestMessagesSearchSuccess 断言成功响应符合 DSH 内置 provider 的解析契约：
// 必须有 web_search_tool_result block，其 content[] 为 web_search_result，
// 摘要走 text block 的 citations[]。
func TestMessagesSearchSuccess(t *testing.T) {
	var upstreamQuery string
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw := new(bytes.Buffer)
		_, _ = raw.ReadFrom(r.Body)
		_ = json.Unmarshal(raw.Bytes(), &body)
		if q, ok := body["query"].(string); ok {
			upstreamQuery = q
		}
		_, _ = w.Write([]byte(`{"query":"golang","type":"text2text","provider":"0",
			"results":[
				{"title":"Go 官网","url":"https://go.dev","snippet":"摘要一"},
				{"title":"","url":"https://example.com/x","snippet":""}
			],"total_results":2,"response_time_ms":100}`))
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up})

	rec := doMessages(t, h, "", messagesBody("Perform a web search for the query: golang", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 前缀必须被剥掉，只搜 "golang"。
	if upstreamQuery != "golang" {
		t.Errorf("upstream query=%q want golang（前缀须剥离）", upstreamQuery)
	}

	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   []struct {
				Type  string `json:"type"`
				URL   string `json:"url"`
				Title string `json:"title"`
			} `json:"content"`
			Citations []struct {
				Type      string `json:"type"`
				URL       string `json:"url"`
				CitedText string `json:"cited_text"`
			} `json:"citations"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response must be JSON: %v", err)
	}
	if out.Type != "message" || out.Role != "assistant" || out.StopReason != "end_turn" {
		t.Errorf("envelope=%+v", out)
	}
	if out.Model != "deepseek-v4-flash" {
		t.Errorf("model=%q want 请求里的 model", out.Model)
	}
	if len(out.Content) < 2 {
		t.Fatalf("content blocks=%d want >=2（结果块 + citations 文本块）", len(out.Content))
	}
	res := out.Content[0]
	if res.Type != "web_search_tool_result" {
		t.Errorf("content[0].type=%q want web_search_tool_result", res.Type)
	}
	if res.ToolUseID == "" {
		t.Error("tool_use_id must be non-empty")
	}
	if len(res.Content) != 2 {
		t.Fatalf("results=%d want 2", len(res.Content))
	}
	if res.Content[0].URL != "https://go.dev" || res.Content[0].Title != "Go 官网" {
		t.Errorf("results[0]=%+v", res.Content[0])
	}
	// 无标题时用 URL 兜底（客户端按 title ?? hostname 渲染）。
	if res.Content[1].Title != "https://example.com/x" {
		t.Errorf("空标题兜底=%q want URL", res.Content[1].Title)
	}
	// 摘要只能走 citations。
	text := out.Content[1]
	if text.Type != "text" {
		t.Fatalf("content[1].type=%q want text", text.Type)
	}
	if len(text.Citations) != 1 || text.Citations[0].CitedText != "摘要一" || text.Citations[0].URL != "https://go.dev" {
		t.Errorf("citations=%+v", text.Citations)
	}
}

// TestMessagesSearchStringContent 断言 content 为纯字符串形态也能提取 query。
func TestMessagesSearchStringContent(t *testing.T) {
	var upstreamQuery string
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		raw := new(bytes.Buffer)
		_, _ = raw.ReadFrom(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw.Bytes(), &body)
		upstreamQuery, _ = body["query"].(string)
		_, _ = w.Write([]byte(`{"query":"x","results":[{"title":"t","url":"https://a.com"}],"total_results":1}`))
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up})

	body := `{"model":"m","tools":[{"type":"web_search_20250305"}],
		"messages":[{"role":"user","content":"Perform a web search for the query: 中文查询"}]}`
	if rec := doMessages(t, h, "", body); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if upstreamQuery != "中文查询" {
		t.Errorf("upstream query=%q want 中文查询", upstreamQuery)
	}
}

// TestMessagesSearchRejectsNonSearch 断言未声明 web_search 的请求被明确拒绝
// （而不是把用户消息当搜索词静默执行）。
func TestMessagesSearchRejectsNonSearch(t *testing.T) {
	called := false
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up})

	rec := doMessages(t, h, "", messagesBody("今天天气如何", false))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("error body must be JSON: %v", err)
	}
	if out.Type != "error" || out.Error.Type != "invalid_request_error" {
		t.Errorf("error envelope=%+v", out)
	}
	if called {
		t.Error("未声明搜索工具时不应打上游")
	}
}

// TestMessagesSearchValidation 断言空 query / 非 JSON 被拒。
func TestMessagesSearchValidation(t *testing.T) {
	called := false
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{}`))
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up})

	cases := []struct{ name, body string }{
		{"非 JSON", `not json`},
		{"query 为空", messagesBody("", true)},
		{"query 仅前缀", messagesBody("Perform a web search for the query:", true)},
		{"query 过短", messagesBody("Perform a web search for the query: a", true)},
	}
	for _, tc := range cases {
		rec := doMessages(t, h, "", tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d want 400 body=%s", tc.name, rec.Code, rec.Body)
		}
	}
	if called {
		t.Error("校验失败不应打上游")
	}
}

// TestMessagesSearchRequiresAuth 断言鉴权与 /v1/search 同口径。
func TestMessagesSearchRequiresAuth(t *testing.T) {
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("鉴权失败不得打上游")
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up, APIKey: "secret"})

	if rec := doMessages(t, h, "", messagesBody("Perform a web search for the query: x", true)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无鉴权 code=%d want 401", rec.Code)
	}
}

// TestMessagesSearchUpstreamErrorShape 断言上游错误按 Anthropic 错误形态返回。
func TestMessagesSearchUpstreamErrorShape(t *testing.T) {
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":429,"msg":"rate limit exceeded, will reset in 60s"}`))
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up})

	rec := doMessages(t, h, "", messagesBody("Perform a web search for the query: golang", true))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429", rec.Code)
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("error body must be JSON: %v", err)
	}
	if out.Type != "error" || out.Error.Type != "rate_limit_error" {
		t.Errorf("error envelope=%+v want rate_limit_error", out)
	}
	if !strings.Contains(out.Error.Message, "rate limit") {
		t.Errorf("message=%q want 含上游原文", out.Error.Message)
	}
}

// TestMessagesRouteDoesNotShadowV1Messages 断言搜索端点没有占用 /v1/messages
// （该路径保留给 Anthropic 模型对话）。
func TestMessagesRouteDoesNotShadowV1Messages(t *testing.T) {
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("不得打上游")
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /v1/messages code=%d want 404（该路径留给模型对话）", rec.Code)
	}
}

// TestMessagesQueryIgnoresHistory 回归：只取最后一条带文本的消息。
//
// 拼接整段历史会让客户端带多轮上下文时把历史问句一起当搜索词送上游，
// 且固定前缀因不在开头而剥不掉（最终搜一段垃圾）。
func TestMessagesQueryIgnoresHistory(t *testing.T) {
	items := []messagesTurnItem{
		{Content: json.RawMessage(`[{"type":"text","text":"帮我查下 Go 版本"}]`)},
		{Content: json.RawMessage(`[{"type":"text","text":"Perform a web search for the query: golang"}]`)},
	}
	if got := extractMessagesQuery(items); got != "golang" {
		t.Errorf("query=%q want golang（历史不得混入）", got)
	}

	// 反向：历史在前、最新一条是纯字符串形态时也要取到最后一条。
	items2 := []messagesTurnItem{
		{Content: json.RawMessage(`[{"type":"text","text":"Perform a web search for the query: old"}]`)},
		{Content: json.RawMessage(`"Perform a web search for the query: newest"`)}, // content 为纯字符串形态（JSON 字符串字面量）
	}
	if got := extractMessagesQuery(items2); got != "newest" {
		t.Errorf("query=%q want newest", got)
	}
}

// TestMessagesResponseIDsAreUnique 回归：msg id 与 tool_use_id 必须逐次唯一。
//
// 用 query 哈希会让同一 query 的重复搜索返回同一个 id——违反 Anthropic 协议里
// 「id 标识一次工具调用」的语义，下游按 id 缓存/去重会互相覆盖。
func TestMessagesResponseIDsAreUnique(t *testing.T) {
	mk := func(query string) messagesResponse {
		return buildMessagesSearchResponse(query, "m", &upstream.SearchOutcome{
			Query:   query,
			Results: []upstream.SearchResult{{Title: "t", URL: "https://a.com", Snippet: "s"}},
		})
	}
	a, b := mk("golang"), mk("golang")
	if a.ID != "" && a.ID == b.ID {
		t.Errorf("msg id 重复: %q（同 query 两次调用必须不同）", a.ID)
	}
	idOf := func(r messagesResponse) string {
		if blk, ok := r.Content[0].(messagesToolResultBlock); ok {
			return blk.ToolUseID
		}
		return ""
	}
	if x, y := idOf(a), idOf(b); x != "" && x == y {
		t.Errorf("tool_use_id 重复: %q", x)
	}
	if !strings.HasPrefix(a.ID, "msg_wb2api_") {
		t.Errorf("msg id=%q want msg_wb2api_ 前缀", a.ID)
	}
}

// searchItems 从 Messages 响应里取 web_search_tool_result 的结果项。
func searchItems(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var resp struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	for _, blk := range resp.Content {
		if blk["type"] != "web_search_tool_result" {
			continue
		}
		raw, _ := blk["content"].([]any)
		out := make([]map[string]any, 0, len(raw))
		for _, it := range raw {
			if m, ok := it.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	t.Fatalf("响应里没有 web_search_tool_result: %s", body)
	return nil
}

// TestMessagesMaxResultsAndDomainFilter 网关扩展字段：
// max_results 收敛到上游上限、allowed_domains 在本地严格过滤、单一域名自动加 site:。
func TestMessagesMaxResultsAndDomainFilter(t *testing.T) {
	var gotQuery string
	var gotMax float64
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotQuery, _ = body["query"].(string)
		gotMax, _ = body["max_results"].(float64)
		_, _ = w.Write([]byte(`{"query":"q","type":"text2text","provider":"1","results":[
			{"title":"wx1","url":"https://mp.weixin.qq.com/s/1","snippet":"a"},
			{"title":"other1","url":"https://developers.weixin.qq.com/x","snippet":"b"},
			{"title":"wx2","url":"http://mp.weixin.qq.com/s/2","snippet":"c"},
			{"title":"other2","url":"https://hub.baai.ac.cn/view/1","snippet":"d"},
			{"title":"wx3","url":"https://mp.weixin.qq.com/s/3","snippet":"e"},
			{"title":"other3","url":"https://openai.weixin.qq.com/","snippet":"f"},
			{"title":"wx4","url":"https://mp.weixin.qq.com/s/4","snippet":"g"}
		],"total_results":7}`))
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up})

	body := `{"model":"m","max_tokens":1024,"max_results":3,"allowed_domains":["https://mp.weixin.qq.com/s/anything"],
		"tools":[{"type":"web_search_20250305","name":"web_search"}],
		"messages":[{"role":"user","content":"Perform a web search for the query: 智能体 落地"}]}`
	rec := doMessages(t, h, "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	items := searchItems(t, rec.Body.Bytes())
	if len(items) != 3 {
		t.Fatalf("items=%d want 3: %v", len(items), items)
	}
	for _, it := range items {
		if u, _ := it["url"].(string); !strings.Contains(u, "mp.weixin.qq.com") {
			t.Errorf("过滤后仍出现非公众号: %v", it)
		}
	}
	if gotMax != 10 {
		t.Errorf("upstream max_results=%v want 10（过滤需要更多候选）", gotMax)
	}
	if !strings.Contains(gotQuery, "site:mp.weixin.qq.com") {
		t.Errorf("query=%q want site: 注入", gotQuery)
	}
	if got := rec.Header().Get("X-WB2A-Search-Max"); got != "3" {
		t.Errorf("X-WB2A-Search-Max=%q", got)
	}
	if got := rec.Header().Get("X-WB2A-Search-Filtered"); got != "3" {
		t.Errorf("X-WB2A-Search-Filtered=%q want 3", got)
	}
}

// TestMessagesExcludedDomains 黑名单优先于白名单。
func TestMessagesExcludedDomains(t *testing.T) {
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"query":"q","type":"text2text","results":[
			{"title":"keep","url":"https://good.example/a","snippet":"a"},
			{"title":"drop","url":"https://spam.good.example/b","snippet":"b"},
			{"title":"keep2","url":"https://other.example/c","snippet":"c"}
		],"total_results":3}`))
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up})

	body := `{"model":"m","max_tokens":1024,"excluded_domains":["spam.good.example"],
		"tools":[{"type":"web_search_20250305","name":"web_search"}],
		"messages":[{"role":"user","content":"Perform a web search for the query: qq"}]}`
	rec := doMessages(t, h, "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	items := searchItems(t, rec.Body.Bytes())
	if len(items) != 2 {
		t.Fatalf("items=%d want 2: %v", len(items), items)
	}
	for _, it := range items {
		if u, _ := it["url"].(string); strings.Contains(u, "spam.good.example") {
			t.Errorf("黑名单域名未被过滤: %v", it)
		}
	}
}

// TestMessagesFetchContent fetch_content 给前 N 条结果补正文，失败只影响该条。
func TestMessagesFetchContent(t *testing.T) {
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agenttool/v1/search":
			_, _ = w.Write([]byte(`{"query":"q","type":"text2text","results":[
				{"title":"ok","url":"https://mp.weixin.qq.com/s/ok","snippet":"a"},
				{"title":"bad","url":"https://mp.weixin.qq.com/s/bad","snippet":"b"}
			],"total_results":2}`))
		case "/agenttool/v1/webfetch":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["url"] == "https://mp.weixin.qq.com/s/bad" {
				_, _ = w.Write([]byte(`{"code":15003,"msg":"blocked"}`))
				return
			}
			_, _ = w.Write([]byte(`{"url":"https://mp.weixin.qq.com/s/ok","title":"ok",
				"content":"完整正文","content_length":4,"status_code":200}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up})

	body := `{"model":"m","max_tokens":1024,"fetch_content":true,"fetch_max":2,
		"tools":[{"type":"web_search_20250305","name":"web_search"}],
		"messages":[{"role":"user","content":"Perform a web search for the query: qq"}]}`
	rec := doMessages(t, h, "", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	items := searchItems(t, rec.Body.Bytes())
	if len(items) != 2 {
		t.Fatalf("items=%d", len(items))
	}
	byURL := map[string]map[string]any{}
	for _, it := range items {
		byURL[it["url"].(string)] = it
	}
	ok := byURL["https://mp.weixin.qq.com/s/ok"]
	if ok["content"] != "完整正文" {
		t.Errorf("ok item=%v", ok)
	}
	bad := byURL["https://mp.weixin.qq.com/s/bad"]
	if bad["content_error"] == nil || bad["content_error"] == "" {
		t.Errorf("bad item 应带 content_error: %v", bad)
	}
	if got := rec.Header().Get("X-WB2A-Search-Fetched"); got != "2" {
		t.Errorf("X-WB2A-Search-Fetched=%q", got)
	}
}
