#!/usr/bin/env bash
# 手动验收：改监听地址 →「立即重启服务」真的换进程 + 失败时回滚
# 用独立临时实例与临时库，不碰开发实例（:9090）。
set -uo pipefail
cd "$(dirname "$0")/../.."
export PATH="${HOME}/.local/go/bin:$PATH"
export CGO_ENABLED=0

DIR=$(mktemp -d /tmp/acs-restart-XXXX)
DB="$DIR/acs.db"
LOG="$DIR/acs.log"
PID="$DIR/acs.pid"

pass=0; fail=0
ok(){ echo "  [通过] $1"; pass=$((pass+1)); }
no(){ echo "  [失败] $1"; fail=$((fail+1)); }

cleanup(){
  [ -f "$PID" ] && kill "$(cat "$PID")" 2>/dev/null
  pkill -f "acs-restart-" 2>/dev/null
  [ -n "${BUSY_PID:-}" ] && kill "$BUSY_PID" 2>/dev/null
  return 0
}
trap cleanup EXIT

go build -o "$DIR/acs" ./cmd/acs || { echo "编译失败"; exit 1; }

echo "== 1. 起一个临时实例（:19090） =="
setsid env ACS_LISTEN=:19090 ACS_DB="$DB" ACS_PIDFILE="$PID" ACS_LOG_LEVEL=info \
  "$DIR/acs" >"$LOG" 2>&1 &
for _ in $(seq 1 40); do curl -sf -o /dev/null http://127.0.0.1:19090/login && break; sleep 0.25; done
OLD_PID=$(cat "$PID" 2>/dev/null)
[ -n "$OLD_PID" ] && ok "实例起来了 pid=$OLD_PID（pidfile 生效）" || no "实例没起来"

echo "== 2. 保存新的面板监听 :19443 =="
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST -d 'acs_listen=:19090&web_listen=:19443&auth=0' \
  http://127.0.0.1:19090/settings)
[ "$code" = "303" ] && ok "设置保存返回 303" || no "设置保存返回 $code"

html=$(curl -s http://127.0.0.1:19090/settings)
case "$html" in *"立即重启服务"*) ok "设置页出现「立即重启服务」按钮";; *) no "设置页没有重启按钮";; esac
case "$html" in *'action="/settings/restart"'*) ok "按钮指向 /settings/restart";; *) no "按钮地址不对";; esac
case "$html" in *"确定现在重启服务吗"*) ok "按钮带二次确认";; *) no "没有二次确认";; esac

# GET 不该触发重启
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19090/settings/restart)
[ "$code" = "405" ] && ok "GET /settings/restart 是 405（只能 POST）" || no "GET 返回 $code"

echo "== 3. 点「立即重启服务」 =="
body=$(curl -s -X POST http://127.0.0.1:19090/settings/restart)
case "$body" in *"http://127.0.0.1:19443/"*) ok "重启页给出新地址";; *) no "重启页没给出新地址";; esac
case "$body" in *'data-restart-url="http://127.0.0.1:19443/"'*) ok "重启页带自动跳转";; *) no "重启页缺自动跳转";; esac
case "$body" in *"新进程已经起来"*) ok "如实说明新进程已就绪";; *) no "缺少就绪说明";; esac

sleep 1.5
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19443/)
[ "$code" = "200" ] && ok "新地址 :19443 能访问（$code）" || no "新地址访问失败（$code）"
# CWMP 端口必须在**新进程**手里继续听着（设备随时会上报）——句柄交接的意义就在这
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST --data-binary '' http://127.0.0.1:19090/acs)
if [ "$code" != "000" ]; then ok "CWMP 端口 :19090 仍在听（$code）"; else no "CWMP 端口没了"; fi

