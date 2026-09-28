package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// cliParams 造一份贴近真机的无线参数：
// 主机 LANDevice.1.WLANConfiguration（1=2.4G 两台上网设备 + 一条残留空行、5=5G 一台），
// 子设备 X_HW_APDevice.1（2.4G 两台、5G 一台）和 X_HW_APDevice.4（没有终端）。
func cliParams() []store.Param {
	now := time.Now()
	mk := func(name, val string) store.Param {
		return store.Param{Name: name, Value: val, ValueType: "string", Source: "getvalues", UpdatedAt: now}
	}
	var ps []store.Param
	add := func(name, val string) { ps = append(ps, mk(name, val)) }

	h := "InternetGatewayDevice.LANDevice.1.WLANConfiguration."
	add(h+"1.SSID", "HomeWiFi")
	add(h+"1.X_HW_RFBand", "2.4GHz")
	add(h+"1.Channel", "6")
	add(h+"1.AssociatedDeviceNumberOfEntries", "2")
	add(h+"1.AssociatedDevice.1.AssociatedDeviceMACAddress", "02:00:00:00:00:B1")
	add(h+"1.AssociatedDevice.1.AssociatedDeviceIPAddress", "192.168.1.11")
	add(h+"1.AssociatedDevice.1.RSSI", "-41")
	add(h+"1.AssociatedDevice.1.SNR", "43")
	add(h+"1.AssociatedDevice.1.RxRate", "72")
	add(h+"1.AssociatedDevice.1.TxRate", "65")
	add(h+"1.AssociatedDevice.1.Uptime", "3600")
	add(h+"1.AssociatedDevice.2.AssociatedDeviceMACAddress", "02:00:00:00:00:B2")
	add(h+"1.AssociatedDevice.2.AssociatedDeviceIPAddress", "192.168.1.12")
	// 残留行：条目数说 2 台，表里却有第 3 行 —— 不能当成一台终端
	add(h+"1.AssociatedDevice.3.AssociatedDeviceMACAddress", "02:00:00:00:00:FF")
	add(h+"1.AssociatedDevice.3.AssociatedDeviceIPAddress", "0.0.0.0")

	add(h+"5.SSID", "HomeWiFi-5G")
	add(h+"5.X_HW_RFBand", "5GHz")
	add(h+"5.AssociatedDeviceNumberOfEntries", "1")
	add(h+"5.AssociatedDevice.1.AssociatedDeviceMACAddress", "02:00:00:00:00:C1")
	add(h+"5.AssociatedDevice.1.AssociatedDeviceIPAddress", "192.168.1.21")

	sub := func(inst string, n24, n5 int) {
		base := "InternetGatewayDevice.X_HW_APDevice." + inst + ".WLANConfiguration."
		add(base+"1.SSID", "SubWiFi-"+inst)
		add(base+"1.X_HW_RFBand", "2.4G")
		add(base+"1.AssociatedDeviceNumberOfEntries", itoa(n24))
		add(base+"2.SSID", "SubWiFi-"+inst+"-5G")
		add(base+"2.X_HW_RFBand", "5G")
		add(base+"2.AssociatedDeviceNumberOfEntries", itoa(n5))
		for k := 1; k <= n24; k++ {
			p := base + "1.AssociatedDevice." + itoa(k) + "."
			add(p+"AssociatedDeviceMACAddress", "02:00:0"+inst+":00:00:0"+itoa(k))
			add(p+"AssociatedDeviceIPAddress", "10.0."+inst+"."+itoa(k))
		}
		for k := 1; k <= n5; k++ {
			p := base + "2.AssociatedDevice." + itoa(k) + "."
			add(p+"AssociatedDeviceMACAddress", "02:00:0"+inst+":00:01:0"+itoa(k))
			add(p+"AssociatedDeviceIPAddress", "10.0."+inst+".1"+itoa(k))
		}
	}
	sub("1", 2, 1)
	sub("4", 0, 0)
	return ps
}

func itoa(n int) string {
	return string(rune('0' + n)) // 用例里只有 0..9，够用
}

