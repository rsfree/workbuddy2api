package main

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
//
// realm 为空串时 pool.WeightedAvailableUIDsForModelRealm 退化为现状
// （AvailableUIDsForModel），老调用（无前缀模型名）语义零改动。
//
// 返回列表可能对快过期账号重复同一 UID，作为虚拟实例权重；会话哈希分配无需感知
// 权重细节，已有绑定的快路径仍直接返回原账号，不做迁移。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.WeightedAvailableUIDsForModelRealm(bare, realm)
	}
}

// realmAwareStickyUsable 构造「已绑定会话还能否继续用这个号」的 realm 感知闭包。
//
// 与 realmAwareAvailableForModel 的唯一差别：**不看 inFlight 在途名额**。
// 前者回答"能不能分配一个新会话给它"（满载就不能，否则选完立刻抢不到名额），
// 本闭包回答"已绑定的会话还能不能留在这"（满载只是临时压力，把会话搬走会让
// 账号级上游前缀缓存全冷）。两者的分工见 session.Config.StickyUsable 的注释。
func realmAwareStickyUsable(p *pool.Pool) func(uid, model string) bool {
	return func(uid, model string) bool {
		realm, bare := server.ResolveModel(model)
		return p.StickyEligibleForModelRealm(uid, bare, realm)
	}
}
