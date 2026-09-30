package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// recCtrl 记录「批量下发」到底给哪些设备发了什么（stubCtrl 只吞不记）。
type recCtrl struct {
	stubCtrl
	sets  map[int64][]store.Param // 设备 ID → 收到的参数
	order []int64                 // 调用顺序（每台设备应该只调用一次）
}

func (c *recCtrl) SetParameters(deviceID int64, params []store.Param) error {
	if c.sets == nil {
		c.sets = map[int64][]store.Param{}
	}
	c.sets[deviceID] = append([]store.Param{}, params...)
	c.order = append(c.order, deviceID)
	return nil
}

// batchFixture 造两台设备，都带华为那套无线参数（2.4G=实例 1、5G=实例 5）。
func batchFixture(t *testing.T) (*store.Store, int64, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	id1, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "SimRouter", SerialNumber: "BATCH-A"})
	if err != nil {
		t.Fatal(err)
	}
	id2, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "SimRouter", SerialNumber: "BATCH-B"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{id1, id2} {
		if err := st.UpsertParams(id, huaweiWifiParams(), "getvalues"); err != nil {
			t.Fatal(err)
		}
	}
	return st, id1, id2
}

func TestParseIDs(t *testing.T) {
	// 原生表单（重复键）与分享链接（逗号分隔）都要认
	got := parseIDs([]string{"3", "1,2", " 4 ", "", "x", "1"})
	if len(got) != 4 || got[0] != 3 || got[1] != 1 || got[2] != 2 || got[3] != 4 {
		t.Errorf("解析结果 = %v，期望 [3 1 2 4]", got)
	}
	if len(parseIDs(nil)) != 0 {
		t.Error("空输入应得到空")
	}
}

func TestFilterBands(t *testing.T) {
	bands := WifiOverview(huaweiWifiParams())
	if len(bands) != 2 {
		t.Fatalf("样本应有 2 个频段，得到 %d", len(bands))
	}
	if got := filterBands(bands, ""); len(got) != 2 {
		t.Errorf("空值 = 不过滤，得到 %d", len(got))
	}
	if got := filterBands(bands, "all"); len(got) != 2 {
		t.Errorf("all = 不过滤，得到 %d", len(got))
	}
	got := filterBands(bands, "5G")
	if len(got) != 1 || got[0].Instance != 5 {
		t.Errorf("按频段标签 5G 过滤 = %+v", got)
	}
	if got := filterBands(bands, "不存在的频段"); len(got) != 0 {
		t.Errorf("匹配不上应得到空，得到 %+v", got)
	}
}

// 批量表单 → 每台设备真正要写的参数：参数名与类型都取设备自己报的那套。
func TestBatchSets(t *testing.T) {
	params := huaweiWifiParams()
	bands := WifiOverview(params)

	form := url.Values{"use_ssid": {"1"}, "v_ssid": {"NewNet"}}
	sets, missing := batchSets(form, params, bands)
	if len(sets) != 2 {
		t.Fatalf("两个频段各一条 SSID，得到 %d 条：%+v", len(sets), sets)
	}
	for _, s := range sets {
		if !strings.HasSuffix(s.Name, ".SSID") {
			t.Errorf("写错参数了：%s", s.Name)
		}
		if s.Value != "NewNet" {
			t.Errorf("值不对：%q", s.Value)
		}
		if s.ValueType == "" {
			t.Errorf("没带上参数类型：%+v", s)
		}
	}
	if len(missing) != 0 {
		t.Errorf("不该有缺参数的记录：%v", missing)
	}
}

