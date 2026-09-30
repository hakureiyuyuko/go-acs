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
	// HasCandidate 表示设备**报过**疑似光功率的参数（哪怕值不合常理被弃用了）。
	// 用来区分「设备压根没报」和「报了但我们不敢显示」—— 这两种在界面上
	// 都是 "-"，但排查方向完全不同。
	HasCandidate bool
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
	sub    int
	// won 表示这个候选最终被采用了。
	won bool
	// note 说明为什么没被采用（空 = 采用了）。诊断页要展示它，
	// 否则「设备明明报了参数，界面却是 -」这种现场无从下手。
	note string
	// scaled 是「疑似私有单位」时的换算建议（如设备值 385 → 3.85 dBm）。
	// 只是**建议**，不会自动套用 —— 猜单位等于编数，界面上只给线索。
	scaled string
}

// 未采用的原因（这些串会走 {{TS}} 翻译，所以必须是固定的整句，不能拼进变量）
const (
	noteImplausible = "值超出光模块的合理范围（-40~+10 dBm）"
	noteLowerScore  = "同一方向有更像光功率的参数"
	noteStale       = "同一方向有更新的读数"
	noteHostWins    = "主机自己已上报，子设备的不参与"
)

// opticalGroup 是「同一台设备（主机 or 某台子设备）的同一个方向」这一组候选。
type opticalGroup struct {
	sub  int
	rx   bool
	hits []opticalHit
}

// winner 返回这组里被采用的候选；没有可用的返回 nil。
func (g *opticalGroup) winner() *opticalHit {
	if g == nil {
		return nil
	}
	for i := range g.hits {
		if g.hits[i].won {
			return &g.hits[i]
		}
	}
	return nil
}

// pick 在这组候选里选出采用者，并给落选的写上原因。
//
// 顺序：先剔除物理上不可能的值（它们仍然留在 hits 里，只是被标注，诊断页要看）；
// 剩下的按「分值高 → 时间新」取优。
func (g *opticalGroup) pick() {
	best := -1
	for i := range g.hits {
		h := &g.hits[i]
		if !opticalUnitPlausible(h.value) {
			h.note = noteImplausible
			h.scaled = opticalScalingHint(h.value)
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		b := &g.hits[best]
		switch {
		case h.score > b.score:
			b.note = noteLowerScore
			best = i
		case h.score == b.score && h.upd.After(b.upd):
			b.note = noteStale
			best = i
		default:
			if h.score == b.score {
				h.note = noteStale
			} else {
				h.note = noteLowerScore
			}
		}
	}
	if best >= 0 {
		g.hits[best].won = true
		g.hits[best].note = ""
	}
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
	return f >= -40 && f <= 10
}

// opticalScalingHint 给「疑似私有单位」的值算一个换算建议。
//
// 有些厂家不用 dBm 直接报数，而是报 0.1 / 0.01 / 0.001 dBm 的整数
// （例：设备值 385 其实是 3.85 dBm）；收光功率是负数，有的固件又把它
// 当无符号数报（例：16687 其实是 -16.687 dBm）。所以这里正负两种都试：
// **只有换算后恰好落进光模块合理范围**才给出建议，否则返回空 —— 猜不出就不猜。
//
// 返回的只是提示文案，**不会**被自动套用到显示值上 —— 猜单位等于编数。
func opticalScalingHint(v string) string {
	// 本来就合理的读数不用换算（这个函数只在值不合理时被调用，这里是兜底）
	if opticalUnitPlausible(v) {
		return ""
	}
	s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "dBm"))
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f == 0 {
		return ""
	}
	// 换算后的值要落进**更紧**的区间（-30~+10）：宽松区间会把
	// 385÷10=-38.5 这种勉强够得着的也当成建议，反而更误导。
	// 每个除数先看正解再看负解（收光为负、有些固件当无符号数报，两种都试），
	// 命中就返回 —— 只给一个最可能的，不罗列一堆让人自己挑。
	for _, div := range []float64{10, 100, 1000} {
		q := f / div
		if q >= -30 && q <= 10 {
			return "÷" + strconv.FormatFloat(div, 'f', 0, 64) + " ≈ " +
				strconv.FormatFloat(q, 'f', 2, 64) + " dBm"
		}
		if -q >= -30 && -q <= 10 {
			return "−÷" + strconv.FormatFloat(div, 'f', 0, 64) + " ≈ " +
				strconv.FormatFloat(-q, 'f', 2, 64) + " dBm"
		}
	}
	return ""
}

