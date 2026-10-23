package upstream

import "testing"

// TestCanonicalModelAliasHit 别名命中：kimi-k3.1 → kimi-k3-1。
func TestCanonicalModelAliasHit(t *testing.T) {
	if got := CanonicalModel("kimi-k3.1"); got != "kimi-k3-1" {
		t.Fatalf("CanonicalModel(kimi-k3.1)=%q want kimi-k3-1", got)
	}
}

// TestCanonicalModelPassThrough 非别名模型名原样透传（含规范名自身与相近但不应误伤的名字）。
func TestCanonicalModelPassThrough(t *testing.T) {
	for _, m := range []string{
		"kimi-k3-1",    // 规范名自身不动
		"kimi-k3",      // 老 K3 与 K3.1 是不同模型，不可吞
		"kimi-k3.1-pro", // 未坐实对应关系，暂不收（收了要改断言）
		"kimi-k2.7", "glm-5.3", "",
	} {
		if got := CanonicalModel(m); got != m {
			t.Errorf("CanonicalModel(%q)=%q want 原样", m, got)
		}
	}
}
