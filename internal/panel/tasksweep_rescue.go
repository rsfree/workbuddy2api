// tasksweep_rescue.go 零积分账号的「免生成任务」补给。
//
// 背景（2026-10-30 线上实测）：余额为 0 的号（credits=0 且总额已知）发真实对话
// 必然 429 code=14018「额度已用尽」——付费动作跑一遍只是白打一次请求、白占十几秒
// 队列时间，进度一动也不会动（日志证据：小s 542c4878 连续 4 条
// 「召唤/上报失败: 真实对话: chat http 429 14018」）。它们**还能做**的是纯事件上报
// 类任务（/v2/report、/v2/user-asset：对话事件、模板、画布、小程序对话……），
// 这类动作不花额度，完成后照样 +100/+300 分，是死号回血的唯一通路。
//
// 此前这条通路只开了一道缝：补给白名单硬编码为 Sequential_Tasks_7 一个码
// （issue #174 当时的窄口径）。结果就是「已经没积分了还不做任务」——线上 30 个
// 0 分号手里还有 16 个可做的免生成任务（Sequential_Tasks_1..6）躺着不动，
// 每轮清扫的「零积分补给」恒为 0。按码白名单的根因是当时没有「动作花不花额度」
// 这个属性，只能硬点名一个最安全的码。
//
// 现在按动作属性判定（autoAction.Paid）而不是码白名单：所有免生成动作都进补给
// 计划，付费动作显式排除。两条硬约束不能去掉：
//   - 单轮上限 zeroCreditRescueMax：Sequential_Tasks_3 要按真人节奏补 5 条事件
//     （mpChatEventGap 45s/条 ≈ 4 分钟），无上限会把 per-account 锁占成小时级，
//     拖住成长队列与手动入口；
//   - 失败退避 rescueRetryTTL：任务点亮是**异步**的，回读没动不代表下轮重放就能动；
//     每 15 分钟重放同一条只会让上游不断地看到重复事件（反作弊面）。
package panel

