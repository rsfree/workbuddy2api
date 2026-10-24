// model_alias_test.go 模型别名端到端：别名写法在选号/出站之前归一到规范名。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// bodyCaptureUpstream 记录发往上游的请求体（别名断言的锚点：出站 body 里的 model
// 必须是规范名，否则上游 11102）。其余行为与 newFakeUpstream 一致（200 SSE）。
func bodyCaptureUpstream(t *testing.T, got *string) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			*got = string(b)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestModelAliasRewritesOutboundBody 别名 kimi-k3.1 必须归一为 kimi-k3.1-pro 再出站；
// 上游没有 kimi-k3.1 这个 id（11102），且 K3.1 系列仅 Pro 一款，不可投向 k3-1。
func TestModelAliasRewritesOutboundBody(t *testing.T) {
	var got string
	up := bodyCaptureUpstream(t, &got)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"kimi-k3.1","messages":[],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatalf("upstream body not json: %v", err)
	}
	if obj["model"] != "kimi-k3.1-pro" {
		t.Fatalf("出站 model=%v want kimi-k3.1-pro（别名未归一，上游必然 11102）", obj["model"])
	}
}

// TestModelAliasWithRealmPrefix cn:kimi-k3.1 → cn 域 + 规范名（前缀剥离不受影响）。
func TestModelAliasWithRealmPrefix(t *testing.T) {
	var got string
	up := bodyCaptureUpstream(t, &got)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:kimi-k3.1","messages":[],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(got, `"model":"kimi-k3.1-pro"`) {
		t.Fatalf("cn: 前缀 + 别名：出站 body 未含规范名，got=%s", got)
	}
}

// TestModelAliasDoesNotTouchOthers 非别名模型名原样出站（含相近名 kimi-k3，
// 老 K3 与 K3.1 是不同模型，误吞会造成计费/能力错配）。
func TestModelAliasDoesNotTouchOthers(t *testing.T) {
	var got string
	up := bodyCaptureUpstream(t, &got)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	for _, m := range []string{"kimi-k3", "glm-5.2", "kimi-k3-1"} {
		got = ""
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+m+`","messages":[],"stream":true}`)))
		if rec.Code != 200 {
			t.Fatalf("model=%s code=%d body=%s", m, rec.Code, rec.Body)
		}
		if !strings.Contains(got, `"model":"`+m+`"`) {
			t.Errorf("model=%s 被改写：got=%s", m, got)
		}
	}
}
