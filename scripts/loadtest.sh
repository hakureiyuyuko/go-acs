#!/usr/bin/env bash
# 压测：模拟「大规模断电恢复」——N 台设备同时发 1 BOOT 冲向 ACS。
#
#   scripts/loadtest.sh                 # 默认阶梯 10 / 50 / 100 / 200 / 400
#   scripts/loadtest.sh -n 200          # 只跑一档
#   scripts/loadtest.sh -N "25 75 150"  # 自定义阶梯
#   scripts/loadtest.sh -n 200 -e 4484  # 指定每台设备的参数规模
#
# 设备模板默认照真机华为 V271-20（FTTR 主机）：TR-098、FTTR 子设备 3 台（光纤组网）、
# X_HW_APDevice 子树下额外 4484 个参数（真机实测就是 4484）—— 纳管时 ACS 要枚举、
# 取值、写库的正是这棵树。想换规模调 -e。
#
# 每档都用**全新的库和实例**（冷启动 = 所有设备都是新设备，最坏情况）。
# 结果与日志落在 -o 指定的目录（默认 ~/Desktop/TEMP/loadtest/<时间戳>）。
set -uo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)

TAG=$(date +%Y%m%d-%H%M%S)
OUT=${ACS_LOADTEST_OUT:-$HOME/Desktop/TEMP/loadtest/$TAG}
N_LIST="10 50 100 200 400"
EXTRA=4484          # 每台设备在 X_HW_APDevice 下的额外参数个数（真机 V271-20 = 4484）
FTTR=3              # FTTR 子设备数（真机 V271-20 带 3 台）
PORT=19900
LIMIT=600           # 单档最长等待秒数
AEXTRA=""           # 额外透传给 ACS 的环境变量，如 -E "ACS_DB_MAX_CONNS=4"
KEEP=0              # 1 = 保留每档的库

while [ $# -gt 0 ]; do
  case "$1" in
    -n) N_LIST="$2"; shift 2;;
    -N) N_LIST="$2"; shift 2;;
    -e) EXTRA="$2"; shift 2;;
    -f) FTTR="$2"; shift 2;;
    -o) OUT="$2"; shift 2;;
    -p) PORT="$2"; shift 2;;
    -t) LIMIT="$2"; shift 2;;
    -E) AEXTRA="$2"; shift 2;;
    -k) KEEP=1; shift;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) echo "未知参数：$1（-h 看用法）" >&2; exit 2;;
  esac
done

if ! command -v go >/dev/null 2>&1; then
  export PATH="$HOME/.local/go/bin:$PATH"
fi
mkdir -p "$OUT" || exit 1

echo "== 编译 =="
CGO_ENABLED=0 go build -o "$OUT/acs" ./cmd/acs || exit 1
CGO_ENABLED=0 go build -o "$OUT/cpesim" ./test/cpesim || exit 1

printf '%s\n' "档位 | 成功 | 总耗时 | 中位耗时 | 最慢 | ACS CPU(s) | ACS 峰值RSS | 错误/警告 | 设备 | 参数行 | 库大小" > "$OUT/summary.txt"
echo
printf '%-6s %-12s %-12s %-9s %-9s %-9s %-11s %-9s %-8s %-8s %-6s %-9s %s\n' \
  台数 成功 总耗时 中位 最慢 ACS_CPU 模拟器_CPU ACS_峰值RSS 错误_警告 下发任务 设备 参数行 库大小
echo "---------------------------------------------------------------------------------------"