import (
	"log"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// zeroCreditRescueMax 单账号单轮最多补跑几个免生成任务。
// 3 的依据：一轮清扫 15 分钟，最贵的一条（Sequential_Tasks_3，补 5 条 45s 事件）
// 约 4 分钟，3 条最坏 ≈ 8 分钟，仍在两轮之间跑得完，也不会把账号锁长期占住。
const zeroCreditRescueMax = 3

// rescueRetryTTL 单条任务补给失败后的退避时长。45 分钟 = 每轮清扫 15 分钟，
// 一天最多重试 ~30 次→降到 ~10 次，且仍能在当日多次尝试（异步计分偶发丢事件
// 时，当天就能补回来）。
var rescueRetryTTL = 45 * time.Minute

// rescueKey 退避表键（与 acceptStuckKey 同构：NUL 分隔，避免拼接歧义）。
func rescueKey(uid, code string) string { return uid + "\x00" + code }

// rescueBlocked 报告 (uid, code) 是否仍在补给退避期内。过期条目顺手清掉。
func (p *Panel) rescueBlocked(uid, code string) bool {
	key := rescueKey(uid, code)
	now := time.Now()
	p.rescueMu.Lock()
	defer p.rescueMu.Unlock()
	until, ok := p.rescueStuck[key]
	if !ok {
		return false
	}
	if now.After(until) {
		delete(p.rescueStuck, key)
		return false
	}
	return true
}

// markRescueBlocked 把一批任务码记进补给退避表（覆盖写，续期到 now+TTL）。
func (p *Panel) markRescueBlocked(uid string, codes []string) {
	if len(codes) == 0 {
		return
	}
	until := time.Now().Add(rescueRetryTTL)
	p.rescueMu.Lock()
	defer p.rescueMu.Unlock()
	if p.rescueStuck == nil {
		p.rescueStuck = make(map[string]time.Time, 64)
	}
	for _, c := range codes {
		p.rescueStuck[rescueKey(uid, c)] = until
	}
}

// clearRescueBlocked 清掉一批任务码的退避（这次真的点亮了）。
func (p *Panel) clearRescueBlocked(uid string, codes []string) {
	if len(codes) == 0 {
		return
	}
	p.rescueMu.Lock()
	defer p.rescueMu.Unlock()
	for _, c := range codes {
		delete(p.rescueStuck, rescueKey(uid, c))
	}
}

// pruneRescueBlocked 清掉全表过期条目，每轮清扫开头调一次（与 pruneAcceptStuck 同处）。
func (p *Panel) pruneRescueBlocked() {
	now := time.Now()
	p.rescueMu.Lock()
	defer p.rescueMu.Unlock()
	for k, until := range p.rescueStuck {
		if now.After(until) {
			delete(p.rescueStuck, k)
		}
	}
}

// zeroCreditRescuePlan 挑出零积分账号本轮该补跑的免生成任务（纯函数，便于单测）。
//
// 过滤口径：
//   - claimed / locked：没有可做的事（locked 是上游串行解锁，跑了也不落账）；
//   - claimable（进度已达标）：不该重跑动作，交给清扫阶段二统一领奖；
//   - 无自动动作：不知道该做什么，等适配；
//   - 付费动作（act.Paid）：余额 0 时必然 14018，跑它纯属浪费；
//   - 退避期内：上轮刚跑过且没点亮。
//
// 排序按 autoActions 下标（Sequential 链 N 依赖 N-1，必须按序解锁）；
// 截断到 limit，返回计划与「因退避被跳过」的条数。
func zeroCreditRescuePlan(tasks []upstream.Task,
	blocked func(code string) bool, limit int) (plan []upstream.Task, skipped int) {
	return rescuePlan(tasks, blocked, limit, false)
}

// sequentialRescuePlan 非零积分账号的 Sequential 链推进计划。
//
// 与零积分口径的差别只有两点：只收 Sequential 族（其余免生成任务有余额的号
// 会在成长队列里跑到，不必让清扫替它们做）；单轮最多 1 条——链是串行的，上游
// 同一时刻只有一环 unlocked，多收只是把同一环重复上报（反作弊面）。
func sequentialRescuePlan(tasks []upstream.Task,
	blocked func(code string) bool) (plan []upstream.Task, skipped int) {
	return rescuePlan(tasks, blocked, 1, true)
}

// rescuePlan 补给计划通用实现。seqOnly=true 时只收 Sequential 链。
func rescuePlan(tasks []upstream.Task,
	blocked func(code string) bool, limit int, seqOnly bool) (plan []upstream.Task, skipped int) {
	for _, t := range tasks {
		// 进度达标的不重跑动作（Claimable 由解析层算好，这里再按 progress 兜一次：
		// 手搓的 Task / 其它来源的快照不一定带 Claimable）。
		if t.Claimed || t.Locked || t.Claimable || (t.Target > 0 && t.Current >= t.Target) {
			continue
		}
		if seqOnly {
			if _, ok := sequentialTaskIndex(t.TaskCode); !ok {
				continue
			}
		}
		act := actionForTask(t)
		if act == nil || act.Paid {
			continue
		}
		if blocked != nil && blocked(t.TaskCode) {
			skipped++
			continue
		}
		plan = append(plan, t)
	}
	sort.SliceStable(plan, func(i, j int) bool {
		return taskPriorityLess(plan[i], plan[j])
	})
	if limit > 0 && len(plan) > limit {
		plan = plan[:limit]
	}
	return plan, skipped
}

// runZeroCreditRescue 对单账号执行补给计划，返回：
//   - rescued：本轮真正点亮并到账的任务数（回读 claimed 才算，上报 200 不算）；
//   - skipped：因退避被跳过的条数（可观测性：它该在 0 与候选数之间波动）；
//   - credit/energy：本轮补给带来的奖励增量（按任务登记的 reward 计）。
//
// 数据源是调用方刚拉到的任务快照（本轮 accept 之后的那个），避免重复拉列表。
func (p *Panel) runRescuePlan(a *auth.Auth, uid string,
	tasks []upstream.Task, seqOnly bool) (rescued, skipped int, credit, energy int64) {
	blocked := func(code string) bool { return p.rescueBlocked(uid, code) }
	var plan []upstream.Task
	if seqOnly {
		plan, skipped = sequentialRescuePlan(tasks, blocked)
	} else {
		plan, skipped = zeroCreditRescuePlan(tasks, blocked, zeroCreditRescueMax)
	}
	label := "零积分补给"
	if seqOnly {
		label = "Sequential 推进"
	}
	for _, t := range plan {
		msg, err := p.runGrowthQueued(a, t.TaskCode)
		if err != nil {
			p.markRescueBlocked(uid, []string{t.TaskCode})
			log.Printf("panel: %s uid=%s code=%s 失败（退避 %s）: %v",
				label, uid, t.TaskCode, rescueRetryTTL, err)
			continue
		}
		// 只有回读确认 claimed 才算回血成功；上报 200 不等于进度点亮
		// （与 accept 的 200+OK 不落账同源：上游计分是异步且可能整体回滚）。
		after, rerr := p.taskByCode(a, t.TaskCode)
		if rerr != nil || after == nil || !after.Claimed {
			p.markRescueBlocked(uid, []string{t.TaskCode})
			log.Printf("panel: %s uid=%s code=%s: %s（未点亮，退避 %s）",
				label, uid, t.TaskCode, msg, rescueRetryTTL)
			continue
		}
		rescued++
		credit += t.Credit
		energy += t.Energy
		p.clearRescueBlocked(uid, []string{t.TaskCode})
		log.Printf("panel: %s uid=%s code=%s: %s（+%d 分 +%d 能）",
			label, uid, t.TaskCode, msg, t.Credit, t.Energy)
	}
	return rescued, skipped, credit, energy
}
