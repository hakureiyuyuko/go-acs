package web

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 这一文件回答一个问题：**谁连在哪个 WLAN 上**。
//
// 数据来源是标准的无线关联终端表：
//
//	主机：<root>LANDevice.1.WLANConfiguration.{i}.AssociatedDevice.{k}.*
//	子机：<root>X_HW_APDevice.{inst}.WLANConfiguration.{j}.AssociatedDevice.{k}.*
//	     （TR-181 Multi-AP：<root>WiFi.DataElements.Network.Device.{inst}.…）
//
// 真机（华为 FTTR 主机 + 3 台子光猫）实测：主机自己的 WLAN 上一台终端都没有，
// 终端全挂子光猫上 —— 所以「这台设备连了多少终端」必须把子设备算进来，
// 并且要说清哪些连主机、哪些连哪台子机。

// hostNameRe 抠出设备「主机列表」里的一行：TR-098 的 LANDevice.1.Hosts.Host.{i}.x
// 与 TR-181 的 Hosts.Host.{i}.x 都以这段结尾。
//
// 这张表是**唯一**能拿到终端名的常用地方（关联终端表里通常没有名字）——
// 实测那台华为 FTTR 主机只列了 3 台子光猫（InterfaceType=PON），WiFi 终端一律没有名字，
// 界面上就诚实地显示 N/A。
var hostNameRe = regexp.MustCompile(`(?i)\.Hosts\.Host\.(\d+)\.(MACAddress|HostName)$`)

// isClientNameField 判断终端行上的字段是不是“名字”（各家写法不一）。
func isClientNameField(field string) bool {
	switch strings.ToLower(field) {
	case "hostname", "associateddevicehostname", "x_hw_hostname",
		"x_hw_associateddevicedescriptions", "x_hw_devicename", "devicename":
		return true
	}
	return false
}

// wlanPathRe 把参数名拆成「WLAN 实例前缀 + WLAN 实例号 + 剩余路径」。
//
// 只认 WLANConfiguration / AccessPoint 这两种容器（Radio/SSID 上不会有关联终端表）。
var wlanPathRe = regexp.MustCompile(`(?i)^(.*(?:WLANConfiguration|AccessPoint)\.(\d+))\.(.+)$`)

// subOwnerRe 从参数名里认出这是哪台 FTTR 子设备（主机没有这一段）。
var subOwnerRe = regexp.MustCompile(`(?i)(?:X_HW_APDevice|DataElements\.Network\.Device)\.(\d+)\.`)

// clientIdxRe 抠出关联终端表里的「实例号 + 字段名」。
var clientIdxRe = regexp.MustCompile(`(?i)^AssociatedDevice\.(\d+)\.(.+)$`)

// ClientInfo 是一台已连终端。
type ClientInfo struct {
	MAC string
	IP  string
	// HostName 是终端名（设备不一定知道它 —— 界面上拿不到就显示 N/A，不编）。
	// 来源两个：终端行自己的名字字段（如华为 X_HW_AssociatedDevicedescriptions），
	// 以及设备的主机列表 LANDevice.1.Hosts.Host.{i}.HostName（按 MAC 对）。
	HostName string
	RSSI     string
	SNR      string
	// Quality 是**设备自报**的信号质量（0..100，如华为的 X_HW_SingalQuality）。
	// 有它就用它 —— 各家对 RSSI 到「几格」的换算不一样，设备自己算的更可信。
	Quality   string
	RxRate    string
	TxRate    string
	Bandwidth string
	Uptime    string
}

// signalPct 算出要显示的信号百分比（0..100）；ok=false 表示信号未知（拿不到质量值也拿不到 RSSI）。
//
// 优先用设备自报的质量；没有再把 RSSI 按业界常见口径换算：
// -50 dBm 及以上算满格、-100 dBm 及以下算 0（中间线性）。
func (c ClientInfo) signalPct() (int, bool) {
	if v := strings.TrimSpace(c.Quality); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return clampPct(int(n + 0.5)), true
		}
	}
	if v := strings.TrimSpace(c.RSSI); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return clampPct(int((n+100)*2 + 0.5)), true
		}
	}
	return 0, false
}

// SignalPct / SignalBars / HasSignal 都是给模板直接调的方法 ——
// **必须只有一个返回值**：html/template 只认 (值, error) 两返回值，
// 写成 (值, bool) 会报 “invalid function signature”，而且错误发生在渲染中途，
// 页面会被截断（踩过：页尾混进一行 template 错误文本）。
func (c ClientInfo) SignalPct() int {
	pct, _ := c.signalPct()
	return pct
}

