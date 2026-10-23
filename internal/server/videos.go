// videos.go 视频生成前门：POST /v1/videos/generations（提交）与
// GET /v1/videos/{id}（查询）。
//
// 为什么不用「一次请求等到成片」：上游是异步任务（提交拿 task_id，成片靠轮询），
// 一次请求同步等会长时间占住在途租约，且客户端超时后重试会重复扣费。故拆成
// 提交 / 查询两步，与上游语义一一对应。
//
// 任务归属：查询必须用**提交时的同一账号**（上游按账号隔离任务），因此网关在内存里
// 维护 task_id → 账号 的映射。进程重启会丢映射（任务仍在跑，但查不到），这是
// 有意的取舍：不落盘凭据到任务文件里，避免多一份敏感数据。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

const (
	// videoToolLabel 视频流量在请求日志「模型」列里的兜底标识（提交时会替换成真实模型名）。
	videoToolLabel = "WorkBuddy视频生成"

	// videoBodyLimit 提交请求体上限：提示词 2.5k 字 + 若干参考 URL，256KB 富余。
	videoBodyLimit = 256 << 10

	// videoTaskTTL 任务映射保留时长。视频任务典型 1–10 分钟完成，2 小时足够
	// 客户端轮询与补查；过期后查询返回 404（任务本身仍在上游）。
	videoTaskTTL = 2 * time.Hour
)

// videoTaskEntry 一次视频任务的内存登记项。
type videoTaskEntry struct {
	acct    *auth.Auth
	model   string
	created time.Time
	// recorded 成片积分只记一次：客户端会反复轮询，重复记账会把用量统计放大。
	recorded bool
}

// rememberVideoTask 登记 task → 账号，并顺带清理过期项。
func (h *Handler) rememberVideoTask(taskID string, acct *auth.Auth, model string) {
	if taskID == "" || acct == nil {
		return
	}
	h.videoMu.Lock()
	defer h.videoMu.Unlock()
	if h.videoTasks == nil {
		h.videoTasks = make(map[string]*videoTaskEntry)
	}
	now := time.Now()
	for id, e := range h.videoTasks {
		if now.Sub(e.created) > videoTaskTTL {
			delete(h.videoTasks, id)
		}
	}
	h.videoTasks[taskID] = &videoTaskEntry{acct: acct, model: model, created: now}
}

// lookupVideoTask 取任务登记项；不存在（或已过期）返回 false。
func (h *Handler) lookupVideoTask(taskID string) (*videoTaskEntry, bool) {
	h.videoMu.Lock()
	defer h.videoMu.Unlock()
	e, ok := h.videoTasks[taskID]
	if !ok {
		return nil, false
	}
	if time.Since(e.created) > videoTaskTTL {
		delete(h.videoTasks, taskID)
		return nil, false
	}
	return e, true
}

// videoRequest POST /v1/videos/generations 入参。
//
// 同时接受官方字段（image_url / last_image_url）和更符合直觉的短名（image /
// last_image）；参考素材既可以是字符串数组，也可以是 [{"image_url": ...}]。
type videoRequest struct {
	Model           string            `json:"model"`
	Prompt          string            `json:"prompt"`
	Seconds         int               `json:"seconds"`
	Image           string            `json:"image"`
	ImageURL        string            `json:"image_url"`
	LastImage       string            `json:"last_image"`
	LastImageURL    string            `json:"last_image_url"`
	ReferenceImages []json.RawMessage `json:"reference_images"`
	ReferenceVideos []json.RawMessage `json:"reference_videos"`
	ReferenceAudios []json.RawMessage `json:"reference_audios"`
	Resolution      string            `json:"resolution"`
	AspectRatio     string            `json:"aspect_ratio"`
	EnableAudio     *bool             `json:"enable_audio"`
	Watermark       *bool             `json:"watermark"`
}

