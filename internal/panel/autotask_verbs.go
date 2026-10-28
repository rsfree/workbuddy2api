// autotask_verbs.go 未知任务码的「文案动词」兜底映射。
//
// 为什么需要它——精确表（autoActions）只能覆盖已经见过的码，上游换一次码形状
// 就漏一批：chat_5 改成 chat_times_5、template_5 改成 use_templates、或者运营
// 直接甩一个新活动码上来。专家族模式（expert_use_*）解决的是「体验某专家」这一
// 类，其余动作用户都得等人工补表。
//
// 本文件按**任务文案里的行为动词**把未知码落到已有动作上，判据是标题/描述的
// 中文关键词（上游标题模板稳定：和 AI 聊天 5 次 / 使用 5 个模板 / 探索优秀灵感…），
// 而不是码形状——形状是运营自由字段，文案是给用户看的，改形状比改文案容易得多。
//
// 边界（重要）：**只收纯事件上报类动作**。
//
//   - 收：对话（ReportChatActivity）、模板、灵感、画布、定时任务、资料库、主题、
//     Buddy 应用——都是 /v2/report 或 /v2/user-asset 的幂等事件，跑错了最坏结果
//     是上游多一条无效事件，不花额度、不改账号状态。
//   - 不收：skill_1（真实对话）、Model_chat_*（真实对话）、black_cat（真实对话）、
//     expert_*（真实召唤+对话）、first_buddy（依赖当日活跃与领养状态）。这些一旦
//     误判就是白烧额度或改账号状态，必须保持人工确认，只走精确表。
//
// 顺序纪律：actionForTask 里 精确表 → 专家族 → 本文件。前面两步更具体，本文件
// 只吃它们都没认领的码。
package panel

