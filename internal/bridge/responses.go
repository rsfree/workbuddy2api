// responses.go OpenAI Responses API（Codex 等）↔ Chat Completions 翻译。
//
// 与 anthropic.go 同属协议翻译层：请求方向把 Responses 的 input/instructions
// 折成 chat messages，响应方向把 chat completion 重新包成 response 对象与
// response.* SSE 事件流。
package bridge

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ResponsesToChat 把 Responses 请求翻成 Chat Completions 请求。
//
// 支持的 input 形态：
//   - 字符串：整段当一条 user 消息；
//   - 条目数组：message（role/content，content 内 input_text / input_image /
//     output_text）、function_call、function_call_output。
//
// reasoning 条目（上一轮思考链）与 item_reference 直接跳过：上游不接受回灌，
// 也不影响上下文语义。
func ResponsesToChat(req map[string]any) (map[string]any, error) {
	if req == nil {
		return nil, errors.New("请求体为空")
	}
	messages := []any{}

	if ins := Str(req["instructions"]); ins != "" {
		messages = append(messages, map[string]any{"role": "system", "content": ins})
	}

	switch input := req["input"].(type) {
	case string:
		if strings.TrimSpace(input) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": input})
		}
	case []any:
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch Str(item["type"]) {
			case "message", "":
				role := Str(item["role"])
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					// Responses 的 developer 指令等价于 chat 的 system。
					role = "system"
				}
				content, hasImage := responsesContent(item["content"])
				msg := map[string]any{"role": role}
				if hasImage {
					msg["content"] = content
				} else {
					msg["content"] = ContentToText(item["content"])
				}
				if calls, ok := item["tool_calls"].([]any); ok && len(calls) > 0 {
					msg["tool_calls"] = calls
				}
				messages = append(messages, msg)
			case "function_call":
				args := Str(item["arguments"])
				if args == "" {
					args = "{}"
				}
				callID := Str(item["call_id"])
				if callID == "" {
					callID = Str(item["id"])
				}
				messages = append(messages, map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []any{map[string]any{
						"id":   callID,
						"type": "function",
						"function": map[string]any{
							"name":      Str(item["name"]),
							"arguments": args,
						},
					}},
				})
			case "function_call_output":
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": Str(item["call_id"]),
					"content":      ContentToText(item["output"]),
				})
			}
		}
	case nil:
		// 无 input：交给下面的空校验报错
	default:
		messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprint(input)})
	}
	if len(messages) == 0 {
		return nil, errors.New("input 不能为空")
	}

	out := map[string]any{
		"model":    req["model"],
		"messages": messages,
		"stream":   Truthy(req["stream"]),
	}
	if v, ok := req["max_output_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	} else if v, ok := req["max_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	}
	if v, ok := req["temperature"]; ok && v != nil {
		out["temperature"] = v
	}
	if v, ok := req["top_p"]; ok && v != nil {
		out["top_p"] = v
	}
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, rawTool := range tools {
			t, ok := rawTool.(map[string]any)
			if !ok {
				continue
			}
			// Responses 的工具是扁平形态 {type:"function", name, parameters}；
			// 也兼容已嵌套的 {type:"function", function:{...}}。
			name := Str(t["name"])
			params := t["parameters"]
			desc := Str(t["description"])
			if fn, ok := t["function"].(map[string]any); ok {
				name = Str(fn["name"])
				params = fn["parameters"]
				desc = Str(fn["description"])
			}
			if name == "" {
				continue
			}
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			converted = append(converted, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": name, "description": desc, "parameters": params,
				},
			})
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}
	if v, ok := req["tool_choice"]; ok && v != nil {
		out["tool_choice"] = v
	}
	if v, ok := req["parallel_tool_calls"]; ok && v != nil {
		out["parallel_tool_calls"] = v
	}
	// text.format 是 Responses 的结构化输出写法，等价 chat 的 response_format。
	// 只转 json_schema / json_object 两种有对应物的形态，text 保持缺省。
	if text, ok := req["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			switch Str(format["type"]) {
			case "json_schema":
				rf := map[string]any{
					"type": "json_schema",
					"json_schema": map[string]any{
						"name":   orDefault(Str(format["name"]), "output"),
						"schema": format["schema"],
					},
				}
				if v, ok := format["strict"]; ok {
					rf["json_schema"].(map[string]any)["strict"] = v
				}
				out["response_format"] = rf
			case "json_object":
				out["response_format"] = map[string]any{"type": "json_object"}
			}
		}
	}
	return out, nil
}

