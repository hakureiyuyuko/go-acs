package restart

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIsSystemd(t *testing.T) {
	get := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if IsSystemd(get(map[string]string{})) {
		t.Error("空环境不该判定成 systemd")
	}
	if !IsSystemd(get(map[string]string{"INVOCATION_ID": "abc"})) {
		t.Error("有 INVOCATION_ID 就是 systemd 起的")
	}
	if !IsSystemd(get(map[string]string{"JOURNAL_STREAM": "9:12345"})) {
		t.Error("有 JOURNAL_STREAM 就是 systemd 起的")
	}
}

func TestListenAddrs(t *testing.T) {
	got := ListenAddrs(" :9090 ", "", ":4433", ":9090", ":4433")
	want := []string{":4433", ":9090"}
	if len(got) != len(want) {
		t.Fatalf("去重/去空不对：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个不对：得到 %v，想要 %v", i, got, want)
		}
	}
	if n := len(ListenAddrs("", "  ")); n != 0 {
		t.Errorf("全是空串时该返回空，得到 %d 个", n)
	}
}

func TestDialAddr(t *testing.T) {
	cases := map[string]string{
		":4433":             "127.0.0.1:4433",
		"0.0.0.0:9090":      "127.0.0.1:9090",
		"[::]:9090":         "127.0.0.1:9090",
		"[::1]:9090":        "[::1]:9090",
		"192.168.50.158:80": "192.168.50.158:80",
	}
	for in, want := range cases {
		if got := DialAddr(in); got != want {
			t.Errorf("DialAddr(%q) = %q，想要 %q", in, got, want)
		}
	}
}

// freePort 拿一个当前空闲的端口号（绑一下再放掉）。
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("拿空闲端口失败：%v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestInheritEncodeDecode(t *testing.T) {
	got := EncodeInherit([]string{":7547", ":4433"}, 3)
	if got != ":7547=3,:4433=4" {
		t.Fatalf("编码不对：%q", got)
	}
	m := DecodeInherit(got)
	if m[":7547"] != 3 || m[":4433"] != 4 {
		t.Fatalf("解码不对：%v", m)
	}

	// 脏数据不该把程序带崩：空串、怪东西、小于 3 的句柄（那是标准输入输出）都丢掉
	m = DecodeInherit(" , :7547=5 , 乱写 , :4433=x , :9090=2 , :8080=3")
	if len(m) != 2 || m[":7547"] != 5 || m[":8080"] != 3 {
		t.Errorf("脏数据处理不对：%v", m)
	}
	if n := len(DecodeInherit("")); n != 0 {
		t.Errorf("空串该返回空表，得到 %d 项", n)
	}
}

func TestWithEnvReplaces(t *testing.T) {
	// 上一次重启留下的 ACS_INHERIT_LISTEN 必须被**替换**掉，
	// 不然子进程里同时存在新旧两个值，读到哪个不一定。
	env := []string{"PATH=/bin", InheritEnv + "=:1111=3", "HOME=/root"}
	out := withEnv(env, map[string]string{InheritEnv: ":2222=3"})
	n := 0
	for _, kv := range out {
		if strings.HasPrefix(kv, InheritEnv+"=") {
			n++
			if kv != InheritEnv+"=:2222=3" {
				t.Errorf("该是新值，得到 %q", kv)
			}
		}
	}
	if n != 1 {
		t.Errorf("该只剩一项，得到 %d 项：%v", n, out)
	}
	joined := strings.Join(out, "|")
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "HOME=/root") {
		t.Errorf("其它环境变量不该动：%v", out)
	}
}

func TestAdoptRejectsBadFD(t *testing.T) {
	// 不是监听套接字的句柄：只能报错，不能当成监听器用起来
	if ln, err := Adopt(0); err == nil {
		_ = ln.Close()
		t.Error("句柄 0（标准输入）不该被当成监听器")
	}
}

func TestPrecheck(t *testing.T) {
	// 空闲端口：过
	if err := Precheck([]string{freePort(t)}, nil); err != nil {
		t.Errorf("空闲端口不该报错：%v", err)
	}

	// 被占用的端口：要拦住，并且说人话
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败：%v", err)
	}
	defer ln.Close()
	busy := ln.Addr().String()
	err = Precheck([]string{busy}, nil)
	if err == nil {
		t.Fatal("端口被占用却没报错")
	}
	if !strings.Contains(err.Error(), "端口被别的程序占用") {
		t.Errorf("报错文案没说到点子上：%v", err)
	}
	if !strings.Contains(err.Error(), busy) {
		t.Errorf("报错里该带上出问题的地址：%v", err)
	}

	// held：本来就是我们自己占着的地址，要跳过（改面板端口时 ACS 端口没动就是这种）
	if err := Precheck([]string{busy}, map[string]bool{busy: true}); err != nil {
		t.Errorf("自己占着的地址该跳过：%v", err)
	}

	// 地址本身不合法
	if err := Precheck([]string{"这不是地址"}, nil); err == nil {
		t.Error("非法地址该报错")
	}
}

func TestWaitHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// 起来了：拿到响应就算成功
	if err := WaitHTTP(u.Host, 3*time.Second, nil); err != nil {
		t.Errorf("服务在跑却判定失败：%v", err)
	}

	// 401 / 404 之类也算「活着」（端口通了就是起来了）
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}))
	defer redirect.Close()
	ru, _ := url.Parse(redirect.URL)
	if err := WaitHTTP(ru.Host, 3*time.Second, nil); err != nil {
		t.Errorf("跳转响应该算活着：%v", err)
	}

	// 一直连不上：超时报错
	err = WaitHTTP(freePort(t), 400*time.Millisecond, nil)
	if err == nil {
		t.Error("端口没人听却判定成功")
	}
	if !strings.Contains(err.Error(), "一直没起来") {
		t.Errorf("超时文案不对：%v", err)
	}

	// 新进程中途退出：立刻失败，不用等满超时
	start := time.Now()
	err = WaitHTTP(freePort(t), 5*time.Second, func() bool { return false })
	if err == nil {
		t.Error("新进程已退出却没报错")
	}
	if !strings.Contains(err.Error(), "很快退出") {
		t.Errorf("该提示新进程退出了：%v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("该立刻失败，却等了 %v", el)
	}
}
