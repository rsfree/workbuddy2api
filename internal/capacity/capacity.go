// Package capacity 从现有运行数据生成只读容量预测与运维建议。
// 不调用上游、不修改账号或配置；用量与请求归档分别代表账号尝试和客户端逻辑请求。
package capacity

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

type Input struct {
	Now                  time.Time
	ForecastDays         int
	RecentHours          int
	Last24h, Previous24h usage.Snapshot
	Last7d               usage.Snapshot
	Accounts             []pool.Status
	CreditEntries        []credithist.Entry
	Events               []reqlog.Event
	HiddenModels         map[string]struct{}
	MaxInFlight          int
	MaxInFlightGlobal    int
	ArchiveBytes         int64
}

type Method struct {
	DailyBurn            string  `json:"daily_burn"`
	PlannedReplenishment string  `json:"planned_replenishment"`
	Headroom             float64 `json:"headroom"`
	AccountUnit          string  `json:"account_unit"`
	Concurrency          string  `json:"concurrency"`
	UsageSemantics       string  `json:"usage_semantics"`
}

type UsageWindow struct {
	AccountAttempts int64   `json:"account_attempts"`
	AttemptErrors   int64   `json:"attempt_errors"`
	Credits         float64 `json:"credits"`
	Tokens          int64   `json:"tokens"`
	CreditSamples   int64   `json:"credit_samples"`
}

type UsageReport struct {
	Last24h                            UsageWindow `json:"last_24h"`
	Previous24h                        UsageWindow `json:"previous_24h"`
	Last7d                             UsageWindow `json:"last_7d"`
	DailyCreditsForecast               float64     `json:"daily_credits_forecast"`
	DailyAccountAttemptsForecast       float64     `json:"daily_account_attempts_forecast"`
	CreditGrowthVsPrevious24hPct       *float64    `json:"credit_growth_vs_previous_24h_pct"`
	PositiveReplenishmentObserved24h   int64       `json:"positive_replenishment_observed_24h"`
	PositiveReplenishmentAssumedPerDay float64     `json:"positive_replenishment_assumed_per_day"`
}

type PoolReport struct {
	AccountsTotal              int              `json:"accounts_total"`
	AccountsEnabled            int              `json:"accounts_enabled"`
	AccountsCreditPositive     int              `json:"accounts_credit_positive"`
	AccountsCoolingWithCredit  int              `json:"accounts_cooling_with_credit"`
	AccountsAvailableEstimate  int              `json:"accounts_available_estimate"`
	CreditsRemaining           int64            `json:"credits_remaining"`
	CreditsTotalObserved       int64            `json:"credits_total_observed"`
	NewAccountCreditUnitP25    float64          `json:"new_account_credit_unit_p25"`
	NewAccountCreditUnitMedian float64          `json:"new_account_credit_unit_median"`
	CreditsExpiring            map[string]int64 `json:"credits_expiring"`
	MaxInFlightPerAccount      int              `json:"max_in_flight_per_account"`
	MaxInFlightGlobal          int              `json:"max_in_flight_global"`
	InFlightPoolLimitEstimate  *int             `json:"in_flight_pool_limit_estimate"`
}

type ModelReport struct {
	Model               string         `json:"model"`
	Requests            int            `json:"requests"`
	SuccessRate         float64        `json:"success_rate"`
	Credits             float64        `json:"credits"`
	Statuses            map[string]int `json:"statuses"`
	HTTP400             int            `json:"http_400"`
	HTTP429             int            `json:"http_429"`
	HTTP503             int            `json:"http_503"`
	Recent6hRequests    int            `json:"recent_6h_requests"`
	Recent6hSuccessRate *float64       `json:"recent_6h_success_rate"`
}

type TrafficReport struct {
	Requests               int            `json:"requests"`
	SuccessRate            float64        `json:"success_rate"`
	Statuses               map[string]int `json:"statuses"`
	Outcomes               map[string]int `json:"outcomes"`
	AttemptsPerRequest     float64        `json:"attempts_per_request"`
	Credits                float64        `json:"credits"`
	P95DurationMs          float64        `json:"p95_duration_ms"`
	AvgInFlightEstimate    float64        `json:"avg_in_flight_estimate"`
	Peak5mInFlightEstimate float64        `json:"peak_5m_in_flight_estimate"`
	Models                 []ModelReport  `json:"models"`
}

