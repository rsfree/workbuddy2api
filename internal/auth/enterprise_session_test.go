package auth

import (
	"encoding/base64"
	"testing"
)

// mustJWT 用给定 payload 拼一个最小 JWT（header.payload.signature）。
// IsEnterpriseSession 不校验签名，只解 payload，故签名段可为任意占位。
func mustJWT(t *testing.T, payload string) string {
	t.Helper()
	return "hdr." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// TestIsEnterpriseSession 企业上下文判定（2026-10-23 收紧）：
//   - 必须**绑定租户**（JWT enterprise_id 非空）才算企业会话；
//   - 只有 token_source=enterprise_switch、enterprise_id 为空 → 否。
//     实测这类号仍正常计分（web 点击 0/1→1/1、对话 0/5→1/5），早前按
//     token_source 单独拦截属于误判。
//   - 无声明 / 非 JWT / 空 token 一律否（老凭证零回归）；enterpriseId 非空短路为真。
func TestIsEnterpriseSession(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{"enterprise_switch 未绑定租户", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch"}`), false},
		{"enterprise_switch 绑定租户", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch","enterprise_id":"e9"}`), true},
		{"enterprise_id 空串不算绑定", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch","enterprise_id":""}`), false},
		{"oneid_login 带租户也不算", mustJWT(t, `{"sub":"u1","token_source":"oneid_login","enterprise_id":"e9"}`), false},
		{"无 token_source 声明", mustJWT(t, `{"sub":"u1","scope":"openid profile"}`), false},
		{"其他 token_source", mustJWT(t, `{"sub":"u1","token_source":"password"}`), false},
		{"带填充 base64url（未绑定）", "hdr." + base64.URLEncoding.EncodeToString([]byte(`{"token_source":"enterprise_switch"}`)) + ".sig", false},
		{"payload 非 JSON", "hdr." + base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".sig", false},
		{"非 JWT 形态", "opaque-token", false},
		{"空 token", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", AccessToken: c.token}
			if got := a.IsEnterpriseSession(); got != c.want {
				t.Errorf("IsEnterpriseSession()=%v want %v", got, c.want)
			}
		})
	}

	t.Run("enterpriseId 非空短路为真", func(t *testing.T) {
		a := &Auth{UID: "u1", EnterpriseID: "e1", AccessToken: mustJWT(t, `{"sub":"u1"}`)}
		if !a.IsEnterpriseSession() {
			t.Error("IsEnterprise()=true 时 IsEnterpriseSession() 应恒为 true")
		}
	})

	t.Run("EnterpriseSwitchOnly 只认未绑定租户的切换标记", func(t *testing.T) {
		only := &Auth{UID: "u1", AccessToken: mustJWT(t, `{"token_source":"enterprise_switch"}`)}
		if !only.EnterpriseSwitchOnly() {
			t.Error("token_source=enterprise_switch 且 enterprise_id 空 → EnterpriseSwitchOnly 应为 true")
		}
		bound := &Auth{UID: "u1", AccessToken: mustJWT(t, `{"token_source":"enterprise_switch","enterprise_id":"e9"}`)}
		if bound.EnterpriseSwitchOnly() {
			t.Error("已绑定租户不应算 SwitchOnly")
		}
		personal := &Auth{UID: "u1", AccessToken: mustJWT(t, `{"token_source":"oneid_login"}`)}
		if personal.EnterpriseSwitchOnly() {
			t.Error("oneid_login 不应算 SwitchOnly")
		}
	})

	t.Run("企业切换会话不影响 IsEnterprise（计费口径隔离）", func(t *testing.T) {
		a := &Auth{UID: "u1", AccessToken: mustJWT(t, `{"token_source":"enterprise_switch"}`)}
		if a.IsEnterprise() {
			t.Error("仅 token_source=enterprise_switch 不应翻转 IsEnterprise（额度查询仍走个人口径）")
		}
	})
}
