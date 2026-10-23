package panel

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestTaskCatalogObserveAndPersist 目录的核心语义：同一个码只在**首次**出现时
// 算新任务；落盘后重启不再重复报新（否则每次重启都会把所有任务当新任务重跑）。
func TestTaskCatalogObserveAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task_catalog.json")
	c := newTaskCatalog(path)

	chat5 := upstream.Task{TaskCode: "chat_5", Title: "和 AI 聊天 5 次", Credit: 100, Energy: 5, Target: 5}
	if !c.observe(chat5) {
		t.Fatal("首次出现的任务应判定为新任务")
	}
	if c.observe(chat5) {
		t.Fatal("同一任务第二次出现不应再算新任务")
	}
	expert := upstream.Task{
		TaskCode: "expert_use_bowangwang", Title: "体验「直播电商教练」",
		Credit: 50, Energy: 5, AcceptStatus: "not_accepted",
	}
	if !c.observe(expert) {
		t.Fatal("族模式任务首次出现应判定为新任务")
	}
	if err := c.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("目录文件未落盘: %v", err)
	}

	// 重启：重新载入后元数据（奖励/动作说明/出现轮次）保留。
	c2 := newTaskCatalog(path)
	snap := c2.snapshot()
	if len(snap) != 2 {
		t.Fatalf("目录条目 = %d, want 2", len(snap))
	}
	byCode := map[string]CatalogEntry{}
	for _, e := range snap {
		byCode[e.Code] = e
	}
	if got := byCode["expert_use_bowangwang"]; got.Auto == "" {
		t.Errorf("expert_use_* 条目应带族动作说明，got %+v", got)
	}
	if got := byCode["chat_5"]; got.Credit != 100 || got.Energy != 5 {
		t.Errorf("chat_5 奖励 = %d/%d, want 100/5", got.Credit, got.Energy)
	}
	if got := byCode["chat_5"]; got.Seen != 2 {
		t.Errorf("chat_5 出现轮次 = %d, want 2（落盘前观察过两次）", got.Seen)
	}
	// 重新观察同一批：都不再是新任务（重启不重置「新」标记）。
	if c2.observe(chat5) || c2.observe(expert) {
		t.Fatal("已落盘的任务重启后不应再算新任务")
	}
}

// TestTaskCatalogInMemory 空路径 = 纯内存：不写文件，也不因 flush 报错（测试与
// 未接线部署依赖这一条）。
func TestTaskCatalogInMemory(t *testing.T) {
	c := newTaskCatalog("")
	if !c.observe(upstream.Task{TaskCode: "weekly_x", Title: "签到"}) {
		t.Fatal("首次观察应为新任务")
	}
	if err := c.flush(); err != nil {
		t.Fatalf("空路径 flush 不应报错: %v", err)
	}
	if got := len(c.snapshot()); got != 1 {
		t.Fatalf("快照条目 = %d, want 1", got)
	}
}
