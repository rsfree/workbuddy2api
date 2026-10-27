// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
//  3. image_url 归一化（上游只认 OpenAI 对象形态，字符串会 400 code=11101）
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// PrepareBodyOpt 单 pass 改写；sanitize=false 时行为完全还原（仅强制 stream + 归一化 tool_choice）。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, nil, nil)
}

// PrepareBodyOptWithEfforts 在 PrepareBodyOpt 基础上按模型 supportedEfforts 降级 reasoning_effort：
// 仅当请求显式携带且模型不支持该档位时，改为 ≤请求档位的最高支持档；支持档全部高于请求档时取最低档；
// 未知模型/未知档位/未携带该字段一律透传。efforts 为 nil 表示未知（不降级）。
//
// 向后兼容封装：不传 defaultEfforts（无模型声明默认档），thinking.go 回退硬编码 high。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, efforts, nil)
}

// PrepareBodyOptWithEffortsAndDefault 完整管线：efforts 降级 + thinking.go 按
// defaultEfforts（模型声明默认档）补档。defaultEfforts 为 nil 时与旧行为一致
// （deepseek 缺档回退硬编码 high）。
func PrepareBodyOptWithEffortsAndDefault(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	// max_completion_tokens → max_tokens 翻译（吸收上游 PR #116，Closes #117）：
	// OpenAI 规范里 max_tokens 已 deprecated、max_completion_tokens 是新字段；
	// DeepSeek Harness 等新客户端只发别名。WorkBuddy 上游（CN /v2 与 global
	// /console 同源）只认 max_tokens——别名透传会被上游忽略后回落默认输出上限
	// （实测 32000），长流任务被截。
	translateMaxCompletionTokens(obj)
	clampGPTMinMaxTokens(obj)
	// stream_options 仅当 body 未显式带时补 {include_usage: true}（D7）：
	// 官方 CLI 流式必发该字段，上游据此在末帧返回 usage 用量；显式带则不覆盖。
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeToolChoice(obj)
	normalizeToolPatterns(obj)
	normalizeRoles(obj)
	normalizeImageURL(obj)
	dropReasoningContentParts(obj)
	// tool 配对三步（见 tool_pairing.go）：先合并再重排再清理。所有模型一律执行（独立于
	// deepseek-only 的 sanitize 开关）。这是「让请求通过」的安全网——不完整配对的
	// tool_calls/tool 结果会让上游对之后每条消息都返 400，必须先行剔除；
	// 插在结果中间的非 tool 消息（Codex image_resize_notice）同样判配对断裂，
	// 先 repack 挪后，再 cleanup 删孤儿，两侧同口径。
	//
	// 顺序不能换：mergeAdjacentToolCalls 必须最先跑——它把「背靠背的两条
	// assistant.tool_calls」合成一条（部分 agent 客户端回放并行调用的报文形状），是上游
	// deepseek 系模型 11148 的正面修复；先合并再 repack，repack 才看得到完整的一批调用。
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = mergeAdjacentToolCalls(msgs)
		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		// 空串 content 收尾修复（见 repairEmptyContent）：必须排在 cleanup 之后——
		// cleanup 摘掉孤儿 tool_calls 后，一条 content:"" 的 assistant 会变成
		// 「裸空消息」，正是上游 11151 的触发面（tool_pairing_test.go 的
		// 「空name带结果」用例就是这条路径）。
		msgs, _ = repairEmptyContent(msgs)
		// 无改动时两步都返回原 slice，这里回写等于零操作；任一步重排/删除
		// （哪怕后续步骤零改动）也必须落到 obj——不能只在「最后一步改动」时回写，
		// 否则 repack 单独生效的结果会被原 slice 覆盖丢失。
		obj["messages"] = msgs
	}
	// DeepSeek 思维链开关（见 thinking.go）：注入 thinking.type=enabled + 缺档补默认档。
	// 先于 normalizeReasoningEffort 执行：补入的默认档也要走既有降级管线，
	// 模型不支持默认档时自动落到 ≤ 默认档的最高支持档（不出站不合规档位）。
	modelName, _ := obj["model"].(string)
	injectThinking(obj, lookupDefaultEffort(defaultEfforts, modelName))
	normalizeReasoningEffort(obj, efforts)
	// DeepSeek 多轮一致性：assistant 消息带 reasoning 痕迹时回填 reasoning_content
	// （requiresReasoningContentOnAssistantMessages，见 thinking.go）。
	backfillReasoningContent(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为上游
// 认的 max_tokens（吸收上游 PR #116）。规则：显式 max_tokens 优先（别名只删）；
// 别名非正数值（0/null/负数）不翻译（0/null 语义是「未设置」，负数是非法值，
// 翻译等于把垃圾搬进 max_tokens）；非数值别名（字符串等畸形）不翻译（原样
// 透传由上游报 11101 参数错）。两域同口径：CN /v2 与 global /console 是同一套
// API，翻译不分 realm。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens") // 无论翻译与否，别名一律删（减少 body 体积与排障噪音）
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return // 显式 max_tokens 优先：别名只删不译
	}
	// json.Unmarshal 数字 → float64（整数去整后回写，避免 1.28e5 科学计数法/小数
	// 尾巴进上游 body）；其他数值类型防御性兼容（int 家族——手构造 map 的调用方）。
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
}

