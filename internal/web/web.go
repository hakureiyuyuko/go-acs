// Package web 提供最小的查看界面与 JSON API。
//
// 刻意不引入任何前端构建链（无 npm/vite）：模板 + 一点原生 JS，
// 单二进制直接带着页面走。
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/i18n"
	"github.com/hakureiyuyuko/go-acs/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Controller 是 ACS 的控制能力，由 cwmp.Server 实现。
// 用接口是为了避免 web 包反向依赖 cwmp 包。
type Controller interface {
	// RequestRefresh 让设备重新上报基本信息。
	RequestRefresh(deviceID int64) error
	// FetchSubtree 枚举某个参数子树下的参数并把值取回来。
	FetchSubtree(deviceID int64, path string, exclude []string, max int) error
	// FetchWiFi 采集无线概况（看板上的 2.4G/5G 那一栏）。
	FetchWiFi(deviceID int64) error
	// FetchNames 只枚举参数名不取值（浏览参数树）。
	FetchNames(deviceID int64, path string, nextLevel bool) error
	// WakeDevice 主动唤醒设备（发 Connection Request），返回给用户看的一句话。
	WakeDevice(deviceID int64) (string, error)
	// Diagnose 下发一次 ping 诊断。iface 是可选承载接口（留空 = 设备自选）。
	Diagnose(deviceID int64, host string, count int, iface string) error
	// Reboot 下发一次重启（破坏性操作，界面上是红按钮 + 二次确认）。
	Reboot(deviceID int64) error
	// SetParameters 下发 SetParameterValues（改 WiFi 名字/密码/开关等）。
	SetParameters(deviceID int64, params []store.Param) error
}

// Server 是界面服务。
type Server struct {
	store *store.Store
	ctrl  Controller
	tpls  map[string]*template.Template // 每种语言一套（`{{T "..."}}` 在解析期就绑好语言）
	opt   Options
	log   *slog.Logger
}

// kv 是详情页里的一行「字段 - 值」。
type kv struct {
	K string
	V string
}

// Register 把界面路由挂到 mux 上。
// Options 是 Register 的可选项。
type Options struct {
	// Auth 是面板凭据（nil = 不启用）。它是可在线更新的对象：
	// 改账号密码立即生效，只有监听端口才需要重启。
	Auth *Creds
	// Runtime 是**当前进程实际在用**的监听/账号值：设置页拿它跟“保存后使用”的值对照。
	Runtime RuntimeSettings
	// Log 用来记面板登录等安全事件（nil = 不记）。
	Log *slog.Logger
	// Restart 由 cmd/acs 注入：让面板上「立即重启服务」能真把服务重启起来。
	// nil = 没有这个能力（比如测试里），界面上会退回「自己去命令行重启」的说法。
	// 进程这一侧的事（换进程、systemd 托管、回滚）都在 cmd/acs 里，web 只管界面。
	Restart func() (RestartMode, error)
}

// RestartMode 说明这次重启是用哪种方式完成的 —— 影响界面上怎么告诉用户
// 「接下来会发生什么」（systemd 拉起要等它几秒，自己起的新进程则是已经就绪）。
type RestartMode string

const (
	// RestartSystemd：进程干净退出，等 systemd（Restart=always）把它拉起来。
	RestartSystemd RestartMode = "systemd"
	// RestartSelf：自己起了新进程，新地址已经确认能访问。
	RestartSelf RestartMode = "self"
)

