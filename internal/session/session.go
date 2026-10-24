// Package session 会话粘性路由：同一会话（conversationId / metadata 键）尽量绑定同一账号。
//
// 设计参考 antigravityProxyGo internal/session（fast-path RLock / 双段分配 / TTL / 持久化），
// 但改为纯内存 + redisstore 异步镜像：
//   - 命中走 RLock 快查（绝大多数请求已绑定）；
//   - 未命中/失效走写锁 re-check 后分配，避免同 key 并发重复分配（TOCTOU 防护）；
//   - 分配优先"空闲账号"（未绑定任何会话的可用号）哈希，其次全池哈希（双段策略）；
//   - LastActive 滚动续期，TTL 过期由后台 GC 或快路径惰性过期清理；
//   - 每次绑定变更 fire-and-forget 镜像到 redisstore（防重启丢粘性）。
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
)

// entry 单条会话绑定。
type entry struct {
	uid        string
	lastActive time.Time
}

// Config 路由依赖；Available 返回"可用账号"（healthy 且未占满在途）的有序 uid 列表，
// 由 pool.AvailableUIDs 提供。Store 可为 redisstore.Noop（纯内存）。
type Config struct {
	TTL        time.Duration
	GCInterval time.Duration
	Store      redisstore.Store
	Available  func() []string
	// AvailableForModel 按请求模型返回"在该模型上可用"的账号（healthy 且未占满在途，
	// 且未被该模型限流/限额）。nil 时回落 Available（无模型维度，行为与引入前一致）。
	//
	// 为什么粘性需要模型维度：绑定只记 uid，而同一个会话可能换模型。账号被 6004
	// 模型级限额后对**其他模型**仍可用（issue #31 豁免），此时若只按账号级可用性
	// 校验，会话会被钉在这个号上反复失败——正是"限额后换不动号"的观感来源。
	//
	// 🔴 它只用于**分配新会话**（必须能立刻承接），不要拿来做已绑定会话的命中校验：
	// 该列表排除了满载在途的账号，拿它校验会把「临时繁忙」误判成「持久失效」，
	// 见 StickyUsable。
	AvailableForModel func(model string) []string

	// StickyUsable 判断**已绑定**的账号在该模型上还能不能继续用：只看健康/封禁/
	// 冷却/模型级限额，**不看 inFlight 在途名额**。
	//
	// 这是 2026-10-24 修掉的一处成本大坑：此前命中校验复用 AvailableForModel，
	// 粘性号一旦被并发占满就会被判失效 → 解绑并重新哈希 → 会话被搬到另一个号，
	// 而上游前缀缓存是账号级的，搬走即全冷。线上实测「同号相邻请求命中 94.2%、
	// 换号相邻请求命中 34.2%」，换号请求占 70%、吃掉 59% 成本。
	//
	// 满载时返回 true → 调用方拿到该 uid 后由 handler 判 StickyBusy：本次请求
	// 溢出到普通轮换，绑定保留，压力一过立刻回热。
	//
	// nil 时回落 AvailableForModel（旧语义：满载也算不可用），保持向后兼容。
	StickyUsable func(uid, model string) bool

	// PrefixAffinity 开启**前缀亲和**：把「system + tools 相同」的多个会话尽量
	// 引到同一个账号，让那段共同前缀的缓存只在一个号上冷启动一次。
	//
	// 与会话粘性的分工（粒度不同、强/弱关系）：
	//   - 粘性（entries）：同一会话多轮 → 同号。命中时亲和根本不参与。
	//   - 亲和（aff）：不同会话共享前缀 → 同号。只在「会话尚未绑定」时当提示用。
	//
	// 为什么另一份收益：客户端并行跑多个会话（或没有 conversation_id 而落到派生键）
	// 时，每个会话都是独立键，会各自哈希到不同账号，同一段几万 token 的 system
	// 提示词因此被重复冷启动 N 次。线上实测换号请求的命中率是 34.2% 而非 0，
	// 说明各账号里本来就散落着同前缀的碎片缓存。
	PrefixAffinity bool

	// AffinityTTL 亲和记录存活时长（0 = 默认 20m）。故意比会话 TTL 短：
	// 见 internal/session/affinity.go 的 defaultAffinityTTL 注释。
	AffinityTTL time.Duration

	// AffinityMax 亲和注册表容量上限（0 = 默认 4096）。
	AffinityMax int
}

