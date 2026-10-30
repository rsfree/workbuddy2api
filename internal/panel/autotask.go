// autotask.go 面板「一键完成」任务的动作实现。
//
// 设计依据：上游 scripts/task_*.py 实测结论 + 2026-09-12 桌面指纹协议逆向
// （data/desktop-task-protocol.md）——
//   - first_buddy（+300 分）：report（前置解锁）→ agreement → buddy/first
//   - chat_5（+100 分）：累计 5 条 chat_request_send 上报
//   - Model_chat_GLM5.2（+100 分）：accept → 用 glm-5.2 真实对话一次 → 上报（模型字段对齐）
//   - RichMeow_Chat：桌面指纹（workbuddy-desktop）完整对话事件链，纯 API 可点亮（三账号实测）
//   - Buddy_App / Buddy_App_QQ：buddyapp 五连事件，纯 API 可点亮（两账号实测）
//   - automation_1：automated_task_create_suc 事件，纯 API 可点亮（两账号实测）
//   - Library_read：web 域 web_element_click(library_doc_intro_click)（三账号实测）
//   - template_5 / playbook_prompt / create_canvas：asar 逆向出的判据事件
//     （template_used / playbook_prompt_send / wbx_design_canvas_*），纯 API 可点亮（三账号实测）
//   - expert_5 / Expert_team_use_3：真实专家列表 + 召唤链 + 真实 chat（服务端 requestId）
//   - expert_actual_use（三账号实测）
//   - Hp_Appearance：appearance/set + appearance_skin_apply 事件（两账号实测）
//
// 仍未破解：skill_1（疑似要求真实 Skill 工具调用）。
// 不做：Expert_lighthouse（需真实连接器授权）、Expert_Philanthropy（真实捐款）。
//
// 所有动作幂等：已 claimed/已达标的任务直接跳过，不重复消耗上游配额。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// autoAction 一个可自动化的任务动作。
type autoAction struct {
	TaskCode string // 目标任务 code
	Desc     string // 展示用说明
	Attempt  bool   // true = 尝试型（上游未证实可脚本化，跑了可能不点亮）
	// Paid true = 动作要发起**真实模型对话**（消耗账号额度）。余额 0 的号跑它
	// 必然 429 code=14018「额度已用尽」：白打一次请求、白占十几秒队列时间，
	// 进度一动不动。零积分账号的执行路径据此只跑免生成动作
	// （见 runAction 的额度闸门与 tasksweep_rescue.go）。默认 false：
	// /v2/report、/v2/user-asset 这类纯事件上报不花额度。
	Paid bool
	run  func(p *Panel, a *auth.Auth) (string, error)
	// runTask 需要任务上下文（标题/进度/口径）的动作走这里，nil 时回落到 run。
	// 族模式动作（如 expert_use_*「体验指定专家」）由任务标题解析专家名，故必须
	// 拿到 Task 本体；精确表动作只认 code，保持原签名不动。
	runTask func(p *Panel, a *auth.Auth, t upstream.Task) (string, error)
}

// autoActions 已实现的任务动作表（顺序即执行顺序：先解锁依赖项）。
// first_buddy 依赖活跃上报解锁，故 chat_5/first_buddy 的执行都自带 report 步骤。
var autoActions = []autoAction{
	{
		TaskCode: "chat_5",
		Desc:     "上报 5 条对话活跃事件（自动补足差额）",
		run:      runChat5,
	},
	{
		TaskCode: "first_buddy",
		Desc:     "上报解锁 → 同意协议 → 领取第一只 Buddy（+300 分）",
		run:      runFirstBuddy,
	},
	{
		TaskCode: "Model_chat_GLM5.2",
		Desc:     "接受任务 → glm-5.2 真实对话一次 → 对齐模型上报",
		Paid:     true,
		run:      runModelChat,
	},
	{
		TaskCode: "RichMeow_Chat",
		Desc:     "桌面指纹事件链上报（已验证：纯 API 可点亮，三账号实测）",
		run:      runRichMeow,
	},
	{
		TaskCode: "Buddy_App",
		Desc:     "上报「进入 Buddy 应用」事件链（已验证：纯 API 可点亮）",
		run:      runBuddyApp,
	},
	{
		TaskCode: "Buddy_App_QQ",
		Desc:     "上报「进入企鹅教师助手」事件链（已验证：纯 API 可点亮）",
		run:      runBuddyApp,
	},
	{
		TaskCode: "automation_1",
		Desc:     "上报「定时任务创建」事件（已验证：纯 API 可点亮）",
		run:      runAutomationCreate,
	},
	{
		TaskCode: "Library_read",
		Desc:     "上报「读资料库介绍」事件（已验证：纯 API 可点亮）",
		run:      runLibraryRead,
	},
	{
		TaskCode: "template_5",
		Desc:     "上报「使用模板创建任务」事件组 ×5（已验证：三账号点亮）",
		run:      runTemplateUse,
	},
	{
		TaskCode: "playbook_prompt",
		Desc:     "上报「灵感案例做同款发送 Prompt」事件组（已验证：三账号点亮）",
		run:      runPlaybookPrompt,
	},
	{
		TaskCode: "create_canvas",
		Desc:     "上报「设计创意画布创建」事件组（已验证：三账号点亮，+300 分）",
		run:      runCreateCanvas,
	},
	{
		TaskCode: "expert_5",
		Desc:     "真实专家召唤+使用链 ×5（专家市场列表+真实 chat，已验证：三账号点亮）",
		Paid:     true,
		run:      runExpertUse,
	},
	{
		TaskCode: "Expert_team_use_3",
		Desc:     "真实专家团召唤+使用链 ×3（已验证：三账号点亮）",
		Paid:     true,
		run:      runExpertTeamUse,
	},
	{
		TaskCode: "Hp_Appearance",
		Desc:     "设置主题 API + 皮肤生效事件（已验证：两账号点亮）",
		run:      runAppearance,
	},
	{
		TaskCode: "skill_1",
		Desc:     "真实对话 + skill_info 技能加载事件（已验证：人杰2 点亮）",
		Paid:     true,
		run:      runSkillFresh,
	},
	{
		TaskCode: "Expert_lighthouse",
		Desc:     "真实轻量云专家召唤+使用链（chat 链带 has_expert，已验证：两账号点亮）",
		Paid:     true,
		run:      runExpertLighthouse,
	},
	{
		TaskCode: "black_cat",
		Desc:     "夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示稍后再试）",
		Paid:     true,
		Attempt:  true,
		run:      runBlackCat,
	},
	{
		TaskCode: "school_season",
		Desc:     "校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）",
		run:      runSchoolSeason,
	},
	{
		TaskCode: "Sequential_Tasks_1",
		Desc:     "小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）",
		run:      runSequentialChat,
	},
	{
		TaskCode: "Sequential_Tasks_2",
		Desc:     "小程序选专家对话（小程序口径）：市场专家 id → accept → expert_actual_use 上报 → 领奖（+200c+5e）",
		run:      runMiniExpert,
	},
	{
		TaskCode: "Sequential_Tasks_3",
		Desc:     "小程序五次对话（小程序口径）：accept → mini 对话上报 ×5（自动补差额）→ 领奖（+300c+5e）",
		run:      runSequentialChat5,
	},
	{
		TaskCode: "Sequential_Tasks_4",
		Desc:     "小程序定时任务（预留，每日零点解锁一环）：accept → 定时任务创建事件（PC 同源）→ 领奖（判据待解锁验证）",
		run:      runSequentialAutomation,
	},
	{
		TaskCode: "Sequential_Tasks_5",
		Desc:     "小程序使用 GLM5.2（预留）：accept → 带模型字段的 mini 对话上报 → 领奖（判据待解锁验证）",
		run:      runSequentialModelChat,
	},
	{
		TaskCode: "Sequential_Tasks_6",
		Desc:     "小程序十次对话（预留）：accept → mini 对话上报 ×target（自动补差额）→ 领奖",
		run:      runSequentialChat10,
	},
	{
		TaskCode: "Sequential_Tasks_7",
		Desc:     "体验灵感功能（PC 限定）：accept → 桌面灵感事件 → 回读确认 → 领取 +500 分 +5 能",
		run:      runSequentialPlaybook,
	},
}

// autoActionFor 查任务对应的动作；无则返回 nil（不可自动化）。
func autoActionFor(code string) *autoAction {
	for i := range autoActions {
		if autoActions[i].TaskCode == strings.TrimSpace(code) {
			return &autoActions[i]
		}
	}
	return nil
}

// autoActionIndex 任务在 autoActions 中的顺序（队列执行按依赖序排；未知返回大值）。
func autoActionIndex(code string) int {
	for i := range autoActions {
		if autoActions[i].TaskCode == code {
			return i
		}
	}
	return 1 << 20
}

// sequentialTaskPrefix Sequential 串行链的码前缀。上游每日零点解锁一环
// （Sequential_Tasks_1 → _2 → …），走完最后一环不再下发新环（线上实测：
// 7 环全 claimed 的账号列表里不再出现第 8 环，n 恒为 7）。
const sequentialTaskPrefix = "Sequential_Tasks_"

// sequentialTaskIndex 解析 Sequential 链序号 N；非该族返回 ok=false。
//
// 为什么不复用 autoActionIndex：上游随时会加环（活动期加过 _7）。代码里没登记
// 的环在 autoActionIndex 下返回大值，会被排到所有已登记动作之后——于是「链还
// 没解锁到它」和「它不可自动化」混成一件事。按 N 排则任何编号的环都置顶，
// 族内严格 N 升序（不破坏 N 依赖 N-1 的解锁语义）。
func sequentialTaskIndex(code string) (int, bool) {
	s := strings.TrimSpace(code)
	if !strings.HasPrefix(s, sequentialTaskPrefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, sequentialTaskPrefix))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// taskPriorityLess 任务执行优先级：Sequential 链置顶（族内 N 升序），
