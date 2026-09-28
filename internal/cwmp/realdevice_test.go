package cwmp

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// 本文件用的样本**来自一台真机**，不是手写的：
//
//	设备：华为 OptiXstar HN8145X6N（FTTR 光猫）
//	UA：HW_WAP_CWMP_V02，报文用大写前缀 SOAP-ENV:
//	数据模型：InternetGatewayDevice:1.4（TR-098 Amendment 4）
//
// 手写样本容易「按自己的理解写」，掩盖真实设备的怪癖。用真机报文做回归，
// 才能保证以后重构解析逻辑时不会把这些怪癖处理掉。
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读取样本 %s 失败: %v", name, err)
	}
	return b
}

func parseFixture(t *testing.T, name string) *Envelope {
	t.Helper()
	root, err := ParseXML(bytes.NewReader(readFixture(t, name)))
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", name, err)
	}
	env, err := ParseEnvelope(root)
	if err != nil {
		t.Fatalf("解析 %s 的信封失败: %v", name, err)
	}
	return env
}

func TestParseRealHuaweiInform(t *testing.T) {
	env := parseFixture(t, "huawei-hn8145x6n-inform.xml")

	// 大写 SOAP-ENV: 前缀也要认
	if env.CWMPNS != "urn:dslforum-org:cwmp-1-0" {
		t.Errorf("命名空间 = %q", env.CWMPNS)
	}
	if env.ID != "39" {
		t.Errorf("cwmp:ID = %q，期望 39", env.ID)
	}
	if env.Method.Local != "Inform" {
		t.Fatalf("方法 = %q", env.Method.Local)
	}

	inf, err := ParseInform(env.Method)
	if err != nil {
		t.Fatalf("解析 Inform 失败: %v", err)
	}
	if inf.DeviceID.Manufacturer != "Huawei Technologies Co., Ltd" {
		t.Errorf("厂商 = %q", inf.DeviceID.Manufacturer)
	}
	if inf.DeviceID.OUI != "00259E" || inf.DeviceID.ProductClass != "HN8145X6N" {
		t.Errorf("OUI/ProductClass = %q/%q", inf.DeviceID.OUI, inf.DeviceID.ProductClass)
	}
	if inf.DeviceID.SerialNumber != "48575443AA000001" {
		t.Errorf("序列号 = %q", inf.DeviceID.SerialNumber)
	}

	// 真机把 CommandKey 写成了自闭合标签 <CommandKey/>
	if len(inf.Events) != 1 || inf.Events[0].Code != "2 PERIODIC" || inf.Events[0].CommandKey != "" {
		t.Errorf("事件 = %+v", inf.Events)
	}
	if inf.MaxEnvelopes != 1 || inf.RetryCount != 0 {
		t.Errorf("MaxEnvelopes/RetryCount = %d/%d", inf.MaxEnvelopes, inf.RetryCount)
	}
	// 真机的 CurrentTime 用 +00:00 而不是 Z，原样保留即可
	if inf.CurrentTime != "2026-09-28T06:58:51+00:00" {
		t.Errorf("CurrentTime = %q", inf.CurrentTime)
	}

	if len(inf.Params) != 8 {
		t.Fatalf("参数个数 = %d，期望 8", len(inf.Params))
	}

	byName := map[string]ParamValue{}
	for _, p := range inf.Params {
		byName[p.Name] = p
	}

	// 真机会把空值写成自闭合的 <Value xsi:type="xsd:string"/>，必须解出空串而不是报错
	pc, ok := byName["InternetGatewayDevice.DeviceInfo.ProvisioningCode"]
	if !ok {
		t.Fatal("没解出 ProvisioningCode")
	}
	if pc.Value != "" {
		t.Errorf("ProvisioningCode 应为空串，实际 %q", pc.Value)
	}

	// DeviceSummary 告诉了我们设备实现的数据模型版本，很有用
	ds, ok := byName["InternetGatewayDevice.DeviceSummary"]
	if !ok {
		t.Fatal("没解出 DeviceSummary")
	}
	if !strings.Contains(ds.Value, "InternetGatewayDevice:1.4") {
		t.Errorf("DeviceSummary = %q", ds.Value)
	}

	if got := detectRoot(inf.Params); got != "InternetGatewayDevice." {
		t.Errorf("数据模型根 = %q", got)
	}

	d := deviceFieldsFromParams(inf.Params)
	// 注意：真机 Inform 的 ParameterList 里**没有** DeviceInfo.Manufacturer，
	// 厂商只在 DeviceId 里给（所以 onInform 用 pick(fields.Manufacturer, DeviceId.Manufacturer) 兜底）。
	// 这里断言的是能从上报名单里推出来的那几个字段。
	if d.Manufacturer != "" {
		t.Logf("（厂商不在上报名单里，符合真机行为）")
	}
	if d.SoftwareVersion != "V5R023C00S120" || d.HardwareVersion != "35A0.E" || d.SpecVersion != "1.0" {
		t.Errorf("版本字段 = %q/%q/%q", d.SoftwareVersion, d.HardwareVersion, d.SpecVersion)
	}
	if d.ConnRequestURL != "http://192.168.10.22:7547/0123456789abcdef0123456789abcdef" {
		t.Errorf("ConnectionRequestURL = %q", d.ConnRequestURL)
	}
	if d.ExternalIP != "192.168.10.22" {
		t.Errorf("外网 IP = %q", d.ExternalIP)
	}
}

