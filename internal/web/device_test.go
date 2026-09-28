package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"acs/internal/store"
)

// 详情页的「删除设备」：删完回首页并给提示；重复删要给「不存在」的提示而不是崩。
func TestHandleDeviceDelete(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer st.Close()

	delID, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "SimRouter", SerialNumber: "DEL-ME"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertParams(delID, []store.Param{{Name: "InternetGatewayDevice.X", Value: "1"}}, "getvalues"); err != nil {
		t.Fatal(err)
	}
	keepID, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "SimRouter", SerialNumber: "KEEP"})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	if err := Register(mux, st, &stubCtrl{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	post := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		return w
	}
	delPath := "/devices/" + strconv.FormatInt(delID, 10) + "/delete"

	w := post(delPath)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("应该 303，得到 %d：%s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/?msg=") || strings.Contains(loc, "err=1") {
		t.Fatalf("删完应带着成功提示回首页：%q", loc)
	}
	if !strings.Contains(urlUnescape(loc), "已删除") {
		t.Errorf("提示里应说明已删除：%q", urlUnescape(loc))
	}
	if _, err := st.GetDevice(delID); err == nil {
		t.Error("设备没被删掉")
	}
	if n, _ := st.CountParams(delID); n != 0 {
		t.Errorf("参数没级联删除：%d", n)
	}
	if _, err := st.GetDevice(keepID); err != nil {
		t.Error("另一台设备被误删了")
	}

	// 再删一次：给提示、不能 500
	w = post(delPath)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("重复删应该 303 回首页，得到 %d", w.Code)
	}
	if got := w.Header().Get("Location"); !strings.Contains(got, "err=1") {
		t.Errorf("重复删应提示设备不存在：%q", got)
	}

	// 详情页上的按钮：红色 + 二次确认，且说清「只删本地记录」
	btnID, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "SimRouter", SerialNumber: "BTN"})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/devices/"+strconv.FormatInt(btnID, 10), nil))
	body := rec.Body.String()
	for _, want := range []string{
		`action="/devices/` + strconv.FormatInt(btnID, 10) + `/delete"`,
		"data-confirm=",
		"删除设备",
		"不会动设备本身",
		"下次上报会重新纳管",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("详情页缺少 %q", want)
		}
	}
	if n := strings.Count(body, `class="danger"`); n < 2 {
		t.Errorf("重启与删除都该是红色按钮（danger），只有 %d 个", n)
	}
}

func urlUnescape(s string) string {
	if i := strings.Index(s, "?"); i >= 0 {
		s = s[i+1:]
	}
	if v, err := url.ParseQuery(s); err == nil {
		return v.Get("msg")
	}
	return s
}
