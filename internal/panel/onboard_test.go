package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestParseOnboardMode 参数归一化：缺省 auto、假值关闭、真值强制、乱填回落 auto。
func TestParseOnboardMode(t *testing.T) {
	cases := map[string]string{
		// 缺省与非法值一律 off：批量导入按账号拆单请求，缺省 auto 会被拆单绕成"每次都只进 1 个新号"。
		"":        onboardModeOff,
		"  ":      onboardModeOff,
		"garbage": onboardModeOff,
		"auto":    onboardModeAuto,
		"AUTO":    onboardModeAuto,
		"default": onboardModeAuto,
		"1":       onboardModeForce,
		"true":    onboardModeForce,
		"on":      onboardModeForce,
		"force":   onboardModeForce,
		"0":       onboardModeOff,
		"false":   onboardModeOff,
		"off":     onboardModeOff,
		"no":      onboardModeOff,
	}
	for in, want := range cases {
		if got := parseOnboardMode(in); got != want {
			t.Errorf("parseOnboardMode(%q)=%q want %q", in, got, want)
		}
	}
}

// TestShouldOnboard 触发判定：auto 只认单号（授权链接登录形态），批量不动。
func TestShouldOnboard(t *testing.T) {
	cases := []struct {
		mode string
		n    int
		want bool
	}{
		{onboardModeAuto, 0, false},
		{onboardModeAuto, 1, true},
		{onboardModeAuto, 2, false},
		{onboardModeAuto, 20, false},
		{onboardModeForce, 0, false},
		{onboardModeForce, 1, true},
		{onboardModeForce, 20, true},
		{onboardModeOff, 1, false},
		{onboardModeOff, 20, false},
	}
	for _, c := range cases {
		if got := shouldOnboard(c.mode, c.n); got != c.want {
			t.Errorf("shouldOnboard(%q,%d)=%v want %v", c.mode, c.n, got, c.want)
		}
	}
}

