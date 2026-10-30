// tasksweep.go 周期幂等「补领清扫 + 新任务发现」。
//
// 主职责——accept 尚未接受的任务、claim 已达标未领的任务；另有一个窄口径
// 例外：零积分账号会补跑**全部免生成动作**（无模型生成，不花额度），让耗尽账号
// 分钟级回血，不必干等次日 01:00 的全量成长队列。判定按 autoAction.Paid，
// 不再硬编码单个码（这里曾经只放 Sequential_Tasks_7，导致 0 分号手里还有
// 十几个可做的免生成任务躺着不动，见 tasksweep_rescue.go）。来源见 issue #174：
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
	"hash/fnv"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
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
	Rescued    int   `json:"rescued"`    // 零积分账号免生成任务补跑并成功领取的条数
	// Sequential 本轮经分片轮转选中的链推进**候选**数（动作在清扫收尾后由
	// 独立批次执行，成败见日志里的「Sequential 推进 uid=...」行）。
	// 与 Rescued 分开记：一个衡量「死号回血」，一个衡量「链覆盖」，混在一起
	// 就看不出「有些号总会漏」有没有真的被修掉。
	Sequential int `json:"sequential"`
	// RescueBackoff 零积分补给里因「上轮跑过没点亮」退避而跳过的条数
	// （见 tasksweep_rescue.go）。它正常应>0：说明确实有做不动的任务在被压制，
	// 而不是每 15 分钟反复重放同一条事件。
	RescueBackoff int `json:"rescue_backoff"`
	// AcceptBackoff 本轮因「accept 不落账」负缓存而跳过的任务数（见
	// tasksweep_accept_backoff.go）。只做可观测性：它正常应该大于 0、且 Accepted
	// 随之趋近 0——两者同时为 0 才说明上游又开始落账了。
	AcceptBackoff int `json:"accept_backoff"`

	// newCodes 新发现且可自动的任务码（不导出：只在本轮调度内部流转）。
	newCodes []string
}

// RunTaskSweepOnce 对所有账号做一轮幂等补领清扫（账号间并发、账号内串行）。
// 由 scheduler 的 ticker 调用（详见 scheduler.StartTaskSweep）。
func (p *Panel) RunTaskSweepOnce() TaskSweepStats {
	var st TaskSweepStats
	var mu sync.Mutex
	var wg sync.WaitGroup
	// 链推进候选：清扫阶段只收集，wg.Wait 之后交给独立 goroutine 错峰执行。
	var seqJobs []seqJob
	var seqMu sync.Mutex
	// 目录是否尚未建立（升级/新机器首轮）：首轮所有码都「没见过」，若据此触发执行，
	// 会在生产上一把跑掉全账号的待办（每个含真实对话）。所以首轮只登记不触发，
	// 由每日成长队列或手动按钮接手；从第二轮起，真正的新码才即时执行。
	bootstrapping := p.catalog().total() == 0
	// 退避表剪枝：清掉已过期的条目（顺带避免长期运行后旧键累积）。
	p.pruneAcceptStuck()
	p.pruneRescueBlocked()

	// 并发闸：223 个账号原先是一次性铺开，每号 2 次列表请求 → 2 秒内 400+ 个
	// 请求打上游，mp 口径（X-Client-Platform: miniprogram）最先被 429
	// （2026-10-30 14:31 实测：4 个号在补给时直接 soft_rate）。压到 8 路并发，
	// 整轮从 ~2 秒拉长到 ~1 分钟，仍在 15 分钟周期内，换来的是限流不再自伤。
	sem := make(chan struct{}, sweepConcurrency)
	for _, ps := range p.cfg.Pool.List() {
		if ps.Disabled {
			continue // 禁用账号不参与（与任务中心扫描同口径）
		}
		a := p.cfg.Pool.AuthByUID(ps.UID)
		if a == nil || a.IsGlobal() || a.IsEnterprise() {
			continue // 无个人成长体系：零上游请求
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string, credits, creditsTotal int64) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rescueAllFree := credits <= 0 && creditsTotal > 0 && p.shouldRunDiscovered()
			r := p.sweepAccountWithRescue(a, uid, rescueAllFree)
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
			st.Rescued += r.rescued
			if len(r.seqTasks) > 0 {
				seqMu.Lock()
				seqJobs = append(seqJobs, seqJob{a: a, uid: uid, tasks: r.seqTasks})
				seqMu.Unlock()
			}
			st.RescueBackoff += r.rescueBlocked
			st.AcceptBackoff += r.acceptBackoff
			st.newCodes = append(st.newCodes, r.newCodes...)
		}(a, ps.UID, ps.Credits, ps.CreditsTotal)
	}
	wg.Wait()

	// v1 关卡口径（/activity/growth/tasks，无 /v2 前缀）是**另一套任务体系**，
	// 与 v2 列表互不相交，此前完全没被扫到。它单独扫：不进执行队列（无 reward、
	// claim 恒 400），只把「上游上了什么新关卡」记进目录。
	// 门控：本轮 0 个账号真正跑过（全部因账号锁跳过 / 池为空）时**一个请求都不发**。
	// tasksweep_test.go 的「忙账号不产生上游请求」契约就是靠这条守的。
	if st.Accounts > st.Skipped {
		st.Discovered += p.observeLegacyCatalog()
	}

	// 目录落盘：本轮观察到的新码持久化，重启后不再重复报「新任务」。
	if st.Discovered > 0 {
		if err := p.catalog().flush(); err != nil {
			log.Printf("panel: 任务目录落盘失败: %v", err)
		}
	}

	// 链推进放到清扫收尾之后再起：此时列表/accept/claim 阶段已经跑完，动作不再与
	// 它抢同一份上游配额；独立 goroutine + 独立低并发池，不拖 ticker。
	st.Sequential = len(seqJobs)
	p.startSequentialRescue(seqJobs)

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

	if st.Claims() > 0 || st.Errors > 0 || st.Accepted > 0 || st.Discovered > 0 || st.Rescued > 0 || st.Sequential > 0 || st.AcceptBackoff > 0 || st.RescueBackoff > 0 {
		log.Printf("panel: 任务补领清扫：%d 个账号（跳过 %d）· 新任务 %d（可自动 %d）· 接受 %d · accept退避 %d · 领奖 %d · 零积分补给 %d · Sequential 候选 %d（退避 %d）· +%d 分 +%d 能 · 出错 %d",
			st.Accounts, st.Skipped, st.Discovered, st.NewAuto, st.Accepted, st.AcceptBackoff, st.Claimed, st.Rescued, st.Sequential, st.RescueBackoff, st.Credits, st.Energy, st.Errors)
	}
	return st
}

