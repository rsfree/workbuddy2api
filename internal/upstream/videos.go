// videos.go 视频生成：/v2/videos/generations（提交）与 /v2/videos/tasks（查询）。
//
// 上游契约（对齐官方客户端 VideoService，实测 2026-10 抓码确认）：
//   - 提交：POST {chatBase}/v2/videos/generations
//     body {prompt, model, watermark, seconds?, image_url?, last_image_url?,
//     reference_images[{image_url}], reference_videos[{video_url}],
//     reference_audios[{audio_url}], extra_parameters{resolution,aspect_ratio,enable_audio}}
//     resp {code,msg,requestId,data:{id:"<task_id>"}}
//   - 查询：POST {chatBase}/v2/videos/tasks，body {task_id}
//     resp {code,msg,requestId,data:{status:"completed|failed|...",data:[{url}],usage:{credit}}}
//
// 与 chat/images 不同，视频是**异步任务**：提交只拿 task_id，成片要靠查询轮询。
// 任务归属账号——查询必须用提交时的同一个账号，否则上游会回任务不存在。故
// upstream 层只提供「按账号提交 / 按账号查询」两个原子操作，任务→账号的映射
// 由 handler 维护（见 internal/server/videos.go）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	videoGenerationsPath = "/v2/videos/generations"
	videoTasksPath       = "/v2/videos/tasks"

	// videoSubmitTimeout 提交超时。提交本身只排队，正常 1–3s；给 3 分钟容忍
	// 上游排队抖动，超时即视为传输层故障（不罚号）。
	videoSubmitTimeout = 3 * time.Minute

	// videoQueryTimeout 单次查询超时。查询是轻量状态读取，30s 足够。
	videoQueryTimeout = 30 * time.Second

	// VideoDefaultModel 默认视频模型（官方 product config 的 defaultVideoModel）。
	VideoDefaultModel = "minimax-video-h3"

	// VideoMaxPromptLen 提示词上限（官方按模型能力校验，最长 2500）。
	VideoMaxPromptLen = 2500

	// VideoMaxReferenceImages / Videos / Audios 参考素材上限（取各模型能力的最大值）。
	VideoMaxReferenceImages = 9
	VideoMaxReferenceVideos = 3
	VideoMaxReferenceAudios = 3
)

// VideoSubmitInput 提交一次视频生成所需的参数（已归一）。
//
// 字段名与上游错误码/官方客户端保持一致：image_url 是首帧，last_image_url 是尾帧，
// reference_* 是参考素材（三者互斥规则由模型能力决定，网关只做数量上限校验）。
type VideoSubmitInput struct {
	Model           string
	Prompt          string
	Seconds         int
	ImageURL        string
	LastImageURL    string
	ReferenceImages []string
	ReferenceVideos []string
	ReferenceAudios []string
	Resolution      string
	AspectRatio     string
	EnableAudio     *bool
	Watermark       *bool
}

// VideoAsset 成片地址。上游可能同时给封面/时长等字段，这里只取下载必需的 url。
type VideoAsset struct {
	URL string `json:"url"`
}

// VideoSubmitOutcome 提交结果。
type VideoSubmitOutcome struct {
	TaskID    string
	RequestID string
}

// VideoTaskOutcome 查询结果。
type VideoTaskOutcome struct {
	TaskID    string
	Status    string // completed / failed / 其它（排队中）
	Videos    []VideoAsset
	Credit    float64
	HasCredit bool
	RequestID string
}

