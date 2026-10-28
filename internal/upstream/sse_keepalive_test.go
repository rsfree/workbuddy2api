package upstream

import (
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStreamHintKeepaliveDuringSilence 上游静默时网关必须给客户端补心跳。
//
// 为什么：上游在长思考（deepseek 推理 / kimi 长上下文）期间可能整分钟不发
// 任何字节，而 Claude Code / Codex 的空闲阈值只有 60–120s——先断的是客户端，
// 上游还在跑，这一轮额度已经烧掉了。这里把 idle 压到 40ms 验证：
//   - 静默期真的写出 `: keepalive`；
//   - 心跳出现在业务帧**之前**（不是收尾时才补）；
//   - 心跳不破坏 `data: [DONE]` 收尾。
func TestStreamHintKeepaliveDuringSilence(t *testing.T) {
	oldIdle, oldCheck := sseKeepaliveIdle, sseKeepaliveCheck
	sseKeepaliveIdle, sseKeepaliveCheck = 40*time.Millisecond, 10*time.Millisecond
	defer func() { sseKeepaliveIdle, sseKeepaliveCheck = oldIdle, oldCheck }()

	pr, pw := io.Pipe()
	rec := httptest.NewRecorder()

	var wg sync.WaitGroup
	var serr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		serr = StreamHint(rec, pr, nil)
	}()

	// 模拟「上游正在长思考」：150ms 内一个字节都不给。
	time.Sleep(150 * time.Millisecond)
	if _, err := io.WriteString(pw, `data: {"choices":[{"delta":{"content":"hi"},"index":0}]}`+"\n\n"); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if _, err := io.WriteString(pw, "data: [DONE]\n\n"); err != nil {
		t.Fatalf("write done: %v", err)
	}
	_ = pw.Close()
	wg.Wait()

	body := rec.Body.String()
	ki := strings.Index(body, sseKeepaliveFrame)
	if ki < 0 {
		t.Fatalf("静默期没有心跳帧：%q", body)
	}
	hi := strings.Index(body, `"hi"`)
	if hi < 0 {
		t.Fatalf("业务帧丢了：%q", body)
	}
	if ki > hi {
		t.Errorf("心跳应在业务帧之前，got keepalive@%d content@%d", ki, hi)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("[DONE] 收尾被破坏：%q", body)
	}
	if serr != nil {
		t.Errorf("StreamHint err=%v", serr)
	}
	// 心跳必须是完整的注释行（以 ':' 开头、独占一帧），不能是半截 data 帧。
	if !strings.Contains(body, "\n\n"+sseKeepaliveFrame) && !strings.HasPrefix(body, sseKeepaliveFrame) {
		t.Errorf("心跳帧没有独占一行/一帧：%q", body)
	}
}

// TestStreamHintNoKeepaliveWhileActive 持续吐数据时不该插入心跳：
// 每帧都会刷新 lastWrite，静默阈值永远不成立。这条防止「心跳写得太勤」
// 把正常流撑大（客户端日志噪音 + 无谓 flush）。
func TestStreamHintNoKeepaliveWhileActive(t *testing.T) {
	oldIdle, oldCheck := sseKeepaliveIdle, sseKeepaliveCheck
	sseKeepaliveIdle, sseKeepaliveCheck = 60*time.Millisecond, 10*time.Millisecond
	defer func() { sseKeepaliveIdle, sseKeepaliveCheck = oldIdle, oldCheck }()

	pr, pw := io.Pipe()
	rec := httptest.NewRecorder()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = StreamHint(rec, pr, nil)
	}()

	// 总时长 > idle，但帧间隔 < idle：不该有心跳。
	for i := 0; i < 8; i++ {
		if _, err := io.WriteString(pw, `data: {"choices":[{"delta":{"content":"x"},"index":0}]}`+"\n\n"); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _ = io.WriteString(pw, "data: [DONE]\n\n")
	_ = pw.Close()
	wg.Wait()

	if body := rec.Body.String(); strings.Contains(body, sseKeepaliveFrame) {
		t.Errorf("活跃流里不该出现心跳：%q", body)
	}
}
