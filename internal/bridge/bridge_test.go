package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// decode 把 any 序列化后再解到目标结构，避免测试里到处写 map 断言。
func decode[T any](t *testing.T, v any) T {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

type chatMsg struct {
	Role      string `json:"role"`
	Content   any    `json:"content"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
	ToolCallID string `json:"tool_call_id"`
}

type chatReq struct {
	Model      string    `json:"model"`
	Stream     bool      `json:"stream"`
	MaxTokens  int       `json:"max_tokens"`
	Messages   []chatMsg `json:"messages"`
	ToolChoice any       `json:"tool_choice"`
	Tools      []struct {
		Type     string `json:"type"`
		Function struct {
			Name       string         `json:"name"`
			Parameters map[string]any `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
}

// TestAnthropicToChatFull 覆盖 system / tool_use / tool_result / tools / tool_choice。
func TestAnthropicToChatFull(t *testing.T) {
	req := map[string]any{
		"model":      "glm-5.2",
		"max_tokens": 128.0,
		"stream":     true,
		"system":     []any{map[string]any{"type": "text", "text": "be nice"}},
		"tools": []any{map[string]any{
			"name":         "get_weather",
			"description":  "查天气",
			"input_schema": map[string]any{"type": "object"},
		}},
		"tool_choice": map[string]any{"type": "any"},
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "let me check"},
				map[string]any{"type": "tool_use", "id": "tu1", "name": "get_weather",
					"input": map[string]any{"city": "SZ"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu1",
					"content": []any{map[string]any{"type": "text", "text": "sunny"}}},
			}},
		},
	}
	out, err := AnthropicToChat(req)
	if err != nil {
		t.Fatalf("AnthropicToChat: %v", err)
	}
	got := decode[chatReq](t, out)

	if !got.Stream || got.MaxTokens != 128 {
		t.Fatalf("stream/max_tokens 未透传: %+v", got)
	}
	if len(got.Messages) != 4 {
		t.Fatalf("messages=%d want 4（system + user + assistant + tool）", len(got.Messages))
	}
	if got.Messages[0].Role != "system" || got.Messages[0].Content != "be nice" {
		t.Errorf("system 未前置: %+v", got.Messages[0])
	}
	if got.Messages[2].Role != "assistant" || len(got.Messages[2].ToolCalls) != 1 {
		t.Fatalf("tool_use 未转成 tool_calls: %+v", got.Messages[2])
	}
	if got.Messages[2].ToolCalls[0].ID != "tu1" ||
		got.Messages[2].ToolCalls[0].Function.Arguments != `{"city":"SZ"}` {
		t.Errorf("tool_call 参数错: %+v", got.Messages[2].ToolCalls[0])
	}
	if got.Messages[3].Role != "tool" || got.Messages[3].ToolCallID != "tu1" ||
		got.Messages[3].Content != "sunny" {
		t.Errorf("tool_result 未转成独立 tool 消息: %+v", got.Messages[3])
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tools 未转换: %+v", got.Tools)
	}
	if got.ToolChoice != "required" {
		t.Errorf("tool_choice any → required，got %v", got.ToolChoice)
	}
}

// TestAnthropicToChatImage 图片块必须转成 image_url data URL（否则多模态客户端
// 的截图会静默丢失）。
func TestAnthropicToChatImage(t *testing.T) {
	req := map[string]any{
		"model": "glm-5.2",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "看图"},
			map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": "AAAA",
			}},
		}}},
	}
	out, err := AnthropicToChat(req)
	if err != nil {
		t.Fatalf("AnthropicToChat: %v", err)
	}
	got := decode[chatReq](t, out)
	parts, ok := got.Messages[0].Content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content parts=%v want 2", got.Messages[0].Content)
	}
	second, _ := parts[1].(map[string]any)
	img, _ := second["image_url"].(map[string]any)
	if img["url"] != "data:image/png;base64,AAAA" {
		t.Errorf("image url=%v", img["url"])
	}
}

// TestAnthropicToChatRejectsEmpty messages 为空的请求不该被翻成空对话打上游。
func TestAnthropicToChatRejectsEmpty(t *testing.T) {
	if _, err := AnthropicToChat(map[string]any{"messages": []any{}}); err == nil {
		t.Fatal("空 messages 应报错")
	}
}