for N in $(echo "$N_LIST" | tr ',' ' '); do
  W="$OUT/n$N"
  rm -rf "$W"; mkdir -p "$W"
  DB="$W/acs.db"
  ACSPORT=$((PORT + N % 90))
  LOG="$W/acs.log"
  SIMLOG="$W/sim.log"
  RSSFILE="$W/rss.txt"

  # 干净的实例：全新库 = 所有设备都是新设备
  # 注意：额外变量要走 env —— 展开出来的 KEY=VALUE 不会被 bash 当成赋值语句
  # shellcheck disable=SC2086  # AEXTRA 就是要按空格拆成多个 KEY=VALUE
  env ACS_DB="$DB" ACS_LISTEN=":$ACSPORT" ACS_LOG_LEVEL=info ACS_WEB_AUTH=off \
      ACS_TASK_HISTORY_LIMIT=500 ACS_INFORM_HISTORY_LIMIT=500 $AEXTRA \
      "$OUT/acs" > "$LOG" 2>&1 &
  ACSPID=$!

  # 就绪
  ready=0
  for _ in $(seq 1 60); do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:$ACSPORT/" || true)
    if [ "$code" = "200" ]; then ready=1; break; fi
    sleep 0.2
  done
  if [ "$ready" != 1 ]; then
    echo "  实例没起来（端口 $ACSPORT），跳过这一档"
    kill "$ACSPID" 2>/dev/null
    continue
  fi

  # 采样 ACS 的峰值 RSS
  : > "$RSSFILE"
  ( while kill -0 "$ACSPID" 2>/dev/null; do
      rss=$(awk '/VmRSS/{print $2}' "/proc/$ACSPID/status" 2>/dev/null)
      [ -n "${rss:-}" ] && echo "$rss" >> "$RSSFILE"
      sleep 0.5
    done ) &
  SAMPLER=$!

  CPU0=$(awk '{print $14+$15}' "/proc/$ACSPID/stat" 2>/dev/null)

  echo "== $N 台设备同时上报（V271-20 模板：FTTR $FTTR 台 + X_HW_APDevice 额外 $EXTRA 参数）=="
  t0=$(date +%s.%N)
  /usr/bin/time -f "%U %S" -o "$W/simcpu.txt" \
    timeout "$LIMIT" "$OUT/cpesim" -acs "http://127.0.0.1:$ACSPORT/acs" \
      -serial LOAD -count "$N" -once -event "1 BOOT" \
      -dm 098 -model V271-20 -manufacturer "Huawei Technologies Co., Ltd." \
      -product-class V271-20 -oui 00259E \
      -fttr "$FTTR" -fttr-optical -extra-params "$EXTRA" \
      > "$SIMLOG" 2>&1
  simrc=$?
  t1=$(date +%s.%N)

  # 给 ACS 一点时间收尾（会话关闭、写库落盘）
  sleep 5

  CPU1=$(awk '{print $14+$15}' "/proc/$ACSPID/stat" 2>/dev/null)
  # 注意 awk 里 printf 后面的 ">" 会被当成输出重定向，算术要先括号起来
  CPU=$(awk -v a="${CPU0:-0}" -v b="${CPU1:-0}" 'BEGIN{ d=b-a; if (d<0) d=0; printf "%.1f", d/100 }')
  RSSMAX=$(sort -n "$RSSFILE" 2>/dev/null | tail -1)
  RSSMAX=$(( ${RSSMAX:-0} / 1024 ))
  DEVN=$(python3 -c "
import sqlite3,sys
try:
    db=sqlite3.connect('$DB')
    print(db.execute('select count(*) from devices').fetchone()[0], db.execute('select count(*) from device_params').fetchone()[0])
except Exception:
    print('0 0')" 2>/dev/null)
  DEV=$(echo "$DEVN" | awk '{print $1}')
  PARAMS=$(echo "$DEVN" | awk '{print $2}')
  DBSIZE=$(du -h "$DB" 2>/dev/null | cut -f1)
  ERRS=$(grep -c 'level=ERROR' "$LOG" 2>/dev/null)
  WARNS=$(grep -c 'level=WARN' "$LOG" 2>/dev/null)
  SIMCPU=$(awk '{printf "%.1f", $1+$2}' "$W/simcpu.txt" 2>/dev/null)
  DISPATCH=$(grep -c '下发任务' "$LOG" 2>/dev/null)
  LOCKED=$(grep -c '会话被占用' "$LOG" 2>/dev/null)
  result=$(grep "压测结果" "$SIMLOG" | tail -1 | sed 's/^.*压测结果：//')
  ok=$(echo "$result" | awk -F'｜' '{print $1}')
  total=$(echo "$result" | awk -F'｜' '{print $2}')
  med=$(echo "$result" | awk -F'｜' '{print $3}' | sed 's/.*中位 //;s/ \/.*//')
  worst=$(echo "$result" | awk -F'｜' '{print $4}' | sed 's/.*最慢 //')
  if [ "$simrc" = "124" ]; then total="${total:-超时}(被 $LIMIT 秒上限掐断)"; fi
  [ -z "${ok:-}" ] && ok="(没跑出结果)"

  printf '%-6s %-12s %-12s %-9s %-9s %-9s %-11s %-9s %-8s %-8s %-6s %-9s %s\n' \
    "$N" "${ok:-?}" "${total:-?}" "${med:-?}" "${worst:-?}" "${CPU}s" "${SIMCPU:-?}s" \
    "${RSSMAX}MB" "$ERRS/$WARNS" "${DISPATCH:-0}" "$DEV" "$PARAMS" "$DBSIZE"
  printf '%s\n' "$N | $ok | $total | $med | $worst | ACS ${CPU}s | 模拟器 ${SIMCPU}s | ${RSSMAX}MB | 错误/警告 $ERRS/$WARNS | 下发 $DISPATCH | 被占用 $LOCKED | 设备 $DEV | 参数行 $PARAMS | $DBSIZE" >> "$OUT/summary.txt"

  kill "$SAMPLER" 2>/dev/null
  kill "$ACSPID" 2>/dev/null
  wait "$ACSPID" 2>/dev/null
  if [ "$KEEP" != 1 ]; then rm -f "$DB" "$DB-wal" "$DB-shm"; fi
  echo
done

echo "== 明细在 $OUT（summary.txt + 每档的 acs.log / sim.log）=="
cat "$OUT/summary.txt"
