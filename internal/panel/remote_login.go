// remote_login.go 面板内嵌「远程浏览器登录」：把服务器上的 Chromium 画面投到面板,
// 让人在服务器 IP 上完成 OAuth 授权（短信、滑块、扫码都能过）。
//
// 为什么需要它：设备流本身只要求「某个浏览器完成登录」，但当账号有 IP 风控时，
// 用自己的浏览器登录会把 token 绑到另一个出口 IP。远程浏览器解决的是这个，
// 不是「拿不到 token」—— token 仍由 loginPoll 从 state 取回，落盘/进池/签到
// 全部复用既有链路，本文件不碰凭据。
//
// 架构：sidecar（deploy/login-remote，playwright + chromium）只认一个 URL，
// 不认站点、不抓凭据；本文件负责发起设备流、下发一次性 WS ticket，
// 并把浏览器的 WebSocket 反向代理到 sidecar —— 浏览器永远不直连 sidecar。
package panel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/linguo2625469/workbuddy2api-panel/internal/egress"
)

// remoteTicketTTL WS ticket 有效期：够前端建连即可，越短越好。
const remoteTicketTTL = 60 * time.Second

// remoteTicket 一次性 WS 凭据。浏览器无法给 WebSocket 加 Authorization 头,
// 所以用「先 Bearer 换 ticket，再拿 ticket 连 WS」的方式，避免 api_key 进 URL。
type remoteTicket struct {
	sidecar string // sidecar 侧的 ticket
	state   string // 绑定的设备流 state（poll 用）
	exp     time.Time
}

