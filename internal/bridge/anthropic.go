// Package bridge 在 OpenAI Chat Completions 与其它对话协议之间做双向翻译。
//
// 为什么需要它：上游只有 OpenAI 形态的 /chat/completions，而真实客户端说的是
// 别的协议——Claude Code / Anthropic SDK 只说 /v1/messages，Codex 只说
// /v1/responses。翻译层只做纯函数转换（请求进来、响应出去），不含任何选号、
// 轮转、冷却、记账逻辑：那些由 server 侧复用 chatCompletions 同一份链路，
// 保证新协议前门与 /v1/chat/completions 的成功/失败处置完全一致。
package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RandID 生成 "<prefix>_<hex>" 形态的协议内 id（msg_/resp_/fc_ 等）。
// 上游不提供可复用的 id 时用它兜底——Anthropic/Responses 客户端都要求 id 非空
// 且同一条消息内稳定，缺 id 会被 SDK 判为畸形响应。
func RandID(prefix string, n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		// 随机源不可用时的确定性兜底：宁可 id 不唯一（仅影响日志归并），
		// 也不要把请求打成 500。
		return prefix + "_" + strings.Repeat("0", n)
	}
	return prefix + "_" + hex.EncodeToString(b)[:n]
}

// Str 把 JSON 解码后的 any 归一成字符串。数字按整值优先（tool 参数里的
// index 之类要能被 fmt 回来），其余类型一律空串（协议字段类型不符时按缺省处理）。
func Str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case json.Number:
		return t.String()
	}
	return ""
}

// IntOf 把 JSON 数字归一成 int（缺失/非法 → 0）。
func IntOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	}
	return 0
}

// Truthy 判定协议里的布尔字段（接受 bool / "true" / 1）。
func Truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	}
	return false
}

// ---------------------------------------------------------------- 请求方向

