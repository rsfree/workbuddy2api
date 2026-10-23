package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestTaskSweepDiscoversNewTasks 清扫顺手做「发现」：首次出现的码计入 Discovered，
// 其中带自动动作的（族模式 expert_use_*）计入 NewAuto；第二轮不再重复发现。
// 同时验证 accept 覆盖到新任务（发现即接受，不用等面板点按钮）。
func TestTaskSweepDiscoversNewTasks(t *testing.T) {
	b := &sweepBackend{
		listFn: func(isMP bool) []map[string]any {
			if isMP {
				return nil
			}
			return []map[string]any{
				// 无自动动作的常规任务：只接受，不算可自动。
				sweepTask("invite_1", 1, 0, "not_accepted", nil),
				// 新增的「体验指定专家」族任务：可自动。
				sweepTask("expert_use_bowangwang", 1, 0, "not_accepted",
					map[string]any{"title": "体验「直播电商教练」"}),
			}
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})

	first := p.RunTaskSweepOnce()
	if first.Discovered != 2 {
		t.Errorf("首轮发现新任务 = %d, want 2", first.Discovered)
	}
	if first.NewAuto != 1 {
		t.Errorf("首轮可自动新任务 = %d, want 1（只有 expert_use_* 命中族模式）", first.NewAuto)
	}
	if first.Accepted != 2 {
		t.Errorf("首轮接受 = %d, want 2（新任务也要被接受）", first.Accepted)
	}
	if first.Errors != 0 {
		t.Errorf("首轮 errors = %d, want 0", first.Errors)
	}

	second := p.RunTaskSweepOnce()
	if second.Discovered != 0 || second.NewAuto != 0 {
		t.Errorf("次轮不应重复发现：discovered=%d newAuto=%d, want 0/0", second.Discovered, second.NewAuto)
	}

	// 目录：两条都在，族任务带动作说明。
	snap := p.catalog().snapshot()
	if len(snap) != 2 {
		t.Fatalf("目录条目 = %d, want 2", len(snap))
	}
	for _, e := range snap {
		switch e.Code {
		case "expert_use_bowangwang":
			if e.Auto == "" {
				t.Errorf("expert_use_* 应带动作说明，got %+v", e)
			}
			if e.Title != "体验「直播电商教练」" {
				t.Errorf("标题 = %q", e.Title)
			}
		case "invite_1":
			if e.Auto != "" {
				t.Errorf("invite_1 无自动动作，Auto 应为空，got %q", e.Auto)
			}
		default:
			t.Errorf("目录出现未知条目 %+v", e)
		}
	}
}

// TestTaskSweepAcceptFallbackIsolatesBadCode 整批 accept 被上游打回（新任务码上游
// 不认时最常见）→ 逐条重试，好码照常接受，不被一个坏码拖死。
func TestTaskSweepAcceptFallbackIsolatesBadCode(t *testing.T) {
	b := &sweepBackend{
		listFn: func(isMP bool) []map[string]any {
			if isMP {
				return nil
			}
			return []map[string]any{
				sweepTask("chat_5", 5, 0, "not_accepted", nil),
				sweepTask("bad_code_1", 1, 0, "not_accepted", nil),
			}
		},
		acceptFn: func(codes []string, isMP bool) (int, string) {
			// 批量（>1 条）直接 400；单条只有 bad_code_1 失败。
			if len(codes) > 1 {
				return 400, `{"code":400,"msg":"invalid task_code"}`
			}
			if codes[0] == "bad_code_1" {
				return 400, `{"code":400,"msg":"invalid task_code"}`
			}
			return 0, ""
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})
	st := p.RunTaskSweepOnce()

	if st.Accepted != 1 {
		t.Errorf("accepted = %d, want 1（chat_5 应被逐条重试救回来）", st.Accepted)
	}
	if st.Errors != 1 {
		t.Errorf("errors = %d, want 1（bad_code_1 单独失败计一次）", st.Errors)
	}
}

// TestTasksScanAllExposesAutoAndNew 扫描接口的对外契约：未领取未锁定的任务**全部**
// 列出（含未适配），逐条带 auto（能否自动完成）与 new（目录首次出现）标记。
// 这是前端「新」「未适配」标签的数据来源，不能退化成只列可自动化任务。
func TestTasksScanAllExposesAutoAndNew(t *testing.T) {
	b := &sweepBackend{
		listFn: func(isMP bool) []map[string]any {
			if isMP {
				return nil
			}
			return []map[string]any{
				sweepTask("expert_use_maidehao", 1, 0, "not_accepted",
					map[string]any{"title": "体验「中国电商运营专家」"}),
				sweepTask("invite_1", 1, 0, "not_accepted", nil),
				sweepTask("done_1", 1, 1, "claimed", nil), // 已领取：不出现在扫描里
			}
		},
	}
	p := newSweepPanel(t, b, &auth.Auth{UID: "u1", AccessToken: "at"})

	scan := func() (growth []scanTask, pending, auto, newCount int) {
		t.Helper()
		rec := httptest.NewRecorder()
		p.tasksScanAll(rec, httptest.NewRequest(http.MethodPost, "/panel/api/tasks/scan_all", nil))
		var resp struct {
			Accounts []struct {
				Growth []scanTask `json:"growth"`
			} `json:"accounts"`
			PendingCount int `json:"pending_count"`
			AutoCount    int `json:"auto_count"`
			NewCount     int `json:"new_count"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("scan_all 响应解析失败: %v（body=%s）", err, rec.Body.String())
		}
		for _, a := range resp.Accounts {
			growth = append(growth, a.Growth...)
		}
		return growth, resp.PendingCount, resp.AutoCount, resp.NewCount
	}

	growth, pending, auto, newCount := scan()
	if pending != 2 || len(growth) != 2 {
		t.Fatalf("pending=%d len=%d, want 2/2（已领取的 done_1 不列）", pending, len(growth))
	}
	if auto != 1 {
		t.Errorf("auto_count = %d, want 1（只有 expert_use_* 可自动）", auto)
	}
	if newCount != 2 {
		t.Errorf("首轮 new_count = %d, want 2", newCount)
	}
	for _, g := range growth {
		switch g.TaskCode {
		case "expert_use_maidehao":
			if !g.Auto || !g.New {
				t.Errorf("expert_use_maidehao: auto=%v new=%v, want true/true", g.Auto, g.New)
			}
			if g.Title != "体验「中国电商运营专家」" {
				t.Errorf("标题未透出: %q", g.Title)
			}
		case "invite_1":
			if g.Auto {
				t.Error("invite_1 无自动动作，auto 应为 false")
			}
		}
	}

	// 第二轮：目录已记录 → 不再是新任务。
	_, _, _, newCount2 := scan()
	if newCount2 != 0 {
		t.Errorf("次轮 new_count = %d, want 0", newCount2)
	}
}
