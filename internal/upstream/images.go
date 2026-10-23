// images.go 图像生成端点：/v2/images/generations 与 /v2/images/edits。
//
// 上游契约（读官方客户端 2026-10 实现）：
//   - 两个端点同域（product.endpoint，CN = www.workbuddy.cn / global = www.workbuddy.ai），
//     与 chat 的 copilot.tencent.com 不同域，故 base 走 webBase。
//   - 请求体是 OpenAI images 形态 + hunyuan 分支的少量扩展字段：
//     generations = {model, prompt, size, n?, footnote?, revise?{value}}；
//     edits       = generations + {image: ["<url|data URI>", ...], input_fidelity?}。
//     hunyuan 分支**不认** response_format / quality / style / background，
//     非 hunyuan 分支则必须带 response_format=b64_json（官方客户端行为）。
//   - 响应是统一信封，图片数组嵌在 data.data 里（不是 OpenAI 顶层 data）：
//     {code, msg, request_id, data:{data:[{url|b64_json, ...}], usage:{credit}}}
//   - code != 0 即使 HTTP 200 也是失败（官方客户端直接抛错）。
//
// 与 chat 的差异只有三点：base 域、长首字节超时、业务错误判定。选号/租约/刷新/
// 冷却一律复用 handler 的既有语义（见 internal/server/images.go）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	// imageGenerationsPath 文生图 / 通用图像生成。
	imageGenerationsPath = "/v2/images/generations"
	// imageEditsPath 图生图（垫图 / 编辑）。
	imageEditsPath = "/v2/images/edits"

	// imageHeaderTimeout 图像请求的「首字节前」硬上限。
	//
	// 不能复用 chat 的 HeaderTimeout（默认 60s）：生图请求在上游排队 + 出图，
	// 官方客户端自身对单次生图的实测 p99 在分钟级。这里放到 15 分钟，
	// 真正的总预算由 handler 的 images.deadline_seconds 控制（默认 420s），
	// 本值只是「绝不无限挂住」的兜底。
	imageHeaderTimeout = 15 * time.Minute

	// imageMaxResponseBytes 单次图像响应的读取上限。
	// b64_json 形态下 4 张 2K 图约 20–40MB，留一倍余量；超过视为异常响应。
	imageMaxResponseBytes = 96 << 20
)

// ImageKind 图像端点类型。
type ImageKind int

const (
	// ImageGeneration 文生图（/v2/images/generations）。
	ImageGeneration ImageKind = iota
	// ImageEdit 图生图（/v2/images/edits）。
	ImageEdit
)

// Path 返回该类型对应的上游路径。
func (k ImageKind) Path() string {
	if k == ImageEdit {
		return imageEditsPath
	}
	return imageGenerationsPath
}

// String 用于日志。
func (k ImageKind) String() string {
	if k == ImageEdit {
		return "edits"
	}
	return "generations"
}

// ImageItem 单张生成结果。字段与官方客户端读取的形态一致：
// 有 b64_json 用 b64_json，否则用 url（二者至少有一个）。
type ImageItem struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// ImageOutcome 一次成功生成的结果。
type ImageOutcome struct {
	Items     []ImageItem
	Credit    float64
	HasCredit bool
	RequestID string
}

// ImageHeaders 图像请求头。
//
// 复用 ChatHeaders 的桌面端全套身份头（与 chat / search 同一客户端指纹），再按
// 官方 ImageService 的差异覆盖三处：
//   - Accept：非流式 JSON（ChatHeaders 为 chat 流设成 event-stream）
//   - Content-Type：application/json
//   - X-Agent-Intent / X-Agent-Runtime：官方 buildConversationHeaders 固定 craft/other
func (c *Client) ImageHeaders(req *http.Request, a *auth.Auth, meta ChatMeta) {
	c.ChatHeaders(req, a, "", meta)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Runtime", "other")
}

