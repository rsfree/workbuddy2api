package capacity

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

func snapshot(credits float64, requests int64) usage.Snapshot {
	return usage.Snapshot{Totals: usage.Agg{Credits: credits, Requests: requests, CreditSamples: requests}}
}

func TestAnalyzeForecastAndDisabledAccounts(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 35, 0, 0, time.FixedZone("CST", 8*3600))
	r := Analyze(Input{
		Now: now, ForecastDays: 7, RecentHours: 24,
		Last24h: snapshot(1000, 100), Previous24h: snapshot(500, 50), Last7d: snapshot(4000, 400),
		Accounts: []pool.Status{
			{Credits: 1000, CreditsTotal: 1000, Realm: "cn"},
			{Credits: 1000, CreditsTotal: 2000, Realm: "cn"},
			{Credits: 1000, CreditsTotal: 3000, Realm: "global"},
			{Credits: 1000, CreditsTotal: 4000, Disabled: true},
		},
		CreditEntries: []credithist.Entry{{Time: now.Add(-time.Hour), Delta: 200}},
		MaxInFlight:   8, MaxInFlightGlobal: 2,
	})
	// 日耗取最近 24h；7 天毛需求 8400，余额 3000，计划补给 700，缺口 4700。
	// 有效账号总额度 P25=(1000+2000)/2=1500，因此补 4 个。
	if got := *r.Forecast.Horizons["7"].Planned.AdditionalAccounts; got != 4 {
		t.Fatalf("additional accounts=%d want 4", got)
	}
	if r.Pool.AccountsEnabled != 3 || r.Pool.CreditsRemaining != 3000 || r.Pool.NewAccountCreditUnitP25 != 1500 {
		t.Fatalf("unexpected pool report: %+v", r.Pool)
	}
	if r.Pool.InFlightPoolLimitEstimate == nil || *r.Pool.InFlightPoolLimitEstimate != 18 {
		t.Fatalf("realm-aware pool limit=%v want 18", r.Pool.InFlightPoolLimitEstimate)
	}
	if r.Usage.PositiveReplenishmentAssumedPerDay != 100 {
		t.Fatalf("assumed replenishment=%v", r.Usage.PositiveReplenishmentAssumedPerDay)
	}
}

func TestAnalyzeExpiryAndHiddenModel(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	events := make([]reqlog.Event, 20)
	for i := range events {
		events[i] = reqlog.Event{Time: now.Add(-time.Hour), Model: "cn:kimi-k3-1", Status: 400, DurationMs: 100}
	}
	r := Analyze(Input{
		Now: now, ForecastDays: 3, RecentHours: 24, Last24h: snapshot(100, 20), Previous24h: snapshot(100, 20),
		Accounts: []pool.Status{{Credits: 1000, CreditsTotal: 1000, CreditsExpiryBuckets: []pool.ExpiryBucket{{ExpireAt: now.Add(48 * time.Hour), Remain: 400}}}},
		Events:   events, HiddenModels: map[string]struct{}{"cn:kimi-k3-1": {}}, MaxInFlight: 8,
	})
	if r.Pool.CreditsExpiring["3"] != 400 || r.Forecast.Horizons["3"].Planned.CreditsUsableAfterExpiry != 600 {
		t.Fatalf("expiry not deducted: %+v", r.Forecast.Horizons["3"].Planned)
	}
	for _, rec := range r.Recommendations {
		if rec.Category == "model" && rec.Priority != "P2" {
			t.Fatalf("hidden model must not be a failure recommendation: %+v", rec)
		}
	}
}

func TestAnalyzeEmptyDataDoesNotInventAccountCount(t *testing.T) {
	r := Analyze(Input{Now: time.Now(), ForecastDays: 7, RecentHours: 24})
	if r.Forecast.Horizons["7"].Planned.AdditionalAccounts != nil {
		t.Fatal("missing account baseline must return null, not zero")
	}
	if r.Traffic.Requests != 0 || r.Confidence != "low" {
		t.Fatalf("unexpected empty report: %+v", r)
	}
}

func TestAnalyzeRejectsMissingCreditSamples(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	r := Analyze(Input{
		Now: now, ForecastDays: 7,
		Last24h:     usage.Snapshot{Totals: usage.Agg{Requests: 100}},
		Previous24h: usage.Snapshot{Totals: usage.Agg{Requests: 100}},
		Accounts:    []pool.Status{{Credits: 1000, CreditsTotal: 1000}}, MaxInFlight: 8,
	})
	if r.Usage.CreditForecastAvailable || r.Forecast.Horizons["7"].Planned.AdditionalAccounts != nil || r.Forecast.RunwayDays != nil {
		t.Fatalf("missing credit samples produced a deterministic forecast: %+v", r)
	}
	found := false
	for _, rec := range r.Recommendations {
		found = found || rec.Category == "data" && rec.Priority == "P1"
	}
	if !found {
		t.Fatal("missing credit samples must produce a data-quality recommendation")
	}
}

