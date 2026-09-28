package web

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"acs/internal/store"
)

// WifiBand 是看板上「一个设备的某个频段」的概况。
type WifiBand struct {
	Instance int    // 实例号（真机上 2.4G 是 1、5G 是 5，不连续）
	Label    string // 显示用：2.4G / 5G / 实例 N
	Band     string // 设备自报的频段，如 2.4GHz / 5GHz
	SSID     string
	BSSID    string
	Channel  string
	Standard string
	Security string
	Clients  string // 已连终端数
	Status   string // Up / Disabled / ...
	// Note 是频段原始值的补充说明；当 Label 已经能表达清楚时为空
	// （避免界面上出现「2.4G 2.4GHz」这种重复）。
	Note   string
	On     bool // 射频是否开着
	HaveOn bool // 是否知道开关状态（设备没报就不知道）

	Empty bool // 实例存在但没采到任何字段
}

// wifiInstanceRe 从参数名里抠出「容器名 + 实例号 + 字段名」。//
// 同时兼容 TR-098 的 WLANConfiguration.{i}.<字段> 和 TR-181 的
// WiFi.Radio.{i}.<字段> / WiFi.SSID.{i}.<字段> / WiFi.AccessPoint.{i}.<字段>。
var wifiInstanceRe = regexp.MustCompile(`(?i)(?:WLANConfiguration|Radio|SSID|AccessPoint)\.(\d+)\.([A-Za-z0-9_]+)$`)

// WifiOverview 把一堆无线参数整理成「按实例分组」的概况。
// params 只包含无线相关参数（由 store.WifiParams 取出）。
func WifiOverview(params []store.Param) []WifiBand {
	type acc struct {
		band WifiBand
		ssid string
	}
	byInst := map[int]*acc{}

	get := func(inst int) *acc {
		if a, ok := byInst[inst]; ok {
			return a
		}
		a := &acc{band: WifiBand{Instance: inst}}
		byInst[inst] = a
		return a
	}

	for _, p := range params {
		m := wifiInstanceRe.FindStringSubmatch(p.Name)
		if m == nil {
			continue
		}
		inst, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		field := strings.ToLower(m[2])
		a := get(inst)
		v := strings.TrimSpace(p.Value)

		switch field {
		case "ssid":
			// TR-181 里 Device.WiFi.SSID.{i}.SSID 也是这个字段名，直接取
			if v != "" {
				a.ssid = v
			}
		case "x_hw_rfband", "operatingfrequencyband":
			if v != "" {
				a.band.Band = v
			}
		case "bssid":
			a.band.BSSID = v
		case "channel":
			a.band.Channel = v
		case "standard", "x_hw_standard":
			if v != "" {
				a.band.Standard = v
			}
		case "beacontype":
			// 仅在没拿到更具体的加密方式时，用它兜底显示认证类型
			if a.band.Security == "" {
				a.band.Security = v
			}
		case "wpaencryptionmodes", "x_hw_wpaand11iencryptionmodes":
			if v != "" {
				a.band.Security = v
			}
		case "totalassociations", "associateddevicenumberofentries":
			a.band.Clients = v
		case "status":
			a.band.Status = v
		case "enable":
			// 服务开关。TR-181 的 Radio.{i}.Enable / SSID.{i}.Enable 都落这里
			if v == "1" || strings.EqualFold(v, "true") {
				a.band.On, a.band.HaveOn = true, true
			} else if v == "0" || strings.EqualFold(v, "false") {
				a.band.On, a.band.HaveOn = false, true
			}
		case "radioenabled":
			// 厂商私有但很常见，优先级高于 Enable
			if v == "1" || strings.EqualFold(v, "true") {
				a.band.On, a.band.HaveOn = true, true
			} else if v == "0" || strings.EqualFold(v, "false") {
				a.band.On, a.band.HaveOn = false, true
			}
		}
	}

	out := make([]WifiBand, 0, len(byInst))
	for _, a := range byInst {
		b := a.band
		b.SSID = a.ssid
		b.Label = bandLabel(b.Band, b.Instance)
		b.Note = bandNote(b.Label, b.Band)
		b.Empty = b.SSID == "" && b.Channel == "" && b.Standard == "" && b.Status == ""
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if rank(out[i].Band) != rank(out[j].Band) {
			return rank(out[i].Band) < rank(out[j].Band)
		}
		return out[i].Instance < out[j].Instance
	})
	return out
}

