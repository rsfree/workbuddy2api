// proto_chat.go Anthropic Messages / OpenAI Responses 两个对话前门。
//
// 两个端点的实现都只有三步：解析请求 → 翻成 chat 请求 → 交给 chatCompletions
// 跑既有链路，输出经 bridgeWriter 翻回目标协议。网关的选号、粘性、轮转、
// 冷却、降级、记账因此一行都不用改，也不会与 /v1/chat/completions 走岔。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/bridge"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// messagesChat POST /v1/messages：Anthropic Messages 兼容前门。
// Claude Code / Anthropic SDK 只认这个路径，请求与响应都按 Anthropic 协议翻译。
func (h *Handler) messagesChat(w http.ResponseWriter, r *http.Request) {
	req, err := readProtoBody(r)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	chatBody, err := bridge.AnthropicToChat(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.forwardToChat(w, r, protoAnthropic, chatBody)
}

// responsesChat POST /v1/responses：OpenAI Responses 兼容前门（Codex 等）。
func (h *Handler) responsesChat(w http.ResponseWriter, r *http.Request) {
	req, err := readProtoBody(r)
	if err != nil {
		writeProtoJSONError(w, http.StatusBadRequest, "invalid_request", "请求体解析失败: "+err.Error())
		return
	}
	chatBody, err := bridge.ResponsesToChat(req)
	if err != nil {
		writeProtoJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	h.forwardToChat(w, r, protoResponses, chatBody)
}

// baselineLogTail 命中基线且新增段估算超过该值时打一条 INFO。
//
// 阈值不是拍脑袋：一条普通 user 消息粗估约 100~300 token，正常轮次到不了 500；
// 到了 500 说明客户端一次灌进大段上下文（粘贴长文件、工具输出回填），那时
// 「base + tail」最容易偏，值得留痕。低于此值只进 /status 的统计，不刷日志。
const baselineLogTail = 500

// countTokens POST /v1/messages/count_tokens：Anthropic 的输入 token 预估。
//
// Claude Code 在上下文压缩前会调它；网关侧没有上游分词器，先按字符数粗估
// （bridge.EstimateInputTokens），再用 tokencalib 学到的系数换算成上游口径的
// token 数（系数来自同客户端每次对话响应里的 usage.prompt_tokens）。
// 宁可给量级正确的估算，也不回 404 让客户端走异常分支。
func (h *Handler) countTokens(w http.ResponseWriter, r *http.Request) {
	req, err := readProtoBody(r)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	chatBody, err := bridge.AnthropicToChat(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	idx := bridge.IndexMessages(chatBody)
	key := calibKey(bridge.Str(chatBody["model"]))

	// 首选「会话基线」：同一客户端每轮发的是累积上下文，上一轮的真实
	// usage.prompt_tokens 就是这批消息的真数，只需补上本轮新增的那几条。
	// 完全命中（matched == 全部消息）时连增量都是 0，直接返回上游原话。
	if h.cfg.Baseline != nil {
		if base, matched, ok := h.cfg.Baseline.Lookup(key, idx.Digests); ok {
			tail := idx.TailTokens(matched)
			out := base + h.cfg.TokenCalib.AdjustTail(key, tail)
			// 登记预测：客户端多半紧接着用同一批 messages 发 chat，那次的真实
			// usage.prompt_tokens 会在 Record 里与这条配对，累计成 /status 的
			// token_baseline.pred_by_model.mae——这是判断「估得准不准」的唯一数据源。
			// 同时带上 base 与 tail 的字符粗估：配对后相减即「新增段的真实增量」，
			// 交给 tail 比例单独学习（整段 slope 混着固定开销，不能直接用于增量）。
			h.cfg.Baseline.Predict(key, idx.Digests, out, base, tail)
			// 只在新增段较长时留痕：tail 小（常见的一两条 user）没什么可优化的，
			// 逐次打日志会把 INFO 刷满；tail 大说明客户端在灌大块上下文，正是误差来源。
			if tail >= baselineLogTail {
				log.Printf("INFO: [server] count_tokens model=%s mode=prefix matched=%d/%d base=%d tail=%d out=%d",
					key, matched, len(idx.Digests), base, tail, out)
			}
			writeJSON(w, http.StatusOK, map[string]any{"input_tokens": out})
			return
		}
		// 未命中：首轮、上下文被压缩、换了会话，或客户端每轮都在改 system。
		// 这类事件频率远低于命中，全量记录——线上 hits 为 0 时靠它定位是哪种原因。
		log.Printf("INFO: [server] count_tokens model=%s mode=miss msgs=%d est=%d（无基线匹配：首轮/压缩/换会话）",
			key, len(idx.Digests), idx.Total)
	}

	// 没有匹配的基线（首轮、上下文被压缩、换了会话）：回落到整段系数换算。
	out := h.cfg.TokenCalib.Adjust(key, idx.Total)
	if h.cfg.Baseline != nil {
		// 无基线可用：预测值与 tail 段无从拆分（base=0），只登记整体误差。
		h.cfg.Baseline.Predict(key, idx.Digests, out, 0, 0)
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": out})
}

// calibKey 校准器的模型键，必须与 chat 链路同口径（剥 realm 前缀 + 别名归一）。
// chat 侧传的是 bareModel；这里若直接用请求里的原始 model 名，同一个模型会被
// 写成两个键（"glm-5.2" 与 "cn:glm-5.2"），两边都学不满样本。
func calibKey(model string) string {
	_, bare, _ := resolveModelDetail(model)
	return upstream.CanonicalModel(bare)
}

// forwardToChat 把翻译后的 chat 请求喂给 chatCompletions，输出经 bridgeWriter 翻回。
//
// 用「同进程内再进一次 chatCompletions」而不是复制管线：请求对象是克隆的，
// 只换 URL/Body，其余（Header 里的会话 id、客户端 IP、TraceID）原样带着走，
// 保证粘性、日志来源、上游透传头的行为与直连 chat 完全一致。
func (h *Handler) forwardToChat(w http.ResponseWriter, r *http.Request, kind protoKind, chatBody map[string]any) {
	raw, err := json.Marshal(chatBody)
	if err != nil {
		writeProtoJSONError(w, http.StatusInternalServerError, "marshal_error", err.Error())
		return
	}
	model := bridge.Str(chatBody["model"])

	bw := newBridgeWriter(w, kind, model)
	defer bw.Close()

	inner := r.Clone(r.Context())
	inner.Method = http.MethodPost
	inner.URL.Path = "/v1/chat/completions"
	inner.URL.RawQuery = ""
	inner.RequestURI = "/v1/chat/completions"
	inner.Body = io.NopCloser(bytes.NewReader(raw))
	inner.ContentLength = int64(len(raw))
	inner.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	}
	h.chatCompletions(bw, inner)
}

// readProtoBody 读取并解析协议前门的 JSON 请求体。
func readProtoBody(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errEmptyBody
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return req, nil
}

type protoBodyError string

func (e protoBodyError) Error() string { return string(e) }

const errEmptyBody = protoBodyError("请求体为空")

// writeProtoJSONError 写 OpenAI 形态的错误体（Responses 与 chat 同形）。
func writeProtoJSONError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	raw, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": code},
	})
	_, _ = w.Write(raw)
}

// withAuthProto 协议前门的鉴权包装：失败时按目标协议的错误体回 401，
// 而不是一律 OpenAI 形态（Anthropic SDK 见到非 Anthropic 错误体解析会报
// 更难懂的解析错误，掩盖真正的 401）。
func (h *Handler) withAuthProto(kind protoKind, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			if kind == protoAnthropic {
				writeAnthropicError(w, http.StatusUnauthorized, "missing or invalid API key")
				return
			}
			writeProtoJSONError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// isProtoPath 判断是否为新对话协议前门（请求日志按真实路径记账）。
func isProtoPath(path string) bool {
	return path == "/v1/messages" || path == "/v1/responses" || path == "/v1/messages/count_tokens"
}

// protoModelLabel 请求日志里给协议前门打的类型标签（chat 路径由 chatStat 覆盖，
// 但 count_tokens 不存在模型调用，需要在日志里能区分出来）。
func protoModelLabel(path string) string {
	if path == "/v1/messages/count_tokens" {
		return "count_tokens"
	}
	return ""
}