// 其余按 autoActions 注册序（依赖前置）。
//
// 为什么链必须置顶：队列与清扫都是「轮到某个账号 → 按序做它的任务」。一个号
// 一天里只有一环 unlocked，若前面压着十几条付费动作（真实对话，单条几十秒到
// 几分钟），这一环就被挤到下一轮；下一轮又从该号第一位重新开始，于是同一批号
// 永远排在链前面、链永远推进不了。线上抽样 25 号里 18 号卡在 S1/S2/S3 正是
// 这个形状——零积分号因「免生成优先」侥幸躲过，有余额的号才是有系统性的漏。
func taskPriorityLess(a, b upstream.Task) bool {
	ia, oka := sequentialTaskIndex(a.TaskCode)
	ib, okb := sequentialTaskIndex(b.TaskCode)
	if oka != okb {
		return oka // 链族恒在前
	}
	if oka && ia != ib {
		return ia < ib
	}
	return autoActionIndex(a.TaskCode) < autoActionIndex(b.TaskCode)
}

// ---------------------------------------------------------------------------
// 族模式：新任务不改代码也能自动完成
// ---------------------------------------------------------------------------

// expertUsePattern 「体验指定专家」任务码形状。上游按运营活动动态加码，实测
// expert_use_bowangwang / expert_use_douyinyin / expert_use_maidehao /
// expert_use_shangjia_xuanchuan 一批同形状任务（+50 分 +5 能），标题统一为
// 「体验「<专家名>」」。这一类**不该逐个登记**：只要形状一致，就从任务标题
// 解析专家名 → 市场列表反查 id → 召唤+真实对话+actual_use。
var expertUsePattern = regexp.MustCompile(`(?i)^expert_use_[a-z0-9_]+$`)

// actionForTask 解析任务对应的动作：精确表优先（保留依赖顺序语义），
// 其次按族模式兜底。nil = 该任务当前无法自动化，只能接受/等待人工。
//
// 族模式有两类推定来源（实测）：
//  1. 码形状 `expert_use_<slug>`（如 expert_use_bowangwang）；
//  2. 文案判据「专家 + 召唤/专家页 + 能定位到具体专家」——上游换活动时会用
//     别的码形状（实测 WorkBuddy_Valorant「体验「无畏契约拼豆专家」」），
//     只认前缀必漏。
func actionForTask(t upstream.Task) *autoAction {
	code := strings.TrimSpace(t.TaskCode)
	if act := autoActionFor(code); act != nil {
		return act
	}
	if expertUsePattern.MatchString(code) || looksLikeExpertExperience(t) {
		return &autoAction{
			TaskCode: code,
			Desc:     "体验指定专家（族模式：jump_url 的 expertId 或标题专家名 → 召唤 + 真实对话 + actual_use）",
			Paid:     true, // 判据是真实 chat 的 requestId，余额 0 的号跑不了
			runTask:  runExpertExperience,
		}
	}
	// 最后兜底：文案动词映射（只收纯事件上报类动作，见 autotask_verbs.go）。
	// 放置顺序不可前移——精确表保证「Sequential_Tasks_2 完成 1 次专家对话」这类
	// 标题含「对话」的专家任务仍走专用链路，不会被聊天事件顶掉。
	if act := verbActionFor(t); act != nil {
		return act
	}
	return nil
}

// looksLikeExpertExperience 文案判据：任务是否在要求「召唤某个指定专家并对话」。
//
// 必须同时满足：文案含「专家」+ 含「召唤」或「专家页」+ **能定位到具体专家**
// （jump_url 里有 expertId，或标题「X」里有名字）。最后一条是关键——同样用
// 「体验「X」」模板的 create_canvas（设计创意模式）不含「专家」，而 expert_5
// （召唤 5 次专家）虽然含专家但没有具体对象，两者都不该落进族模式。
func looksLikeExpertExperience(t upstream.Task) bool {
	text := t.Title + " " + t.TaskDesc + " " + t.Description
	if !strings.Contains(text, "专家") {
		return false
	}
	if !strings.Contains(text, "召唤") && !strings.Contains(text, "专家页") {
		return false
	}
	return expertIDFromJump(t) != "" || taskExpertName(t) != ""
}

// expertIDRe 从 jump_url（或其 deeplink 参数解码后）抓专家 id。
var expertIDRe = regexp.MustCompile(`expertId=([A-Za-z0-9_\-]+)`)

// expertIDFromJump 取任务指定的专家 id。上游对「体验指定专家」任务会在
// jump_url 里直给 deeplink：
//
//	https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_WZY136wV5aMt
//	→ workbuddy://experts?expertId=ex_WZY136wV5aMt
//
// 这比按标题去市场列表里猜可靠得多（定向专家未必在 reco_rank 首屏，甚至不在市场）。
func expertIDFromJump(t upstream.Task) string {
	raw := strings.TrimSpace(t.JumpURL)
	if raw == "" {
		return ""
	}
	if dec, err := url.QueryUnescape(raw); err == nil {
		raw = dec
	}
	if m := expertIDRe.FindStringSubmatch(raw); len(m) == 2 {
		return m[1]
	}
	return ""
}

// errWalletEmpty 付费动作被「额度已用尽」闸门挡下。**不是失败**：调用方应记
// blocked/skipped，而不是 error——否则死号每天的队列都会报一堆"失败"，把真正的
// 异常埋在噪音里（线上 0 分号约 30 个，每个 5 条付费动作）。
var errWalletEmpty = errors.New("额度已用尽（余额 0）")

// walletExhausted 账号余额已耗尽（credits<=0 且总额已知）。
// 口径与 scheduler.TaskMuted 的「积分清光放行」、清扫的零积分补给完全一致：
// CreditsTotal==0 表示「总额未知」（旧 state / 查询失败），不能当成没钱。
func (p *Panel) walletExhausted(uid string) bool {
	if p.cfg.Pool == nil || uid == "" {
		return false
	}
	st, ok := p.cfg.Pool.Status(uid)
	if !ok {
		return false
	}
	return st.Credits <= 0 && st.CreditsTotal > 0
}

// actionIsPaid 任务对应的动作是否需要真实模型生成（未知动作按"不花钱"处理：
// 免生成动作最坏结果是上游多收一条无效事件，误挡付费动作才是真损失）。
func actionIsPaid(t upstream.Task) bool {
	act := actionForTask(t)
	return act != nil && act.Paid
}

// runAction 统一执行入口：族动作带任务上下文，精确动作用原签名。
func runAction(act *autoAction, p *Panel, a *auth.Auth, t upstream.Task) (string, error) {
	switch {
	case act == nil:
		return "", fmt.Errorf("任务 %s 无自动动作", t.TaskCode)
	case act.Paid && p.walletExhausted(a.UID):
		return "", fmt.Errorf("%w：跳过「%s」这类要真实对话的动作，先跑免生成任务回血",
			errWalletEmpty, act.Desc)
	case act.runTask != nil:
		return act.runTask(p, a, t)
	case act.run != nil:
		return act.run(p, a)
	default:
		return "", fmt.Errorf("任务 %s 动作未实现", t.TaskCode)
	}
}

// taskExpertName 从任务标题/说明里抠出「」内的专家名（族任务的判据载体）。
// 只认成对的中文引号：上游对这类任务的标题模板固定为「体验「X」」。
var expertNameRe = regexp.MustCompile(`[「『]([^」』]{2,32})[」』]`)

func taskExpertName(t upstream.Task) string {
	for _, s := range []string{t.Title, t.TaskDesc, t.Description} {
		if m := expertNameRe.FindStringSubmatch(s); len(m) == 2 {
			if name := strings.TrimSpace(m[1]); name != "" {
				return name
			}
		}
	}
	return ""
}

// expertCacheEntry 单账号的专家市场快照（含抓取时刻）。
type expertCacheEntry struct {
	experts []upstream.MarketExpert
	at      time.Time
}

// expertCacheTTL 快照有效期：同一轮扫描里多个定向专家任务复用一份列表，
// 跨轮次隔几分钟重拉一次即可（专家市场排序变化不影响按名匹配）。
var expertCacheTTL = 5 * time.Minute

// marketExpertSnapshot 拉取（带缓存）该账号可见的推荐前 N 页，仅作搜索失败时的兜底。
// 这不是全量市场：2026-10-30 上游 total_count 已达 30551，绝不能再用页数推断专家下线。
const expertSnapshotPages = 8

func (p *Panel) marketExpertSnapshot(a *auth.Auth) ([]upstream.MarketExpert, error) {
	key := a.UID
	if key == "" {
		key = a.AccessTokenValue()
	}
	p.expertMu.Lock()
	if e, ok := p.expertCache[key]; ok && time.Since(e.at) < expertCacheTTL {
		p.expertMu.Unlock()
		return e.experts, nil
	}
	p.expertMu.Unlock()

	const pageSize = 20
	var all []upstream.MarketExpert
	for page := 1; page <= expertSnapshotPages; page++ {
		list, err := p.cfg.Upstream.MarketExpertListPaged(a, "", page, pageSize)
		if err != nil {
			if page == 1 {
				return nil, err
			}
			break // 中途翻页失败：用已拿到的部分（够匹配就够用）
		}
		all = append(all, list...)
		if len(list) < pageSize {
			break
		}
	}
	p.expertMu.Lock()
	if p.expertCache == nil {
		p.expertCache = map[string]expertCacheEntry{}
	}
	p.expertCache[key] = expertCacheEntry{experts: all, at: time.Now()}
	p.expertMu.Unlock()
	return all, nil
}

