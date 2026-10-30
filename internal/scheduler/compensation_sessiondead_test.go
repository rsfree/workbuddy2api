package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestCompensationMarksDeadSession 补偿查询遇到 invalid_token（会话已终止）时：
// 面板文案要归一化，并且**一次即**打「凭证已失效」标签 + 禁用。
//
// 口径变化（2026-10-30）：invalid_token 是确定性终态（access token 与 refresh 双死），
// 原先和 12153 共用「连续 3 次才禁用」的阈值，结果是任务/补偿链路里一个死号要连续
// 撞墙 3 轮才退出池子，期间每个动作都白打一轮 401。阈值只留给 12153（那个才会被
// 瞬时误报，见下一个用例）。
func TestCompensationMarksDeadSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="copilot", error="invalid_token"`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`<html><body><h1>401 Authorization Required</h1></body></html>`))
	}))
	defer srv.Close()

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "坏凭证", AccessToken: "at"})
	s := New(Config{Pool: pl, Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}})

	first := s.CompensationStatusAll()
	if len(first) != 1 {
		t.Fatalf("rows = %d, want 1", len(first))
	}
	if !strings.Contains(first[0].Error, "重新登录") {
		t.Errorf("error = %q, 应是归一化后的人话", first[0].Error)
	}
	if strings.Contains(strings.ToLower(first[0].Error), "<html") {
		t.Errorf("error 仍含 HTML: %q", first[0].Error)
	}
	// invalid_token 一次即终态：打标签 + 禁用（面板随即出现「凭证失效」红标，
	// 并可一键剔除此号）。
	st, _ := pl.Status("u1")
	if !st.SessionDead {
		t.Fatal("invalid_token 应打上「凭证已失效」标签")
	}
	if !st.Disabled {
		t.Fatal("invalid_token 是终态，应立刻退出选号池")
	}

	// 再跑一次仍是幂等的（不 panic、不改结论）。
	s.CompensationStatusAll()
	if st2, _ := pl.Status("u1"); !st2.SessionDead || !st2.Disabled {
		t.Fatalf("重复观测后的状态漂移: %+v", st2)
	}
}

// TestCompensationCounts12153WithoutDisabling 12153 offline session 可能被瞬时触发
// （网络抖动/上游闪断/refresh 竞态），**不能**一次判死：仍走 pool.NoteSessionDead
// 的连续计数，攒够阈值才禁用，且全程不打「凭证已失效」标签（那标签意味着"确定要
// 重新登录"，误标会让运维去剔一个其实还能用的号）。
func TestCompensationCounts12153WithoutDisabling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "疑似", AccessToken: "at"})
	s := New(Config{Pool: pl, Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}})

	s.CompensationStatusAll()
	if st, _ := pl.Status("u1"); st.Disabled || st.SessionDead {
		t.Fatalf("单次 12153 不该判死/打标签: %+v", st)
	}
	for i := 1; i < pool.SessionDeadThreshold(); i++ {
		s.CompensationStatusAll()
	}
	st, _ := pl.Status("u1")
	if !st.Disabled {
		t.Fatalf("连续 %d 次 12153 后应禁用", pool.SessionDeadThreshold())
	}
	// 判死即等同于「只能重新登录」（12153 与 invalid_token 的终态和解法相同）：
	// 标签必须一并出现，否则这批号在面板上只是「已禁用·原因不明」，进不了
	// 「剔除失效号」的清单。区别只在判死前的门槛（3 次 vs 1 次）。
	if !st.SessionDead {
		t.Error("12153 攒够阈值判死后应打「凭证已失效」标签（终态与 invalid_token 相同）")
	}
}