func TestBuildClientTree(t *testing.T) {
	nodes := []FttrNode{{Instance: 1, Model: "K251e"}, {Instance: 4, Model: "K251-20"}}
	tree := BuildClientTree(cliParams(), nodes)

	// 主机：2.4G 2 台、5G 1 台
	if len(tree.Host) != 2 {
		t.Fatalf("主机应有 2 个 WLAN 分组，实际 %d", len(tree.Host))
	}
	if got := len(tree.Host[0].Clients); got != 2 {
		t.Errorf("主机 2.4G 应有 2 台，实际 %d", got)
	}
	if tree.Host[0].Band != "2.4G" || tree.Host[0].SSID != "HomeWiFi" {
		t.Errorf("主机 2.4G 分组解析错：%+v", tree.Host[0])
	}
	// 残留行（条目数之外的）要被截掉，并且记下条数
	if tree.Host[0].Stale != 1 {
		t.Errorf("应当记下 1 条残留行，实际 %d", tree.Host[0].Stale)
	}
	for _, g := range tree.Groups() {
		for _, c := range g.Clients {
			if strings.Contains(c.MAC, "00:00:FF") {
				t.Errorf("残留行被当成了终端：%+v", c)
			}
		}
	}

	// 子机 1：2.4G 2 台 + 5G 1 台；子机 4：0 台
	if got := tree.SubsBy[1]; len(got) != 2 || len(got[0].Clients) != 2 || len(got[1].Clients) != 1 {
		t.Errorf("子机 1 的分组不对：%+v", got)
	}
	if tree.SubsBy[1][0].Owner != "子机 1（K251e）" {
		t.Errorf("子机归属文字不对：%q", tree.SubsBy[1][0].Owner)
	}
	if got := len(tree.SubsBy[4][0].Clients); got != 0 {
		t.Errorf("子机 4 应该没有终端，实际 %d", got)
	}

	// 合计：2.4G = 主机 2 + 子机 2；5G = 主机 1 + 子机 1
	if total, host, sub := tree.CountInBand("2.4GHz"); total != 4 || host != 2 || sub != 2 {
		t.Errorf("2.4G 合计错：total=%d host=%d sub=%d", total, host, sub)
	}
	if total, host, sub := tree.CountInBand("5G"); total != 2 || host != 1 || sub != 1 {
		t.Errorf("5G 合计错：total=%d host=%d sub=%d", total, host, sub)
	}
	if total, _, _ := tree.CountInBand(""); total != 0 {
		t.Error("没有频段时不该统计出终端")
	}

	// 弹窗内容是「主机 + 子机」两组，且顺序是主机在前
	gs := tree.GroupsInBand("2.4G")
	if len(gs) != 2 || gs[0].Owner != "主机" || gs[1].Owner != "子机 1（K251e）" {
		t.Errorf("2.4G 的弹窗分组不对（没终端的子机不该出现在弹窗里）：%+v", gs)
	}
	// 弹窗数据里也不该出现 0 台的分组
	_, subs := BuildClientViews(tree, nodes)
	if len(subs) != 2 || !subs[0].HasWlan || subs[0].Total != 3 || subs[1].Total != 0 {
		t.Errorf("子设备终端视图不对：%+v", subs)
	}
	if len(subs[1].Groups) != 0 {
		t.Errorf("0 台终端的子机不该有弹窗分组：%+v", subs[1].Groups)
	}
}

// 无线概览的终端列 = 主机 + 子光猫的合计；同一频段有多行时只在有 SSID 的那行加子机数。
func TestWireBandClients(t *testing.T) {
	nodes := []FttrNode{{Instance: 1, Model: "K251e"}, {Instance: 4, Model: "K251-20"}}
	tree := BuildClientTree(cliParams(), nodes)

	bands := []WifiBand{
		{Instance: 1, Band: "2.4GHz", SSID: "HomeWiFi", Clients: "2"},
		{Instance: 5, Band: "5GHz", SSID: "", Clients: ""}, // 真机上有一行是没 SSID 的空实例
		{Instance: 6, Band: "5GHz", SSID: "HomeWiFi-5G", Clients: "1"},
	}
	WireBandClients(bands, tree)

	if bands[0].ClientsAll != 4 || bands[0].SubClients != 2 {
		t.Errorf("2.4G 应为 2+2=4（子机 2）：%+v", bands[0])
	}
	if bands[0].ModalID != "climodal-24G" {
		t.Errorf("2.4G 的弹窗 id 不对：%q", bands[0].ModalID)
	}
	if bands[2].ClientsAll != 2 || bands[2].SubClients != 1 {
		t.Errorf("5G 应为 1+1=2，且只算在有 SSID 的那行：%+v", bands[2])
	}
	if bands[1].SubClients != 0 {
		t.Errorf("没 SSID 的那行不该重复计入子机数：%+v", bands[1])
	}
	if bands[1].ModalID == "" {
		t.Error("同一频段的两行都应该能点开该频段的终端列表")
	}
}

