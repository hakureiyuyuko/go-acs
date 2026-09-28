package cwmp

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SendConnectionRequest 主动唤醒一台 CPE（TR-069 Annex A 的 Connection Request）。
//
// 做法就是对设备自己上报的 ConnectionRequestURL 发一个 HTTP GET。
// 设备认识这个请求之后会**立刻回连 ACS 开一次会话**（Inform 事件码 6 CONNECTION REQUEST），
// 于是我们排队的任务就不用等下一次周期上报（真机上是 120 秒）了。
//
// 认证：真机（华为）回的是 HTTP **Digest** 挑战
//
//	WWW-Authenticate: Digest realm="HuaweiHomeGateway",nonce="…",qop="auth",algorithm="MD5"
//
// 所以按 RFC 2617 算一次 response 再发。也顺手支持 Basic —— 有些设备用这个。
// 账号密码由**我们自己 provision**（写 ManagementServer.ConnectionRequestUsername/Password）：
// 真机不回读这两个参数，但它们是可写的。
func SendConnectionRequest(rawURL, username, password string, timeout time.Duration) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("ConnectionRequestURL 不是合法 URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("不支持的协议 %q（只允许 http/https）", u.Scheme)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	client := &http.Client{
		Timeout: timeout,
		// 不要跟随跳转：ConnectionRequestURL 不该跳转，跟随反而可能被引到别处
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	do := func(auth string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "light-acs")
		req.Header.Set("Connection", "close")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		return client.Do(req)
	}

	resp, err := do("")
	if err != nil {
		return fmt.Errorf("连接设备失败: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil // 设备没要认证（或者本地已有会话），直接成功
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("设备返回 %s", resp.Status)
	}

	challenge := resp.Header.Get("WWW-Authenticate")
	if challenge == "" {
		return fmt.Errorf("设备要认证但没给 WWW-Authenticate")
	}
	if username == "" {
		return fmt.Errorf("设备要求认证（%s），但我们还没有该设备的 ConnectionRequest 账号密码", truncateStr(challenge, 80))
	}

	auth, err := buildAuthorization(challenge, http.MethodGet, u, username, password)
	if err != nil {
		return err
	}
	resp2, err := do(auth)
	if err != nil {
		return fmt.Errorf("带认证重试失败: %w", err)
	}
	defer resp2.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp2.Body, 4096))
	if resp2.StatusCode >= 200 && resp2.StatusCode < 300 {
		return nil
	}
	if resp2.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("认证未通过（设备仍然是 401）—— 设备上的 ConnectionRequest 账号密码和我们的不一致")
	}
	return fmt.Errorf("带认证后设备返回 %s", resp2.Status)
}

// buildAuthorization 按挑战头拼出 Authorization。
func buildAuthorization(challenge, method string, u *url.URL, username, password string) (string, error) {
	scheme, params := splitAuthHeader(challenge)
	switch strings.ToLower(scheme) {
	case "basic":
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password)), nil
	case "digest":
		return buildDigest(params, method, u, username, password)
	default:
		return "", fmt.Errorf("不支持的认证方式 %q", scheme)
	}
}

// digestParam 是 WWW-Authenticate 里的一项。
type digestParam struct {
	value  string
	quoted bool
}

// splitAuthHeader 把 `Digest realm="a",nonce="b"` 拆成方案名和参数表。
func splitAuthHeader(h string) (string, map[string]digestParam) {
	h = strings.TrimSpace(h)
	i := strings.IndexAny(h, " \t")
	if i < 0 {
		return h, nil
	}
	scheme, rest := h[:i], strings.TrimSpace(h[i+1:])

	params := map[string]digestParam{}
	for _, part := range splitTopLevel(rest) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(kv[0]))
		v := strings.TrimSpace(kv[1])
		p := digestParam{}
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			p.value, p.quoted = v[1:len(v)-1], true
		} else {
			p.value = v
		}
		params[k] = p
	}
	return scheme, params
}

// splitTopLevel 按逗号切分，但不动引号里的逗号（nonce/opaque 里可能有）。
func splitTopLevel(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, strings.TrimSpace(cur.String()))
	}
	return out
}

func buildDigest(params map[string]digestParam, method string, u *url.URL, username, password string) (string, error) {
	realm := params["realm"].value
	nonce := params["nonce"].value
	if nonce == "" {
		return "", fmt.Errorf("挑战里没有 nonce")
	}
	algorithm := strings.ToUpper(params["algorithm"].value)
	if algorithm == "" {
		algorithm = "MD5"
	}
	if algorithm != "MD5" {
		return "", fmt.Errorf("暂不支持 Digest 算法 %s（只实现了 MD5）", algorithm)
	}

	// 真机上 CPE 的 challenge 里带的有可能是空的 qop。TR-069 Annex A 要求 qop=auth，
	// 但也有设备不发或发空串，这时按无 qop 的老式算法算。
	qopRaw := params["qop"].value
	qop := ""
	for _, q := range strings.Split(qopRaw, ",") {
		if strings.EqualFold(strings.TrimSpace(q), "auth") {
			qop = "auth"
			break
		}
	}

	uri := u.RequestURI()
	if uri == "" {
		uri = "/"
	}

	ha1 := md5hex(username + ":" + realm + ":" + password)
	ha2 := md5hex(method + ":" + uri)

	var response string
	var extra string
	if qop == "auth" {
		nc := "00000001"
		cnonce := randomHex(8)
		response = md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
		extra = fmt.Sprintf(", qop=%s, nc=%s, cnonce=\"%s\"", qop, nc, cnonce)
	} else {
		response = md5hex(ha1 + ":" + nonce + ":" + ha2)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=%s`,
		username, realm, nonce, uri, response, algorithm)
	b.WriteString(extra)
	if p, ok := params["opaque"]; ok && p.value != "" {
		fmt.Fprintf(&b, `, opaque="%s"`, p.value)
	}
	return b.String(), nil
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
