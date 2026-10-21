// egress.go 出口隔离的 Transport 装配：把 egress.Binding 变成独立的
// http.Client（每出口一个连接池）。
//
// 为什么必须按出口隔离连接池
// --------------------------
// http.Transport 的 Proxy / DialContext 是**连接级**配置，连接一旦建好就绑定
// 了当时的源地址或代理链路。若多个出口共享一个 Transport，空闲连接会被后来
// 的请求复用——A 账号的请求可能坐着 B 账号出口的连接出去，"每账号固定 IP"
// 当场失效，而且这种串用在日志里看不出来（连接层没有账号概念）。
package upstream

import (
	"errors"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// egressClient 一个出口对应的 client 对（短 RPC + 聊天流各一个 Timeout）。
// 两者共享同一个 Transport，连接池按出口只建一份。
type egressClient struct {
	key  string
	http *http.Client
	chat *http.Client
	// lastUsed UnixNano，LRU 淘汰用。atomic：命中路径要刷新它，不能为此加锁。
	lastUsed atomic.Int64
}

// SetEgress 装配出口策略（main 启动期调用一次；nil = 直连）。
func (c *Client) SetEgress(r *egress.Resolver) {
	c.egress.Store(r)
}

// Egress 返回当前出口策略（可能为 nil）。
func (c *Client) Egress() *egress.Resolver {
	return c.egress.Load()
}

// bindFor 解析该账号的出站绑定。未启用出口策略时返回零值（直连）。
//
// Bind 失败（配置缺失 / 无可用本机地址）时**回落直连并节流告警**，而不是让
// 请求失败：出口隔离是风控增强项，不该成为可用性的单点。
func (c *Client) bindFor(a *auth.Auth) egress.Binding {
	r := c.egress.Load()
	if r == nil || !r.Enabled() || a == nil {
		return egress.Binding{}
	}
	b, err := r.Bind(a.UID, a.Nickname, a.Group())
	if err != nil {
		c.warnEgress("bind", a, err)
		return egress.Binding{}
	}
	return b
}

// warnEgress 出口相关告警节流：同一类告警最多每 60s 一条，避免每请求刷屏。
func (c *Client) warnEgress(op string, a *auth.Auth, err error) {
	now := time.Now().UnixNano()
	last := c.egressWarnAt.Load()
	if now-last < int64(time.Minute) {
		return
	}
	if !c.egressWarnAt.CompareAndSwap(last, now) {
		return
	}
	uid, nick := "", ""
	if a != nil {
		uid, nick = a.UID, a.Nickname
	}
	log.Printf("WARN: [egress] %s acct=%s: %v (fallback to direct)", op, logfmt.Label(uid, nick), err)
}

// httpFor 返回该账号的短 RPC client。
func (c *Client) httpFor(a *auth.Auth) *http.Client {
	hc, _ := c.clientPairFor(a)
	return hc
}

// chatHTTPFor 返回该账号的聊天流 client。
func (c *Client) chatHTTPFor(a *auth.Auth) *http.Client {
	_, hc := c.clientPairFor(a)
	return hc
}

// clientPairFor 取（或创建）该账号出口对应的 client 对。
func (c *Client) clientPairFor(a *auth.Auth) (*http.Client, *http.Client) {
	return c.clientPairForBinding(c.bindFor(a))
}

// clientPairForBinding 按已解析的绑定取 client 对。调用方若需要按出口记账
// （NoteResult），应先用 bindFor 拿绑定再调本函数，避免解析两次。
func (c *Client) clientPairForBinding(b egress.Binding) (*http.Client, *http.Client) {
	key := b.Key()
	if key == "direct" {
		return c.HTTP, c.chatHTTP()
	}

	now := time.Now().UnixNano()
	if v, ok := c.egressClients.Load(key); ok {
		ec := v.(*egressClient)
		ec.lastUsed.Store(now)
		return ec.http, ec.chat
	}

	ec := &egressClient{key: key}
	ec.lastUsed.Store(now)
	tr := c.newEgressTransport(b)
	// Timeout 语义与默认 client 对齐：短 RPC 受总时长约束，聊天流不受
	//（首字节由 ResponseHeaderTimeout 管，流中空闲由 IdleTimeout 管）。
	timeout := 120 * time.Second
	if c.HTTP != nil && c.HTTP.Timeout > 0 {
		timeout = c.HTTP.Timeout
	}
	ec.http = &http.Client{Timeout: timeout, Transport: tr}
	ec.chat = &http.Client{Timeout: 0, Transport: tr}

	// 并发首次见面：只保留先落地的那一份。落败者的 Transport 没有发出过请求，
	// 但仍显式清一次空闲连接（防御：万一有并发请求已借它建连）。
	actual, loaded := c.egressClients.LoadOrStore(key, ec)
	if loaded {
		roundTripCloseIdle(tr)
		ae := actual.(*egressClient)
		return ae.http, ae.chat
	}
	c.evictEgress(c.egress.Load())
	return ec.http, ec.chat
}

// evictEgress 超出上限时淘汰最久未用的出口（关闭其空闲连接）。
//
// 只在**新建**出口时调用，命中路径零开销。并发淘汰可能多删一两个（两个
// goroutine 各自淘汰不同的最旧项），代价仅是下次重建连接池，不影响正确性。
func (c *Client) evictEgress(r *egress.Resolver) {
	limit := 64
	if r != nil {
		limit = r.MaxTransports()
	}
	for {
		count := 0
		var oldestKey string
		var oldest int64
		c.egressClients.Range(func(k, v any) bool {
			count++
			ec := v.(*egressClient)
			lu := ec.lastUsed.Load()
			if oldestKey == "" || lu < oldest {
				oldestKey, oldest = k.(string), lu
			}
			return true
		})
		if count <= limit || oldestKey == "" {
			return
		}
		if v, ok := c.egressClients.LoadAndDelete(oldestKey); ok {
			if ec, ok := v.(*egressClient); ok {
				roundTripCloseIdle(ec.http.Transport)
			}
		}
	}
}

// newEgressTransport 构造该出口专属的 Transport。
func (c *Client) newEgressTransport(b egress.Binding) *http.Transport {
	tr := newTransport()
	// 每个出口一份连接池：按单出口的并发量收敛，不套用主池的 100/20。
	tr.MaxIdleConns = egressMaxIdleConns
	tr.MaxIdleConnsPerHost = egressMaxIdleConnsPerHost
	switch {
	case b.Proxy != nil:
		tr.Proxy = http.ProxyURL(b.Proxy)
	case b.LocalIP != nil:
		// local 模式：绑定 TCP 源地址。Proxy 必须显式置 nil——newTransport 默认
		// 是 ProxyFromEnvironment，环境里若配了 HTTP_PROXY，请求会先走代理，
		// 源地址绑定被完全绕过，看起来"配了但没生效"。
		tr.Proxy = nil
		d := newDialer()
		d.LocalAddr = &net.TCPAddr{IP: b.LocalIP}
		tr.DialContext = d.DialContext
	default:
		tr.Proxy = nil
	}
	// ResponseHeaderTimeout 与主 Transport 保持同一个生效值（main 会按 config
	// 覆盖 c.HeaderTimeout；此处新建的 Transport 需要自己接上，否则回到默认 60s）。
	if c.HeaderTimeout > 0 {
		tr.ResponseHeaderTimeout = c.HeaderTimeout
	}
	return tr
}

// noteEgressResult 记录一次出口使用结果，驱动出口级熔断。
//
// 只统计**链路**结果，不碰账号状态：代理挂了 / 源地址不通，不代表账号有问题。
func (c *Client) noteEgressResult(b egress.Binding, ok bool) {
	r := c.egress.Load()
	if r == nil || !r.Enabled() {
		return
	}
	r.NoteResult(b.Key(), ok)
}

// egressCooling 该出口是否处于熔断冷却期。
func (c *Client) egressCooling(b egress.Binding) (bool, time.Duration) {
	r := c.egress.Load()
	if r == nil || !r.Enabled() {
		return false, 0
	}
	return r.Cooling(b.Key())
}

// isEgressError 判断一次 Do 失败是否属于**出站链路**故障（代理握手 / 源地址
// 不可达），而非上游业务错误。
//
// Go 的 http.Transport 走 socks5 时，代理握手失败会包成
// *net.OpError{Op: "proxyconnect"}；本机源地址不可达通常是 *net.OpError 的
// connect 阶段失败。两者都不该算到账号头上。
func isEgressError(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		switch opErr.Op {
		case "proxyconnect":
			return true
		}
		// 源地址绑定失败：connect 阶段的 EADDRNOTAVAIL / ENETUNREACH。
		if opErr.Op == "dial" && opErr.Source != nil {
			return true
		}
	}
	return false
}
