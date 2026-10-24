// images.go OpenAI 兼容的图像生成前门：/v1/images/generations 与 /v1/images/edits。
//
// 设计参考 loomy2api 的 imagefront：edits 与 generations 不是两套语义，只是载体
// 不同（multipart 文件上传 vs JSON）。因此两个入口共用同一条「归一 → 选号 →
// 上游调用 → 响应封装」管道，不做第二套适配。
//
// 与 loomy2api 的差异来自上游契约（见 internal/upstream/images.go）：
//   - WorkBuddy 的图生图是**独立端点** /v2/images/edits，不是把图塞进 generations；
//   - 图生图字段是 `image: ["<url|data URI>", ...]`（官方客户端恒发数组）；
//   - 响应是 {code,msg,data:{data:[{url|b64_json}],usage:{credit}}} 信封，
//     必须在这里展平成 OpenAI 的 {created,data:[...]}；
//   - 余额/限流/模型错误继续走网关既有 Classify + applyErrorPolicy（选号、冷却、
//     熔断、token 刷新全部复用），不引入第二套账号健康语义。
package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

const (
	// defaultImageModel 文生图默认模型。取**服务端 product config 里实际存在**的
	// text-to-image 模型（2026-10 工作账号目录只有 hunyuan-image-alpha）；官方 CLI
	// 的硬回退是 hunyuan-image-v3.0，账号已开通时可显式配置成它。
	defaultImageModel = "hunyuan-image-alpha"
	// defaultImageEditModel 图生图默认模型。取官方当前目录里带 image-to-image
	// 标签的 hunyuan-image-alpha-edit；hunyuan-image-v2.0-general-edit 是 CLI 的
	// 兜底值，可通过配置切换。
	defaultImageEditModel = "hunyuan-image-alpha-edit"

	// defaultImageSize 官方 buildBaseRequestBody 的缺省 size。
	defaultImageSize = "1024x1024"

	// imageMaxRequestBytes 单次入站请求上限（含 multipart 文件与 JSON 里的
	// data URI）。4 张 24MB 原图转 data URI 后约 128MB，取 96MB 兜住常见形态，
	// 同时避免一个坏请求把内存打满。
	imageMaxRequestBytes = 96 << 20
	// imageMaxRefChars 单张参考图字符串上限（字符数）。base64 膨胀 4/3，
	// 32M 字符 ≈ 24MB 原图，再大就不是「参考图」而是误传了整个文件。
	imageMaxRefChars = 32 << 20
	// imageMaxFetchBytes 从 http(s) URL 拉取参考图时的原始字节上限（24MB）。
	imageMaxFetchBytes = 24 << 20
	// imageFetchTimeout 单个 URL 参考图的下载上限。4 张最坏 100s，仍在生图
	// 总预算（420s）之内。
	imageFetchTimeout = 25 * time.Second
	// imageMaxN 单次最多生成张数。上游对 n 的上限没有公开声明，按 4 收敛
	//（与主流图像接口一致），超出截断并回 warning 头。
	imageMaxN = 4
	// imageAttemptCap 单次上游生图调用的硬上限。真正的总预算由
	// images.deadline_seconds 控制（默认 420s），本值只防「配置了超大预算」时
	// 单次调用无限挂住。
	imageAttemptCap = 15 * time.Minute
	// imageMaxFootnoteRunes 官方 Hunyuan footnote 字段上限（16 字符）。
	imageMaxFootnoteRunes = 16
)

// imageFailure 轮转全部失败后的出口状态（渲染成 OpenAI error 形态）。
type imageFailure struct {
	status int
	msg    string
	hint   string
}

// imageCall 一次待执行的图像请求。
type imageCall struct {
	kind    upstream.ImageKind
	model   string
	realm   string
	payload map[string]any
}

// ---------- 入口 ----------

// imageGenerations POST /v1/images/generations。
//
// 兼容 loomy2api 的一处便利：如果请求体里带了 image/images（部分客户端用
// generations 端点做垫图），自动改走 edits 端点，而不是把参考图静默丢掉。
func (h *Handler) imageGenerations(w http.ResponseWriter, r *http.Request) {
	h.imageFront(w, r, upstream.ImageGeneration)
}

// imageEdits POST /v1/images/edits：支持 OpenAI multipart，也接受 JSON。
func (h *Handler) imageEdits(w http.ResponseWriter, r *http.Request) {
	h.imageFront(w, r, upstream.ImageEdit)
}

func (h *Handler) imageFront(w http.ResponseWriter, r *http.Request, kind upstream.ImageKind) {
	meta := imageMetaFromRequest(r)

	r.Body = http.MaxBytesReader(w, r.Body, imageMaxRequestBytes)
	raw := map[string]any{}
	var err error
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		var refs []string
		raw, refs, err = parseImageMultipart(r)
		// multipart 的 image part 已归一成 data URI；放回 raw，交给 prepareImage
		// 走与 JSON 完全相同的收集/截断/模型解析路径。
		if len(refs) > 0 {
			raw["image"] = refs
		}
	} else {
		raw, err = parseImageJSON(r)
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	prep, fail := h.prepareImage(r.Context(), raw, kind)
	if fail != nil {
		writeImageFailure(w, fail)
		return
	}
	warns := h.applyImageHeaders(r.Context(), w, prep)

	out, fail := h.runImage(r.Context(), prep.call, meta)
	if fail != nil {
		writeImageFailure(w, fail)
		return
	}

	// 上游对 hunyuan 的 n 实际只出一张（2026-10-24 实测：n=2 返回 1 张、扣 1 张
	// 的积分）。不静默：把「请求几张 / 实际几张」挂到 warning 头，调用方才能判断
	// 是上游不支撑还是自己没传对。
	if n, ok := prep.call.payload["n"].(int); ok && n > len(out.Items) {
		warns = append(warns, fmt.Sprintf("requested-n-%d-got-%d", n, len(out.Items)))
		w.Header().Set("X-WB2A-Image-Warn", strings.Join(warns, ","))
	}

	if out.RequestID != "" {
		w.Header().Set("X-WB2A-Upstream-Request-Id", out.RequestID)
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, http.StatusOK, openAIImageResponse(out))
}

