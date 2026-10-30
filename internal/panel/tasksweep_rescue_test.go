// tasksweep_rescue_test.go 零积分补给（tasksweep_rescue.go）与额度闸门的行为契约。
//
// 三个不可退让的契约：
//  1. 补给计划只收免生成动作——付费动作在余额 0 时必然 14018，跑它纯属浪费；
//  2. 计划要按 autoActions 顺序（Sequential 链 N 依赖 N-1）、且受单轮上限约束；
//  3. runAction 的额度闸门必须真的挡住付费动作（零上游请求），且**不能**把
//     CreditsTotal==0（总额未知）的号误当成没钱。
package panel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestZeroCreditRescuePlan 计划筛选：已领取 / 锁定 / 已达标 / 未适配 /
// 付费动作 / 退避中的一律不进计划；顺序按 autoActions 下标；超上限截断。
func TestZeroCreditRescuePlan(t *testing.T) {
	tasks := []upstream.Task{
		{TaskCode: "chat_5", Target: 5, Current: 2},                     // 免生成、未达标 → 收
		{TaskCode: "Sequential_Tasks_1", Target: 1, Current: 0},         // 免生成 → 收
		{TaskCode: "expert_use_maidehao", Title: "体验「电商运营」", Target: 1}, // 付费 → 弃
		{TaskCode: "Model_chat_GLM5.2", Target: 1},                      // 付费 → 弃
		{TaskCode: "locked_1", Locked: true},                            // 锁定 → 弃
		{TaskCode: "chat_5_claimed", Claimed: true},                     // 已领 → 弃
		{TaskCode: "template_5", Target: 5, Current: 5},                 // 已达标（claimable）→ 弃
		{TaskCode: "wb_wechat_oa_subscribe_task", Target: 1},            // 未适配 → 弃
		{TaskCode: "Sequential_Tasks_3", Target: 5},                     // 退避中 → 弃
	}
	blocked := map[string]bool{"Sequential_Tasks_3": true}
	plan, skipped := zeroCreditRescuePlan(tasks, func(c string) bool { return blocked[c] }, 0)

	var codes []string
	for _, t2 := range plan {
		codes = append(codes, t2.TaskCode)
	}
	want := "chat_5,Sequential_Tasks_1"
	if strings.Join(codes, ",") != want {
		t.Fatalf("plan = %v, want %s", codes, want)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1（Sequential_Tasks_3 在退避期）", skipped)
	}

	// 上限截断：只留前 1 条（顺序上 chat_5 在 Sequential_Tasks_1 之前）。
	plan, _ = zeroCreditRescuePlan(tasks, nil, 1)
	if len(plan) != 1 || plan[0].TaskCode != "chat_5" {
		t.Errorf("limit=1 plan = %+v, want [chat_5]", plan)
	}
}

// TestWalletExhausted 口径：credits<=0 且总额已知才算耗尽；总额未知（0）不拦。
func TestWalletExhausted(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "dead", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "unknown", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "rich", AccessToken: "at"})
	p.SetCredits("dead", 0, 4200)
	p.SetCredits("unknown", 0, 0)
	p.SetCredits("rich", 120, 4200)

	pn := &Panel{cfg: Config{Pool: p}}
	cases := map[string]bool{"dead": true, "unknown": false, "rich": false, "": false, "missing": false}
	for uid, want := range cases {
		if got := pn.walletExhausted(uid); got != want {
			t.Errorf("walletExhausted(%q) = %v, want %v", uid, got, want)
		}
	}
}

