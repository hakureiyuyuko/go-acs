package cwmp

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 一份贴近真实设备的 Inform（前缀用的是 soap-env，跟很多国产光猫一致）。
const sampleInform = `<?xml version="1.0" encoding="UTF-8"?>
<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xmlns:cwmp="urn:dslforum-org:cwmp-1-0">
<soap-env:Header><cwmp:ID soap-env:mustUnderstand="1">1234</cwmp:ID></soap-env:Header>
<soap-env:Body>
<cwmp:Inform>
<DeviceId>
<Manufacturer>SimVendor</Manufacturer>
<OUI>001122</OUI>
<ProductClass>SimRouter</ProductClass>
<SerialNumber>ACSIM0000001</SerialNumber>
</DeviceId>
<Event soap-enc:arrayType="cwmp:EventStruct[2]">
<EventStruct><EventCode>0 BOOTSTRAP</EventCode><CommandKey></CommandKey></EventStruct>
<EventStruct><EventCode>1 BOOT</EventCode><CommandKey></CommandKey></EventStruct>
</Event>
<MaxEnvelopes>1</MaxEnvelopes>
<CurrentTime>2026-09-28T06:48:36.000Z</CurrentTime>
<RetryCount>0</RetryCount>
<ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[2]">
<ParameterValueStruct><Name>InternetGatewayDevice.DeviceInfo.SoftwareVersion</Name><Value xsi:type="xsd:string">1.0.0-sim</Value></ParameterValueStruct>
<ParameterValueStruct><Name>InternetGatewayDevice.DeviceInfo.UpTime</Name><Value xsi:type="xsd:unsignedInt">3600</Value></ParameterValueStruct>
</ParameterList>
<SomeVendorExtension><Whatever>ignore me</Whatever></SomeVendorExtension>
</cwmp:Inform>
</soap-env:Body>
</soap-env:Envelope>`

func TestParseInform(t *testing.T) {
	root, err := ParseXML(strings.NewReader(sampleInform))
	if err != nil {
		t.Fatalf("解析 XML 失败: %v", err)
	}
	env, err := ParseEnvelope(root)
	if err != nil {
		t.Fatalf("解析信封失败: %v", err)
	}
	if env.ID != "1234" {
		t.Errorf("cwmp:ID = %q，期望 1234", env.ID)
	}
	if env.CWMPNS != "urn:dslforum-org:cwmp-1-0" {
		t.Errorf("CWMP 命名空间 = %q", env.CWMPNS)
	}
	if env.Method == nil || env.Method.Local != "Inform" {
		t.Fatalf("方法名不是 Inform")
	}

	inf, err := ParseInform(env.Method)
	if err != nil {
		t.Fatalf("解析 Inform 失败: %v", err)
	}
	if inf.DeviceID.SerialNumber != "ACSIM0000001" {
		t.Errorf("序列号 = %q", inf.DeviceID.SerialNumber)
	}
	if inf.DeviceID.OUI != "001122" {
		t.Errorf("OUI = %q（应保留原样）", inf.DeviceID.OUI)
	}
	if !inf.HasEvent("0 BOOTSTRAP") || !inf.HasEvent("1 BOOT") {
		t.Errorf("事件列表 = %v", inf.EventCodes())
	}
	if len(inf.Params) != 2 {
		t.Fatalf("参数个数 = %d，期望 2", len(inf.Params))
	}
	// 类型归一化成 XML Schema 的规范写法（写成 unsignedInt，不是 unsignedint）
	if inf.Params[1].Type != "unsignedInt" {
		t.Errorf("参数类型 = %q，期望 unsignedInt", inf.Params[1].Type)
	}
	if inf.MaxEnvelopes != 1 {
		t.Errorf("MaxEnvelopes = %d", inf.MaxEnvelopes)
	}
}

// 未知的厂商扩展节点不能被当成错误（R-FR3-1）。
func TestParseInformToleratesUnknownElements(t *testing.T) {
	if !strings.Contains(sampleInform, "SomeVendorExtension") {
		t.Fatal("测试样本里应该有厂商扩展节点")
	}
	root, _ := ParseXML(strings.NewReader(sampleInform))
	env, _ := ParseEnvelope(root)
	if _, err := ParseInform(env.Method); err != nil {
		t.Fatalf("有厂商扩展节点时不该报错: %v", err)
	}
}