// imagePrepared 一次归一化后的图像请求：可直接交给 runImage。
type imagePrepared struct {
	call     imageCall
	warns    []string
	replaced string
}

// prepareImage 把入站 map 归一成上游可执行调用：
// 参考图收集/截断/URL 下载/base64 补 MIME → generations 带图自动转 edits →
// 模型解析与默认模型/兜底 → 白名单请求体。两个前门（/v1/images/* 与 chat）共用。
func (h *Handler) prepareImage(ctx context.Context, raw map[string]any, kind upstream.ImageKind) (*imagePrepared, *imageFailure) {
	cfg := h.imagesCfg()

	refs, err := collectImageRefs(raw)
	if err != nil {
		return nil, &imageFailure{status: http.StatusBadRequest, msg: err.Error()}
	}
	var warns []string
	// 参考图上限：只截尾不重排（提示词里的「第一张/第二张」依赖顺序）。
	if len(refs) > cfg.MaxRefImages {
		refs = refs[:cfg.MaxRefImages]
		warns = append(warns, fmt.Sprintf("images-truncated-to-%d", cfg.MaxRefImages))
	}
	// URL/base64 归一（上游只吃 data URI；官方客户端也是先下载 URL 再转）。
	refs, err = normalizeImageRefs(ctx, refs, cfg.FetchSourceIP)
	if err != nil {
		return nil, &imageFailure{status: http.StatusBadRequest, msg: err.Error()}
	}
	if kind == upstream.ImageEdit && len(refs) == 0 {
		return nil, &imageFailure{status: http.StatusBadRequest,
			msg: "image is required: /v1/images/edits needs at least one reference image"}
	}
	if kind == upstream.ImageGeneration && len(refs) > 0 {
		// generations 带图 = 图生图（与 loomy2api 的 imagefront 同口径）。
		kind = upstream.ImageEdit
		warns = append(warns, "route-to-edits")
	}

	model, realm, replaced, fail := h.resolveImageModel(stringField(raw["model"]), kind, cfg)
	if fail != nil {
		return nil, fail
	}
	// 文本模型 + 参考图：上游 edits 需要 image-to-image 模型，自动切到编辑模型。
	if kind == upstream.ImageEdit && strings.EqualFold(model, cfg.DefaultModel) {
		model = cfg.EditModel
		warns = append(warns, "model-switched-to-"+cfg.EditModel)
	}
	// 编辑模型但没图：generations 端点会拿编辑模型名去请求文本生图，必错。
	// 给出可操作的 400（chat 前门会先把它切成默认文本模型）。
	if kind == upstream.ImageGeneration && len(refs) == 0 && strings.EqualFold(model, cfg.EditModel) {
		return nil, &imageFailure{status: http.StatusBadRequest,
			msg: fmt.Sprintf("model %q requires a reference image; use %q for text-to-image", model, cfg.DefaultModel)}
	}

	payload, bodyWarns, err := buildImagePayload(kind, model, raw, refs)
	if err != nil {
		return nil, &imageFailure{status: http.StatusBadRequest, msg: err.Error()}
	}
	warns = append(warns, bodyWarns...)
	return &imagePrepared{
		call:     imageCall{kind: kind, model: model, realm: realm, payload: payload},
		warns:    warns,
		replaced: replaced,
	}, nil
}

// applyImageHeaders 把模型/路由/告警写进响应头，并同步请求日志的 model 列。
// 返回 warns 供调用方在拿到上游结果后再追加（如 n 被上游忽略）。
func (h *Handler) applyImageHeaders(ctx context.Context, w http.ResponseWriter, prep *imagePrepared) []string {
	if tr := requestTraceFromContext(ctx); tr != nil {
		tr.model = prep.call.model
	}
	w.Header().Set("X-WB2A-Image-Model", prep.call.model)
	if prep.replaced != "" {
		log.Printf("[image] model %q is not a known image model, fallback to %s", prep.replaced, prep.call.model)
		w.Header().Set("X-WB2A-Model-Fallback", prep.replaced+" -> "+prep.call.model)
	}
	for _, warn := range prep.warns {
		if warn == "route-to-edits" {
			w.Header().Set("X-WB2A-Image-Route", "edits")
			break
		}
	}
	if len(prep.warns) > 0 {
		w.Header().Set("X-WB2A-Image-Warn", strings.Join(prep.warns, ","))
	}
	return prep.warns
}

// ---------- chat 前门 ----------