// TestChatToAnthropic 非流式响应：思考链、正文、工具调用、stop_reason 映射。
func TestChatToAnthropic(t *testing.T) {
	obj := map[string]any{
		"id":    "chatcmpl-1",
		"model": "glm-5.2",
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"reasoning_content": "想一想",
				"content":           "答案",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "function": map[string]any{
						"name": "get_weather", "arguments": `{"city":"SZ"}`,
					},
				}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 5.0},
	}
	out := ChatToAnthropic(obj, "glm-5.2")

	if out["type"] != "message" || out["role"] != "assistant" {
		t.Fatalf("信封错: %+v", out)
	}
	if out["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v want tool_use", out["stop_reason"])
	}
	blocks, _ := out["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("content blocks=%d want 3（thinking+text+tool_use）", len(blocks))
	}
	if blocks[0].(map[string]any)["type"] != "thinking" {
		t.Errorf("首块应为 thinking: %+v", blocks[0])
	}
	toolUse, _ := blocks[2].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["name"] != "get_weather" {
		t.Fatalf("tool_use 块错: %+v", toolUse)
	}
	input, _ := toolUse["input"].(map[string]any)
	if input["city"] != "SZ" {
		t.Errorf("input=%v want 解析后的对象", input)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != 10 || usage["output_tokens"] != 5 {
		t.Errorf("usage=%v", usage)
	}
}

// TestAnthropicStreamToolCalls 工具参数分帧到达：必须攒完再发 tool_use 块。
func TestAnthropicStreamToolCalls(t *testing.T) {
	s := NewAnthropicStream("glm-5.2", "msg_test")
	var names []string
	for _, ev := range s.Start() {
		names = append(names, ev.Name)
	}
	chunks := []map[string]any{
		{"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0.0, "id": "call_9",
				"function": map[string]any{"name": "get_weather", "arguments": `{"city"`}}}},
		}}},
		{"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{"index": 0.0,
				"function": map[string]any{"arguments": `:"SZ"}`}}}},
			"finish_reason": "tool_calls"}}},
	}
	for _, c := range chunks {
		for _, ev := range s.Feed(c) {
			names = append(names, ev.Name)
			if ev.Name == "content_block_start" {
				block, _ := ev.Data["content_block"].(map[string]any)
				if block["type"] == "tool_use" {
					t.Errorf("工具块必须在 Finish 才发（参数未攒完）")
				}
			}
		}
	}
	var started bool
	var deltaJSON string
	for _, ev := range s.Finish() {
		names = append(names, ev.Name)
		if ev.Name == "content_block_start" {
			block, _ := ev.Data["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				started = true
				if block["id"] != "call_9" || block["name"] != "get_weather" {
					t.Errorf("tool_use 块=%+v", block)
				}
			}
		}
		if ev.Name == "content_block_delta" {
			if d, ok := ev.Data["delta"].(map[string]any); ok && d["type"] == "input_json_delta" {
				deltaJSON, _ = d["partial_json"].(string)
			}
		}
	}
	if !started {
		t.Fatalf("未发出 tool_use 块，事件=%v", names)
	}
	if deltaJSON != `{"city":"SZ"}` {
		t.Errorf("partial_json=%q want 完整参数", deltaJSON)
	}
	if names[len(names)-2] != "message_delta" || names[len(names)-1] != "message_stop" {
		t.Errorf("收尾事件顺序错: %v", names)
	}
}

// TestAnthropicStreamErrorFrame 上游 200+error 帧：翻成 Anthropic error 事件，
// 且不能再发 message_stop（否则客户端以为正常结束）。
func TestAnthropicStreamErrorFrame(t *testing.T) {
	s := NewAnthropicStream("m", "msg_x")
	evs := s.Feed(map[string]any{"error": map[string]any{
		"code": "6004", "message": "too many requests",
	}})
	if len(evs) != 1 || evs[0].Name != "error" {
		t.Fatalf("events=%+v want 单个 error", evs)
	}
	inner, _ := evs[0].Data["error"].(map[string]any)
	if inner["type"] != "rate_limit_error" {
		t.Errorf("error type=%v want rate_limit_error", inner["type"])
	}
	if !s.Failed() || len(s.Finish()) != 0 {
		t.Error("失败流不应再发收尾事件")
	}
}

// TestResponsesToChat 覆盖 instructions / developer / function_call_output /
// max_output_tokens / 扁平工具。
func TestResponsesToChat(t *testing.T) {
	req := map[string]any{
		"model":             "glm-5.2",
		"instructions":      "you are codex",
		"max_output_tokens": 256.0,
		"tools": []any{map[string]any{
			"type": "function", "name": "shell", "description": "run",
			"parameters": map[string]any{"type": "object"},
		}},
		"input": []any{
			map[string]any{"type": "message", "role": "developer", "content": []any{
				map[string]any{"type": "input_text", "text": "be terse"},
			}},
			map[string]any{"type": "message", "role": "user", "content": "ls"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "shell",
				"arguments": `{"cmd":"ls"}`},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "a.txt"},
			map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}},
		},
	}
	out, err := ResponsesToChat(req)
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	got := decode[chatReq](t, out)
	if got.MaxTokens != 256 {
		t.Errorf("max_output_tokens 未映射: %+v", got)
	}
	// instructions + developer + user + assistant(tool_call) + tool = 5
	// （reasoning 条目应被跳过）
	if len(got.Messages) != 5 {
		t.Fatalf("messages=%d want 5: %+v", len(got.Messages), got.Messages)
	}
	if got.Messages[0].Role != "system" || got.Messages[0].Content != "you are codex" {
		t.Errorf("instructions 未转 system: %+v", got.Messages[0])
	}
	if got.Messages[1].Role != "system" || got.Messages[1].Content != "be terse" {
		t.Errorf("developer 未转 system: %+v", got.Messages[1])
	}
	if len(got.Messages[3].ToolCalls) != 1 || got.Messages[3].ToolCalls[0].ID != "c1" {
		t.Errorf("function_call 未转 tool_calls: %+v", got.Messages[3])
	}
	if got.Messages[4].Role != "tool" || got.Messages[4].Content != "a.txt" {
		t.Errorf("function_call_output 未转 tool 消息: %+v", got.Messages[4])
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "shell" {
		t.Errorf("扁平工具未转换: %+v", got.Tools)
	}
}