// gptMinMaxTokens GPT 系上游接受的 max_tokens 下限。
const gptMinMaxTokens = 16

// clampGPTMinMaxTokens 把 GPT 系模型过小的 max_tokens 抬到下限。
//
// 背景：上游 GPT 系（实测 gpt-6-sol / gpt-6-luna / gpt-5.6-sol）对 max_tokens < 16
// 一律 400 code=11133 model_param_invalid（15 拒、16 过，同号同 body 对照）；hy4 等
// 非 GPT 模型无此限制。Claude Code 切模型时发 max_tokens 极小的探针，全号轮转同样
// 被拒 → 客户端 503，模型永远切不过去。账号与 body 其余部分无关，换号无用，只能
// 在发送前修。抬到下限只放宽输出上限、不改语义；未携带字段 / 非数值 / 已达下限一律不动。
func clampGPTMinMaxTokens(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !strings.Contains(strings.ToLower(model), "gpt-") {
		return
	}
	var v int64
	switch n := obj["max_tokens"].(type) {
	case float64:
		v = int64(n)
	case int64:
		v = n
	case int:
		v = int64(n)
	default:
		return
	}
	if v < gptMinMaxTokens {
		obj["max_tokens"] = int64(gptMinMaxTokens)
		log.Printf("max_tokens clamped model=%s %d -> %d", model, v, gptMinMaxTokens)
	}
}

// effortRank 档位从低到高。
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort 按模型 supportedEfforts 降级 reasoning_effort（snake/camel 双字段兼容）。
//   - 请求档位模型支持 → 原样透传
//   - 请求档位不支持 → 改为 ≤请求档位的最高支持档（降级）
//   - 支持档全部高于请求档 → 取最低支持档（偏离最小）
//   - 未知模型/未知档位/未携带字段/模型未缓存 → 一律透传
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// 在 ≤请求档位的支持档里选最高档；命中且与请求不同才改写。
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// 支持档全部高于请求档：取最低支持档。
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化是「协议兼容」（补上游 role 白名单），不是「内容脱敏」，
// 因此有意与 SanitizeFingerprints / sanitize 参数解耦：即使 sanitize=false 也照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeImageURL 兼容 OpenAI chat 多模态内容的两种 image_url 写法。
//
// OpenAI Chat Completions 规范使用对象形态 {"url":"...","detail":"..."}，
// 部分客户端（以及 Responses -> Chat 转换器）会发送字符串形态 "data:..." 或
// "https://..."。WorkBuddy 上游只接受对象形态，字符串会返回 400 code=11101
// "cannot unmarshal string into ... ImageContent"。
//
// 这里只做形状转换：字符串转 {"url": 原值}；已有对象及其中 url/detail/mime_type
// 原样保留；空字符串、缺失值、对象内非法 url 一律不补默认值，让上游返回真实错误。
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				continue
			}
			part["image_url"] = map[string]any{"url": imageURL}
		}
	}
}

