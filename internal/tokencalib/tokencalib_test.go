package tokencalib

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestAdjustWithoutHistory 无历史时必须是恒等变换：校准坏掉不能比粗估更差。
func TestAdjustWithoutHistory(t *testing.T) {
	c := New("")
	for _, est := range []int{0, 1, 37, 128000} {
		if got := c.Adjust("m", est); got != est {
			t.Fatalf("Adjust(%d)=%d want 恒等", est, got)
		}
	}
	// nil 接收者同样恒等（handler 未装配校准器时）。
	var nilC *Calibrator
	if got := nilC.Adjust("m", 500); got != 500 {
		t.Fatalf("nil Adjust(500)=%d want 500", got)
	}
}

// TestFitLinear 用 y=2x+300 造 60 对观测：参数应朝真值收敛（受先验收缩影响
// 会略偏低），且对长 prompt 的估算必须明显优于原始粗估。
func TestFitLinear(t *testing.T) {
	c := New("")
	for i := 0; i < 60; i++ {
		x := 1000 + i*150
		c.Observe("m", x, 2*x+300)
	}
	p := c.Params("m")
	if p.Samples != 60 {
		t.Fatalf("samples=%d want 60", p.Samples)
	}
	if p.Slope < 1.6 || p.Slope > 2.05 {
		t.Errorf("slope=%.2f want ≈2（含收缩）", p.Slope)
	}
	if p.Intercept < 150 || p.Intercept > 320 {
		t.Errorf("intercept=%.0f want ≈300（含收缩）", p.Intercept)
	}

	// 关键收益：估算误差从「差 2 倍」收敛到「差 15% 以内」。
	est := 20000
	want := 2*est + 300
	got := c.Adjust("m", est)
	if math.Abs(float64(got-want))/float64(want) > 0.15 {
		t.Errorf("Adjust(%d)=%d want ≈%d（±15%%）", est, got, want)
	}
}

// TestShrinkageSmallSamples 只有一条（甚至比例离谱的）观测时，参数必须被
// 先验压住：不能因为一次「实际 5 倍于估算」就让所有后续估算翻 5 倍。
func TestShrinkageSmallSamples(t *testing.T) {
	c := New("")
	c.Observe("m", 100, 500) // 比例 5
	p := c.Params("m")
	if p.Slope >= 2 {
		t.Fatalf("单样本 slope=%.2f 应被先验显著压低", p.Slope)
	}
	if got := c.Adjust("m", 100); got <= 100 || got > 300 {
		t.Fatalf("单样本 Adjust(100)=%d want 略高于 100 且远低于 500", got)
	}
}

// TestRatioFallback 估算值几乎恒定（例如全是短对话）时方差项趋零：只能定
// 比例、截距归零，绝不能解出天文数字的 slope。
func TestRatioFallback(t *testing.T) {
	c := New("")
	for i := 0; i < 40; i++ {
		c.Observe("m", 100, 250) // 恒定 x
	}
	p := c.Params("m")
	if p.Intercept != 0 {
		t.Errorf("intercept=%.1f want 0（x 无方差）", p.Intercept)
	}
	if p.Slope < 1.5 || p.Slope > 2.5 {
		t.Errorf("slope=%.2f want ≈2.5 附近", p.Slope)
	}
	if got := c.Adjust("m", 1000); got < 1500 || got > 2500 {
		t.Errorf("Adjust(1000)=%d want ≈2500", got)
	}
}

// TestClampOutliers 离谱观测（上游把缓存命中算进 prompt 或网关改了注入长度）
// 不能把参数推出合理区间。
func TestClampOutliers(t *testing.T) {
	c := New("")
	for i := 0; i < 200; i++ {
		x := 1000 + i*50
		c.Observe("m", x, 50*x) // 比例恒为 50（例如上游把缓存命中全算进 prompt）
	}
	p := c.Params("m")
	if p.Slope > maxSlope || p.Intercept > maxIntercept {
		t.Fatalf("参数越界: slope=%.2f intercept=%.0f", p.Slope, p.Intercept)
	}
	if p.Slope != maxSlope {
		t.Errorf("slope=%.2f want 被夹到 %.2f", p.Slope, maxSlope)
	}
}

