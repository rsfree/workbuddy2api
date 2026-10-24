// proto_baseline_test.go count_tokens 的「会话基线」闭环：真实 usage 只在上游
// 跑完之后才有，所以它只能从 chat 链路回流，再被下一次 count_tokens 复用。
//
// 这里刻意按线上时序走：先探（count_tokens 未命中，登记预测）→ 再真跑一轮 chat
// （拿到真实 prompt_tokens，顺带给上一条预测打分）→ 再探（这次必须原样返回真数）。
// 三段串起来才证明「复用」这件事真的通了，单测只测 Baseline 覆盖不到跨层接线。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/tokencalib"
)

// newBaselineHandler 装配带基线缓存的 handler。
func newBaselineHandler(t *testing.T, prompt int) (*Handler, *tokencalib.Baseline) {
	t.Helper()
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		return 200, calibSSE(prompt), true
	})
	base := tokencalib.NewBaseline(0)
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		TokenCalib: tokencalib.New(""),
		Baseline:   base,
	})
	return h, base
}

// TestCountTokensReusesUpstreamUsage 核心断言：count_tokens 对同一批 messages
// 必须返回上游真数（不是粗估、不是系数换算）。
func TestCountTokensReusesUpstreamUsage(t *testing.T) {
	const realPrompt = 12000
	h, base := newBaselineHandler(t, realPrompt)

	anthMsgs := `[{"role":"user","content":"hello baseline"}]`
	anth := `{"model":"glm-5.2","messages":` + anthMsgs + `}`

	// ① 首轮探针：没有任何历史，只能粗估（并登记预测）。
	first := postCountTokens(t, h, anth)
	if first <= 0 || first >= realPrompt {
		t.Fatalf("首轮估算=%d 应在 (0,%d) 之间（此时无基线）", first, realPrompt)
	}

	// ② 真跑一轮，usage.prompt_tokens 回流成基线。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":`+anthMsgs+`}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/messages code=%d body=%s", rec.Code, rec.Body.String())
	}

	// ③ 再探同一批：完全命中 → 一字不差地返回上游原话，零估算。
	if got := postCountTokens(t, h, anth); got != realPrompt {
		t.Fatalf("count_tokens=%d want %d（完全命中应直接复用真实 usage）", got, realPrompt)
	}

	// ④ 追加一条消息：基线 + 新增增量，必须比基线大、但远小于「整段重新估」。
	appended := `{"model":"glm-5.2","messages":[{"role":"user","content":"hello baseline"},` +
		`{"role":"user","content":"` + strings.Repeat("x", 300) + `"}]}`
	got := postCountTokens(t, h, appended)
	if got <= realPrompt {
		t.Fatalf("追加后 count_tokens=%d 应大于基线 %d", got, realPrompt)
	}
	if got > realPrompt*2 {
		t.Fatalf("追加后 count_tokens=%d 偏离基线 %d 太远（增量估算失控）", got, realPrompt)
	}

	snap := base.Snapshot()
	if snap["hits"].(uint64) < 2 {
		t.Errorf("hits=%v want ≥2（第 ③④ 步都该命中）", snap["hits"])
	}
	if snap["misses"].(uint64) < 1 {
		t.Errorf("misses=%v want ≥1（首轮探针）", snap["misses"])
	}
}

// TestCountTokensBaselinePerModel 基线按模型隔离：不同模型同一批 messages
// 不能互借真数（tokenizer 不同，借过去就是错值）。
func TestCountTokensBaselinePerModel(t *testing.T) {
	const realPrompt = 12000
	h, _ := newBaselineHandler(t, realPrompt)

	anthMsgs := `[{"role":"user","content":"same text both models"}]`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":`+anthMsgs+`}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/messages code=%d body=%s", rec.Code, rec.Body.String())
	}

	if got := postCountTokens(t, h, `{"model":"glm-5.2","messages":`+anthMsgs+`}`); got != realPrompt {
		t.Fatalf("同模型=%d want %d", got, realPrompt)
	}
	// 换模型：同样的消息不该命中 glm-5.2 的基线。
	if got := postCountTokens(t, h, `{"model":"kimi-k2.5","messages":`+anthMsgs+`}`); got == realPrompt {
		t.Fatalf("换模型仍复用了另一模型的真数（%d），基线未按模型隔离", got)
	}
}

// TestStatusExposesBaselineAccuracy /status 要能看到「预测 vs 真实」的误差，
// 否则线上无法判断这套估算能不能用、该不该继续。
func TestStatusExposesBaselineAccuracy(t *testing.T) {
	const realPrompt = 12000
	h, _ := newBaselineHandler(t, realPrompt)

	anthMsgs := `[{"role":"user","content":"accuracy probe"}]`
	anth := `{"model":"glm-5.2","messages":` + anthMsgs + `}`
	predicted := postCountTokens(t, h, anth) // 登记一条预测

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":`+anthMsgs+`}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/messages code=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.status(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var out struct {
		TokenBaseline struct {
			Hits         int                       `json:"hits"`
			Misses       int                       `json:"misses"`
			PredVerified int                       `json:"pred_verified"`
			PredPending  int                       `json:"pred_pending"`
			PredByModel  map[string]map[string]any `json:"pred_by_model"`
		} `json:"token_baseline"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("/status 不是 JSON: %v", err)
	}
	if out.TokenBaseline.PredVerified != 1 {
		t.Fatalf("pred_verified=%d want 1（预测应已被那次 chat 的真实值打分）", out.TokenBaseline.PredVerified)
	}
	if out.TokenBaseline.PredPending != 0 {
		t.Fatalf("pred_pending=%d want 0", out.TokenBaseline.PredPending)
	}
	st := out.TokenBaseline.PredByModel["glm-5.2"]
	if st == nil {
		t.Fatalf("pred_by_model 缺 glm-5.2: %v", out.TokenBaseline.PredByModel)
	}
	mae, _ := st["mae"].(float64)
	// 预测就是粗估（约几百 token），真实 12000 → 误差必然很大。这不是缺陷，
	// 正是要暴露的东西：首轮没有基线时估算偏差有多大，线上靠这个数决定要不要优化。
	wantMae := 1 - float64(predicted)/float64(realPrompt)
	if mae < wantMae-0.01 || mae > wantMae+0.01 {
		t.Fatalf("mae=%v want ≈%.3f（predicted=%d / actual=%d）", mae, wantMae, predicted, realPrompt)
	}
}

// TestCountTokensBaselineIgnoresErrorResponses 上游报错没有 usage，不能进基线：
// 否则下一次 count_tokens 会拿一条根本不存在的「真值」回去。
func TestCountTokensBaselineIgnoresErrorResponses(t *testing.T) {
	up, _ := newProtoUpstream(t, func(string) (int, string, bool) {
		return 500, `{"error":{"message":"boom","type":"api_error"}}`, false
	})
	base := tokencalib.NewBaseline(0)
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		TokenCalib: tokencalib.New(""),
		Baseline:   base,
	})

	anthMsgs := `[{"role":"user","content":"boom"}]`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":`+anthMsgs+`}`)))
	if rec.Code == http.StatusOK {
		t.Fatalf("上游 500 时不该回 200: %s", rec.Body.String())
	}
	if n := base.Snapshot()["entries"].(int); n != 0 {
		t.Fatalf("错误响应被记进基线: entries=%d", n)
	}
}
