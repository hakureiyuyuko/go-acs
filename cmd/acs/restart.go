package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/restart"
	"github.com/hakureiyuyuko/go-acs/internal/store"
	"github.com/hakureiyuyuko/go-acs/internal/web"
)

// restartService 造出「面板上点『立即重启服务』」时真正干活的那个函数。
//
// 两种形态（见 internal/restart 的包注释）：
//
//	systemd 托管：我们只干净退出，systemd（Restart=always）随后把新进程拉起来。
//	其它方式：自己起一个新进程，确认新地址能应答，再退出自己。
//
// 非 systemd 这条路的关键其实是**句柄交接**：进程里没变的端口（典型是 CWMP 那个）
// 老进程还听着，新进程去重新 bind 必然 "address already in use" —— 端口不是配置，
// 是内核里那个 socket，只能把 socket 直接递给新进程（live 就是这些句柄）。
// 变了的端口由这里先把 socket 建好再交出去，顺手把关了「新端口能不能绑」。
//
// oldACS / oldWeb 是**当前进程实际在用**的监听地址，也是回滚时的落点。stop 触发优雅退出。
func restartService(st *store.Store, log *slog.Logger, oldACS, oldWeb string, live map[string]net.Listener, stop func()) func() (web.RestartMode, error) {
	// 自己占着的地址：预检要跳过，也是判断「该不该递句柄」的依据
	held := make(map[string]bool, len(live))
	for a := range live {
		held[a] = true
	}

	var busy atomic.Bool
	return func() (web.RestartMode, error) {
		// 防连点：同一时刻只允许一次重启（按钮双击、两个标签页同时点）
		if !busy.CompareAndSwap(false, true) {
			return "", errors.New("正在重启中，请稍等几秒")
		}

		// 没成功就把设置退回原样：不然设置页会一直挂着「待重启 + 立即重启服务」，
		// 而那个地址根本绑不上，每点一次都白点。
		rollback := func(why error) (web.RestartMode, error) {
			_ = st.SetSetting("listen", oldACS)
			_ = st.SetSetting("web_listen", oldWeb)
			log.Error("改监听地址失败，已回滚到原地址",
				"err", why, "rolled_back_listen", oldACS, "rolled_back_panel", oldWeb)
			busy.Store(false)
			return "", fmt.Errorf("%v（已回滚到原地址）", why)
		}

		// 库里存的是「重启后要用的」地址（空的 web_listen = 面板跟 ACS 同端口）
		newACS := settingOr(st, "listen", oldACS)
		newWeb := ""
		if v, ok, err := st.GetSetting("web_listen"); err == nil && ok {
			newWeb = strings.TrimSpace(v)
		}
		panel := newWeb
		if panel == "" {
			panel = newACS
		}
		targets := restart.ListenAddrs(newACS, newWeb)

		if restart.IsSystemd(os.Getenv) {
			// systemd 托管：只能**干净退出**，不能自己再起一个（Restart=always 马上会拉
			// 一个新的，两个进程抢同一个端口）。所以把关全靠预检：新地址现在能绑上吗？
			if err := restart.Precheck(targets, held); err != nil {
				return rollback(err)
			}
			log.Info("面板改了监听地址，退出进程交给 systemd 拉起",
				"new_listen", newACS, "new_panel_listen", panel)
			// 先把响应发回浏览器，再退出（否则用户看到的是连接被重置）
			go func() {
				time.Sleep(1200 * time.Millisecond)
				stop()
			}()
			return web.RestartSystemd, nil
		}

		// 非 systemd：把新进程要用的监听句柄凑齐 —— 手里有的直接交，没有的先建
		files := make([]*os.File, 0, len(targets))
		pass := make([]string, 0, len(targets))
		closeFiles := func() {
			for _, f := range files {
				_ = f.Close()
			}
		}
		for _, a := range targets {
			ln, held := live[a]
			if !held {
				var err error
				ln, err = net.Listen("tcp", a)
				if err != nil {
					closeFiles()
					return rollback(fmt.Errorf("%s 用不了：%v", a, restart.FriendlyListenErr(err)))
				}
			}
			f, err := ln.(*net.TCPListener).File()
			if err != nil {
				if !held {
					_ = ln.Close()
				}
				closeFiles()
				return rollback(fmt.Errorf("交出 %s 的监听句柄失败：%v", a, err))
			}
			if !held {
				_ = ln.Close() // 父进程不服务新端口，只把句柄交给新进程
			}
			files = append(files, f)
			pass = append(pass, a)
		}
		defer closeFiles() // 父进程手里那份副本用完就关（不影响递出去的）

		child, err := restart.Spawn(files, map[string]string{
			restart.InheritEnv: restart.EncodeInherit(pass, 3),
		})
		if err != nil {
			return rollback(fmt.Errorf("起新进程失败：%v", err))
		}
		// 收尸：不 Wait 的话，退出的子进程会变成僵尸，signal 0 也还探得到「活着」
		done := make(chan struct{})
		go func() {
			_ = child.Wait()
			close(done)
		}()
		alive := func() bool {
			select {
			case <-done:
				return false
			default:
				return true
			}
		}

		// 等新进程就位：
		//   - 面板地址也换了 → 必须等新地址真的回 HTTP；
		//   - 只换了 ACS 端口（面板地址没变）→ 那个地址的 socket 是递给新进程的**同一个**，
		//     去连它只会连到自己，所以只确认新进程活着，别把「连上自己」当成「新进程起来了」。
		if restart.DialAddr(panel) != restart.DialAddr(oldWeb) {
			err = restart.WaitHTTP(restart.DialAddr(panel), 8*time.Second, alive)
		} else {
			err = waitAlive(alive, 1500*time.Millisecond)
		}

		if err != nil {
			// 起不来：把新进程收拾掉，设置退回原样，服务照旧在旧地址上跑
			_ = child.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = child.Process.Kill()
			}
			return rollback(err)
		}

		log.Info("面板改了监听地址，已起新进程接管",
			"new_pid", child.Process.Pid, "new_listen", newACS, "new_panel_listen", panel)
		go func() {
			time.Sleep(1200 * time.Millisecond)
			stop() // 优雅退出：新进程已经确认在跑了
		}()
		return web.RestartSelf, nil
	}
}

// waitAlive 只等新进程活着，不碰网络。
func waitAlive(alive func() bool, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !alive() {
			return errors.New("新进程启动后很快退出了")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// writePidFile 在 ACS_PIDFILE 指定的文件里写上自己的 pid（dev-server.sh 用它做
// status/stop 的依据）。自动重启会换进程，所以**新进程自己写**才准；
// 退出时只删「还是自己的」那份，别把新进程刚写的删掉。
func writePidFile() func() {
	path := strings.TrimSpace(os.Getenv("ACS_PIDFILE"))
	if path == "" {
		return func() {}
	}
	mine := fmt.Sprintf("%d", os.Getpid())
	if err := os.WriteFile(path, []byte(mine+"\n"), 0o644); err != nil {
		return func() {}
	}
	return func() {
		b, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(b)) == mine {
			_ = os.Remove(path)
		}
	}
}
