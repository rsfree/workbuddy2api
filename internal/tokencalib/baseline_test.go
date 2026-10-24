package tokencalib

import (
	"math"
	"testing"
)

// TestBaselineLookupLongestPrefix 前缀匹配的语义：记录是入参的前缀才算命中，
// 多条命中取最长，模型不同不串。
func TestBaselineLookupLongestPrefix(t *testing.T) {
	b := NewBaseline(8)
	b.Record("m", []uint64{1, 2}, 100)
	b.Record("m", []uint64{1, 2, 3}, 150)
	b.Record("other", []uint64{1}, 999)

	// 入参更长：取最长的记录，matched 报告匹配了几条（增量从这里往后算）。
	if tok, matched, ok := b.Lookup("m", []uint64{1, 2, 3, 4}); !ok || tok != 150 || matched != 3 {
		t.Fatalf("lookup(1,2,3,4) = %d,%d,%v want 150,3,true", tok, matched, ok)
	}
	// 完全命中：matched == len(digests)，调用方据此跳过一切估算。
	if tok, matched, ok := b.Lookup("m", []uint64{1, 2}); !ok || tok != 100 || matched != 2 {
		t.Fatalf("lookup(1,2) = %d,%d,%v want 100,2,true", tok, matched, ok)
	}
	// 不是前缀（中间分叉）不能命中，否则会拿错基线。
	if _, _, ok := b.Lookup("m", []uint64{2, 3}); ok {
		t.Fatal("lookup(2,3) 不该命中")
	}
	// 记录比入参长也不算前缀。
	if _, _, ok := b.Lookup("m", []uint64{1}); ok {
		t.Fatal("lookup(1) 不该命中更长的记录")
	}
	// 跨模型不串：别的模型存过同样前缀，也不能被拿出来用。
	if tok, matched, ok := b.Lookup("other", []uint64{1, 2, 3}); !ok || tok != 999 || matched != 1 {
		t.Fatalf("lookup(other) = %d,%d,%v want 999,1,true", tok, matched, ok)
	}
}

// TestBaselineRecordUpdatesSameDigests 同指纹重放（客户端重试、并发重放）就地
// 更新，不能把同一个会话占成两个槽位。
func TestBaselineRecordUpdatesSameDigests(t *testing.T) {
	b := NewBaseline(4)
	b.Record("m", []uint64{7, 8}, 100)
	b.Record("m", []uint64{7, 8}, 120)

	if n := b.Snapshot()["entries"].(int); n != 1 {
		t.Fatalf("entries=%d want 1（重放不该占新槽位）", n)
	}
	if tok, _, _ := b.Lookup("m", []uint64{7, 8}); tok != 120 {
		t.Fatalf("tokens=%d want 120（后来的真值覆盖旧的）", tok)
	}
}

// TestBaselineEvictsOldest 满了以后按最后使用时间淘汰最旧的一条，容量恒定。
func TestBaselineEvictsOldest(t *testing.T) {
	b := NewBaseline(2)
	b.Record("m", []uint64{1}, 10)
	b.Record("m", []uint64{2}, 20)
	b.Record("m", []uint64{1}, 11) // 刷新 1 的序号
	b.Record("m", []uint64{3}, 30) // 该淘汰 2

	if n := b.Snapshot()["entries"].(int); n != 2 {
		t.Fatalf("entries=%d want 2", n)
	}
	if _, _, ok := b.Lookup("m", []uint64{1}); !ok {
		t.Fatal("被刷新的条目不该被淘汰")
	}
	if _, _, ok := b.Lookup("m", []uint64{2}); ok {
		t.Fatal("最旧条目应被淘汰")
	}
	if _, _, ok := b.Lookup("m", []uint64{3}); !ok {
		t.Fatal("新条目应存在")
	}
}

