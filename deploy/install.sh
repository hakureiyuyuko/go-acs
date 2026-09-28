#!/usr/bin/env bash
# 轻量 TR-069 ACS —— 安装脚本（systemd）
#
# 在解压出来的目录里直接运行：
#   sudo ./install.sh                          # 默认装到 /usr/local/bin，数据放 /var/lib/acs
#   sudo ./install.sh --web-listen :8080       # 面板单独端口（面板和 CWMP 分开）
#   sudo ACS_WEB_USER=admin ACS_WEB_PASS=xxx ./install.sh   # 顺便设面板账号密码
#
# 脚本可重复运行：再跑一次就是原地升级（保留数据库与配置文件）。
set -euo pipefail

# ---------- 默认值（都能用参数或环境变量覆盖）----------
SERVICE=${ACS_SERVICE_NAME:-acs}
BIN_DIR=${ACS_BIN_DIR:-/usr/local/bin}
DATA_DIR=${ACS_DATA_DIR:-/var/lib/acs}
ENV_FILE=${ACS_ENV_FILE:-/etc/default/acs}
UNIT_DIR=${ACS_UNIT_DIR:-/etc/systemd/system}
SVC_USER=${ACS_SERVICE_USER:-acs}
SVC_GROUP=${ACS_SERVICE_GROUP:-$SVC_USER}

LISTEN=${ACS_LISTEN:-:7547}
WEB_LISTEN=${ACS_WEB_LISTEN:-}
WEB_USER=${ACS_WEB_USER:-}
WEB_PASS=${ACS_WEB_PASS:-}
CPE_USER=${ACS_USER:-}
CPE_PASS=${ACS_PASSWORD:-}

BINARY=
NO_SERVICE=0
FORCE=0
DRY_RUN=0

HERE=$(cd "$(dirname "$0")" && pwd)
PKG_VERSION=$(cat "$HERE/VERSION" 2>/dev/null || echo unknown)

log()  { printf '\033[32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m警告:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m错误:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<EOF
用法: sudo ./install.sh [选项]

  --listen ADDR         CWMP（TR-069）监听地址，默认 ${LISTEN}
  --web-listen ADDR     面板监听地址；留空 = 与 CWMP 同一个端口
  --web-user NAME       面板账号（只在数据库里还没设置过时生效一次）
  --web-pass PASS       面板密码（同上；也可以装完在面板「设置」页里改）
  --cpe-user NAME       CPE 侧的 HTTP 认证账号（一般不用开）
  --cpe-pass PASS       CPE 侧的 HTTP 认证密码
  --bin-dir DIR         二进制安装目录，默认 ${BIN_DIR}
  --data-dir DIR        数据目录（数据库、备份），默认 ${DATA_DIR}
  --env-file FILE       配置文件，默认 ${ENV_FILE}
  --unit-dir DIR        systemd 单元目录，默认 ${UNIT_DIR}
  --service-name NAME   服务名，默认 ${SERVICE}
  --service-user USER   以哪个用户运行，默认 ${SVC_USER}
  --binary FILE         不用同目录的 acs 二进制，改用这个
  --no-service          只放文件、不碰 systemd（容器里或手工管理时用）
  --force               覆盖已存在的配置文件
  --dry-run             只打印要做什么，不实际改系统
  -h, --help            看这个帮助
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --listen)       [ $# -ge 2 ] || die "--listen 缺参数"; LISTEN=$2; shift 2;;
    --web-listen)   [ $# -ge 2 ] || die "--web-listen 缺参数"; WEB_LISTEN=$2; shift 2;;
    --web-user)     [ $# -ge 2 ] || die "--web-user 缺参数"; WEB_USER=$2; shift 2;;
    --web-pass)     [ $# -ge 2 ] || die "--web-pass 缺参数"; WEB_PASS=$2; shift 2;;
    --cpe-user)     [ $# -ge 2 ] || die "--cpe-user 缺参数"; CPE_USER=$2; shift 2;;
    --cpe-pass)     [ $# -ge 2 ] || die "--cpe-pass 缺参数"; CPE_PASS=$2; shift 2;;
    --bin-dir)      [ $# -ge 2 ] || die "--bin-dir 缺参数"; BIN_DIR=$2; shift 2;;
    --data-dir)     [ $# -ge 2 ] || die "--data-dir 缺参数"; DATA_DIR=$2; shift 2;;
    --env-file)     [ $# -ge 2 ] || die "--env-file 缺参数"; ENV_FILE=$2; shift 2;;
    --unit-dir)     [ $# -ge 2 ] || die "--unit-dir 缺参数"; UNIT_DIR=$2; shift 2;;
    --service-name) [ $# -ge 2 ] || die "--service-name 缺参数"; SERVICE=$2; shift 2;;
    --service-user) [ $# -ge 2 ] || die "--service-user 缺参数"; SVC_USER=$2; SVC_GROUP=$2; shift 2;;
    --binary)       [ $# -ge 2 ] || die "--binary 缺参数"; BINARY=$2; shift 2;;
    --no-service)   NO_SERVICE=1; shift;;
    --force)        FORCE=1; shift;;
    --dry-run)      DRY_RUN=1; shift;;
    -h|--help)      usage; exit 0;;
    *)              die "未知参数：$1（--help 看用法）";;
  esac
