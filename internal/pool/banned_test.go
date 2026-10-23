package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 命中口径：上游 11140 的两种文案（历史 "restricted by upstream" 与新版
// "banned by upstream ... re-login required"）+ 裸业务码都要认。大小写不敏感。
func TestIsUpstreamBannedReasonBanned(t *testing.T) {
	cases := []string{
		"account restricted by upstream (11140 request illegal); appeal in WorkBuddy (submit feedback) to lift the restriction",
		"account banned by upstream (11140 request illegal), re-login required",
		"11140 request illegal",
		"ACCOUNT BANNED BY UPSTREAM",
		"Restricted By Upstream",
	}
	for _, c := range cases {
		if !IsUpstreamBannedReason(c) {
			t.Errorf("应判为上游封禁却被放过: %q", c)
		}
	}
}

// 反向闸门：这些「看着像封号、其实还能救」的 reason 一个都不能命中——误判会把
// 还可用的账号连凭证一起剔走。
func TestIsUpstreamBannedReasonRecoverable(t *testing.T) {
	cases := []string{
		"",
		"manual disable (panel)",
		"14017 trial not activated",
		"account restricted", // 无 11140/upstream 关键词：不猜
		"credit exhausted",
		"11128 channel refused (request illegal)", // 通道校验失败，账号健康
		"6004 model rate limit",
		"consecutive failures",
	}
	for _, c := range cases {
		if IsUpstreamBannedReason(c) {
			t.Errorf("可恢复态被误判为上游封禁: %q", c)
		}
	}
}

// Banned 只取「disabled 且因上游处置」的账号，按 UID 稳定排序；人工禁用与
// 14017 试用未激活必须留在池子里。
func TestBannedSelectsOnlyUpstreamBanned(t *testing.T) {
	p := New("")
	for _, uid := range []string{"u1", "u2", "u3", "u4", "u5"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.Disable("u1", "account restricted by upstream (11140 request illegal); appeal in WorkBuddy")
	p.Disable("u2", "account banned by upstream (11140 request illegal), re-login required")
	p.Disable("u3", "manual disable (panel)")
	p.Disable("u4", "14017 trial not activated")
	// u5 未禁用

	got := p.Banned()
	if len(got) != 2 {
		t.Fatalf("期望命中 2 个，实际 %d 个: %+v", len(got), got)
	}
	if got[0].UID != "u1" || got[1].UID != "u2" {
		t.Errorf("命中清单或排序不对: %s, %s", got[0].UID, got[1].UID)
	}
	for _, st := range got {
		if !st.Disabled || st.DisabledReason == "" {
			t.Errorf("命中项应带禁用原因: %+v", st)
		}
	}
	// 只读：调用一次不应改变池内账号数。
	if n := len(p.List()); n != 5 {
		t.Errorf("Banned 不应改池状态，账号数变成了 %d", n)
	}
}
