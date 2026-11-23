package upstream

import (
	"encoding/json"
	"testing"
)

// repairEmptyContent 的两种补充形态（2026-11-23）：
//
//  1. 多模态数组里的「结构化空文本」`[{"type":"text","text":""}]`——旧实现只处理
//     ""/null/缺键，这种形态会原样出站，是线上 11151 剩余的候选来源。
//  2. 时序：repairEmptyContent 此前跑在 sanitizeMessages 之前，而 sanitizeText
//     末尾 TrimSpace——整段内容都是指纹时会被洗成空串，等于兜底之后又制造一个
//     11151。现在统一在 sanitize 之后跑（见 PrepareBodyOptWithEffortsAndDefault）。
func TestRepairEmptyContentTextParts(t *testing.T) {
	t.Run("全空 text part → 单空格", func(t *testing.T) {
		in := msgs(t, `[{"role":"user","content":"hi"},`+
			`{"role":"assistant","content":[{"type":"text","text":""}]}]`)
		out, changed := repairEmptyContent(in)
		if !changed {
			t.Fatal("changed=false，期望命中结构化空文本")
		}
		if got := out[1].(map[string]any)["content"]; got != " " {
			t.Fatalf("content = %#v，期望单空格", got)
		}
	})

	t.Run("空白 text part → 单空格", func(t *testing.T) {
		in := msgs(t, `[{"role":"assistant","content":[{"type":"text","text":"   "}]}]`)
		out, changed := repairEmptyContent(in)
		if !changed {
			t.Fatal("changed=false，期望命中纯空白 text")
		}
		if got := out[0].(map[string]any)["content"]; got != " " {
			t.Fatalf("content = %#v，期望单空格", got)
		}
	})

	t.Run("多个 text part 全空 → 单空格", func(t *testing.T) {
		in := msgs(t, `[{"role":"system","content":[{"type":"text","text":""},{"type":"text","text":" "}]}]`)
		out, changed := repairEmptyContent(in)
		if !changed {
			t.Fatal("changed=false，期望命中")
		}
		if got := out[0].(map[string]any)["content"]; got != " " {
			t.Fatalf("content = %#v，期望单空格", got)
		}
	})

	// 以下三种必须保持原样：改动会破坏真实内容或实测已放行的形态。
	t.Run("含非空文本不动", func(t *testing.T) {
		raw := `[{"role":"assistant","content":[{"type":"text","text":""},{"type":"text","text":"答案"}]}]`
		in := msgs(t, raw)
		out, changed := repairEmptyContent(in)
		if changed {
			t.Fatal("含非空文本时不应改动")
		}
		if got, _ := json.Marshal(out[0].(map[string]any)["content"]); string(got) != `[{"text":"","type":"text"},{"text":"答案","type":"text"}]` {
			t.Fatalf("content 被改动: %s", got)
		}
	})

	t.Run("含图片 part 不动", func(t *testing.T) {
		raw := `[{"role":"assistant","content":[{"type":"text","text":""},` +
			`{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]`
		in := msgs(t, raw)
		if _, changed := repairEmptyContent(in); changed {
			t.Fatal("含图片 part 时不应改动")
		}
	})

	t.Run("空数组不动（实测放行）", func(t *testing.T) {
		in := msgs(t, `[{"role":"assistant","content":[]}]`)
		if _, changed := repairEmptyContent(in); changed {
			t.Fatal("空数组实测放行，不应改动")
		}
	})
}

// 端到端时序守卫：sanitize 把 content 洗成空串之后，兜底必须仍然生效。
// 旧顺序（先 repair 后 sanitize）在这个入参下会留下 content:""，直接 11151。
func TestPrepareBodyEmptyContentAfterSanitize(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","messages":[` +
		`{"role":"system","content":"cc_entrypoint=cli;"},` +
		`{"role":"user","content":"hi"}]}`)
	out := PrepareBodyOpt(body, true)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	ms := obj["messages"].([]any)
	sys := ms[0].(map[string]any)
	content, _ := sys["content"].(string)
	if content == "" {
		t.Fatal(`sanitize 之后 content 仍是空串：兜底时序失效（会触发上游 11151）`)
	}
	if content != " " {
		t.Fatalf("content = %q，期望单空格", content)
	}
	if role, _ := sys["role"].(string); role != "system" {
		t.Fatalf("role = %q，期望 system（位置语义不得变化）", role)
	}
}
