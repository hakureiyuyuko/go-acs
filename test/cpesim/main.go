// Command cpesim 是一个最小可用的 TR-069 CPE 模拟器，用来在没有真机的情况下
// 端到端验收 ACS。
//
// 它做的事：按 CWMP 的时序向 ACS 上报 Inform，然后不断用空 POST 领取任务、
// 执行、把结果回给 ACS，直到 ACS 回 204 表示会话结束。
//
// 用法示例：
//
//	go run ./test/cpesim -acs http://127.0.0.1:7547/acs -once
//	go run ./test/cpesim -dm 181 -serial XXX -interval 30
package main

import (
	"bytes"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"acs/internal/cwmp"
)

type simulator struct {
	acsURL      string
	user        string
	pass        string
	cwmpVersion string
	event       string
	once        bool
	interval    time.Duration

	// 设备身份
	manufacturer string
	oui          string
	productClass string
	serial       string
	model        string

	// 参数表（TR-069 的数据模型）
	params map[string]string
	types  map[string]string

	crCh chan struct{}
}

func main() {
	s := &simulator{params: map[string]string{}, types: map[string]string{}, crCh: make(chan struct{}, 1)}

	var root string
	var crPort int
	flag.StringVar(&s.acsURL, "acs", "http://127.0.0.1:7547/acs", "ACS 的 CWMP 地址")
	flag.StringVar(&s.user, "user", "", "CPE→ACS 认证账号")
	flag.StringVar(&s.pass, "pass", "", "CPE→ACS 认证密码")
	flag.StringVar(&s.cwmpVersion, "cwmp", "1.0", "CWMP 版本 1.0/1.1/1.2/1.3")
	flag.StringVar(&s.event, "event", "0 BOOTSTRAP", "首次上报的事件码")
	flag.BoolVar(&s.once, "once", false, "跑完一次会话就退出（适合脚本验收）")
	flag.DurationVar(&s.interval, "interval", 30*time.Second, "周期上报间隔（0 表示会话结束就退出）")
	flag.StringVar(&s.manufacturer, "manufacturer", "SimVendor", "厂商")
	flag.StringVar(&s.oui, "oui", "001122", "OUI（6 位十六进制）")
	flag.StringVar(&s.productClass, "product-class", "SimRouter", "产品类")
	flag.StringVar(&s.serial, "serial", "ACSIM0000001", "序列号")
	flag.StringVar(&s.model, "model", "SimModel-X1", "型号名")
	flag.StringVar(&root, "dm", "098", "数据模型：098(TR-098) 或 181(TR-181)")
	flag.IntVar(&crPort, "cr-port", 0, "ConnectionRequest 监听端口（0 = 随机）")
	flag.Parse()

	rootPrefix := "InternetGatewayDevice."
	specVersion := "1.0"
	if strings.Contains(root, "181") {
		rootPrefix = "Device."
		specVersion = "2.0"
	}

	s.buildParams(rootPrefix, specVersion)

	// 起一个本地 HTTP 服务当 ConnectionRequestURL，并把它写进参数表
	crURL, err := s.startConnectionRequestServer(crPort)
	if err != nil {
		log.Printf("注意：无法启动 ConnectionRequest 监听：%v", err)
	}
	if crURL != "" {
		s.params[rootPrefix+"ManagementServer.ConnectionRequestURL"] = crURL
		s.types[rootPrefix+"ManagementServer.ConnectionRequestURL"] = "string"
	}

	log.Printf("CPE 模拟器启动 serial=%s dm=%s acs=%s 参数=%d 条",
		s.serial, rootPrefix, s.acsURL, len(s.params))

	event := s.event
	for round := 0; ; round++ {
		if err := s.runSession(event); err != nil {
			log.Printf("会话出错：%v", err)
		}
		if s.once || s.interval <= 0 {
			log.Printf("（-once/interval=0）结束")
			return
		}
		// 等下一个周期，或者被 Connection Request 唤醒
		select {
		case <-s.crCh:
			event = "6 CONNECTION REQUEST"
			log.Printf("收到连接请求，立刻回连（event=%s）", event)
			time.Sleep(200 * time.Millisecond)
		case <-time.After(s.interval):
			s.tick(rootPrefix)
			event = "2 PERIODIC"
		}
	}
}

