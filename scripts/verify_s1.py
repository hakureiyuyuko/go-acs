#!/usr/bin/env python3
"""S1 验收：对真的 ACS 发真的 HTTP/CWMP 报文，逐条断言。

用法: verify_s1.py http://127.0.0.1:PORT
只依赖标准库。
"""
import base64
import json
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
        spv = [t for t in full["tasks"] if t["Kind"] == "SetParameterValues"]
        check("已入队 SetParameterValues", len(spv) == 1, [(t["Kind"], t["Status"]) for t in full["tasks"][:3]])
        if spv:
            check("只包含改动过的那 1 个参数",
                  spv[0]["Payload"].count("WLANConfiguration.1.SSID") == 1,
                  spv[0]["Payload"][:140])

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
            payload = spv[0]["Payload"]
            check("没动过的参数没有被一起写入",
                  "TotalAssociations" not in payload and "BeaconType" not in payload)

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

    print("== 17. 认证（默认实例未启用，只验证未认证时可通）==")
    st, _, _, _ = post(envelope("urn:dslforum-org:cwmp-1-0", "u3", "<cwmp:GetRPCMethods/>"))
    check("未启用认证时无凭证也能通", st == 200, st)

    print()
    total = _n["pass"] + _n["fail"]
    print(f"结果：通过 {_n['pass']} / 失败 {_n['fail']} / 共 {total}")
    return 1 if _n["fail"] else 0


if __name__ == "__main__":
    sys.exit(main())
