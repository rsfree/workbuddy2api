# workbuddy2api 部署记录 · sched-1（154.40.45.18）

线上入口：**https://workbuddy2api.1task.cn** → 面板 `/panel/`

> **2026-10-23 两实例合并**：`wb2api.1task.cn`（旧 stage-1 hk）与 `workbuddy2api.1task.cn`（sched-1）
> 收敛为**单实例**。三个域名同后端、同 `api_key`（继承旧值 `sk-ba3949e4...`），旧实例容器已下线。
> 详见文末「两实例合并」。

## 拓扑

```
浏览器 / OpenAI SDK
   │  三个入口域名（同证书体系、同后端）
   │  ├ https://workbuddy2api.1task.cn   （主）
   │  ├ https://wb2api.1task.cn         （旧域名，别名）
   │  └ https://wb2api-hk.1task.cn      （旧限流别名：600r/m + body 4m）
   ▼
154.40.45.18 宝塔 nginx  ──proxy_pass──▶  127.0.0.1:39017
                                              │ 容器 workbuddy2api（network_mode: host）
                                              ▼
                                         容器内 127.0.0.1:39017（wb2api 二进制）
```

- 机器：`ssh root@154.40.45.18`（Ubuntu 24.04，Docker 26.1.4 / compose v5.5.1，宝塔 nginx 1.30.4）
- 目录：`/opt/workbuddy2api`（源码 + compose + config.json + auths/ + data/）
- vhost：`/www/server/panel/vhost/nginx/{workbuddy2api,wb2api,wb2api-hk}.1task.cn.conf`
- 证书：`/www/server/panel/vhost/cert/<域名>/{fullchain,privkey}.pem`（每域名独立签发，互不牵连）
- DNS：阿里云 `1task.cn`（TTL 600）
  - A `workbuddy2api` → `154.40.45.18`（RecordId 2112884845180717056）
  - A `wb2api` → `154.40.45.18`（RecordId 2106755037275239424，2026-10-23 切换）
  - A `wb2api-hk` → `154.40.45.18`（RecordId 2108176556782111744，2026-10-23 切换）

## 🔴 出口隔离：必须 host 网络（2026-10-21 实测修正）

`egress.mode=local` + `identity_source=uid` 的语义是「按 uid 哈希绑定**本机公网 IP**」。
但 bridge 模式下容器只看得见自己的 `172.30.x`，日志会打：

```
[egress] local[1 ip]：按身份哈希分配出口（group_by=account）
[egress]   172.30.1.2      ← 71 账号 [...]     ← 全部挤一个槽位，隔离形同虚设
```

实测此时所有请求的真实源 IP 都是宿主默认路由 `154.40.45.34`。改 `network_mode: host` 后：

```
[egress] local[32 ip]：按身份哈希分配出口（group_by=account）
[egress]   154.40.45.153   ← 6 账号 [...]      ← 71 号散到 28/32 个槽位
```

24 并发实测出站源 IP **17 个互不相同**。配套改动（缺一不可）：

1. compose 删掉 `ports:`（host 网络下无效），改 `network_mode: host`；
2. `config.json` 的 `listen` 改 `127.0.0.1:39017`（对外仍只经 nginx，vhost 不用动）；
3. healthcheck 探针端口同步 `39017`；
4. `wan` 上 32 个地址**逐个用 `curl --interface <ip>` 验过全部可出网**；hash 槽位在启动时冻结，
   地址增减会导致账号换出口 → 增删 IP 后应 `docker compose up -d --force-recreate` 重排。


## 重部署 / 升级

```bash
# 本地：只传源码（排除 auths/data/config.json）
cd ~/PycharmProjects/AI/reverse/workbuddy2api
tar czf - --exclude=.git --exclude=.workbuddy --exclude=auths --exclude=data \
  --exclude=config.json . | ssh root@154.40.45.18 'tar xzf - -C /opt/workbuddy2api'

# 服务器：重建
ssh root@154.40.45.18 'cd /opt/workbuddy2api && docker compose up -d --build'
```

`auths/`（凭证）与 `data/`（池状态）在宿主目录，重建镜像不丢。

## 首次部署踩到的点（复用时的清单）

1. **用 host 网络，不用端口映射**：`egress.mode=local` 必须在 host 网络下才能看见 `wan` 的公网 IP
   （见上节）。与 nginx 的契约改成「程序监听 `127.0.0.1:39017`，vhost `proxy_pass` 不变」。
   39005/39006/39016 已被 ark-compat / async-task 占用。