// BlocksToText 把 Anthropic 的 content（字符串或块数组）压成纯文本。
// 只取 text 与 tool_result 的内容；image 块在这里被丢弃（要保留图片请走
// anthropicContent 的 parts 路径）。
func BlocksToText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, block := range c {
			switch b := block.(type) {
			case string:
				if b != "" {
					parts = append(parts, b)
				}
			case map[string]any:
				switch Str(b["type"]) {
				case "text":
					if t := Str(b["text"]); t != "" {
						parts = append(parts, t)
					}
				case "tool_result":
					if t := BlocksToText(b["content"]); t != "" {
						parts = append(parts, t)
					}
				case "image":
					// 文本兜底路径无法表达图片，用占位符提示模型「这里原本有图」。
					parts = append(parts, "[image]")
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return fmt.Sprint(content)
}

// anthropicImageURL 把 Anthropic 的 image 块转成 OpenAI 可用 URL。
// base64 源拼成 data URL（上游认这种形态），url 源直接透传。
func anthropicImageURL(block map[string]any) string {
	src, _ := block["source"].(map[string]any)
	if src == nil {
		return ""
	}
	switch Str(src["type"]) {
	case "base64":
		data := Str(src["data"])
		if data == "" {
			return ""
		}
		mt := Str(src["media_type"])
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + data
	case "url":
		return Str(src["url"])
	}
	return ""
}

// AnthropicToChat 把 Anthropic /v1/messages 请求翻成 Chat Completions 请求。
//
// 保真度取舍：
//   - system（字符串或块数组）→ 首条 system 消息；cache_control 属 Anthropic
//     的缓存提示，OpenAI 形态无对应字段，丢弃（不影响语义）。
//   - text / image → 文本与 image_url part；tool_use → assistant.tool_calls；
//     tool_result → 独立的 tool 消息（Anthropic 把工具结果放在 user 消息里，
//     OpenAI 要求独立 role=tool，且必须紧跟发起调用的 assistant 消息）。
//   - thinking / redacted_thinking 块丢弃：历史思考链不属于对话内容，上游
//     也不接受回灌。
//   - top_k / metadata / thinking 预算等 Anthropic 专有参数丢弃——不认识的
//     参数硬塞给上游只会换来 11101 bad params。
func AnthropicToChat(req map[string]any) (map[string]any, error) {
	if req == nil {
		return nil, errors.New("请求体为空")
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		return nil, errors.New("messages 不能为空")
	}

	messages := []any{}
	if sys := req["system"]; sys != nil {
		if text := BlocksToText(sys); text != "" {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}

	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := Str(m["role"])
		if role == "" {
			role = "user"
		}
		content := m["content"]
		if s, ok := content.(string); ok {
			messages = append(messages, map[string]any{"role": role, "content": s})
			continue
		}
		blocks, _ := content.([]any)
		if blocks == nil {
			messages = append(messages, map[string]any{"role": role, "content": ""})
			continue
		}

		var texts []string
		var parts []any
		var toolCalls []any
		var toolMsgs []any
		hasImage := false
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			switch Str(block["type"]) {
			case "text":
				if t := Str(block["text"]); t != "" {
					texts = append(texts, t)
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "image":
				if u := anthropicImageURL(block); u != "" {
					hasImage = true
					parts = append(parts, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": u},
					})
				}
			case "tool_use":
				id := Str(block["id"])
				if id == "" {
					id = RandID("call", 12)
				}
				var args []byte
				if block["input"] == nil {
					args = []byte("{}")
				} else {
					args, _ = json.Marshal(block["input"])
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      Str(block["name"]),
						"arguments": string(args),
					},
				})
			case "tool_result":
				toolMsgs = append(toolMsgs, map[string]any{
					"role":         "tool",
					"tool_call_id": Str(block["tool_use_id"]),
					"content":      BlocksToText(block["content"]),
				})
			}
			// thinking / redacted_thinking / document 等：丢弃（见函数注释）。
		}
		// 工具结果必须先落位：OpenAI 要求 role=tool 紧跟发起调用的 assistant 消息。
		// 同一条 Anthropic user 消息里既有 tool_result 又有 text 时（"结果给你，
		// 继续做"），把 text 排在前面会让 tool 消息接在 user 之后，严格的上游实现
		// 直接按 11101 参数错误拒掉整轮请求。
		messages = append(messages, toolMsgs...)
		// tool_result 块出现在 user 消息里，翻译成独立的 tool 消息，
		// 且不产生空的 user 脚手架（否则上游会看到一条空 user 消息）。
		if len(texts) > 0 || len(toolCalls) > 0 {
			out := map[string]any{"role": role}
			switch {
			case hasImage:
				out["content"] = parts
			case len(texts) > 0:
				out["content"] = strings.Join(texts, "\n")
			default:
				out["content"] = nil
			}
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
			}
			messages = append(messages, out)
		}
	}

	out := map[string]any{
		"model":    req["model"],
		"messages": messages,
		"stream":   Truthy(req["stream"]),
	}
	if v, ok := req["max_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	}
	if v, ok := req["temperature"]; ok && v != nil {
		out["temperature"] = v
	}
	if v, ok := req["top_p"]; ok && v != nil {
		out["top_p"] = v
	}
	if seq, ok := req["stop_sequences"].([]any); ok && len(seq) > 0 {
		out["stop"] = seq
	}
	if v, ok := req["metadata"].(map[string]any); ok {
		// Anthropic 的 metadata.user_id 是会话归并提示，OpenAI 侧对应 user 字段，
		// 顺带传给上游可改善会话粘性（上游不用也无害）。
		if u := Str(v["user_id"]); u != "" {
			out["user"] = u
		}
	}

	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, rawTool := range tools {
			t, ok := rawTool.(map[string]any)
			if !ok || Str(t["name"]) == "" {
				continue
			}
			// Anthropic 的内置 server tool（web_search_20250305 之类）没有
			// input_schema，也没有对应的 OpenAI 函数实体——跳过，不伪造。
			schema := t["input_schema"]
			if schema == nil {
				continue
			}
			converted = append(converted, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        Str(t["name"]),
					"description": Str(t["description"]),
					"parameters":  schema,
				},
			})
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}

	if choice, ok := req["tool_choice"].(map[string]any); ok {
		switch Str(choice["type"]) {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			out["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": Str(choice["name"])},
			}
		case "none":
			out["tool_choice"] = "none"
		}
		if Truthy(choice["disable_parallel_tool_use"]) {
			out["parallel_tool_calls"] = false
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- 响应方向

// StopReason 把 OpenAI finish_reason 映射为 Anthropic stop_reason。
func StopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

// ChatToAnthropic 把非流式 Chat Completion 翻成 Anthropic message。
func ChatToAnthropic(obj map[string]any, model string) map[string]any {
	choices, _ := obj["choices"].([]any)
	var choice map[string]any
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	message, _ := choice["message"].(map[string]any)

	content := []any{}
	if r := Str(message["reasoning_content"]); r != "" {
		// signature 留空：Anthropic 客户端把 thinking 块整体当作历史回灌时不校验
		// 签名，但字段必须存在，否则 SDK 解析报错。
		content = append(content, map[string]any{
			"type": "thinking", "thinking": r, "signature": "",
		})
	}
	if t := Str(message["content"]); t != "" {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			var input any = map[string]any{}
			if s := Str(fn["arguments"]); s != "" {
				if err := json.Unmarshal([]byte(s), &input); err != nil {
					// 参数不是合法 JSON：Anthropic 的 input 必须是对象，退化成
					// 把原文塞进 {"_raw": "..."}，至少不让整个响应解析失败。
					input = map[string]any{"_raw": s}
				}
			}
			id := Str(call["id"])
			if id == "" {
				id = RandID("call", 12)
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": id,
				"name": Str(fn["name"]), "input": input,
			})
		}
	}

	usage, _ := obj["usage"].(map[string]any)
	anthUsage := map[string]any{
		"input_tokens":  IntOf(usage["prompt_tokens"]),
		"output_tokens": IntOf(usage["completion_tokens"]),
	}
	// 缓存命中/写入：Anthropic 用 cache_read_input_tokens / cache_creation_input_tokens
	// 单独报，客户端据此算成本（读取按 0.1x、写入按 1.25x，不报就等于按原价算）。
	//
	// 原先只读 prompt_tokens_details.cached_tokens 一个路径：上游把真实命中写在
	// prompt_cache_hit_tokens / cache_read_input_tokens、而兼容别名 cached_tokens
	// 填 0 时，这里就把命中整个丢了。统一走 usageCacheRead（多别名取最优）。
	if c, ok := usageCacheRead(usage); ok {
		anthUsage["cache_read_input_tokens"] = c
	}
	if w := usageCacheWrite(usage); w > 0 {
		anthUsage["cache_creation_input_tokens"] = w
	}
	// 上游的积分扣费透出到响应里，客户端可观测成本（非标准字段，忽略无害）。
	if pts, ok := usage["credit"]; ok {
		anthUsage["credit"] = pts
	}

	return map[string]any{
		"id":            orDefault(Str(obj["id"]), RandID("msg", 24)),
		"type":          "message",
		"role":          "assistant",
		"model":         orDefault(model, Str(obj["model"])),
		"content":       content,
		"stop_reason":   StopReason(Str(choice["finish_reason"])),
		"stop_sequence": nil,
		"usage":         anthUsage,
	}
}

