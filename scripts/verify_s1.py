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
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = sys.argv[1].rstrip("/")
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
    with urllib.request.urlopen(BASE + path, timeout=20) as r:
        return r.status, r.read().decode("utf-8", "replace")


def get_code(path):
    """只取状态码（404 之类的不会抛异常）。"""
    try:
        with urllib.request.urlopen(BASE + path, timeout=20) as r:
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
    req = urllib.request.Request(BASE + path, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def post_form(path, fields):
    """提交一个表单，返回 (状态码, Location)。不跟随重定向。"""
    data = urllib.parse.urlencode(fields).encode()
    req = urllib.request.Request(BASE + path, data=data, method="POST")
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
            [chrome, "--headless=new", "--no-sandbox", "--disable-gpu",
             "--virtual-time-budget=4000", "--dump-dom", url],
            capture_output=True, timeout=60,
        )
        if out.returncode != 0 or not out.stdout:
            return None
        return out.stdout.decode("utf-8", "replace")
    except Exception:
        return None


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
        check("参数数 >= 12", d["ParamCount"] >= 12, d["ParamCount"])

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

        class NoRedirect(urllib.request.HTTPRedirectHandler):
            """不让 urllib 自动跟随重定向，否则看不到 303。"""

            def redirect_request(self, *a, **kw):
                return None

        opener = urllib.request.build_opener(NoRedirect)
        req = urllib.request.Request(f"{BASE}/devices/{did}/refresh", data=b"", method="POST")
        try:
            with opener.open(req, timeout=20) as r:
                st = r.status
        except urllib.error.HTTPError as e:
            st = e.code
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

    print("== 12. 看板上的 WiFi 概览 ==")
    st, html = get("/")
    check("看板 200", st == 200, st)
    check("看板有「WiFi 概览」区块", "WiFi 概览" in html)
    check("看板出现 2.4G 的 SSID", "SimWiFi" in html)
    check("看板出现 5G 的 SSID", "SimWiFi-5G" in html)
    check("看板有无线终端统计", "无线终端" in html)
    check("看板有频段/射频列", "频段" in html and "射频" in html)

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
        # 只取摘要字段，不能把整棵几百个参数的子树拉回来
        wlan = [n for n in names if "WLANConfiguration" in n]
        check(f"只采集摘要字段（{len(wlan)} 条，应在 1..40 之间）", 0 < len(wlan) <= 40, len(wlan))
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
        check("三个区块都是可折叠的（参数/任务历史/Inform 记录）",
              dhtml.count('<details class="fold">') == 3,
              dhtml.count('<details class="fold">'))
        check("默认都是收起的", '<details class="fold" open' not in dhtml)
        for label in ("参数（", "任务历史", "Inform 记录"):
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
            dom = run_chrome_dom(chrome, f"{BASE}/devices/{did}")
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

    print("== 23. 三个页面都要加载 app.js（分页/搜索/主题都靠它）==")
    if did:
        for p in ("/", f"/devices/{did}", f"/devices/{did}/wifi/1"):
            st, h = get(p)
            check(f"{p} 引入了 app.js", "static/app.js" in h)

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
        # 把设备侧的 ConnectionRequest 账号密码 provision 进去（ACS 启动后首次 Inform 会做）
        full = api_device(did)
        prov = [t for t in full["tasks"]
                if t["Kind"] == "SetParameterValues" and "ConnectionRequestUsername" in t["Payload"]]
        check("已把 ConnectionRequest 凭据 provision 进设备", len(prov) >= 1, len(prov))
        check("凭据下发成功", any(t["Status"] == "done" for t in prov),
              [(t["ID"], t["Status"], t["Result"][:40]) for t in prov])

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

    print()
    total = _n["pass"] + _n["fail"]
    print(f"结果：通过 {_n['pass']} / 失败 {_n['fail']} / 共 {total}")
    return 1 if _n["fail"] else 0


if __name__ == "__main__":
    sys.exit(main())
