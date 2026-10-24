package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// newAffinityTestHandler 装配「多号 + 会话粘性 + 前缀亲和」的 handler，闭包口径与
// 生产 cmd/server/wiring.go 完全一致（AvailableForModel 含在途、StickyUsable 不含）。
//
// 返回 served 指针：每次上游调用把 Authorization 的 token 换成 uid 追加进去，
// 便于断言「这次是谁服务的」。
func newAffinityTestHandler(t *testing.T, p *pool.Pool, tokens map[string]string, rec *reqlog.Recorder) (h *Handler, served *[]string) {
	t.Helper()
	auth.SetGlobalEnabled(true)

	tokenUID := tokens
	out := &[]string{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		tok := strings.TrimPrefix(authz, "Bearer ")
		uid, ok := tokenUID[tok]
		if !ok {
			uid = "unknown:" + tok
		}
		*out = append(*out, uid)
		return 200, sseOK, true
	})

	sess := session.New(session.Config{
		TTL:            time.Minute,
		Store:          redisstore.Noop{},
		Available:      p.AvailableUIDs,
		PrefixAffinity: true,
		AvailableForModel: func(model string) []string {
			realm, bare := ResolveModel(model)
			return p.WeightedAvailableUIDsForModelRealm(bare, realm)
		},
		StickyUsable: func(uid, model string) bool {
			realm, bare := ResolveModel(model)
			return p.StickyEligibleForModelRealm(uid, bare, realm)
		},
	})
	return NewHandler(Config{Pool: p, Upstream: up, Session: sess, RequestLog: rec}), out
}

// affinityBody 造一个「共用同一段 system 提示词、但属于不同会话」的请求体。
//
// 注意 messages 里**故意不放** conversation_id：真实客户端（IDE 插件类）确实是靠
// system+首条 user 派生会话键的，而这正是前缀亲和要救的场景——派生键含 user 文本，
// 十个并行任务就是十个键、散到十个号上，那段几万 token 的 system 提示词被冷启动十次。
func affinityBody(conv, task string) string {
	return `{"model":"cn:glm-5.2","stream":true,"metadata":{"conversation_id":"` + conv + `"},` +
		`"messages":[{"role":"system","content":"you are a coding agent, shared long prompt"},` +
		`{"role":"user","content":"` + task + `"}]}`
}

// affinityKeyFor 复算 handler 内部的亲和键（前缀指纹 + 规范化模型名），
// 用来从外部观察注册表状态。
func affinityKeyFor(body string) string {
	return session.PrefixKey([]byte(body)) + "\x00" + "cn:glm-5.2"
}

// TestAffinityConvergesNewConversations 是本功能的主断言：
// **同一段 system 提示词、不同会话**必须被撮合到同一个账号。
//
// 这是会话粘性做不到的：粘性的键是会话，新会话一律走哈希分配，于是每个新会话都是
// 一次冷启动。线上换号请求的前缀命中率 34.2%（而非 0）说明账号里本来就散落着
// 同前缀碎片——把它们收拢到一个号上就是这里的收益。
func TestAffinityConvergesNewConversations(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "ua", AccessToken: "ta", ExpiresAt: 9999999999},
		&auth.Auth{UID: "ub", AccessToken: "tb", ExpiresAt: 9999999999},
		&auth.Auth{UID: "uc", AccessToken: "tc", ExpiresAt: 9999999999},
		&auth.Auth{UID: "ud", AccessToken: "td", ExpiresAt: 9999999999},
	)
	rec := reqlog.New(reqlog.Config{})
	h, served := newAffinityTestHandler(t, p, map[string]string{
		"ta": "ua", "tb": "ub", "tc": "uc", "td": "ud",
	}, rec)

	send := func(conv, task string) string {
		t.Helper()
		*served = nil
		body := affinityBody(conv, task)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("conv=%s code=%d body=%s", conv, w.Code, w.Body.String())
		}
		if len(*served) == 0 {
			t.Fatalf("conv=%s: no upstream call recorded", conv)
		}
		return (*served)[len(*served)-1]
	}

	first := send("conv-1", "task one")
	// 4 个号、4 个不同会话：没有亲和的话哈希分配约等于随机，几乎必然会散开。
	for i := 2; i <= 4; i++ {
		got := send("conv-"+string(rune('0'+i)), "task number "+string(rune('0'+i)))
		if got != first {
			t.Fatalf("conversation %d landed on %q, want the same account %q as conv-1 "+
				"(affinity must collapse same-prefix conversations)", i, got, first)
		}
	}

	m := rec.Snapshot().Cache
	if m.AffinityNew != 1 {
		t.Errorf("affinity_new=%d want 1 (only the first request establishes affinity)", m.AffinityNew)
	}
	if m.AffinityHit != 3 {
		t.Errorf("affinity_hit=%d want 3", m.AffinityHit)
	}
	if m.AffinityHitRate != 75 {
		t.Errorf("affinity_hit_rate=%v want 75 (3/4)", m.AffinityHitRate)
	}
}