type Scenario struct {
	Days                             int     `json:"days"`
	CreditsRequiredWith20pctHeadroom float64 `json:"credits_required_with_20pct_headroom"`
	CreditsUsableAfterExpiry         float64 `json:"credits_usable_after_expiry"`
	CreditsReplenishmentAssumed      float64 `json:"credits_replenishment_assumed"`
	CreditsShortfall                 float64 `json:"credits_shortfall"`
	AdditionalAccounts               *int    `json:"additional_accounts"`
}

type Horizon struct {
	Planned Scenario `json:"planned_50pct_replenishment"`
	Stable  Scenario `json:"stable_replenishment"`
	None    Scenario `json:"no_replenishment"`
}

type Recommendation struct {
	Priority string `json:"priority"`
	Category string `json:"category"`
	Title    string `json:"title"`
	Action   string `json:"action"`
	Evidence string `json:"evidence"`
}

type Report struct {
	GeneratedAt   time.Time           `json:"generated_at"`
	Method        Method              `json:"method"`
	Confidence    string              `json:"confidence"`
	RuntimePolicy map[string][]string `json:"runtime_policy"`
	Usage         UsageReport         `json:"usage"`
	Pool          PoolReport          `json:"pool"`
	Traffic       TrafficReport       `json:"traffic"`
	Forecast      struct {
		RunwayDays float64            `json:"runway_days_without_headroom"`
		Horizons   map[string]Horizon `json:"horizons"`
	} `json:"forecast"`
	Recommendations []Recommendation `json:"recommendations"`
	Coverage        struct {
		RequestEvents        int     `json:"request_events"`
		RequestHours         float64 `json:"request_hours"`
		RequestBytes         int64   `json:"request_bytes"`
		CreditHistoryHours   float64 `json:"credit_history_hours"`
		CreditHistoryEntries int     `json:"credit_history_entries"`
		UsageBuckets         int     `json:"usage_buckets"`
	} `json:"coverage"`
	Parameters struct {
		ForecastDays int  `json:"forecast_days"`
		RecentHours  int  `json:"recent_hours"`
		Cached       bool `json:"cached"`
	} `json:"parameters"`
}

