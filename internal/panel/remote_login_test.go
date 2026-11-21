package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 未配置 sidecar 时必须明确 501，而不是 500 或静默成功 —— 面板前端据此
// 决定「服务器浏览器」按钮是否出现。三条路径逐一验证。
func TestRemoteLoginDisabledReturns501(t *testing.T) {
	p := New(Config{})
	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/panel/api/login/remote/start", `{"realm":"cn"}`},
		{http.MethodPost, "/panel/api/login/remote/ticket", `{"state":"x"}`},
		{http.MethodPost, "/panel/api/login/remote/stop", `{}`},
	}
	for _, c := range cases {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		p.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s: got %d, want 501（body=%s）", c.method, c.path, rr.Code, rr.Body.String())
		}
	}
}

// ticket 契约：一次性、绑定 state/sidecar ticket、过期即废、伪造一律拒绝。
// 只测正面等于没测 —— 这里每一条都带反向断言。
func TestRemoteTicketOneShotAndExpiry(t *testing.T) {
	p := New(Config{})

	tk := p.issueRemoteTicket("state-a", "side-a")
	state, side, ok := p.consumeRemoteTicket(tk)
	if !ok || state != "state-a" || side != "side-a" {
		t.Fatalf("首次消费应成功且绑定正确，got ok=%v state=%q side=%q", ok, state, side)
	}
	if _, _, ok := p.consumeRemoteTicket(tk); ok {
		t.Fatal("同一 ticket 第二次必须拒绝（一次性）")
	}

	tk2 := p.issueRemoteTicket("state-b", "side-b")
	p.remoteMu.Lock()
	v := p.remoteTickets[tk2]
	v.exp = time.Now().Add(-time.Second)
	p.remoteTickets[tk2] = v
	p.remoteMu.Unlock()
	if _, _, ok := p.consumeRemoteTicket(tk2); ok {
		t.Fatal("过期 ticket 必须拒绝")
	}

	if _, _, ok := p.consumeRemoteTicket("deadbeef"); ok {
		t.Fatal("未知 ticket 必须拒绝")
	}
	if _, _, ok := p.consumeRemoteTicket(""); ok {
		t.Fatal("空 ticket 必须拒绝")
	}
}

// WS 入口没有 withAuth（浏览器无法给 WS 加 Bearer 头），所以 ticket 是唯一
// 门槛：这里的断言就是它的安全边界。
func TestRemoteWSRejectsBadTicket(t *testing.T) {
	p := New(Config{LoginRemoteURL: "http://127.0.0.1:1", LoginRemoteToken: "tok"})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/login/remote/ws?ticket=forged", nil)
	p.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("伪造 ticket 应 401，got %d", rr.Code)
	}
}

// sidecarPost：错误体里的 error 字段要透出（面板要显示「远程浏览器启动失败：
// no browser」这种可诊断信息），共享 token 必须出现在请求头里。
func TestSidecarPost(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Remote-Token")
		switch r.URL.Path {
		case "/ok":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "s1", "ticket": "t1", "state": "live",
				"meta": map[string]int{"width": 1280, "height": 800},
			})
		case "/fail":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"no browser"}`))
		case "/plain":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		}
	}))
	defer srv.Close()

	var out sidecarSession
	if err := sidecarPost(srv.URL, "secret", "/ok", map[string]string{"url": "https://x"}, 2*time.Second, &out); err != nil {
		t.Fatalf("成功路径不应报错：%v", err)
	}
	if out.ID != "s1" || out.Ticket != "t1" || out.Meta.Width != 1280 {
		t.Fatalf("响应解析错误：%+v", out)
	}
	if gotToken != "secret" {
		t.Fatalf("共享 token 未透传，got %q", gotToken)
	}

	err := sidecarPost(srv.URL, "secret", "/fail", nil, 2*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "no browser") {
		t.Fatalf("错误体应透出 error 字段，got %v", err)
	}

	err = sidecarPost(srv.URL, "secret", "/plain", nil, 2*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("非 JSON 错误体也应可见，got %v", err)
	}
}

// realm 解析：缺省/非法回落 cn，只有显式 global 才切域（与 loginStart 原行为一致）。
func TestParseRealmBody(t *testing.T) {
	cases := map[string]string{
		`{"realm":"global"}`: "global",
		`{"realm":"cn"}`:     "cn",
		`{"realm":"GLOBAL"}`: "cn",
		`{"realm":"us"}`:     "cn",
		`{}`:                 "cn",
		`not-json`:           "cn",
		``:                   "cn",
	}
	for body, want := range cases {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		if got := parseRealmBody(req); got != want {
			t.Fatalf("body=%q: got %q, want %q", body, got, want)
		}
	}
}