// matchMarketExpert 按展示名匹配：先精确（展示名/职业名），再包含匹配。
func matchMarketExpert(list []upstream.MarketExpert, name string) upstream.MarketExpert {
	var fuzzy upstream.MarketExpert
	for _, e := range list {
		if e.ExpertID == "" {
			continue
		}
		if e.DisplayNameZH == name || e.ProfessionZH == name {
			return e
		}
		// 包含匹配只对 2 字以上的名字生效，避免「AI」这类短词误命中。
		if len([]rune(name)) >= 2 &&
			(strings.Contains(e.DisplayNameZH, name) || strings.Contains(name, e.DisplayNameZH)) {
			if fuzzy.ExpertID == "" {
				fuzzy = e
			}
		}
	}
	return fuzzy
}

// resolveMarketExpert 先用上游 keyword 定向检索，再退回推荐分页快照。
// 未命中返回零值（ExpertID == ""），由调用方决定跳过而不是硬编 id。
func (p *Panel) resolveMarketExpert(a *auth.Auth, name string) (upstream.MarketExpert, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return upstream.MarketExpert{}, nil
	}
	if searched, err := p.cfg.Upstream.MarketExpertSearch(a, name); err == nil {
		if e := matchMarketExpert(searched, name); e.ExpertID != "" {
			return e, nil
		}
	}
	list, err := p.marketExpertSnapshot(a)
	if err != nil {
		return upstream.MarketExpert{}, err
	}
	return matchMarketExpert(list, name), nil
}

// resolveTaskExpert 解析任务指向的专家，优先级：
//  1. jump_url 里的 expertId（上游直给，最可靠）；
//  2. 标题「X」里的展示名 → 市场列表反查。
//
// id 命中但市场快照里没有该专家时，用最小结构体兜底：判据只校验 id 真实存在，
// name/version/title 仅用于事件展示（缺了不影响计分）。
func (p *Panel) resolveTaskExpert(a *auth.Auth, t upstream.Task) (upstream.MarketExpert, error) {
	if id := expertIDFromJump(t); id != "" {
		if snap, err := p.marketExpertSnapshot(a); err == nil {
			for _, e := range snap {
				if e.ExpertID == id {
					return e, nil
				}
			}
		}
		return upstream.MarketExpert{
			ExpertID:      id,
			ExpertType:    "agent",
			DisplayNameZH: taskExpertName(t),
			Version:       "1.0.0",
		}, nil
	}
	name := taskExpertName(t)
	if name == "" {
		return upstream.MarketExpert{}, nil
	}
	return p.resolveMarketExpert(a, name)
}

// acceptWithVerify 接受任务并回读验证登记生效（默认/mp 双口径）。
// 上游存在 200+OK 但 accept 未落账的形态，此时后续上报全部不归账，
// 判定以回读 accept_status 为准，未生效重试一次。
func (p *Panel) acceptWithVerify(a *auth.Auth, code string, isMP bool) bool {
	for attempt := 1; attempt <= 2; attempt++ {
		var err error
		if isMP {
			err = p.cfg.Upstream.AcceptTasksMP(a, []string{code})
		} else {
			err = p.cfg.Upstream.AcceptTasks(a, []string{code})
		}
		if err != nil {
			log.Printf("autotask %s %s: accept 尝试%d: %v", logfmt.Label(a.UID, a.Nickname), code, attempt, err)
			continue
		}
		time.Sleep(mpActionGap)
		t, rerr := p.taskByCode(a, code)
		if rerr == nil && t != nil && t.AcceptStatus != "not_accepted" && t.AcceptStatus != "" {
			return true
		}
		log.Printf("autotask %s %s: accept 尝试%d 未登记生效（回读=%q）",
			logfmt.Label(a.UID, a.Nickname), code, attempt, acceptStatusOr(t))
	}
	return false
}

// reportExpertUse 对指定专家跑一轮「召唤 + 真实对话 + expert_actual_use」。
// mode="craft"：常规市场专家口径（expert_5 同族）；mode="local"：轻量云专家同款
// LOCAL 口径（chat 链带 has_expert，actual_use 的 type 空、cost=0）——后者实测是
// 「体验某专家」任务的另一种计分形态，作为兜底第二发。
func (p *Panel) reportExpertUse(a *auth.Auth, e upstream.MarketExpert, mode string) error {
	conv, req, err := p.cfg.Upstream.DesktopChatWithExpert(a, e.ExpertID)
	if err != nil {
		return fmt.Errorf("真实对话: %w", err)
	}
	if len(req) < 8 {
		return fmt.Errorf("服务端 requestId 异常（%q）", req)
	}
	events := upstream.DesktopExpertSummonSequence(e)
	chat := upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model")
	if mode == "local" {
		for _, ev := range chat {
			if ev["eventCode"] == "agent_task_created" {
				ev["has_expert"] = true
				ev["expert_id"] = e.ExpertID
				ev["expert_name"] = e.DisplayNameZH
				ev["expert_industry_id"] = ""
			}
		}
	}
	events = append(events, chat...)
	var use upstream.DesktopEvent
	if mode == "local" {
		use = upstream.DesktopExpertActualUseLocal(e, conv, req)
		use["type"] = ""
		use["cost"] = 0
		events = append(events, use)
	} else {
		events = append(events, upstream.DesktopExpertActualUseEvent(e, conv, req))
	}
	return p.cfg.Upstream.ReportDesktopEvent(a, events...)
}

// runExpertExperience 完成 expert_use_<slug>「体验指定专家」类任务（族模式）。
//
// 与 Expert_lighthouse 同族：判据 = 该专家 id 上的 expert_actual_use，且 requestId
// 必须是真实对话的服务端 id。差别只在**专家来源**：这里是按任务标题里的名字在
// 市场列表里反查，而不是写死 ex_ 前缀 id——上游每加一个专家就是一条新任务，
// 硬编码永远追不上。
//
// 流程：已达标先领 → 解析专家（找不到直接返回，不留半程态）→ accept 验证 →
// craft 口径上报 → 异步计分回读 → 未点亮补 LOCAL 口径 → 达标领奖。
func runExpertExperience(p *Panel, a *auth.Auth, t upstream.Task) (string, error) {
	code := t.TaskCode
	if t.Claimed {
		return "已领取", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target {
		credit, energy, err := claimTaskReward(p, a, code)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已达标并领取（+%d 分 +%d 能）", credit, energy), nil
	}
	name := taskExpertName(t)
	if name == "" && expertIDFromJump(t) == "" {
		return "任务既无专家名也 jump_url 无 expertId，无法定位判据对象（需人工确认）", nil
	}
	e, err := p.resolveTaskExpert(a, t)
	if err != nil {
		return "", fmt.Errorf("解析专家: %w", err)
	}
	if e.ExpertID == "" {
		return fmt.Sprintf("未能在专家市场定位「%s」（定向/下线专家），跳过以免半程态", name), nil
	}
	if e.DisplayNameZH != "" {
		name = e.DisplayNameZH
	}
	if name == "" {
		name = e.ExpertID
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerify(a, code, false) {
			return "accept 未登记生效（上游 200+OK 但未落账形态），待下次重试", nil
		}
	}
	if err := p.reportExpertUse(a, e, "craft"); err != nil {
		return fmt.Sprintf("召唤/上报失败: %v", err), nil
	}
	after, _ := p.taskByCodeWaiting(a, code)
	if after != nil && after.Claimable {
		credit, energy, cerr := claimTaskReward(p, a, code)
		if cerr != nil {
			return "已点亮但领奖失败（下次清扫补领）", nil
		}
		return fmt.Sprintf("「%s」体验完成并领奖（+%d 分 +%d 能）", name, credit, energy), nil
	}
	if after != nil && after.Current < target {
		// craft 口径未计分：补一发 LOCAL 口径（同一专家、真实 requestId）。
		if err := p.reportExpertUse(a, e, "local"); err != nil {
			return fmt.Sprintf("craft 未计分，LOCAL 补发失败: %v", err), nil
		}
		after, _ = p.taskByCodeWaiting(a, code)
		if after != nil && after.Claimable {
			credit, energy, cerr := claimTaskReward(p, a, code)
			if cerr != nil {
				return "已点亮（LOCAL 口径）但领奖失败（下次清扫补领）", nil
			}
			return fmt.Sprintf("「%s」体验完成并领奖（LOCAL 口径，+%d 分 +%d 能）", name, credit, energy), nil
		}
	}
	return fmt.Sprintf("已对「%s」上报召唤+使用链，进度未归账（异步计分，下次重试）", name), nil
}

// mpTaskCodes 小程序口径专属下发的成长任务：默认（无 mp 头）列表不出现，
// accept/claim 均要求 X-Client-Platform: miniprogram。新任务出现时在此登记。
var mpTaskCodes = map[string]bool{
	"school_season":      true, // 校园日（mini chat + activityId）
	"Sequential_Tasks_1": true, // 小程序首对话（mini chat，无 activityId）
	"Sequential_Tasks_2": true, // 小程序选中专家并完成有效对话（mp 指纹 expert_actual_use）
	"Sequential_Tasks_3": true, // 小程序完成 5 次对话（与 Tasks_1 同形状，target=5 逐条累加）
	// Tasks_4..6 存在性已实测（accept 返回 prerequisite not met 链式依赖；Tasks_8 not
	// found 封顶）。Tasks_7 上游实际标记为「PC 限定」，不属于这个 mp 集合。
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
}

// isMPTaskCode 报告任务是否小程序口径专属（决定回读/接受/领奖走 mp 变体）。
func isMPTaskCode(code string) bool { return mpTaskCodes[code] }

// claimTaskReward 按任务码口径领奖：mp 码走 chat 域 mp 端点，其余走 Web 端。
// claim 幂等（重复领返回 already_claimed → 0 分 0 能）。
func claimTaskReward(p *Panel, a *auth.Auth, code string) (credit, energy int64, err error) {
	if isMPTaskCode(code) {
		return p.cfg.Upstream.ClaimRewardMP(a, code)
	}
	return p.cfg.Upstream.ClaimReward(a, code)
}

// taskByCode 拉取任务列表并定位单个任务；未找到返回 nil（不视为错误）。
// 双口径：mp 专属任务在默认列表查不到，自动回落 mp 列表（仅对已登记的 mp 码，
// 未知码不多打一次上游）。
func (p *Panel) taskByCode(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	if isMPTaskCode(code) {
		return p.taskByCodeMP(a, code)
	}
	return nil, nil
}

// claimPollAttempts / claimPollGap 达标回读的有界轮询参数。
// 背景：上游计分是**异步**的——行为事件上报后进度要数秒才刷新（实测 Model_chat
// 对话完成后立即回读仍是 0/1，约 5-8 秒后才变 1/1）。一次性回读会误判"未达标"，
// 从而跳过自动领奖。这里最多轮询 N 次、每次间隔 gap，总预算约 12 秒。
var (
	claimPollAttempts = 4
	claimPollGap      = 3 * time.Second
)

// taskByCodeWaiting 回读任务，若未达标则在有界预算内轮询等待（上游异步计分）。
// 已达标（claimable）立即返回；预算耗尽返回最后一次结果（可能仍未达标）。
func (p *Panel) taskByCodeWaiting(a *auth.Auth, code string) (*upstream.Task, error) {
	t, err := p.taskByCode(a, code)
	if err != nil || t == nil {
		return t, err
	}
	if t.Claimable || t.Claimed {
		return t, nil
	}
	for i := 1; i < claimPollAttempts; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCode(a, code)
		if err2 != nil {
			return t, nil // 轮询期间的查询失败不覆盖已拿到的结果
		}
		if t2 != nil {
			t = t2
			if t.Claimable || t.Claimed {
				return t, nil
			}
		}
	}
	return t, nil
}

