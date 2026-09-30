package web

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// fttrCandidates 描述一类「装子设备」的对象。
//
// 必须与 cwmp.fttrProbeCandidates 保持一致 —— web 包不反向依赖 cwmp，
// 所以这里抄了一份。改一处记得改另一处。
//
//   - detect：顶层对象前缀，能力探测枚举顶层时看这个
//   - instance：实例号紧跟在这个前缀后面（可能比 detect 多一级）
//   - 华为 FTTR：对象直接是 X_HW_APDevice.{i}.，两者相同
//   - 标准 TR-181 Multi-AP：对象是 DataElements.Network.，
//     而实例在 …Network.Device.{i}.，多一层 Device.
type fttrCandidate struct {
	detect   string
	instance string
}

var fttrCandidates = []fttrCandidate{
	{"internetgatewaydevice.x_hw_apdevice.", "internetgatewaydevice.x_hw_apdevice."},
	{"device.wifi.dataelements.network.", "device.wifi.dataelements.network.device."},
}

// FttrNode 是一台 FTTR 子设备（子光猫 / 子 AP）。
type FttrNode struct {
	Instance  int
	Model     string
	Serial    string
	MAC       string
	Online    bool
	HasOnline bool
	Status    string
	Firmware  string
	Hardware  string
	Channel   string
	Band      string
	Uptime    string
	Signal    string
	Sync      string
	Mode      string
	// ModesSupported / InternetAccess 是设备自报的另外两个跟组网有关的字段，
	// 不单独成列，放在「组网」的悬停提示里（免得我们自己的判断被当成设备事实）。
	ModesSupported string
	InternetAccess string
	// 光功率。命名各家不一（标准 OpticalRxPower、华为 X_HW_RxPower…），
	// 按叶子名后缀认，允许中间多一层（…{i}.Optical.RxPower）。
	RxPower string
	TxPower string
	// RxUpdated / TxUpdated 记这两个读数各自的时间：同一台子设备可能报多个
	// 疑似光功率的参数，选谁靠「分值 + 时间」比，所以时间要分开存。
	RxUpdated time.Time
	TxUpdated time.Time
	Updated   time.Time
}

// FttrOverview 从设备已采集的参数里解析出子设备列表。
//
// ok=false 表示**这台设备没有这类对象**，界面应当整块不渲染 ——
// 而不是渲染一个空区块让人以为坏了。
//
// 为什么这么判：顶层对象节点（如 …X_HW_APDevice.）只会在能力探测（next_level 枚举）
// 或子树枚举时产生，所以「存在以候选前缀开头的参数名」就足以说明设备有这能力。
func FttrOverview(params []store.Param) ([]FttrNode, bool) {
	// 第一遍：按 detect 前缀判断能力是否存在，并推出实例前缀。
	//
	// 保留设备自己的拼法（真机上出现过 InternetGateWayDevice. 这种大小写写错的情况），
	// 直接拿它去查/比较才不会漏。
	instancePrefix := ""
	for _, c := range fttrCandidates {
		for _, p := range params {
			if !strings.HasPrefix(strings.ToLower(p.Name), c.detect) {
				continue
			}
			head := p.Name[:len(c.detect)]     // 设备拼法的 detect 部分
			tail := c.instance[len(c.detect):] // 相对 detect 多出的那级（如 "device."）
			instancePrefix = head + tail
			break
		}
		if instancePrefix != "" {
			break
		}
	}
	if instancePrefix == "" {
		return nil, false
	}

	// 子设备字段形如 <instancePrefix><实例号>.<字段名>（字段名本身还可以带层级）
	re := regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(instancePrefix) + `(\d+)\.(.+)$`)

	byInst := map[int]*FttrNode{}
	for _, p := range params {
		m := re.FindStringSubmatch(p.Name)
		if m == nil {
			continue
		}
		inst, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		field := strings.TrimSpace(m[2])
		// 光功率优先（允许带中间层级，如 …{i}.Optical.RxPower）
		rx, tx := opticalField(field)
		if !rx && !tx {
			// 其余的只认**直属**字段：不能把子设备自己的无线配置
			// （…{i}.WLANConfiguration.1.SSID）当成子设备属性。
			if strings.Contains(field, ".") {
				continue
			}
		}
		n := byInst[inst]
		if n == nil {
			n = &FttrNode{Instance: inst}
			byInst[inst] = n
		}
		if p.UpdatedAt.After(n.Updated) {
			n.Updated = p.UpdatedAt
		}
		v := strings.TrimSpace(p.Value)
		if rx {
			// 子设备光功率同样可能命中多个私有参数，按分值取最像标准光功率的那个
			// （跟概览页 OpticalOverview 一个口径）。
			cur := opticalHit{value: n.RxPower, upd: n.RxUpdated, score: opticalScore(p.Name, field, true, false)}
			h := opticalHit{value: v, name: p.Name, upd: p.UpdatedAt, score: opticalScore(p.Name, field, true, false)}
			if cur.better(h) {
				n.RxPower = v
				n.RxUpdated = p.UpdatedAt
			}
			continue
		}
		if tx {
			cur := opticalHit{value: n.TxPower, upd: n.TxUpdated, score: opticalScore(p.Name, field, false, true)}
			h := opticalHit{value: v, name: p.Name, upd: p.UpdatedAt, score: opticalScore(p.Name, field, false, true)}
			if cur.better(h) {
				n.TxPower = v
				n.TxUpdated = p.UpdatedAt
			}
			continue
		}
		// 按**叶子名后缀**匹配：不同型号的字段名有出入，用后缀更耐用
		switch {
		case strings.HasSuffix(strings.ToLower(field), "supportedworkingmode"):
			n.ModesSupported = v
		case strings.HasSuffix(strings.ToLower(field), "internetaccessmode"):
			n.InternetAccess = v
		case strings.HasSuffix(strings.ToLower(field), "devicetype"):
			n.Model = v
		case strings.HasSuffix(strings.ToLower(field), "serialnumber"):
			n.Serial = v
		case strings.HasSuffix(strings.ToLower(field), "macaddr"):
			n.MAC = v
		case strings.HasSuffix(strings.ToLower(field), "onlineflag"):
			n.HasOnline = true
			n.Online = v == "1" || strings.EqualFold(v, "true")
		case strings.HasSuffix(strings.ToLower(field), "devicestatus"):
			n.Status = v
		case strings.HasSuffix(strings.ToLower(field), "softwareversion"):
			n.Firmware = v
		case strings.HasSuffix(strings.ToLower(field), "hardwareversion"):
			n.Hardware = v
		case strings.HasSuffix(strings.ToLower(field), "currentchannel"):
			n.Channel = v
		case strings.HasSuffix(strings.ToLower(field), "supportedrfband"):
			n.Band = v
		case strings.HasSuffix(strings.ToLower(field), "uptime"):
			n.Uptime = v
		case strings.HasSuffix(strings.ToLower(field), "signalintensity"):
			n.Signal = v
		case strings.HasSuffix(strings.ToLower(field), "syncstatus"):
			n.Sync = v
		case strings.HasSuffix(strings.ToLower(field), "workingmode"):
			n.Mode = v
		}
	}

	out := make([]FttrNode, 0, len(byInst))
	for _, n := range byInst {
		out = append(out, *n)
	}
	// 实例号不连续是常态（真机上是 1/2/4），按号排序更符合直觉
	sort.Slice(out, func(i, j int) bool { return out[i].Instance < out[j].Instance })
	return out, true
}

