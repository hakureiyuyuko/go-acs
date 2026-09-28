package web

import (
	"testing"

	"acs/internal/store"
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

func TestWifiOverviewEmpty(t *testing.T) {
	if got := WifiOverview(nil); len(got) != 0 {
		t.Errorf("没有无线参数时应返回空，实际 %+v", got)
	}
	// 无关参数不能被误收
	if got := WifiOverview([]store.Param{{Name: "InternetGatewayDevice.DeviceInfo.UpTime", Value: "1"}}); len(got) != 0 {
		t.Errorf("无关参数不该进 WiFi 概览，实际 %+v", got)
	}
}
