package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// restartMux 造一个带「立即重启服务」能力的面板（stub 掉真正换进程那部分）。
func restartMux(t *testing.T, rt RuntimeSettings, restart func() (RestartMode, error)) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mux := http.NewServeMux()
	if err := Register(mux, st, &stubCtrl{}, Options{Runtime: rt, Restart: restart}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	return mux, st
}

func TestPanelBaseURL(t *testing.T) {
	cases := []struct {
		host, listen, want string
	}{
		{"192.168.50.158:9090", ":4433", "http://192.168.50.158:4433/"},
		{"127.0.0.1:7547", ":4433", "http://127.0.0.1:4433/"},
		{"127.0.0.1:9090", "0.0.0.0:4433", "http://127.0.0.1:4433/"},
		{"127.0.0.1:9090", "[::]:4433", "http://127.0.0.1:4433/"},
		{"127.0.0.1:9090", "[::1]:4433", "http://[::1]:4433/"},
		// 监听里写死了 IP 就以它为准（那才是面板真正绑上的地址）
		{"192.168.50.158:9090", "10.0.0.5:4433", "http://10.0.0.5:4433/"},
		// 没有端口的监听（异常输入）不该拼出半个地址
		{"127.0.0.1:9090", "4433", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/settings/restart", nil)
		r.Host = c.host
		if got := panelBaseURL(r, c.listen); got != c.want {
			t.Errorf("Host=%s listen=%s：得到 %q，想要 %q", c.host, c.listen, got, c.want)
		}
	}
}

// 改了面板端口：重启后给一张「面板已迁到新地址」的页，并且带自动跳转的信息。
func TestSettingsRestartPageMoved(t *testing.T) {
	rt := RuntimeSettings{ACSListen: ":9090", WebListen: ""}
	mux, st := restartMux(t, rt, func() (RestartMode, error) { return RestartSelf, nil })
	if err := st.SetSetting(settingListen, ":9090"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(settingWebListen, ":4433"); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("POST", "/settings/restart", nil)
	r.Host = "192.168.50.158:9090"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d，想要 200（body=%s）", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"http://192.168.50.158:4433/",                    // 新地址
		`data-restart-url="http://192.168.50.158:4433/"`, // 跳转目标
		"正在重启服务",                                         // 标题文案
		"新进程已经起来",                                        // RestartSelf 的说明
	} {
		if !strings.Contains(body, want) {
			t.Errorf("重启页里缺少 %q\n%s", want, body)
		}
	}
	if got := w.Header().Get("Connection"); got != "close" {
		t.Errorf("换端口时该明确收尾连接，Connection=%q", got)
	}
}

// 只改了 ACS 端口（面板地址没变）：页面不该说「已迁到新地址」。
func TestSettingsRestartPageSameAddress(t *testing.T) {
	rt := RuntimeSettings{ACSListen: ":9090", WebListen: ":8080"}
	mux, st := restartMux(t, rt, func() (RestartMode, error) { return RestartSystemd, nil })
	if err := st.SetSetting(settingListen, ":7547"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(settingWebListen, ":8080"); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("POST", "/settings/restart", nil)
	r.Host = "127.0.0.1:8080"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	body := w.Body.String()
	if !strings.Contains(body, "面板地址没有变化") {
		t.Errorf("面板地址没变时该说清楚：%s", body)
	}
	if strings.Contains(body, "data-restart-url") {
		t.Error("地址没变就不该带自动跳转")
	}
	if !strings.Contains(body, "systemd") {
		t.Errorf("systemd 方式该如实说明：%s", body)
	}
}

// 重启失败：回设置页报错，并且把原因带出来。
func TestSettingsRestartFailureRedirects(t *testing.T) {
	mux, st := restartMux(t, RuntimeSettings{ACSListen: ":9090"}, func() (RestartMode, error) {
		return "", errors.New(":4433 用不了：端口被别的程序占用了（已回滚到原地址）")
	})
	if err := st.SetSetting(settingWebListen, ":4433"); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("POST", "/settings/restart", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("状态码 %d，想要 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/settings?") || !strings.Contains(loc, "err=1") {
		t.Errorf("该回设置页报错，Location=%q", loc)
	}
	if !strings.Contains(loc, "%E7%AB%AF%E5%8F%A3") { // URL 编码后的「端口」
		t.Errorf("报错原因没带出来：%q", loc)
	}
}

// 没注入重启能力（测试/嵌入场景）：如实说「请手动重启」，而不是假装重启了。
func TestSettingsRestartUnsupported(t *testing.T) {
	mux, st := restartMux(t, RuntimeSettings{ACSListen: ":9090"}, nil)
	if err := st.SetSetting(settingWebListen, ":4433"); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/settings/restart", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("状态码 %d，想要 303", w.Code)
	}
	if !strings.Contains(w.Header().Get("Location"), "err=1") {
		t.Errorf("该报错：%q", w.Header().Get("Location"))
	}
}

// 设置页：地址改过就出现「立即重启服务」按钮（带二次确认）；没有这个能力时给命令行说法。
func TestSettingsPageRestartButton(t *testing.T) {
	rt := RuntimeSettings{ACSListen: ":9090", WebListen: ""}
	mux, st := restartMux(t, rt, func() (RestartMode, error) { return RestartSelf, nil })
	if err := st.SetSetting(settingListen, ":9090"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(settingWebListen, ":4433"); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/settings", nil))
	body := w.Body.String()
	for _, want := range []string{
		"立即重启服务",
		`action="/settings/restart"`,
		"确定现在重启服务吗", // 二次确认
		"新端口起不来会自动退回旧地址",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("设置页缺少 %q", want)
		}
	}

	// 没有重启能力：不摆按钮，说清楚自己怎么重启
	mux2, st2 := restartMux(t, rt, nil)
	if err := st2.SetSetting(settingWebListen, ":4433"); err != nil {
		t.Fatal(err)
	}
	w2 := httptest.NewRecorder()
	mux2.ServeHTTP(w2, httptest.NewRequest("GET", "/settings", nil))
	if b := w2.Body.String(); strings.Contains(b, "立即重启服务") || !strings.Contains(b, "systemctl restart acs") {
		t.Errorf("没有重启能力时不该摆按钮，且该给出手动重启办法：%s", b)
	}
}

// 地址没改过：设置页不出现重启区（不打扰）。
func TestSettingsPageNoRestartWhenClean(t *testing.T) {
	mux, st := restartMux(t, RuntimeSettings{ACSListen: ":9090", WebListen: ""},
		func() (RestartMode, error) { return RestartSelf, nil })
	if err := st.SetSetting(settingListen, ":9090"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(settingWebListen, ""); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/settings", nil))
	if b := w.Body.String(); strings.Contains(b, "立即重启服务") {
		t.Errorf("地址没改过就不该出现重启按钮：%s", b)
	}
}

// 重启接口必须是 POST，且和其它面板路由一样要登录。
func TestSettingsRestartNeedsPostAndLogin(t *testing.T) {
	mux, _ := restartMux(t, RuntimeSettings{ACSListen: ":9090"},
		func() (RestartMode, error) { return RestartSelf, nil })

	// GET 不该触发重启（用 405 挡掉）
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/settings/restart", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /settings/restart 状态码 %d，想要 405", w.Code)
	}

	// 开着鉴权时：没登录态 → 跳登录页
	hash, _ := HashPassword("pw-123456")
	creds := NewCreds("admin", hash, []byte("test-secret-key"))
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mux3 := http.NewServeMux()
	if err := Register(mux3, st, &stubCtrl{}, Options{
		Auth:    creds,
		Runtime: RuntimeSettings{ACSListen: ":9090"},
		Restart: func() (RestartMode, error) { return RestartSelf, nil },
	}); err != nil {
		t.Fatal(err)
	}
	w3 := httptest.NewRecorder()
	mux3.ServeHTTP(w3, httptest.NewRequest("POST", "/settings/restart", nil))
	if w3.Code != http.StatusSeeOther || !strings.Contains(w3.Header().Get("Location"), "/login") {
		t.Errorf("未登录该跳登录页，得到 %d %q", w3.Code, w3.Header().Get("Location"))
	}
}
