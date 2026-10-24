package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestAcceptStuckLifecycle 负缓存基本语义：记入 / 命中 / 解除 / TTL 到期自动失效。
func TestAcceptStuckLifecycle(t *testing.T) {
	p := &Panel{}

	if p.acceptStuckActive("u1", "chat_5") {
		t.Fatal("未记入时不应命中退避")
	}
	p.markAcceptStuck("u1", []string{"chat_5", "skill_1"})
	if !p.acceptStuckActive("u1", "chat_5") || !p.acceptStuckActive("u1", "skill_1") {
		t.Fatal("记入后应命中退避")
	}
	// 账号维度隔离：别的账号同名任务码不受影响。
	if p.acceptStuckActive("u2", "chat_5") {
		t.Fatal("退避必须按账号隔离，u2 不应被 u1 影响")
	}
	// 解除只影响指定码。
	p.clearAcceptStuck("u1", []string{"chat_5"})
	if p.acceptStuckActive("u1", "chat_5") {
		t.Fatal("clear 后不应再命中")
	}
	if !p.acceptStuckActive("u1", "skill_1") {
		t.Fatal("clear 其他码不应误清 skill_1")
	}
}

// TestAcceptStuckExpires 到期后必须自动失效，且读取路径顺手清理（无需定时 GC）。
func TestAcceptStuckExpires(t *testing.T) {
	p := &Panel{}
	p.acceptStuck = map[string]time.Time{
		acceptStuckKey("u1", "chat_5"): time.Now().Add(-time.Second), // 已过期
	}
	if p.acceptStuckActive("u1", "chat_5") {
		t.Fatal("已过期的退避不应命中")
	}
	p.acceptStuckMu.Lock()
	_, still := p.acceptStuck[acceptStuckKey("u1", "chat_5")]
	p.acceptStuckMu.Unlock()
	if still {
		t.Fatal("过期条目应在读取时被删除")
	}
}

// TestPruneAcceptStuck 只剪过期条目，未过期的保留。
func TestPruneAcceptStuck(t *testing.T) {
	p := &Panel{}
	p.acceptStuck = map[string]time.Time{
		acceptStuckKey("u1", "old"): time.Now().Add(-time.Hour),
		acceptStuckKey("u1", "new"): time.Now().Add(time.Hour),
	}
	p.pruneAcceptStuck()
	p.acceptStuckMu.Lock()
	n := len(p.acceptStuck)
	p.acceptStuckMu.Unlock()
	if n != 1 {
		t.Fatalf("剪枝后应只剩 1 条（未过期的），got %d", n)
	}
	if !p.acceptStuckActive("u1", "new") {
		t.Fatal("未过期的条目不应被剪掉")
	}
}

// growthSrv 起一个假上游：任务列表的 accept_status 由 status 决定，
// 并统计收到的 accept 请求次数。
func growthSrv(t *testing.T, status string, accepts *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"tasks": []map[string]any{
					{"task_code": "chat_5", "title": "对话 5 次", "target": 5, "current": 0,
						"accept_status": status, "reward_credit": 100, "reward_energy": 5},
				}},
			})
		case r.Method == http.MethodPost:
			*accepts++
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		}
	}))
}

// TestSettleAcceptBackoffMarksWhenStatusDoesNotFlip 复现线上真实形态：accept 回 200
// 但回读仍是 not_accepted → 必须记进退避表，否则每 15 分钟重复空转。
func TestSettleAcceptBackoffMarksWhenStatusDoesNotFlip(t *testing.T) {
	accepts := 0
	srv := growthSrv(t, "not_accepted", &accepts)
	defer srv.Close()
	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, WebBaseCN: srv.URL, BillingBaseCN: srv.URL,
	}}}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	p.settleAcceptBackoff(a, "u1", []string{"chat_5"})
	if !p.acceptStuckActive("u1", "chat_5") {
		t.Fatal("回读仍 not_accepted 时必须记进退避（这是线上空转的根因）")
	}
}

// TestSettleAcceptBackoffClearsWhenStatusFlips accept 真的落账时不能把码钉在退避表里。
func TestSettleAcceptBackoffClearsWhenStatusFlips(t *testing.T) {
	accepts := 0
	srv := growthSrv(t, "accepted", &accepts)
	defer srv.Close()
	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, WebBaseCN: srv.URL, BillingBaseCN: srv.URL,
	}}}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	// 先人为记入，模拟上一轮判定过；本轮回读翻成 accepted 应被清除。
	p.markAcceptStuck("u1", []string{"chat_5"})
	p.settleAcceptBackoff(a, "u1", []string{"chat_5"})
	if p.acceptStuckActive("u1", "chat_5") {
		t.Fatal("状态已翻成 accepted 时必须解除退避")
	}
}

// TestSettleAcceptBackoffSkipsEmptyAttempt 没发过 accept 就不该产生任何上游调用，
// 也不该改动退避表（调用方已按 ok+mpOK>0 门控，这里是第二道保险）。
func TestSettleAcceptBackoffSkipsEmptyAttempt(t *testing.T) {
	accepts := 0
	srv := growthSrv(t, "not_accepted", &accepts)
	defer srv.Close()
	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, WebBaseCN: srv.URL, BillingBaseCN: srv.URL,
	}}}
	p.settleAcceptBackoff(&auth.Auth{AccessToken: "at", UID: "u1"}, "u1", nil)
	if accepts != 0 {
		t.Fatalf("attempted 为空时不应有上游调用，got %d", accepts)
	}
	p.acceptStuckMu.Lock()
	n := len(p.acceptStuck)
	p.acceptStuckMu.Unlock()
	if n != 0 {
		t.Fatalf("attempted 为空时不应写入退避表，got %d 条", n)
	}
}