// Router 会话粘性路由器。
type Router struct {
	mu      sync.RWMutex
	entries map[string]entry
	// aff 前缀亲和注册表：前缀指纹 → 该前缀缓存目前热在哪个账号（见 affinity.go）。
	aff  map[string]affEntry
	cfg  Config
	stop chan struct{}
	// dirty 自上次成功落盘以来是否有变更（见 persist.go）。只在 dirty 时写盘，
	// 无流量时完全不产生 IO。
	dirty bool
	// statePath / stateStop 本地快照落盘的目标与后台 goroutine 的停止信号。
	// 用**独立的** stop channel 而不是复用 stop：GC 与落盘的启停时机不同
	// （落盘关停时要先 flush 再退），共用一个 channel 会让 close 语义互相干扰。
	statePath string
	stateStop chan struct{}
}

// New 构建路由器。若 cfg.Store 为 nil 则用 Noop（纯内存）；cfg.Available 为 nil 视为空池。
// TTL/GCInterval 非正取默认（30m / 5m）——main 从 config 解析后传入，这里兜底。
func New(cfg Config) *Router {
	if cfg.Store == nil {
		cfg.Store = redisstore.Noop{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	return &Router{entries: map[string]entry{}, aff: map[string]affEntry{}, cfg: cfg}
}

// StartGC 启动后台 GC goroutine（幂等）。进程退出时调 StopGC。
//
// stop channel 必须在启 goroutine 前捕获到**局部变量**：goroutine 在 select 里
// 每轮重新求值 r.stop 是无锁读，而 StopGC 持写锁把它置 nil——既是数据竞争
// （-race 可复现），又会在读到 nil 后让该 case 永久阻塞（nil channel 永不就绪），
// 于是关停彻底失效：goroutine 再也不会退出，ticker 无限触发 gcOnce（goroutine
// 泄漏 + 关停后仍持续 GC）。捕获局部变量后，close(stop) 与 select 观测的是同一个
// channel，StopGC 一定能让 goroutine 退出。
func (r *Router) StartGC() {
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	r.stop = stop
	r.mu.Unlock()

	go func() {
		t := time.NewTicker(r.cfg.GCInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.gcOnce(time.Now())
			}
		}
	}()
}

// StopGC 停止后台 GC（幂等）。
func (r *Router) StopGC() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
}

// LoadFromStore 启动时从 redisstore 恢复绑定（内存覆盖本地，读操作仅此处发生）。
// 已有本地绑定被保留——Redis 仅为恢复备份，本地一旦建立即为权威。
func (r *Router) LoadFromStore() {
	binds := r.cfg.Store.LoadBinds()
	if len(binds) == 0 {
		return
	}
	now := time.Now()
	r.mu.Lock()
	loaded := 0
	for key, uid := range binds {
		if _, exists := r.entries[key]; exists {
			continue
		}
		r.entries[key] = entry{uid: uid, lastActive: now}
		loaded++
	}
	r.mu.Unlock()
	if loaded > 0 {
		log.Printf("[session] 从 Redis 恢复 %d 条粘性会话绑定", loaded)
	}
}

// Resolve 返回会话 key 应绑定的账号 uid，ok=false 表示当前无可用账号。
// 无模型维度（等价于 ResolveForModel(key, "")），保留给不关心模型的调用方。
func (r *Router) Resolve(key string) (string, bool) {
	return r.ResolveForModel(key, "")
}

// LookupForModel 只复用已有有效绑定，不通过哈希为新会话分配账号。
// 有序账号池策略用它保留粘性，未绑定会话由 Pool 选号并在成功后 Bind。
func (r *Router) LookupForModel(key, model string) (string, bool) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	e, found := r.entries[key]
	if !found || expired(e, now, r.cfg.TTL) || !r.stickyOK(e.uid, model) {
		return "", false
	}
	r.entries[key] = entry{uid: e.uid, lastActive: now}
	r.dirty = true // 续期也是变更：重启后要按新时间判过期
	r.cfg.Store.SetBind(key, e.uid, r.cfg.TTL)
	return e.uid, true
}

