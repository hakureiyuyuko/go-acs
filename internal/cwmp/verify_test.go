package cwmp

import "testing"

// verifyReadBack 是「读回核对」的判定逻辑。
//
// 真机背景：华为 HN8145X6N 对 WLANConfiguration.5.RadioEnabled 的写入返回了
// SetParameterValuesResponse Status=0（表示接受），但读回来值根本没变。
// 没有这道核对，界面会显示「设置成功」，运维会以为 WiFi 已经开了。
func TestVerifyReadBack(t *testing.T) {
	expect := []ParamValue{
		{Name: "Device.WiFi.Radio.5.RadioEnabled", Value: "1", Type: "boolean"},
		{Name: "Device.WiFi.SSID.1.SSID", Value: "NewName", Type: "string"},
	}
	// 写入前：RadioEnabled 是 0，SSID 是 OldName
	prev := []ParamValue{
		{Name: "Device.WiFi.Radio.5.RadioEnabled", Value: "0"},
		{Name: "Device.WiFi.SSID.1.SSID", Value: "OldName"},
	}

	t.Run("全部一致", func(t *testing.T) {
		got := []ParamValue{
			{Name: "Device.WiFi.Radio.5.RadioEnabled", Value: "1"},
			{Name: "Device.WiFi.SSID.1.SSID", Value: "NewName"},
		}
		p, u := verifyReadBack(expect, prev, got)
		if len(p) != 0 || len(u) != 0 {
			t.Errorf("应无问题，实际 problems=%v unverifiable=%v", p, u)
		}
	})

	t.Run("布尔值宽松比较", func(t *testing.T) {
		got := []ParamValue{
			{Name: "Device.WiFi.Radio.5.RadioEnabled", Value: "true"},
			{Name: "Device.WiFi.SSID.1.SSID", Value: "NewName"},
		}
		p, _ := verifyReadBack(expect, prev, got)
		if len(p) != 0 {
			t.Errorf("true/1 应视为一致，实际 %v", p)
		}
	})

	t.Run("值没生效：写前非空、写后没变", func(t *testing.T) {
		got := []ParamValue{
			{Name: "Device.WiFi.Radio.5.RadioEnabled", Value: "0"},
			{Name: "Device.WiFi.SSID.1.SSID", Value: "NewName"},
		}
		p, u := verifyReadBack(expect, prev, got)
		if len(p) != 1 {
			t.Fatalf("应报 1 个未生效，实际 problems=%v unverifiable=%v", p, u)
		}
		if len(u) != 0 {
			t.Errorf("不该有无法核对项: %v", u)
		}
	})

	t.Run("读回里根本没这个参数", func(t *testing.T) {
		got := []ParamValue{{Name: "Device.WiFi.SSID.1.SSID", Value: "NewName"}}
		p, _ := verifyReadBack(expect, prev, got)
		if len(p) != 1 {
			t.Fatalf("应报 1 个未生效，实际 %v", p)
		}
	})

	t.Run("名字大小写不敏感", func(t *testing.T) {
		got := []ParamValue{
			{Name: "device.wifi.radio.5.radioenabled", Value: "0"},
			{Name: "Device.WiFi.SSID.1.SSID", Value: "NewName"},
		}
		p, _ := verifyReadBack(expect, prev, got)
		if len(p) != 1 {
			t.Errorf("应报 1 个未生效，实际 %v", p)
		}
	})
}

// 关键：**写only 参数（如 WiFi 密码）不能被当成「未生效」**。
//
// 真机背景：往 WLANConfiguration.5.PreSharedKey.1.KeyPassphrase 写密码，
// 设备回 Status=0 但读回永远是空串（能改不能读）。如果一律判失败，
// 就会把一次成功的改密码报成失败。
func TestVerifyReadBackWriteOnlyParam(t *testing.T) {
	expect := []ParamValue{
		{Name: "...PreSharedKey.1.KeyPassphrase", Value: "secret123", Type: "string"},
	}
	// 写入前就是空的（设备从来没回读过它）
	prev := []ParamValue{{Name: "...PreSharedKey.1.KeyPassphrase", Value: ""}}

	t.Run("读回是空串", func(t *testing.T) {
		got := []ParamValue{{Name: "...PreSharedKey.1.KeyPassphrase", Value: ""}}
		p, u := verifyReadBack(expect, prev, got)
		if len(p) != 0 {
			t.Errorf("不该判失败（设备本来就不回读），实际 %v", p)
		}
		if len(u) != 1 {
			t.Errorf("应报无法核对，实际 %v", u)
		}
	})

	t.Run("读回压根没带这个参数", func(t *testing.T) {
		p, u := verifyReadBack(expect, prev, nil)
		if len(p) != 0 || len(u) != 1 {
			t.Errorf("应算无法核对，实际 problems=%v unverifiable=%v", p, u)
		}
	})

	t.Run("写入前非空、写后空了 → 算未生效", func(t *testing.T) {
		prev2 := []ParamValue{{Name: "...PreSharedKey.1.KeyPassphrase", Value: "oldsecret"}}
		got := []ParamValue{{Name: "...PreSharedKey.1.KeyPassphrase", Value: ""}}
		p, u := verifyReadBack(expect, prev2, got)
		if len(p) != 1 || len(u) != 0 {
			t.Errorf("应算未生效，实际 problems=%v unverifiable=%v", p, u)
		}
	})
}

// 写入无线参数后要能识别出来、并重采无线概况。
// 真机上因此漏过：只回读了改动的那一个参数，界面上的状态/信道
// 停在写入前，导致“5GHz 已经起来了却显示 Disabled”。
func TestContainsWiFiParam(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"InternetGatewayDevice.LANDevice.1.WLANConfiguration.5.RadioEnabled", true},
		{"Device.WiFi.Radio.1.Enable", true},
		{"InternetGatewayDevice.DeviceInfo.SoftwareVersion", false},
		{"Device.ManagementServer.URL", false},
	}
	for _, c := range cases {
		got := containsWiFiParam([]ParamValue{{Name: c.name}})
		if got != c.want {
			t.Errorf("containsWiFiParam(%q) = %v，期望 %v", c.name, got, c.want)
		}
	}
	if containsWiFiParam(nil) {
		t.Error("空列表不该算无线参数")
	}
}

func TestSameValue(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1", "1", true},
		{"1", "true", true},
		{"0", "false", true},
		{"1", "0", false},
		{"AES", "AES", true},
		{"AES", "TKIP", false},
		// 非布尔值不能被 normBool 的“空”误判成相等
		{"AES", "", false},
		{"", "0", false},
	}
	for _, c := range cases {
		if got := sameValue(c.a, c.b); got != c.want {
			t.Errorf("sameValue(%q, %q) = %v，期望 %v", c.a, c.b, got, c.want)
		}
	}
}