2. **属主**：容器以 uid 10001 运行 ⇒ `chown -R 10001:10001 auths data config.json`（config.json 也要，面板要写回）。
3. **健康探针不能打 `/healthz`**：无账号时它**故意** 503（`healthy:0`），docker 会长期显示 unhealthy。compose 里已覆盖为 `/panel/`。
4. **SSE**：vhost 里 `proxy_buffering off` + `proxy_request_buffering off` + `proxy_read_timeout 3600s`，否则流式会被攒住/掐断。不需要 WebSocket upgrade（不引入 `$connection_upgrade`，宝塔主配置未必有 map）。
5. **`nginx -t` 与 `-s reload` 必须分两行**（`set -e` 不管 AND-OR 列表首命令）。
6. **证书签发走 dns_ali**（DNS-01），不依赖 :80 挑战，签发即可用；`--install-cert` 的 reloadcmd 已挂 `nginx -s reload`。
7. **验证用 `--resolve host:443:127.0.0.1`**（带 SNI），只给 `-H Host` 会落到 default_server。
8. 🔴 **推源码时不能整包覆盖 `docker-compose.yml`**：仓库根目录那份是**默认 bridge 版**，服务器这份是
   **host 网络版**（出口隔离的前提）。2026-10-22 升级时就因为 `tar -xzf` 原样解包把它盖掉，
   容器被重建到 bridge 网络 → 启动日志 `[egress] local[1 ip]`、只看得见 `172.30.1.2`，
   101 个号全挤一个出口，且 `listen 127.0.0.1:39017` 在 bridge 里等于宿主机 nginx 打不通（对外直接 502）。
   正确做法：解包前把本目录的 compose 排除，或在解包后立刻 `cp deploy/sched-1/docker-compose.yml /opt/workbuddy2api/`
   再 `docker compose up -d`。**升级后必查启动日志第一行是不是 `local[32 ip]`。**

## 当前状态

- 容器 healthy，面板 200，未鉴权 `/v1/*` 返回 401（api_key 已随机生成在 `/opt/workbuddy2api/config.json`）
- 账号池为空（`{"healthy":0,"total":0}`）⇒ 需在面板「添加账号」走 OAuth，凭证落 `auths/` 后热加载，无需重启

## 两实例合并（2026-10-23）

### 为什么

两套实例各持一份号池，运维要跑两遍；旧 stage-1 的 139 个号里 112 个已被上游 11140 封禁，
只剩 27 个可用。合并后单池 50 个号（23 存量 + 27 迁入），避免"旧实例还在跑但号已死"的假象。

### 做了什么

1. **号池迁移**：旧实例未封禁的 27 个号按 `auths/workbuddy-<uid>.json` 原样取回，用
   `workbuddy2api-account-import` 技能导入（27/27 成功，4.07s）。
2. **鉴权 key 继承**：新实例 `api_key` 改为旧值 `sk-ba3949e4...`（面板 `POST /panel/api/config`
   热生效并落盘），线上转发/客户端零改动。
3. **域名收敛**：`wb2api` / `wb2api-hk` 各自签发证书 + 新增 vhost（内容与主域名一致，
   `wb2api-hk` 保留原有 600r/m 限流与 4m body），阿里云 A 记录一并指向 `154.40.45.18`。
4. **旧实例下线**：`docker stop workbuddy2api` + `--restart=no`；旧 nginx 的两个 vhost 改为
   **回源桥**（`proxy_pass https://workbuddy2api.1task.cn`），只服务 DNS 缓存未过期的客户端。

### 现在的结构

| 域名 | 后端 | 限流 | 备注 |
|---|---|---|---|
| workbuddy2api.1task.cn | 127.0.0.1:39017 | 无 | 主入口，`client_max_body_size 512m` |
| wb2api.1task.cn | 127.0.0.1:39017 | 无 | 旧域名别名，同主入口 |
| wb2api-hk.1task.cn | 127.0.0.1:39017 | 600r/m，burst 120 | 旧限流入口，body 限 4m |
| （旧机 154.21.201.183） | → 新实例 | 同上 | 回源桥，可择机拆除 |

### 拆除回源桥（DNS TTL 过后）

```bash
# 旧机 154.21.201.183
cp /root/wb2api-hk-proxy.inc.bak-20261023 /www/server/panel/vhost/nginx/wb2api-hk-proxy.inc
# 或直接删掉两个 vhost，让旧机彻底不再承载该服务
/www/server/nginx/sbin/nginx -t && /www/server/nginx/sbin/nginx -s reload
```

⚠️ 回源桥的证书仍是旧机本地 HTTP-01 签发，DNS 迁走后**无法自动续期**（到期 2027-01）。
长期保留桥的话需改 DNS-01，否则 90 天后会因证书过期而报错——计划拆除则无需处理。

### 回滚

