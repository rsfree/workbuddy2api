// 出口租约的写入侧：登录成功后把「本次登录实际使用的源地址」钉给该 uid。
//
// 这样「登录 IP」与「使用 IP」在语义上合流：登录用哪个出口，入池后就一直用哪个。
// 新号不再依赖「轮转碰巧命中槽位」，老号重登也能把出口拉回原处。
package panel

import (
	"log"

	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
)

// leaseOnLogin 在登录成功、拿到 uid 之后调用。
//
// loginIP 为空表示本次登录没有绑定源地址（复制链接登录 / egress 未启用），
// 此时不写租约 —— 不能凭空给账号编一个出口。
func (p *Panel) leaseOnLogin(uid, loginIP string) {
	if uid == "" || loginIP == "" || p.cfg.Egress == nil {
		return
	}
	if err := p.cfg.Egress.SetLease(uid, loginIP); err != nil {
		// 地址不在候选列表等：只记录，不影响登录结果（登录本身已经成功）
		log.Printf("panel: 出口租约写入失败 uid=%s ip=%s: %v", uid, loginIP, err)
		return
	}
	p.persistEgressLeases()
	log.Printf("panel: 出口租约 uid=%s → %s（登录即固定，重启不回退）", uid, loginIP)
}

// persistEgressLeases 把内存租约整体写回文件。
// 整表写而不是追加：避免并发登录时出现半截文件，也让「清除租约」自然生效。
func (p *Panel) persistEgressLeases() {
	path := p.cfg.EgressLeaseFile
	if path == "" || p.cfg.Egress == nil {
		return
	}
	p.leaseFileMu.Lock()
	defer p.leaseFileMu.Unlock()
	if err := egress.SaveLeaseFile(path, p.cfg.Egress.Leases()); err != nil {
		log.Printf("panel: 出口租约落盘失败（仅内存生效）：%v", err)
	}
}