// Image 对指定账号执行一次图像生成或编辑。
//
// 只负责「一次上游调用 + 信封解析」，不涉及选号/轮转/冷却（那些属于 handler，
// 与 chat / search 的分层一致）。payload 由 handler 按模型分支构造并白名单化，
// 这里原样序列化——上游对未知字段的态度是忽略或 400，白名单应在入口收敛。
//
// 返回的 error：HTTP 非 2xx 或业务 code != 0 时是 *Error（带 Kind，供 handler
// 决定是否罚号/轮转）；传输层故障是普通 error 或 ErrEgress。
func (c *Client) Image(ctx context.Context, a *auth.Auth, kind ImageKind, payload map[string]any, meta ChatMeta) (*ImageOutcome, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal image payload: %w", err)
	}
	url := c.webBase(a) + kind.Path()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.ImageHeaders(req, a, meta)

	// 出口绑定只解析一次：既用于选 client，也用于出口级熔断记账（与 chat 同口径）。
	binding := c.bindFor(a)
	_, _, hc := c.clientTripleForBinding(binding)
	resp, err := hc.Do(req)
	if err != nil {
		c.noteEgressResult(binding, false)
		if isEgressError(err) {
			return nil, &Error{Kind: ErrEgress, Msg: "egress: " + err.Error()}
		}
		return nil, err
	}
	c.noteEgressResult(binding, true)
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, imageMaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read image response: %w", err)
	}
	if len(raw) > imageMaxResponseBytes {
		return nil, &Error{Kind: ErrServer, Status: resp.StatusCode,
			Msg: fmt.Sprintf("image response exceeds %d MiB", imageMaxResponseBytes>>20)}
	}

	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 400)}
	}

	var env struct {
		Code      int    `json:"code"`
		Msg       string `json:"msg"`
		RequestID string `json:"request_id"`
		// 部分端点用驼峰（视频任务轮询就是 requestId），图像侧两种都兜住。
		RequestIDCamel string `json:"requestId"`
		Data           struct {
			Data  []ImageItem `json:"data"`
			Usage struct {
				Credit float64 `json:"credit"`
			} `json:"usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse image response: %w (body: %s)", err, truncate(string(raw), 200))
	}
	if env.RequestID == "" {
		env.RequestID = env.RequestIDCamel
	}
	if env.Code != 0 {
		// 200 + 业务 code：先让 Classify 认余额/限流/模型等结构化标记；认不出时
		// 按图像业务码补判（6004 模型级限流、11102/11158 模型不可用），最后才归
		// ErrClient（只换号、不罚号），避免把业务拒绝算成账号故障。
		k := Classify(resp.StatusCode, string(raw))
		if k == ErrNone {
			switch {
			case IsModelRateLimit(string(raw)):
				k = ErrSoftRate
			case hasBusinessCode(string(raw), modelBlockCode), hasBusinessCode(string(raw), modelForbiddenCode):
				k = ErrModelBlocked
			default:
				k = ErrClient
			}
		}
		// HTTP 200 的业务错误没有正确的状态码可透传：按分类映射成语义等价的状态，
		// 让 handler 的末端出口不再把「请求被拒」报成 503。
		status := resp.StatusCode
		if status < 400 {
			switch k {
			case ErrHardCredit:
				status = http.StatusPaymentRequired
			case ErrSoftRate:
				status = http.StatusTooManyRequests
			default:
				status = http.StatusBadRequest
			}
		}
		msg := env.Msg
		if msg == "" {
			msg = truncate(string(raw), 300)
		}
		if env.RequestID != "" {
			msg = fmt.Sprintf("%s (code: %d, request_id: %s)", msg, env.Code, env.RequestID)
		} else {
			msg = fmt.Sprintf("%s (code: %d)", msg, env.Code)
		}
		return nil, &Error{Kind: k, Status: status, Msg: msg}
	}
	if len(env.Data.Data) == 0 {
		return nil, &Error{Kind: ErrServer, Status: resp.StatusCode,
			Msg: "image response contains no data"}
	}

	out := &ImageOutcome{
		Items:     env.Data.Data,
		Credit:    env.Data.Usage.Credit,
		HasCredit: env.Data.Usage.Credit > 0,
		RequestID: env.RequestID,
	}
	return out, nil
}
