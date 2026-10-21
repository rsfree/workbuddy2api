// Package egress 解析「账号 → 出口链路」的映射策略。
//
// 设计边界：本包只做**纯策略**（配置 + 确定性哈希 + 出口健康状态 + 本机地址
// 枚举），不建连接、不持有 http.Client。真正的 Transport 缓存与拨号在 upstream
// 侧（见 upstream/egress.go），这样策略可以脱离网络单测。
//
// 三种出口形态
// ------------
//   - local：绑定**本机多个出口 IP**。部署机若有多张网卡 / 多个 IP alias /
//     多个 EIP 映射，可按账号 hash 分散到不同源地址。零外部依赖、零额外延迟，
//     是默认策略。
//   - static：所有账号共用同一个静态代理（http/https/socks5）。
//   - pool-keyed / pool-pin：自建 SOCKS5 代理池。keyed 由代理侧按用户名哈希
//     选出口；pin 由客户端算槽位端口。
//
// 为什么「按账号固定出口」必须由客户端决定
// ----------------------------------------
// SOCKS5 是传输层协议，入口只看得到「连到哪个 host:port」；HTTP 的
// Authorization 头位于客户端与目标站之间的 TLS 会话内部，端到端加密，入口在
// 密码学上读不到。local 模式同理——源地址是内核在 connect 时选定的，只能由
// 发起方指定。所以映射只能在网关侧算好。
package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode 出口模式。
type Mode string

const (
	// ModeDirect 直连（不绑定源地址、不走代理）。零值，保证未配置时行为不变。
	ModeDirect Mode = "direct"
	// ModeLocal 绑定本机多个出口 IP，按身份 hash 分配。
	ModeLocal Mode = "local"
	// ModeStatic 所有账号共用同一个静态代理（http/https/socks5）。
	ModeStatic Mode = "static"
	// ModePoolKeyed 代理池「用户名即键」入口：代理侧按用户名哈希选出口。
	ModePoolKeyed Mode = "pool-keyed"
	// ModePoolPin 代理池「槽位端口」入口：客户端算槽位，连 base+slot 端口。
	ModePoolPin Mode = "pool-pin"
)

// GroupBy 分组维度。
type GroupBy string

const (
	// GroupByAccount 每个账号一个出口（默认）。
	GroupByAccount GroupBy = "account"
	// GroupByGroup 同组账号共用一个出口；未分组的账号仍按账号独立。
	GroupByGroup GroupBy = "group"
)

// Local 本机多出口 IP 配置。
type Local struct {
	// IPs 显式指定的源地址列表；空 = 自动发现本机网卡地址。
	IPs []string `json:"ips,omitempty"`
	// Interfaces 只在这些网卡上发现地址（空 = 全部网卡）。
	Interfaces []string `json:"interfaces,omitempty"`
	// IncludeIPv6 是否把 IPv6 地址纳入候选。默认 false：多数上游对 IPv6
	// 出口的风控策略与 IPv4 不同，混用会让「同账号同出口」的假设失效。
	IncludeIPv6 bool `json:"include_ipv6,omitempty"`
	// ExcludePrefixes 额外排除的网卡名前缀（在默认虚拟网卡过滤之外）。
	ExcludePrefixes []string `json:"exclude_prefixes,omitempty"`
}

// Pool 代理池入口参数。
//
// 注意：Host / Password / Salt 一律不设内置默认值。仓库里不出现任何真实入口
// 地址或口令——它们只能来自配置文件或环境变量（main 侧负责覆盖），避免把
// 自建代理池的拓扑随开源代码一起泄露。
type Pool struct {
	// Host 池入口主机（不含 scheme/端口）。
	Host string `json:"host"`
	// Password SOCKS5 口令。生产建议留空并改用 PasswordEnv。
	Password string `json:"password,omitempty"`
	// UsernameTemplate 用户名模板，支持 {identity} 与 {hash} 两个占位符。
	// 空 = "{identity}"。例："acct-{identity}"。
	UsernameTemplate string `json:"username_template,omitempty"`
	// Salt {hash} 占位符的派生盐。空 = 不加盐（仍做 sha256）。
	Salt string `json:"salt,omitempty"`
	// KeyedPort 「用户名即键」入口端口。
	KeyedPort int `json:"keyed_port,omitempty"`
	// PinBasePort 槽位端口基址（第 N 槽 = PinBasePort+N）。
	PinBasePort int `json:"pin_base_port,omitempty"`
	// PinSlots 槽位总数。
	PinSlots int `json:"pin_slots,omitempty"`
	// RoundPort 轮询入口端口（仅文档展示，本包不主动回落）。
	RoundPort int `json:"round_port,omitempty"`
}

