package cwmp

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/store"
)

// ConnectionRequest 相关的参数名（TR-098）。TR-181 是 Device.ManagementServer.*，字段同名。
const (
	pConnReqURL  = "ConnectionRequestURL"
	pConnReqUser = "ConnectionRequestUsername"
	pConnReqPass = "ConnectionRequestPassword"
)

// connReqParam 拼出某个数据模型根下的 ManagementServer 参数全名。
func connReqParam(root, leaf string) string {
	if strings.HasPrefix(root, "Device.") {
		return "Device.ManagementServer." + leaf
	}
	return "InternetGatewayDevice.ManagementServer." + leaf
}

// lookupMgmt 从已采集的参数里按叶子名找 ManagementServer 下的值。
//
// 用例：设备把我们的 ConnectionRequestURL 上报在 Inform 里，而账号密码不一定有。
// 按后缀匹配，TR-098 / TR-181 两种路径都能命中。
func lookupMgmt(params []store.Param, leaf string) string {
	want := strings.ToLower("." + leaf)
	for _, p := range params {
		if strings.HasSuffix(strings.ToLower(p.Name), want) && !strings.HasSuffix(p.Name, ".") {
			return strings.TrimSpace(p.Value)
		}
	}
	return ""
}

// connReqTaskRetry 是「上一次写 ConnectionRequest 凭据失败后，隔多久再试一次」。
//
// 新设备接入时这一步必须自己走完（否则唤醒永远是 401），但也不能每轮 Inform 都刷一条任务，
// 所以失败后隔一段时间重试，并且最多试 connReqMaxAttempts 次。
const (
	connReqTaskRetry   = 10 * time.Minute
	connReqMaxAttempts = 3
)

// connReqState 是「设备上的 ConnectionRequest 凭据到底规范了没有」的判定结果。
type connReqState int

const (
	// connReqUnknown：历史里没有我们写 CR 凭据的记录（新设备，或者记录被裁掉了）
	connReqUnknown connReqState = iota
	// connReqSatisfied：写成功过，而且写的就是当前这套账号密码
	connReqSatisfied
	// connReqInFlight：已经有一条在排队 / 正在执行，等它出结果
	connReqInFlight
	// connReqRetryLater：上次失败了，还没到重试时间
	connReqRetryLater
	// connReqGivingUp：连着失败到上限了，别再刷任务（日志里会说明）
	connReqGivingUp
)

