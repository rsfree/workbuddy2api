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

// TestCompensationMarksDeadSession 补偿查询遇到 invalid_token（会话已终止）时，
// 面板文案要归一化，并且把这次失败计入 session_dead 连续计数——攒够阈值账号自动
// 禁用，不必等人盯着「查询失败」那行自己判断。
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
	// 单次失败不应立刻禁用（阈值保护，防 12153 误杀）。
	if st, _ := pl.Status("u1"); st.Disabled {
		t.Fatal("首次失败就禁用属于过度反应，应攒够阈值")
	}

	// 攒到阈值 → 自动禁用。
	for i := 1; i < pool.SessionDeadThreshold(); i++ {
		s.CompensationStatusAll()
	}
	if st, _ := pl.Status("u1"); !st.Disabled {
		t.Fatalf("连续 %d 次 invalid_token 后仍未禁用", pool.SessionDeadThreshold())
	}
}
