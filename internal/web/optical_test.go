package web

import (
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

func TestHostOpticalFrom(t *testing.T) {
	p := func(name, value string) store.Param {
		return store.Param{Name: name, Value: value}
	}

	cases := []struct {
		what   string
		params []store.Param
		rx, tx string
		rxName string
	}{
		{
			what: "TR-098 常规位置（WANPONInterfaceConfig）",
			params: []store.Param{
				p("InternetGatewayDevice.WANDevice.1.WANPONInterfaceConfig.OpticalRxPower", "-21.5"),
				p("InternetGatewayDevice.WANDevice.1.WANPONInterfaceConfig.OpticalTxPower", "1.8"),
			},
			rx: "-21.5 dBm", tx: "1.8 dBm",
			rxName: "InternetGatewayDevice.WANDevice.1.WANPONInterfaceConfig.OpticalRxPower",
		},
		{
			what: "华为私有命名（X_HW_RxPower / X_HW_TxPower）",
			params: []store.Param{
				p("InternetGatewayDevice.Optical.Interface.1.X_HW_RxPower", "-19.42"),
				p("InternetGatewayDevice.Optical.Interface.1.X_HW_TxPower", "2.35"),
			},
			rx: "-19.42 dBm", tx: "2.35 dBm",
		},
		{
			what: "同方向多个候选：优先光口路径，且不受参数表顺序影响",
			params: []store.Param{
				p("InternetGatewayDevice.Optical.RxPowerRef", "-25.0"), // 先遇到，但不是光口
				p("InternetGatewayDevice.WANDevice.1.WANPONInterfaceConfig.OpticalRxPower", "-21.5"),
			},
			rx: "-21.5 dBm", tx: "",
		},
		{
			what: "无线的发射功率不能被当成发光功率",
			params: []store.Param{
				p("InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.TransmitPower", "200"),
				p("InternetGatewayDevice.LANDevice.1.WiFi.X_HW_Txpower", "30"),
				p("InternetGatewayDevice.X_HW_APDevice.1.TransmitPower", "100,100"),
			},
			rx: "", tx: "",
		},
		{
			what: "FTTR 子光猫的功率不算主机头上（它属于子设备那一行）",
			params: []store.Param{
				p("InternetGatewayDevice.X_HW_APDevice.1.X_HW_RxPower", "-19.0"),
			},
			rx: "", tx: "",
		},
		{
			what: "厂家私有的百分比 / 原始值不能当功率显示",
			params: []store.Param{
				p("InternetGatewayDevice.Optical.Interface.1.RxPowerPercent", "23"),
				p("InternetGatewayDevice.Optical.Interface.1.TxPowerRaw", "16687"),
			},
			rx: "", tx: "",
		},
		{
			what: "真实的收光/发光与诱饵同时存在时，选中真值",
			params: []store.Param{
				p("InternetGatewayDevice.Optical.Interface.1.RxPowerPercent", "23"),
				p("InternetGatewayDevice.Optical.Interface.1.RxPower", "-23.4"),
				p("InternetGatewayDevice.Optical.Interface.1.TxPowerRaw", "16687"),
				p("InternetGatewayDevice.Optical.Interface.1.TxPower", "2.1"),
			},
			rx: "-23.4 dBm", tx: "2.1 dBm",
		},
		{
			what: "设备回了带单位的原值也能规整",
			params: []store.Param{
				p("InternetGatewayDevice.Optical.Interface.1.RxPower", "-23.4 dBm"),
			},
			rx: "-23.4 dBm",
		},
		{
			what: "非数字（N/A / 空）不显示",
			params: []store.Param{
				p("InternetGatewayDevice.Optical.Interface.1.RxPower", "N/A"),
				p("InternetGatewayDevice.Optical.Interface.1.TxPower", ""),
			},
			rx: "", tx: "",
		},
		{
			what: "压根没有光功率参数（这台设备就是不报）",
			params: []store.Param{
				p("InternetGatewayDevice.DeviceInfo.UpTime", "3600"),
			},
			rx: "", tx: "",
		},
	}

	for _, c := range cases {
		got := hostOpticalFrom(c.params)
		if got.Rx != c.rx || got.Tx != c.tx {
			t.Errorf("%s：得到 Rx=%q Tx=%q，想要 Rx=%q Tx=%q", c.what, got.Rx, got.Tx, c.rx, c.tx)
		}
		if c.rxName != "" && got.RxName != c.rxName {
			t.Errorf("%s：读数来源记错了：%q（想要 %q）", c.what, got.RxName, c.rxName)
		}
		if c.rx != "" && got.RxName == "" {
			t.Errorf("%s：有读数却没记来源参数", c.what)
		}
	}
}

func TestOpticalValuePlausible(t *testing.T) {
	ok := map[string]bool{
		"-21.5":     true,
		"2.1":       true,
		"-40":       true,
		"10":        true,
		"-23.4 dBm": true,
		"385":       false, // 私有百分比
		"16687":     false, // 原始 ADC 值
		"-100":      false, // 语音 Tone 那种量级
		"":          false,
		"-":         false,
		"N/A":       false,
		"abc":       false,
	}
	for v, want := range ok {
		if got := opticalValuePlausible(v); got != want {
			t.Errorf("opticalValuePlausible(%q) = %v，想要 %v", v, got, want)
		}
	}
}
