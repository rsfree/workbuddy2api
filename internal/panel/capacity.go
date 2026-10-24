package panel

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/capacity"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

const capacityCacheTTL = 5 * time.Minute

type capacityCacheEntry struct {
	report    capacity.Report
	createdAt time.Time
}

// capacityReport 提供服务内只读容量预测。结果缓存 5 分钟；refresh=1 强制重算。
// 完整归档扫描只在缓存未命中时发生，且多个并发请求串行合并，不影响模型请求热路径。
func (p *Panel) capacityReport(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil || p.cfg.Usage == nil || p.cfg.RequestLog == nil || p.cfg.CreditHistory == nil {
		writeErr(w, http.StatusNotImplemented, "capacity analysis requires pool, usage, request log and credit history")
		return
	}
	days, err := boundedQueryInt(r, "days", 7, 1, 30)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hours, err := boundedQueryInt(r, "hours", 24, 1, 168)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	key := fmt.Sprintf("%d/%d", days, hours)
	force := r.URL.Query().Get("refresh") == "1"
	requestedAt := time.Now()

	p.capacityMu.Lock()
	defer p.capacityMu.Unlock()
	now := time.Now()
	if p.capacityCache == nil {
		p.capacityCache = map[string]capacityCacheEntry{}
	}
	if cached, ok := p.capacityCache[key]; ok && now.Sub(cached.createdAt) < capacityCacheTTL {
		// 普通请求命中 TTL；排队期间已由另一请求完成强制刷新时直接复用，
		// 避免 N 个 refresh=1 串行执行 N 次全归档扫描。
		if !force || cached.createdAt.After(requestedAt) {
			report := cached.report
			report.Parameters.Cached = true
			writeJSON(w, http.StatusOK, report)
			return
		}
	}

	// 同一 now 贯穿所有窗口，避免边界桶在多次快照间漂移。
	last := p.cfg.Usage.SnapshotWindow(usage.Window{From: now.Add(-24 * time.Hour), To: now}, nil, nil)
	previous := p.cfg.Usage.SnapshotWindow(usage.Window{From: now.Add(-48 * time.Hour), To: now.Add(-24 * time.Hour)}, nil, nil)
	week := p.cfg.Usage.SnapshotWindow(usage.Window{From: now.Add(-7 * 24 * time.Hour), To: now}, nil, nil)
	events, err := p.cfg.RequestLog.ReadAllArchiveContext(r.Context(), reqlog.Filter{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read request archive: "+err.Error())
		return
	}
	cnLimit, globalLimit := p.cfg.Pool.InFlightLimits()
	hidden := map[string]struct{}{}
	if p.cfg.Live != nil {
		hidden = p.cfg.Live.Load().HiddenModels
	}
	archive := p.cfg.RequestLog.Snapshot().Archive
	report := capacity.Analyze(capacity.Input{
		Now: now, ForecastDays: days, RecentHours: hours,
		Last24h: last, Previous24h: previous, Last7d: week,
		Accounts: p.cfg.Pool.List(), CreditEntries: p.cfg.CreditHistory.Read(0), Events: events,
		HiddenModels: hidden, MaxInFlight: cnLimit, MaxInFlightGlobal: globalLimit,
		ArchiveBytes: archive.Bytes,
	})
	p.capacityCache[key] = capacityCacheEntry{report: report, createdAt: time.Now()}
	writeJSON(w, http.StatusOK, report)
}

func boundedQueryInt(r *http.Request, name string, fallback, min, max int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, min, max)
	}
	return n, nil
}
