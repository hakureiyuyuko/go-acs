package cwmp

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"acs/internal/store"
)

func newTestServer(t *testing.T, cfg Config) (*Server, *store.Store, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	id, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "S1"})
	if err != nil {
		t.Fatalf("建测试设备失败: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewServer(st, cfg, log), st, id
}

func TestFilterLeafNames(t *testing.T) {
	infos := []ParamInfo{
		{Name: "Device.WiFi."},                              // 对象节点，跳过
		{Name: "Device.WiFi.SSID"},                          // 保留
		{Name: "Device.WiFi.AssociatedDevice.1."},           // 对象节点，跳过
		{Name: "Device.WiFi.AssociatedDevice.1.MACAddress"}, // 含 AssociatedDevice，跳过
		{Name: "Device.WiFi.Channel"},                       // 保留
		{Name: "Device.WiFi.RadioEnabled"},                  // 保留
	}

	// 不排除任何东西时：只跳过对象节点，叶子参数全保留
	got := filterLeafNames(infos, nil, nil, 0)
	want := []string{
		"Device.WiFi.SSID",
		"Device.WiFi.AssociatedDevice.1.MACAddress",
		"Device.WiFi.Channel",
		"Device.WiFi.RadioEnabled",
	}
	if len(got) != len(want) {
		t.Fatalf("结果 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}

	// 排除子串
	got = filterLeafNames(infos, nil, []string{"AssociatedDevice", "Channel"}, 0)
	if len(got) != 2 || got[0] != "Device.WiFi.SSID" || got[1] != "Device.WiFi.RadioEnabled" {
		t.Errorf("排除后 = %v", got)
	}

	// 截断
	got = filterLeafNames(infos, nil, nil, 1)
	if len(got) != 1 || got[0] != "Device.WiFi.SSID" {
		t.Errorf("截断后 = %v", got)
	}
}

// 真机实测（华为 HN8145X6N）：一次 GetParameterValues 最多只回 256 个参数，
// 多出来的静默丢弃。所以必须分批 —— 这个测试守住分批逻辑。
func TestEnqueueGPVDividedChunks(t *testing.T) {
	srv, st, devID := newTestServer(t, Config{MaxParamsPerRequest: 200})

	names := make([]string, 450)
	for i := range names {
		names[i] = fmt.Sprintf("Device.P%d", i)
	}

	batches, err := srv.enqueueGPVDivided(devID, names)
	if err != nil {
		t.Fatal(err)
	}
	if batches != 3 {
		t.Fatalf("450 个参数按每批 200 应分成 3 批，实际 %d", batches)
	}

	tasks, err := st.ListTasks(devID, 10)
	if err != nil {
		t.Fatal(err)
	}
	// ListTasks 按 id 倒序，反转成入队顺序
	var sizes []int
	for i := len(tasks) - 1; i >= 0; i-- {
		var p gpvPayload
		if err := json.Unmarshal([]byte(tasks[i].Payload), &p); err != nil {
			t.Fatalf("任务载荷不是合法 JSON: %v", err)
		}
		sizes = append(sizes, len(p.Names))
	}
	want := []int{200, 200, 50}
	if len(sizes) != len(want) {
		t.Fatalf("批次数 = %d，期望 %d", len(sizes), len(want))
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Errorf("第 %d 批大小 = %d，期望 %d", i+1, sizes[i], want[i])
		}
	}

	// 参数一个都不能丢
	total := 0
	for _, s := range sizes {
		total += s
	}
	if total != len(names) {
		t.Errorf("分批后参数总数 = %d，期望 %d（不能丢）", total, len(names))
	}

	// 空列表不应入队
	if n, err := srv.enqueueGPVDivided(devID, nil); err != nil || n != 0 {
		t.Errorf("空列表不该入队: n=%d err=%v", n, err)
	}
}

func TestEnqueueGPVDividedUsesDefaultLimit(t *testing.T) {
	// MaxParamsPerRequest 没配时用默认值（不能变成「一次全发」）
	srv, st, devID := newTestServer(t, Config{})
	if srv.cfg.MaxParamsPerRequest != 0 {
		t.Fatal("测试前提：配置里没设上限")
	}
	names := make([]string, defaultMaxParamsPerRequest+1)
	for i := range names {
		names[i] = fmt.Sprintf("Device.Q%d", i)
	}
	batches, err := srv.enqueueGPVDivided(devID, names)
	if err != nil {
		t.Fatal(err)
	}
	if batches != 2 {
		t.Errorf("应按默认上限 %d 分成 2 批，实际 %d 批", defaultMaxParamsPerRequest, batches)
	}
	_ = st
}

// 枚举 → 自动取值的链路：GPN 任务载荷里的 then_fetch 决定要不要接着入队 GPV。
func TestFilterLeafNamesRespectsEmpty(t *testing.T) {
	if got := filterLeafNames(nil, nil, nil, 0); len(got) != 0 {
		t.Errorf("空输入应得空结果，实际 %v", got)
	}
	if got := filterLeafNames([]ParamInfo{{Name: "A."}, {Name: ""}}, nil, nil, 0); len(got) != 0 {
		t.Errorf("只有对象节点时应得空结果，实际 %v", got)
	}
}

// Include 白名单：只保留以指定后缀结尾的名字（看板的 WiFi 摘要靠它避免拉回整棵子树）。
func TestFilterLeafNamesInclude(t *testing.T) {
	infos := []ParamInfo{
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID"},
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.TotalAssociations"},
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.PreSharedKey.1.KeyPassphrase"},
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.AssociatedDevice.1.MACAddress"},
	}
	got := filterLeafNames(infos, []string{".SSID", ".TotalAssociations"}, nil, 0)
	if len(got) != 2 {
		t.Fatalf("白名单应筛出 2 条，实际 %d: %v", len(got), got)
	}
	if !strings.HasSuffix(got[0], ".SSID") || !strings.HasSuffix(got[1], ".TotalAssociations") {
		t.Errorf("筛选结果不对: %v", got)
	}

	// 白名单与黑名单同时生效
	got = filterLeafNames(infos, []string{".SSID", ".TotalAssociations"}, []string{"Total"}, 0)
	if len(got) != 1 || !strings.HasSuffix(got[0], ".SSID") {
		t.Errorf("白+黑名单同时生效时结果不对: %v", got)
	}
}
