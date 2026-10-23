// fetch.go 网页正文抓取：/agenttool/v1/webfetch。
//
// 上游契约（对齐官方客户端 WebFetchTool，实测 2026-10）：
//   - POST {chatBase}/agenttool/v1/webfetch，同步 JSON
//   - 请求体 {"url":..., "prompt":...}；prompt 是「对抓到的内容要做什么」的指令，
//     但实测不会裁剪返回的 content（传「只提取标题」仍回完整正文），由调用方自行使用
//   - 成功响应为**扁平结构**（非 {code,msg,data} 信封）：
//     {url, title, content, content_type, content_length, truncated, status_code, context}
//   - content 已是 markdown（标题/正文/列表），公众号文章实测 0.2s 返回 3.5k 字全文
//   - 业务错误才带 code/msg，且通常同时伴随 HTTP 4xx
//
// 与 search.go 同一定位：只做「一次上游调用 + 解析」，不涉及选号/轮转/冷却。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// fetchPath 网页抓取端点。与搜索同属 agenttool 家族，base 复用 chatBase。
	fetchPath = "/agenttool/v1/webfetch"

	// fetchTimeout 单次抓取上限。上游对公众号这类重页面实测 0.2–1s，60s 足够；
	// 必须显式设置：抓取是同步 JSON 请求，上游 hang 时无超时会一直占住在途租约。
	fetchTimeout = 60 * time.Second

	// FetchMaxURLLen URL 长度上限。超长 URL 基本是错误用法（把整个页面塞进 query），
	// 在入口拦掉比让上游回一个语焉不详的 400 好。
	FetchMaxURLLen = 4096

	// FetchMaxPromptLen prompt 长度上限（官方 schema 未硬约束，这里给个防御值）。
	FetchMaxPromptLen = 4000

	// fetchDefaultPrompt prompt 为空时的默认指令。上游 schema 要求必填，
	// 网关替调用方补一个中性指令，避免客户端不传就被 400。
	fetchDefaultPrompt = "提取页面正文内容，保留标题与结构化信息"
)

// FetchOutcome 一次网页抓取的结果，字段名与上游响应一致，便于直接透出。
type FetchOutcome struct {
	URL           string `json:"url"`
	Title         string `json:"title,omitempty"`
	Content       string `json:"content"`
	ContentType   string `json:"content_type,omitempty"`
	ContentLength int    `json:"content_length,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	StatusCode    int    `json:"status_code,omitempty"`
}

// ValidateFetchURL 校验抓取目标：只允许 http/https，且必须有主机名。
// 私网地址不在这里拦——上游服务端自己有 isPrivateIP 校验，网关侧再拦会与
// 官方客户端行为不一致（内网文档站是合法用法）。
func ValidateFetchURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("url is required")
	}
	if len(raw) > FetchMaxURLLen {
		return "", fmt.Errorf("url is too long (max %d bytes)", FetchMaxURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("url must use http or https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("url must have a host")
	}
	return raw, nil
}

// Fetch 对指定账号执行一次网页抓取。
//
// 返回的 error 在 HTTP 非 2xx 或业务 code != 0 时是 *Error（带 Kind，供 handler
// 决定是否罚号），其余（序列化/读体失败）是普通 error（传输层故障不罚号）。
func (c *Client) Fetch(ctx context.Context, a *auth.Auth, target, prompt string, meta ChatMeta) (*FetchOutcome, error) {
	target, err := ValidateFetchURL(target)
	if err != nil {
		return nil, &Error{Kind: ErrClient, Msg: err.Error()}
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		prompt = fetchDefaultPrompt
	}
	if len(prompt) > FetchMaxPromptLen {
		prompt = prompt[:FetchMaxPromptLen]
	}
	// 字段顺序与官方客户端一致：url → prompt。
	body, err := json.Marshal(struct {
		URL    string `json:"url"`
		Prompt string `json:"prompt"`
	}{target, prompt})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+fetchPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// 与搜索同一套头族：同属 agenttool，官方客户端也是同一个身份发出的。
	c.SearchHeaders(req, a, "", meta)
	return c.doFetch(a, req)
}

// doFetch 发抓取请求并解析扁平响应（错误分类口径与 doSearch 完全一致）。
func (c *Client) doFetch(a *auth.Auth, req *http.Request) (*FetchOutcome, error) {
	resp, err := c.httpFor(a).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		FetchOutcome
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if parsed.Code != 0 {
		kind := Classify(resp.StatusCode, parsed.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode,
			Msg: fmt.Sprintf("code=%d msg=%s", parsed.Code, truncate(parsed.Msg, 160))}
	}
	out := parsed.FetchOutcome
	if strings.TrimSpace(out.Content) == "" {
		// 200 且 code=0 但正文为空：多半是目标页需要登录/被风控。按请求级错误处理，
		// 不罚账号（换号也拿不到同一页面的内容）。
		return nil, &Error{Kind: ErrClient, Status: resp.StatusCode,
			Msg: fmt.Sprintf("upstream returned empty content for %s (status_code=%d)", out.URL, out.StatusCode)}
	}
	if out.URL == "" {
		out.URL = req.URL.String() // 理论上不会发生；兜底避免对外 url 为空
	}
	if out.ContentLength == 0 {
		out.ContentLength = len(out.Content)
	}
	return &out, nil
}
