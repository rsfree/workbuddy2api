package server

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedWriter 并发安全的响应记录器：心跳 goroutine 与 handler goroutine
// 都会写它（bridgeWriter 内部用一把锁串行化，这里再保护读侧）。
type lockedWriter struct {
	mu     sync.Mutex
	hdr    http.Header
	status int
	body   bytes.Buffer
}

func (w *lockedWriter) Header() http.Header { return w.hdr }

func (w *lockedWriter) WriteHeader(s int) {
	w.mu.Lock()
	w.status = s
	w.mu.Unlock()
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func (w *lockedWriter) Flush() {}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

// TestBridgeKeepaliveDuringSilence 翻译层（/v1/messages、/v1/responses）自己
// 必须发心跳：上游的 `: heartbeat` 在 handleFrame 里被当注释块丢掉，这条路径
// 上客户端在长思考期间**一个字节都收不到**。
//
// Anthropic 用官方 `event: ping`，Responses 用 SSE 注释行；Close 之后不许再写
// （handler 返回后 ResponseWriter 失效）。
func TestBridgeKeepaliveDuringSilence(t *testing.T) {
	oldIdle, oldCheck := sseKeepaliveIdle, sseKeepaliveCheck
	sseKeepaliveIdle, sseKeepaliveCheck = 40*time.Millisecond, 10*time.Millisecond
	defer func() { sseKeepaliveIdle, sseKeepaliveCheck = oldIdle, oldCheck }()

	cases := []struct {
		name string
		kind protoKind
		want string
	}{
		{"anthropic", protoAnthropic, anthropicPing},
		{"responses", protoResponses, sseComment},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &lockedWriter{hdr: make(http.Header)}
			bw := newBridgeWriter(rec, tc.kind, "glm-5.3")
			bw.Header().Set("Content-Type", "text/event-stream")
			bw.WriteHeader(http.StatusOK)

			// 上游开始长思考：此后不再有任何业务帧。
			time.Sleep(150 * time.Millisecond)
			bw.Close()

			got := rec.String()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("静默期没有心跳 %q：\n%q", tc.want, got)
			}
			// 心跳必须在收尾事件之前，而不是收尾时才补。
			fin := strings.Index(got, "message_stop")
			if tc.kind == protoResponses {
				fin = strings.Index(got, "response.completed")
			}
			if fin >= 0 && strings.Index(got, tc.want) > fin {
				t.Errorf("心跳落在收尾之后（等于没保活）：\n%q", got)
			}

			// Close 之后心跳 goroutine 必须已退出。
			n := len(got)
			time.Sleep(80 * time.Millisecond)
			if after := rec.String(); len(after) != n {
				t.Errorf("Close 之后仍在写：%d → %d 字节", n, len(after))
			}
		})
	}
}

// TestBridgeNoKeepaliveWhileActive 持续有业务帧时不插入心跳。
func TestBridgeNoKeepaliveWhileActive(t *testing.T) {
	oldIdle, oldCheck := sseKeepaliveIdle, sseKeepaliveCheck
	sseKeepaliveIdle, sseKeepaliveCheck = 60*time.Millisecond, 10*time.Millisecond
	defer func() { sseKeepaliveIdle, sseKeepaliveCheck = oldIdle, oldCheck }()

	rec := &lockedWriter{hdr: make(http.Header)}
	bw := newBridgeWriter(rec, protoResponses, "glm-5.3")
	bw.Header().Set("Content-Type", "text/event-stream")
	bw.WriteHeader(http.StatusOK)

	// 总时长超过 idle，但帧间隔小于 idle：lastWrite 一直被刷新。
	for i := 0; i < 8; i++ {
		if _, err := bw.Write([]byte(`data: {"choices":[{"delta":{"content":"x"},"index":0}]}` + "\n\n")); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	bw.Close()

	if got := rec.String(); strings.Contains(got, sseComment) {
		t.Errorf("活跃流里不该出现心跳：\n%q", got)
	}
}