// ---------------------------------------------------------------- 流式方向

// Event 一条待写出的 SSE 事件（event 名 + JSON 数据）。
type Event struct {
	Name string
	Data map[string]any
}

type toolAccum struct {
	ID   string
	Name string
	Args strings.Builder
}

// AnthropicStream 把 OpenAI SSE 增量翻成 Anthropic SSE 事件。
//
// 用法：Start() 发首帧 → 每个上游 chunk 喂 Feed() → Finish() 收尾。
// 工具调用必须攒完再发：Anthropic 的 input_json_delta 要求先有
// content_block_start（含 name/id），而上游把参数分多帧下发。
type AnthropicStream struct {
	Model     string
	MessageID string

	usage        map[string]any
	finishReason string

	textOpen  bool
	thinkOpen bool
	blockIdx  int

	tools  map[int]*toolAccum
	order  []int
	failed bool
}

// NewAnthropicStream 构造翻译器（messageID 为空时自动生成）。
func NewAnthropicStream(model, messageID string) *AnthropicStream {
	if messageID == "" {
		messageID = RandID("msg", 24)
	}
	return &AnthropicStream{
		Model:     model,
		MessageID: messageID,
		usage:     map[string]any{},
		tools:     map[int]*toolAccum{},
	}
}

// Start 发出 message_start。
func (s *AnthropicStream) Start() []Event {
	return []Event{{Name: "message_start", Data: map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.MessageID,
			"type":          "message",
			"role":          "assistant",
			"model":         s.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}}}
}

