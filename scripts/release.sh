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
# 用法：
#   ./scripts/release.sh                  # 推 + 等镜像就绪，不碰线上
#   ./scripts/release.sh --deploy         # 再部署到 sched-1 并校验版本
#   ./scripts/release.sh --tag v1.13.39   # 不推代码，只等/部署指定 tag（补发场景）
#   ./scripts/release.sh --dry-run        # 只打印将要做什么
#
# 退出码：0 全部就绪；1 失败（信息里会说是哪一步、怎么回滚）。

set -euo pipefail

REMOTE="rsfree443"
GHCR_IMAGE="rsfree/workbuddy2api"
HOST="root@154.40.45.18"
DEPLOY_DIR="/opt/workbuddy2api"
SERVICE="wb2api"

DO_DEPLOY=0
DRY_RUN=0
TARGET_TAG=""
TAG_TIMEOUT=900      # 等发版 tag（release workflow 约 2-3 分钟，留足余量）
IMAGE_TIMEOUT=1800   # 等镜像（QEMU arm64 实测 5-15 分钟）

while [ $# -gt 0 ]; do
  case "$1" in
    --deploy)      DO_DEPLOY=1 ;;
    --dry-run)     DRY_RUN=1 ;;
    --tag)         TARGET_TAG="${2:?--tag 需要版本号，如 v1.13.39}"; shift ;;
    --remote)      REMOTE="${2:?}"; shift ;;
    --host)        HOST="${2:?}"; shift ;;
    --dir)         DEPLOY_DIR="${2:?}"; shift ;;
    --service)     SERVICE="${2:?}"; shift ;;
    -h|--help)     sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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

# 远端所有 vX.Y.Z 形式的 tag（不排序，只做「前后差集」判断新 tag 出现没）
remote_tags() {
  git ls-remote --tags "$REMOTE" 'refs/tags/v*' \
    | awk '{print $2}' | sed 's#refs/tags/##' | grep -v '\^{}$' \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -u
}

# ---------------------------------------------------------------- 1. 推送
if [ -n "$TARGET_TAG" ]; then
  log "跳过推送，直接处理已存在的 $TARGET_TAG"
  VERSION="${TARGET_TAG#v}"
else
  [ -z "$(git status --porcelain)" ] || die "工作区有未提交改动，先 commit 或 stash（避免把半个改动推上线）"

  log "同步远端 main"
  git fetch --tags "$REMOTE" main
  if ! git merge-base --is-ancestor "$REMOTE/main" HEAD 2>/dev/null; then
    warn "本地落后远端（通常是工作流的 bump 提交），rebase 一下"
    run git rebase "$REMOTE/main"
  fi

  TAGS_BEFORE="$(remote_tags)"
  log "推送到 $REMOTE/main"
  pushed=0
  for attempt in 1 2 3; do
    if run git push "$REMOTE" HEAD:main; then pushed=1; break; fi
    warn "push 被拒（第 $attempt 次），重新同步后重试"
    git fetch --tags "$REMOTE" main
    [ "$DRY_RUN" = 1 ] || git rebase "$REMOTE/main"
  done
  [ "$pushed" = 1 ] || die "推送三次均被拒，检查是否有别的机器在推同一分支"
  ok "已推送 $(git rev-parse --short HEAD)"

  # dry-run 到此为止：后面两步都是「等远端发生某件事」，真等会空转十几分钟。
  if [ "$DRY_RUN" = 1 ]; then
    plan="等新 tag → 等镜像双架构就绪"
    if [ "$DO_DEPLOY" = 1 ]; then
      plan="${plan} → 部署到 ${HOST}:${DEPLOY_DIR}"
    fi
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
  ok "发版 tag：$NEW_TAG"
fi

TAG="v${VERSION}"

# ----------------------------------------------------------------- 3. 等镜像
log "等 GHCR 镜像 $GHCR_IMAGE:${VERSION}（最长 ${IMAGE_TIMEOUT}s）"

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
  ok "镜像就绪：$platforms"
fi

# ------------------------------------------------------------------- 4. 部署
if [ "$DO_DEPLOY" = 0 ]; then
  cat <<EOF

就绪。部署的话：
  ./scripts/release.sh --tag ${TAG} --deploy
EOF
  exit 0
fi

log "部署 ${TAG} 到 ${HOST}:${DEPLOY_DIR}（服务 ${SERVICE}）"
if [ "$DRY_RUN" = 1 ]; then
  printf '   [dry-run] ssh %s：备份 compose → 改 image tag 为 %s → pull → up -d → 校验 overview\n' "$HOST" "$VERSION"
  exit 0
fi

ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 "$HOST" \
  "TAG='$VERSION' DIR='$DEPLOY_DIR' SVC='$SERVICE' bash -s" <<'REMOTE'
set -euo pipefail
cd "$DIR"

CUR="$(sed -n -E 's#^[[:space:]]*image:[[:space:]]*.*:([0-9][^[:space:]]*)$#\1#p' docker-compose.yml | head -n1)"
if [ "$CUR" = "$TAG" ]; then
  echo "compose 已是 ${TAG}，跳过重建"
else
  BAK="docker-compose.yml.bak-${CUR:-unknown}"
  [ -f "$BAK" ] || cp -f docker-compose.yml "$BAK"
  echo "备份 → ${BAK}（回滚：cp $BAK docker-compose.yml && docker compose up -d ${SVC}）"
  sed -i -E "s#^([[:space:]]*image:[[:space:]]*ghcr\.io/[^:]+):.*#\1:${TAG}#" docker-compose.yml
  grep -n 'image:' docker-compose.yml
  docker compose pull "$SVC"
  docker compose up -d "$SVC"
fi

KEY="$(python3 -c "import json;print(json.load(open('config.json'))['api_key'])")"
echo "等面板自报版本 $TAG-panel ..."
for i in $(seq 1 30); do
  V="$(curl -fsS -H "Authorization: Bearer $KEY" http://127.0.0.1:39017/panel/api/overview 2>/dev/null \
       | python3 -c 'import sys,json;print(json.load(sys.stdin).get("version",""))' 2>/dev/null || true)"
  if [ "$V" = "${TAG}-panel" ]; then
    echo "版本校验通过：$V"
    curl -fsS -H "Authorization: Bearer $KEY" http://127.0.0.1:39017/panel/api/overview \
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
echo "面板 90s 内没自报 ${TAG}-panel，检查：docker logs --tail 50 workbuddy2api" >&2
exit 1
REMOTE

ok "部署完成：$TAG"
