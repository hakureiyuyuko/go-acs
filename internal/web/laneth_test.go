package web

import (
	"testing"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

func ethParam(name, value string) store.Param {
	return store.Param{Name: name, Value: value, UpdatedAt: time.Now()}
}

// 真机（华为 V271-20 主机）的四个网口之一：2.5G 口，插着线，扛了 32.9 GB 上行。
func TestLanEthOverviewRealDevice(t *testing.T) {
	b := "InternetGatewayDevice.LANDevice.1.LANEthernetInterfaceConfig."
	params := []store.Param{
		ethParam(b+"1.Name", "eth0:1"),
		ethParam(b+"1.Status", "Up"),
		ethParam(b+"1.Enable", "1"),
		ethParam(b+"1.MaxBitRate", "2500"),
		ethParam(b+"1.DuplexMode", "Auto"),
		ethParam(b+"1.X_HW_Speed", "Auto_2500"),
		ethParam(b+"1.X_HW_DuplexMode", "Auto_Full"),
		ethParam(b+"1.MACAddress", "7C:39:85:83:B2:FA"),
		ethParam(b+"1.Stats.BytesSent", "35376428045"),
		ethParam(b+"1.Stats.BytesReceived", "199926599617"),
		// 另一个口没插线：MaxBitRate=Auto，只有 X_HW_Speed 能看出协商档次
		ethParam(b+"3.Name", "eth0:3"),
		ethParam(b+"3.Status", "NoLink"),
		ethParam(b+"3.MaxBitRate", "Auto"),
		ethParam(b+"3.X_HW_Speed", "Auto_10"),
		ethParam(b+"3.X_HW_DuplexMode", "Auto_Half"),
	}

	ports, ok := LanEthOverview(params)
	if !ok || len(ports) != 2 {
		t.Fatalf("该解析出 2 个网口，得到 ok=%v n=%d", ok, len(ports))
	}
	p := ports[0]
	if p.Name != "eth0:1" || !p.Up || p.MAC != "7C:39:85:83:B2:FA" {
		t.Errorf("第一个口解析不对：%+v", p)
	}
	if p.Rate != "2.5 Gbps" {
		t.Errorf("速率该是 2.5 Gbps（MaxBitRate=2500），得到 %q", p.Rate)
	}
	if p.Duplex != "自动协商（全双工）" {
		t.Errorf("双工该按 X_HW_DuplexMode 说人话，得到 %q", p.Duplex)
	}
	if p.Tx != "32.9 GB" || p.Rx != "186.2 GB" {
		t.Errorf("流量换算不对：↑%q ↓%q", p.Tx, p.Rx)
	}
	if p.RateRaw == "" || p.DuplexRaw == "" {
		t.Errorf("设备原文该留着做悬停：%+v", p)
	}

	q := ports[1]
	if q.Name != "eth0:3" || q.Up {
		t.Errorf("第二个口该是未连接：%+v", q)
	}
	if q.Rate != "自动协商（10 Mbps）" {
		t.Errorf("MaxBitRate=Auto 时该退回 X_HW_Speed，得到 %q", q.Rate)
	}
	if q.Duplex != "自动协商（半双工）" {
		t.Errorf("双工不对：%q", q.Duplex)
	}
	if n := lanEthUpCount(ports); n != 1 {
		t.Errorf("该数出 1 个已连接，得到 %d", n)
	}
}

// TR-181 的命名也要认；但 Lane 对象（Device.Ethernet.Link.{i}）不能混进来 ——
// 它也有 Status/MACAddress，串台就会凭空多出几行。
func TestLanEthOverviewTR181AndNoLinkObject(t *testing.T) {
	ports, ok := LanEthOverview([]store.Param{
		ethParam("Device.Ethernet.Interface.1.Name", "eth0"),
		ethParam("Device.Ethernet.Interface.1.Status", "Up"),
		ethParam("Device.Ethernet.Interface.1.MaxBitRate", "1000"),
		ethParam("Device.Ethernet.Interface.1.DuplexMode", "Full"),
	})
	if !ok || len(ports) != 1 {
		t.Fatalf("TR-181 该认出一个口，得到 ok=%v n=%d", ok, len(ports))
	}
	if ports[0].Rate != "1 Gbps" || ports[0].Duplex != "全双工" {
		t.Errorf("TR-181 解析不对：%+v", ports[0])
	}

	if _, ok := LanEthOverview([]store.Param{
		ethParam("Device.Ethernet.Link.1.Status", "Up"),
		ethParam("Device.Ethernet.Link.1.MACAddress", "AA:BB:CC:DD:EE:FF"),
	}); ok {
		t.Error("只有 Link 对象（没有 Interface）时不该当成网口表")
	}
}

// 没有网口对象的设备：整块不显示（不摆空表）。
func TestLanEthOverviewEmpty(t *testing.T) {
	if _, ok := LanEthOverview(nil); ok {
		t.Error("空参数不该说有网口")
	}
	if _, ok := LanEthOverview([]store.Param{
		ethParam("InternetGatewayDevice.LANDevice.1.Hosts.Host.1.HostName", "PC"),
	}); ok {
		t.Error("只有 Hosts 参数不该说有网口")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},     // 没报就是空（界面显示 -）
		{"N/A", ""},  // 设备写了非数字
		{"0", "0 B"}, // 真·零字节
		{"1023", "1023 B"},
		{"1536", "1.5 KB"},
		{"1048576", "1.0 MB"},
		{"35376428045", "32.9 GB"},
		{"199926599617", "186.2 GB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%q) = %q，想要 %q", c.in, got, c.want)
		}
	}
}

func TestMbpsHuman(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{10, "10 Mbps"}, {100, "100 Mbps"}, {1000, "1 Gbps"}, {2500, "2.5 Gbps"}, {10000, "10 Gbps"},
	}
	for _, c := range cases {
		if got := mbpsHuman(c.in); got != c.want {
			t.Errorf("mbpsHuman(%d) = %q，想要 %q", c.in, got, c.want)
		}
	}
}