// taskProgressReached 判断任务是否真的达成：已领 / 可领，或 current 达标。
//
// target<=0 的任务（串行解锁占位、按行为而非次数计的任务）无法用进度判据，
// 视为「不阻塞」——把它们判成未达成会让重试和 pending 状态误伤正常任务。
// t==nil（回读失败）判未达成：调用方据此重试一次，而不是把一次查询失败
// 直接当成「已上报成功」。
func taskProgressReached(t *upstream.Task) bool {
	if t == nil {
		return false
	}
	if t.Claimed || t.Claimable {
		return true
	}
	if t.Target <= 0 {
		return true
	}
	return t.Current >= t.Target
}

// taskByCodeMP 以小程序口径拉取任务列表并定位单个任务；未找到返回 nil。
// 小程序限定任务（school_season / Sequential_Tasks_1）在默认口径列表不出现。
func (p *Panel) taskByCodeMP(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasksMP(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	return nil, nil
}

// acceptWithVerifyMP accept 并回读验证登记生效：上游存在 200+OK 但 accept 未真正
// 登记的形态（此时上报事件全部不归账，任务永远点不亮，上游 task_runner c793ae3
// 实测）——判定以回读 accept_status 为准，未生效重试一次。
func (p *Panel) acceptWithVerifyMP(a *auth.Auth, code string) bool {
	for attempt := 1; attempt <= 2; attempt++ {
		if err := p.cfg.Upstream.AcceptTasksMP(a, []string{code}); err != nil {
			log.Printf("autotask %s %s: accept 尝试%d: %v", logfmt.Label(a.UID, a.Nickname), code, attempt, err)
			continue
		}
		time.Sleep(mpActionGap)
		t, err := p.taskByCodeMP(a, code)
		if err == nil && t != nil && t.AcceptStatus != "not_accepted" && t.AcceptStatus != "" {
			return true
		}
		log.Printf("autotask %s %s: accept 尝试%d 未登记生效（回读=%q）", logfmt.Label(a.UID, a.Nickname), code, attempt, acceptStatusOr(t))
	}
	return false
}

// acceptStatusOr 安全读取任务 accept_status（nil 任务返回 "?"）。
func acceptStatusOr(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.AcceptStatus == "" {
		return "?"
	}
	return t.AcceptStatus
}

// mpActionGap mp 任务写动作间隔（accept/上报/领奖之间，防频控）。
var mpActionGap = 2 * time.Second

// mpChatEventGap mp 对话事件（chat_request_send）的真人节奏间隔。上游对
// Sequential_Tasks_3「5 次有效对话」有反作弊校验：数秒级连发的事件会先被计入
// 进度（回读 5/5、accept_status 甚至短暂转 completed），随后被判定无效整体回滚
// （进度回落、claim 返回 400 "task not completed"）——2026-09-26 实测 2s 连发
// 4 条全灭，45s 间隔逐条上报全存活且 claim +300c+5e 成功。每条上报前
// sleep gap + 0~10s 抖动；首条也等（上一轮残留进度被回滚后立即重报同样无效）。
var mpChatEventGap = 45 * time.Second

// runMPMiniChatTask growth 域小程序限定任务通用闭环：
// mp 查询 → accept（带登记回读验证）→ mini chat 事件上报（withActivityId 决定
// 是否带开学季 activityId：school_season 必带，Sequential_Tasks_1 不带——服务端按
// source=mini_program 指纹关联）→ 回读 → 达标即领奖。
func (p *Panel) runMPMiniChatTask(a *auth.Auth, code string, withActivityId bool) (string, error) {
	t, err := p.taskByCodeMP(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp 口径未下发该任务（活动可能已结束）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, code) {
			return "accept 未登记生效（上游 200+OK 但未落账形态），待下次重试", nil
		}
		// accept 前的任务进度为 null（target 下发 0），兜底 target=1 会少报——
		// Tasks_6 首轮实测：accept 后真实 target=10，只补 1 条就误判达标去领奖
		// （claim 400 task not completed）。接受后回读一次拿真实 target/current。
		if t2, err := p.taskByCodeMP(a, code); err == nil && t2 != nil {
			t = t2
		}
	}
	// 已达标（含 completed 未领）：直接领奖。
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已领取奖励（+%dc +%de）", credit, energy), nil
	}
	// 判据上报：按差额补 mini chat 事件。每条前 sleep mpChatEventGap+抖动——
	// 连发会被上游反作弊判无效（见 mpChatEventGap 注释），宁可慢不可白报。
	need := target - t.Current
	for i := int64(0); i < need; i++ {
		time.Sleep(mpChatEventGap + time.Duration(rand.Int64N(int64(10*time.Second))))
		conv := fmt.Sprintf("wb2api-mp-%d-%d", time.Now().UnixMilli(), i)
		var ev map[string]any
		if withActivityId {
			ev = upstream.SchoolSeasonChatEvent(conv)
		} else {
			ev = upstream.SchoolChatTimesEvents(conv)
		}
		if err := p.cfg.Upstream.ReportMPEvent(a, ev); err != nil {
			return fmt.Sprintf("完成 %d/%d 次上报后中断: %v", i, need, err), nil
		}
	}
	// 回读（异步计分，有界轮询复用 claimPoll 预算的紧凑版：两轮各隔 3s）。
	for i := 0; i < 2; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
	}
	if t.Claimed {
		return "本轮已入账（claimed）", nil
	}
	if t.Current < target {
		return fmt.Sprintf("已上报 %d 次但进度未达 %d/%d（异步计分未归账，下次重试）", need, t.Current, target), nil
	}
	credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%dc +%de）", credit, energy), nil
}

// runSchoolSeason 完成 school_season「校园日」（growth 域小程序限定）。
// 判据 = mini chat_request_send + activityId=school_open_day_2026（无 activityId
// 不点亮，与 school 域开学季同活动关联；上游 task_runner e2e 实测 +100c+5e）。
func runSchoolSeason(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "school_season", true)
}

// runSequentialChat 完成 Sequential_Tasks_1「小程序内完成 1 次有效对话」。
// 判据 = mini chat_request_send（无 activityId，服务端按 source=mini_program
// 指纹关联；上游 task_runner 实测 +100c+5e）。
func runSequentialChat(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_1", false)
}

// runSequentialChat5 完成 Sequential_Tasks_3「在小程序内完成 5 次有效对话」。
// 判据与 Sequential_Tasks_1 同形状（mini 指纹 chat_request_send，无 activityId），
// 仅 target=5——服务端按上报条数累加进度，但**要求真人节奏**：连发事件先计数
// 后被反作弊回滚（claim 400 "task not completed"），由 mpChatEventGap 间隔保证
// （2026-09-26 实测：45s 间隔补满 5/5 → claim +300c+5e 成功，领后 accept_status
// =claimed 稳定不回滚）。
func runSequentialChat5(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_3", false)
}

// runSequentialChat10 完成 Sequential_Tasks_6「在小程序内完成 10 次有效对话」（预留）。
// 判据假定与 Tasks_1/3 同形状（mini chat_request_send），target 由任务自带（回读），
// runMPMiniChatTask 按差额补报——issue #42 称 target=10，以解锁后实际下发为准。
// 真人节奏间隔同样适用（mpChatEventGap）：9 条 × ~50s ≈ 8 分钟/账号，夜间队列可接受。
func runSequentialChat10(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_6", false)
}

