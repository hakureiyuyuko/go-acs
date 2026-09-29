package web

import (
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

func mkParam(name, value string) store.Param {
	return store.Param{Name: name, Value: value, Source: "getvalues"}
}

// 概览页的「收光 / 发光」：主机自己报了就用主机的。
func TestOpticalOverviewHost(t *testing.T) {
	got := OpticalOverview([]store.Param{
		mkParam("InternetGatewayDevice.WANDevice.1.X_HW_RxPower", "-21.5"),
		mkParam("InternetGatewayDevice.WANDevice.1.X_HW_TxPower", "2.5"),
	})
	if got.Rx != "-21.5 dBm" || got.Tx != "2.5 dBm" {
		t.Errorf("主机光功率 = Rx %q / Tx %q", got.Rx, got.Tx)
	}
	if got.SourceText != "" {
		t.Errorf("主机自己上报时不该标注来源，得到 %q", got.SourceText)
	}
	if !got.Has() {
		t.Error("读到了光功率，Has 应为 true")
	}
}

// 主机没报、FTTR 子设备报了：用子设备的，但必须说明是哪一台 ——
// 子光猫的收光跟主机的收光不是一回事，混着看会误判线路。
func TestOpticalOverviewFallsBackToSubDevice(t *testing.T) {
	got := OpticalOverview([]store.Param{
		mkParam("InternetGatewayDevice.X_HW_APDevice.2.X_HW_RxPower", "-18.0"),
		mkParam("InternetGatewayDevice.X_HW_APDevice.2.X_HW_TxPower", "2.0"),
		mkParam("InternetGatewayDevice.X_HW_APDevice.1.X_HW_RxPower", "-19.0"),
	})
	if got.Rx != "-19.0 dBm" {
		t.Errorf("应取实例号最小的那台子设备，得到 %q", got.Rx)
	}
	if got.SourceText != "来自子设备 1" {
		t.Errorf("应注明来自子设备 1，得到 %q", got.SourceText)
	}
}

// 只要主机报了任何一项，就不该拿子设备的来充数。
func TestOpticalOverviewHostWins(t *testing.T) {
	got := OpticalOverview([]store.Param{
		mkParam("InternetGatewayDevice.X_HW_APDevice.1.X_HW_RxPower", "-19.0"),
		mkParam("InternetGatewayDevice.WANDevice.1.Optical.RxPower", "-23.1"),
	})
	if got.Rx != "-23.1 dBm" || got.SourceText != "" {
		t.Errorf("主机优先失效：Rx %q / 来源 %q", got.Rx, got.SourceText)
	}
}

// 无线的发射功率（TransmitPower）不是光功率，不能被当成「发光」。
func TestOpticalOverviewIgnoresWirelessTransmitPower(t *testing.T) {
	got := OpticalOverview([]store.Param{
		mkParam("InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.TransmitPower", "100"),
		mkParam("InternetGatewayDevice.X_HW_APDevice.1.TransmitPowerSupported", "100,50"),
	})
	if got.Has() {
		t.Errorf("无线发射功率被当成光功率了：%+v", got)
	}
}

// 什么都没读到：返回空值（界面显示 "-"），不编数字。
func TestOpticalOverviewEmpty(t *testing.T) {
	got := OpticalOverview([]store.Param{
		mkParam("InternetGatewayDevice.WANDevice.1.X_HW_RxPower", ""), // 上报了但是空串
		mkParam("InternetGatewayDevice.DeviceInfo.ModelName", "X1"),
	})
	if got.Has() || got.Rx != "" || got.Tx != "" || got.SourceText != "" {
		t.Errorf("没读到就该是空的，得到 %+v", got)
	}
}

// 设备把单位一起报回来了（真机上见过 "−19.0dBm" 这种写法）就别再补一个。
func TestWithDbm(t *testing.T) {
	if got := withDbm("-19.0"); got != "-19.0 dBm" {
		t.Errorf("纯数字应补单位，得到 %q", got)
	}
	if got := withDbm("-19.0dBm"); got != "-19.0dBm" {
		t.Errorf("已经带单位的不该重复补，得到 %q", got)
	}
}
