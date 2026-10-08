package cwmp

import (
	"context"
	"time"
)

// 任务派发：把「有任务排队」的设备主动叫起来干活。
//
// 这是这个 ACS 的一条原则：**凡是我们这边发起的动作**（采集概况、下发参数、诊断、
// 重启、刷新子设备…）都走 Connection Request 让设备立刻回连；**被动等设备周期性
// 上报只适用于设备自己发起的上报** —— 那时任务顺手就带下去了。
//
// 为什么要这么定：设备的周期可以长到几十分钟（真机上有 30 分钟的），干等的话
// 界面上点一下要等半小时才见效，看着就像坏了，而且我们根本不知道它什么时候会来。
//
// 判断条件（每一个都要过）：
//   - 这台设备还有 pending 任务；
//   - 设备在线（离线的不白叫，交给离线巡检去探）；
//   - 它现在**没有任务在跑**，也不是刚上报完（那就是正处在一次会话里：
//     我们发起的任务在这次会话里就会被接着下发，不用叫）；
//   - 距上次唤醒（人工 / 探测 / 派发都算）超过冷却时间，别把设备叫得停不下来。
func (s *Server) DispatchPendingTasks(ctx context.Context) {
	ids, err := s.store.DevicesWithPendingTasks()
	if err != nil {
		s.log.Warn("查待下发任务失败", "err", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	now := time.Now()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if s.wokenWithin(id, s.wakeCooldown, now) {
			continue
		}
		d, err := s.store.GetDevice(id)
		if err != nil || d == nil {
			continue
		}
		if !d.Online {
			continue
		}
		// 正在跑任务 → 设备就在会话里，别打断它（剩下的任务会被接着下发）
		if s.store.HasRunningTask(id) {
			continue
		}
		// 刚上报完（说明会话可能刚开始）→ 给这次会话一点时间把任务取走
		if s.wakeIdle > 0 && now.Sub(d.LastInformAt) < s.wakeIdle {
			continue
		}
		if err := s.connectionRequest(d); err != nil {
			// 设备不可达是常事（NAT 后面的 URL 打不通）——任务照样排队，
			// 等它下次上报或者冷却后再试，所以这里只记 debug。
			s.log.Debug("派发唤醒没成功，任务继续排队",
				"device_id", id, "url", d.ConnRequestURL, "err", err)
			continue
		}
		s.log.Info("有任务排队，主动唤醒设备",
			"device_id", id, "serial", d.SerialNumber, "url", d.ConnRequestURL)
	}
}

// wokenWithin 判断这台设备上次主动唤醒是不是还在冷却期内。
func (s *Server) wokenWithin(deviceID int64, cooldown time.Duration, now time.Time) bool {
	if cooldown <= 0 {
		return false
	}
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	at, ok := s.wakeAt[deviceID]
	return ok && now.Sub(at) < cooldown
}

// noteWake 记一次主动唤醒（成功失败都记）—— 冷却靠它，免得反复叫同一台设备。
func (s *Server) noteWake(deviceID int64, at time.Time) {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.wakeAt == nil {
		s.wakeAt = map[int64]time.Time{}
	}
	s.wakeAt[deviceID] = at
}