func Analyze(in Input) Report {
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	if in.ForecastDays < 1 {
		in.ForecastDays = 7
	}
	if in.RecentHours < 1 {
		in.RecentHours = 24
	}
	last, prev := in.Last24h.Totals, in.Previous24h.Totals
	dailyBurn := math.Max(last.Credits, .7*last.Credits+.3*prev.Credits)
	dailyRequests := math.Max(float64(last.Requests), .7*float64(last.Requests)+.3*float64(prev.Requests))
	replenishment, creditCoverage := replenishment(in.CreditEntries, in.Now)
	poolReport := accountState(in)
	unit := poolReport.NewAccountCreditUnitP25
	traffic := trafficSummary(in.Events, in.Now.Add(-time.Duration(in.RecentHours)*time.Hour), in.Now)
	traffic6 := trafficSummary(in.Events, in.Now.Add(-6*time.Hour), in.Now)
	health := map[string]ModelReport{}
	for _, m := range traffic6.Models {
		health[m.Model] = m
	}
	for i := range traffic.Models {
		m := &traffic.Models[i]
		if recent, ok := health[m.Model]; ok {
			m.Recent6hRequests, m.Recent6hSuccessRate = recent.Requests, &recent.SuccessRate
		}
	}
	r := Report{GeneratedAt: in.Now, Confidence: "low", Pool: poolReport, Traffic: traffic,
		Method: Method{
			DailyBurn:            "max(last_24h, 0.7*last_24h + 0.3*previous_24h)",
			PlannedReplenishment: "50% of positive credit deltas observed in the last 24h",
			Headroom:             .20, AccountUnit: "p25 of positive credits_total across enabled accounts",
			Concurrency:    "5-minute completed-request work / 300s; estimate, not exact lease occupancy",
			UsageSemantics: "usage account attempts; request archive logical client requests",
		}, RuntimePolicy: map[string][]string{"hidden_models": hiddenNames(in.HiddenModels)}}
	r.Usage = UsageReport{Last24h: window(last), Previous24h: window(prev), Last7d: window(in.Last7d.Totals),
		DailyCreditsForecast: rounded(dailyBurn, 2), DailyAccountAttemptsForecast: rounded(dailyRequests, 0),
		PositiveReplenishmentObserved24h: replenishment, PositiveReplenishmentAssumedPerDay: rounded(float64(replenishment)*.5, 2)}
	if prev.Credits > 0 {
		growth := rounded((last.Credits/prev.Credits-1)*100, 2)
		r.Usage.CreditGrowthVsPrevious24hPct = &growth
	}
	r.Forecast.Horizons = map[string]Horizon{}
	days := []int{1, 3, in.ForecastDays}
	for _, d := range days {
		if _, exists := r.Forecast.Horizons[fmt.Sprint(d)]; exists {
			continue
		}
		expiry := poolReport.CreditsExpiring[fmt.Sprint(d)]
		if d > 7 {
			expiry = poolReport.CreditsExpiring["30"]
		}
		r.Forecast.Horizons[fmt.Sprint(d)] = Horizon{
			Planned: scenario(d, dailyBurn, poolReport.CreditsRemaining, float64(replenishment)*.5, unit, expiry),
			Stable:  scenario(d, dailyBurn, poolReport.CreditsRemaining, float64(replenishment), unit, expiry),
			None:    scenario(d, dailyBurn, poolReport.CreditsRemaining, 0, unit, expiry),
		}
	}
	netBurn := math.Max(.01, dailyBurn-float64(replenishment)*.5)
	r.Forecast.RunwayDays = rounded(float64(poolReport.CreditsRemaining)/netBurn, 2)
	var earliestEvent, earliestCredit time.Time
	for _, e := range in.Events {
		if earliestEvent.IsZero() || e.Time.Before(earliestEvent) {
			earliestEvent = e.Time
		}
	}
	for _, e := range in.CreditEntries {
		if earliestCredit.IsZero() || e.Time.Before(earliestCredit) {
			earliestCredit = e.Time
		}
	}
	requestHours := durationHours(in.Now, earliestEvent)
	r.Coverage.RequestEvents, r.Coverage.RequestHours, r.Coverage.RequestBytes = len(in.Events), rounded(requestHours, 2), in.ArchiveBytes
	r.Coverage.CreditHistoryHours, r.Coverage.CreditHistoryEntries = rounded(math.Min(durationHours(in.Now, earliestCredit), 24), 2), len(in.CreditEntries)
	r.Coverage.UsageBuckets = in.Last7d.Buckets
	if requestHours >= 168 && creditCoverage >= 24 {
		r.Confidence = "high"
	} else if requestHours >= 24 && creditCoverage >= 12 {
		r.Confidence = "medium"
	}
	r.Recommendations = recommendations(in, r)
	r.Parameters.ForecastDays, r.Parameters.RecentHours = in.ForecastDays, in.RecentHours
	return r
}

func window(a usage.Agg) UsageWindow {
	return UsageWindow{a.Requests, a.Errors, rounded(a.Credits, 2), a.TotalTokens, a.CreditSamples}
}

func accountState(in Input) PoolReport {
	r := PoolReport{CreditsExpiring: map[string]int64{"1": 0, "3": 0, "7": 0, "30": 0}, MaxInFlightPerAccount: in.MaxInFlight, MaxInFlightGlobal: in.MaxInFlightGlobal}
	r.AccountsTotal = len(in.Accounts)
	var totals []float64
	limit, unlimited := 0, false
	for _, a := range in.Accounts {
		if a.Disabled || a.Paused {
			continue
		}
		r.AccountsEnabled++
		if a.Credits > 0 {
			r.CreditsRemaining += a.Credits
			r.AccountsCreditPositive++
		}
		if a.CreditsTotal > 0 {
			totals = append(totals, float64(a.CreditsTotal))
			r.CreditsTotalObserved += a.CreditsTotal
		}
		cooling := a.Credits > 0 && (a.Cooling || a.Until.After(in.Now))
		if cooling {
			r.AccountsCoolingWithCredit++
		}
		if a.Credits > 0 && !cooling {
			r.AccountsAvailableEstimate++
			n := in.MaxInFlight
			if a.Realm == "global" && in.MaxInFlightGlobal > 0 {
				n = in.MaxInFlightGlobal
			}
			if n > 0 {
				limit += n
			} else {
				unlimited = true
			}
		}
		for _, b := range a.CreditsExpiryBuckets {
			if b.Remain <= 0 || !b.ExpireAt.After(in.Now) {
				continue
			}
			for _, d := range []int{1, 3, 7, 30} {
				if !b.ExpireAt.After(in.Now.Add(time.Duration(d) * 24 * time.Hour)) {
					r.CreditsExpiring[fmt.Sprint(d)] += b.Remain
				}
			}
		}
	}
	r.NewAccountCreditUnitP25, r.NewAccountCreditUnitMedian = rounded(percentile(totals, .25), 0), rounded(percentile(totals, .5), 0)
	if !unlimited {
		r.InFlightPoolLimitEstimate = &limit
	}
	return r
}

