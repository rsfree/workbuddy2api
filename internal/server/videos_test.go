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
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// TestVideoSubmitAndPoll 提交流程：返回 task id；轮询用提交时的账号拿成片。
func TestVideoSubmitAndPoll(t *testing.T) {
	var submitPath, submitBody, queryBody string
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/videos/generations":
			submitPath = r.URL.Path
			b, _ := io.ReadAll(r.Body)
			submitBody = string(b)
			_, _ = w.Write([]byte(`{"code":0,"requestId":"req-1","data":{"id":"task-1"}}`))
		case "/v2/videos/tasks":
			b, _ := io.ReadAll(r.Body)
			queryBody = string(b)
			_, _ = w.Write([]byte(`{"code":0,"data":{"status":"completed",
				"data":[{"url":"https://cdn.example/out.mp4"}],"usage":{"credit":9.9}}}`))
		default:
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
	})
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up, RequestLog: reqLog})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/videos/generations",
		strings.NewReader(`{"prompt":"雪地里的猫","seconds":5,"resolution":"768P"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("submit code=%d body=%s", rec.Code, rec.Body)
	}
	var submitted map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &submitted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if submitted["id"] != "task-1" || submitted["status"] != "queued" {
		t.Fatalf("submit resp=%v", submitted)
	}
	if submitted["model"] != "minimax-video-h3" {
		t.Errorf("默认模型=%v want minimax-video-h3", submitted["model"])
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("视频提交缺少 X-Request-Id")
	}
	if snap := reqLog.Snapshot(); snap.Completed != 1 || len(snap.Recent) != 1 ||
		snap.Recent[0].Path != "/v1/videos/generations" || snap.Recent[0].Model != "minimax-video-h3" {
		t.Fatalf("视频提交未正确进入请求日志：%+v", snap)
	}
	if got := rec.Header().Get("X-WB2A-Video-Task"); got != "task-1" {
		t.Errorf("X-WB2A-Video-Task=%q", got)
	}
	if submitPath != "/v2/videos/generations" {
		t.Errorf("submit path=%q", submitPath)
	}
	if !strings.Contains(submitBody, `"prompt":"雪地里的猫"`) {
		t.Errorf("submit body=%s", submitBody)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/videos/task-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("poll code=%d body=%s", rec.Code, rec.Body)
	}
	var polled map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &polled); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if polled["status"] != "completed" || polled["url"] != "https://cdn.example/out.mp4" {
		t.Fatalf("poll resp=%v", polled)
	}
	usageMap, _ := polled["usage"].(map[string]any)
	if usageMap["credit"] != 9.9 {
		t.Errorf("usage=%v", polled["usage"])
	}
	if queryBody != `{"task_id":"task-1"}` {
		t.Errorf("query body=%s", queryBody)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("视频轮询缺少 X-Request-Id")
	}
	if snap := reqLog.Snapshot(); snap.Completed != 2 || len(snap.Recent) != 2 ||
		snap.Recent[0].Path != "/v1/videos/task-1" || snap.Recent[0].Status != http.StatusOK {
		t.Fatalf("视频轮询未正确进入请求日志：%+v", snap)
	}
}

// TestVideoPollUnknownTask 未登记的 task 直接 404，不打上游。
func TestVideoPollUnknownTask(t *testing.T) {
	var calls int32
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{}`))
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/videos/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("未知任务不应打上游，实际 %d 次", got)
	}
}

// TestVideoSubmitValidation 无提示词且无首帧 → 400，不打上游。
func TestVideoSubmitValidation(t *testing.T) {
	var calls int32
	up := newSearchUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"id":"x"}}`))
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/videos/generations",
		strings.NewReader(`{"prompt":"  "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("参数错误不应打上游，实际 %d 次", got)
	}
}