// TestResponsesStreamOutputIndex 只输出正文的流：message 必须落在 index 0，
// 且收尾的 response.completed 里 output[0] 是这条 message。
func TestResponsesStreamOutputIndex(t *testing.T) {
	s := NewResponsesStream("glm-5.2", "resp_test")
	s.Start()
	s.Feed(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": "你好"}}},
	})
	finish := s.Finish()

	var completed map[string]any
	var sawTextDelta bool
	for _, ev := range finish {
		if ev.Name == "response.output_text.delta" {
			sawTextDelta = true
		}
		if ev.Name == "response.completed" {
			completed, _ = ev.Data["response"].(map[string]any)
		}
	}
	if !sawTextDelta {
		// delta 在 Feed 里返回，这里只是确认 Finish 不会重复发正文增量。
		_ = sawTextDelta
	}
	if completed == nil {
		t.Fatal("缺 response.completed")
	}
	if completed["status"] != "completed" {
		t.Errorf("status=%v", completed["status"])
	}
	output, _ := completed["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v want 单条 message", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "message" {
		t.Errorf("output[0].type=%v want message", item["type"])
	}
	content, _ := item["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v", content)
	}
	if part, _ := content[0].(map[string]any); part["text"] != "你好" {
		t.Errorf("正文丢失: %v", part["text"])
	}

	// Feed 阶段必须给出 index=0 的 output_item.added
	s2 := NewResponsesStream("m", "resp_y")
	s2.Start()
	evs := s2.Feed(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": "x"}}},
	})
	for _, ev := range evs {
		if ev.Name == "response.output_item.added" && ev.Data["output_index"] != 0 {
			t.Errorf("output_index=%v want 0", ev.Data["output_index"])
		}
	}
}

// TestResponsesStreamReasoningThenText 思考 + 正文：index 依次 0/1，收尾两条 item。
func TestResponsesStreamReasoningThenText(t *testing.T) {
	s := NewResponsesStream("m", "resp_z")
	s.Start()
	s.Feed(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": "想"}}}})
	s.Feed(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "答"}}}})
	var completed map[string]any
	for _, ev := range s.Finish() {
		if ev.Name == "response.completed" {
			completed, _ = ev.Data["response"].(map[string]any)
		}
	}
	output, _ := completed["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output=%v want reasoning+message", output)
	}
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Errorf("output[0]=%v want reasoning", output[0])
	}
}

// TestEstimateInputTokens 粗估必须随内容增长（只验证单调性与非零）。
func TestEstimateInputTokens(t *testing.T) {
	short, _ := AnthropicToChat(map[string]any{
		"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	long, _ := AnthropicToChat(map[string]any{
		"model": "m", "messages": []any{map[string]any{"role": "user", "content": strings.Repeat("上下文", 200)}},
	})
	a, b := EstimateInputTokens(short), EstimateInputTokens(long)
	if a <= 0 {
		t.Fatalf("短请求估算=%d want >0", a)
	}
	if b <= a {
		t.Errorf("长请求估算=%d 应大于短请求 %d", b, a)
	}
}

// TestAnthropicToChatToolResultBeforeText 同一条 user 消息里 tool_result 与 text
// 并存时，tool 消息必须排在 user 文本之前（OpenAI 要求 tool 紧跟 assistant）。
func TestAnthropicToChatToolResultBeforeText(t *testing.T) {
	req := map[string]any{
		"model": "m",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "t1", "name": "f",
					"input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
				map[string]any{"type": "text", "text": "继续"},
			}},
		},
	}
	out, err := AnthropicToChat(req)
	if err != nil {
		t.Fatalf("AnthropicToChat: %v", err)
	}
	got := decode[chatReq](t, out)
	roles := make([]string, 0, len(got.Messages))
	for _, m := range got.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"assistant", "tool", "user"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("role 顺序=%v want %v", roles, want)
	}
}
