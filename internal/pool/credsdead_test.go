package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestMarkCredsDeadTagsAndDisables 401 invalid_token 一次即终态：打标签 + 禁用 + 文案落进
// reason（运维 grep invalid_token 就能把这批号和别的禁用原因分开）。
func TestMarkCredsDeadTagsAndDisables(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if !p.MarkCredsDead("u1", "凭证已失效（401 invalid_token）") {
		t.Fatal("MarkCredsDead 应对存在的账号返回 true")
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if !st.SessionDead {
		t.Error("应打上 SessionDead 标签")
	}
	if st.SessionDeadMsg == "" {
		t.Error("应记录证据文案（面板 hover 展示上游原文）")
	}
	if st.SessionDeadAt.IsZero() {
		t.Error("应记录观测时刻")
	}
	if !st.Disabled {
		t.Error("死凭证不许留在选号池：应同时禁用")
	}
	if st.DisabledReason != credsDeadReason {
		t.Errorf("disabled_reason=%q want %q", st.DisabledReason, credsDeadReason)
	}
	// 禁用后不可选（否则每个请求白打一轮 401）。
	if p.Pick() != nil {
		t.Error("凭证失效号不应被选中")
	}
}

// TestMarkCredsDeadUnknownUID 不存在的 uid 是空操作（并发移除后迟到的观测）。
func TestMarkCredsDeadUnknownUID(t *testing.T) {
	p := New("")
	if p.MarkCredsDead("ghost", "x") {
		t.Error("不存在的 uid 应返回 false")
	}
}

// TestMarkCredsDeadPersists 标签跨重启：上游判死的号重启后仍是死的，面板不能失忆
// （否则「一键剔除」每次重启都要重新攒人）。
func TestMarkCredsDeadPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.MarkCredsDead("u1", "证据")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1", AccessToken: "at1"}) // 同一份凭证（重启扫描）
	st, ok := p2.Status("u1")
	if !ok || !st.SessionDead || !st.Disabled {
		t.Fatalf("标签应持久化: ok=%v st=%+v", ok, st)
	}
	if st.SessionDeadMsg != "证据" {
		t.Errorf("session_dead_msg=%q want 证据", st.SessionDeadMsg)
	}
}

// TestCredsDeadClearedByNewToken 重新登录（凭证变化）是标签唯一的自动解除条件。
// 同一份 token 被重新扫描（启动 SyncToDir / 反复 import）**不能**清标签。
func TestCredsDeadClearedByNewToken(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "old"})
	p.MarkCredsDead("u1", "证据")

	// 同一个 token 再 upsert 一次（重启扫描）：标签保留。
	p.Add(&auth.Auth{UID: "u1", AccessToken: "old"})
	if st, _ := p.Status("u1"); !st.SessionDead {
		t.Fatal("同一份凭证重复进池不应清标签")
	}

	// 换了 token = 真的重新登录过：标签清掉。
	p.Add(&auth.Auth{UID: "u1", AccessToken: "new"})
	st, _ := p.Status("u1")
	if st.SessionDead || st.SessionDeadMsg != "" || !st.SessionDeadAt.IsZero() {
		t.Fatalf("凭证变化后应清标签: %+v", st)
	}
}

// TestCredsDeadClearedByRevive 人工复活（解冻）后不能同时显示「可用」和「凭证失效」。
func TestCredsDeadClearedByRevive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.MarkCredsDead("u1", "证据")

	p.ReviveDisabled("u1")
	st, _ := p.Status("u1")
	if st.SessionDead || st.Disabled {
		t.Fatalf("解冻后应无标签且可用: %+v", st)
	}

	// Revive（面板「解冻」按钮）同样清标签。
	p.MarkCredsDead("u1", "证据")
	p.Revive("u1")
	if st, _ := p.Status("u1"); st.SessionDead {
		t.Fatalf("Revive 后应无标签: %+v", st)
	}
}

// TestCredsDeadClearedBySuccess 任意成功都是「账号还活着」的证据。
func TestCredsDeadClearedBySuccess(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.MarkCredsDead("u1", "证据")
	p.NoteSuccess("u1")
	if st, _ := p.Status("u1"); st.SessionDead {
		t.Fatalf("成功后应清标签: %+v", st)
	}
}

// TestCredsDeadClearedByRefreshSuccess refresh 成功证明凭证还能换出新 token。
func TestCredsDeadClearedByRefreshSuccess(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.MarkCredsDead("u1", "证据")
	p.ClearSessionDead("u1")
	if st, _ := p.Status("u1"); st.SessionDead {
		t.Fatalf("refresh 成功应清标签: %+v", st)
	}
}

// TestSessionDeadFromLegacyReason 本次改动前就被 session_dead 禁掉的老号没有标签字段，
// 但要在面板上与新的失效号同等可见（否则「一键剔除」永远看不到它们）。
func TestSessionDeadFromLegacyReason(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"12153 session dead", true},
		{"refresh session dead", true},
		// credsDeadReason 单独出现（没有显式标签）刻意**不**反推成失效：它永远
		// 与标签一起写入，若把它当依据，「标签被成功/重登清掉但 reason 还在」就会
		// 反推回 true —— 标签永远清不掉（见 IsSessionDeadReason 注释）。
		{credsDeadReason, false},
		{"manual disable (panel)", false},
		{"14017 trial not activated", false},
		{"account restricted by upstream (11140 request illegal)", false},
		{"", false},
	}
	for _, c := range cases {
		p := New("")
		p.Add(&auth.Auth{UID: "u1"})
		p.Disable("u1", c.reason)
		st, _ := p.Status("u1")
		if st.SessionDead != c.want {
			t.Errorf("reason=%q SessionDead=%v want %v", c.reason, st.SessionDead, c.want)
		}
	}
}

// TestIsSessionDeadReasonReviveNotTagged 复活清 reason 后，反推口径不能残留标签。
func TestIsSessionDeadReasonReviveNotTagged(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	if st, _ := p.Status("u1"); !st.SessionDead {
		t.Fatal("precondition: 老号应被反推成 SessionDead")
	}
	p.ReviveDisabled("u1")
	if st, _ := p.Status("u1"); st.SessionDead {
		t.Fatalf("复活后不应再有标签: %+v", st)
	}
}

// TestSessionDeadAccounts 只返回带标签的账号，且排序稳定（面板预演清单直接渲染）。
func TestSessionDeadAccounts(t *testing.T) {
	p := New("")
	for _, uid := range []string{"c", "a", "b"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.MarkCredsDead("c", "x")
	p.MarkCredsDead("a", "x")
	got := p.SessionDeadAccounts()
	if len(got) != 2 || got[0].UID != "a" || got[1].UID != "c" {
		t.Fatalf("命中集合/顺序不对: %+v", got)
	}
	if got[0].SessionDeadAt.IsZero() || got[0].SessionDeadAt.After(time.Now()) {
		t.Errorf("时刻应已写入且不在未来: %v", got[0].SessionDeadAt)
	}
}
