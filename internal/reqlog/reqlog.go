// Package reqlog 记录脱敏的请求级指标与可选 JSONL 归档。
//
// 内存指标有界保存最近 100 条并维护进程级计数；磁盘归档只写请求元数据，
// 不写提示词、响应正文、Authorization 或其它凭证。可选的调用来源（客户端 IP /
// User-Agent，见 Event.ClientIP/UserAgent）由 server 按配置开关决定是否填充。
// 归档队列满时丢弃并计数，不允许日志写盘阻塞模型请求。
package reqlog

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	recentLimit     = 100
	defaultFileMax  = int64(16 << 20)
	defaultQueue    = 1024
	defaultReadMax  = 1000
	archiveFileGlob = "requests-*.jsonl"
)

// NewRequestID 生成不含用户信息的本地请求 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

// Sticky 会话粘性落点取值（Event.Sticky）。
const (
	StickyHit  = "hit"  // 命中绑定号
	StickyBusy = "busy" // 绑定号满载，本次溢出（绑定保留）
	StickyMiss = "miss" // 绑定号持久不可用，已解绑重分配
	StickyNew  = "new"  // 会话首次出现，本次分配
)

// Affinity 前缀亲和落点取值（Event.Affinity）。
//
// 与会话粘性分开统计，因为两者回答的是不同问题：
//   - sticky 回答「同一会话有没有换号」；
//   - affinity 回答「共享同一段 system+tools 的多个会话，有没有被撮合到同一个号」。
//
// 把两者混进一个比例会让诊断失真：一个纯新建会话很多的时段，sticky 全是 new、
// affinity 却可能大面积 hit，混在一起看不出究竟哪条机制在起作用。
const (
	AffinityHit  = "hit"  // 用亲和号服务（前缀缓存应命中）
	AffinityMiss = "miss" // 有亲和但本次没用上（满载溢出 / 竞态失效）→ 前缀被带离
	AffinityNew  = "new"  // 此前无亲和记录，本次成功后建立
)

const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// Config 归档参数。Enabled=false 时仍保留内存指标。
type Config struct {
	Dir           string
	Enabled       bool
	RetentionDays int
	MaxBytes      int64
	FileMaxBytes  int64
	QueueSize     int
}

// Event 是一条脱敏请求记录。Account 只保存“昵称(uid8)”标签，不保存完整 UID。
//
// ClientIP / UserAgent 是**调用来源**：面板「运行日志」用它回答"这条请求是谁打进来的"。
// 二者由 server 侧按 logging.request_client_info 开关决定是否填充（关掉即保持空串，
// 归档里不会出现来源字段）——来源信息比 token 计数敏感，运营可自行决定是否落盘。
// 仍然不写提示词、响应正文、Authorization 或其它凭证。
type Event struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id"`
	Path             string    `json:"path"`
	Account          string    `json:"account,omitempty"`
	Model            string    `json:"model,omitempty"`
	Status           int       `json:"status"`
	OK               bool      `json:"ok"`
	Outcome          string    `json:"outcome"`
	DurationMs       int64     `json:"duration_ms"`
	TTFBMs           int64     `json:"ttfb_ms,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	PromptTokens     int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens int64     `json:"completion_tokens,omitempty"`
	TotalTokens      int64     `json:"total_tokens,omitempty"`
	Credit           float64   `json:"credit,omitempty"`
	HasCredit        bool      `json:"credit_known"`
	// CacheHitTokens / CacheMissTokens 上游前缀缓存命中/未命中 token（issue #92）。
	// 上游未回该维度时两者皆零值省略；hit=0 + miss>0 即整段未命中。
	CacheHitTokens  int64  `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int64  `json:"cache_miss_tokens,omitempty"`
	ClientIP        string `json:"client_ip,omitempty"`
	UserAgent       string `json:"user_agent,omitempty"`
	// Egress 本次请求**实际走出去**的出口标签（出口隔离的落地证据）。
	// 与 ClientIP 是相反方向：ClientIP 是谁调进来，Egress 是从哪个出口出去。
	// 取值：local 模式是绑定的源地址（"154.40.45.140"）；代理模式是
	// "proxy:<host>"（带用户名时 "proxy:<host>/<user>"）；未启用出口策略或解析
	// 失败回落直连是 "direct"。看到 direct 就说明这一请求没走专属出口——排查
	// 「代理配了但没生效」的第一现场。旧归档行没有该字段（omitempty）。
	Egress string `json:"egress,omitempty"`
	// Sticky 会话粘性路由本轮的落点（hit / busy / miss / new；空 = 无会话键）。
	// 它回答「这一跳的账号是不是会话原来绑定的那个」——换号会打掉账号级上游
	// 前缀缓存，所以这是缓存命中的**先行指标**：
	//   hit  = 命中绑定号（应命缓存）
	//   busy = 绑定号在途满载，本次溢出到别的号（绑定保留，压力过后回热）
	//   miss = 绑定号持久不可用（冷却/封禁/该模型限额），已解绑并重新分配
	//   new  = 该会话首次出现，本次分配并绑定
	Sticky string `json:"sticky,omitempty"`
	// Affinity 前缀亲和落点（hit / miss / new；空 = 该请求未参与前缀亲和）。
	// 只在「会话尚无绑定」时才有值——会话强粘性命中时亲和根本不参与决策，
	// 那种情况记空串，避免把两条机制的指标混在一起。
	Affinity string `json:"affinity,omitempty"`
}