// Peek 只读返回当前绑定（不续期、不分配、不解绑）。
//
// 供调用方做「换号观测」：handler 需要在 ResolveForModel **之前**拿到旧绑定，
// 才能在事后区分「命中同号（hit）」与「绑定号失效被换掉（miss）」——只看
// ResolveForModel 的返回值永远无法分辨这两者，因为两者都会返回一个 uid。
// 没有它，面板的换号率会把真实换号误记成同号命中，指标就失去意义。
func (r *Router) Peek(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[key]
	if !ok || expired(e, time.Now(), r.cfg.TTL) {
		return "", false
	}
	return e.uid, true
}

// ResolveForModel 返回会话 key 在该模型上应绑定的账号 uid。
// 命中且账号在该模型可用 → 滚动 lastActive 并直接返回；否则（绑定号已冷却/占满/
// 被该模型限流）走重新分配。
//
// 为什么必须带模型：绑定只记 uid，同一个会话可能换模型；账号被 6004 模型级限额后
// 对其他模型仍可用（见 pool.healthyForModel 的 softRateModel 豁免）。若只按账号级
// 可用性校验，会话会被钉在一个"对当前模型不可用"的号上反复失败。
func (r *Router) ResolveForModel(key, model string) (string, bool) {
	now := time.Now()

	// ── Fast path: RLock 快查 ──────────────────────────────
	r.mu.RLock()
	e, found := r.entries[key]
	r.mu.RUnlock()
	if found && !expired(e, now, r.cfg.TTL) {
		if r.stickyOK(e.uid, model) {
			r.touch(key, e.uid, now)
			return e.uid, true
		}
		// 绑定号已冷却/封禁/该模型限额 → 持久失效，落入慢路径重分配。
		// 注意：在途满载**不**走这里（stickyOK 不看 inFlight），满载由 handler
		// 判 StickyBusy 后本次溢出、绑定保留——见 StickyUsable 注释。
	}

	// ── Slow path: 写锁 re-check 后分配 ────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()

	// re-check：并发同 key 可能已被其他 goroutine 分配好。
	if e2, found2 := r.entries[key]; found2 && !expired(e2, now, r.cfg.TTL) {
		if r.stickyOK(e2.uid, model) {
			r.entries[key] = entry{uid: e2.uid, lastActive: now}
			return e2.uid, true
		}
		delete(r.entries, key) // 持久失效：清掉再分配
	}

	uids := r.availableSlice(model)
	if len(uids) == 0 {
		return "", false
	}

	// 双段策略：优先"空闲账号"（未被任何会话绑定的可用号），其次全池。
	bound := map[string]bool{}
	for _, v := range r.entries {
		bound[v.uid] = true
	}
	var idle []string
	for _, u := range uids {
		if !bound[u] {
			idle = append(idle, u)
		}
	}
	pool2 := idle
	if len(pool2) == 0 {
		pool2 = uids
	}
	uid := pool2[hashIndex(key, len(pool2))]

	prev, existed := r.entries[key]
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.dirty = true
	if existed && prev.uid != uid {
		r.cfg.Store.DelBind(key)
	}
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
	return uid, true
}

// touch 滚动 lastActive 并异步镜像（只在快路径命中时写最后一次）。
func (r *Router) touch(key, uid string, now time.Time) {
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.dirty = true
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Bind 显式把会话 key 绑定到 uid（幂等覆盖旧值），并异步镜像到 redisstore。
// 供"粘性跟随最终成功号"用：请求成功返回前，把会话重绑到实际成功的账号，让多轮对话下一跳稳定
// 收敛到"对该会话持续成功的号"（对齐 antigravity 语义）。空 key 直接返回（无会话则不绑）。
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.dirty = true
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Unbind 解除会话绑定（请求失败时调用，让该会话下次重新分配）。返回是否存在。
func (r *Router) Unbind(key string) bool {
	r.mu.Lock()
	_, found := r.entries[key]
	if found {
		delete(r.entries, key)
		r.dirty = true
	}
	r.mu.Unlock()
	if found {
		r.cfg.Store.DelBind(key)
	}
	return found
}

// Count 返回当前绑定数（供 /status 观测）。
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// gcOnce 清理 TTL 过期的绑定，并镜像删除。
func (r *Router) gcOnce(now time.Time) int {
	r.mu.Lock()
	var expiredKeys []string
	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.cfg.TTL {
			expiredKeys = append(expiredKeys, key)
		}
	}
	for _, key := range expiredKeys {
		delete(r.entries, key)
	}
	// 前缀亲和同样按 TTL 清理：过期不清会让前缀长期押在一个已不再承接它的账号上。
	for k, e := range r.aff {
		if now.Sub(e.lastSeen) > r.affinityTTL() {
			delete(r.aff, k)
			r.dirty = true
		}
	}
	if len(expiredKeys) > 0 {
		r.dirty = true
	}
	r.mu.Unlock()
	for _, key := range expiredKeys {
		r.cfg.Store.DelBind(key)
	}
	return len(expiredKeys)
}

