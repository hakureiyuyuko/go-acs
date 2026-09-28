package web

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// 面板设置页：在线修改两个监听端口与面板账号密码。
//
// 存在 settings 表里（跟其它配置一个地方），**重启后生效** ——
// 监听端口没法在不重启的情况下安全切换（换了端口，正在访问的这个连接就断了，
// 而且 CWMP 端口一断，在线设备会报「连不上 ACS」）。所以页面上如实写明「重启后生效」，
// 并把「当前生效」与「保存后使用」两栏并排显示，避免改完不知道为什么没变。
//
// settings 键：
//
//	listen      ACS（CWMP）监听地址
//	web_listen  面板监听地址（空 = 与 ACS 同一个端口，一套路由两用）
//	web_user    面板账号（空 = 不启用账号密码保护）
//	web_pass    面板密码散列（PBKDF2，见 auth.go）
const (
	settingListen    = "listen"
	settingWebListen = "web_listen"
	settingWebUser   = "web_user"
	settingWebPass   = "web_pass"
)

// RuntimeSettings 是「当前生效」那一栏的值。
//
// 监听地址是进程启动时定的（改它必须重启），账号密码则是可在线更新的
// （Creds 每次请求都读当前值，设置页保存后立即生效）。
type RuntimeSettings struct {
	ACSListen string
	WebListen string
	// Path 是 CWMP 端点路径（默认 /acs）：设置页拿它拼一个示例 ACS 地址。
	Path string
}

// settingsView 是设置页要渲染的数据。
type settingsView struct {
	Runtime RuntimeSettings
	// 面板账号（当前生效值，改完立即生效）
	WebUser string
	WebAuth bool

	// 库里存着的监听地址（保存后重启才生效）
	StoredListen    string
	StoredWebListen string
	// HasPassword 表示已经设过密码（新密码留空 = 不修改）
	HasPassword bool
	// PendingPorts 表示监听地址改过、还没重启
	PendingPorts bool

	Notice    string
	NoticeErr bool
}

// handleSettings 渲染设置页。
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	v := settingsView{
		Runtime:         s.opt.Runtime,
		StoredListen:    s.opt.Runtime.ACSListen,
		StoredWebListen: s.opt.Runtime.WebListen,
		Notice:          strings.TrimSpace(r.URL.Query().Get("msg")),
		NoticeErr:       r.URL.Query().Get("err") == "1",
	}
	if s.opt.Auth != nil {
		v.WebUser, _ = s.opt.Auth.Get()
		v.WebAuth = s.opt.Auth.Enabled()
	}
	if x, ok, err := s.store.GetSetting(settingListen); err == nil && ok && strings.TrimSpace(x) != "" {
		v.StoredListen = strings.TrimSpace(x)
	}
	if x, ok, err := s.store.GetSetting(settingWebListen); err == nil && ok {
		v.StoredWebListen = strings.TrimSpace(x)
	}
	if x, ok, err := s.store.GetSetting(settingWebPass); err == nil && ok {
		v.HasPassword = strings.TrimSpace(x) != ""
	}
	v.PendingPorts = v.StoredListen != v.Runtime.ACSListen ||
		v.StoredWebListen != v.Runtime.WebListen
	s.render(w, "settings.html", v)
}

// handleSettingsSave 保存设置（校验完再写库）。
func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	back := "/settings"
	fail := func(msg string) {
		http.Redirect(w, r, back+"?msg="+url.QueryEscape(msg)+"&err=1", http.StatusSeeOther)
	}

	listen, err := normalizeListen(strings.TrimSpace(r.FormValue("acs_listen")))
	if err != nil {
		fail("ACS 监听地址不对：" + err.Error())
		return
	}
	if listen == "" {
		fail("ACS 监听地址不能为空")
		return
	}
	webListen, err := normalizeListen(strings.TrimSpace(r.FormValue("web_listen")))
	if err != nil {
		fail("面板监听地址不对：" + err.Error())
		return
	}

	authOn := r.FormValue("auth") == "1"
	user := strings.TrimSpace(r.FormValue("web_user"))
	pass := r.FormValue("web_pass")
	pass2 := r.FormValue("web_pass2")
	storedPass := ""
	if x, ok, err := s.store.GetSetting(settingWebPass); err == nil && ok {
		storedPass = strings.TrimSpace(x)
	}
	hasPass := storedPass != ""

	if authOn {
		if !validPanelUser(user) {
			fail("面板账号要 2–32 位，只能用字母、数字与 . _ - @")
			return
		}
		if pass == "" && !hasPass {
			fail("启用了账号密码保护，就得设一个密码")
			return
		}
		if pass != "" && len(pass) < 6 {
			fail("密码至少 6 位")
			return
		}
		if pass != pass2 {
			fail("两次输入的密码不一样")
			return
		}
	} else if pass != "" && pass != pass2 {
		fail("两次输入的密码不一样")
		return
	}

	if err := s.store.SetSetting(settingListen, listen); err != nil {
		fail("保存失败：" + err.Error())
		return
	}
	if err := s.store.SetSetting(settingWebListen, webListen); err != nil {
		fail("保存失败：" + err.Error())
		return
	}
	if authOn {
		if err := s.store.SetSetting(settingWebUser, user); err != nil {
			fail("保存失败：" + err.Error())
			return
		}
		if pass != "" {
			hash, err := HashPassword(pass)
			if err != nil {
				fail("密码散列失败：" + err.Error())
				return
			}
			if err := s.store.SetSetting(settingWebPass, hash); err != nil {
				fail("保存失败：" + err.Error())
				return
			}
		}
	} else {
		// 关掉保护：把账号清空即可（散列留着不碍事，想彻底清可以自己删掉 web_pass）
		if err := s.store.SetSetting(settingWebUser, ""); err != nil {
			fail("保存失败：" + err.Error())
			return
		}
		user = ""
	}

	// 账号密码**立即生效**（不用重启）：直接更新内存里那份凭据。
	if s.opt.Auth != nil {
		switch {
		case !authOn:
			s.opt.Auth.Set("", "")
		case pass != "":
			hash := ""
			if saved, ok, err := s.store.GetSetting(settingWebPass); err == nil && ok {
				hash = strings.TrimSpace(saved)
			}
			s.opt.Auth.Set(user, hash)
		default:
			// 只改了账号、没改密码：散列保持库里那份
			s.opt.Auth.Set(user, storedPass)
		}
	}

	msg := "已保存。"
	if authOn {
		msg += "账号密码已立即生效。"
	} else {
		msg += "面板账号密码保护已关闭（立即生效）。"
	}
	if webListen == "" || webListen == listen {
		msg += "监听地址（面板与 ACS 共用 " + listen + "）重启 ACS 后生效。"
	} else {
		msg += "监听地址（面板 " + webListen + "、ACS " + listen + "）重启 ACS 后生效。"
	}
	http.Redirect(w, r, back+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// normalizeListen 规整监听地址：接受 "9090"、":9090"、"0.0.0.0:9090"、"[::1]:9090"。
// 空串表示「与 ACS 同一个端口」（面板监听才允许，调用方判断）。
func normalizeListen(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if !strings.Contains(v, ":") {
		v = ":" + v
	}
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		return "", fmt.Errorf("要写成 [主机]:端口，例如 :9090（%v）", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("端口要在 1–65535 之间")
	}
	if host != "" {
		if ip := net.ParseIP(host); ip == nil && host != "localhost" {
			return "", fmt.Errorf("主机部分要写 IP（或留空表示所有网卡）")
		}
	}
	return net.JoinHostPort(host, port), nil
}
