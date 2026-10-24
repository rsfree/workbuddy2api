package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// TestStickyOverflowKeepsBinding 覆盖 2026-10-24 修的「乒乓漂移」：
//
// 粘性号只是因为并发满载（max_in_flight 打满）时，本次请求会溢出到别的号，
// 但**绑定必须留在原号**：
//   - 若在 ResolveForModel 里把满载当失效 → 解绑重分配（永久搬号）；
//   - 若在成功后无条件 Bind 到溢出号 → 每满一次搬一次（乒乓）。
//
// 两种都会让账号级上游前缀缓存全冷。线上实测：同号相邻请求命中 94.2%，
// 换号相邻请求命中 34.2%。这里断言「溢出成功后绑定仍是原号，压力解除后回原号」。
func TestStickyOverflowKeepsBinding(t *testing.T) {
	auth.SetGlobalEnabled(true)
	p := testPoolWith(
		&auth.Auth{UID: "ua", AccessToken: "ta", ExpiresAt: 9999999999},
		&auth.Auth{UID: "ub", AccessToken: "tb", ExpiresAt: 9999999999},
	)
	p.SetMaxInFlight(1) // 单号只接 1 个在途，便于制造「满载」

	// token → uid：上游只看 Authorization，这里换回 uid 便于和粘性绑定比对。
	tokenUID := map[string]string{"ta": "ua", "tb": "ub"}
	var served []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		tok := strings.TrimPrefix(authz, "Bearer ")
		uid, ok := tokenUID[tok]
		if !ok {
			uid = "unknown:" + tok
		}
		served = append(served, uid)
		return 200, sseOK, true
	})

	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     redisstore.Noop{},
		Available: p.AvailableUIDs,
		AvailableForModel: func(model string) []string {
			realm, bare := ResolveModel(model)
			return p.WeightedAvailableUIDsForModelRealm(bare, realm)
		},
		StickyUsable: func(uid, model string) bool {
			realm, bare := ResolveModel(model)
			return p.StickyEligibleForModelRealm(uid, bare, realm)
		},
	})
	// 接上请求日志记录器：粘性落点要一路写进 Event（面板换号率的数据源），
	// 这里顺便把「指标真的是从真实请求链路累计出来的」也一并锁住。
	rec := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, RequestLog: rec})

	body := `{"model":"cn:glm-5.2","stream":true,"messages":[{"role":"user","content":"sticky overflow probe"}]}`
	key := session.ExtractKey([]byte(body))
	if key == "" {
		t.Fatal("body should derive a session key")
	}
	const sessModel = "cn:glm-5.2"

	send := func() string {
		t.Helper()
		served = nil
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		if len(served) == 0 {
			t.Fatal("no upstream call recorded")
		}
		return served[len(served)-1]
	}

	first := send()
	if uid, ok := sess.LookupForModel(key, sessModel); !ok || uid != first {
		t.Fatalf("initial binding=%q ok=%v, served by %q", uid, ok, first)
	}

	// 制造满载：占掉该号唯一在途名额（handler 结束后会 Release，所以这里手动占住）。
	if !p.Acquire(first) {
		t.Fatal("acquire for saturation failed")
	}
	defer p.Release(first)

	// 满载期间：请求应溢出到另一个号成功。
	second := send()
	if second == first {
		t.Fatalf("expected overflow to another account, still served by %q", first)
	}
	// 🔴 核心断言：绑定不能漂移到溢出号。
	if uid, ok := sess.LookupForModel(key, sessModel); !ok || uid != first {
		t.Fatalf("binding drifted to overflow account: got %q ok=%v want %q", uid, ok, first)
	}

	// 再溢出一轮，确认不是"只漂一次"而是稳定不漂。
	third := send()
	if third == first {
		t.Fatalf("still saturated, should keep overflowing, got %q", first)
	}
	if uid, ok := sess.LookupForModel(key, sessModel); !ok || uid != first {
		t.Fatalf("binding drifted on repeated overflow: got %q want %q", uid, first)
	}

	// 此刻的落点分布：1 次新会话 + 2 次满载溢出，且没有一次被误记成同号命中。
	if m := rec.Snapshot().Cache; m.StickyNew != 1 || m.StickyBusy != 2 || m.StickyHit != 0 || m.StickyMiss != 0 {
		t.Fatalf("sticky metrics after overflow: %+v (want new=1 busy=2 hit=0 miss=0)", m)
	}

	// 压力解除后必须回到原号（说明缓存能重新热起来）。
	p.Release(first)
	back := send()
	if back != first {
		t.Fatalf("after release should return to bound account %q, got %q", first, back)
	}

	if m := rec.Snapshot().Cache; m.StickyHit != 1 || m.StickyBusy != 2 || m.StickyMiss != 0 {
		t.Fatalf("sticky metrics after recovery: %+v (want hit=1 busy=2 miss=0)", m)
	}

	// 持久失效（冷却/封禁/该模型限额）→ 必须换号并让绑定跟随新号，
	// 否则会话被钉在一个不可用的号上反复失败。
	p.Disable(first, "test")
	next := send()
	if next == first {
		t.Fatalf("disabled account must not serve, got %q", first)
	}
	if uid, ok := sess.LookupForModel(key, sessModel); !ok || uid != next {
		t.Fatalf("binding should follow new account %q after persistent failure, got %q ok=%v", next, uid, ok)
	}
	// 换号必须记成 miss 而不是 hit——靠 Peek 在 resolve 前抓旧绑定才分得清。
	if m := rec.Snapshot().Cache; m.StickyMiss != 1 {
		t.Fatalf("persistent failover should be recorded as miss: %+v", m)
	}
}
