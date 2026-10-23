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

// TestOverviewMarksEnterpriseSwitchAccounts 概览必须把「企业切换会话」的账号单独列出来：
// 这类号读端点正常但行为事件被上游静默丢弃（成长任务永远 0/x），前端据此在账号表格
// 与任务中心打「企业切换·不计分」标——否则会被误判成判据没适配，反复白查。
//
// 口径要点：只列 token_source=enterprise_switch，真企业版（EnterpriseID 非空）
// 已有「企业版」标记，不重复计入。
func TestOverviewMarksEnterpriseSwitchAccounts(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u-switch", Nickname: "切换号", AccessToken: testJWT(`{"token_source":"enterprise_switch"}`)})
	pl.Add(&auth.Auth{UID: "u-personal", Nickname: "个人号", AccessToken: testJWT(`{"token_source":"oneid_login"}`)})
	pl.Add(&auth.Auth{UID: "u-real-ent", Nickname: "真企业", EnterpriseID: "e1", AccessToken: testJWT(`{"token_source":"oneid_login"}`)})

	p := New(Config{Version: "test", Pool: pl})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		EnterpriseSwitch []string `json:"enterprise_switch"`
		Count            int      `json:"enterprise_switch_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if resp.Count != 1 || len(resp.EnterpriseSwitch) != 1 || resp.EnterpriseSwitch[0] != "u-switch" {
		t.Fatalf("enterprise_switch = %v (count %d), want 仅 u-switch", resp.EnterpriseSwitch, resp.Count)
	}
}