// 终端条目的次要信息（给弹窗里那行小字用）。
func TestClientInfoMeta(t *testing.T) {
	c := ClientInfo{MAC: "AA:BB:CC:DD:EE:FF", IP: "1.2.3.4", RSSI: "-41", SNR: "43",
		RxRate: "72", TxRate: "65", Bandwidth: "40MHz", Uptime: "3600"}
	got := c.Meta()
	for _, want := range []string{"-41 dBm", "SNR 43", "72/65 Mbps", "40MHz", "在线 1 小时 0 分"} {
		if !strings.Contains(got, want) {
			t.Errorf("次要信息里应含 %q：%q", want, got)
		}
	}
	// 只有 MAC/IP 的设备（不报 RSSI 等）不该拼出一串分隔符
	if got := (ClientInfo{MAC: "AA"}).Meta(); got != "" {
		t.Errorf("没有次要信息时应为空，实际 %q", got)
	}
}

// TR-181 Multi-AP：AccessPoint.{i}.AssociatedDevice.{k}.MACAddress / IPAddress
func TestBuildClientTreeTR181(t *testing.T) {
	now := time.Now()
	ps := []store.Param{
		{Name: "Device.WiFi.AccessPoint.1.SSID", Value: "Dev", UpdatedAt: now},
		{Name: "Device.WiFi.AccessPoint.1.AssociatedDeviceNumberOfEntries", Value: "1", UpdatedAt: now},
		{Name: "Device.WiFi.AccessPoint.1.AssociatedDevice.1.MACAddress", Value: "AA:BB:CC:00:00:01", UpdatedAt: now},
		{Name: "Device.WiFi.AccessPoint.1.AssociatedDevice.1.IPAddress", Value: "192.168.1.5", UpdatedAt: now},
	}
	tree := BuildClientTree(ps, nil)
	if len(tree.Host) != 1 || len(tree.Host[0].Clients) != 1 {
		t.Fatalf("TR-181 终端没解析出来：%+v", tree.Host)
	}
	if got := tree.Host[0].Clients[0]; got.MAC != "AA:BB:CC:00:00:01" || got.IP != "192.168.1.5" {
		t.Errorf("TR-181 终端内容不对：%+v", got)
	}
}

// 真机（华为 V271-20 主机）的终端字段是 X_HW_ 前缀的：RSSI / SNR / 速率 / 带宽 / 在线时长。
// IP 它压根不回（AssociatedDeviceIPAddress 是空串），界面上就显示 “-”，不能编。
func TestBuildClientTreeXHWAliases(t *testing.T) {
	now := time.Now()
	b := "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.AssociatedDevice.1."
	ps := []store.Param{
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID", Value: "W", UpdatedAt: now},
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.AssociatedDeviceNumberOfEntries", Value: "1", UpdatedAt: now},
		{Name: b + "AssociatedDeviceMACAddress", Value: "02:EF:44:2A:B8:24", UpdatedAt: now},
		{Name: b + "AssociatedDeviceIPAddress", Value: "", UpdatedAt: now},
		{Name: b + "X_HW_RSSI", Value: "-46", UpdatedAt: now},
		{Name: b + "X_HW_SNR", Value: "32", UpdatedAt: now},
		{Name: b + "X_HW_RxRate", Value: "117", UpdatedAt: now},
		{Name: b + "X_HW_TxRate", Value: "144", UpdatedAt: now},
		{Name: b + "X_HW_FrequencyWidth", Value: "20MHz", UpdatedAt: now},
		{Name: b + "X_HW_Uptime", Value: "1305861", UpdatedAt: now},
	}
	tree := BuildClientTree(ps, nil)
	if len(tree.Host) != 1 || len(tree.Host[0].Clients) != 1 {
		t.Fatalf("终端没解析出来：%+v", tree.Host)
	}
	c := tree.Host[0].Clients[0]
	if c.RSSI != "-46" || c.SNR != "32" || c.RxRate != "117" || c.TxRate != "144" || c.Bandwidth != "20MHz" {
		t.Errorf("X_HW_ 前缀的字段没认全：%+v", c)
	}
	if c.IP != "" {
		t.Errorf("设备没回 IP，不应该凭空编一个：%q", c.IP)
	}
	if !strings.Contains(c.Meta(), "-46 dBm") || !strings.Contains(c.Meta(), "117/144 Mbps") {
		t.Errorf("次要信息不对：%q", c.Meta())
	}
	if !strings.Contains(c.Meta(), "在线 15 天") {
		t.Errorf("在线时长（X_HW_Uptime）没认出来：%q", c.Meta())
	}
}

