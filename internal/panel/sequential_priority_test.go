// sequential_priority_test.go Sequential 串行链的执行优先级契约。
//
// 三个不可退让的契约：
//  1. 链族恒排在所有其他动作之前（否则被付费动作挤到下一轮 = 永久饿死）；
//  2. 族内严格 N 升序（N 依赖 N-1，乱序会让 accept 不落账）；
//  3. 代码里**未登记**的环也要置顶并按 N 排（上游随时加环，不能要等适配）。
package panel

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestSequentialTaskIndex 解析：只认 <prefix><正整数>，其余一律 false。
func TestSequentialTaskIndex(t *testing.T) {
	cases := []struct {
		code string
		n    int
		ok   bool
	}{
		{"Sequential_Tasks_1", 1, true},
		{"Sequential_Tasks_7", 7, true},
		{" Sequential_Tasks_12 ", 12, true}, // 上游可能继续加环
		{"Sequential_Tasks_0", 0, false},
		{"Sequential_Tasks_", 0, false},
		{"Sequential_Tasks_x", 0, false},
		{"school_season", 0, false},
		{"chat_5", 0, false},
	}
	for _, c := range cases {
		n, ok := sequentialTaskIndex(c.code)
		if ok != c.ok || (ok && n != c.n) {
			t.Errorf("sequentialTaskIndex(%q) = (%d,%v), want (%d,%v)", c.code, n, ok, c.n, c.ok)
		}
	}
}

// TestTaskPriorityLess 链族置顶 + 族内 N 升序 + 未登记环照样置顶。
func TestTaskPriorityLess(t *testing.T) {
	tasks := []upstream.Task{
		{TaskCode: "expert_5"},           // 付费真实对话，注册序最靠前
		{TaskCode: "Sequential_Tasks_3"}, // 链第 3 环
		{TaskCode: "chat_5"},
		{TaskCode: "Sequential_Tasks_1"},
		{TaskCode: "Sequential_Tasks_9"}, // 代码未登记的环
		{TaskCode: "expert_use_maidehao"},
	}
	// 冒泡一轮就够验证比较函数的全序（数据量小，直接插入排序更直观）。
	for i := 1; i < len(tasks); i++ {
		for j := i; j > 0 && taskPriorityLess(tasks[j], tasks[j-1]); j-- {
			tasks[j], tasks[j-1] = tasks[j-1], tasks[j]
		}
	}
	var got []string
	for _, x := range tasks {
		got = append(got, x.TaskCode)
	}
	want := "Sequential_Tasks_1,Sequential_Tasks_3,Sequential_Tasks_9,chat_5,expert_5,expert_use_maidehao"
	if strings.Join(got, ",") != want {
		t.Fatalf("order = %v, want %s", got, want)
	}
}

// TestSequentialRescuePlan 非零积分口径：只收链、单轮只取一环、跳过退避与锁定。
func TestSequentialRescuePlan(t *testing.T) {
	tasks := []upstream.Task{
		{TaskCode: "chat_5", Target: 5, Current: 1},              // 非链 → 弃
		{TaskCode: "expert_5"},                                   // 非链且付费 → 弃
		{TaskCode: "Sequential_Tasks_2", Target: 1, Current: 0},   // 收
		{TaskCode: "Sequential_Tasks_3", Target: 5, Current: 0},   // 收但被 limit=1 截掉
		{TaskCode: "Sequential_Tasks_4", Locked: true},            // 锁定 → 弃
		{TaskCode: "Sequential_Tasks_5", Claimed: true},           // 已领 → 弃
		{TaskCode: "Sequential_Tasks_6", Target: 10, Current: 10}, // 已达标 → 弃（交给领奖）
	}
	plan, _ := sequentialRescuePlan(tasks, nil)
	if len(plan) != 1 || plan[0].TaskCode != "Sequential_Tasks_2" {
		t.Fatalf("plan = %+v, want [Sequential_Tasks_2]", plan)
	}

	// 链的第一环在退避期时，应让位给下一环而不是空跑。
	blocked := map[string]bool{"Sequential_Tasks_2": true}
	plan, skipped := sequentialRescuePlan(tasks, func(c string) bool { return blocked[c] })
	if len(plan) != 1 || plan[0].TaskCode != "Sequential_Tasks_3" || skipped != 1 {
		t.Fatalf("plan = %+v skipped=%d, want [Sequential_Tasks_3] skipped=1", plan, skipped)
	}
}

// TestSequentialRescueSelected 分片轮转：每个分片都能分到号，覆盖率与 1/shard 相当。
// 具体命中哪个分片取决于当前时间片，所以只断言分布均匀（轮转会保证长期覆盖）。
func TestSequentialRescueSelected(t *testing.T) {
	old := sequentialRescueShard
	sequentialRescueShard = 8
	defer func() { sequentialRescueShard = old }()

	hit := 0
	const total = 4000
	for i := 0; i < total; i++ {
		uid := "00000000-0000-0000-0000-" + strings.Repeat("0", 8) + string(rune('a'+i%26)) + strconv.Itoa(i)
		if sequentialRescueSelected(uid) {
			hit++
		}
	}
	// 期望 total/8 = 500，给足浮点/哈希噪声余量（±50%）。
	if hit < 250 || hit > 750 {
		t.Fatalf("selected %d/%d, want ≈%d（分片不均会饿死整批账号）", hit, total, total/8)
	}

	// shard<=1 时全选（测试/小账号池的短路分支）。
	sequentialRescueShard = 1
	if !sequentialRescueSelected("anything") {
		t.Fatal("shard<=1 时应全选")
	}
}

