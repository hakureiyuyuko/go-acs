package web

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// 面板（Web GUI）的账号密码保护。
//
// 用 **HTTP Basic**：轻量、浏览器与脚本都能直接用（`curl -u user:pass`），
// 不需要会话存储、也不会因为进程重启把人踢出去。代价是密码随每个请求以 Base64 发出，
// 在明文 HTTP 上等于明文 —— 所以设置页上写明了「建议只在内网使用，或放在 HTTPS 反代后面」。
//
// 密码**只存散列**：PBKDF2-HMAC-SHA256（随机盐 + 迭代次数），入库格式
//
//	pbkdf2-sha256$<迭代次数>$<盐 hex>$<散列 hex>
//
// 忘了密码没法反推：清掉 settings 表里的 web_pass（或整个 web_* ）重启，就回到无鉴权状态。
const (
	pbkdf2Iters  = 120000
	pbkdf2KeyLen = 32
	pbkdf2Scheme = "pbkdf2-sha256"
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

// Creds 是面板凭据。
//
// 特意做成**可在运行中更新**的指针对象：改账号密码是能立刻生效的（不需要重启），
// 只有监听端口才必须重启 —— 换端口会把当前连接和在线设备的上报一起打断。
// 每次请求都读一次当前值，所以设置页保存后马上就按新账号密码校验。
type Creds struct {
	mu   sync.RWMutex
	user string
	hash string
}

// NewCreds 建一份凭据（user 为空 = 不启用鉴权）。
func NewCreds(user, hash string) *Creds {
	return &Creds{user: strings.TrimSpace(user), hash: strings.TrimSpace(hash)}
}

// Set 更新凭据（设置页保存时调用，立即生效）。
func (c *Creds) Set(user, hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.user, c.hash = strings.TrimSpace(user), strings.TrimSpace(hash)
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

// Guard 给面板路由裹上 Basic 鉴权。没启用时原样放行。
func (c *Creds) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !c.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		user, hash := c.Get()
		u, p, ok := r.BasicAuth()
		// 用户名与密码都比较（常数时间），密码只存散列。
		// 配置不完整（有账号没散列）时一律拒绝。
		if !ok || hash == "" ||
			subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			!VerifyPassword(hash, p) {
			w.Header().Set("WWW-Authenticate", `Basic realm="light-acs", charset="UTF-8"`)
			http.Error(w, "需要登录", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
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
