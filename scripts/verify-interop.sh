#!/usr/bin/env bash
# 与「独立实现」的互通性验证：用 GenieACS 官方的 CPE 模拟器（JS 写的，跟我们的
# Go 代码完全无关）去打我们的 ACS，看能不能正常纳管并取回设备信息。
#
# 这条验证的价值：只跟自己的模拟器对打，两边可能犯了同样的错（比如都按本地名匹配
# 而不管命名空间）。换一个实现来打，才能证明真的互通。
#
# 前置: scripts/fetch-reference.sh  （需要 node/npm）
# 用法: scripts/verify-interop.sh
set -uo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
SIM="$ROOT/reference/genieacs-sim"

if [ ! -d "$SIM" ]; then
  echo "缺少 $SIM，先跑 scripts/fetch-reference.sh"; exit 2
fi
if ! command -v node >/dev/null 2>&1; then
  echo "需要 node；本机未安装，跳过互通性验证"; exit 2
fi

PORT=${ACS_TEST_PORT:-17560}
WORK=$(mktemp -d "${TMPDIR:-/tmp}/acs-interop-XXXXXX")
export PATH="$HOME/.local/go/bin:$PATH"
export CGO_ENABLED=0

cleanup() {
  [ -n "${ACSPID:-}" ] && { kill "$ACSPID" 2>/dev/null; wait "$ACSPID" 2>/dev/null; }
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "== 编译 =="
go build -o "$WORK/acs" ./cmd/acs || exit 1

# genieacs-sim 依赖 commander
if [ ! -d "$SIM/node_modules/commander" ]; then
  echo "== 安装 genieacs-sim 依赖 =="
  (cd "$SIM" && npm install --no-audit --no-fund --force >/dev/null 2>&1) || {
    echo "npm install 失败，跳过"; exit 2; }
fi

echo "== 起 ACS =="
ACS_LISTEN=":$PORT" ACS_DB="$WORK/acs.db" "$WORK/acs" >"$WORK/acs.log" 2>&1 &
ACSPID=$!
for _ in $(seq 1 50); do
  curl -fsS -o /dev/null "http://127.0.0.1:$PORT/" 2>/dev/null && break
  sleep 0.2
done

echo "== 用 GenieACS 官方模拟器连接 =="
(cd "$SIM" && timeout 15 node ./genieacs-sim -u "http://127.0.0.1:$PORT/acs" -p 1 2>&1 | grep -vE '^$' | head -5)

echo
echo "== 检查我们这边收到了什么 =="
python3 - "$PORT" <<'PY'
import json, sys, urllib.request
port = sys.argv[1]
d = json.load(urllib.request.urlopen(f"http://127.0.0.1:{port}/api/devices"))["data"]
ok = fail = 0
def check(desc, cond, extra=""):
    global ok, fail
    if cond: ok += 1; print("  [通过] " + desc)
    else: fail += 1; print("  [失败] " + desc + ((" -> " + str(extra)) if extra else ""))

check("独立实现的 CPE 被成功纳管", len(d) >= 1, len(d))
if d:
    dev = d[0]
    check("厂商解析正确", "Huawei" in dev["Manufacturer"], dev["Manufacturer"])
    check("型号解析正确", dev["ModelName"] == "BM632w", dev["ModelName"])
    check("OUI 解析正确", dev["OUI"] == "202BC1", dev["OUI"])
    check("软件版本解析正确", dev["SoftwareVersion"].startswith("V100R001"), dev["SoftwareVersion"])
    check("数据模型根探测正确", dev["DataModelRoot"] == "InternetGatewayDevice.", dev["DataModelRoot"])
    check("取回参数数 >= 14", dev["ParamCount"] >= 14, dev["ParamCount"])
    check("设备在线", dev["Online"] is True)
print(f"结果：通过 {ok} / 失败 {fail}")
sys.exit(1 if fail else 0)
PY
RC=$?

echo
echo "== ACS 日志 =="
grep -E "收到 Inform|入队|下发|取回|WARN|ERROR" "$WORK/acs.log" | head -12
exit $RC