// Register 把面板路由挂到 mux 上。
func Register(mux *http.ServeMux, st *store.Store, ctrl Controller, opt Options) error {
	// 每种语言各解析一套模板：文案通过 {{T "..."}} 在解析期就绑定了语言，
	// 模板里不用每次渲染都做一次查找（也避免在模板里出现 call 之类的噪音）。
	tpls := make(map[string]*template.Template, len(i18n.Langs))
	for _, lang := range i18n.Langs {
		l := lang
		tpl, err := template.New("").Funcs(template.FuncMap{
			"fmtTime":        formatTime,
			"fmtTimeShort":   formatTimeShort,
			"uptime":         formatUptime,
			"dataModelLabel": dataModelLabel,
			"T": func(key string, args ...any) string {
				return i18n.T(l, key, args...)
			},
			// TS 翻译**后端拼出来的**字符串（任务结果、提示语、分组名…）：
			// 先精确查表，再用占位符做正则匹配，认不出就原样返回。
			"TS": func(text string) string {
				return i18n.TSmart(l, text)
			},
		}).ParseFS(assets, "templates/*.html")
		if err != nil {
			return fmt.Errorf("解析模板失败（%s）: %w", l, err)
		}
		tpls[l] = tpl
	}

	s := &Server{store: st, ctrl: ctrl, tpls: tpls, opt: opt, log: opt.Log}

	// 面板这一套路由统一走鉴权（CWMP 那套在 main 里单独挂，不受影响）。
	// Guard 每次请求都会读一遍当前凭据，所以设置页改完密码后立刻按新的校验。
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		if opt.Auth == nil {
			return h
		}
		g := opt.Auth.Guard(h)
		return func(w http.ResponseWriter, r *http.Request) { g.ServeHTTP(w, r) }
	}

	// 登录页与退出登录不鉴权（否则进不去 / 出不来）
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /logout", s.handleLogout)

	sub, err := fs.Sub(assets, "static")
	if err != nil {
		return err
	}
	// 静态资源不鉴权：登录页本身要用 style.css，而且里面没有业务数据。
	// 静态资源带 no-cache：每次回源确认。没做指纹/ETag 的话它等于每次重新拿，
	// 好处是改了 app.js / style.css 之后刷新就能用上，不会拿着旧脚本跑。
	fsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.StripPrefix("/static/", http.FileServerFS(sub)).ServeHTTP(w, r)
	})
	mux.Handle("GET /static/", fsHandler)

	mux.HandleFunc("GET /{$}", guard(s.handleIndex))
	mux.HandleFunc("GET /settings", guard(s.handleSettings))
	mux.HandleFunc("POST /settings", guard(s.handleSettingsSave))
	// 改完监听地址后「立即重启服务」：确认过才走这里（改端口必须重启才生效）
	mux.HandleFunc("POST /settings/restart", guard(s.handleSettingsRestart))
	mux.HandleFunc("GET /devices/{id}", guard(s.handleDevice))
	mux.HandleFunc("POST /devices/{id}/refresh", guard(s.handleRefresh))
	mux.HandleFunc("POST /devices/{id}/note", guard(s.handleDeviceNote))
	mux.HandleFunc("POST /devices/{id}/diagnose", guard(s.handleDiagnose))
	mux.HandleFunc("POST /devices/{id}/wake", guard(s.handleWake))
	mux.HandleFunc("POST /devices/{id}/reboot", guard(s.handleReboot))
	mux.HandleFunc("POST /devices/{id}/delete", guard(s.handleDeviceDelete))
	mux.HandleFunc("POST /devices/{id}/fetch", guard(s.handleFetch))
	mux.HandleFunc("POST /devices/{id}/wifi", guard(s.handleWifi))
	mux.HandleFunc("GET /devices/{id}/wifi/{inst}", guard(s.handleWifiEdit))
	mux.HandleFunc("POST /devices/{id}/wifi/{inst}", guard(s.handleWifiSave))
	mux.HandleFunc("GET /api/devices", guard(s.apiDevices))
	mux.HandleFunc("GET /api/devices/{id}", guard(s.apiDevice))
	mux.HandleFunc("POST /api/devices/{id}/fetch", guard(s.apiFetch))
	mux.HandleFunc("POST /api/devices/{id}/names", guard(s.apiFetchNames))
	mux.HandleFunc("POST /api/devices/{id}/wifi", guard(s.apiWifi))
	return nil
}