func replenishment(entries []credithist.Entry, now time.Time) (int64, float64) {
	var sum int64
	var earliest time.Time
	for _, e := range entries {
		if earliest.IsZero() || e.Time.Before(earliest) {
			earliest = e.Time
		}
		if e.Delta > 0 && !e.Time.Before(now.Add(-24*time.Hour)) && !e.Time.After(now) {
			sum += e.Delta
		}
	}
	return sum, math.Min(durationHours(now, earliest), 24)
}

func durationHours(now, earliest time.Time) float64 {
	if earliest.IsZero() || earliest.After(now) {
		return 0
	}
	return now.Sub(earliest).Hours()
}

func trafficSummary(events []reqlog.Event, since, now time.Time) TrafficReport {
	r := TrafficReport{Statuses: map[string]int{}, Outcomes: map[string]int{}, Models: []ModelReport{}}
	type acc struct {
		report    ModelReport
		statuses  map[int]int
		successes int
	}
	models := map[string]*acc{}
	work := map[int64]float64{}
	var durations []float64
	var attempts, durationSum int64
	var successes int
	for _, e := range events {
		if e.Time.Before(since) || e.Time.After(now) {
			continue
		}
		r.Requests++
		if e.OK {
			successes++
		}
		r.Statuses[fmt.Sprint(e.Status)]++
		outcome := e.Outcome
		if outcome == "" {
			outcome = "unknown"
		}
		r.Outcomes[outcome]++
		n := e.Attempts
		if n < 1 {
			n = 1
		}
		attempts += int64(n)
		d := e.DurationMs
		if d < 0 {
			d = 0
		}
		durationSum += d
		durations = append(durations, float64(d))
		r.Credits += e.Credit
		work[e.Time.Unix()/300*300] += float64(d)
		name := e.Model
		if name == "" {
			name = "(unknown)"
		}
		m := models[name]
		if m == nil {
			m = &acc{report: ModelReport{Model: name}, statuses: map[int]int{}}
			models[name] = m
		}
		m.report.Requests++
		m.report.Credits += e.Credit
		m.statuses[e.Status]++
		if e.OK {
			m.successes++
		}
	}
	if r.Requests > 0 {
		r.SuccessRate = rounded(100*float64(successes)/float64(r.Requests), 2)
		r.AttemptsPerRequest = rounded(float64(attempts)/float64(r.Requests), 4)
	}
	r.Credits = rounded(r.Credits, 2)
	r.P95DurationMs = rounded(percentile(durations, .95), 0)
	r.AvgInFlightEstimate = rounded(float64(durationSum)/1000/math.Max(1, now.Sub(since).Seconds()), 2)
	for _, v := range work {
		r.Peak5mInFlightEstimate = math.Max(r.Peak5mInFlightEstimate, v/300000)
	}
	r.Peak5mInFlightEstimate = rounded(r.Peak5mInFlightEstimate, 2)
	for _, m := range models {
		m.report.SuccessRate = rounded(100*float64(m.successes)/float64(m.report.Requests), 2)
		m.report.Credits = rounded(m.report.Credits, 2)
		m.report.HTTP400, m.report.HTTP429, m.report.HTTP503 = m.statuses[400], m.statuses[429], m.statuses[503]
		m.report.Statuses = map[string]int{}
		for status, n := range m.statuses {
			m.report.Statuses[fmt.Sprint(status)] = n
		}
		r.Models = append(r.Models, m.report)
	}
	sort.Slice(r.Models, func(i, j int) bool {
		if r.Models[i].Requests == r.Models[j].Requests {
			return r.Models[i].Model < r.Models[j].Model
		}
		return r.Models[i].Requests > r.Models[j].Requests
	})
	return r
}

