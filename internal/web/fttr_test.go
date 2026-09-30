package web

import (
	"strings"
	"testing"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 样本取自真机（华为 V271-20 FTTR 主机）读回的 X_HW_APDevice 子设备表。
func TestFttrOverviewButRealDeviceParams(t *testing.T) {
	b := "InternetGatewayDevice.X_HW_APDevice."
	mk := func(name, val string) store.Param {
		return store.Param{Name: b + name, Value: val, ValueType: "string", Source: "getvalues", UpdatedAt: time.Now()}
	}
	params := []store.Param{
		// 顶层对象节点：能力探测时枚举得到
		{Name: b, Value: "", Source: "getnames"},
		// 实例 1：K251e
		mk("1.SerialNumber", "HWTCAA000001"),
		mk("1.DeviceType", "K251e"),
		mk("1.APMacAddr", "02:73:E2:51:DA:EA"),
		mk("1.ApOnlineFlag", "1"),
		mk("1.DeviceStatus", "OK"),
		mk("1.SoftwareVersion", "V5R023C10S326"),
		mk("1.HardwareVersion", "3A17.A"),
		mk("1.CurrentChannel", "6,36"),
		mk("1.SupportedRFBand", "2.4G,5G"),
		mk("1.SignalIntensity", "0"),
		mk("1.SyncStatus", "3"),
		mk("1.UpTime", "361:36:38"),
		mk("1.WorkingMode", "repeater"),
		// 实例 2：K251-20
		mk("2.SerialNumber", "48575443AA000002"),
		mk("2.DeviceType", "K251-20"),
		mk("2.APMacAddr", "02:16:C8:61:F4:BC"),
		mk("2.ApOnlineFlag", "1"),
		mk("2.SoftwareVersion", "V5R023C10S300"),
		mk("2.CurrentChannel", "1,36"),
		// 实例 4：K251-20（实例号不连续，缺 3）
		mk("4.SerialNumber", "48575443AA000003"),
		mk("4.DeviceType", "K251-20"),
		mk("4.ApOnlineFlag", "0"),
		mk("4.CurrentChannel", "7,36"),
		// 子设备自己的无线配置：不应被当成子设备顶层字段
		mk("1.WLANConfiguration.1.SSID", "LabWifi"),
		mk("1.WLANConfiguration.1.KeyPassphrase", ""),
	}

	nodes, ok := FttrOverview(params)
	if !ok {
		t.Fatal("应当识别出设备具备 FTTR 子设备能力")
	}
	if len(nodes) != 3 {
		t.Fatalf("应解析出 3 台子设备，实际 %d", len(nodes))
	}
	// 实例号必须排序，而且不连续也要保留原号
	if nodes[0].Instance != 1 || nodes[1].Instance != 2 || nodes[2].Instance != 4 {
		t.Errorf("实例号 = %d/%d/%d，期望 1/2/4", nodes[0].Instance, nodes[1].Instance, nodes[2].Instance)
	}
	if nodes[0].Model != "K251e" || nodes[0].Serial != "HWTCAA000001" || nodes[0].MAC != "02:73:E2:51:DA:EA" {
		t.Errorf("实例 1 解析错: %+v", nodes[0])
	}
	if nodes[0].Firmware != "V5R023C10S326" || nodes[0].Hardware != "3A17.A" {
		t.Errorf("实例 1 版本解析错: %+v", nodes[0])
	}
	if nodes[0].Channel != "6,36" || nodes[0].Band != "2.4G,5G" || nodes[0].Uptime != "361:36:38" {
		t.Errorf("实例 1 信道/频段/时长解析错: %+v", nodes[0])
	}
	if !nodes[0].Online || !nodes[1].Online {
		t.Error("实例 1/2 应当在线")
	}
	if nodes[2].Online {
		t.Error("实例 4 的 ApOnlineFlag=0，应当离线")
	}
	if fttrOnlineCount(nodes) != 2 {
		t.Errorf("在线数应为 2，实际 %d", fttrOnlineCount(nodes))
	}
}

// 没有子设备对象的设备：必须返回 ok=false，界面才会整块不显示。
func TestFttrOverviewWithoutCapability(t *testing.T) {
	params := []store.Param{
		{Name: "InternetGatewayDevice.DeviceInfo.ModelName", Value: "HN8145X6N"},
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID", Value: "X"},
		{Name: "InternetGatewayDevice.LANDevice.", Value: ""},
	}
	if nodes, ok := FttrOverview(params); ok {
		t.Errorf("不该识别出 FTTR 能力，却返回了 %d 台", len(nodes))
	}
	if _, ok := FttrOverview(nil); ok {
		t.Error("空参数不该识别出 FTTR 能力")
	}
}

// 标准 TR-181 Multi-AP（Wi-Fi Data Elements）也要能认出来。
func TestFttrOverviewTR181DataElements(t *testing.T) {
	params := []store.Param{
		{Name: "Device.WiFi.DataElements.Network.", Value: "", Source: "getnames"},
		{Name: "Device.WiFi.DataElements.Network.Device.1.SerialNumber", Value: "SN-1"},
		{Name: "Device.WiFi.DataElements.Network.Device.1.SoftwareVersion", Value: "1.2.3"},
	}
	nodes, ok := FttrOverview(params)
	if !ok || len(nodes) != 1 {
		t.Fatalf("应识别出 1 台 Multi-AP 设备，ok=%v nodes=%d", ok, len(nodes))
	}
	if nodes[0].Serial != "SN-1" || nodes[0].Firmware != "1.2.3" {
		t.Errorf("解析错: %+v", nodes[0])
	}
}

// 大小写不该影响识别（真机上厂商写法五花八门）。
func TestFttrOverviewCaseInsensitive(t *testing.T) {
	params := []store.Param{
		{Name: "InternetGateWayDevice.X_HW_APDevice.", Value: ""},
		{Name: "internetgatewaydevice.x_hw_apdevice.1.SerialNumber", Value: "S"},
	}
	nodes, ok := FttrOverview(params)
	if !ok || len(nodes) != 1 || nodes[0].Serial != "S" {
		t.Errorf("大小写差异不该影响识别: ok=%v nodes=%+v", ok, nodes)
	}
}

// 光功率 / 组网模式：样本是各家常见的几种写法（华为 X_HW_RxPower、标准 OpticalRxPower）。
func TestFttrOverviewOpticalAndMode(t *testing.T) {
	b := "InternetGatewayDevice.X_HW_APDevice."
	mk := func(name, val string) store.Param {
		return store.Param{Name: b + name, Value: val, Source: "getvalues", UpdatedAt: time.Now()}
	}
	params := []store.Param{
		{Name: b, Value: "", Source: "getnames"},
		// 实例 1：光纤组网 + 光功率（华为写法）
		mk("1.SerialNumber", "SN-FIBER"),
		mk("1.WorkingMode", "fttr"),
		mk("1.SignalIntensity", "0"),
		mk("1.X_HW_RxPower", "-19.2"),
		mk("1.X_HW_TxPower", "2.5"),
		// 实例 2：无线组网 —— 即使设备也报了光功率，也不该显示（无线没有光口）
		mk("2.SerialNumber", "SN-WIFI"),
		mk("2.WorkingMode", "wifi"),
		mk("2.SignalIntensity", "-45"),
		mk("2.X_HW_RxPower", "0"),
		// 实例 3：有线（以太网）组网，同样不显示光功率
		mk("3.SerialNumber", "SN-ETH"),
		mk("3.WorkingMode", "eth"),
		mk("3.X_HW_RxPower", "-20.1"),
		// 实例 4：组网模式归不了一类（真机上就是 repeater）→ 原样显示，光功率照常给
		mk("4.SerialNumber", "SN-UNKNOWN"),
		mk("4.WorkingMode", "repeater"),
		mk("4.SupportedWorkingMode", "repeater"),
		mk("4.SignalIntensity", "0"),
		mk("4.Optical.RxPower", "-21.7"), // 嵌套写法也要认
		// 无线发射功率不是光功率，别认错
		mk("4.TransmitPower", "100,100"),
	}
	nodes, ok := FttrOverview(params)
	if !ok || len(nodes) != 4 {
		t.Fatalf("应解析出 4 台子设备，ok=%v nodes=%d", ok, len(nodes))
	}
	n := map[int]FttrNode{}
	for _, x := range nodes {
		n[x.Instance] = x
	}

	if got := n[1].OpticalPower(); got != "Rx -19.20 dBm / Tx 2.50 dBm" {
		t.Errorf("光纤组网的光功率显示不对：%q", got)
	}
	if got := n[1].ModeText(); got != "光纤组网" {
		t.Errorf("WorkingMode=fttr 应显示光纤组网，得到 %q", got)
	}

	if got := n[2].OpticalPower(); got != "" {
		t.Errorf("无线组网不该显示光功率，却得到 %q", got)
	}
	if got := n[2].ModeText(); got != "无线组网（信号 -45）" {
		t.Errorf("无线组网的文字不对：%q", got)
	}

	if got := n[3].OpticalPower(); got != "" {
		t.Errorf("有线组网不该显示光功率，却得到 %q", got)
	}
	if got := n[3].ModeText(); got != "有线组网" {
		t.Errorf("WorkingMode=eth 应显示有线组网，得到 %q", got)
	}

	// 归不了一类时：显示设备自报的原值，不美化；光功率仍显示（没有证据说是无线/有线）
	if got := n[4].ModeText(); got != "repeater" {
		t.Errorf("判不出组网模式时应显示原值，得到 %q", got)
	}
	if got := n[4].OpticalPower(); got != "Rx -21.70 dBm" {
		t.Errorf("嵌套写法的光功率没认出来：%q", got)
	}
	if n[4].TxPower != "" {
		t.Errorf("TransmitPower 不该被当成光发射功率：%q", n[4].TxPower)
	}
	// SupportedWorkingMode 不能被当成 WorkingMode（后缀匹配很容易踩）
	if n[4].Mode != "repeater" || n[4].ModesSupported != "repeater" {
		t.Errorf("WorkingMode / SupportedWorkingMode 串了：%+v", n[4])
	}
	// 悬停提示里要能看到原始字段
	if h := n[4].ModeHint(); !strings.HasPrefix(h, "设备上报：") || !strings.Contains(h, "repeater") {
		t.Errorf("组网悬停提示不完整：%q", h)
	}

	if !fttrHasOptical(nodes) {
		t.Error("有光功率时整列应当渲染")
	}
	onlyRepeater := []FttrNode{n[4]}
	onlyRepeater[0].RxPower = ""
	if fttrHasOptical(onlyRepeater) {
		t.Error("一台都没读到光功率时不该渲染那一列")
	}
}