// imageChat 图像模型的 /v1/chat/completions 前门：
// 把 messages 折成 prompt + 参考图，复用 images 管道生图，再把结果封成
// chat.completion（stream=true 时回 SSE chunk）。这样只支持 chat 协议的客户端
// （Cherry Studio、各种 OpenAI SDK 对话封装）也能直接选图像模型生图。
func (h *Handler) imageChat(w http.ResponseWriter, r *http.Request, body []byte, model string) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "body is not a JSON object: "+err.Error())
		return
	}
	cfg := h.imagesCfg()
	prompt, refs := chatPromptImages(req)

	raw := map[string]any{"model": model, "prompt": prompt}
	if v, ok := req["n"]; ok {
		raw["n"] = v
	}
	if s := strings.TrimSpace(stringField(req["size"])); s != "" {
		raw["size"] = s
	}
	for _, k := range []string{"quality", "style", "background"} {
		if v := strings.TrimSpace(stringField(req[k])); v != "" {
			raw[k] = v
		}
	}
	if len(refs) > 0 {
		raw["image"] = refs
	}

	var preWarns []string
	if len(refs) == 0 && strings.EqualFold(model, cfg.EditModel) {
		// 编辑模型没垫图：chat 客户端多半只是选了「图像模型」。切到文本生图
		// 模型比回 400 更符合对话场景，且用 warning 头明确告知切换。
		raw["model"] = cfg.DefaultModel
		preWarns = append(preWarns, "model-switched-to-"+cfg.DefaultModel)
	}

	prep, fail := h.prepareImage(r.Context(), raw, upstream.ImageGeneration)
	if fail != nil {
		writeImageFailure(w, fail)
		return
	}
	prep.warns = append(preWarns, prep.warns...)
	warns := h.applyImageHeaders(r.Context(), w, prep)

	out, fail := h.runImage(r.Context(), prep.call, imageMetaFromRequest(r))
	if fail != nil {
		writeImageFailure(w, fail)
		return
	}

	if n, ok := prep.call.payload["n"].(int); ok && n > len(out.Items) {
		warns = append(warns, fmt.Sprintf("requested-n-%d-got-%d", n, len(out.Items)))
		w.Header().Set("X-WB2A-Image-Warn", strings.Join(warns, ","))
	}
	if out.RequestID != "" {
		w.Header().Set("X-WB2A-Upstream-Request-Id", out.RequestID)
	}

	text := imageMarkdown(out)
	created := time.Now().Unix()
	id := fmt.Sprintf("chatcmpl-img_%d", time.Now().UnixNano())
	if chatStreamRequested(req) {
		writeImageChatStream(w, id, prep.call.model, created, text, out)
		return
	}
	usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	if out.HasCredit {
		usage["credit"] = out.Credit
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   prep.call.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": usage,
	})
}

// chatPromptImages 把 chat messages 折成 prompt + 参考图列表。
// 优先 user 消息（避免客户端注入的长 system prompt 污染生图提示词）；user 里
// 没有可用文本/图片时再回落全量消息。
func chatPromptImages(req map[string]any) (string, []string) {
	var userTexts, allTexts, userImgs, allImgs []string
	msgs, _ := req["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		role := strings.ToLower(stringField(m["role"]))
		isUser := role == "" || role == "user"

		var texts, imgs []string
		switch c := m["content"].(type) {
		case string:
			if s := strings.TrimSpace(c); s != "" {
				texts = append(texts, s)
			}
		case []any:
			for _, p := range c {
				part, _ := p.(map[string]any)
				if part == nil {
					continue
				}
				switch strings.ToLower(stringField(part["type"])) {
				case "text", "input_text":
					if s := strings.TrimSpace(stringField(part["text"])); s != "" {
						texts = append(texts, s)
					}
				case "image_url", "input_image", "image":
					if u := chatImageURL(part); u != "" {
						imgs = append(imgs, u)
					}
				}
			}
		}
		// 少数客户端把参考图放在 message.images 而不是 content parts。
		if extra, ok := m["images"].([]any); ok {
			for _, it := range extra {
				if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
					imgs = append(imgs, strings.TrimSpace(s))
				}
			}
		}
		if isUser {
			userTexts = append(userTexts, texts...)
			userImgs = append(userImgs, imgs...)
		} else {
			allTexts = append(allTexts, texts...)
			allImgs = append(allImgs, imgs...)
		}
	}
	texts, imgs := userTexts, userImgs
	if len(texts) == 0 {
		texts = allTexts
	}
	if len(imgs) == 0 {
		imgs = allImgs
	}
	prompt := strings.TrimSpace(strings.Join(texts, "\n"))
	if prompt == "" {
		prompt = "生成一张图片"
	}
	return prompt, imgs
}

// chatImageURL 从内容块里取图片引用，兼容各家拼法：
//
//	{"type":"image_url","image_url":{"url":"…"}}   OpenAI chat
//	{"type":"input_image","image_url":"…"}         OpenAI responses
//	{"type":"image","image":"…"}                   部分客户端
//	{"type":"image","image":{"url":"…"}}           同上，对象形态
func chatImageURL(part map[string]any) string {
	for _, key := range []string{"image_url", "image", "url"} {
		switch v := part[key].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case map[string]any:
			if s := strings.TrimSpace(stringField(v["url"])); s != "" {
				return s
			}
		}
	}
	return ""
}

func chatStreamRequested(req map[string]any) bool {
	switch v := req["stream"].(type) {
	case bool:
		return v
	case string:
		b, _ := strconv.ParseBool(strings.TrimSpace(v))
		return b
	case float64:
		return v != 0
	default:
		return false
	}
}

// imageMarkdown 把生成结果封成 chat 正文。URL 优先（客户端可直接渲染/下载）；
// 只有 b64_json 时内联 data URI，保证内容不是空的。
func imageMarkdown(out *upstream.ImageOutcome) string {
	parts := make([]string, 0, len(out.Items))
	for _, it := range out.Items {
		switch {
		case it.URL != "":
			parts = append(parts, "![image]("+it.URL+")\n"+it.URL)
		case it.B64JSON != "":
			parts = append(parts, "![image](data:image/png;base64,"+it.B64JSON+")")
		}
	}
	return strings.Join(parts, "\n\n")
}

// writeImageChatStream 把一次生图结果写成最小可用的 chat SSE：
// role → content → finish(stop, 带 usage.credit) → [DONE]。
func writeImageChatStream(w http.ResponseWriter, id, model string, created int64, text string, out *upstream.ImageOutcome) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	chunk := func(delta map[string]any, finish any, usage map[string]any) {
		obj := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		if usage != nil {
			obj["usage"] = usage
		}
		b, _ := json.Marshal(obj)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	chunk(map[string]any{"role": "assistant"}, nil, nil)
	chunk(map[string]any{"content": text}, nil, nil)
	var usage map[string]any
	if out.HasCredit {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "credit": out.Credit}
	}
	chunk(map[string]any{}, "stop", usage)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// ---------- 入站解析 ----------

func parseImageJSON(r *http.Request) (map[string]any, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	payload := map[string]any{}
	if len(bytes.TrimSpace(body)) == 0 {
		return payload, nil
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("body is not a JSON object: %w", err)
	}
	return payload, nil
}