// EnsureConnReqCredentials 保证设备上的 ConnectionRequest 账号密码就是我们要用的那套。
//
// 为什么必须我们写：真机（华为）对 ConnectionRequestURL 要求 HTTP Digest，
// 而 ConnectionRequestPassword 设备**不回读**（实测回空串），但两个参数都是**可写**的。
// 不写进去，我们发出去的唤醒请求永远 401 —— 所以这是纳管流程的一部分，新设备接进来
// 在**同一次会话**里就会下发（见 onInform 里的调用点）。
//
// 判「有没有规范好」不能只看库里的值：密码压根读不回来，库里永远是空。
// 所以看**任务历史**里我们上一次写它的结果 —— 成功且写的就是当前这套才算数；
// 失败了隔一会儿重试（设备忙、临时拒绝都遇到过），重试到上限就停手并告警，
// 不把任务历史刷满。
func (s *Server) EnsureConnReqCredentials(deviceID int64) {
	if !s.cfg.ConnReqEnabled || s.cfg.ConnReqUser == "" {
		return
	}
	// 进程内快速路径：本进程已经确认过「设备上就是这套凭据」就不再查库
	s.connReqMu.Lock()
	if s.connReqDone == nil {
		s.connReqDone = map[int64]bool{}
	}
	done := s.connReqDone[deviceID]
	s.connReqMu.Unlock()
	if done {
		return
	}

	// 设备自己上报的用户名（这个能回读）跟我们不一致 → 不管历史如何都必须重写：
	// 凭据是设备侧真正生效的那份，跟「我们上次写过」不是一回事（运营商可能改过）。
	params, err := s.store.ListParams(deviceID)
	if err != nil {
		return
	}
	curUser := lookupMgmt(params, pConnReqUser)
	mismatch := curUser != "" && curUser != s.cfg.ConnReqUser
	if mismatch {
		s.log.Info("设备上的 ConnectionRequest 用户名与我们配置的不一致，重新下发",
			"device_id", deviceID, "device_user", curUser, "want", s.cfg.ConnReqUser)
	}

	state, attempts := s.connReqTaskState(deviceID)
	switch state {
	case connReqInFlight:
		// 已经在写了，等它出结果（不一致也先等这条落地）
		return
	case connReqSatisfied:
		if !mismatch {
			s.connReqMu.Lock()
			s.connReqDone[deviceID] = true
			s.connReqMu.Unlock()
			return
		}
	case connReqRetryLater:
		return
	case connReqGivingUp:
		s.log.Warn("写 ConnectionRequest 凭据连续失败，不再重试（该设备暂时无法被主动唤醒）",
			"device_id", deviceID, "attempts", attempts, "user", s.cfg.ConnReqUser)
		return
	}

	d, err := s.store.GetDevice(deviceID)
	if err != nil {
		return
	}
	vals := []ParamValue{
		{Name: connReqParam(d.DataModelRoot, pConnReqUser), Value: s.cfg.ConnReqUser, Type: "string"},
		{Name: connReqParam(d.DataModelRoot, pConnReqPass), Value: s.cfg.ConnReqPass, Type: "string"},
	}
	if _, err := s.EnqueueSetParameters(deviceID, vals); err != nil {
		s.log.Warn("下发 ConnectionRequest 凭据失败", "device_id", deviceID, "err", err)
		return
	}
	if attempts > 0 {
		s.log.Info("重试写入 ConnectionRequest 凭据（上次失败）",
			"device_id", deviceID, "attempt", attempts+1, "user", s.cfg.ConnReqUser)
	} else {
		s.log.Info("已入队：把 ConnectionRequest 凭据写进设备（设备不回读密码，所以得我们自己 provision）",
			"device_id", deviceID, "user", s.cfg.ConnReqUser)
	}
}

// connReqTaskState 看这台设备最近一次「写 ConnectionRequest 凭据」的任务怎么样了。
// 判断依据是任务历史而不是库里的参数值：密码设备不回读，库里永远是空串，
// 拿它当判据会把「已经写好了」一直误判成「还没写」。
//
// 返回状态与「已经失败过几次」。
func (s *Server) connReqTaskState(deviceID int64) (connReqState, int) {
	tasks, err := s.store.ListTasks(deviceID, 50)
	if err != nil {
		// 读不到历史就当没写过：宁可多写一次（幂等），也不要因为读库失败而让唤醒坏掉
		return connReqUnknown, 0
	}
	failed := 0
	for _, t := range tasks {
		if t.Kind != TaskSetParameterValues {
			continue
		}
		p, ok := decodeConnReqSet(t.Payload)
		if !ok {
			continue // 不是写 CR 凭据的那条
		}
		switch t.Status {
		case "done":
			if p.matches(s.cfg.ConnReqUser, s.cfg.ConnReqPass) {
				return connReqSatisfied, failed
			}
			// 写成功了，但写的是旧值（比如刚改过 connreq 配置）→ 当作没写过，重写
			return connReqUnknown, failed
		case "pending", "running":
			return connReqInFlight, failed
		default: // failed
			// 继续往下数：连着失败几次决定要不要停手
			failed++
			if failed == 1 && time.Since(t.CreatedAt) < s.connReqRetryInterval() {
				// 刚失败不久：先等着，别每轮 Inform 都刷任务
				return connReqRetryLater, failed
			}
		}
	}
	switch {
	case failed >= connReqMaxAttempts:
		return connReqGivingUp, failed
	case failed > 0:
		return connReqUnknown, failed
	}
	return connReqUnknown, 0
}

// connReqSet 是我们写进设备的那两个参数。
type connReqSet struct {
	user, pass string
}

// connReqRetryInterval 是失败重试间隔（测试里可以把 Server.connReqRetry 调小）。
func (s *Server) connReqRetryInterval() time.Duration {
	if s.connReqRetry > 0 {
		return s.connReqRetry
	}
	return connReqTaskRetry
}

