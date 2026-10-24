package scheduler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// taskMutedAt 是新号任务静默期的唯一判定口径，这里把每条边界都钉住：
// 少一条都会出现「老号被静默三天」或「新号照跑任务」的静默事故。
func TestTaskMutedAt(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	imported := func(d time.Duration) time.Time { return now.Add(-d) }

	cases := []struct {
		name      string
		st        pool.Status
		graceDays int
		want      bool
	}{
		{
			name:      "导入1天_可信来源_有积分_静默",
			st:        pool.Status{ImportedAt: imported(24 * time.Hour), ImportedAtSource: "import", Credits: 2200, CreditsTotal: 2200},
			graceDays: 3,
			want:      true,
		},
		{
			name:      "导入2天23小时_仍在窗口内",
			st:        pool.Status{ImportedAt: imported(71 * time.Hour), ImportedAtSource: "import", Credits: 100, CreditsTotal: 2200},
			graceDays: 3,
			want:      true,
		},
		{
			name:      "导入正好满3天_放行",
			st:        pool.Status{ImportedAt: imported(72 * time.Hour), ImportedAtSource: "import", Credits: 2200, CreditsTotal: 2200},
			graceDays: 3,
			want:      false,
		},
		{
			name:      "导入5天_放行",
			st:        pool.Status{ImportedAt: imported(120 * time.Hour), ImportedAtSource: "import", Credits: 2200, CreditsTotal: 2200},
			graceDays: 3,
			want:      false,
		},
		{
			name:      "mtime推断来源_不静默",
			st:        pool.Status{ImportedAt: imported(2 * time.Hour), ImportedAtSource: "mtime", Credits: 2200, CreditsTotal: 2200},
			graceDays: 3,
			want:      false,
		},
		{
			name:      "来源未知_不静默",
			st:        pool.Status{ImportedAt: imported(2 * time.Hour), Credits: 2200, CreditsTotal: 2200},
			graceDays: 3,
			want:      false,
		},
		{
			name:      "导入时间为零_不静默",
			st:        pool.Status{ImportedAtSource: "import", Credits: 2200, CreditsTotal: 2200},
			graceDays: 3,
			want:      false,
		},
		{
			name:      "积分清光_提前放行",
			st:        pool.Status{ImportedAt: imported(6 * time.Hour), ImportedAtSource: "import", Credits: 0, CreditsTotal: 2200},
			graceDays: 3,
			want:      false,
		},
		{
			name:      "积分未知_保守静默",
			st:        pool.Status{ImportedAt: imported(6 * time.Hour), ImportedAtSource: "import", Credits: 0, CreditsTotal: 0},
			graceDays: 3,
			want:      true,
		},
		{
			name:      "静默期关闭_放行",
			st:        pool.Status{ImportedAt: imported(time.Hour), ImportedAtSource: "import", Credits: 2200, CreditsTotal: 2200},
			graceDays: 0,
			want:      false,
		},
		{
			name:      "静默期负数_等于关闭",
			st:        pool.Status{ImportedAt: imported(time.Hour), ImportedAtSource: "import", Credits: 2200, CreditsTotal: 2200},
			graceDays: -1,
			want:      false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := taskMutedAt(c.st, now, c.graceDays)
			if got != c.want {
				t.Fatalf("taskMutedAt() = %v (%s), want %v", got, why, c.want)
			}
			if got && why == "" {
				t.Fatal("静默命中时必须给出可读原因（运维靠它判断哪天转正）")
			}
		})
	}
}

// 静默期的日志节流：同一账号同一任务同一天只打一行，跨天重新打。
func TestNoteTaskGraceSkipThrottle(t *testing.T) {
	now := time.Now()
	s := New(Config{})
	st := pool.Status{UID: "uid-1", Nickname: "n1", ImportedAt: now.Add(-time.Hour),
		ImportedAtSource: "import", Credits: 2200, CreditsTotal: 2200}

	s.NoteTaskGraceSkip(st, "travel", "day1")
	s.mu.Lock()
	first := s.graceLogged["uid-1|travel"]
	s.mu.Unlock()
	if first != now.Format("2006-01-02") {
		t.Fatalf("首次记录日期 = %q, want %q", first, now.Format("2006-01-02"))
	}

	// 伪造昨天：应为"跨天"，会重新打日志并把日期刷成今天。
	s.mu.Lock()
	s.graceLogged["uid-1|travel"] = now.Add(-24 * time.Hour).Format("2006-01-02")
	s.mu.Unlock()
	s.NoteTaskGraceSkip(st, "travel", "day2")
	s.mu.Lock()
	got := s.graceLogged["uid-1|travel"]
	s.mu.Unlock()
	if got != now.Format("2006-01-02") {
		t.Fatalf("跨天后日期 = %q, want 今天", got)
	}
}

func TestTravelEntrySkipsNewAccountWithoutUpstreamCalls(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()
	s, p := newTravelScheduler(t, srv, "u1")
	s.SetNewAccountGraceDays(3)
	p.AuthByUID("u1").EnsureImportedAt(time.Now().Add(-time.Hour))
	p.SetCredits("u1", 2200, 2200)
	s.RunTravelNow()
	if got := stub.infoCalls.Load(); got != 0 {
		t.Fatalf("new account grace leaked %d upstream travel calls", got)
	}
	p.SetCredits("u1", 0, 2200) // 积分清光是明确的提前放行条件。
	s.RunTravelNow()
	if got := stub.infoCalls.Load(); got != 1 {
		t.Fatalf("zero-credit account should resume travel, calls=%d", got)
	}
}

func TestBlackcatEntrySkipsNewAccountWithoutUpstreamCalls(t *testing.T) {
	oldNow := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 24, 23, 30, 0, 0, time.UTC) }
	t.Cleanup(func() { nowForNightWindow = oldNow })
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"data":[]}`))
	}))
	defer srv.Close()
	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	a.EnsureImportedAt(time.Now().Add(-time.Hour))
	p.Add(a)
	p.SetCredits("u1", 2200, 2200)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, NewAccountGraceDays: 3})
	s.RunBlackcatNow()
	if got := calls.Load(); got != 0 {
		t.Fatalf("new account grace leaked %d blackcat upstream calls", got)
	}
	p.SetCredits("u1", 0, 2200)
	s.RunBlackcatNow()
	if got := calls.Load(); got != 1 {
		t.Fatalf("zero-credit account should query blackcat tasks once, calls=%d", got)
	}
}
