#!/usr/bin/env bash
# 轻量 TR-069 ACS —— 卸载脚本
#
# 默认只拆服务与二进制，**保留数据与配置**（免得误删设备记录和面板账号）：
#   sudo ./uninstall.sh
# 连数据一起清掉：
#   sudo ./uninstall.sh --purge
set -euo pipefail

SERVICE=${ACS_SERVICE_NAME:-acs}
BIN_DIR=${ACS_BIN_DIR:-/usr/local/bin}
DATA_DIR=${ACS_DATA_DIR:-/var/lib/acs}
ENV_FILE=${ACS_ENV_FILE:-/etc/default/acs}
UNIT_DIR=${ACS_UNIT_DIR:-/etc/systemd/system}
SVC_USER=${ACS_SERVICE_USER:-acs}

PURGE=0
NO_SERVICE=0
DRY_RUN=0

log()  { printf '\033[32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m警告:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m错误:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<EOF
用法: sudo ./uninstall.sh [选项]

  --purge            连数据库、配置、服务用户一起删（默认保留）
  --bin-dir DIR      二进制目录，默认 ${BIN_DIR}
  --data-dir DIR     数据目录，默认 ${DATA_DIR}
  --env-file FILE    配置文件，默认 ${ENV_FILE}
  --unit-dir DIR     systemd 单元目录，默认 ${UNIT_DIR}
  --service-name NAME  服务名，默认 ${SERVICE}
  --service-user USER  服务用户，默认 ${SVC_USER}
  --no-service       别碰 systemd（容器里或手工管理时用）
  --dry-run          只打印要做什么
  -h, --help         看这个帮助
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --purge)        PURGE=1; shift;;
    --bin-dir)      [ $# -ge 2 ] || die "--bin-dir 缺参数"; BIN_DIR=$2; shift 2;;
    --data-dir)     [ $# -ge 2 ] || die "--data-dir 缺参数"; DATA_DIR=$2; shift 2;;
    --env-file)     [ $# -ge 2 ] || die "--env-file 缺参数"; ENV_FILE=$2; shift 2;;
    --unit-dir)     [ $# -ge 2 ] || die "--unit-dir 缺参数"; UNIT_DIR=$2; shift 2;;
    --service-name) [ $# -ge 2 ] || die "--service-name 缺参数"; SERVICE=$2; shift 2;;
    --service-user) [ $# -ge 2 ] || die "--service-user 缺参数"; SVC_USER=$2; shift 2;;
    --no-service)   NO_SERVICE=1; shift;;
    --dry-run)      DRY_RUN=1; shift;;
    -h|--help)      usage; exit 0;;
    *)              die "未知参数：$1（--help 看用法）";;
  esac
done

if [ "$DRY_RUN" != 1 ] && [ "$(id -u)" != 0 ] && [ "$NO_SERVICE" = 0 ]; then
  die "需要 root 权限，请用 sudo 运行（或加 --no-service）"
fi

run() {
  if [ "$DRY_RUN" = 1 ]; then printf '   [dry-run] %s\n' "$*"; else "$@"; fi
}

if [ "$PURGE" = 1 ]; then
  log "卸载 ${SERVICE}（数据目录 ${DATA_DIR} 会一起删除）"
else
  log "卸载 ${SERVICE}（保留数据目录 ${DATA_DIR}）"
fi

if [ "$NO_SERVICE" = 0 ]; then
  if systemctl list-unit-files "${SERVICE}.service" >/dev/null 2>&1; then
    run systemctl disable --now "$SERVICE" || warn "停服务时出错，继续清理"
  fi
fi

if [ -f "$UNIT_DIR/${SERVICE}.service" ]; then
  run rm -f "$UNIT_DIR/${SERVICE}.service"
  if [ "$NO_SERVICE" = 0 ]; then
    run systemctl daemon-reload
  fi
fi

if [ -f "$BIN_DIR/$SERVICE" ]; then
  run rm -f "$BIN_DIR/$SERVICE"
else
  warn "没找到 $BIN_DIR/$SERVICE"
fi

if [ "$PURGE" = 1 ]; then
  run rm -rf "$DATA_DIR"
  run rm -f "$ENV_FILE"
  if id -u "$SVC_USER" >/dev/null 2>&1; then
    if [ "$DRY_RUN" = 1 ]; then
      printf '   [dry-run] userdel %s\n' "$SVC_USER"
    else
      userdel "$SVC_USER" 2>/dev/null || warn "删除用户 ${SVC_USER} 失败（有进程占用？）"
    fi
  fi
  log "已连数据与配置一起删除"
else
  cat <<EOF

==> 服务与二进制已移除，下面这些还在（要一起删就加 --purge）：
    数据库与备份  ${DATA_DIR}
    配置文件      ${ENV_FILE}
EOF
  if id -u "$SVC_USER" >/dev/null 2>&1; then
    echo "    服务用户      ${SVC_USER}"
  fi
  echo
fi
