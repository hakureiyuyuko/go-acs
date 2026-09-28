package web

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 面板（Web GUI）的登录与账号密码保护。
//
// 形态：**独立登录页 + 会话 cookie**（不是 HTTP Basic）。原因：
//
//   - Basic 没法「退出登录」：浏览器会把凭据一直带着，关标签页也没用；
//   - 密码随每个请求以 Base64 发出（明文 HTTP 上等于明文），且无法吊销；
//   - 面板上的写操作（重启、删设备、改 WiFi）用一次性登录态更合适。
//
// 登录态是**签名 cookie**（无服务端会话表）：
//
//		v1.<epoch>.<到期时间戳>.<HMAC-SHA256>
//
//	  - 密钥存在 settings 表里（`panel_secret`），重启不失效、也不会每次重启把人踢出去；
//	  - epoch 在**改密码时自增**，于是所有旧登录态立刻失效（当年在别的机器上登录过的也一起踢掉）；
//	  - cookie 是 HttpOnly + SameSite=Lax（防 JS 读取、防跨站 POST），默认 7 天，
//	    活跃使用时会自动续期（剩余不足一半就重发一次）。
//
// 密码**只存散列**：PBKDF2-HMAC-SHA256（随机盐 + 迭代次数），入库格式
//
//	pbkdf2-sha256$<迭代次数>$<盐 hex>$<散列 hex>
//
// 忘了密码没法反推：清掉 settings 表里的 web_pass（或整个 web_* ）重启，就回到无鉴权状态；
// 也可以用 ACS_WEB_AUTH=off 临时关掉。
const (
	pbkdf2Iters  = 120000
	pbkdf2KeyLen = 32
	pbkdf2Scheme = "pbkdf2-sha256"
)

// 面板登录态相关常量。
const (
	// panelCookieName 是登录态 cookie 名。
	panelCookieName = "acs_panel"
	// panelSessionTTL 是登录态有效期（活跃使用会续期，不动就过期）。
	panelSessionTTL = 7 * 24 * time.Hour
	// panelLoginPath 是登录页路径。
	panelLoginPath = "/login"
	// loginFailWindow / loginFailLimit：同一来源连续失败多少次后短暂锁定。
	loginFailWindow = 5 * time.Minute
	loginFailLimit  = 8
	loginLockFor    = time.Minute
)

