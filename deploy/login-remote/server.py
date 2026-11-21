# -*- coding: utf-8 -*-
"""远程浏览器 sidecar：把服务端 Chromium 的画面与输入搬进 workbuddy2api 面板。

职责边界（刻意收窄，不要扩展）：
  - 只认「一个 URL」。不认站点、不解析凭据、不碰账号库、不读 profile。
  - 为什么不需要抓凭据：WorkBuddy 是 OAuth 设备流，浏览器只要在服务器 IP 上把
    授权链接登录完成，token 就挂在 state 上，由 Go 侧的 /panel/api/login/poll
    取回、落盘、进池。本服务只负责「让人看见并操作」。
  - 站点差异为零 ⇒ 以后换任何站点都只改一个 URL，不改这里。

CDP 链路（参考 login-adapter/login_kit/remote.py 已验证的契约）：
    Page.startScreencast  → JPEG 帧（WS 二进制）
    Input.dispatch*       → 鼠标/键盘原样回放

协议：
    POST /session            {"url": "..."}          → {"id","ticket","meta","state"}
    POST /session/ticket                              → {"ticket"}（断线重连用）
    POST /session/close                               → {"ok": true}
    GET  /ws?ticket=<t>      WS：服务端→JSON 事件 + 二进制 JPEG
                                  客户端→JSON 命令
    GET  /healthz

鉴权：所有 HTTP 与 WS 之外的入口要求 `X-Remote-Token: <REMOTE_TOKEN>`。
      WS 用一次性 ticket（60 秒），因为浏览器无法自定义 WebSocket 头。
"""
from __future__ import annotations

import asyncio
import base64
import ipaddress
import json
import os
import secrets
import socket
import time
import uuid

from aiohttp import WSMsgType, web
from playwright.async_api import async_playwright

VIEWPORT = {"width": 1280, "height": 800}
SESSION_TIMEOUT = 600       # 单会话硬上限 10 分钟
DETACH_GRACE = 90           # 页面关闭后保留 90 秒，允许刷新/重连
TICKET_TTL = 60.0
LIVE_WAIT = 45.0            # 建会话时等待页面就绪的上限
SHARED_TOKEN = os.environ.get("REMOTE_TOKEN", "")
PORT = int(os.environ.get("PORT", "8099"))
# host 网络部署时务必设 127.0.0.1：本服务能开浏览器，不该被内网其他机器直接调用。
HOST = os.environ.get("HOST", "0.0.0.0")
LOGIN_HOME = os.environ.get("LOGIN_HOME", "https://www.codebuddy.cn")

_TICKETS: dict[str, float] = {}


def issue_ticket() -> str:
    now = time.time()
    for k, exp in list(_TICKETS.items()):
        if exp < now:
            _TICKETS.pop(k, None)
    t = secrets.token_urlsafe(24)
    _TICKETS[t] = now + TICKET_TTL
    return t


def verify_ticket(t: str) -> bool:
    """一次性：验过即销毁。"""
    exp = _TICKETS.pop(t or "", 0)
    return bool(exp) and exp >= time.time()


def log(msg: str) -> None:
    print(f"[login-remote] {time.strftime('%H:%M:%S')} {msg}", flush=True)


