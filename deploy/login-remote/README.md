# login-remote · 远程浏览器 sidecar

把服务器上的 Chromium 画面投进 workbuddy2api 面板，让人**在服务器出口 IP 上**完成
WorkBuddy / CodeBuddy 的 OAuth 登录（短信、滑块、扫码都能过）。

## 它是什么，不是什么

| 是 | 不是 |
|---|---|
| 打开主服务下发的一个授权 URL | 不认识站点、不解析凭据、不碰账号库 |
| CDP `Page.startScreencast` 投屏 + `Input.dispatch*` 回放输入 | 不做浏览器自动化登录、不绕过验证码 |
| 单会话 / 10 分钟硬上限 / 一次性 ticket | 不是公网可控浏览器（默认只绑环回） |

**为什么不需要抓凭据**：WorkBuddy 是设备流（`/v2/plugin/auth/state` → 用户在浏览器登录 →
token 挂在 `state` 上）。浏览器只要把登录做完，主服务原有的
`GET /panel/api/login/poll` 就能取回 token、落盘 `auths/workbuddy-<uid>.json`、
进池、签到、触发 onboard。凭据全程不经过本服务。

**为什么需要它**：登录页会显示并绑定发起请求的出口 IP

```
本次登录请求来自 CodeBuddy CLI，来源：中国 江苏省 南京市 | IP: 117.88.13.35
```

在本地浏览器登录 = token 绑到本地出口 IP；账号有 IP 风控时，需要让登录发生在服务器上。

## 架构

```
面板浏览器 ──WS(/panel/api/login/remote/ws?ticket=一次性)──> wb2api(Go)
        ↑ canvas 画面 / 输入命令                              │ 原样转发
        └──────────────────────────────────────────────────────┤
                                                               ▼
                                              login-remote(sidecar) ──CDP──> Chromium
```

- 浏览器**不直连** sidecar：sidecar 只在 compose 内网（本地）或 `127.0.0.1`（线上 host 网络）。
- WS 门槛是 `start` 下发的一次性 ticket（60 秒）。浏览器无法给 WebSocket 加
  `Authorization` 头，所以 API key 不放进 URL。
- 断线重连走 `POST /panel/api/login/remote/ticket`，sidecar 保留页面 90 秒，接回同一屏。

## 启用

### 1) 镜像

`.github/workflows/login-remote-ghcr.yml` 在 `deploy/login-remote/**` 变化时构建
`ghcr.io/rsfree/workbuddy2api-login-remote:latest`（仅 amd64）。首次启用前手动触发一次：

```bash
gh workflow run login-remote-ghcr.yml -R rsfree/workbuddy2api
```

### 2) 服务器

```bash
cd /opt/workbuddy2api
# .env 里加一行（不要提交进仓库）
echo 'WB2API_LOGIN_REMOTE_TOKEN='"$(openssl rand -hex 24)" >> .env

docker compose --profile login-remote pull login-remote
docker compose --profile login-remote up -d login-remote
docker compose ps login-remote          # 等 healthy
curl -s http://127.0.0.1:8099/healthz    # {"ok":true,"session":null}
```

### 3) 主服务

`docker-compose.yml` 主服务的 environment 取消这两行注释，然后重建：

```yaml
      - WB2API_LOGIN_REMOTE_URL=http://127.0.0.1:8099
      - WB2API_LOGIN_REMOTE_TOKEN=${WB2API_LOGIN_REMOTE_TOKEN}
```

```bash
docker compose up -d wb2api
curl -s -H "Authorization: Bearer <api_key>" \
  http://127.0.0.1:39017/panel/api/login/remote/status   # {"enabled":true}
```

未配置时该接口返回 `{"enabled":false}`，面板的「在服务器浏览器中登录」入口自动隐藏，
原有「复制授权链接」登录完全不受影响。

### 4) nginx

WS 必须单独放行 `Upgrade`/`Connection`（仓库内三个入口配置已加好
`location = /panel/api/login/remote/ws`）：

```nginx
location = /panel/api/login/remote/ws {
    proxy_pass http://127.0.0.1:39017;
    proxy_http_version 1.1;
    proxy_set_header Upgrade    $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_buffering off;
    proxy_read_timeout 900s;
}
```

改完 `nginx -t && nginx -s reload`。

## 出口绑定（egress.mode=local）

登录绑定的 IP 与入池后的使用 IP 是否一致，决定了两条路径：

| 场景 | 出口选择 | 精度 |
|---|---|---|
| **重登老号**（账号行「重登」） | 该账号的租约地址；无租约时用 `LocalSlot(uid)` | 登录 IP = 使用 IP |
| **新号首登**（添加账号） | 账号数**最少**的出口（平票按游标顺延） | 登录成功后立刻写成租约 ⇒ 此后固定不变 |

### 登录即租约

登录成功时，把**本次实际使用的源 IP** 写成该 uid 的租约（`data/egress_leases.json`），
此后该账号所有请求都走这个出口 —— `egress` 里租约优先于哈希：

```
Bind(uid) = LeaseOf(uid) ?? LocalSlot(uid)
```