// HashPassword 生成可入库的密码散列。
func HashPassword(pass string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pass, salt, pbkdf2Iters, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", pbkdf2Scheme, pbkdf2Iters,
		hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

// VerifyPassword 校验密码（常数时间比较，避免计时侧信道）。
func VerifyPassword(stored, pass string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Scheme {
		return false
	}
	iters, err := strconv.Atoi(parts[1])
	if err != nil || iters <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pass, salt, iters, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Creds 是面板凭据 + 登录态管理器。
//
// 特意做成**可在运行中更新**的指针对象：改账号密码是能立刻生效的（不需要重启），
// 只有监听端口才必须重启 —— 换端口会把当前连接和在线设备的上报一起打断。
type Creds struct {
	mu     sync.RWMutex
	user   string
	hash   string
	secret []byte // 登录态 cookie 的 HMAC 密钥
	epoch  int64  // 改密码就 +1，让所有旧登录态失效

	failsMu sync.Mutex
	fails   map[string]*loginFails

	now func() time.Time // 便于单测注入时间
}

type loginFails struct {
	count int
	last  time.Time
}

// NewCreds 建一份凭据（user 为空 = 不启用鉴权）。
// secret 是登录态 cookie 的签名密钥；为空时用进程内随机值（重启会踢掉所有人，谨慎）。
func NewCreds(user, hash string, secret []byte) *Creds {
	if len(secret) == 0 {
		secret = make([]byte, 32)
		_, _ = rand.Read(secret)
	}
	return &Creds{
		user:   strings.TrimSpace(user),
		hash:   strings.TrimSpace(hash),
		secret: secret,
		fails:  map[string]*loginFails{},
		now:    time.Now,
	}
}

// Set 更新凭据（设置页保存时调用，立即生效）。
//
// 用户名或密码变了就把 epoch +1：所有旧登录态立刻失效 —— 改完密码得重新登录，
// 别处（别人的浏览器）也不会再用旧票进来。
func (c *Creds) Set(user, hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	user, hash = strings.TrimSpace(user), strings.TrimSpace(hash)
	if user != c.user || hash != c.hash {
		c.epoch++
	}
	c.user, c.hash = user, hash
}

// Get 读当前凭据。
func (c *Creds) Get() (user, hash string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.user, c.hash
}

// Enabled 表示是否启用鉴权。
func (c *Creds) Enabled() bool {
	if c == nil {
		return false
	}
	user, _ := c.Get()
	return user != ""
}

// ---------- 登录态 ----------

// sign 给一段载荷签名。
func (c *Creds) sign(payload string) string {
	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// issueToken 生成一个新的登录态 token。
func (c *Creds) issueToken() string {
	c.mu.RLock()
	epoch, user := c.epoch, c.user
	c.mu.RUnlock()
	exp := c.now().Add(panelSessionTTL).Unix()
	payload := fmt.Sprintf("v1|%d|%d|%s", epoch, exp, user)
	return fmt.Sprintf("v1.%d.%d.%s", epoch, exp, c.sign(payload))
}

// checkToken 校验 token（签名、epoch、有效期），返回剩余有效期是否还够。
func (c *Creds) checkToken(tok string) (ok bool, needRenew bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return false, false
	}
	epoch, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false, false
	}
	exp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return false, false
	}
	c.mu.RLock()
	curEpoch, user := c.epoch, c.user
	c.mu.RUnlock()
	payload := fmt.Sprintf("v1|%d|%d|%s", epoch, exp, user)
	if subtle.ConstantTimeCompare([]byte(parts[3]), []byte(c.sign(payload))) != 1 {
		return false, false
	}
	if epoch != curEpoch {
		// 改过密码（或换过账号）：旧票作废
		return false, false
	}
	now := c.now()
	if now.Unix() >= exp {
		return false, false
	}
	return true, exp-now.Unix() < int64(panelSessionTTL.Seconds())/2
}

// LoggedIn 判断这个请求是否带着有效的登录态。
func (c *Creds) LoggedIn(r *http.Request) bool {
	if !c.Enabled() {
		return true
	}
	ck, err := r.Cookie(panelCookieName)
	if err != nil || ck.Value == "" {
		return false
	}
	ok, _ := c.checkToken(ck.Value)
	return ok
}

// setCookie 下发（或续期）登录态。
func (c *Creds) setCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     panelCookieName,
		Value:    c.issueToken(),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r != nil && r.TLS != nil, // 走了 HTTPS 反代就别让 cookie 落到明文通道
		MaxAge:   int(panelSessionTTL.Seconds()),
	})
}