// stickyOK 判断已绑定账号能否继续承接当前模型：优先用注入的 StickyUsable
// （健康口径，不看 inFlight），未注入时回落 availableSet（旧口径，含 inFlight）。
//
// 回落分支只服务不注入的调用方（测试/老装配）；生产由 cmd/server/wiring.go 注入。
func (r *Router) stickyOK(uid, model string) bool {
	if uid == "" {
		return false
	}
	if r.cfg.StickyUsable != nil {
		return r.cfg.StickyUsable(uid, model)
	}
	return r.availableSet(model)[uid]
}

// availableSet 把 AvailableForModel(model) 的有序列表转集合（快路径命中校验用）。
func (r *Router) availableSet(model string) map[string]bool {
	uids := r.availableSlice(model)
	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	return set
}

// availableSlice 安全调用 AvailableForModel；未注入时回落 Available（nil 视空池）。
func (r *Router) availableSlice(model string) []string {
	if r.cfg.AvailableForModel != nil {
		return r.cfg.AvailableForModel(model)
	}
	if r.cfg.Available == nil {
		return nil
	}
	return r.cfg.Available()
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// hashIndex FNV-1a 哈希取模（antigravity 双段分配的稳定散列）。
func hashIndex(key string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

// ExtractKey 从请求体提取会话键；按下列顺序依次尝试，找不到时回退到
// 内容派生的稳定键（见 deriveKey），仍为空则返回空串（绝不失败）。
//  1. metadata.conversation_id
//  2. metadata.conversationId
//  3. conversation_id
//  4. conversationId
//  5. prompt_cache_key（OpenAI 前缀缓存键，见下）
//  6. 派生键：system 提示词 + 首条用户消息的哈希（客户端不发会话 id 时的回退，
//     带 user_id 时把 user 维度混入哈希当命名空间——见下方契约说明）
//
// 前 5 项均为 conversation 维度（对话级）。metadata.user_id **本身不作粘性键**
// （P1-anti-monopoly 剔除）：user 维度粒度过粗——一个 user 的全部并行对话
// 会钉同一账号（粘性范围远大于上游 prompt cache 的对话级边界），且曾抢占顶层
// conversation_id 的优先级。旧 user_id 绑定靠 TTL 自然过期，键消失不产生脏绑定。
// user_id 现在只作为第 6 项派生键的**命名空间**参与哈希（见下方 2026-10-24 修订），
// 不再导致「完全不粘性」。
//
// issue #35：客户端实际发 camelCase 的 conversationId，此前只识别 snake_case，
// 导致粘性路由不命中、同对话轮转不同账号、上游上下文缓存 miss。现两种命名均识别，
// snake_case 优先级高于 camelCase（同值不同名命中同一对话时返回相同值，天然不混用）。
//
// 第 5 项 prompt_cache_key：pi-ai 驱动的客户端（dsh 等）把会话 ID 放在这个 OpenAI
// 前缀缓存字段里（而非 conversation_id），网关在 upstream 侧本就认它（见
// InjectPromptCacheKey 优先级 1：客户端自带则原值保留）。纳入识别后，这类客户端
// 无需改配置即可命中粘性。置于显式会话键之后、派生键之前，绝不抢占 conversation
// 维度的优先级。
//
// 🔴 派生键与 user_id（2026-10-24 修订，推翻旧「不派生」契约）：
//
// 旧实现是 body 带 metadata.user_id / 顶层 user_id 时**完全不派生**，让这类客户端
// 回落加权轮换。上线后发现代价极大：这类客户端每条请求都换号，而上游前缀缓存是
// **账号级**的，于是同一份上下文每次都在冷账号上重算。线上实测（1.8h / 1000 条）：
//   - glm-5.3-flash 缓存命中 11.9%、kimi-k3-1 7.0%、kimi-k2.5 5.1%、deepseek-v4-flash 0%
//   - 同一份 66340 token 上下文 8 秒内打到 4 个账号：两个命中 99.9%，两个 0%
//   - 对照实验：同 body 连发两次，不带 user_id → 同一账号；带 user_id → 两个不同账号
//
// 而「粘住即命中」在实测里是 99.9%（prompt=5123 miss=3）。
//
// 新契约：user_id 不再抑制派生，而是**作为命名空间混入哈希**（见 userScopeOf）。
// P1-anti-monopoly 的原意是「别把某个 user 的全部并行对话钉到同一账号」，那份担忧
// 在混入哈希后依然成立——键仍由 system + 首条 user 决定，粒度还是对话级：
//   - 同一 user 的不同对话 → 不同键（不会被整体钉在一个号上）
//   - 不同 user 的相同内容 → 不同键（租户隔离不破）
//   - 同一对话多轮 → 同键（上游前缀缓存可命中）
//
// 无 user_id 的请求哈希材料与旧实现逐字节一致，既有绑定不受影响。
func ExtractKey(body []byte) string {
	obj := parseBody(body)
	if obj == nil {
		return ""
	}
	return extractKeyFrom(obj)
}

// parseBody 把请求体解析成通用 map；空体 / 非法 JSON / 字面 null 均返回 nil。
func parseBody(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	return obj
}

// ExtractKeys 一次解析同时取会话键与前缀指纹（前缀指纹见 PrefixKey）。
//
// 为什么合并：热路径上 handler 本来就要解析一次 body 拿会话键，前缀亲和如果再
// 单独调 PrefixKey(body) 就会为同一个几十 KB 的 body 多解一次。合并后解析次数
// 不增，前缀指纹的边际成本只剩一次 SHA-256。ExtractKey 保留为薄包装（外部
// 调用方与既有测试不变）。
func ExtractKeys(body []byte) (sessKey, prefixKey string) {
	obj := parseBody(body)
	if obj == nil {
		return "", ""
	}
	return extractKeyFrom(obj), prefixKeyOf(obj)
}

// ExtractKeysSized 同 ExtractKeys，额外返回前缀材料的字节数（见 prefixPartsOf）。
//
// 为什么要这个观测（2026-10-25）：线上出现「亲和大量 hit、整体缓存命中率却只有
// 8%」的时段——亲和确实把这些请求撮合到了同一个号，但那批请求 ~1k prompt 里
// 共同前缀只有一百多 token，可命中的部分本来就小。没有 prefix 体积，这个现象
// 只能靠翻原始 body 才能发现，面板上看就是「机制失灵」。
func ExtractKeysSized(body []byte) (sessKey, prefixKey string, prefixBytes int) {
	obj := parseBody(body)
	if obj == nil {
		return "", "", 0
	}
	prefixKey, prefixBytes = prefixPartsOf(obj)
	return extractKeyFrom(obj), prefixKey, prefixBytes
}

// extractKeyFrom 是 ExtractKey 的「已解析」版本，键优先级链见 ExtractKey 注释。
func extractKeyFrom(obj map[string]any) string {
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["conversationId"]); v != "" {
		return v
	}
	// 5. prompt_cache_key：OpenAI 系的会话级前缀缓存键，语义就是"同一会话复用同一
	//    前缀"，与粘性诉求同源。放显式会话键之后、派生键之前，不抢占优先级。
	if v := strOrEmpty(obj["prompt_cache_key"]); v != "" {
		return v
	}
	// 派生键（带 user_id 时以用户命名空间混入哈希，见 deriveKey 与上方契约说明）。
	return deriveKey(obj)
}

// userScopeOf 取已解析 body 的 user 维度标识（metadata.user_id 优先，其次顶层
// user_id；非空字符串才算），作为派生键的命名空间。无标识返回空串。
//
// 只认 user_id 这两个位置，不认 OpenAI 的顶层 user 字段：user 常被客户端塞成
// 每请求变化的随机值（或以 request-id 形态出现），拿它入键会让派生键逐请求漂移，
// 把当前本来能粘住的客户端一起弄坏。user_id 的语义是稳定的租户/账号标识。
func userScopeOf(obj map[string]any) string {
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["user_id"]); v != "" {
			return v
		}
	}
	return strOrEmpty(obj["user_id"])
}

