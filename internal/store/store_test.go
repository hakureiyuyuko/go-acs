package store

import (
	"path/filepath"
	"strings"
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

func TestParamsUpsertNamesDoesNotWipeValue(t *testing.T) {
	st := newTestStore(t)
	id, _, _ := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: "S"})

	// 先正经读到值（GetParameterValues）
	if err := st.UpsertParams(id, []Param{{Name: "X.Y", Value: "192.168.1.1", ValueType: "string"}}, "getvalues"); err != nil {
		t.Fatal(err)
	}
	before, _, _ := st.GetParam(id, "X.Y")

	// 再浏览参数树（名字枚举不带值）—— 不能把值刷成空串，也不该刷采集时间
	time.Sleep(10 * time.Millisecond)
	if err := st.UpsertParams(id, []Param{{Name: "X.Y", Writable: true}}, "getnames"); err != nil {
		t.Fatal(err)
	}

	p, ok, err := st.GetParam(id, "X.Y")
	if err != nil || !ok {
		t.Fatalf("参数没找到: %v", err)
	}
	if p.Value != "192.168.1.1" {
		t.Errorf("名字枚举把值改掉了：%q（这会让整个参数表看起来“没数据”）", p.Value)
	}
	if p.ValueType != "string" {
		t.Errorf("名字枚举把类型改掉了：%q", p.ValueType)
	}
	if !p.Writable {
		t.Error("名字枚举带来的“可写”信息应当保留")
	}
	if !p.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("名字枚举不该刷采集时间：%v → %v", before.UpdatedAt, p.UpdatedAt)
	}

	// 真正取到的空值（设备就这么回的）仍然要如实落库
	if err := st.UpsertParams(id, []Param{{Name: "X.Y", Value: "", ValueType: "string"}}, "getvalues"); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := st.GetParam(id, "X.Y"); p.Value != "" {
		t.Errorf("真读到空值时应如实记录，得到 %q", p.Value)
	}
}