// responsesContent 把 Responses 的 content 转成 chat 的 content。
// 只含文本时走纯字符串（消费端更兼容）；含图片时返回 parts 数组。
func responsesContent(content any) (any, bool) {
	items, ok := content.([]any)
	if !ok {
		return nil, false
	}
	var parts []any
	var texts []string
	hasImage := false
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch Str(item["type"]) {
		case "input_text", "output_text", "text", "":
			if t := Str(item["text"]); t != "" {
				texts = append(texts, t)
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "input_image":
			url := Str(item["image_url"])
			if url == "" {
				// 也接受 {"image_url":{"url":...}} 的嵌套写法。
				if m, ok := item["image_url"].(map[string]any); ok {
					url = Str(m["url"])
				}
			}
			if url != "" {
				hasImage = true
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		}
	}
	if hasImage {
		return parts, true
	}
	return strings.Join(texts, "\n"), false
}

// ContentToText 把 Responses 的 content/output（字符串或条目数组）压成纯文本。
func ContentToText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var parts []string
		for _, raw := range c {
			switch item := raw.(type) {
			case string:
				parts = append(parts, item)
			case map[string]any:
				switch Str(item["type"]) {
				case "input_text", "output_text", "text", "":
					if t := Str(item["text"]); t != "" {
						parts = append(parts, t)
					}
				case "input_image":
					parts = append(parts, "[image]")
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return fmt.Sprint(content)
}

// ---------------------------------------------------------------- 响应方向

// ChatToResponse 把非流式 Chat Completion 翻成 Responses 的 response 对象。
func ChatToResponse(obj map[string]any, model string) map[string]any {
	choices, _ := obj["choices"].([]any)
	var choice map[string]any
	if len(choices) > 0 {
		choice, _ = choices[0].(map[string]any)
	}
	message, _ := choice["message"].(map[string]any)

	output := []any{}
	if r := Str(message["reasoning_content"]); r != "" {
		output = append(output, map[string]any{
			"type":    "reasoning",
			"id":      RandID("rs", 16),
			"summary": []any{map[string]any{"type": "summary_text", "text": r}},
		})
	}
	if t := Str(message["content"]); t != "" {
		output = append(output, map[string]any{
			"type":   "message",
			"id":     RandID("msg", 16),
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": t, "annotations": []any{},
			}},
		})
	}
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			args := Str(fn["arguments"])
			if args == "" {
				args = "{}"
			}
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        RandID("fc", 16),
				"call_id":   Str(call["id"]),
				"name":      Str(fn["name"]),
				"arguments": args,
				"status":    "completed",
			})
		}
	}

	usage, _ := obj["usage"].(map[string]any)
	prompt, completion := IntOf(usage["prompt_tokens"]), IntOf(usage["completion_tokens"])
	respUsage := map[string]any{
		"input_tokens":  prompt,
		"output_tokens": completion,
		"total_tokens":  prompt + completion,
	}
	if d, ok := usage["completion_tokens_details"].(map[string]any); ok {
		respUsage["output_tokens_details"] = map[string]any{
			"reasoning_tokens": IntOf(d["reasoning_tokens"]),
		}
	}
	if v, ok := usage["credit"]; ok {
		respUsage["credit"] = v
	}

	name := model
	if m := Str(obj["model"]); m != "" {
		name = m
	}
	status := "completed"
	finish := Str(choice["finish_reason"])
	if finish == "length" {
		status = "incomplete"
	}
	resp := map[string]any{
		"id":                 orDefault(Str(obj["id"]), RandID("resp", 20)),
		"object":             "response",
		"created_at":         createdOf(obj["created"]),
		"status":             status,
		"model":              name,
		"output":             output,
		"usage":              respUsage,
		"error":              nil,
		"incomplete_details": nil,
	}
	if finish == "length" {
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return resp
}

// ResponsesStream 把 chat 增量翻译成 Responses SSE 事件。
//
// output_index 按「条目实际开启顺序」分配并记住：reasoning 与 message 可能
// 只出现其一，硬编码 0/1 会让只输出正文的流把正文标成 index=1，客户端
// 按 index 归并时对不上。
type ResponsesStream struct {
	Model      string
	ResponseID string
	seq        int

	reasoningID  string
	reasoningIdx int
	reasoning    strings.Builder

	messageID  string
	messageIdx int
	text       strings.Builder

	tools  map[int]*toolAccum
	order  []int
	nextID int // 下一个可用 output_index

	usage  map[string]any
	failed bool
}

