package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// errFake 代替“控制接口返回的拒绝原因”。
var errFake = errors.New("已经有一次重启在排队")

// stubCtrl 只记录收到的参数，不真下发任务。
type stubCtrl struct {
	diagHost  string
	diagCount int
	diagIface string
	reboots   int64 // 收到的重启次数（设备 ID）
	rebootErr error // 非 nil 时 Reboot 返回这个错
}

func (c *stubCtrl) RequestRefresh(int64) error { return nil }
func (c *stubCtrl) FetchSubtree(int64, string, []string, int) error {
	return nil
}
func (c *stubCtrl) FetchWiFi(int64) error                    { return nil }
func (c *stubCtrl) FetchNames(int64, string, bool) error     { return nil }
func (c *stubCtrl) WakeDevice(int64) (string, error)         { return "已唤醒", nil }
func (c *stubCtrl) SetParameters(int64, []store.Param) error { return nil }
func (c *stubCtrl) Diagnose(_ int64, host string, count int, iface string) error {
	c.diagHost, c.diagCount, c.diagIface = host, count, iface
	return nil
}
func (c *stubCtrl) Reboot(id int64) error {
	if c.rebootErr != nil {
		return c.rebootErr
	}
	c.reboots = id
	return nil
}

// 表单里的「承载接口」要一路传到控制接口（留空则传空 = 设备自选）。
func TestHandleDiagnosePassesInterface(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer st.Close()

	ctrl := &stubCtrl{}
	mux := http.NewServeMux()
	if err := Register(mux, st, ctrl, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}

	const iface = "InternetGatewayDevice.WANDevice.1.WANConnectionDevice.2.WANIPConnection.1"
	form := url.Values{"host": {"www.baidu.com"}, "count": {"3"}, "interface": {iface}}
	req := httptest.NewRequest("POST", "/devices/1/diagnose", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("应该是 303，得到 %d：%s", w.Code, w.Body.String())
	}
	if ctrl.diagHost != "www.baidu.com" || ctrl.diagCount != 3 || ctrl.diagIface != iface {
		t.Fatalf("参数没传到：host=%q count=%d iface=%q", ctrl.diagHost, ctrl.diagCount, ctrl.diagIface)
	}

	// 不带 interface 字段（老界面/脚本）→ 空字符串，不能报错
	form = url.Values{"host": {"1.1.1.1"}, "count": {"2"}}
	req = httptest.NewRequest("POST", "/devices/1/diagnose", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther || ctrl.diagIface != "" {
		t.Fatalf("不填承载接口时应为空：%d %q", w.Code, ctrl.diagIface)
	}
}

// 重启按钮：POST 到 /devices/{id}/reboot 要真的调到控制接口，失败时把原因回到界面上。
func TestHandleReboot(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer st.Close()

	ctrl := &stubCtrl{}
	mux := http.NewServeMux()
	if err := Register(mux, st, ctrl, Options{}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/devices/9/reboot", nil))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("应该是 303，得到 %d：%s", w.Code, w.Body.String())
	}
	if ctrl.reboots != 9 {
		t.Fatalf("重启没传到控制接口：%d", ctrl.reboots)
	}

	// 被拒（例如已经有一次在排队）时：带 err=1 回到详情页，而不是 500
	ctrl.rebootErr = errFake
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/devices/9/reboot", nil))
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "err=1") {
		t.Fatalf("拒绝时应带错误提示回跳：%d %s", w.Code, w.Header().Get("Location"))
	}
}

// 承载接口的备选就来自这台设备的 WAN 连接（含 TR069 那条管理连接）。
func TestDiagInterfaceOptions(t *testing.T) {
	links := []WanLink{
		{
			Path:      "InternetGatewayDevice.WANDevice.1.WANConnectionDevice.2.WANIPConnection.1",
			Container: "WANIPConnection",
			Name:      "2_TR069_R_VID_",
			Status:    "Connected",
			IP:        "192.168.10.23",
			Service:   "TR069",
		},
	}
	opts := diagInterfaceOptions(links)
	if len(opts) != 1 {
		t.Fatalf("应该有 1 个候选，得到 %d", len(opts))
	}
	if opts[0].K != links[0].Path {
		t.Fatalf("候选值应该是完整路径：%q", opts[0].K)
	}
	for _, want := range []string{"2_TR069_R_VID_", "192.168.10.23", "TR069"} {
		if !strings.Contains(opts[0].V, want) {
			t.Fatalf("候选描述里应含 %q：%q", want, opts[0].V)
		}
	}
	if got := diagInterfaceOptions(nil); len(got) != 0 {
		t.Fatalf("没有 WAN 连接时不应给候选：%v", got)
	}
}
