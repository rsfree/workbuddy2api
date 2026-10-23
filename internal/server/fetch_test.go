package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestFetchEndpoint /v1/fetch 透出上游正文与元数据。
func TestFetchEndpoint(t *testing.T) {
	var gotPath, gotBody string
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"url":"https://mp.weixin.qq.com/s/x","title":"公众号文章",
			"content":"# 标题\n正文","content_type":"text/html","content_length":8,
			"truncated":false,"status_code":200}`))
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/fetch",
		strings.NewReader(`{"url":"https://mp.weixin.qq.com/s/x","prompt":"提取正文"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if gotPath != "/agenttool/v1/webfetch" {
		t.Errorf("upstream path=%q", gotPath)
	}
	if !strings.Contains(gotBody, `"url":"https://mp.weixin.qq.com/s/x"`) {
		t.Errorf("upstream body=%s", gotBody)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body)
	}
	if resp["title"] != "公众号文章" || resp["content"] != "# 标题\n正文" {
		t.Errorf("resp=%v", resp)
	}
	if resp["object"] != "web_fetch" {
		t.Errorf("object=%v", resp["object"])
	}
}

// TestFetchRejectsBadURL 非 http(s) 或缺失 url 直接 400，不打上游。
func TestFetchRejectsBadURL(t *testing.T) {
	var calls int32
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{}`))
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up})

	for _, body := range []string{`{}`, `{"url":"ftp://x/a"}`, `{"url":"javascript:alert(1)"}`} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/fetch", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s code=%d want 400", body, rec.Code)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("非法 URL 不应打上游，实际 %d 次", got)
	}
}