// NormalizeVideoInput 归一入参：模型默认值、提示词裁剪、参考素材数量上限。
// 参数级错误返回 error（handler 转 400），不做任何上游调用。
func NormalizeVideoInput(in VideoSubmitInput) (VideoSubmitInput, error) {
	in.Model = strings.TrimSpace(in.Model)
	if in.Model == "" {
		in.Model = VideoDefaultModel
	}
	in.Prompt = strings.TrimSpace(in.Prompt)
	if len(in.Prompt) > VideoMaxPromptLen {
		return in, fmt.Errorf("prompt must not exceed %d characters", VideoMaxPromptLen)
	}
	hasRef := in.ImageURL != "" || in.LastImageURL != "" ||
		len(in.ReferenceImages) > 0 || len(in.ReferenceVideos) > 0
	if in.Prompt == "" && !hasRef {
		return in, fmt.Errorf("prompt is required for text-to-video")
	}
	if in.Seconds < 0 || in.Seconds > 60 {
		return in, fmt.Errorf("seconds must be between 1 and 60")
	}
	if len(in.ReferenceImages) > VideoMaxReferenceImages {
		return in, fmt.Errorf("reference_images supports at most %d items", VideoMaxReferenceImages)
	}
	if len(in.ReferenceVideos) > VideoMaxReferenceVideos {
		return in, fmt.Errorf("reference_videos supports at most %d items", VideoMaxReferenceVideos)
	}
	if len(in.ReferenceAudios) > VideoMaxReferenceAudios {
		return in, fmt.Errorf("reference_audios supports at most %d items", VideoMaxReferenceAudios)
	}
	if in.LastImageURL != "" && in.ImageURL == "" {
		// 官方能力里 lastFrameRequiresFirstFrame=true：只给尾帧没有意义。
		return in, fmt.Errorf("last_image requires image (first frame) to be set")
	}
	return in, nil
}

// videoRef 上游参考素材形态：{"image_url": ...} / {"video_url": ...} / {"audio_url": ...}。
type videoRef struct {
	ImageURL string `json:"image_url,omitempty"`
	VideoURL string `json:"video_url,omitempty"`
	AudioURL string `json:"audio_url,omitempty"`
}

// videoRequestBody 出站请求体。
// 字段顺序对齐官方 buildConfiguredRequest：prompt → model → watermark → seconds →
// image_url → last_image_url → reference_* → extra_parameters。
type videoRequestBody struct {
	Prompt          string         `json:"prompt"`
	Model           string         `json:"model"`
	Watermark       bool           `json:"watermark"`
	Seconds         int            `json:"seconds,omitempty"`
	ImageURL        string         `json:"image_url,omitempty"`
	LastImageURL    string         `json:"last_image_url,omitempty"`
	ReferenceImages []videoRef     `json:"reference_images,omitempty"`
	ReferenceVideos []videoRef     `json:"reference_videos,omitempty"`
	ReferenceAudios []videoRef     `json:"reference_audios,omitempty"`
	ExtraParameters map[string]any `json:"extra_parameters,omitempty"`
}

