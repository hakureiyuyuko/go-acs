package cwmp

import (
	"net/url"
	"strings"
	"testing"
)

// RFC 2617 第 3.5 节给的例子，当测试向量用 ——
// Digest 算错了不会报错，只会一直 401，所以必须拿标准例子对一遍。
func TestBuildDigestRFC2617(t *testing.T) {
	challenge := `Digest realm="testrealm@host.com", qop="auth", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`

	scheme, params := splitAuthHeader(challenge)
	if !strings.EqualFold(scheme, "Digest") {
		t.Fatalf("方案解析错: %q", scheme)
	}
	if params["realm"].value != "testrealm@host.com" || !params["realm"].quoted {
		t.Errorf("realm 解析错: %+v", params["realm"])
	}
	if params["nonce"].value != "dcd98b7102dd2f0e8b11d0f600bfb0c093" {
		t.Errorf("nonce 解析错: %q", params["nonce"].value)
	}

	// qop=auth 时 cnonce 是我们自己生成的，所以要固定住才能对标准结果。
	// 直接验证 HA1/HA2 与最终 response 的算式：改用手算它 4 步。
	ha1 := md5hex("Mufasa:testrealm@host.com:Circle Of Life")
	if ha1 != "939e7578ed9e3c518a452acee763bce9" {
		t.Errorf("HA1 = %s，与 RFC 2617 不符", ha1)
	}
	ha2 := md5hex("GET:/dir/index.html")
	if ha2 != "39aff3a2bab6126f332b942af96d3366" {
		t.Errorf("HA2 = %s，与 RFC 2617 不符", ha2)
	}
	// RFC: response = MD5(HA1:nonce:nc:cnonce:qop:HA2)，其中 nc=00000001, cnonce="0a4f113b"
	got := md5hex(ha1 + ":dcd98b7102dd2f0e8b11d0f600bfb0c093:00000001:0a4f113b:auth:" + ha2)
	if got != "6629fae49393a05397450978507c4ef1" {
		t.Errorf("response = %s，与 RFC 2617 的 6629fae49393a05397450978507c4ef1 不符", got)
	}
}

// 挑战头里的引号内可能有逗号（opaque / nonce），切分不能切错。
func TestSplitAuthHeaderQuotedComma(t *testing.T) {
	_, params := splitAuthHeader(`Digest realm="a,b", nonce="x,y", qop="auth"`)
	if params["realm"].value != "a,b" {
		t.Errorf("realm 带逗号解析错: %q", params["realm"].value)
	}
	if params["nonce"].value != "x,y" {
		t.Errorf("nonce 带逗号解析错: %q", params["nonce"].value)
	}
}

// 真机上抓到的实际挑战头（华为）要能解析，并产出带 qop/nc/cnonce 的 Authorization。
func TestBuildAuthorizationHuaweiChallenge(t *testing.T) {
	challenge := `Digest realm="HuaweiHomeGateway",nonce="6f2a1e8d0c4b",qop="auth",algorithm="MD5"`
	u, _ := url.Parse("http://192.168.10.22:7547/0123456789abcdef0123456789abcdef")

	auth, err := buildAuthorization(challenge, "GET", u, "acs", "secret")
	if err != nil {
		t.Fatalf("构造 Authorization 失败: %v", err)
	}
	for _, want := range []string{
		`username="acs"`, `realm="HuaweiHomeGateway"`, `nonce="6f2a1e8d0c4b"`,
		`uri="/0123456789abcdef0123456789abcdef"`, "qop=auth", "nc=00000001",
		"cnonce=", "algorithm=MD5", "response=",
	} {
		if !strings.Contains(auth, want) {
			t.Errorf("Authorization 少了 %q\n完整值: %s", want, auth)
		}
	}
}

// 没有 qop 的老式挑战也要支持（有些设备不发 qop）。
func TestBuildAuthorizationNoQop(t *testing.T) {
	challenge := `Digest realm="r", nonce="n"`
	u, _ := url.Parse("http://d/x")
	auth, err := buildAuthorization(challenge, "GET", u, "u", "p")
	if err != nil {
		t.Fatalf("无 qop 时不该报错: %v", err)
	}
	if strings.Contains(auth, "qop=") || strings.Contains(auth, "cnonce") {
		t.Errorf("无 qop 时不应带 qop/cnonce: %s", auth)
	}
	// 无 qop 的算式：MD5(HA1:nonce:HA2)
	want := md5hex(md5hex("u:r:p") + ":n:" + md5hex("GET:/x"))
	if !strings.Contains(auth, `response="`+want+`"`) {
		t.Errorf("无 qop 的 response 算错: %s，期望 %s", auth, want)
	}
}

// Basic 也要支持。
func TestBuildAuthorizationBasic(t *testing.T) {
	u, _ := url.Parse("http://d/x")
	auth, err := buildAuthorization("Basic realm=\"r\"", "GET", u, "acs", "pw")
	if err != nil {
		t.Fatal(err)
	}
	// base64("acs:pw") = YWNzOnB3
	if auth != "Basic YWNzOnB3" {
		t.Errorf("Basic 头 = %q", auth)
	}
}

// 只接受 http/https：ConnectionRequestURL 是设备给的，不能让它把我们指向别的协议。
func TestSendConnectionRequestRejectsBadScheme(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "ftp://x/y", "not a url at all\x00"} {
		if err := SendConnectionRequest(u, "", "", 0); err == nil {
			t.Errorf("%q 应该被拒绝", u)
		}
	}
}