// collectOpticalGroups 把参数里所有「疑似光功率」的读数按
// 「设备（主机/子设备）+ 方向（收/发）」分组收集起来。
//
// 注意：值物理上不可能的**也收进来**（只是后面不选它）—— 诊断页要能看到
// 「设备到底报了什么」，否则像中兴那种「报了但值离谱」的现场无从排查。
func collectOpticalGroups(params []store.Param) []*opticalGroup {
	idx := map[string]*opticalGroup{}
	var order []*opticalGroup
	group := func(sub int, rx bool) *opticalGroup {
		k := strconv.Itoa(sub) + ":" + strconv.FormatBool(rx)
		if g := idx[k]; g != nil {
			return g
		}
		g := &opticalGroup{sub: sub, rx: rx}
		idx[k] = g
		order = append(order, g)
		return g
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
		sub := 0
		if m := subOwnerRe.FindStringSubmatch(p.Name); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				sub = n
			}
		}
		h := opticalHit{
			rx:    rx,
			tx:    tx,
			value: v,
			name:  p.Name,
			upd:   p.UpdatedAt,
			score: opticalScore(p.Name, leaf, rx, tx),
			sub:   sub,
		}
		g := group(sub, rx)
		g.hits = append(g.hits, h)
	}
	return order
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
	groups := collectOpticalGroups(params)
	for _, g := range groups {
		g.pick()
	}
	find := func(sub int, rx bool) *opticalGroup {
		for _, g := range groups {
			if g.sub == sub && g.rx == rx {
				return g
			}
		}
		return nil
	}

	hostRxG, hostTxG := find(0, true), find(0, false)
	hostRx, hostTx := hostRxG.winner(), hostTxG.winner()

	// 有没有任何候选（哪怕被弃用）—— 用来区分「没报」和「报了不敢显示」
	for _, g := range groups {
		if len(g.hits) > 0 {
			out.HasCandidate = true
			break
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

	// 主机没有 → 退到子设备。多台子设备都报了就取实例号最小的那台：
	// params 的顺序取决于库里的排序，不排一下的话「哪台子设备」会随查询顺序变，
	// 界面上同一个值一会儿标 1 一会儿标 2。
	var subs []int
	for _, g := range groups {
		if g.sub <= 0 {
			continue
		}
		dup := false
		for _, s := range subs {
			if s == g.sub {
				dup = true
				break
			}
		}
		if !dup {
			subs = append(subs, g.sub)
		}
	}
	sort.Ints(subs)
	for _, inst := range subs {
		rxG, txG := find(inst, true), find(inst, false)
		rxH, txH := rxG.winner(), txG.winner()
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

// OpticalCandidate 是诊断页要展示的一条「疑似光功率」记录。
//
// 各家参数名不统一、私有字段又多，「设备报了但界面显示 -」这种现场
// 光看读数查不出原因，所以把判定过程摊开：读到哪些参数、各自什么值、
// 谁被采用、没被采用的因为什么。
type OpticalCandidate struct {
	Name   string
	Value  string
	Dir    string // "rx" / "tx"
	Score  int
	Won    bool
	Note   string // 为什么没被采用（空 = 采用了 / 或主机优先时子设备的说明）
	Scaled string // 疑似私有单位时的换算建议（空 = 没有）
	Sub    int    // 0 = 主机
}

// OpticalCandidates 列出这台设备所有疑似光功率的参数及其判定结果（诊断用）。
//
// 按「是否采用 → 分值」排序，采用的那条排最前，方便一眼看到读的是哪个。
func OpticalCandidates(params []store.Param) []OpticalCandidate {
	groups := collectOpticalGroups(params)
	for _, g := range groups {
		g.pick()
	}
	// 主机已经报了的，子设备那组整组不参与（跟 OpticalOverview 一个口径），
	// 但要给它们写上原因 —— 不然诊断页上会看到一堆「没被采用」却没理由。
	hostRxG, hostTxG := (*opticalGroup)(nil), (*opticalGroup)(nil)
	for _, g := range groups {
		if g.sub != 0 {
			continue
		}
		if g.rx && hostRxG == nil {
			hostRxG = g
		}
		if !g.rx && hostTxG == nil {
			hostTxG = g
		}
	}
	hostWon := (hostRxG != nil && hostRxG.winner() != nil) ||
		(hostTxG != nil && hostTxG.winner() != nil)

	var out []OpticalCandidate
	for _, g := range groups {
		dir := "tx"
		if g.rx {
			dir = "rx"
		}
		for i := range g.hits {
			h := &g.hits[i]
			note := h.note
			if hostWon && g.sub != 0 && note == "" {
				note = noteHostWins
			}
			out = append(out, OpticalCandidate{
				Name:   h.name,
				Value:  h.value,
				Dir:    dir,
				Score:  h.score,
				Won:    h.won && !(hostWon && g.sub != 0),
				Note:   note,
				Scaled: h.scaled,
				Sub:    h.sub,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Won != out[j].Won {
			return out[i].Won
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// opticalPick 在两个候选里挑一个：没值就让位，分值高者胜，同分取更新更晚的。
//
// 跟 OpticalOverview 用的是同一套口径（子设备区块和概览页必须一致，
// 否则同一个数在两处显示不一样）。
func opticalPick(cur, h opticalHit) opticalHit {
	switch {
	case cur.value == "":
		return h
	case h.score > cur.score:
		return h
	case h.score == cur.score && h.upd.After(cur.upd):
		return h
	}
	return cur
}