// derivedKeyPrefix 派生键前缀，与显式会话 id 的命名空间隔离：
// 即便客户端恰好传了形如 "d-<hex>" 的显式 id 也不至于与派生键混淆（显式 id 优先返回）。
const derivedKeyPrefix = "d-"

// deriveKey 从消息内容派生稳定会话键：SHA-256(system 文本 + 首条 user 文本) 前 16 字节。
//
// 为什么用「system + 首条 user」而不是全部消息：
//   - 多轮对话里历史消息每轮追加，全量哈希会每轮变化 → 粘性完全失效；
//   - system 与首条 user 在一次对话中恒定，足以区分不同对话；
//   - 同一会话多轮请求 → 同一键 → 稳定粘住同一账号（上游 prompt 缓存命中）。
//
// 取不到用户文本（纯图片等）时返回空串：不粘性，退回普通轮换（安全降级）。
func deriveKey(obj map[string]any) string {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	systemText, firstUserText := "", ""
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		text := messageText(msg["content"])
		switch strOrEmpty(msg["role"]) {
		case "system", "developer":
			if systemText == "" {
				systemText = text
			}
		case "user":
			if firstUserText == "" {
				// 首条 user 用内容签名（ids.go contentSignature）而非纯文本提取：
				// 纯图片轮（无 text part）在 messageText 下恒空串 → 派生键失效
				//（首图会话的粘性盲区）；签名口径下图片 part 以 [type:摘要] 入键。
				firstUserText = userContentSignature(msg["content"])
			}
		}
		if firstUserText != "" && systemText != "" {
			break // 都已拿到：停止遍历长历史
		}
	}
	if firstUserText == "" {
		return "" // 无用户消息：无从归属会话
	}
	// user 命名空间前缀：无 user_id 时材料与修订前逐字节一致（既有绑定不失效）；
	// 有 user_id 时把用户维度混入哈希——不同用户永不同键（隔离），同一用户的
	// 不同对话仍各自成键（对话级粒度不变），同一对话多轮仍是同键（粘性）。
	material := systemText + "\x00" + firstUserText
	if scope := userScopeOf(obj); scope != "" {
		material = scope + "\x00" + material
	}
	sum := sha256.Sum256([]byte(material))
	return derivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// userContentSignature 把已解析的 content（any）转回 RawMessage 后取**会话级稳定**
