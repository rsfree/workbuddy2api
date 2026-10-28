// growth_assets.go 成长资产巡检：签到活动状态 + 能量余额（只读）。
//
// 为什么单独一个端点、而不是塞进 overview——overview 每次刷新都跑，而这两个
// 数据每个账号要各打一次上游（225 个号 = 450 个请求），不能挂在热路径上。
// 这里做成按需拉取，前端卡片打开时才查。
//
// 为什么两个数据合一个端点——它们是同一次「这个号还有多少肉没吃」的判断材料，
// 分两个端点前端要并发两次、还要自己对齐 uid。合并后一次遍历两请求。
//
// 口径纪律：
//   - global 与企业版一律跳过（无 CN 成长体系，签到 400 / growth 403）；
//   - 单账号失败不影响整体，错误计数进 summary，前端能看见「几个号没查到」；
//   - 结果按连签天数降序（最容易掉签的号排前面，运维一眼看到该补谁）。
package panel

import (
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// growthAssetConcurrency 并发上限：225 个号 × 2 请求，12 并发下约 10~20 秒，
// 再高容易触发上游限流（实测同一 token 短时多请求会 429）。
const growthAssetConcurrency = 12

// growthAssetDefaultLimit 默认巡检账号数。全量 225 个号要打 450 次上游，
// 而「活动什么时候结束、今天漏签没有」看前 60 个号就够了。
const growthAssetDefaultLimit = 60

// growthAssetHardLimit 上限，防止 ?limit=99999 打穿上游。
const growthAssetHardLimit = 300

// growthAssetAccount 单账号资产快照（前端卡片一行）。
type growthAssetAccount struct {
	UID             string `json:"uid"`
	Nickname        string `json:"nickname,omitempty"`
	Active          bool   `json:"active"`
	TodayCheckedIn  bool   `json:"today_checked_in"`
	StreakDays      int    `json:"streak_days"`
	WeekCheckinDays int    `json:"week_checkin_days"`
	WeekProgress    []bool `json:"week_progress,omitempty"`
	TotalCredits    int64  `json:"total_credits"`
	DailyCredit     int64  `json:"daily_credit"`
	Energy          int64  `json:"energy"`
	// Err 非空表示该号查询失败（签到活动或能量其一失败即记），
	// 前端在这些行上打「未查到」而不是当成 0 展示。
	Err string `json:"err,omitempty"`
}

// growthAssetSummary 汇总（前端顶部统计条 + 运维判断）。
type growthAssetSummary struct {
	Accounts     int   `json:"accounts"`      // 实际巡检账号数
	CheckedIn    int   `json:"checked_in"`    // 今日已签
	Missing      int   `json:"missing"`       // 今日未签（= 还能立刻拿的分）
	Active       int   `json:"active"`        // 活动开启中的账号数
	Errors       int   `json:"errors"`        // 查询失败账号数
	EnergyTotal  int64 `json:"energy_total"`  // 能量合计
	CreditsTotal int64 `json:"credits_total"` // 活动期内累计到账积分合计
	MinStreak    int   `json:"min_streak"`
	MaxStreak    int   `json:"max_streak"`
}

// growthAssetActivity 活动元信息（全账号一致，取任一成功样本）。
type growthAssetActivity struct {
	Active       bool   `json:"active"`
	ActivityName string `json:"activity_name,omitempty"`
	ThemeName    string `json:"theme_name,omitempty"`
	Season       int    `json:"season"`
	StartTime    string `json:"start_time,omitempty"`
	EndTime      string `json:"end_time,omitempty"`
	DailyCredit  int64  `json:"daily_credit"`
}

// growthAssets 成长资产巡检（只读）。查询参数 limit 控制巡检账号数。
func (p *Panel) growthAssets(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Upstream == nil {
		writeErr(w, http.StatusNotImplemented, "upstream not available")
		return
	}
	limit := growthAssetDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > growthAssetHardLimit {
		limit = growthAssetHardLimit
	}

	// 只挑有成长体系的号：global / 企业版查这两个接口必然 403/400，
	// 白白把 summary.errors 撑起来，还会误导成「坏了」。
	type target struct {
		a   *auth.Auth
		uid string
		nik string
	}
	var targets []target
	for _, ps := range p.cfg.Pool.List() {
		if ps.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(ps.UID)
		if a == nil || a.AccessTokenValue() == "" || a.IsGlobal() || a.IsEnterprise() {
			continue
		}
		targets = append(targets, target{a: a, uid: ps.UID, nik: ps.Nickname})
		if len(targets) >= limit {
			break
		}
	}

	accounts := make([]growthAssetAccount, 0, len(targets))
	var mu sync.Mutex
	// actSnap 活动元信息快照：全账号一致，取第一个成功样本即可（换季时
	// season/theme 一起变，前端靠它判断「活动换代了」）。
	var actSnap *upstream.CheckinActivity
	var wg sync.WaitGroup
	sem := make(chan struct{}, growthAssetConcurrency)
	started := time.Now()

	for _, tg := range targets {
		wg.Add(1)
		go func(tg target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			row := growthAssetAccount{UID: tg.uid, Nickname: tg.nik}
			st, err := p.cfg.Upstream.CheckinActivityStatus(tg.a)
			if err != nil {
				row.Err = "签到活动: " + truncateStr(err.Error(), 90)
			} else {
				mu.Lock()
				if actSnap == nil {
					actSnap = st
				}
				mu.Unlock()
				row.Active = st.Active
				row.TodayCheckedIn = st.TodayCheckedIn
				row.StreakDays = st.StreakDays
				row.WeekCheckinDays = st.WeekCheckinDays
				row.WeekProgress = st.WeekProgress
				row.TotalCredits = st.TotalCredits
				row.DailyCredit = st.DailyCredit
			}
			// 能量失败不覆盖签到结果（两条独立链路，一个挂了另一个仍有效）。
			if en, eerr := p.cfg.Upstream.Energy(tg.a); eerr == nil {
				row.Energy = en.Balance
			} else if row.Err == "" {
				row.Err = "能量: " + truncateStr(eerr.Error(), 90)
			}

			mu.Lock()
			accounts = append(accounts, row)
			mu.Unlock()
		}(tg)
	}
	wg.Wait()

	// 连签天数降序：最容易掉签的号排前面（同天数按 uid 稳定排序）。
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].StreakDays != accounts[j].StreakDays {
			return accounts[i].StreakDays < accounts[j].StreakDays
		}
		return accounts[i].UID < accounts[j].UID
	})

	sum := growthAssetSummary{Accounts: len(accounts), MinStreak: -1}
	act := growthAssetActivity{}
	for _, row := range accounts {
		if row.Err != "" {
			sum.Errors++
		}
		if row.Active {
			sum.Active++
		}
		if row.TodayCheckedIn {
			sum.CheckedIn++
		} else if row.Err == "" {
			sum.Missing++
		}
		sum.EnergyTotal += row.Energy
		sum.CreditsTotal += row.TotalCredits
		if sum.MinStreak < 0 || row.StreakDays < sum.MinStreak {
			sum.MinStreak = row.StreakDays
		}
		if row.StreakDays > sum.MaxStreak {
			sum.MaxStreak = row.StreakDays
		}
	}
	if sum.MinStreak < 0 {
		sum.MinStreak = 0
	}

	if actSnap != nil {
		act = growthAssetActivity{
			Active:       actSnap.Active,
			ActivityName: actSnap.ActivityName,
			ThemeName:    actSnap.ThemeName,
			Season:       actSnap.Season,
			StartTime:    actSnap.StartTime,
			EndTime:      actSnap.EndTime,
			DailyCredit:  actSnap.DailyCredit,
		}
	}

	log.Printf("panel: 成长资产巡检：%d 个账号 · 已签 %d · 未签 %d · 能量合计 %d · 耗时 %s",
		sum.Accounts, sum.CheckedIn, sum.Missing, sum.EnergyTotal, time.Since(started).Round(time.Millisecond))

	writeJSON(w, http.StatusOK, map[string]any{
		"summary":  sum,
		"activity": act,
		"accounts": accounts,
		"elapsed":  time.Since(started).Milliseconds(),
	})
}
