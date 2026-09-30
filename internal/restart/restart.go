// Package restart 实现「面板上改完监听地址，确认后自动重启服务」的通用部分。
//
// 为什么需要它：改监听端口必须重启进程才生效（端口是进程启动时绑的）。
// 以前界面上只能写一句「重启服务后生效」，然后让用户自己去命令行
// systemctl restart / dev-server.sh restart —— 对装完就不碰命令行的人等于改不动。
//
// 重启有两种形态，取决于这个进程是谁拉起来的：
//
//	systemd 服务：不能自己起一个新进程（systemd 的 Restart=always 随后也会拉一个，
//	             两个进程会抢同一个端口）。正确做法是**干净退出**，让 systemd 拉起新进程。
//	其它（前台跑、dev-server.sh 的后台进程、容器里）：
//	             自己 exec 一个新进程，确认新的监听地址能应答之后再退出自己 ——
//	             这样新地址起不来时还能回滚，不至于把面板弄没。
//
// 这个包只管「怎么安全地换进程」这些可测的零件，编排在 cmd/acs 里。
package restart

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// IsSystemd 判断当前进程是不是由 systemd 拉起的。
//
// INVOCATION_ID / JOURNAL_STREAM 都是 systemd 给服务进程设的环境变量，
// 前台手跑、dev-server.sh 起的进程都没有。
func IsSystemd(getenv func(string) string) bool {
	return getenv("INVOCATION_ID") != "" || getenv("JOURNAL_STREAM") != ""
}

// ListenAddrs 把要使用的监听地址去重、去空，返回稳定的顺序（便于比较）。
func ListenAddrs(addrs ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Precheck 在真动手重启之前，先试着绑一下这些地址。
//
// 目的：端口被别的进程占着 / 地址写错了，都要在**还活着的时候**发现并报错，
// 不能等进程退了才发现新地址起不来（那时候面板已经没了，用户只能去命令行救）。
//
// held 是「当前进程自己正占着」的地址：它们当然绑不上，要跳过
// （常见情形：只改面板端口，ACS 端口没动）。
func Precheck(addrs []string, held map[string]bool) error {
	for _, a := range addrs {
		if held[a] {
			continue
		}
		ln, err := net.Listen("tcp", a)
		if err != nil {
			return fmt.Errorf("%s 用不了：%w", a, FriendlyListenErr(err))
		}
		_ = ln.Close()
	}
	return nil
}

// FriendlyListenErr 把 bind 错误说成人话（界面上要显示给用户看）。
func FriendlyListenErr(err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if errors.Is(opErr.Err, syscall.EADDRINUSE) {
			return errors.New("端口被别的程序占用了")
		}
		if errors.Is(opErr.Err, syscall.EADDRNOTAVAIL) {
			return errors.New("本机没有这个地址")
		}
		if errors.Is(opErr.Err, syscall.EACCES) {
			return errors.New("没有权限绑这个端口")
		}
	}
	return err
}

// DialAddr 把监听地址换成「本机自己去连」用的地址。
//
// 监听 ":4433" / "0.0.0.0:4433" 时要去连 127.0.0.1；绑了具体 IP 的就连那个 IP。
func DialAddr(listen string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// WaitHTTP 轮询 addr，直到它开始回 HTTP 响应为止。
//
// 判据是「拿到任何 HTTP 响应」（200/401/404 都算）：说明进程已经把端口绑上并在跑。
// childAlive 非 nil 时用来提前失败 —— 新进程中途退出了就别再等满超时。
func WaitHTTP(addr string, timeout time.Duration, childAlive func() bool) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 不跟跳转，301/303 也算「活着」
		},
	}
	url := "http://" + addr + "/login"
	var lastErr error
	for time.Now().Before(deadline) {
		if childAlive != nil && !childAlive() {
			return fmt.Errorf("新进程启动后很快退出了（%v）", lastErr)
		}
		resp, err := client.Get(url)
		if err == nil {
			_, _ = resp.Body.Read(make([]byte, 1))
			_ = resp.Body.Close()
			return nil
		}
		lastErr = err
		time.Sleep(150 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("超时")
	}
	return fmt.Errorf("新地址 %s 一直没起来（%v）", addr, lastErr)
}

// Spawn 起一个自己的新进程：同一个可执行文件、同样的参数与环境。
//
// 关键点：
//   - Setsid：脱离当前会话，本进程退出后新进程不会被连带杀掉；
//   - 标准输出/错误照旧继承（写到同一个日志文件 / systemd journal），日志不丢；
//   - files 里的监听句柄以 3、4… 依次交给新进程（见 InheritEnv）——
//     老进程还攥着的端口（比如没变的 CWMP 端口）**不能**让新进程去重新绑，
//     否则必然 "address already in use"，只能把手里的 socket 直接递过去。
func Spawn(files []*os.File, extraEnv map[string]string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("找不到自己的可执行文件：%w", err)
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = withEnv(os.Environ(), extraEnv)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	cmd.ExtraFiles = files
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("起新进程失败：%w", err)
	}
	return cmd, nil
}

// InheritEnv 是新进程从哪里读「接管哪个句柄」的环境变量。
// 格式：addr=fd,addr=fd（如 ":7547=3,:4433=4"）。
const InheritEnv = "ACS_INHERIT_LISTEN"

// EncodeInherit 把「地址 → 句柄号」编成环境变量的值。
func EncodeInherit(addrs []string, firstFD int) string {
	parts := make([]string, 0, len(addrs))
	for i, a := range addrs {
		parts = append(parts, fmt.Sprintf("%s=%d", a, firstFD+i))
	}
	return strings.Join(parts, ",")
}

// DecodeInherit 解析环境变量里那份「地址 → 句柄号」。
func DecodeInherit(s string) map[string]int {
	out := map[string]int{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.LastIndex(part, "=")
		if i <= 0 {
			continue
		}
		addr := strings.TrimSpace(part[:i])
		fd, err := strconv.Atoi(strings.TrimSpace(part[i+1:]))
		if err != nil || addr == "" || fd < 3 {
			continue
		}
		out[addr] = fd
	}
	return out
}

// Adopt 从上一个进程递过来的句柄上取回监听器。
func Adopt(fd int) (net.Listener, error) {
	f := os.NewFile(uintptr(fd), "inherited-listener")
	if f == nil {
		return nil, fmt.Errorf("句柄 %d 不可用", fd)
	}
	ln, err := net.FileListener(f)
	_ = f.Close() // FileListener 已经 dup 了一份，这份可以关了
	if err != nil {
		return nil, fmt.Errorf("句柄 %d 不是监听套接字：%w", fd, err)
	}
	return ln, nil
}

// withEnv 在环境变量里**替换**指定项（不去重的话，子进程会同时看到新旧两个值，
// 比如上一次重启留下的 ACS_INHERIT_LISTEN，读出来是哪个就不一定了）。
func withEnv(env []string, extra map[string]string) []string {
	out := make([]string, 0, len(env)+len(extra))
	for _, kv := range env {
		name := kv
		if i := strings.Index(kv, "="); i >= 0 {
			name = kv[:i]
		}
		if _, replace := extra[name]; replace {
			continue
		}
		out = append(out, kv)
	}
	// 固定的顺序，便于排查
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+extra[k])
	}
	return out
}