// TestIgnoreInvalidObservations actual=0（上游没带 usage）不能被当成真实
// 输入长度学进去，否则会迅速把估算压到底。
func TestIgnoreInvalidObservations(t *testing.T) {
	c := New("")
	c.Observe("m", 1000, 0)
	c.Observe("m", 0, 1000)
	if p := c.Params("m"); p.Samples != 0 {
		t.Fatalf("samples=%d want 0（非法观测被忽略）", p.Samples)
	}
}

// TestPersistRoundTrip 重启后参数必须原样恢复：否则每次重启都会让
// Claude Code 的上下文估算跳一下。
func TestPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token-calib.json")
	c := New(path)
	for i := 0; i < 30; i++ {
		c.Observe("m", 1000+i*100, 3*(1000+i*100))
	}
	before := c.Params("m")
	c.Save()

	c2 := New(path)
	after := c2.Params("m")
	if after.Samples != before.Samples || math.Abs(after.Slope-before.Slope) > 1e-9 {
		t.Fatalf("恢复后参数漂移: before=%+v after=%+v", before, after)
	}
	if got, want := c2.Adjust("m", 5000), c.Adjust("m", 5000); got != want {
		t.Fatalf("恢复后 Adjust=%d want %d", got, want)
	}
}

// TestCorruptFileFallsBack 损坏的持久化文件只应导致「按无历史处理」，
// 不能让进程起不来。
func TestCorruptFileFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token-calib.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("准备坏文件: %v", err)
	}
	c := New(path)
	if got := c.Adjust("m", 777); got != 777 {
		t.Fatalf("坏文件时 Adjust(777)=%d want 777", got)
	}
}

// TestSnapshotNilSafe /status 在未装配校准器时也要能拿到空 map。
func TestSnapshotNilSafe(t *testing.T) {
	var c *Calibrator
	if snap := c.Snapshot(); snap == nil || len(snap) != 0 {
		t.Fatalf("nil Snapshot=%v want 空 map", snap)
	}
}

// TestShrinkageFadesWithSamples 先验必须随累计样本退干净。
//
// 边界（实测踩到）：早先把权重绑在**衰减后**的样本数上，而 decay 把有效样本
// 锁在 1/(1-0.98)=50 附近 ⇒ 无论来多少流量，权重都停在 50/58，slope 永远只有
// 真值的 86%，长上下文恒定偏小 14% 且无法自愈。
func TestShrinkageFadesWithSamples(t *testing.T) {
	c := New("")
	for i := 0; i < 200; i++ {
		x := 1000 + i*50
		c.Observe("m", x, 2*x+300)
	}
	p := c.Params("m")
	if p.Slope < 1.96 || p.Slope > 2.0 {
		t.Errorf("200 条后 slope=%.3f want ≈2（先验应已退到 <2%%）", p.Slope)
	}
	if p.Intercept < 288 || p.Intercept > 300 {
		t.Errorf("200 条后 intercept=%.1f want ≈300", p.Intercept)
	}
}

// TestRatioOnlyBandCapsSlope 单条「短请求 + 巨大固定开销」不能把斜率顶到天上。
//
// 边界（实测踩到）：x=10、actual=509 给出比值 50.9，早先被当成 slope 直接夹到
// 上限 6，紧接着的 count_tokens 从 10 跳到 60。x 无方差时 slope/intercept 本就
// 不可辨识，只在字符/token 比的现实带宽内微调才是诚实的回答。
func TestRatioOnlyBandCapsSlope(t *testing.T) {
	c := New("")
	c.Observe("m", 10, 509)
	p := c.Params("m")
	if p.Slope > ratioOnlyMaxSlope {
		t.Fatalf("单样本 slope=%.2f 超出无方差带宽上限 %.1f", p.Slope, ratioOnlyMaxSlope)
	}
	if got := c.Adjust("m", 10); got > 40 {
		t.Fatalf("单样本 Adjust(10)=%d want ≤40（宁可保守，等样本覆盖不同长度再放开）", got)
	}
}