// Config 出口策略配置。
type Config struct {
	Mode Mode `json:"mode"`
	// StaticURL mode=static 时的代理 URL（http:// / https:// / socks5://）。
	StaticURL string `json:"static_url,omitempty"`
	Local     Local  `json:"local,omitempty"`
	Pool      Pool   `json:"pool"`
	// IdentitySource 身份取值来源："uid"（默认）| "nickname" | "group"。
	// 影响 hash 输入，因此也影响出口分配；改动等于重排所有账号的出口。
	IdentitySource string `json:"identity_source,omitempty"`
	// GroupBy 分组维度；空 = account。
	GroupBy GroupBy `json:"group_by,omitempty"`
	// Groups 账号 → 分组名 的显式映射（覆盖 auth 文件里的 group 字段）。
	Groups map[string]string `json:"groups,omitempty"`
	// FailThreshold 同一出口连续失败多少次进入冷却；<=0 回落 5。
	FailThreshold int `json:"fail_threshold,omitempty"`
	// FailCooldown 出口冷却时长；<=0 回落 60s。
	FailCooldown time.Duration `json:"-"`
	// FailCooldownText 配置里的时长文本（"60s"/"5m"），由 main 侧解析后写入
	// FailCooldown。留在此处便于面板回显原始值。
	FailCooldownText string `json:"fail_cooldown,omitempty"`
	// MaxTransports 出口 Transport 缓存上限（超出按最久未用淘汰）；<=0 回落 64。
	MaxTransports int `json:"max_transports,omitempty"`
}

// 默认值（只在配置缺省时生效；不含任何真实地址，不构成泄露面）。
const (
	defaultFailThreshold = 5
	defaultFailCooldown  = 60 * time.Second
	defaultMaxTransports = 64
	defaultKeyedPort     = 2090
	defaultPinBasePort   = 1188
	defaultPinSlots      = 16
)

// ErrNoHost 池模式未配置入口主机。
var ErrNoHost = errors.New("egress: pool host is empty")

// ErrNoLocalIP local 模式未发现任何可用地址。
var ErrNoLocalIP = errors.New("egress: no usable local IP found")

// Binding 一次请求的出站绑定结果。
//
// Proxy 与 LocalIP 互斥：Proxy 非空走代理；LocalIP 非空绑定源地址；
// 两者都空 = 直连（使用进程默认源地址）。
type Binding struct {
	// Identity 出口身份（哈希输入），用于日志与面板对账。
	Identity string
	// Proxy 代理 URL；nil = 不走代理。
	Proxy *url.URL
	// LocalIP 源地址绑定；nil = 不绑定。
	LocalIP net.IP
	// key 预先算好的缓存键。Bind 时算一次——请求路径会多次调用 Key()
	//（取 client、记出口结果），每次重新拼字符串是纯浪费。
	key string
}

// Key 该绑定的稳定标识（Transport 缓存键）：出口不同 = 连接池必须隔离。
func (b Binding) Key() string {
	if b.key != "" {
		return b.key
	}
	return b.computeKey()
}

// computeKey 实际计算缓存键（Bind 内部调用一次，外部不应再调）。
func (b Binding) computeKey() string {
	if b.Proxy != nil {
		// 口令不进缓存键：同一 host:port+用户名即同一出口，避免键里带密码。
		host := b.Proxy.Host
		if b.Proxy.User != nil {
			return "proxy|" + b.Proxy.Scheme + "|" + host + "|" + b.Proxy.User.Username()
		}
		return "proxy|" + b.Proxy.Scheme + "|" + host
	}
	if b.LocalIP != nil {
		return "local|" + b.LocalIP.String()
	}
	return "direct"
}

