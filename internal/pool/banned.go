package pool

import "strings"

// upstreamBannedMarkers 上游「账号已被处置」的禁用原因特征串（大小写不敏感）。
//
// 语义边界刻意收窄：只认「上游已把账号判死、本地无论怎么操作都救不回来」的禁用。
// 不收录以下几种「看起来像封号、其实还能救」的态：
//   - manual disable (panel)：人工禁用，解冻即可；
//   - 14017 trial not activated：试用未激活，补激活/换号后可恢复；
//   - 积分冷却 / 软冷却 / 熔断：走冷却域，根本不写 disabled。
//
// 漏判的代价只是「少剔几个」（面板仍可单号移除，运维可兜底）；误判的代价是把还能
// 用的账号连凭证一起剔走。所以这里宁可窄，不做模糊匹配。
//
// 11140 是上游业务码（`{"code":11140,"msg":"request illegal"}`，displayMsg 为
// 「账号存在异常行为」）。文案在不同版本里出现过 "restricted by upstream" 与
// "banned by upstream, re-login required" 两种，故按 code + 两种文案同时兜。
// 注意 11128 的 msg 同为 "request illegal" 但语义是通道校验失败（账号本身健康），
// 它不会落成禁用原因，这里也不会命中。
var upstreamBannedMarkers = []string{
	"11140",
	"banned by upstream",
	"restricted by upstream",
}

// IsUpstreamBannedReason 判断一条禁用原因是否属于「上游封禁」。
// 空串（老 state 无 reason）恒为 false——没有证据就不动它。
func IsUpstreamBannedReason(reason string) bool {
	if reason == "" {
		return false
	}
	r := strings.ToLower(reason)
	for _, m := range upstreamBannedMarkers {
		if strings.Contains(r, m) {
			return true
		}
	}
	return false
}

// Banned 返回当前因上游处置被硬禁用、可整批剔除的账号。
//
// 只读快照（与 List 同一把读锁、同一 UID 排序），不改任何池状态——面板先把这份清单
// 展示给运维确认，确认后才逐个 Remove。
//
// 与 Disabled 计数的区别：CountsDetailedWithPaused 的 disabled 含人工禁用与
// 14017 等可恢复态，本函数只取其中真正「判死」的那部分。
func (p *Pool) Banned() []Status {
	all := p.List()
	out := make([]Status, 0, len(all))
	for _, st := range all {
		if st.Disabled && IsUpstreamBannedReason(st.DisabledReason) {
			out = append(out, st)
		}
	}
	return out
}
