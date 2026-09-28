package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
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
			t.Error("畸形散列应一律拒绝：", bad)
		}
	}
}

// doLogin 走一遍登录表单，返回登录态 cookie。
func doLogin(t *testing.T, h http.Handler, user, pass string) *http.Cookie {
	t.Helper()
	form := url.Values{"user": {user}, "pass": {pass}, "next": {"/"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("登录应 303，得到 %d：%s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == panelCookieName && c.Value != "" {
			return c
		}
	}
	t.Fatal("登录成功却没下发登录态 cookie")
	return nil
}

func reqWithCookie(method, target string, c *http.Cookie) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if c != nil {
		req.AddCookie(c)
	}
	return req
}

// newAuthTestMux 按生产同样的方式挂一遍路由（含 Guard 与登录/退出路由）。
func newAuthTestMux(t *testing.T, creds *Creds) http.Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mux := http.NewServeMux()
	if err := Register(mux, st, &stubCtrl{}, Options{Auth: creds}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}
	return mux
}

// 面板登录：没有登录态就跳登录页（API 则 401），登录后放行；退出/改密码即失效。
func TestPanelLogin(t *testing.T) {
	hash, _ := HashPassword("pw-123456")
	creds := NewCreds("admin", hash, []byte("test-secret-key"))
	mux := newAuthTestMux(t, creds)

	get := func(target string, c *http.Cookie) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, reqWithCookie("GET", target, c))
		return w
	}

	t.Run("未启用鉴权时一律放行", func(t *testing.T) {
		off := NewCreds("", "", nil)
		m := newAuthTestMux(t, off)
		w := httptest.NewRecorder()
		m.ServeHTTP(w, reqWithCookie("GET", "/", nil))
		if w.Code != 200 {
			t.Errorf("未启用应放行，得到 %d", w.Code)
		}
	})

	t.Run("未登录时页面跳登录页", func(t *testing.T) {
		w := get("/devices/1", nil)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("应 303 跳登录页，得到 %d", w.Code)
		}
		loc := w.Header().Get("Location")
		if !strings.HasPrefix(loc, "/login?next=") || !strings.Contains(loc, url.QueryEscape("/devices/1")) {
			t.Errorf("跳转地址不对：%q", loc)
		}
	})
	t.Run("未登录时 API 给 401 JSON", func(t *testing.T) {
		w := get("/api/devices", nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("应 401，得到 %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "需要登录") {
			t.Errorf("应回 JSON 错误，实际 %q", w.Body.String())
		}
	})
	t.Run("登录页本身不需要登录", func(t *testing.T) {
		w := get("/login", nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `name="pass"`) {
			t.Fatalf("登录页应能打开：%d", w.Code)
		}
	})
	t.Run("账号密码不对不进", func(t *testing.T) {
		form := url.Values{"user": {"admin"}, "pass": {"wrong"}, "next": {"/"}}
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("密码错应 401，得到 %d", w.Code)
		}
		if len(w.Result().Cookies()) != 0 {
			t.Error("失败不该下发登录态")
		}
		if !strings.Contains(w.Body.String(), "账号或密码不对") {
			t.Error("登录页应提示错误")
		}
	})

	cookie := doLogin(t, mux, "admin", "pw-123456")
	t.Run("账号密码对了放行", func(t *testing.T) {
		if w := get("/", cookie); w.Code != 200 {
			t.Fatalf("带登录态应放行，得到 %d", w.Code)
		}
	})
	t.Run("伪造的 cookie 不认", func(t *testing.T) {
		bad := &http.Cookie{Name: panelCookieName, Value: "v1.0.9999999999.deadbeef"}
		if w := get("/", bad); w.Code != http.StatusSeeOther {
			t.Errorf("签名不对应跳登录页，得到 %d", w.Code)
		}
	})
	t.Run("退出登录后回到未登录状态", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/logout", nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
			t.Fatalf("退出应跳登录页：%d %q", w.Code, w.Header().Get("Location"))
		}
		cleared := false
		for _, c := range w.Result().Cookies() {
			if c.Name == panelCookieName && c.MaxAge < 0 {
				cleared = true
			}
		}
		if !cleared {
			t.Error("退出时应清掉登录态 cookie")
		}
	})
	t.Run("改密码后旧登录态立刻失效", func(t *testing.T) {
		hash2, _ := HashPassword("pw-654321")
		creds.Set("admin", hash2)
		if w := get("/", cookie); w.Code != http.StatusSeeOther {
			t.Errorf("改密码后旧票应失效，得到 %d", w.Code)
		}
		// 用新密码能进
		fresh := doLogin(t, mux, "admin", "pw-654321")
		if w := get("/", fresh); w.Code != 200 {
			t.Errorf("新密码登录后应放行，得到 %d", w.Code)
		}
		cookie = fresh
	})

	t.Run("过期后失效", func(t *testing.T) {
		base := time.Now()
		creds.now = func() time.Time { return base }
		tok := creds.issueToken()
		creds.now = func() time.Time { return base.Add(panelSessionTTL + time.Minute) }
		if ok, _ := creds.checkToken(tok); ok {
			t.Error("过期 token 不该通过")
		}
		creds.now = time.Now
	})
	t.Run("剩余不足一半会续期", func(t *testing.T) {
		base := time.Now()
		creds.now = func() time.Time { return base }
		tok := creds.issueToken()
		creds.now = func() time.Time { return base.Add(panelSessionTTL / 2).Add(time.Minute) }
		ok, renew := creds.checkToken(tok)
		if !ok || !renew {
			t.Errorf("应变判为有效且需要续期：ok=%v renew=%v", ok, renew)
		}
		ck := &http.Cookie{Name: panelCookieName, Value: tok}
		w := get("/", ck)
		if w.Code != 200 || len(w.Result().Cookies()) == 0 {
			t.Error("该续期时应该重发 cookie 并放行")
		}
		creds.now = time.Now
	})
	t.Run("关掉保护后不需要登录", func(t *testing.T) {
		creds.Set("", "")
		if w := get("/", nil); w.Code != 200 {
			t.Errorf("关闭保护后应放行，得到 %d", w.Code)
		}
	})
}

