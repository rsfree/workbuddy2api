package panel

import (
	"log"
	"time"
)

// riskBlocked 上游风控熔断闸门：所有"会自动向上游发动作"的入口开跑前都必须过这一关。
//
// 为什么需要它：2026-11-01 00:04 一分钟内 139 个账号同时吃到 11140
// （"Your account has been restricted due to unusual activity"），
// 峰值一秒 34 个、且此前 3 分钟零请求 —— 上游是**名单式批量处置**，
// 名单早已存在，我们每跑一轮就是把还没被点到的号一个个撞出来。
// 这种情况下「单号退避」毫无意义，唯一有效的是整体停下。
//
// 判定口径见 pool.RiskGuard：5 分钟窗口内 ≥3 次 11140 → 熔断 30 分钟。
// 与 pool.breaker_threshold=3 同口径，不引入第二套数字。
//
// scene 是日志里的场景名（"成长队列"/"任务清扫"/"新任务即时执行"），
// 用于区分是哪条通路被拦下 —— 三处共用同一闸门但不该在日志里混成一句。
func (p *Panel) riskBlocked(scene string) bool {
	if p == nil || p.cfg.Pool == nil {
		return false
	}
	now := time.Now()
	if !p.cfg.Pool.RiskTripped(now) {
		return false
	}
	st := p.cfg.Pool.RiskState(now)
	log.Printf("panel: ⛔ 上游风控熔断中，跳过%s（%ds 窗口内 %d 次 11140 ≥ 阈值 %d，熔断至 %s，累计 %d 次/触发 %d 次）",
		scene, st.WindowSec, st.WindowHits, st.Threshold,
		st.Until.Format("2006-01-02 15:04:05"), st.Bans, st.Trips)
	return true
}
