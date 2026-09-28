package web

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"acs/internal/store"
)

// WanLink 是一条 WAN 连接（TR-098 的 WANIPConnection / WANPPPConnection，
// 或 TR-181 的 PPP.Interface）。
type WanLink struct {
	Instance   int
	Path       string // 完整实例前缀，界面上当标题兜底用
	Container  string // WANIPConnection / WANPPPConnection / Interface
	Name       string
	Status     string
	Enabled    string
	IP         string
	Mask       string
	Gateway    string
	MAC        string
	Addressing string
	NAT        string
	Service    string
	VLAN       string
	Uptime     string
	Updated    time.Time
}

// wanLinkRe 抠出「实例前缀 + 字段名」。
//
// 两种结构都要认：
//   - TR-098：InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANIPConnection.1.<字段>
//   - TR-181：Device.PPP.Interface.1.<字段>（字段名里还带点，如 IPCP.LocalIPAddress）
var (
	wanRe098  = regexp.MustCompile(`(?i)^(.*WANConnectionDevice\.\d+\.(WANIPConnection|WANPPPConnection)\.\d+)\.(.+)$`)
	wanRe181  = regexp.MustCompile(`(?i)^(Device\.PPP\.Interface\.\d+)\.(.+)$`)
	wanInstRe = regexp.MustCompile(`(\d+)$`)
)

// WanOverview 从设备已采集的参数里解析 WAN 连接列表。
//
// ok=false 表示**这台设备没有 WAN 连接信息**，界面整块不渲染。
func WanOverview(params []store.Param) ([]WanLink, bool) {
	byKey := map[string]*WanLink{}

	for _, p := range params {
		var prefix, field, container string
		if m := wanRe098.FindStringSubmatch(p.Name); m != nil {
			prefix, container, field = m[1], m[2], m[3]
		} else if m := wanRe181.FindStringSubmatch(p.Name); m != nil {
			prefix, container, field = m[1], "Interface", m[2]
		} else {
			continue
		}

		link := byKey[prefix]
		if link == nil {
			inst := 0
			if m := wanInstRe.FindStringSubmatch(prefix); m != nil {
				inst, _ = strconv.Atoi(m[1])
			}
			link = &WanLink{Path: prefix, Container: container, Instance: inst}
			byKey[prefix] = link
		}
		if p.UpdatedAt.After(link.Updated) {
			link.Updated = p.UpdatedAt
		}

		v := strings.TrimSpace(p.Value)
		switch {
		case strings.HasSuffix(strings.ToLower(field), "connectionstatus"),
			strings.HasSuffix(strings.ToLower(field), "status"):
			link.Status = v
		case strings.HasSuffix(strings.ToLower(field), "externalipaddress"),
			strings.HasSuffix(strings.ToLower(field), "ipcp.localipaddress"):
			link.IP = v
		case strings.HasSuffix(strings.ToLower(field), "subnetmask"):
			link.Mask = v
		case strings.HasSuffix(strings.ToLower(field), "defaultgateway"),
			strings.HasSuffix(strings.ToLower(field), "ipcp.remoteipaddress"):
			link.Gateway = v
		case strings.HasSuffix(strings.ToLower(field), "macaddress"):
			link.MAC = v
		case strings.HasSuffix(strings.ToLower(field), "addressingtype"):
			link.Addressing = v
		case strings.HasSuffix(strings.ToLower(field), "natenabled"):
			if v == "1" {
				link.NAT = "NAT"
			} else {
				link.NAT = "-"
			}
		case strings.HasSuffix(strings.ToLower(field), "servicelist"),
			strings.HasSuffix(strings.ToLower(field), "x_hw_service"):
			link.Service = v
		case strings.HasSuffix(strings.ToLower(field), "x_hw_vlan"):
			link.VLAN = v
		case strings.HasSuffix(strings.ToLower(field), "uptime"):
			link.Uptime = v
		case strings.HasSuffix(strings.ToLower(field), "enable"):
			link.Enabled = v
		case strings.EqualFold(field, "Name"):
			link.Name = v
		}
	}

	if len(byKey) == 0 {
		return nil, false
	}
	// byKey 的 key 是字符串，map 遍历无序，得排一下才稳定
	out := make([]WanLink, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, true
}

// WanLabel 给一条连接起个能看懂的标题：优先用设备自报的 Name，
// 没有就退回实例路径（真机上有的设备 Name 是空的）。
func (w WanLink) WanLabel() string {
	if strings.TrimSpace(w.Name) != "" {
		return w.Name
	}
	return w.Path
}

// WanKind 把容器名翻成人话。
func (w WanLink) WanKind() string {
	switch {
	case strings.Contains(w.Path, "WANPPPConnection"):
		return "PPP"
	case strings.Contains(w.Path, "WANIPConnection"):
		return "IP"
	case strings.Contains(w.Path, "PPP.Interface"):
		return "PPP"
	}
	return w.Container
}

// wanConnCount 统计状态为 Connected 的条数（给标题小字用）。
func wanConnCount(links []WanLink) int {
	n := 0
	for _, l := range links {
		if strings.EqualFold(l.Status, "Connected") {
			n++
		}
	}
	return n
}
