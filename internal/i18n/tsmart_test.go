package i18n

import "testing"

// 后端拼出来的字符串（任务结果、提示语、分组名）靠 TSmart 翻译：
// 先精确查表，再用占位符匹配，最后逐段替换。这里把几种典型形态钉住。
func TestTSmart(t *testing.T) {
	cases := map[string]string{
		"已采集 218 个参数": "Collected 218 parameter(s)",
		"设置成功":        "Set successfully",
		"设置成功；2 个参数设备未回读，无法核对：X：读回里没有这个参数": "Set successfully; 2 parameter(s) were not read back: X: not present in the read-back",
		"诊断进行中…":                        "Diagnostics in progress…",
		"子机 1（K251-20）":                 "Sub-device 1 (K251-20)",
		"主机 · SimWiFi · 2.4G":           "Host · SimWiFi · 2.4G",
		"在线 1 小时 0 分":                   "online 1h 0m",
		"信号 100%（4/4 格） · RSSI -41 dBm": "signal 100% (4/4 bars) · RSSI -41 dBm",
		"设备上报：repeater · 上行 DHCP":       "Device reports: repeater · uplink DHCP",
		"已删除「客厅光猫」（只删本地记录：参数 / 任务 / 上报历史）。设备若还配着本 ACS 地址，下次上报会重新纳管。": "Deleted “客厅光猫” (local records only: parameters / tasks / inform history). If it still points at this ACS it is onboarded again on the next report.",
		// 中文界面原样返回；认不出的也原样返回（宁可中文，也别显示错的东西）
		"已采集 3 个参数": "Collected 3 parameter(s)",
		"这句还没翻译":    "这句还没翻译",
	}
	for in, want := range cases {
		if got := TSmart(LangEN, in); got != want {
			t.Errorf("TSmart(%q) = %q，期望 %q", in, got, want)
		}
	}
	if got := TSmart(LangZH, "已采集 3 个参数"); got != "已采集 3 个参数" {
		t.Errorf("中文界面应原样返回，得到 %q", got)
	}
}