// NewResponsesStream 构造翻译器（responseID 为空时自动生成）。
func NewResponsesStream(model, responseID string) *ResponsesStream {
	if responseID == "" {
		responseID = RandID("resp", 20)
	}
	return &ResponsesStream{
		Model: model, ResponseID: responseID,
		reasoningIdx: -1, messageIdx: -1,
		usage: map[string]any{}, tools: map[int]*toolAccum{},
	}
}

func (s *ResponsesStream) next() int { s.seq++; return s.seq }

func (s *ResponsesStream) assign() int {
	idx := s.nextID
	s.nextID++
	return idx
}

func (s *ResponsesStream) envelope(status string, output []any) map[string]any {
	usage := map[string]any{
		"input_tokens":  IntOf(s.usage["prompt_tokens"]),
		"output_tokens": IntOf(s.usage["completion_tokens"]),
		"total_tokens":  IntOf(s.usage["prompt_tokens"]) + IntOf(s.usage["completion_tokens"]),
	}
	return map[string]any{
		"id": s.ResponseID, "object": "response", "status": status,
		"model": s.Model, "output": output, "usage": usage,
		"created_at": nowUnix(),
	}
}

// Start 发出 response.created 与 response.in_progress。
func (s *ResponsesStream) Start() []Event {
	base := s.envelope("in_progress", []any{})
	return []Event{
		{Name: "response.created", Data: map[string]any{
			"type": "response.created", "sequence_number": s.next(), "response": base,
		}},
		{Name: "response.in_progress", Data: map[string]any{
			"type": "response.in_progress", "sequence_number": s.next(), "response": base,
		}},
	}
}

