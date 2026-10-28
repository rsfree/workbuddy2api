// usage_alias.go 桥接层的 usage 缓存字段解析。
//
// 为什么需要这一层：上游在 usage 里用 5 个别名同时报同一个"缓存命中"数值
// （prompt_tokens_details.cached_tokens / prompt_cache_hit_tokens /
// cache_read_input_tokens / cached_tokens / input_tokens_details.cached_tokens），
// 且兼容别名常被写成 0。两个目标协议只认各自的字段名：
//
//	Anthropic → cache_read_input_tokens / cache_creation_input_tokens
//	Responses → input_tokens_details.cached_tokens
//
// 之前两个目标协议各写了一部分，且**流式路径整段漏掉**（实测 kimi-k3.1-pro：
// 网关自身账本记到 70.7% 命中率，客户端拿到的 usage 里却一个缓存字段都没有）。
// 统一走 upstream.UsageCacheHitTokens 这个唯一口径，避免第三份解析逻辑漂移。
package bridge

import "github.com/linguo2625469/workbuddy2api-panel/internal/upstream"

// usageCacheRead 返回缓存命中 token 数；无观测或为 0 时 ok=false。
//
// 为 0 时不输出字段（而不是输出 0）：Anthropic 的 cache_read_input_tokens 是
// "可选、只在命中时出现"，硬塞 0 会让客户端把"本次没命中"和"该协议不支持缓存"
// 混为一谈。
func usageCacheRead(usage map[string]any) (int, bool) {
	if usage == nil {
		return 0, false
	}
	hit, ok := upstream.UsageCacheHitTokens(usage)
	if !ok || hit <= 0 {
		return 0, false
	}
	return int(hit), true
}

// usageCacheWrite 返回写入缓存的 token 数（Anthropic 的
// cache_creation_input_tokens，按 1.25x 计价）。上游两个字段都出现过，取先非零者。
func usageCacheWrite(usage map[string]any) int {
	for _, key := range []string{"cache_creation_input_tokens", "prompt_cache_write_tokens"} {
		if n := IntOf(usage[key]); n > 0 {
			return n
		}
	}
	return 0
}
