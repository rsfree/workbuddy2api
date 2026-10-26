// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"strings"
	"sync/atomic"
	"time"
)

// ExtraModel 静态补录的模型条目（config models.extra）。
//
// 用途：上游账号目录（product config）里没有、但希望对客户端可见的模型。
// 典型场景是「产品已宣传 / 账号级路由尚未下发」的模型——列出来不等于上游可用，
// 真正的可用性仍由上游判定（例如生图路由缺失会返 14401）。因此这里只描述
// 「怎么列」，不参与任何可用性判断。
type ExtraModel struct {
	ID   string   `json:"id"`             // 裸名默认归 cn 域；"cn:x"/"global:x" 指定域
	Type string   `json:"type,omitempty"` // "image"/"video"；空 = 普通 chat 模型
	Name string   `json:"name,omitempty"` // 展示名，空则省略
	Tags []string `json:"tags,omitempty"` // 能力标签，空则省略
}

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey               string        // 网关/面板共同鉴权密钥；空 = 不鉴权
	SoftCooldown         time.Duration // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool          // 出站请求体指纹脱敏
	RecordClientInfo     bool          // 请求日志是否记录调用来源（客户端 IP / UA）
	HiddenModels         map[string]struct{} // 精确隐藏的模型 id（已归一化为 "cn:x"/"global:x" 全量形式）；nil = 不隐藏
	ExtraModels          []ExtraModel        // 静态补录条目（已归一化 id）；nil = 不补录
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }

// NormalizeHiddenModels 把用户配置的 hidden 条目归一化为精确 id 集合。
//
// 条目带合法 "cn:"/"global:" 前缀 → 只隐藏该域；裸名 → 两域都隐藏。
// 大小写敏感（与 internal/server.resolveModel 的前缀规则对称：取第一个 ":",
// 前段恰为 "cn"/"global" 才剥离）。返回的 map 只读共享，调用方不得修改。
func NormalizeHiddenModels(hidden []string) map[string]struct{} {
	if len(hidden) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hidden)*2)
	for _, m := range hidden {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if prefix, bare, ok := strings.Cut(m, ":"); ok && (prefix == "cn" || prefix == "global") {
			out[prefix+":"+bare] = struct{}{}
		} else {
			out["cn:"+m] = struct{}{}
			out["global:"+m] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeExtraModels 把 models.extra 归一化为带域前缀、去重、类型收敛的条目。
//
// 规则与 NormalizeHiddenModels 对称但有两点不同：
//   - 裸名默认归 **cn**（补录要给客户端一个确切 id，不能两域同时冒出来）；
//   - 允许 id 与 type 之间的弱校验：type 只认 image/video，其余（含显式 "chat"）
//     一律落空字符串（= 普通 chat 模型，不附带媒体端点字段）。
//
// 返回的切片只读共享，调用方不得修改。
func NormalizeExtraModels(extra []ExtraModel) []ExtraModel {
	if len(extra) == 0 {
		return nil
	}
	out := make([]ExtraModel, 0, len(extra))
	seen := make(map[string]struct{}, len(extra))
	for _, m := range extra {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		realm := "cn"
		if prefix, bare, ok := strings.Cut(id, ":"); ok && (prefix == "cn" || prefix == "global") {
			realm, id = prefix, strings.TrimSpace(bare)
		}
		if id == "" {
			continue
		}
		full := realm + ":" + id
		if _, dup := seen[full]; dup {
			continue
		}
		seen[full] = struct{}{}
		typ := strings.ToLower(strings.TrimSpace(m.Type))
		if typ != "image" && typ != "video" {
			typ = ""
		}
		out = append(out, ExtraModel{
			ID:   full,
			Type: typ,
			Name: strings.TrimSpace(m.Name),
			Tags: m.Tags,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
