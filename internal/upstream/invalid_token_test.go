package upstream

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestInvalidTokenMapsToSessionDead APISIX 对「会话已被终止」的固定答复是
// 401 + `WWW-Authenticate: ... error="invalid_token"` + 默认 HTML 页。
// 这类错误必须归 ErrSessionDead（走连续计数→禁用链路），且 message 归一化成
// 一句人话——原始 HTML 进面板会显示成一坨标签（2026-10-23 实测：941****5）。
func TestInvalidTokenMapsToSessionDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="copilot", error="invalid_token", error_description="invalid token"`)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`<html><head><title>401 Authorization Required</title></head>` +
			`<body><center><h1>401 Authorization Required</h1></center></body></html>`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	_, err := c.GetCompensationStatus(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("错误类型 = %T, want *upstream.Error", err)
	}
	if ue.Kind != ErrSessionDead {
		t.Errorf("Kind = %v, want ErrSessionDead（invalid_token 是终态，不能落 ErrClient 只换号）", ue.Kind)
	}
	if strings.Contains(strings.ToLower(ue.Msg), "<html") || strings.Contains(ue.Msg, "</") {
		t.Errorf("message 仍含 HTML 片段: %q", ue.Msg)
	}
	if !strings.Contains(ue.Msg, "重新登录") {
		t.Errorf("message = %q, 应给出可执行结论「重新登录」", ue.Msg)
	}
}

// TestOther401StaysClient 不带 invalid_token 声明的 401 不得升级成 session_dead：
// WAF/代理层的普通 401 若被当成死会话，会误禁用健康账号（12153 误杀的历史教训）。
func TestOther401StaysClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`<html><body>401 Authorization Required</body></html>`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	_, err := c.GetCompensationStatus(&auth.Auth{UID: "u1", AccessToken: "at"})
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("错误类型 = %T, want *upstream.Error", err)
	}
	if ue.Kind == ErrSessionDead {
		t.Error("普通 401（无 invalid_token）不应归 session_dead，否则会误杀账号")
	}
}

// TestIsCredsDead 只有「401 invalid_token」算凭证已死（终态、面板可一键剔除）；
// 12153 offline session 与普通 4xx 都不算——12153 可能被临时性触发，交给连续计数。
func TestIsCredsDead(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"传输层错误", errors.New("connection reset"), false},
		{"invalid_token", &Error{Kind: ErrSessionDead, Status: 401,
			Msg: "凭证已失效（401 invalid_token）：上游会话已被终止，refresh 也无法恢复，请重新登录该账号"}, true},
		{"12153 不带 invalid_token", &Error{Kind: ErrSessionDead, Status: 400,
			Msg: "code=12153 msg=Offline user session not found"}, false},
		{"非 session_dead 类", &Error{Kind: ErrSoftRate, Status: 429, Msg: "invalid_token 出现在限流文案里"}, false},
		{"包装后的错误", fmt.Errorf("wrap: %w", &Error{Kind: ErrSessionDead, Status: 401, Msg: "x invalid_token y"}), true},
	}
	for _, c := range cases {
		if got := IsCredsDead(c.err); got != c.want {
			t.Errorf("%s: IsCredsDead=%v want %v", c.name, got, c.want)
		}
	}
	// typed nil *Error 不能 panic（错误可能是 (*Error)(nil) 包装进 interface）。
	var typedNil *Error
	if IsCredsDead(typedNil) {
		t.Error("typed nil 应返回 false")
	}
}
