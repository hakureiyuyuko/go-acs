#!/usr/bin/env bash
# 轻量 TR-069 ACS —— 升级脚本
#
#   sudo ./update.sh                 # 从 GitHub 拉最新版（默认）
#   sudo ./update.sh --check         # 只看有没有新版本，不动手
#   sudo ./update.sh --tag v1.0.1    # 升到指定版本
#   sudo ./update.sh --file acs-1.0.1-linux-amd64.tar.gz   # 用本地包（内网/离线）
#
# 流程：下载 → 校验 SHA256 → 停服务 → 备份旧二进制 → 换上新二进制 → 起服务 → 健康检查；
# 起不来就自动回滚到旧二进制。
set -euo pipefail

REPO=${ACS_REPO:-hakureiyuyuko/go-acs}
API=${ACS_API:-https://api.github.com}
SERVICE=${ACS_SERVICE_NAME:-acs}
BIN_DIR=${ACS_BIN_DIR:-/usr/local/bin}
DATA_DIR=${ACS_DATA_DIR:-/var/lib/acs}
ENV_FILE=${ACS_ENV_FILE:-/etc/default/acs}
UNIT_DIR=${ACS_UNIT_DIR:-/etc/systemd/system}
SVC_USER=${ACS_SERVICE_USER:-acs}
KEEP_BACKUPS=${ACS_KEEP_BACKUPS:-3}

TAG=
FILE=
CHECK_ONLY=0
NO_SERVICE=0
DRY_RUN=0
HERE=$(cd "$(dirname "$0")" && pwd)

log()  { printf '\033[32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m警告:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m错误:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<EOF
用法: sudo ./update.sh [选项]

  --tag TAG          指定版本（如 v1.0.1），默认取最新 release
  --file 包.tar.gz   用本地安装包升级（内网或离线时用）
  --check            只检查有没有新版本
  --bin-dir DIR      二进制目录，默认 ${BIN_DIR}
  --data-dir DIR     数据目录（备份放这里），默认 ${DATA_DIR}
  --env-file FILE    配置文件（健康检查时读端口），默认 ${ENV_FILE}
  --unit-dir DIR     systemd 单元目录，默认 ${UNIT_DIR}
  --service-name NAME  服务名，默认 ${SERVICE}
  --service-user USER  服务用户，默认 ${SVC_USER}
  --no-service       不碰 systemd，只换二进制
  --keep N           保留最近 N 个备份，默认 ${KEEP_BACKUPS}
  --dry-run          只打印要做什么
  -h, --help         看这个帮助
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --tag)          [ $# -ge 2 ] || die "--tag 缺参数"; TAG=$2; shift 2;;
    --file)         [ $# -ge 2 ] || die "--file 缺参数"; FILE=$2; shift 2;;
    --check)        CHECK_ONLY=1; shift;;
    --bin-dir)      [ $# -ge 2 ] || die "--bin-dir 缺参数"; BIN_DIR=$2; shift 2;;
    --data-dir)     [ $# -ge 2 ] || die "--data-dir 缺参数"; DATA_DIR=$2; shift 2;;
    --env-file)     [ $# -ge 2 ] || die "--env-file 缺参数"; ENV_FILE=$2; shift 2;;
    --unit-dir)     [ $# -ge 2 ] || die "--unit-dir 缺参数"; UNIT_DIR=$2; shift 2;;
    --service-name) [ $# -ge 2 ] || die "--service-name 缺参数"; SERVICE=$2; shift 2;;
    --service-user) [ $# -ge 2 ] || die "--service-user 缺参数"; SVC_USER=$2; shift 2;;
    --no-service)   NO_SERVICE=1; shift;;
    --keep)         [ $# -ge 2 ] || die "--keep 缺参数"; KEEP_BACKUPS=$2; shift 2;;
    --dry-run)      DRY_RUN=1; shift;;
    -h|--help)      usage; exit 0;;
    *)              die "未知参数：$1（--help 看用法）";;
  esac
done

if [ "$DRY_RUN" != 1 ] && [ "$CHECK_ONLY" != 1 ] && [ "$(id -u)" != 0 ] && [ "$NO_SERVICE" = 0 ]; then
  die "需要 root 权限，请用 sudo 运行（或加 --no-service）"
fi

run() {
  if [ "$DRY_RUN" = 1 ]; then printf '   [dry-run] %s\n' "$*"; else "$@"; fi
}

# 读配置里生效的值 / 从监听地址里取端口
envval() { grep -E "^$1=" "$ENV_FILE" 2>/dev/null | tail -1 | cut -d= -f2- || true ; }
port_of() {
  local addr=${1##*:}
  case "$addr" in ''|*[!0-9]*) echo "" ;; *) echo "$addr" ;; esac
}

case "$(uname -m)" in
  x86_64|amd64)   ARCH=amd64;;
  aarch64|arm64)  ARCH=arm64;;
  *)              ARCH=$(uname -m); warn "没见过的架构 ${ARCH}，按原样去找包名";;
