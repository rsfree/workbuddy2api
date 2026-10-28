// bridge.go 把 chatCompletions 的输出就地翻译成其它对话协议。
//
// 为什么不把 chatCompletions 拆成「核心 + 多个前门」：那是 700+ 行、串起
// 选号/粘性/轮转/冷却/降级/记账的单一链路，任何拆分都要在每个分支上再复制
// 一份「知道当前是什么协议」的判断。这里的做法相反——让 chatCompletions
// 原样跑，只喂给它翻译后的请求，并把它写出来的 OpenAI 形态响应**在写入器层面**
// 翻译回目标协议。好处是管线只有一份：账号处置、错误分类、白名单、记账
// 对新协议前门天然一致，后续改 chat 也不会漏改这里。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/bridge"
)

// protoKind 目标协议种类。
type protoKind int

const (
	protoAnthropic protoKind = iota // /v1/messages
	protoResponses                  // /v1/responses
)

// SSE 心跳参数（与 upstream 的 chat SSE 同口径，见 upstream/sse.go）。
// 翻译层这条路径**必须**自己发心跳：上游的 `: heartbeat` 在 handleFrame 里被
// 当注释块丢弃，客户端（Claude Code / Codex）在长思考期间收不到任何字节就会
// 主动断开——上游还在跑，这一轮额度已经烧掉了。
// 两个时间参数是 var 而非 const：测试压到毫秒级才能断言心跳（运行期不写）。
var (
	sseKeepaliveIdle  = 15 * time.Second
	sseKeepaliveCheck = 5 * time.Second
)

// anthropicPing 是 Anthropic 官方的保活事件形态（客户端按 type=ping 忽略）。
const anthropicPing = "event: ping\ndata: {\"type\":\"ping\"}\n\n"

// sseComment 是通用 SSE 注释保活帧（chat / Responses 两条路径）。
const sseComment = ": keepalive\n\n"

// bridgeWriter 实现 http.ResponseWriter + http.Flusher：
// 上游（chatCompletions）看到的是普通写入器，客户端看到的是目标协议。
type bridgeWriter struct {
	dst   http.ResponseWriter
	kind  protoKind
	model string

	// hdr 自己持有一份响应头：chatCompletions 会往里塞 OpenAI 专属字段，
	// 不能直接漏给目标协议客户端（例如 Content-Type 必须由翻译层决定）。
	hdr    http.Header
	status int

	stream bool
	opened bool // 是否已向客户端发出响应头

	body    bytes.Buffer // 非流式：完整 JSON 响应体
	pending bytes.Buffer // 流式：尚未凑齐一帧的尾部字节

	anth *bridge.AnthropicStream
	rsp  *bridge.ResponsesStream

	// mu 保护 dst 的全部写入与 lastWrite：心跳 goroutine 与 handler goroutine
	// 会并发写同一个 ResponseWriter（http.ResponseWriter 不保证并发安全）。
	mu        sync.Mutex
	lastWrite time.Time
	keepStop  chan struct{}
	keepDone  chan struct{}
}

func newBridgeWriter(dst http.ResponseWriter, kind protoKind, model string) *bridgeWriter {
	return &bridgeWriter{dst: dst, kind: kind, model: model, hdr: make(http.Header)}
}

// Header 返回翻译层自己的响应头容器。
func (b *bridgeWriter) Header() http.Header { return b.hdr }

// WriteHeader 记录状态码并按 Content-Type 决定流式还是缓冲。
func (b *bridgeWriter) WriteHeader(status int) {
	if b.status != 0 {
		return
	}
	b.status = status
	// 只有 2xx 且上游声明了 SSE 才走流式；错误响应一律缓冲后整体翻译。
	if status < 300 && strings.Contains(strings.ToLower(b.hdr.Get("Content-Type")), "text/event-stream") {
		b.stream = true
		b.startStream()
	}
}

// startStream 初始化流式翻译器并发出首帧事件。
func (b *bridgeWriter) startStream() {
	h := b.dst.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	b.dst.WriteHeader(b.status)
	b.opened = true

	switch b.kind {
	case protoAnthropic:
		b.anth = bridge.NewAnthropicStream(b.model, "")
		b.emit(b.anth.Start())
	case protoResponses:
		b.rsp = bridge.NewResponsesStream(b.model, "")
		b.emit(b.rsp.Start())
	}
	b.startKeepalive()
}

// startKeepalive 起一个后台 goroutine，静默超过 sseKeepaliveIdle 就往客户端
// 补一帧心跳。只在流式已开（opened）后调用。
func (b *bridgeWriter) startKeepalive() {
	stop := make(chan struct{})
	done := make(chan struct{})
	payload := sseComment
	if b.kind == protoAnthropic {
		payload = anthropicPing
	}
	b.mu.Lock()
	b.lastWrite = time.Now()
	b.keepStop, b.keepDone = stop, done
	b.mu.Unlock()

	go func() {
		defer close(done)
		t := time.NewTicker(sseKeepaliveCheck)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				b.mu.Lock()
				if time.Since(b.lastWrite) >= sseKeepaliveIdle {
					if _, err := io.WriteString(b.dst, payload); err == nil {
						b.lastWrite = time.Now()
						if f, ok := b.dst.(http.Flusher); ok {
							f.Flush()
						}
					}
				}
				b.mu.Unlock()
			}
		}
	}()
}