// Feed 处理一个上游 SSE 增量对象。
func (s *AnthropicStream) Feed(obj map[string]any) []Event {
	var out []Event

	// 上游 error 帧（200 已开流 + 一帧 error：6004 限流 / 内容拦截 / 审核）：
	// 直接翻成 Anthropic 的 error 事件并把流标记为失败，后续不再发正常增量。
	if e, ok := obj["error"].(map[string]any); ok && len(e) > 0 {
		s.failed = true
		return append(out, Event{Name: "error", Data: map[string]any{
			"type":  "error",
			"error": map[string]any{"type": ErrorKind(Str(e["code"]) + " " + Str(e["message"])), "message": ErrorMessage(e)},
		}})
	}

	if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
		for k, v := range u {
			s.usage[k] = v
		}
	}

	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return out
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)

	if thinking := Str(delta["reasoning_content"]); thinking != "" {
		if !s.thinkOpen {
			s.thinkOpen = true
			out = append(out, Event{Name: "content_block_start", Data: map[string]any{
				"type": "content_block_start", "index": s.blockIdx,
				"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
			}})
		}
		out = append(out, Event{Name: "content_block_delta", Data: map[string]any{
			"type": "content_block_delta", "index": s.blockIdx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": thinking},
		}})
	}

	if text := Str(delta["content"]); text != "" {
		if s.thinkOpen {
			s.thinkOpen = false
			out = append(out, Event{Name: "content_block_stop", Data: map[string]any{
				"type": "content_block_stop", "index": s.blockIdx,
			}})
			s.blockIdx++
		}
		if !s.textOpen {
			s.textOpen = true
			out = append(out, Event{Name: "content_block_start", Data: map[string]any{
				"type": "content_block_start", "index": s.blockIdx,
				"content_block": map[string]any{"type": "text", "text": ""},
			}})
		}
		out = append(out, Event{Name: "content_block_delta", Data: map[string]any{
			"type": "content_block_delta", "index": s.blockIdx,
			"delta": map[string]any{"type": "text_delta", "text": text},
		}})
	}

	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			idx := IntOf(call["index"])
			fn, _ := call["function"].(map[string]any)
			acc, exists := s.tools[idx]
			if !exists {
				acc = &toolAccum{}
				s.tools[idx] = acc
				s.order = append(s.order, idx)
			}
			if id := Str(call["id"]); id != "" {
				acc.ID = id
			}
			if name := Str(fn["name"]); name != "" {
				acc.Name = name
			}
			if args := Str(fn["arguments"]); args != "" {
				acc.Args.WriteString(args)
			}
		}
	}

	if fr := Str(choice["finish_reason"]); fr != "" {
		s.finishReason = fr
	}
	return out
}

// Failed 报告本流是否已因上游 error 帧判死（失败后不再发 message_delta/completed）。
func (s *AnthropicStream) Failed() bool { return s.failed }

