package panel

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// 闸门语义：未熔断放行、熔断拦截、池为 nil 放行（未接线场景不能误伤）。
func TestRiskBlockedGate(t *testing.T) {
	p := &Panel{}
	if p.riskBlocked("测试") {
		t.Error("Pool 为 nil 时应放行")
	}

	pl := &pool.Pool{}
	p.cfg.Pool = pl
	if p.riskBlocked("测试") {
		t.Error("初始状态不该拦截")
	}

	// 打满阈值
	now := time.Now()
	for i := 0; i < pool.RiskThreshold; i++ {
		pl.NoteUpstreamBan(now)
	}
	if !p.riskBlocked("测试") {
		t.Error("达到阈值后必须拦截")
	}
}

// nil Panel 安全。
func TestRiskBlockedNilPanel(t *testing.T) {
	var p *Panel
	if p.riskBlocked("测试") {
		t.Error("nil Panel 不该 panic，也不该报拦截")
	}
}
