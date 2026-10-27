package server

import (
	"strings"
	"testing"
	"time"
)

// TestPrefixLostSignal 判据：只有「前缀折算 token 显著超过整个 prompt」才告警，
// 正常请求（前缀是 prompt 的一部分）永不误报；同模型在冷却内只报一次。
func TestPrefixLostSignal(t *testing.T) {
	old := prefixLostWarnEvery
	prefixLostWarnEvery = time.Hour
	defer func() { prefixLostWarnEvery = old }()

	fresh := func() *prefixLostSignal { return &prefixLostSignal{last: map[string]time.Time{}} }

	// 实案：115,640 B ≈ 28,910 tok 前缀，上游只计 130 tok
	s := fresh()
	fired, msg := s.note("kimi-k3.1-pro", 115640, 130, false)
	if !fired {
		t.Fatal("115,640B 前缀 vs prompt=130 必须告警")
	}
	if !strings.Contains(msg, "115640") || !strings.Contains(msg, "prompt=130") {
		t.Errorf("告警应带具体数字: %s", msg)
	}
	if !strings.Contains(msg, "tool_choice=none") {
		t.Errorf("告警应给出最常见原因: %s", msg)
	}
	// 冷却内重复同模型不再报
	if fired2, _ := s.note("kimi-k3.1-pro", 115640, 130, false); fired2 {
		t.Error("冷却内不应重复告警")
	}
	// 冷却按模型隔离：别的模型仍要报
	if fired3, _ := s.note("deepseek-v4.1-flash", 40000, 100, false); !fired3 {
		t.Error("不同模型应各自独立计数")
	}

	// 正常：前缀只是 prompt 的一部分 → 不报
	if f, _ := fresh().note("m", 40000, 20000, false); f {
		t.Error("前缀 10k tok / prompt 20k tok 属正常，不该告警")
	}
	// 边界：中文前缀 bytes/4 低估一半，1.4 倍仍在容忍内（不报）
	if f, _ := fresh().note("m", 40000, 7000, false); f {
		t.Error("1.43 倍属中文低估容忍范围，不该告警")
	}
	// 2 倍以上必报
	if f, _ := fresh().note("m", 40000, 4000, false); !f {
		t.Error("2.5 倍背离必须告警")
	}
	// 无观测/无前缀 → 不报
	if f, _ := fresh().note("m", 0, 100, false); f {
		t.Error("无前缀材料不该告警")
	}
	if f, _ := fresh().note("m", 5000, 0, false); f {
		t.Error("无 prompt 观测不该告警")
	}
	if f, _ := fresh().note("", 5000, 100, false); f {
		t.Error("无模型名不该告警")
	}

	// 客户端自己声明 none → 剥离属预期，即使背离再大也不告警（否则每个
	// 纯文本会话都刷一条 WARN，真异常会被噪音埋掉）。
	if f, _ := fresh().note("m", 115640, 130, true); f {
		t.Error("声明 none 的预期剥离不该告警")
	}
}

// TestRequiredIgnoredSignal 「要求必调工具却 0 调用」告警的判据。
//
// 上游实测忽略 tool_choice 取值，这条 WARN 是假设失效点的探针：一旦上游开始
// 真正实现该字段（required 生效）或改成硬报错，行为变化会先在这里露出来。
func TestRequiredIgnoredSignal(t *testing.T) {
	old := requiredIgnoredEvery
	requiredIgnoredEvery = time.Hour
	defer func() { requiredIgnoredEvery = old }()

	fresh := func() *requiredIgnoredSignal { return &requiredIgnoredSignal{last: map[string]time.Time{}} }

	// 要求必调 + 0 调用 → 告警
	fired, msg := fresh().note("kimi-k3.1-pro", true, 0)
	if !fired {
		t.Fatal("required + 0 tool_calls 必须告警")
	}
	if !strings.Contains(msg, "required") || !strings.Contains(msg, "kimi-k3.1-pro") {
		t.Errorf("告警文案缺关键信息: %s", msg)
	}

	// 有调用 → 静默
	if f, _ := fresh().note("m", true, 2); f {
		t.Error("确有 tool_calls 时不该告警")
	}
	// 没声明 required → 静默（auto 不调工具是正常的，模型自主决定）
	if f, _ := fresh().note("m", false, 0); f {
		t.Error("未声明 required 时不该告警")
	}
	// 空模型名 → 静默
	if f, _ := fresh().note("", true, 0); f {
		t.Error("无模型名不该告警")
	}

	// 每模型冷却：同模型只报一次，别的模型独立
	s := fresh()
	if f, _ := s.note("m1", true, 0); !f {
		t.Fatal("首次应告警")
	}
	if f, _ := s.note("m1", true, 0); f {
		t.Error("冷却内不应重复告警")
	}
	if f, _ := s.note("m2", true, 0); !f {
		t.Error("不同模型应独立计数")
	}
}

// TestResponseToolCallCount 非流式响应的 tool_calls 计数，覆盖 message/delta 两种摆放。
func TestResponseToolCallCount(t *testing.T) {
	mk := func(choices ...any) map[string]any { return map[string]any{"choices": choices} }
	msg := func(n int) map[string]any {
		tcs := make([]any, n)
		for i := range tcs {
			tcs[i] = map[string]any{"id": "c"}
		}
		return map[string]any{"message": map[string]any{"tool_calls": tcs}}
	}
	cases := []struct {
		name string
		resp map[string]any
		want int
	}{
		{"无 choices", map[string]any{}, 0},
		{"纯文本", mk(map[string]any{"message": map[string]any{"content": "hi"}}), 0},
		{"单次调用", mk(msg(1)), 1},
		{"并行 3 个", mk(msg(3)), 3},
		{"delta 摆放", mk(map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"id": "c1"}, map[string]any{"id": "c2"}}}}), 2},
		{"choices 非数组", map[string]any{"choices": "bad"}, 0},
		// 🔴 网关自身 Aggregate 产出的形态：tool_calls 是 []map[string]any 而非 []any。
		// 只认 []any 会让「客户端收到 N 个调用」被数成 0，进而把正常轮次误判成跳过。
		{"Aggregate 原生形态 []map[string]any", mk(map[string]any{"message": map[string]any{
			"tool_calls": []map[string]any{{"id": "a"}, {"id": "b"}}}}), 2},
	}
	for _, c := range cases {
		if got := responseToolCallCount(c.resp); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}
