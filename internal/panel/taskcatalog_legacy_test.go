package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestCatalogObserveLegacyMarksSourceAndLevel v1 关卡进目录必须带 source=legacy
// 与固定动作说明：这两个字段是「别把它当成漏配动作的 v2 任务」的唯一依据。
func TestCatalogObserveLegacyMarksSourceAndLevel(t *testing.T) {
	c := newTaskCatalog("")

	fresh := c.observeLegacy(upstream.LegacyTask{
		Code: "first_chat", Title: "完成一次对话", LevelName: "养虾尝试", Status: "done",
	})
	if !fresh {
		t.Fatal("首条应为新码")
	}
	if fresh := c.observeLegacy(upstream.LegacyTask{Code: "first_chat"}); fresh {
		t.Error("重复码不应报新")
	}

	entries := c.snapshot()
	if len(entries) != 1 {
		t.Fatalf("len(entries)=%d want 1", len(entries))
	}
	e := entries[0]
	if e.Source != sourceLegacy {
		t.Errorf("Source=%q want %q", e.Source, sourceLegacy)
	}
	if e.Level != "养虾尝试" {
		t.Errorf("Level=%q want 养虾尝试", e.Level)
	}
	if e.Auto != legacyAutoNote {
		t.Errorf("Auto=%q want legacyAutoNote", e.Auto)
	}
	if e.Credit != 0 || e.Energy != 0 {
		t.Errorf("v1 关卡不应有奖励：credit=%d energy=%d", e.Credit, e.Energy)
	}
	if e.Seen != 2 {
		t.Errorf("Seen=%d want 2（两次观察）", e.Seen)
	}
}

// TestCatalogObserveV2DefaultsSource v2 任务（含升级前落盘、无 source 的老目录）
// 必须回落 v2，否则前端会把历史条目全当成来源不明。
func TestCatalogObserveV2DefaultsSource(t *testing.T) {
	c := newTaskCatalog("")
	c.observe(upstream.Task{TaskCode: "chat_5", Title: "和 AI 聊天 5 次", Credit: 100, Target: 5})

	entries := c.snapshot()
	if len(entries) != 1 {
		t.Fatalf("len=%d want 1", len(entries))
	}
	if got := entries[0].Source; got != sourceV2 {
		t.Errorf("Source=%q want %q", got, sourceV2)
	}
}

// TestCatalogLegacyThenV2KeepsFirstSource 同一码若先以 v1 登记，再被 v2 观察到，
// 来源不应被 later-wins 覆盖——两套口径实测不重叠，真出现重叠说明上游合流了，
// 保留首次来源比静默改写更容易发现问题。
func TestCatalogLegacyThenV2KeepsFirstSource(t *testing.T) {
	c := newTaskCatalog("")
	c.observeLegacy(upstream.LegacyTask{Code: "dual", Title: "v1 视角"})
	c.observe(upstream.Task{TaskCode: "dual", Title: "v2 视角", Credit: 50})

	entries := c.snapshot()
	if len(entries) != 1 {
		t.Fatalf("len=%d want 1", len(entries))
	}
	e := entries[0]
	if e.Source != sourceLegacy {
		t.Errorf("Source=%q want 保留 %q", e.Source, sourceLegacy)
	}
	// 但奖励/标题这些 v2 才有的信息应当补上（v1 无 reward，不能把 0 写死）。
	if e.Credit != 50 {
		t.Errorf("Credit=%d want 50（v2 补齐）", e.Credit)
	}
	if e.Title != "v2 视角" {
		t.Errorf("Title=%q want v2 视角", e.Title)
	}
}
