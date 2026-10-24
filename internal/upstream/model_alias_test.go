package upstream

import "testing"

// TestCanonicalModelAliasHit 别名命中：官方点号写法 → 规范名。
func TestCanonicalModelAliasHit(t *testing.T) {
	for from, want := range map[string]string{
		// kimi-k3.1 上游无此 id，K3.1 系列仅 Pro 一款 ⇒ 投 k3.1-pro
		// （曾误投 k3-1，导致贵 67% 且档位被压平，2026-10-24 更正）。
		"kimi-k3.1": "kimi-k3.1-pro",
	} {
		if got := CanonicalModel(from); got != want {
			t.Errorf("CanonicalModel(%q)=%q want %q", from, got, want)
		}
	}
}

// TestCanonicalModelPassThrough 非别名模型名原样透传（含规范名自身与相近但不应误伤的名字）。
func TestCanonicalModelPassThrough(t *testing.T) {
	for _, m := range []string{
		"kimi-k3-1",     // 规范名自身不动
		"kimi-k3.1-pro", // 上游真实 id，绝不改写（改写=劫持到另一个价位的模型）
		"kimi-k3",       // 老 K3 与 K3.1 是不同模型，不可吞
		// 高速版不收：上游存在但按账号类型拒绝（11158），映射会变成"假高速版"。
		"kimi-k3.1-pro-highspeed",
		"kimi-k2.7", "glm-5.3", "",
	} {
		if got := CanonicalModel(m); got != m {
			t.Errorf("CanonicalModel(%q)=%q want 原样", m, got)
		}
	}
}