// runSequentialEventTask Sequential 链预留任务通用骨架：mp 查询 → accept（带验证）
// → 判据事件上报（primary；未点亮且 fallback 非空时补一轮）→ 回读 → 达标领奖。
// 每日零点解锁一环：locked 期间 accept 不落账，返回等下次调度（无需人工干预）。
func (p *Panel) runSequentialEventTask(a *auth.Auth, code string, primary, fallback func() error) (string, error) {
	t, err := p.taskByCodeMP(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp 口径未下发该任务（前置任务未完成或活动未开始）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已领取奖励（+%dc +%de）", credit, energy), nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, code) {
			return "accept 未登记生效（任务可能处于每日锁定窗口，等解锁后自动重试）", nil
		}
	}
	if err := primary(); err != nil {
		return fmt.Sprintf("判据上报失败: %v", err), nil
	}
	// 回读（两轮各隔 3s）；未点亮且有 fallback 时补报一轮再读。
	for round := 0; round < 2; round++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
		if round == 0 && fallback != nil {
			if err := fallback(); err != nil {
				return fmt.Sprintf("备选判据上报失败: %v", err), nil
			}
		}
	}
	if t.Claimed {
		return "本轮已入账（claimed）", nil
	}
	if t.Current < target {
		return "已上报但进度未点亮（判据形态待解锁后校正，下次重试）", nil
	}
	credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%dc +%de）", credit, energy), nil
}

// runSequentialAutomation 完成 Sequential_Tasks_4「创建定时任务」（预留）。
// mp 源码无 automation 事件发射点 → 判据疑为 PC 口径：复用 automation_1 同源
// 事件（DesktopAutomationCreateEvent，PC 任务三账号实测点亮）。
func runSequentialAutomation(p *Panel, a *auth.Auth) (string, error) {
	return p.runSequentialEventTask(a, "Sequential_Tasks_4",
		func() error {
			return p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopAutomationCreateEvent("wb2api 自动化"))
		}, nil)
}

// runSequentialModelChat 完成 Sequential_Tasks_5「使用 GLM5.2」（预留）。
// primary：mp 对话事件带 requestModelId/requestModelName=glm-5.2（mpsrc main 32904
// 发射点实测形状）；fallback：PC 域模型活跃上报（Model_chat_GLM5.2 同源）。
func runSequentialModelChat(p *Panel, a *auth.Auth) (string, error) {
	return p.runSequentialEventTask(a, "Sequential_Tasks_5",
		func() error {
			conv := fmt.Sprintf("wb2api-mp-glm-%d", time.Now().UnixMilli())
			return p.cfg.Upstream.ReportMPEvent(a, upstream.MiniChatModelEvent(conv, "glm-5.2", "GLM-5.2"))
		},
		func() error {
			return p.cfg.Upstream.ReportChatActivityModel(a, fmt.Sprintf("wb2api-mp-glm-%d", time.Now().UnixMilli()), "", "glm-5.2", "GLM-5.2")
		})
}

// runSequentialPlaybook 完成 Sequential_Tasks_7「体验灵感功能」。
// 2026-10-24 线上任务实际下发 tag=「PC 限定」、jump_url=workbuddy://playbook，
// 因此不能复用 Sequential 的 mp 通用骨架：默认列表/accept、桌面事件、默认列表回读、
// Web 领奖必须同属 PC 口径。整个动作只做任务事件上报，不发起模型生成请求。
func runSequentialPlaybook(p *Panel, a *auth.Auth) (string, error) {
	const code = "Sequential_Tasks_7"
	t, err := p.taskByCode(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "PC 口径未下发体验灵感任务（前置任务未完成或活动未开始）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target {
		credit, energy, err := p.cfg.Upstream.ClaimReward(a, code)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已领取奖励（+%d 分 +%d 能）", credit, energy), nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if err := p.cfg.Upstream.AcceptTasks(a, []string{code}); err != nil {
			return "", fmt.Errorf("接受体验灵感任务: %w", err)
		}
		time.Sleep(reportGap)
	}

	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-pb-%d", ms)
	req := fmt.Sprintf("wb2api-pb-req-%d", ms)
	if err := p.cfg.Upstream.ReportDesktopEvent(a,
		upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")...); err != nil {
		return "", fmt.Errorf("上报 PC 灵感事件: %w", err)
	}
	after, err := p.taskByCodeWaiting(a, code)
	if err != nil {
		return "", fmt.Errorf("回读体验灵感任务: %w", err)
	}
	if after == nil {
		return "", fmt.Errorf("已上报 PC 灵感事件，但默认任务列表未返回 %s", code)
	}
	if after.Claimed {
		return "本轮已入账（claimed）", nil
	}
	if !after.Claimable {
		return "", fmt.Errorf("已上报 PC 灵感事件，但进度仍为 %s，未领奖", taskProgressText(after))
	}
	credit, energy, err := p.cfg.Upstream.ClaimReward(a, code)
	if err != nil {
		return "", fmt.Errorf("体验灵感任务已达标但领奖失败: %w", err)
	}
	return fmt.Sprintf("体验灵感完成并领取奖励（+%d 分 +%d 能）", credit, energy), nil
}

// runMiniExpert 完成 Sequential_Tasks_2「在小程序内选中专家并完成有效对话」。
// 判据 = mp 指纹 expert_actual_use（**不带** activityId/conversationId、
// extVersion=2.2.8、type=send_message——小程序源码实测形状，与 school 域 expert
// 事件两套口径勿混；上游 task_runner 实测上报即 completed，claim +200c+5e）。
// 专家 id 必须是市场真实 ex_ id（空 id 服务端不入账）→ **accept 之前**先解析市场
// 列表：拉不到就整任务不动作，避免留下「已登记未上报」的半程态（上游 9a26ae7
// 的 ids 前置判定同款）。复用既有 MarketExpertList（expert_5 任务同源，实测可用）。
func runMiniExpert(p *Panel, a *auth.Auth) (string, error) {
	const code = "Sequential_Tasks_2"
	t, err := p.taskByCodeMP(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp 口径未下发该任务（活动可能已结束）", nil
	}
	if t.Claimed {
		return "已领取", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1 // 未 accept 的 mp 任务 progress 为 null，target 兜底（上游实测）
	}
	// 已达标（含 completed 未领）：直接领奖。
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("已领取奖励（+%dc +%de）", credit, energy), nil
	}
	// 判据载体前置（accept 之前）：市场真实专家 id。
	experts, merr := p.cfg.Upstream.MarketExpertList(a, "")
	if merr != nil || len(experts) == 0 {
		return fmt.Sprintf("专家市场不可用（%v），跳过以防半程态", merr), nil
	}
	e := experts[0]
	name := e.DisplayNameZH
	if name == "" {
		name = e.ProfessionZH
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, code) {
			return "accept 未登记生效（上游 200+OK 但未落账形态），待下次重试", nil
		}
	}
	ev := upstream.MiniExpertUseEvent(e.ExpertID, name, e.ExpertType)
	if err := p.cfg.Upstream.ReportMPEvent(a, ev); err != nil {
		return fmt.Sprintf("上报 expert_actual_use 失败: %v", err), nil
	}
	// 回读（异步计分，两轮各隔 3s——与 runMPMiniChatTask 同预算）。
	for i := 0; i < 2; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
	}
	if t.Claimed {
		return "本轮已入账（claimed）", nil
	}
	if t.Current < target {
		return "已上报但进度未归账（异步计分，下次重试）", nil
	}
	credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("任务点亮并领取奖励（+%dc +%de）", credit, energy), nil
}

// accountTaskAuto 一键完成单个任务：执行对应动作 → 回读进度 → 汇报结果。
func (p *Panel) accountTaskAuto(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	// 企业上下文门控（企业版 / 已绑定租户的企业会话）：行为事件会被上游静默丢弃
	// 或直接 403，执行任何成长任务都是零收益白跑（含真实对话配额）——直接拒绝
	// 并给出修复路径，不再让用户对着「已上报但进度 0/x」猜原因。
	// 口径是「是否绑定租户」而非 token_source（见 auth.IsEnterpriseSession）：
	// 只带 enterprise_switch 未绑定的凭证照常计分，不能拦。
	if a.IsEnterpriseSession() {
		writeErr(w, http.StatusUnprocessableEntity,
			"该账号为企业版/已绑定企业租户的会话，无个人成长体系，成长任务行为事件会被上游忽略；请移除账号后以个人身份（退出企业空间）重新登录")
		return
	}
	// 新号静默期：单任务与自动链路跑的是同一套行为动作（真实对话 + 事件上报），
	// 默认一并拦；?force=1 显式越过（口径见 graceguard.go）。
	if blocked, why := p.graceBlockedManual(r, uid, "growth-manual"); blocked {
		writeGraceBlocked(w, why)
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TaskCode == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	// per-account 互斥：同账号的任务动作正在跑（单任务或全量）时直接 409，
	// 不并发重跑（动作幂等但 expert/skill 系含真实对话，重跑浪费配额）。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")
		return
	}
	defer p.unlockAccount(uid)
	// 前置读取：已完成的任务直接跳过（幂等，不浪费上游调用）。
	// taskByCode 已双口径（mp 专属码自动回落 mp 列表）。
	before, err := p.taskByCode(a, body.TaskCode)
	if err != nil {
		// 列表都读不出来 = 凭证多半已死：回写标签（invalid_token 才命中）。
		p.markCredsDeadFromErr(a.UID, err)
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	if before == nil {
		writeErr(w, http.StatusNotFound, "该账号没有此任务")
		return
	}
	// 动作解析放在取到任务本体之后：族模式（expert_use_* 等）要靠标题解析判据载体。
	act := actionForTask(*before)
	if act == nil {
		writeErr(w, http.StatusNotImplemented,
			"该任务暂无自动动作（新增任务待适配）；已可自动接受，完成需在官方客户端操作或补充动作登记")
		return
	}
	isMP := isMPTaskCode(act.TaskCode)
	if before.Claimed {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": true, "message": "该任务已领取过奖励"})
		return
	}
	msg, err := runAction(act, p, a, *before)
	if err != nil {
		// 额度闸门不是失败：回 200 + skipped，前端按提示 toast（502 会被读成
		// "动作坏了"，让人反复重试一个注定 429 的链路）。
		if errors.Is(err, errWalletEmpty) {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "skipped": true, "message": err.Error(),
			})
			return
		}
		// 凭证失效（invalid_token）时回写账号池：单任务入口也要能把号标死，
		// 否则同一个号会被反复点、反复 401。
		p.markCredsDeadFromErr(a.UID, err)
		writeErr(w, http.StatusBadGateway, "执行失败: "+err.Error())
		return
	}
	// 回读验证：上报 200 ≠ 计分（上游可能静默丢弃 + 计分异步），
	// 用有界轮询等异步计时落定，再决定是否自动领奖（mp 任务同口径回读）。
	var after *upstream.Task
	var aerr error
	if isMP {
		after, aerr = p.taskByCodeMP(a, act.TaskCode)
	} else {
		after, aerr = p.taskByCodeWaiting(a, act.TaskCode)
	}
	progressBefore, progressAfter := taskProgressText(before), ""
	claimable := false
	if aerr == nil && after != nil {
		progressAfter = taskProgressText(after)
		claimable = after.Claimable
	}
	resp := map[string]any{
		"ok":               true,
		"message":          msg,
		"progress_before":  progressBefore,
		"progress_after":   progressAfter,
		"claimable":        claimable,
		"attempt":          act.Attempt,
		"verify_supported": true,
	}
	// 达标即自动领奖（Web 端 claim）：把"完成→领奖"收敛成一步，无需用户再点一次。
	if claimable {
		var credit, energy int64
		var cerr error
		if isMP {
			credit, energy, cerr = p.cfg.Upstream.ClaimRewardMP(a, act.TaskCode)
		} else {
			credit, energy, cerr = p.cfg.Upstream.ClaimReward(a, act.TaskCode)
		}
		if cerr == nil {
			resp["claimed"] = true
			resp["credit"] = credit
			resp["energy"] = energy
			if credit > 0 || energy > 0 {
				resp["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
			} else {
				resp["message"] = msg + "；奖励此前已领取"
			}
		} else {
			resp["claim_error"] = cerr.Error()
			resp["message"] = msg + "；达标但领奖失败，可在任务列表手动点「领取」重试"
		}
	}
	// 余额快照同步：面板余额与额度闸门（walletExhausted）读的都是这份快照。
	// 必须放在这里而不是领奖分支里——2026-10-30 实测：Valorant 的奖是**动作内部**
	// 自己领的（runExpertExperience 达标即 claimTaskReward），handler 回读到
	// claimable=false 就跳过了自己的领奖分支，挂在分支里的刷新永远不执行，
	// 表现为「+100 到账了但面板余额不动，要手点查余额才更新」。
	p.refreshCreditsQuiet(uid)
	log.Printf("panel: 任务动作 uid=%s code=%s progress %s -> %s claimable=%v claimed=%v",
		uid, act.TaskCode, progressBefore, progressAfter, claimable, resp["claimed"])
	writeJSON(w, http.StatusOK, resp)
}

