// tasksweep_accept_backoff.go accept 负缓存。
//
// 为什么需要：上游对「报名/接受任务」的接口回 200 OK 但不回写状态。线上实测
// （2026-10-24，68 账号 / 4.5 小时）：
//   - 每轮清扫「接受 463 · 领奖 0 · 出错 0」，**连续 18 轮数字一字不差**；
//   - 单账号回读：21 个任务里 20 个 accept_status 仍是 not_accepted，仅 1 个 accepted；
//   - 换算下来约 4.4 万次无效 accept/天，全部打在上游。
//
// 这些请求拿不到任何收益，却在持续消耗上游配额、抬高风控暴露面。于是加一层
// 按 (账号, 任务码) 的**负缓存**：accept 之后回读一次任务列表，凡是「发了 accept、
// 回读仍未 accepted」的组合记进退避表，TTL 内不再重试。
//
// 为什么退避而不是直接永久拉黑：任务码的可用性会随活动/周期变化，今天不落账的码
// 明天可能就正常了。TTL 到点后自动重试一次，用最小代价跟上变化。
//
// 为什么回读一次而不是靠下一轮观察：靠下一轮观察要先多打一整轮 accept 才能判定，
// 而回读只多 1 次列表请求（接受数为 0 时连这一次也省掉）——用 1 次列表换掉整批
// accept 是明显划算的交易。
package panel

import (
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// acceptStuckTTL 负缓存时长。6h 的含义：一天最多重试 4 次，相对每 15 分钟一次
// 把无效 accept 压掉约 96%。测试可临时改写。
var acceptStuckTTL = 6 * time.Hour

// acceptStuckKey 退避表键。用 NUL 分隔避免 uid 与任务码拼接产生歧义
// （两者都是上游下发的不透明字符串，不保证不含分隔符外的其它字符）。
func acceptStuckKey(uid, code string) string {
	return uid + "\x00" + code
}

// acceptStuckActive 报告 (uid, code) 是否仍在退避期内。已过期的条目顺手清掉，
// 让表不会因为 TTL 到期而无限膨胀（无需额外的定时 GC）。
func (p *Panel) acceptStuckActive(uid, code string) bool {
	key := acceptStuckKey(uid, code)
	now := time.Now()
	p.acceptStuckMu.Lock()
	defer p.acceptStuckMu.Unlock()
	until, ok := p.acceptStuck[key]
	if !ok {
		return false
	}
	if now.After(until) {
		delete(p.acceptStuck, key)
		return false
	}
	return true
}

// markAcceptStuck 把一批任务码记进退避表（覆盖写，续期到 now+TTL）。
func (p *Panel) markAcceptStuck(uid string, codes []string) {
	if len(codes) == 0 {
		return
	}
	until := time.Now().Add(acceptStuckTTL)
	p.acceptStuckMu.Lock()
	defer p.acceptStuckMu.Unlock()
	if p.acceptStuck == nil {
		p.acceptStuck = make(map[string]time.Time, 64)
	}
	for _, c := range codes {
		p.acceptStuck[acceptStuckKey(uid, c)] = until
	}
}

// clearAcceptStuck 清掉一批任务码的退避（accept 这次真的落账了）。
func (p *Panel) clearAcceptStuck(uid string, codes []string) {
	if len(codes) == 0 {
		return
	}
	p.acceptStuckMu.Lock()
	defer p.acceptStuckMu.Unlock()
	for _, c := range codes {
		delete(p.acceptStuck, acceptStuckKey(uid, c))
	}
}

// pruneAcceptStuck 清掉全表过期条目，每轮清扫开头调一次。
// 条目上限天然有界（账号数 × 任务码数，量级 10^3），但仍按时间剪枝以免长期运行时
// 反复重建的旧键累积。
func (p *Panel) pruneAcceptStuck() {
	now := time.Now()
	p.acceptStuckMu.Lock()
	defer p.acceptStuckMu.Unlock()
	for k, until := range p.acceptStuck {
		if now.After(until) {
			delete(p.acceptStuck, k)
		}
	}
}

// settleAcceptBackoff accept 之后的回读结算：把「发了 accept 但状态没翻」的组合
// 记进退避，把「这次真的翻成 accepted/completed」的解除退避。
//
// 回读失败时什么都不做（不记账）：宁可下一轮多打一次 accept，也不要因为一次瞬时
// 失败把一整批任务误判成「不落账」而退避 6 小时。
func (p *Panel) settleAcceptBackoff(a *auth.Auth, uid string, attempted []string) {
	if len(attempted) == 0 {
		return
	}
	fresh, _, err := p.listTasksBoth(a)
	if err != nil {
		return
	}
	status := make(map[string]string, len(fresh))
	for _, t := range fresh {
		status[t.TaskCode] = t.AcceptStatus
	}
	var stuck, settled []string
	for _, c := range attempted {
		st, seen := status[c]
		if seen && (st == "accepted" || st == "completed") {
			settled = append(settled, c)
			continue
		}
		// 未见（任务码消失）或仍是 not_accepted：都算没落账。
		stuck = append(stuck, c)
	}
	p.markAcceptStuck(uid, stuck)
	p.clearAcceptStuck(uid, settled)
}
