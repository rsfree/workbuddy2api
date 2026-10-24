package upstream

import (
	"encoding/json"
	"testing"
)

// repairEmptyContent 的判据来自 2026-10-24 对 cn:kimi-k2.7 的逐形态实测（见
// payload.go 注释里的矩阵）。核心不是「content 为空就修」，而是：
// assistant/system 且无 tool_calls 时，content 为 ""/null/缺键才触发 11151。
// 带 tool_calls 的消息、user、tool、空白串、"[]" 实测全部放行，不能动。

func TestRepairEmptyContent(t *testing.T) {
	t.Run("assistant 空串 → 单空格", func(t *testing.T) {
		in := msgs(t, `[{"role":"user","content":"hi"},{"role":"assistant","content":""},{"role":"user","content":"next"}]`)
		out, changed := repairEmptyContent(in)
		if !changed {
			t.Fatal("changed=false，期望命中")
		}
		if got := out[1].(map[string]any)["content"]; got != " " {
			t.Errorf("content = %q，期望单空格", got)
		}
		// 条数与顺序零变化（位置语义依赖它）；summarize 只吐 role(ids)。
		assertSummary(t, summarize(out), []string{"user(-)", "assistant(-)", "user(-)"})
	})

	t.Run("assistant null → 单空格", func(t *testing.T) {
		// 第一版只堵了 ""，实测发现 null 同样 11151（线上曾用 deepseek 测 null 通过，
		// 误以为通用；kimi 系才拒）。
		in := msgs(t, `[{"role":"assistant","content":null}]`)
		out, changed := repairEmptyContent(in)
		if !changed {
			t.Fatal("changed=false，期望命中 null")
		}
		if got := out[0].(map[string]any)["content"]; got != " " {
			t.Errorf("content = %#v，期望单空格", got)
		}
	})

	t.Run("assistant 缺 content 键 → 补单空格", func(t *testing.T) {
		in := msgs(t, `[{"role":"assistant"}]`)
		out, changed := repairEmptyContent(in)
		if !changed {
			t.Fatal("changed=false，期望命中缺键")
		}
		if got := out[0].(map[string]any)["content"]; got != " " {
			t.Errorf("content = %#v，期望单空格", got)
		}
	})

	t.Run("system 三种空形态同样命中", func(t *testing.T) {
		cases := []struct {
			name string
			src  string
		}{
			{"空串", `[{"role":"system","content":""}]`},
			{"null", `[{"role":"system","content":null}]`},
			{"缺键", `[{"role":"system"}]`},
		}
		for _, c := range cases {
			out, changed := repairEmptyContent(msgs(t, c.src))
			if !changed {
				t.Fatalf("%s: changed=false，期望命中", c.name)
			}
			if got := out[0].(map[string]any)["content"]; got != " " {
				t.Errorf("%s: content = %#v，期望单空格", c.name, got)
			}
		}
	})

	t.Run("带 tool_calls 的空 content 一律不动", func(t *testing.T) {
		// tool_calls 消息的 content 标准形状就是 null，改它反而破坏规范形态，
		// 且实测该形态本就 200 放行。
		for _, c := range []struct {
			name string
			src  string
		}{
			{"null", `[{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]},{"role":"tool","tool_call_id":"c1","content":"r"}]`},
			{"空串", `[{"role":"assistant","content":"","tool_calls":[{"id":"c1"}]},{"role":"tool","tool_call_id":"c1","content":"r"}]`},
			{"缺键", `[{"role":"assistant","tool_calls":[{"id":"c1"}]},{"role":"tool","tool_call_id":"c1","content":"r"}]`},
		} {
			in := msgs(t, c.src)
			before, _ := json.Marshal(in[0])
			out, changed := repairEmptyContent(in)
			if changed {
				t.Errorf("%s: changed=true，带 tool_calls 不该被改", c.name)
			}
			after, _ := json.Marshal(out[0])
			if string(before) != string(after) {
				t.Errorf("%s: 消息被改动\nbefore=%s\nafter=%s", c.name, before, after)
			}
		}
	})

	t.Run("user 空 content 不动", func(t *testing.T) {
		// 实测 user ""/null 放行；动它没有收益。
		for _, src := range []string{
			`[{"role":"user","content":""}]`,
			`[{"role":"user","content":null}]`,
			`[{"role":"user"}]`,
		} {
			if _, changed := repairEmptyContent(msgs(t, src)); changed {
				t.Errorf("%s: changed=true，user 不该被改", src)
			}
		}
	})

	t.Run("tool 空 content 不动", func(t *testing.T) {
		in := msgs(t, `[{"role":"assistant","content":"x","tool_calls":[{"id":"c1"}]},{"role":"tool","tool_call_id":"c1","content":""}]`)
		if _, changed := repairEmptyContent(in); changed {
			t.Error("changed=true，tool 空内容实测放行、不该被改")
		}
	})

	t.Run("空白串不动", func(t *testing.T) {
		in := msgs(t, `[{"role":"assistant","content":"   "}]`)
		if _, changed := repairEmptyContent(in); changed {
			t.Error("changed=true，空白串实测 200、不该被改")
		}
	})

	t.Run("空数组不动", func(t *testing.T) {
		in := msgs(t, `[{"role":"assistant","content":[]}]`)
		out, changed := repairEmptyContent(in)
		if changed {
			t.Error("changed=true，[] 实测放行、不该被改")
		}
		if got, ok := out[0].(map[string]any)["content"].([]any); !ok || len(got) != 0 {
			t.Errorf("content = %#v，期望仍是空数组", out[0].(map[string]any)["content"])
		}
	})

	t.Run("正常文本不动", func(t *testing.T) {
		in := msgs(t, `[{"role":"user","content":"hello"},{"role":"assistant","content":"world"}]`)
		if _, changed := repairEmptyContent(in); changed {
			t.Error("changed=true，正常文本不该被改")
		}
	})

	t.Run("空 messages 直接返回", func(t *testing.T) {
		out, changed := repairEmptyContent([]any{})
		if changed || len(out) != 0 {
			t.Errorf("changed=%v len=%d，期望 false/0", changed, len(out))
		}
	})

	t.Run("非 map 元素跳过且条数不变", func(t *testing.T) {
		in := []any{
			map[string]any{"role": "system", "content": ""},
			"not-a-map",
			map[string]any{"role": "assistant"},
		}
		out, changed := repairEmptyContent(in)
		if !changed {
			t.Fatal("changed=false，期望命中")
		}
		if len(out) != 3 {
			t.Fatalf("len=%d，期望 3", len(out))
		}
		if out[1] != "not-a-map" {
			t.Error("非 map 元素应原样保留")
		}
	})
}