// stopKeepalive 停心跳并**等它退出**——Close 之后 handler 就返回了，ResponseWriter
// 不许再被写（否则是 use-after-free 形态的偶发 panic / 空响应）。
func (b *bridgeWriter) stopKeepalive() {
	stop, done := b.keepStop, b.keepDone
	if stop == nil {
		return
	}
	b.keepStop, b.keepDone = nil, nil
	close(stop)
	// 等待带超时：心跳可能卡在写（客户端不读、TCP 窗口满），无限等会把 Close
	// 也钉死，handler 不返回、连接不释放。超时后放手，goroutine 随写失败退出。
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// Write 累积输出。流式时按帧解析，凑齐一帧就立刻翻译下发。
func (b *bridgeWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.WriteHeader(http.StatusOK)
	}
	if b.stream {
		b.pending.Write(p)
		b.drain()
		return len(p), nil
	}
	return b.body.Write(p)
}

// Flush 实现 http.Flusher（upstream.StreamHint 依赖它做逐帧推送）。
func (b *bridgeWriter) Flush() {
	if b.stream {
		b.drain() // emit 自己持锁；这里不能再套一层（sync.Mutex 不可重入）
		b.mu.Lock()
		if f, ok := b.dst.(http.Flusher); ok {
			f.Flush()
		}
		b.mu.Unlock()
		return
	}
	if b.opened {
		if f, ok := b.dst.(http.Flusher); ok {
			f.Flush()
		}
	}
}

// drain 把 pending 里已完整的帧翻译下发。
func (b *bridgeWriter) drain() {
	for {
		data := b.pending.Bytes()
		idx := bytes.Index(data, []byte("\n\n"))
		if idx < 0 {
			return
		}
		frame := string(data[:idx])
		b.pending.Next(idx + 2)
		b.handleFrame(frame)
	}
}

// handleFrame 处理一帧 chat SSE。
//
// 🔴 一个 SSE 事件块**可以包含多条行**：注释（`: heartbeat` 心跳）、`event:`、`id:`
// 与 `data:` 都可能并存，且 data 行可以有多条（按规范用 \n 拼接）。因此必须遍历块内
// 每一行去收集 data，而不能假设「整块以 data: 开头」。
//
// 这条曾经写错成 `if !strings.HasPrefix(frame, "data:") { return }`，后果很严重：
// 上游在流首必发一条 `: heartbeat` 心跳，它和紧随其后的第一帧同属一个 \n\n 块，
// 于是整块被丢弃——而第一帧恰好是**携带 tool_calls 的 name/id 的那一帧**。
// 实测（2026-10-28，glm-5.3）：/v1/responses 流式 4/4 轮丢失全部工具调用
// （output 为空但 output_tokens=12），非流式完全正常；/v1/chat/completions 因为
// 字节原样透传、不经过本函数，所以看不出问题——这就是它长期没被发现的原因。
//
// 同一处还影响 /v1/messages：首个 content 分片会被静默吃掉（正文少开头几个字）。
func (b *bridgeWriter) handleFrame(frame string) {
	// 收集块内所有 data 行；注释/event/id/空行一律忽略（心跳不该影响数据）。
	var payloads []string
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payloads = append(payloads, strings.TrimPrefix(line, "data:"))
	}
	if len(payloads) == 0 {
		return // 纯心跳/纯注释块
	}
	payload := strings.TrimSpace(strings.Join(payloads, "\n"))
	if payload == "" || payload == "[DONE]" {
		return // 收尾由 Close 统一发（翻译器需要 Finish 才能补完工具调用块）
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		// 单帧畸形不打断整条流：客户端宁愿少一段文字，也不愿收到 500。
		return
	}
	switch b.kind {
	case protoAnthropic:
		if b.anth != nil {
			b.emit(b.anth.Feed(obj))
		}
	case protoResponses:
		if b.rsp != nil {
			b.emit(b.rsp.Feed(obj))
		}
	}
}

// emit 按目标协议的 SSE 线格式写出事件。
func (b *bridgeWriter) emit(events []bridge.Event) {
	if len(events) == 0 {
		return
	}
	// 整批事件在同一把锁里写完：心跳只允许落在事件之间，不允许插进某个
	// event/data 对里（那会让客户端把半帧当完整事件解析）。
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ev := range events {
		raw, err := json.Marshal(ev.Data)
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(b.dst, "event: %s\ndata: %s\n\n", ev.Name, raw); err != nil {
			return
		}
	}
	b.lastWrite = time.Now()
	if f, ok := b.dst.(http.Flusher); ok {
		f.Flush()
	}
}

