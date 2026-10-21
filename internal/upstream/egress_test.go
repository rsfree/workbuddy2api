package upstream

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
)

// 不同出口必须拿到不同的 client/Transport：共享连接池会让请求坐着别的出口的
// 空闲连接出去，"每账号固定 IP"静默失效。
func TestEgressTransportIsolation(t *testing.T) {
	c := New()
	r := egress.New(egress.Config{
		Mode:  egress.ModeLocal,
		Local: egress.Local{IPs: []string{"10.9.0.1", "10.9.0.2"}},
	})
	c.SetEgress(r)

	// 找两个落在不同出口的账号（哈希可能把前几个都放到同一槽）。
	var a1, a2 *auth.Auth
	for i := 0; i < 128 && a2 == nil; i++ {
		a := &auth.Auth{UID: fmt.Sprintf("uid-%03d", i)}
		if a1 == nil {
			a1 = a
			continue
		}
		if c.bindFor(a).Key() != c.bindFor(a1).Key() {
			a2 = a
		}
	}
	if a2 == nil {
		t.Fatal("128 个账号没有落到两个不同出口，哈希分布异常")
	}

	h1, ch1 := c.clientPairFor(a1)
	h2, ch2 := c.clientPairFor(a2)

	// 同一账号必须复用同一对 client（否则每次请求都新建连接池）。
	if h1b, ch1b := c.clientPairFor(a1); h1b != h1 || ch1b != ch1 {
		t.Fatal("同一账号的 client 未复用")
	}
	// 不同出口必须完全隔离。
	if h1 == h2 || ch1 == ch2 {
		t.Fatal("不同出口复用了 client")
	}
	if h1.Transport == h2.Transport {
		t.Fatal("不同出口复用了 Transport（连接池会串出口）")
	}
	// 短 RPC 与聊天流共享同一 Transport（同一出口只建一份池）。
	if h1.Transport != ch1.Transport {
		t.Fatal("同一出口的 http/chat 应共享 Transport")
	}
	if ch1.Timeout != 0 {
		t.Fatalf("聊天 client 不应有总时长限制: %v", ch1.Timeout)
	}
	// local 模式必须显式关掉环境代理：否则 HTTP_PROXY 会绕过源地址绑定。
	tr, ok := h1.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport 类型: %T", h1.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("local 模式的 Transport 不应启用环境代理")
	}
	if tr.DialContext == nil {
		t.Fatal("local 模式必须自定义 DialContext 以绑定源地址")
	}
}

func TestEgressDisabledUsesSharedClients(t *testing.T) {
	c := New()
	a := &auth.Auth{UID: "u1"}
	h, ch := c.clientPairFor(a)
	if h != c.HTTP || ch != c.ChatHTTP {
		t.Fatal("未启用出口策略时应复用默认 client")
	}
}

func TestEgressProxyTransport(t *testing.T) {
	c := New()
	r := egress.New(egress.Config{
		Mode: egress.ModePoolKeyed,
		Pool: egress.Pool{Host: "pool.invalid", Password: "pw", KeyedPort: 2090},
	})
	c.SetEgress(r)
	h, _ := c.clientPairFor(&auth.Auth{UID: "u1"})
	tr, ok := h.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport 类型: %T", h.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("pool 模式应启用 Proxy")
	}
	u, err := tr.Proxy(&http.Request{})
	if err != nil {
		t.Fatalf("proxy func: %v", err)
	}
	if u.Host != "pool.invalid:2090" {
		t.Fatalf("proxy host: %s", u.Host)
	}
}

func TestIsEgressError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"proxyconnect", &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"wrapped proxyconnect", fmt.Errorf("do: %w", &net.OpError{Op: "proxyconnect", Err: errors.New("auth failed")}), true},
		{"dial with source", &net.OpError{Op: "dial", Source: &net.TCPAddr{IP: net.ParseIP("10.0.0.1")}, Err: errors.New("cannot assign")}, true},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := isEgressError(tc.err); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// 出口缓存必须有上限：账号数 × 出口数可能远超预期，无界缓存会堆积 Transport。
func TestEgressClientCacheEvicts(t *testing.T) {
	c := New()
	r := egress.New(egress.Config{
		Mode:          egress.ModeLocal,
		Local:         egress.Local{IPs: []string{"10.8.0.1", "10.8.0.2", "10.8.0.3"}},
		MaxTransports: 2,
	})
	c.SetEgress(r)
	for i := 0; i < 16; i++ {
		c.clientPairFor(&auth.Auth{UID: fmt.Sprintf("uid-%d", i)})
	}
	n := 0
	c.egressClients.Range(func(_, _ any) bool { n++; return true })
	if n > 2 {
		t.Fatalf("出口缓存未淘汰: %d > 2", n)
	}
}
