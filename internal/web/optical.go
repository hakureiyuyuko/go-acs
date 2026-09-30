package web

import (
	"strings"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// HostOptical 是**主机自己**的收光 / 发光功率（详情页「基本信息」里那两行）。
//
// 子设备（FTTR 从光猫）的光功率不在这里 —— 那属于各台子设备，在 FTTR 表格里按行显示。
// 真机上主机的收光与子光猫的收光差得远，混在一起会误导人。
type HostOptical struct {
	Rx string // 显示用（如 "-21.50 dBm"）；没读到就是空
	Tx string
	// RxName / TxName 是读数来自哪个参数（排查「显示 -」时用得上）。
	RxName string
	TxName string
}

// Has 表示读到了任何一个方向的功率。
func (o HostOptical) Has() bool {
	return strings.TrimSpace(o.Rx) != "" || strings.TrimSpace(o.Tx) != ""
}

// opticalDenyPath 里的路径不参与主机光功率判定 —— 它们是**无线**的功率参数，
// 名字里同样带 power，混进来就会把发射功率当发光功率显示。
//
// 踩过的坑：设备 1（HN8145X6N）有 `LANDevice.1.WiFi.X_HW_Txpower`（无线 30），
// 用「叶子名 = x_hw_txpower」去认会正好命中。
var opticalDenyPath = []string{
	".wlanconfiguration.", // 无线配置（TransmitPower 在这里）
	".wifi.",              // 华为私有的无线参数（X_HW_Txpower / X_HW_PowerValue）
	"x_hw_apdevice.",      // FTTR 子光猫（它们的功率是各自子设备的，不算主机）
	"x_hw_ap.",
	".associateddevice.", // 终端表
	".hosts.",            // 终端列表
	"x_hw_wlanpowervalue",
	"transmitpowersupported",
}

// opticalGoodPath 里的路径更像「光口」，同一方向命中多个时优先它。
var opticalGoodPath = []string{"pon", "optical", "opm", ".optic"}

// hostOpticalFrom 从设备已采集的参数里认出主机自己的收光 / 发光。
//
// 口径跟 FTTR 子设备那套一致（复用 opticalField：按**叶子名**认，不猜具体参数名）：
//  1. 叶子名认得出是收/发光功率（RxPower / OpticalRxPower / X_HW_RxPower…）；
//  2. 排除无线相关路径（见 opticalDenyPath）；
//  3. 值必须在光模块讲得通的范围内（-40~+10 dBm）—— 厂家私有的百分比、原始 ADC 值
//     （真机上见过 385、16687）不能当功率显示；
//  4. 同一方向多个候选：优先路径里有 pon/optical 的，其次取先遇到的。
//
// 都认不出就返回空，界面不显示这两行或显示 -，**不编数字**。
// hostOpticalFrom 从设备已采集的参数里认出主机自己的收光 / 发光。
//
// 优先走**映射表**（store.ParamAlias：各家私有参数名 + 原始值换算，验证过的机型都在库里），
// 没命中任何映射的新机型退回兜底启发式（按叶子名认方向，见 heuristicValue）。
// 两条路都不成就是空，界面显示 -，**不编数字**。
func hostOpticalFrom(aliases []store.ParamAlias, params []store.Param) HostOptical {
	var out HostOptical
	for _, rf := range ResolvePanelFields(aliases, params) {
		switch rf.Field.Key {
		case "rx_power":
			out.Rx, out.RxName = rf.Value, rf.Name
		case "tx_power":
			out.Tx, out.TxName = rf.Value, rf.Name
		}
	}
	return out
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// opticalValuePlausible 判这个读数像不像光模块的收/发光功率。
//
// 光功率大致落在 -40 ~ +10 dBm（再高要烧模块，再低基本收不到光）。
// 落在这个区间外的，多半不是功率本身（百分比、原始寄存器值），不显示。
//
// 注意：现在这道闸的主战场搬到了 ResolvePanelFields（每个字段都有自己的区间）；
// 这个函数留给 FTTR 子设备那套与测试用。
func opticalValuePlausible(v string) bool {
	f, ok := decodeRaw("identity", v)
	if !ok {
		return false
	}
	return f >= -40 && f <= 10
}
