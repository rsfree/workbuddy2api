#!/usr/bin/env bash
# 一条命令走完：推代码 → 等发版 tag → 等多架构镜像 → 可选部署上线。
#
# 存在的理由（都是踩过的坑，别再手工重来一遍）：
#   1. push 常被拒。远端会有发版工作流自己造的 `chore(release): vX [skip release]`
#      提交，本地 HEAD 落后它一版是**正常状态**（bump 由工作流负责，手改会冲突）。
#      所以这里先 fetch + rebase 再推，被拒就重试，不用人工判断。
#   2. semver 镜像不随 tag 级联。GITHUB_TOKEN 推的 tag 不再触发别的 workflow，
#      镜像得单独派发（release.yml 末尾已自动补这一步；本脚本仍会等它落地）。
#   3. 判镜像就绪别看 run 状态。run 常显示 in_progress 好几分钟，而 GHCR 的
#      manifest 其实早就 amd64+arm64 齐了（后面在跑无关的收尾）。只认 manifest
#      的 platforms 列表，这是唯一可靠信号。
#
# 用法见 --help。
# 退出码：0 全部就绪；1 失败（信息里会说是哪一步、怎么回滚）。

set -euo pipefail

# --------------------------------------------------------------- 部署目标
# 优先级：命令行 > 环境变量 > 下面的默认值。
#
# 为什么不从 deploy/sched-1/ 读：那个目录里的 README 是散文、docker-compose.yml 是
# **模板**（build: .，没有 image 行），没有任何机器可读的 host/dir 字段。为了"读配置"
# 得先发明一种新格式的文件——那不是单一事实来源，只是把同一份信息挪个地方，还多
# 一层解析和一处可能不同步的地方。换机器时 export 三个变量即可，不用改这个文件。
HOST="${WB2API_DEPLOY_HOST:-root@154.40.45.18}"
DEPLOY_DIR="${WB2API_DEPLOY_DIR:-/opt/workbuddy2api}"
SERVICE="${WB2API_DEPLOY_SERVICE:-wb2api}"
REMOTE="${WB2API_GIT_REMOTE:-rsfree443}"
GHCR_REGISTRY="ghcr.io"
GHCR_IMAGE="rsfree/workbuddy2api"        # 不含 registry，manifest API 用这个
IMAGE_REPO="${GHCR_REGISTRY}/${GHCR_IMAGE}"  # 容器里 {{.Config.Image}} 的完整写法

DO_DEPLOY=0
DRY_RUN=0
TARGET_TAG=""
TAG_TIMEOUT="${WB2API_TAG_TIMEOUT:-900}"       # 等发版 tag（release workflow 约 2-3 分钟）
IMAGE_TIMEOUT="${WB2API_IMAGE_TIMEOUT:-1800}"  # 等镜像（QEMU arm64 实测 5-15 分钟）

usage() {
  cat <<'EOF'
用法：scripts/release.sh [选项]

  （无选项）              推 main → 等发版 tag → 等镜像双架构就绪
  --deploy               再部署到线上容器并校验（幂等：已同版本就跳过重建）
  --tag v1.13.40         不推代码，只处理已存在的 tag（补发 / 重部署场景）
  --dry-run              只打印将要做什么，不推、不等、不动线上
  -h, --help             本帮助

覆盖部署目标（优先级：命令行 > 环境变量 > 内置默认）：
  --remote <name>        git remote                默认 rsfree443
  --host <user@addr>     ssh 目标                  默认 root@154.40.45.18
  --dir <path>           远端部署目录              默认 /opt/workbuddy2api
  --service <name>       compose 服务名            默认 wb2api

  环境变量：WB2API_GIT_REMOTE / WB2API_DEPLOY_HOST / WB2API_DEPLOY_DIR /
            WB2API_DEPLOY_SERVICE / WB2API_TAG_TIMEOUT / WB2API_IMAGE_TIMEOUT

退出码：0 就绪；1 失败（会说明卡在哪一步、怎么回滚）。
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --deploy)      DO_DEPLOY=1 ;;
    --dry-run)     DRY_RUN=1 ;;
    --tag)         TARGET_TAG="${2:?--tag 需要版本号，如 v1.13.40}"; shift ;;
    --remote)      REMOTE="${2:?}"; shift ;;
    --host)        HOST="${2:?}"; shift ;;
    --dir)         DEPLOY_DIR="${2:?}"; shift ;;
    --service)     SERVICE="${2:?}"; shift ;;
    -h|--help)     usage; exit 0 ;;
    *)             echo "未知参数：$1（--help 看用法）" >&2; exit 1 ;;
  esac
  shift