// TestSequentialRescueRunsForFundedAccount 端到端：**有余额**的号（老逻辑下
// 完全进不了补给——rescue 只在 credits<=0 时开）在分片命中时会把 Sequential
// 链真正推进一步，且不顺手跑其他免生成任务（chat_5 留给成长队列）。
func TestSequentialRescueRunsForFundedAccount(t *testing.T) {
	fastPlaybookTest(t) // reportGap / claimPollGap 归零
	oldGap, oldShard, oldHour := mpChatEventGap, sequentialRescueShard, sequentialRescueHour.Load()
	mpChatEventGap, sequentialRescueShard = 0, 1 // 不分片：本轮必然轮到
	sequentialRescueHour.Store(0)                // 清小时槽：否则与其它用例串扰（每小时只排一批）
	t.Cleanup(func() {
		mpChatEventGap, sequentialRescueShard = oldGap, oldShard
		sequentialRescueHour.Store(oldHour)
	})

	claimed := map[string]bool{}
	var lit atomic.Bool // mini 对话事件上报后进度点亮

	b := &sweepBackend{
		listFn: func(isMP bool) []map[string]any {
			if isMP {
				if claimed["Sequential_Tasks_1"] {
					// 回读口径：上游用 accept_status=claimed 表示已领（与本文件
					// 其它端到端用例一致），单给 current=1/target=1 算 claimable。
					return []map[string]any{sweepTask("Sequential_Tasks_1", 1, 1, "claimed", nil)}
				}
				cur := int64(0)
				if lit.Load() {
					cur = 1
				}
				return []map[string]any{sweepTask("Sequential_Tasks_1", 1, cur, "accepted", nil)}
			}
			// 默认口径的免生成任务：有余额的号该留在成长队列里做，清扫不代劳。
			return []map[string]any{sweepTask("chat_5", 5, 0, "accepted", nil)}
		},
		claimFn: func(code string) (int, map[string]any) {
			claimed[code] = true
			return claimOK(map[string]any{"already_claimed": false, "credit": 100, "energy": 5})
		},
	}
	inner := b.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/report") {
			lit.Store(true)
		}
		inner(w, r)
	}))
	defer srv.Close()

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "funded", AccessToken: "at"})
	pl.SetCredits("funded", 500, 4200) // 有余额：老逻辑下 rescue=false
	p := &Panel{cfg: Config{
		Pool: pl,
		Upstream: &upstream.Client{
			HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
		},
	}}

	st := p.RunTaskSweepOnce()
	if st.Sequential != 1 {
		t.Fatalf("Sequential = %d, want 1（有余额号的链应被收集为候选；stats=%+v）", st.Sequential, st)
	}
	if st.Rescued != 0 {
		t.Errorf("Rescued = %d, want 0（零积分补给不该参与）", st.Rescued)
	}
	// 动作在清扫收尾后的独立批次里跑（tasksweep_sequential.go）：等它收工
	// 再断言结果，否则看的是「刚收集完、还没执行」的中间态。
	waitSequentialBatch(t)
	if !claimed["Sequential_Tasks_1"] {
		t.Error("Sequential_Tasks_1 未被领取")
	}
	if claimed["chat_5"] {
		t.Error("有余额号的 chat_5 不该被清扫代做（那是成长队列的职责）")
	}
	if st.Errors != 0 {
		t.Errorf("errors = %d, want 0", st.Errors)
	}

	// 第二轮：链已 claimed → 不再重复推进。
	if second := p.RunTaskSweepOnce(); second.Sequential != 0 {
		t.Errorf("second Sequential = %d, want 0（已领取不重复跑）", second.Sequential)
	}
}

// TestSequentialRescueDue 每小时只排一批：同小时重复调用必须返回 false，
// 否则清扫每 15 分钟一轮会把同一片账号连推 4 次（白烧 mp 配额）。
func TestSequentialRescueDue(t *testing.T) {
	old := sequentialRescueHour.Load()
	sequentialRescueHour.Store(0)
	t.Cleanup(func() { sequentialRescueHour.Store(old) })

	const hour int64 = 480000
	if !sequentialRescueDue(hour) {
		t.Fatal("本小时首次应返回 true")
	}
	if sequentialRescueDue(hour) {
		t.Fatal("同一小时第二次应返回 false（每小时只排一批）")
	}
	if !sequentialRescueDue(hour + 1) {
		t.Fatal("跨小时应再排一批")
	}
}

// waitSequentialBatch 等后台链推进批次收尾（有界等待，避免测试挂死）。
func waitSequentialBatch(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for sequentialRescueBusy.Load() {
		if time.Now().After(deadline) {
			t.Fatal("后台 Sequential 批次 15s 内未收尾")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