// Resolver 出口策略解析器。并发安全。
type Resolver struct {
	cfg Config

	// localIPs 启动时冻结的本机地址快照。排序保证进程重启后 hash 分配不漂移
	//（net.Interfaces 的枚举顺序在部分平台不保证稳定）。
	localIPs []net.IP
	localErr error

	mu   sync.Mutex
	fail map[string]*failState
}

type failState struct {
	consecutive int
	until       time.Time
}

// New 构造解析器并规范化默认值。local 模式的地址发现在此处完成一次。
func New(cfg Config) *Resolver {
	if cfg.GroupBy == "" {
		cfg.GroupBy = GroupByAccount
	}
	if cfg.IdentitySource == "" {
		cfg.IdentitySource = "uid"
	}
	if cfg.FailThreshold <= 0 {
		cfg.FailThreshold = defaultFailThreshold
	}
	if cfg.FailCooldown <= 0 {
		cfg.FailCooldown = defaultFailCooldown
	}
	if cfg.MaxTransports <= 0 {
		cfg.MaxTransports = defaultMaxTransports
	}
	if cfg.Pool.KeyedPort <= 0 {
		cfg.Pool.KeyedPort = defaultKeyedPort
	}
	if cfg.Pool.PinBasePort <= 0 {
		cfg.Pool.PinBasePort = defaultPinBasePort
	}
	if cfg.Pool.PinSlots <= 0 {
		cfg.Pool.PinSlots = defaultPinSlots
	}
	r := &Resolver{cfg: cfg, fail: map[string]*failState{}}
	if cfg.Mode == ModeLocal {
		r.localIPs, r.localErr = resolveLocalIPs(cfg.Local)
	}
	return r
}

// Mode 返回生效模式（未配置时 direct）。
func (r *Resolver) Mode() Mode {
	if r == nil {
		return ModeDirect
	}
	return r.cfg.Mode
}

// Enabled 是否启用出口隔离。
func (r *Resolver) Enabled() bool {
	if r == nil {
		return false
	}
	switch r.cfg.Mode {
	case ModeLocal:
		return len(r.localIPs) > 0
	case ModeStatic:
		return strings.TrimSpace(r.cfg.StaticURL) != ""
	case ModePoolKeyed, ModePoolPin:
		return strings.TrimSpace(r.cfg.Pool.Host) != ""
	default:
		return false
	}
}

// GroupByMode 返回生效的分组维度（日志/面板展示用）。
func (r *Resolver) GroupByMode() GroupBy {
	if r == nil {
		return GroupByAccount
	}
	return r.cfg.GroupBy
}

// LocalIPs 返回本机候选出口地址（local 模式）。
func (r *Resolver) LocalIPs() []net.IP {
	if r == nil {
		return nil
	}
	out := make([]net.IP, len(r.localIPs))
	copy(out, r.localIPs)
	return out
}

// LocalError 地址发现失败的原因（Enabled 为 false 时可用于告警）。
func (r *Resolver) LocalError() error {
	if r == nil {
		return nil
	}
	return r.localErr
}

// MaxTransports 出口 Transport 缓存上限。
func (r *Resolver) MaxTransports() int {
	if r == nil {
		return defaultMaxTransports
	}
	return r.cfg.MaxTransports
}

// Group 取账号生效的分组名：config.Groups 优先，其次 auth 文件里的 group。
func (r *Resolver) Group(uid, authGroup string) string {
	if r != nil {
		if g, ok := r.cfg.Groups[uid]; ok {
			return strings.TrimSpace(g)
		}
	}
	return strings.TrimSpace(authGroup)
}

// Identity 取账号的**出口身份**（决定哈希输入，因此也决定出口）。
func (r *Resolver) Identity(uid, nickname, authGroup string) string {
	g := r.Group(uid, authGroup)
	if r != nil && r.cfg.GroupBy == GroupByGroup && g != "" {
		return "grp:" + g
	}
	src := "uid"
	if r != nil && r.cfg.IdentitySource != "" {
		src = r.cfg.IdentitySource
	}
	switch src {
	case "nickname":
		if s := strings.TrimSpace(nickname); s != "" {
			return "nick:" + s
		}
	case "group":
		if g != "" {
			return "grp:" + g
		}
	}
	return "acct:" + uid
}

