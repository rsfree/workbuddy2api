// credsdead.go 「凭证已失效」标签：识别 / 打标 / 批量剔除的数据面。
//
// 标签语义 = **session 已死，需重新登录**。为什么它要独立于 disabled：上游对已终止
// 的会话回 401 invalid_token（refresh 也换不回来，access token 报 invalid_token、
// refreshToken 报 12153），这类号留在池里是纯损耗——每次被选中都白打一轮上游、
// 任务队列里每个动作都以同一句错误结束（2026-10-30 线上：队列整片
// `upstream session_dead (http 401): 凭证已失效…`）。而它在面板上既不是 banned
// （不是 11140 封禁）也不是冷却（等不来自愈），运维只能一页页翻日志手工找。所以：
//   - 面板按标签展示红标并支持一键剔除（POST /panel/api/accounts/prune_session_dead）；
//   - 标签的清除只有三条路：重新登录（凭证变化）、任意请求成功、人工复活。
//
// 两个**入口**（处置不同，终态相同），别混：
//   - 401 invalid_token（upstream.IsCredsDead）→ MarkCredsDead：确定性终态，**一次即**
//     打标签 + 禁用。这是新观测的主路径。
//   - 12153 offline session 攒够连续阈值（NoteSessionDead）→ 用 reason 反推打标签
//     （见 IsSessionDeadReason）。12153 可能瞬时误报，所以判死要 3 次；但一旦判死，
//     它和 invalid_token 一样"只能重新登录"，标签必须覆盖，否则这批号在账号表里
//     只是「已禁用·原因不明」，永远进不了可剔除清单。
package pool

import (
	"strings"
	"time"
)

// credsDeadReason 凭证失效写进 reason 的文案。刻意保留 invalid_token 字样：
// 运维在状态文件/日志里 grep 这一个词，就能把「需重新登录」的号和其它禁用原因分开。
const credsDeadReason = "credential invalid (401 invalid_token): 上游会话已被终止，需重新登录"

// sessionDeadReasonMarkers 禁用原因里「session 已死」的特征串（大小写不敏感）。
//
// 覆盖两种文案："12153 session dead"（chat 路径的即时禁用 + 连续 3 次阈值）与
// "refresh session dead"（refresh 路径）。作用有二：① 给本次改动**之前**就已禁用
// 的老 state 补标签（那批号没有标签字段）；② 让 12153 判死的号与 invalid_token 判死
// 的号在面板上同等可见（同样的终态、同样的解法，标签不该只覆盖其中一半）。
//
// 🔴 刻意**不收录** credsDeadReason：那条文案永远与显式标签一同写入，若把它也当
// 反推依据，就会出现「标签已被成功/重登清掉、但 reason 还留在原地 → 反推又把它
// 变回失效号」的死循环（实测踩过：标签清不掉，Revive 之外任何清除路径都失效）。
// 反推只服务于"没有标签字段的老 state"这一个场景。
var sessionDeadReasonMarkers = []string{
	"12153 session dead",
	"refresh session dead",
}

// IsSessionDeadReason 判断一条禁用原因是否属于「session 已死、需重新登录」。
// 空串恒为 false——没有证据就不打标签。
func IsSessionDeadReason(reason string) bool {
	if reason == "" {
		return false
	}
	r := strings.ToLower(reason)
	for _, m := range sessionDeadReasonMarkers {
		if strings.Contains(r, m) {
			return true
		}
	}
	return false
}

// MarkCredsDead 记录「凭证已失效」的确定性证据（upstream.IsCredsDead 为真的路径
// 调用），并顺手禁用：死凭证不可能自愈，留在选号池里只会让每个请求白打一轮 401。
//
// 与 NoteSessionDead 的分工：那个是**连续计数**（12153 可能误报，3 次才判死），
// 这个是**一次定终态**（invalid_token 是上游对死会话的固定答复）。
//
// 已 disabled 的号仍会打标签（幂等覆盖时刻/文案）：禁用原因可能是别的（例如先被
// 人工禁用再被发现凭证死了），标签要反映最新证据。uid 不存在返回 false。
func (p *Pool) MarkCredsDead(uid, msg string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.credsDead = true
	e.credsDeadAt = time.Now()
	if msg = strings.TrimSpace(msg); msg != "" {
		e.credsDeadMsg = msg
	}
	if !e.disabled {
		p.disableLocked(e, credsDeadReason)
	}
	p.dirty.Store(true)
	return true
}

// ClearCredsDead 清标签（重新登录 / 人工复活 / 请求成功的路径调用）。
// 不存在的 uid 或本来没有标签都是空操作。
func (p *Pool) ClearCredsDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || !e.credsDead {
		return
	}
	e.credsDead = false
	e.credsDeadAt = time.Time{}
	e.credsDeadMsg = ""
	p.dirty.Store(true)
}

// SessionDeadAccounts 返回当前带「凭证已失效」标签、可整批剔除的账号。
//
// 只读快照（与 List 同一把读锁、同一 UID 排序），不改任何池状态——面板先把清单
// 展示给运维确认，确认后才逐个 Remove。口径 = Status.SessionDead（既有标签，也有
// 从历史禁用原因反推出来的老号），与 overview 的 session_dead 计数同源。
func (p *Pool) SessionDeadAccounts() []Status {
	all := p.List()
	out := make([]Status, 0, len(all))
	for _, st := range all {
		if st.SessionDead {
			out = append(out, st)
		}
	}
	return out
}
