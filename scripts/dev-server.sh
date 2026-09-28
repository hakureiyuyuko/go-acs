#!/usr/bin/env bash
# 本机开发/联调用：把 ACS 以后台方式跑起来（带 pidfile），方便真机随时上报。
#
# 用法:
#   scripts/dev-server.sh start      # 编译并后台启动
#   scripts/dev-server.sh stop
#   scripts/dev-server.sh restart
#   scripts/dev-server.sh status
#   scripts/dev-server.sh log        # 跟踪日志
#
# 环境变量:
#   ACS_PORT       默认 9090
#   ACS_DB         默认 <repo>/data/acs.db
#   ACS_LOG_LEVEL  默认 info（联调排障用 debug）
#   ACS_LOG_SOAP   设 1 会在日志里打印原始 SOAP 报文（排障用，日志会变大）
#   ACS_USER / ACS_PASSWORD  启用 CPE 认证
set -uo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)

# 本机的 Go 装在 ~/.local/go（见 README）
if ! command -v go >/dev/null 2>&1; then
  export PATH="$HOME/.local/go/bin:$PATH"
fi

PORT="${ACS_PORT:-9090}"
DB="${ACS_DB:-$ROOT/data/acs.db}"
LOGFILE="${ACS_LOGFILE:-$ROOT/data/acs.log}"
PIDFILE="$ROOT/data/acs.pid"
BIN="$ROOT/bin/acs"
LEVEL="${ACS_LOG_LEVEL:-info}"

mkdir -p "$(dirname "$DB")"

running() {
  [ -f "$PIDFILE" ] || return 1
  local p
  p=$(cat "$PIDFILE" 2>/dev/null)
  [ -n "$p" ] && kill -0 "$p" 2>/dev/null
}

do_build() {
  echo "编译中..."
  (cd "$ROOT" && CGO_ENABLED=0 go build -o "$BIN" ./cmd/acs) || { echo "编译失败"; exit 1; }
}

do_start() {
  if running; then
    echo "已经在运行 pid=$(cat "$PIDFILE")"
    return 0
  fi
  do_build
  : >"$LOGFILE"
  # setsid：脱离当前会话，父进程退出后不会被连带杀掉
  setsid env \
    ACS_LISTEN=":$PORT" \
    ACS_DB="$DB" \
    ACS_LOG_LEVEL="$LEVEL" \
    ACS_LOG_SOAP="${ACS_LOG_SOAP:-0}" \
    ACS_USER="${ACS_USER:-}" \
    ACS_PASSWORD="${ACS_PASSWORD:-}" \
    "$BIN" </dev/null >"$LOGFILE" 2>&1 &
  sleep 1

  local p
  p=$(pgrep -x acs | head -1)
  if [ -z "$p" ]; then
    echo "启动失败，日志："
    tail -n 20 "$LOGFILE"
    return 1
  fi
  echo "$p" >"$PIDFILE"
  echo "已启动 pid=$p"
  echo "  CWMP 端点   http://<本机IP>:$PORT/acs  与  http://<本机IP>:$PORT/"
  echo "  界面        http://127.0.0.1:$PORT/"
  echo "  数据库      $DB"
  echo "  日志        $LOGFILE"
  local ip
  ip=$(ip -4 -brief addr show scope global 2>/dev/null | awk '{print $3}' | cut -d/ -f1 | head -1)
  [ -n "$ip" ] && echo "  本机 IP     $ip  ->  给 CPE 配 http://$ip:$PORT/"
}

do_stop() {
  if running; then
    local p
    p=$(cat "$PIDFILE")
    kill "$p" 2>/dev/null
    for _ in $(seq 1 25); do kill -0 "$p" 2>/dev/null || break; sleep 0.2; done
    kill -0 "$p" 2>/dev/null && kill -9 "$p" 2>/dev/null
    echo "已停止 pid=$p"
  else
    echo "没在运行"
  fi
  rm -f "$PIDFILE"
}

case "${1:-status}" in
  start)   do_start ;;
  stop)    do_stop ;;
  restart) do_stop; do_start ;;
  status)
    if running; then
      echo "运行中 pid=$(cat "$PIDFILE")"
      echo "最近 5 条日志："
      tail -n 5 "$LOGFILE" | cut -c1-160
    else
      echo "未运行"
    fi
    ;;
  log)     tail -f "$LOGFILE" ;;
  *)
    echo "用法: $0 {start|stop|restart|status|log}"; exit 2 ;;
esac