// rank 用于排序：2.4G 在前、5G 次之、未知/其它垫底。
func rank(band string) int {
	b := strings.ToLower(band)
	switch {
	case strings.Contains(b, "2.4"):
		return 0
	case strings.Contains(b, "5g"):
		return 1
	default:
		return 2
	}
}

// bandLabel 给出人类可读的频段标签。
// 频段以设备自报的为准；设备没报就老实显示实例号，不瞎猜。
func bandLabel(band string, inst int) string {
	b := strings.ToLower(band)
	switch {
	case strings.Contains(b, "2.4"):
		return "2.4G"
	case strings.Contains(b, "5g"):
		return "5G"
	case strings.Contains(b, "6g"):
		return "6G"
	case band != "":
		return band
	default:
		return "实例 " + strconv.Itoa(inst)
	}
}

// bandNote 只在标签没能表达清楚时给出原始频段值。
func bandNote(label, band string) string {
	if band == "" {
		return ""
	}
	switch label {
	case "2.4G", "5G", "6G":
		return "" // 标签已经说清楚了
	}
	return band
}

// wifiLeafIndex 返回某个实例下「叶子名（小写）-> 参数」的索引。
//
// 用叶子名而不是完整路径做索引，是为了同时支持 TR-098 的
// WLANConfiguration.{i}.SSID 和 TR-181 的 WiFi.SSID.{i}.SSID —— 两者叶子名都是 SSID。
// 叶子名撞车时（如 KeyPassphrase 与 PreSharedKey.1.KeyPassphrase）优先取层级更少的那个，
// 保证结果稳定。
func wifiLeafIndex(inst int, params []store.Param) map[string]store.Param {
	out := map[string]store.Param{}
	for _, p := range params {
		m := wifiInstanceRe.FindStringSubmatch(p.Name)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n != inst {
			continue
		}
		key := strings.ToLower(m[2])
		if old, ok := out[key]; ok && strings.Count(old.Name, ".") <= strings.Count(p.Name, ".") {
			continue
		}
		out[key] = p
	}
	return out
}

