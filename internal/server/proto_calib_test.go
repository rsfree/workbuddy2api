// proto_calib_test.go count_tokens 的在线校准闭环：对话响应里的真实
// usage.prompt_tokens 要能反过来修正 count_tokens 的输出。
//
// 样本采集点在 recordAttempt（chat 链路 usage 的唯一汇聚点），不在协议前门的
// 写入器里——所以这里同时验两件事：①/v1/messages 的流量能供血；②直接打
// /v1/chat/completions 的流量同样供血（前门只是复用同一条链路的壳）。
// 单测覆盖不到这条跨层链路：系数「学是学了」但没接上时，表现为 count_tokens
// 永远返回字符粗估值，Claude Code 的压缩时机依旧偏早/偏晚。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/bridge"
	"github.com/linguo2625469/workbuddy2api-panel/internal/tokencalib"
)

// calibSSE 造一条带指定 prompt_tokens 的 chat 流（末帧带 usage）。
func calibSSE(prompt int) string {
	first := `{"id":"c1","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2",` +
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}`
	last := fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2",`+
		`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":%d,"completion_tokens":2,"total_tokens":%d}}`, prompt, prompt+2)
	return "data: " + first + "\n\n" + "data: " + last + "\n\n" + "data: [DONE]\n\n"
}

// postCountTokens 走 HTTP 层调 count_tokens，返回 input_tokens。
func postCountTokens(t *testing.T, h *Handler, body string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("count_tokens code=%d body=%s", rec.Code, rec.Body.String())
	}
	n, _ := decodeBody(t, rec)["input_tokens"].(float64)
	return int(n)
}

// TestCountTokensLearnsFromResponses 校准闭环：先取一次粗估，再让上游每轮都
// 回「实际 = 3 倍粗估」，几十轮后 count_tokens 必须向真实值收敛。
func TestCountTokensLearnsFromResponses(t *testing.T) {
	// 先按粗估跑一次拿到基准，再决定上游该回多少——这样断言不依赖
	// EstimateInputTokens 的具体公式（公式改了测试不用跟着改）。
	actualPrompt := 0
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		return 200, calibSSE(actualPrompt), true
	})
	cal := tokencalib.New("")
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		TokenCalib: cal,
	})

	// 探针文本取长一点：估算值太小的话，四舍五入会淹没比例差异。
	probe := strings.Repeat("hello world, ", 40)
	body := `{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"` + probe + `"}]}`

	before := postCountTokens(t, h, body)
	if before < 50 {
		t.Fatalf("基准估算=%d 太小，无法验证比例", before)
	}

	// 关键：30 轮必须用**不同长度**。长度全相同（x 无方差）时 slope 与 intercept
	// 不可辨识，校准器会按设计退到「窄带宽」分支（只允许斜率在 0.5~2 内微调），
	// 那时学不到 3 倍——那是保守行为，不是失败。要验证「能学到」就得给出方差。
	for i := 0; i < 30; i++ {
		text := strings.Repeat("hello world, ", 10+i*3)
		// 上游回「实际 = 3 × 该请求的本地粗估」：比例固定 3，与长度无关，
		// 正好对应一条过原点的直线 y=3x。
		actualPrompt = 3 * estOf(text)
		reqBody := `{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"` + text + `"}]}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody)))
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 轮 /v1/messages code=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}

	after := postCountTokens(t, h, body)
	ratio := float64(after) / float64(before)
	// 目标 3.0；先验收缩会把它压在 3 以下（30 条时权重 ≈ 0.88）。
	if ratio < 2.4 || ratio > 3.05 {
		t.Fatalf("校准后估算=%d（基准 %d）比例 %.2f want ≈3", after, before, ratio)
	}

	// /status 必须能看到学到的参数，运维才能判断「系数偏了」还是「没学到」。
	p := cal.Params("glm-5.2")
	if p.Samples != 30 {
		t.Errorf("samples=%d want 30", p.Samples)
	}
	if p.Slope < 2.4 || p.Slope > 3.05 {
		t.Errorf("slope=%.2f want ≈3（含收缩）", p.Slope)
	}

	rec := httptest.NewRecorder()
	h.status(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var out struct {
		TokenCalib map[string]tokencalib.Params `json:"token_calib"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("/status 不是 JSON: %v", err)
	}
	if _, ok := out.TokenCalib["glm-5.2"]; !ok {
		t.Errorf("/status.token_calib 缺模型条目: %v", out.TokenCalib)
	}
}

