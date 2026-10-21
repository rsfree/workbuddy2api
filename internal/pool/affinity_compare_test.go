package pool

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// pickSerial 复刻读锁化之前的语义：选号 + 写 lastUsed 全在同一把写锁内。
// 只用于对照实验，不参与生产路径。
func (p *Pool) pickSerialForTest() *auth.Auth {
	now := time.Now()
	p.mu.Lock()
	e := p.pickLocked(nil, "", "", now)
	if e != nil {
		p.pickSeq++
		e.lastUsed = now
		e.usedSeq = p.pickSeq
	}
	p.mu.Unlock()
	if e == nil {
		return nil
	}
	return e.a
}

type spreadResult struct {
	name     string
	min, max int
	stddev   float64
	total    int
	collides int // 与上一次选中同号、且间隔 < minPickGap 的次数
	picks    int
}

func runSpread(name string, pick func(*Pool) *auth.Auth) spreadResult {
	const accounts = 46
	const workers = 8
	const perWorker = 3000

	p := New("")
	defer p.Close()
	idx := map[string]int{}
	lastAt := make([]time.Time, accounts)
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("uid-%03d", i)
		idx[uid] = i
		p.Add(&auth.Auth{UID: uid, AccessToken: "t"})
		p.mu.Lock()
		p.byUID[uid].credits = 1000 // 等权重：分布应接近均匀
		p.mu.Unlock()
	}

	var mu sync.Mutex
	counts := make([]int, accounts)
	var collides, picks int

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				a := pick(p)
				if a == nil {
					continue
				}
				now := time.Now()
				k := idx[a.UID]
				mu.Lock()
				counts[k]++
				picks++
				if !lastAt[k].IsZero() && now.Sub(lastAt[k]) < minPickGap {
					collides++
				}
				lastAt[k] = now
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// 同一账号的并发访问无法用"上一次选中"完全表达，这里统计的是
	// 「该号在上一次被选中后 minPickGap 内又被选中」的次数——即防撞号失效次数。
	res := spreadResult{name: name, min: counts[0], max: counts[0], total: picks, collides: collides, picks: picks}
	sum := 0.0
	for _, c := range counts {
		if c < res.min {
			res.min = c
		}
		if c > res.max {
			res.max = c
		}
		sum += float64(c)
	}
	mean := sum / float64(accounts)
	var sq float64
	for _, c := range counts {
		sq += (float64(c) - mean) * (float64(c) - mean)
	}
	res.stddev = math.Sqrt(sq / float64(accounts))
	return res
}

func TestPickAffinityComparison(t *testing.T) {
	serial := runSpread("write-lock(旧)", func(p *Pool) *auth.Auth { return p.pickSerialForTest() })
	concurrent := runSpread("read-lock(新)", func(p *Pool) *auth.Auth { return p.Pick() })

	for _, r := range []spreadResult{serial, concurrent} {
		t.Logf("%-16s total=%-6d min=%-4d max=%-4d stddev=%.2f  gap<100ms=%-5d (%.3f%%)",
			r.name, r.total, r.min, r.max, r.stddev, r.collides, float64(r.collides)*100/float64(r.picks))
	}
	// 分布均匀性：标准差相对均值的偏离不应超过 5%。
	mean := float64(concurrent.total) / 46.0
	if concurrent.stddev/mean > 0.05 {
		t.Errorf("读锁版分布离散度过高: stddev=%.2f mean=%.2f", concurrent.stddev, mean)
	}
}

// 顺带看一眼每个账号的分布是否被字典序/权重饿死。
func TestPickSpreadShape(t *testing.T) {
	const accounts = 46
	p := New("")
	defer p.Close()
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("uid-%03d", i)
		p.Add(&auth.Auth{UID: uid, AccessToken: "t"})
		p.mu.Lock()
		p.byUID[uid].credits = 1000
		p.mu.Unlock()
	}
	counts := map[string]int{}
	for i := 0; i < 20000; i++ {
		if a := p.Pick(); a != nil {
			counts[a.UID]++
		}
	}
	min, max := 1<<30, 0
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
		if counts[k] < min {
			min = counts[k]
		}
		if counts[k] > max {
			max = counts[k]
		}
	}
	sort.Strings(keys)
	t.Logf("单线程 20000 次：账号数=%d min=%d max=%d 偏差=%.1f%%", len(keys), min, max, float64(max-min)*100/float64(max))
}

// 会话亲和性：粘性命中走 PickByUIDForModel，与 pick 的加权抽签是两条路。
// 并发下同一 uid 的命中必须稳定返回同一个账号（这是"不跳号"的前提）。
func TestStickyAffinityUnderConcurrency(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "sticky-1", AccessToken: "t"})
	p.mu.Lock()
	p.byUID["sticky-1"].credits = 1000
	p.mu.Unlock()

	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]int{}
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a := p.PickByUIDForModel("sticky-1", "any-model"); a != nil {
				mu.Lock()
				got[a.UID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != 1 || got["sticky-1"] != 64 {
		t.Fatalf("粘性命中不稳定: %v（期望全部命中 sticky-1）", got)
	}
	// 未命中记录不存在时不应误返回其他账号。
	if a := p.PickByUIDForModel("not-exist", "m"); a != nil {
		t.Fatalf("不存在的 uid 不应命中: %s", a.UID)
	}
}

// 真实速率下的分布：压测的 24000 次选号在 0.4s 内完成（≈60k picks/s），
// 远超实际网关。这里用 8 并发 + 1ms 间隔（≈8k QPS 上限，单请求平均还会再降）
// 复现生产节奏，观察离散度是否回到单线程水平。
func TestPickSpreadAtRealisticRate(t *testing.T) {
	const accounts = 46
	const workers = 8
	const perWorker = 200

	p := New("")
	defer p.Close()
	idx := map[string]int{}
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("uid-%03d", i)
		idx[uid] = i
		p.Add(&auth.Auth{UID: uid, AccessToken: "t"})
		p.mu.Lock()
		p.byUID[uid].credits = 1000
		p.mu.Unlock()
	}

	var mu sync.Mutex
	counts := make([]int, accounts)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if a := p.Pick(); a != nil {
					mu.Lock()
					counts[idx[a.UID]]++
					mu.Unlock()
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()

	min, max, sum := counts[0], counts[0], 0
	for _, c := range counts {
		if c < min {
			min = c
		}
		if c > max {
			max = c
		}
		sum += c
	}
	mean := float64(sum) / float64(accounts)
	var sq float64
	for _, c := range counts {
		sq += (float64(c) - mean) * (float64(c) - mean)
	}
	stddev := math.Sqrt(sq / float64(accounts))
	// 基准取「独立均匀抽签」的理论标准差，而不是 0：
	//   stddev = sqrt(mean * (1 - 1/n))
	// 旧写锁版能到 0.4（几乎完美轮转），是 minPickGap + 写锁串行的副产品——
	// 那反而削弱了加权语义（等频选号，不看权重）。读锁化后回到真随机，
	// 与理论值的偏离应在 1.5 倍以内；超过说明选号真的在扎堆。
	theory := math.Sqrt(mean * (1 - 1.0/float64(accounts)))
	t.Logf("真实速率（8 并发 × 1ms）：total=%d min=%d max=%d mean=%.1f stddev=%.2f 理论随机=%.2f (%.1fx)",
		sum, min, max, mean, stddev, theory, stddev/theory)
	if stddev > theory*1.5 {
		t.Errorf("真实速率下选号扎堆: stddev=%.2f > 理论 %.2f × 1.5", stddev, theory)
	}
}