done

[ -n "$BINARY" ] || BINARY="$HERE/acs"
[ -f "$BINARY" ] || die "找不到二进制 $BINARY —— 请在解压后的目录里运行，或用 --binary 指定路径"
[ -x "$BINARY" ] || chmod +x "$BINARY"

if [ "$NO_SERVICE" = 0 ] && [ ! -f "$HERE/acs.service" ]; then
  die "同目录缺少 acs.service（systemd 单元模板）；若想手工管理服务请加 --no-service"
fi

# 需要 root：默认路径都在系统目录下，且要动 systemd。手工指定了可写目录时可以不用 root。
if [ "$DRY_RUN" != 1 ] && [ "$(id -u)" != 0 ] && [ "$NO_SERVICE" = 0 ]; then
  die "需要 root 权限，请用 sudo 运行（或加 --no-service 只放文件）"
fi

run() {
  if [ "$DRY_RUN" = 1 ]; then
    printf '   [dry-run] %s\n' "$*"
  else
    "$@"
  fi
}

# 把 stdin 写进文件（带权限），dry-run 时不落盘
write_file() {
  local path=$1 mode=$2 owner=${3:-} ; local tmp; tmp=$(mktemp)
  cat > "$tmp"
  if [ "$DRY_RUN" = 1 ]; then
    printf '   [dry-run] 写入 %s（权限 %s）:\n' "$path" "$mode"
    sed 's/^/       /' "$tmp"
    rm -f "$tmp"
    return 0
  fi
  install -d -m 0755 "$(dirname "$path")"
  if [ -n "$owner" ]; then
    install -m "$mode" -o "$owner" -g "$SVC_GROUP" "$tmp" "$path"
  else
    install -m "$mode" "$tmp" "$path"
  fi
  rm -f "$tmp"
}

# 读配置里生效的值（装了之后才知道最终监听在哪）
eff() { grep -E "^$1=" "$ENV_FILE" 2>/dev/null | tail -1 | cut -d= -f2- ; }
port_of() {
  local addr=${1##*:}
  case "$addr" in ''|*[!0-9]*) echo "" ;; *) echo "$addr" ;; esac
}

log "轻量 TR-069 ACS 安装（版本 ${PKG_VERSION}）"
log "二进制 → ${BIN_DIR}/${SERVICE}   数据 → ${DATA_DIR}   配置 → ${ENV_FILE}"

# ---------- 1. 服务用户 ----------
if [ "$NO_SERVICE" = 0 ]; then
  if id -u "$SVC_USER" >/dev/null 2>&1; then
    log "用户 ${SVC_USER} 已存在，继续"
  elif [ "$DRY_RUN" = 1 ]; then
    printf '   [dry-run] useradd --system --home-dir %s --shell /usr/sbin/nologin %s\n' "$DATA_DIR" "$SVC_USER"
  else
    useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin \
            --comment "TR-069 ACS" "$SVC_USER" \
      || warn "创建用户 ${SVC_USER} 失败（如果只是想升级，可以忽略）"
  fi
fi

# ---------- 2. 二进制 ----------
run install -d -m 0755 "$BIN_DIR"
run install -m 0755 "$BINARY" "$BIN_DIR/$SERVICE"

# ---------- 3. 数据目录 ----------
if [ "$DRY_RUN" = 1 ]; then
  printf '   [dry-run] install -d -m 0750 %s（属主 %s）\n' "$DATA_DIR" "$SVC_USER"
elif id -u "$SVC_USER" >/dev/null 2>&1; then
  install -d -m 0750 -o "$SVC_USER" -g "$SVC_GROUP" "$DATA_DIR"
else
  install -d -m 0750 "$DATA_DIR"
fi
if [ -f "$HERE/VERSION" ]; then
  write_file "$DATA_DIR/VERSION" 0644 < "$HERE/VERSION"
fi

# ---------- 4. 配置文件 ----------
if [ -f "$ENV_FILE" ] && [ "$FORCE" != 1 ]; then
  log "保留已存在的配置 ${ENV_FILE}（要覆盖请加 --force）"
else
  write_file "$ENV_FILE" 0600 <<EOF
