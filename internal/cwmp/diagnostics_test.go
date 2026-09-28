package cwmp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// diagTask 造一条诊断任务（载荷跟真实入队时一样）。
func diagTask(t *testing.T, host string, count int, iface string) *store.Task {
	t.Helper()
	p, _ := json.Marshal(diagPayload{
		Host:      host,
		Count:     count,
		Prefix:    "InternetGatewayDevice.IPPingDiagnostics.",
		Interface: iface,
	})
	return &store.Task{Kind: TaskDiagnostics, Payload: string(p), CommandKey: "k1"}
}

// 指定承载接口时：Interface 要写进报文，而且必须排在 DiagnosticsState 之前
// —— 设备看到 Requested 就开始跑 ping，那时候出口得已经就位。
func TestBuildTaskBodyDiagnosticsInterface(t *testing.T) {
	const iface = "InternetGatewayDevice.WANDevice.1.WANConnectionDevice.2.WANIPConnection.1"
	body, err := buildTaskBody(diagTask(t, "www.baidu.com", 3, iface))
	if err != nil {
		t.Fatalf("构造报文失败: %v", err)
	}
	for _, want := range []string{
		"<Name>InternetGatewayDevice.IPPingDiagnostics.Interface</Name>",
		"<Value xsi:type=\"xsd:string\">" + iface + "</Value>",
		"<Name>InternetGatewayDevice.IPPingDiagnostics.Host</Name>",
		"<Name>InternetGatewayDevice.IPPingDiagnostics.NumberOfRepetitions</Name>",
		"<Name>InternetGatewayDevice.IPPingDiagnostics.DiagnosticsState</Name>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("报文里缺少 %s\n%s", want, body)
		}
	}
	iIface := strings.Index(body, "IPPingDiagnostics.Interface")
	iHost := strings.Index(body, "IPPingDiagnostics.Host")
	iState := strings.Index(body, "IPPingDiagnostics.DiagnosticsState")
	if !(iIface < iHost && iHost < iState) {
		t.Fatalf("顺序不对（应该 Interface → Host → DiagnosticsState）：%d/%d/%d", iIface, iHost, iState)
	}
}

// 不指定承载接口时：报文里不能有 Interface 这一项（留空 = 设备自己选）。
func TestBuildTaskBodyDiagnosticsWithoutInterface(t *testing.T) {
	body, err := buildTaskBody(diagTask(t, "119.29.29.29", 4, ""))
	if err != nil {
		t.Fatalf("构造报文失败: %v", err)
	}
	if strings.Contains(body, "IPPingDiagnostics.Interface") {
		t.Fatalf("留空时不该下发 Interface：\n%s", body)
	}
	if !strings.Contains(body, "<Value xsi:type=\"xsd:string\">119.29.29.29</Value>") {
		t.Fatalf("目标没写进去：\n%s", body)
	}
}

func TestNormalizeDiagInterface(t *testing.T) {
	ok := []string{
		"", // 留空合法
		"InternetGatewayDevice.WANDevice.1.WANConnectionDevice.2.WANIPConnection.1",
		"Device.IP.Interface.1",
		"br0", // 不限定写法
	}
	for _, v := range ok {
		if got, err := normalizeDiagInterface(v); err != nil || got != v {
			t.Fatalf("应通过：%q → %q, %v", v, got, err)
		}
	}
	bad := []string{
		"a b", "a<b>", "a&b", "1.1.1.1; rm -rf /", "..",
		"InternetGatewayDevice.X.", ".InternetGatewayDevice", "机接口",
	}
	for _, v := range bad {
		if _, err := normalizeDiagInterface(v); err == nil {
			t.Fatalf("应被拒绝：%q", v)
		}
	}
	// 前后空白要裁掉（界面上粘贴容易带）
	if got, err := normalizeDiagInterface("  Device.IP.Interface.1 \t"); err != nil || got != "Device.IP.Interface.1" {
		t.Fatalf("应裁掉空白：%q, %v", got, err)
	}
}
