package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/i18n"
	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// setParamTaskKind 必须与 cwmp.TaskSetParameterValues 一致（web 不反向依赖 cwmp）。
// 批量下发完要在结果表里回显「这条任务现在什么状态」，靠它去 tasks 里认。
const setParamTaskKind = "SetParameterValues"

// batchRunKey 是「最近一次批量下发的结果」存在 settings 表里的键。
//
// 为什么落到库里而不是塞在 URL  / 重定向参数里：结果要能**刷新后还在看**
// （任务状态是几秒后才会变的，用户肯定会刷），而设备多选时 URL 会很长。
// settings 是现成的键值表，单用户面板够用。
const batchRunKey = "batch_wifi_last"

// BatchField 是批量表单里的一个「可改字段」。
//
// 跟单设备页（WifiForm）的区别：批量表单面对的是**一批设备**，每台设备的
// 参数名 / 候选值都不一样，所以这里只给「字段 + 支持度」，真正要写哪个参数名
// 留到提交时按每台设备的实报参数去解。
type BatchField struct {
	Key     string
	Label   string
	Kind    string // text | password | bool | select
	Options []WifiOption
	// Support / Combos：这个字段在多少个「设备 × 频段」组合上真的存在。
	// 展示出来是为了让人心里有数 —— 一次性给 50 台设备改信道，
	// 其中有 3 台根本没这个参数时，不该默默少改了 3 台。
	Support int
	Combos  int
}

// BatchTarget 是一台被选中的设备及其要下发的频段。
type BatchTarget struct {
	Device *store.Device
	Bands  []WifiBand
}

// BatchResult 是「批量下发」里一台设备的结果。
type BatchResult struct {
	DeviceID int64
	Name     string
	Bands    string
	Count    int    // 下发了多少个参数
	Skipped  string // 跳过原因（空 = 已入队）
	Err      string // 入队失败的原因
	Wake     string // 主动唤醒的结果（失败也只当提示）
	// Task 是这台设备最近一条 SetParameterValues 任务（回显用，可能为 nil）。
	// 不存进 JSON：任务状态会变，每次展示都要重新读。
	Task *store.Task `json:"-"`
}

// batchRun 是存进 settings 的那一份结果快照。
type batchRun struct {
	At time.Time `json:"at"`
	// Queued 是真正入队的设备台数（结果表很长时不用一条条数）。
	Queued  int           `json:"queued"`
	Results []BatchResult `json:"results"`
}

