package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newImageUpstream 构造一个只处理图像的假上游：记录每次请求，按回调返回。
func newImageUpstream(t *testing.T, fn func(r *http.Request) (int, string)) (*upstream.Client, *int32) {
	t.Helper()
	var calls int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			status, body := fn(r)
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		WebBaseCN: "https://img.example",
	}
	return up, &calls
}

func imageTestAccount(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: "at-" + uid, ExpiresAt: 9999999999}
}

// withFakeImageFetch 替换 URL 参考图下载器：真实 client 有 SSRF 防护，会拒绝
// 测试用的假域名/回环地址。
func withFakeImageFetch(t *testing.T) {
	t.Helper()
	old := imageFetchClient
	imageFetchClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(strings.NewReader("\x89PNG\r\n\x1a\nref-bytes")),
		}, nil
	})}
	t.Cleanup(func() { imageFetchClient = old })
}

// TestImageGenerationsSuccess 文生图：路径、请求体、OpenAI 响应形态与积分透出。
func TestImageGenerationsSuccess(t *testing.T) {
	var gotPath, gotBody string
	up, _ := newImageUpstream(t, func(r *http.Request) (int, string) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return 200, `{"code":0,"request_id":"rid","data":{"data":[{"url":"https://cdn.example/1.png"}],"usage":{"credit":7}}}`
	})
	h := NewHandler(Config{Pool: testPoolWith(imageTestAccount("u1")), Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"hunyuan-image-v3.0","prompt":"a cat","size":"512x512","n":2}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if gotPath != "/v2/images/generations" {
		t.Errorf("upstream path=%q want /v2/images/generations", gotPath)
	}
	if !strings.Contains(gotBody, `"prompt":"a cat"`) || !strings.Contains(gotBody, `"n":2`) {
		t.Errorf("upstream body=%s", gotBody)
	}
	var resp struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
		Usage struct {
			Credit float64 `json:"credit"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body)
	}
	if len(resp.Data) != 1 || resp.Data[0].URL != "https://cdn.example/1.png" {
		t.Errorf("data=%+v", resp.Data)
	}
	if resp.Usage.Credit != 7 {
		t.Errorf("usage.credit=%v want 7", resp.Usage.Credit)
	}
	// 上游只回 1 张但请求 n=2：必须回 warning，不静默。
	if got := rec.Header().Get("X-WB2A-Image-Warn"); !strings.Contains(got, "requested-n-2-got-1") {
		t.Errorf("X-WB2A-Image-Warn=%q want requested-n-2-got-1", got)
	}
}

// TestImageEditsMultipart 图生图 multipart：文件转 data URI，走独立 edits 端点。
func TestImageEditsMultipart(t *testing.T) {
	var gotPath, gotBody string
	up, _ := newImageUpstream(t, func(r *http.Request) (int, string) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return 200, `{"code":0,"data":{"data":[{"b64_json":"aGk="}]}}`
	})
	h := NewHandler(Config{Pool: testPoolWith(imageTestAccount("u1")), Upstream: up})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "make it blue")
	_ = mw.WriteField("model", "hunyuan-image-alpha-edit")
	fw, err := mw.CreateFormFile("image", "a.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("\x89PNG\r\n\x1a\nsome-bytes"))
	fw2, err := mw.CreateFormFile("image", "b.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw2.Write([]byte("\x89PNG\r\n\x1a\nsecond-bytes"))
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/v1/images/edits", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if gotPath != "/v2/images/edits" {
		t.Errorf("upstream path=%q want /v2/images/edits", gotPath)
	}
	if !strings.Contains(gotBody, `"image":["data:image/png;base64,`) {
		t.Errorf("upstream body 未把文件转成 data URI: %s", gotBody)
	}
	var sent struct {
		Image []string `json:"image"`
	}
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if len(sent.Image) != 2 {
		t.Errorf("image parts=%d want 2（多图顺序与数量都要保留）", len(sent.Image))
	}
}

// TestImageGenerationsWithRefRoutesToEdits generations 带参考图时自动改走 edits，
// 且未显式指定 model 时用图生图默认模型（不把垫图静默丢掉）。
func TestImageGenerationsWithRefRoutesToEdits(t *testing.T) {
	withFakeImageFetch(t)
	var gotPath, gotBody string
	up, _ := newImageUpstream(t, func(r *http.Request) (int, string) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return 200, `{"code":0,"data":{"data":[{"url":"https://cdn.example/2.png"}]}}`
	})
	h := NewHandler(Config{Pool: testPoolWith(imageTestAccount("u1")), Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"prompt":"blend them","images":["https://cdn.example/ref.png"]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if gotPath != "/v2/images/edits" {
		t.Errorf("upstream path=%q want /v2/images/edits", gotPath)
	}
	if !strings.Contains(gotBody, `"model":"`+defaultImageEditModel+`"`) {
		t.Errorf("body=%s want model %s", gotBody, defaultImageEditModel)
	}
	if !strings.Contains(gotBody, `"image":["data:image/png;base64,`) {
		t.Errorf("body=%s want URL ref normalized to data URI", gotBody)
	}
}

// TestImageInvalidIsNotRotated 请求级错误（图无效 11135）不得把所有账号轮一遍。
func TestImageInvalidIsNotRotated(t *testing.T) {
	up, calls := newImageUpstream(t, func(r *http.Request) (int, string) {
		return 400, `{"code":11135,"msg":"invalid image data"}`
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(imageTestAccount("u1"), imageTestAccount("u2")),
		Upstream: up,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"hunyuan-image-v3.0","prompt":"x","size":"512x512"}`)))

	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("upstream calls=%d want 1（请求级错误不轮转）", got)
	}
	if !strings.Contains(rec.Body.String(), "invalid image data") {
		t.Errorf("body=%s want 上游原文", rec.Body)
	}
}

// TestImageFallbackModel 未知图像模型名按配置兜底，并对调用方透出替换关系。
func TestImageFallbackModel(t *testing.T) {
	var gotBody string
	up, _ := newImageUpstream(t, func(r *http.Request) (int, string) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return 200, `{"code":0,"data":{"data":[{"url":"https://cdn.example/3.png"}]}}`
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(imageTestAccount("u1")),
		Upstream: up,
		Images: &ImagesConfig{
			Enabled:       true,
			DefaultModel:  defaultImageModel,
			EditModel:     defaultImageEditModel,
			FallbackModel: "hunyuan-image-alpha",
		},
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations",
		strings.NewReader(`{"model":"brand-new-image-model","prompt":"x"}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(gotBody, `"model":"hunyuan-image-alpha"`) {
		t.Errorf("body=%s want fallback model", gotBody)
	}
	if got := rec.Header().Get("X-WB2A-Model-Fallback"); !strings.Contains(got, "brand-new-image-model") {
		t.Errorf("X-WB2A-Model-Fallback=%q", got)
	}
}

// TestChatRejectsImageModel 图像模型不能走 chat：本地 400，不打上游（上游只会回 11102）。
// TestImageChatCompletion 图像模型的 chat 前门：messages → prompt → 生图 →
// chat.completion（system 提示词不混进生图 prompt）。
func TestImageChatCompletion(t *testing.T) {
	var gotPath, gotBody string
	up, _ := newImageUpstream(t, func(r *http.Request) (int, string) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return 200, `{"code":0,"data":{"data":[{"url":"https://cdn.example/chat.png"}],"usage":{"credit":5.71}}}`
	})
	h := NewHandler(Config{Pool: testPoolWith(imageTestAccount("u1")), Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+defaultImageModel+`","messages":[`+
			`{"role":"system","content":"you are a helpful assistant"},`+
			`{"role":"user","content":"draw a cat"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if gotPath != "/v2/images/generations" {
		t.Errorf("upstream path=%q", gotPath)
	}
	if strings.Contains(gotBody, "helpful assistant") {
		t.Errorf("system prompt 混进了生图 prompt: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"prompt":"draw a cat"`) {
		t.Errorf("upstream body=%s", gotBody)
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			Credit float64 `json:"credit"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body)
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 {
		t.Fatalf("resp=%+v", resp)
	}
	if !strings.Contains(resp.Choices[0].Message.Content, "https://cdn.example/chat.png") {
		t.Errorf("content=%q", resp.Choices[0].Message.Content)
	}
	if resp.Usage.Credit != 5.71 {
		t.Errorf("usage.credit=%v", resp.Usage.Credit)
	}
}

// TestImageChatStream stream=true 时回 chat.completion.chunk + [DONE]。
func TestImageChatStream(t *testing.T) {
	up, _ := newImageUpstream(t, func(r *http.Request) (int, string) {
		return 200, `{"code":0,"data":{"data":[{"url":"https://cdn.example/s.png"}],"usage":{"credit":1.5}}}`
	})
	h := NewHandler(Config{Pool: testPoolWith(imageTestAccount("u1")), Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+defaultImageModel+`","stream":true,"messages":[{"role":"user","content":"draw a sun"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"chat.completion.chunk", "https://cdn.example/s.png", `"finish_reason":"stop"`, "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream 缺少 %q: %s", want, body)
		}
	}
}

// TestImageChatWithImageRoutesToEdits chat 带图 → edits + 编辑模型。
func TestImageChatWithImageRoutesToEdits(t *testing.T) {
	withFakeImageFetch(t)
	var gotPath, gotBody string
	up, _ := newImageUpstream(t, func(r *http.Request) (int, string) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return 200, `{"code":0,"data":{"data":[{"url":"https://cdn.example/e.png"}]}}`
	})
	h := NewHandler(Config{Pool: testPoolWith(imageTestAccount("u1")), Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+defaultImageModel+`","messages":[{"role":"user","content":[`+
			`{"type":"text","text":"make it blue"},`+
			`{"type":"image_url","image_url":{"url":"https://cdn.example/ref.png"}}]}]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if gotPath != "/v2/images/edits" {
		t.Errorf("upstream path=%q want /v2/images/edits", gotPath)
	}
	if !strings.Contains(gotBody, `"model":"`+defaultImageEditModel+`"`) {
		t.Errorf("body=%s want edit model", gotBody)
	}
	if !strings.Contains(gotBody, `"image":["data:image/png;base64,`) {
		t.Errorf("body=%s want normalized ref", gotBody)
	}
}

// TestImageChatEditModelWithoutImage 编辑模型没垫图 → 切默认文本模型并告警。
func TestImageChatEditModelWithoutImage(t *testing.T) {
	var gotBody string
	up, _ := newImageUpstream(t, func(r *http.Request) (int, string) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return 200, `{"code":0,"data":{"data":[{"url":"https://cdn.example/d.png"}]}}`
	})
	h := NewHandler(Config{Pool: testPoolWith(imageTestAccount("u1")), Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+defaultImageEditModel+`","messages":[{"role":"user","content":"draw a dog"}]}`)))

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(gotBody, `"model":"`+defaultImageModel+`"`) {
		t.Errorf("body=%s want default model switch", gotBody)
	}
	if got := rec.Header().Get("X-WB2A-Image-Warn"); !strings.Contains(got, "model-switched-to-") {
		t.Errorf("X-WB2A-Image-Warn=%q", got)
	}
}

func TestIsBlockedImageIP(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true},
		{"100.64.0.1", true},
		{"0.0.0.0", true},
		{"::1", true},
		{"fd00::1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false},
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("bad test ip %s", tc.ip)
		}
		if got := isBlockedImageIP(ip); got != tc.blocked {
			t.Errorf("isBlockedImageIP(%s)=%v want %v", tc.ip, got, tc.blocked)
		}
	}
}