esac

if [ -f "$DATA_DIR/VERSION" ]; then
  had_version=1
  cur_version=$(cat "$DATA_DIR/VERSION")
else
  had_version=0
  cur_version="(未知)"
fi
log "当前版本 ${cur_version}；架构 ${ARCH}"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/acs-update-XXXXXX")
trap 'rm -rf "$WORK"' EXIT

# ---------- 1. 拿到安装包 ----------
strip_v() { echo "${1#v}"; }

if [ -n "$FILE" ]; then
  [ -f "$FILE" ] || die "找不到安装包 $FILE"
  tarball=$FILE
  new_version=$(basename "$FILE" | sed -E 's/^acs-([^-]+)-.*/\1/')
  log "使用本地安装包 $FILE（版本 ${new_version}）"
else
  command -v curl >/dev/null 2>&1 || die "需要 curl 来下载安装包（离线请用 --file）"
  if [ -n "$TAG" ]; then
    API_URL="${API}/repos/${REPO}/releases/tags/${TAG}"
  else
    API_URL="${API}/repos/${REPO}/releases/latest"
  fi
  log "查询 ${API_URL}"
  rel=$(curl -fsSL --retry 3 --max-time 30 "$API_URL") \
    || die "拉不到 release 信息（网络不通？也可以 --file 用本地包）"
  # 压成一行，省得依赖 jq：两种格式（带不带换行）都能抠
  flat=$(printf '%s' "$rel" | tr -d '\r\n')
  new_version=$(printf '%s' "$flat" | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/')
  [ -n "$new_version" ] || die "release 信息里没有 tag_name"
  new_tag=$new_version          # 拼下载地址要用原始 tag（带 v）
  new_version=$(strip_v "$new_version")

  if [ "$new_version" = "${cur_version#v}" ]; then
    log "已经是最新版本（${new_version}）"
    [ "$CHECK_ONLY" = 1 ] && exit 0
    log "相同版本也继续重装一遍（想跳过就 Ctrl-C）"
  fi
  if [ "$CHECK_ONLY" = 1 ]; then
    log "有新版本：${new_version}（当前 ${cur_version}）"
    exit 0
  fi

  asset="acs-${new_version}-linux-${ARCH}.tar.gz"
  # 下载地址不用刮 JSON：release 资产的 URL 形状是固定的 /releases/download/<tag>/<文件名>，
  # 比正则靠谱（GitHub 的 JSON 里 asset 对象还嵌着 uploader 子对象，正则很容易蹿到别的资产上）
  dl_base="${ACS_DL_BASE:-https://github.com/${REPO}/releases/download}/${new_tag}"
  url="${dl_base}/${asset}"
  sums_url="${dl_base}/SHA256SUMS"

  log "下载 ${asset}"
  if [ "$DRY_RUN" = 1 ]; then
    printf '   [dry-run] curl -fL -o %s %s\n' "$WORK/$asset" "$url"
  elif ! curl -fL --retry 3 --max-time 300 -o "$WORK/$asset" "$url"; then
    die "下载失败：${url}
     （这个 release 里可能没有 ${asset}；也可以用 --file 指定本地包）"
  fi
  tarball="$WORK/$asset"

  if [ "$DRY_RUN" = 1 ]; then
    echo "   [dry-run] 下载 SHA256SUMS：${sums_url}"
  elif ! curl -fsSL --max-time 60 -o "$WORK/SHA256SUMS" "$sums_url"; then
    rm -f "$WORK/SHA256SUMS"
    warn "SHA256SUMS 下载失败，跳过校验"
  fi
fi

# ---------- 2. 校验 SHA256 ----------
if [ "$DRY_RUN" != 1 ] && [ -f "$WORK/SHA256SUMS" ]; then
  want=$(grep -E "  ${asset:-$(basename "$tarball")}$" "$WORK/SHA256SUMS" | awk '{print $1}' | head -1)
  got=$(sha256sum "$tarball" | awk '{print $1}')
  if [ -z "$want" ]; then
    warn "SHA256SUMS 里没有 $(basename "$tarball")，跳过校验"
  elif [ "$want" != "$got" ]; then
    die "校验失败：包可能损坏或被篡改（期望 ${want}，实际 ${got}）"
  else
    log "SHA256 校验通过"
  fi
fi

# ---------- 3. 解包并试跑 ----------
if [ "$DRY_RUN" = 1 ]; then
  printf '   [dry-run] 解包 %s 到临时目录，检查新二进制能不能跑\n' "$tarball"
  NEWBIN="$tarball"
else
  # --no-same-owner：包里带的属主信息在有些环境下没法还原（比如容器 / 用户命名空间里），
  # 解出来的文件归当前用户就够，反正下一步是 install -m 0755 安装
  tar --no-same-owner -xzf "$tarball" -C "$WORK" || die "解包失败"
  NEWBIN=$(find "$WORK" -type f -name acs -perm -u+x | head -1)
  [ -n "$NEWBIN" ] || die "包里没有 acs 二进制"
  chmod +x "$NEWBIN"
  # 跑一下看能不能执行（架构不匹配 / 文件损坏都会在这里露出来）
  if out=$("$NEWBIN" -listen '' 2>&1); then
    warn "新二进制试跑没报错，有点意外（输出：${out}）"
  else
    case "$out" in
      *监听地址*|*flag*|*Usage*|*usage*) log "新二进制可执行（版本 ${new_version}）";;
      *) die "新二进制跑不起来：${out}" ;;
    esac
  fi
