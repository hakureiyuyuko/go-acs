package cwmp

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// FTTR 子设备以前只在纳管那次能力探测里采一次，界面上的「采集」会永远停在那一刻。
// 这里钉住定期刷新的规则：真的有子设备才刷、到间隔才刷、枚举用设备自己的拼法。
//
// 场景靠「间隔」构造，不直接改库里的时间：
//   - 间隔设成 1 小时 = 刚采过，不该刷；
//   - 间隔设成 1 纳秒 = 到期了，该刷。
func TestRefreshFttrIfDue(t *testing.T) {
	seedFttr := func(t *testing.T, st *store.Store, id int64, name string) {
		t.Helper()
		if err := st.UpsertParams(id, []store.Param{{Name: name, Value: "K153-10"}}, "getvalues"); err != nil {
			t.Fatal(err)
		}
	}
	fttrTask := func(t *testing.T, st *store.Store, id int64) []*store.Task {
		t.Helper()
		ts, err := st.ListTasks(id, 20)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}

	t.Run("没有子设备对象的设备不发任务", func(t *testing.T) {
		srv, st, id := newTestServer(t, Config{WiFiRefreshInterval: time.Nanosecond})
		srv.refreshFttrIfDue(id)
		if ts := fttrTask(t, st, id); len(ts) != 0 {
			t.Fatalf("没有 FTTR 对象的设备不该发任务，却发了 %d 条", len(ts))
		}
	})

	t.Run("刚采过不刷", func(t *testing.T) {
		srv, st, id := newTestServer(t, Config{WiFiRefreshInterval: time.Hour})
		seedFttr(t, st, id, "InternetGatewayDevice.X_HW_APDevice.1.DeviceType")
		srv.refreshFttrIfDue(id)
		if ts := fttrTask(t, st, id); len(ts) != 0 {
			t.Fatalf("刚采过不该刷，却发了 %d 条", len(ts))
		}
	})

	t.Run("到期就刷：整棵子树枚举 + 顺手取值", func(t *testing.T) {
		srv, st, id := newTestServer(t, Config{WiFiRefreshInterval: time.Nanosecond})
		seedFttr(t, st, id, "InternetGatewayDevice.X_HW_APDevice.1.DeviceType")
		srv.refreshFttrIfDue(id)
		ts := fttrTask(t, st, id)
		if len(ts) != 1 {
			t.Fatalf("到期后该入队一条子设备枚举任务，得到 %d 条", len(ts))
		}
		if ts[0].Kind != TaskGetParameterNames {
			t.Errorf("任务类型不对：%s", ts[0].Kind)
		}
		var p gpnPayload
		if err := json.Unmarshal([]byte(ts[0].Payload), &p); err != nil {
			t.Fatalf("任务负载解析失败: %v (%s)", err, ts[0].Payload)
		}
		if p.Path != "InternetGatewayDevice.X_HW_APDevice." {
			t.Errorf("枚举路径不对：%q", p.Path)
		}
		if !p.ThenFetch {
			t.Error("应该枚举完顺手把值取回来（ThenFetch）")
		}
	})

	t.Run("沿用设备自己的拼法", func(t *testing.T) {
		// 真机上出现过 InternetGateWayDevice. 这种大小写写错，照着我们的拼法去枚举会失败
		srv, st, id := newTestServer(t, Config{WiFiRefreshInterval: time.Nanosecond})
		seedFttr(t, st, id, "InternetGateWayDevice.X_HW_APDevice.1.DeviceType")
		srv.refreshFttrIfDue(id)
		ts := fttrTask(t, st, id)
		if len(ts) != 1 {
			t.Fatalf("该入队一条，得到 %d 条", len(ts))
		}
		var p gpnPayload
		if err := json.Unmarshal([]byte(ts[0].Payload), &p); err != nil {
			t.Fatal(err)
		}
		if p.Path != "InternetGateWayDevice.X_HW_APDevice." {
			t.Errorf("该用设备自己的拼法，得到 %q", p.Path)
		}
	})

	t.Run("间隔为 0（只在纳管时采一次）时不刷", func(t *testing.T) {
		srv, st, id := newTestServer(t, Config{WiFiRefreshInterval: 0})
		seedFttr(t, st, id, "InternetGatewayDevice.X_HW_APDevice.1.DeviceType")
		srv.refreshFttrIfDue(id)
		if ts := fttrTask(t, st, id); len(ts) != 0 {
			t.Errorf("间隔为 0 时不该定期刷，得到 %d 条任务", len(ts))
		}
	})
}

// LastSubtreeAt 是上面的判断依据：既要给出「最新采集时间」，也要给出样本参数名。
func TestLastSubtreeAt(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "acs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id, _, err := st.UpsertDevice(&store.Device{OUI: "001122", ProductClass: "R", SerialNumber: "S2"})
	if err != nil {
		t.Fatal(err)
	}

	if _, _, ok := st.LastSubtreeAt(id, fttrProbeCandidates); ok {
		t.Error("没有这类参数时该返回 ok=false")
	}

	if err := st.UpsertParams(id, []store.Param{
		{Name: "InternetGatewayDevice.X_HW_APDevice.1.DeviceType", Value: "K153-10"},
		{Name: "InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.Name", Value: "1_INTERNET"},
	}, "getvalues"); err != nil {
		t.Fatal(err)
	}
	sample, last, ok := st.LastSubtreeAt(id, fttrProbeCandidates)
	if !ok {
		t.Fatal("该找得到子设备参数")
	}
	if sample != "InternetGatewayDevice.X_HW_APDevice.1.DeviceType" {
		t.Errorf("样本名不对：%q", sample)
	}
	if time.Since(last) > time.Minute {
		t.Errorf("采集时间不对：%v", last)
	}

	// 前缀匹配不区分大小写（设备拼法不统一）
	if _, _, ok := st.LastSubtreeAt(id, []string{"internetgatewaydevice.x_hw_apdevice."}); !ok {
		t.Error("小写前缀也该匹配得上")
	}
}
