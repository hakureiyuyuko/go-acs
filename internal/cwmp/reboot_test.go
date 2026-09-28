package cwmp

import (
	"strings"
	"testing"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 重启报文要带 CommandKey（设备回 RebootResponse 时会原样带回，便于配对）。
func TestBuildTaskBodyReboot(t *testing.T) {
	body, err := buildTaskBody(&store.Task{
		Kind:       TaskReboot,
		CommandKey: "reboot-key-1",
	})
	if err != nil {
		t.Fatalf("构造报文失败: %v", err)
	}
	if !strings.Contains(body, "<cwmp:Reboot><CommandKey>reboot-key-1</CommandKey></cwmp:Reboot>") {
		t.Fatalf("重启报文不对：%s", body)
	}
}

// 重启是破坏性操作：同一台设备上不能堆多个（有排队/进行中的就拒绝）。
func TestEnqueueRebootRefusesDuplicate(t *testing.T) {
	srv, st, devID := newTestServer(t, Config{})

	id, err := srv.EnqueueReboot(devID)
	if err != nil {
		t.Fatalf("第一次入队失败: %v", err)
	}
	if id == 0 {
		t.Fatal("应该返回任务 ID")
	}
	// 任务载荷为空（重启不需要参数），任务类型必须是 Reboot
	tk, err := st.GetTask(id)
	if err != nil || tk == nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if tk.Kind != TaskReboot || tk.CommandKey == "" {
		t.Fatalf("任务不对：kind=%q key=%q", tk.Kind, tk.CommandKey)
	}

	if _, err := srv.EnqueueReboot(devID); err == nil {
		t.Fatal("排队中还能再下发一次重启 —— 必须拒绝")
	}

	// 设备回执之后（任务结束）就应该能再重启一次
	if err := st.CompleteTask(id, "设备已接受重启指令，正在重启…"); err != nil {
		t.Fatalf("标记完成失败: %v", err)
	}
	if _, err := srv.EnqueueReboot(devID); err != nil {
		t.Fatalf("上一次结束后应该允许再次重启: %v", err)
	}
}

func TestEnqueueRebootUnknownDevice(t *testing.T) {
	srv, _, _ := newTestServer(t, Config{})
	if _, err := srv.EnqueueReboot(4242); err == nil {
		t.Fatal("设备不存在时应报错（不能建出孤儿任务）")
	}
}
