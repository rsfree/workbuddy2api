// grace.go 新号任务静默期：刚导入的账号前 N 天不做「赚积分」类任务，签到照常。
//
// 为什么需要（2026-10-24 用户口径）：新买的号换设备/换出口登录后立刻跑旅行、
// 活跃上报、夜猫子对话这类行为链路，最容易被上游风控盯上；而签到是「每天必须
// 做、动作最轻」的一项，本身也是回血（积分）与解冻（冷却账号）的唯一路径，
// 停不得。所以门控只覆盖行为类任务，签到 / 连登兑换 / token 保活 / 余额刷新 /
// 任务补领（纯领奖）全部照常。
//
// 唯一的例外是「积分清光」：credits 归零后账号既不能接流量也不能赚钱，此时
// 再静默等于让号干耗，所以放行——让它自己把积分做回来。
//
// 判定只认可信来源（pool.Status.ImportedAtSource == "import"）：mtime 推断值在
// 搬机器/重写文件后会集体漂移成"刚刚导入"，拿它判新号会把整池老号误静默。
package scheduler

import (
	"fmt"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// NewAccountGraceDays 读当前静默期天数（schedMu 保护，支持面板热改）。
func (s *Scheduler) NewAccountGraceDays() int {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.NewAccountGraceDays
}

// TaskMuted 判断该账号本刻是否应跳过「赚积分」类任务（旅行 / 活跃上报 /
// 夜猫子 / 成长队列）。返回 (true, 原因) 表示跳过。
//
// 调用方（含面板成长队列）统一走这里，避免判定口径分叉：新号静默期的定义
// 只有这一处。
func (s *Scheduler) TaskMuted(st pool.Status) (bool, string) {
	return taskMutedAt(st, time.Now(), s.NewAccountGraceDays())
}

// taskMutedAt 纯判定（可测）：graceDays<=0 关闭；只认可信来源；积分清光放行。
func taskMutedAt(st pool.Status, now time.Time, graceDays int) (bool, string) {
	if graceDays <= 0 {
		return false, ""
	}
	// 来源不可信（mtime 推断 / 未知）一律不门控：宁可让老号继续跑任务，
	// 也不要因为一次文件搬运把整池静默三天。
	if st.ImportedAtSource != "import" || st.ImportedAt.IsZero() {
		return false, ""
	}
	until := st.ImportedAt.Add(time.Duration(graceDays) * 24 * time.Hour)
	if !now.Before(until) {
		return false, ""
	}
	// 积分清光（credits=0 且总额已知）→ 放行：静默期的目的是"别在风控窗口里
	// 折腾"，不是"让没钱的号闲着"。
	if st.Credits <= 0 && st.CreditsTotal > 0 {
		return false, ""
	}
	day := int(now.Sub(st.ImportedAt).Hours()/24) + 1
	if day < 1 {
		day = 1
	}
	return true, fmt.Sprintf("新号静默期 第%d/%d天（%s 到期，credits=%d）",
		day, graceDays, until.Format("01-02 15:04"), st.Credits)
}

// NoteTaskGraceSkip 每个账号每个自然日只打一行跳过日志：静默期内每小时都可能
// 命中，逐次打会把日志刷成噪音（运维真正要看的是"这号什么时候转正"）。
func (s *Scheduler) NoteTaskGraceSkip(st pool.Status, task, why string) {
	day := time.Now().Format("2006-01-02")
	s.mu.Lock()
	if s.graceLogged == nil {
		s.graceLogged = make(map[string]string)
	}
	key := st.UID + "|" + task
	seen := s.graceLogged[key]
	s.graceLogged[key] = day
	s.mu.Unlock()
	if seen == day {
		return
	}
	log.Printf("%s %s: skip task (%s)", task, logfmt.Label(st.UID, st.Nickname), why)
}

// graceSkip 供各任务循环复用的一站式判断：命中静默期则打（每日一次的）日志并
// 返回 true。task 只用于日志前缀（travel / activity / blackcat）。
func (s *Scheduler) graceSkip(st pool.Status, task string) bool {
	muted, why := s.TaskMuted(st)
	if !muted {
		return false
	}
	s.NoteTaskGraceSkip(st, task, why)
	return true
}