// parseImageMultipart 流式读取 multipart：保持 image part 的顺序（提示词里的
// 「第一张/第二张」依赖顺序，用 ParseMultipartForm 的 map 会丢序）。
func parseImageMultipart(r *http.Request) (map[string]any, []string, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, fmt.Errorf("parse multipart: %w", err)
	}
	payload := map[string]any{}
	var refs []string

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read multipart: %w", err)
		}
		name := part.FormName()
		data, err := io.ReadAll(io.LimitReader(part, imageMaxRefChars+1))
		_ = part.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("read field %s: %w", name, err)
		}
		if len(data) > imageMaxRefChars {
			return nil, nil, fmt.Errorf("field %s exceeds %d MB limit", name, imageMaxRefChars>>20)
		}

		if isImageField(name) {
			uri, ok := imagePartDataURI(data)
			if !ok {
				trimmed := strings.TrimSpace(string(data))
				if trimmed == "" {
					continue // 空 part 视同没传（与官方客户端 / loomy2api 同口径）
				}
				if len(trimmed) > imageMaxRefChars {
					return nil, nil, fmt.Errorf("image %s exceeds %d MB limit", name, imageMaxRefChars>>20)
				}
				uri = trimmed
			}
			refs = append(refs, uri)
			continue
		}
		if name == "mask" {
			// WorkBuddy 的 edits 端点没有 mask 字段；静默丢弃比送上去换一个
			// 400 更清楚，同时用 warning 头告诉调用方。
			continue
		}
		payload[name] = strings.TrimSpace(string(data))
	}
	return payload, refs, nil
}

func isImageField(name string) bool {
	return name == "image" || strings.HasPrefix(name, "image[") || name == "image[]"
}

// imagePartDataURI 判断 part 内容是不是图片二进制，是则转 data URI。
// 文本 URL 返回 false（调用方保留原文）。
func imagePartDataURI(data []byte) (string, bool) {
	if len(data) == 0 {
		return "", false
	}
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("data:image/")) {
		return string(trimmed), true
	}
	if bytes.HasPrefix(trimmed, []byte("http://")) || bytes.HasPrefix(trimmed, []byte("https://")) {
		return "", false
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return "", false
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), true
}

// collectImageRefs 合并 `images` 与 `image` 两个字段，保持出现顺序。
// 去重只发生在「跨字段」：同一字段内的重复要留着，否则提示词里的「第二张」
// 会因为两张图恰好相同而被悄悄抹掉下标。
func collectImageRefs(payload map[string]any) ([]string, error) {
	var out []string
	seenFromPrev := make(map[string]struct{})
	for _, key := range []string{"images", "image"} {
		v, exists := payload[key]
		if !exists {
			continue
		}
		list, err := imageList(v)
		if err != nil {
			return nil, fmt.Errorf("%s field: %w", key, err)
		}
		for _, s := range list {
			if _, dup := seenFromPrev[s]; dup {
				continue
			}
			out = append(out, s)
		}
		for _, s := range list {
			seenFromPrev[s] = struct{}{}
		}
	}
	// 参考图已归一成 refs，原始字段删掉：避免它们被当成未知字段带到上游。
	delete(payload, "image")
	delete(payload, "images")
	return out, nil
}

// imageList 把参考图字段归一成非空字符串列表。
// 支持 "url"、["url1","url2"]、null（= 不传）；空串与超长元素直接报错。
func imageList(v any) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return nil, fmt.Errorf("must not be an empty string (omit the field or pass null)")
		}
		if len(s) > imageMaxRefChars {
			return nil, fmt.Errorf("single image exceeds %d MB limit", imageMaxRefChars>>20)
		}
		return []string{s}, nil
	case []string:
		out := make([]string, 0, len(x))
		for _, s := range x {
			s = strings.TrimSpace(s)
			if s == "" {
				return nil, fmt.Errorf("array must not contain empty strings")
			}
			if len(s) > imageMaxRefChars {
				return nil, fmt.Errorf("single image exceeds %d MB limit", imageMaxRefChars>>20)
			}
			out = append(out, s)
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("array elements must be strings (URL, data URI or base64)")
			}
			s = strings.TrimSpace(s)
			if s == "" {
				return nil, fmt.Errorf("array must not contain empty strings")
			}
			if len(s) > imageMaxRefChars {
				return nil, fmt.Errorf("single image exceeds %d MB limit", imageMaxRefChars>>20)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("accepts a string or an array of strings (URL, data URI or base64)")
	}
}

// ---------- 参考图形态归一 ----------

// imageFetchClient 下载 URL 参考图专用 client。
//
// 必须做 SSRF 防护：网关是公网服务，`image` 字段的 URL 来自调用方，若不校验
// 目标 IP，就能借网关打内网（127.0.0.1、169.254.169.254 元数据、容器网段）。
//   - DialContext 自行解析 DNS 并只拨「公网 IP」，杜绝 DNS rebinding；
//   - Proxy=nil：环境代理会绕过 IP 校验；
//   - 重定向最多 3 跳，每一跳仍走同一 DialContext 校验。
//
// 变量而非常量：测试要用 loopback 的 httptest server 替换它（生产路径绝不能放开）。
var imageFetchClient = &http.Client{
	Timeout: imageFetchTimeout,
	Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           safeImageDialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("unsupported redirect scheme %q", req.URL.Scheme)
		}
		return nil
	},
}