// 签名，供 deriveKey（粘性键）专用。与 TurnKey 的 contentSignature（全文摘）不同：
// 文本 part 照常拼接；非文本 part 只入 "[type]" 占位**不入内容摘要**——同一逻辑
// 图片的 URL 逐轮变化（签名 URL/重编码 base64）是真实场景，内容摘要入键会让
// 派生键随轮漂移、粘性名存实亡（fork 既有契约：image url changes must not
// break derived key stability）。占位仍能区分"有无图片/图片数量"，纯图片首条
// 消息（无 text part）由此可派生非空键（首图会话粘性盲区修复，对齐上游 G1
// 的目标语义而保留 fork 的稳定性口径）。marshal 失败按无内容处理（不伪造）。
func userContentSignature(content any) string {
	raw, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	st := strings.TrimSpace(string(raw))
	if st == "" || st == "null" {
		return ""
	}
	if st[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return ""
		}
		return str
	}
	if st[0] != '[' {
		return ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	hasNonText := false
	for _, pr := range parts {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(pr, &p) != nil {
			return ""
		}
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
			continue
		}
		hasNonText = true
		b.WriteString("\n[" + p.Type + "]\n")
	}
	out := b.String()
	if !hasNonText {
		return out
	}
	return strings.TrimSpace(out)
}

// messageText 提取消息 content 的文本表示。
// 兼容：字符串 / [{type:"text",text:"..."}] 数组（OpenAI 多模态）；其他类型取空。
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, part := range v {
			if p, ok := part.(map[string]any); ok {
				sb.WriteString(strOrEmpty(p["text"]))
			}
		}
		return sb.String()
	}
	return ""
}

// strOrEmpty 把 JSON 字符串字段安全转 string（非字符串类型返回空）。
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