// dropReasoningContentParts 剔除 chat content 数组里的 reasoning_text / reasoning 片段。
//
// Codex 只走 /v1/responses：new-api 的 responses→chat 转换器
// （openai_responses_to_openai_chat_completions）会把助手历史里的思考片段原样透传进
// /v1/chat/completions。上游 content 类型白名单不含 reasoning_text，命中即 HTTP 400
// code=11101 "unsupported content type at index 0: reasoning_text"。
//
// 思考片段只是模型私有推理，下游续写不需要，剔除无副作用：
//   - 删除 type 为 reasoning_text / reasoning 的片段（TrimSpace 后比较）
//   - 若删除后 content 为空且该消息无 tool_calls，整条消息一并删除
func dropReasoningContentParts(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(msgs))
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			out = append(out, rawMsg)
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			out = append(out, rawMsg)
			continue
		}
		kept := make([]any, 0, len(parts))
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				kept = append(kept, rawPart)
				continue
			}
			ptype, _ := part["type"].(string)
			if strings.TrimSpace(ptype) == "reasoning_text" || strings.TrimSpace(ptype) == "reasoning" {
				continue
			}
			kept = append(kept, rawPart)
		}
		if len(kept) == 0 {
			if _, hasToolCalls := msg["tool_calls"]; !hasToolCalls {
				continue
			}
		}
		msg["content"] = kept
		out = append(out, rawMsg)
	}
	obj["messages"] = out
}

// ensureConsoleSystem global realm 兜底 system 注入（吸收 PR #45，防 console 域上游 code 11-128）：
// 首条消息非 system 时在 messages 最前补一条 fallback system（"You are a helpful assistant."）。
// 仅对 global 请求调用（CN 现状不动；即使首条就是 system 也不重复注入）。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不在这里二次错误化）。
func ensureConsoleSystem(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	first, ok := msgs[0].(map[string]any)
	if ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return body // 首条已是 system：不注入
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, msgs...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions（唯一能真正禁止调用的手段）
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	// suppressChoice 处理 tool_choice=none：删字段 + 删整个 tools/functions 子树。
	//
	// 🔴 为什么必须删 tools 而不只是发 "none"——2026-10-28 真实请求实测（隔离实例，
	// 单账号，deepseek-v4.1-flash 与 kimi-k3.1-pro 双模型交叉验证）：
	//   · tool_choice="required" 问「1+1 等于几」，3/3 轮都没调工具 —— 若该字段生效，
	//     模型必须强制调用；它没有 ⇒ **上游忽略 tool_choice 的语义**
	//   · 同一对话（上一轮调过该工具）发 "none"：
	//       剥离 tools → 调工具 4/4，prompt 120
	//       保留 tools → 调工具 4/4，prompt 355
	//     两者行为完全相同、后者每轮多花 235 token ⇒ 保留 schema 的收益是零，
	//     而剔除整段 schema 是纯赚（实案单次 115,640 B ≈ 29k token）。
	//   · 剥离后模型仍能凭历史里的那一次调用正确产出 {"city":"北京"} —— 说明它不
	//     依赖 tools 声明来理解 tool_calls/role=tool 历史（早先"剥离会让模型看不懂
	//     历史"的推断不成立，别再按那个思路改回去）。
	//
	// 于是：剥离 tools 是**唯一**能实现 none 语义的手段（字段本身无效），顺带把
	// 整段 schema 从计费里省掉。代价是"上游是否真不调"只能靠工具不可见来保证，
	// 历史里出现过的工具仍有被复用的可能——这是上游不认该字段的必然结果，不是
	// 网关能修的。
	suppressChoice := func() {
		delete(obj, "tool_choice")
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			suppressChoice()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			suppressChoice()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// ToolChoiceNone 报告入站 body 是否声明了 tool_choice=none。
//
// 供可观测侧区分「预期剥离」与「异常丢失」：声明 none 的请求会被
// normalizeToolChoice 剥掉整个 tools 子树，于是入站 prefix_bytes 与上游 prompt
// 必然严重背离——这是设计使然，不该触发前缀丢失告警。两种入站协议都要认：
//   - OpenAI chat：`"tool_choice":"none"` 或 `{"type":"none"}`
//   - Anthropic messages（bridge 转 chat 后同样是 none）：
//     `"tool_choice":{"type":"none"}`
//
// body 不可解析时返回 false（宁可多报，不漏报）。
func ToolChoiceNone(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	switch v := obj["tool_choice"].(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "none")
	case map[string]any:
		typ, _ := v["type"].(string)
		return strings.EqualFold(strings.TrimSpace(typ), "none")
	}
	return false
}

// ToolChoiceRequired 报告入站 body 是否声明了「本回合必须调工具」。
//
// 用于观测「上游是否真的解析了 tool_choice」——2026-10-28 实测发现上游忽略该字段
// 的取值（required 也照样可能不调），但那是当时的实现，可能变化。这条探针让假设
// 的失效点自己暴露：客户端要求 required 却整回合没有任何 tool_calls 时打 WARN。
//
// 三种写法都算 required，与 bridge/anthropic.go 的映射对齐：
//   - OpenAI 字符串 "required"
//   - OpenAI 对象 {"type":"required"}
//   - Anthropic 对象 {"type":"any"}（bridge 会转成 required）
func ToolChoiceRequired(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	switch v := obj["tool_choice"].(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "required")
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		return typ == "required" || typ == "any"
	}
	return false
}