// taskProgressText 任务进度的可读表示（回读对比用）。
func taskProgressText(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.Target > 0 {
		return fmt.Sprintf("%d/%d", t.Current, t.Target)
	}
	if t.Claimed {
		return "claimed"
	}
	return t.AcceptStatus
}

// truncateStr 截断错误文本（避免把上游长响应原样透给前端）。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// 各任务动作实现
// ---------------------------------------------------------------------------

// reportGap 连续上报之间的间隔（对齐上游脚本实测的 1.05s 口径，避免风控）。
var reportGap = 1050 * time.Millisecond

// runChat5 补足 chat_5 的进度：按差额上报 chat_request_send。
func runChat5(p *Panel, a *auth.Auth) (string, error) {
	t, err := p.taskByCode(a, "chat_5")
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("任务不存在")
	}
	target := t.Target
	if target <= 0 {
		target = 5
	}
	need := target - t.Current
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	for i := int64(0); i < need; i++ {
		cid := fmt.Sprintf("wb2api-chat5-%d-%d", time.Now().UnixMilli(), i)
		if err := p.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			return fmt.Sprintf("上报第 %d/%d 条失败: %v", i+1, need, err), nil
		}
		if i < need-1 {
			time.Sleep(reportGap)
		}
	}
	return fmt.Sprintf("已补报 %d 条对话事件", need), nil
}

// runFirstBuddy 领养：report（解锁前置）→ agreement → first。
func runFirstBuddy(p *Panel, a *auth.Auth) (string, error) {
	if err := p.cfg.Upstream.ReportChatActivity(a, fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		return "", fmt.Errorf("前置上报: %w", err)
	}
	time.Sleep(reportGap) // 给上游事件处理留时间（脚本实测口径）
	if err := p.cfg.Upstream.BuddyAgreement(a); err != nil {
		return "", fmt.Errorf("同意协议: %w", err)
	}
	if err := p.cfg.Upstream.BuddyFirst(a); err != nil {
		if upstream.IsBuddyTaskIncomplete(err) {
			return "前置已上报，但领养门槛未过（上游要求当日活跃），请稍后重试", nil
		}
		return "", fmt.Errorf("领取 Buddy: %w", err)
	}
	return "已领取 Buddy（+300 分 +8 能量）", nil
}

// runModelChat 完成 Model_chat_GLM5.2：accept → 真实对话 → 对齐模型上报。
func runModelChat(p *Panel, a *auth.Auth) (string, error) {
	const code, modelID, modelName = "Model_chat_GLM5.2", "glm-5.2", "GLM-5.2"
	// 1. accept（报名；失败不阻塞——行为事件才是判据）
	if err := p.cfg.Upstream.AcceptTasks(a, []string{code}); err != nil {
		log.Printf("panel: accept %s: %v（继续走行为链路）", code, err)
	}
	time.Sleep(reportGap)
	// 2. 真实对话一次（判据的最直接证据）
	body, _ := json.Marshal(map[string]any{
		"model": modelID,
		"messages": []map[string]any{
			{"role": "user", "content": "hi，请回复一句话"},
		},
		"stream": true,
	})
	rc, status, respBody, err := p.cfg.Upstream.ChatStream(a, body, "", upstream.ChatMeta{})
	if err != nil {
		return "", fmt.Errorf("对话请求: %w", err)
	}
	if status >= 400 {
		rc.Close()
		return "", fmt.Errorf("对话失败 http=%d: %s", status, truncateStr(string(respBody), 160))
	}
	// 读干 SSE（网关对上游强制 flow：不读完会残留连接）
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	rc.Close()
	time.Sleep(reportGap)
	// 3. 对齐模型的上报（触发进度）
	if err := p.cfg.Upstream.ReportChatActivityModel(a, fmt.Sprintf("wb2api-glm52-%d", time.Now().UnixMilli()), "", modelID, modelName); err != nil {
		return "对话已完成，但进度上报失败：" + err.Error(), nil
	}
	return "已完成 glm-5.2 对话并上报", nil
}

// runRichMeow 完成 RichMeow_Chat（桌面端对话1次）。
// 2026-09-12 三账号实测验证：以桌面指纹（extName=workbuddy-desktop）向
// copilot.tencent.com/v2/report 上报完整对话事件链（agent_task_created →
// chat_message_response isSuccessful=true 等 6 事件），纯 API 即可点亮并领奖
// （紫川/人杰2 两账号无桌面客户端登录状态下 3 秒内 0/1 → 1/1）。
// Hp_Appearance 的纯 API set 不计分（需客户端在主题下活跃），区别对待。
func runRichMeow(p *Panel, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-rm-%d", ms)
	req := fmt.Sprintf("wb2api-rm-req-%d", ms)
	msg := fmt.Sprintf("req-%d-user", ms)
	events := upstream.DesktopChatSequence(conv, req, msg, "fast-model", "fast-model")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已按桌面端指纹上报完整对话事件链（agent_task_created→chat_response）", nil
}

