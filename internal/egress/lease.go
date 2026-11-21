// 出口租约：给账号显式指定源地址，优先于按身份哈希推导。
//
// 为什么需要它
// ------------
// 哈希是「无状态」的省事做法：不用存映射，重启也稳定。但它有两个结构性缺陷：
//   - 账号登录前 uid 未知，算不出槽位，登录 IP 与入池后的使用 IP 必然错位；
//   - 想给某个号换出口，只能改全局 identity_source（等于重排全池）。
//
// 租约把「推导」换成「分配 + 记住」：登录用哪个出口，入池后就用哪个出口。
// 存一张 uid → IP 的表，代价远小于哈希带来的不可控。
//
// 一致性约定
// ----------
//   - 租约地址必须在 local 候选列表里；配置变更后失效的租约**自动回退哈希**，
//     不会把账号打到不存在的源地址上。
//   - 租约不取消出口冷却：坏出口仍会被临时顺延，恢复后回到租约地址。
package egress

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// LeaseOf 返回该账号的租约地址；无租约或租约已失效返回 false。
func (r *Resolver) LeaseOf(uid string) (net.IP, bool) {
	if r == nil || uid == "" {
		return nil, false
	}
	p := r.leases.Load()
	if p == nil {
		return nil, false
	}
	ip, ok := (*p)[uid]
	if !ok {
		// 二次校验：配置在运行期不会变（localIPs 启动期冻结），这里只防表被
		// 外部塞入非法地址（SetLeases 已过滤，属纵深防御）。
		return nil, false
	}
	if !r.hasLocalIP(ip) {
		return nil, false
	}
	return ip, true
}

// hasLocalIP 该地址是否在当前候选列表里。
func (r *Resolver) hasLocalIP(ip net.IP) bool {
	if r == nil || ip == nil {
		return false
	}
	for _, cand := range r.localIPs {
		if cand.Equal(ip) {
			return true
		}
	}
	return false
}

// Leases 返回租约快照（uid → 地址字符串），供持久化与面板展示。
func (r *Resolver) Leases() map[string]string {
	out := map[string]string{}
	if r == nil {
		return out
	}
	p := r.leases.Load()
	if p == nil {
		return out
	}
	for uid, ip := range *p {
		out[uid] = ip.String()
	}
	return out
}

// SetLeases 整体替换租约表（启动加载 / 运维批量重置）。
//
// 无效条目（地址非法、不在候选列表、uid 为空）被丢弃并汇总返回，
// 而不是让整张表失败：一条坏记录不该把其余租约一起废掉。
func (r *Resolver) SetLeases(m map[string]string) error {
	if r == nil {
		return nil
	}
	next := make(leaseTable, len(m))
	var bad []string
	for uid, raw := range m {
		uid = strings.TrimSpace(uid)
		ip := net.ParseIP(strings.TrimSpace(raw))
		switch {
		case uid == "":
			continue
		case ip == nil:
			bad = append(bad, uid+"="+raw+"(地址非法)")
		case !r.hasLocalIP(ip):
			bad = append(bad, uid+"="+raw+"(不在 local 候选列表)")
		default:
			next[uid] = ip
		}
	}
	r.leaseMu.Lock()
	r.leases.Store(&next)
	r.leaseMu.Unlock()
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("egress: 丢弃 %d 条无效租约: %s", len(bad), strings.Join(bad, "; "))
	}
	return nil
}

// SetLease 写入/覆盖单条租约。地址必须是 local 候选之一。
func (r *Resolver) SetLease(uid, rawIP string) error {
	if r == nil {
		return fmt.Errorf("egress: resolver is nil")
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return fmt.Errorf("egress: uid is empty")
	}
	ip := net.ParseIP(strings.TrimSpace(rawIP))
	if ip == nil {
		return fmt.Errorf("egress: invalid lease ip %q", rawIP)
	}
	if !r.hasLocalIP(ip) {
		return fmt.Errorf("egress: lease ip %q 不在 local 候选列表", rawIP)
	}
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	cur := r.leases.Load()
	next := make(leaseTable, 1)
	if cur != nil {
		next = make(leaseTable, len(*cur)+1)
		for k, v := range *cur {
			next[k] = v
		}
	}
	next[uid] = ip
	r.leases.Store(&next)
	return nil
}

// ClearLease 删除租约（回落到哈希分配）。不存在时是空操作。
func (r *Resolver) ClearLease(uid string) {
	if r == nil || uid == "" {
		return
	}
	r.leaseMu.Lock()
	defer r.leaseMu.Unlock()
	cur := r.leases.Load()
	if cur == nil {
		return
	}
	if _, ok := (*cur)[uid]; !ok {
		return
	}
	next := make(leaseTable, len(*cur))
	for k, v := range *cur {
		if k != uid {
			next[k] = v
		}
	}
	r.leases.Store(&next)
}

// LeaseCount 当前有效租约条数（日志/面板用）。
func (r *Resolver) LeaseCount() int {
	if r == nil {
		return 0
	}
	p := r.leases.Load()
	if p == nil {
		return 0
	}
	n := 0
	for _, ip := range *p {
		if r.hasLocalIP(ip) {
			n++
		}
	}
	return n
}