fi

if [ "$DRY_RUN" = 1 ]; then
  printf '   [dry-run] 备份 %s/%s → %s/backups/，然后换成新二进制并重启 %s\n' \
    "$BIN_DIR" "$SERVICE" "$DATA_DIR" "$SERVICE"
  exit 0
fi

# ---------- 4. 停服务、备份、换二进制 ----------
if [ "$NO_SERVICE" = 0 ]; then
  systemctl stop "$SERVICE" 2>/dev/null || warn "停服务失败（可能本来就没在跑）"
fi

install -d -m 0750 "$DATA_DIR/backups"
stamp=$(date +%Y%m%d-%H%M%S)
safe_cur=$(printf '%s' "$cur_version" | tr -c 'A-Za-z0-9._-' '_')
backup="$DATA_DIR/backups/${SERVICE}-${safe_cur}-${stamp}"
if [ -f "$BIN_DIR/$SERVICE" ]; then
  install -m 0755 "$BIN_DIR/$SERVICE" "$backup"
  log "已备份旧二进制 → ${backup}"
else
  warn "没找到正在用的二进制 $BIN_DIR/$SERVICE（这次算全新安装）"
fi

restore() {
  if [ -f "$backup" ]; then
    install -m 0755 "$backup" "$BIN_DIR/$SERVICE"
    # 版本号也得跟着回退，否则数据目录里记的是新版本、跑的却是旧二进制
    if [ "$had_version" = 1 ]; then
      printf '%s\n' "$cur_version" > "$DATA_DIR/VERSION"
    else
      rm -f "$DATA_DIR/VERSION"
    fi
    if [ "$NO_SERVICE" = 0 ]; then
      systemctl start "$SERVICE" 2>/dev/null || true
    fi
    warn "已回滚到旧二进制 ${cur_version}"
  fi
}

install -d -m 0755 "$BIN_DIR"
install -m 0755 "$NEWBIN" "$BIN_DIR/$SERVICE" || { restore; die "写二进制失败"; }

# 顺带把包里的脚本/单元模板/版本号刷新到数据目录，方便以后本地直接用新脚本
pkgdir=$WORK/$(basename "$tarball" .tar.gz)
if [ -d "$pkgdir" ]; then
  install -d -m 0755 "$DATA_DIR/pkg"
  for f in install.sh uninstall.sh update.sh acs.service VERSION; do
    if [ -f "$pkgdir/$f" ]; then
      mode=0644; case "$f" in *.sh) mode=0755;; esac
      install -m "$mode" "$pkgdir/$f" "$DATA_DIR/pkg/$f"
    fi
  done
  [ -f "$pkgdir/VERSION" ] && install -m 0644 "$pkgdir/VERSION" "$DATA_DIR/VERSION"
  log "脚本与单元模板已刷新到 ${DATA_DIR}/pkg"
fi

# ---------- 5. 起服务 + 健康检查 ----------
WEB=$(envval ACS_WEB_LISTEN); LIS=$(envval ACS_LISTEN)
if [ "$NO_SERVICE" = 0 ]; then
  systemctl daemon-reload
  systemctl start "$SERVICE" || { restore; die "服务起不来，已回滚（看 journalctl -u $SERVICE -n 50）"; }
  sleep 2
  if command -v curl >/dev/null 2>&1; then
    port=$(port_of "${WEB:-${LIS:-:7547}}")
    [ -n "$port" ] || port=7547
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${port}/" || true)
    case "$code" in
      2*|3*|401|403) log "服务健康（HTTP ${code}）";;
      *) restore; die "新版本起来了但面板没响应（HTTP ${code:-无响应}），已回滚" ;;
    esac
  fi
fi

# ---------- 6. 清理旧备份 ----------
{ ls -1t "$DATA_DIR"/backups/${SERVICE}-* 2>/dev/null || true; } |
  tail -n +$((KEEP_BACKUPS + 1)) | while read -r old; do
    rm -f "$old"
  done

log "升级完成：${cur_version} → ${new_version}"
echo "   看状态： systemctl status ${SERVICE}"
echo "   看日志： journalctl -u ${SERVICE} -f"
