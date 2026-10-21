package pool

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 低 QPS 下 Top5 截断会把流量集中到少数账号吗？
func TestTopKBiasProbe(t *testing.T) {
	const accounts = 46
	const picks = 300

	build := func(credits int64) *Pool {
		p := New("")
		for i := 0; i < accounts; i++ {
			uid := fmt.Sprintf("uid-%03d", i)
			p.Add(&auth.Auth{UID: uid, AccessToken: "t"})
			p.mu.Lock()
			p.byUID[uid].credits = credits
			p.mu.Unlock()
		}
		return p
	}

	for _, credits := range []int64{1000, -1} { // -1 = 积分未观测（未知）
		label := fmt.Sprintf("credits=%d", credits)
		if credits < 0 {
			label = "credits=unknown"
		}
		p := build(credits)
		counts := map[string]int{}
		// minPickGap=0 等价于「请求间隔 > 100ms」的低 QPS 场景：
		// 每次 Pick 时所有账号都已冷却完毕，Top5 完整可用，走不到 LRU 兜底。
		oldGap := minPickGap
		minPickGap = 0
		if minPickGap != 0 {
			t.Fatalf("minPickGap 未生效")
		}
		for i := 0; i < picks; i++ {
			if a := p.Pick(); a != nil {
				counts[a.UID]++
			}
		}
		minPickGap = oldGap
		used := make([]string, 0, len(counts))
		for uid := range counts {
			used = append(used, uid)
		}
		sort.Strings(used)
		// 用了多少号、最多的前 5 个占了多少
		type kv struct {
			uid string
			n   int
		}
		all := make([]kv, 0, len(counts))
		for _, u := range used {
			all = append(all, kv{u, counts[u]})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n })
		top5 := 0
		for i := 0; i < 5 && i < len(all); i++ {
			top5 += all[i].n
		}
		t.Logf("%-16s 用到账号=%d/%d  前5名承接=%.0f%%  最高=%d 最低=%d",
			label, len(counts), accounts, float64(top5)*100/float64(picks), all[0].n, all[len(all)-1].n)
		p.Close()
	}
}

// 冷启动：池里混有「积分已知」与「积分未知（0）」，后者会不会被长期压制？
func TestNewAccountStarvationProbe(t *testing.T) {
	const known = 10
	const fresh = 5
	const picks = 500

	p := New("")
	defer p.Close()
	for i := 0; i < known; i++ {
		uid := fmt.Sprintf("known-%02d", i)
		p.Add(&auth.Auth{UID: uid, AccessToken: "t"})
		p.mu.Lock()
		p.byUID[uid].credits = 10000
		p.mu.Unlock()
	}
	for i := 0; i < fresh; i++ {
		uid := fmt.Sprintf("fresh-%02d", i)
		p.Add(&auth.Auth{UID: uid, AccessToken: "t"})
		p.mu.Lock()
		p.byUID[uid].credits = 0 // 刚加入、还没查过余额
		p.mu.Unlock()
	}

	oldGap := minPickGap
	minPickGap = 0
	counts := map[string]int{}
	for i := 0; i < picks; i++ {
		if a := p.Pick(); a != nil {
			counts[a.UID]++
		}
	}
	minPickGap = oldGap

	knownHits, freshHits := 0, 0
	for uid, n := range counts {
		if len(uid) > 5 && uid[:5] == "known" {
			knownHits += n
		} else {
			freshHits += n
		}
	}
	t.Logf("存量 %d 号承接=%d 次（%.1f%%），新号 %d 个承接=%d 次（%.1f%%）",
		known, knownHits, float64(knownHits)*100/float64(picks),
		fresh, freshHits, float64(freshHits)*100/float64(picks))
	for i := 0; i < fresh; i++ {
		uid := fmt.Sprintf("fresh-%02d", i)
		t.Logf("  %s 被选中 %d 次", uid, counts[uid])
	}
	// 回归守卫：新号占池 1/3，至少要拿到 10% 的流量，否则又回到「越新越不用」
	// 的死锁（修复前实测 0/500）。
	if freshHits < picks/10 {
		t.Errorf("新号承接不足: %d/%d（修复前为 0）", freshHits, picks)
	}
}