import (
	"fmt"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// maxVerbReport 单次兜底动作的事件上限：防止上游给个 target=999 的任务把
// 上报循环跑成风暴（正常情况下 target ≤ 10）。
const maxVerbReport = 20

// verbRule 一条「关键词 → 已有动作」规则。
// keyword 命中任一即算匹配；needTarget=true 表示动作要按任务 target 循环执行。
type verbRule struct {
	keywords   []string
	needTarget bool
	desc       string
	run        func(p *Panel, a *auth.Auth) (string, error)
	runTask    func(p *Panel, a *auth.Auth, t upstream.Task) (string, error)
}

// verbRules 关键词表。顺序无关（关键词互斥度已人工核对），但新增时优先加更具体的
// 词——「设计创意」比「创意」具体，「专家团」比「专家」具体。
var verbRules = []verbRule{
	{
		keywords:   []string{"对话", "聊天"},
		needTarget: true,
		desc:       "族匹配「对话/聊天」→ 按 target 补报对话活跃事件（纯事件，不花额度）",
		runTask:    runChatByTarget,
	},
	{
		keywords:   []string{"模板"},
		needTarget: true,
		desc:       "族匹配「模板」→ 按 target 重复上报模板使用事件组（纯事件）",
		runTask:    runTemplateByTarget,
	},
	{
		keywords: []string{"灵感", "案例"},
		desc:     "族匹配「灵感/案例」→ 上报 playbook 点击+发送 Prompt 事件组",
		run:      runPlaybookPrompt,
	},
	{
		keywords: []string{"画布", "设计创意"},
		desc:     "族匹配「画布/设计创意」→ 上报设计创意画布创建事件组",
		run:      runCreateCanvas,
	},
	{
		keywords: []string{"定时任务", "自动化"},
		desc:     "族匹配「定时任务/自动化」→ 上报定时任务创建事件",
		run:      runAutomationCreate,
	},
	{
		keywords: []string{"资料库"},
		desc:     "族匹配「资料库」→ 上报资料库介绍阅读事件",
		run:      runLibraryRead,
	},
	{
		keywords: []string{"主题", "皮肤"},
		desc:     "族匹配「主题/皮肤」→ 设置默认主题并上报皮肤生效事件",
		run:      runAppearance,
	},
	{
		keywords: []string{"发现应用", "应用市场", "Buddy 应用"},
		desc:     "族匹配「发现应用/应用市场」→ 上报 Buddy 应用进入五连事件",
		run:      runBuddyApp,
	},
}

// verbActionFor 按文案动词把未知任务码映射为动作；nil = 无可映射。
//
// 只看 Title / TaskDesc / Description 三个面向用户的字段，不看码本身——码形状
// 是运营字段，正是这里要绕开的东西。
func verbActionFor(t upstream.Task) *autoAction {
	text := strings.Join([]string{t.Title, t.TaskDesc, t.Description}, " ")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	// 含「专家」的一律不做动词映射：专家类判据要看是否有具体对象/召唤链，
	// 上面两个专用分支才是正解；落到「对话」只会白跑一次且大概率不点亮。
	if strings.Contains(text, "专家") {
		return nil
	}
	for _, r := range verbRules {
		if !containsAny(text, r.keywords) {
			continue
		}
		return &autoAction{
			TaskCode: t.TaskCode,
			Desc:     r.desc,
			run:      r.run,
			runTask:  r.runTask,
		}
	}
	return nil
}

// containsAny 任一关键词出现即真。
func containsAny(text string, keywords []string) bool {
	for _, k := range keywords {
		if strings.Contains(text, k) {
			return true
		}
	}
	return false
}

// runChatByTarget 按任务 target 补报对话活跃事件。
//
// 与精确表的 runChat5 是同一判据（ReportChatActivity 纯事件，不发起真实对话），
// 区别是 target 与进度都取调用方传进来的 Task——兜底场景下这个码不在
// autoActions 里，回查 p.taskByCode(code) 也查不到，只能信当前快照。
func runChatByTarget(p *Panel, a *auth.Auth, t upstream.Task) (string, error) {
	if t.Target <= 0 {
		// 没有 target 的任务（一次性/事件型）靠上报一条即可推进，这里按 1 处理：
		// 上游对这类码的判据是「发生过一次对话」，多报无益。
		t.Target, t.Current = 1, 0
	}
	need := t.Target - t.Current
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	if need > maxVerbReport {
		need = maxVerbReport
	}
	var ok int64
	for i := int64(0); i < need; i++ {
		cid := fmt.Sprintf("wb2api-verb-%d-%d", time.Now().UnixMilli(), i)
		if err := p.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			return fmt.Sprintf("已补报 %d/%d 条，第 %d 条失败: %v", ok, need, i+1, err), nil
		}
		ok++
		if i < need-1 {
			time.Sleep(reportGap)
		}
	}
	return fmt.Sprintf("已补报 %d 条对话事件（target=%d）", ok, t.Target), nil
}

// verbTemplates 模板载体池。与 runTemplateUse 同一批模板（实测服务端不校验
// template_id 真实性），按 target 循环取用。
var verbTemplates = [][2]string{
	{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"},
	{"4", "活动策划"}, {"5", "代码评审"},
}

// runTemplateByTarget 按任务 target 重复上报「使用模板」事件组。
func runTemplateByTarget(p *Panel, a *auth.Auth, t upstream.Task) (string, error) {
	need := t.Target - t.Current
	if t.Target <= 0 {
		need = int64(len(verbTemplates))
	}
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	if need > maxVerbReport {
		need = maxVerbReport
	}
	var ok int64
	for i := int64(0); i < need; i++ {
		tp := verbTemplates[int(i)%len(verbTemplates)]
		ms := time.Now().UnixMilli()
		conv := fmt.Sprintf("wb2api-vtpl-%d-%d", ms, i)
		req := fmt.Sprintf("wb2api-vtpl-req-%d-%d", ms, i)
		events := upstream.DesktopTemplateUseSequence(conv, req, tp[0], tp[1])
		if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
			return fmt.Sprintf("已上报 %d/%d 组，第 %d 组失败: %v", ok, need, i+1, err), nil
		}
		ok++
		if i < need-1 {
			time.Sleep(300 * time.Millisecond)
		}
	}
	return fmt.Sprintf("已上报模板使用事件 %d 组（target=%d）", ok, t.Target), nil
}
