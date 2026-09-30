// 纳管时自动规范 ConnectionRequest 凭据的测试。
//
// 【教训】这份测试原来被手滑写进了 connreq_test.go（那个文件里是 Digest/RFC2617 那些），
// 整份覆盖掉了 —— 所以单独放一个文件，名字也写清楚它管什么。
package cwmp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 写 CR 凭据的任务 payload（跟服务端真正下发的形状一致）。
func connReqPayload(root, user, pass string) string {
	b, _ := json.Marshal(spvPayload{Values: []ParamValue{
		{Name: connReqParam(root, pConnReqUser), Value: user, Type: "string"},
		{Name: connReqParam(root, pConnReqPass), Value: pass, Type: "string"},
	}})
	return string(b)
}

func TestDecodeConnReqSet(t *testing.T) {
	// TR-098
	s, ok := decodeConnReqSet(connReqPayload("InternetGatewayDevice.", "acs", "pw"))
	if !ok || !s.matches("acs", "pw") {
		t.Errorf("TR-098 payload 没认出来：%+v ok=%v", s, ok)
	}
	// TR-181
	s, ok = decodeConnReqSet(connReqPayload("Device.", "acs", "pw"))
	if !ok || !s.matches("acs", "pw") {
		t.Errorf("TR-181 payload 没认出来：%+v ok=%v", s, ok)
	}
	// 值不一致
	if s.matches("acs", "OTHER") {
		t.Error("密码不一致却判定为同一套")
	}
	// 不是写 CR 凭据的任务（比如改 WiFi 名字）
	other, _ := json.Marshal(spvPayload{Values: []ParamValue{
		{Name: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID", Value: "x", Type: "string"},
	}})
	if _, ok := decodeConnReqSet(string(other)); ok {
		t.Error("改 SSID 的任务被当成了写 CR 凭据")
	}
	// 坏 JSON
	if _, ok := decodeConnReqSet("{not json"); ok {
		t.Error("坏 JSON 不该认出来")
	}
}

func TestConnReqTaskState(t *testing.T) {
	srv, st, id := newTestServer(t, Config{ConnReqEnabled: true, ConnReqUser: "acs", ConnReqPass: "secret"})

	// 什么任务都没有：新设备，该写
	if s, n := srv.connReqTaskState(id); s != connReqUnknown || n != 0 {
		t.Errorf("没有历史时该是 unknown/0，得到 %v/%d", s, n)
	}

	// 排队中：等它出结果，别重复下发
	if _, err := st.EnqueueTask(&store.Task{DeviceID: id, Kind: TaskSetParameterValues,
		Payload: connReqPayload("InternetGatewayDevice.", "acs", "secret")}); err != nil {
		t.Fatal(err)
	}
	if s, _ := srv.connReqTaskState(id); s != connReqInFlight {
		t.Errorf("有排队任务时该是 inFlight，得到 %v", s)
	}

	// 写成功且值就是当前这套：算规范好了
	tasks, _ := st.ListTasks(id, 10)
	if err := st.CompleteTask(tasks[0].ID, "设置成功"); err != nil {
		t.Fatal(err)
	}
	if s, _ := srv.connReqTaskState(id); s != connReqSatisfied {
		t.Errorf("写成功后该是 satisfied，得到 %v", s)
	}

	// 配置改过（密码变了）：写的是旧值，得重写
	if _, err := st.EnqueueTask(&store.Task{DeviceID: id, Kind: TaskSetParameterValues,
		Payload: connReqPayload("InternetGatewayDevice.", "acs", "new-secret")}); err != nil {
		t.Fatal(err)
	}
	tasks, _ = st.ListTasks(id, 10)
	if err := st.CompleteTask(tasks[0].ID, "设置成功"); err != nil {
		t.Fatal(err)
	}
	if s, _ := srv.connReqTaskState(id); s != connReqUnknown {
		t.Errorf("写的是旧凭据时该重写（unknown），得到 %v", s)
	}
}

func TestConnReqTaskStateRetryAndGiveUp(t *testing.T) {
	srv, st, id := newTestServer(t, Config{ConnReqEnabled: true, ConnReqUser: "acs", ConnReqPass: "secret"})

	// 失败过、但刚失败：先等等，别每轮 Inform 都刷任务
	taskID, err := st.EnqueueTask(&store.Task{DeviceID: id, Kind: TaskSetParameterValues,
		Payload: connReqPayload("InternetGatewayDevice.", "acs", "secret")})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FailTask(taskID, "CPE 返回错误 9003"); err != nil {
		t.Fatal(err)
	}
	if s, n := srv.connReqTaskState(id); s != connReqRetryLater || n != 1 {
		t.Errorf("刚失败该是 retryLater/1，得到 %v/%d", s, n)
	}

	// 把重试间隔调成 0：同一条历史立刻就可以重试了
	srv.connReqRetry = time.Nanosecond
	if s, n := srv.connReqTaskState(id); s != connReqUnknown || n != 1 {
		t.Errorf("过了重试间隔该允许重写（unknown/1），得到 %v/%d", s, n)
	}

	// 连续失败到上限：停手（别再刷任务）
	for i := 0; i < connReqMaxAttempts-1; i++ {
		tid, err := st.EnqueueTask(&store.Task{DeviceID: id, Kind: TaskSetParameterValues,
			Payload: connReqPayload("InternetGatewayDevice.", "acs", "secret")})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.FailTask(tid, "CPE 返回错误 9003"); err != nil {
			t.Fatal(err)
		}
	}
	if s, n := srv.connReqTaskState(id); s != connReqGivingUp || n < connReqMaxAttempts {
		t.Errorf("连续失败到上限该 givingUp，得到 %v/%d", s, n)
	}
}

// EnsureConnReqCredentials：新设备接入就下发；已经有在跑的不重复下发；
// 确认写好之后本进程就不再查库（第二次调用直接跳过）。
func TestEnsureConnReqCredentialsEnqueuesOnce(t *testing.T) {
	srv, st, id := newTestServer(t, Config{ConnReqEnabled: true, ConnReqUser: "acs", ConnReqPass: "secret"})

	countSPV := func() int {
		tasks, err := st.ListTasks(id, 50)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, x := range tasks {
			if x.Kind == TaskSetParameterValues {
				n++
			}
		}
		return n
	}

	srv.EnsureConnReqCredentials(id)
	if n := countSPV(); n != 1 {
		t.Fatalf("新设备该下发一次，得到 %d 条", n)
	}
	// 任务还在排队：不该再下发一条
	srv.EnsureConnReqCredentials(id)
	if n := countSPV(); n != 1 {
		t.Fatalf("已经在排队时不该重复下发，得到 %d 条", n)
	}

	// 设备把用户名回读成别人的（比如运营商改过）：必须重写
	if err := st.UpsertParams(id, []store.Param{
		{Name: "InternetGatewayDevice.ManagementServer.ConnectionRequestUsername", Value: "isp-admin"},
	}, "getvalues"); err != nil {
		t.Fatal(err)
	}
	srv.connReqDone = map[int64]bool{} // 清掉进程内记忆，模拟重启后再看一遍
	tasks, _ := st.ListTasks(id, 5)
	if err := st.CompleteTask(tasks[0].ID, "设置成功"); err != nil {
		t.Fatal(err)
	}
	srv.EnsureConnReqCredentials(id)
	if n := countSPV(); n != 2 {
		t.Fatalf("设备上的用户名不一致时该重写，得到 %d 条", n)
	}
}

// 关掉主动唤醒时什么都不做（不写设备）。
func TestEnsureConnReqDisabled(t *testing.T) {
	srv, st, id := newTestServer(t, Config{ConnReqEnabled: false, ConnReqUser: "acs", ConnReqPass: "secret"})
	srv.EnsureConnReqCredentials(id)
	if n, _ := st.PendingTaskCount(id); n != 0 {
		t.Errorf("关掉主动唤醒时不该下发任何任务，得到 %d 条", n)
	}
}

// 完整走一遍「新设备接入 → 写凭据 → 成功 → 唤醒可用」的状态流转（不碰真网络）。
func TestConnReqCredentialFlow(t *testing.T) {
	srv, st, id := newTestServer(t, Config{ConnReqEnabled: true, ConnReqUser: "acs", ConnReqPass: "secret"})

	srv.EnsureConnReqCredentials(id)
	tasks, _ := st.ListTasks(id, 5)
	if len(tasks) != 1 || !strings.Contains(tasks[0].Payload, "ConnectionRequestPassword") {
		t.Fatalf("下发的应该是写 CR 凭据的任务：%+v", tasks)
	}
	// 设备执行成功（真实场景里还会回读用户名）
	if err := st.CompleteTask(tasks[0].ID, "设置成功"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertParams(id, []store.Param{
		{Name: "InternetGatewayDevice.ManagementServer.ConnectionRequestUsername", Value: "acs"},
		{Name: "InternetGatewayDevice.ManagementServer.ConnectionRequestPassword", Value: ""},
	}, "getvalues"); err != nil {
		t.Fatal(err)
	}

	// 再劝一次：状态是 satisfied，且此后走进程内快速路径（不再查库）
	srv.EnsureConnReqCredentials(id)
	if n, _ := st.PendingTaskCount(id); n != 0 {
		t.Errorf("已经规范好了不该再下发，得到 %d 条", n)
	}
	srv.connReqMu.Lock()
	done := srv.connReqDone[id]
	srv.connReqMu.Unlock()
	if !done {
		t.Error("确认规范之后该记进进程内快速路径，否则每轮 Inform 都要查库")
	}
}
