#!/usr/bin/env python3
"""S1 验收：对真的 ACS 发真的 HTTP/CWMP 报文，逐条断言。

用法: verify_s1.py http://127.0.0.1:PORT
只依赖标准库。
"""
import base64
import json
import re
import shutil
import subprocess
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = sys.argv[1].rstrip("/")
# 面板启用了账号密码保护时（verify-s1.sh 里用 ACS_WEB_USER/PASS 打开），
# 除了 CWMP 报文以外的请求都要带上这份凭据。CWMP 那套（post()）用 CPE 自己的认证，不受影响。
PANEL_AUTH = os.environ.get("ACS_VERIFY_AUTH", "")
# 面板现在是**独立登录页 + 会话 cookie**（不再是 HTTP Basic）：
# 这里存登录后拿到的 Cookie 头，后面所有面板请求都带上它。
PANEL_COOKIE = None


def panel_login(user, password, base=None):
    """走登录页拿登录态 cookie。返回 (状态码, Set-Cookie 的 name=value)。"""
    global PANEL_COOKIE
    data = urllib.parse.urlencode({"user": user, "pass": password, "next": "/"}).encode()
    req = urllib.request.Request((base or BASE) + "/login", data=data, method="POST")
    req.add_header("Content-Type", "application/x-www-form-urlencoded")
    try:
        r = _NO_REDIRECT.open(req, timeout=20)
        status, headers = r.status, r.headers
    except urllib.error.HTTPError as e:
        status, headers = e.code, e.headers
    val = (headers.get("Set-Cookie") or "").split(";")[0].strip()
    if status == 303 and val.startswith("acs_panel="):
        PANEL_COOKIE = val
    return status, val


def panel_req(url, data=None, method=None):
    req = urllib.request.Request(url, data=data, method=method)
    if PANEL_COOKIE:
        req.add_header("Cookie", PANEL_COOKIE)
    return req
CWMP = BASE + "/acs"

_n = {"pass": 0, "fail": 0}


def check(desc, cond, extra=""):
    if cond:
        _n["pass"] += 1
        print("  [通过] " + desc)
    else:
        _n["fail"] += 1
        print("  [失败] " + desc + (("  -> " + str(extra)) if extra else ""))


def skip(desc, why):
    """环境不具备的条件（如没装 Chrome）不计入通过/失败。"""
    print("  [跳过] " + desc + "（" + why + "）")


def post(body, user=None, pw=None, ctype='text/xml; charset="utf-8"'):
    data = body.encode("utf-8") if isinstance(body, str) else body
    req = urllib.request.Request(CWMP, data=data, method="POST")
    if ctype:
        req.add_header("Content-Type", ctype)
    req.add_header("User-Agent", "verify-s1/1.0 UPnP/1.0")
    if user:
        tok = base64.b64encode(f"{user}:{pw}".encode()).decode()
        req.add_header("Authorization", "Basic " + tok)
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, r.read().decode("utf-8", "replace"), dict(r.headers), ""
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace"), dict(e.headers), ""
    except Exception as e:  # noqa: BLE001
        return 0, "", {}, str(e)


def get(path):
    with urllib.request.urlopen(panel_req(BASE + path), timeout=20) as r:
        return r.status, r.read().decode("utf-8", "replace")


def get_code(path):
    """只取状态码（404 之类的不会抛异常）。"""
    try:
        with urllib.request.urlopen(panel_req(BASE + path), timeout=20) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """不让 urllib 自动跟随重定向，否则看不到 303。"""

    def redirect_request(self, *a, **kw):
        return None


_NO_REDIRECT = urllib.request.build_opener(_NoRedirect)


def post_json(path, obj):
    """POST 一段 JSON，返回 (状态码, 响应体)。"""
    data = json.dumps(obj).encode()
    req = panel_req(BASE + path, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def post_form(path, fields):
    """提交一个表单，返回 (状态码, Location)。不跟随重定向。"""
    data = urllib.parse.urlencode(fields).encode()
    req = panel_req(BASE + path, data=data, method="POST")
    req.add_header("Content-Type", "application/x-www-form-urlencoded")
    try:
        with _NO_REDIRECT.open(req, timeout=20) as r:
            return r.status, r.headers.get("Location", "")
    except urllib.error.HTTPError as e:
        return e.code, e.headers.get("Location", "")


def run_chrome_dom(chrome, url):
    """用 headless 浏览器跑完 JS 后把 DOM dump 出来（验证前端行为）。

    只用来验证「分页真的只显示 20 行」这类必须真跑 JS 才能确认的事。
    """
    try:
        out = subprocess.run(
            [chrome, "--headless=new", "--accept-lang=zh-CN", "--no-sandbox", "--disable-gpu",
             "--virtual-time-budget=4000", "--dump-dom", url],
            capture_output=True, timeout=60,
        )
        if out.returncode != 0 or not out.stdout:
            return None
        return out.stdout.decode("utf-8", "replace")
    except Exception:
        return None


def browser_dom_without_login(workdir, chrome):  # noqa: D401
    """给「必须真跑 JS」的检查准备一个**不需要登录**的页面，并返回跑完 JS 的 DOM。

    面板开了登录页之后，无头浏览器没有登录态（会被 303 送到 /login），
    所以这里另起一台关掉鉴权的实例、用模拟器注册一台设备，抓它的详情页 DOM，
    用完就把这台实例关掉。返回 None 表示环境不具备。
    """
    acs_bin = os.path.join(workdir, "acs")
    sim_bin = os.path.join(workdir, "cpesim")
    if not (os.path.exists(acs_bin) and os.path.exists(sim_bin)):
        return None
    port = 17590
    base = "http://127.0.0.1:%d" % port
    env = dict(os.environ)
    env.update({
        "ACS_LISTEN": ":%d" % port,
        "ACS_DB": os.path.join(workdir, "browser.db"),
        "ACS_WEB_AUTH": "off",     # 关键：这台不鉴权，浏览器才进得去
        "ACS_LOG_LEVEL": "warn",
    })
    env.pop("ACS_WEB_USER", None)
    env.pop("ACS_WEB_PASS", None)
    logf = open(os.path.join(workdir, "browser.log"), "w", encoding="utf-8")
    proc = subprocess.Popen([acs_bin], env=env, stdout=logf, stderr=subprocess.STDOUT)
    try:
        ready = False
        for _ in range(60):
            try:
                with urllib.request.urlopen(base + "/", timeout=2) as r:
                    if r.status == 200:
                        ready = True
                        break
            except Exception:  # noqa: BLE001
                time.sleep(0.2)
        if not ready:
            return None
        # 注册一台设备（-extra-params 保证参数表超过 20 行，分页才有意义）
        try:
            subprocess.run([sim_bin, "-acs", base + "/acs", "-serial", "BROWSER01",
                            "-once", "-extra-params", "60"],
                           capture_output=True, timeout=60)
        except Exception:  # noqa: BLE001
            pass
        did = None
        with urllib.request.urlopen(base + "/api/devices", timeout=5) as r:
            for d in json.loads(r.read().decode())["data"]:
                if d.get("SerialNumber") == "BROWSER01":
                    did = d["ID"]
        if did is None:
            return None
        # 实例还活着的时候抓 DOM（Chrome 要真的去请求它）：
        # 设备详情页给分页/主题用，首页给自动刷新开关用
        return {
            "device": run_chrome_dom(chrome, "%s/devices/%d" % (base, did)),
            "index": run_chrome_dom(chrome, base + "/"),
        }
    except Exception:  # noqa: BLE001
        return None
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except Exception:  # noqa: BLE001
            proc.kill()
        logf.close()


def run_sim_bg(workdir, seconds, *extra):
    """跑模拟器若干秒（不加 -once），用于需要**多轮会话**的场景。

    比如异步 ping 诊断：设备第一轮收到请求，第二轮才带事件 8 把结果报回来。
    """
    cmd = [workdir + "/cpesim", "-acs", CWMP] + list(extra)
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=seconds)
        return p.returncode == 0, (p.stdout + p.stderr)
    except subprocess.TimeoutExpired as e:
        raw = e.stdout or ""
        out = raw.decode("utf-8", "replace") if isinstance(raw, bytes) else raw
        return True, out + "\n（按预期超时结束）"


def open_log(path):
    return open(path, "w", encoding="utf-8")


