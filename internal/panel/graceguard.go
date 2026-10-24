// graceguard.go 手动任务入口的新号静默期闸门。
//
// 背景：静默期（scheduler/grace.go）原先只挂在四条自动链路上 —— travel /
// blackcat / startGrowthQueue / runDiscoveredTasks。而「单任务」与「一键完成」
// 走的是另一条 runAutoAll 管线，那里没有这道判定：同一套行为动作（真实对话 +
// 事件上报），批量入口被拦、单号入口放行，等于静默期可以被一个按钮整个绕开。
//
// 口径：默认拦（409 + 原因 + 解除时间），显式 ?force=1 才放行 —— 保留手动越权
// 的自由，但必须自证知情；越过时打一行日志留痕，事后能答"谁在静默期跑过"。
// 判定本身完全复用 scheduler.TaskMuted，不另立第二套口径（含它的两个例外：
// mtime 推断来源不拦、credits 清零放行）。
package panel

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// forceParam 请求是否显式声明越过新号静默期（?force=1|true|yes）。
func forceParam(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get("force"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// manualGraceBlock 手动任务入口的静默期判定（入参是 Status，便于单测）。
// 返回 (true, 原因) 表示调用方应回 409；?force=1 时返回 false 并留一行越过日志。
func (p *Panel) manualGraceBlock(r *http.Request, st pool.Status, task string) (bool, string) {
	// 裸用/测试路径没有调度器：不拦（与自动链路同口径，缺调度器即无静默期概念）。
	if p.cfg.Scheduler == nil {
		return false, ""
	}
	muted, why := p.cfg.Scheduler.TaskMuted(st)
	if !muted {
		return false, ""
	}
	if forceParam(r) {
		log.Printf("panel: 手动任务越过新号静默期 %s task=%s（force=1；%s）",
			logfmt.Label(st.UID, st.Nickname), task, why)
		return false, ""
	}
	// 复用 NoteTaskGraceSkip 的「每账号每日一行」节流：静默期内反复点按钮
	// 不该把日志刷成噪音，运维要看的只是"这号什么时候转正"。
	p.cfg.Scheduler.NoteTaskGraceSkip(st, task, why)
	return true, why
}

// graceBlockedManual 按 uid 取当前状态后判定（供 HTTP handler 直接调用）。
// 账号不存在时返回 false：让后续 accountByUID 的 404 语义继续生效，不在这里抢答。
func (p *Panel) graceBlockedManual(r *http.Request, uid, task string) (bool, string) {
	if p.cfg.Pool == nil {
		return false, ""
	}
	st, ok := p.cfg.Pool.Status(uid)
	if !ok {
		return false, ""
	}
	return p.manualGraceBlock(r, st, task)
}

// writeGraceBlocked 统一的 409 回执：说清「为什么被拦 / 何时解除 / 怎么越过」。
// 只写"被静默期拦截"而不给到期时间，会让人反复重试——原因串里已带 day N/M 与到期时刻。
func writeGraceBlocked(w http.ResponseWriter, why string) {
	writeErr(w, http.StatusConflict, fmt.Sprintf(
		"新号静默期拦截（%s）：手动任务与自动链路同口径，新导入账号默认不做成长行为链路。"+
			"确认要立刻执行请加 ?force=1（会立即消耗真实对话配额，风控风险自负）", why))
}