// waitOnboard 等 worker 把队列消化到给定条数（避免测试里 sleep 定长）。
func waitOnboard(t *testing.T, p *Panel, want int) []onboardRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.onboardMu.Lock()
		n := len(p.onboardLog)
		out := append([]onboardRecord(nil), p.onboardLog...)
		p.onboardMu.Unlock()
		if n >= want {
			finished := 0
			for _, r := range out {
				if r.Status != "running" {
					finished++
				}
			}
			if finished >= want {
				return out
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 onboard 记录超时（want=%d）", want)
	return nil
}

// TestOnboardWorkerRecordsDelta 两个账号入队：串行执行（并发恒为 1），
// 留痕记录 before/after/delta/claimed，顺序与入队一致。
func TestOnboardWorkerRecordsDelta(t *testing.T) {
	p := &Panel{}
	var running, maxRunning atomic.Int32
	var order []string
	p.onboardRun = func(_ *Panel, a *auth.Auth) (int, int64, int64, error) {
		cur := running.Add(1)
		for {
			m := maxRunning.Load()
			if cur <= m || maxRunning.CompareAndSwap(m, cur) {
				break
			}
		}
		order = append(order, a.UID)
		time.Sleep(15 * time.Millisecond)
		running.Add(-1)
		return 7, 1962, 4162, nil
	}

	p.enqueueOnboard([]*auth.Auth{
		{UID: "u-1", Nickname: "n1"},
		{UID: "u-2", Nickname: "n2"},
	}, "test")

	recs := waitOnboard(t, p, 2)
	if got := maxRunning.Load(); got != 1 {
		t.Errorf("worker 未串行：最大并发=%d", got)
	}
	if len(order) != 2 || order[0] != "u-1" || order[1] != "u-2" {
		t.Errorf("执行顺序=%v want [u-1 u-2]", order)
	}
	for _, r := range recs {
		if r.Status != "done" {
			t.Errorf("uid=%s status=%s want done", r.UID, r.Status)
		}
		if r.CreditsBefore != 1962 || r.CreditsAfter != 4162 || r.Delta != 2200 {
			t.Errorf("uid=%s 积分留痕错误: %d→%d delta=%d", r.UID, r.CreditsBefore, r.CreditsAfter, r.Delta)
		}
		if r.Claimed != 7 {
			t.Errorf("uid=%s claimed=%d want 7", r.UID, r.Claimed)
		}
		if r.Reason != "test" {
			t.Errorf("uid=%s reason=%q", r.UID, r.Reason)
		}
		if r.FinishedAt < r.StartedAt || r.FinishedAt == 0 {
			t.Errorf("uid=%s 时间戳异常 %d -> %d", r.UID, r.StartedAt, r.FinishedAt)
		}
	}
}

// TestOnboardMarkSkipped 企业版/忙碌等「不跑」情形记 skipped 而非 error。
func TestOnboardMarkSkipped(t *testing.T) {
	p := &Panel{}
	p.onboardRun = func(_ *Panel, _ *auth.Auth) (int, int64, int64, error) {
		return 0, 0, 0, errOnboardEnterprise
	}
	p.enqueueOnboard([]*auth.Auth{{UID: "ent-1", Nickname: "e"}}, "test")
	recs := waitOnboard(t, p, 1)
	if recs[0].Status != "skipped" {
		t.Fatalf("status=%s want skipped", recs[0].Status)
	}
	if recs[0].Message == "" {
		t.Fatal("skipped 记录应带原因")
	}
}

// TestOnboardMarkError 真错误记 error 并保留消息。
func TestOnboardMarkError(t *testing.T) {
	p := &Panel{}
	p.onboardRun = func(_ *Panel, _ *auth.Auth) (int, int64, int64, error) {
		return 0, 100, 100, fmt.Errorf("读取基线余额: boom")
	}
	p.enqueueOnboard([]*auth.Auth{{UID: "bad-1"}}, "test")
	recs := waitOnboard(t, p, 1)
	if recs[0].Status != "error" || recs[0].Message == "" {
		t.Fatalf("status=%s msg=%q want error+msg", recs[0].Status, recs[0].Message)
	}
}

// TestOnboardStatusHandler 查询接口：最新在前、limit 生效、total 为全量。
func TestOnboardStatusHandler(t *testing.T) {
	p := &Panel{}
	p.onboardLog = []onboardRecord{
		{UID: "a", Status: "done", Delta: 100},
		{UID: "b", Status: "done", Delta: 200},
		{UID: "c", Status: "running"},
	}
	rec := httptest.NewRecorder()
	p.onboardStatus(rec, httptest.NewRequest(http.MethodGet, "/panel/api/onboard?limit=2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("http=%d", rec.Code)
	}
	var body struct {
		OK      bool            `json:"ok"`
		Total   int             `json:"total"`
		Records []onboardRecord `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != 3 || len(body.Records) != 2 {
		t.Fatalf("total=%d len=%d want 3/2", body.Total, len(body.Records))
	}
	if body.Records[0].UID != "c" || body.Records[1].UID != "b" {
		t.Fatalf("顺序错误: %v", []string{body.Records[0].UID, body.Records[1].UID})
	}
}

// importPanel 搭一个可直接调 importCockpit 的面板（假上游兜底 200，不触真实网络）。
func importPanel(t *testing.T) *Panel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	t.Cleanup(srv.Close)
	pl := pool.New("")
	return &Panel{cfg: Config{
		Pool: pl,
		Upstream: &upstream.Client{
			HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
		},
		AuthDir: t.TempDir(),
	}}
}

// cockpitBody 拼 multipart 请求体：N 个账号放在**同一个** file 字段的 JSON 数组里
// （服务端 FormFile 只读第一份，拆成多个 file 字段等于只导入第一个）。
func cockpitBody(t *testing.T, uids ...string) (*bytes.Buffer, string) {
	t.Helper()
	var items []map[string]any
	for _, uid := range uids {
		items = append(items, map[string]any{
			"uid": uid, "nickname": "nick-" + uid,
			"access_token": "at-" + uid, "refresh_token": "rt-" + uid,
			"expires_at": time.Now().Add(720 * time.Hour).UnixMilli(),
			"domain":     "www.codebuddy.cn",
		})
	}
	raw, _ := json.Marshal(items)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "a.json")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	fw.Write(raw)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

// TestImportAutoTriggersOnboardForSingleNewAccount 授权链接登录的形态：
// 单账号导入 + 显式 onboard=auto → 响应 onboard_queued=1 → 队列跑完留痕。
func TestImportAutoTriggersOnboardForSingleNewAccount(t *testing.T) {
	p := importPanel(t)
	ran := make(chan string, 4)
	p.onboardRun = func(_ *Panel, a *auth.Auth) (int, int64, int64, error) {
		ran <- a.UID
		return 20, 1947, 4147, nil
	}

	body, ct := cockpitBody(t, "uid-single")
	req := httptest.NewRequest(http.MethodPost, "/panel/api/import/cockpit?onboard=auto", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	p.importCockpit(rec, req)

	var resp struct {
		Imported      int `json:"imported"`
		OnboardQueued int `json:"onboard_queued"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.Imported != 1 || resp.OnboardQueued != 1 {
		t.Fatalf("imported=%d queued=%d want 1/1 body=%s", resp.Imported, resp.OnboardQueued, rec.Body.String())
	}
	select {
	case uid := <-ran:
		if uid != "uid-single" {
			t.Fatalf("onboard 跑的是 %s", uid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onboard 未执行")
	}
	recs := waitOnboard(t, p, 1)
	if recs[0].Delta != 2200 || recs[0].Status != "done" {
		t.Fatalf("留痕错误: %+v", recs[0])
	}
}

// TestImportAutoSkipsBatch 不该触发的三种形态：批量（即使显式 auto，多号判据不满足）、
// 单号但显式关闭、**单号裸请求（缺省 off，安全侧）**。
func TestImportAutoSkipsBatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		uids  []string
	}{
		{"batch-auto", "?onboard=auto", []string{"uid-a", "uid-b"}},
		{"single-off", "?onboard=0", []string{"uid-c"}},
		{"single-no-param", "", []string{"uid-d"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := importPanel(t)
			var calls atomic.Int32
			p.onboardRun = func(_ *Panel, _ *auth.Auth) (int, int64, int64, error) {
				calls.Add(1)
				return 0, 0, 0, nil
			}
			body, ct := cockpitBody(t, tc.uids...)
			req := httptest.NewRequest(http.MethodPost, "/panel/api/import/cockpit"+tc.query, body)
			req.Header.Set("Content-Type", ct)
			rec := httptest.NewRecorder()
			p.importCockpit(rec, req)

			var resp struct {
				OnboardQueued int `json:"onboard_queued"`
			}
			json.Unmarshal(rec.Body.Bytes(), &resp)
			if resp.OnboardQueued != 0 {
				t.Fatalf("queued=%d want 0 body=%s", resp.OnboardQueued, rec.Body.String())
			}
			time.Sleep(50 * time.Millisecond)
			if calls.Load() != 0 {
				t.Fatalf("不应执行 onboard，实际跑了 %d 次", calls.Load())
			}
		})
	}
}

// TestImportForceOnboardBatch onboard=1 时批量也全部触发。
func TestImportForceOnboardBatch(t *testing.T) {
	p := importPanel(t)
	var calls atomic.Int32
	p.onboardRun = func(_ *Panel, _ *auth.Auth) (int, int64, int64, error) {
		calls.Add(1)
		return 1, 10, 110, nil
	}
	body, ct := cockpitBody(t, "uid-x", "uid-y", "uid-z")
	req := httptest.NewRequest(http.MethodPost, "/panel/api/import/cockpit?onboard=1", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	p.importCockpit(rec, req)

	var resp struct {
		OnboardQueued int `json:"onboard_queued"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.OnboardQueued != 3 {
		t.Fatalf("queued=%d want 3 body=%s", resp.OnboardQueued, rec.Body.String())
	}
	waitOnboard(t, p, 3)
	if calls.Load() != 3 {
		t.Fatalf("执行次数=%d want 3", calls.Load())
	}
}

// TestOnboardOnLoginQueuesNewAccount 授权登录成功（新号）→ 立即入队执行。
// 这是面板 login/start → 浏览器授权 → login/poll 链路的触发点：账号直接进池，
// 全程不经过 import/cockpit，所以触发必须挂在这里而不是导入接口上。
func TestOnboardOnLoginQueuesNewAccount(t *testing.T) {
	p := &Panel{}
	ran := make(chan string, 1)
	p.onboardRun = func(_ *Panel, a *auth.Auth) (int, int64, int64, error) {
		ran <- a.UID
		return 20, 1947, 4147, nil
	}

	if ok := p.onboardOnLogin(&auth.Auth{UID: "login-new", Nickname: "n"}, true, "cn"); !ok {
		t.Fatal("新号登录应触发自动清任务")
	}
	select {
	case uid := <-ran:
		if uid != "login-new" {
			t.Fatalf("onboard 跑的是 %s", uid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("登录成功后 onboard 未执行")
	}
	recs := waitOnboard(t, p, 1)
	if recs[0].Reason != "login:poll:cn" {
		t.Errorf("reason=%q want login:poll:cn", recs[0].Reason)
	}
	if recs[0].Status != "done" || recs[0].Delta != 2200 {
		t.Errorf("留痕错误: %+v", recs[0])
	}
}

// TestOnboardOnLoginSkipsExisting 老号重登（只是刷凭证）不触发：
// 任务幂等重跑无新增收益，只白耗真实对话。
func TestOnboardOnLoginSkipsExisting(t *testing.T) {
	p := &Panel{}
	var calls atomic.Int32
	p.onboardRun = func(_ *Panel, _ *auth.Auth) (int, int64, int64, error) {
		calls.Add(1)
		return 0, 0, 0, nil
	}

	if ok := p.onboardOnLogin(&auth.Auth{UID: "login-old"}, false, "cn"); ok {
		t.Fatal("老号重登不应触发")
	}
	time.Sleep(80 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("不应执行 onboard，实际跑了 %d 次", calls.Load())
	}
	p.onboardMu.Lock()
	n := len(p.onboardLog)
	p.onboardMu.Unlock()
	if n != 0 {
		t.Fatalf("不应有留痕，实际 %d 条", n)
	}
}

// TestOnboardOnLoginNilAuth 空凭证兜底：不 panic、不入队。
func TestOnboardOnLoginNilAuth(t *testing.T) {
	p := &Panel{}
	if ok := p.onboardOnLogin(nil, true, "cn"); ok {
		t.Fatal("nil 凭证不应入队")
	}
}