// 高积分优先：修复「未知积分」分支后，确认正常加权语义没被削弱。
func TestHighCreditPreferenceIntact(t *testing.T) {
	const rich, poor = 5, 5
	const picks = 400

	p := New("")
	defer p.Close()
	for i := 0; i < rich; i++ {
		uid := fmt.Sprintf("rich-%02d", i)
		p.Add(&auth.Auth{UID: uid, AccessToken: "t"})
		p.mu.Lock()
		p.byUID[uid].credits = 10000
		p.byUID[uid].creditsTotal = 10000
		p.mu.Unlock()
	}
	for i := 0; i < poor; i++ {
		uid := fmt.Sprintf("poor-%02d", i)
		p.Add(&auth.Auth{UID: uid, AccessToken: "t"})
		p.mu.Lock()
		p.byUID[uid].credits = 1000
		p.byUID[uid].creditsTotal = 1000
		p.mu.Unlock()
	}

	oldGap := minPickGap
	minPickGap = 0
	richHits, poorHits := 0, 0
	for i := 0; i < picks; i++ {
		a := p.Pick()
		if a == nil {
			continue
		}
		if len(a.UID) > 4 && a.UID[:4] == "rich" {
			richHits++
		} else {
			poorHits++
		}
	}
	minPickGap = oldGap

	t.Logf("高积分 10k × %d → %d 次（%.0f%%）；低积分 1k × %d → %d 次（%.0f%%）",
		rich, richHits, float64(richHits)*100/float64(picks),
		poor, poorHits, float64(poorHits)*100/float64(picks))
	// 10 倍积分差 + Top5 截断：高积分号应拿走绝大部分。低积分号在低 QPS 下
	// 可能完全不被选（高积分号始终占满 top5），这是「优先用完高积分」的设计
	// 取舍，不是缺陷——它们的价值在于积分少、过期损失也小。
	if richHits < picks*7/10 {
		t.Errorf("高积分号承接不足（%d/%d），加权策略被削弱", richHits, picks)
	}
	_ = poorHits
}

// 但「低积分 + 快过期」必须能翻盘：否则快过期积分永远等不到消耗窗口，
	// 到期直接作废。这是 prefer_expiring ×3 的设计目的。
func TestExpiringLowCreditWinsSlot(t *testing.T) {
	const normal, expiring = 5, 2
	const picks = 400

	p := New("")
	p.SetPreferExpiring(true)
	defer p.Close()
	for i := 0; i < normal; i++ {
		uid := fmt.Sprintf("rich-%02d", i)
		p.Add(&auth.Auth{UID: uid, AccessToken: "t"})
		p.mu.Lock()
		p.byUID[uid].credits = 10000
		p.byUID[uid].creditsTotal = 10000
		p.mu.Unlock()
	}
	soon := time.Now().Add(6 * time.Hour)
	for i := 0; i < expiring; i++ {
		uid := fmt.Sprintf("soon-%02d", i)
		p.Add(&auth.Auth{UID: uid, AccessToken: "t"})
		p.mu.Lock()
		e := p.byUID[uid]
		e.credits = 500 // 积分很少
		e.creditsTotal = 500
		e.creditsExpiring = 500
		e.creditsEarliestRemaining = 500
		e.creditsEarliestExpiry = soon
		p.mu.Unlock()
	}

	oldGap := minPickGap
	minPickGap = 0
	soonHits := 0
	for i := 0; i < picks; i++ {
		if a := p.Pick(); a != nil && len(a.UID) > 4 && a.UID[:4] == "soon" {
			soonHits++
		}
	}
	minPickGap = oldGap

	t.Logf("快过期低积分 2 号承接 %d/%d（%.0f%%）", soonHits, picks, float64(soonHits)*100/float64(picks))
	if soonHits == 0 {
		t.Error("快过期积分从未被选中：×3 加权未能把它送进 top5，到期将直接作废")
	}
}
