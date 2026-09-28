package cwmp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sessionCookieName 是我们回给 CPE 的会话标识 cookie。
//
// 为什么需要它：CWMP 每个 HTTP 请求只承载一个 RPC，而 ACS 的 HTTP 是无状态的，
// 必须有个办法把「同一台 CPE 的一次会话里的多个 POST」串起来。
// 参考实现 GenieACS 用的就是这个 session cookie（CPE 支持 cookie 时会回传）。
// 不是所有 CPE 都回传 cookie，所以还有一份按「来源 IP + User-Agent」的兜底指纹。
const sessionCookieName = "session"

// Session 表示一台 CPE 的一次配置会话。
type Session struct {
	ID       string
	Key      string // 兜底指纹：IP|UA
	DeviceID int64  // 0 表示还不知道是哪台设备（Inform 之前）
	Created  time.Time
	LastSeen time.Time

	cwmpNS string // 本次会话使用的 CWMP 命名空间

	pendingTask int64 // 已下发、等待 CPE 回执的任务 ID

	lock chan struct{} // 信号量，保证同一会话串行处理
}

func (s *Session) tryLock(timeout time.Duration) bool {
	select {
	case s.lock <- struct{}{}:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (s *Session) unlock() { <-s.lock }

// sessionManager 维护所有在途会话。
type sessionManager struct {
	mu       sync.Mutex
	byID     map[string]*Session
	byKey    map[string]*Session
	byDevice map[int64]*Session
	timeout  time.Duration
}

func newSessionManager(timeout time.Duration) *sessionManager {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &sessionManager{
		byID:     map[string]*Session{},
		byKey:    map[string]*Session{},
		byDevice: map[int64]*Session{},
		timeout:  timeout,
	}
}

func newSessionID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b)
}

// sessionKeyOf 给出无 cookie 时的兜底指纹。
func sessionKeyOf(r *http.Request) string {
	ip := clientIP(r)
	ua := r.UserAgent()
	user := ""
	if u, _, ok := r.BasicAuth(); ok {
		user = u
	}
	return ip + "|" + ua + "|" + user
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// acquire 找到或新建本次请求所属的会话。
func (m *sessionManager) acquire(r *http.Request) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()

	if id := cookieValue(r, sessionCookieName); id != "" {
		if s, ok := m.byID[id]; ok {
			return s
		}
		// cookie 指向的会话已过期：沿用同一个 id 重开，避免 CPE 反复换会话
		return m.registerLocked(&Session{ID: id, Key: sessionKeyOf(r)})
	}

	key := sessionKeyOf(r)
	if s, ok := m.byKey[key]; ok {
		return s
	}
	return m.registerLocked(&Session{ID: newSessionID(), Key: key})
}

func (m *sessionManager) registerLocked(s *Session) *Session {
	now := time.Now()
	s.Created = now
	s.LastSeen = now
	s.lock = make(chan struct{}, 1)
	m.byID[s.ID] = s
	if s.Key != "" {
		m.byKey[s.Key] = s
	}
	return s
}

// bindDevice 把会话绑到具体设备上。同一设备已有别的会话时，旧会话作废
// （CPE 一次只跑一个会话，出现第二个说明旧的已经死了）。
func (m *sessionManager) bindDevice(s *Session, deviceID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.byDevice[deviceID]; ok && old != s {
		m.dropLocked(old)
	}
	s.DeviceID = deviceID
	m.byDevice[deviceID] = s
}

func (m *sessionManager) touch(s *Session) {
	m.mu.Lock()
	s.LastSeen = time.Now()
	m.mu.Unlock()
}

// end 结束一个会话（CPE 已经收到 204 或会话不再有效）。
func (m *sessionManager) end(s *Session) {
	m.mu.Lock()
	m.dropLocked(s)
	m.mu.Unlock()
}

// hasSession 判断设备当前是否有在途会话。
func (m *sessionManager) hasSession(deviceID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.byDevice[deviceID]
	return ok
}

func (m *sessionManager) dropLocked(s *Session) {
	if s == nil {
		return
	}
	if m.byID[s.ID] == s {
		delete(m.byID, s.ID)
	}
	if s.Key != "" && m.byKey[s.Key] == s {
		delete(m.byKey, s.Key)
	}
	if s.DeviceID != 0 && m.byDevice[s.DeviceID] == s {
		delete(m.byDevice, s.DeviceID)
	}
}

// janitor 定期清理超时会话。
func (m *sessionManager) janitor(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			m.mu.Lock()
			for _, s := range m.byID {
				if now.Sub(s.LastSeen) > m.timeout {
					m.dropLocked(s)
				}
			}
			m.mu.Unlock()
		}
	}
}

// cookieValue 从 Cookie 头里取出指定 cookie。
// 有些设备不按规范只用 ";" 分隔，还会用 ","、带空格，所以容错解析（同 GenieACS 做法）。
func cookieValue(r *http.Request, name string) string {
	raw := r.Header.Get("Cookie")
	if raw == "" {
		return ""
	}
	parts := strings.FieldsFunc(raw, func(c rune) bool { return c == ';' || c == ',' })
	for _, p := range parts {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if strings.TrimSpace(kv[0]) == name {
			return strings.Trim(strings.TrimSpace(kv[1]), `"`)
		}
	}
	return ""
}
