package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestRunDiscoveredTasksOnlyTouchesCodes 新任务即时执行的「影响面」契约：
// 只跑传入的新码，同账号的其它待办一律不动——否则一次发现会把全量待办重跑一遍
// （真实对话有配额成本，活动期新任务一批批出现时尤其危险）。
func TestRunDiscoveredTasksOnlyTouchesCodes(t *testing.T) {
	oldGap, oldReport, oldPoll := mpActionGap, reportGap, claimPollAttempts
	mpActionGap, reportGap, claimPollAttempts = 0, 0, 1
	t.Cleanup(func() { mpActionGap, reportGap, claimPollAttempts = oldGap, oldReport, oldPoll })

	var mu sync.Mutex
	accepted := map[string]bool{}
	claimed := map[string]bool{}
	var chats atomic.Int32
	var claimedCodes []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			mu.Lock()
			tasks := make([]map[string]any, 0, 2)
			for _, code := range []string{"expert_use_a", "expert_use_b"} {
				cur, status := 0, "not_accepted"
				if claimed[code] {
					cur, status = 1, "claimed"
				} else if accepted[code] {
					cur, status = 1, "accepted"
				}
				title := "体验「专家甲」"
				if code == "expert_use_b" {
					title = "体验「专家乙」"
				}
				tasks = append(tasks, map[string]any{
					"task_code": code, "title": title, "target": 1, "current": cur,
					"accept_status": status, "reward_credit": 50, "reward_energy": 5,
				})
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{"tasks": tasks}})
		case strings.HasSuffix(r.URL.Path, "/accept"):
			var body struct {
				Codes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			for _, c := range body.Codes {
				accepted[c] = true
			}
			mu.Unlock()
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		case r.URL.Path == "/portal/operation-platform/market/expert/list":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"experts": []map[string]any{
					{"expert_id": "ex_a", "expert_type": "agent", "display_name_zh": "专家甲", "profession_zh": "甲"},
					{"expert_id": "ex_b", "expert_type": "agent", "display_name_zh": "专家乙", "profession_zh": "乙"},
				}},
			})
		case r.URL.Path == "/v2/chat/completions":
			chats.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"id\":\"0123456789abcdef0123456789abcdef\"}\n\n"))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/activity/growth/tasks/"), "/claim")
			mu.Lock()
			claimed[code] = true
			claimedCodes = append(claimedCodes, code)
			mu.Unlock()
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":50,"energy":5}}`))
		default:
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		}
	}))
	defer srv.Close()

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p := &Panel{cfg: Config{Pool: pl, Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}

	p.runDiscoveredTasks([]string{"expert_use_a"})

	mu.Lock()
	got := append([]string(nil), claimedCodes...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "expert_use_a" {
		t.Fatalf("领取的任务 = %v, want 仅 expert_use_a（同账号 expert_use_b 不该被跑）", got)
	}
	if n := chats.Load(); n != 1 {
		t.Errorf("真实对话次数 = %d, want 1（只跑新码，不碰其余待办）", n)
	}
}

// nameForTestCode 每个任务码对应不同专家名：让「上报了哪个专家」能反推回任务码，
// 假上游据此推进对应任务的进度（accept 本身不产生进度，与真实语义一致）。
func nameForTestCode(code string) string {
	switch code {
	case "expert_use_a":
		return "专家甲"
	case "expert_use_c":
		return "专家丙"
	case "expert_use_d":
		return "专家丁"
	}
	return "专家未知"
}

// TestDiscoveredRunGate 生产安全闸：
//  1. 目录为空（升级/新机器首轮）→ 只登记，不执行（否则一把跑掉全账号待办）；
//  2. 目录已建立后上游新增码 → 立即执行该码；
//  3. 成长排程被显式关闭 → 只接受不执行。
func TestDiscoveredRunGate(t *testing.T) {
	oldGap, oldReport, oldPoll := mpActionGap, reportGap, claimPollAttempts
	mpActionGap, reportGap, claimPollAttempts = 0, 0, 1
	t.Cleanup(func() { mpActionGap, reportGap, claimPollAttempts = oldGap, oldReport, oldPoll })

	var mu sync.Mutex
	codes := []string{"expert_use_a"} // 阶段二再模拟上游新增 c
	accepted := map[string]bool{}
	completed := map[string]bool{}
	claimed := map[string]bool{}
	var claimedCodes []string
	var chats atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			mu.Lock()
			tasks := make([]map[string]any, 0, len(codes))
			for _, code := range codes {
				cur, status := 0, "not_accepted"
				switch {
				case claimed[code]:
					cur, status = 1, "claimed"
				case completed[code]:
					cur, status = 1, "accepted"
				case accepted[code]:
					status = "accepted"
				}
				tasks = append(tasks, map[string]any{
					"task_code": code, "title": "体验「" + nameForTestCode(code) + "」",
					"target": 1, "current": cur, "accept_status": status,
					"reward_credit": 50, "reward_energy": 5,
				})
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{"tasks": tasks}})
		case strings.HasSuffix(r.URL.Path, "/accept"):
			var body struct {
				Codes []string `json:"task_codes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			for _, c := range body.Codes {
				accepted[c] = true
			}
			mu.Unlock()
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		case r.URL.Path == "/portal/operation-platform/market/expert/list":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"experts": []map[string]any{
					{"expert_id": "ex_a", "expert_type": "agent", "display_name_zh": "专家甲", "profession_zh": "甲"},
					{"expert_id": "ex_c", "expert_type": "agent", "display_name_zh": "专家丙", "profession_zh": "丙"},
					{"expert_id": "ex_d", "expert_type": "agent", "display_name_zh": "专家丁", "profession_zh": "丁"},
				}},
			})
		case r.URL.Path == "/v2/chat/completions":
			chats.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"id\":\"0123456789abcdef0123456789abcdef\"}\n\n"))
		case r.URL.Path == "/v2/report":
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			mu.Lock()
			for _, code := range codes {
				if strings.Contains(string(raw), nameForTestCode(code)) {
					completed[code] = true
				}
			}
			mu.Unlock()
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/activity/growth/tasks/"), "/claim")
			mu.Lock()
			claimed[code] = true
			claimedCodes = append(claimedCodes, code)
			mu.Unlock()
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":50,"energy":5}}`))
		default:
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		}
	}))
	defer srv.Close()

	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	p := &Panel{cfg: Config{Pool: pl, Scheduler: scheduler.New(scheduler.Config{}), Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}

	// 阶段一：目录为空 → 只登记不执行（accept 不产生进度，所以也不该有领奖）。
	first := p.RunTaskSweepOnce()
	if first.Discovered != 1 || first.NewAuto != 1 {
		t.Fatalf("首轮 discovered=%d newAuto=%d, want 1/1", first.Discovered, first.NewAuto)
	}
	time.Sleep(150 * time.Millisecond) // 若误触发，异步执行会在这段时间里发生
	if n := chats.Load(); n != 0 {
		t.Fatalf("首轮（目录初始化）不该执行任何真实对话，got %d 次", n)
	}
	if p.catalog().total() != 1 {
		t.Fatalf("目录条目 = %d, want 1", p.catalog().total())
	}

	// 阶段二：上游新增 expert_use_c → 应只执行它。
	mu.Lock()
	codes = append(codes, "expert_use_c")
	mu.Unlock()
	p.RunTaskSweepOnce()

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		got := append([]string(nil), claimedCodes...)
		mu.Unlock()
		if len(got) > 0 {
			if len(got) != 1 || got[0] != "expert_use_c" {
				t.Fatalf("领取 = %v, want 仅 expert_use_c（其余待办不该被跑）", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("新增码未在 3s 内被自动执行")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := chats.Load(); n != 1 {
		t.Errorf("真实对话次数 = %d, want 1（阶段一 0 次 + 阶段二 1 次）", n)
	}

	// 阶段三：成长排程关闭 → 新码不再触发执行。
	p.cfg.Scheduler = scheduler.New(scheduler.Config{GrowthDisabled: true})
	mu.Lock()
	codes = append(codes, "expert_use_d")
	mu.Unlock()
	p.RunTaskSweepOnce()
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	got := append([]string(nil), claimedCodes...)
	mu.Unlock()
	if len(got) != 1 {
		t.Errorf("成长排程关闭后不应执行新码，claimed = %v", got)
	}
}
