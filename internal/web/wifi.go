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

// wifiInstanceRe 从参数名里抠出「容器名 + 实例号 + 字段名」。
//
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
