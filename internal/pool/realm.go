// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"sort"
	"time"
)

// expiringVirtualSlots 快过期账号在新会话候选集中的虚拟实例权重。
// 3:1 是温和偏好，不是固定比例：账号组成变化会自然改变最终占比。
const expiringVirtualSlots = 3

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// ServableForModelRealm 返回 realm 内此刻**真正能服务该模型**的账号数。
//
// 与 pick 的候选口径一致（healthyForModel 的 6004/11102 模型级冷却豁免、在途租约、
// 积分保底都计入），但**不含 pick 的全冷却兜底**——跨域回退（issue #135）要的正是
// 「这个域此刻有没有健康号」，兜底把冷却号捞回来的乐观语义会掩盖这一点。
//
// 只读：持 RLock 遍历，不改任何状态。realm=="" 时统计全池（与既有 realm 语义一致）。
func (p *Pool) ServableForModelRealm(model, realm string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		if p.floorBlockedForRealmModel(e, model, realm, now) {
			continue
		}
		n++
	}
	return n
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；有效快过期账号出现 expiringVirtualSlots 次。调用方继续按
// 原有序列表哈希，即可让新会话对快过期账号形成温和偏好。重复项按 UID 排序后
// 展开，保证同一账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口，不改变旧方法语义，也不
// 修改配置、状态或 Redis schema。prefer_expiring=false 时退化为逐账号一次。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			if e := p.byUID[uid]; expiringNow(e, now) {
				slots = expiringVirtualSlots
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// StickyEligibleForModelRealm 报告「已绑定会话还能否继续用这个号」，与
// AvailableUIDsForModelRealm 的关键差别是**不看 inFlight（在途名额）**。
//
// 为什么必须拆开（2026-10-24 复盘，线上实测）：
// 会话粘性路由把它拿到 AvailableForModel 列表做**命中校验**，而那份列表排除了
// 满载在途的账号。于是只要粘性号在某一瞬间被并发占满（max_in_flight=8），
// ResolveForModel 就判定「绑定号不可用」→ delete 绑定 → 重新哈希分配。
// 这等于把「临时繁忙」当成「持久失效」，会话被永久搬到另一个账号；而上游前缀
// 缓存是**账号级**的，搬走即全冷。线上 1.6h/1000 条实测：
//   - 同一客户端+模型，相邻请求同号（<5m）→ 缓存命中 94.2%
//   - 相邻请求换号（<5m）→ 命中 34.2%，且 78.6% 的请求几乎全 miss
//   - 换号请求占 70%，吃掉 59% 的 credit 成本
//
// 正确的分工：
//   - 「能不能承接新会话的分配」→ AvailableUIDsForModelRealm（含 inFlight）
//   - 「已绑定的会话还能不能留在这个号上」→ 本方法（只看健康/封禁/冷却/模型限额）
//
// 满载时返回 true，交给 handler 的 PickStickyForModel 判 StickyBusy：本次请求
// 溢出到普通轮换，绑定**保留**（与 handler「StickyBusy 不解绑」的口径对齐）。
func (p *Pool) StickyEligibleForModelRealm(uid, model, realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	if realm != "" && e.a.Realm() != realm {
		return false
	}
	return e.healthyForModel(time.Now(), model)
}