// Feed 吃一个 chat 增量。
func (s *ResponsesStream) Feed(obj map[string]any) []Event {
	var out []Event

	// 上游 error 帧：翻成 response.failed 并把流判死（不再发 completed）。
	if e, ok := obj["error"].(map[string]any); ok && len(e) > 0 {
		s.failed = true
		env := s.envelope("failed", []any{})
		env["error"] = map[string]any{
			"code":    Str(e["code"]),
			"message": ErrorMessage(e),
		}
		return append(out, Event{Name: "response.failed", Data: map[string]any{
			"type": "response.failed", "sequence_number": s.next(), "response": env,
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
	if delta == nil {
		return out
	}

	if think := Str(delta["reasoning_content"]); think != "" {
		if s.reasoningID == "" {
			s.reasoningID = RandID("rs", 16)
			s.reasoningIdx = s.assign()
			out = append(out, Event{Name: "response.output_item.added", Data: map[string]any{
				"type": "response.output_item.added", "sequence_number": s.next(),
				"output_index": s.reasoningIdx,
				"item": map[string]any{
					"type": "reasoning", "id": s.reasoningID, "summary": []any{},
				},
			}}, Event{Name: "response.reasoning_summary_part.added", Data: map[string]any{
				"type": "response.reasoning_summary_part.added", "sequence_number": s.next(),
				"item_id": s.reasoningID, "output_index": s.reasoningIdx, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": ""},
			}})
		}
		s.reasoning.WriteString(think)
		out = append(out, Event{Name: "response.reasoning_summary_text.delta", Data: map[string]any{
			"type": "response.reasoning_summary_text.delta", "sequence_number": s.next(),
			"item_id": s.reasoningID, "output_index": s.reasoningIdx, "summary_index": 0,
			"delta": think,
		}})
	}

	if text := Str(delta["content"]); text != "" {
		if s.messageID == "" {
			s.messageID = RandID("msg", 16)
			s.messageIdx = s.assign()
			out = append(out, Event{Name: "response.output_item.added", Data: map[string]any{
				"type": "response.output_item.added", "sequence_number": s.next(),
				"output_index": s.messageIdx,
				"item": map[string]any{
					"type": "message", "id": s.messageID, "role": "assistant",
					"status": "in_progress", "content": []any{},
				},
			}}, Event{Name: "response.content_part.added", Data: map[string]any{
				"type": "response.content_part.added", "sequence_number": s.next(),
				"item_id": s.messageID, "output_index": s.messageIdx, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			}})
		}
		s.text.WriteString(text)
		out = append(out, Event{Name: "response.output_text.delta", Data: map[string]any{
			"type": "response.output_text.delta", "sequence_number": s.next(),
			"item_id": s.messageID, "output_index": s.messageIdx, "content_index": 0,
			"delta": text,
		}})
	}

	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			idx := IntOf(call["index"])
			acc, exists := s.tools[idx]
			if !exists {
				acc = &toolAccum{}
				s.tools[idx] = acc
				s.order = append(s.order, idx)
			}
			fn, _ := call["function"].(map[string]any)
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
	return out
}

// Failed 报告本流是否已因上游 error 帧判死。
func (s *ResponsesStream) Failed() bool { return s.failed }

// Usage 返回累积到的 usage。
func (s *ResponsesStream) Usage() map[string]any { return s.usage }

// Finish 收尾：关闭已开的条目，补发工具调用条目，最后发 response.completed。
func (s *ResponsesStream) Finish() []Event {
	if s.failed {
		return nil
	}
	var out []Event
	finalOutput := make([]any, s.nextID)
	for i := range finalOutput {
		finalOutput[i] = nil
	}

	if s.reasoningID != "" {
		reason := s.reasoning.String()
		out = append(out,
			Event{Name: "response.reasoning_summary_text.done", Data: map[string]any{
				"type": "response.reasoning_summary_text.done", "sequence_number": s.next(),
				"item_id": s.reasoningID, "output_index": s.reasoningIdx, "summary_index": 0,
				"text": reason,
			}},
			Event{Name: "response.output_item.done", Data: map[string]any{
				"type": "response.output_item.done", "sequence_number": s.next(),
				"output_index": s.reasoningIdx,
				"item": map[string]any{
					"type": "reasoning", "id": s.reasoningID,
					"summary": []any{map[string]any{"type": "summary_text", "text": reason}},
				},
			}},
		)
		finalOutput[s.reasoningIdx] = map[string]any{
			"type": "reasoning", "id": s.reasoningID,
			"summary": []any{map[string]any{"type": "summary_text", "text": reason}},
		}
	}

	if s.messageID != "" {
		text := s.text.String()
		item := map[string]any{
			"type": "message", "id": s.messageID, "role": "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": text, "annotations": []any{},
			}},
		}
		out = append(out,
			Event{Name: "response.output_text.done", Data: map[string]any{
				"type": "response.output_text.done", "sequence_number": s.next(),
				"item_id": s.messageID, "output_index": s.messageIdx, "content_index": 0,
				"text": text,
			}},
			Event{Name: "response.content_part.done", Data: map[string]any{
				"type": "response.content_part.done", "sequence_number": s.next(),
				"item_id": s.messageID, "output_index": s.messageIdx, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
			}},
			Event{Name: "response.output_item.done", Data: map[string]any{
				"type": "response.output_item.done", "sequence_number": s.next(),
				"output_index": s.messageIdx, "item": item,
			}},
		)
		finalOutput[s.messageIdx] = item
	}

	for _, idx := range s.order {
		acc := s.tools[idx]
		if acc == nil || acc.Name == "" {
			continue
		}
		callID := acc.ID
		if callID == "" {
			callID = RandID("call", 12)
		}
		args := acc.Args.String()
		if args == "" {
			args = "{}"
		}
		item := map[string]any{
			"type": "function_call", "id": RandID("fc", 16), "call_id": callID,
			"name": acc.Name, "arguments": args, "status": "completed",
		}
		itemIdx := s.assign()
		out = append(out,
			Event{Name: "response.output_item.added", Data: map[string]any{
				"type": "response.output_item.added", "sequence_number": s.next(),
				"output_index": itemIdx, "item": item,
			}},
			Event{Name: "response.output_item.done", Data: map[string]any{
				"type": "response.output_item.done", "sequence_number": s.next(),
				"output_index": itemIdx, "item": item,
			}},
		)
		if itemIdx >= len(finalOutput) {
			finalOutput = append(finalOutput, make([]any, itemIdx-len(finalOutput)+1)...)
		}
		finalOutput[itemIdx] = item
	}

	// 空的 output_index（只出现工具调用、没有正文/思考时不会发生；防御性剔除）。
	clean := make([]any, 0, len(finalOutput))
	for _, item := range finalOutput {
		if item != nil {
			clean = append(clean, item)
		}
	}

	env := s.envelope("completed", clean)
	if v, ok := s.usage["credit"]; ok {
		env["usage"].(map[string]any)["credit"] = v
	}
	out = append(out, Event{Name: "response.completed", Data: map[string]any{
		"type": "response.completed", "sequence_number": s.next(),
		"response": env,
	}})
	return out
}

// createdOf 取响应里的 created（秒级时间戳）；缺失时用当前时间。
func createdOf(v any) int64 {
	if n := IntOf(v); n > 0 {
		return int64(n)
	}
	return nowUnix()
}

// EstimateInputTokens 粗估请求的输入 token 数（供 /v1/messages/count_tokens）。
//
// 为什么不在网关侧做真 tokenize：上游没有暴露分词器，引入第三方 BPE 也未必与
// 上游一致；客户端（Claude Code）只用这个数字做上下文压缩决策，量级正确即可。
// 口径按「中文约 1 字/token，英文约 4 字符/token」折中取 rune/3，并对每条消息
// 加固定开销（role/分隔符）。
// EstimateInputTokens 估算一个 chat 请求的输入 token 数（messages + tools）。
//
// 上游没有分词接口，只能按字符数估：中文约 1.5 字/token、英文约 4、代码约 3.5，
// 所以取 runes/3 做粗估，再由 tokencalib 用真实 usage.prompt_tokens 校准。
func EstimateInputTokens(chatBody map[string]any) int {
	return estimateMessages(chatBody) + estimateTools(chatBody)
}

// estimateMessages 估算 messages 数组本身的 token（不含 tools）。
func estimateMessages(chatBody map[string]any) int {
	total := 0
	msgs, _ := chatBody["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		total += messageTokens(m)
	}
	return total
}

// messageTokens 单条消息的 token 估算：固定 4（role/分隔符开销）+ 正文 + 工具调用参数。
func messageTokens(m map[string]any) int {
	text := ContentToText(m["content"])
	if s, ok := m["content"].(string); ok {
		text = s
	}
	n := 4 + guessTokens(text)
	if calls, ok := m["tool_calls"].([]any); ok {
		if b, err := json.Marshal(calls); err == nil {
			n += guessTokens(string(b))
		}
	}
	return n
}

// estimateTools 估算 tools 声明的 token（整段 JSON）。
func estimateTools(chatBody map[string]any) int {
	tools, ok := chatBody["tools"].([]any)
	if !ok || len(tools) == 0 {
		return 0
	}
	b, err := json.Marshal(tools)
	if err != nil {
		return 0
	}
	return guessTokens(string(b))
}

// TokenIndex 一次遍历 messages 得到的索引，供 count_tokens 做「前缀基线 + 尾部增量」。
//
// 单独成类型而不是让调用方各算一遍：count_tokens 既要拿每条消息的指纹去和
// 历史对话做前缀匹配，又要知道匹配位置之后的增量估算，两件事都要逐条遍历。
type TokenIndex struct {
	// Digests 每条消息的指纹（顺序敏感，见 digestMessage）。
	Digests []uint64
	// Prefix Prefix[i] = 前 i 条消息的估算累计（不含 tools），长度 = len(Digests)+1。
	Prefix []int
	// Total 全量估算：messages + tools。
	Total int
}

// IndexMessages 构造 TokenIndex。
func IndexMessages(chatBody map[string]any) TokenIndex {
	msgs, _ := chatBody["messages"].([]any)
	idx := TokenIndex{
		Digests: make([]uint64, 0, len(msgs)),
		Prefix:  make([]int, 0, len(msgs)+1),
	}
	idx.Prefix = append(idx.Prefix, 0)
	total := 0
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		total += messageTokens(m)
		idx.Prefix = append(idx.Prefix, total)
		idx.Digests = append(idx.Digests, digestMessage(m))
	}
	idx.Total = total + estimateTools(chatBody)
	return idx
}

// TailTokens 返回第 from 条消息之后的估算（不含 tools）。
//
// 基线里已经含了 tools 与 system 模板的固定开销，增量只算「新增消息」；
// 再加 tools 会把那部分重复计一次。
func (x TokenIndex) TailTokens(from int) int {
	if from < 0 {
		from = 0
	}
	if from >= len(x.Prefix) {
		return 0
	}
	return x.Prefix[len(x.Prefix)-1] - x.Prefix[from]
}

// digestMessage 单条消息的指纹（64 位，取 SHA-256 前 8 字节）。
//
// 为什么用内容摘要而不是「role + 长度」：前缀匹配要求「同一条消息」能被认出来，
// 长度相同但内容不同的两条消息必须区分开，否则会拿错误的历史基线去回答。
func digestMessage(m map[string]any) uint64 {
	b, err := json.Marshal(m)
	if err != nil {
		return 0
	}
	sum := sha256.Sum256(b)
	return binary.BigEndian.Uint64(sum[:8])
}

// guessTokens 按字符数估算 token 数（见 EstimateInputTokens 注释）。
func guessTokens(text string) int {
	if text == "" {
		return 0
	}
	n := len([]rune(text))
	return (n + 2) / 3
}