// parseIDs 解析多选设备 ID。
//
// 两种写法都收：`ids=1&ids=2`（原生 form 复选）与 `ids=1,2,3`（分享链接 / 脚本）。
// 重复的去重，非法的值丢掉 —— 用户手改 URL 不该把页面搞崩。
func parseIDs(raw []string) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, part := range raw {
		for _, p := range strings.Split(part, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			id, err := strconv.ParseInt(p, 10, 64)
			if err != nil || id <= 0 || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// filterBands 按「频段范围」过滤。band 为空或 all 表示不过滤。
//
// 比的是**频段标签**（2.4G / 5G）而不是实例号：真机上 2.4G 是实例 1、5G 是实例 5，
// 而且不同型号还不统一，按实例号选等于让使用者去背每台设备的编号。
func filterBands(bands []WifiBand, band string) []WifiBand {
	band = strings.TrimSpace(band)
	if band == "" || band == "all" {
		return bands
	}
	var out []WifiBand
	for _, b := range bands {
		if b.Label == band {
			out = append(out, b)
		}
	}
	return out
}

// batchFieldValue 取出表单里某个字段要写的值，以及这一项到底改不改。
//
// 返回 write=false 表示「这一项不动」：密码留空、文本框留空、布尔项选了「不改」，
// 都是这个意思 —— 批量下发最怕手滑把几十台设备的 SSID 清空，所以空值一律不下发。
//
// 【布尔项是三态，不再拆成两个复选框】
//
//	值 "1" → 启用
//	值 "0" → 关闭
//	空 / 没这个参数 → 不改
//
// 为什么必须三态：早先是「改」和「值」两个并列的独立复选框，
// 只勾「改」不勾值时服务端分不出「想关」还是「忘勾」—— 于是前端把这种提交
// 拦下来弹窗，结果**批量关不掉无线**（只能开），和单设备页（取消勾选就写 0）
// 不一致。用一个 select 让使用者明确三选一，服务端就不用猜，前端也不用拦。
//
// 非布尔字段仍然沿用「先勾『改』再填值」：那种字段的空值是有意义的
// （比如信道留空 = 不碰），两个控件分工清楚。
func batchFieldValue(form url.Values, def wifiFieldDef) (string, bool) {
	key := "v_" + def.key
	if def.kind == "bool" {
		switch strings.TrimSpace(form.Get(key)) {
		case "1":
			return "1", true
		case "0":
			return "0", true
		}
		return "", false // 空 / 没提交 = 不改
	}
	// 其它字段：先勾「改」才算表态
	if _, on := form["use_"+def.key]; !on {
		return "", false
	}
	v := strings.TrimSpace(form.Get(key))
	if v == "" {
		return "", false
	}
	return v, true
}

// batchSets 拼出一台设备这次要写的全部参数。
//
// 只写「勾了且真的存在且值变了」的字段，理由跟单设备页一致：
// 不把没动过的参数重写一遍，密码留空就真的不碰。
// missing 记下「勾了但这台设备没这个参数」的字段名，界面上要说清楚。
func batchSets(form url.Values, wifiParams []store.Param, bands []WifiBand) (sets []store.Param, missing []string) {
	seenName := map[string]bool{}
	seenMissing := map[string]bool{}
	for _, b := range bands {
		fields := WifiForm(b.Instance, wifiParams)
		for _, def := range wifiFieldDefs {
			// 布尔项的「改不改」由它自己的三态 select 决定；
			// 其它字段要先勾「改」（use_）才算表态。
			v, write := batchFieldValue(form, def)
			if !write {
				continue // 没表态 = 不改
			}
			f, ok := findWifiField(fields, def.key)
			if !ok {
				if !seenMissing[def.label] {
					seenMissing[def.label] = true
					missing = append(missing, def.label)
				}
				continue
			}
			// 布尔字段（启用 / 射频开关这类）**不跳过「值没变化」**。
			//
			// 理由：用户勾上「启用无线 SSID」就是想确保它是开着的，
			// 此时设备原值也常常就是 1 —— 如果按「没变化」跳过，界面上就是
			// 「勾了、点了下发、什么都没发生」，看着纯粹是功能坏了。
			// 幂等地重写一次没有任何副作用，所以布尔项一律下发。
			// 其它字段（SSID / 信道等）仍然只在真的变了才写，少动设备。
			if def.kind != "bool" && v == f.Value {
				continue
			}
			if seenName[f.Param] {
				continue
			}
			seenName[f.Param] = true
			typ := f.Type
			if typ == "" {
				// 参数类型是写回去时要带的 xsi:type，空着会让设备那边解析不出类型。
				// 设备没告诉我们类型时按 string 兜底（跟入库时的兜底一致）。
				typ = "string"
			}
			sets = append(sets, store.Param{Name: f.Param, Value: v, ValueType: typ})
		}
	}
	return sets, missing
}

func findWifiField(fields []WifiFormField, key string) (WifiFormField, bool) {
	for _, f := range fields {
		if f.Key == key {
			return f, true
		}
	}
	return WifiFormField{}, false
}

// handleWifiBatch 展示「批量下发 WiFi 参数」页面。
func (s *Server) handleWifiBatch(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.ListDevices()
	if err != nil {
		http.Error(w, "读取设备列表失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ids := parseIDs(r.URL.Query()["ids"])
	selected := map[int64]bool{}
	for _, id := range ids {
		selected[id] = true
	}

	wifiParams, err := s.store.WifiParams()
	if err != nil {
		wifiParams = map[int64][]store.Param{}
	}

	// 目标设备（按列表顺序，跟概览页一致）与他们各自的频段
	var targets []BatchTarget
	bandOrder := []string{}
	bandSeen := map[string]bool{}
	for _, d := range all {
		if !selected[d.ID] {
			continue
		}
		bands := WifiOverview(wifiParams[d.ID])
		if len(bands) == 0 {
			continue
		}
		targets = append(targets, BatchTarget{Device: d, Bands: bands})
		for _, b := range bands {
			if !bandSeen[b.Label] {
				bandSeen[b.Label] = true
				bandOrder = append(bandOrder, b.Label)
			}
		}
	}

	// 可改字段：只有**至少一台设备有**的字段才出现（全都没有就是设备不支持，不摆空控件）
	combos := 0
	support := map[string]int{}
	options := map[string][]WifiOption{}
	for _, t := range targets {
		for _, b := range t.Bands {
			combos++
			for _, f := range WifiForm(b.Instance, wifiParams[t.Device.ID]) {
				support[f.Key]++
				options[f.Key] = mergeWifiOptions(options[f.Key], f.Options)
			}
		}
	}
	fields := make([]BatchField, 0, len(wifiFieldDefs))
	for _, def := range wifiFieldDefs {
		if support[def.key] == 0 {
			continue
		}
		fields = append(fields, BatchField{
			Key:     def.key,
			Label:   def.label,
			Kind:    def.kind,
			Options: options[def.key],
			Support: support[def.key],
			Combos:  combos,
		})
	}

	// 频段范围候选：全部 + 这些设备上出现过的频段
	lang := s.langOf(w, r)
	bandOpts := []WifiOption{{Value: "all", Label: i18n.T(lang, "全部频段")}}
	for _, l := range bandOrder {
		bandOpts = append(bandOpts, WifiOption{Value: l, Label: l})
	}

	data := map[string]any{
		"Devices":  all,
		"Selected": selected,
		"Targets":  targets,
		"Fields":   fields,
		"Combos":   combos,
		"Bands":    bandOpts,
		"Run":      s.loadBatchRun(r),
		"AuthOn":   s.authEnabled(),
		"Path":     "/wifi/batch",
		// 语言切换链接要带上已选设备，否则切完语言选择就掉了
		"QueryIDs": strings.Join(int64Strings(ids), ","),
		// 提交时一句提示（比如「没勾选任何设备」）
		"Notice":    strings.TrimSpace(r.URL.Query().Get("msg")),
		"NoticeErr": r.URL.Query().Get("err") == "1",
	}
	s.renderLang(w, r, "wifi_batch.html", data)
}

// loadBatchRun 取出最近一次批量下发的结果，并把每台设备的任务状态补上。
func (s *Server) loadBatchRun(r *http.Request) *batchRun {
	if r.URL.Query().Get("run") == "" {
		return nil
	}
	raw, ok, err := s.store.GetSetting(batchRunKey)
	if err != nil || !ok || raw == "" {
		return nil
	}
	var run batchRun
	if err := json.Unmarshal([]byte(raw), &run); err != nil {
		return nil
	}
	// 任务状态是**活的**：存的时候还是 pending，几秒后再看就 done / failed 了，
	// 所以每次展示都重新读一遍（存快照只存「当时下发了什么」，不存状态）。
	for i := range run.Results {
		res := &run.Results[i]
		if d, err := s.store.GetDevice(res.DeviceID); err == nil {
			res.Name = d.DisplayName()
		}
		if tasks, err := s.store.ListTasks(res.DeviceID, 20); err == nil {
			for _, t := range tasks {
				if t.Kind == setParamTaskKind {
					res.Task = t
					break
				}
			}
		}
	}
	return &run
}

// mergeWifiOptions 合并各设备报上来的候选值（去重、按值排序）。
//
// 不同设备的 PossibleChannels 不一样，取并集让人能选；
// 选中的值在设备自己不支持时会写失败（设备会回 9007），那是设备的事，如实呈现。
func mergeWifiOptions(dst, src []WifiOption) []WifiOption {
	seen := map[string]bool{}
	for _, o := range dst {
		seen[o.Value] = true
	}
	for _, o := range src {
		if !seen[o.Value] {
			seen[o.Value] = true
			dst = append(dst, o)
		}
	}
	sort.SliceStable(dst, func(i, j int) bool { return dst[i].Value < dst[j].Value })
	return dst
}

// handleWifiBatchApply 执行批量下发。
//
// 每台设备把各频段要改的参数**聚合成一条 SetParameterValues**（不是每频段一条）：
// 一次会话就能写完，少一轮往返，也不容易出现「2.4G 改了 5G 没改」的半成品。
// 下发完顺手唤醒一次，让设备别等到下一个周期才来取。
func (s *Server) handleWifiBatchApply(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单解析失败", http.StatusBadRequest)
		return
	}
	ids := parseIDs(r.Form["ids"])
	if len(ids) == 0 {
		http.Redirect(w, r, "/wifi/batch?msg="+url.QueryEscape("没有勾选任何设备")+"&err=1", http.StatusSeeOther)
		return
	}
	band := strings.TrimSpace(r.FormValue("band"))
	wifiParams, err := s.store.WifiParams()
	if err != nil {
		wifiParams = map[int64][]store.Param{}
	}

	results := make([]BatchResult, 0, len(ids))
	queued := 0
	for _, id := range ids {
		d, err := s.store.GetDevice(id)
		if err != nil {
			continue // 设备已经不在了（比如刚被删掉），跳过即可
		}
		res := BatchResult{DeviceID: id, Name: d.DisplayName()}
		bands := filterBands(WifiOverview(wifiParams[id]), band)
		res.Bands = bandLabelList(bands)

		sets, missing := batchSets(r.Form, wifiParams[id], bands)
		switch {
		case len(bands) == 0:
			res.Skipped = "没有匹配的频段"
		case len(sets) == 0 && len(missing) > 0:
			res.Skipped = "设备没有这些参数：" + strings.Join(missing, "、")
		case len(sets) == 0:
			res.Skipped = "值没变化，无需下发"
		case s.ctrl == nil:
			res.Err = "未接入控制接口"
		default:
			if err := s.ctrl.SetParameters(id, sets); err != nil {
				res.Err = err.Error()
			} else {
				res.Count = len(sets)
				queued++
				// 唤醒失败不影响正事，只当提示
				if msg, err := s.ctrl.WakeDevice(id); err == nil {
					res.Wake = msg
				} else {
					res.Wake = "唤醒没成功：" + err.Error()
				}
			}
		}
		results = append(results, res)
	}

	run := batchRun{At: time.Now(), Queued: queued, Results: results}
	if raw, err := json.Marshal(run); err == nil {
		_ = s.store.SetSetting(batchRunKey, string(raw))
	}
	back := "/wifi/batch?ids=" + strings.Trim(strings.Join(int64Strings(ids), ","), ",") + "&run=1"
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func bandLabelList(bands []WifiBand) string {
	out := make([]string, 0, len(bands))
	for _, b := range bands {
		out = append(out, b.Label)
	}
	return strings.Join(out, ", ")
}

func int64Strings(ids []int64) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}
