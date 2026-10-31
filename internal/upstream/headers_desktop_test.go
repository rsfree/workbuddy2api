package upstream

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 2026-11-01 取证（app.asar 反解 buildVlmGatewayHeaders / applyGatewayHeaders）定下的
// chat 出站头口径，本组用例把它钉住，防止以后又被"补指纹"改回去。
//
// 真客户端 chat 主路径的实际头集合里：
//   - 没有 X-Machine-ID（只有 feedback 端点发 X-Machine-Id，小写 d）
//   - 完全没有 X-Session-ID（181MB asar 里一次都不存在）
//   - 有 X-Private-Data / X-Agent-Intent / acp-connection-id / traceparent / b3
func newChatReq(t *testing.T, c *Client, a *auth.Auth) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://example.invalid/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	c.ChatHeaders(req, a, "", ChatMeta{ConversationRequestID: "0123456789abcdef0123456789abcdef"})
	return req
}

// TestChatHeadersNoSelfInventedDeviceHeaders 默认不发自创设备头。
func TestChatHeadersNoSelfInventedDeviceHeaders(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u-1"}
	req := newChatReq(t, c, a)

	if v := req.Header.Get("X-Machine-ID"); v != "" {
		t.Errorf("X-Machine-ID = %q，真客户端 chat 不发这个头，默认必须为空", v)
	}
	if v := req.Header.Get("X-Session-ID"); v != "" {
		t.Errorf("X-Session-ID = %q，真客户端根本没有这个头，默认必须为空", v)
	}
}

// TestChatHeadersLegacyDeviceHeadersOptIn 开关打开时恢复旧行为（A/B 回溯用）。
func TestChatHeadersLegacyDeviceHeadersOptIn(t *testing.T) {
	c := &Client{LegacyDeviceHeaders: true}
	a := &auth.Auth{UID: "u-1"}
	req := newChatReq(t, c, a)

	if v := req.Header.Get("X-Machine-ID"); v == "" {
		t.Error("LegacyDeviceHeaders=true 时 X-Machine-ID 应仍有值")
	}
	if v := req.Header.Get("X-Session-ID"); v == "" {
		t.Error("LegacyDeviceHeaders=true 时 X-Session-ID 应仍有值")
	}
}

// TestChatHeadersDesktopIdentity 真客户端的固定身份头必须补齐。
func TestChatHeadersDesktopIdentity(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u-1"}
	req := newChatReq(t, c, a)

	if v := req.Header.Get("X-Private-Data"); v != "true" {
		t.Errorf("X-Private-Data = %q want true", v)
	}
	if v := req.Header.Get("X-Agent-Intent"); v != "craft" {
		t.Errorf("X-Agent-Intent = %q want craft", v)
	}
	acp := req.Header.Get("acp-connection-id")
	if acp == "" {
		t.Fatal("acp-connection-id 缺失")
	}
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidRe.MatchString(acp) {
		t.Errorf("acp-connection-id = %q 不是 UUIDv4 形态", acp)
	}
}

// TestChatHeadersTraceBothFormats 真客户端 W3C traceparent 与 B3 单头同时发。
func TestChatHeadersTraceBothFormats(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u-1"}
	req := newChatReq(t, c, a)

	tp := req.Header.Get("traceparent")
	tpRe := regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
	if !tpRe.MatchString(tp) {
		t.Errorf("traceparent = %q 不符合 W3C 形状 00-<32hex>-<16hex>-01", tp)
	}
	b3 := req.Header.Get("b3")
	b3Re := regexp.MustCompile(`^[0-9a-f]{16,32}-[0-9a-f]{16}-1-[0-9a-f]{16}$`)
	if !b3Re.MatchString(b3) {
		t.Errorf("b3 = %q 不符合 zipkin 单头形状", b3)
	}
}

// TestChatHeadersAttributionStillPresent 既有归因头不能被这次改动误伤。
func TestChatHeadersAttributionStillPresent(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u-1"}
	req := newChatReq(t, c, a)

	for _, k := range []string{"X-IDE-Type", "X-IDE-Name", "X-IDE-Version", "X-Product", "X-Agent-Purpose"} {
		if req.Header.Get(k) == "" {
			t.Errorf("%s 缺失（既有归因头被误伤）", k)
		}
	}
}

// TestDeriveAccountUUIDShapeAndStability 派生 UUID：形态合法、幂等、账号间互异。
func TestDeriveAccountUUIDShapeAndStability(t *testing.T) {
	a := deriveAccountUUID("u-1", "acp")
	if a != deriveAccountUUID("u-1", "acp") {
		t.Error("同 uid 同用途必须幂等")
	}
	if a == deriveAccountUUID("u-2", "acp") {
		t.Error("不同 uid 必须互异")
	}
	if len(a) != 36 {
		t.Errorf("len=%d want 36", len(a))
	}
}
