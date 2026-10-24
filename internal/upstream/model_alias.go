// model_alias.go 模型名别名表（产品命名兼容层）。
//
// 背景：月之暗面官方 API 平台（platform.kimi.com）用点号命名（kimi-k3.1-pro），
// CodeBuddy/WorkBuddy 体系用连字符（kimi-k3-1）。跨平台照抄模型名的客户端会得到
// 上游 11102 "service info not found"（实测 400 model_unavailable、耗时 ~4s、
// 白占一次选号）。别名在**选号/牌价/冷却/隐藏名单/粘性之前**归一到规范名，
// 全链路（选号键、6004 模型级冷却键、模型成本观测、出站 body 重写）自动一致。
//
// 收录原则：只收「确定同一模型」的映射，不做猜测性归一（宁 400 不指错模型）。
// 加条目前须用真凭证探测上游确认目标名存在（/v3/config 或一次真实对话）。
//
// 已知边界：
//   - kimi-k3.1-pro 是**上游独立模型**，不是 kimi-k3-1 的别名。2026-10-24 直连上游
//     实测：`kimi-k3.1-pro` 200 且 echo 原名；两者参数与牌价均不同——
//     k3.1-pro = 3T 级旗舰、x0.97、档位 [low,high,max]、输出上限 32000；
//     k3-1（显示名 Kimi-K3）= x1.62、档位仅 medium、输出上限 1048576。
//     本条曾于 02:20 被误收为 k3-1 的别名，造成「客户端点名 0.97x 的模型，
//     实际按 1.62x 计费且推理档位被压平」。教训：**凡上游真实存在的 id
//     一律不加别名**，别名的唯一用途是翻译上游不存在的写法；
//   - kimi-k3.1-pro-highspeed 故意不收：上游条目**存在**，但按账号类型拒绝
//     （实测 11158 "model [kimi-k3.1-pro-highspeed] is not allowed for user
//     type [personal_free_user]"）。映射到 k3.1-pro 会得到"假高速版"，
//     映射到 k3-1 更是错模型——两种都比直接报错更坏；
//   - 响应回显与请求日志用规范名，客户端以别名计费对账需注意。
package upstream

// modelAliases 客户端写法 → 网关规范名。对裸名与带 cn:/global: 前缀的名字都生效
// （在 resolveModelDetail 剥前缀之后套用），两域同口径——目标域若无该模型，
// 上游自然 11102，不在此预判。
var modelAliases = map[string]string{
	// 官方 API 点号写法 kimi-k3.1 的抄名入口。上游没有 `kimi-k3.1` 这个 id
	//（直连实测 11102），而 K3.1 系列只有 Pro 一个型号（官宣原文「模型选择
	// kimi-k3.1-pro」）⇒ 投向 kimi-k3.1-pro。**不能投 k3-1**：那是上一代
	// Kimi-K3，属另一个模型（2026-10-24 更正的误映射）。
	"kimi-k3.1": "kimi-k3.1-pro",
}

// CanonicalModel 把客户端传来的（已剥 realm 前缀的）模型名归一到网关规范名；
// 未命中别名表原样返回。命中侧效应：调用方应记一条 WARN（可观测谁在依赖别名）。
func CanonicalModel(model string) string {
	if to, ok := modelAliases[model]; ok {
		return to
	}
	return model
}
