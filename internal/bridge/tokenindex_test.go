package bridge

import "testing"

// TestIndexMessagesPrefixAndTail 前缀累计与尾部增量的口径。
//
// 尾部增量刻意不含 tools：基线里的真实值已经包含了 tools 与 system 模板的固定
// 开销，再叠一次会把工具定义算两遍（Claude Code 的 tools 声明有上千 token，
// 重复计算的偏差远大于估算本身的误差）。
func TestIndexMessagesPrefixAndTail(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello world"},
			map[string]any{"role": "assistant", "content": "hi there"},
		},
		"tools": []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "get_weather",
					"description": "query the weather of a city",
					"parameters":  map[string]any{"type": "object"},
				},
			},
		},
	}
	idx := IndexMessages(body)

	if len(idx.Digests) != 2 || len(idx.Prefix) != 3 {
		t.Fatalf("digests=%d prefix=%d want 2,3", len(idx.Digests), len(idx.Prefix))
	}
	if idx.Prefix[0] != 0 {
		t.Fatalf("prefix[0]=%d want 0", idx.Prefix[0])
	}
	if idx.Prefix[1] <= 0 || idx.Prefix[2] <= idx.Prefix[1] {
		t.Fatalf("prefix 未单调递增: %v", idx.Prefix)
	}

	tools := estimateTools(body)
	if tools <= 0 {
		t.Fatalf("tools 估算=%d want >0（用例里声明了 tools）", tools)
	}
	if idx.Total != idx.Prefix[2]+tools {
		t.Fatalf("total=%d want %d", idx.Total, idx.Prefix[2]+tools)
	}

	if got := idx.TailTokens(0); got != idx.Prefix[2] {
		t.Fatalf("TailTokens(0)=%d want %d（全部 messages）", got, idx.Prefix[2])
	}
	if got := idx.TailTokens(1); got != idx.Prefix[2]-idx.Prefix[1] {
		t.Fatalf("TailTokens(1)=%d want %d（只有第 2 条）", got, idx.Prefix[2]-idx.Prefix[1])
	}
	// 完全命中时 matched == len(digests)，增量必须是 0，否则「上游原话」会被加上一段虚构值。
	if got := idx.TailTokens(2); got != 0 {
		t.Fatalf("TailTokens(2)=%d want 0", got)
	}
	if got := idx.TailTokens(9); got != 0 {
		t.Fatalf("越界 TailTokens=%d want 0", got)
	}
	if got := idx.TailTokens(-1); got != idx.Prefix[2] {
		t.Fatalf("负数 from 应按 0 处理，got=%d", got)
	}
}

// TestDigestMessageIdentifies 指纹要能认出「同一条消息」，也要区分「等长但不同」——
// 只按长度或 role 做键会把不同内容误判为同一批，进而拿错历史基线。
func TestDigestMessageIdentifies(t *testing.T) {
	a := map[string]any{"role": "user", "content": "abcdef"}
	b := map[string]any{"role": "user", "content": "abcdeg"} // 与 a 等长
	c := map[string]any{"role": "user", "content": "abcdef"} // 与 a 内容相同

	if digestMessage(a) == 0 {
		t.Fatal("正常消息不该得到指纹 0（0 被当作序列化失败）")
	}
	if digestMessage(a) != digestMessage(c) {
		t.Fatal("内容相同的消息指纹必须相同")
	}
	if digestMessage(a) == digestMessage(b) {
		t.Fatal("等长不同内容的消息指纹必须不同")
	}
}

// TestIndexMessagesOrderSensitive 顺序敏感：同样两条消息换个顺序，前缀索引
// 必须不同，否则「拿上一轮基线 + 这轮增量」会算出错位的结果。
func TestIndexMessagesOrderSensitive(t *testing.T) {
	m1 := map[string]any{"role": "user", "content": "one"}
	m2 := map[string]any{"role": "assistant", "content": "two"}

	x := IndexMessages(map[string]any{"messages": []any{m1, m2}})
	y := IndexMessages(map[string]any{"messages": []any{m2, m1}})

	if len(x.Digests) != 2 || len(y.Digests) != 2 {
		t.Fatalf("digests 长度异常: %d / %d", len(x.Digests), len(y.Digests))
	}
	if x.Digests[0] == y.Digests[0] {
		t.Fatal("换序后首位指纹不该相同")
	}
}

// TestIndexMessagesSkipsNonObject 非对象元素（上游偶发脏数据）跳过而不是崩，
// 但 Prefix 与 Digests 必须同步，否则 matched 与 Prefix 下标错位。
func TestIndexMessagesSkipsNonObject(t *testing.T) {
	idx := IndexMessages(map[string]any{
		"messages": []any{"garbage", map[string]any{"role": "user", "content": "ok"}, 42},
	})
	if len(idx.Digests) != 1 {
		t.Fatalf("digests=%d want 1", len(idx.Digests))
	}
	if len(idx.Prefix) != len(idx.Digests)+1 {
		t.Fatalf("prefix=%d digests=%d 不同步", len(idx.Prefix), len(idx.Digests))
	}
}