// SignalBars 返回信号有几格（0..4）—— 跟商用 ACS 那个小柱子图标一样四格。
func (c ClientInfo) SignalBars() int {
	pct, ok := c.signalPct()
	if !ok {
		return 0
	}
	bars := (pct + 12) / 25 // 四舍五入到 0..4
	if bars == 0 && pct > 0 {
		bars = 1 // 有一点信号就至少给一格，不然看着像“没连上”
	}
	if bars > 4 {
		bars = 4
	}
	return bars
}

// HasSignal 表示信号值拿得到（拿不到就不渲染那个小图标，不编）。
func (c ClientInfo) HasSignal() bool {
	_, ok := c.signalPct()
	return ok
}

// SignalTitle 是悬停提示：把原始数据也给人看。
func (c ClientInfo) SignalTitle() string {
	pct, ok := c.signalPct()
	if !ok {
		return "设备没有上报信号"
	}
	parts := []string{fmt.Sprintf("信号 %d%%（%d/4 格）", pct, c.SignalBars())}
	if v := strings.TrimSpace(c.Quality); v != "" {
		parts = append(parts, "设备自报质量 "+v)
	}
	if v := strings.TrimSpace(c.RSSI); v != "" {
		parts = append(parts, "RSSI "+v+" dBm")
	}
	if v := strings.TrimSpace(c.SNR); v != "" {
		parts = append(parts, "SNR "+v)
	}
	return strings.Join(parts, " · ")
}