// Filter 用于从归档中筛选最近记录。字符串字段一律「包含」匹配（大小写不敏感），
// 便于面板用一段 IP 前缀或 UA 片段捞请求；From/To 是闭区间（零值 = 该侧不设界），
// 供「今天 / 近 7 天 / 自定义区间」这类时间查询使用。
type Filter struct {
	Outcome   string
	Account   string
	Model     string
	ClientIP  string
	UserAgent string
	Egress    string
	From      time.Time
	To        time.Time
}

// ArchiveStats 归档存储状态。
type ArchiveStats struct {
	Enabled       bool   `json:"enabled"`
	Dir           string `json:"dir,omitempty"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	DroppedWrites uint64 `json:"dropped_writes"`
	LastError     string `json:"last_error,omitempty"`
}

// CacheMetrics 上游前缀缓存与粘性路由的累计指标（自进程启动）。
//
// 两个比例的分母刻意不同：
//   - CacheHitRate 用 upstream 回报的 hit+miss token 做分母（缺该维度的请求不计入）；
//   - StickyHitRate 用「粘性被判定的请求数」做分母（hit+busy+miss+new），
//     无会话键的请求不计入。
//
// 之所以把两者并列：换号是缓存冷的主因（账号级前缀缓存），先看 StickyHitRate
// 掉不掉，再看 CacheHitRate 跟不跟，能一眼分出「路由搬了号」还是「上游没缓存」。
type CacheMetrics struct {
	CacheHitTokens  int64   `json:"cache_hit_tokens"`
	CacheMissTokens int64   `json:"cache_miss_tokens"`
	CacheHitRate    float64 `json:"cache_hit_rate"`
	// Sticky 计数与命中率：hit/busy/miss/new 之和为分母。
	StickyHit     int64   `json:"sticky_hit"`
	StickyBusy    int64   `json:"sticky_busy"`
	StickyMiss    int64   `json:"sticky_miss"`
	StickyNew     int64   `json:"sticky_new"`
	StickyHitRate float64 `json:"sticky_hit_rate"`
	// Affinity 计数与命中率：hit/miss/new 之和为分母（只统计会话尚未绑定的请求）。
	// 这是**前缀级**聚合——它上升意味着同一段 system 提示词不再在每个号上重复冷启动。
	AffinityHit     int64   `json:"affinity_hit"`
	AffinityMiss    int64   `json:"affinity_miss"`
	AffinityNew     int64   `json:"affinity_new"`
	AffinityHitRate float64 `json:"affinity_hit_rate"`
}

// Snapshot 一次面板读取的完整指标快照。
type Snapshot struct {
	StartedAt       time.Time `json:"started_at"`
	Completed       int64     `json:"completed"`
	InFlight        int64     `json:"in_flight"`
	Succeeded       int64     `json:"succeeded"`
	Failed          int64     `json:"failed"`
	SuccessRate     float64   `json:"success_rate"`
	HTTPSuccessRate float64   `json:"http_success_rate"`
	AvgDurationMs   float64   `json:"avg_duration_ms"`
	// 前缀缓存与粘性：进程内累计（自启动起），供面板「缓存命中率 / 换号率」展示。
	// 与 Recent 不同，这里不受 recentLimit 影响——它是整段运行期的比例，
	// 用来回答「这次优化到底有没有让缓存热起来」，短窗口抖动不会把它带偏。
	Cache   CacheMetrics `json:"cache"`
	Recent  []Event      `json:"recent"`
	Archive ArchiveStats `json:"archive"`
}

// Recorder 并发安全的有界请求指标与归档记录器。
type Recorder struct {
	mu          sync.Mutex
	started     time.Time
	inFlight    int64
	completed   int64
	succeeded   int64
	httpSuccess int64
	durationSum int64
	// 缓存与粘性累计（见 CacheMetrics）。
	cacheHitTokens  int64
	cacheMissTokens int64
	stickyHit       int64
	stickyBusy      int64
	stickyMiss      int64
	stickyNew       int64
	affinityHit     int64
	affinityMiss    int64
	affinityNew     int64
	recent          []Event
	archive         *archiveWriter
}

// New 创建记录器；Dir 为空或 Enabled=false 时只启用内存指标。
func New(cfg Config) *Recorder {
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	r := &Recorder{started: time.Now(), recent: make([]Event, 0, recentLimit)}
	r.archive = newArchiveWriter(cfg)
	return r
}

// Begin 标记一个请求进入处理。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记录一个请求完成事件并写入归档队列。
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		if e.Status == 200 && e.OK {
			e.Outcome = OutcomeSuccess
		} else {
			e.Outcome = OutcomeHTTPError
		}
	}
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.completed++
	if e.OK {
		r.succeeded++
	}
	if e.Status >= 200 && e.Status < 300 {
		r.httpSuccess++
	}
	r.durationSum += e.DurationMs
	// hit+miss 才是「上游回报了缓存维度」的请求；两者皆 0 的（上游没给该维度、
	// 或非 chat 路径）不进分母，避免把无观测请求算成「命中 0%」。
	if e.CacheHitTokens > 0 || e.CacheMissTokens > 0 {
		r.cacheHitTokens += e.CacheHitTokens
		r.cacheMissTokens += e.CacheMissTokens
	}
	switch e.Sticky {
	case StickyHit:
		r.stickyHit++
	case StickyBusy:
		r.stickyBusy++
	case StickyMiss:
		r.stickyMiss++
	case StickyNew:
		r.stickyNew++
	}
	switch e.Affinity {
	case AffinityHit:
		r.affinityHit++
	case AffinityMiss:
		r.affinityMiss++
	case AffinityNew:
		r.affinityNew++
	}
	r.recent = append([]Event{e}, r.recent...)
	if len(r.recent) > recentLimit {
		r.recent = r.recent[:recentLimit]
	}
	r.mu.Unlock()
	if r.archive != nil {
		r.archive.enqueue(e)
	}
}

// Snapshot 返回进程内指标和归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	s := Snapshot{
		StartedAt: r.started,
		Completed: r.completed,
		InFlight:  r.inFlight,
		Succeeded: r.succeeded,
		Recent:    append([]Event(nil), r.recent...),
	}
	if r.completed > 0 {
		s.Failed = r.completed - r.succeeded
		s.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		s.HTTPSuccessRate = round1(float64(r.httpSuccess) / float64(r.completed) * 100)
		s.AvgDurationMs = float64(r.durationSum) / float64(r.completed)
	}
	s.Cache = CacheMetrics{
		CacheHitTokens:  r.cacheHitTokens,
		CacheMissTokens: r.cacheMissTokens,
		StickyHit:       r.stickyHit,
		StickyBusy:      r.stickyBusy,
		StickyMiss:      r.stickyMiss,
		StickyNew:       r.stickyNew,
		AffinityHit:     r.affinityHit,
		AffinityMiss:    r.affinityMiss,
		AffinityNew:     r.affinityNew,
	}
	if total := r.cacheHitTokens + r.cacheMissTokens; total > 0 {
		s.Cache.CacheHitRate = round1(float64(r.cacheHitTokens) / float64(total) * 100)
	}
	if total := r.stickyHit + r.stickyBusy + r.stickyMiss + r.stickyNew; total > 0 {
		s.Cache.StickyHitRate = round1(float64(r.stickyHit) / float64(total) * 100)
	}
	if total := r.affinityHit + r.affinityMiss + r.affinityNew; total > 0 {
		s.Cache.AffinityHitRate = round1(float64(r.affinityHit) / float64(total) * 100)
	}
	r.mu.Unlock()
	if r.archive != nil {
		s.Archive = r.archive.stats()
	}
	return s
}

// ReadArchive 返回最近的归档事件（按时间倒序）。limit<=0 时回落 200，最大 1000。
func (r *Recorder) ReadArchive(limit int, filter Filter) ([]Event, error) {
	if r == nil || r.archive == nil {
		return nil, nil
	}
	return r.archive.read(limit, filter, false, context.Background())
}

// ReadAllArchive 为离线式聚合读取全部匹配事件，不受面板逐页 1000 条上限影响。
// 调用方应是带缓存的低频管理任务，不能放在模型请求热路径。
func (r *Recorder) ReadAllArchive(filter Filter) ([]Event, error) {
	if r == nil || r.archive == nil {
		return nil, nil
	}
	return r.ReadAllArchiveContext(context.Background(), filter)
}

// ReadAllArchiveContext 在读取前排空当前写入队列，并响应管理请求取消。
// 全量聚合不排序，避免容量分析承担与顺序无关的 O(N log N) 开销。
func (r *Recorder) ReadAllArchiveContext(ctx context.Context, filter Filter) ([]Event, error) {
	if r == nil || r.archive == nil {
		return nil, nil
	}
	if err := r.archive.flushSync(ctx); err != nil {
		return nil, err
	}
	return r.archive.read(0, filter, true, ctx)
}

// Close 刷盘并停止后台归档。
func (r *Recorder) Close() {
	if r == nil || r.archive == nil {
		return
	}
	r.archive.close()
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

type archiveWriter struct {
	cfg       Config
	ch        chan Event
	flushCh   chan chan struct{}
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
	lastErrMu sync.Mutex
	lastErr   string

	file *os.File
	buf  *bufio.Writer
	path string
	day  string
	size int64
}

func newArchiveWriter(cfg Config) *archiveWriter {
	if !cfg.Enabled || strings.TrimSpace(cfg.Dir) == "" {
		return &archiveWriter{cfg: cfg}
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 7
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 100 << 20
	}
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	w := &archiveWriter{
		cfg:     cfg,
		ch:      make(chan Event, cfg.QueueSize),
		flushCh: make(chan chan struct{}),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		w.setErr(err)
		w.cfg.Enabled = false
		close(w.done)
		return w
	}
	go w.run()
	return w
}

func (w *archiveWriter) enabled() bool {
	return w != nil && w.cfg.Enabled && w.cfg.Dir != "" && w.done != nil
}

func (w *archiveWriter) enqueue(e Event) {
	if !w.enabled() {
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
	}
}

func (w *archiveWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case e := <-w.ch:
			w.writeEvent(e)
		case ack := <-w.flushCh:
			w.drainAndFlush()
			close(ack)
		case <-ticker.C:
			w.flush()
			w.prune()
		case <-w.stop:
			for {
				select {
				case e := <-w.ch:
					w.writeEvent(e)
				default:
					w.flush()
					w.closeFile()
					return
				}
			}
		}
	}
}

func (w *archiveWriter) drainAndFlush() {
	for {
		select {
		case e := <-w.ch:
			w.writeEvent(e)
		default:
			w.flush()
			return
		}
	}
}

func (w *archiveWriter) flushSync(ctx context.Context) error {
	if !w.enabled() {
		return nil
	}
	ack := make(chan struct{})
	select {
	case w.flushCh <- ack:
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return nil
	}
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-w.done:
		return nil
	}
}

func (w *archiveWriter) writeEvent(e Event) {
	raw, err := json.Marshal(e)
	if err != nil {
		w.setErr(err)
		return
	}
	now := e.Time
	if now.IsZero() {
		now = time.Now()
	}
	day := now.Format("2006-01-02")
	if w.file == nil || w.day != day {
		if err := w.openFile(now, false); err != nil {
			w.setErr(err)
			return
		}
	}
	if w.size > 0 && w.size+int64(len(raw))+1 > w.cfg.FileMaxBytes {
		if err := w.openFile(now, true); err != nil {
			w.setErr(err)
			return
		}
	}
	if _, err := w.buf.Write(raw); err != nil {
		w.setErr(err)
		return
	}
	if err := w.buf.WriteByte('\n'); err != nil {
		w.setErr(err)
		return
	}
	w.size += int64(len(raw)) + 1
}

func (w *archiveWriter) openFile(now time.Time, rotate bool) error {
	w.closeFile()
	day := now.Format("2006-01-02")
	if err := os.MkdirAll(w.cfg.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(w.cfg.Dir, "requests-"+day+".jsonl")
	if rotate {
		path = nextArchivePath(w.cfg.Dir, day)
	} else if latest := latestArchiveForDay(w.cfg.Dir, day); latest != "" {
		path = filepath.Join(w.cfg.Dir, latest)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.buf = bufio.NewWriterSize(f, 64<<10)
	w.path = path
	w.day = day
	w.size = info.Size()
	return nil
}

func latestArchiveForDay(dir, day string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	prefix := "requests-" + day
	best := ""
	bestIndex := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		idx := 0
		if name[len(prefix):len(name)-len(".jsonl")] != "" {
			part := strings.TrimSuffix(strings.TrimPrefix(name[len(prefix):], "."), ".jsonl")
			n, err := strconv.Atoi(part)
			if err != nil {
				continue
			}
			idx = n
		}
		if idx > bestIndex {
			bestIndex = idx
			best = name
		}
	}
	if best == "" {
		return ""
	}
	return best
}

func nextArchivePath(dir, day string) string {
	base := filepath.Join(dir, "requests-"+day)
	path := base + ".jsonl"
	for i := 1; fileExists(path); i++ {
		path = fmt.Sprintf("%s.%d.jsonl", base, i)
	}
	return path
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func (w *archiveWriter) flush() {
	if w.buf == nil {
		return
	}
	if err := w.buf.Flush(); err != nil {
		w.setErr(err)
	}
}

func (w *archiveWriter) closeFile() {
	if w.buf != nil {
		_ = w.buf.Flush()
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = nil
	w.buf = nil
	w.path = ""
	w.day = ""
	w.size = 0
}

func (w *archiveWriter) prune() {
	if !w.enabled() {
		return
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		w.setErr(err)
		return
	}
	type item struct {
		path  string
		size  int64
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
	var total int64
	cutoff := time.Now().AddDate(0, 0, -w.cfg.RetentionDays)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(w.cfg.Dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: path, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].mtime.Equal(items[j].mtime) {
			return items[i].path < items[j].path
		}
		return items[i].mtime.Before(items[j].mtime)
	})
	for _, it := range items {
		if it.path != w.path && (it.mtime.Before(cutoff) || total > w.cfg.MaxBytes) {
			if err := os.Remove(it.path); err == nil {
				total -= it.size
			}
		}
	}
}

func (w *archiveWriter) read(limit int, filter Filter, all bool, ctx context.Context) ([]Event, error) {
	if !w.enabled() {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !all && limit <= 0 {
		limit = 200
	}
	if !all && limit > defaultReadMax {
		limit = defaultReadMax
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return nil, err
	}
	// 文件读取顺序无关紧要：结果一律按事件时间排序。不能依赖归档文件的 mtime 还原时间
	// 顺序——同一秒内连续轮转写出的多个文件 mtime 经常完全相同（Linux 文件时间戳粒度粗），
	// os.ReadDir 的字典序又会把装着最早事件的基准文件 requests-<day>.jsonl 排在
	// requests-<day>.N.jsonl 之后；目录被整体拷贝 / 恢复备份后 mtime 更不可信。
	var out []Event
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		rows, err := readFile(ctx, filepath.Join(w.cfg.Dir, name), filter)
		if err != nil {
			return out, err
		}
		out = append(out, rows...)
	}
	if !all {
		// 分页契约：按事件时间倒序返回最近 limit 条；全量聚合无需排序。
		sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
		if len(out) > limit {
			out = out[:limit]
		}
	}
	return out, nil
}

func readFile(ctx context.Context, path string, filter Filter) ([]Event, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		// ReadDir 与 Open 之间 prune 可能删除文件；跳过消失文件而非让管理请求 500。
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var out []Event
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil || !filter.match(e) {
			continue
		}
		out = append(out, e)
	}
	return out, scanner.Err()
}

func (f Filter) match(e Event) bool {
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	if !containsFold(e.Account, f.Account) {
		return false
	}
	if !containsFold(e.Model, f.Model) {
		return false
	}
	if !containsFold(e.ClientIP, f.ClientIP) {
		return false
	}
	if !containsFold(e.UserAgent, f.UserAgent) {
		return false
	}
	if !containsFold(e.Egress, f.Egress) {
		return false
	}
	if !f.From.IsZero() && e.Time.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && e.Time.After(f.To) {
		return false
	}
	return true
}

// containsFold 大小写不敏感的子串匹配；needle 为空视为命中（不筛该字段）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func (w *archiveWriter) stats() ArchiveStats {
	s := ArchiveStats{Enabled: w.enabled(), Dir: w.cfg.Dir, DroppedWrites: w.dropped.Load(), LastError: w.errString()}
	if !s.Enabled {
		return s
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		s.LastError = err.Error()
		return s
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		if info, err := entry.Info(); err == nil {
			s.Files++
			s.Bytes += info.Size()
		}
	}
	return s
}

func (w *archiveWriter) close() {
	if !w.enabled() {
		return
	}
	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.done
	})
}

func (w *archiveWriter) setErr(err error) {
	if err == nil {
		return
	}
	w.lastErrMu.Lock()
	w.lastErr = err.Error()
	w.lastErrMu.Unlock()
}

func (w *archiveWriter) errString() string {
	w.lastErrMu.Lock()
	defer w.lastErrMu.Unlock()
	return w.lastErr
}