// ---------- 组网模式与光功率 ----------

// opticalField 判断一个字段（可带层级）是不是光功率字段，是收还是发。
//
// 命名没有统一标准（RxPower / X_HW_RxPower / OpticalPowerRx / ReceiveOpticalPower /
// PonRxPower…），枚举名字永远列不全，所以改成**按语义辨认**：
//
//  1. 叶子名（去掉下划线/横杠/空格后）必须含 power —— 不是功率就不谈光功率；
//  2. 还得有「光」的意味：含 optical / pon，或者是常见的 rxpower / txpower 写法；
//  3. 方向看 rx / receive（收）与 tx / transmit（发）。
//
// 特别注意：无线的发射功率叫 TransmitPower，**不是**光发射功率，
// 凡是 transmitpower 且不带 optical 的一律不算（它既没有光语义也没有 tx 写法）。
func opticalField(field string) (rx, tx bool) {
	leaf := strings.ToLower(strings.TrimSpace(field))
	if i := strings.LastIndex(leaf, "."); i >= 0 {
		leaf = leaf[i+1:]
	}
	norm := strings.NewReplacer("_", "", "-", "", " ", "").Replace(leaf)
	if norm == "" || !strings.Contains(norm, "power") {
		return false, false
	}
	// 无线的发射功率：不带 optical 就不是光功率
	if strings.Contains(norm, "transmitpower") && !strings.Contains(norm, "optical") {
		return false, false
	}
	optical := strings.Contains(norm, "optical") || strings.Contains(norm, "pon")
	if !optical && !strings.Contains(norm, "rxpower") && !strings.Contains(norm, "txpower") {
		return false, false
	}
	switch {
	case strings.Contains(norm, "rx") || strings.Contains(norm, "receive") || strings.Contains(norm, "incoming"):
		return true, false
	case strings.Contains(norm, "tx") || strings.Contains(norm, "transmit") || strings.Contains(norm, "outgoing"):
		return false, true
	}
	return false, false
}

// HasOptical 表示这台子设备上报了光功率（有没有光口、设备报不报，它说了算）。
func (n FttrNode) HasOptical() bool {
	return strings.TrimSpace(n.RxPower) != "" || strings.TrimSpace(n.TxPower) != ""
}

