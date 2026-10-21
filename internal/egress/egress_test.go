package egress

import (
	"net"
	"strings"
	"testing"
	"time"
)

func localResolver(t *testing.T, ips ...string) *Resolver {
	t.Helper()
	return New(Config{Mode: ModeLocal, Local: Local{IPs: ips}})
}

func TestLocalBindingIsDeterministic(t *testing.T) {
	r := localResolver(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")
	first, err := r.Bind("uid-a", "nick-a", "")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if first.LocalIP == nil {
		t.Fatal("local 模式应返回源地址绑定")
	}
	for i := 0; i < 50; i++ {
		got, err := r.Bind("uid-a", "nick-a", "")
		if err != nil {
			t.Fatalf("bind #%d: %v", i, err)
		}
		if got.Key() != first.Key() {
			t.Fatalf("同一账号出口漂移: %s != %s", got.Key(), first.Key())
		}
	}
}

func TestLocalBindingSpreadsAccounts(t *testing.T) {
	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}
	r := localResolver(t, ips...)
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		uid := "uid-" + string(rune('a'+i%26)) + "-" + itoa(i)
		b, err := r.Bind(uid, "", "")
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		seen[b.LocalIP.String()]++
	}
	if len(seen) != len(ips) {
		t.Fatalf("200 个账号只用到 %d/%d 个出口: %v", len(seen), len(ips), seen)
	}
}

func TestGroupByGroupPinsSameExit(t *testing.T) {
	r := New(Config{
		Mode:    ModeLocal,
		Local:   Local{IPs: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}},
		GroupBy: GroupByGroup,
		Groups:  map[string]string{"u1": "g1", "u2": "g1", "u3": "g2"},
	})
	b1, _ := r.Bind("u1", "", "")
	b2, _ := r.Bind("u2", "", "")
	if b1.Key() != b2.Key() {
		t.Fatalf("同组账号应共用出口: %s != %s", b1.Key(), b2.Key())
	}
	b3, _ := r.Bind("u3", "", "")
	if b3.Identity == b1.Identity {
		t.Fatalf("不同分组不应共用身份: %s", b1.Identity)
	}
	// 未分组账号仍按账号维度，不挤进任何组。
	b4, _ := r.Bind("u4", "", "")
	if strings.HasPrefix(b4.Identity, "grp:") {
		t.Fatalf("未分组账号不应落到组身份: %s", b4.Identity)
	}
}

func TestAuthGroupUsedWhenNoOverride(t *testing.T) {
	r := New(Config{
		Mode:    ModeLocal,
		Local:   Local{IPs: []string{"10.0.0.1", "10.0.0.2"}},
		GroupBy: GroupByGroup,
	})
	b, err := r.Bind("u1", "", "from-auth")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if b.Identity != "grp:from-auth" {
		t.Fatalf("应使用 auth 文件里的 group: %s", b.Identity)
	}
}

func TestExplicitIPsAreSortedAndDeduped(t *testing.T) {
	r := New(Config{Mode: ModeLocal, Local: Local{IPs: []string{"10.0.0.3", "10.0.0.1", "10.0.0.1"}}})
	ips := r.LocalIPs()
	want := []string{"10.0.0.1", "10.0.0.3"}
	if len(ips) != len(want) {
		t.Fatalf("去重失败: %v", ips)
	}
	for i := range want {
		if ips[i].String() != want[i] {
			t.Fatalf("排序不稳定: got %v want %v", ips, want)
		}
	}
}

func TestPoolUsernameTemplate(t *testing.T) {
	r := New(Config{
		Mode: ModePoolKeyed,
		Pool: Pool{
			Host:             "pool.invalid",
			Password:         "pw",
			UsernameTemplate: "acct-{identity}",
			KeyedPort:        2090,
		},
	})
	b, err := r.Bind("uid-1", "", "")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if b.Proxy == nil {
		t.Fatal("pool 模式应返回代理 URL")
	}
	user := b.Proxy.User.Username()
	if !strings.HasPrefix(user, "acct-acct-") {
		t.Fatalf("用户名模板未生效: %q", user)
	}
	if strings.Contains(user, ":") {
		t.Fatalf("SOCKS5 用户名不能含冒号: %q", user)
	}
	if pw, _ := b.Proxy.User.Password(); pw != "pw" {
		t.Fatalf("口令未带上: %q", pw)
	}
	if b.Proxy.Host != "pool.invalid:2090" {
		t.Fatalf("端口错误: %s", b.Proxy.Host)
	}
}

// hash 模板必须与 proxy-pool 客户端契约一致：sha256(salt + ":" + identity)[:8] 的 hex。
func TestHashTemplateMatchesContract(t *testing.T) {
	r := New(Config{
		Mode: ModePoolKeyed,
		Pool: Pool{Host: "pool.invalid", UsernameTemplate: "{hash}", Salt: "s3"},
	})
	got := r.Username("uid-1", "", "")
	if len(got) != 16 {
		t.Fatalf("派生名长度应为 16 个 hex 字符: %q", got)
	}
	if strings.Contains(got, "acct") {
		t.Fatalf("hash 模板不应含明文身份: %q", got)
	}
	// 同一身份 + 同一盐 → 稳定；换盐 → 变化。
	if again := r.Username("uid-1", "", ""); again != got {
		t.Fatalf("派生不稳定: %q != %q", again, got)
	}
	r2 := New(Config{Mode: ModePoolKeyed, Pool: Pool{Host: "pool.invalid", UsernameTemplate: "{hash}", Salt: "other"}})
	if r2.Username("uid-1", "", "") == got {
		t.Fatal("换盐后派生名应变化")
	}
}