// renderStatus 同 renderLang，但可以指定 HTTP 状态码（登录失败要回 401）。
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	lang := s.langOf(w, r)
	tpl := s.tpls[lang]
	if tpl == nil {
		tpl = s.tpls[i18n.Default]
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "模板渲染失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 面板页面是实时数据（在线状态、任务进度），禁掉浏览器缓存 —— 否则改了界面/参数，
	// 打开还是旧的，很容易误判成「没生效」。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// render 把模板先渲染到内存、成功后再写出。
//
// 为什么不直接 ExecuteTemplate(w, ...)：模板执行到一半出错时，那样会把**半个页面**
// 发出去，错误文本还会混进 HTML 里（踩过：模板里调了一个签名不对的方法，
// 页面被截断、尾巴上多出一行 `template: ...: invalid function signature`）。
// 缓冲一下，出错就干干净净回 500。
// langOf 定这次请求用哪种语言，顺带把选择记进 cookie。
//
// 优先级：?lang=xx（点语言切换按钮）→ cookie → Accept-Language → 默认中文。
func (s *Server) langOf(w http.ResponseWriter, r *http.Request) string {
	if q := i18n.Normalize(r.URL.Query().Get("lang")); q != "" {
		http.SetCookie(w, &http.Cookie{
			Name: "acs_lang", Value: q, Path: "/",
			MaxAge: 365 * 24 * 3600, SameSite: http.SameSiteLaxMode,
		})
		return q
	}
	if ck, err := r.Cookie("acs_lang"); err == nil {
		if l := i18n.Normalize(ck.Value); l != "" {
			return l
		}
	}
	if l := i18n.Pick(r.Header.Get("Accept-Language")); l != "" {
		return l
	}
	return i18n.Default
}

// pageCommon 是所有页面都要的语言相关数据：当前语言、语言列表、切换器用得到的东西，
// 以及给前端 JS 用的一份译文（分页按钮那些文案在 app.js 里）。
func (s *Server) pageCommon(lang string) map[string]any {
	js := map[string]string{}
	for _, k := range jsKeys {
		js[k] = i18n.T(lang, k)
	}
	return map[string]any{
		"I18NJSON":      i18nJSON(js),
		"Lang":          lang,
		"Langs":         i18n.Langs,
		"LangNames":     i18n.LangNames,
		"OtherLang":     otherLang(lang),
		"OtherLangName": i18n.LangNames[otherLang(lang)],
		"I18N":          js,
	}
}

// i18nJSON 把译文打成可以直接嵌进 <script> 的 JSON 字面量。
func i18nJSON(m map[string]string) template.JS {
	b, err := json.Marshal(m)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

func otherLang(lang string) string {
	if lang == i18n.LangEN {
		return i18n.LangZH
	}
	return i18n.LangEN
}

// jsKeys 是 app.js 里用到的文案（服务端按语言注入到 window.I18N，
// 前端 t("原文") 查表；查不到就原样用中文）。
var jsKeys = []string{
	"自动刷新 %ds：关", "自动刷新 %ds：开",
	"🌙 切换到夜间", "☀ 切换到日间",
	"看到第几页 / 每页多少条",
	"‹ 上一页", "下一页 ›", "全部",
	"每页 %d 条", "共 %d 条 · 第 %d / %d 页", "共 %d 条",
}

// mergeCommon 把公共数据合进页面自己的数据里（页面数据优先）。
func mergeCommon(data map[string]any, common map[string]any) map[string]any {
	for k, v := range common {
		if _, ok := data[k]; !ok {
			data[k] = v
		}
	}
	return data
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	s.renderLang(w, nil, name, data)
}

// renderLang 按请求语言渲染（需要拿到 r 才能协商语言；老调用点走 render 默认中文）。
func (s *Server) renderLang(w http.ResponseWriter, r *http.Request, name string, data any) {
	lang := i18n.Default
	if r != nil {
		lang = s.langOf(w, r)
	}
	tpl := s.tpls[lang]
	if tpl == nil {
		tpl = s.tpls[i18n.Default]
	}
	// 语言相关的公共数据（当前语言、切换器、给 JS 的译文）统一在这里并进去，
	// 各页面自己的数据优先。
	if m, ok := data.(map[string]any); ok {
		data = mergeCommon(m, s.pageCommon(lang))
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "模板渲染失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 面板页面是实时数据（在线状态、任务进度），禁掉浏览器缓存 —— 否则改了界面/参数，
	// 打开还是旧的，很容易误判成「没生效」。
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(buf.Bytes()); err != nil {
		// 客户端断了，没什么可做的
		_ = err
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListDevices()
	if err != nil {
		http.Error(w, "读取设备列表失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	stats, err := s.store.Stats()
	if err != nil {
		http.Error(w, "读取统计失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 无线概况 + 主机光功率：一次查询拿全（两者都只能按名字子串筛，合并省一次扫描）
	wifiParams, opticalParams, err := s.store.SummaryParams()
	if err != nil {
		wifiParams, opticalParams = map[int64][]store.Param{}, map[int64][]store.Param{}
	}
	// 收光 / 发光：只在**列表里真有设备报过**这两列时才显示
	// （跟 FTTR / WAN 区块一个规矩：设备不报就不摆空列）
	aliases, err := s.store.EnabledAliases()
	if err != nil {
		aliases = nil
	}
	opticalByDevice := map[int64]HostOptical{}
	hasOptical := false
	for id, ps := range opticalParams {
		o := hostOpticalFrom(aliases, ps)
		if !o.Has() {
			continue
		}
		opticalByDevice[id] = o
		hasOptical = true
	}

	// 搜索：服务端过滤（结果可以分享 URL，也不依赖 JS）。
	// 搜序列号 / 备注 / 名称 / 产品类 / OUI / SSID。
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	// 在线状态筛选：all / online（含探测中）/ offline / probing。
	// 跟搜索一样走服务端过滤，URL 可以直接分享。
	state := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state")))
	switch state {
	case "online", "offline", "probing":
	default:
		state = "all"
	}
	devices := make([]*store.Device, 0, len(all))
	wifiByDevice := map[int64][]WifiBand{}
	// 「无线终端」列跟详情页用同一套口径：主机 + 子光猫上的已连终端之和
	// （条目数之外残留的行不算，见 BuildClientTree）。
	clientCounts := map[int64]int{}
	for _, d := range all {
		// 先算无线概况：按 SSID 搜索要用到它，表格里的「无线终端」也跟详情页同一口径
		// （主机 + 子光猫上的已连终端之和，条目数之外残留的行不算，见 BuildClientTree）。
		bands := WifiOverview(wifiParams[d.ID])
		tree := BuildClientTree(wifiParams[d.ID], nil)
		n := 0
		for _, g := range tree.Groups() {
			n += len(g.Clients)
		}
		// 筛掉的不进任何一份数据 —— 否则「按离线筛选」时下面的 WiFi 概览
		// 还会把在线设备的频段列出来
		if !deviceMatchesState(d, state) || !deviceMatches(d, bands, q) {
			continue
		}
		if len(bands) > 0 {
			wifiByDevice[d.ID] = bands
		}
		clientCounts[d.ID] = n
		devices = append(devices, d)
	}

	data := map[string]any{
		"Devices":      devices,
		"Stats":        stats,
		"WiFi":         wifiByDevice,
		"ClientCounts": clientCounts,
		"Optical":      opticalByDevice,
		"HasOptical":   hasOptical,
		"Query":        q,
		"State":        state,
		"AuthOn":       s.authEnabled(),
		"Total":        len(all),
		// 筛选条上的数字（在线含探测中，探测中是它的子集）
		"OnlineCount":  stats.Online,
		"OfflineCount": stats.Devices - stats.Online,
		"ProbingCount": stats.Probing,
		"Path":         r.URL.Path,
		// 删除设备等操作会带着提示回到首页
		"Notice":    strings.TrimSpace(r.URL.Query().Get("msg")),
		"NoticeErr": r.URL.Query().Get("err") == "1",
	}
	s.renderLang(w, r, "index.html", data)
}

// deviceMatches 判断设备是否命中搜索词。q 为空则全部命中。
//
// 搜的是「人能一眼看到的那些」：序列号、备注、名称/型号/厂商、产品类、OUI，
// 以及它广播的 SSID（按 SSID 找设备很常用）。
// deviceMatchesState 按在线状态筛设备。state 取值见 handleIndex。
func deviceMatchesState(d *store.Device, state string) bool {
	switch state {
	case "online":
		return d.Online
	case "offline":
		return !d.Online
	case "probing":
		return d.Probing()
	default:
		return true
	}
}

func deviceMatches(d *store.Device, bands []WifiBand, q string) bool {
	if q == "" {
		return true
	}
	q = strings.ToLower(q)
	hay := []string{
		d.SerialNumber, d.Note, d.DisplayName(), d.Manufacturer,
		d.ModelName, d.ProductClass, d.OUI,
	}
	for _, b := range bands {
		hay = append(hay, b.SSID, b.Band)
	}
	for _, h := range hay {
		if h != "" && strings.Contains(strings.ToLower(h), q) {
			return true
		}
	}
	return false
}

// diagTaskKind 是 ping 诊断的任务类型，必须与 cwmp.TaskDiagnostics 一致。
const diagTaskKind = "Diagnostics"

// handleWake 主动唤醒设备（发 Connection Request），让排队的任务立刻下发。
func (s *Server) handleWake(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := "/devices/" + strconv.FormatInt(id, 10)
	if s.ctrl == nil {
		http.Redirect(w, r, back+"?msg="+url.QueryEscape("未接入控制接口")+"&err=1", http.StatusSeeOther)
		return
	}
	msg, err := s.ctrl.WakeDevice(id)
	if err != nil {
		http.Redirect(w, r, back+"?msg="+url.QueryEscape(err.Error())+"&err=1", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleDiagnose 下发一次 ping 诊断。
func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := "/devices/" + strconv.FormatInt(id, 10)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, back+"?msg="+url.QueryEscape("表单解析失败")+"&err=1", http.StatusSeeOther)
		return
	}
	host := strings.TrimSpace(r.Form.Get("host"))
	count, _ := strconv.Atoi(strings.TrimSpace(r.Form.Get("count")))
	// 承载接口：可选。留空 = 由设备自己选出口；填了就让设备从那个接口出去
	// （真机上有的设备自己选的出口没有路由，见 cwmp.diagPayload.Interface）。
	iface := strings.TrimSpace(r.Form.Get("interface"))
	if s.ctrl == nil {
		http.Redirect(w, r, back+"?msg="+url.QueryEscape("未接入控制接口")+"&err=1", http.StatusSeeOther)
		return
	}
	if err := s.ctrl.Diagnose(id, host, count, iface); err != nil {
		http.Redirect(w, r, back+"?msg="+url.QueryEscape(err.Error())+"&err=1", http.StatusSeeOther)
		return
	}
	// 顺手主动唤醒一次：商用 ACS 就是这么做到“点完几秒出结果”的。
	// 唤醒失败也不影响正事（任务仍然会在下一次周期上报时下发），所以只把它当提示。
	msg := "诊断已入队，会在设备下次上报时下发"
	if wakeMsg, werr := s.ctrl.WakeDevice(id); werr == nil {
		msg = "诊断已入队。" + wakeMsg
	} else {
		msg += "（主动唤醒没成功：" + werr.Error() + "）"
	}
	http.Redirect(w, r, back+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleReboot 下发重启。
//
// 界面上这个按钮是红色的、且带二次确认（form 上的 data-confirm，见 app.js）；
// 后端这里不依赖确认框，只负责“不重复下发”和把结果说清楚。
func (s *Server) handleReboot(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := "/devices/" + strconv.FormatInt(id, 10)
	if s.ctrl == nil {
		http.Redirect(w, r, back+"?msg="+url.QueryEscape("未接入控制接口")+"&err=1", http.StatusSeeOther)
		return
	}
	if err := s.ctrl.Reboot(id); err != nil {
		http.Redirect(w, r, back+"?msg="+url.QueryEscape(err.Error())+"&err=1", http.StatusSeeOther)
		return
	}
	// 跟诊断一样：顺手主动唤醒一次。设备在周期上报的话等最多 120 秒，
	// 唤醒一下就能立刻下发。（唤醒失败不影响正事，只当提示）
	msg := "重启指令已入队，会在设备下次上报时下发"
	if wakeMsg, werr := s.ctrl.WakeDevice(id); werr == nil {
		msg = "重启指令已入队。" + wakeMsg + "；设备会断开重连，几分钟后回来"
	} else {
		msg += "（主动唤醒没成功：" + werr.Error() + "）"
	}
	http.Redirect(w, r, back+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleDeviceDelete 删掉一台设备的本地记录（详情页上的红色按钮 + 二次确认）。
//
// 删完回首页：设备详情页已经不存在了，留在原地会变成 404。
// 提示里必须说清“只删本地记录”：设备如果还配着本 ACS 的地址，下次上报会重新纳管。
func (s *Server) handleDeviceDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := s.store.GetDevice(id)
	if err != nil {
		http.Redirect(w, r, "/?msg="+url.QueryEscape("设备不存在，可能已经被删了")+"&err=1", http.StatusSeeOther)
		return
	}
	name := d.DisplayName()
	if err := s.store.DeleteDevice(id); err != nil {
		http.Redirect(w, r, "/devices/"+strconv.FormatInt(id, 10)+"?msg="+
			url.QueryEscape("删除失败："+err.Error())+"&err=1", http.StatusSeeOther)
		return
	}
	msg := "已删除「" + name + "」（只删本地记录：参数 / 任务 / 上报历史）。" +
		"设备若还配着本 ACS 地址，下次上报会重新纳管。"
	http.Redirect(w, r, "/?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleDeviceNote 保存设备备注。
func (s *Server) handleDeviceNote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.GetDevice(id); err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单解析失败", http.StatusBadRequest)
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	// 备注是给人看的，限长主要是防止把整篇文章塞进来
	if runes := []rune(note); len(runes) > 200 {
		note = string(runes[:200])
	}
	if err := s.store.SetDeviceNote(id, note); err != nil {
		http.Error(w, "保存备注失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/devices/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := s.store.GetDevice(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	params, err := s.store.ListParams(id)
	if err != nil {
		http.Error(w, "读取参数失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tasks, err := s.store.ListTasks(id, 100)
	if err != nil {
		http.Error(w, "读取任务失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Inform 记录很多（设备每 120 秒一条，一天就 720 条），
	// 一次多取一些给前端分页用
	informs, err := s.store.ListInforms(id, 100)
	if err != nil {
		http.Error(w, "读取上报记录失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	pending, _ := s.store.PendingTaskCount(id)

	wifiParams, err := s.store.WifiParams()
	if err != nil {
		wifiParams = map[int64][]store.Param{}
	}

	// 厂商私有参数 → 面板字段的映射（光模块读数用）。
	// 读不到就退回叶子名启发式，页面上少几行，不影响别的。
	aliases, err := s.store.EnabledAliases()
	if err != nil {
		aliases = nil
	}

	var diag *store.Task
	diagHost := ""
	diagIface := ""
	for _, t := range tasks {
		if t.Kind == diagTaskKind {
			diag = t // tasks 按 ID 倒序，第一条就是最近一次
			var p struct {
				Host      string `json:"host"`
				Interface string `json:"interface"`
			}
			if json.Unmarshal([]byte(t.Payload), &p) == nil {
				diagHost = p.Host
				diagIface = p.Interface
			}
			break
		}
	}

	// FTTR 子设备：**探测不到就整块不显示**（不是显示一个空区块）
	fttr, hasFttr := FttrOverview(params)
	// WAN 连接：同样，没这类参数就不渲染
	wan, hasWan := WanOverview(params)

	// 关联终端树：主机 + 各子设备。真机上主机自己一台终端都没有，终端全在子光猫上，
	// 所以「终端数」必须把子设备算进来，而且要点得开、能看出是谁连的。
	clients := BuildClientTree(params, fttr)
	bandClients, subClients := BuildClientViews(clients, fttr)
	subClientsBy := map[int]SubClientsView{}
	for _, v := range subClients {
		subClientsBy[v.Instance] = v
	}
	// 无线概况：终端列改成「主机 + 子机」的合计
	wifiBands := WifiOverview(wifiParams[id])
	WireBandClients(wifiBands, clients)

	data := map[string]any{
		"Device":             d,
		"Fttr":               fttr,
		"HasFttr":            hasFttr,
		"FttrOnline":         fttrOnlineCount(fttr),
		"FttrHasOptical":     fttrHasOptical(fttr),
		"Wan":                wan,
		"HasWan":             hasWan,
		"WanConnected":       wanConnCount(wan),
		"Diag":               diag,
		"DiagHost":           diagHost,
		"DiagIface":          diagIface,
		"DiagIfaces":         diagInterfaceOptions(wan),
		"DiagRunning":        diag != nil && (diag.Status == store.TaskRunning || diag.Status == store.TaskPending),
		"Notice":             strings.TrimSpace(r.URL.Query().Get("msg")),
		"NoticeErr":          r.URL.Query().Get("err") == "1",
		"Basic":              basicInfo(d, params, aliases),
		"Params":             params,
		"Tasks":              tasks,
		"TaskHistoryLimit":   s.store.TaskHistoryLimit(),
		"InformHistoryLimit": s.store.InformHistoryLimit(),
		"Informs":            informs,
		"Pending":            pending,
		"WiFi":               wifiBands,
		"BandClients":        bandClients,
		"SubClients":         subClientsBy,
		"SubClientsList":     subClients,
		"Path":               "/devices/" + strconv.FormatInt(id, 10),
		"AuthOn":             s.authEnabled(),
		// 表单默认值：按设备的数据模型根猜一个 WiFi 路径（只是默认值，用户可改）
		"DefaultPath": defaultFetchPath(d),
	}
	s.renderLang(w, r, "device.html", data)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.ctrl != nil {
		_ = s.ctrl.RequestRefresh(id)
	}
	http.Redirect(w, r, "/devices/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleWifiEdit 展示某个频段的无线编辑表单。
func (s *Server) handleWifiEdit(w http.ResponseWriter, r *http.Request) {
	id, inst, ok := s.deviceAndInst(w, r)
	if !ok {
		return
	}
	d, err := s.store.GetDevice(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	wifiParams, _ := s.store.WifiParams()
	all := wifiParams[id]

	bands := WifiOverview(all)
	var band *WifiBand
	for i := range bands {
		if bands[i].Instance == inst {
			band = &bands[i]
		}
	}
	if band == nil {
		http.NotFound(w, r)
		return
	}

	tasks, _ := s.store.ListTasks(id, 8)
	data := map[string]any{
		"Device": d,
		"Band":   band,
		"Fields": WifiForm(inst, all),
		"Tasks":  tasks,
		"Queued": r.URL.Query().Get("queued"),
		"Path":   "/devices/" + strconv.FormatInt(id, 10) + "/wifi/" + strconv.Itoa(inst),
	}
	s.renderLang(w, r, "wifi_edit.html", data)
}

// handleWifiSave 处理无线编辑表单的提交：把真正变了的字段拼成 SetParameterValues 入队。
//
// 只下发「变过的」字段，有两个好处：
//   - 不会把没动过的参数重新写一遍（少一次风险）；
//   - 密码留空就真的不碰（很多 CPE 不返回明文密码，本来就无法“改成一样”）。
func (s *Server) handleWifiSave(w http.ResponseWriter, r *http.Request) {
	id, inst, ok := s.deviceAndInst(w, r)
	if !ok {
		return
	}
	back := "/devices/" + strconv.FormatInt(id, 10) + "/wifi/" + strconv.Itoa(inst)

	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单解析失败", http.StatusBadRequest)
		return
	}

	wifiParams, _ := s.store.WifiParams()
	fields := WifiForm(inst, wifiParams[id])

	var sets []store.Param
	for _, f := range fields {
		if f.ReadOnly {
			continue
		}
		var newVal string
		switch f.Kind {
		case "bool":
			if r.Form.Get("h_"+f.Key) == "" {
				continue // 这个字段没渲染出来
			}
			if r.Form.Get(f.Key) != "" {
				newVal = "1"
			} else {
				newVal = "0"
			}
		case "password":
			newVal = strings.TrimSpace(r.Form.Get(f.Key))
			if newVal == "" {
				continue // 留空 = 不修改
			}
		default:
			v, present := r.Form[f.Key]
			if !present {
				continue
			}
			newVal = strings.TrimSpace(v[0])
		}
		if newVal == f.Value {
			continue // 没变不下发
		}
		sets = append(sets, store.Param{Name: f.Param, Value: newVal, ValueType: f.Type})
	}

	// 高级：直接指定任意参数
	if name := strings.TrimSpace(r.FormValue("adv_name")); name != "" {
		val := r.FormValue("adv_value")
		typ := r.FormValue("adv_type")
		if typ == "" {
			// 拿设备上已经知道的类型；不知道就按 string
			if known, found, err := s.store.GetParam(id, name); err == nil && found && known.ValueType != "" {
				typ = known.ValueType
			} else {
				typ = "string"
			}
		}
		sets = append(sets, store.Param{Name: name, Value: val, ValueType: typ})
	}

	if len(sets) == 0 {
		http.Redirect(w, r, back+"?queued=none", http.StatusSeeOther)
		return
	}
	if s.ctrl == nil {
		http.Error(w, "未接入控制接口", http.StatusInternalServerError)
		return
	}
	if err := s.ctrl.SetParameters(id, sets); err != nil {
		http.Error(w, "入队失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, back+"?queued="+strconv.Itoa(len(sets)), http.StatusSeeOther)
}

// deviceAndInst 解析路径里的设备 ID 与无线实例号。
func (s *Server) deviceAndInst(w http.ResponseWriter, r *http.Request) (int64, int, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, 0, false
	}
	inst, err := strconv.Atoi(r.PathValue("inst"))
	if err != nil {
		http.NotFound(w, r)
		return 0, 0, false
	}
	return id, inst, true
}

// handleWifi 处理界面上的「重新采集无线概况」按钮。
func (s *Server) handleWifi(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.ctrl != nil {
		_ = s.ctrl.FetchWiFi(id)
	}
	http.Redirect(w, r, "/devices/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) apiWifi(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSONError(w, fmt.Errorf("设备 ID 非法"), http.StatusBadRequest)
		return
	}
	if s.ctrl == nil {
		writeJSONError(w, fmt.Errorf("未接入控制接口"), http.StatusInternalServerError)
		return
	}
	if err := s.ctrl.FetchWiFi(id); err != nil {
		writeJSONError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"queued": true,
		"note":   "任务会在设备下次 Inform 时下发；只采集 SSID/开关/信道/标准/加密/终端数等摘要字段",
	})
}

// handleFetch 处理界面上的「读取参数子树」表单。
func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单解析失败", http.StatusBadRequest)
		return
	}
	path := strings.TrimSpace(r.FormValue("path"))
	exclude := splitList(r.FormValue("exclude"))
	max := 0
	if n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("max"))); err == nil {
		max = n
	}
	if path != "" && s.ctrl != nil {
		_ = s.ctrl.FetchSubtree(id, path, exclude, max)
	}
	http.Redirect(w, r, "/devices/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// apiFetchNames 是给脚本用的：POST /api/devices/{id}/names，只枚举参数名不取值。
//
//	{"path":"InternetGatewayDevice.","next_level":true}
//
// 用途是浏览参数树：先把某层有哪些对象列出来，再决定往哪个子树里钻。
// 真机上盲猜路径代价很高（猜错一次就是一整轮上报周期），所以这个能力很有用。
func (s *Server) apiFetchNames(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSONError(w, fmt.Errorf("设备 ID 非法"), http.StatusBadRequest)
		return
	}
	var req struct {
		Path      string `json:"path"`
		NextLevel bool   `json:"next_level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, fmt.Errorf("请求体不是合法 JSON: %w", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeJSONError(w, fmt.Errorf("path 不能为空"), http.StatusBadRequest)
		return
	}
	if s.ctrl == nil {
		writeJSONError(w, fmt.Errorf("未接入控制接口"), http.StatusInternalServerError)
		return
	}
	if err := s.ctrl.FetchNames(id, req.Path, req.NextLevel); err != nil {
		writeJSONError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"queued":     true,
		"path":       req.Path,
		"next_level": req.NextLevel,
		"note":       "只枚举名字不取值；任务会在设备下次 Inform 时下发",
	})
}

// apiFetch 是给脚本用的：POST /api/devices/{id}/fetch
//
//	{"path":"InternetGatewayDevice.LANDevice.1.WLANConfiguration.","exclude":["AssociatedDevice"],"max":200}
func (s *Server) apiFetch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSONError(w, fmt.Errorf("设备 ID 非法"), http.StatusBadRequest)
		return
	}
	var req struct {
		Path    string   `json:"path"`
		Exclude []string `json:"exclude"`
		Max     int      `json:"max"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, fmt.Errorf("请求体不是合法 JSON: %w", err), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeJSONError(w, fmt.Errorf("path 不能为空"), http.StatusBadRequest)
		return
	}
	if s.ctrl == nil {
		writeJSONError(w, fmt.Errorf("未接入控制接口"), http.StatusInternalServerError)
		return
	}
	if err := s.ctrl.FetchSubtree(id, req.Path, req.Exclude, req.Max); err != nil {
		writeJSONError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"queued":  true,
		"path":    req.Path,
		"exclude": req.Exclude,
		"max":     req.Max,
		"note":    "任务会在设备下次 Inform 时下发；枚举和取值在同一个会话里连着做",
	})
}

// splitList 把逗号/换行分隔的输入拆成列表。
func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(c rune) bool {
		return c == ',' || c == '\n' || c == ' '
	}) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) apiDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.ListDevices()
	if err != nil {
		writeJSONError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"data": devices, "count": len(devices)})
}

func (s *Server) apiDevice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSONError(w, fmt.Errorf("设备 ID 非法"), http.StatusBadRequest)
		return
	}
	d, err := s.store.GetDevice(id)
	if err != nil {
		writeJSONError(w, fmt.Errorf("设备不存在"), http.StatusNotFound)
		return
	}
	params, _ := s.store.ListParams(id)
	tasks, _ := s.store.ListTasks(id, 50)
	pending, _ := s.store.PendingTaskCount(id)
	informs, _ := s.store.ListInforms(id, 20)
	writeJSON(w, map[string]any{
		"device":        d,
		"params":        params,
		"tasks":         tasks,
		"pending_tasks": pending,
		"informs":       informs,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeJSONError(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// basicInfo 组装详情页顶部「最基本的设备信息」。
// 数据模型根未知时不做猜测，直接按已有参数原样展示。
func basicInfo(d *store.Device, params []store.Param, aliases []store.ParamAlias) []kv {
	idx := make(map[string]string, len(params))
	for _, p := range params {
		idx[p.Name] = p.Value
	}
	get := func(names ...string) string {
		for _, n := range names {
			if v, ok := idx[n]; ok && v != "" {
				return v
			}
		}
		return ""
	}

	root := d.DataModelRoot
	if root == "" {
		root = "InternetGatewayDevice."
	}

	out := []kv{
		{"厂商", d.Manufacturer},
		{"型号", d.ModelName},
		{"序列号", d.SerialNumber},
		{"OUI", d.OUI},
		{"ProductClass", d.ProductClass},
		{"软件版本", d.SoftwareVersion},
		{"硬件版本", d.HardwareVersion},
		{"Spec 版本", d.SpecVersion},
		{"ProvisioningCode", d.ProvisioningCode},
		{"运行时长", formatUptime(get(root + "DeviceInfo.UpTime"))},
		{"外网 IP", d.ExternalIP},
		{"上报周期", intervalText(d.PeriodicInterval)},
		{"ConnectionRequestURL", d.ConnRequestURL},
		{"数据模型", dataModelLabel(d.DataModelRoot)},
		{"最近事件", d.LastEvents},
		{"最后上报", formatTime(d.LastInformAt)},
		{"最后启动", formatTime(d.LastBootAt)},
		{"来源 IP", d.SourceIP},
		{"User-Agent", d.UserAgent},
	}

	// 空值不展示，避免一屏的 "-"
	kept := out[:0]
	for _, x := range out {
		if strings.TrimSpace(x.V) != "" && x.V != "-" {
			kept = append(kept, x)
		}
	}

	// 光模块读数（收光 / 发光 / 温度 / 电压 / 偏流）：单独放在最后。
	//
	// 显示口径：**只要这台设备报过其中任意一项，就把这几行都列出来**，没报的那几项写 -。
	// 一项都不报就整组不显示 —— 跟 FTTR / WAN 区块一个规矩，不给没有光口的设备
	// 摆五行动 `-` 的空壳。
	//
	// 取值走映射表（各家私有参数名 + 原始值换算），没登记过的机型退回叶子名启发式。
	fields := ResolvePanelFields(aliases, params)
	anyReported := false
	for _, f := range fields {
		if strings.TrimSpace(f.Value) != "" {
			anyReported = true
			break
		}
	}
	if anyReported {
		for _, f := range fields {
			kept = append(kept, kv{f.Field.Label, orDash(f.Value)})
		}
	}
	return kept
}

// orDash 空值显示成 "-"（如实呈现：没读到就是没读到，不编数）。
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// defaultFetchPath 给「读取参数子树」表单一个合理的默认值。
// 只是提示性的默认值，用户可以在界面上改。
func defaultFetchPath(d *store.Device) string {
	root := d.DataModelRoot
	if root == "" {
		root = "InternetGatewayDevice."
	}
	if root == "Device." {
		return "Device.WiFi."
	}
	return root + "LANDevice.1.WLANConfiguration."
}

func intervalText(sec int) string {
	if sec <= 0 {
		return ""
	}
	return (time.Duration(sec) * time.Second).String()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// formatTimeShort 只给时分秒，用在表格里节省宽度。
func formatTimeShort(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("15:04:05")
}

// formatUptime 把 TR-069 的秒数格式化成人看的。
// 直接接受字符串，因为 CPE 上报的 UpTime 就是字符串。
// dataModelLabel 把设备的数据模型根写成使用者看得懂的名字。
// 认不出来就原样显示 —— 不硬猜（有的设备根路径是厂商私有的）。
func dataModelLabel(root string) string {
	switch {
	case strings.HasPrefix(root, "InternetGatewayDevice."):
		return "TR-098"
	case strings.HasPrefix(root, "Device."):
		return "TR-181"
	}
	return root
}

func formatUptime(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	sec, err := strconv.ParseInt(v, 10, 64)
	if err != nil || sec < 0 {
		return v
	}
	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%d 天 %d 小时 %d 分", d, h, m)
	case h > 0:
		return fmt.Sprintf("%d 小时 %d 分", h, m)
	default:
		return fmt.Sprintf("%d 分 %d 秒", m, sec%60)
	}
}