// TestBaselineRejectsBadInput 缺数据必须忽略而不是记成 0：把「上游没带 usage」
// 记成 0 token，会让下一次 count_tokens 直接返回 0，客户端以为上下文空了。
func TestBaselineRejectsBadInput(t *testing.T) {
	b := NewBaseline(4)
	b.Record("", []uint64{1}, 10)                   // 无模型
	b.Record("m", nil, 10)                          // 无消息
	b.Record("m", []uint64{1}, 0)                   // 无 usage
	b.Record("m", []uint64{1, 0}, 10)               // 含指纹 0（序列化失败）
	b.Record("m", make([]uint64, maxDigests+1), 10) // 超长不截断
	b.Record("m", []uint64{2}, -5)                  // 负值

	if n := b.Snapshot()["entries"].(int); n != 0 {
		t.Fatalf("entries=%d want 0（脏输入一条都不该进）", n)
	}
	if _, _, ok := b.Lookup("m", nil); ok {
		t.Fatal("空 digests 不该命中")
	}

	// 裸 nil 也是合法用法（Config.Baseline 未装配），不能崩。
	var nilB *Baseline
	nilB.Record("m", []uint64{1}, 10)
	nilB.Predict("m", []uint64{1}, 10)
	if _, _, ok := nilB.Lookup("m", []uint64{1}); ok {
		t.Fatal("nil Baseline 不该命中")
	}
	if nilB.Snapshot() != nil {
		t.Fatal("nil Baseline 的 Snapshot 应为 nil")
	}
}

// TestBaselinePredictScoredByRecord 预测与随后那次真实 usage 配对打分——
// 这是线上判断「估得准不准」的唯一数据来源。
func TestBaselinePredictScoredByRecord(t *testing.T) {
	b := NewBaseline(4)
	b.Predict("m", []uint64{1, 2}, 110)

	snap := b.Snapshot()
	if snap["pred_pending"].(int) != 1 || snap["pred_verified"].(int) != 0 {
		t.Fatalf("登记后 pending=%v verified=%v want 1,0", snap["pred_pending"], snap["pred_verified"])
	}

	b.Record("m", []uint64{1, 2}, 100) // 真实值来了：110 vs 100 → 10%

	snap = b.Snapshot()
	if snap["pred_pending"].(int) != 0 {
		t.Fatalf("配对后 pending=%v want 0", snap["pred_pending"])
	}
	if snap["pred_verified"].(int) != 1 {
		t.Fatalf("verified=%v want 1", snap["pred_verified"])
	}
	byModel := snap["pred_by_model"].(map[string]map[string]any)
	m := byModel["m"]
	if m == nil {
		t.Fatalf("pred_by_model 缺模型条目: %v", byModel)
	}
	if mae := m["mae"].(float64); math.Abs(mae-0.1) > 1e-6 {
		t.Fatalf("mae=%v want 0.1", mae)
	}
	if worst := m["worst"].(float64); math.Abs(worst-0.1) > 1e-6 {
		t.Fatalf("worst=%v want 0.1", worst)
	}
	if m["samples"].(int) != 1 {
		t.Fatalf("samples=%v want 1", m["samples"])
	}
}

// TestBaselinePredictNotScoredOnDifferentDigests 真值对应的不是这批消息时不能
// 乱配对：上下文被压缩后 messages 变短，拿它的 usage 去评旧预测会把 mae 算假。
func TestBaselinePredictNotScoredOnDifferentDigests(t *testing.T) {
	b := NewBaseline(4)
	b.Predict("m", []uint64{1, 2}, 110)
	b.Record("m", []uint64{1, 2, 3}, 999) // 更长的另一批，不等于预测的那批

	snap := b.Snapshot()
	if snap["pred_pending"].(int) != 1 || snap["pred_verified"].(int) != 0 {
		t.Fatalf("pending=%v verified=%v want 1,0", snap["pred_pending"], snap["pred_verified"])
	}
}

// TestBaselinePendingBounded 客户端探完就压缩时预测永远等不到真值，
// 靠先进先出顶掉，不能让待验证队列无限涨。
func TestBaselinePendingBounded(t *testing.T) {
	b := NewBaseline(4)
	for i := 0; i < maxPreds+4; i++ {
		b.Predict("m", []uint64{uint64(i + 1)}, 100+i)
	}
	if n := b.Snapshot()["pred_pending"].(int); n != maxPreds {
		t.Fatalf("pending=%d want %d", n, maxPreds)
	}
}
