package web

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/hakureiyuyuko/go-acs/internal/i18n"
)

// 面板上「改完监听地址后自动重启服务」。
//
// 由来：改监听端口必须重启进程才生效（端口是启动时绑的）。以前界面只写一句
// 「重启服务后生效」，剩下的让用户自己去命令行 —— 对装完就不碰命令行的人等于改不动。
// 现在：设置页发现地址改过就给出「立即重启服务」按钮（带二次确认），
// 确认后由 cmd/acs 注入的 Restart 真把服务重启起来，浏览器再跳到新地址。
//
// 这里只负责界面与「重启成功之后怎么告诉用户」；换进程、失败回滚那些事在 cmd/acs 里
// （见 internal/restart 与 main.restartService）。

// handleSettingsRestart 执行「立即重启服务」。
func (s *Server) handleSettingsRestart(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) {
		http.Redirect(w, r, "/settings?msg="+url.QueryEscape(msg)+"&err=1", http.StatusSeeOther)
	}

	if s.opt.Restart == nil {
		fail("这个运行方式不支持自动重启，请手动重启服务")
		return
	}

	// 重启后面板会落在哪个地址：面板监听为空 = 面板就在 ACS 那个端口上。
	newWebListen := s.opt.Runtime.ACSListen
	if v, ok, err := s.store.GetSetting(settingWebListen); err == nil && ok && strings.TrimSpace(v) != "" {
		newWebListen = strings.TrimSpace(v)
	}

	mode, err := s.opt.Restart()
	if err != nil {
		fail("重启失败：" + err.Error() + "，服务仍在原地址运行")
		return
	}

	lang := s.langOf(w, r)
	newURL := panelBaseURL(r, newWebListen)
	oldURL := panelBaseURL(r, s.opt.Runtime.WebListen)
	if strings.TrimSpace(s.opt.Runtime.WebListen) == "" {
		oldURL = panelBaseURL(r, s.opt.Runtime.ACSListen)
	}
	same := newURL == "" || newURL == oldURL

	note := i18n.T(lang, "新进程已经起来，新地址可以访问了。")
	if mode == RestartSystemd {
		// systemd 托管：我们只是干净退出，systemd 随后按 RestartSec 把它拉起来
		note = i18n.T(lang, "服务已退出，正在由 systemd 拉起（通常几秒）。")
	}
	if !same {
		// 面板换了端口：当前这个连接马上会被关掉，让浏览器明确知道响应到此为止
		w.Header().Set("Connection", "close")
	}

	s.renderLang(w, r, "restart.html", map[string]any{
		"NewURL":      newURL,
		"SameAddress": same,
		"Note":        note,
		"AutoSeconds": 3,
		"Notice":      strings.TrimSpace(r.URL.Query().Get("msg")),
		"NoticeErr":   r.URL.Query().Get("err") == "1",
		"AuthOn":      s.authEnabled(),
	})
}

// panelBaseURL 算出「浏览器该用哪个地址访问面板」。
//
// 主机名沿用请求里的（保住用户是从 127.0.0.1 还是从内网 IP 进来的），端口换成新的；
// 新监听里如果写死了具体 IP，就直接用它 —— 那才是面板真正绑上的地址。
func panelBaseURL(r *http.Request, listen string) string {
	_, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil || port == "" {
		return ""
	}
	name := strings.TrimSpace(r.Host)
	if h, _, err := net.SplitHostPort(name); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	if host, _, err := net.SplitHostPort(strings.TrimSpace(listen)); err == nil {
		if host != "" && host != "0.0.0.0" && host != "::" {
			name = host
		}
	}
	if name == "" {
		name = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(name, port) + "/"
}
