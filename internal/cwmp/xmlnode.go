package cwmp

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// Node 是一棵「只按本地名匹配」的 XML 元素树。
//
// 为什么不直接用 encoding/xml 的结构体映射：
//   - CPE 的命名空间前缀五花八门（soap: / soap-env: / SOAP-ENV:），前缀不可靠；
//   - 厂商会塞私有扩展节点，结构体映射遇到未知字段容易报错。
//
// 所以先解析成通用树，再按 Local（本地名）去取值 —— 见需求文档 R-FR3-1「容忍未知元素」。
type Node struct {
	Local string // 本地名，如 "Envelope"、"GetParameterValues"
	NS    string // 命名空间 URI（Go 已解析前缀；未声明前缀时可能是前缀字面量）
	Attrs []xml.Attr
	Kids  []*Node
	Text  string
}

// ParseXML 把 XML 流解析成 Node 树。只需要一个根节点。
func ParseXML(r io.Reader) (*Node, error) {
	dec := xml.NewDecoder(r)
	dec.CharsetReader = charsetReader
	var root *Node
	var stack []*Node

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &Node{Local: t.Name.Local, NS: t.Name.Space, Attrs: t.Attr}
			if len(stack) > 0 {
				p := stack[len(stack)-1]
				p.Kids = append(p.Kids, n)
			} else if root == nil {
				root = n
			} else {
				// 多个根节点：只取第一个，其余忽略（脏报文容错）
				continue
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].Text += string(t)
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("XML 文档里没有根元素")
	}
	return root, nil
}

// charsetReader 处理 CPE 在 XML 声明里写非 UTF-8 编码的情况。
// 绝大多数设备是 UTF-8；少数老设备写 iso-8859-1，这里按 latin1 转一下。
// 不认识的编码直接报错，而不是猜（猜错会静默产生乱码）。
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return input, nil
	case "iso-8859-1", "latin1", "windows-1252":
		b, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		var sb strings.Builder
		sb.Grow(len(b))
		for _, c := range b {
			sb.WriteRune(rune(c))
		}
		return strings.NewReader(sb.String()), nil
	default:
		return nil, fmt.Errorf("不支持的字符集 %q", charset)
	}
}

// Child 返回第一个本地名匹配的直接子元素。
func (n *Node) Child(local string) *Node {
	if n == nil {
		return nil
	}
	for _, k := range n.Kids {
		if k.Local == local {
			return k
		}
	}
	return nil
}

// Children 返回所有本地名匹配的直接子元素。
func (n *Node) Children(local string) []*Node {
	if n == nil {
		return nil
	}
	var out []*Node
	for _, k := range n.Kids {
		if k.Local == local {
			out = append(out, k)
		}
	}
	return out
}

// FirstChild 返回第一个子元素（Body 里取方法名用）。
func (n *Node) FirstChild() *Node {
	if n == nil || len(n.Kids) == 0 {
		return nil
	}
	return n.Kids[0]
}

// Trimmed 返回去掉了首尾空白的文本。
func (n *Node) Trimmed() string {
	if n == nil {
		return ""
	}
	return strings.TrimSpace(n.Text)
}

// ChildText 返回子元素的文本。
func (n *Node) ChildText(local string) string {
	return n.Child(local).Trimmed()
}

// Attr 按本地名取属性值（例如 xsi:type 取 type）。
func (n *Node) Attr(local string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}