// 删除设备：本地记录（参数 / 任务 / 上报历史）要一并清掉，不能留下孤儿数据。
func TestDeleteDeviceCascades(t *testing.T) {
	st := newTestStore(t)
	id, _, err := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: "DEL-ME"})
	if err != nil {
		t.Fatal(err)
	}
	keep, _, err := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: "KEEP"})
	if err != nil {
		t.Fatal(err)
	}
	// 被删设备的关联数据
	if err := st.UpsertParams(id, []Param{{Name: "X.Y", Value: "1"}}, "getvalues"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueTask(&Task{DeviceID: id, Kind: "GetParameterValues", Payload: `{"names":["X.Y"]}`}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertInform(&InformRecord{DeviceID: id, Events: "2 PERIODIC", ParamCount: 3}); err != nil {
		t.Fatal(err)
	}
	// 另一台设备的数据，删完后必须还在
	if err := st.UpsertParams(keep, []Param{{Name: "X.Y", Value: "2"}}, "getvalues"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueTask(&Task{DeviceID: keep, Kind: "GetParameterValues", Payload: `{"names":["X.Y"]}`}); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteDevice(id); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := st.GetDevice(id); err == nil {
		t.Error("设备还在库里")
	}
	if n, _ := st.CountParams(id); n != 0 {
		t.Errorf("参数没级联删除：%d 条", n)
	}
	if tasks, _ := st.ListTasks(id, 10); len(tasks) != 0 {
		t.Errorf("任务没级联删除：%d 条", len(tasks))
	}
	if infs, _ := st.ListInforms(id, 10); len(infs) != 0 {
		t.Errorf("上报记录没级联删除：%d 条", len(infs))
	}

	// 其它设备不受影响
	if d, err := st.GetDevice(keep); err != nil || d.SerialNumber != "KEEP" {
		t.Errorf("另一台设备被误删了: %v %+v", err, d)
	}
	if n, _ := st.CountParams(keep); n != 1 {
		t.Errorf("另一台设备的参数被连累了：%d 条", n)
	}
	if tasks, _ := st.ListTasks(keep, 10); len(tasks) != 1 {
		t.Errorf("另一台设备的任务被连累了：%d 条", len(tasks))
	}

	// 重复删：得报错（界面上要能给“设备不存在”的提示）
	if err := st.DeleteDevice(id); err == nil {
		t.Error("删不存在的设备应报错")
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

// 「下发后 CPE 没应答」的任务要能退回待办，且不能无限重发。
func TestRequeueTask(t *testing.T) {
	st := newTestStore(t)
	devID, _, err := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: "REQ"})
	if err != nil {
		t.Fatal(err)
	}
	tid, err := st.EnqueueTask(&Task{DeviceID: devID, Kind: "GetParameterValues", Payload: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	// 下发（置 running）
	claimed, err := st.ClaimNextTask(devID, nil)
	if err != nil || claimed == nil {
		t.Fatalf("取任务失败: %v %+v", err, claimed)
	}
	if claimed.Status != "running" {
		t.Fatalf("应当已置为 running：%q", claimed.Status)
	}

	// 会话结束时退回
	if err := st.RequeueTask(tid, 3, "设备没有应答"); err != nil {
		t.Fatalf("退回失败: %v", err)
	}
	got, err := st.GetTask(tid)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Errorf("应退回 pending，实际 %q", got.Status)
	}
	if got.RetryCount != 1 {
		t.Errorf("重试计数应为 1，实际 %d", got.RetryCount)
	}
	if !strings.Contains(got.Result, "没有应答") {
		t.Errorf("应记下退回原因：%q", got.Result)
	}

	// 反复不应答 → 到上限后判失败（不能永远重发）
	for i := 0; i < 5; i++ {
		if _, err := st.ClaimNextTask(devID, nil); err != nil {
			t.Fatal(err)
		}
		if err := st.RequeueTask(tid, 3, "设备没有应答"); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = st.GetTask(tid)
	if got.Status != "failed" {
		t.Errorf("超过重试上限应判失败，实际 %q", got.Status)
	}

	// 已经有结果的任务不能被退回覆盖
	tid2, err := st.EnqueueTask(&Task{DeviceID: devID, Kind: "Reboot"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimNextTask(devID, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteTask(tid2, "已接受"); err != nil {
		t.Fatal(err)
	}
	if err := st.RequeueTask(tid2, 3, "设备没有应答"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetTask(tid2); got.Status != "done" {
		t.Errorf("已完成的任务不该被退回：%q", got.Status)
	}
}

// 任务历史保留上限：每台设备最多留 N 条，只裁已结束的，未结束的一条都不许丢。
func TestTaskHistoryLimit(t *testing.T) {
	st := newTestStore(t)
	dev, _, err := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: "S1"})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := st.UpsertDevice(&Device{OUI: "A", ProductClass: "P", SerialNumber: "S2"})
	if err != nil {
		t.Fatal(err)
	}

	st.SetTaskHistoryLimit(10)
	var ids []int64
	for i := 0; i < 25; i++ {
		id, err := st.EnqueueTask(&Task{DeviceID: dev, Kind: "GetParameterValues", Payload: `{"names":["A"]}`})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if err := st.CompleteTask(id, "done"); err != nil {
			t.Fatal(err)
		}
	}
	// 另一台设备只留 3 条：别的设备的量不该把它的记录挤掉
	for i := 0; i < 3; i++ {
		if _, err := st.EnqueueTask(&Task{DeviceID: other, Kind: "GetParameterValues", Payload: `{"names":["B"]}`}); err != nil {
			t.Fatal(err)
		}
	}

	tasks, err := st.ListTasks(dev, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 10 {
		t.Fatalf("上限 10，实际留下 %d 条", len(tasks))
	}
	// 留下的是最新的 10 条（ListTasks 按 id 倒序）
	if tasks[0].ID != ids[len(ids)-1] {
		t.Errorf("最新的一条应该留着：%d vs %d", tasks[0].ID, ids[len(ids)-1])
	}
	for _, t2 := range tasks {
		for _, old := range ids[:15] {
			if t2.ID == old {
				t.Errorf("最旧的 %d 条应当被裁掉，但 %d 还在", 15, old)
			}
		}
	}
	if o, _ := st.ListTasks(other, 100); len(o) != 3 {
		t.Errorf("另一台设备的 3 条不该被裁：%d", len(o))
	}

	// 未结束的任务永不删：上限 5，排 2 条 pending + 5 条已完成
	st.SetTaskHistoryLimit(5)
	if _, err := st.PruneTasks(); err != nil {
		t.Fatal(err)
	}
	pend1, _ := st.EnqueueTask(&Task{DeviceID: dev, Kind: "Reboot"})
	pend2, _ := st.EnqueueTask(&Task{DeviceID: dev, Kind: "Reboot"})
	all, _ := st.ListTasks(dev, 100)
	kept := false
	for _, t2 := range all {
		if t2.ID == pend1 || t2.ID == pend2 {
			kept = true
		}
	}
	if !kept && len(all) < 7 {
		t.Errorf("排队的任务被误删了（剩下 %d 条）", len(all))
	}
	if n, _ := st.PendingTaskCount(dev); n < 2 {
		t.Errorf("pending 任务数 = %d，应 ≥2", n)
	}

	// 不限（0）时什么都不裁
	st.SetTaskHistoryLimit(0)
	if n, err := st.PruneTasks(); err != nil || n != 0 {
		t.Errorf("上限为 0 时不该裁剪：n=%d err=%v", n, err)
	}
}