# 轻量 TR-069 ACS 配置（systemd 的 EnvironmentFile）
# 改完执行：sudo systemctl restart ${SERVICE}
# 环境变量名与命令行参数一一对应，含义见 README「配置」一节；
# 只有非空值才生效（留空 = 用程序内置默认）。
ACS_LISTEN=${LISTEN}
ACS_WEB_LISTEN=${WEB_LISTEN}
ACS_DB=${DATA_DIR}/acs.db
# 面板账号密码：首次启动种进数据库，之后以面板「设置」页为准
ACS_WEB_USER=${WEB_USER}
ACS_WEB_PASS=${WEB_PASS}
# CPE 侧 HTTP 认证（一般留空）
ACS_USER=${CPE_USER}
ACS_PASSWORD=${CPE_PASS}
ACS_LOG_LEVEL=info
EOF
fi

# ---------- 5. systemd 单元 ----------
if [ "$NO_SERVICE" = 0 ]; then
  unit_tmp=$(mktemp)
  sed -e "s|@BIN@|${BIN_DIR}/${SERVICE}|g" \
      -e "s|@DATA@|${DATA_DIR}|g" \
      -e "s|@USER@|${SVC_USER}|g" \
      -e "s|@GROUP@|${SVC_GROUP}|g" \
      -e "s|@ENVFILE@|${ENV_FILE}|g" \
      "$HERE/acs.service" > "$unit_tmp"
  if [ "$DRY_RUN" = 1 ]; then
    printf '   [dry-run] 写入 %s/%s.service:\n' "$UNIT_DIR" "$SERVICE"
    sed 's/^/       /' "$unit_tmp"
    rm -f "$unit_tmp"
  else
    install -d -m 0755 "$UNIT_DIR"
    install -m 0644 "$unit_tmp" "$UNIT_DIR/$SERVICE.service"
    rm -f "$unit_tmp"
  fi
fi

# ---------- 6. 起服务 ----------
if [ "$NO_SERVICE" = 0 ]; then
  run systemctl daemon-reload
  run systemctl enable "$SERVICE"
  run systemctl restart "$SERVICE"

  if [ "$DRY_RUN" = 0 ]; then
    sleep 1
    LISTEN_EFF=$(eff ACS_LISTEN); WEB_EFF=$(eff ACS_WEB_LISTEN)
    PANEL_PORT=$(port_of "${WEB_EFF:-${LISTEN_EFF:-$LISTEN}}")
    CWMP_PORT=$(port_of "${LISTEN_EFF:-$LISTEN}")
    OK=""
    if command -v curl >/dev/null 2>&1 && [ -n "$PANEL_PORT" ]; then
      code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${PANEL_PORT}/" || true)
      case "$code" in
        2*|3*|401|403) OK=yes ;;
      esac
      if [ -z "$OK" ]; then
        warn "面板没有正常响应（HTTP ${code:-无响应}），看日志：journalctl -u ${SERVICE} -n 30 --no-pager"
      fi
    fi
    if [ -z "$OK" ]; then
      warn "服务状态：$(systemctl is-active "$SERVICE" 2>/dev/null || echo unknown)"
      warn "排查：systemctl status ${SERVICE} / journalctl -u ${SERVICE} -n 50 --no-pager"
      exit 1
    fi
  fi
fi

IP=$(hostname -I 2>/dev/null | awk '{print $1}')
[ -n "${IP:-}" ] || IP="<本机IP>"
LISTEN_EFF=${LISTEN_EFF:-$(eff ACS_LISTEN)}; WEB_EFF=${WEB_EFF:-$(eff ACS_WEB_LISTEN)}
PANEL_PORT=${PANEL_PORT:-$(port_of "${WEB_EFF:-${LISTEN_EFF:-$LISTEN}}")}
CWMP_PORT=${CWMP_PORT:-$(port_of "${LISTEN_EFF:-$LISTEN}")}

cat <<EOF

$(log "装好了（版本 ${PKG_VERSION}）")
  面板      http://${IP}${PANEL_PORT:+:${PANEL_PORT}}/
  CWMP 端点 http://${IP}${CWMP_PORT:+:${CWMP_PORT}}/acs     ← 光猫的 ACS URL 填这个
EOF

if [ "$NO_SERVICE" = 1 ]; then
  cat <<EOF
  服务      没托管（--no-service），手工启动：
            set -a; . ${ENV_FILE}; set +a; ${BIN_DIR}/${SERVICE} -db ${DATA_DIR}/acs.db
EOF
else
  cat <<EOF
  服务      systemctl status|restart ${SERVICE}   日志：journalctl -u ${SERVICE} -f
EOF
fi

cat <<EOF
  升级      sudo ./update.sh          卸载：sudo ./uninstall.sh
EOF