// BackhaulKind 判回程介质：fiber / wireless / wired，判不出就是空。
//
// 线索都来自设备自报：
//   - SignalIntensity 非 0 → 无线（无线回程才有信号强度；真机光纤/有线回程时它是 0）
//   - WorkingMode 里明确的写法（wifi/wireless、eth/ethernet/wire、fttr/fiber/pon）
//
// 判不出来**不猜**：宁可界面上显示设备自报的原值，也不要编一个“光纤组网”出来。
func (n FttrNode) BackhaulKind() string {
	sig := strings.TrimSpace(n.Signal)
	if sig != "" {
		if v, err := strconv.ParseFloat(sig, 64); err == nil && v != 0 {
			return "wireless"
		}
	}
	m := strings.ToLower(strings.TrimSpace(n.Mode))
	switch {
	case m == "wifi", m == "wireless", m == "wds", strings.Contains(m, "wireless"), strings.Contains(m, "wifi"):
		return "wireless"
	case strings.Contains(m, "fttr"), strings.Contains(m, "fiber"), strings.Contains(m, "optical"), strings.Contains(m, "pon"):
		return "fiber"
	case strings.Contains(m, "eth"), strings.Contains(m, "wire"), strings.Contains(m, "bridge"), strings.Contains(m, "lan"):
		return "wired"
	}
	return ""
}

// ModeText 是「组网」列要显示的文字。
func (n FttrNode) ModeText() string {
	switch n.BackhaulKind() {
	case "wireless":
		if s := strings.TrimSpace(n.Signal); s != "" && s != "0" {
			return "无线组网（信号 " + s + "）"
		}
		return "无线组网"
	case "fiber":
		return "光纤组网"
	case "wired":
		return "有线组网"
	}
	// 判不出来就把设备自报的值原样显示（各家取值没有统一标准，不美化）
	return strings.TrimSpace(n.Mode)
}

// ModeHint 是「组网」列的悬停提示：把设备自报的信息用能读的话说一遍
// （原始字段名对使用者没有意义，但“设备就是这么报的”要能看到）。
func (n FttrNode) ModeHint() string {
	parts := make([]string, 0, 4)
	add := func(prefix, v string) {
		if v = strings.TrimSpace(v); v != "" {
			parts = append(parts, prefix+v)
		}
	}
	add("", n.Mode)
	if v := strings.TrimSpace(n.ModesSupported); v != "" && v != strings.TrimSpace(n.Mode) {
		add("支持 ", v)
	}
	add("上行 ", n.InternetAccess)
	if v := strings.TrimSpace(n.Signal); v != "" && v != "0" {
		add("信号 ", v)
	}
	if len(parts) == 0 {
		return ""
	}
	return "设备上报：" + strings.Join(parts, " · ")
}

// OpticalPower 是「光功率」列要显示的文字（空 = 这格不显示值）。
//
// 规则：**无线 / 有线组网的子设备不显示光功率** —— 它没有光口，
// 就算设备回了一个值（有些固件会回 0 或无效值）也不该展示。
func (n FttrNode) OpticalPower() string {
	if !n.HasOptical() {
		return ""
	}
	switch n.BackhaulKind() {
	case "wireless", "wired":
		return ""
	}
	parts := make([]string, 0, 2)
	if v := strings.TrimSpace(n.RxPower); v != "" {
		parts = append(parts, "Rx "+withDbm(v))
	}
	if v := strings.TrimSpace(n.TxPower); v != "" {
		parts = append(parts, "Tx "+withDbm(v))
	}
	return strings.Join(parts, " / ")
}

// withDbm 把设备回的读数规整成「-14.00 dBm」这样显示。
//
// 设备回的原值什么样都有：`-14`、`-14.0`、`-14.580`、`"-14 dBm"`、甚至空。
// 统一：
//   - 纯数字（可带单位）→ 解析成浮点，固定两位小数，再补 dBm
//   - 解析不出数字（设备回了 "N/A"、"-"、非法值）→ 原样返回，
//     不能拿 0 去顶 —— 那会看起来像一个真实的读数
func withDbm(v string) string {
	s := strings.TrimSpace(v)
	if s == "" {
		return ""
	}
	// 去掉设备自己带的单位，剩下的部分尝试当数字解析
	num := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "dBm"))
	if num == "" {
		return s
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'f', 2, 64) + " dBm"
}

// fttrHasOptical 判断整列要不要渲染：一个都没读到就整列不显示（跟 WAN / FTTR 一个规矩）。
func fttrHasOptical(nodes []FttrNode) bool {
	for _, n := range nodes {
		if n.HasOptical() {
			return true
		}
	}
	return false
}

// fttrClientCount 汇总子设备在线台数（给界面上的小字用）。
func fttrOnlineCount(nodes []FttrNode) int {
	n := 0
	for _, x := range nodes {
		if x.Online {
			n++
		}
	}
	return n
}