// Username 取该身份在代理侧的 SOCKS5 用户名。
//
// 模板占位符：
//
//	{identity} → 带类型前缀的身份（"acct:<uid>" / "grp:<name>" / "nick:<name>"）
//	{uid}      → 纯账号 UID
//	{nick}     → 账号昵称（空则回落 UID）
//	{group}    → 出口分组名（空则回落 UID）
//	{hash}     → sha256(salt + ":" + identity).hex()[:16]
//
// 默认模板 "{identity}"。推荐写法 "acct-{uid}"：既有业务前缀，又不把
// 冒号带进 SOCKS5 用户名（用户名不能含 ':'，会与 URL 分隔符冲突）。
// 需要隐藏明文身份时改用 "{hash}"，与 proxy-pool 的 pool_client 契约逐字节一致。
func (r *Resolver) Username(uid, nickname, authGroup string) string {
	id := r.Identity(uid, nickname, authGroup)
	tpl := "{identity}"
	salt := ""
	if r != nil {
		if strings.TrimSpace(r.cfg.Pool.UsernameTemplate) != "" {
			tpl = r.cfg.Pool.UsernameTemplate
		}
		salt = r.cfg.Pool.Salt
	}
	group := ""
	if r != nil {
		group = r.Group(uid, authGroup)
	}
	nick := strings.TrimSpace(nickname)
	if nick == "" {
		nick = uid
	}
	if group == "" {
		group = uid
	}
	out := strings.ReplaceAll(tpl, "{identity}", id)
	out = strings.ReplaceAll(out, "{uid}", uid)
	out = strings.ReplaceAll(out, "{nick}", nick)
	out = strings.ReplaceAll(out, "{group}", group)
	out = strings.ReplaceAll(out, "{hash}", derive(salt, id))
	// SOCKS5 用户名不能含 ':'（与 URL 分隔符冲突，客户端契约明确禁止）。
	return strings.ReplaceAll(out, ":", "-")
}

// Slot 取 pin 模式下该身份的槽位序号。
func (r *Resolver) Slot(uid, nickname, authGroup string) int {
	slots := defaultPinSlots
	if r != nil {
		slots = r.cfg.Pool.PinSlots
	}
	if slots <= 0 {
		slots = defaultPinSlots
	}
	return int(crc32.ChecksumIEEE([]byte(r.Username(uid, nickname, authGroup))) % uint32(slots))
}

// LocalSlot 取 local 模式下该身份的地址槽位。
func (r *Resolver) LocalSlot(uid, nickname, authGroup string) int {
	n := len(r.localIPs)
	if n == 0 {
		return -1
	}
	sum := sha256.Sum256([]byte(r.Identity(uid, nickname, authGroup)))
	v := uint32(sum[0])<<24 | uint32(sum[1])<<16 | uint32(sum[2])<<8 | uint32(sum[3])
	return int(v % uint32(n))
}

// Bind 返回该账号应使用的出站绑定。
func (r *Resolver) Bind(uid, nickname, authGroup string) (Binding, error) {
	b := Binding{Identity: ""}
	if r == nil {
		return b, nil
	}
	b.Identity = r.Identity(uid, nickname, authGroup)
	b, err := r.bindFor(b, uid, nickname, authGroup)
	if err != nil {
		return b, err
	}
	b.key = b.computeKey()
	return b, nil
}

