package web

import (
	"path/filepath"
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 真机参数集（联通版 V271-20，PON 接入，V5R023C10S200，2026-09-30 实测）。
//
// 同一台设备上有两组读数：
//   - `…OpticalTransceiver.*`：光模块寄存器**原始值**（SFF-8472 编码）
//   - `…X_GponInterafceConfig.*`：整数近似值
//
// 设备自己页面上显示的是前者换算出来的值：
// 收光 -15.95 dBm / 发光 0.00 dBm / 温度 43.0 ℃ / 电压 3.226 V / 偏流 29.0 mA。
// 映射表要让面板显示成一样的东西（原始值优先级更高）。
func realV271Params() []store.Param {
	base := "InternetGatewayDevice.WANDevice.1."
	tr := base + "X_CU_WANEdgeONTPONInterfaceConfig.OpticalTransceiver."
	gp := base + "X_GponInterafceConfig."
	mk := func(name, value string) store.Param { return store.Param{Name: name, Value: value} }
	return []store.Param{
		// 光模块寄存器原始值
		mk(tr+"RXPower", "254"),
		mk(tr+"TXPower", "10000"),
		mk(tr+"Temperature", "11008"),
		mk(tr+"Vcc", "32260"),
		mk(tr+"TXBias", "14500"),
		// 整数近似值
		mk(gp+"RXPower", "-15"),
		mk(gp+"TXPower", "0"),
		mk(gp+"TransceiverTemperature", "43"),
		mk(gp+"SupplyVoltage", "3226"),
		mk(gp+"BiasCurrent", "29"),
		// 干扰项：无线发射功率、私有百分比、状态
		mk("InternetGatewayDevice.LANDevice.1.WiFi.X_HW_Txpower", "30"),
		mk(tr+"RXPowerPercent", "23"),
		mk(tr+"TXPowerRaw", "16687"),
		mk(base+"X_CU_WANEdgeONTPONInterfaceConfig.Status", "Up"),
	}
}

func openAliases(t *testing.T) []store.ParamAlias {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	al, err := st.EnabledAliases()
	if err != nil {
		t.Fatal(err)
	}
	if len(al) == 0 {
		t.Fatal("库里没有映射（迁移没种上？）")
	}
	return al
}

// 真机（联通版 V271-20）：面板显示的数该与设备自己页面完全一致。
func TestResolvePanelFieldsOnRealV271(t *testing.T) {
	fields := ResolvePanelFields(openAliases(t), realV271Params())
	want := map[string]string{
		"rx_power":     "-15.95 dBm", // 254 × 0.1 µW
		"tx_power":     "0.00 dBm",   // 10000 × 0.1 µW
		"temperature":  "43.0 ℃",     // 11008 ÷ 256
		"voltage":      "3.226 V",    // 32260 × 0.1 mV
		"bias_current": "29.00 mA",   // 14500 × 2 µA
	}
	for _, f := range fields {
		w, ok := want[f.Field.Key]
		if !ok {
			t.Errorf("冒出了没预期的字段：%s", f.Field.Key)
			continue
		}
		if f.Value != w {
			t.Errorf("%s = %q，想要 %q（来源 %s = %s）", f.Field.Key, f.Value, w, f.Name, f.Raw)
		}
		if f.Name == "" {
			t.Errorf("%s 没有记下来源参数名（排查要用）", f.Field.Key)
		}
	}
	// 无线发射功率不能被当成发光功率
	for _, f := range fields {
		if f.Name == "InternetGatewayDevice.LANDevice.1.WiFi.X_HW_Txpower" {
			t.Errorf("%s 认到了无线的发射功率上", f.Field.Key)
		}
	}
}

// 只有整数近似值时（没有寄存器对象）：也能显示，精度就是整数级的。
func TestResolvePanelFieldsIntegerOnly(t *testing.T) {
	params := []store.Param{
		{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.RXPower", Value: "-15"},
		{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.TXPower", Value: "0"},
		{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.SupplyVoltage", Value: "3226"},
	}
	got := map[string]string{}
	for _, f := range ResolvePanelFields(openAliases(t), params) {
		if f.Value != "" {
			got[f.Field.Key] = f.Value
		}
	}
	if got["rx_power"] != "-15.00 dBm" || got["tx_power"] != "0.00 dBm" {
		t.Errorf("整数近似值没显示对：%+v", got)
	}
	if got["voltage"] != "3.226 V" {
		t.Errorf("毫伏换算不对：%+v", got)
	}
}

// 原始值是垃圾时（别家私有编码），不能硬套刻度显示离谱数字；
// 这时该退回能用的那条（整数近似值）。
func TestResolvePanelFieldsRejectsImplausibleRaw(t *testing.T) {
	params := []store.Param{
		// 解出来是 +1e6 dBm 量级的垃圾
		{Name: "InternetGatewayDevice.X.CU.OpticalTransceiver.RXPower", Value: "999999999"},
		// 同一台设备的整数读数
		{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.RXPower", Value: "-21"},
	}
	fields := ResolvePanelFields(openAliases(t), params)
	for _, f := range fields {
		if f.Field.Key != "rx_power" {
			continue
		}
		if f.Value != "-21.00 dBm" {
			t.Errorf("垃圾原始值该被挡掉、退回整数读数，实际 %q（来源 %s）", f.Value, f.Name)
		}
	}
}

// 没登记过的新机型（中兴那种 Optical.Interface.1.RxPower）：映射一条都不命中，
// 退回叶子名启发式，照样能显示。
func TestResolvePanelFieldsFallsBackToHeuristic(t *testing.T) {
	params := []store.Param{
		{Name: "InternetGatewayDevice.Optical.Interface.1.RxPower", Value: "-23.4"},
		{Name: "InternetGatewayDevice.Optical.Interface.1.TxPower", Value: "2.1"},
	}
	got := map[string]string{}
	for _, f := range ResolvePanelFields(openAliases(t), params) {
		if f.Value != "" {
			got[f.Field.Key] = f.Value
		}
	}
	if got["rx_power"] != "-23.40 dBm" || got["tx_power"] != "2.10 dBm" {
		t.Errorf("兜底启发式没生效：%+v", got)
	}
	// 温度/电压/偏流没有启发式（叶子名太通用，乱认还不如不显示）
	if _, ok := got["temperature"]; ok {
		t.Errorf("没有映射时不该凭叶子名猜温度：%+v", got)
	}
}

func TestDecodeRaw(t *testing.T) {
	cases := []struct {
		kind, raw string
		want      float64
		ok        bool
	}{
		{"identity", "-15", -15, true},
		{"identity", "-15.95 dBm", -15.95, true},
		{"dbm_01uw", "254", -15.95, true},
		{"dbm_01uw", "10000", 0, true},
		{"dbm_01uw", "0", 0, false}, // 0 µW 取对数无意义
		{"div256", "11008", 43, true},
		{"mv", "3226", 3.226, true},
		{"mv01", "32260", 3.226, true},
		{"ua2", "14500", 29, true},
		{"不认识", "1", 0, false},
		{"identity", "", 0, false},
		{"identity", "N/A", 0, false},
	}
	for _, c := range cases {
		got, ok := decodeRaw(c.kind, c.raw)
		if ok != c.ok {
			t.Errorf("decodeRaw(%q,%q) ok=%v，想要 %v", c.kind, c.raw, ok, c.ok)
			continue
		}
		// 取对数那条会有点小数尾巴（254 → -15.9517），显示时再按字段小数位四舍五入
		if ok && (got < c.want-0.01 || got > c.want+0.01) {
			t.Errorf("decodeRaw(%q,%q) = %v，想要 %v", c.kind, c.raw, got, c.want)
		}
	}
}

// 面板字段的定义要保持自洽（key 不重复、区间方向对、单位不空）。
func TestPanelFieldsSane(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range panelFields {
		if f.Key == "" || f.Label == "" || f.Unit == "" {
			t.Errorf("字段定义不完整：%+v", f)
		}
		if seen[f.Key] {
			t.Errorf("字段 key 重复：%s", f.Key)
		}
		seen[f.Key] = true
		if f.Max <= f.Min {
			t.Errorf("%s 的区间反了：%v ~ %v", f.Key, f.Min, f.Max)
		}
		if f.Decimals < 0 || f.Decimals > 4 {
			t.Errorf("%s 的小数位离谱：%d", f.Key, f.Decimals)
		}
	}
}
