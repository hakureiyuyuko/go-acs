package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if got.Rx != "-21.50 dBm" || got.Tx != "2.50 dBm" {
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
	if got.Rx != "-19.00 dBm" {
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
	if got.Rx != "-23.10 dBm" || got.SourceText != "" {
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
// 读数一律固定两位小数。
func TestWithDbm(t *testing.T) {
	cases := []struct{ in, want string }{
		{"-19.0", "-19.00 dBm"},
		{"-19.0dBm", "-19.00 dBm"}, // 设备自带单位，别补成 "-19.00 dBm dBm"
		{"-14", "-14.00 dBm"},
		{"-14.58", "-14.58 dBm"},
		{"2.5", "2.50 dBm"},
		{"-21.5 ", "-21.50 dBm"}, // 前后空白
		{"", ""},                 // 没值就是空
		// 解析不出数字的原样返回，**不能**当成 0 —— 那会看起来像真实读数
		{"N/A", "N/A"},
		{"--", "--"},
		{"", ""},
	}
	for _, c := range cases {
		if got := withDbm(c.in); got != c.want {
			t.Errorf("withDbm(%q) = %q，要的是 %q", c.in, got, c.want)
		}
	}
}

// 【真机回归】中兴 ZXHN F610GV9：同一台猫上有多个带 power 的参数，
// 私有的百分比/原始值（385、16687）把真正的光功率顶掉了。
// 修复后必须选中语义最像标准光功率的那个，且跟库里返回顺序无关。
func TestOpticalOverviewPrefersRealReadingOverVendorPrivateValue(t *testing.T) {
	// 顺序刻意让私有值排前面 —— 旧的"第一个命中就赢"就是这么错的
	params := []store.Param{
		mkParam("InternetGatewayDevice.Optical.Interface.1.TxPowerPercent", "385"),
		mkParam("InternetGatewayDevice.Optical.Interface.1.TxPowerRaw", "16687"),
		mkParam("InternetGatewayDevice.Optical.Interface.1.RxPowerPercent", "23"),
		mkParam("InternetGatewayDevice.Optical.Interface.1.RxPower", "-23.4"),
		mkParam("InternetGatewayDevice.Optical.Interface.1.TxPower", "2.1"),
	}
	got := OpticalOverview(params)
	if got.Rx != "-23.40 dBm" || got.Tx != "2.10 dBm" {
		t.Errorf("没选中真正的光功率：Rx %q / Tx %q（来源 %q）", got.Rx, got.Tx, got.SourceName)
	}
	if got.SourceName != "InternetGatewayDevice.Optical.Interface.1.RxPower" {
		t.Errorf("来源参数标错了：%q", got.SourceName)
	}

	// 同样的参数换个顺序，结果必须一致（旧实现会随查询顺序漂移）
	shuffled := []store.Param{params[3], params[1], params[4], params[0], params[2]}
	if again := OpticalOverview(shuffled); again.Rx != got.Rx || again.Tx != got.Tx {
		t.Errorf("取值随参数顺序变了：%q/%q vs %q/%q", again.Rx, again.Tx, got.Rx, got.Tx)
	}
}

// 值明显不可能是光功率（百分比、原始 ADC 值）时宁可不算，
// 也不能把 385 dBm / 16687 dBm 这种物理上不存在的数显示出来。
func TestOpticalOverviewRejectsImplausibleValues(t *testing.T) {
	got := OpticalOverview([]store.Param{
		mkParam("InternetGatewayDevice.Optical.Interface.1.RxPower", "385"),
		mkParam("InternetGatewayDevice.Optical.Interface.1.TxPower", "16687"),
	})
	if got.Has() {
		t.Errorf("超出光模块物理范围的值不该显示：%+v", got)
	}
	// 边界：正常范围要保留（-40 ~ +10 dBm 是真实读数区）
	for _, v := range []string{"-40", "-3.5", "10", "0"} {
		g := OpticalOverview([]store.Param{mkParam("InternetGatewayDevice.Optical.Interface.1.RxPower", v)})
		if !g.Has() {
			t.Errorf("%s dBm 是正常读数，不该被过滤", v)
		}
	}
}

// 同一方向两个候选分值相同时，取更新时间更晚的那个（重采过的才准）。
func TestOpticalOverviewPrefersNewerOnTie(t *testing.T) {
	old := mkParam("InternetGatewayDevice.Optical.Interface.1.RxPower", "-23.4")
	old.UpdatedAt = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	fresh := mkParam("InternetGatewayDevice.Optical.Interface.1.OpticalRxPower", "-24.1")
	fresh.UpdatedAt = time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)

	got := OpticalOverview([]store.Param{old, fresh})
	if got.Rx != "-24.10 dBm" {
		t.Errorf("分值相同时应取更新的读数，得到 %q", got.Rx)
	}
	// 反过来排也一样
	if again := OpticalOverview([]store.Param{fresh, old}); again.Rx != got.Rx {
		t.Errorf("结果随顺序变了：%q vs %q", again.Rx, got.Rx)
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
	if !strings.Contains(body, "-21.50 dBm") || !strings.Contains(body, "1.80 dBm") {
		t.Error("设备页应显示已采集到的收光/发光")
	}

	// 点按钮：入队后跳回设备页
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, httptest.NewRequest("POST", "/devices/1/optical", nil))
	if w2.Code != http.StatusSeeOther {
		t.Errorf("POST 应 303 跳回设备页，得到 %d", w2.Code)
	}
}
