package web

import (
	"testing"
	"time"

	"acs/internal/store"
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
