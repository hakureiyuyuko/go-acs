package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/i18n"
	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 几个小工具，避免和别的测试文件里的同名助手打架。
func newReq(method, path string) *http.Request { return httptest.NewRequest(method, path, nil) }

func serve(t *testing.T, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func doGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t, h, newReq("GET", path))
}

func doGetWithCookie(t *testing.T, h http.Handler, path, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	req := newReq("GET", path)
	req.Header.Set("Cookie", cookie)
	return serve(t, h, req)
}

// 模板里用到的文案必须都有英文译文 —— 这条防线专门防「改了中文忘了翻译」，
// 因为 key 就是中文原文，中文一改 key 就变了，英文会静默回落到中文。
func TestAllTemplateTextsTranslated(t *testing.T) {
	tplKey := regexp.MustCompile(`\{\{T "([^"]+)"`)
	jsKey := regexp.MustCompile(`t\("([^"]+)"\)`)

	seen := map[string]string{} // key → 出处
	err := fs.WalkDir(assets, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(b)
		if strings.HasSuffix(path, ".html") {
			for _, m := range tplKey.FindAllStringSubmatch(text, -1) {
				seen[m[1]] = path
			}
		}
		if strings.HasSuffix(path, "app.js") {
			for _, m := range jsKey.FindAllStringSubmatch(text, -1) {
				// createElement("div") 之类的会被 t\("…"\) 误伤，过滤掉已知的非文案
				switch m[1] {
				case "div", "span", "select", "option", "button":
					continue
				}
				seen[m[1]] = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("读模板失败: %v", err)
	}
	if len(seen) < 100 {
		t.Fatalf("只找到 %d 条文案，模板解析大概出问题了", len(seen))
	}
	missing := 0
	for key, from := range seen {
		if !i18n.Has(i18n.LangEN, key) {
			t.Errorf("缺英文译文：%q（出自 %s）", key, from)
			missing++
		}
	}
	if missing == 0 {
		t.Logf("模板与 app.js 共 %d 条文案，全部有英文译文", len(seen))
	}
}

// 英文页面里不该再出现中文（除了标记为「设备数据」的内容）。
func TestEnglishPagesHaveNoChinese(t *testing.T) {
	creds := NewCreds("", "", nil) // 关掉鉴权，直接拿页面
	mux := newAuthTestMux(t, creds)

	cjk := regexp.MustCompile(`[\p{Han}]`)
	// 顶栏的语言切换（指向 ?lang=xx 的那个链接）里带的是语言自称，检查时排除
	switcherRe := regexp.MustCompile(`(?s)<a class="ghost" href="\?lang=[^"]*"[^>]*>.*?</a>`)
	i18nJSONRe := regexp.MustCompile(`(?s)<script>window\.I18N = .*?</script>`)

	// assertNoChinese 检查一页英文页面里没有残留中文。
	// 语言切换按钮上显示的是「语言自己的名字」（中文 / English），这是刻意的；
	// window.I18N 那份 JSON 的 key 就是中文原文（按设计如此），两处都排除。
	assertNoChinese := func(t *testing.T, mux http.Handler, path string) {
		t.Helper()
		w := doGet(t, mux, path)
		if w.Code != 200 {
			t.Errorf("%s 应 200，得到 %d", path, w.Code)
			return
		}
		body := w.Body.String()
		body = switcherRe.ReplaceAllString(body, "")
		body = i18nJSONRe.ReplaceAllString(body, "")
		if cjk.MatchString(body) {
			// 只提示前几处，方便定位
			locs := cjk.FindAllStringIndex(body, 5)
			var samples []string
			for _, loc := range locs {
				s := body[loc[0]:min(loc[0]+40, len(body))]
				samples = append(samples, strings.Split(s, "\n")[0])
			}
			t.Errorf("%s 的英文页面里还有中文：%s", path, strings.Join(samples, " | "))
		}
	}

	for _, path := range []string{"/?lang=en", "/settings?lang=en"} {
		assertNoChinese(t, mux, path)
	}

	// 设备详情页也要查：那一页的字段大多是**数据拼出来的**（kv 的键、任务结果、分组名…），
	// 模板里扫不到字面量，最容易漏翻 —— 「收光 / 发光」两行就这样漏过一次。
	st2, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st2.Close() })
	id, _, err := st2.UpsertDevice(&store.Device{
		OUI: "001122", ProductClass: "SimRouter", SerialNumber: "EN-1",
		Manufacturer: "Example", ModelName: "Sim", DataModelRoot: "InternetGatewayDevice.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.UpsertParams(id, []store.Param{
		{Name: "InternetGatewayDevice.DeviceInfo.UpTime", Value: "3600"},
		// 有光功率读数，才会渲染那两行
		{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.RXPower", Value: "-15"},
		{Name: "InternetGatewayDevice.WANDevice.1.X_GponInterafceConfig.TXPower", Value: "2"},
	}, "getvalues"); err != nil {
		t.Fatal(err)
	}
	mux2 := http.NewServeMux()
	if err := Register(mux2, st2, &stubCtrl{}, Options{}); err != nil {
		t.Fatal(err)
	}
	assertNoChinese(t, mux2, "/devices/"+strconv.FormatInt(id, 10)+"?lang=en")
}

// 登录页也要能切语言（那一页不需要登录）。
func TestLoginPageEnglish(t *testing.T) {
	hash, _ := HashPassword("pw-123456")
	mux := newAuthTestMux(t, NewCreds("admin", hash, []byte("secret")))
	body := doGet(t, mux, "/login?lang=en").Body.String()
	for _, want := range []string{"Sign in", "Username", "Password"} {
		if !strings.Contains(body, want) {
			t.Errorf("英文登录页缺少 %q", want)
		}
	}
	if regexp.MustCompile(`[\p{Han}]`).MatchString(strings.ReplaceAll(body, "中文", "")) {
		t.Error("英文登录页还有中文")
	}
}

// 语言切换：?lang= 写 cookie、Accept-Language 兜底、cookie 优先。
func TestLanguageNegotiation(t *testing.T) {
	mux := newAuthTestMux(t, NewCreds("", "", nil))

	// ?lang=en → 英文，并下发 cookie
	w := doGet(t, mux, "/?lang=en")
	if !strings.Contains(w.Body.String(), "Settings") {
		t.Error("?lang=en 应该渲染英文")
	}
	var langCookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == "acs_lang" {
			langCookie = c.Value
		}
	}
	if langCookie != "en" {
		t.Errorf("应下发 acs_lang=en 的 cookie，得到 %q", langCookie)
	}

	// 带 cookie 的后续请求（没带 lang 参数）仍是英文
	if body := doGetWithCookie(t, mux, "/", "acs_lang=en").Body.String(); !strings.Contains(body, "Managed devices") {
		t.Error("cookie 应让后续请求保持英文")
	}
	// Accept-Language 兜底
	req := newReq("GET", "/")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if body := serve(t, mux, req).Body.String(); !strings.Contains(body, "Managed devices") {
		t.Error("Accept-Language 应被采纳")
	}
	// 默认中文
	if body := doGet(t, mux, "/").Body.String(); !strings.Contains(body, "已纳管设备") {
		t.Error("没有语言线索时应默认中文")
	}
}
