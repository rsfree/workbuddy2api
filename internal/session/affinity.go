package session

import "time"

// affEntry 前缀亲和记录：这段共享前缀的缓存最后是在哪个账号上热的。
//
// 与会话绑定（entry）的区别是**粒度**：
//   - entry 以会话为键（同一对话多轮 → 同号），解决「多轮不换号」；
//   - affEntry 以前缀为键（system + tools 相同的多个会话 → 同号），解决
//     「并行/新建会话把同一段 system 提示词在每个账号上都冷启动一次」。
//
// 两者是「强 / 弱」关系：会话绑定命中时亲和完全用不上；只有会话**尚未建立绑定**
// （新会话、或绑定的号持久失效后重分配）时才拿亲和当提示。
type affEntry struct {
	uid      string
	lastSeen time.Time
}

const (
	// defaultAffinityTTL 亲和记录存活时长，故意比会话 TTL（默认 30m）短。
	//
	// 亲和是**共享**状态：它押注「这个账号接下来还能接这段前缀」。一旦长时间没在
	// 它上面命中，更可能的解释是它被别的流量占满或已降级，继续把前缀往里引只会
	// 增加溢出。TTL 到期即遗忘，下一批成功流量会按当时落点重新建立——这是比
	// 「靠 miss 计数衰减」更简单也更难出错的退出机制。
	defaultAffinityTTL = 20 * time.Minute

	// defaultAffinityMax 注册表容量上限。键的基数是「不同 system+tools 组合」的数量，
	// 多客户端多模型场景下可以到千级；不设上限就是慢性内存泄漏。
	defaultAffinityMax = 4096

	// affinityEvictDiv 超限时按 lastSeen 最旧淘汰 1/N，而不是每次插入都精确 LRU
	// （那是每请求 O(n log n)）。批量淘汰让摊还成本落到 O(1) 量级。
	affinityEvictDiv = 8
)

func (r *Router) affinityTTL() time.Duration {
	if r.cfg.AffinityTTL > 0 {
		return r.cfg.AffinityTTL
	}
	return defaultAffinityTTL
}

func (r *Router) affinityMax() int {
	if r.cfg.AffinityMax > 0 {
		return r.cfg.AffinityMax
	}
	return defaultAffinityMax
}

// NoteAffinity 记录「这段前缀的缓存刚在 uid 上被算出/命中」。
//
// 只应由**真实成功**的请求调用：亲和的价值在于准确，把失败的、被上游拒掉的请求
// 也算进去，会把前缀引到一个其实没有该缓存的账号上。
func (r *Router) NoteAffinity(key, uid string) {
	if !r.cfg.PrefixAffinity || key == "" || uid == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.aff == nil {
		r.aff = map[string]affEntry{}
	}
	if _, exists := r.aff[key]; !exists && len(r.aff) >= r.affinityMax() {
		r.evictAffinityLocked(now)
	}
	r.aff[key] = affEntry{uid: uid, lastSeen: now}
	r.dirty = true
}

// AffinityFor 查这段前缀「应该去哪个账号」；没有可用的亲和记录时 ok=false。
//
// 判定口径与 StickyUsable 一致：**只看健康/封禁/冷却/模型级限额，不看 inFlight**。
// 所以这里返回的号可能此刻满载——那就交给调用方的 PickStickyForModel 判 StickyBusy，
// 本次溢出但亲和记录**保留**。反过来如果这里返回 false，说明是持久失效，记录已被
// 清掉，下一次成功会重建。
func (r *Router) AffinityFor(key, model string) (string, bool) {
	if !r.cfg.PrefixAffinity || key == "" {
		return "", false
	}
	r.mu.RLock()
	e, ok := r.aff[key]
	r.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Since(e.lastSeen) > r.affinityTTL() {
		r.dropAffinity(key, e.uid)
		return "", false
	}
	// stickyOK 会取 pool 读锁，必须在释放 r.mu 之后调用（锁序 session → pool，
	// 与 ResolveForModel 一致；反向不存在）。
	if !r.stickyOK(e.uid, model) {
		r.dropAffinity(key, e.uid)
		return "", false
	}
	return e.uid, true
}

// dropAffinity 删除亲和记录，但只删「仍是这条」的——期间可能已被别的成功请求
// 改指到新账号，那种情况下删掉会丢掉刚刚重建的正确信息。
func (r *Router) dropAffinity(key, uid string) {
	r.mu.Lock()
	if cur, ok := r.aff[key]; ok && cur.uid == uid {
		delete(r.aff, key)
		r.dirty = true
	}
	r.mu.Unlock()
}

// AffinityCount 返回注册的前缀数（观测用）。
func (r *Router) AffinityCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.aff)
}

// evictAffinityLocked 淘汰 lastSeen 最旧的一批记录。调用方必须持写锁。
func (r *Router) evictAffinityLocked(now time.Time) {
	n := len(r.aff) / affinityEvictDiv
	if n < 1 {
		n = 1
	}
	// 先按过期清理，再按最旧淘汰：过期项一定比未过期项更该走。
	expired := 0
	for k, e := range r.aff {
		if now.Sub(e.lastSeen) > r.affinityTTL() {
			delete(r.aff, k)
			expired++
		}
	}
	if expired > 0 {
		r.dirty = true
	}
	if expired >= n {
		return
	}
	type kv struct {
		k string
		t time.Time
	}
	oldest := make([]kv, 0, len(r.aff))
	for k, e := range r.aff {
		oldest = append(oldest, kv{k, e.lastSeen})
	}
	// 只需淘汰 n 个，用简单选择法避免整表排序（表最大也就几千项，但这是插入路径）。
	for i := 0; i < n-expired && i < len(oldest); i++ {
		min := i
		for j := i + 1; j < len(oldest); j++ {
			if oldest[j].t.Before(oldest[min].t) {
				min = j
			}
		}
		oldest[i], oldest[min] = oldest[min], oldest[i]
		delete(r.aff, oldest[i].k)
		r.dirty = true
	}
}