// 信号图标：优先用设备自报的质量，没有才用 RSSI 换算；两者都没有就不渲染（不编）。
func TestClientSignal(t *testing.T) {
	cases := []struct {
		name      string
		in        ClientInfo
		pct, bars int
		hasSignal bool
	}{
		{"设备自报质量优先", ClientInfo{Quality: "55", RSSI: "-62"}, 55, 2, true},
		{"没有质量就用 RSSI（-58 → 84%）", ClientInfo{RSSI: "-58"}, 84, 3, true},
		{"RSSI  -50 及以上算满格", ClientInfo{RSSI: "-41"}, 100, 4, true},
		{"RSSI -100 算 0", ClientInfo{RSSI: "-100"}, 0, 0, true},
		{"很弱也给一格（不像没连上）", ClientInfo{RSSI: "-95"}, 10, 1, true},
		{"质量超出范围要夹住", ClientInfo{Quality: "140"}, 100, 4, true},
		{"什么都没有就不显示图标", ClientInfo{SNR: "30"}, 0, 0, false},
	}
	for _, c := range cases {
		if got := c.in.SignalPct(); got != c.pct {
			t.Errorf("%s：百分比 = %d，期望 %d", c.name, got, c.pct)
		}
		if got := c.in.HasSignal(); got != c.hasSignal {
			t.Errorf("%s：HasSignal = %v，期望 %v", c.name, got, c.hasSignal)
		}
		if got := c.in.SignalBars(); got != c.bars {
			t.Errorf("%s：格数 = %d，期望 %d", c.name, got, c.bars)
		}
	}
	// 悬停提示要把原始数据带上（免得百分比看着像设备事实）
	c := ClientInfo{Quality: "55", RSSI: "-62", SNR: "30"}
	title := c.SignalTitle()
	for _, want := range []string{"信号 55%", "2/4 格", "设备自报质量 55", "RSSI -62 dBm", "SNR 30"} {
		if !strings.Contains(title, want) {
			t.Errorf("悬停提示里应含 %q：%q", want, title)
		}
	}
}

// 模板**必须完整渲染**：以前直接把模板写到 ResponseWriter，执行到一半出错时
// 会发出去半个页面、错误文本还混在 HTML 里（踩过：模板调了签名不对的方法）。
// 现在渲染到内存再输出，所以这里断言页尾标记在，且信号图标真的渲染出来了。
func TestDevicePageRendersCompletely(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer st.Close()
	id, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "RENDER"})
	if err != nil {
		t.Fatal(err)
	}
	b := "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1."
	ps := []store.Param{
		{Name: b + "SSID", Value: "W"},
		{Name: b + "X_HW_RFBand", Value: "2.4GHz"},
		{Name: b + "AssociatedDeviceNumberOfEntries", Value: "1"},
		{Name: b + "AssociatedDevice.1.AssociatedDeviceMACAddress", Value: "AA:BB:CC:DD:EE:FF"},
		{Name: b + "AssociatedDevice.1.AssociatedDeviceIPAddress", Value: "192.168.1.9"},
		{Name: b + "AssociatedDevice.1.RSSI", Value: "-58"},
	}
	if err := st.UpsertParams(id, ps, "getvalues"); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	if err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/devices/"+strconv.FormatInt(id, 10), nil))
	if rec.Code != 200 {
		t.Fatalf("详情页应 200，得到 %d：%s", rec.Code, rec.Body.String()[:200])
	}
	body := rec.Body.String()
	for _, want := range []string{"</html>", "sigbars", `data-sig="84"`, `data-bars="3"`, "84%"} {
		if !strings.Contains(body, want) {
			t.Errorf("页面里应含 %q（模板没渲染完？）", want)
		}
	}
	if strings.Contains(body, "invalid function signature") || strings.Contains(body, "template: ") {
		t.Errorf("页面里混进了模板错误：%s", body[len(body)-200:])
	}
}

