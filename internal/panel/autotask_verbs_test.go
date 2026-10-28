package panel

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestVerbActionForMapsByCopy 换码形状的新任务靠文案动词落到已有动作。
// 用例都取「上游真实标题模板 + 换过的码」，这是最可能发生的漏配形态。
func TestVerbActionForMapsByCopy(t *testing.T) {
	cases := []struct {
		name string
		task upstream.Task
		want bool
	}{
		{
			name: "对话类换码",
			task: upstream.Task{TaskCode: "chat_times_5", Title: "和 AI 聊天 5 次"},
			want: true,
		},
		{
			name: "对话类-无 target 也认",
			task: upstream.Task{TaskCode: "first_conversation", Title: "完成一次对话"},
			want: true,
		},
		{
			name: "模板类换码",
			task: upstream.Task{TaskCode: "use_templates", Title: "使用 3 个模板"},
			want: true,
		},
		{
			name: "灵感类",
			task: upstream.Task{TaskCode: "explore_inspiration", Title: "探索优秀灵感"},
			want: true,
		},
		{
			name: "定时任务类",
			task: upstream.Task{TaskCode: "create_automation", Title: "创建 1 个定时任务"},
			want: true,
		},
		{
			name: "资料库类",
			task: upstream.Task{TaskCode: "read_library", Title: "体验「资料库」"},
			want: true,
		},
		{
			name: "应用类",
			task: upstream.Task{TaskCode: "enter_app", TaskDesc: "进入 Buddy 应用完成一次授权"},
			want: true,
		},
		{
			// 含「专家」一律不做动词映射：专家判据要看具体对象/召唤链，
			// 落到「对话」只会白跑。留给上游精确表或专家族。
			name: "含专家不映射",
			task: upstream.Task{TaskCode: "summon_expert_3", Title: "召唤 3 次专家", TaskDesc: "召唤不同专家并发起对话"},
			want: false,
		},
		{
			// 技能类是真实对话，故意不收（会烧额度），必须人工确认。
			name: "技能类不映射",
			task: upstream.Task{TaskCode: "install_skill", Title: "尝鲜热门技能"},
			want: false,
		},
		{
			name: "空文案不映射",
			task: upstream.Task{TaskCode: "mystery_1"},
			want: false,
		},
	}
	for _, c := range cases {
		act := verbActionFor(c.task)
		if (act != nil) != c.want {
			t.Errorf("%s: verbActionFor(%q)=%v want 可解析=%v", c.name, c.task.TaskCode, act, c.want)
			continue
		}
		if act != nil && act.Desc == "" {
			t.Errorf("%s: 命中的动作必须有说明文案（面板要展示给人看）", c.name)
		}
	}
}

// reportCounter 统计落到 /v2/report 的请求数，并回成功信封。
func reportCounter(t *testing.T) (*upstream.Client, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			atomic.AddInt32(&n, 1)
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	t.Cleanup(srv.Close)
	return &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}, &n
}

// TestRunChatByTargetReportsRemaining 按 target-current 补报，不多不少。
func TestRunChatByTargetReportsRemaining(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	c, n := reportCounter(t)
	p := &Panel{cfg: Config{Upstream: c}}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	msg, err := runChatByTarget(p, a, upstream.Task{TaskCode: "chat_times_7", Target: 7, Current: 4})
	if err != nil {
		t.Fatalf("runChatByTarget: %v", err)
	}
	if got := atomic.LoadInt32(n); got != 3 {
		t.Errorf("上报 %d 次 want 3（target7-current4）: %s", got, msg)
	}

	// 已达标：一条都不该发。
	atomic.StoreInt32(n, 0)
	if _, err := runChatByTarget(p, a, upstream.Task{TaskCode: "x", Target: 3, Current: 3}); err != nil {
		t.Fatalf("已达标不应报错: %v", err)
	}
	if got := atomic.LoadInt32(n); got != 0 {
		t.Errorf("已达标仍上报 %d 次", got)
	}
}

// TestRunChatByTargetCapsRunawayTarget target 异常大时按上限截断，避免上报风暴。
func TestRunChatByTargetCapsRunawayTarget(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	c, n := reportCounter(t)
	p := &Panel{cfg: Config{Upstream: c}}

	if _, err := runChatByTarget(p, &auth.Auth{AccessToken: "at", UID: "u1"},
		upstream.Task{TaskCode: "runaway", Target: 999, Current: 0}); err != nil {
		t.Fatalf("runChatByTarget: %v", err)
	}
	if got := atomic.LoadInt32(n); got != maxVerbReport {
		t.Errorf("上报 %d 次 want 上限 %d", got, maxVerbReport)
	}
}

// TestRunTemplateByTargetLoopsTemplates target 超过模板池长度时循环取用，不越界。
func TestRunTemplateByTargetLoopsTemplates(t *testing.T) {
	c, n := reportCounter(t)
	p := &Panel{cfg: Config{Upstream: c}}

	if _, err := runTemplateByTarget(p, &auth.Auth{AccessToken: "at", UID: "u1"},
		upstream.Task{TaskCode: "tpl_7", Target: 7, Current: 0}); err != nil {
		t.Fatalf("runTemplateByTarget: %v", err)
	}
	if got := atomic.LoadInt32(n); got != 7 {
		t.Errorf("上报 %d 组 want 7", got)
	}
}