// safeImageDialContext 解析目标主机并只连接公网地址。
// 注意 TLS SNI 仍由 http.Transport 用原 hostname 设置，这里只换拨号目标。
func safeImageDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	var lastErr error
	for _, ip := range ips {
		if isBlockedImageIP(ip.IP) {
			lastErr = fmt.Errorf("refusing to connect to non-public address %s", ip.IP)
			continue
		}
		d := &net.Dialer{Timeout: 5 * time.Second}
		// 源 IP 绑定：多公网 IP 主机上默认路由 IP 可能被目标 CDN 拉黑（实测
		// sched-1 的 154.40.45.34 连腾讯云 COS 恒定超时，同机 .65 0.2s 可达）。
		// 配置 images.fetch_source_ip 后只影响 URL 参考图下载，不改生图出口。
		if src, ok := ctx.Value(imageFetchSourceKey{}).(net.IP); ok && src != nil {
			d.LocalAddr = &net.TCPAddr{IP: src}
		}
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no address for %s", host)
	}
	return nil, lastErr
}

// isBlockedImageIP 判定地址是否属于禁止访问的范围（私网/回环/链路本地/保留段）。
func isBlockedImageIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10 CGNAT
			return true
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
			return true
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15 benchmark
			return true
		case v4[0] == 198 && v4[1] == 51 && v4[2] == 100: // TEST-NET-2
			return true
		case v4[0] == 203 && v4[1] == 0 && v4[2] == 113: // TEST-NET-3
			return true
		case v4[0] >= 240: // 240.0.0.0/4 reserved
			return true
		}
	}
	return false
}

// imageFetchSourceKey 把「下载 URL 参考图时绑定的源 IP」塞进 ctx，供
// safeImageDialContext 读取。用 ctx 而不是全局变量：并发请求可以带不同配置。
type imageFetchSourceKey struct{}

// normalizeImageRefs 把参考图统一成上游认识的 data URI。
//
// 优先级与官方客户端一致：data URI 原样保留 → http(s) 下载后转 data URI →
// 裸 base64 补 MIME。本地文件路径网关读不到（那是客户端本机的能力），直接给出
// 可操作的 400，而不是把路径字符串当图片发给上游换一个 10000 internal error。
//
// sourceIP 非空时，URL 下载会绑定该本机源地址（images.fetch_source_ip）；这是给
// 「默认路由 IP 被 CDN 拉黑」的多 IP 主机留的出口，不影响生图请求的账号出口。
func normalizeImageRefs(ctx context.Context, refs []string, sourceIP string) ([]string, error) {
	sourceIP = strings.TrimSpace(sourceIP)
	if sourceIP != "" {
		ip := net.ParseIP(sourceIP)
		if ip == nil {
			return nil, fmt.Errorf("invalid images.fetch_source_ip %q", sourceIP)
		}
		ctx = context.WithValue(ctx, imageFetchSourceKey{}, ip)
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		switch {
		case strings.HasPrefix(ref, "data:"):
			if !strings.HasPrefix(ref, "data:image/") || !strings.Contains(ref, ";base64,") {
				return nil, fmt.Errorf("invalid data URI: expected data:image/<type>;base64,...")
			}
			out = append(out, ref)
		case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
			uri, err := fetchImageAsDataURI(ctx, ref)
			if err != nil {
				return nil, fmt.Errorf("reference image %s: %w", redactImageURL(ref), err)
			}
			out = append(out, uri)
		case looksLikeRawBase64(ref):
			uri, err := rawBase64ToDataURI(ref)
			if err != nil {
				return nil, fmt.Errorf("reference image base64: %w", err)
			}
			out = append(out, uri)
		default:
			return nil, fmt.Errorf("reference image is not accessible: send a data URI, raw base64, or an http(s) URL; local file paths cannot be read by the gateway (use multipart/form-data upload)")
		}
	}
	return out, nil
}

// fetchImageAsDataURI 下载 URL 并转成 data URI（SSRF 防护见 imageFetchClient）。
func fetchImageAsDataURI(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	ctx, cancel := context.WithTimeout(ctx, imageFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", fmt.Errorf("invalid URL")
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "workbuddy2api-image-fetch/1")
	resp, err := imageFetchClient.Do(req)
	if err != nil {
		// 不能直接透传 err：*url.Error 的字符串会带完整 URL（含 COS 预签名
		// query 里的 q-ak / q-signature），错误会回给调用方并进日志。只取最内层
		// 原因——http.Client.Do 可能把 transport 返回的 *url.Error 再包一层，
		// 所以逐层剥到不再是 url.Error 为止。
		inner := err
		for i := 0; i < 5; i++ {
			var ue *url.Error
			if !errors.As(inner, &ue) || ue.Err == inner {
				break
			}
			inner = ue.Err
		}
		return "", fmt.Errorf("download failed: %v", inner)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, imageMaxFetchBytes+1))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}
	if len(data) > imageMaxFetchBytes {
		return "", fmt.Errorf("image exceeds %d MB limit", imageMaxFetchBytes>>20)
	}
	ct := resp.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		ct = mt
	}
	if !strings.HasPrefix(ct, "image/") {
		ct = http.DetectContentType(data)
	}
	if !strings.HasPrefix(ct, "image/") {
		return "", fmt.Errorf("not an image (content-type %q)", ct)
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// looksLikeRawBase64 判断字符串是否像裸 base64。
// 不含 "." 是关键：文件路径（/tmp/a.png、images/foo.png）因此被排除；长度下限
// 100 避免把普通短词误判成图片。
func looksLikeRawBase64(s string) bool {
	if len(s) < 100 || strings.ContainsAny(s, ".\\ \n\r\t") {
		return false
	}
	for _, c := range s {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			continue
		}
		return false
	}
	return true
}

// rawBase64ToDataURI 解前 512 字符探测 MIME，再补成 data URI。
func rawBase64ToDataURI(s string) (string, error) {
	head := s
	if len(head) > 512 {
		head = head[:512]
	}
	if n := len(head) % 4; n != 0 {
		head = head[:len(head)-n]
	}
	raw, err := base64.StdEncoding.DecodeString(head)
	if err != nil {
		return "", fmt.Errorf("invalid base64: %w", err)
	}
	ct := http.DetectContentType(raw)
	if !strings.HasPrefix(ct, "image/") {
		return "", fmt.Errorf("not an image (detected %q)", ct)
	}
	return "data:" + ct + ";base64," + s, nil
}