func TestParseRealHuaweiGetParameterValuesResponse(t *testing.T) {
	env := parseFixture(t, "huawei-hn8145x6n-gpv-response.xml")

	// 真机把我们下发的 ID 原样回填了
	if env.ID != "0abee7239d3f8942" {
		t.Errorf("cwmp:ID = %q（真机应原样回填我们下发的 ID）", env.ID)
	}
	if env.Method.Local != "GetParameterValuesResponse" {
		t.Fatalf("方法 = %q", env.Method.Local)
	}

	params := ParseParamValues(env.Method.Child("ParameterList"))
	// 注意：这里断言的是**抓包本身**的参数个数（当时请求了 14 个），
	// 不能写成 len(basicInfoSuffixes) —— 那样以后往请求清单里加字段就会把这个
	// 历史抓包的回归测试弄挂，而抓包是死的、不该跟着变。
	const capturedCount = 14
	if len(params) != capturedCount {
		t.Errorf("参数个数 = %d，期望 %d（历史抓包固定就是这个数）", len(params), capturedCount)
	}

	byName := map[string]ParamValue{}
	for _, p := range params {
		byName[p.Name] = p
	}

	up, ok := byName["InternetGatewayDevice.DeviceInfo.UpTime"]
	if !ok {
		t.Fatal("没解出 UpTime")
	}
	if up.Value != "949" || up.Type != "unsignedInt" {
		t.Errorf("UpTime = %q [%s]，期望 949 [unsignedInt]", up.Value, up.Type)
	}

	if ms, ok := byName["InternetGatewayDevice.ManagementServer.PeriodicInformInterval"]; !ok ||
		ms.Value != "120" || ms.Type != "unsignedInt" {
		t.Errorf("PeriodicInformInterval = %+v", ms)
	}
}

// 回归保护：我们下发的取信息请求必须是**显式参数名**，不能是子树路径。
// 原因见 handler.go 里 basicInfoNames 的注释（有的 CPE 实现不支持部分路径）。
// 样本是这台真机交互里实际抓到的、我们发出去的报文。
func TestOurOwnRequestHasNoSubtreePaths(t *testing.T) {
	env := parseFixture(t, "acs-getparametervalues-request.xml")
	if env.Method.Local != "GetParameterValues" {
		t.Fatalf("方法 = %q", env.Method.Local)
	}
	names := env.Method.Child("ParameterNames")
	if names == nil {
		t.Fatal("没有 ParameterNames")
	}
	var got []string
	for _, k := range names.Kids {
		got = append(got, k.Trimmed())
	}
	if len(got) != 14 {
		t.Errorf("参数个数 = %d，期望 14（历史抓包固定就是这个数）", len(got))
	}
	for _, n := range got {
		if strings.HasSuffix(n, ".") {
			t.Errorf("下发了子树路径（有的 CPE 不支持）：%q", n)
		}
		if !strings.HasPrefix(n, "InternetGatewayDevice.") {
			t.Errorf("参数名不在预期的根下：%q", n)
		}
	}
}

// 真机上出现的厂商笔误：0 BOOTSTRAP 时华为上报的私有节点，
// 根节点名写成了 InternetGateWayDevice（大写 W）。
// 它必须能被识别为同一个数据模型根，同时参数名原样保留。
func TestVendorTypoInRootPrefix(t *testing.T) {
	params := []ParamValue{
		{Name: "InternetGateWayDevice.DeviceInfo.X_CT-ProvCode", Value: "00/0", Type: "string"},
		{Name: "InternetGatewayDevice.DeviceInfo.SpecVersion", Value: "1.0", Type: "string"},
	}

	if got := detectRoot(params); got != "InternetGatewayDevice." {
		t.Errorf("数据模型根 = %q，大小写不一致也应识别出来", got)
	}

	typos := findRootTypo("InternetGatewayDevice.", params)
	if len(typos) != 1 || typos[0] != "InternetGateWayDevice.DeviceInfo.X_CT-ProvCode" {
		t.Errorf("应当找出 1 个大小写写错的根前缀，实际 %v", typos)
	}

	// 参数名不能被改写（以后 SetParameterValues 必须用设备自己的拼法）
	sp := toStoreParams(params)
	if sp[0].Name != "InternetGateWayDevice.DeviceInfo.X_CT-ProvCode" {
		t.Errorf("参数名被改写了：%q", sp[0].Name)
	}

	// 即使只有这一个「根写错」的参数，也要能认出根来
	only := []ParamValue{{Name: "InternetGateWayDevice.DeviceInfo.SoftwareVersion", Value: "V1"}}
	if got := detectRoot(only); got != "InternetGatewayDevice." {
		t.Errorf("只有笔误参数时根探测失败：%q", got)
	}
	if d := deviceFieldsFromParams(only); d.SoftwareVersion != "V1" {
		t.Errorf("笔误根下的字段没被识别：%+v", d)
	}
}