done

log()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[32m ✓\033[0m %s\n' "$*"; }
warn() { printf '\033[33m !\033[0m %s\n' "$*"; }
die()  { printf '\033[31m ✗\033[0m %s\n' "$*" >&2; exit 1; }
run()  { if [ "$DRY_RUN" = 1 ]; then printf '   [dry-run] %s\n' "$*"; else "$@"; fi; }

need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }
need git; need curl; need python3

cd "$(git rev-parse --show-toplevel)"

[ "$DO_DEPLOY" = 1 ] && log "部署目标 ${HOST}:${DEPLOY_DIR}（compose 服务 ${SERVICE}）"

# 远端所有 vX.Y.Z 形式的 tag（不排序，只做「前后差集」判断新 tag 出现没）
remote_tags() {
  git ls-remote --tags "$REMOTE" 'refs/tags/v*' \
    | awk '{print $2}' | sed 's#refs/tags/##' | grep -v '\^{}$' \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -u
}

# ---------------------------------------------------------------- 1. 推送
if [ -n "$TARGET_TAG" ]; then
  log "跳过推送，直接处理已存在的 ${TARGET_TAG}"
  VERSION="${TARGET_TAG#v}"
else
  [ -z "$(git status --porcelain)" ] || die "工作区有未提交改动，先 commit 或 stash（避免把半个改动推上线）"

  log "同步远端 main"
  git fetch --tags "$REMOTE" main
  if ! git merge-base --is-ancestor "${REMOTE}/main" HEAD 2>/dev/null; then
    warn "本地落后远端（通常是工作流的 bump 提交），rebase 一下"
    run git rebase "${REMOTE}/main"
  fi

  TAGS_BEFORE="$(remote_tags)"
  log "推送到 ${REMOTE}/main"
  pushed=0
  for attempt in 1 2 3; do
    if run git push "$REMOTE" HEAD:main; then pushed=1; break; fi
    warn "push 被拒（第 ${attempt} 次），重新同步后重试"
    git fetch --tags "$REMOTE" main
    [ "$DRY_RUN" = 1 ] || git rebase "${REMOTE}/main"
  done
  [ "$pushed" = 1 ] || die "推送三次均被拒，检查是否有别的机器在推同一分支"
  ok "已推送 $(git rev-parse --short HEAD)"

  # dry-run 到此为止：后面两步都是「等远端发生某件事」，真等会空转十几分钟。
  if [ "$DRY_RUN" = 1 ]; then
    plan="等新 tag → 等镜像双架构就绪"
    [ "$DO_DEPLOY" = 1 ] && plan="${plan} → 部署到 ${HOST}:${DEPLOY_DIR}"
    log "dry-run：将${plan}"
    exit 0
  fi

  # ------------------------------------------------------------ 2. 等 tag
  log "等发版工作流产出新 tag（最长 ${TAG_TIMEOUT}s）"
  NEW_TAG=""
  deadline=$(( $(date +%s) + TAG_TIMEOUT ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    NEW_TAG="$(remote_tags | grep -vxF "$(printf '%s\n' "$TAGS_BEFORE")" | tail -n1 || true)"
    [ -n "$NEW_TAG" ] && break
    sleep 10
  done
  [ -n "$NEW_TAG" ] || die "等不到新 tag。看失败原因：gh run list --repo rsfree/workbuddy2api --limit 3"
  VERSION="${NEW_TAG#v}"
  ok "发版 tag：${NEW_TAG}"
fi

TAG="v${VERSION}"
IMG="${IMAGE_REPO}:${VERSION}"

# ----------------------------------------------------------------- 3. 等镜像
log "等 GHCR 镜像 ${IMG}（最长 ${IMAGE_TIMEOUT}s）"

manifest_platforms() {
  local tag="$1" tok
  tok="$(curl -fsS --noproxy '*' "https://ghcr.io/token?scope=repository:${GHCR_IMAGE}:pull&service=ghcr.io" 2>/dev/null \
        | python3 -c 'import sys,json;print(json.load(sys.stdin).get("token",""))' 2>/dev/null)" || return 1
  [ -n "$tok" ] || return 1
  curl -fsS --noproxy '*' -H "Authorization: Bearer $tok" \
    -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json" \
    "https://ghcr.io/v2/${GHCR_IMAGE}/manifests/${tag}" 2>/dev/null \
    | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
print(" ".join(sorted(
    "%s/%s" % (m.get("platform", {}).get("os", ""), m.get("platform", {}).get("architecture", ""))
    for m in d.get("manifests", [])
)))'
}

if [ "$DRY_RUN" = 1 ]; then
  printf '   [dry-run] 轮询 https://ghcr.io/v2/%s/manifests/%s 直到含 linux/amd64 + linux/arm64\n' "$GHCR_IMAGE" "$VERSION"
else
  platforms=""
  deadline=$(( $(date +%s) + IMAGE_TIMEOUT ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    platforms="$(manifest_platforms "$VERSION" || true)"
    case "$platforms" in
      *linux/amd64*linux/arm64*|*linux/arm64*linux/amd64*) break ;;
    esac
    platforms=""
    sleep 15
  done
  [ -n "$platforms" ] || die "镜像 ${VERSION} 等不到双架构。查构建：gh run list --repo rsfree/workbuddy2api --workflow docker-ghcr.yml --limit 3"
  ok "镜像就绪：${platforms}"
fi

# ------------------------------------------------------------------- 4. 部署
if [ "$DO_DEPLOY" = 0 ]; then
  cat <<EOF

就绪。部署的话：
  ./scripts/release.sh --tag ${TAG} --deploy
EOF
  exit 0
fi

if [ "$DRY_RUN" = 1 ]; then
  printf '   [dry-run] ssh %s：确认目录/compose 存在 → 备份 compose → 切 image tag 为 %s → pull → up -d → 校验镜像与面板版本\n' "$HOST" "$VERSION"
  exit 0
fi

# 先确认目标可用再动它：目录错、compose 缺、docker 不可用这三种错，宁可在改文件
# 之前就撞死，也不要「备份已覆盖 + tag 已切」之后才发现 pull 不下镜像。
log "预检 ${HOST}:${DEPLOY_DIR}"
ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 "$HOST" \
  "DIR='${DEPLOY_DIR}' bash -s" <<'REMOTE' || die "预检失败：检查 --host / --dir，或远端 /opt 路径与 docker 可用性"
set -euo pipefail
cd "$DIR" || { echo "目录不存在：$DIR"; exit 1; }
[ -f docker-compose.yml ] || { echo "缺少 docker-compose.yml"; exit 1; }
[ -f config.json ]        || { echo "缺少 config.json（api_key 从这里读）"; exit 1; }
docker compose version >/dev/null || { echo "docker compose 不可用"; exit 1; }
echo "preflight ok: $(grep -m1 -E '^[[:space:]]*(image|build):' docker-compose.yml | sed -E 's/^[[:space:]]+//')"
REMOTE
ok "预检通过"

log "部署 ${TAG} 到 ${HOST}:${DEPLOY_DIR}"
ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 "$HOST" \
  "TAG='${VERSION}' IMG='${IMG}' DIR='${DEPLOY_DIR}' SVC='${SERVICE}' bash -s" <<'REMOTE'
set -euo pipefail
cd "$DIR"

CUR="$(sed -n -E 's#^[[:space:]]*image:[[:space:]]*.*:([0-9][^[:space:]]*)$#\1#p' docker-compose.yml | head -n1)"
RUN_IMG="$(docker compose ps -q "$SVC" 2>/dev/null | head -n1 | xargs -r docker inspect --format '{{.Config.Image}}' 2>/dev/null || true)"

# 幂等判据看**运行中的容器**，不看 compose 文件。只比文件的话，「compose 已改但
# 没 apply」这种状态会被误判成"已是最新"，然后面板版本校验失败、部署报错收场。
if [ "$CUR" = "$TAG" ] && [ "$RUN_IMG" = "$IMG" ]; then
  echo "已是最新：compose 与运行容器都是 ${TAG}，跳过重建"
else
  echo "compose=${CUR:-?} running=${RUN_IMG:-?} → 目标=${TAG}"
  BAK="docker-compose.yml.bak-${CUR:-unknown}"
  [ -f "$BAK" ] || cp -f docker-compose.yml "$BAK"
  echo "备份 → ${BAK}（回滚：cp ${BAK} docker-compose.yml && docker compose up -d ${SVC}）"
  sed -i -E "s#^([[:space:]]*image:[[:space:]]*ghcr\.io/[^:]+):.*#\1:${TAG}#" docker-compose.yml
  grep -n -m2 -E '^[[:space:]]*image:' docker-compose.yml
  docker compose pull "$SVC"
  docker compose up -d "$SVC"
fi

# 双保险：容器真的跑在目标镜像上（面板版本串只能证明"某个进程"报了这版）
NOW_IMG="$(docker compose ps -q "$SVC" | head -n1 | xargs -r docker inspect --format '{{.Config.Image}}')"
[ "$NOW_IMG" = "$IMG" ] || { echo "容器镜像不符：期望 ${IMG}，实际 ${NOW_IMG}"; exit 1; }
echo "镜像校验通过：${NOW_IMG}"

# 端口从 config.json 的 listen 读，别写死 —— host 网络下改端口时脚本不该静默失败
ADDR="$(python3 -c "import json;d=json.load(open('config.json'));print(d.get('listen') or '127.0.0.1:39017')")"
KEY="$(python3 -c "import json;print(json.load(open('config.json'))['api_key'])")"
case "$ADDR" in *:*) ;; *) ADDR="${ADDR}:39017" ;; esac
case "$ADDR" in 0.0.0.0:*|\[::\]:*) ADDR="127.0.0.1:${ADDR##*:}" ;; esac