// 管线级：修复必须落在真正出站的 body 上（只测函数不测管线等于没测）。
func TestPrepareBodyRepairsEmptyContent(t *testing.T) {
	decode := func(t *testing.T, src string) []any {
		t.Helper()
		out := PrepareBodyOpt([]byte(src), false)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return obj["messages"].([]any)
	}

	t.Run("三种空形态经全链路后都不再为空", func(t *testing.T) {
		cases := map[string]string{
			"assistant 空串": `{"model":"kimi-k2.7","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":""},{"role":"user","content":"next"}]}`,
			"assistant null": `{"model":"kimi-k2.7","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null},{"role":"user","content":"next"}]}`,
			"assistant 缺键":  `{"model":"kimi-k2.7","messages":[{"role":"user","content":"hi"},{"role":"assistant"},{"role":"user","content":"next"}]}`,
			"system 空串":    `{"model":"kimi-k2.7","messages":[{"role":"system","content":""},{"role":"user","content":"hi"}]}`,
		}
		for name, src := range cases {
			for i, raw := range decode(t, src) {
				m := raw.(map[string]any)
				if m["role"] == "user" {
					continue
				}
				if s, ok := m["content"].(string); !ok || s == "" {
					t.Errorf("%s: 第 %d 条出站 content = %#v，仍是空（会触发 11151）", name, i, m["content"])
				}
			}
		}
	})

	t.Run("cleanup 摘掉孤儿 tool_calls 后留下的裸空消息也被修", func(t *testing.T) {
		// 真实路径（与 tool_pairing_test.go「空name带结果」同形）：空 name 的调用与
		// 其结果被 cleanup 成对剔除，assistant 保留 → 变成无 tool_calls 的空消息 → 11151。
		ms := decode(t, `{"model":"kimi-k2.7","messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c1","content":"r1"},
			{"role":"user","content":"next"}]}`)
		for i, raw := range ms {
			m := raw.(map[string]any)
			if s, ok := m["content"].(string); ok && s == "" {
				t.Errorf("第 %d 条出站 content 仍是空串（会触发 11151）", i)
			}
		}
	})

	t.Run("合法 tool_calls 消息的 null content 在管线内保持 null", func(t *testing.T) {
		ms := decode(t, `{"model":"kimi-k2.7","messages":[
			{"role":"user","content":"q"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"c9","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"c9","content":"r"}]}`)
		if len(ms) != 3 {
			t.Fatalf("消息数 = %d，期望 3（合法配对不该被删）", len(ms))
		}
		if got := ms[1].(map[string]any)["content"]; got != nil {
			t.Errorf("assistant content = %#v，期望保持 null", got)
		}
	})

	t.Run("user 空内容与请求字段不受影响", func(t *testing.T) {
		out := PrepareBodyOpt([]byte(`{"model":"kimi-k2.7","messages":[{"role":"user","content":""}]}`), false)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if obj["model"] != "kimi-k2.7" {
			t.Errorf("model = %v，被改动", obj["model"])
		}
		if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != "" {
			t.Errorf("user content = %#v，期望保持空串（实测放行）", got)
		}
	})
}