// TestNormalizeRefRejectsPrivateURL SSRF 防护：URL 参考图指向回环必须被拒。
func TestNormalizeRefRejectsPrivateURL(t *testing.T) {
	_, err := normalizeImageRefs(context.Background(), []string{"http://127.0.0.1:1/a.png"}, "")
	if err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("err=%v want non-public address rejection", err)
	}
}

// TestNormalizeRawBase64 裸 base64 参考图补 MIME 后转 data URI。
func TestNormalizeRawBase64(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 200)...))
	out, err := normalizeImageRefs(context.Background(), []string{raw}, "")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(out) != 1 || !strings.HasPrefix(out[0], "data:image/png;base64,") {
		t.Fatalf("out=%v", out)
	}
}

// TestFetchURLErrorRedactsSignedQuery 下载失败不能把预签名 URL 回给调用方。
func TestFetchURLErrorRedactsSignedQuery(t *testing.T) {
	old := imageFetchClient
	imageFetchClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: r.URL.String(), Err: errors.New("timeout")}
	})}
	t.Cleanup(func() { imageFetchClient = old })

	_, err := fetchImageAsDataURI(context.Background(), "https://cdn.example/a.png?q-signature=SECRET&q-ak=AKID")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "q-signature") {
		t.Fatalf("error leaked signed query: %v", err)
	}
	if !strings.Contains(err.Error(), "download failed") {
		t.Fatalf("err=%v", err)
	}
}