// Close 收尾。必须在 chatCompletions 返回后调用（延迟调用即可）：
// 流式补发 Finish 事件，非流式把缓冲的整包 JSON 翻译后写出。
func (b *bridgeWriter) Close() {
	b.stopKeepalive()
	if b.status == 0 {
		// 什么都没写（chatCompletions 的防抖分支理论上不会发生）：按空成功处理。
		b.WriteHeader(http.StatusOK)
	}
	if b.stream {
		b.drain()
		// 尾巴里可能还有半帧（上游异常中断）：尽力解析一次，再收尾。
		if rest := strings.TrimSpace(b.pending.String()); rest != "" {
			b.handleFrame(rest)
			b.pending.Reset()
		}
		switch b.kind {
		case protoAnthropic:
			if b.anth != nil {
				b.emit(b.anth.Finish())
			}
		case protoResponses:
			if b.rsp != nil {
				b.emit(b.rsp.Finish())
			}
		}
		if f, ok := b.dst.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	b.closeBuffered()
}

// closeBuffered 翻译缓冲的非流式响应（成功体或错误体）。
func (b *bridgeWriter) closeBuffered() {
	raw := b.body.Bytes()
	var obj map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &obj); err != nil {
			// 上游给的不是 JSON：当作网关内部错误，别把半截内容写给客户端。
			log.Printf("WARN: [server] bridge(%d): non-JSON response body: %v", b.kind, err)
			b.writeError(http.StatusBadGateway, "upstream_parse", "upstream returned a malformed response", "")
			return
		}
	}

	if b.status >= 300 || obj == nil || obj["error"] != nil {
		status := b.status
		if status < 300 {
			status = http.StatusBadGateway // 200 里夹 error 对象：按上游失败处理
		}
		code, msg, hint := openAIErrorParts(obj)
		b.writeError(status, code, msg, hint)
		return
	}

	var out any
	switch b.kind {
	case protoAnthropic:
		out = bridge.ChatToAnthropic(obj, b.model)
	case protoResponses:
		out = bridge.ChatToResponse(obj, b.model)
	default:
		out = obj
	}
	b.writeJSONBytes(b.status, out)
}

// writeJSONBytes 写出目标协议的成功响应体。
func (b *bridgeWriter) writeJSONBytes(status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		b.writeError(http.StatusInternalServerError, "marshal_error", err.Error(), "")
		return
	}
	h := b.dst.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	b.dst.WriteHeader(status)
	b.opened = true
	_, _ = b.dst.Write(raw)
}

// writeError 写出目标协议形态的错误体。
func (b *bridgeWriter) writeError(status int, code, msg, hint string) {
	h := b.dst.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	b.dst.WriteHeader(status)
	b.opened = true

	if b.kind == protoResponses {
		// Responses 的错误体与 chat 同形（{"error":{message,type,code}}），
		// 保留上游 code 与 gateway_hint，排障信息一字不丢。
		errObj := map[string]any{"message": msg, "type": "api_error", "code": code}
		if hint != "" {
			errObj["gateway_hint"] = hint
		}
		raw, _ := json.Marshal(map[string]any{"error": errObj})
		_, _ = b.dst.Write(raw)
		return
	}

	errObj := map[string]any{
		"type":    bridge.AnthropicErrorType(status),
		"message": msg,
	}
	if hint != "" {
		// Anthropic 的错误对象是固定 shape，多出的键被 SDK 忽略；gateway_hint
		// 是本网关的排障线索，宁多给也不藏（与 /v1/chat/completions 同口径）。
		errObj["gateway_hint"] = hint
	}
	raw, _ := json.Marshal(map[string]any{"type": "error", "error": errObj})
	_, _ = b.dst.Write(raw)
}

// openAIErrorParts 从 chat 错误体里拆出 code/message/gateway_hint。
func openAIErrorParts(obj map[string]any) (code, msg, hint string) {
	code, msg = "api_error", "upstream request failed"
	e, _ := obj["error"].(map[string]any)
	if e == nil {
		return
	}
	if v := bridge.Str(e["message"]); v != "" {
		msg = v
	} else if v := bridge.Str(e["msg"]); v != "" {
		msg = v
	}
	if v := bridge.Str(e["code"]); v != "" {
		code = v
	} else if v := bridge.Str(e["type"]); v != "" {
		code = v
	}
	hint = bridge.Str(e["gateway_hint"])
	return
}

// writeAnthropicError 直接向客户端写 Anthropic 错误体（入口校验失败等，
// 还没进 chat 链路时使用）。
func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	raw, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    bridge.AnthropicErrorType(status),
			"message": msg,
		},
	})
	_, _ = w.Write(raw)
}
