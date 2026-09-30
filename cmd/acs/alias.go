package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hakureiyuyuko/go-acs/internal/store"
	"github.com/hakureiyuyuko/go-acs/internal/web"
)

// `acs alias …`：管理「厂商私有参数 → 面板字段」的映射表。
//
// 为什么要有这个命令：各家光猫的参数名、位置、单位都不一样，
// 支持一台新光猫就是把它的参数名与换算规则加进库 —— 不该为此改代码、发版。
// 界面上暂时没做编辑器（映射是低频操作），命令行够用，也能写进脚本。
//
// 用法：
//
//	acs alias ls [--db acs.db]
//	acs alias add --field rx_power --suffix .rxpower [--contains opticaltransceiver.] \
//	              [--decode identity] [--priority 10] [--vendor 中兴] [--note 真机验证…] [--off]
//	acs alias rm <id>
//	acs alias kinds
func runAlias(args []string) error {
	if len(args) == 0 {
		aliasUsage()
		return fmt.Errorf("缺少子命令")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return aliasList(rest)
	case "add":
		return aliasAdd(rest)
	case "rm", "del", "delete":
		return aliasRemove(rest)
	case "kinds":
		fmt.Println("支持的换算规则（--decode）：")
		for _, k := range web.DecodeKinds() {
			fmt.Println("  ", k, aliasDecodeHelp(k))
		}
		return nil
	case "-h", "--help", "help":
		aliasUsage()
		return nil
	default:
		aliasUsage()
		return fmt.Errorf("不认识的子命令: %s", sub)
	}
}

func aliasUsage() {
	fmt.Fprint(os.Stderr, `用法：
  acs alias ls [--db acs.db]                     列出全部映射
  acs alias add --field <字段> (--suffix <后缀> | --contains <片段>) [选项]
        --field     面板字段：rx_power / tx_power / temperature / voltage / bias_current
        --suffix    参数名（小写）后缀，如 .rxpower
        --contains  参数名（小写）必须包含的片段，如 opticaltransceiver.
        --decode    原始值换算，见 acs alias kinds（默认 identity）
        --priority  同一字段命中多条时数字大的优先（默认 0）
        --vendor    机型/厂商备注（只给人看）
        --note      说明：在哪台机器上验证过
        --off       加进来但先禁用
  acs alias rm <id>                              删掉一条（id 从 ls 里看）
  acs alias kinds                                列出支持的换算规则

例：给一台中兴光猫加收光映射（参数名形如 …Optical.Interface.1.RxPower）
  acs alias add --field rx_power --contains optical.interface. --suffix .rxpower \
      --vendor 中兴 --priority 5 --note "ZXHN F610GV9 待验证"
`)
}

func aliasDecodeHelp(kind string) string {
	switch kind {
	case "identity":
		return "原样（设备报的就是真实值）"
	case "dbm_01uw":
		return "光功率寄存器：值 × 0.1 µW → dBm（254 → -15.95）"
	case "div256":
		return "光模块温度寄存器：值 ÷ 256 → ℃（11008 → 43.0）"
	case "mv":
		return "毫伏 → V（3226 → 3.226）"
	case "mv01":
		return "0.1 mV → V（32260 → 3.226）"
	case "ua2":
		return "2 µA 为单位 → mA（14500 → 29.0）"
	}
	return ""
}

func aliasOpen(dbPath string) (*store.Store, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("打不开数据库 %s：%v", dbPath, err)
	}
	return store.Open(dbPath)
}

func aliasList(args []string) error {
	fs := flag.NewFlagSet("alias ls", flag.ContinueOnError)
	dbPath := fs.String("db", "acs.db", "SQLite 文件路径（跟服务端用同一个）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := aliasOpen(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	all, err := st.ParamAliases()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Println("（映射表是空的）")
		return nil
	}
	fmt.Printf("%-4s %-13s %-9s %-26s %-18s %-9s %-5s %s\n",
		"id", "字段", "厂商", "参数名包含", "参数名后缀", "换算", "优先", "启用")
	for _, a := range all {
		en := "是"
		if !a.Enabled {
			en = "否"
		}
		fmt.Printf("%-4d %-13s %-9s %-26s %-18s %-9s %-5d %s\n",
			a.ID, a.Field, a.Vendor, orEmpty(a.MatchContains), orEmpty(a.MatchSuffix),
			a.Decode, a.Priority, en)
		if a.Note != "" {
			fmt.Printf("     └ %s\n", a.Note)
		}
	}
	return nil
}

func aliasAdd(args []string) error {
	fs := flag.NewFlagSet("alias add", flag.ContinueOnError)
	dbPath := fs.String("db", "acs.db", "SQLite 文件路径（跟服务端用同一个）")
	field := fs.String("field", "", "面板字段（rx_power / tx_power / temperature / voltage / bias_current）")
	contains := fs.String("contains", "", "参数名必须包含的片段（小写）")
	suffix := fs.String("suffix", "", "参数名后缀（小写）")
	decode := fs.String("decode", "identity", "原始值换算规则（acs alias kinds 看全部）")
	priority := fs.Int("priority", 0, "同一字段命中多条时数字大的优先")
	vendor := fs.String("vendor", "", "机型/厂商备注")
	note := fs.String("note", "", "说明：在哪台机器上验证过")
	off := fs.Bool("off", false, "先禁用（留库备查）")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// 换算规则得认得出来，免得库里存个拼错的、运行时静默失效
	known := false
	for _, k := range web.DecodeKinds() {
		if strings.EqualFold(strings.TrimSpace(*decode), k) {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("不认识的换算规则 %q（可用：%s）", *decode, strings.Join(web.DecodeKinds(), " / "))
	}

	st, err := aliasOpen(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	a := &store.ParamAlias{
		Field:         strings.TrimSpace(*field),
		Vendor:        strings.TrimSpace(*vendor),
		MatchContains: strings.TrimSpace(*contains),
		MatchSuffix:   strings.TrimSpace(*suffix),
		Decode:        strings.ToLower(strings.TrimSpace(*decode)),
		Priority:      *priority,
		Note:          strings.TrimSpace(*note),
		Enabled:       !*off,
	}
	if err := st.UpsertParamAlias(a); err != nil {
		return err
	}
	fmt.Printf("已写入映射：%s 匹配「包含 %q + 后缀 %q」→ %s（优先级 %d）\n",
		a.Field, a.MatchContains, a.MatchSuffix, a.Decode, a.Priority)
	fmt.Println("面板刷新后即可看到效果（读的是库里这张表）。")
	return nil
}

func aliasRemove(args []string) error {
	fs := flag.NewFlagSet("alias rm", flag.ContinueOnError)
	dbPath := fs.String("db", "acs.db", "SQLite 文件路径（跟服务端用同一个）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("用法：acs alias rm <id>（id 从 acs alias ls 里看）")
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		return fmt.Errorf("id 得是数字：%v", err)
	}
	st, err := aliasOpen(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.DeleteParamAlias(id); err != nil {
		return err
	}
	fmt.Printf("已删除映射 #%d\n", id)
	return nil
}

func orEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
