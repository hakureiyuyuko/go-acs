package store

import (
	"path/filepath"
	"testing"
)

// SummaryParams 一次查出「无线 + 光功率」两类参数，并各自分类。
// 分类错了会直接影响列表页那两列与无线概况，所以这里钉住口径。
func TestSummaryParamsSplitsWifiAndOptical(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, _, err := st.UpsertDevice(&Device{OUI: "001122", ProductClass: "R", SerialNumber: "S1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertParams(id, []Param{
		// 无线
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID", Value: "Net"},
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.Channel", Value: "6"},
		// 光功率（真机联通版 V271-20 的私有命名；大小写也要认）
		{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.RXPower", Value: "-15"},
		{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.TXPower", Value: "2"},
		{Name: "InternetGatewayDevice.WANDevice.1.X_CU_WANEdgeONTPONInterfaceConfig.OpticalTransceiver.RXPower", Value: "254"},
		// 无线的发射功率：名字也以 txpower 结尾，会被扫进来，但分类仍算无线那一类
		{Name: "InternetGatewayDevice.LANDevice.1.WiFi.X_HW_Txpower", Value: "30"},
		// 跟两类都无关的
		{Name: "InternetGatewayDevice.DeviceInfo.UpTime", Value: "3600"},
	}, "getvalues"); err != nil {
		t.Fatal(err)
	}

	wifi, optical, err := st.SummaryParams()
	if err != nil {
		t.Fatal(err)
	}
	if len(wifi[id]) == 0 {
		t.Errorf("无线参数没查出来：%+v", wifi)
	}
	if len(optical[id]) != 4 {
		t.Fatalf("光功率类应该 4 条（含无线 X_HW_Txpower），实际 %d：%+v", len(optical[id]), optical[id])
	}
	names := map[string]bool{}
	for _, p := range optical[id] {
		names[p.Name] = true
	}
	if !names["InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.RXPower"] {
		t.Error("私有命名的收光参数没被扫到")
	}
	if names["InternetGatewayDevice.DeviceInfo.UpTime"] {
		t.Error("无关参数被归进了光功率")
	}

	// WifiParams 只是它的一半（老调用点还在用）
	only, err := st.WifiParams()
	if err != nil {
		t.Fatal(err)
	}
	if len(only[id]) != len(wifi[id]) {
		t.Errorf("WifiParams 与 SummaryParams 的无线部分该一致：%d vs %d", len(only[id]), len(wifi[id]))
	}
}

// 光功率那一类的名字后缀（跟 cwmp 的枚举清单、web 的判定口径一致）。
func TestIsOpticalName(t *testing.T) {
	yes := []string{
		"InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.RXPower",
		"InternetGatewayDevice.Optical.Interface.1.OpticalRxPower",
		"Device.Optical.Interface.1.OpticalTxPower",
		"InternetGatewayDevice.LANDevice.1.WiFi.X_HW_Txpower", // 无线，但名字确实以 txpower 结尾
		"InternetGatewayDevice.X.RxPowerDbm",
	}
	no := []string{
		"InternetGatewayDevice.DeviceInfo.UpTime",
		"InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.BiasCurrent",
		"InternetGatewayDevice.Optical.Interface.1.TxPowerPercent", // 私有百分比：不扫进来（后缀对不上）
		"InternetGatewayDevice.X.PowerThreshold",
	}
	for _, n := range yes {
		if !isOpticalName(n) {
			t.Errorf("%q 该被认成光功率类", n)
		}
	}
	for _, n := range no {
		if isOpticalName(n) {
			t.Errorf("%q 不该被认成光功率类", n)
		}
	}
}
