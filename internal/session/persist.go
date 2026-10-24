package session

// 会话状态本地快照：把粘性绑定表（entries）与前缀亲和表（aff）定期落盘，
// 启动时恢复，避免「每次发版/重启 = 一次全量冷启动」。
//
// 为什么需要（2026-10-25 线上实测）：
// 上游前缀缓存是**账号级**的，而这两张表此前只在内存里（redisstore 未配置时
// Store 退化成 Noop）。于是一次 docker compose up -d 就清零：所有活跃会话下一跳
// 全部被哈希到新账号、前缀缓存全冷。同一版本内两次 recreate 的对照数据——
// 重启前 sticky hit/new = 37/38（49%），重启后 9 分钟 16/33（32.7%），
// prompt≥5k 的请求缓存命中从 79.5% 掉到 32.7%。
//
// 与 redisstore 的分工：Redis 是跨机备份（可选、公网 RTT 高、只 fire-and-forget），
// 本文件是**同机重启**这条最高频路径的快速恢复（本地磁盘、原子替换、带时间戳）。
// 两者互不依赖：Redis 未配置时本机制照常工作；Redis 已配置时本机制仍然更快，
// 因为恢复不需要一次 SCAN + N 次 GET。
//
// 🔴 快照必须带 lastActive/lastSeen（不能只存 key→uid）：
// 否则重启会把每条绑定的寿命重置成满 TTL，早已不活跃的会话长期占着账号，
// 真正的新会话反而分不到「空闲号」——双段分配的前提是绑定表反映真实活跃度。

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// stateVersion 快照格式版本。字段语义变化时递增；版本不认识直接忽略——宁可冷启动
// 一次，也不要用语义已变的旧绑定把会话钉在错的账号上（错绑比不绑贵得多）。
const stateVersion = 1

// defaultPersistInterval 落盘周期。30s 是「丢得起的量」与「写得够少」的折中：
// 崩溃最多丢 30s 的绑定变更，代价是这段时间内的新会话重启后要重新分配。
const defaultPersistInterval = 30 * time.Second

// stateDoc 快照文档。
type stateDoc struct {
	Version  int         `json:"version"`
	SavedAt  time.Time   `json:"saved_at"`
	Binds    []stateBind `json:"binds,omitempty"`
	Affinity []stateAff  `json:"affinity,omitempty"`
}

// stateBind 一条会话绑定。键是会话指纹（"d-<hex>" / 显式 conversation id）。
type stateBind struct {
	Key        string    `json:"k"`
	UID        string    `json:"u"`
	LastActive time.Time `json:"t"`
}

// stateAff 一条前缀亲和记录。键是 prefixKey+"\x00"+规范模型名，可能含 NUL——
// JSON 会转义成 \u0000，往返无损。
type stateAff struct {
	Key      string    `json:"k"`
	UID      string    `json:"u"`
	LastSeen time.Time `json:"t"`
}

// SnapshotState 序列化当前未过期的绑定与亲和记录；失败返回 nil（调用方跳过本次落盘）。
func (r *Router) SnapshotState() []byte {
	now := time.Now()
	r.mu.RLock()
	doc := stateDoc{
		Version:  stateVersion,
		SavedAt:  now,
		Binds:    make([]stateBind, 0, len(r.entries)),
		Affinity: make([]stateAff, 0, len(r.aff)),
	}
	for k, e := range r.entries {
		if expired(e, now, r.cfg.TTL) {
			continue
		}
		doc.Binds = append(doc.Binds, stateBind{Key: k, UID: e.uid, LastActive: e.lastActive})
	}
	for k, e := range r.aff {
		if now.Sub(e.lastSeen) > r.affinityTTL() {
			continue
		}
		doc.Affinity = append(doc.Affinity, stateAff{Key: k, UID: e.uid, LastSeen: e.lastSeen})
	}
	r.mu.RUnlock()

	raw, err := json.Marshal(doc)
	if err != nil {
		log.Printf("[session] 状态快照序列化失败: %v", err)
		return nil
	}
	return raw
}