func TestAnalyzeExtrapolatesPartialCreditSamples(t *testing.T) {
	r := Analyze(Input{
		Now: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC), ForecastDays: 1,
		Last24h:     usage.Snapshot{Totals: usage.Agg{Requests: 100, CreditSamples: 50, Credits: 500}},
		Previous24h: snapshot(500, 100),
		Accounts:    []pool.Status{{Credits: 1000, CreditsTotal: 1000}}, MaxInFlight: 8,
	})
	if !r.Usage.CreditForecastAvailable || r.Usage.DailyCreditsForecast != 1000 ||
		r.Usage.CreditSampleCoveragePct == nil || *r.Usage.CreditSampleCoveragePct != 50 || r.Confidence != "low" {
		t.Fatalf("partial sample extrapolation/coverage incorrect: %+v", r.Usage)
	}
	if got := *r.Forecast.Horizons["1"].Planned.AdditionalAccounts; got != 1 {
		t.Fatalf("additional accounts=%d want 1 after extrapolation", got)
	}
}

func TestCreditsExpiringWithinCustomHorizonsAndLegacy(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	account := pool.Status{Credits: 1000, CreditsTotal: 1000, CreditsExpiryBuckets: []pool.ExpiryBucket{
		{ExpireAt: now.Add(48 * time.Hour), Remain: 400},
		{ExpireAt: now.Add(20 * 24 * time.Hour), Remain: 300},
	}}
	for _, tc := range []struct {
		days int
		want int64
	}{{1, 0}, {2, 400}, {4, 400}, {8, 400}, {14, 400}, {29, 700}, {30, 700}} {
		if got := creditsExpiringWithin([]pool.Status{account}, now, tc.days); got != tc.want {
			t.Errorf("days=%d expiry=%d want %d", tc.days, got, tc.want)
		}
	}
	legacy := pool.Status{CreditsEarliestExpiry: now.Add(48 * time.Hour), CreditsEarliestRemaining: 400}
	if got := creditsExpiringWithin([]pool.Status{legacy}, now, 3); got != 400 {
		t.Fatalf("legacy expiry=%d want 400", got)
	}
	legacy.CreditsExpiryBuckets = []pool.ExpiryBucket{{ExpireAt: now.Add(48 * time.Hour), Remain: 100}}
	if got := creditsExpiringWithin([]pool.Status{legacy}, now, 3); got != 100 {
		t.Fatalf("buckets and legacy were double counted: %d", got)
	}
}

func TestAnalyzeRunwayUsesExpiryAndHandlesNonPositiveNetBurn(t *testing.T) {
	now := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	in := Input{
		Now: now, ForecastDays: 7, Last24h: snapshot(100, 100), Previous24h: snapshot(100, 100),
		Accounts:    []pool.Status{{Credits: 1000, CreditsTotal: 1000, CreditsExpiryBuckets: []pool.ExpiryBucket{{ExpireAt: now.Add(24 * time.Hour), Remain: 900}}}},
		MaxInFlight: 8,
	}
	r := Analyze(in)
	if r.Forecast.RunwayDays == nil || *r.Forecast.RunwayDays != 1 {
		t.Fatalf("expiry-aware runway=%v want 1 day", r.Forecast.RunwayDays)
	}
	in.Accounts[0].CreditsExpiryBuckets[0].ExpireAt = now.Add(6 * 24 * time.Hour)
	r = Analyze(in)
	if r.Forecast.RunwayDays == nil || *r.Forecast.RunwayDays != 6 {
		t.Fatalf("consumed credits were expired twice: runway=%v want 6 days", r.Forecast.RunwayDays)
	}
	in.CreditEntries = []credithist.Entry{{Time: now.Add(-time.Hour), Delta: 1000}}
	r = Analyze(in)
	if r.Forecast.RunwayDays != nil {
		t.Fatalf("non-positive net burn must not produce finite runway: %v", *r.Forecast.RunwayDays)
	}
}

func TestHiddenModelMatching(t *testing.T) {
	both := map[string]struct{}{"cn:kimi-k3-1": {}, "global:kimi-k3-1": {}}
	for _, model := range []string{"kimi-k3-1", "cn:kimi-k3-1", "global:kimi-k3-1"} {
		if !isHiddenModel(model, both) {
			t.Errorf("%s should be hidden", model)
		}
	}
	if isHiddenModel("kimi-k3.1-pro", both) {
		t.Fatal("unrelated model must not be hidden")
	}
	cnOnly := map[string]struct{}{"cn:kimi-k3-1": {}}
	if !isHiddenModel("kimi-k3-1", cnOnly) || !isHiddenModel("cn:kimi-k3-1", cnOnly) || isHiddenModel("global:kimi-k3-1", cnOnly) {
		t.Fatal("domain-limited hidden semantics regressed")
	}
}