// mediaRefURL 从参考素材项里取 URL：支持 "https://..." 与 {"image_url":"..."}
// / {"video_url":"..."} / {"audio_url":"..."} / {"url":"..."} 四种写法。
func mediaRefURL(raw json.RawMessage, key string) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for _, k := range []string{key, "url"} {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func mediaRefList(raw []json.RawMessage, key string) []string {
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if u := mediaRefURL(item, key); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// buildVideoInput 把入参归一成 upstream.VideoSubmitInput（只做字段合并与别名处理，
// 校验交给 upstream.NormalizeVideoInput，保证两个前门口径一致）。
func buildVideoInput(req videoRequest) upstream.VideoSubmitInput {
	first := strings.TrimSpace(req.ImageURL)
	if first == "" {
		first = strings.TrimSpace(req.Image)
	}
	last := strings.TrimSpace(req.LastImageURL)
	if last == "" {
		last = strings.TrimSpace(req.LastImage)
	}
	return upstream.VideoSubmitInput{
		Model:           req.Model,
		Prompt:          req.Prompt,
		Seconds:         req.Seconds,
		ImageURL:        first,
		LastImageURL:    last,
		ReferenceImages: mediaRefList(req.ReferenceImages, "image_url"),
		ReferenceVideos: mediaRefList(req.ReferenceVideos, "video_url"),
		ReferenceAudios: mediaRefList(req.ReferenceAudios, "audio_url"),
		Resolution:      strings.TrimSpace(req.Resolution),
		AspectRatio:     strings.TrimSpace(req.AspectRatio),
		EnableAudio:     req.EnableAudio,
		Watermark:       req.Watermark,
	}
}

// videoGenerations 处理 POST /v1/videos/generations：提交任务，立即返回 task id。
func (h *Handler) videoGenerations(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, videoBodyLimit))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	var req videoRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	in, err := upstream.NormalizeVideoInput(buildVideoInput(req))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	var used *auth.Auth
	meta := searchMetaFromRequest(r)
	out, fail := rotateAgentTool(h, r.Context(), "video-submit", func(acct *auth.Auth) (*upstream.VideoSubmitOutcome, error) {
		used = acct // 成功时即出票账号；查询必须用它
		return h.cfg.Upstream.SubmitVideo(r.Context(), acct, in, meta)
	})
	if fail != nil {
		msg := fail.msg
		if fail.hint != "" {
			msg = msg + " (" + fail.hint + ")"
		}
		writeOpenAIError(w, fail.status, "api_error", msg)
		return
	}
	h.rememberVideoTask(out.TaskID, used, in.Model)

	if tr := requestTraceFromContext(r.Context()); tr != nil {
		tr.model = in.Model
	}
	w.Header().Set("X-WB2A-Video-Task", out.TaskID)
	if out.RequestID != "" {
		w.Header().Set("X-WB2A-Upstream-Request-Id", out.RequestID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         out.TaskID,
		"object":     "video",
		"model":      in.Model,
		"status":     "queued",
		"created_at": time.Now().Unix(),
		"poll_url":   "/v1/videos/" + out.TaskID,
	})
}

// videoTask 处理 GET /v1/videos/{id}：用提交时的账号查询任务状态。
func (h *Handler) videoTask(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimSpace(r.PathValue("id"))
	entry, ok := h.lookupVideoTask(taskID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found",
			"unknown video task (tasks are kept in memory for 2h and do not survive a restart)")
		return
	}
	acct := entry.acct
	if tr := requestTraceFromContext(r.Context()); tr != nil {
		tr.model = entry.model
	}
	// token 过期先刷新：查询与 chat 共用 accessToken，带过期 token 打过去会拿到 401，
	// 被错误策略误判成账号故障。
	if acct.NeedsRefresh(h.cfg.RefreshSkew) {
		if err := h.cfg.Upstream.RefreshToken(acct); err == nil {
			_ = acct.SaveAtomic()
		}
	}

	out, err := h.cfg.Upstream.QueryVideo(r.Context(), acct, taskID, searchMetaFromRequest(r))
	if err != nil {
		var ue *upstream.Error
		if errors.As(err, &ue) {
			h.applyErrorPolicy(acct.UID, ue.Kind, ue.Msg, "", ue)
			status := ue.Status
			if ue.Kind == upstream.ErrSoftRate {
				status = http.StatusTooManyRequests
			}
			if status < 400 {
				status = http.StatusBadGateway
			}
			writeOpenAIError(w, status, "api_error", ue.Msg)
			return
		}
		writeOpenAIError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	payload := map[string]any{
		"id":         out.TaskID,
		"object":     "video",
		"model":      entry.model,
		"status":     out.Status,
		"created_at": entry.created.Unix(),
	}
	if len(out.Videos) > 0 {
		payload["videos"] = out.Videos
		payload["url"] = out.Videos[0].URL
	}
	if out.Status == "completed" {
		payload["completed_at"] = time.Now().Unix()
	}
	if out.HasCredit {
		payload["usage"] = map[string]any{"credit": out.Credit}
		// 客户端会反复轮询：积分只在第一次看到 completed 时记一笔。
		h.videoMu.Lock()
		shouldRecord := !entry.recorded
		entry.recorded = true
		h.videoMu.Unlock()
		if shouldRecord {
			h.recordVideoUsage(acct, entry.model, out.Credit)
		}
	}
	if out.RequestID != "" {
		w.Header().Set("X-WB2A-Upstream-Request-Id", out.RequestID)
	}
	writeJSON(w, http.StatusOK, payload)
}

// recordVideoUsage 把成片积分写进用量记录器（与图像同一口径：按出票账号的域记账）。
func (h *Handler) recordVideoUsage(acct *auth.Auth, model string, credit float64) {
	if h.cfg.Usage == nil || acct == nil {
		return
	}
	h.cfg.Usage.Add(time.Now(), acct.Realm(), acct.UID, model, usage.Delta{
		Credit:    credit,
		HasCredit: true,
	}, true)
}