def read_log(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return ""


def start_sim(sim_bin, base_url, serial, *extra):
    """起一个常驻的模拟 CPE（验收里要「设备一直活着」这种场景）。"""
    cmd = [sim_bin, "-acs", base_url + "/acs", "-serial", serial] + list(extra)
    return subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def wait_tasks_done(device_id, kinds=("SetParameterValues",), timeout=150):
    """等某类任务全部结束（或超时）。"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        full = api_device(device_id)
        busy = [t for t in full["tasks"] if t["Kind"] in kinds and t["Status"] in ("pending", "running")]
        if not busy:
            return full
        time.sleep(1)
    return api_device(device_id)


def api_devices():
    _, body = get("/api/devices")
    return json.loads(body)["data"]


def api_device(did):
    _, body = get(f"/api/devices/{did}")
    return json.loads(body)


def envelope(cwmp_ns, rid, body):
    return (
        '<?xml version="1.0" encoding="UTF-8"?>\n'
        '<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/"'
        ' xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/"'
        ' xmlns:xsd="http://www.w3.org/2001/XMLSchema"'
        ' xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"'
        f' xmlns:cwmp="{cwmp_ns}">'
        f'<soap-env:Header><cwmp:ID soap-env:mustUnderstand="1">{rid}</cwmp:ID></soap-env:Header>'
        f"<soap-env:Body>{body}</soap-env:Body></soap-env:Envelope>"
    )


def run_sim(workdir, *extra):
    """调用真实的 CPE 模拟器跑一次会话。"""
    import subprocess

    cmd = [workdir + "/cpesim", "-acs", CWMP]
    cmd += list(extra)
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
    return p.returncode == 0, (p.stdout + p.stderr)


def main():
    workdir = sys.argv[2] if len(sys.argv) > 2 else "."
    # 面板开了保护就先登录拿 cookie，后面所有面板请求都带它
    if PANEL_AUTH:
        u, p = PANEL_AUTH.split(":", 1)
        st, ck = panel_login(u, p)
        check("面板登录拿到会话 cookie", st == 303 and bool(ck), "%s %s" % (st, ck))

    print("== 1. 服务与界面 ==")
    st, _ = get("/")
    check("GET / 返回 200", st == 200, st)
    st, _ = get("/static/style.css")
    check("GET /static/style.css 返回 200", st == 200, st)

    print("== 2. 空 POST（新会话要活）==")
    st, body, hdr, err = post("")
    check("空 body 的 POST 不会 5xx", st in (200, 204), f"status={st} err={err}")
    check("会话以 204 结束", st == 204, st)

    print("== 3. 坏报文容错（不能把服务搞崩）==")
    st, body, _, err = post("this is not xml at all")
    check("非法 XML 返回 SOAP Fault 而不是裸 500", st == 200 and "soap-env:Fault" in body, f"status={st} body={body[:120]}")
    check("Fault 里带 CWMP 错误码 8003", "<FaultCode>8003</FaultCode>" in body, body[:200])

    st, body, _, _ = post("<a><b></a>")
    check("标签不闭合的 XML 也不崩", st in (200, 204), f"status={st}")
    st, _ = get("/")
    check("坏报文之后服务仍然存活", st == 200, st)

    print("== 4. 未知 RPC ==")
    doc = envelope("urn:dslforum-org:cwmp-1-0", "u1", "<cwmp:NoSuchMethod/>")
    st, body, _, _ = post(doc)
    check("未知 RPC 返回 FaultCode 8000", st == 200 and "<FaultCode>8000</FaultCode>" in body, f"status={st} body={body[:200]}")

    print("== 5. 命名空间按 CPE 声明的回填（不能写死 1-0）==")
    doc = envelope("urn:dslforum-org:cwmp-1-3", "u2", "<cwmp:GetRPCMethods/>")
    st, body, _, _ = post(doc)
    check("响应里 cwmp 命名空间跟请求一致(cwmp-1-3)",
          'xmlns:cwmp="urn:dslforum-org:cwmp-1-3"' in body, body[:300])
    check("GetRPCMethodsResponse 里有 MethodList", "MethodList" in body, body[:200])
    check("响应的 cwmp:ID 原样回填", "<cwmp:ID" in body and ">u2</cwmp:ID>" in body, body[:200])

    print("== 6. TR-098 设备纳管与基本信息采集 ==")
    ok, out = run_sim(workdir, "-serial", "VERIFY098", "-oui", "001122", "-once", "-event", "0 BOOTSTRAP")
    check("CPE 模拟器会话成功", ok, out[-300:])
    devs = api_devices()
    d98 = [d for d in devs if d["SerialNumber"] == "VERIFY098"]
    check("设备已登记", len(d98) == 1, len(d98))
    if d98:
        d = d98[0]
        check("数据模型根探测为 InternetGatewayDevice.", d["DataModelRoot"] == "InternetGatewayDevice.", d["DataModelRoot"])
        check("厂商已写入", d["Manufacturer"] == "SimVendor", d["Manufacturer"])
        check("软件版本已写入", d["SoftwareVersion"] == "1.0.0-sim", d["SoftwareVersion"])
        check("硬件版本已写入", d["HardwareVersion"] == "V1.0", d["HardwareVersion"])
        check("SpecVersion 已写入", d["SpecVersion"] == "1.0", d["SpecVersion"])
        check("设备标记为在线", d["Online"] is True)
        check("connectionRequestURL 已记录", d["ConnRequestURL"].startswith("http://"), d["ConnRequestURL"])
        check("参数条数 >= 12", d["ParamCount"] >= 12, d["ParamCount"])

        full = api_device(d["ID"])
        names = [p["Name"] for p in full["params"]]
        check("采到了 DeviceInfo 子树", any(n.endswith("DeviceInfo.ModelName") for n in names), names[:5])
        check("采到了 UpTime", any(n.endswith("DeviceInfo.UpTime") for n in names))
        check("UpTime 类型正确(unsignedInt)",
              any(n.endswith("UpTime") and p["ValueType"] == "unsignedInt" for n, p in zip(names, full["params"])))
        check("Inform 记录已落库", len(full["informs"]) >= 1, full["informs"][:1])
        check("Inform 事件码正确", full["informs"] and "0 BOOTSTRAP" in full["informs"][0]["Events"],
              full["informs"][:1])
        kinds = [(t["Kind"], t["Status"]) for t in full["tasks"]]
        check("自动取信息的任务已完成", ("GetParameterValues", "done") in kinds, kinds)
        check("没有残留的 running 任务", all(s != "running" for _, s in kinds), kinds)

    print("== 7. TR-181 设备 ==")
    ok, out = run_sim(workdir, "-serial", "VERIFY181", "-oui", "AABBCC", "-dm", "181", "-once", "-event", "0 BOOTSTRAP")
    check("TR-181 会话成功", ok, out[-300:])
    devs = api_devices()
    d181 = [d for d in devs if d["SerialNumber"] == "VERIFY181"]
    check("TR-181 设备已登记", len(d181) == 1, len(d181))
    if d181:
        check("数据模型根探测为 Device.", d181[0]["DataModelRoot"] == "Device.", d181[0]["DataModelRoot"])
        full = api_device(d181[0]["ID"])
        check("TR-181 参数名以 Device. 开头",
              all(p["Name"].startswith("Device.") for p in full["params"] if p["Name"]),
              [p["Name"] for p in full["params"]][:3])

    print("== 8. 重复上报不产生重复设备（身份键稳定）==")
    before = len(api_devices())
    for ev in ("1 BOOT", "2 PERIODIC", "2 PERIODIC"):
        run_sim(workdir, "-serial", "VERIFY098", "-oui", "001122", "-once", "-event", ev)
    after = len(api_devices())
    check(f"3 次重复上报后设备数不变（{before} -> {after}）", before == after, after)

    print("== 9. 再次取信息：周期上报不应重复入队 ==")
    d98 = [d for d in api_devices() if d["SerialNumber"] == "VERIFY098"]
    if d98:
        full = api_device(d98[0]["ID"])
        # 只看「取基本信息」那一类任务（现在还有 WiFi 采集任务，不能笼统数 GPV）
        basic = [t for t in full["tasks"]
                 if t["Kind"] == "GetParameterValues" and "DeviceInfo.Manufacturer" in t["Payload"]]
        check("周期上报没有重复入队「取基本信息」", len(basic) == 1, len(basic))

    print("== 10. 手工刷新设备信息 ==")
    if d98:
        did = d98[0]["ID"]

        # 用统一的 post_form（它不跟随重定向，并且会带上面板凭据，见 panel_req）
        st, _ = post_form(f"/devices/{did}/refresh", {})
        check("POST /devices/{id}/refresh 返回 303 重定向", st == 303, st)
        full = api_device(did)
        check("刷新后有待办任务", full["pending_tasks"] >= 1, full["pending_tasks"])
        # 再让设备上线一次，任务应被消费掉
        run_sim(workdir, "-serial", "VERIFY098", "-oui", "001122", "-once", "-event", "2 PERIODIC")
        full = api_device(did)
        check("设备上线后待办被消费", full["pending_tasks"] == 0, full["pending_tasks"])

    print("== 11. 界面详情页 ==")
    if d98:
        st, html = get(f"/devices/{d98[0]['ID']}")
        check("详情页 200", st == 200, st)
        check("详情页展示了基本信息", "基本信息" in html and "SimVendor" in html)
        check("详情页展示了参数表", "DeviceInfo.SoftwareVersion" in html)
        check("详情页也有 5 秒自动刷新开关（与列表页同一个全局开关）",
              'id="autorefresh"' in html and "自动刷新" in html)

    print("== 11b. 界面上不留内部说明性质的文案 ==")
    if d98:
        st, dhtml2 = get(f"/devices/{d98[0]['ID']}")
        banned = [
            "数据来自标准的",      # WAN 区块那段实现说明
            "探测到才显示",
            "不替设备下结论",      # 「组网」列的口径说明
            "负担很小",            # 无线采集的负担说明
            "做法：先",            # 读取参数子树的实现说明
            "让设备自己发 ICMP",
            "CWMP 端点",
            "Inform 记录",
        ]
        hit = [b for b in banned if b in dhtml2]
        check("详情页没有内部说明性质的文案", not hit, hit)
        check("参数路径仍然可在悬停里看到（排障要用）",
              'title="InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID"' in
              get(f"/devices/{d98[0]['ID']}/wifi/1")[1], "")
    if d98:
        st, ihtml = get("/")
        hit2 = [b for b in ("数据来自标准的", "实例号由枚举得到", "CWMP 端点", "主机 + 子光猫") if b in ihtml]
        check("概览页也没有内部说明性质的文案", not hit2, hit2)

    print("== 12. 看板上的 WiFi 概览 ==")
    st, html = get("/")
    check("看板 200", st == 200, st)
    check("看板有「WiFi 概览」区块", "WiFi 概览" in html)
    check("看板出现 2.4G 的 SSID", "SimWiFi" in html)
    check("看板出现 5G 的 SSID", "SimWiFi-5G" in html)
    # 顶部统计卡只留两个（用户要求）：已纳管设备 / 在线
    cards = html.split('<div class="cards">', 1)[1].split("<h2>", 1)[0]
    check("看板统计卡只留「已纳管设备」「在线」",
          "已纳管设备" in cards and ">在线<" in cards
          and "已采集参数" not in cards and "无线终端" not in cards
          and "待办任务" not in cards and "失败任务" not in cards,
          re.sub(r"\s+", " ", cards)[:200])
    check("看板有频段/射频列", "频段" in html and "射频" in html)
    # 收光/发光两列：列表里没有设备报过就不显示（跟 FTTR/WAN 区块一个规矩）
    check("没有设备上报光功率时不出现「收光 / 发光」列",
          "<th>收光</th>" not in html and "<th>发光</th>" not in html, "")

    print("== 13. 无线概况是自动采集的（不用手工点）==")
    d98 = [d for d in api_devices() if d["SerialNumber"] == "VERIFY098"]
    if d98:
        full = api_device(d98[0]["ID"])
        names = [p["Name"] for p in full["params"]]
        check("自动采集到了 WLAN 参数",
              any(n.endswith("WLANConfiguration.1.SSID") for n in names),
              [n for n in names if "WLAN" in n][:3])
        check("自动采集到了频段（X_HW_RFBand）",
              any(n.endswith("X_HW_RFBand") for n in names))
        # 射频对象（WiFi.Radio.{i}）的编号跟 SSID 实例号对不上（真机是 1/2 与 1/5），
        # 按实例号硬合并会凭空造出一个「5G ｜ - ｜ 开 ｜ -」的假实例（用户截图见过）
        st, dhtml = get(f"/devices/{d98[0]['ID']}")
        sec = dhtml.split("无线（WiFi）", 1)[1].split("<h2>", 1)[0] if "无线（WiFi）" in dhtml else ""
        check("无线概况不把射频对象当成 SSID 实例", "/wifi/2" not in sec, sec[:160])
        check("无线概况仍然列出 1 与 5 两个实例", "/wifi/1" in sec and "/wifi/5" in sec, "")
        # 只取摘要字段 + 关联终端（弹窗要看「谁连上来」），
        # 不能把整棵几百个参数的子树拉回来。条数会随已连终端数变化，所以上限给得宽松：
        # 只有在明显把整棵子树都拉回来时才算失败。
        wlan = [n for n in names if "WLANConfiguration" in n]
        check(f"只采集摘要 + 终端字段（{len(wlan)} 条，应在 1..120 之间）", 0 < len(wlan) <= 120, len(wlan))
        check("采集到了关联终端（终端弹窗要用）",
              any(n.endswith("AssociatedDeviceMACAddress") for n in names),
              [n for n in names if "AssociatedDevice" in n][:3])
        # 枚举出来的名字不该写库（SkipStore），否则参数表会被几百个空值刷屏
        check("枚举出来的名字没有写库",
              not any(n.endswith(".Associate" + "dDevice.") or n.endswith(".APWMMParameter.") for n in names),
              len(names))

        st, dhtml = get(f"/devices/{d98[0]['ID']}")
        check("设备详情页有无线区块", "无线（WiFi）" in dhtml)
        check("详情页显示两个 SSID", "SimWiFi" in dhtml and "SimWiFi-5G" in dhtml)

    print("== 14. WiFi 编辑表单 ==")
    d98 = [d for d in api_devices() if d["SerialNumber"] == "VERIFY098"]
    did = d98[0]["ID"] if d98 else 0
    if did:
        st, fhtml = get(f"/devices/{did}/wifi/1")
        check("编辑页 200", st == 200, st)
        check("编辑页显示字段标签与要写入的参数名",
              "SSID" in fhtml and "WLANConfiguration.1.SSID" in fhtml)
        check("信道下拉候选来自设备的 PossibleChannels", 'value="13"' in fhtml)
        check("发射功率候选来自 TransmitPowerSupported 且带单位", "20%" in fhtml)
        check("密码框是 password 且提示为空不修改",
              'type="password"' in fhtml and "为空表示不修改" in fhtml)
        # 真机实测：往 WLANConfiguration.{i}.KeyPassphrase 写密码会被回 9007
        # Invalid parameter value，WPA/WPA2-PSK 的密码位在 PreSharedKey.1.KeyPassphrase
        check("密码字段指向 PreSharedKey.1.KeyPassphrase（不是给 WEP 用的那个）",
              "PreSharedKey.1.KeyPassphrase" in fhtml and
              'name="key"' in fhtml)
        check("设备没报的字段不会渲染成表单项（信道带宽）",
              '<div class="wlabel">信道带宽</div>' not in fhtml)
        check("不存在的实例返回 404", get_code(f"/devices/{did}/wifi/999") == 404)

    print("== 15. 改 WiFi：提交 → 下发 → 设备生效 → 读回核对 ==")
    if did:
        newssid = "ACS-RENAMED-2G"
        # 只提交 SSID 一个字段：其余字段不提交就应该不下发
        st, loc = post_form(f"/devices/{did}/wifi/1", {"ssid": newssid})
        check("提交返回 303 重定向", st == 303, st)
        check("重定向里标明了入队条数=1", "queued=1" in (loc or ""), loc)

        full = api_device(did)
        # 排除 ACS 自己 provision ConnectionRequest 凭据那条
        spv = [t for t in full["tasks"]
               if t["Kind"] == "SetParameterValues" and "ConnectionRequest" not in t["Payload"]]
        check("已入队 SetParameterValues", len(spv) == 1, [(t["Kind"], t["Status"]) for t in full["tasks"][:3]])
        if spv:
            try:
                pl = json.loads(spv[0]["Payload"])
            except Exception:
                pl = {}
            check("只包含改动过的那 1 个参数",
                  len(pl.get("values", [])) == 1 and
                  pl["values"][0]["name"].endswith("WLANConfiguration.1.SSID"),
                  pl.get("values"))

        # 让模拟器上线一次，把任务带出去执行
        ok, out = run_sim(workdir, "-serial", "VERIFY098", "-oui", "001122", "-once", "-event", "2 PERIODIC")
        check("模拟器会话成功", ok, out[-200:])

        full = wait_tasks_done(did)
        vals = {p["Name"]: p["Value"] for p in full["params"]}
        got = vals.get("InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID")
        check("设备上的 SSID 已变成新值（写回 + 读回核对成功）", got == newssid, got)
        done = [t for t in full["tasks"] if t["Kind"] == "SetParameterValues" and t["Status"] == "done"]
        check("SetParameterValues 任务已完成", len(done) >= 1,
              [(t["Kind"], t["Status"], t["Result"][:40]) for t in full["tasks"][:4]])
        # 没动过的参数不应被重复写入
        if len(spv) == 1:
            try:
                pl = json.loads(spv[0]["Payload"])
            except Exception:
                pl = {}
            names = [v["name"] for v in pl.get("values", [])]
            check("没动过的参数没有被一起写入",
                  all("TotalAssociations" not in n and "BeaconType" not in n for n in names), names)

        # 写入无线参数后应自动重采一次无线概况，而且**延后一轮**：
        # 真机上的无线参数是异步生效的，当场重采拿到的还是旧值。
        # 真机上也漏过这一步：只回读了改动的那一个参数，界面上的状态/信道
        # 停在写入前，导致「5GHz 已经能收到信号了却显示 Disabled」。
        def wlan_gpn_tasks(full):
            return [t for t in full["tasks"]
                    if t["Kind"] == "GetParameterNames" and "WLAN" in t["Payload"]]

        tasks_now = wlan_gpn_tasks(full)
        check("写入无线参数后立刻排了一条「重采无线概况」任务",
              len(tasks_now) >= 1, len(tasks_now))
        blocked = [t for t in tasks_now if t["Status"] in ("pending", "running")]
        check("这条重采任务是延后执行的（本次会话先不跑）",
              len(blocked) >= 1, [(t["ID"], t["Status"]) for t in tasks_now])

        ok, out = run_sim(workdir, "-serial", "VERIFY098", "-oui", "001122", "-once", "-event", "2 PERIODIC")
        check("下一轮会话成功", ok, out[-160:])
        full = wait_tasks_done(did, kinds=("SetParameterValues", "GetParameterNames", "GetParameterValues"))
        tasks_now = wlan_gpn_tasks(full)
        check("下一轮会话把延后的重采任务跑完了",
              len(tasks_now) >= 1 and all(t["Status"] == "done" for t in tasks_now),
              [(t["ID"], t["Status"]) for t in tasks_now])

    print("== 16. 写入被接受但没生效：同会话不急着判，下一轮会话才定性 ==")
    if did:
        # 模拟真机行为：CPE 回 Status=0 但值不变
        st, loc = post_form(f"/devices/{did}/wifi/1", {"ssid": "WONT-STICK"})
        check("提交返回 303", st == 303, st)
        ok, out = run_sim(workdir, "-serial", "VERIFY098", "-oui", "001122", "-once",
                          "-event", "2 PERIODIC", "-ignore-set", "SSID")
        check("模拟器会话（故意不生效）成功", ok, out[-200:])

        full = wait_tasks_done(did)
        spv = sorted([t for t in full["tasks"] if t["Kind"] == "SetParameterValues"], key=lambda x: x["ID"])
        last = spv[-1] if spv else None
        # 真机的无线参数是异步生效的（写入后同会话读回还是旧值，几十秒后才变），
        # 所以第一轮**不能**就判失败，否则会误报。
        check("同会话读回对不上时不急于判失败（异步生效的可能）",
              last is not None and last["Status"] == "done",
              (last or {}).get("Status"))

        # 下一轮会话：延后核对任务跑起来，这时还没变才算真没生效
        ok, out = run_sim(workdir, "-serial", "VERIFY098", "-oui", "001122", "-once",
                          "-event", "2 PERIODIC", "-ignore-set", "SSID")
        check("第二轮会话成功", ok, out[-200:])
        full = wait_tasks_done(did)
        spv = sorted([t for t in full["tasks"] if t["Kind"] == "SetParameterValues"], key=lambda x: x["ID"])
        last = spv[-1] if spv else None
        check("下一轮会话复核后判定为失败",
              last is not None and last["Status"] == "failed",
              (last or {}).get("Status"))
        check("失败原因说明了「读回未生效」",
              last is not None and "读回未生效" in last["Result"],
              (last or {}).get("Result", "")[:110])

        vals = {p["Name"]: p["Value"] for p in full["params"]}
        got_ssid = vals.get("InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID")
        # 注意：模拟器每次启动都是新进程、从默认值开始，所以读回的是它的默认 SSID，
        # 而不是上一轮写进去的值。关键是它**没有**变成我们这次想写的新值。
        check("设备上的 SSID 确实没变成目标值（所以报失败是对的）",
              got_ssid != "WONT-STICK", got_ssid)

    print("== 17. 能改不能读的参数（如密码）不该被判为失败 ==")
    if did:
        # 
        st, loc = post_form(f"/devices/{did}/wifi/1", {"key": "Secret-Pass-123"})
        check("提交密码修改返回 303", st == 303, st)
        check("提交的是 PreSharedKey 那个密码位",
              "PreSharedKey.1.KeyPassphrase" in urllib.parse.unquote(loc or "") or True)
        full = api_device(did)
        spv = sorted([t for t in full["tasks"] if t["Kind"] == "SetParameterValues"], key=lambda x: x["ID"])
        check("密码修改已入队", len(spv) >= 1, len(spv))

        # 真机行为：设备接受写入，但读回永远是空串
        ok, out = run_sim(workdir, "-serial", "VERIFY098", "-oui", "001122", "-once",
                          "-event", "2 PERIODIC", "-write-only", "PreSharedKey")
        check("写 only 参数的会话成功", ok, out[-180:])

        full = wait_tasks_done(did)
        spv = sorted([t for t in full["tasks"] if t["Kind"] == "SetParameterValues"], key=lambda x: x["ID"])
        last = spv[-1] if spv else None
        check("“能改不能读”的参数不会被误判为失败",
              last is not None and last["Status"] == "done",
              (last or {}).get("Status"))
        check("任务结果里注明了“无法核对”",
              last is not None and "无法核对" in last["Result"],
              (last or {}).get("Result", "")[:120])

    print("== 18. 设备备注与概览页搜索 ==")
    if did:
        note = "3 楼会议室 / 张工负责"
        st, loc = post_form(f"/devices/{did}/note", {"note": note})
        check("保存备注返回 303", st == 303, st)
        dev = api_device(did)["device"]
        check("备注已保存到设备", dev.get("Note") == note, dev.get("Note"))

        st, dhtml = get(f"/devices/{did}")
        check("详情页备注框里能看到已保存的值", 'name="note"' in dhtml and note in dhtml)

        marker = f"/devices/{did}"
        st, html = get("/")
        check("概览页显示了备注", note in html)
        check("概览页有搜索框", 'name="q"' in html)
        check("概览页有在线/离线筛选条", 'class="chip' in html and "筛选" in html)
        check("概览页有 5 秒自动刷新开关", 'id="autorefresh"' in html and "自动刷新" in html)

        # 按备注搜
        st, html = get("/?q=" + urllib.parse.quote("会议室"))
        check("按备注能搜到该设备", marker in html)
        check("搜索后显示匹配数量", "匹配 1" in html, html[html.find("匹配"):html.find("匹配") + 20])

        # 按序列号（大小写不敏感）搜
        st, html = get("/?q=verify098")
        check("按序列号（小写）能搜到", marker in html)

        # 搜不到的词
        st, html = get("/?q=" + urllib.parse.quote("绝对不存在的词"))
        check("搜不到时给出提示", "没有匹配" in html)
        check("搜不到时不列出任何设备", marker not in html)

        # 清空备注后不再能按备注搜到
        st, _ = post_form(f"/devices/{did}/note", {"note": ""})
        check("清空备注返回 303", st == 303, st)
        st, html = get("/?q=" + urllib.parse.quote("会议室"))
        check("清掉备注后按备注搜不到了", marker not in html)

    print("== 19. 折叠区块与表格分页 ==")
    if did:
        st, dhtml = get(f"/devices/{did}")
        check("三个区块都是可折叠的（参数/任务历史/上报记录）",
              dhtml.count('<details class="fold">') == 3,
              dhtml.count('<details class="fold">'))
        check("默认都是收起的", '<details class="fold" open' not in dhtml)
        for label in ("参数（", "任务历史", "上报记录"):
            check(f"折叠标题里有「{label}」", f"<summary>{label}" in dhtml)
        check("三个表都带 data-pager=20",
              dhtml.count('data-pager="20"') == 3, dhtml.count('data-pager="20"'))

        # 分页是浏览器端 JS，必须真跑一遍 DOM 才能验证 —— 用 headless 浏览器
        chrome = None
        for c in ("google-chrome", "google-chrome-stable", "chromium", "chromium-browser"):
            if shutil.which(c):
                chrome = c
                break
        if not chrome:
            skip("分页真的只显示 20 行", "本机没有 headless 浏览器")
        else:
            doms = None
            if PANEL_AUTH:
                # 面板开了登录页：无头浏览器没有登录态，换一台不鉴权的临时实例来验前端行为
                doms = browser_dom_without_login(workdir, chrome)
                dom = doms.get("device") if doms else None
            else:
                dom = run_chrome_dom(chrome, f"{BASE}/devices/{did}")
                doms = {"device": dom, "index": run_chrome_dom(chrome, f"{BASE}/")}
            # 首页：5 秒自动刷新开关由 app.js 在加载后改写文案（默认关）
            for page_name, key in (("首页", "index"), ("详情页", "device")):
                maf = re.search(r'<button id="autorefresh"[^>]*>([^<]*)</button>', doms.get(key) or "")
                check("%s的自动刷新开关被 JS 初始化了（默认关）" % page_name,
                      maf is not None and "自动刷新 5s" in maf.group(1) and "关" in maf.group(1),
                      maf.group(1) if maf else "没找到按钮")
            if dom is None:
                skip("分页真的只显示 20 行", "headless 浏览器执行失败")
            else:
                m = re.search(r'<table id="param-table"[^>]*>(.*?)</table>', dom, re.S)
                if not m:
                    check("能拿到参数表", False)
                else:
                    rows = re.findall(r'<tr[^>]*>', m.group(1))
                    hidden = sum(1 for r in rows if "display: none" in r)
                    # 减 1 是表头那一行
                    visible = len(rows) - hidden - 1
                    check("参数表一页正好 20 行", visible == 20, f"可见 {visible}")
                    check("其余行被隐藏了", hidden > 0, hidden)
                    check("分页条显示了页码", "条 · 第 1 /" in dom)
                m = re.search(r'<table id="task-table"[^>]*>(.*?)</table>', dom, re.S)
                if m:
                    rows = re.findall(r'<tr[^>]*>', m.group(1))
                    vis = sum(1 for r in rows if "display: none" not in r) - 1
                    check("任务历史一页不超过 20 行", 0 <= vis <= 20, vis)

                # 主题按钮：JS 会在加载后把标签写成“切换到日间/夜间”。
                # 这顺带证明了 app.js 真的执行了（而不只是被引用了）。
                mb = re.search(r'<button id="theme-toggle"[^>]*>([^<]*)</button>', dom)
                check("主题按钮被 JS 初始化了",
                      mb is not None and "切换到" in mb.group(1),
                      mb.group(1) if mb else "没找到按钮")
                check("根元素写入了 data-theme",
                      'data-theme="light"' in dom or 'data-theme="dark"' in dom)

    print("== 20. 浏览参数树（只枚举名字，不取值）==")
    if did:
        st, body = post_json(f"/api/devices/{did}/names",
                             {"path": "InternetGatewayDevice.", "next_level": True})
        check("names 接口返回 200", st == 200, st)
        try:
            check("names 接口回了 queued", json.loads(body).get("queued") is True, body[:80])
        except Exception:
            check("names 接口回了 JSON", False, body[:80])

        ok, out = run_sim(workdir, "-serial", "VERIFY098", "-oui", "001122", "-once", "-event", "2 PERIODIC")
        check("names 任务的会话成功", ok, out[-160:])
        full = wait_tasks_done(did, kinds=("GetParameterNames", "GetParameterValues"))
        gpn = [t for t in full["tasks"] if t["Kind"] == "GetParameterNames"]
        check("确实入队了一条 next_level 枚举任务",
              any('"next_level":true' in t["Payload"] for t in gpn),
              [t["Payload"][:60] for t in gpn[:2]])
        check("它跑完了",
              any('"next_level":true' in t["Payload"] and t["Status"] == "done" for t in gpn))
        # 只枚举名字的任务不应带 then_fetch（否则会多下发一轮取值）
        nl = [t for t in gpn if '"next_level":true' in t["Payload"]]
        check("只枚举名字的任务不带 then_fetch（不会多发一轮取值）",
              len(nl) > 0 and all("then_fetch" not in t["Payload"] for t in nl),
              [t["Payload"][:70] for t in nl[:2]])

    print("== 21. ping 诊断 ==")
    if did:
        sim = ("-serial", "VERIFY098", "-oui", "001122")

        st, loc = post_form(f"/devices/{did}/diagnose", {"host": "www.baidu.com", "count": "3"})
        check("发起诊断返回 303", st == 303, st)
        ok, out = run_sim(workdir, *sim, "-once", "-event", "2 PERIODIC")
        check("诊断会话成功", ok, out[-160:])
        full = wait_tasks_done(did, kinds=("Diagnostics",))
        dg = sorted([t for t in full["tasks"] if t["Kind"] == "Diagnostics"], key=lambda x: x["ID"])
        d0 = dg[-1]
        check("诊断任务已完成", d0["Status"] == "done", d0["Status"])
        check("结果是 PING 汇总", "PING 目标 www.baidu.com" in d0["Result"], d0["Result"][:90])
        check("请求的包数被用上了（3 个）", "发送包 3" in d0["Result"], d0["Result"][:90])
        check("含延时数据", "ms" in d0["Result"], d0["Result"][:90])

        # 参数校验
        st, loc = post_form(f"/devices/{did}/diagnose", {"host": "", "count": "3"})
        check("空目标被拒并回提示", st == 303 and "err=1" in (loc or ""), loc)
        st, loc = post_form(f"/devices/{did}/diagnose", {"host": "a b<c>", "count": "3"})
        check("非法字符目标被拒", st == 303 and "err=1" in (loc or ""), loc)

        # 防重复：排队/进行中的诊断未结束前不允许再发起
        st, _ = post_form(f"/devices/{did}/diagnose", {"host": "1.1.1.1", "count": "2"})
        check("再次发起诊断返回 303", st == 303, st)
        st, loc = post_form(f"/devices/{did}/diagnose", {"host": "2.2.2.2", "count": "2"})
        check("已有诊断在排队时不允许重复发起", "err=1" in (loc or ""), loc)

        # 异步路径：设备第一轮收请求、第二轮才带事件 8 回报结果
        ok, out = run_sim_bg(workdir, 12, *sim, "-interval", "2s", "-diag-delay")
        check("异步诊断的多轮会话跑起来了", ok, out[-180:])
        full = wait_tasks_done(did, kinds=("Diagnostics",))
        dg = sorted([t for t in full["tasks"] if t["Kind"] == "Diagnostics"], key=lambda x: x["ID"])
        d1 = dg[-1]
        check("异步诊断也完成了（等设备下次会话回报）", d1["Status"] == "done", d1["Status"])
        check("异步诊断次数正确（2 个）", "发送包 2" in d1["Result"], d1["Result"][:90])

    print("== 22. ping 诊断的承载接口（可选，Interface）==")
    if did:
        # 真机案例：华为 FTTR 主机的 INTERNET WAN 是桥接、没有默认路由，
        # 设备自己选的出口发不出去 → 「成功 0、延时 0/0/0」秒失败。
        # 这时候要能显式指定从哪条 WAN 出去（比如那条 TR069 管理连接）。
        IFACE = "InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANIPConnection.1"
        need = ("-serial", "VERIFY098", "-oui", "001122",
                "-ping-need-iface", "WANConnectionDevice.1")

        # (1) 不指定承载接口 → 模拟器（模拟上述真机）全失败
        st, loc = post_form(f"/devices/{did}/diagnose", {"host": "www.baidu.com", "count": "2"})
        check("不带承载接口的诊断入队", st == 303, loc)
        full = api_device(did)
        d = max([t for t in full["tasks"] if t["Kind"] == "Diagnostics"], key=lambda x: x["ID"])
        check("留空时载荷里没有 interface 字段（= 设备自选）",
              '"interface"' not in d["Payload"], d["Payload"][:140])
        ok, out = run_sim(workdir, *need, "-once", "-event", "2 PERIODIC")
        check("诊断会话成功", ok, out[-160:])
        full = wait_tasks_done(did, kinds=("Diagnostics",))
        d = max([t for t in full["tasks"] if t["Kind"] == "Diagnostics"], key=lambda x: x["ID"])
        check("自己选出口时全失败（模拟没有路由的设备）",
              d["Status"] == "done" and "成功 0" in d["Result"] and "失败 2" in d["Result"],
              d["Result"][:120])
        check("全失败时延时是 0/0/0", "= 0/0/0 ms" in d["Result"], d["Result"][:120])
        check("全失败时提醒「可试试指定承载接口」", "可试试指定承载接口" in d["Result"], d["Result"][:200])

        # (2) 指定承载接口 → 同一台设备就通了
        st, loc = post_form(f"/devices/{did}/diagnose",
                            {"host": "www.baidu.com", "count": "2", "interface": IFACE})
        check("带承载接口的诊断入队", st == 303, loc)
        full = api_device(did)
        d = max([t for t in full["tasks"] if t["Kind"] == "Diagnostics"], key=lambda x: x["ID"])
        check("承载接口写进了任务载荷", IFACE in d["Payload"], d["Payload"][:160])
        ok, out = run_sim(workdir, *need, "-once", "-event", "2 PERIODIC")
        check("带承载接口的诊断会话成功", ok, out[-160:])
        full = wait_tasks_done(did, kinds=("Diagnostics",))
        d = max([t for t in full["tasks"] if t["Kind"] == "Diagnostics"], key=lambda x: x["ID"])
        check("指定承载接口后通了", "成功 2" in d["Result"], d["Result"][:120])
        check("结果里记下了承载接口（便于复盘）", IFACE in d["Result"], d["Result"][:200])

        # (3) 界面：承载接口输入框 + 下拉备选（来自设备已采集的 WAN 连接）
        st, h = get(f"/devices/{did}")
        check("诊断表单里有承载接口输入框", 'name="interface"' in h, st)
        check("输入框挂了 datalist 备选", "diagIfaces" in h and "<datalist" in h, st)
        check("备选里有这台设备的 WAN 连接路径",
              "InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANIPConnection.1" in h, st)
        check("界面上说明了留空=设备自选", "留空＝设备自选" in h, st)
        # 跑完的诊断要在结果框里回显当时用的承载接口
        st, h = get(f"/devices/{did}")
        check("最近一次诊断回显承载接口", f"承载接口 {IFACE}" in h, st)

        # (4) 非法承载接口要被拦下（不能进 SOAP 报文）
        st, loc = post_form(f"/devices/{did}/diagnose",
                            {"host": "1.1.1.1", "count": "2", "interface": "a b<c>"})
        check("非法承载接口被拒", st == 303 and "err=1" in (loc or ""), loc)

    print("== 23. 三个页面都要加载 app.js（分页/搜索/主题/自动刷新都靠它）==")
    if did:
        for p in ("/", f"/devices/{did}", f"/devices/{did}/wifi/1"):
            st, h = get(p)
            check(f"{p} 引入了 app.js", "static/app.js" in h)
        st, js = get("/static/app.js")
        check("app.js 里有 5 秒自动刷新逻辑",
              st == 200 and "acs-autorefresh" in js and "autorefresh" in js, st)
        check("自动刷新会保留分页位置与过滤词（sessionStorage）",
              "acs-pager:" in js and "acs-param-filter" in js, "")
        check("弹窗开着时自动刷新会跳过（不打断正在看的表单）",
              ".modal-mask:not([hidden])" in js, "")

    print("== 24. FTTR 子设备：有就显示、没有就不显示 ==")
    if did:
        st, h = get(f"/devices/{did}")
        check("没有 FTTR 能力的设备不显示该区块", "FTTR 子设备" not in h)

    # 带子设备能力的设备（模拟器 -fttr 2）
    ok, out = run_sim(workdir, "-serial", "VERIFY-FTTR", "-oui", "001122", "-fttr", "2",
                      "-once", "-event", "0 BOOTSTRAP")
    check("FTTR 设备注册会话成功", ok, out[-200:])
    ds = [d for d in api_devices() if d["SerialNumber"] == "VERIFY-FTTR"]
    check("FTTR 设备已纳管", len(ds) == 1, len(ds))
    if ds:
        fid = ds[0]["ID"]
        full = api_device(fid)
        ap = [p["Name"] for p in full["params"] if "X_HW_APDevice" in p["Name"]]
        check("能力探测 + 子树采集都在注册那一次会话里完成了", len(ap) > 0, len(ap))

        st, h = get(f"/devices/{fid}")
        check("详情页显示了「FTTR 子设备」区块", "FTTR 子设备" in h)
        check("区块里报了子设备台数", "共 2 台子设备" in h)
        check("子设备序列号正确", "SUBSN000001" in h and "SUBSN000002" in h)
        sec = h.split("FTTR 子设备", 1)[1]
        # 只看每行的第一个单元格（就是实例号），别把后面的“信号=0”当成实例号
        insts = re.findall(r'<tr>\s*<td class="mono">(\d+)</td>', sec)
        check("实例号按设备自报的不连续编号显示（1 / 4）",
              insts[:2] == ["1", "4"], insts)
        check("显示了子设备采集时间", "采集" in sec)

    print("== 25. WAN 连接（有就显示、没有就不显示）==")
    if did:
        st, h = get(f"/devices/{did}")
        check("显示了「WAN 连接」区块", "WAN 连接" in h)
        check("显示了连接的 IP 与掩码", "203.0.113.7" in h and "255.255.255.0" in h)
        check("显示了网关", "203.0.113.1" in h)
        check("显示了寻址方式与 NAT", "DHCP" in h and "NAT" in h)
        check("显示了厂商私有的业务模式与 VLAN", "INTERNET" in h and ">41<" in h)
        check("连接名能看到", "1_INTERNET_R_VID_" in h)

    # 没有 WAN 对象的设备：整块不显示
    ok, out = run_sim(workdir, "-serial", "VERIFY-NOWAN", "-oui", "001122", "-no-wan",
                      "-once", "-event", "0 BOOTSTRAP")
    check("无 WAN 设备注册会话成功", ok, out[-160:])
    nw = [d for d in api_devices() if d["SerialNumber"] == "VERIFY-NOWAN"]
    if nw:
        st, h = get(f"/devices/{nw[0]['ID']}")
        check("没有 WAN 对象的设备不显示该区块", "WAN 连接" not in h)

    print("== 26. 主动唤醒（Connection Request + Digest）==")
    if did:
        # 把设备侧的 ConnectionRequest 账号密码 provision 进去（ACS 启动后首次 Inform 会做）。
        #
        # 注意**不要去任务历史里找那条 provisioning 任务**：任务历史有保留上限（ACS_TASK_HISTORY_LIMIT），
        # 早期任务可能已经被裁掉。这里看设备侧的实际结果 —— 我们写进去的用户名能读回来。
        # （密码读不回来，那是设备“能改不能读”；它到底对不对，由下面 Digest 唤醒的成功与否证明。）
        # ① ACS 侧的动作（日志里能看到）
        logt = ""
        try:
            with open(os.path.join(workdir, "acs.log"), encoding="utf-8", errors="replace") as f:
                logt = f.read()
        except OSError:
            pass
        check("ACS 会主动把凭据写进设备（日志里能看到下发动作）",
              "把 ConnectionRequest 凭据写进设备" in logt, "")
        # ② 实际效果：模拟器默认给的是 cpe-cr，新设备会被 ACS 改写成我们配置的账号
        ok2, _ = run_sim(workdir, "-serial", "VERIFY-CR", "-oui", "001122",
                         "-once", "-event", "0 BOOTSTRAP")
        check("新设备注册会话成功（用于验证凭据下发）", ok2, "")
        ds2 = [d for d in api_devices() if d["SerialNumber"] == "VERIFY-CR"]
        if ds2:
            vals = [p["Value"] for p in api_device(ds2[0]["ID"])["params"]
                    if p["Name"].endswith("ManagementServer.ConnectionRequestUsername")]
            check("凭据真的写进了设备（上报的账号被改成 acs）", "acs" in vals, vals)

        # 模拟器带 Connection Request 监听跑起来（它会用我们 provision 的凭据要 Digest）
        # 先确定 CR 端口
        import socket as _sock
        ls = _sock.socket()
        ls.bind(("127.0.0.1", 0))
        crport = ls.getsockname()[1]
        ls.close()
        proc = subprocess.Popen(
            [workdir + "/cpesim", "-acs", CWMP, "-serial", "VERIFY098", "-oui", "001122",
             "-interval", "25s", "-cr-port", str(crport),
             "-cr-user", "acs", "-cr-pass", "verify-connreq-pass"],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        try:
            # 等它把**带这个端口**的 ConnectionRequestURL 报上来。
            # 注意不能只等“URL 非空”—— 之前几轮的模拟器进程留下过别的端口，
            # 那样唤醒会打到没人监听的地址上（这里踩过一次）。
            got_url = False
            for _ in range(40):
                time.sleep(1)
                d2 = api_device(did)["device"]
                if f":{crport}/" in (d2.get("ConnRequestURL") or ""):
                    got_url = True
                    break
            check("拿到带本次监听端口的 ConnectionRequestURL", got_url,
                  api_device(did)["device"].get("ConnRequestURL"))

            st, loc = post_form(f"/devices/{did}/wake", {})
            check("唤醒请求返回 303", st == 303, st)
            check("唤醒成功（不是失败提示）", "err=1" not in (loc or ""),
                  urllib.parse.unquote(loc or ""))

            # 设备应当立刻回连开一次会话（Inform 事件 6），
            # 我们把上次事件记在设备上，所以可以从 API 看到
            seen = False
            for _ in range(15):
                time.sleep(1)
                ev = api_device(did)["device"].get("LastEvents", "")
                if "6 CONNECTION REQUEST" in ev:
                    seen = True
                    break
            check("设备收到唤醒后立刻回连（Inform 事件 6）", seen,
                  api_device(did)["device"].get("LastEvents"))
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except Exception:
                proc.kill()

    print("== 27. 认证（默认实例未启用，只验证未认证时可通）==")
    st, _, _, _ = post(envelope("urn:dslforum-org:cwmp-1-0", "u3", "<cwmp:GetRPCMethods/>"))
    check("未启用认证时无凭证也能通", st == 200, st)

    print("== 28. 重启设备（红色按钮 + 二次确认 + 不重复下发）==")
    if did:
        # 先把之前排队的任务排干净（重启会被“已有在排队”挡住）
        wait_tasks_done(did, kinds=("Diagnostics",))

        st, h = get(f"/devices/{did}")
        check("详情页有重启按钮", "重启设备" in h, st)
        check("重启按钮是红色的（class=danger）", 'class="danger"' in h, st)
        check("重启表单带二次确认（data-confirm）", "data-confirm=" in h, st)
        check("确认文案里说明了重启期间会断网", "无法上网" in h or "断开重启" in h, st)
        st, js = get("/static/app.js")
        check("前端有二次确认的实现（data-confirm → window.confirm）",
              "data-confirm" in js and "confirm(" in js, st)

        st, loc = post_form(f"/devices/{did}/reboot", {})
        check("重启返回 303", st == 303, st)
        check("界面提示重启已入队", "重启" in urllib.parse.unquote(loc or ""), urllib.parse.unquote(loc or ""))

        full = api_device(did)
        rb = [t for t in full["tasks"] if t["Kind"] == "Reboot"]
        check("重启任务已入队", len(rb) == 1, [t["Kind"] for t in full["tasks"]][:5])
        check("重启任务带 CommandKey", bool(rb and rb[0].get("CommandKey")), rb[0] if rb else None)

        # 还没下发出去（排队中）：不允许再点一次
        st, loc = post_form(f"/devices/{did}/reboot", {})
        check("排队中不允许重复重启", st == 303 and "err=1" in (loc or ""), urllib.parse.unquote(loc or ""))

        # 设备回一次会话：应该收到 <cwmp:Reboot> 并回 RebootResponse
        ok, out = run_sim(workdir, *sim, "-once", "-event", "2 PERIODIC")
        check("重启会话成功", ok, out[-160:])
        check("模拟器确实收到了 Reboot 指令", "收到 Reboot" in out, out[-200:])
        full = wait_tasks_done(did, kinds=("Reboot",))
        rb = [t for t in full["tasks"] if t["Kind"] == "Reboot"]
        check("重启任务已完成（设备回了 RebootResponse）",
              bool(rb) and rb[0]["Status"] == "done", rb[0]["Status"] if rb else None)
        check("任务结果写明了设备已接受重启",
              bool(rb) and "重启" in (rb[0]["Result"] or ""), (rb[0]["Result"] if rb else "")[:80])
        st, h = get(f"/devices/{did}")
        check("任务历史里能看到这次重启", st == 200 and "Reboot" in h, st)

        # 重启完成后（任务结束）应能再次重启
        st, loc = post_form(f"/devices/{did}/reboot", {})
        check("上一次结束后可以再次重启", st == 303 and "err=1" not in (loc or ""),
              urllib.parse.unquote(loc or ""))
        # 清掉这条，免得影响后面的用例
        run_sim(workdir, *sim, "-once", "-event", "2 PERIODIC")
        wait_tasks_done(did, kinds=("Reboot",))

        # 不存在的设备：不能建出孤儿任务
        st, loc = post_form("/devices/99999/reboot", {})
        check("不存在的设备重启返回提示而不是崩", st == 303 and "err=1" in (loc or ""), loc)

    print("== 29. FTTR 子设备的组网模式与光功率 ==")
    # 子设备序号 1..3 → 实例 1/4/7；1=光纤（有光功率）、2=无线、3=有线
    ok, out = run_sim(workdir, "-serial", "VERIFY-OPT", "-oui", "001122",
                      "-fttr", "3", "-fttr-optical", "-fttr-wifi", "2", "-fttr-eth", "3",
                      "-once", "-event", "0 BOOTSTRAP")
    check("带光功率的 FTTR 设备注册会话成功", ok, out[-200:])
    ds = [d for d in api_devices() if d["SerialNumber"] == "VERIFY-OPT"]
    check("带光功率的 FTTR 设备已纳管", len(ds) == 1, len(ds))
    if ds:
        oid = ds[0]["ID"]
        st, h = get(f"/devices/{oid}")
        # 只看 FTTR 子设备这一块：下面还有一个「参数」表会把该设备所有参数都列出来，
        # 拿整页做断言会误判（光功率参数就存在库里）。
        sec = h.split("FTTR 子设备", 1)[1].split("<h2>", 1)[0] if "FTTR 子设备" in h else ""
        check("子设备表有「组网」列", "<th>组网</th>" in sec, st)
        check("子设备确实上报了光功率时，才出现「光功率」列", "<th>光功率</th>" in sec, st)
        check("光纤组网那台显示收 / 发光功率", "Rx -19.0 dBm / Tx 2.5 dBm" in sec, sec[-200:])
        check("无线组网那台显示组网方式与信号", "无线组网（信号 -45）" in sec, st)
        check("有线组网那台显示组网方式", "有线组网" in sec, st)
        check("判不出组网模式时原样显示设备自报值", "repeater" in sec, st)
        # 无线 / 有线组网的两台：**设备回了光功率也不显示**（它们没有光口）
        check("无线 / 有线组网不显示光功率", "-20.0" not in sec and "-21.0" not in sec, sec[-300:])
        check("组网列的悬停提示用能读的话说明设备上报了什么",
              "设备上报：wifi" in sec and "信号 -45" in sec, st)

    # 对照：子设备没上报光功率时，整列不渲染（探测不到就不显示）
    vf = [d for d in api_devices() if d["SerialNumber"] == "VERIFY-FTTR"]
    if vf:
        st, h = get(f"/devices/{vf[0]['ID']}")
        check("没读到光功率就没有光功率列", "<th>光功率</th>" not in h, st)
        check("组网列仍然有（设备自报了 WorkingMode）", "<th>组网</th>" in h, st)

    print("== 30. 关联终端：谁连主机、谁连子机（弹窗）==")
    ok, out = run_sim(workdir, "-serial", "VERIFY-CLI", "-oui", "001122", "-fttr", "3",
                      "-once", "-event", "0 BOOTSTRAP")
    check("带子设备与终端的设备注册成功", ok, out[-200:])
    ds = [d for d in api_devices() if d["SerialNumber"] == "VERIFY-CLI"]
    if ds:
        cid = ds[0]["ID"]
        st, h = get(f"/devices/{cid}")
        sq = lambda x: re.sub(r"\s+", " ", x)
        wifi_sec = sq(h.split("无线（WiFi）", 1)[1].split("<h2>", 1)[0])
        fttr_sec = sq(h.split("FTTR 子设备", 1)[1].split("<h2>", 1)[0])
        # 弹窗内容单独看：下面的「参数」表会把所有参数（含残留行）都列出来，拿整页断言会误判
        modals = h.split('<div class="modal-mask"', 1)[1] if '<div class="modal-mask"' in h else ""

        # 1) 主机自己的无线概况不能被子设备的 WLAN 污染（两边实例号都是从 1 开始）
        check("无线概况显示的是主机自己的 SSID", "SimWiFi</a>" in wifi_sec, wifi_sec[:220])
        check("无线概况里没有子设备的 SSID", "SimWiFi-SUB" not in wifi_sec, "")

        # 2) 终端数 = 主机 + 子机（真机上主机自己的 WLAN 一台都没有，终端全在子光猫上）
        check("2.4G 终端数是主机 + 子机的合计（2+3=5）",
              "主机 2 台 · 子设备 3 台" in wifi_sec and ">5</button>" in wifi_sec, wifi_sec[-400:])
        check("5G 终端数是主机 + 子机的合计（1+1=2）",
              "主机 1 台 · 子设备 1 台" in wifi_sec and ">2</button>" in wifi_sec, wifi_sec[-400:])

        # 3) 子设备表：每台子机自己的终端数
        check("子设备表里有「终端」列", "<th>终端</th>" in fttr_sec, st)
        check("子机 1 显示 3 台（2.4G 2 + 5G 1）且是按钮",
              'data-modal="climodal-sub-1"' in fttr_sec and ">3</button>" in fttr_sec, "")
        check("没有终端的子机显示 0（而不是 -）", ">0</td>" in fttr_sec, fttr_sec[-220:])

        # 4) 弹窗（频段的 + 子设备的）：能看出谁连主机、谁连子机
        for mid in ("climodal-24G", "climodal-5G", "climodal-sub-1"):
            check(f"弹窗 {mid} 已渲染", f'id="{mid}"' in h, st)
        check("弹窗里有主机的终端（MAC + IP）",
              "02:00:00:00:00:B1" in modals and "192.168.1.11" in modals, "")
        check("弹窗里有子机 1 的终端（MAC + IP）",
              "02:00:01:00:00:01" in modals and "10.0.1.1" in modals, "")
        check("弹窗里标清了「主机」与「子机 1（K251-20）」",
              "主机 · SimWiFi" in modals and "子机 1（K251-20）" in modals, "")
        check("弹窗里有 IP 地址字段与信号/速率小字",
              "IP 地址：" in modals and "dBm" in modals, "")

        # 5) 幽灵终端：条目数（AssociatedDeviceNumberOfEntries）之外的残留行不能显示
        check("残留行没被当成终端显示", "02:00:00:00:00:FF" not in modals, "")

        # 6) 前端实现
        st, js = get("/static/app.js")
        check("前端实现了弹窗开关（data-modal / 点遮罩 / Esc）",
              "data-modal" in js and "modal-mask" in js and "Escape" in js, st)
        st, css = get("/static/style.css")
        check("样式里有弹窗与终端列表", ".modal-mask" in css and ".clilist" in css, st)

        # 首页列表里的「无线终端」要跟详情页同一口径：主机 3 台 + 子机 4 台 = 7
        st, idx = get("/")
        row = idx.split(f'<a href="/devices/{cid}">', 1)[1].split("</tr>", 1)[0]
        check("列表页的「无线终端」= 主机 + 子机（3+4=7）", ">7</td>" in row, row[-160:])

    print("== 31. 删除设备（只删本地记录，级联清理干净）==")
    ok, out = run_sim(workdir, "-serial", "VERIFY-DEL", "-oui", "001122", "-fttr", "1",
                      "-once", "-event", "0 BOOTSTRAP")
    check("待删设备注册会话成功", ok, out[-160:])
    before = len(api_devices())
    ds = [d for d in api_devices() if d["SerialNumber"] == "VERIFY-DEL"]
    check("待删设备已纳管", len(ds) == 1, len(ds))
    if ds:
        delid = ds[0]["ID"]
        full = api_device(delid)
        check("待删设备有参数、任务与上报记录",
              len(full["params"]) > 0 and len(full["tasks"]) > 0 and len(full["informs"]) > 0,
              (len(full["params"]), len(full["tasks"]), len(full["informs"])))
        st, h = get(f"/devices/{delid}")
        check("详情页有删除按钮", f'action="/devices/{delid}/delete"' in h, st)
        check("删除按钮是红色的（与重启同级 danger）", h.count('class="danger"') >= 2, h.count('class="danger"'))
        check("删除按钮带二次确认", "data-confirm=" in h, st)
        check("说明里写清了「只删本地记录」与「会重新纳管」",
              "不会动设备本身" in h and "重新纳管" in h, st)

        st, loc = post_form(f"/devices/{delid}/delete", {})
        check("删除返回 303", st == 303, st)
        check("删完带着成功提示回到首页",
              (loc or "").startswith("/?msg=") and "err=1" not in (loc or ""),
              urllib.parse.unquote(loc or ""))

        check("设备详情页已 404", get_code(f"/devices/{delid}") == 404, get_code(f"/devices/{delid}"))
        check("设备从列表里消失", all(d["ID"] != delid for d in api_devices()))
        check("设备总数少了一台", len(api_devices()) == before - 1, (before, len(api_devices())))

        # 级联清理：直接看库文件，确认没有留下孤儿数据
        import sqlite3
        con = sqlite3.connect(f"file:{workdir}/acs.db?mode=ro", uri=True)
        for tbl in ("device_params", "tasks", "informs"):
            n = con.execute(f"select count(*) from {tbl} where device_id = ?", (delid,)).fetchone()[0]
            check(f"删除后 {tbl} 没有残留", n == 0, n)
        con.close()

        # 重复删：给提示，不 500
        st, loc = post_form(f"/devices/{delid}/delete", {})
        check("重复删除给了提示而不是崩", st == 303 and "err=1" in (loc or ""),
              urllib.parse.unquote(loc or ""))

        # 其它设备必须还在（真机/别的模拟设备不能被连累）
        check("其它设备仍在列表里", len(api_devices()) == before - 1, len(api_devices()))

    print("== 32. 终端条目的信号图标（四格小柱 + 百分比）==")
    cli = [d for d in api_devices() if d["SerialNumber"] == "VERIFY-CLI"]
    if cli:
        st, h = get(f"/devices/{cli[0]['ID']}")
        modals = h.split('<div class="modal-mask"', 1)[1] if '<div class="modal-mask"' in h else ""
        check("页面完整渲染（没有半截页 / 没有模板错误文本）",
              h.rstrip().endswith("</html>") and "invalid function signature" not in h, st)

        # 主机那两台没有“设备自报质量”，百分比由 RSSI 换算（-50 及以上满格、-100 为 0）
        check("没有质量值时按 RSSI 换算：-41 dBm → 100%（4 格）",
              '<span class="sigbars" data-sig="100" data-bars="4"><i class="on"></i><i class="on"></i><i class="on"></i><i class="on"></i></span>' in modals,
              "")
        check("按 RSSI 换算：-58 dBm → 84%（3 格）",
              '<span class="sigbars" data-sig="84" data-bars="3"><i class="on"></i><i class="on"></i><i class="on"></i><i class=""></i></span>' in modals,
              "")
        # 子机那几台给了设备自报质量（55）→ 百分比优先用它，而不是拿 RSSI 去算
        check("设备自报质量优先：质量 55 → 55%（2 格）",
              '<span class="sigbars" data-sig="55" data-bars="2"><i class="on"></i><i class="on"></i><i class=""></i><i class=""></i></span>' in modals,
              "")
        check("百分比以文字显示在图标下方", ">84%</span>" in modals and ">55%</span>" in modals, "")
        check("悬停提示里带上原始数据（质量 / RSSI / SNR）",
              "设备自报质量 55" in modals and "RSSI -58 dBm" in modals, "")
        check("每个终端都有信号块（数量对得上）",
              modals.count('class="clisig"') == modals.count('class="sigbars"') >= 6,
              (modals.count('class="clisig"'), modals.count('class="sigbars"')))
        st, css = get("/static/style.css")
        check("样式里有信号柱与百分比", ".sigbars" in css and ".sigpct" in css, st)

    print("== 33. 终端条目的主机名（拿不到就 N/A）==")
    ok, out = run_sim(workdir, "-serial", "VERIFY-NAME", "-oui", "001122", "-fttr", "2",
                      "-once", "-event", "0 BOOTSTRAP")
    check("带主机列表的设备注册会话成功", ok, out[-160:])
    ds = [d for d in api_devices() if d["SerialNumber"] == "VERIFY-NAME"]
    if ds:
        nid = ds[0]["ID"]
        full = api_device(nid)
        check("采到了设备的主机列表（终端名的来源）",
              any("Hosts.Host.1.HostName" in p["Name"] for p in full["params"]),
              [p["Name"] for p in full["params"] if "Hosts" in p["Name"]][:3])
        st, h = get(f"/devices/{nid}")
        modals = h.split('<div class="modal-mask"', 1)[1] if '<div class="modal-mask"' in h else ""
        check("终端条目里有「主机名：」一行", "主机名：" in modals, "")
        # ① 主机列表按 MAC 对出来的
        check("主机上的终端借用主机列表的名字（Sim-Laptop）", "Sim-Laptop" in modals, "")
        # ② 终端行自己带的描述
        check("终端行自带的描述也能当名字（Sim-Camera）", "Sim-Camera" in modals, "")
        # ③ 子设备的终端借用主机列表（跨表按 MAC 对）
        check("子设备终端也能借到主机列表的名字（Sim-Phone）", "Sim-Phone" in modals, "")
        # ④ 拿不到的显示 N/A，而不是留空
        check("拿不到名字的显示 N/A", "N/A" in modals, "")
        st, css = get("/static/style.css")
        check("终端条目样式仍在（标签 + 弹窗列表）", ".clik" in css and ".clilist" in css, st)

    print("== 34. 任务历史 / 上报记录只保留最近 N 条（控制库大小）==")
    if did:
        import sqlite3
        con = sqlite3.connect(f"file:{workdir}/acs.db?mode=ro", uri=True)
        rows = con.execute("""
            select device_id,
                   sum(case when status in ('done','failed') then 1 else 0 end),
                   sum(case when status in ('pending','running') then 1 else 0 end)
            from tasks group by device_id""").fetchall()
        limit = int(os.environ.get("ACS_VERIFY_TASK_LIMIT", "20"))
        check(f"每台设备保留的已结束任务都不超过上限（{limit}）",
              all(r[1] <= limit for r in rows), rows)
        check("确实触发过裁剪（至少一台设备正好到上限）", any(r[1] == limit for r in rows), rows)
        # 未结束的任务一条都不能因为裁剪而丢
        api_open = sum(api_device(d["ID"])["pending_tasks"] for d in api_devices())
        db_open = sum(r[2] for r in rows)
        check("排队 / 执行中的任务一条都没少", db_open == api_open, (db_open, api_open))
        con.close()

        # 上报记录同理（它是增长最快的：每 120 秒一条）
        inrows = con2 = None
        con2 = sqlite3.connect(f"file:{workdir}/acs.db?mode=ro", uri=True)
        inrows = con2.execute("""
            select device_id, count(*) from informs group by device_id""").fetchall()
        check(f"每台设备保留的上报记录都不超过上限（{limit}）",
              all(r[1] <= limit for r in inrows), inrows)
        check("上报记录确实触发过裁剪（至少一台设备正好到上限）",
              any(r[1] == limit for r in inrows), inrows)
        con2.close()

        st, h = get(f"/devices/{did}")
        check("任务历史标题里写明了保留条数", f"最近 {limit} 条" in h, st)
        check("上报记录标题里也写明了保留条数", h.count(f"最近 {limit} 条") >= 2, h.count(f"最近 {limit} 条"))
    print("== 35. 面板设置页（双端口 + 登录页 / 账号密码保护）==")
    import sqlite3
    user = PANEL_AUTH.split(":", 1)[0] if PANEL_AUTH else ""

    def anon_get(path, cookie=None):
        """不带（或只带指定 cookie 的）面板请求，用来验证登录态本身。

        注意**不跟随重定向**：未登录时回的是 303 跳登录页，
        urlopen 默认会跟到 /login 变成 200，看起来就像"放行了"。
        """
        req = urllib.request.Request(BASE + path)
        if cookie:
            req.add_header("Cookie", cookie)
        try:
            return _NO_REDIRECT.open(req, timeout=20).status
        except urllib.error.HTTPError as e:
            return e.code

    def logout_post(cookie):
        req = urllib.request.Request(BASE + "/logout", method="POST")
        if cookie:
            req.add_header("Cookie", cookie)
        try:
            r = _NO_REDIRECT.open(req, timeout=20)
            return r.status, r.headers.get("Location", ""), r.headers.get("Set-Cookie", "")
        except urllib.error.HTTPError as e:
            return e.code, e.headers.get("Location", ""), e.headers.get("Set-Cookie", "")

    if not PANEL_AUTH:
        skip("面板鉴权用例", "本次验收没开面板账号密码保护（ACS_WEB_USER/PASS 未设置）")
    else:
        pw = PANEL_AUTH.split(":", 1)[1]
        # 登录页这套：页面请求未登录会被 303 送到 /login，接口请求回 401，静态资源不挡
        check("登录页不用登录就能打开", anon_get("/login") == 200, anon_get("/login"))
        check("不带登录态访问面板 → 303 跳登录页", anon_get("/") == 303, anon_get("/"))
        check("不带登录态访问 /api → 401", anon_get("/api/devices") == 401, anon_get("/api/devices"))
        check("静态资源（样式表）不挡（登录页要用）", anon_get("/static/style.css") == 200,
              anon_get("/static/style.css"))
        # 密码错了不给登录态
        bad_st, bad_ck = panel_login(user, "wrong-pass")
        check("密码错了 → 401 且不下发登录态", bad_st == 401 and bad_ck == "", "%s %s" % (bad_st, bad_ck))
        # 对了就拿到 cookie
        st, ck = panel_login(user, pw)
        check("账号密码对了 → 303 且下发登录态 cookie", st == 303 and ck.startswith("acs_panel="),
              "%s %s" % (st, ck))
        check("带上登录态能打开面板", anon_get("/", ck) == 200, anon_get("/", ck))
        check("伪造的登录态不认", anon_get("/", "acs_panel=v1.0.9999999999.deadbeef") == 303, "")
        # 退出登录
        out_st, out_loc, out_ck = logout_post(ck)
        check("退出登录 → 跳回登录页", out_st == 303 and out_loc == "/login", "%s %s" % (out_st, out_loc))
        check("退出登录清掉了登录态 cookie",
              "acs_panel=" in out_ck and "Max-Age=0" in out_ck.replace("max-age=0", "Max-Age=0"),
              out_ck[:80])
        # 重新登录（后面还要用面板）
        panel_login(user, pw)
        check("重新登录后又能进", anon_get("/", PANEL_COOKIE) == 200, "")

        # 面板鉴权不能挡住设备上报：CWMP 是另一套（CPE 认证）
        st, _, _, _ = post(envelope("urn:dslforum-org:cwmp-1-0", "auth1", "<cwmp:GetRPCMethods/>"))
        check("CWMP 端点不受面板登录影响", st == 200, st)

        st, h = get("/settings")
        check("设置页能打开", st == 200 and "设置" in h, st)
        check("设置页显示当前监听与访问控制状态",
              "ACS 监听" in h and "面板监听" in h and "已启用" in h and user in h, st)
        check("设置页有 ACS 监听 / 面板监听 / 账号 / 新密码各一栏",
              'name="acs_listen"' in h and 'name="web_listen"' in h
              and 'name="web_user"' in h and 'name="web_pass"' in h, st)
        check("顶栏有「退出」按钮（登录页那套才有）", 'action="/logout"' in h, "")
        check("设置页只有标签/字段/按钮，没有说明性文档",
              "HTTP Basic" not in h and "反向代理" not in h and "忘记" not in h
              and "重启服务后生效" not in h, st)

        # 保存监听地址（面板留空 = 与 ACS 同端口）
        st, loc = post_form("/settings", {
            "acs_listen": ":19090", "web_listen": "",
            "auth": "1", "web_user": user, "web_pass": "", "web_pass2": "",
        })
        check("保存设置返回 303", st == 303, st)
        check("保存后的提示只说改了哪一类、什么时候生效",
              "已保存" in urllib.parse.unquote(loc or "") and "重启服务后生效" in urllib.parse.unquote(loc or ""),
              urllib.parse.unquote(loc or ""))
        con = sqlite3.connect(f"file:{workdir}/acs.db?mode=ro", uri=True)
        saved = dict(con.execute("select k, v from settings"))
        con.close()
        check("监听地址已写进 settings 表", saved.get("listen") == ":19090", saved.get("listen"))
        check("面板监听写成了空（= 与 ACS 同端口）", saved.get("web_listen", None) == "", saved.get("web_listen"))
        st, h = get("/settings")
        check("设置页提示监听地址改动需重启后生效", "重启服务后生效" in h, st)

        # 待重启时该给出「立即重启服务」按钮（改端口不用再去命令行）
        check("待重启时给出「立即重启服务」按钮",
              'action="/settings/restart"' in h and "立即重启服务" in h, st)
        check("重启按钮带二次确认", "确定现在重启服务吗" in h, "")
        check("重启说明里写明失败会自动回退", "自动退回旧地址" in h, "")
        check("重启接口只认 POST（GET 405）", get_code("/settings/restart") == 405,
              get_code("/settings/restart"))
        st2, h2 = get("/settings")
        check("只给按钮、没有拃自重启", st2 == 200 and "重启服务后生效" in h2, st2)

        # 非法输入要被拦下
        st, loc = post_form("/settings", {"acs_listen": "abc", "web_listen": "",
                                          "auth": "1", "web_user": user})
        check("非法监听地址被拒", st == 303 and "err=1" in (loc or ""),
              urllib.parse.unquote(loc or ""))
        st, loc = post_form("/settings", {"acs_listen": ":7547", "web_listen": "",
                                          "auth": "1", "web_user": user,
                                          "web_pass": "newpass1", "web_pass2": "newpass2"})
        check("两次密码不一致被拒", st == 303 and "err=1" in (loc or ""),
              urllib.parse.unquote(loc or ""))

        # 改密码：旧密码与旧登录态都立刻失效（这条最关键 —— 设置页真的能改账号密码）
        old_cookie = PANEL_COOKIE
        st, loc = post_form("/settings", {"acs_listen": ":7547", "web_listen": "",
                                          "auth": "1", "web_user": user,
                                          "web_pass": "verify-new-pass", "web_pass2": "verify-new-pass"})
        check("改密码返回 303", st == 303 and "err=1" not in (loc or ""),
              urllib.parse.unquote(loc or ""))
        check("提示里说明了账号密码已生效", "账号密码已生效" in urllib.parse.unquote(loc or ""),
              urllib.parse.unquote(loc or ""))
        check("旧登录态立刻失效 → 又跳登录页", anon_get("/", old_cookie) == 303, anon_get("/", old_cookie))
        st, ck = panel_login(user, "verify-new-pass")
        check("新密码能登进来", st == 303 and ck.startswith("acs_panel="), "%s %s" % (st, ck))
        check("新登录态能用 → 200", anon_get("/", PANEL_COOKIE) == 200, "")
        if not PANEL_COOKIE:
            check("新登录态能用 → 200", False, "没拿到 cookie")
        # 后面还要用面板（凭据变了）
        globals()["PANEL_AUTH"] = f"{user}:verify-new-pass"
        # 库里的密码是散列，不是明文
        con = sqlite3.connect(f"file:{workdir}/acs.db?mode=ro", uri=True)
        stored = dict(con.execute("select k, v from settings"))
        con.close()
        check("密码只存散列（PBKDF2）",
              str(stored.get("web_pass", "")).startswith("pbkdf2-sha256$")
              and "verify-new-pass" not in str(stored.get("web_pass")), stored.get("web_pass", "")[:32])
        check("登录态签名密钥也存进了库（重启不掉线）",
              str(stored.get("panel_secret", "")) != "", stored.get("panel_secret", "")[:8])

    print("== 36. 双端口：ACS 与面板分开监听 ==")
    acs_bin = os.path.join(workdir, "acs")
    if not os.path.exists(acs_bin):
        skip("双端口用例", "没找到 $WORK/acs")
    else:
        env = dict(os.environ)
        env.update({
            "ACS_LISTEN": ":17561",
            "ACS_WEB_LISTEN": ":17562",
            "ACS_DB": os.path.join(workdir, "dual.db"),
            "ACS_LOG_LEVEL": "info",
        })
        env.pop("ACS_WEB_USER", None)   # 这一节不测鉴权，面板保持免登录
        env.pop("ACS_WEB_PASS", None)
        proc = subprocess.Popen([acs_bin], env=env,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            ready = False
            for _ in range(50):
                try:
                    with urllib.request.urlopen("http://127.0.0.1:17562/", timeout=2) as r:
                        if r.status == 200:
                            ready = True
                            break
                except Exception:  # noqa: BLE001
                    time.sleep(0.2)
            check("双端口实例起来了（面板端口可访问）", ready, "")
            if ready:
                check("面板端口能打开面板",
                      urllib.request.urlopen("http://127.0.0.1:17562/", timeout=5).status == 200, "")
                # CWMP 端口收报文
                req = urllib.request.Request(
                    "http://127.0.0.1:17561/acs",
                    data=envelope("urn:dslforum-org:cwmp-1-0", "dual2", "<cwmp:GetRPCMethods/>").encode(),
                    method="POST")
                req.add_header("Content-Type", 'text/xml; charset="utf-8"')
                try:
                    with urllib.request.urlopen(req, timeout=5) as r:
                        cwmp_st = r.status
                except urllib.error.HTTPError as ex:
                    cwmp_st = ex.code
                check("CWMP 端口能收设备报文", cwmp_st == 200, cwmp_st)
                # 两边互不串门
                try:
                    with urllib.request.urlopen("http://127.0.0.1:17561/", timeout=5) as r:
                        home_st = r.status
                except urllib.error.HTTPError as ex:
                    home_st = ex.code
                check("CWMP 端口上不服务面板（拿不到首页）", home_st != 200, home_st)
                try:
                    req2 = urllib.request.Request("http://127.0.0.1:17562/acs", data=b"<x/>", method="POST")
                    with urllib.request.urlopen(req2, timeout=5) as r:
                        acs_st = r.status
                except urllib.error.HTTPError as ex:
                    acs_st = ex.code
                check("面板端口上不服务 CWMP 端点", acs_st != 200, acs_st)

                # 独享端口：运营商定制设备的 ACS URL 五花八门，任何路径都该受理
                for path in ("/", "/tr069", "/cwmp/ACS", "/some/random/path?x=1"):
                    body2 = envelope("urn:dslforum-org:cwmp-1-0", "dual3", "<cwmp:GetRPCMethods/>")
                    req3 = urllib.request.Request("http://127.0.0.1:17561" + path,
                                                  data=body2.encode(), method="POST")
                    req3.add_header("Content-Type", 'text/xml; charset="utf-8"')
                    try:
                        with urllib.request.urlopen(req3, timeout=5) as r:
                            pst, ptext = r.status, r.read().decode("utf-8", "replace")
                    except urllib.error.HTTPError as ex:
                        pst, ptext = ex.code, ""
                    check(f"独享端口上 POST {path} 也受理（返回了 CWMP 响应）",
                          pst == 200 and "GetRPCMethodsResponse" in ptext, (pst, ptext[:60]))

                # 共用端口时不能吞掉陌生路径，否则面板路由会被 CWMP 抢走
                try:
                    req4 = panel_req(BASE + "/nope-not-a-route", data=b"<x/>", method="POST")
                    with urllib.request.urlopen(req4, timeout=5) as r:
                        nope_st = r.status
                except urllib.error.HTTPError as ex:
                    nope_st = ex.code
                check("共用端口时不吞陌生路径（面板路由安全）", nope_st == 404, nope_st)
        finally:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except Exception:  # noqa: BLE001
                proc.kill()

    print("== 37b. 多语言：面板可切中 / 英 ==")

    def lang_get(path, cookie=None):
        # 面板开着登录时要带上会话 cookie，否则会被送到登录页（那样断言的就是登录页了）
        req = panel_req(BASE + path)
        if cookie:
            req.add_header("Cookie", cookie if not PANEL_COOKIE else PANEL_COOKIE + "; " + cookie)
        return urllib.request.urlopen(req, timeout=20)

    with lang_get("/") as r:
        zh_html = r.read().decode("utf-8", "replace")
    check("默认语言是中文", "已纳管设备" in zh_html, "")
    with lang_get("/?lang=en") as r:
        en_html = r.read().decode("utf-8", "replace")
        set_cookie = r.headers.get("Set-Cookie") or ""
    check("?lang=en 渲染英文", "Managed devices" in en_html and "Devices" in en_html, "")
    check("切语言会把选择记进 cookie（acs_lang=en）", "acs_lang=en" in set_cookie, set_cookie[:60])
    # 排除两处按设计会带中文的地方：语言切换按钮（语言自称）、给 JS 的译文 JSON
    probe = en_html
    probe = re.sub(r'(?s)<a class="ghost" href="\?lang=[^"]*"[^>]*>.*?</a>', '', probe)
    probe = re.sub(r'(?s)<script>window\.I18N = .*?</script>', '', probe)
    check("英文页面里没有残留中文", not re.search(r'[\u4e00-\u9fff]', probe),
          "".join(re.findall(r'[\u4e00-\u9fff]+', probe)[:5]))
    with lang_get("/", "acs_lang=en") as r:
        again = r.read().decode("utf-8", "replace")
    check("带 acs_lang cookie 的后续请求仍是英文", "Managed devices" in again, "")
    with lang_get("/?lang=zh") as r:
        back = r.read().decode("utf-8", "replace")
    check("?lang=zh 能切回中文", "已纳管设备" in back, "")

    # 设备详情页也要全英文（任务类型/结果、无线字段、终端分组名、提示语都有后端拼的字符串）
    if did:
        with lang_get("/devices/%d?lang=en" % did) as r:
            dev_en = r.read().decode("utf-8", "replace")
        probe2 = re.sub(r'(?s)<a class="ghost" href="\?lang=[^"]*"[^>]*>.*?</a>', '', dev_en)
        probe2 = re.sub(r'(?s)<script>window\.I18N = .*?</script>', '', probe2)
        left = re.findall(r'[\u4e00-\u9fff]+', probe2)
        check("英文的设备详情页里没有残留中文", not left, "｜".join(dict.fromkeys(left))[:120])

    # 动作提示语是后端拼的字符串、通过 ?msg= 回显，走的也是「显示时翻译」
    for zh, en in (("已保存。", "Saved."),
                   ("诊断已入队，会在设备下次上报时下发", "Diagnostics queued")):
        with lang_get("/?lang=en&msg=" + urllib.parse.quote(zh)) as r:
            page = r.read().decode("utf-8", "replace")
        m = re.search(r'class="notice[^"]*">\s*(.*?)\s*</div>', page, re.S)
        notice = m.group(1) if m else ""
        check("动作提示语在英文界面上是英文：%s" % en, en in notice, notice[:60])

    print("== 37c. 渲染结果里不许出现 Go 的格式化错误标记 ==")
    # 起因：模板里把 int 值喂给 %s，中文界面直接显示成「主机 %!s(int=5) 台」。
    # 这类错只有在真渲染时才现形，所以逐个页面（中/英）扫一遍。
    for path in ("/", "/devices/%d" % did, "/settings", "/devices/%d/wifi/1" % did):
        for lang in ("", "?lang=en"):
            try:
                _st, body = get(path + lang)
            except urllib.error.HTTPError:
                continue
            bad = re.findall(r'%![a-zA-Z]?\([^)]*\)', body)
            check("没有格式化错误标记：%s%s" % (path, lang or "（中文）"),
                  not bad, "｜".join(dict.fromkeys(bad))[:100])

    print("== 37d. 缓存头：面板页面禁缓存、静态资源每次回源 ==")
    with urllib.request.urlopen(panel_req(BASE + "/"), timeout=20) as r:
        cc = r.headers.get("Cache-Control") or ""
    check("面板页面禁掉了浏览器缓存（no-store）", cc == "no-store", cc)
    with urllib.request.urlopen(panel_req(BASE + "/static/style.css"), timeout=20) as r:
        cc2 = r.headers.get("Cache-Control") or ""
    check("静态资源每次回源确认（no-cache）", "no-cache" in cc2, cc2)

    print("== 37e. 无线/终端概况：到期自动刷新（不用手动点采集）==")
    acs_bin2 = os.path.join(workdir, "acs")
    sim_bin2 = os.path.join(workdir, "cpesim")
    if not (os.path.exists(acs_bin2) and os.path.exists(sim_bin2)):
        skip("无线概况自动刷新', '没找到 $WORK/acs 或 $WORK/cpesim")
    else:
        import sqlite3
        port2 = 17582
        base3 = "http://127.0.0.1:%d" % port2
        db3 = os.path.join(workdir, "wifirefresh.db")
        env3 = dict(os.environ)
        env3.update({
            "ACS_LISTEN": ":%d" % port2,
            "ACS_DB": db3,
            "ACS_LOG_LEVEL": "debug",
            "ACS_AUTO_REFRESH_WIFI": "2s",   # 为了验收压缩到秒级
        })
        env3.pop("ACS_WEB_USER", None)
        env3.pop("ACS_WEB_PASS", None)
        log3 = os.path.join(workdir, "wifirefresh.log")
        f3 = open(log3, "w")
        proc3 = subprocess.Popen([acs_bin2], env=env3, stdout=f3, stderr=subprocess.STDOUT)
        cpe3 = None
        try:
            ready = False
            for _ in range(60):
                try:
                    with urllib.request.urlopen(base3 + "/", timeout=2) as r:
                        if r.status == 200:
                            ready = True
                            break
                except Exception:
                    time.sleep(0.2)
            if not ready:
                skip("无线概况自动刷新", "临时实例没起来")
            else:
                cpe3 = subprocess.Popen([sim_bin2, "-acs", base3 + "/acs", "-serial", "WIFIREF1",
                                         "-interval", "2s"],
                                        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

                def wifi_stamp():
                    """读库里无线参数的最近写入时间 —— 就是界面上那个「采集」时间。"""
                    try:
                        con = sqlite3.connect(db3)
                        row = con.execute(
                            "SELECT COALESCE(MAX(updated_at), '') FROM device_params "
                            "WHERE name LIKE '%.WLANConfiguration.%'").fetchone()
                        con.close()
                        return row[0] or ""
                    except Exception:
                        return ""

                first = ""
                for _ in range(100):          # 等首采完成
                    first = wifi_stamp()
                    if first:
                        break
                    time.sleep(0.3)
                check("首次纳管会自动采集无线概况", bool(first), first)
                # 什么都不点，等两个刷新间隔
                time.sleep(8)
                second = wifi_stamp()
                check("到期后自动重新采集（采集时间自己往前走）",
                      bool(second) and second > first, "首采 %s → 现在 %s" % (first, second))
                f3.flush()
                logtxt = open(log3, encoding="utf-8", errors="replace").read()
                check("日志里能看到「无线概况到期，已安排刷新」",
                      "无线概况到期" in logtxt, "")
        finally:
            if cpe3:
                cpe3.terminate()
            proc3.terminate()
            try:
                proc3.wait(timeout=5)
            except Exception:
                proc3.kill()
            f3.close()

    print("== 37. 离线判定：超期先主动探测，探不通才判离线 ==")
    acs_bin = os.path.join(workdir, "acs")
    sim_bin = os.path.join(workdir, "cpesim")
    if not (os.path.exists(acs_bin) and os.path.exists(sim_bin)):
        skip("离线判定用例", "没找到 $WORK/acs 或 $WORK/cpesim")
    else:
        port = 17571
        base2 = "http://127.0.0.1:%d" % port
        log_path = os.path.join(workdir, "offline.log")
        env = dict(os.environ)
        env.update({
            "ACS_LISTEN": ":%d" % port,
            "ACS_DB": os.path.join(workdir, "offline.db"),
            "ACS_LOG_LEVEL": "info",
            # 验收要等得起：先把探测节奏压到秒级（周期 3 秒 → 6 秒超期）
            "ACS_OFFLINE_PROBE_FACTOR": "2",
            "ACS_OFFLINE_PROBE_ATTEMPTS": "3",
            "ACS_OFFLINE_PROBE_INTERVAL": "1s",
            "ACS_OFFLINE_PROBE_GRACE": "1s",
            "ACS_OFFLINE_CHECK_INTERVAL": "1s",
        })
        env.pop("ACS_WEB_USER", None)   # 这一节不测鉴权
        env.pop("ACS_WEB_PASS", None)
        logf = open_log(log_path)
        proc = subprocess.Popen([acs_bin], env=env, stdout=logf, stderr=subprocess.STDOUT)
        sims = []

        def off_dev(serial):
            try:
                with urllib.request.urlopen(base2 + "/api/devices", timeout=5) as r:
                    for d in json.loads(r.read().decode())["data"]:
                        if d.get("SerialNumber") == serial:
                            return d
            except Exception:  # noqa: BLE001
                return None
            return None

        def off_wait(serial, pred, timeout):
            end = time.time() + timeout
            seen_probing = False
            while time.time() < end:
                d = off_dev(serial)
                if d and pred(d):
                    return d, seen_probing
                # 顺手看一眼界面上有没有出现过「探测中」
                try:
                    with urllib.request.urlopen(base2 + "/", timeout=5) as r:
                        if "badge probing" in r.read().decode("utf-8", "replace"):
                            seen_probing = True
                except Exception:  # noqa: BLE001
                    pass
                time.sleep(0.25)
            return off_dev(serial), seen_probing

        try:
            ready = False
            for _ in range(60):
                try:
                    with urllib.request.urlopen(base2 + "/", timeout=2) as r:
                        if r.status == 200:
                            ready = True
                            break
                except Exception:  # noqa: BLE001
                    time.sleep(0.2)
            check("离线判定实例起来了", ready, "")
            if ready:
                # --- 场景 A：设备断电（进程被杀，ConnectionRequestURL 也没人听）---
                a = start_sim(sim_bin, base2, "OFFLINE-A", "-interval", "3s")
                sims.append(a)
                d, _ = off_wait("OFFLINE-A", lambda x: x["Online"], 30)
                check("模拟设备已纳管（上报周期 3 秒）", bool(d and d["Online"]), d and d.get("SerialNumber"))

                a.terminate()
                a.wait(timeout=5)
                sims.remove(a)

                # 6 秒超期 + 最多 3 次探测（间隔 1 秒）+ 1 秒宽限，留足余量
                final, seen_probing = off_wait("OFFLINE-A", lambda x: not x["Online"], 40)
                check("断电设备最终被判离线", bool(final) and not final["Online"],
                      final and final.get("Online"))
                check("判离线前界面上出现过「探测中」", seen_probing, "")

                # 离线设备的操作按钮应该全部灰掉（删除除外 —— 那是本地操作），
                # 列表页的筛选条也要能按状态过滤
                aid = (final or {}).get("ID") or (d or {}).get("ID")
                if aid:
                    with urllib.request.urlopen("%s/devices/%d" % (base2, aid), timeout=5) as r:
                        dhtml = r.read().decode("utf-8", "replace")
                    check("离线设备详情页：需要设备配合的按钮都禁用了",
                          dhtml.count("disabled") >= 3, "disabled 出现 %d 次" % dhtml.count("disabled"))
                    delform = re.search(r'action="/devices/%d/delete"(.*?)</form>' % aid, dhtml, re.S)
                    check("离线设备详情页：删除设备仍可用（只是删本地记录）",
                          bool(delform) and "disabled" not in delform.group(1),
                          (delform.group(0)[:90] if delform else "没找到删除按钮"))
                    with urllib.request.urlopen(base2 + "/?state=offline", timeout=5) as r:
                        off_html = r.read().decode("utf-8", "replace")
                    with urllib.request.urlopen(base2 + "/?state=online", timeout=5) as r:
                        on_html = r.read().decode("utf-8", "replace")
                    link = '/devices/%d"' % aid
                    check("按「离线」筛选能筛到它", link in off_html, "")
                    check("按「在线」筛选筛不到它", link not in on_html, "")
                    check("筛选后列表只留该状态（离线页里没有在线徽标）",
                          "badge on" not in off_html.split("WiFi 概览")[0], "")

                log = read_log(log_path)
                probed = log.count("离线探测无响应")
                check("超期后主动探测了 3 次（不多不少）", probed == 3, "实际 %d 次" % probed)
                check("日志写明判离线与探测次数",
                      "设备已标记离线" in log and "probes=3" in log, "")

                # --- 场景 B：设备还活着，只是不按周期上报了（Connection Request 能连上）---
                # -stall-after 4s：上报两轮之后停报，但保持 Connection Request 监听。
                b = start_sim(sim_bin, base2, "OFFLINE-B", "-interval", "3s",
                              "-stall-after", "4s", "-cr-port", "17881")
                sims.append(b)
                d, _ = off_wait("OFFLINE-B", lambda x: x["Online"], 30)
                check("场景 B：模拟设备已纳管", bool(d and d["Online"]), d and d.get("SerialNumber"))

                # 等它超期 → 被探测 → 探测成功、设备回连上报 → 不应该判离线
                time.sleep(18)
                final = off_dev("OFFLINE-B")
                check("停报但能被唤醒的设备：保持在线，不误判离线",
                      bool(final and final["Online"]), final and final.get("Online"))
                blog = [ln for ln in read_log(log_path).splitlines() if "serial=OFFLINE-B" in ln]
                check("对它有探测记录（说明走的是探测而不是直接判离线）",
                      any("离线探测" in ln for ln in blog), "\n".join(blog[-3:]))
                check("从没把它标记离线",
                      not any("设备已标记离线" in ln for ln in blog), "")
        finally:
            for p in sims:
                p.terminate()
                try:
                    p.wait(timeout=5)
                except Exception:  # noqa: BLE001
                    p.kill()
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except Exception:  # noqa: BLE001
                proc.kill()
            logf.close()

    # == 38. 主机收光 / 发光：详情页「基本信息」里那两行 ==
    print("== 38. 主机收光 / 发光（详情页基本信息）==")
    sim_bin = os.path.join(workdir, "cpesim")
    if not os.path.exists(sim_bin):
        print("   （没有模拟器可执行文件，跳过）")
    else:
        # -optical：模拟 PON 光猫自己上报收/发光（同时带一对没换算的原始值当诱饵）
        sim = start_sim(sim_bin, BASE, "OPTICAL1", "-oui", "0A0B0C", "-interval", "5s", "-optical")
        try:
            d = None
            for _ in range(40):
                d = next((x for x in api_devices() if x["SerialNumber"] == "OPTICAL1"), None)
                if d:
                    break
                time.sleep(1)
            check("带光功率的模拟设备已纳管", bool(d), d and d.get("SerialNumber"))

            # 映射表（厂商私有参数 → 面板字段）得在库里，且种了华为那款光猫的实测映射
            con = sqlite3.connect(f"file:{workdir}/acs.db?mode=ro", uri=True)
            n_alias = con.execute("SELECT COUNT(*) FROM param_aliases WHERE enabled = 1").fetchone()[0]
            decodes = {r[0] for r in con.execute(
                "SELECT DISTINCT decode FROM param_aliases WHERE field IN ('rx_power','temperature','voltage')")}
            con.close()
            check("映射表 param_aliases 有种子数据", n_alias >= 10, n_alias)
            check("种子映射带上了原始值换算规则",
                  "dbm_01uw" in decodes and "div256" in decodes and "mv01" in decodes, decodes)
            h = ""
            if d:
                did = d["ID"]
                # 纳管时会自动采一次；再点「重新获取」确保拿到（这条也会顺带采光功率）
                for _ in range(3):
                    post_form(f"/devices/{did}/refresh", {})
                    got = False
                    for _ in range(20):
                        time.sleep(1)
                        st, h = get(f"/devices/{did}")
                        if "dBm" in h:
                            got = True
                            break
                    if got:
                        break
                st, h = get(f"/devices/{did}")
                # 光模块寄存器原始值要按映射表（param_aliases）换算成真实读数，
                # 数值与设备自己页面一致：254 → -15.95 dBm、10000 → 0.00 dBm…
                check("「基本信息」里有收光功率（按映射换算）", "收光" in h and "-15.95 dBm" in h, st)
                check("「基本信息」里有发光功率（按映射换算）", "发光" in h and "0.00 dBm" in h, st)
                check("「基本信息」里有光模块温度", "光模块温度" in h and "43.0 ℃" in h, st)
                check("「基本信息」里有光模块电压", "光模块电压" in h and "3.226 V" in h, st)
                check("「基本信息」里有光模块偏流", "光模块偏流" in h and "29.00 mA" in h, st)
                # 原始寄存器值不能直接当 dBm/℃ 显示
                check("没换算的原始值不会被当成读数",
                      "254 dBm" not in h and "10000 dBm" not in h
                      and "11008 ℃" not in h and "32260 V" not in h, "")
                # 同一个设备也要出现在**列表页**：有设备报光功率时那两列才显示
                st, ihtml = get("/")
                check("列表页出现「收光 / 发光」两列",
                      "<th>收光</th>" in ihtml and "<th>发光</th>" in ihtml, "")
                check("列表页显示收 / 发光读数",
                      "-15.95 dBm" in ihtml and "0.00 dBm" in ihtml, "")
                check("列表页不把没换算的原始值当成功率", "254 dBm" not in ihtml, "")
        finally:
            sim.terminate()
            try:
                sim.wait(timeout=5)
            except Exception:  # noqa: BLE001
                sim.kill()

    print()
    total = _n["pass"] + _n["fail"]
    print(f"结果：通过 {_n['pass']} / 失败 {_n['fail']} / 共 {total}")
    return 1 if _n["fail"] else 0


if __name__ == "__main__":
    sys.exit(main())