// tick 模拟运行时长在增长。
func (s *simulator) tick(root string) {
	up := root + "DeviceInfo.UpTime"
	if v, ok := s.params[up]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			s.params[up] = strconv.Itoa(n + int(s.interval.Seconds()))
		}
	}
}

// buildParams 造一份「最基本的设备信息」。
func (s *simulator) buildParams(root, specVersion string) {
	di := root + "DeviceInfo."
	ms := root + "ManagementServer."
	wan := root + "WANDevice.1.WANConnectionDevice.1.WANIPConnection.1."

	set := func(name, val, typ string) {
		s.params[name] = val
		s.types[name] = typ
	}

	set(di+"Manufacturer", s.manufacturer, "string")
	set(di+"ManufacturerOUI", s.oui, "string")
	set(di+"ModelName", s.model, "string")
	set(di+"Description", "TR-069 CPE Simulator", "string")
	set(di+"ProductClass", s.productClass, "string")
	set(di+"SerialNumber", s.serial, "string")
	set(di+"HardwareVersion", "V1.0", "string")
	set(di+"SoftwareVersion", "1.0.0-sim", "string")
	set(di+"SpecVersion", specVersion, "string")
	set(di+"ProvisioningCode", "0000", "string")
	set(di+"UpTime", "3600", "unsignedInt")
	set(di+"FirstUseDate", "2026-01-01T00:00:00Z", "dateTime")

	set(ms+"URL", s.acsURL, "string")
	set(ms+"Username", s.user, "string")
	set(ms+"PeriodicInformEnable", "1", "boolean")
	set(ms+"PeriodicInformInterval", strconv.Itoa(int(s.interval.Seconds())), "unsignedInt")
	set(ms+"ConnectionRequestUsername", "cpe-cr", "string")
	set(ms+"ParameterKey", "", "string")

	set(wan+"ExternalIPAddress", "203.0.113.7", "string")
	set(wan+"ConnectionStatus", "Connected", "string")

	// 无线参数。
	// 特意把实例号做成 **1 和 5**（不是 1 和 2）—— 真机（华为 HN8145X6N）就是这么编号的，
	// 写死 1/2 会读空。
	if root == "Device." {
		// TR-181：Radio / SSID / AccessPoint 分开在不同对象下
		set("Device.WiFi.Radio.1.Enable", "1", "boolean")
		set("Device.WiFi.Radio.1.Status", "Up", "string")
		set("Device.WiFi.Radio.1.Channel", "36", "unsignedInt")
		set("Device.WiFi.Radio.1.OperatingFrequencyBand", "5GHz", "string")
		set("Device.WiFi.SSID.1.SSID", "SimWiFi", "string")
		set("Device.WiFi.AccessPoint.1.AssociatedDeviceNumberOfEntries", "2", "unsignedInt")
		set("Device.WiFi.AccessPoint.1.SSIDAdvertisementEnabled", "1", "boolean")
	} else {
		wlan := root + "LANDevice.1.WLANConfiguration."
		set(wlan+"1.SSID", "SimWiFi", "string")
		set(wlan+"1.Enable", "1", "boolean")
		set(wlan+"1.RadioEnabled", "1", "boolean")
		set(wlan+"1.Status", "Up", "string")
		set(wlan+"1.Channel", "6", "unsignedInt")
		set(wlan+"1.Standard", "11ax", "string")
		set(wlan+"1.BSSID", "00:11:22:33:44:55", "string")
		set(wlan+"1.BeaconType", "11i", "string")
		set(wlan+"1.WPAEncryptionModes", "AESEncryption", "string")
		set(wlan+"1.TotalAssociations", "2", "unsignedInt")
		set(wlan+"1.X_HW_RFBand", "2.4GHz", "string")
		// 下面这几个是「可编辑 / 给下拉框提供候选值」用的
		set(wlan+"1.KeyPassphrase", "", "string")
		set(wlan+"1.IEEE11iEncryptionModes", "AESEncryption", "string")
		set(wlan+"1.IEEE11iAuthenticationMode", "PSKAuthentication", "string")
		set(wlan+"1.TransmitPower", "100", "unsignedInt")
		set(wlan+"1.TransmitPowerSupported", "20,40,60,80,100", "string")
		set(wlan+"1.PossibleChannels", "1,2,3,4,5,6,7,8,9,10,11,12,13", "string")

		set(wlan+"5.SSID", "SimWiFi-5G", "string")
		set(wlan+"5.Enable", "1", "boolean")
		set(wlan+"5.RadioEnabled", "0", "boolean")
		set(wlan+"5.Status", "Disabled", "string")
		set(wlan+"5.Channel", "0", "unsignedInt")
		set(wlan+"5.Standard", "11ax", "string")
		set(wlan+"5.BSSID", "00:11:22:33:44:56", "string")
		set(wlan+"5.BeaconType", "11i", "string")
		set(wlan+"5.WPAEncryptionModes", "AESEncryption", "string")
		set(wlan+"5.TotalAssociations", "0", "unsignedInt")
		set(wlan+"5.X_HW_RFBand", "5GHz", "string")
	}
}

