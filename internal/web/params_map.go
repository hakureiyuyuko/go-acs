package web

import (
	"math"
	"strconv"
	"strings"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 面板字段（光模块读数）：面板上要展示的那几行。
//
// key 必须与数据库 `param_aliases.field` 一致 —— 各家光猫的私有参数名、位置、单位
// 都在那张表里映射到这些规范名（见 store.ParamAlias 的说明）。
// 加一个新字段（比如「光模块类型」）才需要动这里的代码；支持一台新光猫只加数据。
type PanelField struct {
	Key      string
	Label    string // 中文标签（也是 i18n 的 key）
	Unit     string
	Decimals int
	// Min / Max 是「物理上讲得通」的区间：解码后落在外面的值不显示。
	// 这道闸同时挡住了把别家私有原始值硬套刻度解出来的离谱数字。
	Min, Max float64
}

var panelFields = []PanelField{
	{Key: "rx_power", Label: "收光", Unit: "dBm", Decimals: 2, Min: -40, Max: 10},
	{Key: "tx_power", Label: "发光", Unit: "dBm", Decimals: 2, Min: -40, Max: 10},
	{Key: "temperature", Label: "光模块温度", Unit: "℃", Decimals: 1, Min: -40, Max: 120},
	{Key: "voltage", Label: "光模块电压", Unit: "V", Decimals: 3, Min: 0, Max: 10},
	{Key: "bias_current", Label: "光模块偏流", Unit: "mA", Decimals: 2, Min: 0, Max: 200},
}

// panelFieldByKey 找字段定义。
func panelFieldByKey(key string) (PanelField, bool) {
	for _, f := range panelFields {
		if f.Key == key {
			return f, true
		}
	}
	return PanelField{}, false
}

// ResolvedField 是某台设备上的一个面板字段读数。
type ResolvedField struct {
	Field PanelField
	Value string // 显示值（含单位）；空 = 这台设备没读到
	Name  string // 读数来源的参数名（排查用）
	Raw   string // 解码前的原始值（排查用）
}

// ResolvePanelFields 按映射表从设备参数里取出面板字段。
//
// 规则：
//  1. 每台设备、每个字段：遍历该设备的参数，找命中的映射（`store.ParamAlias.Matches`），
//     按 priority 高者胜 —— 同一条参数也可能被多条映射命中，取其中最高的；
//  2. 命中后按自己的换算规则解码（如光模块寄存器原始值 254 → -15.95 dBm），
//     解码结果必须落在字段的物理区间内，否则这条不算（继续找下一条映射）；
//  3. 一个字段一条映射都没命中时，收光/发光退回**兜底启发式**（按叶子名认方向），
//     这样没登记过的新机型也还能显示出大致读数，不至于空着。
//
// 返回值与 panelFields 同序，方便模板直接按顺序渲染。
func ResolvePanelFields(aliases []store.ParamAlias, params []store.Param) []ResolvedField {
	out := make([]ResolvedField, 0, len(panelFields))
	for _, f := range panelFields {
		rf := ResolvedField{Field: f}
		if hit, ok := resolveByAlias(aliases, params, f); ok {
			rf.Value, rf.Name, rf.Raw = hit.value, hit.name, hit.raw
		} else if v, name, ok := heuristicValue(f.Key, params); ok {
			rf.Value, rf.Name = formatField(f, v), name
		}
		out = append(out, rf)
	}
	return out
}

type aliasHit struct {
	value string
	name  string
	raw   string
}

// resolveByAlias 用映射表取一个字段。
func resolveByAlias(aliases []store.ParamAlias, params []store.Param, f PanelField) (aliasHit, bool) {
	var best aliasHit
	bestPrio := math.MinInt
	for _, p := range params {
		if strings.TrimSpace(p.Value) == "" {
			continue
		}
		prio := math.MinInt
		var decoded float64
		matched := false
		for _, a := range aliases {
			if !a.Enabled || a.Field != f.Key || !a.Matches(p.Name) {
				continue
			}
			v, ok := decodeRaw(a.Decode, p.Value)
			if !ok || v < f.Min || v > f.Max {
				continue // 解不出来、或解出来不在物理区间里（多半是别家的私有原始值）
			}
			if !matched || a.Priority > prio {
				prio, decoded, matched = a.Priority, v, true
			}
		}
		if !matched {
			continue
		}
		if best.value == "" || prio > bestPrio {
			best = aliasHit{value: formatField(f, decoded), name: p.Name, raw: strings.TrimSpace(p.Value)}
			bestPrio = prio
		}
	}
	if best.value == "" {
		return aliasHit{}, false
	}
	return best, true
}

// heuristicValue 兜底：没有登记映射时，按叶子名认收/发光（老逻辑，只做这两个字段）。
//
// 为什么保留：映射表只登记了我们验证过的机型，新机型一堆 —— 那些设备的
// RxPower / X_HW_RxPower 照样能认出来（只是拿不到原始值换算的精度）。
func heuristicValue(fieldKey string, params []store.Param) (float64, string, bool) {
	if fieldKey != "rx_power" && fieldKey != "tx_power" {
		return 0, "", false
	}
	wantRx := fieldKey == "rx_power"
	f, ok := panelFieldByKey(fieldKey)
	if !ok {
		return 0, "", false
	}
	bestScore, bestVal, bestName := -1, 0.0, ""
	found := false
	for _, p := range params {
		rx, tx := opticalField(p.Name)
		if (wantRx && !rx) || (!wantRx && !tx) {
			continue
		}
		low := strings.ToLower(p.Name)
		if containsAny(low, opticalDenyPath) {
			continue // 无线发射功率、子光猫的那些都不能算主机头上
		}
		v, ok := decodeRaw("identity", p.Value)
		if !ok || v < f.Min || v > f.Max {
			continue
		}
		score := 1
		if containsAny(low, opticalGoodPath) {
			score = 2
		}
		if !found || score > bestScore {
			bestScore, bestVal, bestName, found = score, v, p.Name, true
		}
	}
	if !found {
		return 0, "", false
	}
	return bestVal, bestName, true
}

// decodeRaw 按换算规则把「设备报的原始值」变成面板值。
//
// 光模块寄存器（SFF-8472）那套编码踩过一次坑，所以规则都写死在这里、
// 库里只存规则名：
//
//	identity  原样（设备已经报的是真实值，只是可能精度低）
//	dbm_01uw  光功率：寄存器值 × 0.1 µW → dBm（254 → -15.95 dBm；10000 → 0.00 dBm）
//	div256    光模块温度：寄存器值 ÷ 256 → ℃
//	mv        伏：毫伏 → V
//	mv01      伏：0.1 mV → V（÷10000）
//	ua2       电流：2 µA 为单位 → mA（÷500）
func decodeRaw(kind, raw string) (float64, bool) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "dBm")
	s = strings.TrimSuffix(s, "℃")
	s = strings.TrimSuffix(s, "mA")
	s = strings.TrimSpace(s)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "identity":
		return v, true
	case "dbm_01uw":
		if v <= 0 {
			return 0, false
		}
		return 10*math.Log10(v) - 40, true
	case "div256":
		return v / 256, true
	case "mv":
		return v / 1000, true
	case "mv01":
		return v / 10000, true
	case "ua2":
		return v / 500, true
	}
	return 0, false // 不认识的规则：不猜
}

// formatField 按字段的小数位与单位格式化。
func formatField(f PanelField, v float64) string {
	return strconv.FormatFloat(v, 'f', f.Decimals, 64) + " " + f.Unit
}

// DecodeKinds 列出支持的换算规则名（命令行工具与文档用）。
func DecodeKinds() []string {
	return []string{"identity", "dbm_01uw", "div256", "mv", "mv01", "ua2"}
}