// SubmitVideo 对指定账号提交一次视频生成任务。
func (c *Client) SubmitVideo(ctx context.Context, a *auth.Auth, in VideoSubmitInput, meta ChatMeta) (*VideoSubmitOutcome, error) {
	in, err := NormalizeVideoInput(in)
	if err != nil {
		return nil, &Error{Kind: ErrClient, Msg: err.Error()}
	}
	body := videoRequestBody{
		Prompt: in.Prompt,
		Model:  in.Model,
		// 默认无水印：官方客户端硬编码 watermark=true，但上游接口支持显式关闭。
		// 需要水印的调用方传 watermark=true 即可（合规标识请自行按需保留）。
		Watermark: false,
		Seconds:   in.Seconds,
		ImageURL:  in.ImageURL,
	}
	if in.Watermark != nil {
		body.Watermark = *in.Watermark
	}
	body.LastImageURL = in.LastImageURL
	for _, u := range in.ReferenceImages {
		body.ReferenceImages = append(body.ReferenceImages, videoRef{ImageURL: u})
	}
	for _, u := range in.ReferenceVideos {
		body.ReferenceVideos = append(body.ReferenceVideos, videoRef{VideoURL: u})
	}
	for _, u := range in.ReferenceAudios {
		body.ReferenceAudios = append(body.ReferenceAudios, videoRef{AudioURL: u})
	}
	extra := map[string]any{}
	if in.Resolution != "" {
		extra["resolution"] = in.Resolution
	}
	if in.AspectRatio != "" {
		extra["aspect_ratio"] = in.AspectRatio
	}
	if in.EnableAudio != nil {
		extra["enable_audio"] = *in.EnableAudio
	}
	if len(extra) > 0 {
		body.ExtraParameters = extra
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, videoSubmitTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+videoGenerationsPath, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	// 与 chat 同一套会话头族：视频提交也是一次对话级调用（上游按会话聚合）。
	c.ChatHeaders(req, a, "", meta)
	req.Header.Set("Accept", "application/json")

	var env struct {
		Code      int    `json:"code"`
		Msg       string `json:"msg"`
		RequestID string `json:"requestId"`
		Data      struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.doVideoJSON(a, req, &env); err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, videoBusinessError(env.Code, env.Msg, env.RequestID, "")
	}
	if env.Data.ID == "" {
		return nil, fmt.Errorf("video submit returned no task id (body: %s)", truncate(env.Msg, 120))
	}
	return &VideoSubmitOutcome{TaskID: env.Data.ID, RequestID: env.RequestID}, nil
}

// QueryVideo 用提交时的同一账号查询任务状态。
func (c *Client) QueryVideo(ctx context.Context, a *auth.Auth, taskID string, meta ChatMeta) (*VideoTaskOutcome, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, &Error{Kind: ErrClient, Msg: "task_id is required"}
	}
	raw, err := json.Marshal(struct {
		TaskID string `json:"task_id"`
	}{taskID})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, videoQueryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+videoTasksPath, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c.ChatHeaders(req, a, "", meta)
	req.Header.Set("Accept", "application/json")

	var env struct {
		Code      int    `json:"code"`
		Msg       string `json:"msg"`
		RequestID string `json:"requestId"`
		Data      struct {
			Status string       `json:"status"`
			Data   []VideoAsset `json:"data"`
			Usage  struct {
				Credit float64 `json:"credit"`
			} `json:"usage"`
		} `json:"data"`
	}
	if err := c.doVideoJSON(a, req, &env); err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, videoBusinessError(env.Code, env.Msg, env.RequestID, taskID)
	}
	out := &VideoTaskOutcome{
		TaskID:    taskID,
		Status:    strings.TrimSpace(env.Data.Status),
		Videos:    env.Data.Data,
		Credit:    env.Data.Usage.Credit,
		RequestID: env.RequestID,
	}
	if out.Videos == nil {
		out.Videos = []VideoAsset{}
	}
	// usage.credit 只有在完成时才有意义；0 是合法值（免费任务），故用「是否出现
	// usage 字段」判断——这里以上游状态为准：completed 即认为 credit 可信。
	if out.Status == "completed" {
		out.HasCredit = true
	}
	return out, nil
}

// doVideoJSON 发请求并解信封。错误分类与 doJSON/doSearch 同一口径。
func (c *Client) doVideoJSON(a *auth.Auth, req *http.Request, dst any) error {
	resp, err := c.httpFor(a).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	return nil
}

// videoBusinessError 把 code != 0 的业务失败转成 *Error：先让 Classify 认
// 余额/限流/模型等结构化标记，认不出时归 ErrClient（换号无用、也不罚号）。
func videoBusinessError(code int, msg, requestID, taskID string) error {
	text := msg
	if requestID != "" {
		text = fmt.Sprintf("%s (code: %d, request_id: %s)", msg, code, requestID)
	} else {
		text = fmt.Sprintf("%s (code: %d)", msg, code)
	}
	if taskID != "" {
		text = fmt.Sprintf("%s [task: %s]", text, taskID)
	}
	kind := Classify(http.StatusOK, msg)
	if kind == ErrNone {
		kind = ErrClient
	}
	return &Error{Kind: kind, Status: http.StatusOK, Msg: text}
}
