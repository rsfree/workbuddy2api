package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestObserveLegacyCatalogFeedsDirectory v1 关卡是此前完全没被扫到的第三套口径，
// 每轮只取一个代表账号扫一次，写进目录但**不进执行队列**（无 reward、claim 恒 400）。
func TestObserveLegacyCatalogFeedsDirectory(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/activity/growth/tasks" {
			t.Errorf("path=%s want /activity/growth/tasks（v1 口径无 /v2 前缀）", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls++
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[` +
			`{"code":"first_chat","title":"完成一次对话","level_name":"养虾尝试","status":"done"},` +
			`{"code":"skill_installed","title":"完成技能安装","level_name":"养虾入门","status":"available"}]}}`))
	}))
	t.Cleanup(srv.Close)

	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}})
	p.cfg.Pool.Add(&auth.Auth{AccessToken: "at1", UID: "u1"})
	p.cfg.Pool.Add(&auth.Auth{AccessToken: "at2", UID: "u2"})

	n := p.observeLegacyCatalog()
	if n != 2 {
		t.Fatalf("新码数=%d want 2", n)
	}
	// 只扫一个账号：v1 关卡是全局配置，逐号扫纯属浪费（200+ 账号各一次请求）。
	if calls != 1 {
		t.Errorf("上游请求 %d 次 want 1（只取代表账号）", calls)
	}

	entries := p.catalog().snapshot()
	if len(entries) != 2 {
		t.Fatalf("目录条目=%d want 2", len(entries))
	}
	for _, e := range entries {
		if e.Source != sourceLegacy {
			t.Errorf("%s: Source=%q want legacy", e.Code, e.Source)
		}
		if e.Auto != legacyAutoNote {
			t.Errorf("%s: Auto=%q want legacyAutoNote", e.Code, e.Auto)
		}
		if e.Credit != 0 {
			t.Errorf("%s: Credit=%d want 0（v1 无奖励）", e.Code, e.Credit)
		}
	}

	// 再扫一轮：码已存在 ⇒ 新码数 0（不会每轮都报「新任务」）。
	if n := p.observeLegacyCatalog(); n != 0 {
		t.Errorf("重复扫描新码数=%d want 0", n)
	}
}