// startConnectionRequestServer 起一个假的 CPE 侧 HTTP 服务。
// 返回可用的 URL（用与 ACS 通信时实际使用的本机 IP 拼出来，避免写死 127.0.0.1）。
func (s *simulator) startConnectionRequestServer(port int) (string, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return "", err
	}
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("收到 Connection Request：%s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusOK)
		select {
		case s.crCh <- struct{}{}:
		default:
		}
	})
	go func() { _ = http.Serve(ln, httpMux) }()

	ip := localIPFor(s.acsURL)
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	return fmt.Sprintf("http://%s:%s/", ip, p), nil
}

// localIPFor 通过一次 UDP「连接」探测出到达 ACS 时用的本机 IP。
func localIPFor(acsURL string) string {
	u := strings.TrimPrefix(strings.TrimPrefix(acsURL, "http://"), "https://")
	if i := strings.IndexAny(u, "/"); i >= 0 {
		u = u[:i]
	}
	host, port, err := net.SplitHostPort(u)
	if err != nil {
		host, port = u, "80"
	}
	conn, err := net.Dial("udp", net.JoinHostPort(host, port))
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return "127.0.0.1"
}

// ---------- 会话流程 ----------

func (s *simulator) runSession(event string) error {
	// 1) Inform
	resp, status, err := s.post(s.envelope(randID(), s.informBody(event)))
	if err != nil {
		return fmt.Errorf("发送 Inform 失败: %w", err)
	}
	if status/100 != 2 {
		return fmt.Errorf("Inform 返回了 %d: %s", status, truncate(resp))
	}
	log.Printf("已上报 Inform（event=%s），收到 %d 字节响应", event, len(resp))

	// 2) 空 POST 领任务 → 执行 → 回结果 → 直到 204
	body := ""
	for round := 0; round < 200; round++ {
		resp, status, err = s.post(body)
		if err != nil {
			return fmt.Errorf("会话中断: %w", err)
		}
		if status == http.StatusNoContent || len(bytes.TrimSpace(resp)) == 0 {
			log.Printf("会话结束（HTTP %d）", status)
			return nil
		}

		env, err := parseEnvelope(resp)
		if err != nil {
			return fmt.Errorf("解析 ACS 报文失败: %w（原文: %s）", err, truncate(resp))
		}
		if env.Method == nil {
			return fmt.Errorf("ACS 报文里没有 RPC")
		}
		log.Printf("ACS 下发：%s", env.Method.Local)

		reply, err := s.handle(env.Method)
		if err != nil {
			return err
		}
		if reply == "" {
			log.Printf("会话由 CPE 侧结束")
			return nil
		}
		body = s.envelope(env.ID, reply)
	}
	return fmt.Errorf("会话轮次过多，疑似死循环")
}

type envelopeInfo struct {
	ID     string
	Method *cwmp.Node
}