// RestoreState 把快照合并进当前内存表，返回恢复的绑定数与亲和数。
//
// 三条过滤规则（缺一条都会让恢复变成负债）：
//   - 过期项丢弃（按快照里的原始时间判，不用「现在」续命）；
//   - 内存里已存在的键**不覆盖**——进程内刚建立的落点比快照新；
//   - 亲和表超过容量上限即停止装载（快照可能来自配置更大的旧实例）。
func (r *Router) RestoreState(raw []byte) (binds, affs int) {
	if len(raw) == 0 {
		return 0, 0
	}
	var doc stateDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Printf("[session] 状态快照解析失败，按冷启动处理: %v", err)
		return 0, 0
	}
	if doc.Version != stateVersion {
		log.Printf("[session] 状态快照版本 %d 与本进程 %d 不一致，忽略", doc.Version, stateVersion)
		return 0, 0
	}

	now := time.Now()
	r.mu.Lock()
	for _, b := range doc.Binds {
		if b.Key == "" || b.UID == "" {
			continue
		}
		if expired(entry{uid: b.UID, lastActive: b.LastActive}, now, r.cfg.TTL) {
			continue
		}
		if _, exists := r.entries[b.Key]; exists {
			continue
		}
		r.entries[b.Key] = entry{uid: b.UID, lastActive: b.LastActive}
		binds++
	}
	if r.cfg.PrefixAffinity {
		for _, a := range doc.Affinity {
			if a.Key == "" || a.UID == "" {
				continue
			}
			if now.Sub(a.LastSeen) > r.affinityTTL() {
				continue
			}
			if _, exists := r.aff[a.Key]; exists {
				continue
			}
			if len(r.aff) >= r.affinityMax() {
				break
			}
			r.aff[a.Key] = affEntry{uid: a.UID, lastSeen: a.LastSeen}
			affs++
		}
	}
	r.mu.Unlock()

	if binds > 0 || affs > 0 {
		log.Printf("[session] 从本地快照恢复 %d 条会话绑定 + %d 条前缀亲和", binds, affs)
	}
	return binds, affs
}

// LoadStateFile 读取并恢复快照文件。文件不存在视为冷启动（不打印噪声日志）。
func (r *Router) LoadStateFile(path string) {
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[session] 读取状态快照失败（按冷启动处理）: %v", err)
		}
		return
	}
	r.RestoreState(raw)
}

// saveStateFile 落盘一次：原子替换（tmp + rename），快照前先清 dirty。
//
// dirty 必须在**取快照之前**清掉：取快照期间发生的变更会重新置 dirty，
// 下一轮再写一次；反过来（先写后清）会把这期间的变更永久吞掉。
func (r *Router) saveStateFile(path string) error {
	r.mu.Lock()
	r.dirty = false
	r.mu.Unlock()

	raw := r.SnapshotState()
	if raw == nil {
		r.mu.Lock()
		r.dirty = true
		r.mu.Unlock()
		return nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// StartStateFile 启动后台定期落盘（幂等：重复调用只生效一次）。
//
// 只在 dirty 时写：没有流量 → 没有变更 → 不产生周期性写盘。
// interval<=0 时用默认 30s（关闭持久化请在配置里把 persist_interval 写成 "0"，
// 由调用方决定是否调用本方法，而不是在这里静默变成「不写」）。
func (r *Router) StartStateFile(path string, interval time.Duration) {
	if path == "" {
		return
	}
	if interval <= 0 {
		interval = defaultPersistInterval
	}
	r.mu.Lock()
	if r.stateStop != nil {
		r.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	r.stateStop = stop
	r.statePath = path
	r.mu.Unlock()

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.mu.RLock()
				dirty := r.dirty
				r.mu.RUnlock()
				if !dirty {
					continue
				}
				if err := r.saveStateFile(path); err != nil {
					log.Printf("[session] 状态快照落盘失败（下轮重试）: %v", err)
					r.mu.Lock()
					r.dirty = true
					r.mu.Unlock()
				}
			}
		}
	}()
}

// StopStateFile 停止后台落盘并做最后一次同步落盘（停机能保住最后一段变更）。
// 幂等。
func (r *Router) StopStateFile() {
	r.mu.Lock()
	stop, path := r.stateStop, r.statePath
	r.stateStop = nil
	r.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if path == "" {
		return
	}
	if err := r.saveStateFile(path); err != nil {
		log.Printf("[session] 退出前落盘失败: %v", err)
	}
}
