// fetch.go 网页正文抓取前门 POST /v1/fetch。
//
// 定位：把上游 /agenttool/v1/webfetch 包成一个直接可用的 JSON 端点。与搜索互补——
// 搜索给「有哪些链接」，抓取给「这个链接里到底写了什么」。公众号文章实测 0.2s
// 返回 3.5k 字 markdown 全文，解决搜索只能拿 snippet 的问题。
//
// 轮转语义完全复用 rotateAgentTool（选号 / 在途租约 / token 刷新 / 错误冷却），
// 与 /v1/search/messages 共享同一套账号健康语义。
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	// fetchToolLabel 抓取流量在请求日志「模型」列里的标识（与搜索并列）。
	fetchToolLabel = "WorkBuddy网页抓取"

	// fetchBodyLimit 请求体上限：只有 url + prompt，64KB 富余。
	fetchBodyLimit = 64 << 10
)

// fetchRequest POST /v1/fetch 的入参。
type fetchRequest struct {
	URL    string `json:"url"`
	Prompt string `json:"prompt"`
}

// runFetch 带账号轮转执行一次网页抓取。
// ctx/meta 由调用方传入：搜索的 fetch_content 会并发抓多条，不能各自造会话头。
func (h *Handler) runFetch(ctx context.Context, meta upstream.ChatMeta, target, prompt string) (*upstream.FetchOutcome, *searchFailure) {
	return rotateAgentTool(h, ctx, "fetch", func(acct *auth.Auth) (*upstream.FetchOutcome, error) {
		return h.cfg.Upstream.Fetch(ctx, acct, target, prompt, meta)
	})
}

// webFetch 处理 POST /v1/fetch。
func (h *Handler) webFetch(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, fetchBodyLimit))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	var req fetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object with url/prompt")
		return
	}
	target, verr := upstream.ValidateFetchURL(req.URL)
	if verr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", verr.Error())
		return
	}

	out, fail := h.runFetch(r.Context(), searchMetaFromRequest(r), target, strings.TrimSpace(req.Prompt))
	if fail != nil {
		if tr := requestTraceFromContext(r.Context()); tr != nil {
			tr.model = fetchToolLabel
		}
		msg := fail.msg
		if fail.hint != "" {
			msg = msg + " (" + fail.hint + ")"
		}
		writeOpenAIError(w, fail.status, "api_error", msg)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, http.StatusOK, map[string]any{
		"object":         "web_fetch",
		"url":            out.URL,
		"title":          out.Title,
		"content":        out.Content,
		"content_type":   out.ContentType,
		"content_length": out.ContentLength,
		"truncated":      out.Truncated,
		"status_code":    out.StatusCode,
		// 抓取不消耗模型 token；与搜索保持同一口径，客户端不用区分两种 usage。
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	})
}