func (r *Resolver) bindFor(b Binding, uid, nickname, authGroup string) (Binding, error) {
	switch r.cfg.Mode {
	case ModeLocal:
		n := len(r.localIPs)
		if n == 0 {
			return b, ErrNoLocalIP
		}
		start := r.LocalSlot(uid, nickname, authGroup)
		// 出口冷却顺延：首选地址连续失败后，先借下一个地址顶上，避免整个账号
		// 被一个坏出口钉死。冷却解除后回到原槽位（槽位只由身份决定，不受
		// 冷却影响），所以「固定出口」的语义只在健康时被临时打破。
		//
		// 一次加锁完成全部候选检查：这里是每请求热路径，按槽位逐个调 Cooling()
		// 会在多 IP 配置下放大成 N 次锁往返。
		now := time.Now()
		r.mu.Lock()
		for i := 0; i < n; i++ {
			ip := r.localIPs[(start+i)%n]
			if !r.coolingLocked("local|"+ip.String(), now) {
				r.mu.Unlock()
				b.LocalIP = ip
				return b, nil
			}
		}
		r.mu.Unlock()
		// 全部地址都在冷却：返回首选，让调用方按普通失败处理（此时已无更好的
		// 选择，静默失败反而不如让客户端看到 5xx）。
		b.LocalIP = r.localIPs[start]
		return b, nil
	case ModeStatic:
		u, err := url.Parse(strings.TrimSpace(r.cfg.StaticURL))
		if err != nil {
			return b, fmt.Errorf("egress: parse static_url: %w", err)
		}
		if err := validateProxyScheme(u); err != nil {
			return b, err
		}
		b.Proxy = u
		return b, nil
	case ModePoolKeyed:
		u, err := r.poolURL(r.cfg.Pool.KeyedPort, uid, nickname, authGroup)
		b.Proxy = u
		return b, err
	case ModePoolPin:
		u, err := r.poolURL(r.cfg.Pool.PinBasePort+r.Slot(uid, nickname, authGroup), uid, nickname, authGroup)
		b.Proxy = u
		return b, err
	default:
		return b, nil
	}
}

func (r *Resolver) poolURL(port int, uid, nickname, authGroup string) (*url.URL, error) {
	host := strings.TrimSpace(r.cfg.Pool.Host)
	if host == "" {
		return nil, ErrNoHost
	}
	if port <= 0 {
		return nil, fmt.Errorf("egress: invalid port %d", port)
	}
	u := &url.URL{Scheme: "socks5", Host: host + ":" + strconv.Itoa(port)}
	if name := r.Username(uid, nickname, authGroup); name != "" {
		u.User = url.UserPassword(name, r.cfg.Pool.Password)
	}
	return u, nil
}

func validateProxyScheme(u *url.URL) error {
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
		if u.Host == "" {
			return fmt.Errorf("egress: proxy url has no host")
		}
		return nil
	default:
		return fmt.Errorf("egress: unsupported proxy scheme %q", u.Scheme)
	}
}

// derive 与 proxy-pool 客户端契约保持逐字节一致：
// sha256(salt + ":" + identity) 的十六进制前 16 位（= 前 8 字节）。
func derive(salt, identity string) string {
	sum := sha256.Sum256([]byte(salt + ":" + identity))
	return hex.EncodeToString(sum[:8])
}

// Describe 返回不含任何凭据的出口描述（启动日志/面板用）。
func (r *Resolver) Describe() string {
	if r == nil {
		return "direct"
	}
	switch r.cfg.Mode {
	case ModeLocal:
		return fmt.Sprintf("local[%d ip]", len(r.localIPs))
	case ModeStatic:
		if u, err := url.Parse(strings.TrimSpace(r.cfg.StaticURL)); err == nil && u.Host != "" {
			return "static[" + u.Scheme + "://" + u.Host + "]"
		}
		return "static"
	case ModePoolKeyed:
		return fmt.Sprintf("pool-keyed[%s:%d]", r.cfg.Pool.Host, r.cfg.Pool.KeyedPort)
	case ModePoolPin:
		return fmt.Sprintf("pool-pin[%s:%d+%d]", r.cfg.Pool.Host, r.cfg.Pool.PinBasePort, r.cfg.Pool.PinSlots)
	default:
		return "direct"
	}
}

// NoteResult 记录一次出口使用结果，驱动「出口级」熔断。
//
// 关键区分：这里统计的是**链路**失败，不是上游业务失败。出口挂了不代表账号
// 有问题——把链路故障喂进账号熔断，会把整池健康号逐个冷却掉。
func (r *Resolver) NoteResult(key string, ok bool) {
	if r == nil || !r.Enabled() || key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.fail[key]
	if st == nil {
		st = &failState{}
		r.fail[key] = st
	}
	if ok {
		st.consecutive = 0
		st.until = time.Time{}
		return
	}
	st.consecutive++
	if st.consecutive >= r.cfg.FailThreshold {
		st.until = time.Now().Add(r.cfg.FailCooldown)
		st.consecutive = 0
	}
}

