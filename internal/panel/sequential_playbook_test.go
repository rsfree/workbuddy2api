package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type playbookFake struct {
	current          atomic.Int64
	claimed          atomic.Bool
	progressOnReport bool
	desktopReports   atomic.Int32
	mpReports        atomic.Int32
	webClaims        atomic.Int32
	mpClaims         atomic.Int32
	mpLists          atomic.Int32
}

func (f *playbookFake) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v2/activity/growth/tasks" && r.Method == http.MethodGet:
		if r.Header.Get("X-Client-Platform") == "miniprogram" {
			f.mpLists.Add(1)
		}
		current := f.current.Load()
		status := "accepted"
		if f.claimed.Load() {
			status = "claimed"
			current = 1
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{
			"tasks": []map[string]any{{
				"task_code": "Sequential_Tasks_7", "title": "体验灵感功能", "tag": "PC 限定",
				"target": 1, "current": current, "accept_status": status,
				"reward_credit": 500, "reward_energy": 5, "jump_url": "workbuddy://playbook",
			}},
		}})
	case r.URL.Path == "/v2/activity/growth/tasks/accept":
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
	case r.URL.Path == "/v2/report":
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		if strings.Contains(body, "workbuddy-mp") {
			f.mpReports.Add(1)
		} else {
			f.desktopReports.Add(1)
			if f.progressOnReport && strings.Contains(body, "playbook_prompt_send") {
				f.current.Store(1)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
	case r.URL.Path == "/activity/growth/tasks/Sequential_Tasks_7/claim":
		if r.Header.Get("X-Client-Platform") == "miniprogram" {
			f.mpClaims.Add(1)
		} else {
			f.webClaims.Add(1)
		}
		f.claimed.Store(true)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{
			"already_claimed": false, "credit": 500, "energy": 5,
		}})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
	}
}

func newPlaybookPanel(t *testing.T, f *playbookFake) *Panel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
}

func fastPlaybookTest(t *testing.T) {
	t.Helper()
	oldGap, oldPoll := reportGap, claimPollGap
	reportGap, claimPollGap = 0, 0
	t.Cleanup(func() { reportGap, claimPollGap = oldGap, oldPoll })
}

// PC 限定任务必须从默认列表读取、桌面上报、Web 领奖；不得被 Sequential 的
// mp 通用骨架劫持。领奖后外层队列不得再重复 claim。
func TestSequentialPlaybookUsesPCFlowAndClaimsOnce(t *testing.T) {
	fastPlaybookTest(t)
	if isMPTaskCode("Sequential_Tasks_7") {
		t.Fatal("PC 限定任务不得登记为 mp 专属")
	}
	if !isMPTaskCode("Sequential_Tasks_6") {
		t.Fatal("其它 Sequential mp 任务不应被连带修改")
	}
	f := &playbookFake{progressOnReport: true}
	p := newPlaybookPanel(t, f)
	msg, err := p.runGrowthQueued(&auth.Auth{UID: "u1", AccessToken: "at"}, "Sequential_Tasks_7")
	if err != nil {
		t.Fatalf("runGrowthQueued: %v", err)
	}
	if !strings.Contains(msg, "+500 分 +5 能") || !strings.Contains(msg, "1/1") {
		t.Fatalf("msg=%q", msg)
	}
	if f.desktopReports.Load() != 1 || f.mpReports.Load() != 0 || f.webClaims.Load() != 1 || f.mpClaims.Load() != 0 || f.mpLists.Load() != 0 {
		t.Fatalf("desktop=%d mpReports=%d webClaims=%d mpClaims=%d mpLists=%d",
			f.desktopReports.Load(), f.mpReports.Load(), f.webClaims.Load(), f.mpClaims.Load(), f.mpLists.Load())
	}
}

// 上报 200 但进度未点亮不是成功：必须返回错误，且不得发领奖请求。
func TestSequentialPlaybookDoesNotFakeCompletion(t *testing.T) {
	fastPlaybookTest(t)
	f := &playbookFake{progressOnReport: false}
	p := newPlaybookPanel(t, f)
	_, err := p.runGrowthQueued(&auth.Auth{UID: "u1", AccessToken: "at"}, "Sequential_Tasks_7")
	if err == nil || !strings.Contains(err.Error(), "进度仍为") {
		t.Fatalf("err=%v want 未点亮错误", err)
	}
	if f.webClaims.Load() != 0 || f.mpClaims.Load() != 0 {
		t.Fatalf("未达标却调用领奖: web=%d mp=%d", f.webClaims.Load(), f.mpClaims.Load())
	}
}

// 周期清扫只对零积分账号补跑这条纯事件任务；有余额账号保持原有
// accept/claim 语义，不被提前触发行为链路。
func TestTaskSweepZeroCreditPlaybookRescue(t *testing.T) {
	fastPlaybookTest(t)
	for _, tc := range []struct {
		name            string
		credits         int64
		creditsTotal    int64
		growthDisabled  bool
		wantReports     int32
		wantRescued     int
		wantClaimed     int
		wantCreditTotal int64
	}{
		{name: "零积分补跑并领取", credits: 0, wantReports: 1, wantRescued: 1, wantClaimed: 1, wantCreditTotal: 500},
		{name: "非零积分不触发行为任务", credits: 100, wantReports: 0, wantRescued: 0, wantClaimed: 0, wantCreditTotal: 0},
		{name: "总额未知不误判零积分", credits: 0, creditsTotal: -1, wantReports: 0, wantRescued: 0, wantClaimed: 0, wantCreditTotal: 0},
		{name: "成长排程关闭不补跑", credits: 0, growthDisabled: true, wantReports: 0, wantRescued: 0, wantClaimed: 0, wantCreditTotal: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &playbookFake{progressOnReport: true}
			p := newPlaybookPanel(t, f)
			pl := pool.New("")
			a := &auth.Auth{UID: "u1", AccessToken: "at"}
			pl.Add(a)
			total := tc.creditsTotal
			if total == 0 {
				total = 6000
			} else if total < 0 {
				total = 0
			}
			pl.SetCredits(a.UID, tc.credits, total)
			p.cfg.Pool = pl
			if tc.growthDisabled {
				p.cfg.Scheduler = scheduler.New(scheduler.Config{GrowthDisabled: true})
			}
			st := p.RunTaskSweepOnce()
			if f.desktopReports.Load() != tc.wantReports || f.mpReports.Load() != 0 {
				t.Fatalf("desktop=%d mp=%d, want desktop=%d mp=0", f.desktopReports.Load(), f.mpReports.Load(), tc.wantReports)
			}
			if st.Rescued != tc.wantRescued || st.Claimed != tc.wantClaimed || st.Credits != tc.wantCreditTotal || st.Energy != tc.wantCreditTotal/100 {
				t.Fatalf("stats=%+v", st)
			}
			if got := f.webClaims.Load(); got != int32(tc.wantClaimed) {
				t.Fatalf("web claims=%d want %d", got, tc.wantClaimed)
			}
		})
	}
}
