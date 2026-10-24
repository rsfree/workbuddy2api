// proto_chat_test.go /v1/messages 与 /v1/responses 两个前门的端到端测试。
//
// 断言重点不在翻译细节（那部分在 internal/bridge 有纯函数单测），而在
// 「两个前门确实跑在 chatCompletions 同一条链路上」：请求被翻成 chat 形态打给
// 上游、上游的流被翻回目标协议、错误按目标协议的信封回给客户端。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// protoCapture 记录上游收到的请求体，供断言「真的按 chat 形态发出去了」。
type protoCapture struct {
	mu     sync.Mutex
	bodies []string
}

func (c *protoCapture) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return ""
	}
	return c.bodies[len(c.bodies)-1]
}

func (c *protoCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

// newProtoUpstream 构造一个按请求体决定响应的 fake 上游。
func newProtoUpstream(t *testing.T, respond func(body string) (status int, body2 string, isStream bool)) (*upstream.Client, *protoCapture) {
	t.Helper()
	cap := &protoCapture{}
	client := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			cap.mu.Lock()
			cap.bodies = append(cap.bodies, string(raw))
			cap.mu.Unlock()
			status, body, isStream := respond(string(raw))
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return client, cap
}

func newProtoHandler(t *testing.T, up *upstream.Client) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
}

// toolSSE 上游把工具调用分两帧下发（参数拆开），最后一帧带 finish_reason。
const toolSSE = "data: {\"id\":\"chatcmpl-t\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_7\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\"\"}}]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-t\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":\\\"SZ\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n" +
	"data: [DONE]\n\n"

// decodeBody 把响应体解成 map。
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, rec.Body.String())
	}
	return out
}

// TestMessagesNonStream Anthropic 非流式：请求翻成 chat 打上游，响应翻回 message。
func TestMessagesNonStream(t *testing.T) {
	up, cap := newProtoUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := newProtoHandler(t, up)

	body := `{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	// 上游必须收到 chat 形态：messages 数组 + user 消息。
	// 注意上游恒为流式（网关侧聚合），所以这里不校验 stream 字段的值——
	// 客户端要的非流式语义由 chatCompletions 的 Aggregate 负责。
	sent := cap.last()
	if !strings.Contains(sent, `"messages"`) || !strings.Contains(sent, `"role":"user"`) {
		t.Errorf("上游收到非法形态: %s", sent)
	}
	if !strings.Contains(sent, `"max_tokens":64`) {
		t.Errorf("max_tokens 未透传: %s", sent)
	}

	out := decodeBody(t, rec)
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Fatalf("信封错: %v", out)
	}
	if out["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v want end_turn", out["stop_reason"])
	}
	content, _ := out["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("content 为空: %v", out)
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "你好" {
		t.Errorf("正文块=%v want text=你好", first)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != float64(1) || usage["output_tokens"] != float64(1) {
		t.Errorf("usage=%v", usage)
	}
}

// TestMessagesStream Anthropic 流式：SSE 事件序列与 text_delta 必须齐备。
func TestMessagesStream(t *testing.T) {
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := newProtoHandler(t, up)

	body := `{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q", ct)
	}
	out := rec.Body.String()
	wants := []string{
		"event: message_start",
		`"type":"text_delta"`,
		`"text":"你好"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("SSE 缺 %q；实际输出:\n%s", w, out)
		}
	}
	// 原始的 OpenAI 帧结构不能泄漏给 Anthropic 客户端。
	if strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Errorf("chat 帧未被翻译:\n%s", out)
	}
}

// TestMessagesStreamToolUse 工具调用流：必须产出 tool_use 块 + input_json_delta。
func TestMessagesStreamToolUse(t *testing.T) {
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		return 200, toolSSE, true
	})
	h := newProtoHandler(t, up)

	body := `{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"天气"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	out := rec.Body.String()
	wants := []string{
		`"type":"tool_use"`,
		`"name":"get_weather"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"city\":\"SZ\"}"`,
		`"stop_reason":"tool_use"`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("SSE 缺 %q；实际输出:\n%s", w, out)
		}
	}
}

// TestMessagesErrorEnvelope 上游失败的 4xx 必须翻成 Anthropic 错误信封，
// 且上游原文留在 message 里（error-passthrough 语义不能丢）。
func TestMessagesErrorEnvelope(t *testing.T) {
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		return 400, `{"code":"11-115","msg":"prompt is too long: 99999 tokens > 8192 maximum"}`, false
	})
	h := newProtoHandler(t, up)

	body := `{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	out := decodeBody(t, rec)
	if out["type"] != "error" {
		t.Fatalf("信封=%v want type=error", out)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["type"] != "invalid_request_error" {
		t.Errorf("error.type=%v", errObj["type"])
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "prompt is too long") {
		t.Errorf("上游原文丢失: %q", msg)
	}
}

// TestMessagesAuthEnvelope 鉴权失败也要按 Anthropic 形状回（SDK 才能正确报 401）。
func TestMessagesAuthEnvelope(t *testing.T) {
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		t.Error("未鉴权不得打上游")
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   "sk-test",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
	out := decodeBody(t, rec)
	errObj, _ := out["error"].(map[string]any)
	if out["type"] != "error" || errObj["type"] != "authentication_error" {
		t.Errorf("envelope=%v", out)
	}
}

// TestResponsesNonStream Responses 非流式：input 折成 chat，响应翻成 response 对象。
func TestResponsesNonStream(t *testing.T) {
	up, cap := newProtoUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := newProtoHandler(t, up)

	body := `{"model":"glm-5.2","input":"hi"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if sent := cap.last(); !strings.Contains(sent, `"role":"user"`) {
		t.Errorf("input 未折成 user 消息: %s", sent)
	}
	out := decodeBody(t, rec)
	if out["object"] != "response" || out["status"] != "completed" {
		t.Fatalf("信封=%v", out)
	}
	output, _ := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v", out["output"])
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "message" {
		t.Fatalf("output[0]=%v", item)
	}
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "你好" {
		t.Errorf("output_text=%v", part)
	}
}

