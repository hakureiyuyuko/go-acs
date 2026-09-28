package cwmp

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// 标准命名空间。CPE 用什么前缀都行，这里定义的是 URI。
const (
	NSSoapEnv = "http://schemas.xmlsoap.org/soap/envelope/"
	NSSoapEnc = "http://schemas.xmlsoap.org/soap/encoding/"
	NSXSD     = "http://www.w3.org/2001/XMLSchema"
	NSXSI     = "http://www.w3.org/2001/XMLSchema-instance"

	// CWMP 命名空间 = cwmpNSBase + 版本号，版本号 1.0 ~ 1.4
	cwmpNSBase = "urn:dslforum-org:cwmp-"

	// DefaultCWMPVersion 是探测不到版本时的兜底。
	DefaultCWMPVersion = "1.0"
)

// CWMPVersionOf 从命名空间 URI 反推协议版本，如 "urn:dslforum-org:cwmp-1-2" -> "1.2"。
func CWMPVersionOf(ns string) (string, bool) {
	if !strings.HasPrefix(ns, cwmpNSBase) {
		return "", false
	}
	v := strings.TrimPrefix(ns, cwmpNSBase)
	if v == "" {
		return "", false
	}
	return v, true
}

// Envelope 是从报文里抽出来的关键信息。
type Envelope struct {
	ID     string // cwmp:ID，必须在响应里原样回填
	CWMPNS string // CPE 使用的 CWMP 命名空间（响应要与之保持一致）
	Method *Node  // Body 里的那个 RPC（可能是请求，也可能是响应）
	Body   *Node  // Body 节点本身
	Raw    *Node  // 整个 Envelope 节点
}

// ParseEnvelope 从一个解析好的 Node 树里提取信封信息。
func ParseEnvelope(root *Node) (*Envelope, error) {
	if root == nil {
		return nil, fmt.Errorf("空文档")
	}
	if root.Local != "Envelope" {
		// 有些设备会多包一层，尝试往下找一次
		if env := root.Child("Envelope"); env != nil {
			root = env
		} else {
			return nil, fmt.Errorf("根元素不是 Envelope，而是 <%s>", root.Local)
		}
	}

	env := &Envelope{Raw: root}
	if h := root.Child("Header"); h != nil {
		if id := h.Child("ID"); id != nil {
			env.ID = id.Trimmed()
		}
	}

	body := root.Child("Body")
	if body == nil {
		return nil, fmt.Errorf("缺少 soap:Body")
	}
	env.Body = body
	if m := body.FirstChild(); m != nil {
		env.Method = m
		// 注意：这里要保存完整的命名空间 URI（urn:dslforum-org:cwmp-1-2），
		// 不能只存版本号 —— 响应里的 xmlns:cwmp 必须是完整 URI。
		if _, ok := CWMPVersionOf(m.NS); ok {
			env.CWMPNS = m.NS
		}
	}
	if env.CWMPNS == "" {
		// 探测不到就按兜底版本响应，并保持自身一致
		env.CWMPNS = cwmpNSBase + DefaultCWMPVersion
	}
	return env, nil
}

// esc 转义文本节点/属性值。
func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// NewEnvelope 拼一个完整的 SOAP 信封。
// cwmpNS 为完整命名空间 URI（空则用兜底版本）；id 为空时用 "0"。
func NewEnvelope(cwmpNS, id, bodyXML string) string {
	if cwmpNS == "" {
		cwmpNS = cwmpNSBase + DefaultCWMPVersion
	}
	if id == "" {
		id = "0"
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<soap-env:Envelope`)
	b.WriteString(` xmlns:soap-env="` + NSSoapEnv + `"`)
	b.WriteString(` xmlns:soap-enc="` + NSSoapEnc + `"`)
	b.WriteString(` xmlns:xsd="` + NSXSD + `"`)
	b.WriteString(` xmlns:xsi="` + NSXSI + `"`)
	b.WriteString(` xmlns:cwmp="` + esc(cwmpNS) + `"`)
	b.WriteString(` soap-env:encodingStyle="` + NSSoapEnc + `">`)
	b.WriteString(`<soap-env:Header>`)
	b.WriteString(`<cwmp:ID soap-env:mustUnderstand="1">` + esc(id) + `</cwmp:ID>`)
	b.WriteString(`</soap-env:Header>`)
	b.WriteString(`<soap-env:Body>`)
	if bodyXML != "" {
		b.WriteString(bodyXML)
	}
	b.WriteString(`</soap-env:Body>`)
	b.WriteString(`</soap-env:Envelope>`)
	return b.String()
}

// ACS 侧的 Fault 码（8000 段）。
//
// 注意：9000 段是 CPE 发给 ACS 的错误码（例如 CPE 回 "9000 Method not supported"），
// ACS 只会「记录」不会「发送」；ACS 自己回给 CPE 的错用 8000 段。
const (
	FaultMethodNotSupported = 8000
	FaultRequestDenied      = 8001
	FaultInternalError      = 8002
	FaultInvalidArguments   = 8003
	FaultResourcesExceeded  = 8004
	FaultRetryRequest       = 8005
)

// faultText 给出错误码的默认描述（用于 FaultString）。
func faultText(code int) string {
	switch code {
	case FaultMethodNotSupported:
		return "Method not supported"
	case FaultRequestDenied:
		return "Request denied"
	case FaultInternalError:
		return "Internal error"
	case FaultInvalidArguments:
		return "Invalid arguments"
	case FaultResourcesExceeded:
		return "Resources exceeded"
	case FaultRetryRequest:
		return "Retry request"
	default:
		return "Internal error"
	}
}

// FaultBody 拼一个 CWMP Fault 的 Body 内容（不含 Envelope）。
func FaultBody(code int, msg string) string {
	if msg == "" {
		msg = faultText(code)
	}
	return `<soap-env:Fault>` +
		`<faultcode>Server</faultcode>` +
		`<faultstring>CWMP fault</faultstring>` +
		`<detail><cwmp:Fault>` +
		`<FaultCode>` + fmt.Sprint(code) + `</FaultCode>` +
		`<FaultString>` + esc(msg) + `</FaultString>` +
		`</cwmp:Fault></detail>` +
		`</soap-env:Fault>`
}
