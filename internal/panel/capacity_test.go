package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/capacity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/credithist"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

func TestCapacityEndpointAndCache(t *testing.T) {
	u := usage.New("")
	u.Add(time.Now(), "cn", "u1", "glm", usage.Delta{Credit: 10, HasCredit: true}, true)
	r := reqlog.New(reqlog.Config{Enabled: true, Dir: t.TempDir(), RetentionDays: 7, MaxBytes: 1 << 20})
	r.Record(reqlog.Event{Time: time.Now(), Model: "glm", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess, DurationMs: 100})
	t.Cleanup(r.Close) // 不预先 Close：接口必须先刷盘，覆盖生产中的实时归档读取。
	p := New(Config{Pool: pool.New(""), Usage: u, RequestLog: r, CreditHistory: credithist.New("", 10)})

	get := func(path string) (int, capacity.Report) {
		t.Helper()
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var report capacity.Report
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
		}
		return rec.Code, report
	}
	code, first := get("/panel/api/capacity?days=3&hours=24")
	if code != http.StatusOK || first.Traffic.Requests != 1 || first.Parameters.ForecastDays != 3 || first.Parameters.Cached {
		t.Fatalf("first response code=%d report=%+v", code, first)
	}
	code, second := get("/panel/api/capacity?days=3&hours=24")
	if code != http.StatusOK || !second.Parameters.Cached || !second.GeneratedAt.Equal(first.GeneratedAt) {
		t.Fatalf("cache miss or changed cutoff: code=%d first=%v second=%v", code, first.GeneratedAt, second.GeneratedAt)
	}
	code, _ = get("/panel/api/capacity?days=31")
	if code != http.StatusBadRequest {
		t.Fatalf("invalid days code=%d want 400", code)
	}
}

func TestCapacityEndpointRequiresRecorders(t *testing.T) {
	p := New(Config{Pool: pool.New("")})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/capacity", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d want 501", rec.Code)
	}
}