// TestResponsesStream Responses 流式：事件序列齐备且没有 chat 帧泄漏。
func TestResponsesStream(t *testing.T) {
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := newProtoHandler(t, up)

	body := `{"model":"glm-5.2","input":"hi","stream":true}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

	out := rec.Body.String()
	wants := []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.completed",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("SSE 缺 %q；实际输出:\n%s", w, out)
		}
	}
	if strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Errorf("chat 帧未被翻译:\n%s", out)
	}
}

// TestCountTokens Anthropic count_tokens：本地估算，不打上游。
func TestCountTokens(t *testing.T) {
	up, cap := newProtoUpstream(t, func(string) (int, string, bool) {
		t.Error("count_tokens 不应打上游")
		return 200, sseOK, true
	})
	h := newProtoHandler(t, up)

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"帮我算一下这段文字有多少 token"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if cap.count() != 0 {
		t.Errorf("不该调用上游，实际 %d 次", cap.count())
	}
	out := decodeBody(t, rec)
	n, _ := out["input_tokens"].(float64)
	if n <= 0 {
		t.Errorf("input_tokens=%v want >0", out["input_tokens"])
	}
}

// TestProtoPathTraced 两个前门必须进请求日志账本（否则面板看不到这类流量）。
func TestProtoPathTraced(t *testing.T) {
	for _, p := range []string{"/v1/messages", "/v1/responses", "/v1/messages/count_tokens"} {
		if !isProtoPath(p) {
			t.Errorf("%s 未纳入 isProtoPath", p)
		}
	}
	if isProtoPath("/v1/search/messages") {
		t.Error("搜索端点不该被当成对话前门")
	}
}

// TestMessagesStreamOverRealHTTP 走真实 TCP 连接（而非 Recorder）验证流式：
// 抓「响应头先于正文下发」「Content-Type 正确」「逐帧可读」这类只在真连接上
// 才暴露的问题（Recorder 不做 chunked 编码）。
func TestMessagesStreamOverRealHTTP(t *testing.T) {
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := newProtoHandler(t, up)
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Proxy 置 nil：本机环境变量里的 HTTP_PROXY 会把 localhost 也代理走，
	// 导致测试在 CI/本机表现不一致。
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	out := string(raw)
	if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") {
		t.Errorf("SSE 不完整:\n%s", out)
	}
}