// TestNormalizeRefInvalidSourceIP 配置里的源 IP 非法时直接报错，不静默回落。
func TestNormalizeRefInvalidSourceIP(t *testing.T) {
	_, err := normalizeImageRefs(context.Background(), []string{"https://example.com/a.png"}, "not-an-ip")
	if err == nil || !strings.Contains(err.Error(), "fetch_source_ip") {
		t.Fatalf("err=%v want invalid fetch_source_ip", err)
	}
}

// TestModelsIncludesMedia /v1/models 末尾动态追加图像/视频模型：
// 图像带专用端点，视频只标 type（端点尚未实现，不给假 endpoint）。
func TestModelsIncludesMedia(t *testing.T) {
	clearDynamicModelsCacheForTest()
	t.Cleanup(clearDynamicModelsCacheForTest)

	up, _ := newImageUpstream(t, func(r *http.Request) (int, string) { return 200, `{}` })
	up.StoreMediaModels("cn", []upstream.ModelInfo{
		{ID: "hunyuan-image-alpha", Name: "Hunyuan Image Alpha", Type: "image", Tags: []string{"text-to-image"}},
		{ID: "kling-video-v3", Name: "Kling-Video-V3", Type: "video", Tags: []string{"text-to-video", "image-to-video"}},
	})
	h := NewHandler(Config{Pool: testPoolWith(imageTestAccount("u1")), Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body)
	}
	byID := map[string]map[string]any{}
	for _, m := range resp.Data {
		byID[m["id"].(string)] = m
	}
	img, ok := byID["cn:hunyuan-image-alpha"]
	if !ok {
		t.Fatalf("image model missing: %v", byID)
	}
	if img["type"] != "image" || img["endpoint"] != "/v1/images/generations" || img["edits_endpoint"] != "/v1/images/edits" {
		t.Errorf("image entry=%v", img)
	}
	video, ok := byID["cn:kling-video-v3"]
	if !ok {
		t.Fatalf("video model missing: %v", byID)
	}
	if video["type"] != "video" {
		t.Errorf("video type=%v", video["type"])
	}
	if video["endpoint"] != "/v1/videos/generations" || video["poll_endpoint"] != "/v1/videos/{id}" {
		t.Errorf("video entry 端点=%v", video)
	}
	if video["name"] != "Kling-Video-V3" {
		t.Errorf("video name=%v", video["name"])
	}
}