// 勾了但设备没有这个参数：记下来告诉用户，不能默默少改几台。
func TestBatchSetsMissing(t *testing.T) {
	params := []store.Param{{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID", Value: "x"}}
	bands := WifiOverview(params)
	form := url.Values{"use_key": {"1"}, "v_key": {"pw"}}
	sets, missing := batchSets(form, params, bands)
	if len(sets) != 0 {
		t.Errorf("没这个参数就不该产出要写的值：%+v", sets)
	}
	if len(missing) != 1 || missing[0] != "无线密码" {
		t.Errorf("应记下缺的是「无线密码」，得到 %v", missing)
	}
}

// 密码留空 = 不修改（批量下发最怕手滑把几十台设备的密码清掉）。
func TestBatchSetsEmptyPasswordSkipped(t *testing.T) {
	params := huaweiWifiParams()
	bands := WifiOverview(params)
	form := url.Values{"use_key": {"1"}, "v_key": {"   "}}
	sets, _ := batchSets(form, params, bands)
	if len(sets) != 0 {
		t.Errorf("密码留空应不下发，得到 %+v", sets)
	}
}

// 值跟现网一样就不下发（避免把没动过的参数重写一遍）。
func TestBatchSetsUnchangedSkipped(t *testing.T) {
	params := huaweiWifiParams()
	bands := WifiOverview(params)
	form := url.Values{"use_ssid": {"1"}, "v_ssid": {"WirelessNet"}} // 2.4G 现网就是这个
	sets, _ := batchSets(form, params, bands)
	if len(sets) != 1 {
		t.Fatalf("只有 5G 的 SSID 变了，应只下发 1 条，得到 %d：%+v", len(sets), sets)
	}
	if !strings.Contains(sets[0].Name, "WLANConfiguration.5.") {
		t.Errorf("应只写 5G 那条，得到 %s", sets[0].Name)
	}
}

// 布尔字段：勾了「改」但没勾值 = 关；勾了值 = 开。
//
// 注意这里**两个频段都会下发**（2.4G 原值本来就是 1）：
// 布尔项有意不参与「值没变化就跳过」—— 勾了「启用」就该写一次，
// 否则界面上是「勾了、点了下发、什么都没发生」，看着像功能坏了。
func TestBatchSetsBool(t *testing.T) {
	params := huaweiWifiParams()
	bands := WifiOverview(params)
	form := url.Values{"use_radio": {"1"}, "v_radio": {"1"}}
	sets, _ := batchSets(form, params, bands)
	if len(sets) != 2 {
		t.Fatalf("两个频段的射频开关都要下发，得到 %d：%+v", len(sets), sets)
	}
	for _, s := range sets {
		if s.Value != "1" || !strings.HasSuffix(s.Name, "RadioEnabled") {
			t.Errorf("布尔字段写错了：%+v", s)
		}
	}

	// 只勾「改」不勾值 = 明确要关
	form = url.Values{"use_radio": {"1"}}
	sets, _ = batchSets(form, params, bands)
	if len(sets) != 2 {
		t.Fatalf("两个频段都要下发关闭，得到 %d", len(sets))
	}
	for _, s := range sets {
		if s.Value != "0" {
			t.Errorf("只勾「改」应下发 0，得到 %+v", s)
		}
	}

	// 什么都没勾 = 一条都不下发
	if sets, _ := batchSets(url.Values{}, params, bands); len(sets) != 0 {
		t.Errorf("没勾任何项不该下发，得到 %+v", sets)
	}
}

// 一次批量：每台设备**各一条** SetParameterValues（不是每个频段一条）。
func TestHandleBatchApplyOneTaskPerDevice(t *testing.T) {
	st, id1, id2 := batchFixture(t)
	ctrl := &recCtrl{}
	mux := http.NewServeMux()
	if err := Register(mux, st, ctrl, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}

	form := url.Values{
		"ids":      {strconv.FormatInt(id1, 10) + "," + strconv.FormatInt(id2, 10)},
		"band":     {"all"},
		"use_ssid": {"1"}, "v_ssid": {"BatchNet"},
	}
	req := httptest.NewRequest("POST", "/wifi/batch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("应 303 回批量页，得到 %d：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Location"), "run=1") {
		t.Errorf("应带着 run=1 跳回结果页：%q", w.Header().Get("Location"))
	}
	if len(ctrl.order) != 2 {
		t.Fatalf("每台设备应各一次下发，实际调用了 %d 次：%v", len(ctrl.order), ctrl.order)
	}
	for _, id := range []int64{id1, id2} {
		if len(ctrl.sets[id]) != 2 {
			t.Errorf("设备 %d 应一次下发 2 条（2.4G + 5G），得到 %d：%+v",
				id, len(ctrl.sets[id]), ctrl.sets[id])
		}
	}
}

// 只下发到 2.4G：5G 那条不能出现在下发的参数里。
func TestHandleBatchApplyBandFilter(t *testing.T) {
	st, id1, _ := batchFixture(t)
	ctrl := &recCtrl{}
	mux := http.NewServeMux()
	if err := Register(mux, st, ctrl, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}

	form := url.Values{
		"ids":      {strconv.FormatInt(id1, 10)},
		"band":     {"2.4G"},
		"use_ssid": {"1"}, "v_ssid": {"Only24"},
	}
	req := httptest.NewRequest("POST", "/wifi/batch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(httptest.NewRecorder(), req)
	sets := ctrl.sets[id1]
	if len(sets) != 1 {
		t.Fatalf("只选 2.4G 时应只下发 1 条，得到 %d：%+v", len(sets), sets)
	}
	if !strings.Contains(sets[0].Name, "WLANConfiguration.1.") {
		t.Errorf("下发到了别的频段：%s", sets[0].Name)
	}
}

// 一台设备都没勾：不该下发任何东西，回到批量页给提示。
func TestHandleBatchApplyNoDevice(t *testing.T) {
	st, _, _ := batchFixture(t)
	ctrl := &recCtrl{}
	mux := http.NewServeMux()
	if err := Register(mux, st, ctrl, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	req := httptest.NewRequest("POST", "/wifi/batch", strings.NewReader("use_ssid=1&v_ssid=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("应 303，得到 %d", w.Code)
	}
	if len(ctrl.order) != 0 {
		t.Errorf("没勾设备却下发了：%v", ctrl.order)
	}
	if !strings.Contains(w.Header().Get("Location"), "err=1") {
		t.Errorf("应带着错误提示回去：%q", w.Header().Get("Location"))
	}
}

// 批量页：勾了设备进来要渲染出可改字段与频段候选。
func TestHandleWifiBatchPage(t *testing.T) {
	st, id1, id2 := batchFixture(t)
	mux := http.NewServeMux()
	if err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET",
		"/wifi/batch?ids="+strconv.FormatInt(id1, 10)+","+strconv.FormatInt(id2, 10), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d：%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{`name="use_ssid"`, `<option value="2.4G"`, `<option value="5G"`} {
		if !strings.Contains(body, want) {
			t.Errorf("批量页缺少 %q", want)
		}
	}
	// URL 里带过来的设备要被预选上（没带的不能是选中的）
	for _, id := range []int64{id1, id2} {
		if !checkboxChecked(body, strconv.FormatInt(id, 10)) {
			t.Errorf("设备 %d 应被预选", id)
		}
	}
}

// checkboxChecked 看某个 value 的复选框是不是勾上的（属性之间可能有换行，不能按整串匹配）。
func checkboxChecked(body, value string) bool {
	re := regexp.MustCompile(`<input[^>]*value="` + value + `"[^>]*>`)
	m := re.FindString(body)
	return m != "" && strings.Contains(m, "checked")
}

// 下发结果要能刷新后还在看（存在 settings 里，不靠 URL 传）。
func TestBatchRunPersisted(t *testing.T) {
	st, id1, _ := batchFixture(t)
	mux := http.NewServeMux()
	if err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	form := url.Values{"ids": {strconv.FormatInt(id1, 10)}, "band": {"all"}, "use_ssid": {"1"}, "v_ssid": {"Persist"}}
	req := httptest.NewRequest("POST", "/wifi/batch", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(httptest.NewRecorder(), req)

	// 换一个请求（相当于刷新 / 别人打开）也要看得到结果
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/wifi/batch?run=1", nil))
	body := w.Body.String()
	if !strings.Contains(body, "下发结果") {
		t.Errorf("刷新后应还能看到下发结果：\n%s", body)
	}
	if !strings.Contains(body, "已对 1 台设备下发") {
		t.Error("结果页应显示下发台数")
	}
}

// 布尔项此前是无条件返回 "1"/"0"：只勾「改」没勾值 → 下发 "0"，
// 等于「想启用却把无线关了」；勾了值 → "1"，又因为设备原值本来就是 1
// 被「值没变化」吃掉，表现为「勾了启用什么都没发生」。现在必须能区分三种情况。
func TestBatchFieldValueBool(t *testing.T) {
	def := wifiFieldDef{key: "radio", kind: "bool"}
	field := WifiFormField{Key: "radio", Kind: "bool", Value: "1"}

	// 1) 勾了值 = 启用
	form := url.Values{"use_radio": {"1"}, "v_radio": {"1"}}
	if v, ok := batchFieldValue(form, def, field); !ok || v != "1" {
		t.Errorf("勾了值应得到 1/true，得到 %q/%v", v, ok)
	}

	// 2) 只勾了「改」没勾值 = 明确的「关」，要下发 0
	form = url.Values{"use_radio": {"1"}}
	if v, ok := batchFieldValue(form, def, field); !ok || v != "0" {
		t.Errorf("只勾「改」应得到 0/true（明确要关），得到 %q/%v", v, ok)
	}

	// 3) 两个都没勾 = 不改（绝不能误下发）
	form = url.Values{}
	if v, ok := batchFieldValue(form, def, field); ok {
		t.Errorf("都没勾应表示不改，得到 %q/%v", v, ok)
	}
}

// 勾了「改」+ 勾了值，且设备原值就是 1：这不是「没变化」，必须真的下发一条，
// 否则用户勾了「启用无线 SSID」却什么都不会发生。
func TestBatchSetsBoolEnableIsNotSwallowed(t *testing.T) {
	params := huaweiWifiParams() // 2.4G 那路 RadioEnabled 原值就是 1
	// 频段要用 WifiOverview 算：它才带 Instance，而 batchSets 是按实例号取字段的
	bands := WifiOverview(params)[:1] // 只要 2.4G 那一路
	form := url.Values{
		"use_radio": {"1"},
		"v_radio":   {"1"},
	}
	sets, _ := batchSets(form, params, bands)
	if len(sets) != 1 {
		t.Fatalf("应下发 1 个参数（原值就是 1 也要下发，不能被「没变化」吃掉），得到 %d：%+v", len(sets), sets)
	}
	if !strings.HasSuffix(sets[0].Name, ".RadioEnabled") || sets[0].Value != "1" {
		t.Errorf("应把 RadioEnabled 设为 1，得到 %s=%s", sets[0].Name, sets[0].Value)
	}
}
