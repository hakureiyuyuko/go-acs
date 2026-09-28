package cwmp

import (
	"fmt"
	"strings"

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

// EnsureConnReqCredentials 保证设备上的 ConnectionRequest 账号密码就是我们要用的那套。
//
// 为什么必须我们写：真机（华为）对 ConnectionRequestURL 要求 HTTP Digest，
// 而 ConnectionRequestUsername/Password 它**不回读**（实测都是空串），
// 但这两个参数是**可写**的。不写进去，我们发出去的唤醒请求永远 401。
//
// 只在设备当前值与我们配置的不一致时才下发，避免每轮 BOOTSTRAP 都白写一次。
func (s *Server) EnsureConnReqCredentials(deviceID int64) {
	if !s.cfg.ConnReqEnabled || s.cfg.ConnReqUser == "" {
		return
	}
	// 进程内记一笔：这两个参数设备不回读，我们无从从库里确认它已经生效，
	// 只能自己记「已经下发过了」。重启后每台设备会再下发一次，代价可接受。
	s.connReqMu.Lock()
	if s.connReqDone == nil {
		s.connReqDone = map[int64]bool{}
	}
	if s.connReqDone[deviceID] {
		s.connReqMu.Unlock()
		return
	}
	s.connReqMu.Unlock()
	// 看库里已知的值 —— 不能只看本次 Inform 带的那几个参数（Inform 只带一小部分）
	params, err := s.store.ListParams(deviceID)
	if err != nil {
		return
	}
	curUser := lookupMgmt(params, pConnReqUser)
	curPass := lookupMgmt(params, pConnReqPass)
	if curUser == s.cfg.ConnReqUser && curPass == s.cfg.ConnReqPass {
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
	s.connReqMu.Lock()
	s.connReqDone[deviceID] = true
	s.connReqMu.Unlock()
	s.log.Info("已入队：把 ConnectionRequest 凭据写进设备（设备不回读，所以得我们自己 provision）",
		"device_id", deviceID, "user", s.cfg.ConnReqUser)
}

// WakeDevice 主动唤醒一台设备（发 Connection Request），返回给用户看的一句话结果。
//
// 设备收到之后会立刻回连 ACS 开一次会话（Inform 事件码 6 CONNECTION REQUEST），
// 排队中的任务就会被马上下发 —— 这就是「不用等下一次周期上报」的关键。
func (s *Server) WakeDevice(deviceID int64) (string, error) {
	if !s.cfg.ConnReqEnabled {
		return "", fmt.Errorf("主动唤醒功能已关闭（用 -connection-request 打开）")
	}
	d, err := s.store.GetDevice(deviceID)
	if err != nil {
		return "", fmt.Errorf("读设备失败: %w", err)
	}
	params, err := s.store.ListParams(deviceID)
	if err != nil {
		return "", fmt.Errorf("读参数失败: %w", err)
	}
	target := lookupMgmt(params, pConnReqURL)
	if target == "" {
		return "", fmt.Errorf("这台设备还没上报 ConnectionRequestURL（在它上报之前无法主动唤醒）")
	}

	user, pass := s.cfg.ConnReqUser, s.cfg.ConnReqPass
	if user == "" {
		// 没配置就试试设备上已有的（说不定是别的 ACS 配的）
		user, pass = lookupMgmt(params, pConnReqUser), lookupMgmt(params, pConnReqPass)
	}

	if err := SendConnectionRequest(target, user, pass, s.cfg.ConnReqTimeout); err != nil {
		s.log.Warn("主动唤醒失败", "device_id", deviceID, "url", target, "err", err)
		return "", fmt.Errorf("唤醒失败：%w", err)
	}
	s.log.Info("主动唤醒成功（设备会立刻回连开一次会话）",
		"device_id", deviceID, "serial", d.SerialNumber, "url", target)
	return "已主动唤醒设备，它应该马上回连（几秒内任务就会下发）", nil
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