func scenario(days int, burn float64, remaining int64, replenishment, unit float64, expiry int64) Scenario {
	gross := burn * float64(days) * 1.2
	usable := math.Max(0, float64(remaining-expiry))
	shortfall := math.Max(0, gross-usable-replenishment*float64(days))
	s := Scenario{Days: days, CreditsRequiredWith20pctHeadroom: rounded(gross, 2), CreditsUsableAfterExpiry: rounded(usable, 2), CreditsReplenishmentAssumed: rounded(replenishment*float64(days), 2), CreditsShortfall: rounded(shortfall, 2)}
	if unit > 0 {
		n := int(math.Ceil(shortfall / unit))
		s.AdditionalAccounts = &n
	}
	return s
}

func recommendations(in Input, r Report) []Recommendation {
	var out []Recommendation
	add := func(p, c, t, a, e string) { out = append(out, Recommendation{p, c, t, a, e}) }
	h := r.Forecast.Horizons[fmt.Sprint(in.ForecastDays)]
	main, low, high := h.Planned.AdditionalAccounts, h.Stable.AdditionalAccounts, h.None.AdditionalAccounts
	if main == nil {
		add("P1", "data", "缺少单号额度基线", "先刷新账号积分总额度；无法可靠换算补号数。", "credits_total 没有有效样本。")
	} else if *main > 0 {
		priority := "P1"
		if r.Forecast.RunwayDays < 3 {
			priority = "P0"
		}
		add(priority, "accounts", fmt.Sprintf("未来 %d 天计划补 %d 个号", in.ForecastDays, *main), fmt.Sprintf("分批补充并观察实际额度；补给稳定时约 %s 个，完全不补给的保守上界约 %s 个。", accountCount(low), accountCount(high)), fmt.Sprintf("日耗 %.2f credits，现有 %d，单号保守额度 %.0f，预计裸续航 %.1f 天。", r.Usage.DailyCreditsForecast, r.Pool.CreditsRemaining, r.Pool.NewAccountCreditUnitP25, r.Forecast.RunwayDays))
	} else {
		add("P2", "accounts", "当前窗口无需为积分补号", "保持每日复核；需求或模型结构变化后重跑。", fmt.Sprintf("%d 天计划缺口为 0，裸续航约 %.1f 天。", in.ForecastDays, r.Forecast.RunwayDays))
	}
	requests := r.Traffic.Requests
	rate429 := 0.0
	if requests > 0 {
		rate429 = 100 * float64(r.Traffic.Statuses["429"]) / float64(requests)
	}
	limitOK := r.Pool.InFlightPoolLimitEstimate == nil || r.Traffic.Peak5mInFlightEstimate < .7*float64(*r.Pool.InFlightPoolLimitEstimate)
	if requests > 0 && rate429 < 1 && limitOK {
		limitText := "不限"
		if r.Pool.InFlightPoolLimitEstimate != nil {
			limitText = fmt.Sprint(*r.Pool.InFlightPoolLimitEstimate)
		}
		add("P2", "concurrency", "暂不提高单号并发", fmt.Sprintf("max_in_flight 保持 %d；先解决积分和具体模型失败，不用加并发掩盖问题。", in.MaxInFlight), fmt.Sprintf("近 %dh 429 占比 %.2f%%，5 分钟并发估算峰值 %.2f，池上限估算 %s。", in.RecentHours, rate429, r.Traffic.Peak5mInFlightEstimate, limitText))
	} else if rate429 >= 1 {
		add("P1", "concurrency", "限流仍明显，优先降并发而非加号", fmt.Sprintf("把 max_in_flight 从 %d 试降到 %d，观察 429、软冷却和成功率后再回调。", in.MaxInFlight, maxInt(1, int(float64(in.MaxInFlight)*.75))), fmt.Sprintf("近 %dh 429 占比 %.2f%%。", in.RecentHours, rate429))
	}
	for _, m := range r.Traffic.Models {
		if _, hidden := in.HiddenModels[m.Model]; hidden {
			add("P2", "model", fmt.Sprintf("模型 %s 已按配置隐藏", m.Model), "相关 400 属于预期的网关拒绝，不按模型故障处理，也不需要补号或改超时。", fmt.Sprintf("models.hidden 包含该模型；近 %dh 仍有 %d 次点名调用，应从客户端模型列表移除。", in.RecentHours, m.Requests))
			continue
		}
		recentBad := m.Recent6hRequests >= 20 && m.Recent6hSuccessRate != nil && *m.Recent6hSuccessRate < 90
		if m.Requests < 20 || (m.SuccessRate >= 90 && !recentBad) {
			continue
		}
		if m.Recent6hRequests >= 20 && m.Recent6hSuccessRate != nil && *m.Recent6hSuccessRate >= 97 {
			add("P2", "model", fmt.Sprintf("模型 %s 此前异常，近 6h 已恢复", m.Model), "暂不改路由或隐藏模型；继续观察 24h 成功率，避免用旧故障做过期调参。", fmt.Sprintf("近 %dh 成功率 %.2f%%，但近 6h %d 次、成功率 %.2f%%。", in.RecentHours, m.SuccessRate, m.Recent6hRequests, *m.Recent6hSuccessRate))
			continue
		}
		action := "失败主要来自中断或流式响应；优先核对客户端超时、SSE 空闲超时与上游稳定性，不要用补号掩盖。"
		if m.HTTP400 > 0 && m.HTTP400 >= maxInt(m.HTTP429, m.HTTP503) {
			action = "优先检查该模型的参数兼容性/目录映射；400 不是补号能解决的问题。可临时将客户端切到同能力健康模型。"
		} else if m.HTTP429+m.HTTP503 > 0 {
			action = "优先检查该模型对应域的账号健康与路由；显式 cn:/global: 请求不会跨域回退，先对照健康域再调整。"
		}
		recentText := "—"
		if m.Recent6hSuccessRate != nil {
			recentText = fmt.Sprintf("%.2f", *m.Recent6hSuccessRate)
		}
		add("P1", "model", fmt.Sprintf("模型 %s 成功率异常", m.Model), action, fmt.Sprintf("近 %dh %d 次、成功率 %.2f%%；近 6h %d 次、成功率 %s%%；400=%d，429=%d，503=%d。", in.RecentHours, m.Requests, m.SuccessRate, m.Recent6hRequests, recentText, m.HTTP400, m.HTTP429, m.HTTP503))
	}
	days := math.Max(r.Coverage.RequestHours/24, 1.0/24)
	mb30 := math.Max(100, math.Ceil(float64(r.Coverage.RequestBytes)/days*30*1.3/(1<<20)/100)*100)
	add("P2", "observability", "延长请求归档以提高下次预测可信度", fmt.Sprintf("建议 request_retention_days=30、request_archive_max_mb=%.0f；usage 长期桶继续保留，请求归档用于成功率和模型健康复核。", mb30), fmt.Sprintf("当前请求归档覆盖约 %.1f 天、%d 条，积分流水覆盖约 %.1fh。", r.Coverage.RequestHours/24, r.Coverage.RequestEvents, r.Coverage.CreditHistoryHours))
	priority := map[string]int{"P0": 0, "P1": 1, "P2": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority == out[j].Priority {
			return out[i].Category < out[j].Category
		}
		return priority[out[i].Priority] < priority[out[j].Priority]
	})
	return out
}

func hiddenNames(hidden map[string]struct{}) []string {
	out := make([]string, 0, len(hidden))
	for name := range hidden {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
func accountCount(n *int) string {
	if n == nil {
		return "未知"
	}
	return fmt.Sprint(*n)
}
func percentile(values []float64, fraction float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	pos := fraction * float64(len(values)-1)
	lo, hi := int(math.Floor(pos)), int(math.Ceil(pos))
	if lo == hi {
		return values[lo]
	}
	return values[lo]*(1-(pos-float64(lo))) + values[hi]*(pos-float64(lo))
}
func rounded(v float64, digits int) float64 {
	factor := math.Pow10(digits)
	return math.Round(v*factor) / factor
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
