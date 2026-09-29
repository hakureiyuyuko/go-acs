package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/i18n"
)

// 概览页设备表有哪些列可以由使用者自己挑。
//
// 为什么需要：同一套 ACS 有人要盯着「最后上报」排障、有人只关心序列号和版本、
// 有人想一眼看到收发光功率 —— 全列出来表格太宽（真机上十来列就得横向滚），
// 全砍掉又总有人抱怨少了某一列。所以做成「可勾选」，并把选择记进 cookie。
//
// 名称列与操作列**不参与勾选**：名称是进详情页的唯一入口，操作列的「采集 WiFi」
// 是概览页唯一能触发的下发动作，砍掉等于这页废了。
//
// Label 用的是中文原文（跟 i18n 的约定一致：中文本身就是 key），
// 模板里走 {{TS $c.Label}} 翻译。
type ColDef struct {
	Key   string // URL / cookie 里用的短名，改名等于改用户的选择，别乱动
	Label string
	Hint  string // 表头 hover 提示（可选）
	On    bool   // 默认是否显示
}

// overviewColDefs 的顺序就是表格里的列顺序。
var overviewColDefs = []ColDef{
	{Key: "status", Label: "状态", On: true},
	{Key: "note", Label: "备注", On: true},
	{Key: "serial", Label: "序列号", On: true},
	{Key: "sw", Label: "软件版本", On: true},
	{Key: "model", Label: "数据模型", On: true},
	{Key: "lastinform", Label: "最后上报", On: true},
	{Key: "params", Label: "参数", On: true},
	{Key: "ip", Label: "上报 IP", On: true},
	{Key: "clients", Label: "无线终端", On: true},
	{Key: "rx", Label: "收光", On: true, Hint: "设备上报的接收光功率（没上报显示 -）"},
	{Key: "tx", Label: "发光", On: true, Hint: "设备上报的发送光功率（没上报显示 -）"},
}

// colDefsFor 给模板用的列定义（已按当前语言译好标签与提示）。
//
// 在这里翻译而不是在模板里 {{T $c.Label}}：模板函数 T 只认字面量 key，
// 而列定义是数据驱动的一张表 —— 交给 Go 侧翻完再塞给模板最省事，
// 也保证英文页面里不会漏出中文。
func colDefsFor(lang string) []ColDef {
	out := make([]ColDef, 0, len(overviewColDefs))
	for _, c := range overviewColDefs {
		out = append(out, ColDef{
			Key:   c.Key,
			Label: i18n.T(lang, c.Label),
			Hint:  i18n.T(lang, c.Hint),
			On:    c.On,
		})
	}
	return out
}

func defaultCols() map[string]bool {
	out := make(map[string]bool, len(overviewColDefs))
	for _, c := range overviewColDefs {
		if c.On {
			out[c.Key] = true
		}
	}
	return out
}

func allCols() map[string]bool {
	out := make(map[string]bool, len(overviewColDefs))
	for _, c := range overviewColDefs {
		out[c.Key] = true
	}
	return out
}

// colsCookie 是「显示项」选择存的地方。
//
// 为什么不存库：这是每个浏览器的**个人偏好**（跟主题、自动刷新开关一个性质），
// 存服务端反而要把「谁」这个概念引进来 —— 面板现在只有一个账号。
const colsCookie = "acs_cols"

// parseCols 把 "status,serial,rx" 解析成勾选表。
//
// 不认识的 key 直接丢掉：老版本存的 cookie 里可能有已经下线的列，
// 带着它进模板只会多出一列空表头。
func parseCols(raw string) map[string]bool {
	raw = strings.TrimSpace(raw)
	switch raw {
	case "":
		return nil // 调用方据此决定「回落到默认」还是「一列都不勾」
	case "all":
		return allCols()
	case "default":
		return defaultCols()
	}
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		k := strings.TrimSpace(part)
		for _, c := range overviewColDefs {
			if c.Key == k {
				out[k] = true
			}
		}
	}
	return out
}

// formatCols 按固定列序拼回字符串（便于比较 / 存 cookie / 放进 URL）。
//
// 列序取 overviewColDefs 的顺序而不是 map 遍历顺序 —— map 顺序不稳定，
// 会让同一个选择在不同请求里拼出不同的串（cookie 反复重写，看着像没生效）。
func formatCols(cols map[string]bool) string {
	parts := make([]string, 0, len(overviewColDefs))
	for _, c := range overviewColDefs {
		if cols[c.Key] {
			parts = append(parts, c.Key)
		}
	}
	return strings.Join(parts, ",")
}

// resolveCols 定这次请求显示哪些列，顺带把 URL 上的选择记进 cookie。
//
// 优先级：?cols=xx（点「应用」或分享链接）→ cookie → 默认。
// URL 里带了 cols 就覆盖 cookie：这样分享出去的链接在别人浏览器里也是同样的列。
// cols=none 是合法的「一列都不勾」（只剩名称与操作），所以要看参数**在不在**，
// 而不是看它空不空。
func (s *Server) resolveCols(w http.ResponseWriter, r *http.Request) map[string]bool {
	q := r.URL.Query()
	if q.Has("cols") {
		cols := parseCols(q.Get("cols"))
		if cols == nil {
			cols = map[string]bool{}
		}
		http.SetCookie(w, &http.Cookie{
			Name: colsCookie, Value: formatCols(cols), Path: "/",
			MaxAge: 365 * 24 * int(time.Hour/time.Second), SameSite: http.SameSiteLaxMode,
		})
		return cols
	}
	if ck, err := r.Cookie(colsCookie); err == nil {
		if cols := parseCols(ck.Value); cols != nil {
			return cols
		}
	}
	return defaultCols()
}

// wantOptical 判断要不要去库里捞光功率（没勾这两列就别查）。
func wantOptical(cols map[string]bool) bool { return cols["rx"] || cols["tx"] }
