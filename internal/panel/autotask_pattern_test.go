package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestActionForTaskResolvesFamily 动作解析：精确表优先，族模式兜底（码形状 + 文案判据），
// 其余未知码保持「不可自动化」（不能瞎跑真实对话）。
func TestActionForTaskResolvesFamily(t *testing.T) {
	cases := []struct {
		name   string
		task   upstream.Task
		want   bool
		family bool // true = 族模式（需要任务上下文的 runTask）
	}{
		{name: "精确表", task: upstream.Task{TaskCode: "chat_5"}, want: true},
		{name: "精确表-expert_5", task: upstream.Task{TaskCode: "expert_5", Title: "召唤 5 次专家"}, want: true},
		{name: "码形状-slug", task: upstream.Task{TaskCode: "expert_use_bowangwang", Title: "体验「直播电商教练」"}, want: true, family: true},
		{name: "码形状-大小写", task: upstream.Task{TaskCode: "Expert_use_shangjia_xuanchuan", Title: "体验「商家宣传页与海报设计师」"}, want: true, family: true},
		{
			name: "文案判据-换码形状",
			task: upstream.Task{
				TaskCode: "WorkBuddy_Valorant", Title: "体验「无畏契约拼豆专家」",
				TaskDesc: "即日起至11月15日，在专家页搜索并召唤「无畏契约拼豆专家」，完成一次对话。",
			},
			want: true, family: true,
		},
		{
			name: "文案判据-需能定位专家",
			task: upstream.Task{TaskCode: "expert_5_special", Title: "召唤 5 次专家", TaskDesc: "召唤 5 位不同的专家并发起对话。"},
			want: false, // 含专家/召唤但没有具体对象，不落族模式
		},
		{
			// 原意图：标题沿用「体验「X」」模板但对象不是专家，不该落**专家族**。
			// 该意图仍然成立（走的是画布分支、不召唤专家）；但 2026-10-28 起它会
			// 被文案动词族按「设计创意/画布」接管，所以从「不可自动化」变成
			// 「可自动」——这正是兜底映射的目的：换码形状的画布任务不必再人工补表。
			name: "同模板但非专家-落画布动词族",
			task: upstream.Task{
				TaskCode: "design_canvas_new", Title: "体验「设计创意模式」",
				Description: "进入「新建任务」，切换滑块到「设计创意」模式，并创建1个画布。",
			},
			want: true,
		},
		{name: "未知码", task: upstream.Task{TaskCode: "invite_friend_1"}, want: false},
		{name: "缺 slug", task: upstream.Task{TaskCode: "expert_use"}, want: false},
		{name: "空 slug", task: upstream.Task{TaskCode: "expert_use_"}, want: false},
		{name: "其他", task: upstream.Task{TaskCode: "Skill_market_use"}, want: false},
	}
	for _, c := range cases {
		act := actionForTask(c.task)
		if (act != nil) != c.want {
			t.Errorf("%s: actionForTask(%q) = %v, want 可解析=%v", c.name, c.task.TaskCode, act, c.want)
			continue
		}
		if act == nil {
			continue
		}
		if c.family != (act.runTask != nil) {
			t.Errorf("%s: runTask 存在 = %v, want %v", c.name, act.runTask != nil, c.family)
		}
	}
}

// TestExpertIDFromJump 上游把目标专家 id 直接写在 jump_url 的 deeplink 里（编码/裸两种形态）。
func TestExpertIDFromJump(t *testing.T) {
	cases := []struct{ jump, want string }{
		{
			jump: "https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_WZY136wV5aMt&buddy_type=workbuddy",
			want: "ex_WZY136wV5aMt",
		},
		{jump: "workbuddy://experts?expertId=ex_abc-123", want: "ex_abc-123"},
		{jump: "workbuddy://expert", want: ""},
		{jump: "", want: ""},
	}
	for _, c := range cases {
		if got := expertIDFromJump(upstream.Task{JumpURL: c.jump}); got != c.want {
			t.Errorf("expertIDFromJump(%q) = %q, want %q", c.jump, got, c.want)
		}
	}
}

