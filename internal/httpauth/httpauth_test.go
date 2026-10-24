package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(authz string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

func TestVerifyBearer(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		authz string
		want  bool
	}{
		{"空 key 放行（未启用鉴权）", "", "", true},
		{"空 key 也放行任意头", "", "Bearer whatever", true},
		{"正确 key", "sk-abc123", "Bearer sk-abc123", true},
		{"错误 key", "sk-abc123", "Bearer sk-wrong", false},
		{"缺 Authorization 头", "sk-abc123", "", false},
		{"缺 Bearer 前缀", "sk-abc123", "sk-abc123", false},
		{"前缀大小写不符（规范要求精确）", "sk-abc123", "bearer sk-abc123", false},
		{"多余空格", "sk-abc123", "Bearer  sk-abc123", false},
		{"前缀相同但内容短", "sk-abc123", "Bearer sk-abc12", false},
		{"前缀相同但内容长", "sk-abc123", "Bearer sk-abc1234", false},
		{"key 恰好是前缀", "sk-abc", "Bearer sk-abcdef", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(req(c.authz), c.key); got != c.want {
				t.Errorf("VerifyBearer(key=%q, authz=%q) = %v, want %v", c.key, c.authz, got, c.want)
			}
		})
	}
}

// TestVerifyBearerWithoutHeaderStillCompares 缺头路径不应因"提前返回"而暴露形状差异：
// 这里只验证它确实返回 false 且不 panic（常量时间的性质无法用单测断言，靠实现保证）。
func TestVerifyBearerWithoutHeaderStillCompares(t *testing.T) {
	if VerifyBearer(req(""), "any-key") {
		t.Error("missing header must not pass")
	}
}

func TestDigestIsFixedLength(t *testing.T) {
	// 不同长度输入摘要后应等长（这是常量时间比较的前提）
	if len(digest("")) != len(digest("a-much-longer-secret-value")) {
		t.Error("digest length must not depend on input length")
	}
	if len(digest("x")) != 32 {
		t.Errorf("sha256 digest length = %d, want 32", len(digest("x")))
	}
}

// TestVerifyBearerXAPIKey Anthropic 客户端（Claude Code 等）只发 x-api-key，
// 不认 Authorization: Bearer。只支持后者会把它们挡在 401 外面——这是线上实测
// 踩到的硬伤，不是可选的兼容项。
func TestVerifyBearerXAPIKey(t *testing.T) {
	withKey := func(apiKey, authz string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		if apiKey != "" {
			r.Header.Set("x-api-key", apiKey)
		}
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		return r
	}
	cases := []struct {
		name   string
		apiKey string
		authz  string
		want   bool
	}{
		{"仅 x-api-key 正确", "sk-abc123", "", true},
		{"仅 x-api-key 错误", "sk-wrong", "", false},
		{"x-api-key 前后有空格（客户端复制粘贴常见）", "  sk-abc123  ", "", true},
		{"Authorization 正确 + x-api-key 错误：以 Authorization 为准", "sk-wrong", "Bearer sk-abc123", true},
		{"Authorization 错误 + x-api-key 正确：仍失败（显式头优先）", "sk-abc123", "Bearer sk-wrong", false},
		{"Authorization 是别的方案时不回落到 x-api-key", "sk-abc123", "Basic dXNlcjpwYXNz", false},
		{"两个头都没有", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(withKey(c.apiKey, c.authz), "sk-abc123"); got != c.want {
				t.Errorf("VerifyBearer(x-api-key=%q, authz=%q) = %v, want %v", c.apiKey, c.authz, got, c.want)
			}
		})
	}
}