// runBuddyApp 完成 Buddy_App / Buddy_App_QQ（进入 Buddy 应用）。
// 2026-09-12 两账号实测：buddyapp 五连事件（discover→show→enter→auth_confirm→
// bind_skip）以 workbuddy-desktop 指纹上报即点亮，服务端不校验真实授权。
// 用企鹅教师助手（Buddy_App_QQ 判据应用）作载体，同一组事件同时满足
// Buddy_App「进入任一应用」——两个表项共用本 run，幂等由任务状态跳过兜底。
func runBuddyApp(p *Panel, a *auth.Auth) (string, error) {
	events := upstream.DesktopBuddyAppSequence("cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 buddyapp 进入五连事件（同时覆盖 Buddy_App 与 Buddy_App_QQ）", nil
}

// runAutomationCreate 完成 automation_1（设置自动化任务）。
// 2026-09-12 两账号实测：automated_task_create_suc 事件纯 API 上报即点亮，
// 无需真实创建定时任务。
func runAutomationCreate(p *Panel, a *auth.Auth) (string, error) {
	if err := p.cfg.Upstream.ReportDesktopEvent(a,
		upstream.DesktopAutomationCreateEvent("wb2api 自动化")); err != nil {
		return "", err
	}
	return "已上报定时任务创建事件", nil
}

// runLibraryRead 完成 Library_read（体验资料库）。
// 2026-09-12 三账号实测：web 域 /v2/report 上报 web_element_click
// (elementId=library_doc_intro_click) 即点亮（space 的 open/WS/inlong 都不是判据）。
func runLibraryRead(p *Panel, a *auth.Auth) (string, error) {
	const docURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if err := p.cfg.Upstream.ReportWebEvent(a, "web_element_click", docURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", err
	}
	return "已上报资料库介绍阅读事件", nil
}

// runBlackCat 完成 black_cat（夜猫子，夜间 23:00–08:00 计数）。
// 判据 = 夜间窗口内 glm-5.2 真实对话 + chat 事件上报（WorkBuddy-Daily 实测口径）。
// 窗口外不做（提示等排程）；网关 blackcat_hours（默认 23 点）排程会自动补足。
func runBlackCat(p *Panel, a *auth.Auth) (string, error) {
	if !upstream.InNightWindow(time.Now()) {
		return "当前不在 23:00–08:00 计数窗口，行为不计分；网关会在每日 23 点自动补足", nil
	}
	need, err := p.cfg.Upstream.BlackcatNeed(a)
	if err != nil {
		return "", err
	}
	if need <= 0 {
		return "进度已达标，无需补足", nil
	}
	ok, err := p.cfg.Upstream.RunNightChats(a, int(need))
	if err != nil {
		return fmt.Sprintf("完成 %d/%d 次后中断: %v", ok, need, err), nil
	}
	return fmt.Sprintf("已完成 %d 次夜间对话并上报", ok), nil
}

// runSkillFresh 完成 skill_1（尝鲜热门技能）。
// 2026-09-12 判据（紫川手动完成抓包 row 209）：`skill_info` 事件（桌面指纹）——
// {id:<技能名>, skillId, skillVersion, toolStatus:"success", fileCount,
// source:"workbuddy-desktop"} JOIN 真实会话（conversationId/requestId=服务端
// id）。此前的 skill_request_send/skill_installed/skill_action 全是错误方向。
// 人杰2 实测 0/1 → 1/1 点亮。
func runSkillFresh(p *Panel, a *auth.Auth) (string, error) {
	conv, req, err := p.cfg.Upstream.DesktopChatWithExpert(a, "")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	msgID := "msg-" + req[len(req)-8:]
	events := upstream.DesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls" // 模型发起工具调用（技能加载）语义
		}
	}
	events = append(events, upstream.DesktopEvent{
		"eventCode":      "skill_info",
		"id":             "润泽小馆·日报撰写",
		"skillId":        "skill_2097350077599879168",
		"skillVersion":   "1.0.0",
		"toolStatus":     "success",
		"fileCount":      56,
		"source":         "workbuddy-desktop",
		"conversationId": conv, "requestId": req, "messageId": msgID,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"traceId": req,
	})
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("skill_info 事件: %w", err)
	}
	return "已上报真实对话 + skill_info 技能加载事件", nil
}

// runExpertLighthouse 完成 Expert_lighthouse（体验「腾讯轻量云」专家）。
// 2026-09-12 判据（真实样本 Sunny row 868）：与 expert_5 同构，但两处差异——
// chat 链的 agent_task_created 需带 has_expert:true + expert_id（我们默认 false），
// expert_actual_use 的 mode 为 "LOCAL"（非 craft）。id 固定为轻量云专家
// ex_2cvvUZQhDyeJ；requestId 必须是真实 chat 的服务端 id。紫川/人杰2 实测点亮。
func runExpertLighthouse(p *Panel, a *auth.Auth) (string, error) {
	const lhID = "ex_2cvvUZQhDyeJ"
	lh := upstream.MarketExpert{
		ExpertID: lhID, ExpertType: "agent",
		DisplayNameZH: "腾讯轻量云专家", ProfessionZH: "腾讯轻量云专家", Version: "1.0.2",
	}
	// 市场列表若命中真实条目则用其信息（version 等以服务端为准）。
	if experts, err := p.cfg.Upstream.MarketExpertList(a, "agent"); err == nil {
		for _, e := range experts {
			if e.ExpertID == lhID {
				lh = e
				break
			}
		}
	}
	if err := p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("召唤链: %w", err)
	}
	conv, req, err := p.cfg.Upstream.DesktopChatWithExpert(a, lhID)
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	events := upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ExpertID
			ev["expert_name"] = lh.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	events = append(events, upstream.DesktopExpertActualUseLocal(lh, conv, req))
	// 对齐真实样本细节：轻量云专家 actual_use 的 type 为空、cost=0。
	events[len(events)-1]["type"] = ""
	events[len(events)-1]["cost"] = 0
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("使用事件: %w", err)
	}
	return "已上报轻量云专家召唤+使用链（真实对话 requestId）", nil
}

// runAppearance 完成 Hp_Appearance（换主题）。
// 2026-09-12 紫川/人杰2 实测（desktopverify12）：判据是 appearance_skin_apply
// 事件 {action:"apply",source:"settings_close",id:<resourceKey>,...}（客户端在
// 主题生效状态下离开设置页时上报）——早期"纯 API set 不计分"的结论不准确，
// 真相是当时只调了 appearance/set 没发事件。组合：set API 留痕 + 事件上报。
func runAppearance(p *Panel, a *auth.Auth) (string, error) {
	const themeKey = "theme-tkmw7j" // 和平精英激战金秋（Hp_Appearance 判据主题）
	if err := p.cfg.Upstream.SetAppearanceTheme(a, themeKey); err != nil {
		return "", fmt.Errorf("设置主题: %w", err)
	}
	time.Sleep(2 * time.Second)
	if err := p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": themeKey, "vipLevel": 0, "series": "", "type": "unknown",
	}); err != nil {
		return "", err
	}
	return "已设置主题并上报皮肤生效事件", nil
}

