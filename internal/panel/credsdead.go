// credsdead.go 任务路径的「凭证已失效」回写。
//
// 背景（2026-10-30 线上）：任务队列里成片的 `upstream session_dead (http 401):
// 凭证已失效（401 invalid_token）…`——账号的 session 已被上游终止，refresh 也换
// 不回来。但面板的任务链路（队列 / 单任务 / 一键完成 / 任务列表）此前**完全不碰
// 账号池状态**：失败只写进队列条目的 message，号仍然以「可用」姿态留在池子里，
// 下一轮照样被选中、照样白打一轮 401。运维要找出「哪些号要重新登录」只能翻日志。
//
// 这里补上那根线：任务路径拿到 *upstream.Error 且 upstream.IsCredsDead 为真时，
// 回写 pool.MarkCredsDead（打标签 + 禁用），面板随即在账号行打「凭证失效」红标，
// 并提供 prune_session_dead 一键剔除。
//
// 为什么只认 invalid_token：12153 可能被临时性触发，任务路径一次失败就判死会误杀
// 健康号（pool.NoteSessionDead 的阈值就是为这个存在的）。invalid_token 是上游对
// 死会话的固定答复，一次即终态。
package panel

import (
	"errors"
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// markCredsDeadFromErr 把一次上游错误里的「凭证已失效」证据落进账号池。
// 非 invalid_token / 非 *upstream.Error / 无账号池时是空操作（err 为 nil 也安全）。
// 返回是否真的打了标签（供调用方决定要不要改队列文案，目前不需要）。
func (p *Panel) markCredsDeadFromErr(uid string, err error) bool {
	if p.cfg.Pool == nil || uid == "" || !upstream.IsCredsDead(err) {
		return false
	}
	var ue *upstream.Error
	msg := ""
	if errors.As(err, &ue) && ue != nil {
		msg = ue.Msg
	}
	if !p.cfg.Pool.MarkCredsDead(uid, msg) {
		return false // 账号已不在池里（并发移除）：没什么可标记的
	}
	log.Printf("panel: %s 凭证已失效（401 invalid_token）— 已打标签并禁用，面板可一键剔除", logfmt.Label(uid, ""))
	return true
}
