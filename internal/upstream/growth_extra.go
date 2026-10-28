// growth_extra.go growth 域「非任务型」积分入口：v1 关卡、能量账户、签到活动。
//
// 为什么单独一份——三者的共同点是**此前完全没接入**，而不是任务列表的附属字段：
//
//   - v1 关卡（GET /activity/growth/tasks，无 /v2 前缀）是**另一套任务体系**。
//     实测与 /v2 列表互不相交：v2 下发 chat_5/expert_5/… 共 20 码，v1 下发
//     first_chat/skill_installed/wechat_linked/expert_summoned/template_used
//     五个「养虾」关卡（level_name 从「养虾尝试」到「控虾大神」）。字段里
//     **没有任何 reward_credit/reward_energy**，且 web 域 claim 一律
//     `400 task not completed`（2026-10-28 两账号实测）⇒ 它是纯进度/等级体系，
//     不产生积分。接入目的只有一个：**发现**——上游换了什么新关卡要能看见。
//
//   - 能量账户（GET /activity/growth/energy）：credit 之外的第二条资产线，
//     奖励里常带 energy（如 first_buddy +8、连登档位 +5）。余额此前从未展示。
//
//   - 签到活动（POST /billing/meter/checkin-activity-status）：签到动作
//     （daily-checkin）一直有跑，但**活动本身的元信息**没人读——赛季号、主题名、
//     连签天数、本周 7 格进度、累计到账、活动截止日（实测 season 10
//     「Buddy加油站」，2026-09-30 ~ 2026-12-31）。活动是赛季制，换季/开新活动
//     时只有这个接口能第一时间看出来。
//
// 三个接口都只读，不产生进度、不消耗额度。
package upstream

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// legacyTasksPath 是 v1 关卡列表端点。注意**没有 /v2 前缀**——带 /v2 就落到
// v2 任务列表上，两者是不同资源。
const legacyTasksPath = "/activity/growth/tasks"

// checkinActivityPath 签到活动状态（billing 域，POST + 空 body）。
// 同族还有 checkin-status（旧版签到，实测 active=false）、
// get-user-resource-summary（套餐额度），均属额度而非积分，暂不接入。
const checkinActivityPath = "/billing/meter/checkin-activity-status"

// growthEnergyPath 能量账户余额（growth 域，GET）。
const growthEnergyPath = "/activity/growth/energy"

// LegacyTask v1 关卡条目。刻意不复用 Task：字段集完全不同（无 reward/progress/
// accept 语义），混用会让调用方误以为可以 accept/claim。
type LegacyTask struct {
	Code      string `json:"code"`
	Title     string `json:"title,omitempty"`
	Desc      string `json:"description,omitempty"`
	LevelName string `json:"level_name,omitempty"` // 「养虾尝试」…「控虾大神」
	Status    string `json:"status,omitempty"`     // available / done
	URL       string `json:"url,omitempty"`
}

// ListLegacyTasks 拉取 v1 关卡列表（data.tasks[]）。
// 与 ListTasks 是不同资源，调用方须分开处理；本方法只服务于「发现」。
func (c *Client) ListLegacyTasks(a *auth.Auth) ([]LegacyTask, error) {
	data, err := c.growthJSON(a, http.MethodGet, legacyTasksPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tasks []LegacyTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Tasks, nil
}

// GrowthEnergy 能量账户快照。
type GrowthEnergy struct {
	Balance       int64 `json:"balance"`
	TotalConsumed int64 `json:"total_consumed"`
	TotalEarned   int64 `json:"total_earned"`
}

// Energy 查询能量余额（growth 域，GET，只读）。
func (c *Client) Energy(a *auth.Auth) (*GrowthEnergy, error) {
	data, err := c.growthJSON(a, http.MethodGet, growthEnergyPath, nil)
	if err != nil {
		return nil, err
	}
	out := &GrowthEnergy{}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	return out, nil
}

// CheckinActivity 签到活动快照（赛季制）。
//
// 字段口径实测（2026-10-28，season 10「Buddy加油站」）：
//   - week_progress 是 7 格布尔，表示本周（周一起）哪几天签过；
//   - streak_bonus_* 是连签到档位时的额外奖励，0 表示当前无待发；
//   - start/end_time 是活动窗口，过期后 active=false；
//   - activity_name/theme_name/season 用于识别「换季了」——上游换活动时这
//     三个值会变，而接口路径不变。
type CheckinActivity struct {
	Active bool `json:"active"`
	// TodayCheckedIn 今日是否已签；false 即当天还有 100 分没拿。
	TodayCheckedIn bool `json:"today_checked_in"`
	StreakDays     int  `json:"streak_days"`
	// DailyCredit 每次签到固定发放的积分；TodayCredit 今日实际到账。
	DailyCredit int64 `json:"daily_credit"`
	TodayCredit int64 `json:"today_credit"`
	// IsStreakDay 当天是否为连签奖励日；NextStreakDay 距下一个奖励日还有几天。
	IsStreakDay       bool  `json:"is_streak_day"`
	NextStreakDay     int   `json:"next_streak_day"`
	StreakBonusDays   int   `json:"streak_bonus_days"`
	StreakBonusCredit int64 `json:"streak_bonus_credit"`

	CheckinDates []string `json:"checkin_dates"`
	// WeekCheckinDays 本周已签天数，WeekProgress 为逐日明细（周一起）。
	WeekCheckinDays int    `json:"week_checkin_days"`
	WeekProgress    []bool `json:"week_progress"`
	// TotalCredits 本活动期内累计到账积分。
	TotalCredits int64  `json:"total_credits"`
	StartTime    string `json:"start_time"`
	EndTime      string `json:"end_time"`
	// ThemeName / Season / ActivityName 活动身份，换季判据。
	ThemeName    string `json:"theme_name"`
	Season       int    `json:"season"`
	ActivityName string `json:"activity_name"`
}

// CheckinActivityStatus 查询签到活动状态（billing 域，POST + 空 body；只读）。
// 上游对该路径只注册了 POST，GET 返回裸 404（实测）。
func (c *Client) CheckinActivityStatus(a *auth.Auth) (*CheckinActivity, error) {
	data, err := c.billingJSON(a, http.MethodPost, checkinActivityPath, map[string]any{})
	if err != nil {
		return nil, err
	}
	out := &CheckinActivity{}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	return out, nil
}
