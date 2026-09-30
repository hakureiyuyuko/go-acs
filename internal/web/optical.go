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
	Rx string // 收光（带单位，如 "-19.00 dBm"）；没读到就是空
	Tx string // 发光
	// SourceName 是读数来自哪个参数（界面上 hover 能看到，排查时才说得清读的是什么）。
	SourceName string
	// SourceText 说明这读数是从哪来的：主机自己上报的为空（不用解释），
	// 只有「主机没报、拿的是子设备的」才要写清楚 —— 免得把子光猫的光功率
	// 当成主机的看（真机上这两者差得远）。
	SourceText string
	Updated    time.Time
}

// Has 表示这台设备读到了任何光功率。
func (o OpticalPower) Has() bool { return o.Rx != "" || o.Tx != "" }

// opticalHit 是一次「疑似光功率参数」的候选。
//
// 【为什么要打分而不是取第一个命中】真机上同一台猫会同时存在好几个带 power 的参数
// （厂家私有的百分比、端口状态、光模块能力值……），而**查出来的顺序取决于数据库排序**，
// 不是设备的物理含义。之前谁先被遍历到就用谁，于是同一台设备刷新几次可能显示不同的数。
// 实测中兴 ZXHN F610GV9 显示成 385 dBm / 16687 dBm —— 那是读到了私有的原始值。
// 现在按「参数名有多像标准光功率」打分排序，分高的优先，同分再比更新时间。
type opticalHit struct {
	rx, tx bool
	value  string
	name   string
	upd    time.Time
	score  int
}

// better 判断现有候选 cur 是否应当被新候选 other 顶替。
//
// 规则：cur 还没有值 → 让位；分值不同 → 高分者胜；
// 分值相同 → 取更新时间更晚的（重采过的读数才是新的）；
// 都一样 → 保留先到的（结果稳定，不会来回跳）。
func (h *opticalHit) better(other opticalHit) bool {
	if h.value == "" {
		return true
	}
	if other.score != h.score {
		return other.score > h.score
	}
	return other.upd.After(h.upd)
}

// opticalScore 给光功率候选参数打分：越像标准的收/发光功率分越高。
//
// 只看**设备自己的参数名**，不碰值、不猜值 —— 判不出高低的一律 0 分，
// 退化成"最先到的那条"这一原有行为。
func opticalScore(fullName, leaf string, rx, tx bool) int {
	norm := opticalNorm(fullName)
	nLeaf := opticalNorm(leaf)

	score := 0
	// 名字里直接写了 optical/pon 的，比只有 rxpower/txpower 的更可信
	if strings.Contains(norm, "optical") || strings.Contains(norm, "pon") {
		score += 8
	}
	// 收/发方向写全的（RxPower / ReceiveOpticalPower）比只有 rx 的强
	switch {
	case rx && strings.Contains(nLeaf, "rxpower"):
		score += 6
	case tx && strings.Contains(nLeaf, "txpower"):
		score += 6
	case rx && strings.Contains(nLeaf, "receive"):
		score += 4
	case tx && strings.Contains(nLeaf, "transmit"):
		score += 4
	}
	// 叶子名就是标准写法（OpticalRxPower / RxPower）的，最高优先
	switch nLeaf {
	case "opticalrxpower", "opticaltxpower", "rxpower", "txpower":
		score += 4
	}
	// 明确是光功率的容器层级（TR-098 WANPONInterfaceConfig、标准 Optical.Interface.{i}.）
	if strings.Contains(norm, "wanponinterfaceconfig") || strings.Contains(norm, "opticalinterface") {
		score += 3
	}
	// 负分项：明显不是「读数」的参数，避免被私有字段顶掉真正的功率值
	for _, bad := range []string{
		"threshold", // 告警门限
		"alarm",     // 告警状态
		"attenuat",  // 衰减量
		"percent",   // 厂家私有的百分比
		"ratio",     // 比值
		"raw",       // 原始寄存器值
		"status",    // 状态
		"enable",    // 开关
		"support",   // 能力位
		"voltage",   // 电压
		"current",   // 电流
		"temperature",
	} {
		if strings.Contains(norm, bad) {
			score -= 10
		}
	}
	return score
}

// opticalNorm 把参数名归一化后用于匹配（去掉大小写、下划线、连字符、空格）。
func opticalNorm(s string) string {
	return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(s)))
}

