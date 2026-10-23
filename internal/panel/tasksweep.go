// tasksweep.go 周期幂等「补领清扫 + 新任务发现」。
//
// 主职责——只 accept 尚未接受的任务、只 claim 已达标未领的任务，**不跑任何
// 行为链路**（不真实对话、不上报事件）。来源见 issue #174：
//
//   - 队列只收「有自动动作」的任务（精确表或族模式），其余任务即使达标也永远
//     扫不进队列、永远不领奖；
//   - 队列路径要先重跑一遍行为动作（可能失败/前置条件已变），且一天只跑一次；
//   - 「一键完成」对已达标任务直接跳过不领。
//
// 顺带职责（新增）——**发现新任务**：每轮把任务列表喂进任务目录（taskcatalog.go），
// 首次出现的任务码落日志/打标/持久化；若该新任务已有自动动作（如 expert_use_*
// 「体验指定专家」），本轮结束后立刻排一轮成长队列，把「发现 → 接受 → 完成」
// 收敛成分钟级闭环，不必等每日固定时点。
//
// 上述三条链路都不负责「把已达标的奖励领掉」，于是积分卡在 2000/2100/2300 上不去 4000。
// 本文件补的就是这条缺失链路：分钟级周期、幂等、可开关。
//
// 设计要点（与 issue 一致）：
//   - 判据只用 progress 口径 Task.Claimable（!claimed && target>0 && current>=target），
//     **不用 accept_status == "completed"**：它在回读时会短暂转 completed 后整体回滚，
//     据此领奖会误领。
//   - claim 幂等（重复领返回 already_claimed → 0 分 0 能），宁可多试一轮也不漏领。
//   - accept 幂等且无害（报名不产生进度）；整批失败时逐条重试，把「个别新码上游
//     不认」的影响隔离在单条上，不拖死同批合法任务。
//   - 与单任务 / 一键完成 / 成长队列共用 per-account 锁：正在跑动作的账号本轮跳过，
//     不打断正在进行的动作链路。
//   - global（无 CN 成长体系）与企业版（上游 403 growth system is only available
//     for personal users）账号都不发起任何上游请求。
package panel