class SourceProxy:
    """极简 HTTP CONNECT 代理：把每条出站连接绑定到指定源地址。

    为什么需要它：Chromium 没有 --bind-address，源地址只能由内核在 connect 时
    决定。主服务的 egress.mode=local 是在 Go 的 net.Dialer 里绑，浏览器拿不到；
    把出站统一收进这个代理，就能让「登录绑定的 IP」等于「账号入池后的槽位 IP」。

    只支持 CONNECT（HTTPS）。登录页全走 HTTPS；明文 http 一律 405，不做转发。
    """

    def __init__(self, source_ip: str):
        self.source_ip = source_ip
        self.server: asyncio.AbstractServer | None = None
        self.port = 0
        # 计数是「浏览器真的走了这个出口」的唯一直接证据（本地/服务端都无法
        # 从页面内容区分是否绕过代理），healthz 暴露给运维对账。
        self.conns = 0    # 成功建立的隧道（浏览器确实走了这个出口）
        self.drops = 0    # 目标不可达/超时（被墙、对端拒绝）——与出口无关
        self.fails = 0    # 源地址不可用（EADDRNOTAVAIL）——这才是出口故障

    async def start(self) -> None:
        self.server = await asyncio.start_server(self._handle, "127.0.0.1", 0)
        self.port = self.server.sockets[0].getsockname()[1]
        log(f"源地址代理 127.0.0.1:{self.port} → {self.source_ip}")

    async def stop(self) -> None:
        if self.server is not None:
            self.server.close()
            try:
                await self.server.wait_closed()
            except Exception:  # noqa: BLE001
                pass
            self.server = None

    @staticmethod
    async def _pipe(src: asyncio.StreamReader, dst: asyncio.StreamWriter) -> None:
        try:
            while True:
                data = await src.read(65536)
                if not data:
                    break
                dst.write(data)
                await dst.drain()
        except Exception:  # noqa: BLE001
            pass
        finally:
            try:
                dst.close()
            except Exception:  # noqa: BLE001
                pass

    async def _handle(self, reader: asyncio.StreamReader,
                      writer: asyncio.StreamWriter) -> None:
        up_writer = None
        try:
            head = await asyncio.wait_for(reader.readline(), timeout=15)
            parts = head.decode("latin1").split()
            if len(parts) < 2 or parts[0].upper() != "CONNECT":
                writer.write(b"HTTP/1.1 405 Method Not Allowed\r\n\r\n")
                await writer.drain()
                return
            host, _, port = parts[1].rpartition(":")
            if not host:
                host, port = parts[1], "443"
            while True:  # 丢掉剩余请求头
                line = await asyncio.wait_for(reader.readline(), timeout=15)
                if line in (b"\r\n", b"\n", b""):
                    break
            # 本机目标直接拒绝，且不计入任何失败计数：这不是出口故障。
            #
            # 注意 `localhost.weixin.qq.com:14013` 这类**公网域名**：微信侧故意把它
            # 解析到环回供客户端探测本地回调。host 是域名（不是 127.0.0.1），
            # 所以必须按域名前缀判断，否则代理会替浏览器去连 127.0.0.1 而报 refused。
            if (host.startswith("127.") or host == "localhost"
                    or host == "::1" or host.startswith("[::1")
                    or host.startswith("localhost.")):
                writer.write(b"HTTP/1.1 403 Forbidden\r\n\r\n")
                await writer.drain()
                return
            # 连接超时必须有：页面上的外部资源（广告、被墙域名）永远连不通，
            # 挂起会把 browser.close() 拖到几十秒（实测 52s），进而卡死新建会话。
            try:
                up_reader, up_writer = await asyncio.wait_for(
                    asyncio.open_connection(host, int(port),
                                            local_addr=(self.source_ip, 0)),
                    timeout=8)
            except OSError as e:
                import errno as _errno
                if e.errno == _errno.EADDRNOTAVAIL:
                    self.fails += 1   # 源地址不可用 = 真正的出口故障
                else:
                    self.drops += 1   # 目标侧问题，与出口无关
                raise
            writer.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            await writer.drain()
            self.conns += 1
            await asyncio.gather(self._pipe(reader, up_writer),
                                 self._pipe(up_reader, writer))
        except Exception as e:  # noqa: BLE001
            # 计数已在具体分支里记过；这里只记录不可达/异常，不再重复加 fails
            if not isinstance(e, OSError):
                self.drops += 1
            log(f"代理连接失败 {self.source_ip}: {type(e).__name__}: {e}")
            try:
                writer.write(b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
                await writer.drain()
            except Exception:  # noqa: BLE001
                pass
        finally:
            if up_writer is not None:
                try:
                    up_writer.close()
                except Exception:  # noqa: BLE001
                    pass
            try:
                writer.close()
            except Exception:  # noqa: BLE001
                pass


class Session:
    def __init__(self, url: str, source_ip: str = ""):
        self.id = uuid.uuid4().hex[:10]
        self.url = url
        # 空 = 走宿主机默认出口；非空 = 经 SourceProxy 绑定该源地址（重登老号时用）
        self.source_ip = source_ip
        self.proxy: SourceProxy | None = None
        self.state = "starting"          # starting|live|closed|error
        self.error = ""
        self.created = time.time()
        self.last_detach = 0.0
        self.meta = dict(VIEWPORT)
        self.frames: asyncio.Queue = asyncio.Queue(maxsize=2)
        self.cmds: asyncio.Queue = asyncio.Queue()
        self.stop = asyncio.Event()
        self.viewer: web.WebSocketResponse | None = None
        self.page = None
        self.cdp = None
        self.task: asyncio.Task | None = None
        self.events: list[dict] = []     # 建会话早期的错误，等 WS 连上再补发

    # ------------------------------------------------------------ 生命周期
    def start(self) -> None:
        self.task = asyncio.create_task(self._run(), name=f"remote-{self.id}")

    async def close(self) -> None:
        """释放浏览器。**必须有上限**：页面上的挂起连接会让 browser.close()
        等上几十秒，而调用点（新建会话/HTTP close）都在锁内，一卡就是全站卡。

        阶梯：停循环 → 关页面（2s，断开挂起连接）→ 等任务收尾（3s）→ 取消（2s）。
        """
        self.stop.set()
        if self.page is not None:
            try:
                await asyncio.wait_for(self.page.close(run_before_unload=False), timeout=2)
            except Exception:  # noqa: BLE001
                pass
        task = self.task
        if task and not task.done():
            try:
                await asyncio.wait_for(asyncio.shield(task), timeout=3)
            except Exception:  # noqa: BLE001
                task.cancel()
                try:
                    await asyncio.wait_for(task, timeout=2)
                except Exception:  # noqa: BLE001
                    pass
        self.state = "closed"

    async def wait_ready(self) -> None:
        deadline = time.time() + LIVE_WAIT
        while time.time() < deadline:
            if self.state in ("live", "error", "closed"):
                return
            await asyncio.sleep(0.1)

    # -------------------------------------------------------------- 主循环
    async def _run(self) -> None:
        try:
            async with async_playwright() as p:
                launch_args = [
                    "--no-sandbox",
                    "--disable-dev-shm-usage",
                    "--disable-blink-features=AutomationControlled",
                    "--hide-scrollbars",
                    "--mute-audio",
                ]
                if self.source_ip:
                    # 源地址不可用时在这里就失败（open_connection 会 bind 失败），
                    # 不静默回落到默认出口 —— 那正是这条功能要避免的事。
                    self.proxy = SourceProxy(self.source_ip)
                    await self.proxy.start()
                    launch_args.append(
                        f"--proxy-server=http://127.0.0.1:{self.proxy.port}")
                browser = await p.chromium.launch(headless=True, args=launch_args)
                try:
                    # UA 版本跟随实际内核，避免版本号对不上反而成为特征
                    ver = browser.version or "141.0.0.0"
                    ua = ("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
                          f"(KHTML, like Gecko) Chrome/{ver} Safari/537.36")
                    ctx = await browser.new_context(
                        viewport=dict(VIEWPORT), locale="zh-CN",
                        timezone_id="Asia/Shanghai", user_agent=ua)
                    # 去掉最明显的自动化标志（人工仍需看屏幕操作，不绕过验证码）
                    await ctx.add_init_script(
                        "Object.defineProperty(navigator,'webdriver',{get:()=>undefined})")
                    page = await ctx.new_page()
                    self.page = page
                    await page.goto(self.url, wait_until="domcontentloaded",
                                    timeout=60000)
                    cdp = await ctx.new_cdp_session(page)
                    self.cdp = cdp
                    cdp.on("Page.screencastFrame", self._on_frame)
                    await cdp.send("Page.startScreencast", {
                        # 登录页不是视频：降质量 + 隔帧，WAN 下带宽从 ~20Mbps 降到 ~5Mbps
                        "format": "jpeg", "quality": 55,
                        "maxWidth": VIEWPORT["width"],
                        "maxHeight": VIEWPORT["height"],
                        "everyNthFrame": 2,
                    })
                    self.state = "live"
                    log(f"会话 {self.id} live url={self.url} "
                        f"source={self.source_ip or 'default'}")
                    # WS 可能在 starting 阶段就连上了（首屏慢时主服务会先返回 ticket），
                    # 这里补一次状态，否则前端文案会一直停在 starting。
                    await self._notify({"type": "state", "state": "live",
                                        "meta": dict(self.meta)})
                    await self._loop()
                finally:
                    try:
                        await asyncio.wait_for(browser.close(), timeout=5)
                    except Exception:  # noqa: BLE001
                        pass
                    if self.proxy is not None:
                        await self.proxy.stop()
                        self.proxy = None
                    if self.state != "error":
                        self.state = "closed"
                    log(f"会话 {self.id} {self.state}")
        except asyncio.CancelledError:
            self.state = "closed"
            raise
        except Exception as e:  # noqa: BLE001
            self.state = "error"
            self.error = f"{type(e).__name__}: {e}"
            self.events.append({"type": "error", "message": self.error})
            log(f"会话 {self.id} 失败：{self.error}")

    def _on_frame(self, frame: dict) -> None:
        """CDP 回调（同 loop、同步）：只做 ack 与入队，绝不 await。"""
        try:
            sid = frame.get("sessionId")
            if self.cdp is not None and sid:
                asyncio.get_running_loop().create_task(
                    self.cdp.send("Page.screencastFrameAck", {"sessionId": sid}))
            jpeg = base64.b64decode(frame.get("data") or "")
            if not jpeg:
                return
            meta = frame.get("metadata") or {}
            w, h = meta.get("deviceWidth"), meta.get("deviceHeight")
            if isinstance(w, int) and isinstance(h, int) and w > 0 and h > 0:
                self.meta = {"width": w, "height": h}
            # 队列只留最新帧：宁可丢帧也不让画面越拖越久
            while True:
                try:
                    self.frames.get_nowait()
                except asyncio.QueueEmpty:
                    break
            self.frames.put_nowait(jpeg)
        except Exception:  # noqa: BLE001
            pass

    async def _notify(self, ev: dict) -> None:
        """有观看者就直发，没有就攒着等连上补发。"""
        if self.viewer is not None:
            try:
                await self.viewer.send_json(ev)
                return
            except Exception:  # noqa: BLE001
                pass
        self.events.append(ev)

    async def _loop(self) -> None:
        while not self.stop.is_set():
            if time.time() - self.created > SESSION_TIMEOUT:
                self.events.append({"type": "error",
                                    "message": "远程会话超时（10 分钟），请重新发起"})
                self.stop.set()
                break
            if (self.viewer is None and self.last_detach
                    and time.time() - self.last_detach > DETACH_GRACE):
                log(f"会话 {self.id} 无人连接超过 {DETACH_GRACE}s，回收")
                break
            try:
                cmd = await asyncio.wait_for(self.cmds.get(), timeout=0.5)
            except asyncio.TimeoutError:
                continue
            try:
                await self._handle(cmd)
            except Exception as e:  # noqa: BLE001
                self.events.append({"type": "error",
                                    "message": f"操作失败：{type(e).__name__}: {e}"})

    # ---------------------------------------------------------------- 输入
    async def _handle(self, cmd: dict) -> None:
        cdp, t = self.cdp, cmd.get("t")
        if cdp is None:
            return
        if t == "mouse":
            await cdp.send("Input.dispatchMouseEvent", {
                "type": cmd.get("type", "mouseMoved"),
                "x": float(cmd.get("x", 0)), "y": float(cmd.get("y", 0)),
                "button": cmd.get("button", "none"),
                "buttons": int(cmd.get("buttons", 0)),
                "clickCount": int(cmd.get("clickCount", 0)),
            })
        elif t == "wheel":
            await cdp.send("Input.dispatchMouseEvent", {
                "type": "mouseWheel",
                "x": float(cmd.get("x", 0)), "y": float(cmd.get("y", 0)),
                "deltaX": float(cmd.get("deltaX", 0)),
                "deltaY": float(cmd.get("deltaY", 0)),
            })
        elif t == "text":
            text = str(cmd.get("text") or "")
            if text:
                await cdp.send("Input.insertText", {"text": text})
        elif t == "key":
            await cdp.send("Input.dispatchKeyEvent", {
                "type": cmd.get("type", "keyDown"),
                "key": cmd.get("key", ""), "code": cmd.get("code", ""),
                # 缺 keyCode 时 CDP 不会触发默认行为（回车/退格），兜底给 0
                "windowsVirtualKeyCode": int(cmd.get("keyCode", 0) or 0),
                "nativeVirtualKeyCode": int(cmd.get("keyCode", 0) or 0),
            })
        elif t == "back" and self.page:
            await self.page.go_back()
        elif t == "reload" and self.page:
            await self.page.reload()
        elif t == "home" and self.page:
            await self.page.goto(self.url, wait_until="domcontentloaded")
        elif t == "stop":
            self.stop.set()

    # ------------------------------------------------------------ 帧推送
    async def pump(self, ws: web.WebSocketResponse) -> None:
        while not ws.closed:
            jpeg = await self.frames.get()
            await ws.send_bytes(jpeg)


SESSION: Session | None = None
_LOCK = asyncio.Lock()


def _auth(request: web.Request) -> bool:
    if not SHARED_TOKEN:
        return True
    return request.headers.get("X-Remote-Token", "") == SHARED_TOKEN


async def handle_create(request: web.Request) -> web.Response:
    if not _auth(request):
        return web.json_response({"error": "invalid_token"}, status=401)
    try:
        body = await request.json()
    except Exception:  # noqa: BLE001
        return web.json_response({"error": "invalid_json"}, status=400)
    url = str(body.get("url") or "").strip()
    if not url.startswith(("http://", "https://")):
        return web.json_response({"error": "url must be http(s)"}, status=400)

    source_ip = str(body.get("source_ip") or "").strip()
    if source_ip:
        try:
            ip = ipaddress.ip_address(source_ip)
        except ValueError:
            return web.json_response({"error": "invalid source_ip"}, status=400)
        if ip.version != 4:
            return web.json_response({"error": "source_ip must be IPv4"}, status=400)
        # 先裸 bind 一次：地址不属于本机时立刻报错，别等浏览器连出去才发现
        probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            probe.bind((source_ip, 0))
        except OSError as e:
            return web.json_response(
                {"error": f"source_ip 不可用（{source_ip}）：{e}"}, status=400)
        finally:
            probe.close()
        source_ip = str(ip)

    global SESSION
    async with _LOCK:
        if SESSION is not None:
            # 铁律：一次只开一个会话，旧的一律关掉（多开会互相覆盖登录态）
            await SESSION.close()
        SESSION = Session(url, source_ip=source_ip)
        SESSION.start()
    await SESSION.wait_ready()
    if SESSION.state == "error":
        return web.json_response({"error": SESSION.error}, status=502)
    if SESSION.state != "live":
        # 页面慢（首屏导航未完成）也先把会话交出去：画面到了自然会推送
        log(f"会话 {SESSION.id} 仍未 live（{SESSION.state}），先返回 ticket")
    return web.json_response({
        "id": SESSION.id, "ticket": issue_ticket(),
        "meta": SESSION.meta, "state": SESSION.state,
        "source_ip": SESSION.source_ip,
    })


async def handle_ticket(request: web.Request) -> web.Response:
    if not _auth(request):
        return web.json_response({"error": "invalid_token"}, status=401)
    if SESSION is None or SESSION.state in ("closed", "error"):
        return web.json_response({"error": "no_session"}, status=404)
    return web.json_response({"ticket": issue_ticket(), "state": SESSION.state})


async def handle_close(request: web.Request) -> web.Response:
    if not _auth(request):
        return web.json_response({"error": "invalid_token"}, status=401)
    global SESSION
    async with _LOCK:
        if SESSION is not None:
            await SESSION.close()
            SESSION = None
    return web.json_response({"ok": True})


async def handle_ws(request: web.Request) -> web.WebSocketResponse:
    if not verify_ticket(request.query.get("ticket", "")):
        raise web.HTTPUnauthorized(text="invalid ticket")
    s = SESSION
    if s is None or s.state in ("closed", "error"):
        raise web.HTTPGone(text="no session")

    ws = web.WebSocketResponse(heartbeat=30, max_msg_size=1 << 20)
    await ws.prepare(request)
    s.viewer, s.last_detach = ws, 0.0
    log(f"会话 {s.id} 面板已连接")

    await ws.send_json({"type": "state", "state": s.state, "meta": s.meta})
    for ev in s.events:
        await ws.send_json(ev)
    s.events.clear()

    sender = asyncio.create_task(s.pump(ws))
    try:
        async for msg in ws:
            if msg.type == WSMsgType.TEXT:
                try:
                    cmd = json.loads(msg.data)
                except Exception:  # noqa: BLE001
                    continue
                if isinstance(cmd, dict):
                    s.cmds.put_nowait(cmd)
            elif msg.type in (WSMsgType.ERROR, WSMsgType.CLOSE):
                break
    finally:
        sender.cancel()
        if s.viewer is ws:
            s.viewer = None
            s.last_detach = time.time()
        log(f"会话 {s.id} 面板已断开（{DETACH_GRACE}s 内可重连）")
    return ws


async def handle_health(request: web.Request) -> web.Response:
    return web.json_response({
        "ok": True,
        "session": SESSION.state if SESSION else None,
        "viewer": bool(SESSION and SESSION.viewer),
        "source_ip": SESSION.source_ip if SESSION else "",
        "proxy_conns": SESSION.proxy.conns if (SESSION and SESSION.proxy) else 0,
        "proxy_fails": SESSION.proxy.fails if (SESSION and SESSION.proxy) else 0,
        "proxy_drops": SESSION.proxy.drops if (SESSION and SESSION.proxy) else 0,
    })


async def on_startup(app: web.Application) -> None:
    log(f"启动：{HOST}:{PORT} token={'set' if SHARED_TOKEN else 'EMPTY(不安全)'} "
        f"home={LOGIN_HOME}")


async def on_cleanup(app: web.Application) -> None:
    global SESSION
    if SESSION is not None:
        await SESSION.close()
        SESSION = None


def main() -> None:
    # 空 token 等于不鉴权：只能绑环回，否则就是一个公网可控浏览器
    if not SHARED_TOKEN and HOST not in ("127.0.0.1", "localhost", "::1"):
        raise SystemExit(f"REMOTE_TOKEN 为空时必须绑定环回地址（当前 HOST={HOST}）")
    app = web.Application(client_max_size=1 << 20)
    app.router.add_get("/healthz", handle_health)
    app.router.add_post("/session", handle_create)
    app.router.add_post("/session/ticket", handle_ticket)
    app.router.add_post("/session/close", handle_close)
    app.router.add_get("/ws", handle_ws)
    app.on_startup.append(on_startup)
    app.on_cleanup.append(on_cleanup)
    web.run_app(app, host=HOST, port=PORT, print=None)


if __name__ == "__main__":
    main()