旧机数据完整保留（`/opt/workbuddy2api/auths` 139 个文件、容器 `Exited`）：
`docker update --restart=always workbuddy2api && docker start workbuddy2api`，
再把 `wb2api-hk-proxy.inc` 的 `proxy_pass` 改回 `http://127.0.0.1:39018`，并把 DNS 切回。

## 版本记录

- `1.13.20`（2026-10-24）：新号任务静默期 `schedule.new_account_grace_days`（默认 3 天）——
  导入后前 3 天只签到/保活/刷新余额，跳过旅行/活跃/夜猫子/成长队列，积分清光提前放行；
  auth 文件顶层新增 `imported_at`（可信来源，mtime 推断值不参与判定）。回滚：
  `docker-compose.yml.bak-1.13.19-grace`。

- `1.13.21`（2026-10-24）：新号静默期**不再停活跃上报**（用户口径「活跃上报不要停」）——
  连登天数断一天补不回来，且活跃上报是 `first_buddy` 领养的前置心跳；静默期收窄为
  旅行 / 夜猫子 / 成长队列。回归用例 `TestRunActivityNowIgnoresNewAccountGrace`。回滚：
  `docker-compose.yml.bak-1.13.20-grace`。

- `1.13.28`（2026-10-24）：**新增对话协议前门** `POST /v1/messages`（Anthropic Messages）、
  `POST /v1/responses`（OpenAI Responses）、`POST /v1/messages/count_tokens`。入口翻成 chat
  请求后复用同一条 `chatCompletions` 链路（选号/粘性/轮转/冷却/记账零改动），出口在写入器层
  翻回目标协议。`count_tokens` 按真实 `usage.prompt_tokens` 在线自校准，样本采在 chat 链路
  usage 唯一汇聚点，参数落 `data/token-calib.json`、`GET /status` 的 `token_calib` 可观测。
  回滚：`cp .pre-v1.13.28 docker-compose.yml && docker compose up -d`。

- `1.13.29`（2026-10-24）：**鉴权接受 `x-api-key`**。1.13.28 上线后实测发现 Claude Code
  默认只发 `x-api-key` 不认 `Authorization: Bearer`，直接 401 —— 前门的核心用户被挡在门外。
  现在两个头都认（Bearer 优先，`Authorization` 是别的方案时不回落），常量时间比较性质不变。
  回滚：`cp .pre-v1.13.29 docker-compose.yml && docker compose up -d`。

- `1.13.30`（2026-10-24）：**`count_tokens` 改用会话基线**。之前只有「整段字符粗估 × 系数」，
  误差来自整段；现在按逐条消息的 SHA-256 指纹做前缀匹配，命中后返回「上一轮真实
  `usage.prompt_tokens` + 新增消息的估算」，完全命中（同一批 messages 跑过 chat）时直接返回
  上游原话、误差 0；匹配不上才回落系数换算。新增 `token_baseline` 观测（`hits`/`misses`/
  `pred_by_model.mae`/`pred_pending`）与两类日志（`mode=miss` 全量、`mode=prefix` 仅当新增段
  ≥500 token）。纯内存不落盘，重启即清（换会话基线本来无用）。回滚：
  `cp .pre-v1.13.30 docker-compose.yml && docker compose up -d`。

## 上线验证清单（1.13.28/1.13.29 实测）

```
/panel/                            200，overview version=1.13.29-panel，68 账号 / 55 healthy
/v1/messages 无鉴权                 401 {"type":"error","error":{"type":"authentication_error"}}
/v1/messages x-api-key             200 type=message stop=end_turn
/v1/messages 流式                   event: message_start → content_block_delta… → message_stop（69 帧）
                                   首字节 1.30s / 总时长 1.54s ⇒ nginx 未整包缓冲
/v1/responses                      200 object=response status=completed
/v1/messages + tools→强制 tool_choice  stop=tool_use，blocks=[thinking,text,tool_use]，
                                   input={"city":"北京"}
/v1/messages/count_tokens          200 {"input_tokens":20}（随 token_calib 学到的系数浮动）
错误信封                            空 messages→400 invalid_request_error（Anthropic 形状）
                                   /v1/responses 不存在的模型→error.code=model_unavailable
启动日志                            local[32 ip]，[tokencalib] 已启用 data/token-calib.json
```

> 校准实况（上线 10 分钟、真实流量）：`deepseek-v4.1-flash` 49 样本 slope≈1.96、
> `kimi-k2.5` 12 样本 slope≈0.89 + intercept≈557（说明上游把固定开销算进了 prompt_tokens，
> intercept 正是为它准备的）、另有 4 个模型在学。文件在 `data/token-calib.json`，重建容器不丢。
