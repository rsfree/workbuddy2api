// tasksweep_sequential.go 清扫侧的 Sequential 链推进（与列表扫描错峰执行）。
//
// 为什么不能就在 sweepAccountWithRescue 里顺手做掉（v1.13.61 的写法，已废）：
// 那条路径跑在清扫的 8 路信号量**内部**，而链的单个动作是「按真人节奏」的
// ——mpChatEventGap 45s/条，Sequential_Tasks_3 要 5 条（≈4 分钟）、Tasks_6 要
// 10 条（≈8 分钟）。8 个槽被动作占满后整轮列表扫描直接停摆；更糟的是动作与
// 列表抢**同一份**上游配额，2026-10-31 00:49 那轮实测：被推的 12 个号里 10+
// 个立刻 soft_rate 429，整轮清扫从 1 分钟拖到 10 分钟以上还没收尾。
//
// 所以拆成两段：
//   1. 清扫阶段只**收集候选**（谁本轮轮到、要推哪一环），不执行；
//   2. 清扫跑完后由本文件在独立 goroutine + 独立低并发池里执行，不占清扫槽、
//      不与列表扫描同一瞬间抢配额。
//
// 与清扫的另外两处约束保持同口径：账号锁（tryLockAccount）避免与队列/手动入口
// 撞车；失败走 rescueStuck 退避，429 这类瞬时错误用短退避（见 tasksweep_rescue.go）。
package panel

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// seqJob 一个待推进的账号：tasks 是清扫阶段拿到的任务快照（本轮 accept 之后）。
type seqJob struct {
	a     *auth.Auth
	uid   string
	tasks []upstream.Task
}

// sequentialRescueConcurrency 链推进的独立并发。取 3 而不跟清扫的 8：
// 单号动作最长 8 分钟，3 路下一批 12 个号约 8-10 分钟收尾；再高会把 mp 口径
// （本族全靠小程序头）单独打到限流。
var sequentialRescueConcurrency = 3

// sequentialRescueBusy 上一批还在跑时不再叠加新批。
//
// 为什么要挡：清扫每 15 分钟一轮，而一批动作可能要 8-10 分钟（极端情况更久）。
// 不挡的话每轮都起一个 goroutine，多个批次同时抢同一份配额，等于自己造 429；
// 而且候选会成倍堆积。顺延不丢——下一轮仍会重新扫到同一批待办。
var sequentialRescueBusy atomic.Bool

// startSequentialRescue 在后台推进一批候选（不阻塞清扫 ticker）。
//
// 节奏是**每小时一批**，不是每轮清扫一批：清扫 15 分钟一轮（补领需要这个频率），
// 若跟着排，同一小时会重复推同一片账号，把 mp 配额白白烧在同一批人身上。
// 判定顺序不能换——先看上一批是否还在跑（在跑就不占小时槽，让本小时后面几轮
// 清扫再试），再占小时槽，最后才抢 busy：
//  1. busy 检查在前且**不占槽**：批次顺延时本小时仍有补救机会；
//  2. 抢槽成功才算"本小时已排"，避免空转把槽占掉。
func (p *Panel) startSequentialRescue(jobs []seqJob) {
	if len(jobs) == 0 {
		return
	}
	if sequentialRescueBusy.Load() {
		log.Printf("panel: Sequential 推进：上一批未跑完，%d 个候选留在本小时后续清扫重试", len(jobs))
		return
	}
	hour := time.Now().Unix() / int64(time.Hour/time.Second)
	if !sequentialRescueDue(hour) {
		return // 本小时已经排过一批（清扫每 15 分钟一轮，这里把它压成 1 小时）
	}
	if !sequentialRescueBusy.CompareAndSwap(false, true) {
		log.Printf("panel: Sequential 推进：上一批未跑完，%d 个候选留在本小时后续清扫重试", len(jobs))
		return
	}
	go func() {
		defer sequentialRescueBusy.Store(false)
		sem := make(chan struct{}, sequentialRescueConcurrency)
		var wg sync.WaitGroup
		for _, j := range jobs {
			wg.Add(1)
			go func(j seqJob) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				// 清扫阶段已释放账号锁；这里重新拿。拿不到说明该号正在被
				// 队列/手动入口/其它批次使用，直接跳过（下一轮自然再轮到）。
				if !p.tryLockAccount(j.uid) {
					return
				}
				defer p.unlockAccount(j.uid)
				rescued, _, credit, energy := p.runRescuePlan(j.a, j.uid, j.tasks, true)
				if rescued > 0 {
					log.Printf("panel: Sequential 推进批次：uid=%s +%d 分 +%d 能", j.uid, credit, energy)
				}
			}(j)
		}
		wg.Wait()
	}()
}