// redactImageURL 去掉 query（签名参数），避免把 COS 预签名 URL 写进日志/错误。
func redactImageURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// ---------- 上游请求体 ----------

// buildImagePayload 按模型分支构造上游请求体，只保留白名单字段。
//
// 为什么白名单而不是透传：官方客户端对各 provider 的字段支持不一致，且上游对
// 未知字段并不总是忽略（loomy2api 实测 seed/watermark 这类扩展字段会换 400）。
// 网关在这里收敛，宁可丢弃并回 warning，也不让一次 400 白耗账号轮转。
func buildImagePayload(kind upstream.ImageKind, model string, raw map[string]any, refs []string) (map[string]any, []string, error) {
	prompt := strings.TrimSpace(stringField(raw["prompt"]))
	if prompt == "" {
		return nil, nil, fmt.Errorf("prompt is required")
	}
	hunyuan := strings.HasPrefix(strings.ToLower(model), "hunyuan-")

	p := map[string]any{"model": model, "prompt": prompt}
	// 默认无水印。官方客户端生图路径从不发这个字段（各 provider 模型自带
	// 「AI生成」角标），但上游接口接受 watermark 开关；调用方传 true 可保留。
	wm := false
	if b, ok := boolField(raw["watermark"]); ok {
		wm = b
	}
	p["watermark"] = wm
	size := strings.TrimSpace(stringField(raw["size"]))
	if size == "" {
		size = defaultImageSize
	}
	p["size"] = size

	var warns []string
	if n, ok, err := intField(raw["n"]); err != nil {
		return nil, nil, fmt.Errorf("n: %w", err)
	} else if ok {
		if n < 1 {
			n = 1
		}
		if n > imageMaxN {
			n = imageMaxN
			warns = append(warns, fmt.Sprintf("n-clamped-to-%d", imageMaxN))
		}
		p["n"] = n
	} else if hunyuan {
		// 官方 hunyuan 分支固定发送 n（缺省 1）。
		p["n"] = 1
	}

	if kind == upstream.ImageEdit {
		if len(refs) == 0 {
			return nil, nil, fmt.Errorf("image is required for edits")
		}
		arr := make([]string, len(refs))
		copy(arr, refs)
		p["image"] = arr
		if v := strings.TrimSpace(stringField(raw["input_fidelity"])); v != "" {
			p["input_fidelity"] = v
		}
	}

	if hunyuan {
		// hunyuan 分支：官方客户端只扩展 footnote / revise，且**不发**
		// response_format（发了也不认）。
		if f := strings.TrimSpace(stringField(raw["footnote"])); f != "" {
			if runes := []rune(f); len(runes) > imageMaxFootnoteRunes {
				f = string(runes[:imageMaxFootnoteRunes])
				warns = append(warns, "footnote-truncated")
			}
			p["footnote"] = f
		}
		if b, ok := boolField(raw["revise"]); ok {
			p["revise"] = map[string]any{"value": b}
		}
	} else {
		rf := strings.TrimSpace(stringField(raw["response_format"]))
		if rf == "" {
			rf = "b64_json"
		}
		if rf != "url" && rf != "b64_json" {
			return nil, nil, fmt.Errorf("response_format must be \"url\" or \"b64_json\"")
		}
		p["response_format"] = rf
		for _, k := range []string{"quality", "style", "background"} {
			if v := strings.TrimSpace(stringField(raw[k])); v != "" {
				p[k] = v
			}
		}
	}

	return p, warns, nil
}

// ---------- 模型解析 ----------

// resolveImageModel 解析图像模型：realm 前缀 → 别名归一 → 默认模型 / 兜底。
//
// 与 chat 的关键差异：对话端点拼错模型名时上游会**静默回落到默认对话模型**并
// 照常扣分；图像端点不能赌这一点，所以：
//   - 空 model → 用图像默认模型（不是配置里的 chat 默认模型）；
//   - 命中网关对话模型目录 → 直接 400（它一定不是图像模型）；
//   - 配置了 fallback 且模型不是已知图像模型 → 用 fallback，并回 X-WB2A-Model-Fallback。
func (h *Handler) resolveImageModel(rawModel string, kind upstream.ImageKind, cfg ImagesConfig) (model, realm, replaced string, fail *imageFailure) {
	raw := strings.TrimSpace(rawModel)
	defaultModel := cfg.DefaultModel
	if kind == upstream.ImageEdit {
		defaultModel = cfg.EditModel
	}
	if raw == "" {
		return defaultModel, "cn", "", nil
	}

	realm, bare, _ := resolveModelDetail(raw)
	if canon := upstream.CanonicalModel(bare); canon != bare {
		log.Printf("WARN: [image] model alias: %s -> %s", bare, canon)
		bare = canon
	}
	if strings.EqualFold(bare, cfg.DefaultModel) || strings.EqualFold(bare, cfg.EditModel) {
		return bare, realm, "", nil
	}
	if h.isChatModel(bare) {
		return "", realm, "", &imageFailure{
			status: http.StatusBadRequest,
			msg: fmt.Sprintf("model %q is a chat model, not an image model; use %s for text-to-image or %s for image-to-image",
				bare, cfg.DefaultModel, cfg.EditModel),
			hint: "image models are served on /v1/images/generations and /v1/images/edits",
		}
	}
	if fb := strings.TrimSpace(cfg.FallbackModel); fb != "" {
		return fb, realm, raw, nil
	}
	// 未知名字（可能是上游新增的图像模型）：放行，由上游判定。
	return bare, realm, "", nil
}

// isChatModel 判断模型是否在网关的对话模型目录缓存里。
// 只读缓存，不发上游请求；缓存冷/空时返回 false（宁缺勿滥）。
func (h *Handler) isChatModel(model string) bool {
	for _, mi := range cachedModelsSnapshot() {
		if strings.EqualFold(mi.ID, model) {
			return true
		}
	}
	return false
}

