package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

func TestParseCols(t *testing.T) {
	// 空串 = 「没给」，交给调用方回落到默认（跟「一列都不勾」要分得开）
	if got := parseCols(""); got != nil {
		t.Errorf("空串应返回 nil，得到 %v", got)
	}
	if got := parseCols("all"); len(got) != len(overviewColDefs) {
		t.Errorf("all 应勾上全部 %d 列，得到 %d", len(overviewColDefs), len(got))
	}
	// default：默认那几列（目前默认全开，但「默认」和「全选」是两个概念 ——
	// 以后加新列时默认可能不开，这里只保证「默认」这个词被认得、且结果非空）
	def := parseCols("default")
	if len(def) == 0 {
		t.Error("default 应解析出至少一列")
	}
	for _, k := range []string{"status", "serial", "clients", "rx", "tx"} {
		if !def[k] {
			t.Errorf("默认应包含 %q：%+v", k, def)
		}
	}
	got := parseCols("serial,rx, tx")
	if len(got) != 3 || !got["serial"] || !got["rx"] || !got["tx"] {
		t.Errorf("解析 serial,rx,tx 失败：%+v", got)
	}
	// 不认识的 key（老 cookie 里已下线的列）要丢掉，不能多渲染一列空表头
	if got := parseCols("serial,不存在的列"); len(got) != 1 || !got["serial"] {
		t.Errorf("未知列应被丢弃：%+v", got)
	}
	// 可逆：解析 → 拼回 → 再解析应当一致
	want := map[string]bool{"status": true, "rx": true, "tx": true}
	if back := parseCols(formatCols(want)); len(back) != len(want) {
		t.Errorf("往返不一致：%q → %+v", formatCols(want), back)
	}
}

func TestFormatColsStableOrder(t *testing.T) {
	// 列序必须是固定的：顺序飘会让 cookie 每次都被重写，看着像「没保存住」
	a := formatCols(map[string]bool{"tx": true, "serial": true, "status": true})
	b := formatCols(map[string]bool{"status": true, "tx": true, "serial": true})
	if a != b {
		t.Errorf("同样的选择拼出了不同的串：%q vs %q", a, b)
	}
	if a != "status,serial,tx" {
		t.Errorf("应按列定义顺序输出，得到 %q", a)
	}
}

// 显示项的选择要能跨请求保持（页面 5 秒自动刷新一次，存不住就等于没有）。
func TestResolveColsCookie(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer st.Close()
	// 没有设备时表格整个不渲染，得先放一台进去
	if _, _, err := st.UpsertDevice(&store.Device{
		OUI: "001122", ProductClass: "SimRouter", SerialNumber: "COL-TEST",
	}); err != nil {
		t.Fatalf("造设备失败: %v", err)
	}
	mux := http.NewServeMux()
	if err := Register(mux, st, &stubCtrl{}, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}

	// 1) ?cols= 带上选择：页面按它渲染，并下发 cookie
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/?cols=serial,rx", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d：%s", w.Code, w.Body.String())
	}
	var raw string
	for _, c := range w.Result().Cookies() {
		if c.Name == colsCookie {
			raw = c.Value
		}
	}
	if raw != "serial,rx" {
		t.Fatalf("cookie 应记下选择，得到 %q", raw)
	}
	body := w.Body.String()
	if !hasTh(body, "序列号") || hasTh(body, "无线终端") {
		t.Errorf("只勾了 serial/rx，不该出现其它列表头：\n%s", headLine(body))
	}

	// 2) 不带 cols 参数：按 cookie 渲染（自动刷新就是这种请求）
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Cookie", colsCookie+"=serial,rx")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req)
	if !hasTh(w2.Body.String(), "序列号") || hasTh(w2.Body.String(), "状态") {
		t.Errorf("cookie 里的选择没生效：\n%s", headLine(w2.Body.String()))
	}

	// 3) 没有 cookie 时回落到默认（默认里有状态和无线终端）
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, httptest.NewRequest("GET", "/", nil))
	if !hasTh(w3.Body.String(), "状态") || !hasTh(w3.Body.String(), "无线终端") {
		t.Errorf("没给选择时应按默认列渲染：\n%s", headLine(w3.Body.String()))
	}
}

// 收光 / 发光没勾选时不该去查光功率（列表页要尽量少查一次库）。
func TestWantOptical(t *testing.T) {
	if wantOptical(map[string]bool{"serial": true}) {
		t.Error("没勾光功率时不应查询")
	}
	if !wantOptical(map[string]bool{"rx": true}) || !wantOptical(map[string]bool{"tx": true}) {
		t.Error("勾了收光或发光就该查")
	}
}

// deviceTableHead 取出设备表表头那一段（页面下面还有个 WiFi 概况表，别混在一起看）。
func deviceTableHead(body string) string {
	start := strings.Index(body, `<table id="devices-table"`)
	if start < 0 {
		return ""
	}
	rest := body[start:]
	end := strings.Index(rest, "</thead>")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// hasTh 判断设备表有没有这一列。
func hasTh(body, label string) bool {
	return strings.Contains(deviceTableHead(body), "<th>"+label+"</th>")
}

func headLine(body string) string { return deviceTableHead(body) }
