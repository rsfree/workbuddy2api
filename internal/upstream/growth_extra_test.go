package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// extraServer 起一个按 path 分发的桩：growth 域走 ChatBaseCN，billing 域走
// BillingBaseCN，两者在本测试里指向同一个桩，靠 path 区分。
func extraServer(t *testing.T, wantPath, wantMethod, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wantPath {
			t.Errorf("path=%s want %s", r.URL.Path, wantPath)
		}
		if r.Method != wantMethod {
			t.Errorf("method=%s want %s", r.Method, wantMethod)
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
}

// TestListLegacyTasksParsesV1Shape v1 关卡是**另一套资源**：路径无 /v2，
// 字段只有 code/title/level_name/status，没有任何 reward——解析层不能因为
// 缺 reward 就当成异常（那会让每轮扫描都报错）。
func TestListLegacyTasksParsesV1Shape(t *testing.T) {
	c := extraServer(t, "/activity/growth/tasks", http.MethodGet,
		`{"code":0,"msg":"OK","data":{"tasks":[`+
			`{"task_id":1,"code":"first_chat","title":"完成一次对话","description":"使用 WorkBuddy 完成至少一次对话交互","level_name":"养虾尝试","status":"done","url":""},`+
			`{"task_id":2,"code":"skill_installed","title":"完成技能安装","level_name":"养虾入门","status":"available"}]}}`)

	tasks, err := c.ListLegacyTasks(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("ListLegacyTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("len=%d want 2", len(tasks))
	}
	first := tasks[0]
	if first.Code != "first_chat" || first.Status != "done" || first.LevelName != "养虾尝试" {
		t.Errorf("first=%+v want code=first_chat status=done level=养虾尝试", first)
	}
	// 空 url / 缺 description 不应导致解析失败或脏值。
	if tasks[1].Desc != "" || tasks[1].URL != "" {
		t.Errorf("second=%+v want zero desc/url", tasks[1])
	}
}

// TestEnergyParses 能量账户只有三个数字字段。
func TestEnergyParses(t *testing.T) {
	c := extraServer(t, "/activity/growth/energy", http.MethodGet,
		`{"code":0,"msg":"OK","data":{"balance":93,"total_consumed":0,"total_earned":93}}`)

	e, err := c.Energy(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("Energy: %v", err)
	}
	if e.Balance != 93 || e.TotalEarned != 93 || e.TotalConsumed != 0 {
		t.Errorf("energy=%+v want balance/total_earned=93 consumed=0", e)
	}
}

// TestCheckinActivityStatusParsesAllFields 签到活动是赛季制，换季判据靠
// season/theme_name/end_time 三个字段；周进度是 7 格布尔，长度必须原样保留
// （截断会让前端画错格子）。
func TestCheckinActivityStatusParsesAllFields(t *testing.T) {
	c := extraServer(t, "/billing/meter/checkin-activity-status", http.MethodPost,
		`{"code":0,"msg":"OK","data":{"active":true,"today_checked_in":true,"streak_days":4,`+
			`"daily_credit":100,"today_credit":100,"is_streak_day":false,"next_streak_day":0,`+
			`"streak_bonus_days":0,"streak_bonus_credit":0,`+
			`"checkin_dates":["2026-10-28","2026-10-27","2026-10-26","2026-10-25"],`+
			`"week_checkin_days":3,"week_progress":[true,true,true,false,false,false,false],`+
			`"total_credits":400,"start_time":"2026-09-30 00:00:00","end_time":"2026-12-31 23:59:59",`+
			`"theme_name":"Buddy加油站","season":10,"activity_name":"Buddy应用"}}`)

	st, err := c.CheckinActivityStatus(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("CheckinActivityStatus: %v", err)
	}
	if !st.Active || !st.TodayCheckedIn {
		t.Errorf("active/today_checked_in=false want true")
	}
	if st.StreakDays != 4 || st.WeekCheckinDays != 3 {
		t.Errorf("streak=%d week=%d want 4/3", st.StreakDays, st.WeekCheckinDays)
	}
	if len(st.WeekProgress) != 7 {
		t.Fatalf("len(week_progress)=%d want 7", len(st.WeekProgress))
	}
	if !st.WeekProgress[0] || st.WeekProgress[3] {
		t.Errorf("week_progress=%v want [T T T F F F F]", st.WeekProgress)
	}
	if st.Season != 10 || st.ThemeName != "Buddy加油站" || st.EndTime != "2026-12-31 23:59:59" {
		t.Errorf("identity=%+v want season=10 theme=Buddy加油站 end=2026-12-31", st)
	}
	if st.DailyCredit != 100 || st.TotalCredits != 400 {
		t.Errorf("credit daily=%d total=%d want 100/400", st.DailyCredit, st.TotalCredits)
	}
}

// TestCheckinActivityStatusInactiveNoError 活动未开启（active=false）是 200 正常
// 返回，不是错误：赛季之间有空窗期，那段时间每账号都查，不能把日志刷成错误。
func TestCheckinActivityStatusInactiveNoError(t *testing.T) {
	c := extraServer(t, "/billing/meter/checkin-activity-status", http.MethodPost,
		`{"code":0,"msg":"OK","data":{"active":false,"today_checked_in":false,"streak_days":0}}`)

	st, err := c.CheckinActivityStatus(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("inactive should not error: %v", err)
	}
	if st.Active {
		t.Error("Active=true want false")
	}
}
