package web

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"acs/internal/store"
)

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("s3cret-pass")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$") {
		t.Fatalf("散列格式不对：%q", h)
	}
	if strings.Contains(h, "s3cret-pass") {
		t.Fatal("散列里出现了明文密码")
	}
	if !VerifyPassword(h, "s3cret-pass") {
		t.Error("正确密码应通过")
	}
	if VerifyPassword(h, "s3cret-Pass") {
		t.Error("错误密码不应通过")
	}
	// 同一密码两次散列不同（随机盐）
	h2, _ := HashPassword("s3cret-pass")
	if h == h2 {
		t.Error("两次散列应当不同（要有随机盐）")
	}
	for _, bad := range []string{"", "plain", "pbkdf2-sha256$0$$", "pbkdf2-sha256$x$aa$bb", "md5$1$aa$bb"} {
		if VerifyPassword(bad, "whatever") {
			t.Errorf("畸形散列应一律拒绝：%q", bad)
		}
	}
}

// 面板鉴权：没启用就放行；启用了就得凭对账号密码；改完凭据立即生效（不用重启）。
func TestCredsGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	do := func(c *Creds, auth string) int {
		req := httptest.NewRequest("GET", "/", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Basic "+basic(auth))
		}
		w := httptest.NewRecorder()
		c.Guard(ok).ServeHTTP(w, req)
		return w.Code
	}

	// 没启用：一律放行
	c := NewCreds("", "")
	if got := do(c, ""); got != 200 {
		t.Errorf("未启用时应放行，得到 %d", got)
	}

	hash, _ := HashPassword("pw-123456")
	c.Set("admin", hash)
	if got := do(c, ""); got != 401 {
		t.Errorf("启用后不带凭据应 401，得到 %d", got)
	}
	if got := do(c, "admin:wrong"); got != 401 {
		t.Errorf("密码错应 401，得到 %d", got)
	}
	if got := do(c, "root:pw-123456"); got != 401 {
		t.Errorf("账号错应 401，得到 %d", got)
	}
	if got := do(c, "admin:pw-123456"); got != 200 {
		t.Errorf("凭据对应 200，得到 %d", got)
	}

	// 在线改密码：旧密码立刻失效（设置页保存后就是这个效果）
	hash2, _ := HashPassword("pw-654321")
	c.Set("admin", hash2)
	if got := do(c, "admin:pw-123456"); got != 401 {
		t.Errorf("旧密码应立刻失效，得到 %d", got)
	}
	if got := do(c, "admin:pw-654321"); got != 200 {
		t.Errorf("新密码应立刻可用，得到 %d", got)
	}

	// 有账号没散列（配置不全）：宁可不放行
	c.Set("admin", "")
	if got := do(c, "admin:"); got != 401 {
		t.Errorf("散列缺失时应拒绝，得到 %d", got)
	}
	// 关掉保护
	c.Set("", "")
	if got := do(c, ""); got != 200 {
		t.Errorf("关掉后应放行，得到 %d", got)
	}
}

func basic(creds string) string {
	return base64.StdEncoding.EncodeToString([]byte(creds))
}

