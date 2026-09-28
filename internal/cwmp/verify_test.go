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

	t.Run("全部一致", func(t *testing.T) {
		got := []ParamValue{
			{Name: "Device.WiFi.Radio.5.RadioEnabled", Value: "1"},
			{Name: "Device.WiFi.SSID.1.SSID", Value: "NewName"},
		}
		if p := verifyReadBack(expect, got); len(p) != 0 {
			t.Errorf("应无问题，实际 %v", p)
		}
	})

	t.Run("布尔值宽松比较", func(t *testing.T) {
		got := []ParamValue{
			{Name: "Device.WiFi.Radio.5.RadioEnabled", Value: "true"},
			{Name: "Device.WiFi.SSID.1.SSID", Value: "NewName"},
		}
		if p := verifyReadBack(expect, got); len(p) != 0 {
			t.Errorf("true/1 应视为一致，实际 %v", p)
		}
	})

	t.Run("值没生效", func(t *testing.T) {
		got := []ParamValue{
			{Name: "Device.WiFi.Radio.5.RadioEnabled", Value: "0"},
			{Name: "Device.WiFi.SSID.1.SSID", Value: "NewName"},
		}
		p := verifyReadBack(expect, got)
		if len(p) != 1 {
			t.Fatalf("应报 1 个不一致，实际 %v", p)
		}
	})

	t.Run("读回里根本没这个参数", func(t *testing.T) {
		got := []ParamValue{{Name: "Device.WiFi.SSID.1.SSID", Value: "NewName"}}
		p := verifyReadBack(expect, got)
		if len(p) != 1 {
			t.Fatalf("应报 1 个不一致，实际 %v", p)
		}
	})

	t.Run("名字大小写不敏感", func(t *testing.T) {
		got := []ParamValue{
			{Name: "device.wifi.radio.5.radioenabled", Value: "0"},
			{Name: "Device.WiFi.SSID.1.SSID", Value: "NewName"},
		}
		if p := verifyReadBack(expect, got); len(p) != 1 {
			t.Errorf("应报 1 个不一致，实际 %v", p)
		}
	})
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