// Cooling 该出口是否处于冷却期，以及剩余时长。
func (r *Resolver) Cooling(key string) (bool, time.Duration) {
	if r == nil || !r.Enabled() || key == "" {
		return false, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.fail[key]
	if st == nil || st.until.IsZero() {
		return false, 0
	}
	if d := st.until.Sub(time.Now()); d > 0 {
		return true, d
	}
	st.until = time.Time{}
	st.consecutive = 0
	return false, 0
}

// coolingLocked 冷却判定的无锁版本。调用方必须持有 r.mu。
// 过期标记就地清理（不另起后台 GC：这里每一次进入都是真实的「正在选这个出口」）。
func (r *Resolver) coolingLocked(key string, now time.Time) bool {
	st := r.fail[key]
	if st == nil || st.until.IsZero() {
		return false
	}
	if now.Before(st.until) {
		return true
	}
	st.until = time.Time{}
	st.consecutive = 0
	return false
}

// Snapshot 出口健康快照（面板/日志用，不含口令）。
type Snapshot struct {
	Key         string
	Consecutive int
	CoolingFor  time.Duration
}

// Snapshot 列出当前所有出口状态。
func (r *Resolver) Snapshot() []Snapshot {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	out := make([]Snapshot, 0, len(r.fail))
	for k, st := range r.fail {
		var cooling time.Duration
		if !st.until.IsZero() && now.Before(st.until) {
			cooling = st.until.Sub(now)
		}
		out = append(out, Snapshot{Key: k, Consecutive: st.consecutive, CoolingFor: cooling})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// virtualPrefixes 默认排除的虚拟网卡名前缀：这些接口上的地址不是出公网的
// 源地址，纳进来会让账号被分配到一个「连不出去」的槽位。
var virtualPrefixes = []string{
	"lo", "docker", "veth", "br-", "virbr", "vmnet", "vboxnet",
	"utun", "tun", "tap", "wg", "zt", "tailscale", "cni", "flannel", "kube",
}

// resolveLocalIPs 枚举本机候选出口地址。
//
// 规则：
//   - 显式配置 IPs 时只校验可用性，不做发现（部署机地址固定时更可控）；
//   - 自动发现时遍历网卡，跳过 down/loopback/虚拟网卡，保留全局单播地址；
//   - 结果**排序**：接口枚举顺序在部分平台不稳定，不排序会让同一账号在重启
//     后换出口，直接破坏「固定出口」的承诺。
func resolveLocalIPs(cfg Local) ([]net.IP, error) {
	if len(cfg.IPs) > 0 {
		out := make([]net.IP, 0, len(cfg.IPs))
		for _, s := range cfg.IPs {
			ip := net.ParseIP(strings.TrimSpace(s))
			if ip == nil {
				return nil, fmt.Errorf("egress: invalid local ip %q", s)
			}
			out = append(out, ip)
		}
		out = dedupeIPs(out)
		sortIPs(out)
		return out, nil
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("egress: list interfaces: %w", err)
	}
	only := map[string]bool{}
	for _, n := range cfg.Interfaces {
		only[strings.TrimSpace(n)] = true
	}
	exclude := append([]string{}, virtualPrefixes...)
	exclude = append(exclude, cfg.ExcludePrefixes...)

	var ips []net.IP
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(only) > 0 && !only[iface.Name] {
			continue
		}
		if excludedInterface(iface.Name, exclude) {
			continue
		}
		addrs, aerr := iface.Addrs()
		if aerr != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, perr := net.ParseCIDR(addr.String())
			if perr != nil || ip == nil {
				continue
			}
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
				continue
			}
			if ip.To4() == nil && !cfg.IncludeIPv6 {
				continue
			}
			ips = append(ips, ip)
		}
	}
	ips = dedupeIPs(ips)
	sortIPs(ips)
	if len(ips) == 0 {
		return nil, ErrNoLocalIP
	}
	return ips, nil
}

func excludedInterface(name string, prefixes []string) bool {
	lower := strings.ToLower(name)
	for _, p := range prefixes {
		if p == "" {
			continue
		}
		if strings.HasPrefix(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func dedupeIPs(in []net.IP) []net.IP {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, ip := range in {
		k := ip.String()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, ip)
	}
	return out
}

func sortIPs(ips []net.IP) {
	sort.Slice(ips, func(i, j int) bool { return ips[i].String() < ips[j].String() })
}