// matches 判断这次写进去的是不是当前配置的这套。
func (c connReqSet) matches(user, pass string) bool {
	return c.user == user && c.pass == pass
}

// decodeConnReqSet 从 SetParameterValues 的 payload 里认出「写 ConnectionRequest 凭据」的任务。
//
// payload 是我们自己拼的 JSON，按参数名后缀匹配即可（TR-098 / TR-181 都能认）。
func decodeConnReqSet(payload string) (connReqSet, bool) {
	var p spvPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return connReqSet{}, false
	}
	var out connReqSet
	seen := false
	for _, v := range p.Values {
		low := strings.ToLower(v.Name)
		switch {
		case strings.HasSuffix(low, "."+strings.ToLower(pConnReqUser)):
			out.user = v.Value
			seen = true
		case strings.HasSuffix(low, "."+strings.ToLower(pConnReqPass)):
			out.pass = v.Value
			seen = true
		}
	}
	return out, seen
}

// WakeDevice 主动唤醒一台设备（发 Connection Request），返回给用户看的一句话结果。
//
// 设备收到之后会立刻回连 ACS 开一次会话（Inform 事件码 6 CONNECTION REQUEST），
// 排队中的任务就会被马上下发 —— 这就是「不用等下一次周期上报」的关键。
func (s *Server) WakeDevice(deviceID int64) (string, error) {
	d, err := s.store.GetDevice(deviceID)
	if err != nil {
		return "", fmt.Errorf("读设备失败: %w", err)
	}
	if err := s.connectionRequest(d); err != nil {
		s.log.Warn("主动唤醒失败", "device_id", deviceID, "url", d.ConnRequestURL, "err", err)
		return "", fmt.Errorf("唤醒失败：%w", err)
	}
	s.log.Info("主动唤醒成功（设备会立刻回连开一次会话）",
		"device_id", deviceID, "serial", d.SerialNumber, "url", d.ConnRequestURL)
	return "已主动唤醒设备，它应该马上回连（几秒内任务就会下发）", nil
}

// connectionRequest 给一台设备发一次 Connection Request（解析地址与凭据 → HTTP GET）。
//
// 两种场景共用：用户在界面上点「立即唤醒」，以及离线巡检里主动探测
// （见 offline.go：超期没上报时先探几次，探不通才判离线）。
// 调用方决定怎么记日志 —— 这里的同一个错误，在人工唤醒时是「唤醒失败」，
// 在离线巡检里是「探测无响应」。
func (s *Server) connectionRequest(d *store.Device) error {
	if !s.cfg.ConnReqEnabled {
		return fmt.Errorf("主动唤醒功能已关闭（用 -connection-request 打开）")
	}
	// 设备上报的 URL 落在 devices 表里；库里没有就从已采集参数里再找一次
	// （有些设备把 ConnectionRequestURL 放在 Inform 的参数列表里带上来）
	target := d.ConnRequestURL
	var params []store.Param
	if p, err := s.store.ListParams(d.ID); err == nil {
		params = p
		if target == "" {
			target = lookupMgmt(params, pConnReqURL)
		}
	}
	if target == "" {
		return fmt.Errorf("这台设备还没上报 ConnectionRequestURL（在它上报之前无法主动唤醒）")
	}

	user, pass := s.cfg.ConnReqUser, s.cfg.ConnReqPass
	if user == "" {
		// 没配置就试试设备上已有的（说不定是别的 ACS 配的）
		user, pass = lookupMgmt(params, pConnReqUser), lookupMgmt(params, pConnReqPass)
	}
	return SendConnectionRequest(target, user, pass, s.cfg.ConnReqTimeout)
}

// WakeDeviceQuiet 是给「顺手试一下唤醒」的场景用的：不关心结果，只记日志。
func (s *Server) WakeDeviceQuiet(deviceID int64) {
	if _, err := s.WakeDevice(deviceID); err != nil {
		s.log.Info("顺手唤醒没成功（不影响正事，任务仍会按周期上报下发）",
			"device_id", deviceID, "reason", err)
	}
}

// FetchWAN 给外部（Web/REST）用：采集 WAN 连接概况。
func (s *Server) FetchWAN(deviceID int64) error {
	_, err := s.EnqueueFetchWAN(deviceID)
	return err
}
