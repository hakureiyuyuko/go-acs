package web

import (
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 样本取自真机（华为 HN8145X6N）读回的无线参数：2.4G 是实例 1、5G 是实例 5。
func huaweiWifiParams() []store.Param {
	mk := func(name, value string) store.Param {
		return store.Param{Name: name, Value: value, Source: "getvalues"}
	}
	b := "InternetGatewayDevice.LANDevice.1.WLANConfiguration."
	return []store.Param{
		mk(b+"1.SSID", "WirelessNet"),
		mk(b+"1.X_HW_RFBand", "2.4GHz"),
		mk(b+"1.Channel", "6"),
		mk(b+"1.X_HW_Standard", "11ax"),
		mk(b+"1.X_HW_WPAand11iEncryptionModes", "TKIPandAESEncryption"),
		mk(b+"1.RadioEnabled", "1"),
		mk(b+"1.TotalAssociations", "1"),
		mk(b+"1.Status", "Up"),

		mk(b+"5.SSID", "WirelessNet-5G"),
		mk(b+"5.X_HW_RFBand", "5GHz"),
		mk(b+"5.Channel", "0"),
		mk(b+"5.X_HW_Standard", "11ax"),
		mk(b+"5.RadioEnabled", "0"),
		mk(b+"5.TotalAssociations", "0"),
		mk(b+"5.Status", "Disabled"),
	}
}

func TestWifiOverviewGroupsByInstance(t *testing.T) {
	bands := WifiOverview(huaweiWifiParams())
	if len(bands) != 2 {
		t.Fatalf("应得到 2 个频段，实际 %d: %+v", len(bands), bands)
	}

	// 2.4G 必须排在前面
	if bands[0].Instance != 1 || bands[0].Label != "2.4G" {
		t.Errorf("第一个频段 = 实例 %d / %s", bands[0].Instance, bands[0].Label)
	}
	if bands[0].SSID != "WirelessNet" || bands[0].Channel != "6" || bands[0].Clients != "1" {
		t.Errorf("2.4G 概况 = %+v", bands[0])
	}
	if !bands[0].HaveOn || !bands[0].On {
		t.Errorf("2.4G 射频应该识别为「开」: %+v", bands[0])
	}

	if bands[1].Instance != 5 || bands[1].Label != "5G" {
		t.Errorf("第二个频段 = 实例 %d / %s", bands[1].Instance, bands[1].Label)
	}
	if bands[1].SSID != "WirelessNet-5G" {
		t.Errorf("5G SSID = %q", bands[1].SSID)
	}
	// 5G 射频是关的（RadioEnabled=0），必须识别成「关」而不是「未知」
	if !bands[1].HaveOn || bands[1].On {
		t.Errorf("5G 射频应该识别为「关」: %+v", bands[1])
	}

	if got := wifiCount(bands); got != 1 {
		t.Errorf("终端总数 = %d，期望 1", got)
	}
}

// 设备没报频段时不能瞎猜。真机上实例号是 1 和 5，猜成 2.4G/5G 就会错。
func TestWifiOverviewDoesNotGuessBand(t *testing.T) {
	b := "Device.WiFi.Radio."
	params := []store.Param{
		{Name: b + "1.Status", Value: "Up"},
		{Name: "Device.WiFi.SSID.1.SSID", Value: "MyNet"},
		{Name: b + "1.Channel", Value: "36"},
		{Name: b + "1.OperatingFrequencyBand", Value: "5GHz"},
	}
	bands := WifiOverview(params)
	if len(bands) != 1 {
		t.Fatalf("TR-181 的 Radio 与 SSID 同实例号应合并成 1 条，实际 %d: %+v", len(bands), bands)
	}
	if bands[0].SSID != "MyNet" {
		t.Errorf("SSID 没合并进来: %+v", bands[0])
	}
	if bands[0].Label != "5G" {
		t.Errorf("频段标签 = %q，期望 5G", bands[0].Label)
	}
}

func TestWifiOverviewTruthiness(t *testing.T) {
	// TotalAssociations 是字符串 "0" —— 模板里要能显示出来，不能当成「没数据」
	bands := WifiOverview([]store.Param{
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.2.SSID", Value: "X"},
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.2.TotalAssociations", Value: "0"},
	})
	if len(bands) != 1 || bands[0].Clients != "0" {
		t.Fatalf("0 个终端应保留为字符串 \"0\"，实际 %+v", bands)
	}
	if got := wifiCount(bands); got != 0 {
		t.Errorf("终端总数 = %d，期望 0", got)
	}

	// 既没有频段也没有已知实例时，标签给出实例号而不是乱猜
	if bands[0].Label != "实例 2" {
		t.Errorf("标签 = %q，期望「实例 2」（设备没报频段就不猜）", bands[0].Label)
	}
}

func TestWifiFormBuildsFromDeviceParams(t *testing.T) {
	b := "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1."
	mk := func(name, value, typ string) store.Param {
		return store.Param{Name: b + name, Value: value, ValueType: typ, Writable: true, Source: "getnames"}
	}
	params := []store.Param{
		mk("SSID", "WirelessNet", "string"),
		mk("Enable", "1", "boolean"),
		mk("RadioEnabled", "1", "boolean"),
		mk("AutoChannelEnable", "1", "boolean"),
		mk("Channel", "6", "unsignedInt"),
		mk("PossibleChannels", "1,2,3,6,11,13", "string"),
		mk("BeaconType", "11i", "string"),
		mk("IEEE11iEncryptionModes", "AESEncryption", "string"),
		mk("TransmitPower", "100", "unsignedInt"),
		mk("TransmitPowerSupported", "20,40,60,80,100", "string"),
		mk("KeyPassphrase", "", "string"),
	}

	fields := WifiForm(1, params)
	byKey := map[string]WifiFormField{}
	for _, f := range fields {
		byKey[f.Key] = f
	}

	for _, k := range []string{"ssid", "enable", "radio", "auto_channel", "channel", "auth", "cipher", "power", "key"} {
		if _, ok := byKey[k]; !ok {
			t.Errorf("字段 %s 没生成出来", k)
		}
	}
	// 设备没报的参数不要凭空出现
	if _, ok := byKey["bandwidth"]; ok {
		t.Error("设备没有 OperatingChannelBandwidth，不该出现信道带宽字段")
	}
	if _, ok := byKey["standard"]; ok {
		t.Error("设备没报 Standard，不该出现无线标准字段")
	}

	// 参数名必须是设备的真实拼写（大小写敏感）
	if byKey["ssid"].Param != b+"SSID" {
		t.Errorf("SSID 参数名 = %q", byKey["ssid"].Param)
	}
	if byKey["ssid"].Value != "WirelessNet" || byKey["ssid"].Kind != "text" {
		t.Errorf("SSID 字段 = %+v", byKey["ssid"])
	}
	if byKey["enable"].Kind != "bool" || byKey["enable"].Value != "1" {
		t.Errorf("启用字段 = %+v", byKey["enable"])
	}

	// 信道下拉选项来自设备的 PossibleChannels，而且当前值 6 要能选中
	ch := byKey["channel"]
	if ch.Kind != "select" || len(ch.Options) != 6 {
		t.Errorf("信道字段 = %+v", ch)
	}
	if !hasOption(ch.Options, "6") {
		t.Error("信道下拉里没有当前值 6")
	}

	// 发射功率：候选来自 TransmitPowerSupported，带 % 单位
	pw := byKey["power"]
	if pw.Kind != "select" || len(pw.Options) != 5 {
		t.Errorf("发射功率字段 = %+v", pw)
	}
	if pw.Options[0].Label != "20%" {
		t.Errorf("发射功率选项标签 = %q，期望 20%%", pw.Options[0].Label)
	}

	// 密码字段：type=password，并且类型要带对（写回去用）
	if byKey["key"].Kind != "password" {
		t.Errorf("密码字段类型 = %q", byKey["key"].Kind)
	}
	if byKey["channel"].Type != "unsignedInt" {
		t.Errorf("信道类型 = %q，写回去必须是 xsd:unsignedInt", byKey["channel"].Type)
	}
}

func TestWifiFormSelectFallsBackToText(t *testing.T) {
	b := "Device.WiFi.Radio.1."
	params := []store.Param{
		{Name: b + "Channel", Value: "36", ValueType: "unsignedInt", Writable: true},
	}
	fields := WifiForm(1, params)
	if len(fields) != 1 {
		t.Fatalf("字段数 = %d", len(fields))
	}
	// 没拿到 PossibleChannels，下拉框没有候选 -> 该退化成文本框，而不是给个空下拉
	if fields[0].Kind != "text" {
		t.Errorf("应退化成 text，实际 %q", fields[0].Kind)
	}
}

func TestWifiFormMarksReadOnly(t *testing.T) {
	b := "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1."
	params := []store.Param{
		// 有可写信息（来自 GetParameterNames）时，只读的字段要标出来
		{Name: b + "SSID", Value: "X", ValueType: "string", Writable: false, Source: "getnames"},
		{Name: b + "Enable", Value: "1", ValueType: "boolean", Writable: true, Source: "getnames"},
	}
	fields := WifiForm(1, params)
	if len(fields) == 0 || !fields[0].ReadOnly {
		t.Errorf("SSID 应标记为只读: %+v", fields)
	}

	// 没有可写信息时（全都是取值得来的、没枚举过名字）不该一律标只读，
	// 否则整个表单都会被置灰。
	for i := range params {
		params[i].Source = "getvalues"
		params[i].Writable = false
	}
	fields = WifiForm(1, params)
	if fields[0].ReadOnly {
		t.Error("没有可写信息时不应标记只读")
	}
}

// 真机背景：往 WLANConfiguration.{i}.KeyPassphrase 写密码，华为 HN8145X6N 回了
// 9007 Invalid parameter value。WPA/WPA2-PSK 的密码在 PreSharedKey.1.KeyPassphrase 下，
// 后者才是给 WEP 用的。所以密码字段必须优先选 PreSharedKey 那个。
func TestWifiFormPrefersPreSharedKeyForPassword(t *testing.T) {
	b := "InternetGatewayDevice.LANDevice.1.WLANConfiguration.5."
	params := []store.Param{
		{Name: b + "SSID", Value: "X", ValueType: "string", Writable: true},
		{Name: b + "BeaconType", Value: "11i", ValueType: "string", Writable: true},
		{Name: b + "KeyPassphrase", Value: "", ValueType: "string", Writable: true},
		{Name: b + "PreSharedKey.1.KeyPassphrase", Value: "", ValueType: "string", Writable: true},
	}
	fields := WifiForm(5, params)
	var key *WifiFormField
	for i := range fields {
		if fields[i].Key == "key" {
			key = &fields[i]
		}
	}
	if key == nil {
		t.Fatal("密码字段没生成")
	}
	if key.Param != b+"PreSharedKey.1.KeyPassphrase" {
		t.Errorf("密码应写到 PreSharedKey.1.KeyPassphrase，实际 %q", key.Param)
	}

	// 没有 PreSharedKey 表（例如纯 WEP 设备）时退回 KeyPassphrase
	params2 := []store.Param{
		{Name: b + "KeyPassphrase", Value: "", ValueType: "string", Writable: true},
	}
	fields2 := WifiForm(5, params2)
	if len(fields2) != 1 || fields2[0].Param != b+"KeyPassphrase" {
		t.Errorf("没 PreSharedKey 时应退回 KeyPassphrase，实际 %+v", fields2)
	}
}

func TestMatchCandidate(t *testing.T) {
	cases := []struct {
		rel, cand string
		want      bool
	}{
		{"ssid", "ssid", true},
		{"ssid", "x_hw_ssid", false}, // 单段候选只比叶子，不做尾部匹配
		{"ssid", "SP", false},
		{"presharedkey.1.keypassphrase", "presharedkey.1.keypassphrase", true},
		{"presharedkey.1.keypassphrase", "keypassphrase", true}, // 单段候选命中叶子
		{"keypassphrase", "presharedkey.1.keypassphrase", false},
		{"radioenabled", "radio", false},
	}
	for _, c := range cases {
		if got := matchCandidate(c.rel, c.cand); got != c.want {
			t.Errorf("matchCandidate(%q, %q) = %v，期望 %v", c.rel, c.cand, got, c.want)
		}
	}
}

// 概览页搜索：序列号 / 备注 / 名称 / 产品类 / OUI / SSID 都能搜。
func TestDeviceMatches(t *testing.T) {
	d := &store.Device{
		SerialNumber: "48575443AA000001",
		Manufacturer: "Huawei Technologies Co., Ltd",
		ModelName:    "HN8145X6N",
		ProductClass: "HN8145X6N",
		OUI:          "00259E",
		Note:         "3 楼会议室",
	}
	bands := []WifiBand{{SSID: "LabWifi"}}

	yes := []string{"", "48575443AA00", "0001", "3 楼", "会议室", "huawei", "8145x6n", "00259e", "labwifi"}
	for _, q := range yes {
		if !deviceMatches(d, bands, q) {
			t.Errorf("应命中: %q", q)
		}
	}
	no := []string{"不存在的序列号", "zzz", "小米"}
	for _, q := range no {
		if deviceMatches(d, bands, q) {
			t.Errorf("不应命中: %q", q)
		}
	}

	// 空备注不应把空串当成“命中一切”
	d2 := &store.Device{SerialNumber: "X"}
	if deviceMatches(d2, nil, "abc") {
		t.Error("无关搜索词不该命中")
	}
}

// 备注变更后要能搜到（模拟实际流程：写库 -> 列表过滤）。
func TestDeviceMatchesAfterNoteUpdate(t *testing.T) {
	d := &store.Device{SerialNumber: "SN1"}
	if deviceMatches(d, nil, "会议室") {
		t.Fatal("设置备注前不该命中")
	}
	d.Note = "会议室"
	if !deviceMatches(d, nil, "会议室") {
		t.Error("设置备注后应能搜到")
	}
}

func TestWifiOverviewEmpty(t *testing.T) {
	if got := WifiOverview(nil); len(got) != 0 {
		t.Errorf("没有无线参数时应返回空，实际 %+v", got)
	}
	// 无关参数不能被误收
	if got := WifiOverview([]store.Param{{Name: "InternetGatewayDevice.DeviceInfo.UpTime", Value: "1"}}); len(got) != 0 {
		t.Errorf("无关参数不该进 WiFi 概览，实际 %+v", got)
	}
}