// sidecarSession 是 sidecar POST /session 的响应。
type sidecarSession struct {
	ID     string `json:"id"`
	Ticket string `json:"ticket"`
	State  string `json:"state"`
	Error  string `json:"error"`
	Meta   struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"meta"`
}

// sidecarPost 向 sidecar 发一次 JSON POST。base 已去掉尾部斜杠。
func sidecarPost(base, token, path string, body any, timeout time.Duration, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Remote-Token", token)
	}
	c := &http.Client{Timeout: timeout}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("sidecar %d: %s", resp.StatusCode, e.Error)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// loginRemoteCfg 返回 sidecar 地址与共享 token；URL 为空 = 未启用。
func (p *Panel) loginRemoteCfg() (base, token string, ok bool) {
	base = strings.TrimRight(strings.TrimSpace(p.cfg.LoginRemoteURL), "/")
	if base == "" {
		return "", "", false
	}
	return base, p.cfg.LoginRemoteToken, true
}

// issueRemoteTicket 签发一次性 WS ticket。
func (p *Panel) issueRemoteTicket(state, sidecarTicket string) string {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	t := base64.RawURLEncoding.EncodeToString(buf)
	now := time.Now()
	p.remoteMu.Lock()
	if p.remoteTickets == nil {
		p.remoteTickets = map[string]remoteTicket{}
	}
	for k, v := range p.remoteTickets {
		if now.After(v.exp) {
			delete(p.remoteTickets, k)
		}
	}
	p.remoteTickets[t] = remoteTicket{sidecar: sidecarTicket, state: state, exp: now.Add(remoteTicketTTL)}
	p.remoteMu.Unlock()
	return t
}

// consumeRemoteTicket 校验并销毁（一次性）。断线重连走 /remote/ticket 换新的。
func (p *Panel) consumeRemoteTicket(t string) (state, sidecarTicket string, ok bool) {
	p.remoteMu.Lock()
	defer p.remoteMu.Unlock()
	v, found := p.remoteTickets[t]
	if !found {
		return "", "", false
	}
	delete(p.remoteTickets, t)
	if time.Now().After(v.exp) {
		return "", "", false
	}
	return v.state, v.sidecar, true
}

// egressIPFor 返回该账号在 local 模式下的**槽位**出口地址。
//
// 刻意不用 Resolver.Bind：Bind 会在出口冷却时顺延到下一个地址，而重登要的正是
// 「固定出口」语义——槽位只由身份决定，不受冷却影响。非 local / 未启用返回空。
func (p *Panel) egressIPFor(uid, nickname, group string) string {
	r := p.cfg.Egress
	if r == nil || r.Mode() != egress.ModeLocal {
		return ""
	}
	// 租约优先：登录即固定的号，其「应有出口」就是租约地址，而不是哈希结果。
	if ip, ok := r.LeaseOf(uid); ok {
		return ip.String()
	}
	ips := r.LocalIPs()
	slot := r.LocalSlot(uid, nickname, group)
	if slot < 0 || slot >= len(ips) {
		return ""
	}
	return ips[slot].String()
}

// loginRemoteStatus 面板据此决定「在服务器浏览器中登录」入口是否出现。
// 只报配置态，不触发任何上游/浏览器动作。
func (p *Panel) loginRemoteStatus(w http.ResponseWriter, r *http.Request) {
	_, _, ok := p.loginRemoteCfg()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": ok})
}

// loginRemoteStart 发起「服务器浏览器」登录：
//  1. 复用设备流拿授权链接（state 进 logins，poll 可用）
//  2. 让 sidecar 打开该链接
//  3. 下发一次性 WS ticket 给前端
func (p *Panel) loginRemoteStart(w http.ResponseWriter, r *http.Request) {
	base, token, ok := p.loginRemoteCfg()
	if !ok {
		writeErr(w, http.StatusNotImplemented, "远程浏览器未启用：WB2API_LOGIN_REMOTE_URL 未配置")
		return
	}
	// uid 非空 = 重登已有账号：用它入池后的槽位出口出站，避免「重登换 IP」。
	// 新号登录时 uid 未知，一致性在登录前无法成立，只能事后在结果里提示槽位。
	var req struct {
		Realm string `json:"realm"`
		UID   string `json:"uid"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&req)
	}
	realm := "cn"
	if req.Realm == "global" {
		realm = "global"
	}
	sourceIP := ""
	if req.UID != "" {
		a := p.cfg.Pool.AuthByUID(req.UID)
		if a == nil {
			writeErr(w, http.StatusNotFound, "账号不存在（uid="+req.UID+"）")
			return
		}
		sourceIP = p.egressIPFor(a.UID, a.Nickname, a.Group())
		if sourceIP == "" {
			writeErr(w, http.StatusBadRequest, "该账号无法确定槽位出口（egress 非 local 或本机地址为空）")
			return
		}
	} else {
		// 新号：uid 未知、算不出槽位，按 IP 顺序轮转分摊。目的不是让单个号对齐，
		// 而是避免一批新号全从宿主机默认出口进来（那是更明显的特征）。
		sourceIP = p.nextLoginIP()
	}
	state, authURL, err := p.startDeviceAuth(realm)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// 记在会话上：poll 拿它和入池槽位对比，不一致就提醒。
	if sourceIP != "" {
		p.loginMu.Lock()
		if sess, ok := p.logins[state]; ok {
			sess.sourceIP = sourceIP
			p.logins[state] = sess
		}
		p.loginMu.Unlock()
	}
	var ss sidecarSession
	payload := map[string]string{"url": authURL}
	if sourceIP != "" {
		payload["source_ip"] = sourceIP
	}
	// sidecar 内部最多等 45s 页面就绪，这里给 70s 兜底
	if err := sidecarPost(base, token, "/session", payload, 70*time.Second, &ss); err != nil {
		// 别留一个永远没人登录的 state 占着 TTL
		p.dropLogin(state)
		writeErr(w, http.StatusBadGateway, "远程浏览器启动失败："+err.Error())
		return
	}
	wt := p.issueRemoteTicket(state, ss.Ticket)
	log.Printf("panel: 远程浏览器登录就绪 realm=%s session=%s 出口=%s（state=%s...）",
		realm, ss.ID, sourceIP, state[:min(8, len(state))])
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"state":   state,
		"realm":   realm,
		"session": ss.ID,
		"ws":      "/panel/api/login/remote/ws?ticket=" + url.QueryEscape(wt),
		"meta":    ss.Meta,
		// 空 = 走默认出口（新号）；非空 = 该账号的槽位出口（重登）
		"source_ip": sourceIP,
	})
}