// TestResolveTaskExpertPrefersJumpURL jump_url 的 id 命中市场快照时用完整元数据；
// 市场里没有该专家时用最小结构体兜底（判据只校验 id 真实存在）。
func TestResolveTaskExpertPrefersJumpURL(t *testing.T) {
	srv, _ := newExpertMarketServer(t)
	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}

	// 命中市场（ex_dy 在假市场里）。
	e, err := p.resolveTaskExpert(a, upstream.Task{
		TaskCode: "expert_use_x", Title: "体验「抖音策略师」",
		JumpURL: "workbuddy://experts?expertId=ex_dy",
	})
	if err != nil || e.ExpertID != "ex_dy" || e.DisplayNameZH != "抖音策略师" {
		t.Errorf("命中市场应返回完整元数据，got %+v err=%v", e, err)
	}
	// 市场里没有：仍返回带 id 的最小结构体。
	e2, err := p.resolveTaskExpert(a, upstream.Task{
		TaskCode: "WorkBuddy_Valorant", Title: "体验「无畏契约拼豆专家」",
		JumpURL: "workbuddy://experts?expertId=ex_not_in_market",
	})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if e2.ExpertID != "ex_not_in_market" || e2.DisplayNameZH != "无畏契约拼豆专家" {
		t.Errorf("兜底结构体 = %+v, want id=ex_not_in_market name=无畏契约拼豆专家", e2)
	}
}

// TestTaskExpertName 专家名从标题/达成条件里解析（判据载体）。
func TestTaskExpertName(t *testing.T) {
	cases := []struct {
		task upstream.Task
		want string
	}{
		{upstream.Task{Title: "体验「直播电商教练」"}, "直播电商教练"},
		{upstream.Task{Title: "体验「抖音策略师」"}, "抖音策略师"},
		{upstream.Task{Title: "召唤 5 次专家", TaskDesc: "体验「中国电商运营专家」"}, "中国电商运营专家"},
		{upstream.Task{Title: "无引号标题", Description: "去『商家宣传页与海报设计师』转一圈"}, "商家宣传页与海报设计师"},
		{upstream.Task{Title: "无引号标题"}, ""},
	}
	for _, c := range cases {
		if got := taskExpertName(c.task); got != c.want {
			t.Errorf("taskExpertName(%+v) = %q, want %q", c.task, got, c.want)
		}
	}
}

// TestResolveMarketExpert 市场列表匹配：精确命中优先，包含匹配兜底，未命中返回零值
// （调用方据此跳过，绝不硬编 expert id）。
func TestResolveMarketExpert(t *testing.T) {
	srv, hits := newExpertMarketServer(t)
	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}

	e, err := p.resolveMarketExpert(a, "直播电商教练")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if e.ExpertID != "ex_bw" {
		t.Errorf("精确匹配 expert_id = %q, want ex_bw", e.ExpertID)
	}
	// 模糊命中（包含匹配）。
	e2, err := p.resolveMarketExpert(a, "抖音策略")
	if err != nil || e2.ExpertID != "ex_dy" {
		t.Errorf("包含匹配 = %+v err=%v, want ex_dy", e2, err)
	}
	// 未命中：零值，不报错。
	e3, err := p.resolveMarketExpert(a, "不存在的专家")
	if err != nil || e3.ExpertID != "" {
		t.Errorf("未命中应返回零值，got %+v err=%v", e3, err)
	}
	// 三次查询只打一次上游（快照缓存）。
	if got := hits.Load(); got != 1 {
		t.Errorf("专家市场请求数 = %d, want 1（应复用快照）", got)
	}
}

