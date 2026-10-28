// taskcatalog.go 任务目录：记录「上游下发过哪些任务码」。
//
// 为什么需要它——上游会随时上新增任务（典型如 expert_use_<slug>「体验某专家」、
// 一波活动期的 Sequential_Tasks_N）。靠 autoActions 硬编码表，新任务永远慢一拍：
// 既进不了待办队列，也没有「这是新任务」的信号，只能靠人盯着成长中心页面看。
//
// 本文件补的是「发现」这一环：每轮扫描/清扫把当前任务列表喂进来，
//   - 第一次出现的码 = 新任务 → 落日志、打标（前端展示「新」）、并在有自动
//     动作时立刻排一轮执行（发现 → 接受 → 完成，不必等每日 1 点的成长队列）；
//   - 目录持久化（默认与 state 同目录 task_catalog.json），重启不丢，避免
//     「每次重启都把所有任务当新任务重跑一遍」。
//
// 只增不删：任务下线后条目保留，便于回查「这个码什么时候出现过、奖励多少」。
// 空路径 = 纯内存（测试或未接线时），不产生任何文件 IO。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// CatalogEntry 单个任务码的目录条目。
type CatalogEntry struct {
	Code      string    `json:"code"`
	Title     string    `json:"title,omitempty"`
	Credit    int64     `json:"credit,omitempty"`
	Energy    int64     `json:"energy,omitempty"`
	Target    int64     `json:"target,omitempty"`
	Auto      string    `json:"auto,omitempty"` // 已具备的自动动作说明；空 = 未适配
	// Source 任务码来自哪套口径：v2（默认/PC）、mp（小程序）、legacy（v1 关卡）。
	// 三套是**不同资源**：v1 只有进度语义，v2/mp 才有 accept/claim。混在一张
	// 目录里必须能区分，否则「未适配」会被误读成漏做动作。
	Source    string    `json:"source,omitempty"`
	// Level v1 关卡的等级名（「养虾尝试」…「控虾大神」），v2/mp 恒空。
	Level     string    `json:"level,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Seen      int       `json:"seen"` // 出现在列表中的轮次数
}

// taskCatalog 任务码目录（进程内单例，Panel.catalog() 惰性构建）。
type taskCatalog struct {
	mu      sync.Mutex
	path    string
	entries map[string]*CatalogEntry
	dirty   bool
}

// newTaskCatalog 构建目录；path 非空时尝试载入既有内容（损坏只记日志，不阻塞）。
func newTaskCatalog(path string) *taskCatalog {
	c := &taskCatalog{path: path, entries: map[string]*CatalogEntry{}}
	if path == "" {
		return c
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("panel: 任务目录读取失败（按空目录继续）: %v", err)
		}
		return c
	}
	var file struct {
		Entries []CatalogEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		log.Printf("panel: 任务目录解析失败（按空目录继续）: %v", err)
		return c
	}
	for i := range file.Entries {
		e := file.Entries[i]
		if e.Code == "" {
			continue
		}
		c.entries[e.Code] = &e
	}
	return c
}

// observe 记录一次任务出现；返回该码是否为**首次**出现（即新任务）。
// 元数据（标题/奖励/动作说明）每次刷新，进度类字段不落目录。
func (c *taskCatalog) observe(t upstream.Task) bool {
	code := strings.TrimSpace(t.TaskCode)
	if code == "" {
		return false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[code]
	if !ok {
		e = &CatalogEntry{Code: code, FirstSeen: now, Source: sourceV2}
		c.entries[code] = e
	}
	// 老目录（升级前落盘）没有 source 字段：v2 是历史唯一来源，直接补默认。
	if e.Source == "" {
		e.Source = sourceV2
	}
	if t.Title != "" {
		e.Title = t.Title
	}
	e.Credit, e.Energy = t.Credit, t.Energy
	if t.Target > 0 {
		e.Target = t.Target
	}
	// 动作说明随注册表变化（新适配上线后旧条目自动补上），空了就清掉。
	if act := actionForTask(t); act != nil {
		e.Auto = act.Desc
	} else {
		e.Auto = ""
	}
	e.LastSeen = now
	e.Seen++
	c.dirty = true
	return !ok
}

// 任务码来源标记（见 CatalogEntry.Source）。
const (
	sourceV2     = "v2"     // /v2/activity/growth/tasks（PC 默认口径）
	sourceMP     = "mp"     // 同上 + X-Client-Platform: miniprogram
	sourceLegacy = "legacy" // /activity/growth/tasks（v1「养虾」关卡，无领奖语义）
)

// legacyAutoNote v1 关卡的固定动作说明。
//
// 不能留空——空 = 「未适配（需人工登记动作）」，而这 5 个码**本来就没有动作可做**：
// 响应无 reward_credit/reward_energy，web 与 chat 两个域的 claim 均返回
// 400 task not completed（2026-10-28 两账号实测）。写成明确文案，运维才不会被
// 目录里的「未适配」误导去补动作。
const legacyAutoNote = "v1 关卡：仅进度/等级展示，无 reward 字段且 claim 恒 400，不参与领奖"

// observeLegacy 记录一条 v1 关卡，返回是否首次出现。
// 与 observe 分开是因为字段集不同（无 reward/progress/accept），强行复用 Task
// 会让「reward 缺失」看起来像解析异常。
func (c *taskCatalog) observeLegacy(t upstream.LegacyTask) bool {
	code := strings.TrimSpace(t.Code)
	if code == "" {
		return false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[code]
	if !ok {
		e = &CatalogEntry{Code: code, FirstSeen: now, Source: sourceLegacy}
		c.entries[code] = e
	}
	if e.Source == "" {
		e.Source = sourceLegacy
	}
	if t.Title != "" {
		e.Title = t.Title
	}
	if t.LevelName != "" {
		e.Level = t.LevelName
	}
	e.Auto = legacyAutoNote
	e.LastSeen = now
	e.Seen++
	c.dirty = true
	return !ok
}

// total 已登记的任务码数量。0 = 目录尚未建立（升级/新机器的首轮）。
func (c *taskCatalog) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// snapshot 目录快照（按首次出现时间倒序：新的在前，便于前端/接口直接展示）。
func (c *taskCatalog) snapshot() []CatalogEntry {
	c.mu.Lock()
	out := make([]CatalogEntry, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, *e)
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.After(out[j].FirstSeen)
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// flush 落盘（原子写：tmp + rename）。无变更 / 无路径直接返回。
// 失败只记日志：目录是辅助观测，不该让清扫整轮失败。
func (c *taskCatalog) flush() error {
	c.mu.Lock()
	if !c.dirty || c.path == "" {
		c.mu.Unlock()
		return nil
	}
	payload := struct {
		Entries []CatalogEntry `json:"entries"`
	}{}
	for _, e := range c.entries {
		payload.Entries = append(payload.Entries, *e)
	}
	c.dirty = false
	path := c.path
	c.mu.Unlock()

	sort.Slice(payload.Entries, func(i, j int) bool {
		return payload.Entries[i].FirstSeen.Before(payload.Entries[j].FirstSeen)
	})
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

// catalog 返回面板的任务目录（首次调用时按配置路径载入）。
func (p *Panel) catalog() *taskCatalog {
	p.catOnce.Do(func() { p.cat = newTaskCatalog(p.cfg.CatalogFile) })
	return p.cat
}

// observeTasks 把一批任务喂进目录，返回其中**首次出现**的任务（新任务）。
func (p *Panel) observeTasks(tasks []upstream.Task) []upstream.Task {
	cat := p.catalog()
	var fresh []upstream.Task
	for _, t := range tasks {
		if cat.observe(t) {
			fresh = append(fresh, t)
		}
	}
	return fresh
}

// newTaskLogged 进程内已详细播报过的任务码：首次部署（目录为空）时所有任务都算
// 「新」，多账号重复刷同一批码会把日志冲掉——每个码只播报一次，后续账号静默。
var newTaskLogged sync.Map

// logNewTasks 新任务落日志：带上奖励与「能不能自动完成」，让运维一眼看出该不该
// 补动作（未适配 = 需要在 autoActions 里登记或加族模式）。
func logNewTasks(uid string, fresh []upstream.Task) {
	for _, t := range fresh {
		if _, dup := newTaskLogged.LoadOrStore(t.TaskCode, struct{}{}); dup {
			continue
		}
		how := "未适配（需人工登记动作）"
		if act := actionForTask(t); act != nil {
			how = "可自动完成：" + act.Desc
		}
		log.Printf("panel: 发现新任务 uid=%s code=%s title=%q 奖励 %d 分 %d 能 target=%d → %s",
			uid, t.TaskCode, t.Title, t.Credit, t.Energy, t.Target, how)
	}
}

// tasksCatalog 任务目录查询（新任务在前）：面板「新任务」视图与排障用。
func (p *Panel) tasksCatalog(w http.ResponseWriter, r *http.Request) {
	entries := p.catalog().snapshot()
	newIn24h := 0
	for _, e := range entries {
		if time.Since(e.FirstSeen) <= 24*time.Hour {
			newIn24h++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   len(entries),
		"new_24h": newIn24h,
	})
}
