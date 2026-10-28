// usage_alias_test.go 桥接层缓存字段透出的回归测试。
//
// 背景（2026-10-28 实案）：kimi-k3.1-pro 的网关账本记到 70.7% 缓存命中、3200 万
// 命中 token，但客户端拿到的响应里**一个缓存字段都没有**。原因是 chat→目标协议的
// usage 重映射漏字段：
//
//	· Anthropic 非流式只读 prompt_tokens_details.cached_tokens 一个别名
//	· Anthropic 流式整段只有 output_tokens（连 input_tokens 都没有）
//	· Responses 两条都没有 input_tokens_details.cached_tokens
//
// 这些字段是客户端算钱的唯一依据，缺了就等于把"打了 0.1 折的读取"按原价记。
package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestUsageCacheReadAliases 五个别名任意一个带真值都要认出来。
//
// 上游会同时写全部别名，但兼容别名常被填 0（真实命中在 prompt_cache_hit_tokens
// 或 cache_read_input_tokens 里）。只认某一个键就会漏。
func TestUsageCacheReadAliases(t *testing.T) {
	cases := []struct {
		name  string
		usage map[string]any
		want  int
	}{
		{"prompt_tokens_details.cached_tokens",
			map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": float64(1792)}}, 1792},
		{"prompt_cache_hit_tokens（兼容别名 cached_tokens=0）",
			map[string]any{"prompt_cache_hit_tokens": float64(1792), "cached_tokens": float64(0)}, 1792},
		{"cache_read_input_tokens",
			map[string]any{"cache_read_input_tokens": float64(1792)}, 1792},
		{"顶层 cached_tokens",
			map[string]any{"cached_tokens": float64(1792)}, 1792},
		{"input_tokens_details.cached_tokens",
			map[string]any{"input_tokens_details": map[string]any{"cached_tokens": float64(1792)}}, 1792},
		{"全零 → 无观测", map[string]any{"prompt_cache_hit_tokens": float64(0)}, 0},
		{"字段缺失 → 无观测", map[string]any{"prompt_tokens": float64(100)}, 0},
		{"nil usage", nil, 0},
	}
	for _, c := range cases {
		got, ok := usageCacheRead(c.usage)
		if c.want == 0 {
			if ok {
				t.Errorf("%s: 期望无观测，got %d", c.name, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("%s: got (%d,%v) want (%d,true)", c.name, got, ok, c.want)
		}
	}
}

// TestUsageCacheWrite 写入侧两个别名取先非零者。
func TestUsageCacheWrite(t *testing.T) {
	if got := usageCacheWrite(map[string]any{"cache_creation_input_tokens": float64(50)}); got != 50 {
		t.Errorf("cache_creation_input_tokens: got %d", got)
	}
	if got := usageCacheWrite(map[string]any{"prompt_cache_write_tokens": float64(70)}); got != 70 {
		t.Errorf("prompt_cache_write_tokens 兜底: got %d", got)
	}
	if got := usageCacheWrite(map[string]any{"prompt_tokens": float64(1)}); got != 0 {
		t.Errorf("无写入字段应为 0，got %d", got)
	}
}

// TestChatToAnthropicCacheRead Anthropic 非流式的 cache_read_input_tokens 必须出现。
//
// 用「真实命中在 prompt_cache_hit_tokens、兼容别名 cached_tokens=0」这个最容易漏的
// 组合：修复前只读 prompt_tokens_details.cached_tokens，会整段丢掉命中。
func TestChatToAnthropicCacheRead(t *testing.T) {
	obj := map[string]any{
		"id": "chatcmpl-1",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "ok"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":           float64(2025),
			"completion_tokens":       float64(8),
			"prompt_cache_hit_tokens": float64(1792),
			"cached_tokens":           float64(0), // 兼容别名是 0，不能采信
		},
	}
	out := ChatToAnthropic(obj, "kimi-k3.1-pro")
	usage, _ := out["usage"].(map[string]any)
	if got := IntOf(usage["cache_read_input_tokens"]); got != 1792 {
		t.Errorf("cache_read_input_tokens=%d want 1792（usage=%v）", got, usage)
	}
	if got := IntOf(usage["input_tokens"]); got != 2025 {
		t.Errorf("input_tokens=%d want 2025", got)
	}

	// 未命中时不输出该字段（而不是输出 0）
	obj2 := map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": "x"}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": float64(87), "completion_tokens": float64(2)},
	}
	u2, _ := ChatToAnthropic(obj2, "m")["usage"].(map[string]any)
	if _, exists := u2["cache_read_input_tokens"]; exists {
		t.Errorf("未命中时不该输出 cache_read_input_tokens: %v", u2)
	}
}