// TestCountTokensUnaffectedWithoutCalibrator 没装配校准器时保持纯粗估，
// 且不能因为 nil 指针崩掉（裸用 / 测试路径）。
func TestCountTokensUnaffectedWithoutCalibrator(t *testing.T) {
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		t.Error("count_tokens 不应打上游")
		return 200, sseOK, true
	})
	h := newProtoHandler(t, up) // Config.TokenCalib 为 nil

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`
	if got := postCountTokens(t, h, body); got <= 0 {
		t.Fatalf("input_tokens=%d want >0", got)
	}
}

// TestCalibrationSkipsErrorResponses 上游报错（没有 usage）不能喂进校准器：
// 否则错误流量会把系数带偏。
func TestCalibrationSkipsErrorResponses(t *testing.T) {
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		return 500, `{"error":{"message":"boom","type":"api_error"}}`, false
	})
	cal := tokencalib.New("")
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		TokenCalib: cal,
	})

	body := `{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	if rec.Code == http.StatusOK {
		t.Fatalf("上游 500 时不该回 200: %s", rec.Body.String())
	}
	if p := cal.Params("glm-5.2"); p.Samples != 0 {
		t.Fatalf("错误响应被计入校准: samples=%d", p.Samples)
	}
}

// TestChatPathTrainsCalibration 只打 /v1/chat/completions（完全不走协议前门）
// 也要能把系数学到手：recordAttempt 是 usage 的唯一汇聚点，Claude Code 之外
// 的流量因此也是校准样本——这正是「别在 bridgeWriter 里单独再采一份」的理由。
func TestChatPathTrainsCalibration(t *testing.T) {
	actualPrompt := 0
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		return 200, calibSSE(actualPrompt), true
	})
	cal := tokencalib.New("")
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		TokenCalib: cal,
	})

	text := strings.Repeat("hello world, ", 40)
	// 估算侧只看 messages 内容，两个协议的同一条 user 文本估算值相同，
	// 所以可以直接用 count_tokens 的输出当基准。
	anthBody := `{"model":"glm-5.2","messages":[{"role":"user","content":"` + text + `"}]}`
	before := postCountTokens(t, h, anthBody)
	actualPrompt = 3 * before

	chatBody := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"` + text + `"}]}`
	for i := 0; i < 30; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody)))
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 轮 chat code=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}

	p := cal.Params("glm-5.2")
	if p.Samples != 30 {
		t.Fatalf("chat 流量没进校准器: samples=%d want 30", p.Samples)
	}
	if after := postCountTokens(t, h, anthBody); after <= before {
		t.Fatalf("校准后估算=%d 未高于基准 %d（chat 样本未生效）", after, before)
	}
}

// TestCalibrationSkipsImageRequests 带图请求不能当样本：图像 token 会计入上游
// prompt_tokens，而字符估算侧数不到图，混进来会把 slope 抬高、反而让纯文本
// count_tokens 偏大。
func TestCalibrationSkipsImageRequests(t *testing.T) {
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		return 200, calibSSE(9000), true
	})
	cal := tokencalib.New("")
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		TokenCalib: cal,
	})

	chatBody := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"这张图里是什么"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("chat code=%d body=%s", rec.Code, rec.Body.String())
	}
	if p := cal.Params("glm-5.2"); p.Samples != 0 {
		t.Fatalf("带图请求被计入校准: samples=%d", p.Samples)
	}
}

// estOf 用网关自己的估算函数算单条 user 文本的粗估，测试里不另写一份公式。
func estOf(text string) int {
	return bridge.EstimateInputTokens(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": text}},
	})
}
