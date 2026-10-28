package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// growthAssetServer 桩：按 path 回固定信封（签到活动 / 能量各一份）。
func growthAssetServer(t *testing.T) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/billing/meter/checkin-activity-status":
			if r.Method != http.MethodPost {
				t.Errorf("checkin-activity-status method=%s want POST", r.Method)
			}
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"active":true,"today_checked_in":true,` +
				`"streak_days":4,"daily_credit":100,"today_credit":100,` +
				`"week_checkin_days":3,"week_progress":[true,true,true,false,false,false,false],` +
				`"total_credits":400,"start_time":"2026-09-30 00:00:00","end_time":"2026-12-31 23:59:59",` +
				`"theme_name":"Buddy加油站","season":10,"activity_name":"Buddy应用"}}`))
		case "/activity/growth/energy":
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"balance":93,"total_consumed":0,"total_earned":93}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
}

// TestGrowthAssetsSkipsEnterpriseAndSummarizes 企业版必须被排除（无个人成长体系，
// 查了必然 403，只会把 errors 撑起来误导成「坏了」），并核对汇总数字。
func TestGrowthAssetsSkipsEnterpriseAndSummarizes(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Upstream: growthAssetServer(t)})
	p.cfg.Pool.Add(&auth.Auth{AccessToken: "at1", UID: "u1", Nickname: "甲"})
	// 企业版：签到 400「企业账号不支持该操作」、growth 403，必须跳过。
	p.cfg.Pool.Add(&auth.Auth{AccessToken: "at2", UID: "u2", EnterpriseID: "e1"})

	rec := httptest.NewRecorder()
	p.growthAssets(rec, httptest.NewRequest(http.MethodGet, "/panel/api/growth_assets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Summary struct {
			Accounts     int   `json:"accounts"`
			CheckedIn    int   `json:"checked_in"`
			Missing      int   `json:"missing"`
			Active       int   `json:"active"`
			Errors       int   `json:"errors"`
			EnergyTotal  int64 `json:"energy_total"`
			CreditsTotal int64 `json:"credits_total"`
			MinStreak    int   `json:"min_streak"`
			MaxStreak    int   `json:"max_streak"`
		} `json:"summary"`
		Activity struct {
			Season      int    `json:"season"`
			ThemeName   string `json:"theme_name"`
			EndTime     string `json:"end_time"`
			DailyCredit int64  `json:"daily_credit"`
		} `json:"activity"`
		Accounts []struct {
			UID             string `json:"uid"`
			TodayCheckedIn  bool   `json:"today_checked_in"`
			StreakDays      int    `json:"streak_days"`
			WeekCheckinDays int    `json:"week_checkin_days"`
			WeekProgress    []bool `json:"week_progress"`
			TotalCredits    int64  `json:"total_credits"`
			Energy          int64  `json:"energy"`
			Err             string `json:"err"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}

	if resp.Summary.Accounts != 1 {
		t.Fatalf("Accounts=%d want 1（企业版应被跳过）", resp.Summary.Accounts)
	}
	if len(resp.Accounts) != 1 || resp.Accounts[0].UID != "u1" {
		t.Fatalf("accounts=%+v want 仅 u1", resp.Accounts)
	}
	row := resp.Accounts[0]
	if !row.TodayCheckedIn || row.StreakDays != 4 || row.WeekCheckinDays != 3 {
		t.Errorf("row=%+v want 已签/连签4/本周3", row)
	}
	if len(row.WeekProgress) != 7 {
		t.Errorf("week_progress 长度=%d want 7", len(row.WeekProgress))
	}
	if row.Energy != 93 || row.TotalCredits != 400 {
		t.Errorf("energy=%d total_credits=%d want 93/400", row.Energy, row.TotalCredits)
	}
	if resp.Summary.CheckedIn != 1 || resp.Summary.Missing != 0 || resp.Summary.Active != 1 {
		t.Errorf("summary=%+v want checked_in=1 missing=0 active=1", resp.Summary)
	}
	if resp.Summary.EnergyTotal != 93 || resp.Summary.CreditsTotal != 400 {
		t.Errorf("汇总 energy=%d credits=%d want 93/400", resp.Summary.EnergyTotal, resp.Summary.CreditsTotal)
	}
	if resp.Activity.Season != 10 || resp.Activity.ThemeName != "Buddy加油站" {
		t.Errorf("activity=%+v want season=10 theme=Buddy加油站", resp.Activity)
	}
	if resp.Activity.EndTime != "2026-12-31 23:59:59" {
		t.Errorf("EndTime=%q want 2026-12-31 23:59:59", resp.Activity.EndTime)
	}
}

// TestGrowthAssetsLimitCaps 查询上限：?limit=99999 必须被夹到硬上限，不能打穿上游。
func TestGrowthAssetsLimitCaps(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Upstream: growthAssetServer(t)})
	for i := 0; i < 3; i++ {
		p.cfg.Pool.Add(&auth.Auth{AccessToken: "at", UID: "u" + string(rune('a'+i))})
	}

	rec := httptest.NewRecorder()
	p.growthAssets(rec, httptest.NewRequest(http.MethodGet, "/panel/api/growth_assets?limit=99999", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp struct {
		Summary struct {
			Accounts int `json:"accounts"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析: %v", err)
	}
	// 池里只有 3 个号，上限再大也只会查 3 个。
	if resp.Summary.Accounts != 3 {
		t.Errorf("Accounts=%d want 3（池内全查）", resp.Summary.Accounts)
	}
}
