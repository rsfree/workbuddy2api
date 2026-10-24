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
	return usage.Snapshot{Totals: usage.Agg{Credits: credits, Requests: requests}}
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
