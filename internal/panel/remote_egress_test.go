package panel

import (
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
)

// localEgress 构造一个确定性的 local 出口解析器（显式 IP，不依赖本机网卡）。
func localEgress(ips ...string) *egress.Resolver {
	return egress.New(egress.Config{Mode: egress.ModeLocal, Local: egress.Local{IPs: ips}})
}

// 槽位出口必须与 egress 自己的分配逐字节一致 —— 否则「重登用原出口」会变成
// 「重登用一个看起来像原出口的另一个 IP」，比不做还糟。
func TestEgressIPForMatchesSlot(t *testing.T) {
	eg := localEgress("10.1.0.1", "10.1.0.2", "10.1.0.3")
	p := New(Config{Egress: eg})
	for _, uid := range []string{"u-1", "u-2", "u-3", "a", "some-long-uid-0001"} {
		got := p.egressIPFor(uid, "", "")
		slot := eg.LocalSlot(uid, "", "")
		want := eg.LocalIPs()[slot].String()
		if got != want {
			t.Fatalf("uid=%s: got %q, want %q（slot=%d）", uid, got, want, slot)
		}
	}
}

// 分组模式下槽位由分组决定，不是 uid —— 同组必须落在同一出口。
func TestEgressIPForGroupBy(t *testing.T) {
	eg := egress.New(egress.Config{
		Mode:           egress.ModeLocal,
		Local:          egress.Local{IPs: []string{"10.2.0.1", "10.2.0.2"}},
		GroupBy:        egress.GroupByGroup,
		IdentitySource: "uid",
	})
	p := New(Config{Egress: eg})
	a := p.egressIPFor("uid-a", "", "team-x")
	b := p.egressIPFor("uid-b", "", "team-x")
	if a == "" || a != b {
		t.Fatalf("同组必须同出口：a=%q b=%q", a, b)
	}
}

// 未启用 / 非 local 一律返回空：调用方据此走默认出口，不能静默算出一个假 IP。
func TestEgressIPForDisabled(t *testing.T) {
	cases := map[string]*egress.Resolver{
		"nil":    nil,
		"direct": egress.New(egress.Config{Mode: egress.ModeDirect}),
		// local 但地址发现失败（指定一个不存在的网卡）：必须返回空而不是瞎猜。
		// 注意 local 且 Local.IPs 为空时会**自动发现本机地址**，不是「没配就没有」。
		"local-no-addr": egress.New(egress.Config{
			Mode:  egress.ModeLocal,
			Local: egress.Local{Interfaces: []string{"definitely-not-a-nic"}},
		}),
		"static": egress.New(egress.Config{Mode: egress.ModeStatic, StaticURL: "http://127.0.0.1:1"}),
	}
	for name, r := range cases {
		p := New(Config{Egress: r})
		if got := p.egressIPFor("u", "", ""); got != "" {
			t.Fatalf("%s: 期望空（默认出口），got %q", name, got)
		}
	}
}

// 三种提示语义：一致不提示、不一致要同时给出两个 IP、新号（登录出口未知）要提示槽位。
func TestEgressConsistencyHint(t *testing.T) {
	eg := localEgress("10.3.0.1", "10.3.0.2")
	p := New(Config{Egress: eg})
	want := p.egressIPFor("u-1", "", "")
	if want == "" {
		t.Fatal("前置条件失败：槽位出口为空")
	}

	if ip, hint := p.egressConsistency("u-1", "", "", want); ip != want || hint != "" {
		t.Fatalf("一致时不应提示：ip=%q hint=%q", ip, hint)
	}

	ip2, hint2 := p.egressConsistency("u-1", "", "", "203.0.113.9")
	if ip2 != want || !strings.Contains(hint2, "203.0.113.9") || !strings.Contains(hint2, want) {
		t.Fatalf("不一致时应同时给出两个 IP：ip=%q hint=%q", ip2, hint2)
	}

	ip3, hint3 := p.egressConsistency("u-1", "", "", "")
	if ip3 != want || !strings.Contains(hint3, want) || !strings.Contains(hint3, "重登") {
		t.Fatalf("新号应提示槽位并建议重登：ip=%q hint=%q", ip3, hint3)
	}
}

// egress 未启用时一致性检查必须完全静默（不能在结果里塞空 IP 的提示）。
func TestEgressConsistencySilentWithoutLocal(t *testing.T) {
	p := New(Config{})
	if ip, hint := p.egressConsistency("u", "", "", "1.2.3.4"); ip != "" || hint != "" {
		t.Fatalf("未启用 egress 时应静默：ip=%q hint=%q", ip, hint)
	}
}