echo "等面板自报版本 ${TAG}-panel（${ADDR}）..."
for i in $(seq 1 30); do
  V="$(curl -fsS -H "Authorization: Bearer $KEY" "http://${ADDR}/panel/api/overview" 2>/dev/null \
       | python3 -c 'import sys,json;print(json.load(sys.stdin).get("version",""))' 2>/dev/null || true)"
  if [ "$V" = "${TAG}-panel" ]; then
    echo "版本校验通过：$V"
    curl -fsS -H "Authorization: Bearer $KEY" "http://${ADDR}/panel/api/overview" \
      | python3 -c '
import sys, json
d = json.load(sys.stdin)
print("账号", {k: d.get(k) for k in ("total", "healthy", "cooling", "banned")})
c = d.get("cache") or {}
print("缓存命中 %s%% | 粘性 %s%% | 亲和 %s%% | 前缀占比 %s%%" % (
    c.get("cache_hit_rate"), c.get("sticky_hit_rate"),
    c.get("affinity_hit_rate"), c.get("prefix_share_est")))
print("会话绑定", d.get("sticky_sessions"), "· 前缀种数", d.get("affinity_prefixes"))'
    exit 0
  fi
  sleep 3
done
echo "面板 90s 内没自报 ${TAG}-panel，检查：docker logs --tail 50 $(docker compose ps -q "$SVC" | head -n1)" >&2
exit 1
REMOTE

ok "部署完成：${TAG}"
