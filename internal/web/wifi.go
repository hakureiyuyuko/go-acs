package web

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
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
	// Updated 是这个频段相关参数最后一次采集的时间。
	// 界面上要显示它：真机上就因为看不到“这是多久前的值”而误判过
	// （5GHz 射频已起来、能搜到信号，界面还显示 Disabled）。
	Updated time.Time
	// Note 是频段原始值的补充说明；当 Label 已经能表达清楚时为空
	// （避免界面上出现「2.4G 2.4GHz」这种重复）。
	Note   string
	On     bool // 射频是否开着
	HaveOn bool // 是否知道开关状态（设备没报就不知道）

	Empty bool // 实例存在但没采到任何字段

	// ClientsAll / SubClients：终端数的「合计」与其中来自 FTTR 子设备的部分。
	// **必须把子设备的终端算进来**：真机上主机自己的 WLAN 一台终端都没有，
	// 终端全挂在子光猫上，只看 Clients 会显示 0。
	ClientsAll int
	SubClients int
	// ModalID 非空时，「终端」那一格可以点开对应频段的终端弹窗。
	// 同一频段有多行（真机上有一行没 SSID 的空实例）时，子机数量只算在有 SSID 的那行，
	// 否则同一台子设备会被重复计入。
	ModalID string
	// HasClientsBtn：这一格是渲染成按钮还是纯数字 —— 只有真有终端时才做成按钮。
	HasClientsBtn bool
}

// wifiInstanceRe 从参数名里抠出「容器名 + 实例号 + 剩余路径」。
//
// 注意结尾用 (.+)$ 而不是 ([A-Za-z0-9_]+)$ —— 实例号后面可能还有多级路径，
// 比如 PreSharedKey.1.KeyPassphrase；只取最后一段会把这种参数整个漏掉。
// 同时兼容 TR-098 的 WLANConfiguration.{i}.x 与 TR-181 的
// WiFi.Radio.{i}.x / WiFi.SSID.{i}.x / WiFi.AccessPoint.{i}.x。
var wifiInstanceRe = regexp.MustCompile(`(?i)(?:WLANConfiguration|Radio|SSID|AccessPoint)\.(\d+)\.(.+)$`)

// isSubDeviceWifi 判断这个参数是不是 FTTR 子设备自己的无线参数。
//
// 子设备（X_HW_APDevice.{i} / TR-181 Multi-AP 的 DataElements.Network.Device.{i}）
// 也带一套 WLANConfiguration，实例号同样从 1 开始 —— 它们不能混进**主机**的无线概览/编辑表单，
// 否则主机的「2.4G」那一行会显示成子光猫的 SSID（真机上真的会这么误导人）。
func isSubDeviceWifi(name string) bool { return subOwnerRe.MatchString(name) }

// WifiOverview 把一堆无线参数整理成「按实例分组」的概况。
// params 只包含无线相关参数（由 store.WifiParams 取出）。
func WifiOverview(params []store.Param) []WifiBand {
	type acc struct {
		band WifiBand
		ssid string
	}
	byInst := map[int]*acc{}

	touch := func(a *acc, p store.Param) {
		if p.UpdatedAt.After(a.band.Updated) {
			a.band.Updated = p.UpdatedAt
		}
	}

	get := func(inst int) *acc {
		if a, ok := byInst[inst]; ok {
			return a
		}
		a := &acc{band: WifiBand{Instance: inst}}
		byInst[inst] = a
		return a
	}

	for _, p := range params {
		if isSubDeviceWifi(p.Name) {
			continue
		}
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
		touch(a, p)
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

// wifiInstanceParams 取出某个实例下的所有叶子参数，并算出**相对实例的路径**。
//
// 相对路径是必需的：同一个实例下有 KeyPassphrase 和 PreSharedKey.1.KeyPassphrase
// 两个不同的参数，只看最后一段分不开。
type wifiParam struct {
	P   store.Param
	Rel string // 小写，如 "presharedkey.1.keypassphrase"
}

func wifiInstanceParams(inst int, params []store.Param) []wifiParam {
	var out []wifiParam
	for _, p := range params {
		if isSubDeviceWifi(p.Name) {
			continue
		}
		loc := wifiInstanceRe.FindStringSubmatchIndex(p.Name)
		if loc == nil || len(loc) < 6 {
			continue
		}
		n, err := strconv.Atoi(p.Name[loc[2]:loc[3]])
		if err != nil || n != inst {
			continue
		}
		out = append(out, wifiParam{P: p, Rel: strings.ToLower(p.Name[loc[4]:loc[5]])})
	}
	return out
}

// matchCandidate 判断一个参数是否命中候选。
//   - 单段候选（如 "ssid"）只比较最后一段；
//   - 多段候选（如 "presharedkey.1.keypassphrase"）比较尾部，用来区分同名叶子。
func matchCandidate(rel, candidate string) bool {
	c := strings.ToLower(strings.TrimSpace(candidate))
	if c == "" {
		return false
	}
	if !strings.Contains(c, ".") {
		leaf := rel[strings.LastIndex(rel, ".")+1:]
		return leaf == c
	}
	return strings.HasSuffix(rel, c)
}

// WifiInstances 返回参数里出现过的所有无线实例号（升序）。
func WifiInstances(params []store.Param) []int {
	seen := map[int]bool{}
	for _, p := range params {
		if isSubDeviceWifi(p.Name) {
			continue
		}
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
// leaf 是候选（按优先级），单段比较叶子名，多段比较尾部路径。
type wifiFieldDef struct {
	key     string
	label   string
	kind    string
	leaf    []string
	optFrom string // 候选值来自哪个参数（单段写叶子名）
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
	// WPA/WPA2-PSK 的密码在 PreSharedKey.1.KeyPassphrase 下；
	// WLANConfiguration.{i}.KeyPassphrase 是给 WEP 的。
	// 真机实测：往后者写密码，设备回 9007 Invalid parameter value，
	// 所以优先写 PreSharedKey 那个，拿不到才退回。
	{key: "key", label: "无线密码", kind: "password",
		leaf: []string{"presharedkey.1.keypassphrase", "keypassphrase"},
		hint: "为空表示不修改。设备一般不返回明文密码，这里为空是正常的。"},
}

// WifiForm 根据设备实报的参数拼出编辑表单。
func WifiForm(inst int, wifiParams []store.Param) []WifiFormField {
	instParams := wifiInstanceParams(inst, wifiParams)
	out := make([]WifiFormField, 0, len(wifiFieldDefs))

	find := func(leaf []string) (wifiParam, bool) {
		for _, cand := range leaf {
			for _, ip := range instParams {
				if matchCandidate(ip.Rel, cand) {
					return ip, true
				}
			}
		}
		return wifiParam{}, false
	}

	for _, def := range wifiFieldDefs {
		found, ok := find(def.leaf)
		if !ok {
			continue // 设备没这个参数就不要出这个字段，不猜
		}

		f := WifiFormField{
			Key:      def.key,
			Label:    def.label,
			Kind:     def.kind,
			Param:    found.P.Name,
			Value:    found.P.Value,
			Type:     found.P.ValueType,
			Hint:     def.hint,
			Suffix:   def.suffix,
			ReadOnly: !found.P.Writable && hasWritableInfo(wifiParams),
		}

		switch {
		case def.optFrom != "":
			if src, hit := find([]string{def.optFrom}); hit {
				f.Options = optionsFromParam(src.P, def.suffix)
			}
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