// Finish 收尾：补完未关闭的块、发出累积的工具调用，再发 message_delta + message_stop。
func (s *AnthropicStream) Finish() []Event {
	if s.failed {
		return nil
	}
	var out []Event

	if s.thinkOpen {
		s.thinkOpen = false
		out = append(out, Event{Name: "content_block_stop", Data: map[string]any{
			"type": "content_block_stop", "index": s.blockIdx,
		}})
		s.blockIdx++
	}
	if s.textOpen {
		s.textOpen = false
		out = append(out, Event{Name: "content_block_stop", Data: map[string]any{
			"type": "content_block_stop", "index": s.blockIdx,
		}})
		s.blockIdx++
	}

	for _, idx := range s.order {
		acc := s.tools[idx]
		if acc == nil || acc.Name == "" {
			continue
		}
		id := acc.ID
		if id == "" {
			id = RandID("call", 12)
		}
		out = append(out, Event{Name: "content_block_start", Data: map[string]any{
			"type": "content_block_start", "index": s.blockIdx,
			"content_block": map[string]any{
				"type": "tool_use", "id": id, "name": acc.Name, "input": map[string]any{},
			},
		}})
		partial := acc.Args.String()
		if partial == "" {
			partial = "{}"
		}
		out = append(out, Event{Name: "content_block_delta", Data: map[string]any{
			"type": "content_block_delta", "index": s.blockIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
		}})
		out = append(out, Event{Name: "content_block_stop", Data: map[string]any{
			"type": "content_block_stop", "index": s.blockIdx,
		}})
		s.blockIdx++
	}

	// 收尾 usage 必须给全三个维度。
	//
	// 原先只有 output_tokens（+credit）：input_tokens 缺失、缓存字段更是完全没有，
	// 客户端（Claude Code 等）看到的用量永远是"0 输入、0 缓存"——实测
	// kimi-k3.1-pro 的网关账本记 70.7% 命中，客户端却一个缓存字段都拿不到。
	//
	// message_start 里的 input_tokens 只能是 0：上游把 usage 放在末帧，开始推流时
	// 还不知道真实值。所以真实用量统一在 message_delta 补全，客户端按累计口径
	// 读取即可（这是桥接 chat→Anthropic 的固有约束，不是可以省的一步）。
	usage := map[string]any{
		"input_tokens":  IntOf(s.usage["prompt_tokens"]),
		"output_tokens": IntOf(s.usage["completion_tokens"]),
	}
	if c, ok := usageCacheRead(s.usage); ok {
		usage["cache_read_input_tokens"] = c
	}
	if w := usageCacheWrite(s.usage); w > 0 {
		usage["cache_creation_input_tokens"] = w
	}
	if v, ok := s.usage["credit"]; ok {
		usage["credit"] = v
	}
	out = append(out,
		Event{Name: "message_delta", Data: map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": StopReason(s.finishReason), "stop_sequence": nil},
			"usage": usage,
		}},
		Event{Name: "message_stop", Data: map[string]any{"type": "message_stop"}},
	)
	return out
}

// Usage 返回累积到的 usage（供记账与调试）。
func (s *AnthropicStream) Usage() map[string]any { return s.usage }

// ---------------------------------------------------------------- 错误

// ErrorMessage 从上游 error 对象里取可读信息。
func ErrorMessage(e map[string]any) string {
	if m := Str(e["message"]); m != "" {
		return m
	}
	if m := Str(e["msg"]); m != "" {
		return m
	}
	if m := Str(e["detail"]); m != "" {
		return m
	}
	return "upstream error"
}

// ErrorKind 按错误文本推断 Anthropic 错误类型。
// 上游错误是自有 code（6004 / queue.full / content_*），没有 Anthropic 的错误
// 分类；按关键词归类比一律 api_error 有用——客户端会按 rate_limit_error /
// overloaded_error 自动退避重试。
func ErrorKind(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "rate") && strings.Contains(t, "limit"),
		strings.Contains(t, "6004"), strings.Contains(t, "too many"):
		return "rate_limit_error"
	case strings.Contains(t, "overload"), strings.Contains(t, "busy"),
		strings.Contains(t, "queue.full"), strings.Contains(t, "capacity"):
		return "overloaded_error"
	}
	return "api_error"
}

// AnthropicErrorType 把 HTTP 状态码映射为 Anthropic 错误类型。
func AnthropicErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