这样「登录 IP」和「使用 IP」在语义上合流，**新号不需要重登来对齐**。

- 租约地址必须在 `egress.local.ips` 候选里；配置变更导致失效时**自动回退哈希**并告警。
- 租约不取消出口冷却：坏出口仍会被临时顺延，恢复后自动回到租约地址。
- 删掉 `data/egress_leases.json` 重启 = 全部回退哈希，是最干净的运维开关。
- `nextLoginIP` 按「当前账号数最少」选，而不是盲轮转 —— 租约会让分布偏离哈希，
  必须靠负载优先把它拉回均匀。

实现方式：sidecar 内置一个只做 CONNECT 的本地代理，每条隧道
`open_connection(local_addr=(源IP, 0))` 绑定源地址，Chromium 通过
`--proxy-server` 走它。不需要 root、不需要 iptables，也不需要改宿主网络。

`GET /healthz` 可用于对账：

```json
{"session":"live","source_ip":"1.2.3.4",
 "proxy_conns":29,   // 成功建立的隧道 = 确实走了这个出口
 "proxy_fails":0,    // 源地址不可用（EADDRNOTAVAIL）—— 真正的出口故障
 "proxy_drops":0}    // 目标不可达/被墙 —— 与出口无关
```

> 本地开发机通常只有一个公网出口，绑内网源地址经 NAT 后上游看到的仍是同一个公网 IP；
> 线上 32 个地址是直接绑在网卡上的公网 alias，绑哪个就出哪个。

## 使用

面板 → 添加账号 → 「在服务器浏览器中登录」→ 画面里扫码/输手机号 → 登录完成后面板
自动提示「已添加 …，账号已载入池中」。

账号行 → **「重登」**：用该账号的槽位出口重新打开登录页，登录完成后覆盖同一 uid 的凭证。
适用于 `session_dead`、凭证过期、或想把登录 IP 拉回原出口的号。

- 登录结果会带上出口对比：`egress_hint` 字段指出「登录出口 ≠ 槽位出口」，
  提示用「重登」纠正；新号则直接告知「入池后固定使用出口 X」。

- 中文/长文本用画面下方的**粘贴框**：`keydown` 拿不到输入法合成结果。
- 「后退 / 刷新页面」用于页面卡住；「结束远程」立即释放浏览器。

## 已知边界

1. **新号登录前无法预知槽位**：uid 未知，`LocalSlot` 算不出来。现在的做法是
   「登录用最空的出口 → 登录后写租约固定」，所以**入池后是一致的**，但登录前无法
   预先声明这个号将使用哪个 IP。
2. **租约会让分布偏离哈希**：靠「账号数最少优先」维持均匀；若某个公网 IP 被上游
   拉黑，需要清掉相关租约或调整 `local.ips` 后重启（失效租约自动回退哈希）。
3. **10 分钟硬上限**：超时自动回收，需重新发起（`session.TIMEOUT`）。
4. **单会话**：新的 start 会关掉旧会话（多开浏览器会互相覆盖登录态）。
5. **首次真实登录未端到端验证**：链路到「画面可操作、帧持续到达」已验证；登录完成后的
   落盘/进池复用既有 `loginPoll`（本次未改动），首次使用请确认面板出现「已添加」。

## 排障

| 现象 | 大概率原因 | 处理 |
|---|---|---|
| 面板没有「在服务器浏览器中登录」 | 主服务 env 未配 / 未重建 | 查 `/remote/status` 是否 `enabled:true` |
| 点开一直「准备中」 | sidecar 未起或 token 不一致 | `docker logs workbuddy2api-login-remote`；核对两边 token |
| 画面空白但状态已连接 | 页面还在加载 / 画面暂无变化 | 等 5–10 秒；点「刷新页面」 |
| WS 连上立刻断 | nginx 未放行 Upgrade | 按上面第 4 步加 location |
| 中文全是方块 | 镜像缺 CJK 字体 | 重建镜像（Dockerfile 已装 `fonts-wqy-microhei`） |
| 提示「会话超时（10 分钟）」 | 硬上限 | 重新点「在服务器浏览器中登录」 |
| 登录完成了面板没反应 | 登录的不是该 state 的授权页 | 看服务端日志 state 前 8 位是否一致；重新发起 |
| 点「重登」后一直「正在打开」 | 旧会话关闭被挂起连接拖住 | 已在 sidecar 加关闭超时（页面 2s + 任务 3s + 取消 2s）；仍卡则看 sidecar 日志 |
| `proxy_fails` 一直涨 | 源地址不属于本机/已被摘除 | 查 `egress.local.ips` 与网卡实际地址；这是真故障，别当网络抖动 |

## 本地开发

```bash
cd deploy/login-remote
uv venv && uv pip install -r requirements.txt
uv run playwright install chromium

REMOTE_TOKEN=dev PORT=8188 uv run python server.py
curl -s http://127.0.0.1:8188/healthz
```

主服务指向它：

```bash
WB2API_LOGIN_REMOTE_URL=http://127.0.0.1:8188 \
WB2API_LOGIN_REMOTE_TOKEN=dev \
go run ./cmd/server -config /tmp/wb2api-smoke/config.json
```
