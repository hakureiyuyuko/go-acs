package web

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// LanEthPort 是详情页「以太网口」表的一行。
//
// 只放**设备自己报的**东西：状态、协商速率/双工、MAC、收发字节数、采集时间。
// 速率/双工/流量额外给人话版本，原文放悬停里（免得我们的解读被当成设备事实）。
type LanEthPort struct {
	Instance  int
	Name      string
	Status    string // 设备原文：Up / NoLink / Down / Disabled…
	Up        bool
	Rate      string // 人话：2.5 Gbps / 自动协商（100 Mbps）
	RateRaw   string // 设备原文：MaxBitRate=2500 / X_HW_Speed=Auto_2500
	Duplex    string // 人话：自动协商（全双工）
	DuplexRaw string
	MAC       string
	Tx        string // 人话：35.4 GB
	Rx        string
	Updated   time.Time
}

// lanEthInstanceRes 是以太网口对象的两种命名。
//
// 必须跟 cwmp.lanEthSubtreePath 枚举的那两棵树对上：
//   - TR-098：LANDevice.{i}.LANEthernetInterfaceConfig.{j}.
//   - TR-181：Device.Ethernet.Interface.{i}.（不跟着 Link.{i} 混进来，
//     那个对象也有 Status/MACAddress，会串台）
var lanEthInstanceRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)lanethernetinterfaceconfig\.(\d+)\.(.+)$`),
	regexp.MustCompile(`(?i)^device\.ethernet\.interface\.(\d+)\.(.+)$`),
}

// ethSpeedRe 匹配华为那种「Auto_2500」的速率写法。
var ethSpeedRe = regexp.MustCompile(`(?i)^auto_(\d+)$`)

// LanEthOverview 从设备已采集的参数里解析出以太网口列表。
//
// ok=false 表示**这台设备没有网口对象**，界面应当整块不渲染 ——
// 跟 WAN / FTTR 区块一个规矩，不给没有网口的设备摆一张空表。
func LanEthOverview(params []store.Param) ([]LanEthPort, bool) {
	byInst := map[int]*LanEthPort{}

	for _, p := range params {
		inst, field, ok := matchLanEth(p.Name)
		if !ok {
			continue
		}
		port := byInst[inst]
		if port == nil {
			port = &LanEthPort{Instance: inst}
			byInst[inst] = port
		}
		if p.UpdatedAt.After(port.Updated) {
			port.Updated = p.UpdatedAt
		}
		v := strings.TrimSpace(p.Value)
		switch strings.ToLower(field) {
		case "name":
			port.Name = v
		case "status":
			port.Status = v
			port.Up = strings.EqualFold(v, "Up")
		case "macaddress":
			port.MAC = v
		case "maxbitrate":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				port.Rate = mbpsHuman(n)
			} else if v != "" && !strings.EqualFold(v, "Auto") {
				port.Rate = v
			}
			port.RateRaw = appendRaw(port.RateRaw, "MaxBitRate="+v)
		case "x_hw_speed":
			port.RateRaw = appendRaw(port.RateRaw, "X_HW_Speed="+v)
			// 只当 MaxBitRate 给不出数字时**兑底**用它（真机上 MaxBitRate=2500、
			// X_HW_Speed=Auto_2500 是同一个意思，显示「2.5 Gbps」比「自动协商（2.5 Gbps）」干净）。
			// 这个判断不能依赖参数顺序，所以只在 Rate 还空着时写。
			if port.Rate != "" {
				break
			}
			if m := ethSpeedRe.FindStringSubmatch(v); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
					port.Rate = "自动协商（" + mbpsHuman(n) + "）"
				}
			}
		case "duplexmode":
			port.DuplexRaw = appendRaw(port.DuplexRaw, "DuplexMode="+v)
			if v != "" && !strings.EqualFold(v, "Auto") {
				port.Duplex = duplexHuman(v)
			}
		case "x_hw_duplexmode":
			port.DuplexRaw = appendRaw(port.DuplexRaw, "X_HW_DuplexMode="+v)
			if port.Duplex == "" {
				port.Duplex = duplexHuman(v)
			}
		case "stats.bytessent":
			port.Tx = humanBytes(v)
		case "stats.bytesreceived":
			port.Rx = humanBytes(v)
		}
	}

	out := make([]LanEthPort, 0, len(byInst))
	for _, p := range byInst {
		out = append(out, *p)
	}
	// 实例号不连续是常态，按号排序更符合直觉
	sort.Slice(out, func(i, j int) bool { return out[i].Instance < out[j].Instance })
	return out, len(out) > 0
}

// lanEthUpCount 数一下有几个口是「连着线」的（表头那行用）。
func lanEthUpCount(ports []LanEthPort) int {
	n := 0
	for _, p := range ports {
		if p.Up {
			n++
		}
	}
	return n
}

// matchLanEth 从参数名里认出「第几个口 + 相对字段名」。
func matchLanEth(name string) (inst int, field string, ok bool) {
	for _, re := range lanEthInstanceRes {
		m := re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		return n, m[2], true
	}
	return 0, "", false
}

// appendRaw 把设备原文拼进悬停提示（"A=1 · B=2"）。
func appendRaw(sofar, add string) string {
	if strings.TrimSpace(add) == "" {
		return sofar
	}
	if sofar == "" {
		return add
	}
	return sofar + " · " + add
}

// mbpsHuman 把 Mbps 数字说成人话：100 → 100 Mbps，2500 → 2.5 Gbps。
func mbpsHuman(n int) string {
	if n >= 1000 {
		return strconv.FormatFloat(float64(n)/1000, 'f', -1, 64) + " Gbps"
	}
	return strconv.Itoa(n) + " Mbps"
}

// duplexHuman 把双工模式说成人话（Auto_Full → 自动协商（全双工））。
func duplexHuman(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "full", "full_duplex", "fullduplex":
		return "全双工"
	case "half", "half_duplex", "halfduplex":
		return "半双工"
	case "auto_full":
		return "自动协商（全双工）"
	case "auto_half":
		return "自动协商（半双工）"
	case "auto", "":
		return ""
	}
	return v
}

// humanBytes 把字节数说成人话（35376428045 → 32.9 GB）。
//
// 按 1024 进制（KB/MB/GB），跟家用路由器的口径一致；小于 1 KB 就报字节数。
// 解析不出来（空串、N/A）就返回空，界面显示 -。
func humanBytes(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return ""
	}
	const unit = 1024
	if n < unit {
		return strconv.FormatUint(n, 10) + " B"
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + units[i]
}