// TestAffinityKeepsTargetWhenBusy 锁住「亲和号满载时不回写」：
//
// 亲和号临时满载 → 本次溢出到别的号 → 绑定落在**实际服务的号**上（该会话的下一次
// 直接命中它，不再从亲和出发），但**亲和记录仍指向原号**——否则整段前缀会被拖到
// 这个临时接活的账号，下次又搬一次，形成前缀级乒乓。
//
// 对照会话粘性的同类场景（stickyOverflow）：那里选择「绑定留在原号」，因为请求的
// 是「别把这段会话搬走」；这里选择「绑定跟随服务号」，因为新会话的缓存确实暖在了
// 服务号上。两者不冲突，判据都是「缓存实际在哪」。
func TestAffinityKeepsTargetWhenBusy(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "ua", AccessToken: "ta", ExpiresAt: 9999999999},
		&auth.Auth{UID: "ub", AccessToken: "tb", ExpiresAt: 9999999999},
	)
	p.SetMaxInFlight(1) // 单号只接 1 个在途，便于制造「满载」
	rec := reqlog.New(reqlog.Config{})
	h, served := newAffinityTestHandler(t, p, map[string]string{"ta": "ua", "tb": "ub"}, rec)

	sess := h.cfg.Session
	send := func(conv, task string) string {
		t.Helper()
		*served = nil
		body := affinityBody(conv, task)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("conv=%s code=%d body=%s", conv, w.Code, w.Body.String())
		}
		return (*served)[len(*served)-1]
	}

	first := send("conv-1", "task one")
	affKey := affinityKeyFor(affinityBody("conv-1", "task one"))
	if uid, ok := sess.AffinityFor(affKey, "cn:glm-5.2"); !ok || uid != first {
		t.Fatalf("affinity after first request: uid=%q ok=%v want %q", uid, ok, first)
	}

	// 占住亲和号的唯一在途名额 → 后续新会话必然溢出。
	if !p.Acquire(first) {
		t.Fatal("acquire for saturation failed")
	}

	second := send("conv-2", "task two")
	if second == first {
		t.Fatalf("saturated account should not serve, got %q", first)
	}
	// 🔴 亲和记录不能被拖到溢出号。
	if uid, ok := sess.AffinityFor(affKey, "cn:glm-5.2"); !ok || uid != first {
		t.Fatalf("affinity target drifted to overflow account: uid=%q ok=%v want %q", uid, ok, first)
	}
	// 但 conv-2 的会话绑定必须落在实际服务的号（否则它下一轮又从亲和出发、又被判 busy）。
	if uid, ok := sess.LookupForModel("conv-2", "cn:glm-5.2"); !ok || uid != second {
		t.Fatalf("overflowed session should bind to its serving account %q, got %q ok=%v", second, uid, ok)
	}
	if m := rec.Snapshot().Cache; m.AffinityNew != 1 || m.AffinityMiss != 1 {
		t.Fatalf("after overflow: new=%d miss=%d want new=1 miss=1", m.AffinityNew, m.AffinityMiss)
	}

	// 压力解除 → 后续新会话回到亲和号（说明缓存能重新热起来）。
	p.Release(first)
	third := send("conv-3", "task three")
	if third != first {
		t.Fatalf("after release should return to affinity account %q, got %q", first, third)
	}
	if m := rec.Snapshot().Cache; m.AffinityHit != 1 {
		t.Fatalf("affinity_hit=%d want 1 after recovery", m.AffinityHit)
	}
}

// TestAffinitySkipsBoundSessions 锁住「会话已有绑定时亲和完全不参与」：
// 强粘性优先级高于弱亲和，且这种情况不该被计进 affinity 指标（否则亲和率被
// 大量无关请求稀释，面板上就分不清两条机制各自的效果）。
func TestAffinitySkipsBoundSessions(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "ua", AccessToken: "ta", ExpiresAt: 9999999999},
		&auth.Auth{UID: "ub", AccessToken: "tb", ExpiresAt: 9999999999},
	)
	rec := reqlog.New(reqlog.Config{})
	h, served := newAffinityTestHandler(t, p, map[string]string{"ta": "ua", "tb": "ub"}, rec)

	send := func(conv, task string) string {
		t.Helper()
		*served = nil
		body := affinityBody(conv, task)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
		return (*served)[len(*served)-1]
	}

	first := send("conv-1", "task one")
	// 同一会话第二轮：应当靠会话粘性命中（sticky hit），亲和记空。
	if got := send("conv-1", "task one, second turn"); got != first {
		t.Fatalf("second turn should hit the same account, got %q want %q", got, first)
	}
	m := rec.Snapshot().Cache
	if m.StickyHit != 1 {
		t.Errorf("sticky_hit=%d want 1", m.StickyHit)
	}
	if m.AffinityHit != 0 {
		t.Errorf("affinity_hit=%d want 0: bound sessions must not be counted as affinity", m.AffinityHit)
	}
	if m.AffinityNew != 1 {
		t.Errorf("affinity_new=%d want 1 (only the first turn established it)", m.AffinityNew)
	}
}
