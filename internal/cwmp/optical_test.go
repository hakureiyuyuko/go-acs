package cwmp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 光功率没有统一参数名，所以采集方式是「枚举光口子树」而不是猜参数名：
// 这里锁定入队的是一条带 ThenFetch 的 GetParameterNames，路径落在光口所在子树。
func TestEnqueueFetchOptical(t *testing.T) {
	s, st, devID := newTestServer(t, Config{})

	if _, err := s.EnqueueFetchOptical(devID); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	tasks, err := st.ListTasks(devID, 10)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("一条任务都没入队")
	}
	// 根未知时两套命名各入队一条，所以这里找「枚举光口那几条」而不是只认第一条
	var got *store.Task
	for _, tk := range tasks {
		if tk.ID == 0 || tk.Kind != TaskGetParameterNames {
			continue
		}
		var q gpnPayload
		if json.Unmarshal([]byte(tk.Payload), &q) != nil {
			continue
		}
		if strings.Contains(q.Path, "WANDevice") || strings.Contains(q.Path, "Optical.") {
			got = tk
			break
		}
	}
	if got == nil {
		t.Fatalf("没有入队枚举光口子树的任务，实际任务：%d 条", len(tasks))
	}
	var p gpnPayload
	if err := json.Unmarshal([]byte(got.Payload), &p); err != nil {
		t.Fatalf("解析任务内容失败: %v", err)
	}
	if !strings.Contains(p.Path, "WANDevice") && !strings.Contains(p.Path, "Optical.") {
		t.Errorf("应枚举光口所在子树（WANDevice / Optical），得到 %q", p.Path)
	}
	if !p.ThenFetch {
		t.Error("枚举完要接着取值（ThenFetch），否则拿不到数值")
	}
	if p.SkipStore != true {
		t.Error("只存值不存节点名（SkipStore），免得几百个子树节点名把界面刷屏")
	}
}

// 数据模型根不同，要枚举的子树也不同；根未知时两套都试。
func TestOpticalSubtreePaths(t *testing.T) {
	cases := map[string]bool{
		"InternetGatewayDevice.": true,
		"Device.":                true,
		"":                       true,
	}
	for root := range cases {
		paths := opticalSubtreePaths(root)
		if len(paths) == 0 {
			t.Fatalf("root %q 应至少给一条子树路径", root)
		}
		for _, p := range paths {
			if !strings.HasSuffix(p, ".") {
				t.Errorf("root %q：子树路径应以 . 结尾，得到 %q", root, p)
			}
		}
	}
	if got := opticalSubtreePaths("InternetGatewayDevice."); len(got) != 1 ||
		got[0] != "InternetGatewayDevice.WANDevice." {
		t.Errorf("TR-098 只该枚举 WANDevice，得到 %v", got)
	}
	if got := opticalSubtreePaths(""); len(got) < 2 {
		t.Errorf("根未知时两套命名都要试，得到 %v", got)
	}
}