// isImageModelName 判断模型是否该走 chat 生图前门。
// 先认配置里的模型；再按常见图像模型前缀兜底（上游新增模型 / 未写进配置时
// 也能命中）。对话目录里确实存在的模型一律不抢——那是正常 chat 流量。
func (h *Handler) isImageModelName(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	c := h.imagesCfg()
	if !c.Enabled {
		return false
	}
	for _, name := range []string{c.DefaultModel, c.EditModel, c.FallbackModel} {
		if name != "" && strings.EqualFold(name, model) {
			return true
		}
	}
	if h.isChatModel(model) {
		return false
	}
	lower := strings.ToLower(model)
	for _, prefix := range []string{
		"hunyuan-image", "gpt-image", "qwen-image", "seedream", "doubao-seedream", "flux", "imagen",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// ---------- 轮转内核 ----------

// runImage 带账号轮转执行一次图像请求。
//
// 轮转语义与 chat / search 一致：PickExcludingForRealm → 在途租约 → token 预刷新
// → 上游调用 → 按 ErrKind 走 applyErrorPolicy → 抖动退避换号。区别只有两处：
//   - 总预算 images.deadline_seconds：预算只决定「还换不换号」，不打断在途调用；
//   - 请求级错误（图无效 / 参数错 / prompt 超长 / 内容拦截）立即返回，不轮转。
func (h *Handler) runImage(ctx context.Context, call imageCall, meta upstream.ChatMeta) (*upstream.ImageOutcome, *imageFailure) {
	cfg := h.imagesCfg()
	started := time.Now()
	deadline := started.Add(cfg.Deadline)
	crossRealm := h.cfg.Pool.CrossDomainFallback()

	tried := map[string]bool{}
	var lastErr error
	var lastStatus int
	var lastKind upstream.ErrKind

	for i := 0; i < h.cfg.MaxRotate; i++ {
		if !time.Now().Before(deadline) {
			if lastErr == nil {
				lastStatus = http.StatusGatewayTimeout
			}
			log.Printf("[image] deadline budget exhausted (%.0fs), stop rotating", time.Since(started).Seconds())
			break
		}

		pickRealm := call.realm
		if crossRealm && h.cfg.Pool.ServableForModelRealm(call.model, pickRealm) == 0 &&
			h.cfg.Pool.ServableForModelRealm(call.model, otherRealm(pickRealm)) > 0 {
			pickRealm = otherRealm(pickRealm)
		}
		acct := h.cfg.Pool.PickExcludingForRealm(tried, call.model, pickRealm)
		if acct == nil && crossRealm {
			acct = h.cfg.Pool.PickExcludingForRealm(tried, call.model, otherRealm(pickRealm))
		}
		if acct == nil {
			break
		}
		tried[acct.UID] = true

		if !h.cfg.Pool.Acquire(acct.UID) {
			if !rotateBackoff(i, ctx) {
				break
			}
			continue
		}

		// token 临近过期先刷新：生图与 chat 共用 accessToken，过期 token 打过去
		// 会换回 401，被 applyErrorPolicy 当账号故障处置、误伤健康号。
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if rerr := h.cfg.Upstream.RefreshToken(acct); rerr != nil {
				h.cfg.Pool.Release(acct.UID)
				lastErr = rerr
				var ue *upstream.Error
				if errors.As(rerr, &ue) {
					lastStatus, lastKind = ue.Status, ue.Kind
					h.applyErrorPolicy(acct.UID, ue.Kind, ue.Msg, call.model, ue)
				} else {
					h.cfg.Pool.NoteFailures(acct.UID)
					lastStatus = http.StatusServiceUnavailable
				}
				if !rotateBackoff(i, ctx) {
					break
				}
				continue
			}
			if serr := acct.SaveAtomic(); serr != nil {
				log.Printf("ERR: [server] image refresh acct=%s: save auth failed: %v",
					logfmt.Label(acct.UID, acct.Nickname), serr)
			}
		}

		if tr := requestTraceFromContext(ctx); tr != nil {
			tr.account = logfmt.Label(acct.UID, acct.Nickname)
			tr.model = call.model
			if h.cfg.Upstream != nil {
				tr.egress = h.cfg.Upstream.EgressLabel(acct)
			}
		}

		attemptCap := time.Until(deadline)
		if attemptCap <= 0 {
			h.cfg.Pool.Release(acct.UID)
			lastStatus = http.StatusGatewayTimeout
			break
		}
		if attemptCap > imageAttemptCap {
			attemptCap = imageAttemptCap
		}
		attemptStarted := time.Now()
		attemptCtx, cancel := context.WithTimeout(ctx, attemptCap)
		out, err := h.cfg.Upstream.Image(attemptCtx, acct, call.kind, call.payload, meta)
		cancel()
		h.cfg.Pool.Release(acct.UID)

		// 生图没有 token；仍记一次账号请求数 + 延迟，面板的账号请求列才准。
		latencyMs := time.Since(attemptStarted).Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		h.cfg.Pool.RecordTokenUsage(acct.UID, pool.TokenUsageDelta{
			Model: call.model, HasLatencyMs: true, LatencyMs: latencyMs,
		})

		if err == nil {
			h.recordImageAttempt(acct, call.model, out.Credit, out.HasCredit, true)
			return out, nil
		}
		// 失败尝试也计入请求数（与 chat 的 recordAttempt 同口径）：否则面板里
		// 「生图失败率」永远是 0，轮转放大在用量视图里看不见。
		h.recordImageAttempt(acct, call.model, 0, false, false)
		lastErr = err

		var ue *upstream.Error
		if errors.As(err, &ue) {
			lastStatus, lastKind = ue.Status, ue.Kind
			h.applyErrorPolicy(acct.UID, ue.Kind, ue.Msg, call.model, ue)
			switch ue.Kind {
			case upstream.ErrImageInvalid, upstream.ErrBadParams, upstream.ErrPromptTooLong, upstream.ErrContentBlocked:
				// 请求级错误：同一个 body 换哪个号都一样，立即返回原文。
				status := http.StatusBadRequest
				return nil, &imageFailure{
					status: status,
					msg:    ue.Msg,
					hint:   h.hintOf(ue.Kind, ue.Msg, call.model, false, ue),
				}
			}
		} else if isUpstreamTimeout(err, ctx.Err() != nil) {
			// 超时不是账号的错：不喂连败计数，也不再换号撞同一个慢上游。
			lastStatus = http.StatusGatewayTimeout
			log.Printf("WARN: [server] image timeout acct=%s: %v (rotation stopped, account not penalized)",
				logfmt.Label(acct.UID, acct.Nickname), err)
			break
		} else {
			// 传输层抖动：只换号 + 喂连败计数，不按业务错误罚。
			h.cfg.Pool.NoteFailures(acct.UID)
			lastStatus = http.StatusServiceUnavailable
		}
		if !rotateBackoff(i, ctx) {
			break
		}
	}

	if lastErr == nil {
		return nil, &imageFailure{
			status: http.StatusServiceUnavailable,
			msg:    "all accounts are temporarily unavailable, please retry later",
			hint:   upstream.NoHealthyAccountHint(),
		}
	}

	status := http.StatusServiceUnavailable
	if lastStatus >= 400 && lastStatus < 500 {
		status = lastStatus
	}
	if lastKind == upstream.ErrSoftRate {
		status = http.StatusTooManyRequests
	}
	if lastStatus == http.StatusGatewayTimeout {
		status = http.StatusGatewayTimeout
	}

	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		return nil, &imageFailure{
			status: status,
			msg:    ue.Msg,
			hint:   h.hintOf(ue.Kind, ue.Msg, call.model, false, ue),
		}
	}
	return nil, &imageFailure{
		status: status,
		msg:    lastErr.Error(),
		hint:   upstream.NoHealthyAccountHint(),
	}
}

