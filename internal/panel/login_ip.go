// login_ip.go 新号登录出口的轮转分配。
//
// 问题：新号登录时 uid 未知，无法用 LocalSlot 算槽位；若全部从宿主机默认出口
// 登录，一批新号会带着**同一个 IP** 的特征进来 —— 这比"登录 IP ≠ 使用 IP"更显眼。
//
// 解法：按 egress local 的地址顺序轮转，把新号登录均摊到本机各个出口，使登录
// 分布与入池后的槽位分布在形状上一致。单个号仍可能登录 A、使用 B，所以 poll
// 结果里会给出对比提示（见 egressConsistency）。
//
// 游标必须落盘：否则每次重启都从 IP[0] 开始，等于把新号重新挤回同一个出口。
package panel

import (
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
)

// loadLoginIPCursor 读取游标（缺失/损坏一律当作 0，不阻塞启动）。
func (p *Panel) loadLoginIPCursor() {
	path := p.cfg.LoginIPCursorFile
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var v struct {
		Cursor int `json:"cursor"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Cursor < 0 {
		return
	}
	p.loginIPCursor = v.Cursor
}

// persistLoginIPCursor 原子写回游标。失败只记日志：轮转退化为进程内，不影响登录。
// 调用方必须持有 loginIPMu。
func (p *Panel) persistLoginIPCursor() {
	path := p.cfg.LoginIPCursorFile
	if path == "" {
		return
	}
	raw, err := json.Marshal(map[string]int{"cursor": p.loginIPCursor})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("panel: 登录出口游标目录创建失败：%v", err)
		return
	}
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("panel: 登录出口游标写入失败：%v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("panel: 登录出口游标替换失败：%v", err)
	}
}

// nextLoginIP 给新号登录分配出口地址：**账号数最少的出口优先**，平票按游标顺延。
//
// 为什么不是纯轮转：租约（登录即固定）会让分布偏离哈希的均匀性，纯轮转会把
// 新号继续往已经偏多的地方塞。按当前实际负载选，能把分布拉回均匀。
//
// 返回空 = 不绑定（egress 未启用 / 非 local / 地址为空），调用方走默认出口，
// 与加这个功能之前的行为完全一致。
func (p *Panel) nextLoginIP() string {
	r := p.cfg.Egress
	if r == nil || r.Mode() != egress.ModeLocal {
		return ""
	}
	ips := r.LocalIPs()
	if len(ips) == 0 {
		return ""
	}
	counts := p.egressLoad(ips)

	p.loginIPMu.Lock()
	defer p.loginIPMu.Unlock()
	start := p.loginIPCursor % len(ips)
	best := pickLeastLoaded(counts, start)
	// 游标推到选中位置的**绝对**下一位（保持递增，不回绕）：既让平票时天然轮流，
	// 也让「已分配多少次」可对账。取模只用于选起点。
	p.loginIPCursor += (best-start+len(ips))%len(ips) + 1
	p.persistLoginIPCursor()
	return ips[best].String()
}

// pickLeastLoaded 从 start 开始找负载最小的下标；平票取离 start 最近的那个，
// 因此同样负载下会自然轮流（配合调用方的游标推进）。
func pickLeastLoaded(counts []int, start int) int {
	n := len(counts)
	if n == 0 {
		return -1
	}
	if start < 0 || start >= n {
		start = 0
	}
	best := start
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if counts[idx] < counts[best] {
			best = idx
		}
	}
	return best
}

// egressLoad 统计每个候选出口当前的账号数。
//
// 优先用 pool 已注入的出口标签（Status.Egress = 实际绑定结果，含租约与冷却顺延）；
// 标签缺失（测试环境/未注入）时回落「租约 → 哈希」自行推算，保证仍有可用分布。
func (p *Panel) egressLoad(ips []net.IP) []int {
	counts := make([]int, len(ips))
	if p.cfg.Pool == nil || p.cfg.Egress == nil {
		return counts
	}
	pos := make(map[string]int, len(ips))
	for i, ip := range ips {
		pos[ip.String()] = i
	}
	for _, st := range p.cfg.Pool.List() {
		label := st.Egress
		if label == "" {
			if ip, ok := p.cfg.Egress.LeaseOf(st.UID); ok {
				label = ip.String()
			} else if a := p.cfg.Pool.AuthByUID(st.UID); a != nil {
				if slot := p.cfg.Egress.LocalSlot(a.UID, a.Nickname, a.Group()); slot >= 0 && slot < len(ips) {
					label = ips[slot].String()
				}
			}
		}
		if i, ok := pos[label]; ok {
			counts[i]++
		}
	}
	return counts
}

// loginIPCursorValue 只读当前游标（测试/观测用）。
func (p *Panel) loginIPCursorValue() int {
	p.loginIPMu.Lock()
	defer p.loginIPMu.Unlock()
	return p.loginIPCursor
}