// 主机名：优先终端行自己带的，其次是设备主机列表按 MAC 对出来的；都没有就是空（界面显示 N/A）。
func TestBuildClientTreeHostName(t *testing.T) {
	now := time.Now()
	b := "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1."
	ps := []store.Param{
		{Name: b + "SSID", Value: "W", UpdatedAt: now},
		{Name: b + "AssociatedDeviceNumberOfEntries", Value: "3", UpdatedAt: now},
		// 1 号：名字在终端行自己身上
		{Name: b + "AssociatedDevice.1.AssociatedDeviceMACAddress", Value: "AA:BB:CC:00:00:01", UpdatedAt: now},
		{Name: b + "AssociatedDevice.1.X_HW_AssociatedDevicedescriptions", Value: "Camera", UpdatedAt: now},
		// 2 号：名字只能从主机列表按 MAC 对出来（大小写不一致也要对上）
		{Name: b + "AssociatedDevice.2.AssociatedDeviceMACAddress", Value: "AA:BB:CC:00:00:02", UpdatedAt: now},
		// 3 号：两处都没有 → 空
		{Name: b + "AssociatedDevice.3.AssociatedDeviceMACAddress", Value: "AA:BB:CC:00:00:03", UpdatedAt: now},
		{Name: "InternetGatewayDevice.LANDevice.1.Hosts.Host.1.MACAddress", Value: "aa:bb:cc:00:00:02", UpdatedAt: now},
		{Name: "InternetGatewayDevice.LANDevice.1.Hosts.Host.1.HostName", Value: "Laptop", UpdatedAt: now},
		{Name: "InternetGatewayDevice.LANDevice.1.Hosts.HostNumberOfEntries", Value: "1", UpdatedAt: now},
	}
	tree := BuildClientTree(ps, nil)
	if len(tree.Host) != 1 {
		t.Fatalf("应该有 1 个分组：%+v", tree.Host)
	}
	got := map[string]string{}
	for _, c := range tree.Host[0].Clients {
		got[c.MAC] = c.HostName
	}
	want := map[string]string{
		"AA:BB:CC:00:00:01": "Camera",
		"AA:BB:CC:00:00:02": "Laptop",
		"AA:BB:CC:00:00:03": "",
	}
	for mac, name := range want {
		if got[mac] != name {
			t.Errorf("%s 的主机名 = %q，期望 %q", mac, got[mac], name)
		}
	}
}

// 子设备的终端也能借用「主机列表」里的名字（真机上主机列表只有子光猫，但别的设备会给全）。
func TestBuildClientTreeHostNameCrossTable(t *testing.T) {
	now := time.Now()
	ps := []store.Param{
		{Name: "InternetGatewayDevice.X_HW_APDevice.1.WLANConfiguration.1.SSID", Value: "S", UpdatedAt: now},
		{Name: "InternetGatewayDevice.X_HW_APDevice.1.WLANConfiguration.1.AssociatedDeviceNumberOfEntries", Value: "1", UpdatedAt: now},
		{Name: "InternetGatewayDevice.X_HW_APDevice.1.WLANConfiguration.1.AssociatedDevice.1.AssociatedDeviceMACAddress", Value: "DE:AD:BE:EF:00:01", UpdatedAt: now},
		{Name: "InternetGatewayDevice.LANDevice.1.Hosts.Host.1.MACAddress", Value: "de:ad:be:ef:00:01", UpdatedAt: now},
		{Name: "InternetGatewayDevice.LANDevice.1.Hosts.Host.1.HostName", Value: "TV", UpdatedAt: now},
	}
	tree := BuildClientTree(ps, []FttrNode{{Instance: 1, Model: "K251e"}})
	gs := tree.SubsBy[1]
	if len(gs) != 1 || len(gs[0].Clients) != 1 {
		t.Fatalf("子设备终端没解析出来：%+v", gs)
	}
	if got := gs[0].Clients[0].HostName; got != "TV" {
		t.Errorf("子设备终端的主机名 = %q，期望 TV（应能从主机列表按 MAC 借到）", got)
	}
}

// TR-181 的 Device.Hosts.Host.{i}.HostName 也要认。
func TestBuildClientTreeHostNameTR181(t *testing.T) {
	now := time.Now()
	ps := []store.Param{
		{Name: "Device.WiFi.AccessPoint.1.AssociatedDeviceNumberOfEntries", Value: "1", UpdatedAt: now},
		{Name: "Device.WiFi.AccessPoint.1.AssociatedDevice.1.MACAddress", Value: "AA:00:00:00:00:01", UpdatedAt: now},
		{Name: "Device.Hosts.Host.1.MACAddress", Value: "AA:00:00:00:00:01", UpdatedAt: now},
		{Name: "Device.Hosts.Host.1.HostName", Value: "Dev-A", UpdatedAt: now},
	}
	tree := BuildClientTree(ps, nil)
	if len(tree.Host) != 1 || len(tree.Host[0].Clients) != 1 {
		t.Fatalf("没解析出来：%+v", tree.Host)
	}
	if got := tree.Host[0].Clients[0].HostName; got != "Dev-A" {
		t.Errorf("TR-181 主机名 = %q，期望 Dev-A", got)
	}
}
