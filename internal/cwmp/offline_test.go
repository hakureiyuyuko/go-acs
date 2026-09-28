package cwmp

import (
	"testing"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 基准时间，测试里一律用相对时间算，避免依赖机器时钟。
var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func dev(online bool, age, probeCount int, probeAge int) *store.Device {
	d := &store.Device{
		ID:               1,
		SerialNumber:     "TESTSN",
		Online:           online,
		PeriodicInterval: 120,
		ConnRequestURL:   "http://10.0.0.1:7547/cr",
		ProbeCount:       probeCount,
		LastInformAt:     t0.Add(-time.Duration(age) * time.Second),
	}
	// probeAge != 0 就设探测时间：计数为 0 也可能是「上次探测成功过」（成功会清零但留时间）
	if probeAge != 0 {
		d.ProbeAt = t0.Add(-time.Duration(probeAge) * time.Second)
	}
	return d
}

func policy() offlinePolicy {
	return offlinePolicy{
		Enabled:  true,
		Factor:   2,
		Attempts: 3,
		Interval: 15 * time.Second,
		Grace:    30 * time.Second,
		Fallback: 10 * time.Minute,
	}
}

func TestPlanOffline(t *testing.T) {
	cases := []struct {
		name   string
		device *store.Device
		mutate func(p *offlinePolicy)
		want   offlineAction
	}{
		{
			name:   "周期内什么都不做",
			device: dev(true, 100, 0, 0),
			want:   offlineKeep,
		},
		{
			name:   "正好到 周期×2 的边界：还没超",
			device: dev(true, 239, 0, 0),
			want:   offlineKeep,
		},
		{
			name:   "超过 周期×2：第一次探测立刻发",
			device: dev(true, 241, 0, 0),
			want:   offlineProbe,
		},
		{
			name:   "探过一次但还不到间隔：等",
			device: dev(true, 300, 1, 5),
			want:   offlineKeep,
		},
		{
			name: "上次探测成功过（计数清零、刚刚探过）：按周期阈值节流，不再敲它",
			// probeAge=5s：距上次"成功探测"才 5 秒，远小于 probe_interval(15s) 与周期阈值(240s)
			device: dev(true, 300, 0, 5),
			want:   offlineKeep,
		},
		{
			name:   "上次探测成功过且已过周期阈值：再探一次确认",
			device: dev(true, 600, 0, 300),
			want:   offlineProbe,
		},
		{
			name:   "探过一次且过了间隔：再探",
			device: dev(true, 300, 1, 20),
			want:   offlineProbe,
		},
		{
			name:   "探满 3 次但宽限期没到：再等等",
			device: dev(true, 400, 3, 10),
			want:   offlineKeep,
		},
		{
			name:   "探满 3 次且过了宽限期：判离线",
			device: dev(true, 400, 3, 31),
			want:   offlineMark,
		},
		{
			name:   "探测后设备回话了（上报时间晚于探测）：清零并保持在线",
			device: dev(true, 300, 2, -60), // probeAge 负数 = 探测时间比"现在-300s"还早，见下
			want:   offlineReset,
		},
		{
			name:   "周期内恢复正常上报：清掉旧计数",
			device: dev(true, 100, 2, 200),
			want:   offlineReset,
		},
		{
			name:   "设备没上报周期：走兜底超时，超了才判离线",
			device: func() *store.Device { d := dev(true, 700, 0, 0); d.PeriodicInterval = 0; return d }(),
			want:   offlineMark,
		},
		{
			name:   "设备没上报周期但还没到兜底超时：不动",
			device: func() *store.Device { d := dev(true, 300, 0, 0); d.PeriodicInterval = 0; return d }(),
			want:   offlineKeep,
		},
		{
			name:   "没有 ConnectionRequestURL：没法探测，直接判离线",
			device: func() *store.Device { d := dev(true, 300, 0, 0); d.ConnRequestURL = ""; return d }(),
			want:   offlineMark,
		},
		{
			name:   "关掉探测：退回纯超时（超期即判离线）",
			device: dev(true, 300, 0, 0),
			mutate: func(p *offlinePolicy) { p.Enabled = false },
			want:   offlineMark,
		},
		{
			name:   "周期很大时用 -offline-probe-max 兜住",
			device: func() *store.Device { d := dev(true, 400, 0, 0); d.PeriodicInterval = 3600; return d }(),
			mutate: func(p *offlinePolicy) { p.Max = 5 * time.Minute },
			want:   offlineProbe,
		},
		{
			name:   "已经离线：只清探测进度",
			device: dev(false, 5000, 2, 100),
			want:   offlineReset,
		},
		{
			name:   "已经离线且没有进度：不动",
			device: dev(false, 5000, 0, 0),
			want:   offlineKeep,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := policy()
			if c.mutate != nil {
				c.mutate(&p)
			}
			// 「探测后设备回话了」：把探测时间推到上报之前
			if c.want == offlineReset && c.device.ProbeCount > 0 && c.device.ProbeAt.After(c.device.LastInformAt) {
				c.device.ProbeAt = c.device.LastInformAt.Add(-time.Second)
			}
			got := planOffline(c.device, t0, p)
			if got.Action != c.want {
				t.Fatalf("动作 = %s，期望 %s（理由：%s）", got.Action, c.want, got.Reason)
			}
			if got.Reason == "" {
				t.Error("应该带上理由，日志要用")
			}
		})
	}
}

// 出厂默认配置下的时间线：120 秒周期的设备，什么时候开始探测、什么时候判离线。
func TestPlanOfflineTimeline(t *testing.T) {
	p := policy() // factor=2 → 240s；3 次探测，间隔 15s，宽限 30s
	d := dev(true, 0, 0, 0)

	// 240 秒：还在周期内
	if got := planOffline(d, t0.Add(239*time.Second), p); got.Action != offlineKeep {
		t.Fatalf("239s 应该不动，实际 %s", got.Action)
	}
	// 241 秒：开始探测
	if got := planOffline(d, t0.Add(241*time.Second), p); got.Action != offlineProbe {
		t.Fatalf("241s 应该探测，实际 %s", got.Action)
	}
	// 探测 1 次后：15 秒内不再探，之后再探
	d.ProbeCount, d.ProbeAt = 1, t0.Add(241*time.Second)
	if got := planOffline(d, t0.Add(250*time.Second), p); got.Action != offlineKeep {
		t.Fatalf("探测间隔内应该等，实际 %s", got.Action)
	}
	if got := planOffline(d, t0.Add(257*time.Second), p); got.Action != offlineProbe {
		t.Fatalf("过了间隔应该再探，实际 %s", got.Action)
	}
	// 探满 3 次：宽限期（30s）内不判离线，过了才判
	d.ProbeCount, d.ProbeAt = 3, t0.Add(300*time.Second)
	if got := planOffline(d, t0.Add(320*time.Second), p); got.Action != offlineKeep {
		t.Fatalf("宽限期内应该等，实际 %s", got.Action)
	}
	if got := planOffline(d, t0.Add(331*time.Second), p); got.Action != offlineMark {
		t.Fatalf("过了宽限期应该判离线，实际 %s", got.Action)
	}
	// 判离线那一刻，设备总共"多活了" 240+3×15+30 ≈ 315 秒（周期的 2.6 倍）
}
