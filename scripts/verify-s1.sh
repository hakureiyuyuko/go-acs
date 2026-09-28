#!/usr/bin/env bash
# S1 验收脚本：编译真实的二进制、起真实的 HTTP + SQLite，用真实的报文跑一遍。
#
# 用法: scripts/verify-s1.sh
# 依赖: go、python3
set -uo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)

# 找一个空闲端口
PORT=${ACS_TEST_PORT:-17547}
WORK=$(mktemp -d "${TMPDIR:-/tmp}/acs-verify-XXXXXX")
DB="$WORK/acs.db"
LOG="$WORK/acs.log"

if ! command -v go >/dev/null 2>&1; then
  export PATH="$HOME/.local/go/bin:$PATH"
fi

cleanup() {
  if [ -n "${ACSPID:-}" ]; then
    kill "$ACSPID" 2>/dev/null
    wait "$ACSPID" 2>/dev/null
  fi
  if [ "${KEEP:-0}" != "1" ]; then rm -rf "$WORK"; fi
}
trap cleanup EXIT

echo "== 编译 =="
export CGO_ENABLED=0
go build -o "$WORK/acs" ./cmd/acs || exit 1
go build -o "$WORK/cpesim" ./test/cpesim || exit 1
echo "  到 $WORK"

echo "== 起 ACS（端口 $PORT，库 $DB）=="
ACS_LISTEN=":$PORT" ACS_DB="$DB" ACS_LOG_LEVEL=info \
  "$WORK/acs" >"$LOG" 2>&1 &
ACSPID=$!

# 等服务起来（最多 10 秒）
for i in $(seq 1 50); do
  if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/" 2>/dev/null; then break; fi
  sleep 0.2
done
if ! curl -fsS -o /dev/null "http://127.0.0.1:$PORT/" 2>/dev/null; then
  echo "ACS 没能起来，日志："
  cat "$LOG"
  exit 1
fi
echo "  已就绪"

echo
python3 "$ROOT/scripts/verify_s1.py" "http://127.0.0.1:$PORT" "$WORK"
RC=$?

echo
if [ "$RC" != "0" ]; then
  echo "== ACS 日志（末尾 60 行）=="
  tail -n 60 "$LOG"
else
  echo "== ACS 日志（末尾 15 行）=="
  tail -n 15 "$LOG"
fi
echo
if [ "$RC" = "0" ]; then
  echo "S1 验收全部通过"
else
  echo "S1 验收存在失败项"
fi
exit $RC