// recordImageAttempt 把一次生图尝试写进用量记录器（积分来自上游 usage.credit）。
// acct 必须是本次真正出票的账号：跨域回退时 realm 与实际请求域一致，不能记成
// 请求前缀声明的域。失败尝试同样计入请求数（ok=false），供面板算失败率。
func (h *Handler) recordImageAttempt(acct *auth.Auth, model string, credit float64, hasCredit, ok bool) {
	if h.cfg.Usage == nil || acct == nil {
		return
	}
	d := usage.Delta{}
	if hasCredit {
		d.Credit = credit
		d.HasCredit = true
	}
	h.cfg.Usage.Add(time.Now(), acct.Realm(), acct.UID, model, d, ok)
}

// ---------- 响应与错误 ----------

// openAIImageResponse 把上游信封展平成 OpenAI images 响应。
//
// 上游 data.data 里的元素要么带 url、要么带 b64_json，原样保留；两者都有时都输
// 出（部分客户端偏好 url，部分只认 b64_json）。
func openAIImageResponse(out *upstream.ImageOutcome) map[string]any {
	items := make([]map[string]any, 0, len(out.Items))
	for _, it := range out.Items {
		m := map[string]any{}
		if it.B64JSON != "" {
			m["b64_json"] = it.B64JSON
		}
		if it.URL != "" {
			m["url"] = it.URL
		}
		if it.RevisedPrompt != "" {
			m["revised_prompt"] = it.RevisedPrompt
		}
		items = append(items, m)
	}
	resp := map[string]any{
		"created": time.Now().Unix(),
		"data":    items,
	}
	if out.HasCredit {
		// OpenAI 没有积分概念；挂在 usage 下，客户端忽略未知字段，运维可核对扣分。
		resp["usage"] = map[string]any{"credit": out.Credit}
	}
	return resp
}

func writeImageFailure(w http.ResponseWriter, f *imageFailure) {
	code := "upstream_error"
	switch f.status {
	case http.StatusBadRequest:
		code = "invalid_request_error"
	case http.StatusUnauthorized:
		code = "invalid_api_key"
	case http.StatusNotFound:
		code = "model_not_found"
	case http.StatusTooManyRequests:
		code = "rate_limit_exceeded"
	case http.StatusServiceUnavailable:
		code = "no_healthy_account"
	case http.StatusGatewayTimeout:
		code = "upstream_timeout"
	}
	writeOpenAIErrorHint(w, f.status, code, f.msg, f.hint)
}

// imageMetaFromRequest 从入站请求头提取会话头族（与 search 同口径）。
// 图像请求同样会被上游按会话聚合，缺聚合主键会被视为请求不完整。
func imageMetaFromRequest(r *http.Request) upstream.ChatMeta {
	meta := upstream.ChatMeta{
		ConversationID:        strings.TrimSpace(r.Header.Get("X-Conversation-ID")),
		ConversationRequestID: strings.TrimSpace(r.Header.Get("X-Conversation-Request-ID")),
		TraceID:               strings.TrimSpace(r.Header.Get("X-Trace-ID")),
	}
	if meta.ConversationRequestID == "" {
		meta.ConversationRequestID = session.NewMessageID()
	}
	return meta
}

// ---------- 小工具 ----------

func stringField(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		return ""
	}
}

func intField(v any) (int, bool, error) {
	switch x := v.(type) {
	case nil:
		return 0, false, nil
	case float64:
		return int(x), true, nil
	case int:
		return x, true, nil
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return 0, false, fmt.Errorf("must be an integer")
		}
		return int(n), true, nil
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, false, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, false, fmt.Errorf("must be an integer")
		}
		return n, true, nil
	default:
		return 0, false, fmt.Errorf("must be an integer")
	}
}

func boolField(v any) (bool, bool) {
	switch x := v.(type) {
	case nil:
		return false, false
	case bool:
		return x, true
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(x))
		if err != nil {
			return false, false
		}
		return b, true
	default:
		return false, false
	}
}
