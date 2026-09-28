package cwmp

import (
	"context"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// 离线判定的规则（用户定的）：
//
//	设备按它自己上报的周期（PeriodicInformInterval）上报；
//	超过 周期×factor 还没上报 → 认为不按周期了，主动发 Connection Request 探测；
//	最多探 attempts 次（每次隔 interval）；
//	探完还是没回音、再等 grace → 标记离线。
//
// 为什么不直接用固定超时：TR-069 里设备断电是**不会通知** ACS 的，
// ACS 只有在设备主动连上来时才知道它活着。所以「多久没动静算掉线」应该跟设备
// 自己声明的周期挂钩 —— 5 分钟周期和 1 小时周期的设备，用同一个阈值一定有一边不合适。
//
// 探测本身也是验证：能连上说明设备还活着（可能只是周期上报出了问题），
// 那就别标离线；连不上（TCP 超时 / 拒绝）才是真掉线。

// offlinePolicy 是判定要用到的配置（从 config 拷过来，方便单测）。
type offlinePolicy struct {
	Enabled   bool          // 关掉就退回「纯超时」
	Factor    int           // 周期倍数
	Attempts  int           // 最多探测几次
	Interval  time.Duration // 两次探测的间隔
	Grace     time.Duration // 最后一次探测后再等多久才判离线
	Max       time.Duration // 周期×倍数的上限（0=不设）
	Fallback  time.Duration // 设备没上报周期时的兜底超时
	HasCRAddr bool          // 这台设备有没有 ConnectionRequestURL（没有就没法探测）
}

// offlineAction 是巡检对一台设备要做的动作。
type offlineAction int

const (
	offlineKeep  offlineAction = iota // 什么都不用做
	offlineProbe                      // 发一次 Connection Request 探测
	offlineMark                       // 标记离线
	offlineReset                      // 设备回话了/又在线了，清掉探测进度
)

func (a offlineAction) String() string {
	switch a {
	case offlineProbe:
		return "probe"
	case offlineMark:
		return "mark"
	case offlineReset:
		return "reset"
	default:
		return "keep"
	}
}

// offlineDecision 带上理由与时间信息，日志和界面都用得上。
type offlineDecision struct {
	Action   offlineAction
	Elapsed  time.Duration // 距最后一次上报多久了
	Deadline time.Duration // 本次判定用的周期阈值（周期×倍数）
	Reason   string
}

// planOffline 是纯函数：给一台设备的当前状态和当前时间，算出该做什么。
// 不碰数据库、不发网络请求，方便把边界用例都测到。
func planOffline(d *store.Device, now time.Time, p offlinePolicy) offlineDecision {
	elapsed := time.Duration(0)
	if !d.LastInformAt.IsZero() {
		elapsed = now.Sub(d.LastInformAt)
	}

	// 已经离线的设备：只把探测进度清干净（免得再上线时带着旧计数）
	if !d.Online {
		if d.ProbeCount > 0 {
			return offlineDecision{offlineReset, elapsed, 0, "设备已离线，清掉旧的探测进度"}
		}
		return offlineDecision{Action: offlineKeep, Elapsed: elapsed,
			Reason: "设备已离线，没有探测进度要清"}
	}

	// 探测期间设备回过话（上报时间晚于最后一次探测）：探测成功，清零，保持在线
	if d.ProbeCount > 0 && !d.LastInformAt.IsZero() && d.ProbeAt.Before(d.LastInformAt) {
		return offlineDecision{offlineReset, elapsed, 0, "探测后设备已上报，保持在线"}
	}

	// 设备没上报周期信息：退回固定超时（与旧行为一致）
	if d.PeriodicInterval <= 0 {
		if elapsed >= p.Fallback {
			return offlineDecision{offlineMark, elapsed, p.Fallback, "设备没上报周期，超过兜底超时"}
		}
		return offlineDecision{Action: offlineKeep, Elapsed: elapsed, Deadline: p.Fallback,
			Reason: "设备没上报周期，还没到兜底超时"}
	}

	deadline := time.Duration(p.Factor) * time.Duration(d.PeriodicInterval) * time.Second
	if p.Max > 0 && deadline > p.Max {
		deadline = p.Max
	}
	if elapsed < deadline {
		// 还在周期内：如果之前探过（比如上一轮网络抖动），现在恢复上报了就清零
		if d.ProbeCount > 0 {
			return offlineDecision{offlineReset, elapsed, deadline, "周期内已恢复上报"}
		}
		return offlineDecision{offlineKeep, elapsed, deadline, "周期内"}
	}

	// 超期了：没法探测就直接判离线（没有 ConnectionRequestURL，探也白探）
	if !p.Enabled || d.ConnRequestURL == "" {
		return offlineDecision{offlineMark, elapsed, deadline, "超期未上报，且无法主动探测"}
	}

	if d.ProbeCount >= p.Attempts {
		// 探测次数用完了，再等一个宽限期
		if d.ProbeAt.IsZero() || now.Sub(d.ProbeAt) >= p.Grace {
			return offlineDecision{offlineMark, elapsed, deadline, "探测次数用尽仍无回音"}
		}
		return offlineDecision{Action: offlineKeep, Elapsed: elapsed, Deadline: deadline,
			Reason: "探测次数用尽，等宽限期"}
	}

	// 该探测了：第一次立即探；之后按间隔节流。
	// 上次探测**成功**过（计数被清零）的设备是「活着但没按周期报」，节流放宽到周期阈值，
	// 免得每 15 秒去敲它一次；正在判定生死的（有失败计数）才按 probe_interval 快速连探。
	throttle := p.Interval
	if d.ProbeCount == 0 {
		throttle = deadline
	}
	if !d.ProbeAt.IsZero() && now.Sub(d.ProbeAt) < throttle {
		return offlineDecision{Action: offlineKeep, Elapsed: elapsed, Deadline: deadline,
			Reason: "距上次探测还没到间隔"}
	}
	return offlineDecision{offlineProbe, elapsed, deadline, "超期未上报，主动探测"}
}

// offlinePolicyFor 把运行配置转成判定用的策略（设备自身的状态在 planOffline 里看）。
func (s *Server) offlinePolicyFor() offlinePolicy {
	return offlinePolicy{
		Enabled:  s.cfg.OfflineProbe,
		Factor:   s.cfg.OfflineProbeFactor,
		Attempts: s.cfg.OfflineProbeAttempts,
		Interval: s.cfg.OfflineProbeInterval,
		Grace:    s.cfg.OfflineProbeGrace,
		Max:      s.cfg.OfflineProbeMax,
		Fallback: s.cfg.OfflineAfter,
	}
}

// SweepOffline 巡检一遍所有设备的在线状态（后台定时跑，见 StartJanitor）。
//
// 返回本次探测了几台、判离线几台，便于日志与验收断言。
func (s *Server) SweepOffline(ctx context.Context) (probed, offlined int) {
	devices, err := s.store.ListDevices()
	if err != nil {
		s.log.Warn("离线巡检：读设备列表失败", "err", err)
		return 0, 0
	}
	now := time.Now()
	policy := s.offlinePolicyFor()
	for _, d := range devices {
		if ctx.Err() != nil {
			return probed, offlined
		}
		dec := planOffline(d, now, policy)
		switch dec.Action {
		case offlineReset:
			if err := s.store.ResetProbe(d.ID); err != nil {
				s.log.Warn("离线巡检：清探测进度失败", "device_id", d.ID, "err", err)
			}
		case offlineProbe:
			// 探测本身也是「设备还活着」的最强证据：能连上就说明只是没按周期上报。
			//
			// 并发发出去：一台连不上的设备要等满 ConnReqTimeout（默认 10 秒），
			// 一批设备同时掉线时串行探测会把整个巡检拖垮（下一次巡检的 tick 直接被丢掉）。
			probed++
			attempt := d.ProbeCount + 1
			go func(dev *store.Device, attempt int, elapsed time.Duration) {
				err := s.connectionRequest(dev)
				at := time.Now()
				if err != nil {
					if recordErr := s.store.RecordProbe(dev.ID, at); recordErr != nil {
						s.log.Warn("离线巡检：记录探测次数失败", "device_id", dev.ID, "err", recordErr)
					}
					s.log.Info("离线探测无响应（设备没应答 Connection Request）",
						"device_id", dev.ID, "serial", dev.SerialNumber,
						"attempt", attempt, "of", s.cfg.OfflineProbeAttempts, "err", err)
					return
				}
				if recordErr := s.store.RecordProbeOK(dev.ID, at); recordErr != nil {
					s.log.Warn("离线巡检：记录探测结果失败", "device_id", dev.ID, "err", recordErr)
				}
				s.log.Info("离线探测成功（设备还活着，只是没按周期上报）",
					"device_id", dev.ID, "serial", dev.SerialNumber,
					"elapsed", elapsed.Round(time.Second).String())
			}(d, attempt, dec.Elapsed)
		case offlineMark:
			if err := s.store.MarkOffline(d.ID); err != nil {
				s.log.Warn("离线巡检：标记离线失败", "device_id", d.ID, "err", err)
				continue
			}
			offlined++
			s.log.Info("设备已标记离线",
				"device_id", d.ID, "serial", d.SerialNumber,
				"reason", dec.Reason,
				"elapsed", dec.Elapsed.Round(time.Second).String(),
				"threshold", dec.Deadline.Round(time.Second).String(),
				"probes", d.ProbeCount)
		}
	}
	return probed, offlined
}
