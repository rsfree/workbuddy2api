package egress

import (
	"strings"
	"sync"
	"testing"
)

func leaseResolver(t *testing.T, ips ...string) *Resolver {
	t.Helper()
	return New(Config{Mode: ModeLocal, Local: Local{IPs: ips}})
}

// 租约必须压过哈希：这是「登录 IP = 使用 IP」的全部依据。
func TestLeaseOverridesHash(t *testing.T) {
	r := leaseResolver(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")
	const uid = "u-lease"
	hashIP, err := r.Bind(uid, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// 故意租到与哈希结果不同的地址
	target := "10.0.0.1"
	if hashIP.LocalIP.String() == target {
		target = "10.0.0.2"
	}
	if err := r.SetLease(uid, target); err != nil {
		t.Fatalf("SetLease: %v", err)
	}
	got, err := r.Bind(uid, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.LocalIP.String() != target {
		t.Fatalf("租约未生效：got %s, want %s（hash 原本是 %s）",
			got.LocalIP, target, hashIP.LocalIP)
	}

	// 清除后必须回落到哈希（同一账号、同一配置，结果应与最初一致）
	r.ClearLease(uid)
	back, err := r.Bind(uid, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if back.LocalIP.String() != hashIP.LocalIP.String() {
		t.Fatalf("清除租约后应回哈希：got %s want %s", back.LocalIP, hashIP.LocalIP)
	}
}

// 租约地址不在候选列表（配置改了 / 写坏了）：必须回退哈希，不能把号打到
// 一个不存在的源地址上（那会让该号所有请求 EADDRNOTAVAIL）。
func TestLeaseInvalidFallsBackToHash(t *testing.T) {
	r := leaseResolver(t, "10.0.1.1", "10.0.1.2")
	const uid = "u-bad-lease"

	// 直接 SetLease 一个不在列表的地址：应被拒绝
	if err := r.SetLease(uid, "192.0.2.99"); err == nil {
		t.Fatal("不在候选列表的租约应被拒绝")
	}
	if err := r.SetLease(uid, "not-an-ip"); err == nil {
		t.Fatal("非法地址应被拒绝")
	}

	// SetLeases 里的坏条目被丢弃，好条目保留
	err := r.SetLeases(map[string]string{
		uid:       "192.0.2.99",
		"u-good":  "10.0.1.2",
		"u-empty": "",
	})
	if err == nil || !strings.Contains(err.Error(), "无效租约") {
		t.Fatalf("应返回坏条目汇总，got %v", err)
	}
	if n := r.LeaseCount(); n != 1 {
		t.Fatalf("应只保留 1 条有效租约，got %d", n)
	}
	got, _ := r.Bind(uid, "", "")
	hash, _ := r.Bind(uid, "", "")
	if got.LocalIP.String() != hash.LocalIP.String() {
		t.Fatalf("坏租约的号应回退哈希")
	}
}

// 出口冷却不能被租约抵消：坏出口仍要临时顺延，恢复后自动回来。
func TestLeaseRespectsCooling(t *testing.T) {
	r := leaseResolver(t, "10.0.2.1", "10.0.2.2")
	const uid = "u-cool"
	if err := r.SetLease(uid, "10.0.2.1"); err != nil {
		t.Fatal(err)
	}
	// 打满阈值让该出口进入冷却
	for i := 0; i < defaultFailThreshold; i++ {
		r.NoteResult("local|10.0.2.1", false)
	}
	got, err := r.Bind(uid, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.LocalIP.String() == "10.0.2.1" {
		t.Fatal("租约地址冷却中，应顺延到其他出口")
	}
	// 恢复后回到租约地址
	r.NoteResult("local|10.0.2.1", true)
	back, _ := r.Bind(uid, "", "")
	if back.LocalIP.String() != "10.0.2.1" {
		t.Fatalf("冷却解除后应回到租约地址，got %s", back.LocalIP)
	}
}

// 并发读写：bindFor 每请求读，登录时会写。跑 -race 时这条要干净。
func TestLeaseConcurrentAccess(t *testing.T) {
	r := leaseResolver(t, "10.0.3.1", "10.0.3.2")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uid := "u-" + string(rune('a'+i))
			for j := 0; j < 50; j++ {
				_ = r.SetLease(uid, "10.0.3.1")
				_, _ = r.Bind(uid, "", "")
				_ = r.Leases()
				r.ClearLease(uid)
			}
		}(i)
	}
	wg.Wait()
}

// 非 local 模式下租约不生效（没有候选地址可绑），也不能 panic。
func TestLeaseIgnoredOutsideLocal(t *testing.T) {
	r := New(Config{Mode: ModeDirect})
	if err := r.SetLease("u", "10.0.0.1"); err == nil {
		t.Fatal("direct 模式下不应接受租约")
	}
	if _, ok := r.LeaseOf("u"); ok {
		t.Fatal("direct 模式下不应有租约")
	}
	b, _ := r.Bind("u", "", "")
	if b.LocalIP != nil {
		t.Fatalf("direct 模式应不绑定源地址，got %v", b.LocalIP)
	}
}

// 租约地址必须参与快照输出（持久化要用），且是字符串形态。
func TestLeaseSnapshot(t *testing.T) {
	r := leaseResolver(t, "10.0.4.1", "10.0.4.2")
	_ = r.SetLease("u1", "10.0.4.1")
	_ = r.SetLease("u2", "10.0.4.2")
	m := r.Leases()
	if m["u1"] != "10.0.4.1" || m["u2"] != "10.0.4.2" {
		t.Fatalf("快照不正确：%v", m)
	}
	if r.LeaseCount() != 2 {
		t.Fatalf("LeaseCount=%d want 2", r.LeaseCount())
	}
}