// clampPct 把百分比夹到 0..100。
func clampPct(n int) int {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

// Meta 是终端条目下面那行灰色小字（把非空字段拼起来）。
func (c ClientInfo) Meta() string {
	parts := make([]string, 0, 6)
	if v := strings.TrimSpace(c.RSSI); v != "" {
		parts = append(parts, v+" dBm")
	}
	if v := strings.TrimSpace(c.SNR); v != "" {
		parts = append(parts, "SNR "+v)
	}
	if rx, tx := strings.TrimSpace(c.RxRate), strings.TrimSpace(c.TxRate); rx != "" || tx != "" {
		parts = append(parts, rx+"/"+tx+" Mbps")
	}
	if v := strings.TrimSpace(c.Bandwidth); v != "" {
		parts = append(parts, v)
	}
	if v := strings.TrimSpace(c.Uptime); v != "" {
		if s := formatUptime(v); s != "" {
			parts = append(parts, "在线 "+s)
		}
	}
	return strings.Join(parts, " · ")
}

// ClientGroup 是「某个 WLAN 实例下的终端」。
type ClientGroup struct {
	Owner    string // 「主机」或「子机 1（K251e）」
	SubInst  int    // 0 = 主机，否则是子设备实例号
	WlanInst int
	SSID     string
	Band     string // 归一化后的频段键：2.4G / 5G / 6G
	Channel  string
	Clients  []ClientInfo
	// Stale 是被 AssociatedDeviceNumberOfEntries 截掉的残留行数（真机上会有）。
	Stale   int
	Updated time.Time
}

// Label 是这个分组的显示用标题。
func (g ClientGroup) Label() string {
	parts := []string{g.Owner}
	if g.SSID != "" {
		parts = append(parts, g.SSID)
	}
	if g.Band != "" {
		parts = append(parts, g.Band)
	} else if g.WlanInst > 0 {
		parts = append(parts, "WLAN "+strconv.Itoa(g.WlanInst))
	}
	return strings.Join(parts, " · ")
}

// ClientTree 是整台设备的终端树。
type ClientTree struct {
	Host   []ClientGroup         // 主机各 WLAN 的终端
	Subs   []ClientGroup         // 各子设备各 WLAN 的终端（按实例号、WLAN 号排序）
	SubsBy map[int][]ClientGroup // 子设备实例号 → 分组
}

// BuildClientTree 解析终端树。nodes 是同一台设备上的 FTTR 子设备
// （用来把「子机 3」写成「子机 3（K251-20）」，拿不到型号也不影响解析）。
//
// **条目数以 AssociatedDeviceNumberOfEntries 为准**：真机上这张表会残留上一次读的
// 空行（实测：NumberOfEntries=1，表里却有 2 行，第 2 行 MAC/IP/RSSI 全空），
// 不截断界面上就会多出幽灵终端。
func BuildClientTree(params []store.Param, nodes []FttrNode) *ClientTree {
	model := map[int]string{}
	for _, n := range nodes {
		model[n.Instance] = n.Model
	}

	type acc struct {
		prefix, ssid, band, channel string
		inst, owner                 int
		nEntries, hasN              int
		clients                     map[int]*ClientInfo
		updated                     time.Time
	}
	byWlan := map[string]*acc{}
	// 设备「主机列表」里的 MAC → 名字（终端表通常没有名字，这里是唯一的补充来源）
	hostTbl := map[string]map[string]string{}
	touch := func(a *acc, t time.Time) {
		if t.After(a.updated) {
			a.updated = t
		}
	}

	for _, p := range params {
		// 主机列表（LANDevice.1.Hosts.Host.{i} / TR-181 的 Hosts.Host.{i}）先收集起来
		if hm := hostNameRe.FindStringSubmatch(p.Name); hm != nil {
			if v := strings.TrimSpace(p.Value); v != "" {
				tbl := hostTbl[hm[1]]
				if tbl == nil {
					tbl = map[string]string{}
					hostTbl[hm[1]] = tbl
				}
				tbl[strings.ToLower(hm[2])] = v
			}
			continue
		}
		m := wlanPathRe.FindStringSubmatch(p.Name)
		if m == nil {
			continue
		}
		wlanPrefix, rest := m[1], m[3]
		inst, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		owner := 0
		if sm := subOwnerRe.FindStringSubmatch(p.Name); sm != nil {
			if n, err := strconv.Atoi(sm[1]); err == nil {
				owner = n
			}
		}

		a := byWlan[wlanPrefix]
		if a == nil {
			a = &acc{prefix: wlanPrefix, inst: inst, owner: owner, clients: map[int]*ClientInfo{}}
			byWlan[wlanPrefix] = a
		}
		touch(a, p.UpdatedAt)
		v := strings.TrimSpace(p.Value)

		if cm := clientIdxRe.FindStringSubmatch(rest); cm != nil {
			idx, err := strconv.Atoi(cm[1])
			if err != nil {
				continue
			}
			ci := a.clients[idx]
			if ci == nil {
				ci = &ClientInfo{}
				a.clients[idx] = ci
			}
			switch strings.ToLower(cm[2]) {
			case "associateddevicemacaddress", "macaddress", "x_hw_macaddress":
				ci.MAC = v
			case "associateddeviceipaddress", "ipaddress", "x_hw_ipaddress":
				ci.IP = v
			case "rssi", "x_hw_rssi", "signalstrength", "x_hw_signalstrength":
				ci.RSSI = v
			case "snr", "x_hw_snr":
				ci.SNR = v
			case "singalquality", "signalquality", "x_hw_singalquality", "x_hw_signalquality":
				// 设备自报的信号质量（0..100）；界面上优先用它
				ci.Quality = v
			case "rxrate", "x_hw_rxrate":
				ci.RxRate = v
			case "txrate", "x_hw_txrate":
				ci.TxRate = v
			case "lastdatatransmitrate":
				// 有的设备只给这个（协商速率），当收/发速率兜底
				if ci.RxRate == "" {
					ci.RxRate = v
				}
				if ci.TxRate == "" {
					ci.TxRate = v
				}
			case "frequencywidth", "x_hw_frequencywidth":
				ci.Bandwidth = v
			case "uptime", "x_hw_uptime":
				ci.Uptime = v
			default:
				if isClientNameField(cm[2]) && ci.HostName == "" {
					// 终端行自己带的名字（如华为 X_HW_AssociatedDevicedescriptions）
					ci.HostName = v
				}
			}
			continue
		}

		// WLAN 自身字段：实例号后面只剩一层（SSID / 频段 / 信道 / 条目数）
		if strings.Contains(rest, ".") {
			continue
		}
		switch strings.ToLower(rest) {
		case "ssid":
			if v != "" {
				a.ssid = v
			}
		case "x_hw_rfband", "operatingfrequencyband", "rfband":
			if v != "" {
				a.band = bandKey(v)
			}
		case "channel":
			a.channel = v
		case "associateddevicenumberofentries", "totalassociations":
			if n, err := strconv.Atoi(v); err == nil {
				a.nEntries, a.hasN = n, 1
			}
		}
	}

	// MAC → 名字：主机列表优先，各终端行自己带的名字也能给别的表借用
	hostNames := map[string]string{}
	for _, tbl := range hostTbl {
		if mac, name := strings.ToLower(tbl["macaddress"]), strings.TrimSpace(tbl["hostname"]); mac != "" && name != "" {
			hostNames[mac] = name
		}
	}
	for _, a := range byWlan {
		for _, ci := range a.clients {
			mac := strings.ToLower(strings.TrimSpace(ci.MAC))
			if mac != "" && strings.TrimSpace(ci.HostName) != "" {
				if _, ok := hostNames[mac]; !ok {
					hostNames[mac] = strings.TrimSpace(ci.HostName)
				}
			}
		}
	}

	tree := &ClientTree{SubsBy: map[int][]ClientGroup{}}
	for _, a := range byWlan {
		g := ClientGroup{
			SubInst:  a.owner,
			Owner:    ownerLabel(a.owner, model[a.owner]),
			WlanInst: a.inst,
			SSID:     a.ssid,
			Band:     a.band,
			Channel:  a.channel,
			Updated:  a.updated,
		}
		idxs := make([]int, 0, len(a.clients))
		for i := range a.clients {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			ci := a.clients[i]
			if strings.TrimSpace(ci.MAC) == "" && strings.TrimSpace(ci.IP) == "" {
				// 空行（残留或设备刚清空）—— 不算一台终端
				continue
			}
			if a.hasN == 1 && i > a.nEntries {
				g.Stale++
				continue
			}
			info := *ci
			if strings.TrimSpace(info.HostName) == "" {
				info.HostName = hostNames[strings.ToLower(strings.TrimSpace(info.MAC))]
			}
			g.Clients = append(g.Clients, info)
		}
		if a.owner == 0 {
			tree.Host = append(tree.Host, g)
		} else {
			tree.Subs = append(tree.Subs, g)
			tree.SubsBy[a.owner] = append(tree.SubsBy[a.owner], g)
		}
	}
	sortGroups := func(gs []ClientGroup) {
		sort.Slice(gs, func(i, j int) bool { return gs[i].WlanInst < gs[j].WlanInst })
	}
	sortGroups(tree.Host)
	sort.Slice(tree.Subs, func(i, j int) bool {
		if tree.Subs[i].SubInst != tree.Subs[j].SubInst {
			return tree.Subs[i].SubInst < tree.Subs[j].SubInst
		}
		return tree.Subs[i].WlanInst < tree.Subs[j].WlanInst
	})
	for k := range tree.SubsBy {
		gs := tree.SubsBy[k]
		sortGroups(gs)
		tree.SubsBy[k] = gs
	}
	return tree
}

// ownerLabel 把「归属」写成界面上的字样。
func ownerLabel(subInst int, model string) string {
	if subInst == 0 {
		return "主机"
	}
	s := "子机 " + strconv.Itoa(subInst)
	if model != "" {
		s += "（" + model + "）"
	}
	return s
}

// bandKey 把设备自报的频段归一化，方便把子设备的终端合到主机的对应频段上。
func bandKey(band string) string {
	b := strings.ToLower(strings.TrimSpace(band))
	switch {
	case b == "":
		return ""
	case strings.Contains(b, "2.4"):
		return "2.4G"
	case strings.Contains(b, "5g"), strings.Contains(b, "5.8"):
		return "5G"
	case strings.Contains(b, "6g"):
		return "6G"
	}
	return band
}

// SubInstances 返回有终端数据（或至少有 WLAN）的子设备实例号，升序。
func (t *ClientTree) SubInstances() []int {
	out := make([]int, 0, len(t.SubsBy))
	for k := range t.SubsBy {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// Groups 返回「主机 + 所有子机」的分组（主机在前，再按子机实例号）。
func (t *ClientTree) Groups() []ClientGroup {
	out := make([]ClientGroup, 0, len(t.Host)+len(t.Subs))
	out = append(out, t.Host...)
	out = append(out, t.Subs...)
	return out
}

// CountInBand 统计某个频段上的终端数：主机自己 + 所有子机。
//
// 返回的三个数是给界面用的：合计、主机、子机 —— 界面上要能看出一台主机
// 「自己一台没有，全是子光猫连的」这种情况。
func (t *ClientTree) CountInBand(band string) (total, host, sub int) {
	want := bandKey(band)
	if want == "" {
		return 0, 0, 0
	}
	for _, g := range t.Host {
		if g.Band == want {
			host += len(g.Clients)
		}
	}
	for _, g := range t.Subs {
		if g.Band == want {
			sub += len(g.Clients)
		}
	}
	return host + sub, host, sub
}

// GroupsInBand 取某个频段的所有分组（主机 + 子机，按归属排序），给弹窗用。
// 只返回**真的连了终端**的分组 —— 弹窗里列一堆「0 台」没意义。
func (t *ClientTree) GroupsInBand(band string) []ClientGroup {
	want := bandKey(band)
	if want == "" {
		return nil
	}
	out := make([]ClientGroup, 0, 4)
	for _, g := range t.Groups() {
		if g.Band == want && len(g.Clients) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// WireBandClients 把子设备的终端数合到主机各频段上（就地改 bands）。
//
// 「代表行」规则：同一频段有多行时（真机的 5G 就是一行有 SSID、一行是空实例），
// 子机数量只算在有 SSID 的那一行，否则同一台子设备会被重复计入。
// 按钮（ModalID）两行都给：点开看到的是**该频段**的完整列表，不会重复计数。
func WireBandClients(bands []WifiBand, tree *ClientTree) {
	repOf := map[string]int{}
	for i, b := range bands {
		key := bandKey(b.Band)
		if key == "" {
			continue
		}
		idx, ok := repOf[key]
		if !ok {
			repOf[key] = i
			continue
		}
		if bands[idx].SSID == "" && b.SSID != "" {
			repOf[key] = i
		}
	}
	for i := range bands {
		b := &bands[i]
		key := bandKey(b.Band)
		if key == "" {
			continue
		}
		host, _ := strconv.Atoi(strings.TrimSpace(b.Clients))
		b.ClientsAll = host
		if repOf[key] == i {
			total, _, sub := tree.CountInBand(key)
			b.ClientsAll, b.SubClients = total, sub
		}
		b.ModalID = bandModalID(key)
		b.HasClientsBtn = b.ModalID != "" && b.ClientsAll > 0
	}
}

// bandModalID 是频段终端弹窗的固定 id（模板和按钮两边都靠它对接）。
func bandModalID(band string) string {
	key := strings.NewReplacer(".", "", " ", "").Replace(bandKey(band))
	if key == "" {
		return ""
	}
	return "climodal-" + key
}

// subModalID 是子设备终端弹窗的 id。
func subModalID(inst int) string { return "climodal-sub-" + strconv.Itoa(inst) }

// BandClientsView 是「某个频段的终端弹窗」的数据。
type BandClientsView struct {
	Band      string
	Label     string
	ModalID   string
	HostCount int
	SubCount  int
	Total     int
	Groups    []ClientGroup
}

// SubClientsView 是「子设备表里那个终端按钮 + 弹窗」的数据。
type SubClientsView struct {
	Instance int
	Label    string
	ModalID  string
	Total    int
	// HasWlan 表示这台子设备确实带了自己的 WLAN 对象（区分「0 台终端」与「根本没采到」）
	HasWlan bool
	// Groups 只包含连了终端的分组
	Groups []ClientGroup
}

// BuildClientViews 把终端树整理成模板直接能用的两种视图。
func BuildClientViews(tree *ClientTree, nodes []FttrNode) (bands []BandClientsView, subs []SubClientsView) {
	// 频段：以主机自报的频段为准（子机有、主机没有的频段也补上）
	seen := map[string]bool{}
	for _, g := range tree.Groups() {
		if g.Band == "" || seen[g.Band] {
			continue
		}
		seen[g.Band] = true
		total, host, sub := tree.CountInBand(g.Band)
		if total == 0 {
			continue
		}
		bands = append(bands, BandClientsView{
			Band:      g.Band,
			Label:     g.Band,
			ModalID:   bandModalID(g.Band),
			HostCount: host,
			SubCount:  sub,
			Total:     total,
			Groups:    tree.GroupsInBand(g.Band),
		})
	}
	sort.Slice(bands, func(i, j int) bool { return rank(bands[i].Band) < rank(bands[j].Band) })

	model := map[int]string{}
	for _, n := range nodes {
		model[n.Instance] = n.Model
	}
	for _, inst := range tree.SubInstances() {
		gs := tree.SubsBy[inst]
		n := 0
		for _, g := range gs {
			n += len(g.Clients)
		}
		kept := make([]ClientGroup, 0, len(gs))
		for _, g := range gs {
			if len(g.Clients) > 0 {
				kept = append(kept, g)
			}
		}
		subs = append(subs, SubClientsView{
			Instance: inst,
			Label:    ownerLabel(inst, model[inst]),
			ModalID:  subModalID(inst),
			Total:    n,
			HasWlan:  len(gs) > 0,
			Groups:   kept,
		})
	}
	return bands, subs
}
