package pool

import (
	"sync"
	"time"
)

// 上游风控熔断的默认参数。
//
// 2026-11-01 线上实测：00:04 一分钟内 139 个账号同时 11140，峰值一秒 34 个，
// 而 00:00–00:03 没有任何前置请求 —— 上游是**名单式批量处置**：名单早已生成，
// 我们一跑就整片撞上。这种形态下，「单号失败几次就退避」是没有意义的，
// 唯一有效的止损是：**发现被批量处置的迹象就整体停下来**。
//
// 5 分钟 3 次与 Pool 既有的 breaker_threshold=3 同口径，不引入第二套数字。
const (
	// RiskWindow 滑动窗口长度：统计该窗口内的 11140 次数。
	RiskWindow = 5 * time.Minute
	// RiskThreshold 窗口内达到该次数即熔断。
	RiskThreshold = 3
	// RiskCooldown 熔断持续时间。到期后自动恢复（但不会自动重跑队列，
	// 由下一个排程时点决定），给运维留出人工判断的窗口。
	RiskCooldown = 30 * time.Minute
)

// RiskGuard 账号池级别的风控熔断器。
//
// 它只做一件事：记住"最近有没有被上游成批限制"，并把结论提供给自动化入口。
// 刻意不看单个账号的失败率 —— 单号 429/超时是常态，成批 11140 才是灾害。
type RiskGuard struct {
	mu        sync.Mutex
	hits      []time.Time // 窗口内 11140 时刻（升序）
	window    time.Duration
	threshold int
	cooldown  time.Duration
	until     time.Time // 熔断截止时刻；零值 = 未熔断
	bans      int64     // 累计 11140 次数（不过期，纯观测）
	trips     int64     // 累计熔断次数（纯观测）
}

// NewRiskGuard 用默认参数构造熔断器。
func NewRiskGuard() *RiskGuard {
	return &RiskGuard{window: RiskWindow, threshold: RiskThreshold, cooldown: RiskCooldown}
}

// Note 记录一次 11140。返回 true 表示**本次正好触发**熔断（刚进入冷却期），
// 用于让调用方打一条醒目的日志；已在熔断期内再次命中返回 false（不重复告警）。
func (g *RiskGuard) Note(now time.Time) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bans++
	wasTripped := !g.until.IsZero() && now.Before(g.until)

	cut := now.Add(-g.window)
	i := 0
	for i < len(g.hits) && g.hits[i].Before(cut) {
		i++
	}
	g.hits = append(g.hits[i:], now)

	if len(g.hits) >= g.threshold {
		g.until = now.Add(g.cooldown)
		if !wasTripped {
			g.trips++
			return true
		}
	}
	return false
}

// Tripped 报告 now 时刻是否处于熔断期。
func (g *RiskGuard) Tripped(now time.Time) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.until.IsZero() && now.Before(g.until)
}

// RiskState 熔断状态快照（面板展示 / 排查用）。
type RiskState struct {
	WindowHits int       `json:"window_hits"` // 当前窗口内 11140 次数
	Tripped    bool      `json:"tripped"`     // 是否处于熔断期
	Until      time.Time `json:"until"`       // 熔断截止时刻（未熔断为零值）
	Bans       int64     `json:"bans"`        // 累计 11140 次数
	Trips      int64     `json:"trips"`       // 累计熔断次数
	Threshold  int       `json:"threshold"`
	WindowSec  int       `json:"window_sec"`
	CooldownS  int       `json:"cooldown_sec"`
}

// Snapshot 返回熔断状态快照。
func (g *RiskGuard) Snapshot(now time.Time) RiskState {
	if g == nil {
		return RiskState{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cut := now.Add(-g.window)
	n := 0
	for _, t := range g.hits {
		if !t.Before(cut) {
			n++
		}
	}
	return RiskState{
		WindowHits: n,
		Tripped:    !g.until.IsZero() && now.Before(g.until),
		Until:      g.until,
		Bans:       g.bans,
		Trips:      g.trips,
		Threshold:  g.threshold,
		WindowSec:  int(g.window / time.Second),
		CooldownS:  int(g.cooldown / time.Second),
	}
}

// —— 池级接线（Panel / server 都只持有 *Pool，它是唯一的共享点） ——

// guard 返回池级熔断器，懒初始化（零值 Pool 亦可用）。
func (p *Pool) guard() *RiskGuard {
	if p == nil {
		return nil
	}
	p.riskOnce.Do(func() { p.risk = NewRiskGuard() })
	return p.risk
}

// NoteUpstreamBan 记录一次上游 11140（账号被限制）。返回 true 表示本次触发了熔断，
// 调用方应打一条醒目日志。nil 池安全。
func (p *Pool) NoteUpstreamBan(now time.Time) bool {
	return p.guard().Note(now)
}

// RiskTripped 报告 now 时刻是否处于风控熔断期。自动化入口开跑前调用。
func (p *Pool) RiskTripped(now time.Time) bool {
	return p.guard().Tripped(now)
}

// RiskState 返回熔断状态快照（面板 overview / 排查用）。
func (p *Pool) RiskState(now time.Time) RiskState {
	return p.guard().Snapshot(now)
}
