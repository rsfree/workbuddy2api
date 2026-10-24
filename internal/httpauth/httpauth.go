// Package httpauth 网关与面板共用的 Bearer 鉴权原语。
//
// 单独成包的原因：server（/v1/*、/status）与 panel（/panel/api/*）两处鉴权
// 必须完全同口径——此前各自复制了一份"字符串直接比较"的实现，既容易漂移，
// 又都带计时侧信道。统一到这里后，口径只有一份，且天然常量时间比较。
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix 认证方案前缀（大小写敏感，与 HTTP 规范及既有实现一致）。
const bearerPrefix = "Bearer "

// VerifyBearer 校验请求头是否携带正确的密钥。
//
// key 为空表示"未启用鉴权"，恒返回 true（调用方据此放行）。
// 比较用 SHA-256 摘要 + subtle.ConstantTimeCompare：
//   - 常量时间，不因前缀匹配长度而泄露信息；
//   - 先摘要再比较，长度差异被吸收进摘要（不会因长度不同提前返回）；
//   - 摘要本身不可逆，即便有侧信道也拿不到密钥原文。
//
// 密钥取自两处（见 credential）：`Authorization: Bearer <key>`，或 Anthropic
// 客户端惯用的 `x-api-key: <key>`。后者不是可有可无的兼容项——Claude Code 默认
// 只发 x-api-key，不认 Bearer，只支持前者等于把 /v1/messages 前门的核心用户挡在
// 401 外面（2026-10-24 线上实测踩到）。
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	tok, ok := credential(r)
	if !ok {
		// 缺头/方案不对：仍走一次摘要比较，保持耗时形状一致。
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// credential 按优先级取请求携带的密钥。
//
// 顺序有意为之：Authorization: Bearer 优先，其次 x-api-key。Authorization 存在
// 但方案不是 Bearer 时**不回落到** x-api-key——两个头同时出现且互相矛盾时，
// 以显式的 Authorization 为准，避免"上层代理塞了个 Basic 头、下层用 x-api-key
// 意外绕过"这类歧义。
func credential(r *http.Request) (string, bool) {
	authz := r.Header.Get("Authorization")
	if strings.HasPrefix(authz, bearerPrefix) {
		return authz[len(bearerPrefix):], true
	}
	if authz != "" {
		// 明确带了别的鉴权方案：不再看 x-api-key。
		return "", false
	}
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v, true
	}
	return "", false
}

// digest 返回 s 的 SHA-256（定长 32 字节，供常量时间比较）。
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
