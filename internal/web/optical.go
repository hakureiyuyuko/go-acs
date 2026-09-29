package web

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// OpticalPower 是一台设备的光功率读数（概览页的「收光 / 发光」两列）。
type OpticalPower struct {
	Rx string // 收光（带单位，如 "-19.0 dBm"）；没读到就是空
	Tx string // 发光
	// SourceText 说明这读数是从哪来的：主机自己上报的为空（不用解释），
	// 只有「主机没报、拿的是子设备的」才要写清楚 —— 免得把子光猫的光功率
	// 当成主机的看（真机上这两者差得远）。
	SourceText string
	Updated    time.Time
}

// Has 表示这台设备读到了任何光功率。
func (o OpticalPower) Has() bool { return o.Rx != "" || o.Tx != "" }

// OpticalOverview 从设备已采集的参数里取出光功率。
//
// 取值口径（跟 FTTR 区块一致：宁可显示「没读到」，也不要编一个数）：
//  1. 优先**主机自己**的光功率参数（排除子设备 X_HW_APDevice.{i}. 那些）；
//  2. 主机一个都没报，但 FTTR 子设备报了，就取第一台有读数的子设备，
//     并在 SourceText 里注明是哪一台 —— 概览页那一格 hover 能看到；
//  3. 都没有就返回空值，界面显示 "-"。
//
// 判定复用的就是 FTTR 那套 opticalField（按叶子名认，容忍各家写法，
// 且不会把无线的 TransmitPower 当成发光功率）。
func OpticalOverview(params []store.Param) OpticalPower {
	var out OpticalPower
	// subInst > 0 表示这是第几台子设备；0 = 主机直属参数
	type hit struct {
		rx, tx string
		sub    int
		upd    time.Time
	}
	var hostRx, hostTx string
	var hostUpd time.Time
	var subs []hit
	subByInst := map[int]*hit{}

	for _, p := range params {
		leaf := p.Name
		if i := strings.LastIndex(leaf, "."); i >= 0 {
			leaf = leaf[i+1:]
		}
		rx, tx := opticalField(leaf)
		if !rx && !tx {
			continue
		}
		v := strings.TrimSpace(p.Value)
		if v == "" {
			continue
		}

		sub := 0
		if m := subOwnerRe.FindStringSubmatch(p.Name); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				sub = n
			}
		}
		if sub == 0 {
			if rx {
				if hostRx == "" {
					hostRx = v
				}
			} else if hostTx == "" {
				hostTx = v
			}
			if p.UpdatedAt.After(hostUpd) {
				hostUpd = p.UpdatedAt
			}
			continue
		}
		h := subByInst[sub]
		if h == nil {
			subs = append(subs, hit{sub: sub})
			h = &subs[len(subs)-1]
			subByInst[sub] = h
		}
		if rx {
			if h.rx == "" {
				h.rx = v
			}
		} else if h.tx == "" {
			h.tx = v
		}
		if p.UpdatedAt.After(h.upd) {
			h.upd = p.UpdatedAt
		}
	}

	if hostRx != "" || hostTx != "" {
		out.Rx = withDbm(hostRx)
		out.Tx = withDbm(hostTx)
		out.Updated = hostUpd
		return out
	}
	// 多台子设备都报了就取实例号最小的那台：params 的顺序取决于库里的排序，
	// 不排一下的话「哪台子设备」会随查询顺序变，界面上同一个值一会儿标 1 一会儿标 2。
	sort.Slice(subs, func(i, j int) bool { return subs[i].sub < subs[j].sub })
	for _, h := range subs {
		if h.rx == "" && h.tx == "" {
			continue
		}
		out.Rx = withDbm(h.rx)
		out.Tx = withDbm(h.tx)
		out.Updated = h.upd
		// 文案在 Go 侧拼好，模板里走 {{TS}} 翻译
		out.SourceText = "来自子设备 " + strconv.Itoa(h.sub)
		return out
	}
	return out
}
