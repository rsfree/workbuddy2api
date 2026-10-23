package panel

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// testJWT 拼一个最小 JWT：只解 payload、不验签，签名段任意占位即可。
func testJWT(payload string) string {
	return "hdr." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// TestOverviewMarksEnterpriseSwitchAccounts 概览要区分两类企业相关凭证：
//
//	enterprise_switch  = token_source=enterprise_switch 但未绑定租户（enterprise_id 空）
//	                     → 仅中性标注，实测仍计分，**不拦截**；
//	enterprise_session = 真正绑定租户的企业会话 → 行为事件不计分，标红拦截。
//
// 2026-10-23 线上误判即源于把前者当成后者：22 个号全被 422 拦死，实测两条主判据
// （web 点击 / 对话）都正常入账。
func TestOverviewMarksEnterpriseSwitchAccounts(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u-switch", Nickname: "切换未绑定", AccessToken: testJWT(`{"token_source":"enterprise_switch"}`)})
	pl.Add(&auth.Auth{UID: "u-bound", Nickname: "企业会话", AccessToken: testJWT(`{"token_source":"enterprise_switch","enterprise_id":"e9"}`)})
	pl.Add(&auth.Auth{UID: "u-personal", Nickname: "个人号", AccessToken: testJWT(`{"token_source":"oneid_login"}`)})
	pl.Add(&auth.Auth{UID: "u-real-ent", Nickname: "真企业", EnterpriseID: "e1", AccessToken: testJWT(`{"token_source":"oneid_login"}`)})

	p := New(Config{Version: "test", Pool: pl})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		EnterpriseSwitch  []string `json:"enterprise_switch"`
		SwitchCount       int      `json:"enterprise_switch_count"`
		EnterpriseSession []string `json:"enterprise_session"`
		SessionCount      int      `json:"enterprise_session_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if resp.SwitchCount != 1 || len(resp.EnterpriseSwitch) != 1 || resp.EnterpriseSwitch[0] != "u-switch" {
		t.Fatalf("enterprise_switch = %v (count %d), want 仅 u-switch", resp.EnterpriseSwitch, resp.SwitchCount)
	}
	if resp.SessionCount != 1 || len(resp.EnterpriseSession) != 1 || resp.EnterpriseSession[0] != "u-bound" {
		t.Fatalf("enterprise_session = %v (count %d), want 仅 u-bound（真企业版不计入）",
			resp.EnterpriseSession, resp.SessionCount)
	}
}