// opticalUnitPlausible 判读数在物理上讲不讲得通。
//
// 光模块收/发光功率的正常范围大致在 -40 ~ +10 dBm 之间
// （再高要烧模块，再低根本收不到光）。明显跑出这个区间的，多半不是功率读数本身
// （常见于厂家私有的百分比、原始 ADC 值），不能当光功率显示。
func opticalUnitPlausible(v string) bool {
	s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "dBm"))
	if s == "" {
		return false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return false
	}
	return f >= -60 && f <= 30
}

// OpticalOverview 从设备已采集的参数里取出光功率。
//
// 取值口径（跟 FTTR 区块一致：宁可显示「没读到」，也不要编一个数）：
//  1. 优先**主机自己**的光功率参数（排除子设备 X_HW_APDevice.{i} 那些）；
//  2. 主机一个都没报，但 FTTR 子设备报了，就取第一台有读数的子设备，
//     并在 SourceText 里注明是哪一台 —— 概览页那一格 hover 能看到；
//  3. 都没有就返回空值，界面显示 "-"。
//
// 判定复用的就是 FTTR 那套 opticalField（按叶子名认，容忍各家写法，
// 且不会把无线的 TransmitPower 当成发光功率）。
//
// 同一方向（收 or 发）命中多个参数时，按 opticalScore 取最像标准光功率的那个，
// 而不是遍历顺序里的第一个 —— 理由见 opticalHit 的注释。
func OpticalOverview(params []store.Param) OpticalPower {
	var out OpticalPower
	// subInst > 0 表示这是第几台子设备；0 = 主机直属参数
	var hostRx, hostTx *opticalHit
	var subs []int
	subRx := map[int]*opticalHit{}
	subTx := map[int]*opticalHit{}

	consider := func(cur **opticalHit, h opticalHit) {
		if *cur == nil || (*cur).better(h) {
			c := h
			*cur = &c
		}
	}
	// map 里的指针不能取地址（Go 不允许 &m[k]），单开一个函数处理
	considerMap := func(m map[int]*opticalHit, inst int, h opticalHit) {
		cur := m[inst]
		if cur == nil || cur.better(h) {
			c := h
			m[inst] = &c
		}
	}
	seenSub := func(inst int) {
		for _, s := range subs {
			if s == inst {
				return
			}
		}
		subs = append(subs, inst)
	}

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
		// 值不可能是光功率的（百分比、原始 ADC 值、状态码）直接排除，
		// 免得它靠更新时间把真正的读数顶掉。
		if !opticalUnitPlausible(v) {
			continue
		}
		h := opticalHit{
			rx:    rx,
			tx:    tx,
			value: v,
			name:  p.Name,
			upd:   p.UpdatedAt,
			score: opticalScore(p.Name, leaf, rx, tx),
		}

		sub := 0
		if m := subOwnerRe.FindStringSubmatch(p.Name); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				sub = n
			}
		}
		if sub == 0 {
			if rx {
				consider(&hostRx, h)
			} else {
				consider(&hostTx, h)
			}
			continue
		}
		seenSub(sub)
		if rx {
			considerMap(subRx, sub, h)
		} else {
			considerMap(subTx, sub, h)
		}
	}

	if hostRx != nil || hostTx != nil {
		if hostRx != nil {
			out.Rx = withDbm(hostRx.value)
			out.SourceName = hostRx.name
			out.Updated = hostRx.upd
		}
		if hostTx != nil {
			out.Tx = withDbm(hostTx.value)
			if hostTx.upd.After(out.Updated) {
				out.Updated = hostTx.upd
			}
			if out.SourceName == "" {
				out.SourceName = hostTx.name
			}
		}
		return out
	}
	// 多台子设备都报了就取实例号最小的那台：params 的顺序取决于库里的排序，
	// 不排一下的话「哪台子设备」会随查询顺序变，界面上同一个值一会儿标 1 一会儿标 2。
	sort.Ints(subs)
	for _, inst := range subs {
		rxH, txH := subRx[inst], subTx[inst]
		if rxH == nil && txH == nil {
			continue
		}
		if rxH != nil {
			out.Rx = withDbm(rxH.value)
			out.SourceName = rxH.name
			out.Updated = rxH.upd
		}
		if txH != nil {
			out.Tx = withDbm(txH.value)
			if txH.upd.After(out.Updated) {
				out.Updated = txH.upd
			}
			if out.SourceName == "" {
				out.SourceName = txH.name
			}
		}
		// 文案在 Go 侧拼好，模板里走 {{TS}} 翻译
		out.SourceText = "来自子设备 " + strconv.Itoa(inst)
		return out
	}
	return out
}
