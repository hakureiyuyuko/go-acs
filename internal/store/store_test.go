package store

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestUpsertDeviceCreatesThenUpdates(t *testing.T) {
	st := newTestStore(t)

	id1, created, err := st.UpsertDevice(&Device{
		OUI: "001122", ProductClass: "R", SerialNumber: "S1",
		Manufacturer: "ACME", SoftwareVersion: "1.0",
	})
	if err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	if !created {
		t.Error("首次写入应该标记为新建")
	}

	// 第二次：身份相同但字段部分缺失，不能把已有信息抹掉
	id2, created, err := st.UpsertDevice(&Device{
		OUI: "001122", ProductClass: "R", SerialNumber: "S1",
		ModelName: "M-9",
	})
	if err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}
	if created {
		t.Error("第二次不该是新建")
	}
	if id1 != id2 {
		t.Fatalf("同一设备应得到同一个 ID：%d vs %d", id1, id2)
	}

	d, err := st.GetDevice(id1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Manufacturer != "ACME" {
		t.Errorf("厂商被空值覆盖了: %q", d.Manufacturer)
	}
	if d.SoftwareVersion != "1.0" {
		t.Errorf("软件版本被空值覆盖了: %q", d.SoftwareVersion)
	}
	if d.ModelName != "M-9" {
		t.Errorf("新值没有写入: %q", d.ModelName)
	}
	if !d.Online {
		t.Error("上报之后应为在线")
	}
}

func TestMergeDeviceFieldsDoesNotTouchLastInform(t *testing.T) {
	st := newTestStore(t)
	id, _, _ := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: "S"})

	before, _ := st.GetDevice(id)
	time.Sleep(10 * time.Millisecond)

	if err := st.MergeDeviceFields(id, &Device{SoftwareVersion: "2.0", DataModelRoot: "Device."}); err != nil {
		t.Fatal(err)
	}
	after, _ := st.GetDevice(id)

	if after.SoftwareVersion != "2.0" {
		t.Errorf("软件版本没更新: %q", after.SoftwareVersion)
	}
	if after.DataModelRoot != "Device." {
		t.Errorf("数据模型根没更新: %q", after.DataModelRoot)
	}
	if !after.LastInformAt.Equal(before.LastInformAt) {
		t.Errorf("补充信息不该刷新最后上报时间: %v -> %v", before.LastInformAt, after.LastInformAt)
	}
}

func TestParamsUpsertKeepsWritable(t *testing.T) {
	st := newTestStore(t)
	id, _, _ := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: "S"})

	// 先由 GetParameterNames 探到「可写」
	if err := st.UpsertParams(id, []Param{{Name: "X.Y", Writable: true, Source: "getnames"}}, "getnames"); err != nil {
		t.Fatal(err)
	}
	// 再由 Inform 写入（Inform 不带 writable 信息）
	if err := st.UpsertParams(id, []Param{{Name: "X.Y", Value: "v", ValueType: "string"}}, "inform"); err != nil {
		t.Fatal(err)
	}

	p, ok, err := st.GetParam(id, "X.Y")
	if err != nil || !ok {
		t.Fatalf("参数没找到: %v", err)
	}
	if p.Value != "v" {
		t.Errorf("值 = %q", p.Value)
	}
	if !p.Writable {
		t.Error("writable 被 False 覆盖了，应该保持 True")
	}
}

func TestTaskLifecycle(t *testing.T) {
	st := newTestStore(t)
	id, _, _ := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: "S"})

	tid, err := st.EnqueueTask(&Task{DeviceID: id, Kind: "GetParameterValues", Payload: `{"names":["X."]}`})
	if err != nil {
		t.Fatal(err)
	}

	// 去重：同设备同类型已有 pending 时不再入队
	if _, created, _ := st.EnqueueTaskIfAbsent(&Task{DeviceID: id, Kind: "GetParameterValues"}); created {
		t.Error("同类型待办已存在时不该重复入队")
	}

	got, err := st.ClaimNextTask(id, nil)
	if err != nil || got == nil {
		t.Fatalf("取任务失败: %v %v", got, err)
	}
	if got.ID != tid || got.Status != TaskRunning {
		t.Errorf("任务状态 = %+v", got)
	}
	// 取过一次之后不该再取到
	again, err := st.ClaimNextTask(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Error("running 的任务不该被再次取走")
	}

	if err := st.CompleteTask(tid, "ok"); err != nil {
		t.Fatal(err)
	}

	// 残留的 running 任务能退回待办
	if _, err := st.EnqueueTask(&Task{DeviceID: id, Kind: "Reboot"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimNextTask(id, nil); err != nil {
		t.Fatal(err)
	}
	n, err := st.ResetRunningTasks()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应退回 1 条 running 任务，实际 %d", n)
	}
}

// 备注：默认空、可设置、设备上报不会把它冲掉，而且**重复打开同一个库**（迁移幂等）不能出错。
func TestDeviceNoteAndMigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("首次打开失败: %v", err)
	}
	id, _, err := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: "S"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.GetDevice(id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Note != "" {
		t.Errorf("备注默认应为空，实际 %q", d.Note)
	}

	if err := st.SetDeviceNote(id, "3 楼会议室"); err != nil {
		t.Fatal(err)
	}
	// 设备再上报一次，人工写的备注不能被冲掉
	if _, _, err := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: "S", Manufacturer: "ACME"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MergeDeviceFields(id, &Device{SoftwareVersion: "1.0"}); err != nil {
		t.Fatal(err)
	}
	d, _ = st.GetDevice(id)
	if d.Note != "3 楼会议室" {
		t.Errorf("设备上报不应冲掉备注，实际 %q", d.Note)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// 关键：重新打开同一个库 —— 迁移必须幂等，不能因为列已存在而失败
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("重复打开应成功（迁移必须幂等）: %v", err)
	}
	defer st2.Close()
	d, err = st2.GetDevice(id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Note != "3 楼会议室" {
		t.Errorf("重新打开后备注丢了: %q", d.Note)
	}

	// 再打开一次，确保不会反复尝试建列
	st3, err := Open(path)
	if err != nil {
		t.Fatalf("第三次打开应成功: %v", err)
	}
	st3.Close()
}

func TestListDevicesAndStats(t *testing.T) {
	st := newTestStore(t)
	for _, s := range []string{"S1", "S2", "S3"} {
		if _, _, err := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: s}); err != nil {
			t.Fatal(err)
		}
	}
	devs, err := st.ListDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 3 {
		t.Fatalf("设备数 = %d", len(devs))
	}
	stats, err := st.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Devices != 3 || stats.Online != 3 {
		t.Errorf("统计 = %+v", stats)
	}
}

func TestMarkStaleOffline(t *testing.T) {
	st := newTestStore(t)
	id, _, _ := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: "S"})

	// 时间倒推久一点，模拟长时间没上报
	if _, err := st.db.Exec(`UPDATE devices SET last_inform_at = ? WHERE id = ?`,
		ts(time.Now().Add(-2*time.Hour)), id); err != nil {
		t.Fatal(err)
	}
	n, err := st.MarkStaleOffline(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应标记 1 台离线，实际 %d", n)
	}
	d, _ := st.GetDevice(id)
	if d.Online {
		t.Error("应该已经离线")
	}
}
