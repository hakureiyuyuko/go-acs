#!/usr/bin/env python3
"""S1 验收：对真的 ACS 发真的 HTTP/CWMP 报文，逐条断言。

用法: verify_s1.py http://127.0.0.1:PORT
只依赖标准库。
"""
import base64
import json
import sys
import urllib.error
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
        check("UpTime 类型正确(unsignedint)",
              any(n.endswith("UpTime") and p["ValueType"] == "unsignedint" for n, p in zip(names, full["params"])))
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
        gpv = [t for t in full["tasks"] if t["Kind"] == "GetParameterValues"]
        check("周期上报没有重复入队取信息任务", len(gpv) == 1, len(gpv))

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

    print("== 12. 认证（默认实例未启用，只验证未认证时可通）==")
    st, _, _, _ = post(envelope("urn:dslforum-org:cwmp-1-0", "u3", "<cwmp:GetRPCMethods/>"))
    check("未启用认证时无凭证也能通", st == 200, st)

    print()
    total = _n["pass"] + _n["fail"]
    print(f"结果：通过 {_n['pass']} / 失败 {_n['fail']} / 共 {total}")
    return 1 if _n["fail"] else 0


if __name__ == "__main__":
    sys.exit(main())
