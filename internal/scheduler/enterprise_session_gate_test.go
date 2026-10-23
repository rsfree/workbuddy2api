package scheduler

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// sessionMode 凭证形态（2026-10-23 起的判定口径）：
//   - personal    个人号（对照组）
//   - switchOnly  JWT 带 token_source=enterprise_switch 但 enterprise_id 为空
//     —— 实测仍正常计分（web 点击 0/1→1/1、对话 0/5→1/5），**不得门控**
//   - bound       带企业租户（enterprise_id 非空）—— 事件被上游静默丢弃，必须门控
type sessionMode string

const (
	modePersonal   sessionMode = "personal"
	modeSwitchOnly sessionMode = "switch_only"
	modeBound      sessionMode = "bound"
)

// sessionTestScheduler 按凭证形态搭调度器（token 只在 payload 上区分，签名任意）。
func sessionTestScheduler(t *testing.T, mode sessionMode) (*Scheduler, *recorderUpstream) {
	t.Helper()
	r := &recorderUpstream{}
	srv := r.server()
	t.Cleanup(srv.Close)

	payload := `{"sub":"u1"}`
	switch mode {
	case modeSwitchOnly:
		payload = `{"sub":"u1","token_source":"enterprise_switch"}`
	case modeBound:
		payload = `{"sub":"u1","token_source":"enterprise_switch","enterprise_id":"e9"}`
	}
	token := "hdr." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
	a := &auth.Auth{UID: "u1", AccessToken: token, RefreshToken: "rt", ExpiresAt: 9999999999}
	p := pool.New("")
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	return s, r
}

// TestEnterpriseSessionGateSkipsBehaviorTasks 行为类任务门控的**边界**：
//
//   - 真正绑定租户的企业会话（enterprise_id 非空）：/v2/report 等事件上游 200
//     但静默丢弃（实测 2026-10-12），执行即零收益白跑——活跃上报 / 连登管家 /
//     猫猫旅行 / 夜猫子必须一个请求都不发；
//   - 只有 token_source=enterprise_switch、未绑定租户的凭证：实测仍计分
//     （2026-10-23 两条主判据都点亮），必须照常执行，不得被误拦——线上 22 个号
//     就是被这条误判全员 422 的。
//
// 签到对两种形态都不门控（失败是显式报错，不会静默白跑）。
func TestEnterpriseSessionGateSkipsBehaviorTasks(t *testing.T) {
	// 与 TestEnterpriseGateSkipsGrowthTasks 同款：把夜猫子窗口钉在窗口内。
	restoreClock := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 12, 23, 30, 0, 0, time.Local) }
	t.Cleanup(func() { nowForNightWindow = restoreClock })

	cases := []struct {
		name string
		run  func(*Scheduler)
		// endpoint 是该任务在**个人号**上必然发起的上游路径（对照组判据）。
		endpoint string
		// gated=true：绑定租户的企业会话不应发；未绑定租户的切换标记必须照发。
		gated bool
	}{
		{"签到", func(s *Scheduler) { s.RunCheckinNow() }, "/daily-checkin", false},
		{"活跃上报", func(s *Scheduler) { s.RunActivityNow() }, "/v2/report", true},
		{"猫猫旅行", func(s *Scheduler) { s.RunTravelNow() }, "/activity/growth/buddy/info", true},
		{"夜猫子", func(s *Scheduler) { s.RunBlackcatNow() }, "/v2/activity/growth/tasks", true},
		{"连登管家", func(s *Scheduler) { s.RunStreakBonusNow() }, "/activity/growth/streak", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 对照组：个人号必然发起。
			sPer, rPer := sessionTestScheduler(t, modePersonal)
			c.run(sPer)
			if !rPer.has(c.endpoint) {
				t.Fatalf("个人号对照应发起 %s，实际请求=%v", c.endpoint, rPer.snapshot())
			}

			// 未绑定租户的切换标记：必须与个人号一致（不得误拦）。
			sOnly, rOnly := sessionTestScheduler(t, modeSwitchOnly)
			c.run(sOnly)
			if !rOnly.has(c.endpoint) {
				t.Errorf("未绑定租户的 enterprise_switch 不应被门控（实测仍计分），缺少 %s，实际请求=%v",
					c.endpoint, rOnly.snapshot())
			}

			if !c.gated {
				return
			}
			// 绑定租户：必须一个请求都不发。
			sBound, rBound := sessionTestScheduler(t, modeBound)
			c.run(sBound)
			if rBound.has(c.endpoint) {
				t.Errorf("绑定租户的企业会话不应发起 %s，实际请求=%v", c.endpoint, rBound.snapshot())
			}
		})
	}
}

// TestEnterpriseSessionKeepsKeepalive 保活与会话形态无关：token 会过期，
// 两种企业形态都必须照常刷新（否则账号直接躺平）。
func TestEnterpriseSessionKeepsKeepalive(t *testing.T) {
	for _, mode := range []sessionMode{modeSwitchOnly, modeBound} {
		s, r := sessionTestScheduler(t, mode)
		s.RunKeepaliveNow()
		if !r.has("/token/refresh") {
			t.Errorf("%s 必须照常保活，实际请求=%v", mode, r.snapshot())
		}
	}
}