// sweepConcurrency 清扫的账号级并发上限。上游对列表/接任务的限流很敏感
// （mp 口径尤其），一次铺开全部账号只会互相踩；8 路已能在一分钟内扫完 200+ 号。
var sweepConcurrency = 8

// sequentialRescueShard 每轮清扫推进 Sequential 链的账号分片数。
//
// 为什么要分片：有 Sequential 待办的号是千级，而每推进一环要几十秒到几分钟
// （Sequential_Tasks_6 要按真人节奏补 10 条事件 ≈ 8 分钟）。全量铺开会让一轮
// 清扫跑成小时级，把「补领」这条链路一起拖死（清扫是同步 ticker 调用）。
// 按 uid 哈希取 1/shard，分片下标随轮次轮转 ⇒ 每轮约 N/shard 个号
// （1256 号、shard=100 ≈ 12 个，一天 ~96 轮 ≈ 覆盖全量一遍），
// 最坏 shard 轮覆盖全量。
var sequentialRescueShard = 100

// sequentialRescueSlot 当前轮到哪个分片。用时间片（15 分钟一格，与默认清扫
// 周期同尺度）而不是进程内计数器：重启不丢轮转位置，多实例也不会各自从 0 起。
func sequentialRescueSlot() uint32 {
	return uint32(time.Now().Unix() / int64(15*time.Minute/time.Second))
}

// sequentialRescueSelected 该账号本轮是否轮到推进 Sequential 链。
// 纯函数式判定（无状态、无锁），可以放心并发调用。
func sequentialRescueSelected(uid string) bool {
	if sequentialRescueShard <= 1 {
		return true // 测试或小账号池：不分片
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(uid))
	return int(h.Sum32()%uint32(sequentialRescueShard)) ==
		int(sequentialRescueSlot()%uint32(sequentialRescueShard))
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
	rescued                                      int
	seqTasks                                     []upstream.Task // 本轮轮到推进的链任务（最多 1 条）

	rescueBlocked                                int
	acceptBackoff                                int
	newCodes                                     []string // 新发现且具备自动动作的码
	credit, energy                               int64
}

