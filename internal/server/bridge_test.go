package server

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// TestBridgeWriterHeartbeatDoesNotEatFirstFrame SSE 注释行（`: heartbeat`）不能连带
// 吞掉紧随其后的第一个 data 帧。
//
// 实案（2026-10-28，glm-5.3）：上游在流首必发一条 `: heartbeat` 心跳，它与第一帧
// 同属一个 `\n\n` 块。旧实现要求「整块以 data: 开头」，于是整块被丢弃——而第一帧
// 恰好是携带 tool_calls 的 name/id 的那一帧。后果：
//
//	· /v1/responses 流式 4/4 轮丢失全部工具调用（output=[] 但 output_tokens=12）
//	· /v1/messages 流式首个正文分片被吃掉
//	· /v1/chat/completions 不受影响（字节原样透传、不经过本函数）——所以很难发现
func TestBridgeWriterHeartbeatDoesNotEatFirstFrame(t *testing.T) {
	frame := func(content string) string {
		return `data: {"choices":[{"delta":{"content":"` + content + `"},"index":0}]}` + "\n\n"
	}
	rec := &recordingWriter{hdr: make(http.Header)}
	bw := newBridgeWriter(rec, protoResponses, "glm-5.3")

	// 拼一个真实的流：心跳 + 首帧同块，后续帧正常
	stream := ": heartbeat\n" + frame("首") + frame("续") + "data: [DONE]\n\n"
	// 模拟 SSE 头
	bw.Header().Set("Content-Type", "text/event-stream")
	bw.WriteHeader(http.StatusOK)
	if _, err := bw.Write([]byte(stream)); err != nil {
		t.Fatalf("write: %v", err)
	}
	bw.Close()

	got := rec.body.String()
	// 首帧文本必须出现在下发的事件里（旧实现会丢"首"）
	if !strings.Contains(got, "首") {
		t.Errorf("心跳后的第一帧被吞了：\n%s", got)
	}
	if !strings.Contains(got, "续") {
		t.Errorf("后续帧也丢了：\n%s", got)
	}
	if !strings.Contains(got, "response.completed") {
		t.Errorf("流没有正常收尾：\n%s", got)
	}
}

// recordingWriter 收集写出的字节，并提供一个可用的 Flusher。
type recordingWriter struct {
	hdr    http.Header
	status int
	body   bytes.Buffer
}

func (w *recordingWriter) Header() http.Header { return w.hdr }
func (w *recordingWriter) WriteHeader(s int)   { w.status = s }
func (w *recordingWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}
func (w *recordingWriter) Flush() {}
