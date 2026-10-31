package pool

import (
	"testing"
	"time"
)

// 熔断口径（2026-11-01 定）：5 分钟窗口内 ≥3 次 11140 → 熔断 30 分钟。
// 这组用例把口径钉死，避免以后被"调松一点"改回去。
func TestRiskGuardTripsAtThreshold(t *testing.T) {
	g := NewRiskGuard()
	base := time.Date(2026, 11, 1, 0, 4, 0, 0, time.Local)

	if g.Note(base) {
		t.Error("第 1 次不该触发")
	}
	if g.Note(base.Add(time.Second)) {
		t.Error("第 2 次不该触发")
	}
	if !g.Note(base.Add(2 * time.Second)) {
		t.Error("第 3 次（达阈值）必须触发")
	}
	if !g.Tripped(base.Add(time.Minute)) {
		t.Error("触发后应处于熔断期")
	}
}

// 窗口滑出后旧命中必须失效：否则一天下来的 3 次零星封禁会被误判成"批量处置"。
func TestRiskGuardWindowExpiry(t *testing.T) {
	g := NewRiskGuard()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)

	g.Note(base)
	g.Note(base.Add(3 * time.Minute))
	// 第 3 次落在 5 分钟窗口之外（第一条已滑出）
	if g.Note(base.Add(6 * time.Minute)) {
		t.Error("首条已滑出窗口，不该触发")
	}
	if g.Tripped(base.Add(6 * time.Minute)) {
		t.Error("未达阈值不该处于熔断期")
	}
}

// 冷却到期自动恢复：不需要人工复位（但也不会自动重跑队列）。
func TestRiskGuardCooldownRecovers(t *testing.T) {
	g := NewRiskGuard()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	for i := 0; i < RiskThreshold; i++ {
		g.Note(base.Add(time.Duration(i) * time.Second))
	}
	if !g.Tripped(base.Add(time.Minute)) {
		t.Fatal("应已熔断")
	}
	// until 从**最后一次命中**起算（持续命中会持续延长），所以这里要越过 2s 偏移
	if g.Tripped(base.Add(RiskCooldown + 5*time.Second)) {
		t.Error("冷却结束应自动恢复")
	}
}

// 熔断期内再次命中不重复"触发"（避免日志刷屏），但计数照增。
func TestRiskGuardNoDuplicateTrip(t *testing.T) {
	g := NewRiskGuard()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	for i := 0; i < RiskThreshold; i++ {
		g.Note(base.Add(time.Duration(i) * time.Second))
	}
	if g.Note(base.Add(10 * time.Second)) {
		t.Error("熔断期内再次命中不该重复报告触发")
	}
	st := g.Snapshot(base.Add(10 * time.Second))
	if st.Trips != 1 {
		t.Errorf("Trips=%d want 1", st.Trips)
	}
	if st.Bans != int64(RiskThreshold+1) {
		t.Errorf("Bans=%d want %d", st.Bans, RiskThreshold+1)
	}
}

// 零值 / nil 安全：Pool 在测试里大量以 &Pool{} 形式出现，不能因此 panic。
func TestRiskGuardNilSafe(t *testing.T) {
	var g *RiskGuard
	if g.Tripped(time.Now()) {
		t.Error("nil guard 不该报熔断")
	}
	if g.Note(time.Now()) {
		t.Error("nil guard 不该报触发")
	}
	var p *Pool
	if p.RiskTripped(time.Now()) {
		t.Error("nil pool 不该报熔断")
	}
	if p.NoteUpstreamBan(time.Now()) {
		t.Error("nil pool 不该报触发")
	}
	// 零值 Pool（未 New）也要能用
	var zp Pool
	if zp.RiskTripped(time.Now()) {
		t.Error("零值 Pool 初始不该熔断")
	}
}
