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
//   - kimi-k3.1-pro-highspeed（官方高速版）故意不收：WorkBuddy 上游还没有对应
//     模型条目（实测 11102），别过去只会得到"假的高速版"（普通速度），误导性
//     强于直接 400。等上游真上了 highspeed 条目再映射；
//   - 响应回显与请求日志用规范名（kimi-k3-1），客户端若以别名计费对账需注意。
package upstream

// modelAliases 客户端写法 → 网关规范名。对裸名与带 cn:/global: 前缀的名字都生效
// （在 resolveModelDetail 剥前缀之后套用），两域同口径——目标域若无该模型，
// 上游自然 11102，不在此预判。
var modelAliases = map[string]string{
	// 官方 API 点号写法 kimi-k3.1 的抄名入口；WorkBuddy 目录里实际存在的是 k3-1
	//（上游 /v3/config 实测：id=kimi-k3-1、name="Kimi-K3"，即官宣的 K3.1 系列）。
	"kimi-k3.1": "kimi-k3-1",
	// 官方 API 完整名（官宣原文"模型选择 kimi-k3.1-pro"）。坐实依据：K3.1 系列
	// 目前只有 Pro 一个型号（无非 Pro 的 K3.1），WorkBuddy 的 k3-1 即官宣的
	// K3.1 Pro（2026-10-24 官宣，k3-1 在官宣前已进目录 = 合作伙伴早期接入）。
	"kimi-k3.1-pro": "kimi-k3-1",
}

// CanonicalModel 把客户端传来的（已剥 realm 前缀的）模型名归一到网关规范名；
// 未命中别名表原样返回。命中侧效应：调用方应记一条 WARN（可观测谁在依赖别名）。
func CanonicalModel(model string) string {
	if to, ok := modelAliases[model]; ok {
		return to
	}
	return model
}