// ToolContext 描述一个入站请求的工具使用上下文，供「模型是否跳过工具」的观测使用。
type ToolContext struct {
	// Declared 本次 body 里显式声明的 tools 数量（0 = 客户端没给工具）。
	// 只数本次声明的，**不含**历史里出现过的名字——两者必须分开，否则
	// 「本次没给工具」会被历史污染成「给了工具」。
	Declared int
	// History 历史消息里已出现过工具使用痕迹（assistant.tool_calls / role=tool /
	// 旧版 role=function + function_call）。用来区分两类"没调用"：
	//   History=false → 首轮，模型判断不需要工具，正常
	//   History=true  → 对话已建立工具模式却中途放弃，可疑
	History bool
}

// InspectTools 解析入站 body 的工具上下文。body 不可解析时返回零值
// （Declared=0 即不参与分母，宁可漏统计也不制造假分母）。
func InspectTools(body []byte) ToolContext {
	var out ToolContext
	if len(body) == 0 {
		return out
	}
	var req struct {
		Tools []struct {
			Type     string `json:"type"`
			Name     string `json:"name"`
			Function *struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Functions []struct {
			Name string `json:"name"`
		} `json:"functions"`
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
			FunctionCall *struct {
				Name string `json:"name"`
			} `json:"function_call"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return out
	}
	// 只数「函数型」工具：Responses 的 web_search 之类内置工具没有函数体，
	// 计入分母会把"本来就不该调函数"的请求算成跳过。
	for _, t := range req.Tools {
		if t.Function != nil && t.Function.Name != "" {
			out.Declared++
			continue
		}
		if t.Name != "" && (t.Type == "" || t.Type == "function") {
			out.Declared++
		}
	}
	out.Declared += len(req.Functions) // 旧版 functions 协议
	for _, m := range req.Messages {
		if m.Role == "tool" || m.Role == "function" {
			out.History = true
			break
		}
		if len(m.ToolCalls) > 0 || m.FunctionCall != nil {
			out.History = true
			break
		}
	}
	return out
}

// normalizeToolPatterns 归一化 tools 子树里 pattern 的非标准转义 `\_`（→ `_`）。
//
// 上游对 tools[].function.parameters 做严格 JSON Schema/正则文法校验，pattern 含
// `\_`（转义的字面量下划线）会整体拒收：400 code=11129 invalid_function_call_
// parameters（displayMsg「工具定义不合规」）。`\_` 不是任何正则文法的合法转义，
// 但所有主流引擎（RE2/PCRE/JS Annex B）都宽容地视为 `_` 本身——上游校验器比它们
// 全部更严（对照 V8 严格文法 u 标志，唯一同样拒绝的实现）。实案：ZCode 的 exa 插件
// agent_run 工具 runId/previousRunId 带 `^agent\_run\_`，deepseek 系全家确定性 400
// → 网关侧归 ErrClient 只换号不罚但喂连败计数 → 轮转烧满 5 连败触发连败降权、
// 客户端 503（2026-09-29/30 两次实案）。schema 级拒绝换账号无用，只能在发送前修。
//
// 归一无损：`\_` 与 `_` 在所有引擎匹配语义相同（各引擎实测 + 上游对照探针：归一后
// 200），工具方功能不变。只动 tools 子树（pattern 值 + patternProperties 键）；
// 消息正文里的 `\_`（如 Windows 路径 C:\_x）不碰。其余非标转义（`\:` 等）未证实
// 触发，不扩面——有实案再议。独立于 sanitize 开关：这是「让请求通过」，不是脱敏。
func normalizeToolPatterns(obj map[string]any) {
	rawTools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI 形态 tools[].function.parameters；裸 tools[].parameters 兼容。
		if fn, ok := tool["function"].(map[string]any); ok {
			unescapePatternLiteralEscapes(fn["parameters"])
		}
		unescapePatternLiteralEscapes(tool["parameters"])
	}
}

// unescapePatternLiteralEscapes 递归改写 schema 树里 pattern 值与 patternProperties
// 键中的 `\_` → `_`（patternProperties 的键也是正则；map 键不可原地改，命中时重建
// 该层）。
func unescapePatternLiteralEscapes(node any) {
	switch n := node.(type) {
	case map[string]any:
		if p, ok := n["pattern"].(string); ok && strings.Contains(p, `\_`) {
			n["pattern"] = strings.ReplaceAll(p, `\_`, `_`)
		}
		if props, ok := n["patternProperties"].(map[string]any); ok {
			rebuilt := false
			fixed := make(map[string]any, len(props))
			for k, v := range props {
				if strings.Contains(k, `\_`) {
					k = strings.ReplaceAll(k, `\_`, `_`)
					rebuilt = true
				}
				fixed[k] = v
			}
			if rebuilt {
				n["patternProperties"] = fixed
			}
		}
		for _, v := range n {
			unescapePatternLiteralEscapes(v)
		}
	case []any:
		for _, v := range n {
			unescapePatternLiteralEscapes(v)
		}
	}
}

// repairEmptyContent 修掉「content 为空」的 assistant/system 消息——上游 11151
// "a message has empty content" 的唯一触发面。
//
// 实测矩阵（2026-10-24，cn:kimi-k2.7 / kimi-k2.7，同 body 逐形态探测；每组探测
// 前清 state.json 重启，避免被罚号污染后续判定）：
//
//	assistant + ""      + 无 tool_calls → 400 11151
//	assistant + null    + 无 tool_calls → 400 11151
//	assistant + 缺键    + 无 tool_calls → 400 11151
//	system    + ""/null/缺键            → 400 11151
//	assistant + ""/null/缺键 + 有 tool_calls → 200（tool_calls 消息的标准形状）
//	assistant + "   "   + 无 tool_calls → 200（空白串不被判空）
//	assistant + []      + 无 tool_calls → 200
//	user      + ""      → 200
//	tool      + ""/null → 200
//
// 判据因此不是「content 为空」，而是「assistant/system 且没有 tool_calls 时 content
// 必须是有内容的字符串」。修法：把这三种空形态换成单空格。
//
// 为什么是单空格而不是删消息：
//   - 条数与顺序零变化，依赖「第 N 条是 user/assistant」的位置语义（prefill、
//     few-shot 交替约束、reasoning 回填）全不受影响；
//   - 单空格对模型等价于无内容，语义不变，且实测各模型均放行；
//   - 删除会改变对话轮次结构，prefill 场景下行为变化不可控。
//
// 刻意不动 tool_calls 消息的 content：null 是它的标准形状，改它反而破坏规范形态。
//
// 这个修复同时消掉一个隐性损失：修复前该 400 被归为 ErrClient → 罚号 + 轮转，
// 一条坏请求连坐 MaxRotate(3) 个健康账号进软冷却；修好后请求直接成功，不再罚号。
// 触发面只限 assistant/system、user/tool 已实测放行，故不扩大改动面。
func repairEmptyContent(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		// 触发面只限 assistant / system（user/tool 空 content 实测放行）。
		// normalizeRoles 已在管线更早处执行，developer 到这里已是 system。
		if role, _ := msg["role"].(string); role != "assistant" && role != "system" {
			continue
		}
		// 带 tool_calls 的消息放行：content null 是它的标准形状，不能改。
		if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
			continue
		}
		switch c := msg["content"].(type) {
		case nil:
			// 同时覆盖「键存在但为 null」与「键缺失」（取值为 nil）。
			msg["content"] = " "
			changed = true
		case string:
			if c == "" {
				msg["content"] = " "
				changed = true
			}
		}
		// "   "（空白串）、[]、正常文本一律不动：实测全部放行，改它们只增加风险面。
	}
	return messages, changed
}
