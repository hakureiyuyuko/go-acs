package cwmp

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------- 通用数据结构 ----------

// ParamValue 是 ParameterValueStruct 的 Go 表示。
type ParamValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Type  string `json:"type,omitempty"`
}

// DeviceID 是 Inform 里的 DeviceId。
type DeviceID struct {
	Manufacturer string `json:"manufacturer"`
	OUI          string `json:"oui"`
	ProductClass string `json:"product_class"`
	SerialNumber string `json:"serial_number"`
}

// EventStruct 是 Inform 里的一个事件。
type EventStruct struct {
	Code       string `json:"code"`
	CommandKey string `json:"command_key,omitempty"`
}

// Inform 是 CPE 的一次上报。
type Inform struct {
	DeviceID     DeviceID
	Events       []EventStruct
	MaxEnvelopes int
	CurrentTime  string
	RetryCount   int
	Params       []ParamValue
}

// EventCodes 返回纯事件码列表，如 ["0 BOOTSTRAP"]。
func (i *Inform) EventCodes() []string {
	out := make([]string, 0, len(i.Events))
	for _, e := range i.Events {
		out = append(out, e.Code)
	}
	return out
}

// HasEvent 判断事件列表里是否包含指定事件码（前缀匹配，兼容 "4 VALUE CHANGE"）。
func (i *Inform) HasEvent(code string) bool {
	for _, e := range i.Events {
		if strings.EqualFold(strings.TrimSpace(e.Code), code) {
			return true
		}
	}
	return false
}

// CommandKeyOfEvent 取某个事件带的 CommandKey。
func (i *Inform) CommandKeyOfEvent(code string) string {
	for _, e := range i.Events {
		if strings.EqualFold(strings.TrimSpace(e.Code), code) {
			return e.CommandKey
		}
	}
	return ""
}

// ---------- 解析 ----------

// normType 把 "xsd:string" / "xsi:unsignedInt" 归一化成 "string" / "unsignedint"。
func normType(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(s)
}

// ParseParamValues 解析一个 ParameterValueStruct 列表（ParameterList 的内容）。
func ParseParamValues(list *Node) []ParamValue {
	if list == nil {
		return nil
	}
	kids := list.Children("ParameterValueStruct")
	out := make([]ParamValue, 0, len(kids))
	for _, s := range kids {
		v := s.Child("Value")
		t := normType(v.Attr("type"))
		if t == "" {
			t = "string"
		}
		out = append(out, ParamValue{
			Name:  s.ChildText("Name"),
			Value: v.Trimmed(),
			Type:  t,
		})
	}
	return out
}

// ParseParameterInfoStructs 解析 GetParameterNamesResponse 里的 ParameterInfoStruct 列表。
type ParamInfo struct {
	Name     string
	Writable bool
}

func ParseParameterInfoStructs(list *Node) []ParamInfo {
	if list == nil {
		return nil
	}
	var out []ParamInfo
	for _, s := range list.Children("ParameterInfoStruct") {
		wr := strings.TrimSpace(s.ChildText("Writable"))
		out = append(out, ParamInfo{
			Name:     s.ChildText("Name"),
			Writable: wr == "1" || strings.EqualFold(wr, "true"),
		})
	}
	return out
}

// ParseInform 解析 Inform 请求。
func ParseInform(m *Node) (*Inform, error) {
	if m == nil {
		return nil, fmt.Errorf("空的 Inform")
	}
	inf := &Inform{}
	if did := m.Child("DeviceId"); did != nil {
		inf.DeviceID = DeviceID{
			Manufacturer: did.ChildText("Manufacturer"),
			OUI:          strings.ToUpper(did.ChildText("OUI")),
			ProductClass: did.ChildText("ProductClass"),
			SerialNumber: did.ChildText("SerialNumber"),
		}
	}
	if inf.DeviceID.SerialNumber == "" {
		return nil, fmt.Errorf("Inform 缺少 DeviceId/SerialNumber")
	}
	for _, es := range m.Child("Event").Children("EventStruct") {
		inf.Events = append(inf.Events, EventStruct{
			Code:       es.ChildText("EventCode"),
			CommandKey: es.ChildText("CommandKey"),
		})
	}
	if me := m.Child("MaxEnvelopes"); me != nil {
		inf.MaxEnvelopes, _ = strconv.Atoi(me.Trimmed())
	}
	inf.CurrentTime = m.ChildText("CurrentTime")
	if rc := m.Child("RetryCount"); rc != nil {
		inf.RetryCount, _ = strconv.Atoi(rc.Trimmed())
	}
	inf.Params = ParseParamValues(m.Child("ParameterList"))
	return inf, nil
}

// ---------- CPE 侧错误（Fault）----------