NEW_PID=$(cat "$PID" 2>/dev/null)
if [ -n "$NEW_PID" ] && [ "$NEW_PID" != "$OLD_PID" ]; then ok "pidfile 已换成新进程 pid=$NEW_PID"; else no "pidfile 没更新（$NEW_PID）"; fi
grep -q "已起新进程接管" "$LOG" && ok "日志记下了换进程" || no "日志没记"
grep -q "接管了上一个进程递过来的监听句柄.*:19090" "$LOG" && ok "新进程记录了接管 CWMP 句柄" || no "没看到接管记录"

echo "== 4. 失败回滚：新端口被占用 =="
python3 -m http.server 19444 --bind 127.0.0.1 >/dev/null 2>&1 &
BUSY_PID=$!
sleep 0.6
BEFORE_PID=$(cat "$PID" 2>/dev/null)
curl -s -o /dev/null -X POST -d 'acs_listen=:19090&web_listen=:19444&auth=0' http://127.0.0.1:19443/settings
loc=$(curl -s -o /dev/null -w '%{redirect_url}' -X POST http://127.0.0.1:19443/settings/restart)
case "$loc" in *err=1*) ok "端口被占时回设置页报错";; *) no "没报错（$loc）";; esac
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19443/)
[ "$code" = "200" ] && ok "失败后面板仍在旧地址上跑" || no "失败后面板挂了（$code）"
AFTER_PID=$(cat "$PID" 2>/dev/null)
[ "$BEFORE_PID" = "$AFTER_PID" ] && ok "失败时没有换进程（还是 pid=$AFTER_PID）" || no "失败却也换了进程"
html=$(curl -s http://127.0.0.1:19443/settings)
case "$html" in *"立即重启服务"*) no "回滚后不该还显示待重启";; *) ok "设置已回滚（不再显示待重启）";; esac

echo "== 5. 修好端口后能再次重启成功 =="
curl -s -o /dev/null -X POST -d 'acs_listen=:19090&web_listen=:19445&auth=0' http://127.0.0.1:19443/settings
curl -s -o /dev/null -X POST http://127.0.0.1:19443/settings/restart
sleep 1.5
code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19445/)
[ "$code" = "200" ] && ok "改到 :19445 后重启成功（$code）" || no "再次重启失败（$code）"

echo "== 6. 重启页的前端跳转（真跑浏览器） =="
CHROME=""
for c in google-chrome google-chrome-stable chromium chromium-browser; do
  command -v "$c" >/dev/null 2>&1 && { CHROME=$c; break; }
done
if [ -z "$CHROME" ]; then
  echo "  [跳过] 没装无头浏览器"
else
  # 拿真实的 app.js + 重启页同款 DOM，看倒计时到了会不会真跳走。
  # 页面本身得从 http 上访问（file:// 拿不到跨来源的脚本）。
  cat >"$DIR/restart-js.html" <<HTML
<!doctype html><html><head><meta charset="utf-8"><title>t</title></head><body>
<div id="restart-box" data-restart-url="http://127.0.0.1:19445/?jumped=1" data-restart-seconds="1">
  <span data-restart-count>1</span></div>
<script src="http://127.0.0.1:19445/static/app.js"></script>
</body></html>
HTML
  python3 -m http.server 19446 --bind 127.0.0.1 --directory "$DIR" >/dev/null 2>&1 &
  HTTPD_PID=$!
  sleep 0.6
  dom=$("$CHROME" --headless=new --no-sandbox --disable-gpu --accept-lang=zh-CN --virtual-time-budget=6000 \
        --dump-dom "http://127.0.0.1:19446/restart-js.html" 2>/dev/null)
  kill $HTTPD_PID 2>/dev/null
  if printf '%s' "$dom" | grep -q "设备列表 · 轻量 TR-069 ACS"; then
    ok "倒计时到点后真的跳到了新地址（浏览器实证）"
  else
    snippet=$(printf '%s' "$dom" | head -c 160)
    no "没跳转（DOM 开头：$snippet）"
  fi
fi

echo
echo "结果：通过 $pass / 失败 $fail"
echo "临时目录：$DIR（日志 $LOG）"
[ "$fail" = "0" ] || exit 1