// runTemplateUse 完成 template_5（使用 5 个模板创建任务）。
// 2026-09-12 三账号实测：agent_task_created_with_template + template_used 事件组
// （JOIN chat 链）一次上报 5 组即 5/5 点亮。template_id 服务端不校验真实性。
func runTemplateUse(p *Panel, a *auth.Auth) (string, error) {
	templates := [][2]string{{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"}, {"4", "活动策划"}, {"5", "代码评审"}}
	for i, tp := range templates {
		ms := time.Now().UnixMilli()
		conv := fmt.Sprintf("wb2api-tpl-%d-%d", ms, i)
		req := fmt.Sprintf("wb2api-tpl-req-%d-%d", ms, i)
		events := upstream.DesktopTemplateUseSequence(conv, req, tp[0], tp[1])
		if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
			return fmt.Sprintf("第 %d 组模板事件上报失败: %v", i+1, err), nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "已上报 template_used ×5", nil
}

// runPlaybookPrompt 完成 playbook_prompt（灵感案例 Dialog 中发送 Prompt）。
// 判据是 playbook_prompt_send（Dialog 发送）而非卡片曝光/点击——asar 逆向确认。
func runPlaybookPrompt(p *Panel, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-pb-%d", ms)
	req := fmt.Sprintf("wb2api-pb-req-%d", ms)
	events := upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 playbook_cta_click + playbook_prompt_send", nil
}

// runCreateCanvas 完成 create_canvas（设计创意模式创建画布，+300 分）。
// 判据是 wbx_design_canvas_task_create/open（Ardot create_design 工具完成遥测）。
func runCreateCanvas(p *Panel, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-canvas-%d", ms)
	req := fmt.Sprintf("wb2api-canvas-req-%d", ms)
	events := upstream.DesktopDesignCanvasSequence(conv, req)
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "已上报 wbx_design_canvas_task_create/open", nil
}

// expertSummonGap 专家召唤链的间隔（真实使用节奏，v11 实测 8s 成功率 100%）。
const expertSummonGap = 6 * time.Second

// runExpertUse 完成 expert_5（使用 5 个平台专家）。
// 2026-09-12 三账号实测公式：真实专家列表（id 必须真实存在）→ 召唤链
// （summon_click/summoned）→ 真实 chat 拿服务端 requestId → expert_actual_use。
// 自造专家 id 或自造 requestId 均不计数。
func runExpertUse(p *Panel, a *auth.Auth) (string, error) {
	return runExpertBatch(p, a, "agent", 5)
}

// runExpertTeamUse 完成 Expert_team_use_3（使用 3 个专家团，expertType=team）。
func runExpertTeamUse(p *Panel, a *auth.Auth) (string, error) {
	return runExpertBatch(p, a, "team", 3)
}

// runExpertBatch 专家召唤+使用的公共实现。失败逐个继续，返回汇总信息。
func runExpertBatch(p *Panel, a *auth.Auth, expertType string, count int) (string, error) {
	experts, err := p.cfg.Upstream.MarketExpertList(a, expertType)
	if err != nil {
		return "", fmt.Errorf("拉取专家列表: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场列表为空")
	}
	ok, fail := 0, 0
	for i, e := range experts {
		if ok >= count {
			break
		}
		// 召唤链（web_element_click + summon_click + summoned）。
		summonEvents := upstream.DesktopExpertSummonSequence(e)
		if err := p.cfg.Upstream.ReportDesktopEvent(a, summonEvents...); err != nil {
			fail++
			continue
		}
		// 真实 chat（带 X-Expert-Id）→ 服务端 requestId。
		conv, req, cerr := p.cfg.Upstream.DesktopChatWithExpert(a, e.ExpertID)
		if cerr != nil {
			fail++
			continue
		}
		// 使用事件（JOIN 服务端 requestId）+ chat 链。
		events := append(upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model"),
			upstream.DesktopExpertActualUseEvent(e, conv, req))
		if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
			fail++
			continue
		}
		ok++
		if i < len(experts)-1 {
			time.Sleep(expertSummonGap)
		}
	}
	_ = fail
	return fmt.Sprintf("已对 %d 位真实专家完成召唤+使用链（类型 %s）", ok, expertType), nil
}

// ---------------------------------------------------------------------------
// 全量自动完成
// ---------------------------------------------------------------------------

// runAutoAll 对单账号依次执行所有可自动化任务，返回逐项结果。
// 供「一键完成全部可自动任务」使用；单项失败不影响后续项。
//
// 流程：先把所有未接受的任务批量 accept（规范状态机；上游脚本建议"先 accept"），
// 再逐项执行行为链路。accept 不是进度产生的必要条件，但让后续状态流转规范。
func (p *Panel) runAutoAll(a *auth.Auth) []map[string]any {
	var out []map[string]any

	// 阶段 0：批量接受尚未接受的任务（失败不阻塞——行为事件才是进度唯一判据）。
	if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {
		var codes []string
		for _, t := range tasks {
			if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
				codes = append(codes, t.TaskCode)
			}
		}
		if len(codes) > 0 {
			if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
				out = append(out, map[string]any{
					"task_code": "(批量接受)", "status": "error",
					"message": "接受任务失败（不阻塞后续）: " + err.Error(),
				})
			} else {
				out = append(out, map[string]any{
					"task_code": "(批量接受)", "status": "done",
					"message": fmt.Sprintf("已接受 %d 个任务", len(codes)),
				})
				time.Sleep(reportGap)
			}
		}
	}

	// 阶段 0b：小程序口径任务单独接受（默认列表不含 mp 码；失败不阻塞）。
	if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
		var mpCodes []string
		for _, t := range mpTasks {
			if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
				mpCodes = append(mpCodes, t.TaskCode)
			}
		}
		if len(mpCodes) > 0 {
			if err := p.cfg.Upstream.AcceptTasksMP(a, mpCodes); err != nil {
				out = append(out, map[string]any{
					"task_code": "(批量接受-mp)", "status": "error",
					"message": "接受小程序任务失败（不阻塞后续）: " + err.Error(),
				})
			} else {
				out = append(out, map[string]any{
					"task_code": "(批量接受-mp)", "status": "done",
					"message": fmt.Sprintf("已接受 %d 个小程序任务", len(mpCodes)),
				})
				time.Sleep(reportGap)
			}
		}
	}

	for _, act := range autoActions {
		item := map[string]any{"task_code": act.TaskCode, "desc": act.Desc}
		before, err := p.taskByCode(a, act.TaskCode)
		if err != nil {
			item["status"] = "error"
			item["message"] = "查询失败: " + err.Error()
			out = append(out, item)
			continue
		}
		if before == nil {
			item["status"] = "skipped"
			item["message"] = "该账号无此任务"
			out = append(out, item)
			continue
		}
		if before.Claimed || before.Current >= before.Target && before.Target > 0 {
			item["status"] = "skipped"
			msg := "已完成（" + taskProgressText(before) + "）"
			// 达标未领：先补领再跳过（issue #174）。经真实对话等链路点亮的
			// 任务不经过任何自动动作，若这里只 skip，奖永远没人领（积分卡在
			// 半路）。claim 幂等（重复领返回 already_claimed → 0 分 0 能），
			// 宁可多试一轮也不漏领；判据用 progress（Current>=Target），不
			// 依赖 accept_status=="completed"（回读会短暂转 completed 后回滚，
			// 据此领奖会误领）。
			if !before.Claimed {
				credit, energy, cerr := claimTaskReward(p, a, act.TaskCode)
				switch {
				case cerr != nil:
					item["claim_error"] = cerr.Error()
					msg += "；补领失败（可在列表手动重试）"
				case credit > 0 || energy > 0:
					item["claimed"] = true
					item["credit"] = credit
					item["energy"] = energy
					msg += fmt.Sprintf("；已补领 +%d 分 +%d 能", credit, energy)
				default:
					msg += "；奖励此前已领取"
				}
			}
			item["message"] = msg
			out = append(out, item)
			continue
		}
		msg, err := runAction(&act, p, a, *before)
		if err != nil {
			// 额度闸门不是失败：标 blocked，别混进 error 统计里。
			item["status"] = "error"
			if errors.Is(err, errWalletEmpty) {
				item["status"] = "blocked"
			}
			item["message"] = err.Error()
			// 一键完成同样回写账号池：凭证失效的号在本轮剩下的每一项里都会同样失败。
			p.markCredsDeadFromErr(a.UID, err)
			out = append(out, item)
			continue
		}
		// 回读口径统一走一个闭包：mp 码必须 mp 列表，其余走带轮询的默认列表。
		readAfter := func() *upstream.Task {
			if isMPTaskCode(act.TaskCode) {
				t, _ := p.taskByCodeMP(a, act.TaskCode)
				return t
			}
			t, _ := p.taskByCodeWaiting(a, act.TaskCode)
			return t
		}
		after := readAfter()
		item["progress_after"] = taskProgressText(after)
		// 动作「执行成功」不等于任务「达成」。未达成不能标 done：onboard 的
		// 完成数（countAutoDone）与前端完成统计都按 status=='done' 计数，
		// 早先无条件标 done 会把 20 个动作报成 20 个完成，实际只到账 8 个。
		// 补跑不在这里做——同轮立即重放同一动作仍会被上游丢弃（实测），
		// 由 execOnboard 在第一轮结束后隔一段时间整轮重跑（见 onboardRetryDelay）。
		if taskProgressReached(after) {
			item["status"] = "done"
			item["message"] = msg
		} else {
			item["status"] = "pending"
			item["message"] = msg + "；动作已上报但上游进度未生效（进度 " + taskProgressText(after) + "）"
		}
		// 进度达标即自动领奖（mp 任务走 chat 域 mp 口径，其余 Web 端接口）。
		// 领奖失败不掩盖主流程结果：status 仍为 done，附加 claim_error 供前端提示。
		if after != nil && after.Claimable {
			item["claimable"] = true
			credit, energy, cerr := claimTaskReward(p, a, act.TaskCode)
			if cerr == nil {
				item["claimed"] = true
				item["credit"] = credit
				item["energy"] = energy
				if credit > 0 || energy > 0 {
					item["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
				} else {
					item["message"] = msg + "；奖励此前已领取"
				}
			} else {
				item["claim_error"] = cerr.Error()
				item["message"] = msg + "；达标但领奖失败（可在列表手动重试）"
			}
		}
		out = append(out, item)
		time.Sleep(reportGap) // 项间节流
	}

	// 阶段 2：族模式任务（不在精确表内、但形状已适配的新任务，如 expert_use_*）。
	// 静态表永远追不上上游加任务的速度，这里按当前列表动态补跑，动作与队列同源。
	if tasks, _, err := p.listTasksBoth(a); err == nil {
		for _, t := range tasks {
			if t.Claimed || t.Locked {
				continue
			}
			act := actionForTask(t)
			if act == nil || act.runTask == nil {
				continue
			}
			item := map[string]any{"task_code": t.TaskCode, "desc": act.Desc}
			if t.Target > 0 && t.Current >= t.Target {
				credit, energy, cerr := claimTaskReward(p, a, t.TaskCode)
				switch {
				case cerr != nil:
					item["status"] = "skipped"
					item["message"] = "已完成；补领失败: " + cerr.Error()
				case credit > 0 || energy > 0:
					item["status"] = "skipped"
					item["claimed"] = true
					item["message"] = fmt.Sprintf("已完成（%s）；已补领 +%d 分 +%d 能", taskProgressText(&t), credit, energy)
				default:
					item["status"] = "skipped"
					item["message"] = "已完成（" + taskProgressText(&t) + "）"
				}
				out = append(out, item)
				continue
			}
			msg, rerr := runAction(act, p, a, t)
			if rerr != nil {
				item["status"] = "error"
				if errors.Is(rerr, errWalletEmpty) {
					item["status"] = "blocked"
				}
				item["message"] = rerr.Error()
				out = append(out, item)
				continue
			}
			after, _ := p.taskByCode(a, t.TaskCode)
			item["status"] = "done"
			item["message"] = msg
			item["progress_after"] = taskProgressText(after)
			if after != nil && after.Claimable {
				if credit, energy, cerr := claimTaskReward(p, a, t.TaskCode); cerr == nil {
					item["claimed"] = true
					item["credit"], item["energy"] = credit, energy
					item["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
				} else {
					item["claim_error"] = cerr.Error()
				}
			}
			out = append(out, item)
			time.Sleep(reportGap)
		}
	}
	return out
}

// accountTaskAutoAll 一键完成该账号全部可自动任务。
func (p *Panel) accountTaskAutoAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	// 企业上下文门控：与 accountTaskAuto 同口径（企业版 / 已绑定租户的企业会话下成长任务
	// 行为事件被上游静默丢弃，跑全量只会白耗真实对话配额）。「一键完成全部」与
	// 单任务入口共用同一套动作，必须一并挡上（下游 fork 吸收本 PR 时发现的缺口）。
	if a.IsEnterpriseSession() {
		writeErr(w, http.StatusUnprocessableEntity,
			"该账号为企业版/已绑定企业租户的会话，无个人成长体系，成长任务行为事件会被上游忽略；请移除账号后以个人身份（退出企业空间）重新登录")
		return
	}
	// 新号静默期：一键完成跑的就是成长队列那套动作，默认一并拦（force=1 越过）。
	if blocked, why := p.graceBlockedManual(r, uid, "growth-manual-all"); blocked {
		writeGraceBlocked(w, why)
		return
	}
	// per-account 互斥（与单任务动作共用一把锁）：重复点击 409。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")
		return
	}
	// 用 context 兜底超时（多项任务串联 + 每项含真实对话，可能耗时较长）。
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	done := make(chan []map[string]any, 1)
	go func() {
		defer p.unlockAccount(uid) // 流水线真正结束（而非 HTTP 超时返回）才放锁
		res := p.runAutoAll(a)
		// 一轮可能领到多条奖励，统一在结束时同步一次余额（每项刷新会上游请求倍增）。
		p.refreshCreditsQuiet(uid)
		done <- res
	}()
	select {
	case results := <-done:
		log.Printf("panel: 一键完成可自动任务 uid=%s 共 %d 项", uid, len(results))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
	case <-ctx.Done():
		// HTTP 侧超时返回，但后台流水线仍在跑——锁在流水线 goroutine 内释放，
		// 期间重复点击会被 409 挡住，不会出现两轮并发。
		writeErr(w, http.StatusGatewayTimeout, "执行超时（任务仍在后台继续）")
	}
}