// SetParamFault 是 SetParameterValues 失败时逐条的说明。
type SetParamFault struct {
	Name   string
	Code   int
	String string
}

// Fault 是 CPE 回的错误。
type Fault struct {
	Code           int
	String         string
	SetParamFaults []SetParamFault
}

// ParseFault 解析 <soap-env:Fault><detail><cwmp:Fault>... 结构。
func ParseFault(m *Node) *Fault {
	if m == nil {
		return nil
	}
	n := m
	if d := m.Child("detail"); d != nil {
		if cf := d.Child("Fault"); cf != nil {
			n = cf
		}
	}
	f := &Fault{String: n.ChildText("FaultString")}
	f.Code, _ = strconv.Atoi(n.ChildText("FaultCode"))
	for _, sf := range n.Children("SetParameterValuesFault") {
		sp := SetParamFault{
			Name:   sf.ChildText("ParameterName"),
			String: sf.ChildText("FaultString"),
		}
		sp.Code, _ = strconv.Atoi(sf.ChildText("FaultCode"))
		f.SetParamFaults = append(f.SetParamFaults, sp)
	}
	return f
}

// ---------- 响应构造 ----------

// InformResponseBody 构造 InformResponse，MaxEnvelopes 固定回 1（一次一个请求）。
func InformResponseBody() string {
	return `<cwmp:InformResponse><MaxEnvelopes>1</MaxEnvelopes></cwmp:InformResponse>`
}

// GenericResponseBody 构造 xxxResponse 的空响应（如 TransferCompleteResponse）。
func GenericResponseBody(method string) string {
	return `<cwmp:` + esc(method) + `/>`
}

// GetRPCMethodsResponseBody 构造 ACS 支持的 RPC 列表。
func GetRPCMethodsResponseBody(methods []string) string {
	var b strings.Builder
	b.WriteString(`<cwmp:GetRPCMethodsResponse><MethodList soap-enc:arrayType="xsd:string[`)
	b.WriteString(strconv.Itoa(len(methods)))
	b.WriteString(`]">`)
	for _, m := range methods {
		b.WriteString(`<string>` + esc(m) + `</string>`)
	}
	b.WriteString(`</MethodList></cwmp:GetRPCMethodsResponse>`)
	return b.String()
}

// ---------- 请求构造（ACS -> CPE）----------

// GetParameterValuesBody 构造 GetParameterValues 请求。
func GetParameterValuesBody(names []string) string {
	var b strings.Builder
	b.WriteString(`<cwmp:GetParameterValues><ParameterNames soap-enc:arrayType="xsd:string[`)
	b.WriteString(strconv.Itoa(len(names)))
	b.WriteString(`]">`)
	for _, n := range names {
		b.WriteString(`<string>` + esc(n) + `</string>`)
	}
	b.WriteString(`</ParameterNames></cwmp:GetParameterValues>`)
	return b.String()
}

// GetParameterNamesBody 构造 GetParameterNames 请求。
func GetParameterNamesBody(path string, nextLevel bool) string {
	lvl := "0"
	if nextLevel {
		lvl = "1"
	}
	return `<cwmp:GetParameterNames><ParameterPath>` + esc(path) +
		`</ParameterPath><NextLevel>` + lvl + `</NextLevel></cwmp:GetParameterNames>`
}

// SetParameterValuesBody 构造 SetParameterValues 请求（ParameterKey 必须回传）。
func SetParameterValuesBody(vals []ParamValue, parameterKey string) string {
	var b strings.Builder
	b.WriteString(`<cwmp:SetParameterValues><ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[`)
	b.WriteString(strconv.Itoa(len(vals)))
	b.WriteString(`]">`)
	for _, v := range vals {
		t := v.Type
		if t == "" {
			t = "string"
		}
		b.WriteString(`<ParameterValueStruct><Name>` + esc(v.Name) + `</Name>`)
		b.WriteString(`<Value xsi:type="xsd:` + esc(t) + `">` + esc(v.Value) + `</Value>`)
		b.WriteString(`</ParameterValueStruct>`)
	}
	b.WriteString(`</ParameterList><ParameterKey>` + esc(parameterKey) + `</ParameterKey>`)
	b.WriteString(`</cwmp:SetParameterValues>`)
	return b.String()
}

// GetRPCMethodsBody 构造 GetRPCMethods 请求（问 CPE 支持哪些方法）。
func GetRPCMethodsBody() string { return `<cwmp:GetRPCMethods/>` }

// RebootBody 构造 Reboot 请求。
func RebootBody(commandKey string) string {
	return `<cwmp:Reboot><CommandKey>` + esc(commandKey) + `</CommandKey></cwmp:Reboot>`
}