// TestModelsRefreshQuery ?refresh=1 必须绕过 10min TTL 立即重探。
func TestModelsRefreshQuery(t *testing.T) {
	clearDynamicModelsCacheForTest()
	t.Cleanup(clearDynamicModelsCacheForTest)

	up, calls := newImageUpstream(t, func(r *http.Request) (int, string) { return 200, `{}` })
	h := NewHandler(Config{Pool: testPoolWith(imageTestAccount("u1")), Upstream: up})

	// 预热缓存：未带 refresh 的请求不应打上游。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "cached-chat"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("cached request hit upstream %d times", got)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models?refresh=1", nil))
	if got := atomic.LoadInt32(calls); got == 0 {
		t.Fatal("refresh=1 未触发上游重探")
	}
}

func clearDynamicModelsCacheForTest() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// TestImageWatermarkDefaultOff 网关默认不带水印；显式 watermark=true 时透传。
func TestImageWatermarkDefaultOff(t *testing.T) {
	var bodies []string
	up, _ := newImageUpstream(t, func(r *http.Request) (int, string) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		return 200, `{"code":0,"data":{"data":[{"url":"https://cdn.example/1.png"}],"usage":{"credit":1}}}`
	})
	h := NewHandler(Config{Pool: testPoolWith(imageTestAccount("u1")), Upstream: up})

	for _, body := range []string{
		`{"model":"hunyuan-image-alpha","prompt":"a cat"}`,
		`{"model":"hunyuan-image-alpha","prompt":"a cat","watermark":true}`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies=%d", len(bodies))
	}
	if !strings.Contains(bodies[0], `"watermark":false`) {
		t.Errorf("默认未关闭水印: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], `"watermark":true`) {
		t.Errorf("显式开启未透传: %s", bodies[1])
	}
}