// TestAnthropicStreamUsageHasInputAndCache 流式 message_delta 的 usage 必须带
// input_tokens 与 cache_read_input_tokens。
//
// 修复前这里只有 output_tokens(+credit)：客户端看到的永远是"0 输入、0 缓存"。
func TestAnthropicStreamUsageHasInputAndCache(t *testing.T) {
	s := NewAnthropicStream("kimi-k3.1-pro", "msg_x")
	s.Start()
	s.Feed(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"content": "ok"}, "finish_reason": "stop", "index": 0,
		}},
		"usage": map[string]any{
			"prompt_tokens":           float64(2025),
			"completion_tokens":       float64(8),
			"prompt_cache_hit_tokens": float64(1792),
			"cache_read_input_tokens": float64(1792),
			"credit":                  float64(0.04),
		},
	})

	var deltaUsage map[string]any
	found := false
	for _, ev := range s.Finish() {
		if ev.Name != "message_delta" {
			continue
		}
		found = true
		deltaUsage, _ = ev.Data["usage"].(map[string]any)
	}
	if !found || deltaUsage == nil {
		t.Fatal("没有 message_delta 事件或其中没有 usage")
	}
	if got := IntOf(deltaUsage["input_tokens"]); got != 2025 {
		t.Errorf("input_tokens=%d want 2025（usage=%v）", got, deltaUsage)
	}
	if got := IntOf(deltaUsage["output_tokens"]); got != 8 {
		t.Errorf("output_tokens=%d want 8", got)
	}
	if got := IntOf(deltaUsage["cache_read_input_tokens"]); got != 1792 {
		t.Errorf("cache_read_input_tokens=%d want 1792", got)
	}
}

// TestChatToResponseCacheInputDetails Responses 非流式要带 input_tokens_details.cached_tokens。
func TestChatToResponseCacheInputDetails(t *testing.T) {
	obj := map[string]any{
		"id": "chatcmpl-1",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "ok"},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":           float64(2025),
			"completion_tokens":       float64(8),
			"prompt_cache_hit_tokens": float64(1792),
		},
	}
	raw, err := json.Marshal(ChatToResponse(obj, "kimi-k3.1-pro"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	usage, _ := out["usage"].(map[string]any)
	details, _ := usage["input_tokens_details"].(map[string]any)
	if got := IntOf(details["cached_tokens"]); got != 1792 {
		t.Errorf("input_tokens_details.cached_tokens=%d want 1792（usage=%v）", got, usage)
	}
}

// TestResponsesStreamUsageCacheInputDetails 流式 response.completed 的 usage 同样要带。
func TestResponsesStreamUsageCacheInputDetails(t *testing.T) {
	s := NewResponsesStream("kimi-k3.1-pro", "resp_x")
	s.Start()
	s.Feed(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"content": "ok"}, "finish_reason": "stop", "index": 0,
		}},
		"usage": map[string]any{
			"prompt_tokens":           float64(2025),
			"completion_tokens":       float64(8),
			"prompt_cache_hit_tokens": float64(1792),
		},
	})

	var usage map[string]any
	for _, ev := range s.Finish() {
		if ev.Name != "response.completed" {
			continue
		}
		resp, _ := ev.Data["response"].(map[string]any)
		usage, _ = resp["usage"].(map[string]any)
	}
	if usage == nil {
		t.Fatal("response.completed 里没有 usage")
	}
	details, _ := usage["input_tokens_details"].(map[string]any)
	if got := IntOf(details["cached_tokens"]); got != 1792 {
		t.Errorf("input_tokens_details.cached_tokens=%d want 1792（usage=%v）", got, usage)
	}
	if got := IntOf(usage["input_tokens"]); got != 2025 {
		t.Errorf("input_tokens=%d want 2025", got)
	}
	// 不该把上游的内部别名泄漏进目标协议
	if _, exists := usage["prompt_cache_hit_tokens"]; exists {
		t.Errorf("不应把 chat 侧别名透给 Responses 客户端: %v", usage)
	}
	if strings.Contains(string(mustJSON(t, usage)), "prompt_cache_miss_tokens") {
		t.Errorf("不应泄漏 prompt_cache_miss_tokens: %v", usage)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