// 不同前缀写法都要能识别。
func TestPrefixIndependence(t *testing.T) {
	doc := `<?xml version="1.0"?>
<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/"
  xmlns:CWMP="urn:dslforum-org:cwmp-1-2">
<SOAP-ENV:Header><CWMP:ID>abc</CWMP:ID></SOAP-ENV:Header>
<SOAP-ENV:Body><CWMP:Inform>
  <DeviceId><SerialNumber>S1</SerialNumber></DeviceId>
</CWMP:Inform></SOAP-ENV:Body></SOAP-ENV:Envelope>`
	root, err := ParseXML(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	env, err := ParseEnvelope(root)
	if err != nil {
		t.Fatalf("解析信封失败: %v", err)
	}
	if env.CWMPNS != "urn:dslforum-org:cwmp-1-2" {
		t.Errorf("命名空间 = %q，期望 cwmp-1-2", env.CWMPNS)
	}
	if env.Method.Local != "Inform" {
		t.Errorf("方法 = %q", env.Method.Local)
	}
}

// 命名空间探测不到时要优雅兜底，不能报错。
func TestParseEnvelopeFallsBackToDefaultVersion(t *testing.T) {
	doc := `<?xml version="1.0"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">
<soap:Body><Whatever/></soap:Body></soap:Envelope>`
	root, _ := ParseXML(strings.NewReader(doc))
	env, err := ParseEnvelope(root)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if env.CWMPNS != cwmpNSBase+DefaultCWMPVersion {
		t.Errorf("命名空间 = %q，期望兜底 %q", env.CWMPNS, cwmpNSBase+DefaultCWMPVersion)
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	doc := NewEnvelope("urn:dslforum-org:cwmp-1-1", "id-9", GetParameterValuesBody([]string{"Device.DeviceInfo.", "Device.WiFi.SSID.1.SSID"}))
	root, err := ParseXML(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("解析自己生成的报文失败: %v", err)
	}
	env, err := ParseEnvelope(root)
	if err != nil {
		t.Fatalf("解析信封失败: %v", err)
	}
	if env.ID != "id-9" {
		t.Errorf("ID = %q", env.ID)
	}
	if env.Method.Local != "GetParameterValues" {
		t.Fatalf("方法 = %q", env.Method.Local)
	}
	names := env.Method.Child("ParameterNames")
	var got []string
	for _, k := range names.Kids {
		got = append(got, k.Trimmed())
	}
	want := []string{"Device.DeviceInfo.", "Device.WiFi.SSID.1.SSID"}
	if len(got) != len(want) {
		t.Fatalf("参数名 = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("参数名[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
	if !strings.Contains(doc, `soap-env:mustUnderstand="1"`) {
		t.Error("信封里 ID 应带 mustUnderstand")
	}
}

func TestSetParameterValuesKeyEcho(t *testing.T) {
	body := SetParameterValuesBody([]ParamValue{{Name: "X.Y", Value: "1", Type: "boolean"}}, "my-key")
	root, _ := ParseXML(strings.NewReader(NewEnvelope("", "1", body)))
	env, _ := ParseEnvelope(root)
	if env.Method.ChildText("ParameterKey") != "my-key" {
		t.Errorf("ParameterKey 没有正确写入")
	}
	vals := ParseParamValues(env.Method.Child("ParameterList"))
	if len(vals) != 1 || vals[0].Value != "1" || vals[0].Type != "boolean" {
		t.Errorf("参数 = %+v", vals)
	}
}

func TestParseFault(t *testing.T) {
	doc := `<?xml version="1.0"?>
<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/" xmlns:cwmp="urn:dslforum-org:cwmp-1-0">
<soap-env:Body><soap-env:Fault>
<faultcode>Client</faultcode><faultstring>CWMP fault</faultstring>
<detail><cwmp:Fault>
<FaultCode>9003</FaultCode><FaultString>Invalid arguments</FaultString>
<SetParameterValuesFault><ParameterName>Device.X</ParameterName><FaultCode>9007</FaultCode><FaultString>Invalid parameter name</FaultString></SetParameterValuesFault>
</cwmp:Fault></detail>
</soap-env:Fault></soap-env:Body></soap-env:Envelope>`
	root, _ := ParseXML(strings.NewReader(doc))
	env, err := ParseEnvelope(root)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if env.Method.Local != "Fault" {
		t.Fatalf("方法 = %q", env.Method.Local)
	}
	f := ParseFault(env.Method)
	if f == nil || f.Code != 9003 {
		t.Fatalf("Fault = %+v", f)
	}
	if len(f.SetParamFaults) != 1 || f.SetParamFaults[0].Code != 9007 {
		t.Errorf("逐条错误 = %+v", f.SetParamFaults)
	}
}

func TestBasicInfoNames(t *testing.T) {
	got := basicInfoNames("Device.")
	if len(got) != len(basicInfoSuffixes) {
		t.Errorf("单个根时应有 %d 个名字，实际 %d", len(basicInfoSuffixes), len(got))
	}
	for _, n := range got {
		if !strings.HasPrefix(n, "Device.") {
			t.Errorf("不该混入别的根：%q", n)
		}
	}

	// 根未知时两种命名都探测，而不是猜一个
	got = basicInfoNames("")
	if len(got) != 2*len(basicInfoSuffixes) {
		t.Errorf("根未知时应探测两种命名，实际 %d 个", len(got))
	}
	var has098, has181 bool
	for _, n := range got {
		switch {
		case strings.HasPrefix(n, "InternetGatewayDevice."):
			has098 = true
		case strings.HasPrefix(n, "Device."):
			has181 = true
		}
	}
	if !has098 || !has181 {
		t.Error("根未知时应同时包含 TR-098 与 TR-181 的名字")
	}

	// 必须是显式参数名，不能以 "." 结尾（子树路径）——
	// 交叉验证发现有的实现不支持子树查询
	for _, n := range basicInfoNames("") {
		if strings.HasSuffix(n, ".") {
			t.Errorf("不应下发子树路径：%q", n)
		}
	}
}

func TestDetectRoot(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{[]string{"Device.DeviceInfo.SerialNumber"}, "Device."},
		{[]string{"InternetGatewayDevice.DeviceInfo.SerialNumber"}, "InternetGatewayDevice."},
		{[]string{}, ""},
		{[]string{"DeviceID.SerialNumber"}, ""},
	}
	for _, c := range cases {
		var ps []ParamValue
		for _, n := range c.names {
			ps = append(ps, ParamValue{Name: n})
		}
		if got := detectRoot(ps); got != c.want {
			t.Errorf("detectRoot(%v) = %q，期望 %q", c.names, got, c.want)
		}
	}
}

func TestDeviceFieldsFromParams(t *testing.T) {
	params := []ParamValue{
		{Name: "InternetGatewayDevice.DeviceInfo.Manufacturer", Value: "ACME"},
		{Name: "InternetGatewayDevice.DeviceInfo.ModelName", Value: "M1"},
		{Name: "InternetGatewayDevice.DeviceInfo.SoftwareVersion", Value: "9.9"},
		{Name: "InternetGatewayDevice.ManagementServer.PeriodicInformInterval", Value: "300"},
		{Name: "InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANIPConnection.1.ExternalIPAddress", Value: "1.2.3.4"},
	}
	d := deviceFieldsFromParams(params)
	if d.Manufacturer != "ACME" || d.ModelName != "M1" || d.SoftwareVersion != "9.9" {
		t.Errorf("设备字段 = %+v", d)
	}
	if d.PeriodicInterval != 300 {
		t.Errorf("上报周期 = %d", d.PeriodicInterval)
	}
	if d.ExternalIP != "1.2.3.4" {
		t.Errorf("外网 IP = %q", d.ExternalIP)
	}
}

func TestCookieValue(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{`session=abc123`, "abc123"},
		{`foo=1; session=abc123; bar=2`, "abc123"},
		// 有些设备用逗号分隔，甚至带引号
		{`foo=1, session="abc123", bar=2`, "abc123"},
		{`session=abc123`, "abc123"},
		{`other=xyz`, ""},
		{``, ""},
	}
	for _, c := range cases {
		r, _ := http.NewRequest(http.MethodPost, "/acs", nil)
		if c.header != "" {
			r.Header.Set("Cookie", c.header)
		}
		if got := cookieValue(r, "session"); got != c.want {
			t.Errorf("cookieValue(%q) = %q，期望 %q", c.header, got, c.want)
		}
	}
}

func TestFaultBodyHasCwmpStructure(t *testing.T) {
	b := FaultBody(FaultMethodNotSupported, "")
	for _, want := range []string{"<soap-env:Fault>", "<cwmp:Fault>", "<FaultCode>8000</FaultCode>", "Method not supported"} {
		if !strings.Contains(b, want) {
			t.Errorf("Fault 报文缺少 %q：%s", want, b)
		}
	}
}

// 会话加锁：两个并发请求应被串行化，第二个最多等到超时。
func TestSessionLockSerializes(t *testing.T) {
	s := &Session{ID: "x", lock: make(chan struct{}, 1)}
	if !s.tryLock(time.Second) {
		t.Fatal("第一次加锁应该成功")
	}
	if s.tryLock(50 * time.Millisecond) {
		t.Fatal("第二次加锁不该成功（应被占用）")
	}
	s.unlock()
	if !s.tryLock(50 * time.Millisecond) {
		t.Fatal("解锁后应该能加锁")
	}
	s.unlock()
}

func TestReadBodyRejectsOversize(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "/acs", strings.NewReader(strings.Repeat("a", 100)))
	if _, err := readBody(r, 10); err == nil {
		t.Error("超限的请求体应该报错")
	}
}

// 空的 / 只带声明的 XML 不能把服务搞崩。
func TestParseGarbage(t *testing.T) {
	if _, err := ParseXML(strings.NewReader("not xml at all")); err == nil {
		t.Error("非 XML 应该解析失败")
	}
	if _, err := ParseXML(strings.NewReader("")); err == nil {
		t.Error("空文档应该解析失败")
	}
	if _, err := ParseXML(strings.NewReader("<a><b></a>")); err == nil {
		t.Error("闭合标签不匹配应该解析失败")
	}
}

func TestNewEnvelopeEscapesValues(t *testing.T) {
	doc := NewEnvelope("", "1", GetParameterValuesBody([]string{`a<b>&"c`}))
	if !strings.Contains(doc, "a&lt;b&gt;&amp;") {
		t.Errorf("值没有被正确转义: %s", doc)
	}
	// 生成的报文必须是合法 XML
	dec := xml.NewDecoder(strings.NewReader(doc))
	for {
		if _, err := dec.Token(); err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("生成的报文不是合法 XML: %v", err)
		}
	}
}
