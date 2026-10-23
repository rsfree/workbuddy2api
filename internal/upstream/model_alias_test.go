package upstream

import "testing"

// TestCanonicalModelAliasHit 别名命中：官方点号写法 → 规范名。
func TestCanonicalModelAliasHit(t *testing.T) {
	for from, want := range map[string]string{
		"kimi-k3.1":     "kimi-k3-1",
		"kimi-k3.1-pro": "kimi-k3-1",
	} {
		if got := CanonicalModel(from); got != want {
			t.Errorf("CanonicalModel(%q)=%q want %q", from, got, want)
		}
	}
}

// TestCanonicalModelPassThrough 非别名模型名原样透传（含规范名自身与相近但不应误伤的名字）。
func TestCanonicalModelPassThrough(t *testing.T) {
	for _, m := range []string{
		"kimi-k3-1", // 规范名自身不动
		"kimi-k3",   // 老 K3 与 K3.1 是不同模型，不可吞
		// 官方高速版不收：上游无对应条目，别过去会得到"假高速版"（普通速度）。
		"kimi-k3.1-pro-highspeed",
		"kimi-k2.7", "glm-5.3", "",
	} {
		if got := CanonicalModel(m); got != m {
			t.Errorf("CanonicalModel(%q)=%q want 原样", m, got)
		}
	}
}