func TestPoolRequiresHost(t *testing.T) {
	r := New(Config{Mode: ModePoolKeyed})
	if _, err := r.Bind("u", "", ""); err != ErrNoHost {
		t.Fatalf("缺 host 应报 ErrNoHost: %v", err)
	}
	if r.Enabled() {
		t.Fatal("缺 host 时不应视为启用")
	}
}

func TestPinModeUsesSlotPort(t *testing.T) {
	r := New(Config{
		Mode: ModePoolPin,
		Pool: Pool{Host: "pool.invalid", PinBasePort: 1188, PinSlots: 4},
	})
	b, err := r.Bind("uid-x", "", "")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	host, port, err := net.SplitHostPort(b.Proxy.Host)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if host != "pool.invalid" {
		t.Fatalf("host 错误: %s", host)
	}
	switch port {
	case "1188", "1189", "1190", "1191":
	default:
		t.Fatalf("端口应落在槽位区间: %s", port)
	}
}

func TestEgressCoolingRotatesLocalSlot(t *testing.T) {
	r := localResolver(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")
	first, err := r.Bind("uid-cool", "", "")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	// 把首选出口打到冷却。
	for i := 0; i < r.cfg.FailThreshold; i++ {
		r.NoteResult(first.Key(), false)
	}
	if cooling, _ := r.Cooling(first.Key()); !cooling {
		t.Fatal("连续失败应触发出口冷却")
	}
	// 同一账号应顺延到另一个可用出口，而不是继续撞坏出口。
	next, err := r.Bind("uid-cool", "", "")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if next.Key() == first.Key() {
		t.Fatalf("冷却中的出口仍被选中: %s", next.Key())
	}
	// 冷却结束后回到原槽位（确定性）。
	r.NoteResult(first.Key(), true)
	r.mu.Lock()
	delete(r.fail, first.Key())
	r.mu.Unlock()
	back, _ := r.Bind("uid-cool", "", "")
	if back.Key() != first.Key() {
		t.Fatalf("冷却解除后应回到原出口: %s != %s", back.Key(), first.Key())
	}
}

func TestDirectModeIsZeroValue(t *testing.T) {
	var r *Resolver
	b, err := r.Bind("u", "", "")
	if err != nil {
		t.Fatalf("nil resolver 不应报错: %v", err)
	}
	if b.Key() != "direct" {
		t.Fatalf("缺省应为直连: %s", b.Key())
	}
	if r.Enabled() {
		t.Fatal("nil resolver 不应视为启用")
	}
}

func TestStaticProxyURL(t *testing.T) {
	r := New(Config{Mode: ModeStatic, StaticURL: "socks5://user:pw@127.0.0.1:1080"})
	b, err := r.Bind("u", "", "")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if b.Proxy == nil || b.Proxy.Host != "127.0.0.1:1080" {
		t.Fatalf("静态代理未生效: %+v", b.Proxy)
	}
	// 缓存键不能含口令。
	if strings.Contains(b.Key(), "pw") {
		t.Fatalf("transport 缓存键泄露口令: %s", b.Key())
	}
}

func TestInvalidStaticScheme(t *testing.T) {
	r := New(Config{Mode: ModeStatic, StaticURL: "ftp://x:1"})
	if _, err := r.Bind("u", "", ""); err == nil {
		t.Fatal("不支持的 scheme 应报错")
	}
}

func TestIdentitySourceNickname(t *testing.T) {
	r := New(Config{
		Mode:           ModeLocal,
		Local:          Local{IPs: []string{"10.0.0.1", "10.0.0.2"}},
		IdentitySource: "nickname",
	})
	if got := r.Identity("u1", "alice", ""); got != "nick:alice" {
		t.Fatalf("nickname 身份: %s", got)
	}
	// 昵称为空回落 uid，避免所有空昵称账号挤到同一出口。
	if got := r.Identity("u1", "", ""); got != "acct:u1" {
		t.Fatalf("空昵称应回落 uid: %s", got)
	}
}

func TestSnapshotHasNoCredentials(t *testing.T) {
	r := New(Config{
		Mode: ModePoolKeyed,
		Pool: Pool{Host: "pool.invalid", Password: "secret-pw", UsernameTemplate: "u-{identity}"},
	})
	r.NoteResult("proxy|socks5|pool.invalid:2090|u-acct-u1", false)
	for _, s := range r.Snapshot() {
		if strings.Contains(s.Key, "secret-pw") {
			t.Fatalf("快照泄露口令: %+v", s)
		}
	}
}

func TestFailCooldownExpires(t *testing.T) {
	r := New(Config{
		Mode:          ModeLocal,
		Local:         Local{IPs: []string{"10.0.0.1"}},
		FailThreshold: 1,
		FailCooldown:  10 * time.Millisecond,
	})
	b, _ := r.Bind("u", "", "")
	r.NoteResult(b.Key(), false)
	if cooling, _ := r.Cooling(b.Key()); !cooling {
		t.Fatal("应立即冷却")
	}
	time.Sleep(20 * time.Millisecond)
	if cooling, _ := r.Cooling(b.Key()); cooling {
		t.Fatal("冷却应已过期")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