// loginRemoteTicket 断线重连：凭已知 state 向 sidecar 换新 ticket，再签我们自己的。
// 页面不会因此重建（sidecar 保留 90 秒），刷新面板仍能接上同一屏。
func (p *Panel) loginRemoteTicket(w http.ResponseWriter, r *http.Request) {
	base, token, ok := p.loginRemoteCfg()
	if !ok {
		writeErr(w, http.StatusNotImplemented, "远程浏览器未启用")
		return
	}
	var body struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil || body.State == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	p.loginMu.Lock()
	_, known := p.logins[body.State]
	p.loginMu.Unlock()
	if !known {
		writeErr(w, http.StatusNotFound, "unknown or expired state（请重新发起添加账号）")
		return
	}
	var out struct {
		Ticket string `json:"ticket"`
	}
	if err := sidecarPost(base, token, "/session/ticket", nil, 10*time.Second, &out); err != nil {
		writeErr(w, http.StatusBadGateway, "远程浏览器会话已结束："+err.Error())
		return
	}
	wt := p.issueRemoteTicket(body.State, out.Ticket)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"ws": "/panel/api/login/remote/ws?ticket=" + url.QueryEscape(wt),
	})
}

// loginRemoteStop 关闭 sidecar 会话（用户点「结束远程」或关闭弹窗）。
func (p *Panel) loginRemoteStop(w http.ResponseWriter, r *http.Request) {
	base, token, ok := p.loginRemoteCfg()
	if !ok {
		writeErr(w, http.StatusNotImplemented, "远程浏览器未启用")
		return
	}
	if err := sidecarPost(base, token, "/session/close", nil, 10*time.Second, nil); err != nil {
		// 已经关掉也算成功：目标状态一致
		log.Printf("panel: 关闭远程浏览器会话：%v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// loginRemoteWS 把浏览器 WS 原样转发到 sidecar。
//
// 不直连 sidecar 的原因：sidecar 只在 compose 内网、且入口层要求所有运维面
// 走面板（记忆红线：/panel 只经 SSH 隧道）。ticket 一次性 + 60s，即使进了
// nginx 访问日志也已失效。
func (p *Panel) loginRemoteWS(w http.ResponseWriter, r *http.Request) {
	base, token, ok := p.loginRemoteCfg()
	if !ok {
		writeErr(w, http.StatusNotImplemented, "远程浏览器未启用")
		return
	}
	state, scTicket, ok := p.consumeRemoteTicket(r.URL.Query().Get("ticket"))
	if !ok {
		writeErr(w, http.StatusUnauthorized, "invalid or expired ticket")
		return
	}
	_ = state // 已用于绑定；转发本身不需要它

	// Origin 放宽：ticket 由 Bearer 鉴权后的 start 下发且一次性，第三方无法预知。
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		log.Printf("panel: 远程浏览器 WS 升级失败：%v", err)
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(1 << 20) // 客户端只有 JSON 命令

	wsBase := base
	if strings.HasPrefix(wsBase, "https://") {
		wsBase = "wss://" + strings.TrimPrefix(wsBase, "https://")
	} else {
		wsBase = "ws://" + strings.TrimPrefix(wsBase, "http://")
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	up, _, err := websocket.Dial(dctx, wsBase+"/ws?ticket="+url.QueryEscape(scTicket),
		&websocket.DialOptions{HTTPHeader: http.Header{"X-Remote-Token": {token}}})
	dcancel()
	if err != nil {
		_ = c.Close(websocket.StatusInternalError, "remote browser unreachable")
		log.Printf("panel: 连接 sidecar 失败：%v", err)
		return
	}
	defer up.CloseNow()
	up.SetReadLimit(8 << 20) // JPEG 帧，32KB 默认上限必然截断

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if err := up.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}()
	for {
		typ, data, err := up.Read(ctx)
		if err != nil {
			return
		}
		if err := c.Write(ctx, typ, data); err != nil {
			return
		}
	}
}