// TestRunActionWalletGate 余额 0 时付费动作被挡下（errWalletEmpty、零上游请求），
// 免生成动作不受影响。
func TestRunActionWalletGate(t *testing.T) {
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "dead", AccessToken: "at"}
	p.Add(a)
	p.SetCredits("dead", 0, 4200)
	pn := &Panel{cfg: Config{
		Pool: p,
		Upstream: &upstream.Client{
			HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
		},
	}}

	paid := autoActionFor("Model_chat_GLM5.2")
	if paid == nil || !paid.Paid {
		t.Fatal("Model_chat_GLM5.2 必须是付费动作")
	}
	_, err := runAction(paid, pn, a, upstream.Task{TaskCode: "Model_chat_GLM5.2"})
	if !errors.Is(err, errWalletEmpty) {
		t.Fatalf("err = %v, want errWalletEmpty", err)
	}
	if got := reqs.Load(); got != 0 {
		t.Fatalf("被闸门挡下时不应产生上游请求，got %d", got)
	}

	// 免生成动作照常执行（这里只验证没有被闸门拦下：拿到的是"任务不存在"这类
	// 业务错误，而不是 errWalletEmpty）。
	free := autoActionFor("chat_5")
	if _, err := runAction(free, pn, a, upstream.Task{TaskCode: "chat_5"}); errors.Is(err, errWalletEmpty) {
		t.Fatalf("免生成动作不应被额度闸门拦下: %v", err)
	}
}

// TestZeroCreditRescueRunsFreeTasks 端到端：0 积分号走向清扫时，免生成任务
// 真的被补跑、奖励真的到账（回读 claimed 才算），付费动作一条请求都不发。
func TestZeroCreditRescueRunsFreeTasks(t *testing.T) {
	claimed := map[string]bool{}
	reported := atomic.Int32{}

	b := &sweepBackend{
		listFn: func(isMP bool) []map[string]any {
			if isMP {
				return nil
			}
			out := []map[string]any{}
			for _, code := range []string{"chat_5", "expert_5"} {
				if claimed[code] {
					out = append(out, sweepTask(code, 5, 5, "claimed", nil))
					continue
				}
				cur := int64(0) // 默认口径任务；expert_5 是付费动作，不该被动到
				if code == "chat_5" && reported.Load() > 0 {
					cur = 5 // 事件上报后进度点亮
				}
				out = append(out, sweepTask(code, 5, cur, "accepted", nil))
			}
			return out
		},
		claimFn: func(code string) (int, map[string]any) {
			claimed[code] = true
			return claimOK(map[string]any{"already_claimed": false, "credit": 100, "energy": 5})
		},
	}
	inner := b.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/report") {
			reported.Add(1)
		}
		inner(w, r)
	}))
	defer srv.Close()

	pl := pool.New("")
	a := &auth.Auth{UID: "dead", AccessToken: "at"}
	pl.Add(a)
	pl.SetCredits("dead", 0, 4200) // 零积分号：触发补给
	p := &Panel{cfg: Config{
		Pool: pl,
		Upstream: &upstream.Client{
			HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
		},
	}}

	st := p.RunTaskSweepOnce()
	if st.Rescued != 1 {
		t.Fatalf("rescued = %d, want 1（chat_5 应被补跑并领奖；stats=%+v）", st.Rescued, st)
	}
	if st.Credits != 100 || st.Energy != 5 {
		t.Errorf("credits=%d energy=%d, want 100/5", st.Credits, st.Energy)
	}
	if !claimed["chat_5"] {
		t.Error("chat_5 未被领取")
	}
	if claimed["expert_5"] {
		t.Error("付费动作 expert_5 不该被零积分补给执行")
	}
	if st.Errors != 0 {
		t.Errorf("errors = %d, want 0（补给不该产生错误）", st.Errors)
	}

	// 第二轮：chat_5 已 claimed 不再动；补给计划为空 → rescued 归 0。
	reported.Store(0)
	second := p.RunTaskSweepOnce()
	if second.Rescued != 0 {
		t.Errorf("second rescued = %d, want 0（已领取的任务不重复跑）", second.Rescued)
	}
}

// TestSortGrowthFreeFirst 零积分号的任务顺序：免生成在前、付费在后，
// 同组内保持 autoActions 下标序（Sequential 链依赖前置）。
func TestSortGrowthFreeFirst(t *testing.T) {
	tasks := []upstream.Task{
		{TaskCode: "expert_5"},           // 付费
		{TaskCode: "Sequential_Tasks_2"}, // 免生成
		{TaskCode: "chat_5"},             // 免生成（下标更小）
	}
	sortGrowthFreeFirst(tasks)
	got := []string{tasks[0].TaskCode, tasks[1].TaskCode, tasks[2].TaskCode}
	want := []string{"chat_5", "Sequential_Tasks_2", "expert_5"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
