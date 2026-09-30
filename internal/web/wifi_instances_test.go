package web

import (
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 假实例（phantom）与空实例：真机上真的会冒出来，界面不该显示它们。
//
// 背景（2026-09-30 用户截图）：华为 TR-098 设备同时挂两套对象 ——
// `LANDevice.1.WLANConfiguration.{i}`（SSID，实例号真机是 1/5）和
// `LANDevice.1.WiFi.Radio.{i}`（射频，实例号是 1/2）。以前按实例号硬合并，
// 射频 2 就凭空造出一个「实例 2」，在详情页上渲染成
// 「5G ｜ - ｜ 开 ｜ - ｜ - ｜ - ｜ - ｜ -」这样一行无效显示。
func TestWifiOverviewIgnoresRadioOnlyInstance(t *testing.T) {
	b := "InternetGatewayDevice.LANDevice.1."
	params := []store.Param{
		// 真机（HN8145X6N）里的射频对象：只有开关与频段，没有 SSID
		{Name: b + "WiFi.Radio.1.Enable", Value: "1"},
		{Name: b + "WiFi.Radio.1.OperatingFrequencyBand", Value: "2.4GHz"},
		{Name: b + "WiFi.Radio.2.Enable", Value: "1"},
		{Name: b + "WiFi.Radio.2.OperatingFrequencyBand", Value: "5GHz"},
		// 真正的 SSID 实例
		{Name: b + "WLANConfiguration.1.SSID", Value: "HUAWEI-F2DA"},
		{Name: b + "WLANConfiguration.1.X_HW_RFBand", Value: "2.4GHz"},
		{Name: b + "WLANConfiguration.1.Channel", Value: "5"},
		{Name: b + "WLANConfiguration.1.Status", Value: "Up"},
		{Name: b + "WLANConfiguration.5.SSID", Value: "HUAWEI-F2DA-5G"},
		{Name: b + "WLANConfiguration.5.X_HW_RFBand", Value: "5GHz"},
		{Name: b + "WLANConfiguration.5.Channel", Value: "36"},
		{Name: b + "WLANConfiguration.5.Status", Value: "Up"},
	}
	bands := WifiOverview(params)
	if len(bands) != 2 {
		t.Fatalf("应该只有 1 和 5 两个实例，实际 %d 个：%+v", len(bands), bands)
	}
	for _, x := range bands {
		if x.Instance == 2 {
			t.Errorf("射频对象不该造出一个实例 2：%+v", x)
		}
		if x.SSID == "" {
			t.Errorf("不该出现没有 SSID 的行：%+v", x)
		}
	}

	// 可编辑实例同样不该把射频对象算进来（否则详情页会多一个点开什么也没有的「修改」）
	insts := WifiInstances(params)
	if len(insts) != 2 || insts[0] != 1 || insts[1] != 5 {
		t.Errorf("可编辑实例应该是 [1 5]，实际 %v", insts)
	}
}

// 射频对象的字段仍然要能补充到同实例号的 SSID 上（TR-181 就靠这个：
// Device.WiFi.Radio.{i} 提供频段/信道/状态，Device.WiFi.SSID.{i} 提供名字）。
func TestWifiOverviewRadioEnrichesSameInstance(t *testing.T) {
	params := []store.Param{
		{Name: "Device.WiFi.Radio.1.Status", Value: "Up"},
		{Name: "Device.WiFi.Radio.1.Channel", Value: "36"},
		{Name: "Device.WiFi.Radio.1.OperatingFrequencyBand", Value: "5GHz"},
		{Name: "Device.WiFi.Radio.1.Enable", Value: "1"},
		{Name: "Device.WiFi.SSID.1.SSID", Value: "MyNet"},
	}
	bands := WifiOverview(params)
	if len(bands) != 1 {
		t.Fatalf("射频与 SSID 同实例号应合并成一条，实际 %d: %+v", len(bands), bands)
	}
	got := bands[0]
	if got.SSID != "MyNet" || got.Label != "5G" || got.Channel != "36" || got.Status != "Up" || !got.On {
		t.Errorf("射频字段没补充进来：%+v", got)
	}
}

// 什么都不报的实例不显示（不摆空壳）。
func TestWifiOverviewHidesEmptyInstance(t *testing.T) {
	b := "InternetGatewayDevice.LANDevice.1.WLANConfiguration."
	params := []store.Param{
		{Name: b + "1.SSID", Value: "Net"},
		{Name: b + "1.Channel", Value: "6"},
		// 实例 3 只有一个开关，别的什么都没报
		{Name: b + "3.Enable", Value: "1"},
	}
	bands := WifiOverview(params)
	if len(bands) != 1 || bands[0].Instance != 1 {
		t.Fatalf("只该显示有内容的实例 1，实际 %+v", bands)
	}
}
