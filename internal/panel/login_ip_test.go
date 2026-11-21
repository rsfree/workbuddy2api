package panel

import (
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
)

// 轮转必须严格按 egress 的地址顺序走，并且跨轮回绕 —— 顺序一旦不稳定，
// 「均摊到各出口」就不成立，等于回到随机踩点。
func TestNextLoginIPRoundRobin(t *testing.T) {
	eg := localEgress("10.9.0.1", "10.9.0.2", "10.9.0.3")
	p := New(Config{Egress: eg, LoginIPCursorFile: filepath.Join(t.TempDir(), "cursor.json")})
	ips := eg.LocalIPs()
	if len(ips) != 3 {
		t.Fatalf("前置条件失败：ips=%v", ips)
	}
	got := make([]string, 7)
	for i := range got {
		got[i] = p.nextLoginIP()
	}
	for i, g := range got {
		if want := ips[i%3].String(); g != want {
			t.Fatalf("第 %d 次：got %q, want %q（全序列 %v）", i, g, want, got)
		}
	}
}

// 游标落盘的意义：重启后必须接着上次的位置，否则每次发版都把新号挤回 IP[0]。
func TestNextLoginIPCursorSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cursor.json")
	eg := localEgress("10.9.1.1", "10.9.1.2", "10.9.1.3")
	p := New(Config{Egress: eg, LoginIPCursorFile: file})
	for i := 0; i < 4; i++ {
		p.nextLoginIP()
	}
	if cur := p.loginIPCursorValue(); cur != 4 {
		t.Fatalf("游标应为 4，got %d", cur)
	}

	// 模拟进程重启：同配置、同文件重新构造
	p2 := New(Config{Egress: eg, LoginIPCursorFile: file})
	if cur := p2.loginIPCursorValue(); cur != 4 {
		t.Fatalf("重启后游标应为 4，got %d（未落盘或未加载）", cur)
	}
	ips := eg.LocalIPs()
	if next := p2.nextLoginIP(); next != ips[4%3].String() {
		t.Fatalf("重启后第一个分配应接续，got %q want %q", next, ips[4%3].String())
	}
}

// 未启用 egress / 非 local / 地址为空：必须返回空（调用方走默认出口），
// 不能凭空返回一个"看起来像绑定"的地址。
func TestNextLoginIPDisabled(t *testing.T) {
	cases := map[string]*egress.Resolver{
		"nil":    nil,
		"direct": egress.New(egress.Config{Mode: egress.ModeDirect}),
		"static": egress.New(egress.Config{Mode: egress.ModeStatic, StaticURL: "http://127.0.0.1:1"}),
		"local-no-addr": egress.New(egress.Config{
			Mode:  egress.ModeLocal,
			Local: egress.Local{Interfaces: []string{"definitely-not-a-nic"}},
		}),
	}
	for name, r := range cases {
		p := New(Config{Egress: r})
		if got := p.nextLoginIP(); got != "" {
			t.Fatalf("%s: 期望空，got %q", name, got)
		}
	}
}

// 没配游标文件也必须能工作（内存轮转），只是重启会回到起点。
func TestNextLoginIPWithoutFile(t *testing.T) {
	p := New(Config{Egress: localEgress("10.9.2.1", "10.9.2.2")})
	a, b := p.nextLoginIP(), p.nextLoginIP()
	if a == "" || a == b {
		t.Fatalf("两个出口应交替分配，got %q then %q", a, b)
	}
}

// 落盘失败不能影响登录（游标退化为进程内），也不能 panic。
func TestNextLoginIPBadCursorPath(t *testing.T) {
	p := New(Config{
		Egress: localEgress("10.9.3.1"),
		// 把文件路径指到一个已存在的文件下面，MkdirAll 必然失败
		LoginIPCursorFile: "/dev/null/cursor.json",
	})
	if got := p.nextLoginIP(); got != "10.9.3.1" {
		t.Fatalf("落盘失败仍应正常分配，got %q", got)
	}
}

// 负载最低优先：分布被租约拉偏后，新号必须往空的地方补，而不是继续盲轮转。
func TestPickLeastLoaded(t *testing.T) {
	cases := []struct {
		name   string
		counts []int
		start  int
		want   int
	}{
		{"空池从游标起", []int{0, 0, 0}, 0, 0},
		{"空池游标非零", []int{0, 0, 0}, 2, 2},
		{"明显最少", []int{5, 1, 4}, 0, 1},
		{"平票取靠近游标", []int{2, 0, 0}, 2, 2},
		{"平票绕回", []int{0, 3, 0}, 1, 2},
		{"全部满载仍要返回一个", []int{3, 3, 3}, 1, 1},
		{"单出口", []int{9}, 0, 0},
		{"下标越界兜底", []int{1, 0}, 5, 1},
	}
	for _, c := range cases {
		if got := pickLeastLoaded(c.counts, c.start); got != c.want {
			t.Fatalf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
	if pickLeastLoaded(nil, 0) != -1 {
		t.Fatal("空列表应返回 -1")
	}
}

// 租约写入落盘 + 重启读回：这是「登录 IP = 使用 IP」跨重启成立的前提。
func TestLeaseOnLoginPersists(t *testing.T) {
	dir := t.TempDir()
	leaseFile := filepath.Join(dir, "egress_leases.json")
	eg := localEgress("10.11.0.1", "10.11.0.2")
	p := New(Config{Egress: eg, EgressLeaseFile: leaseFile})

	p.leaseOnLogin("uid-a", "10.11.0.2")
	p.leaseOnLogin("uid-b", "10.11.0.1")
	// 空源地址（复制链接登录）不能凭空写租约
	p.leaseOnLogin("uid-c", "")

	got, err := egress.LoadLeaseFile(leaseFile)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if len(got) != 2 || got["uid-a"] != "10.11.0.2" || got["uid-b"] != "10.11.0.1" {
		t.Fatalf("落盘内容不对：%v", got)
	}
	if _, ok := got["uid-c"]; ok {
		t.Fatal("没有源地址的登录不应写租约")
	}

	// 模拟重启：新 Resolver 加载后，uid-a 的出口必须是租约地址而不是哈希结果
	eg2 := localEgress("10.11.0.1", "10.11.0.2")
	if err := eg2.SetLeases(got); err != nil {
		t.Fatalf("重新加载失败：%v", err)
	}
	b, _ := eg2.Bind("uid-a", "", "")
	if b.LocalIP.String() != "10.11.0.2" {
		t.Fatalf("重启后租约未生效：got %s", b.LocalIP)
	}
}

// 非法源地址（不在候选列表）只记日志，不能让登录流程报错。
func TestLeaseOnLoginInvalidIPIgnored(t *testing.T) {
	dir := t.TempDir()
	eg := localEgress("10.12.0.1")
	p := New(Config{Egress: eg, EgressLeaseFile: filepath.Join(dir, "l.json")})
	p.leaseOnLogin("uid-x", "192.0.2.7") // 不在列表
	if n := eg.LeaseCount(); n != 0 {
		t.Fatalf("非法租约不应写入，got %d", n)
	}
}