import (
	"log"
	"strings"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TaskSweepStats 一轮补领清扫的汇总（供日志与潜在的未来 HTTP 回执）。
type TaskSweepStats struct {
	Accounts   int   `json:"accounts"`   // 参与清扫的账号数
	Skipped    int   `json:"skipped"`    // 因正在执行任务动作而跳过的账号数
	Accepted   int   `json:"accepted"`   // 本轮接受成功的任务数
	Claimed    int   `json:"claimed"`    // 本轮领奖成功的任务数
	Credits    int64 `json:"credits"`    // 本轮到账积分
	Energy     int64 `json:"energy"`     // 本轮到账能量
	Errors     int   `json:"errors"`     // 本轮出错（不中断）的次数
	Discovered int   `json:"discovered"` // 本轮首次发现的新任务数（任务目录 diff）
	NewAuto    int   `json:"new_auto"`   // 新任务中具备自动动作的个数

	// newCodes 新发现且可自动的任务码（不导出：只在本轮调度内部流转）。
	newCodes []string
}

// RunTaskSweepOnce 对所有账号做一轮幂等补领清扫（账号间并发、账号内串行）。
// 由 scheduler 的 ticker 调用（详见 scheduler.StartTaskSweep）。
func (p *Panel) RunTaskSweepOnce() TaskSweepStats {
	var st TaskSweepStats
	var mu sync.Mutex
	var wg sync.WaitGroup
	// 目录是否尚未建立（升级/新机器首轮）：首轮所有码都「没见过」，若据此触发执行，
	// 会在生产上一把跑掉全账号的待办（每个含真实对话）。所以首轮只登记不触发，
	// 由每日成长队列或手动按钮接手；从第二轮起，真正的新码才即时执行。
	bootstrapping := p.catalog().total() == 0

	for _, ps := range p.cfg.Pool.List() {
		if ps.Disabled {
			continue // 禁用账号不参与（与任务中心扫描同口径）
		}
		a := p.cfg.Pool.AuthByUID(ps.UID)
		if a == nil || a.IsGlobal() || a.IsEnterprise() {
			continue // 无个人成长体系：零上游请求
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string) {
			defer wg.Done()
			r := p.sweepAccount(a, uid)
			mu.Lock()
			defer mu.Unlock()
			st.Accounts += r.accounts
			st.Skipped += r.skipped
			st.Accepted += r.accepted
			st.Claimed += r.claimed
			st.Credits += r.credit
			st.Energy += r.energy
			st.Errors += r.errors
			st.Discovered += r.discovered
			st.NewAuto += r.newAuto
			st.newCodes = append(st.newCodes, r.newCodes...)
		}(a, ps.UID)
	}
	wg.Wait()

	// 目录落盘：本轮观察到的新码持久化，重启后不再重复报「新任务」。
	if st.Discovered > 0 {
		if err := p.catalog().flush(); err != nil {
			log.Printf("panel: 任务目录落盘失败: %v", err)
		}
	}

	codes := dedupeCodes(st.newCodes)
	switch {
	case len(codes) == 0:
		// 无新码：常态。
	case bootstrapping:
		log.Printf("panel: 任务目录初始化：登记 %d 个任务码（首轮不触发自动执行，避免升级后全量重跑；%d 个可自动的码等每日队列或手动执行）",
			st.Discovered, len(codes))
	case !p.shouldRunDiscovered():
		log.Printf("panel: 新任务可自动 %d 项，但成长排程已关闭（schedule.growth_enabled=false）→ 只接受不执行", len(codes))
	default:
		log.Printf("panel: 新任务可自动 %d 项 → 只对这些码执行（不碰其余待办）：%s", len(codes), strings.Join(codes, ", "))
		go p.runDiscoveredTasks(codes)
	}

	if st.Claims() > 0 || st.Errors > 0 || st.Accepted > 0 || st.Discovered > 0 {
		log.Printf("panel: 任务补领清扫：%d 个账号（跳过 %d）· 新任务 %d（可自动 %d）· 接受 %d · 领奖 %d · +%d 分 +%d 能 · 出错 %d",
			st.Accounts, st.Skipped, st.Discovered, st.NewAuto, st.Accepted, st.Claimed, st.Credits, st.Energy, st.Errors)
	}
	return st
}

// shouldRunDiscovered 新发现的可自动任务是否应立即执行。
// 两个否决条件：成长排程被显式关闭（尊重 schedule.growth_enabled），
// 或目录尚未建立（首轮全量，交给每日队列/手动按钮）。
func (p *Panel) shouldRunDiscovered() bool {
	if p.cfg.Scheduler == nil {
		return true // 无调度器的裸用场景（测试/CLI）：由调用方自己决定
	}
	return p.cfg.Scheduler.GrowthEnabled()
}

// dedupeCodes 去重（多账号会重复报同一个新码）。
func dedupeCodes(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, c := range in {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// Claims 本轮实际到账奖励的任务数（与 Claimed 一致，语义化别名）。
func (s TaskSweepStats) Claims() int { return s.Claimed }

// sweepResult 单账号清扫结果（RunTaskSweepOnce 汇总用，非导出）。
type sweepResult struct {
	accounts, skipped, accepted, claimed, errors int
	discovered, newAuto                          int
	newCodes                                     []string // 新发现且具备自动动作的码
	credit, energy                               int64
}

// sweepAccount 单账号一轮清扫。命中账号锁（正在跑任务动作）时整账号跳过：
// 不打断进行中的链路，下一轮自然补上。
func (p *Panel) sweepAccount(a *auth.Auth, uid string) (res sweepResult) {
	res.accounts = 1
	if !p.tryLockAccount(uid) {
		res.skipped = 1
		return res
	}
	defer p.unlockAccount(uid)

	// 默认 + 小程序口径合并（mp 是默认超集，按 task_code 去重；mp 失败静默）。
	tasks, mpOnly, err := p.listTasksBoth(a)
	if err != nil {
		res.errors++
		return res
	}
	// 目录观察：首次出现的码即新任务。带自动动作的会在本轮结束后触发即时执行，
	// 不带动作的至少留下「什么时候出现过、值多少分」的记录（前端打「新」标）。
	fresh := p.observeTasks(tasks)
	if len(fresh) > 0 {
		logNewTasks(uid, fresh)
		res.discovered = len(fresh)
		for _, t := range fresh {
			if actionForTask(t) != nil {
				res.newAuto++
				res.newCodes = append(res.newCodes, t.TaskCode)
			}
		}
	}

	// 阶段一：接受未接受的任务。默认口径与 mp 口径必须**分开提交**（端标记不同，
	// 混提交会让 mp 任务报 task not found）；整批失败逐条隔离，避免个别新码
	// 拖死其余合法任务。
	var codes, mpCodes []string
	for _, t := range tasks {
		if t.Claimed || t.Locked {
			continue
		}
		if t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		if isMPTaskCode(t.TaskCode) || mpOnly[t.TaskCode] {
			mpCodes = append(mpCodes, t.TaskCode)
		} else {
			codes = append(codes, t.TaskCode)
		}
	}
	ok, failed := p.acceptCodesWithFallback(a, codes, false)
	mpOK, mpFailed := p.acceptCodesWithFallback(a, mpCodes, true)
	res.accepted += ok + mpOK
	res.errors += failed + mpFailed

	// 阶段二：领取已达标的奖励。claim 幂等（重复领 already_claimed → 0 分 0 能），
	// 单项出错只记录、不中断后续任务。
	for _, t := range tasks {
		if !t.Claimable {
			continue
		}
		// mp 口径任务必须走 mp 变体领奖端点（缺 mp 头上游返回 task not found）；
		// 其余走 Web 域端点——与 taskcenter / autotask 的领奖口径一致。
		var c, e int64
		var cErr error
		if isMPTaskCode(t.TaskCode) || mpOnly[t.TaskCode] {
			c, e, cErr = p.cfg.Upstream.ClaimRewardMP(a, t.TaskCode)
		} else {
			c, e, cErr = p.cfg.Upstream.ClaimReward(a, t.TaskCode)
		}
		if cErr != nil {
			res.errors++
			continue
		}
		res.credit += c
		res.energy += e
		res.claimed++
	}
	return res
}
