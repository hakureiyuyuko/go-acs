package cwmp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 任务派发器：有任务排队就把设备叫起来，而不是干等它周期性上报。
//
// 真机上设备的周期可以长到几十分钟，干等的话界面上点一下要等半小时。
func TestDispatchPendingTasks(t *testing.T) {
	var hits atomic.Int64
	cr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK) // 设备回 200 就算唤醒了
	}))
	defer cr.Close()

	enqueue := func(t *testing.T, st *store.Store, id int64) {
		t.Helper()
		if _, err := st.EnqueueTask(&store.Task{DeviceID: id, Kind: TaskGetParameterValues, Payload: "{}"}); err != nil {
			t.Fatal(err)
		}
	}

	srv, st, id := newTestServer(t, Config{ConnReqEnabled: true})
	// newTestServer 建出来的设备默认「在线且刚上报过」，这里补上 Connection Request 地址；
	// 两个时间窗在测试里清掉（否则「刚上报过」会被跳过，那是另一条用例要验的）。
	if _, _, err := st.UpsertDevice(&store.Device{
		OUI: "001122", ProductClass: "R", SerialNumber: "S1", ConnRequestURL: cr.URL + "/cr",
	}); err != nil {
		t.Fatal(err)
	}
	srv.wakeIdle, srv.wakeCooldown = 0, 0

	t.Run("没有排队任务时不叫", func(t *testing.T) {
		srv.DispatchPendingTasks(context.Background())
		if n := hits.Load(); n != 0 {
			t.Fatalf("没任务不该唤醒，发了 %d 次", n)
		}
	})

	t.Run("有任务排队就主动唤醒", func(t *testing.T) {
		enqueue(t, st, id)
		srv.DispatchPendingTasks(context.Background())
		if n := hits.Load(); n != 1 {
			t.Fatalf("该唤醒 1 次，实际 %d", n)
		}
	})

	t.Run("冷却期内不重复叫", func(t *testing.T) {
		srv.wakeCooldown = time.Minute
		srv.noteWake(id, time.Now())
		srv.DispatchPendingTasks(context.Background())
		if n := hits.Load(); n != 1 {
			t.Fatalf("冷却期内不该再叫，实际 %d", n)
		}
	})

	t.Run("设备刚上报过不叫（它正在会话里，任务这次就下发）", func(t *testing.T) {
		srv.wakeCooldown = 0
		srv.wakeIdle = time.Minute
		srv.DispatchPendingTasks(context.Background())
		if n := hits.Load(); n != 1 {
			t.Fatalf("刚上报过不该叫，实际 %d", n)
		}
	})

	t.Run("冷却过了会再叫一次（任务还挂着）", func(t *testing.T) {
		srv.wakeIdle = 0
		srv.wakeCooldown = time.Nanosecond
		srv.DispatchPendingTasks(context.Background())
		if n := hits.Load(); n != 2 {
			t.Fatalf("冷却过后该再叫一次，实际 %d", n)
		}
	})

	t.Run("设备离线时不叫（交给离线巡检）", func(t *testing.T) {
		if err := st.MarkOffline(id); err != nil {
			t.Fatal(err)
		}
		srv.wakeCooldown = 0
		before := hits.Load()
		srv.DispatchPendingTasks(context.Background())
		if n := hits.Load(); n != before {
			t.Fatalf("离线设备不该叫，实际多发了 %d 次", n-before)
		}
	})
}

// 任务做完（没有 pending 了）就不该再叫。
func TestDispatchStopsWhenNoPending(t *testing.T) {
	var hits atomic.Int64
	cr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer cr.Close()

	srv, st, id := newTestServer(t, Config{ConnReqEnabled: true})
	srv.wakeIdle, srv.wakeCooldown = 0, 0
	if _, _, err := st.UpsertDevice(&store.Device{
		OUI: "001122", ProductClass: "R", SerialNumber: "WAKE2", ConnRequestURL: cr.URL,
	}); err != nil {
		t.Fatal(err)
	}
	srv.DispatchPendingTasks(context.Background())
	if n := hits.Load(); n != 0 {
		t.Fatalf("没任务时不该唤醒（device=%d）", id)
	}
}

// 关掉主动唤醒（-connection-request=false）时，派发器也不该发请求。
func TestDispatchRespectsConnReqDisabled(t *testing.T) {
	var hits atomic.Int64
	cr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer cr.Close()

	srv, st, id := newTestServer(t, Config{ConnReqEnabled: false})
	srv.wakeIdle, srv.wakeCooldown = 0, 0
	if _, _, err := st.UpsertDevice(&store.Device{
		OUI: "001122", ProductClass: "R", SerialNumber: "WAKE3", ConnRequestURL: cr.URL,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueTask(&store.Task{DeviceID: id, Kind: TaskGetParameterValues, Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	srv.DispatchPendingTasks(context.Background())
	if n := hits.Load(); n != 0 {
		t.Fatalf("关掉唤醒后不该发请求，实际 %d", n)
	}
}

// store 侧：只列还有 pending 任务的设备，且去重。
func TestDevicesWithPendingTasks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id1, _, _ := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "P1"})
	id2, _, _ := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "P2"})

	ids, err := st.DevicesWithPendingTasks()
	if err != nil || len(ids) != 0 {
		t.Fatalf("还没任务时该是空的：%v %v", ids, err)
	}

	// 同一台设备两条任务 → 只出现一次
	for i := 0; i < 2; i++ {
		if _, err := st.EnqueueTask(&store.Task{DeviceID: id1, Kind: TaskGetParameterValues, Payload: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.EnqueueTask(&store.Task{DeviceID: id2, Kind: TaskGetParameterValues, Payload: "{}"}); err != nil {
		t.Fatal(err)
	}
	ids, err = st.DevicesWithPendingTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != id1 || ids[1] != id2 {
		t.Fatalf("该列出 2 台设备（去重、按 id 升序）：%v", ids)
	}
}