// clearCookie 清掉登录态（退出登录）。
func (c *Creds) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     panelCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// Guard 给面板路由裹上登录校验。没启用时原样放行。
//
// 未登录时：API（/api/）返回 401 JSON，页面请求 303 跳到登录页并把当前地址放进 next。
func (c *Creds) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		if c.LoggedIn(r) {
			// 剩余有效期不足一半时顺手续一下，别让天天在用的管理员突然被踢
			if ck, err := r.Cookie(panelCookieName); err == nil {
				if _, renew := c.checkToken(ck.Value); renew {
					c.setCookie(w, r)
				}
			}
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"需要登录"}`))
			return
		}
		http.Redirect(w, r, panelLoginPath+"?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	})
}

// ---------- 登录 / 退出 ----------

// safeNext 只允许跳到本机路径，避免 next 变成开放重定向。
func safeNext(v string) string {
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.Contains(v, ":") {
		return "/"
	}
	if strings.HasPrefix(v, panelLoginPath) {
		return "/"
	}
	return v
}

// handleLoginPage 渲染登录页（GET /login）。
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	next := safeNext(strings.TrimSpace(r.URL.Query().Get("next")))
	if !s.authEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if s.opt.Auth.LoggedIn(r) {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.renderLang(w, r, "login.html", map[string]any{
		"Next":  next,
		"Error": "",
		"User":  "",
	})
}

// handleLoginSubmit 校验账号密码并下发登录态（POST /login）。
func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	next := safeNext(strings.TrimSpace(r.FormValue("next")))
	user := strings.TrimSpace(r.FormValue("user"))
	pass := r.FormValue("pass")
	ip := clientIPOf(r)

	if locked, wait := s.opt.Auth.locked(ip); locked {
		s.renderLoginError(w, r, next, user,
			fmt.Sprintf("失败次数太多，请 %.0f 秒后再试", wait.Seconds()))
		return
	}
	if s.opt.Auth.verify(user, pass) {
		s.opt.Auth.clearFails(ip)
		s.opt.Auth.setCookie(w, r)
		s.logAuth("面板登录成功", ip, user)
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.opt.Auth.noteFail(ip)
	// 固定延迟一下，别让暴力破解跑得太快
	select {
	case <-time.After(400 * time.Millisecond):
	case <-r.Context().Done():
	}
	s.logAuth("面板登录失败", ip, user)
	s.renderLoginError(w, r, next, user, "账号或密码不对")
}

// handleLogout 退出登录（POST /logout，也接受 GET 方便直接点链接）。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.opt.Auth != nil {
		s.opt.Auth.clearCookie(w)
	}
	http.Redirect(w, r, panelLoginPath, http.StatusSeeOther)
}

func (s *Server) renderLoginError(w http.ResponseWriter, r *http.Request, next, user, msg string) {
	s.renderStatus(w, r, http.StatusUnauthorized, "login.html", map[string]any{
		"Next":  next,
		"Error": msg,
		"User":  user,
	})
}

func (s *Server) authEnabled() bool {
	return s.opt.Auth != nil && s.opt.Auth.Enabled()
}

// logAuth 记一条面板登录日志（登录成功/失败都是值得留痕的事件）。
func (s *Server) logAuth(msg, ip, user string) {
	if s.log != nil {
		s.log.Info(msg, "ip", ip, "user", user)
	}
}

// clientIPOf 取来源 IP（登录失败计数按这个分配）。
func clientIPOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// verify 校验账号密码（用户名与密码都比较，密码只存散列）。
func (c *Creds) verify(user, pass string) bool {
	wantUser, hash := c.Get()
	if hash == "" || wantUser == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(user), []byte(wantUser)) != 1 {
		return false
	}
	return VerifyPassword(hash, pass)
}

// ---------- 失败次数限制（同一来源短时间连续失败就锁一会儿）----------

func (c *Creds) locked(ip string) (bool, time.Duration) {
	c.failsMu.Lock()
	defer c.failsMu.Unlock()
	f, ok := c.fails[ip]
	if !ok {
		return false, 0
	}
	if c.now().Sub(f.last) > loginFailWindow {
		delete(c.fails, ip)
		return false, 0
	}
	if f.count < loginFailLimit {
		return false, 0
	}
	unlock := f.last.Add(loginLockFor)
	if d := unlock.Sub(c.now()); d > 0 {
		return true, d
	}
	delete(c.fails, ip)
	return false, 0
}

func (c *Creds) noteFail(ip string) {
	c.failsMu.Lock()
	defer c.failsMu.Unlock()
	f := c.fails[ip]
	if f == nil || c.now().Sub(f.last) > loginFailWindow {
		f = &loginFails{}
		c.fails[ip] = f
	}
	f.count++
	f.last = c.now()
}

func (c *Creds) clearFails(ip string) {
	c.failsMu.Lock()
	defer c.failsMu.Unlock()
	delete(c.fails, ip)
}

// validPanelUser 校验面板用户名（只允许常见字符，避免把日志/表单搞乱）。
func validPanelUser(u string) bool {
	if len(u) < 2 || len(u) > 32 {
		return false
	}
	for _, r := range u {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '@':
		default:
			return false
		}
	}
	return true
}