// 设置页：保存监听与账号密码（账号密码立即生效，端口重启生效），非法输入要拦下。
func TestSettingsPage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer st.Close()

	hash, _ := HashPassword("old-pass-1")
	creds := NewCreds("admin", hash)
	mux := http.NewServeMux()
	if err := Register(mux, st, &stubCtrl{}, Options{
		Auth: creds,
		Runtime: RuntimeSettings{
			ACSListen: ":9090", WebListen: "", Path: "/acs",
		},
	}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}

	get := func(auth string) (int, string) {
		req := httptest.NewRequest("GET", "/settings", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Basic "+basic(auth))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	post := func(auth string, form url.Values) (int, string) {
		req := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if auth != "" {
			req.Header.Set("Authorization", "Basic "+basic(auth))
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Header().Get("Location")
	}

	// 设置页本身也受保护
	if code, _ := get(""); code != 401 {
		t.Fatalf("设置页应受鉴权保护，得到 %d", code)
	}
	code, body := get("admin:old-pass-1")
	if code != 200 {
		t.Fatalf("设置页打不开：%d", code)
	}
	for _, want := range []string{"设置", "监听地址", "访问控制", ":9090",
		`name="acs_listen"`, `name="web_listen"`, `name="web_user"`, `name="web_pass"`} {
		if !strings.Contains(body, want) {
			t.Errorf("设置页缺少 %q", want)
		}
	}
	// 页面上不该出现说明性的文档（正式产品式的表单：只有标签、字段、按钮）
	for _, dont := range []string{"HTTP Basic", "反向代理", "明文", "忘记", "settings 表", "curl"} {
		if strings.Contains(body, dont) {
			t.Errorf("设置页不该出现说明性文字 %q", dont)
		}
	}

	// 非法监听地址
	if code, loc := post("admin:old-pass-1", url.Values{
		"acs_listen": {"不是地址"}, "web_listen": {""}, "auth": {"1"}, "web_user": {"admin"},
	}); code != 303 || !strings.Contains(loc, "err=1") {
		t.Errorf("非法监听地址应被拒：%d %q", code, loc)
	}
	// 开保护但既没旧密码也没新密码
	empty := NewCreds("", "")
	mux2 := http.NewServeMux()
	if err := Register(mux2, st, &stubCtrl{}, Options{Auth: empty}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/settings", strings.NewReader(url.Values{
		"acs_listen": {":9090"}, "web_listen": {""}, "auth": {"1"}, "web_user": {"admin"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux2.ServeHTTP(w, req)
	if w.Code != 303 || !strings.Contains(w.Header().Get("Location"), "err=1") {
		t.Errorf("启用保护却没密码时应被拒：%d %q", w.Code, w.Header().Get("Location"))
	}
	// 两次密码不一致
	if _, loc := post("admin:old-pass-1", url.Values{
		"acs_listen": {":9090"}, "web_listen": {""}, "auth": {"1"}, "web_user": {"admin"},
		"web_pass": {"aaaaaaaa"}, "web_pass2": {"bbbbbbbb"},
	}); !strings.Contains(loc, "err=1") {
		t.Errorf("两次密码不一致应被拒：%q", loc)
	}

	// 正常保存：端口进库（重启生效），账号密码立即生效
	code, loc := post("admin:old-pass-1", url.Values{
		"acs_listen": {":19090"}, "web_listen": {":18080"}, "auth": {"1"},
		"web_user": {"ops"}, "web_pass": {"new-pass-9"}, "web_pass2": {"new-pass-9"},
	})
	if code != 303 || strings.Contains(loc, "err=1") {
		t.Fatalf("正常保存应成功：%d %q", code, loc)
	}
	for k, want := range map[string]string{
		settingListen:    ":19090",
		settingWebListen: ":18080",
		settingWebUser:   "ops",
	} {
		if v, ok, _ := st.GetSetting(k); !ok || v != want {
			t.Errorf("settings[%s] = %q，期望 %q", k, v, want)
		}
	}
	if stored, _, _ := st.GetSetting(settingWebPass); !strings.HasPrefix(stored, "pbkdf2-sha256$") {
		t.Errorf("密码应存散列，实际 %q", stored)
	}
	// 立即生效：旧账号密码 401，新的 200
	if code, _ := get("admin:old-pass-1"); code != 401 {
		t.Errorf("旧凭据应立刻失效，得到 %d", code)
	}
	if code, _ := get("ops:new-pass-9"); code != 200 {
		t.Errorf("新凭据应立刻可用，得到 %d", code)
	}
	// 设置页这时应该提示端口改动待重启
	if _, body := get("ops:new-pass-9"); !strings.Contains(body, "重启服务后生效") {
		t.Error("改过端口后设置页应提示重启服务后生效")
	}

	// 关掉保护
	if code, loc := post("ops:new-pass-9", url.Values{
		"acs_listen": {":19090"}, "web_listen": {""}, "auth": {"0"}, "web_user": {"ops"},
	}); code != 303 || strings.Contains(loc, "err=1") {
		t.Fatalf("关闭保护应成功：%d %q", code, loc)
	}
	if code, _ := get(""); code != 200 {
		t.Errorf("关闭保护后应能直接打开，得到 %d", code)
	}
}