// WifiInstances 返回参数里出现过的所有无线实例号（升序）。
func WifiInstances(params []store.Param) []int {
	seen := map[int]bool{}
	for _, p := range params {
		m := wifiInstanceRe.FindStringSubmatch(p.Name)
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil {
			seen[n] = true
		}
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// WifiOption 是下拉框的一个选项。
type WifiOption struct {
	Value string
	Label string
}

// WifiFormField 是 WiFi 编辑表单里的一个字段。
//
// 字段是否出现、写向哪个参数、下拉候选值是什么，**全部从设备实报的参数里推导**，
// 不写死 —— 不同型号的无线参数差异很大（而且厂商私有的和标准的经常成对出现）。
type WifiFormField struct {
	Key      string // 表单字段名
	Label    string
	Kind     string // text | password | number | bool | select
	Param    string // 实际要写入的完整参数名
	Value    string
	Type     string // 参数类型（写回去时要带对 xsi:type）
	Options  []WifiOption
	ReadOnly bool // 已知不可写时置上
	Hint     string
	Suffix   string // 数值单位（如 %）
}

var beaconTypeOptions = []WifiOption{
	{"None", "不加密"},
	{"WEP", "WEP"},
	{"11i", "WPA2-PSK"},
	{"WPA", "WPA-PSK"},
	{"11iandWPA", "WPA/WPA2-PSK（混合）"},
}

var cipherOptions = []WifiOption{
	{"AESEncryption", "AES"},
	{"TKIPEncryption", "TKIP"},
	{"TKIPandAESEncryption", "AES+TKIP"},
}

var standardOptions = []WifiOption{
	{"11b", "11b"}, {"11g", "11g"}, {"11n", "11n"},
	{"11a", "11a"}, {"11ac", "11ac"}, {"11ax", "11ax（Wi-Fi 6）"},
}

// wifiFieldDefs 是表单字段的定义表。
// leaf 是叶子名候选（按优先级），取第一个在设备参数里存在的。
type wifiFieldDef struct {
	key     string
	label   string
	kind    string
	leaf    []string
	optFrom string // 候选值来自哪个叶子参数（逗号分隔的列表）
	optMap  []WifiOption
	suffix  string
	hint    string
}

var wifiFieldDefs = []wifiFieldDef{
	{key: "ssid", label: "SSID", kind: "text", leaf: []string{"ssid"}},
	{key: "enable", label: "启用无线 SSID", kind: "bool", leaf: []string{"enable"}},
	{key: "radio", label: "射频开关", kind: "bool", leaf: []string{"radioenabled"}},
	{key: "auto_channel", label: "开启自动信道", kind: "bool", leaf: []string{"autochannelenable"}},
	{key: "channel", label: "无线信道", kind: "select", leaf: []string{"channel"}, optFrom: "possiblechannels"},
	{key: "bandwidth", label: "信道带宽", kind: "select", leaf: []string{"operatingchannelbandwidth"}},
	{key: "auth", label: "加密方式", kind: "select", leaf: []string{"beacontype"}, optMap: beaconTypeOptions},
	{key: "cipher", label: "加密算法", kind: "select", leaf: []string{"ieee11iencryptionmodes", "x_hw_wpaand11iencryptionmodes", "wpaencryptionmodes"}, optMap: cipherOptions},
	{key: "standard", label: "无线标准", kind: "select", leaf: []string{"standard", "x_hw_standard"}, optMap: standardOptions},
	{key: "power", label: "发射功率", kind: "select", leaf: []string{"transmitpower"}, optFrom: "transmitpowersupported", suffix: "%"},
	{key: "key", label: "无线密码", kind: "password", leaf: []string{"keypassphrase"},
		hint: "为空表示不修改。很多 CPE 不回明文密码（能改不能读），所以这里显示为空是正常的。"},
}

// WifiForm 根据设备实报的参数拼出编辑表单。
func WifiForm(inst int, wifiParams []store.Param) []WifiFormField {
	idx := wifiLeafIndex(inst, wifiParams)
	out := make([]WifiFormField, 0, len(wifiFieldDefs))

	for _, def := range wifiFieldDefs {
		var found store.Param
		var ok bool
		for _, l := range def.leaf {
			if p, hit := idx[l]; hit {
				found, ok = p, true
				break
			}
		}
		if !ok {
			continue // 设备没这个参数就不要出这个字段，不猜
		}

		f := WifiFormField{
			Key:      def.key,
			Label:    def.label,
			Kind:     def.kind,
			Param:    found.Name,
			Value:    found.Value,
			Type:     found.ValueType,
			Hint:     def.hint,
			Suffix:   def.suffix,
			ReadOnly: !found.Writable && hasWritableInfo(wifiParams),
		}

		switch {
		case def.optFrom != "":
			f.Options = optionsFromParam(idx[def.optFrom], def.suffix)
		case len(def.optMap) > 0:
			f.Options = append([]WifiOption{}, def.optMap...)
		}
		// 当前值不在候选里时补进去，否则下拉框会选不中
		if len(f.Options) > 0 && f.Value != "" && !hasOption(f.Options, f.Value) {
			f.Options = append([]WifiOption{{Value: f.Value, Label: f.Value + "（当前）"}}, f.Options...)
		}
		// 想要下拉框但一个候选值都拿不到（设备没报 PossibleChannels 之类），
		// 就退回普通文本框 —— 总比给一个空的下拉框强。
		if f.Kind == "select" && len(f.Options) == 0 {
			f.Kind = "text"
		}
		out = append(out, f)
	}
	return out
}

// hasWritableInfo 判断这批参数里有没有「可写」信息。
// 只有做过 GetParameterNames 才会有；否则一律当成可写（让设备自己去拒）。
func hasWritableInfo(params []store.Param) bool {
	for _, p := range params {
		if p.Writable {
			return true
		}
	}
	return false
}

func hasOption(opts []WifiOption, v string) bool {
	for _, o := range opts {
		if o.Value == v {
			return true
		}
	}
	return false
}

// optionsFromParam 把设备自报的逗号列表（如 PossibleChannels="1,2,...,13"）变成下拉选项。
func optionsFromParam(p store.Param, suffix string) []WifiOption {
	if p.Name == "" || strings.TrimSpace(p.Value) == "" {
		return nil
	}
	var out []WifiOption
	for _, part := range strings.Split(p.Value, ",") {
		v := strings.TrimSpace(part)
		if v == "" {
			continue
		}
		out = append(out, WifiOption{Value: v, Label: v + suffix})
	}
	return out
}

// wifiCount 汇总一台设备所有频段的已连终端数（用于列表页那一列）。
func wifiCount(bands []WifiBand) int {
	n := 0
	for _, b := range bands {
		if v, err := strconv.Atoi(b.Clients); err == nil {
			n += v
		}
	}
	return n
}