func ok2Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
}

// loginOn 直接调 handler 的登录流程（含 Guard，路由与生产一致）。
func loginOn(t *testing.T, c *Creds, _ http.Handler, user, pass string) *http.Cookie {
	t.Helper()
	form := url.Values{"user": {user}, "pass": {pass}, "next": {"/"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	s := &Server{opt: Options{Auth: c}}
	s.handleLoginSubmit(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("登录应 303，得到 %d：%s", w.Code, w.Body.String())
	}
	for _, ck := range w.Result().Cookies() {
		if ck.Name == panelCookieName && ck.Value != "" {
			return ck
		}
	}
	t.Fatal("登录成功却没下发 cookie")
	return nil
}

// 连续失败会短暂锁定（直接测计数逻辑，避免走 HTTP 时等那几个 400ms）。
func TestLoginLockout(t *testing.T) {
	c := NewCreds("admin", "x", nil)
	now := time.Now()
	c.now = func() time.Time { return now }
	for i := 0; i < loginFailLimit; i++ {
		if locked, _ := c.locked("1.2.3.4"); locked {
			t.Fatalf("第 %d 次不该锁", i+1)
		}
		c.noteFail("1.2.3.4")
	}
	if locked, wait := c.locked("1.2.3.4"); !locked || wait <= 0 {
		t.Error("达到上限后应锁定")
	}
	// 别的来源不受影响
	if locked, _ := c.locked("5.6.7.8"); locked {
		t.Error("别的 IP 不该被锁")
	}
	// 登录成功会清掉计数
	c.clearFails("1.2.3.4")
	if locked, _ := c.locked("1.2.3.4"); locked {
		t.Error("清掉计数后不该再锁")
	}
	// 过了窗口也自动解锁
	c.noteFail("9.9.9.9")
	now = now.Add(loginFailWindow + time.Minute)
	if locked, _ := c.locked("9.9.9.9"); locked {
		t.Error("过了失败窗口应自动解锁")
	}
}

// 设置页：保存监听与账号密码（账号密码立即生效，端口重启生效），非法输入要拦下。
func TestSettingsPage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	defer st.Close()

	hash, _ := HashPassword("old-pass-1")
	creds := NewCreds("admin", hash, []byte("test-secret"))
	mux := http.NewServeMux()
	if err := Register(mux, st, &stubCtrl{}, Options{
		Auth: creds,
		Runtime: RuntimeSettings{
			ACSListen: ":9090", WebListen: "", Path: "/acs",
		},
	}); err != nil {
		t.Fatalf("挂路由失败: %v", err)
	}

	get := func(c *http.Cookie) (int, string) {
		req := reqWithCookie("GET", "/settings", c)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	post := func(c *http.Cookie, form url.Values) (int, string) {
		req := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if c != nil {
			req.AddCookie(c)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Header().Get("Location")
	}

	// 没登录：设置页应被挡到登录页
	if code, _ := get(nil); code != http.StatusSeeOther {
		t.Fatalf("未登录访问设置页应跳登录页，得到 %d", code)
	}
	// 登录页本身不需要登录，且能打开
	req := httptest.NewRequest("GET", "/login", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `name="pass"`) {
		t.Fatalf("登录页应能打开：%d", w.Code)
	}

	ck := doLogin(t, mux, "admin", "old-pass-1")
	code, body := get(ck)
	if code != 200 {
		t.Fatalf("登录后设置页打不开：%d", code)
	}
	for _, want := range []string{"设置", "监听地址", "访问控制", ":9090",
		`name="acs_listen"`, `name="web_listen"`, `name="web_user"`, `name="web_pass"`,
		`action="/logout"`} {
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
	if code, loc := post(ck, url.Values{
		"acs_listen": {"不是地址"}, "web_listen": {""}, "auth": {"1"}, "web_user": {"admin"},
	}); code != 303 || !strings.Contains(loc, "err=1") {
		t.Errorf("非法监听地址应被拒：%d %q", code, loc)
	}
	// 开保护但既没旧密码也没新密码
	empty := NewCreds("", "", nil)
	mux2 := http.NewServeMux()
	if err := Register(mux2, st, &stubCtrl{}, Options{Auth: empty}); err != nil {
		t.Fatal(err)
	}
	req2 := httptest.NewRequest("POST", "/settings", strings.NewReader(url.Values{
		"acs_listen": {":9090"}, "web_listen": {""}, "auth": {"1"}, "web_user": {"admin"},
	}.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	mux2.ServeHTTP(w2, req2)
	if w2.Code != 303 || !strings.Contains(w2.Header().Get("Location"), "err=1") {
		t.Errorf("启用保护却没密码时应被拒：%d %q", w2.Code, w2.Header().Get("Location"))
	}
	// 两次密码不一致
	if _, loc := post(ck, url.Values{
		"acs_listen": {":9090"}, "web_listen": {""}, "auth": {"1"}, "web_user": {"admin"},
		"web_pass": {"aaaaaaaa"}, "web_pass2": {"bbbbbbbb"},
	}); !strings.Contains(loc, "err=1") {
		t.Errorf("两次密码不一致应被拒：%q", loc)
	}

	// 正常保存：端口进库（重启生效），账号密码立即生效
	code, loc := post(ck, url.Values{
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
	// 改了账号密码 → 旧登录态立即失效，得重新登录
	if code, _ := get(ck); code != http.StatusSeeOther {
		t.Errorf("改完账号密码后旧登录态应失效，得到 %d", code)
	}
	ck = doLogin(t, mux, "ops", "new-pass-9")
	if code, body := get(ck); code != 200 || !strings.Contains(body, "重启服务后生效") {
		t.Errorf("改过端口后设置页应提示重启服务后生效（code=%d）", code)
	}

	// 再改一次密码：别的登录态（这里就是手上这张）会被踢掉，但保存这一下要给当前浏览器补发新票
	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("POST", "/settings", strings.NewReader(url.Values{
		"acs_listen": {":19090"}, "web_listen": {":18080"}, "auth": {"1"}, "web_user": {"ops"},
		"web_pass": {"new-pass-10"}, "web_pass2": {"new-pass-10"},
	}.Encode()))
	req3.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req3.AddCookie(ck)
	mux.ServeHTTP(w3, req3)
	if w3.Code != 303 || strings.Contains(w3.Header().Get("Location"), "err=1") {
		t.Fatalf("改密码应成功：%d %q", w3.Code, w3.Header().Get("Location"))
	}
	var fresh *http.Cookie
	for _, c := range w3.Result().Cookies() {
		if c.Name == panelCookieName && c.Value != "" {
			fresh = c
		}
	}
	if fresh == nil {
		t.Fatal("改完密码后应给当前会话补发登录态（否则管理员会被自己踢出去）")
	}
	if code, _ := get(fresh); code != 200 {
		t.Errorf("补发的登录态应能继续用，得到 %d", code)
	}
	if code, _ := get(ck); code != http.StatusSeeOther {
		t.Errorf("改密码前的登录态应失效，得到 %d", code)
	}

	// 关掉保护
	if code, loc := post(fresh, url.Values{
		"acs_listen": {":19090"}, "web_listen": {""}, "auth": {"0"}, "web_user": {"ops"},
	}); code != 303 || strings.Contains(loc, "err=1") {
		t.Fatalf("关闭保护应成功：%d %q", code, loc)
	}
	if code, _ := get(nil); code != 200 {
		t.Errorf("关闭保护后应能直接打开，得到 %d", code)
	}
}