// sweepAccount 单账号一轮清扫。命中账号锁（正在跑任务动作）时整账号跳过：
// 不打断进行中的链路，下一轮自然补上。
func (p *Panel) sweepAccount(a *auth.Auth, uid string) sweepResult {
	return p.sweepAccountWithRescue(a, uid, false)
}

// sweepAccountWithRescue 在普通补领之外执行补给动作，两个口径：
//   - rescueAllFree（零积分号）：全部免生成动作，是死号回血的唯一通路；
//   - 其余号：本轮分片若轮到该 uid，**只推 Sequential 链一环**。这是「有些号
//     总会漏」的正解——链一天只解锁一环，靠每小时一次的成长队列（全量串行，
//     千级账号跑不完一轮）根本轮不到有余额的号；清扫是全量覆盖的唯一通路，
//     而链是免生成动作，放这里不花额度。
// 付费/高风险行为链（专家对话、夜间对话）仍然只走队列，不由清扫触发。
func (p *Panel) sweepAccountWithRescue(a *auth.Auth, uid string, rescueAllFree bool) (res sweepResult) {
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
		// accept 负缓存：这个 (账号, 任务码) 上一轮接受了但回读没翻，且仍在退避期内
		// → 本轮跳过。**不消耗轮次、不打上游**，把 4.4 万次无效请求/天压到千级。
		if p.acceptStuckActive(uid, t.TaskCode) {
			res.acceptBackoff++
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
	// 回读结算（仅在本轮真发过 accept 时做）：把「回了 200 但状态没翻」的组合记进
	// 退避表。回读失败不改表（宁可多试一轮，不要误退避 6 小时）。
	// 次数：每次 accept 只多 1 次列表请求；接受数归零后连这一次都省掉。
	if ok+mpOK > 0 {
		attempted := make([]string, 0, len(codes)+len(mpCodes))
		attempted = append(attempted, codes...)
		attempted = append(attempted, mpCodes...)
		p.settleAcceptBackoff(a, uid, attempted)
	}

	// 补给分两段，别合并：
	//   1. 零积分号就地跑全部免生成动作（死号回血的唯一通路，动作短）；
	//   2. 其余号按分片轮转推进 Sequential 链——**只收集候选、不执行**。链的动作
	//      按真人节奏 45s/条、最长 8 分钟，就地跑会占满清扫的 8 路信号量并与
	//      列表扫描抢同一份配额（详见 tasksweep_sequential.go）。
	// 已达标项不重跑，留给下方阶段二统一领奖。
	if rescueAllFree {
		rescued, blocked, credit, energy := p.runRescuePlan(a, uid, tasks, false)
		res.rescued += rescued
		res.claimed += rescued
		res.rescueBlocked += blocked
		res.credit += credit
		res.energy += energy
	} else if p.shouldRunDiscovered() && sequentialRescueSelected(uid) {
		if plan, _ := sequentialRescuePlan(tasks, func(code string) bool {
			return p.rescueBlocked(uid, code)
		}); len(plan) > 0 {
			res.seqTasks = plan
		}
	}

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

// observeLegacyCatalog 扫一次 v1 关卡口径并写入任务目录，返回首次出现的码数。
//
// 为什么每轮只扫一个账号：v1 关卡是**全局配置**，实测 40 个账号下发的是同一批
// 5 个码（first_chat / skill_installed / wechat_linked / expert_summoned /
// template_used）。逐号扫会让 200+ 账号每分钟各多打一次上游，收益为零。
//
// 为什么不进执行队列：这 5 个码没有 reward_credit/reward_energy，web 与 chat
// 两个域的 claim 都返回 400 task not completed（2026-10-28 实测）——它是等级
// 进度，不是可领奖任务。写进目录只为「换关卡了要能看见」。
func (p *Panel) observeLegacyCatalog() int {
	for _, ps := range p.cfg.Pool.List() {
		if ps.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(ps.UID)
		if a == nil || a.IsGlobal() || a.IsEnterprise() {
			continue // 无个人成长体系：零上游请求
		}
		tasks, err := p.cfg.Upstream.ListLegacyTasks(a)
		if err != nil {
			// 单点失败直接收工（下一轮自然重试）：目录是辅助观测，不该重试风暴。
			return 0
		}
		n := 0
		for _, t := range tasks {
			if p.catalog().observeLegacy(t) {
				n++
				log.Printf("panel: 发现 v1 关卡 code=%s 标题=%q 等级=%q 状态=%s → %s",
					t.Code, t.Title, t.LevelName, t.Status, legacyAutoNote)
			}
		}
		return n
	}
	return 0
}