// TestRunExpertExperienceCompletesAndClaims expert_use_* 全链路：按标题反查专家 →
// accept（带回读验证）→ 召唤+真实对话+actual_use → 回读达标 → 自动领奖。
func TestRunExpertExperienceCompletesAndClaims(t *testing.T) {
	oldGap, oldPoll := mpActionGap, claimPollAttempts
	mpActionGap, claimPollAttempts = 0, 1
	t.Cleanup(func() { mpActionGap, claimPollAttempts = oldGap, oldPoll })

	var mu sync.Mutex
	accepted := false
	var claims, chats, reports atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			mu.Lock()
			cur, status := 0, "not_accepted"
			if accepted {
				cur, status = 1, "accepted"
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"tasks": []map[string]any{{
					"task_code": "expert_use_bowangwang", "title": "体验「直播电商教练」",
					"target": 1, "current": cur, "accept_status": status,
					"reward_credit": 50, "reward_energy": 5,
				}}},
			})
		case strings.HasSuffix(r.URL.Path, "/accept"):
			mu.Lock()
			accepted = true
			mu.Unlock()
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		case r.URL.Path == "/portal/operation-platform/market/expert/list":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"experts": []map[string]any{{
					"expert_id": "ex_bw", "expert_type": "agent",
					"display_name_zh": "直播电商教练", "profession_zh": "电商教练", "version": "1.0.0",
				}}},
			})
		case r.URL.Path == "/v2/chat/completions":
			chats.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"id\":\"0123456789abcdef0123456789abcdef\"}\n\n"))
		case strings.Contains(r.URL.Path, "/report"):
			reports.Add(1)
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			claims.Add(1)
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":50,"energy":5}}`))
		default:
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		}
	}))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	task := upstream.Task{
		TaskCode: "expert_use_bowangwang", Title: "体验「直播电商教练」",
		Target: 1, Current: 0, AcceptStatus: "not_accepted", Credit: 50, Energy: 5,
	}
	msg, err := runExpertExperience(p, a, task)
	if err != nil {
		t.Fatalf("runExpertExperience: %v", err)
	}
	if !strings.Contains(msg, "领奖") || !strings.Contains(msg, "直播电商教练") {
		t.Errorf("message = %q, 应包含专家名与领奖结果", msg)
	}
	if got := claims.Load(); got != 1 {
		t.Errorf("claim 次数 = %d, want 1", got)
	}
	if got := chats.Load(); got != 1 {
		t.Errorf("真实对话次数 = %d, want 1（craft 口径一次点亮，不该补 LOCAL 那发）", got)
	}
	if got := reports.Load(); got != 1 {
		t.Errorf("事件上报次数 = %d, want 1", got)
	}
}

// TestRunExpertExperienceSkipsUnknownExpert 市场里找不到该专家：直接返回说明，
// 不 accept、不发事件（避免「已登记未上报」的半程态）。
func TestRunExpertExperienceSkipsUnknownExpert(t *testing.T) {
	oldGap := mpActionGap
	mpActionGap = 0
	t.Cleanup(func() { mpActionGap = oldGap })

	var accepts, reports atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
		case r.URL.Path == "/portal/operation-platform/market/expert/list":
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"experts":[]}}`))
		case strings.HasSuffix(r.URL.Path, "/accept"):
			accepts.Add(1)
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		default:
			reports.Add(1)
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
		}
	}))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	msg, err := runExpertExperience(p, &auth.Auth{UID: "u1", AccessToken: "at"}, upstream.Task{
		TaskCode: "expert_use_unknown", Title: "体验「查无此专家」", Target: 1,
	})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if !strings.Contains(msg, "未能") || !strings.Contains(msg, "专家市场") {
		t.Errorf("message = %q, 应说明未能在专家市场定位", msg)
	}
	if accepts.Load() != 0 || reports.Load() != 0 {
		t.Errorf("未命中专家时不应 accept/上报，accept=%d report=%d", accepts.Load(), reports.Load())
	}
}

// newExpertMarketServer 假专家市场：一页两条，返回请求计数。
func newExpertMarketServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/portal/operation-platform/market/expert/list" {
			w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
			return
		}
		hits.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "OK",
			"data": map[string]any{"experts": []map[string]any{
				{"expert_id": "ex_bw", "expert_type": "agent", "display_name_zh": "直播电商教练", "profession_zh": "电商教练"},
				{"expert_id": "ex_dy", "expert_type": "agent", "display_name_zh": "抖音策略师", "profession_zh": "短视频策略"},
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}
