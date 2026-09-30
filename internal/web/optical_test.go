package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// 参数名各家不统一，认的是语义不是枚举：新写法只要设备报了就该认出来，
// 无线的 TransmitPower 依旧不能算光发射功率。
func TestOpticalFieldNaming(t *testing.T) {
	cases := []struct {
		name string
		rx   bool
		tx   bool
	}{
		{"RxPower", true, false},
		{"X_HW_RxPower", true, false},
		{"rx_power", true, false},
		{"OpticalPowerRx", true, false},
		{"ReceiveOpticalPower", true, false},
		{"PonRxPower", true, false},
		{"WANPONInterfaceConfig.OpticalRxPower", true, false},
		{"TxPower", false, true},
		{"X_HW_TxPower", false, true},
		{"tx_power", false, true},
		{"OpticalPowerTx", false, true},
		{"TransmitOpticalPower", false, true},
		{"PonTxPower", false, true},
		{"WANPONInterfaceConfig.OpticalTxPower", false, true},
		// 不该认的
		{"TransmitPower", false, false}, // 无线发射功率
		{"TransmitPowerSupported", false, false},
		{"PowerConsumption", false, false}, // 有 power 但跟光无关
		{"RxBytes", false, false},
		{"OpticalModuleType", false, false}, // 有 optical 但不是功率
	}
	for _, c := range cases {
		rx, tx := opticalField(c.name)
		if rx != c.rx || tx != c.tx {
			t.Errorf("opticalField(%q) = rx %v / tx %v，要的是 rx %v / tx %v",
				c.name, rx, tx, c.rx, c.tx)
		}
	}
}

// 设备页要有「采集光功率」这个入口，否则概览页两列永远只能是 -：
// 纳管时采的基本信息里没有光功率，得让人能手动补一次。
func TestDevicePageHasOpticalButton(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer st.Close()
	if _, _, err := st.UpsertDevice(&store.Device{
		OUI: "001122", ProductClass: "SimRouter", SerialNumber: "OPT-TEST",
	}); err != nil {
		t.Fatalf("造设备失败: %v", err)
	}
	if err := st.UpsertParams(1, []store.Param{
		mkParam("InternetGatewayDevice.WANDevice.1.WANPONInterfaceConfig.OpticalRxPower", "-21.5"),
		mkParam("InternetGatewayDevice.WANDevice.1.WANPONInterfaceConfig.OpticalTxPower", "1.8"),
	}, "getvalues"); err != nil {
		t.Fatalf("写参数失败: %v", err)
	}
	mux := http.NewServeMux()
	if err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/devices/1", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/devices/1/optical") {
		t.Error("设备页应有「采集光功率」的入口（POST /devices/{id}/optical）")
	}
	if !strings.Contains(body, "-21.5 dBm") || !strings.Contains(body, "1.8 dBm") {
		t.Error("设备页应显示已采集到的收光/发光")
	}

	// 点按钮：入队后跳回设备页
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, httptest.NewRequest("POST", "/devices/1/optical", nil))
	if w2.Code != http.StatusSeeOther {
		t.Errorf("POST 应 303 跳回设备页，得到 %d", w2.Code)
	}
}