func parseEnvelope(b []byte) (*envelopeInfo, error) {
	root, err := cwmp.ParseXML(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	env, err := cwmp.ParseEnvelope(root)
	if err != nil {
		return nil, err
	}
	return &envelopeInfo{ID: env.ID, Method: env.Method}, nil
}

func (s *simulator) post(body string) ([]byte, int, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, s.acsURL, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("User-Agent", "cpesim/1.0 UPnP/1.0")
	if s.user != "" {
		req.SetBasicAuth(s.user, s.pass)
	}
	if body != "" {
		req.ContentLength = int64(len(body))
	} else {
		req.ContentLength = 0
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// ---------- 处理 ACS 下发的 RPC ----------

func (s *simulator) handle(m *cwmp.Node) (string, error) {
	switch m.Local {
	case "Fault":
		f := cwmp.ParseFault(m)
		return "", fmt.Errorf("ACS 返回 Fault: code=%d msg=%s", f.Code, f.String)
	case "GetParameterValues":
		return s.getParameterValues(m), nil
	case "GetParameterNames":
		return s.getParameterNames(m), nil
	case "SetParameterValues":
		return s.setParameterValues(m), nil
	case "GetRPCMethods":
		return s.getRPCMethods(), nil
	case "Reboot":
		log.Printf("收到 Reboot（CommandKey=%s），模拟重启", m.ChildText("CommandKey"))
		return `<cwmp:RebootResponse/>`, nil
	case "Download":
		log.Printf("收到 Download（URL=%s），本模拟器不支持", m.ChildText("URL"))
		return faultBody(9001, "Download not supported by simulator"), nil
	default:
		log.Printf("不支持的 RPC：%s，回 9000", m.Local)
		return faultBody(9000, "Method not supported"), nil
	}
}

// getParameterValues 支持两种情况：完整参数名，以及以 "." 结尾的部分路径（返回整棵子树）。
func (s *simulator) getParameterValues(m *cwmp.Node) string {
	reqs := m.Child("ParameterNames")
	var out []cwmp.ParamValue
	seen := map[string]bool{}

	if reqs != nil {
		for _, child := range reqs.Kids {
			name := child.Trimmed()
			if name == "" {
				continue
			}
			if strings.HasSuffix(name, ".") {
				for k := range s.params {
					if strings.HasPrefix(k, name) && !seen[k] {
						seen[k] = true
						out = append(out, s.pv(k))
					}
				}
				continue
			}
			if _, ok := s.params[name]; ok && !seen[name] {
				seen[name] = true
				out = append(out, s.pv(name))
			}
		}
	}

	var b strings.Builder
	b.WriteString(`<cwmp:GetParameterValuesResponse><ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[`)
	b.WriteString(strconv.Itoa(len(out)))
	b.WriteString(`]">`)
	for _, p := range out {
		t := p.Type
		if t == "" {
			t = "string"
		}
		b.WriteString(`<ParameterValueStruct><Name>` + esc(p.Name) + `</Name>`)
		b.WriteString(`<Value xsi:type="xsd:` + esc(t) + `">` + esc(p.Value) + `</Value>`)
		b.WriteString(`</ParameterValueStruct>`)
	}
	b.WriteString(`</ParameterList></cwmp:GetParameterValuesResponse>`)
	return b.String()
}

func (s *simulator) pv(name string) cwmp.ParamValue {
	t := s.types[name]
	if t == "" {
		t = "string"
	}
	return cwmp.ParamValue{Name: name, Value: s.params[name], Type: t}
}

func (s *simulator) getParameterNames(m *cwmp.Node) string {
	path := m.ChildText("ParameterPath")
	nextLevel := m.ChildText("NextLevel") == "1"

	var names []string
	seen := map[string]bool{}
	for k := range s.params {
		if path != "" && !strings.HasPrefix(k, path) {
			continue
		}
		var n string
		if nextLevel {
			// 只返回下一层：把剩下的部分截到第一个 "."
			rest := strings.TrimPrefix(k, path)
			if i := strings.Index(rest, "."); i >= 0 {
				n = path + rest[:i+1]
			} else {
				n = k
			}
		} else {
			n = k
		}
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}

	var b strings.Builder
	b.WriteString(`<cwmp:GetParameterNamesResponse><ParameterList soap-enc:arrayType="cwmp:ParameterInfoStruct[`)
	b.WriteString(strconv.Itoa(len(names)))
	b.WriteString(`]">`)
	for _, n := range names {
		writable := "0"
		if !strings.HasSuffix(n, ".") {
			writable = "1"
		}
		b.WriteString(`<ParameterInfoStruct><Name>` + esc(n) + `</Name><Writable>` + writable + `</Writable></ParameterInfoStruct>`)
	}
	b.WriteString(`</ParameterList></cwmp:GetParameterNamesResponse>`)
	return b.String()
}

func (s *simulator) setParameterValues(m *cwmp.Node) string {
	key := m.ChildText("ParameterKey")
	vals := cwmp.ParseParamValues(m.Child("ParameterList"))
	for _, v := range vals {
		log.Printf("  设置 %s = %s", v.Name, v.Value)
		s.params[v.Name] = v.Value
		s.types[v.Name] = v.Type
	}
	_ = key
	return `<cwmp:SetParameterValuesResponse><Status>0</Status></cwmp:SetParameterValuesResponse>`
}

func (s *simulator) getRPCMethods() string {
	methods := []string{
		"Inform", "GetRPCMethods", "TransferComplete", "GetParameterValues",
		"SetParameterValues", "GetParameterNames", "SetParameterAttributes",
		"GetParameterAttributes", "AddObject", "DeleteObject", "Reboot",
	}
	var b strings.Builder
	b.WriteString(`<cwmp:GetRPCMethodsResponse><MethodList soap-enc:arrayType="xsd:string[`)
	b.WriteString(strconv.Itoa(len(methods)))
	b.WriteString(`]">`)
	for _, m := range methods {
		b.WriteString(`<string>` + m + `</string>`)
	}
	b.WriteString(`</MethodList></cwmp:GetRPCMethodsResponse>`)
	return b.String()
}

// ---------- 报文拼装 ----------

func (s *simulator) informBody(event string) string {
	var b strings.Builder
	b.WriteString(`<cwmp:Inform>`)
	b.WriteString(`<DeviceId>`)
	b.WriteString(`<Manufacturer>` + esc(s.manufacturer) + `</Manufacturer>`)
	b.WriteString(`<OUI>` + esc(s.oui) + `</OUI>`)
	b.WriteString(`<ProductClass>` + esc(s.productClass) + `</ProductClass>`)
	b.WriteString(`<SerialNumber>` + esc(s.serial) + `</SerialNumber>`)
	b.WriteString(`</DeviceId>`)

	b.WriteString(`<Event soap-enc:arrayType="cwmp:EventStruct[1]">`)
	b.WriteString(`<EventStruct><EventCode>` + esc(event) + `</EventCode><CommandKey></CommandKey></EventStruct>`)
	b.WriteString(`</Event>`)

	b.WriteString(`<MaxEnvelopes>1</MaxEnvelopes>`)
	b.WriteString(`<CurrentTime>` + time.Now().UTC().Format("2006-01-02T15:04:05.000Z") + `</CurrentTime>`)
	b.WriteString(`<RetryCount>0</RetryCount>`)

	// 真实 CPE 的 Inform 只带一小部分参数，这里也一样
	names := s.informParamNames()
	b.WriteString(`<ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[` + strconv.Itoa(len(names)) + `]">`)
	for _, n := range names {
		v, ok := s.params[n]
		if !ok {
			continue
		}
		t := s.types[n]
		if t == "" {
			t = "string"
		}
		b.WriteString(`<ParameterValueStruct><Name>` + esc(n) + `</Name>`)
		b.WriteString(`<Value xsi:type="xsd:` + esc(t) + `">` + esc(v) + `</Value>`)
		b.WriteString(`</ParameterValueStruct>`)
	}
	b.WriteString(`</ParameterList>`)
	b.WriteString(`</cwmp:Inform>`)
	return b.String()
}

func (s *simulator) informParamNames() []string {
	var out []string
	for _, suffix := range []string{
		"DeviceInfo.SpecVersion", "DeviceInfo.HardwareVersion", "DeviceInfo.SoftwareVersion",
		"DeviceInfo.ProvisioningCode", "ManagementServer.ParameterKey",
		"ManagementServer.ConnectionRequestURL",
	} {
		for k := range s.params {
			if strings.HasSuffix(k, suffix) {
				out = append(out, k)
			}
		}
	}
	return out
}

func (s *simulator) envelope(id, body string) string {
	ns := "urn:dslforum-org:cwmp-" + s.cwmpVersion
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/"` +
		` xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/"` +
		` xmlns:xsd="http://www.w3.org/2001/XMLSchema"` +
		` xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"` +
		` xmlns:cwmp="` + ns + `">` +
		`<soap-env:Header><cwmp:ID soap-env:mustUnderstand="1">` + esc(id) + `</cwmp:ID></soap-env:Header>` +
		`<soap-env:Body>` + body + `</soap-env:Body>` +
		`</soap-env:Envelope>`
}

func faultBody(code int, msg string) string {
	return `<soap-env:Fault><faultcode>Client</faultcode><faultstring>CWMP fault</faultstring>` +
		`<detail><cwmp:Fault><FaultCode>` + strconv.Itoa(code) + `</FaultCode>` +
		`<FaultString>` + esc(msg) + `</FaultString></cwmp:Fault></detail></soap-env:Fault>`
}

// ---------- 小工具 ----------

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func randID() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
