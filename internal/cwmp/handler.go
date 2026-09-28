// Package cwmp 实现 TR-069/CWMP 的报文编解码、会话管理与 HTTP 入口。
//
// 本文件（handler.go）是核心：把 CPE 发来的一串 HTTP POST（每个只带一个 RPC）
// 组织成一次「会话」，并按需下发任务。
package cwmp

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"acs/internal/store"
)

// 任务类型（存在 tasks.kind 里）。
const (
	TaskGetParameterValues = "GetParameterValues"
	TaskGetParameterNames  = "GetParameterNames"
	TaskSetParameterValues = "SetParameterValues"
	TaskGetRPCMethods      = "GetRPCMethods"
	TaskReboot             = "Reboot"

	// TaskDiagnostics 是一次 ping 诊断。它跟其它任务不同：
	// SetParameterValues 只是第一步（让设备开始跑），结果要等设备回报，
	// 可能跨越好几轮 Inform —— 所以下发后任务会一直保持 running。
	TaskDiagnostics = "Diagnostics"
)

// 任务载荷（JSON）。
type gpvPayload struct {
	Names []string `json:"names"`

	// VerifyTask / Verify：非 0 时表示这是一次「读回核对」。
	//
	// 真机实翻过：华为 HN8145X6N 对 WLANConfiguration.5.RadioEnabled 的写入
	// 返回了 SetParameterValuesResponse Status=0（说“我接受了”），但读回来值根本没变。
	// 如果不读回，界面就会显示“设置成功”，运维会以为 WiFi 已经开了。
	// 所以写完之后把期望值一并带上，读回来对不上就把原任务标为失败。
	VerifyTask int64        `json:"verify_task,omitempty"`
	Verify     []ParamValue `json:"verify,omitempty"`
	// Prev 是写入前的值，用于分辨「未生效」与「设备不回读」（见 spvPayload.Prev）
	Prev []ParamValue `json:"prev,omitempty"`
	// Deferred：这是一次推迟到「设备下一轮会话」的核对。
	// 区分它是因为两种时机的结论不同：
	//   - 同会话核对：对不上先不判错（可能只是还没生效）；
	//   - 下一轮会话核对：还对不上就是真的没生效。
	Deferred bool `json:"deferred,omitempty"`

	// DiagTask 非 0 时表示这是一次「读 ping 诊断结果」，拿到结果后据此更新对应任务。
	DiagTask int64 `json:"diag_task,omitempty"`
}

type gpnPayload struct {
	Path      string `json:"path"`
	NextLevel bool   `json:"next_level"`

	// ThenFetch：拿到参数名之后，自动再下发一条 GetParameterValues 把值取回来。
	//
	// 这是「先枚举再取值」的常规做法（因为不能保证 CPE 支持子树路径的 GetParameterValues，
	// 见 basicInfoNames 的注释），而且两步是在**同一个会话**里连着做的：
	// 我们在收到 GetParameterNamesResponse 的那个 HTTP 响应里就直接带上 GPV 请求，
	// 不用等设备下一次轮询。
	ThenFetch bool `json:"then_fetch,omitempty"`

	// Exclude：参数名里包含任一子串就跳过（例如不要抓 AssociatedDevice 这张大表）。
	Exclude []string `json:"exclude,omitempty"`
	// Include：非空时，只保留以其中任一项为**后缀**的参数名。
	// 用于“只要十来个关键字段”的场景（看板的 WiFi 概览），避免把整棵子树都拉回来。
	Include []string `json:"include,omitempty"`
	// SkipStore：枚举出来的名字不写库。
	// 自动探测（如 WiFi 概览）只用名字做一次取值，不需要把几百个子树节点名
	// 都塞进参数表把界面刷屏。
	SkipStore bool `json:"skip_store,omitempty"`
	// Max：取值名单的最大条数（0 = 不限制）。
	Max int `json:"max,omitempty"`

	// ProbeFTTR：这是一次「能力探测」。枚举完之后看看设备有没有 FTTR / 子设备对象，
	// 有就顺手把那个子树也拉了（同一个会话里接着做）。
	// 探测不到就什么都不做 —— 界面上的「FTTR 子设备」区块直接不显示。
	ProbeFTTR bool `json:"probe_fttr,omitempty"`
}

type spvPayload struct {
	Values       []ParamValue `json:"values"`
	ParameterKey string       `json:"parameter_key"`
	// Prev 是写入前设备上的值（从本地库取）。
	//
	// 为什么需要它：核对时要能分辨下面两种“读回是空的”情况 ——
	//   - 写入前就非空、写后空了 → 可能真没生效；
	//   - 写入前就是空、写后也是空 → 这个参数**设备不回读**（典型：WiFi 密码），
	//     属于「无法核对」而不是「未生效」。没有 Prev 就分不开，会把改密码误判为失败。
	Prev []ParamValue `json:"prev,omitempty"`
}

type rebootPayload struct {
	CommandKey string `json:"command_key"`
}

// ACS 自己告诉 CPE「我支持这些 RPC」。
// 只列本期真正实现了的，避免给 CPE 虚假承诺。
var supportedRPCs = []string{
	"GetRPCMethods",
	"GetParameterValues",
	"GetParameterNames",
	"SetParameterValues",
	"Reboot",
}

// Config 是 CWMP 服务端的运行参数。
type Config struct {
	Path                string
	User                string // 为空表示不校验 CPE 的账号
	Password            string
	SessionTimeout      time.Duration // 会话空闲多久算超时
	LockWait            time.Duration // 同一会话并发请求最多等多久
	AutoFetchDeviceInfo bool          // Inform 后是否自动去取设备基本信息
	AutoFetchWiFi       bool          // 首次纳管/BOOTSTRAP 时是否自动采集无线概况（看板用）
	ProbeCapabilities   bool          // 首次纳管时是否探测设备能力（如有没有 FTTR 子设备）
	MaxBodyBytes        int64
	LogRawSOAP          bool
	OfflineAfter        time.Duration // 超过多久没上报就算离线

	// 主时唤醒（Connection Request）：给设备发一个 HTTP GET，让它立刻回连开一次会话，
	// 于是排队的任务不用等下一次周期上报。
	//
	// 难点在认证：真机的 ConnectionRequestURL 要 HTTP Digest，而
	// ConnectionRequestUsername/Password 两个参数设备**不回读**（实测华为返回空串），
	// 所以必须由我们自己 provision（它们是可写的）—— 不写就永远只能拿到 401。
	ConnReqEnabled bool
	ConnReqUser    string
	ConnReqPass    string
	ConnReqTimeout time.Duration

	// MaxParamsPerRequest 是单次 GetParameterValues 最多带上多少个参数名。
	//
	// 为什么必须分批：真机实测（华为 HN8145X6N）一次最多只回 256 个参数，
	// 请求 376 个也只回 256 个，**超出的部分静默丢弃、不报错**。
	// 分批之后每条任务都会在**同一个会话**里依次下发，不会额外多等一次设备轮询。
	MaxParamsPerRequest int
}

// defaultMaxParamsPerRequest 是本 ACS 单次 GPV 默认可带的参数名个数。
// 取 200：既留在真机上验证过的 256 上限之内，又不会把请求切得太碎。
const defaultMaxParamsPerRequest = 200

// Server 是 ACS 的 CWMP 端点。
type Server struct {
	store *store.Store
	cfg   Config
	log   *slog.Logger
	sess  *sessionManager

	// ConnectionRequest 凭据是否已下发过（这两个参数设备不回读，只能自己记）
	connReqMu   sync.Mutex
	connReqDone map[int64]bool
}

// NewServer 构造 CWMP 服务端。
func NewServer(st *store.Store, cfg Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if cfg.SessionTimeout <= 0 {
		cfg.SessionTimeout = 60 * time.Second
	}
	if cfg.LockWait <= 0 {
		cfg.LockWait = 30 * time.Second
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 4 << 20
	}
	if cfg.OfflineAfter <= 0 {
		cfg.OfflineAfter = 10 * time.Minute
	}
	return &Server{
		store: st,
		cfg:   cfg,
		log:   log,
		sess:  newSessionManager(cfg.SessionTimeout),
	}
}

// StartJanitor 起后台清理：超时会话、离线判定。
func (s *Server) StartJanitor(ctx context.Context) {
	go s.sess.janitor(ctx, 30*time.Second)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := s.store.MarkStaleOffline(s.cfg.OfflineAfter); err == nil && n > 0 {
					s.log.Info("设备超时未上报，已标记离线", "count", n)
				}
				// 诊断收尾：设备不支持 ping、或者干脆没回报时，
				// 不能让任务永远挂着 running。
				if n, err := s.store.FailStaleTasks(TaskDiagnostics, 5*time.Minute,
					"设备未在 5 分钟内回报诊断结果（可能不支持 ping 诊断）"); err == nil && n > 0 {
					s.log.Warn("诊断超时未回报，已判为失败", "count", n)
				}
			}
		}
	}()
}

// RequestRefresh 给外部（Web/REST）用：让某台设备重新上报名单里的基本信息。
func (s *Server) RequestRefresh(deviceID int64) error {
	_, err := s.EnqueueFetchDeviceInfo(deviceID)
	return err
}

// FetchSubtree 给外部（Web/REST）用：枚举某个参数子树下的所有参数并把值取回来。
func (s *Server) FetchSubtree(deviceID int64, path string, exclude []string, max int) error {
	_, err := s.EnqueueFetchSubtree(deviceID, path, exclude, max)
	return err
}

// FetchNames 给外部（Web/REST）用：只枚举参数名，不取值（浏览参数树）。
func (s *Server) FetchNames(deviceID int64, path string, nextLevel bool) error {
	_, err := s.EnqueueFetchNames(deviceID, path, nextLevel)
	return err
}

// FetchWiFi 给外部（Web/REST）用：采集无线概况（看板上的 2.4G/5G 那一栏）。
func (s *Server) FetchWiFi(deviceID int64) error {
	_, err := s.EnqueueFetchWiFi(deviceID)
	return err
}

// EnqueueFetchDeviceInfo 入队一条「取设备基本信息」的任务。
func (s *Server) EnqueueFetchDeviceInfo(deviceID int64) (int64, error) {
	d, err := s.store.GetDevice(deviceID)
	if err != nil {
		return 0, err
	}
	names := basicInfoNames(d.DataModelRoot)
	payload, _ := json.Marshal(gpvPayload{Names: names})
	id, created, err := s.store.EnqueueTaskIfAbsent(&store.Task{
		DeviceID: deviceID,
		Kind:     TaskGetParameterValues,
		Payload:  string(payload),
	})
	if err != nil {
		return 0, err
	}
	if created {
		s.log.Info("已入队：取设备基本信息", "device_id", deviceID, "names", len(names))
	}
	return id, nil
}

// basicInfoSuffixes 是「最基本的设备信息」在数据模型里的相对路径。
var basicInfoSuffixes = []string{
	"DeviceInfo.Manufacturer",
	"DeviceInfo.ManufacturerOUI",
	"DeviceInfo.ModelName",
	"DeviceInfo.Description",
	"DeviceInfo.ProductClass",
	"DeviceInfo.SerialNumber",
	"DeviceInfo.HardwareVersion",
	"DeviceInfo.SoftwareVersion",
	"DeviceInfo.SpecVersion",
	"DeviceInfo.ProvisioningCode",
	"DeviceInfo.UpTime",
	"ManagementServer.ConnectionRequestURL",
	// 主时唤醒（Connection Request）要用它们；设备不一定回读明文，取不到就只试无认证的 GET
	"ManagementServer.ConnectionRequestUsername",
	"ManagementServer.ConnectionRequestPassword",
	"ManagementServer.PeriodicInformInterval",
	"ManagementServer.ParameterKey",
}

// diagPayload 是一次 ping 诊断的载荷。
type diagPayload struct {
	Host   string `json:"host"`
	Count  int    `json:"count"`
	Prefix string `json:"prefix"` // 诊断对象前缀（以 "." 结尾）
}

// diagnosticsPrefix 返回本设备上 ping 诊断对象的前缀。
//
// TR-098 是 InternetGatewayDevice.IPPingDiagnostics.，
// TR-181 是 Device.IP.Diagnostics.IPPing.（字段名两边完全一样）。
// 按设备自报的数据模型根来选 —— 真机上两边都探测过是标准对象。
func diagnosticsPrefix(root string) string {
	if strings.HasPrefix(root, "Device.") {
		return "Device.IP.Diagnostics.IPPing."
	}
	return "InternetGatewayDevice.IPPingDiagnostics."
}

// diagnosticsReadFields 是诊断结果里要读回来的字段（都是只读的）。
var diagnosticsReadFields = []string{
	"DiagnosticsState",
	"SuccessCount",
	"FailureCount",
	"MinimumResponseTime",
	"AverageResponseTime",
	"MaximumResponseTime",
}

// EnqueueDiagnostics 入队一次 ping 诊断。
func (s *Server) EnqueueDiagnostics(deviceID int64, host string, count int) (int64, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return 0, fmt.Errorf("请填要诊断的 IP 或域名")
	}
	// 这串内容会直接进 SOAP 报文，限制字符集（不光是防注入，也能早点提示填错）
	if len(host) > 253 || strings.ContainsAny(host, " \t\r\n<>&\"'") {
		return 0, fmt.Errorf("目标格式不对：%q", host)
	}
	if count <= 0 || count > 20 {
		count = 4
	}
	d, err := s.store.GetDevice(deviceID)
	if err != nil {
		return 0, err
	}
	if open, err := s.store.FindOpenTask(deviceID, TaskDiagnostics); err == nil && open != nil {
		return 0, fmt.Errorf("已经有一次诊断在排队或进行中（任务 #%d），等它结束再说", open.ID)
	}
	payload, _ := json.Marshal(diagPayload{
		Host:   host,
		Count:  count,
		Prefix: diagnosticsPrefix(d.DataModelRoot),
	})
	id, err := s.store.EnqueueTask(&store.Task{
		DeviceID:   deviceID,
		Kind:       TaskDiagnostics,
		Payload:    string(payload),
		CommandKey: newRPCID(),
	})
	if err == nil {
		s.log.Info("已入队：ping 诊断", "device_id", deviceID, "host", host, "count", count)
	}
	return id, err
}

// Diagnose 给外部（Web/REST）用。
func (s *Server) Diagnose(deviceID int64, host string, count int) error {
	_, err := s.EnqueueDiagnostics(deviceID, host, count)
	return err
}

// fttrProbeCandidates 是「装子设备 / 子光猫」的对象前缀（小写用于比较）。
//
// 这件事没有统一标准，所以列候选、探测到哪个用哪个；一个都没探测到就把整个区块藏起来：
//   - 华为 FTTR：InternetGatewayDevice.X_HW_APDevice.（实测过，就是子光猫表）
//   - 标准 TR-181 Multi-AP（Wi-Fi Data Elements）：Device.WiFi.DataElements.Network.
//
// 只认顶层对象（纳管时枚举的就是顶层），更深层的子设备对象暂不支持。
var fttrProbeCandidates = []string{
	"internetgatewaydevice.x_hw_apdevice.",
	"device.wifi.dataelements.network.",
}

// detectFTTRPrefix 从一批参数名里找出 FTTR / 子设备对象的顶层前缀。
// 返回的是**设备自己的拼法**（前 len(候选) 个字符），直接用去枚举不会因大小写而失败。
func detectFTTRPrefix(names []string) (string, bool) {
	for _, n := range names {
		low := strings.ToLower(n)
		for _, c := range fttrProbeCandidates {
			if strings.HasPrefix(low, c) {
				return n[:len(c)], true
			}
		}
	}
	return "", false
}

// EnqueueProbeCapabilities 入队一条「看看设备有什么能力」的探测任务。
// 只枚举顶层对象，不改参数、不下发任何东西。
func (s *Server) EnqueueProbeCapabilities(deviceID int64, root string) (int64, error) {
	if root == "" {
		root = "InternetGatewayDevice."
	}
	return s.enqueueSubtree(deviceID, gpnPayload{
		Path:      root,
		NextLevel: true,
		ProbeFTTR: true,
	})
}

// EnqueueFetchSubtree 入队一条「枚举某个子树下的所有参数，再把值取回来」的任务。
//
// 为什么不直接下发子树路径给 GetParameterValues：有的实现不支持部分路径
// （genieacs-sim 会直接崩），所以走「GetParameterNames 枚举 + GetParameterValues 取值」
// 这条处处都认的路。
func (s *Server) EnqueueFetchSubtree(deviceID int64, path string, exclude []string, max int) (int64, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, fmt.Errorf("参数路径不能为空")
	}
	return s.enqueueSubtree(deviceID, gpnPayload{
		Path:      path,
		ThenFetch: true,
		Exclude:   exclude,
		Max:       max,
	})
}

// EnqueueFetchNames 入队一条「只枚举参数名，不取值」的任务。
//
// 用途：**浏览参数树**。先把某层有哪些对象列出来（nextLevel=true 只看直接子节点），
// 再决定往哪几个子树里钻 —— 比盲猜路径实用得多（真机上猜错一次就是一整轮上报周期）。
func (s *Server) EnqueueFetchNames(deviceID int64, path string, nextLevel bool) (int64, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, fmt.Errorf("参数路径不能为空（要列根下一层可以用 InternetGatewayDevice.）")
	}
	return s.enqueueSubtree(deviceID, gpnPayload{
		Path:      path,
		NextLevel: nextLevel,
	})
}

func (s *Server) enqueueSubtree(deviceID int64, p gpnPayload) (int64, error) {
	payload, _ := json.Marshal(p)
	id, err := s.store.EnqueueTask(&store.Task{
		DeviceID: deviceID,
		Kind:     TaskGetParameterNames,
		Payload:  string(payload),
	})
	if err == nil {
		s.log.Info("已入队：枚举参数子树",
			"device_id", deviceID, "path", p.Path,
			"include", len(p.Include), "exclude", p.Exclude)
	}
	return id, err
}

// wifiSummarySuffixes 是「看板 WiFi 概览」需要的那几个字段（TR-098 与 TR-181 两套命名都列上）。
// 有了它就不用把 400 多个 WLAN 参数全拉回来，只取摘要，轻很多。
var wifiSummarySuffixes = []string{
	".SSID",              // 名称
	".Enable",            // 服务开关
	".RadioEnabled",      // 射频开关（厂商私有，很常见）
	".Status",            // Up / Down / Disabled
	".Channel",           // 当前信道
	".AutoChannelEnable", // 自动信道
	".Standard",          // 无线标准
	".X_HW_Standard",     // 厂商私有的标准
	".BSSID",
	".BeaconType", // 认证类型
	".WPAAuthenticationMode",
	".WPAEncryptionModes",
	".X_HW_WPAand11iAuthenticationMode",
	".X_HW_WPAand11iEncryptionModes",
	".X_HW_RFBand",                     // 频段（厂商私有）
	".OperatingFrequencyBand",          // 频段（TR-181）
	".TotalAssociations",               // 已连终端数
	".AssociatedDeviceNumberOfEntries", // 已连终端数（TR-181）
	".SSIDAdvertisementEnabled",        // 是否广播 SSID
	// 下面几个只读参数不参与展示，但能给编辑表单提供**下拉框候选值**：
	// 信道可选值、发射功率可选值、支持的标准。这比在界面里写死列表靠谱。
	".PossibleChannels",
	".TransmitPowerSupported",
	".X_HW_SupportedStandards",
	// 可编辑的字段：密码与发射功率（读回来通常是空/只读，但写是有效的）
	".KeyPassphrase",
	".TransmitPower",
	".IEEE11iEncryptionModes",
	".IEEE11iAuthenticationMode",
}

// wifiSubtreePath 给出无线参数的子树路径。
// 数据模型根未知时先按 TR-098 猜一个 —— 枚举不到东西不会报错，只是看板那栏空着。
func wifiSubtreePath(root string) string {
	if root == "Device." {
		return "Device.WiFi."
	}
	return root + "LANDevice.1.WLANConfiguration."
}

// wanSummarySuffixes 是 WAN 连接概况要取的字段。
//
// 与 WiFi 同样的做法：先枚举子树拿到真实实例号，再按后缀白名单只取十几个字段，
// 不把整棵 WAN 子树（真机上有 458 个参数）拉回来。
var wanSummarySuffixes = []string{
	".Name",
	".Enable",
	".ConnectionStatus",
	".ConnectionType",
	".PossibleConnectionTypes",
	".ExternalIPAddress",
	".SubnetMask",
	".DefaultGateway",
	".MACAddress",
	".AddressingType",
	".NATEnabled",
	".Uptime",
	".LastConnectionError",
	".DNSEnabled",
	".DNSServers",
	".MaxMTUSize",
	// 厂商私有（真机上就是这两个在说“这条是 INTERNET / TR069”、VLAN 是多少）
	".X_HW_VLAN",
	".X_HW_SERVICELIST",
}

// wanSubtreePath 返回 WAN 连接对象的路径。
func wanSubtreePath(root string) string {
	if strings.HasPrefix(root, "Device.") {
		return "Device.PPP.Interface."
	}
	return "InternetGatewayDevice.WANDevice."
}

// EnqueueFetchWAN 入队一条「采集 WAN 连接概况」的任务。
func (s *Server) EnqueueFetchWAN(deviceID int64) (int64, error) {
	d, err := s.store.GetDevice(deviceID)
	if err != nil {
		return 0, err
	}
	return s.enqueueSubtree(deviceID, gpnPayload{
		Path:      wanSubtreePath(d.DataModelRoot),
		ThenFetch: true,
		Include:   wanSummarySuffixes,
		SkipStore: true,
	})
}

// EnqueueFetchWiFi 入队一条「采集无线概况」的任务。
//
// 走「枚举 + 按后缀白名单取值」：枚举能把真实的实例号圈出来（真机上 2.4G 是 1、5G 是 5，
// 不是 1 和 2，写死就会读空），白名单保证只拉十几个摘要字段而不是四百多个。
// SkipStore 让枚举出来的几百个名字不写库，免得把参数表刷屏。
func (s *Server) EnqueueFetchWiFi(deviceID int64) (int64, error) {
	d, err := s.store.GetDevice(deviceID)
	if err != nil {
		return 0, err
	}
	return s.enqueueSubtree(deviceID, gpnPayload{
		Path:      wifiSubtreePath(d.DataModelRoot),
		ThenFetch: true,
		Include:   wifiSummarySuffixes,
		SkipStore: true,
	})
}

// enqueueGPVDivided 把一批参数名**分批**入队成若干条 GetParameterValues 任务。
//
// 必须分批的原因见 Config.MaxParamsPerRequest 的注释（真机单次 256 上限、超出静默丢弃）。
// 分批不会变慢：这些任务会被 dispatchNextTask 在**同一个会话**里依次下发。
// 返回入队的批次数。
func (s *Server) enqueueGPVDivided(deviceID int64, names []string) (int, error) {
	if len(names) == 0 {
		return 0, nil
	}
	max := s.cfg.MaxParamsPerRequest
	if max <= 0 {
		max = defaultMaxParamsPerRequest
	}
	batches := 0
	for start := 0; start < len(names); start += max {
		end := start + max
		if end > len(names) {
			end = len(names)
		}
		payload, _ := json.Marshal(gpvPayload{Names: names[start:end]})
		if _, err := s.store.EnqueueTask(&store.Task{
			DeviceID: deviceID,
			Kind:     TaskGetParameterValues,
			Payload:  string(payload),
		}); err != nil {
			return batches, err
		}
		batches++
	}
	return batches, nil
}

// EnqueueSetParameters 入队一条设置参数的任务。
//
// ParameterKey 由我们生成、CPE 必须在响应里原样回传，用来把“哪一次设置”对上。
// 同时记下写入前的值（用于之后核对时区分「未生效」与「设备不回读」）。
func (s *Server) EnqueueSetParameters(deviceID int64, vals []ParamValue) (int64, error) {
	if len(vals) == 0 {
		return 0, fmt.Errorf("没有要设置的参数")
	}
	prev := make([]ParamValue, 0, len(vals))
	for _, v := range vals {
		pv := ParamValue{Name: v.Name}
		if p, ok, err := s.store.GetParam(deviceID, v.Name); err == nil && ok {
			pv.Value = p.Value
			pv.Type = p.ValueType
		}
		prev = append(prev, pv)
	}

	key := newRPCID()
	payload, _ := json.Marshal(spvPayload{Values: vals, ParameterKey: key, Prev: prev})
	id, err := s.store.EnqueueTask(&store.Task{
		DeviceID:   deviceID,
		Kind:       TaskSetParameterValues,
		Payload:    string(payload),
		CommandKey: key,
	})
	if err == nil {
		s.log.Info("已入队：设置参数",
			"device_id", deviceID, "count", len(vals), "parameter_key", key)
	}
	return id, err
}

// SetParameters 给外部（Web/REST）用。
// 用 store.Param 传参只是为了复用一个已有类型（Name/Value/ValueType 正好够用）。
func (s *Server) SetParameters(deviceID int64, params []store.Param) error {
	vals := make([]ParamValue, 0, len(params))
	for _, p := range params {
		vals = append(vals, ParamValue{Name: p.Name, Value: p.Value, Type: p.ValueType})
	}
	_, err := s.EnqueueSetParameters(deviceID, vals)
	return err
}

// filterLeafNames 从枚举结果里挑出可以取值的叶子参数名。
//   - 对象节点（以 "." 结尾）跳过，因为它们不是叶子；
//   - include 非空时，只保留以其中任一项为后缀的名字；
//   - 名字包含 exclude 里任一子串的跳过；
//   - 超过 max 就截断。
func filterLeafNames(infos []ParamInfo, include, exclude []string, max int) []string {
	out := make([]string, 0, len(infos))
	for _, in := range infos {
		name := strings.TrimSpace(in.Name)
		if name == "" || strings.HasSuffix(name, ".") {
			continue
		}
		lower := strings.ToLower(name)
		if len(include) > 0 {
			keep := false
			for _, inc := range include {
				if inc != "" && strings.HasSuffix(lower, strings.ToLower(inc)) {
					keep = true
					break
				}
			}
			if !keep {
				continue
			}
		}
		skip := false
		for _, e := range exclude {
			if e != "" && strings.Contains(name, e) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		out = append(out, name)
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out
}

// basicInfoNames 给出要下发的完整参数名列表。
//
// 这里**故意不发子树路径**（如 "Device.DeviceInfo."），而是发一个个显式参数名。
// 原因是用独立的 CPE 实现（GenieACS 官方的 genieacs-sim）交叉验证时发现：
// 它的 GetParameterValues 不支持部分路径，收到子树路径会直接崩。真机大多支持，
// 但显式参数名是兼容性最好的写法（GenieACS 自己也是这么做的）：
// CPE 只会把存在的参数回给我们，不存在的不返回 —— 这本身就完成了数据模型根的探测。
//
// 根未知时把两种命名的同一批参数都发过去，让设备自己回答它支持哪一套
// —— 这是「探测」而不是「假设」。
func basicInfoNames(root string) []string {
	roots := []string{root}
	if root == "" {
		roots = []string{"InternetGatewayDevice.", "Device."}
	}
	out := make([]string, 0, len(roots)*len(basicInfoSuffixes))
	for _, r := range roots {
		for _, sfx := range basicInfoSuffixes {
			out = append(out, r+sfx)
		}
	}
	return out
}

// ---------- HTTP 入口 ----------

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="acs"`)
		http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
		return
	}

	sess := s.sess.acquire(r)
	if !sess.tryLock(s.cfg.LockWait) {
		// 同一会话并发进来（CPE 不该这么干）：让它稍后重试
		s.log.Warn("会话被占用，要求 CPE 稍后重试", "session", sess.ID)
		w.Header().Set("Retry-After", "10")
		http.Error(w, "503 Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	defer sess.unlock()
	defer s.sess.touch(sess)

	// 把会话 ID 用 cookie 回给 CPE：支持 cookie 的设备后续请求就能精确对上会话
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: sess.ID, Path: "/"})
	w.Header().Set("Server", "light-acs")
	w.Header().Set("SOAPServer", "light-acs")

	body, err := readBody(r, s.cfg.MaxBodyBytes)
	if err != nil {
		s.log.Warn("读取请求体失败", "session", sess.ID, "err", err)
		http.Error(w, "400 Bad Request", http.StatusBadRequest)
		return
	}
	if s.cfg.LogRawSOAP && len(body) > 0 {
		s.log.Debug("CPE 报文", "session", sess.ID, "body", string(body))
	}

	// 空 body = CPE 说「我准备好了，有活就发给我」
	if len(bytes.TrimSpace(body)) == 0 {
		s.handleCPEReady(w, sess)
		return
	}

	root, err := ParseXML(bytes.NewReader(body))
	if err != nil {
		s.log.Warn("XML 解析失败", "session", sess.ID, "err", err)
		s.writeEnvelope(w, sess, "", FaultBody(FaultInvalidArguments, "无法解析报文: "+err.Error()))
		return
	}
	env, err := ParseEnvelope(root)
	if err != nil {
		s.log.Warn("信封结构异常", "session", sess.ID, "err", err)
		s.writeEnvelope(w, sess, "", FaultBody(FaultInvalidArguments, err.Error()))
		return
	}
	if env.CWMPNS != "" {
		sess.cwmpNS = env.CWMPNS
	}

	// body 非空但里面没有 RPC —— 有些设备会发一个空信封来「要活」
	if env.Method == nil {
		s.handleCPEReady(w, sess)
		return
	}
	s.dispatch(w, r, sess, env)
}

func (s *Server) authorized(r *http.Request) bool {
	if s.cfg.User == "" {
		return true
	}
	u, p, ok := r.BasicAuth()
	if !ok {
		return false
	}
	uOK := subtle.ConstantTimeCompare([]byte(u), []byte(s.cfg.User)) == 1
	pOK := subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.Password)) == 1
	return uOK && pOK
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, sess *Session, env *Envelope) {
	m := env.Method
	switch m.Local {
	case "Inform":
		s.onInform(w, r, sess, env, m)
	case "Fault":
		s.onFault(w, sess, env, m)
	case "GetParameterValuesResponse":
		s.onGetParameterValuesResponse(w, sess, env, m)
	case "GetParameterNamesResponse":
		s.onGetParameterNamesResponse(w, sess, env, m)
	case "SetParameterValuesResponse":
		s.onSimpleResponse(w, sess, m, "设置参数成功")
	case "GetRPCMethods":
		s.writeEnvelope(w, sess, env.ID, GetRPCMethodsResponseBody(supportedRPCs))
	case "TransferComplete", "AutonomousTransferComplete", "RequestDownload", "Kicked":
		s.onMiscCPERequest(w, sess, env, m)
	default:
		if strings.HasSuffix(m.Local, "Response") {
			s.onSimpleResponse(w, sess, m, "已收到响应")
			return
		}
		s.log.Warn("不支持的 RPC", "session", sess.ID, "method", m.Local)
		s.writeEnvelope(w, sess, env.ID, FaultBody(FaultMethodNotSupported, ""))
	}
}

// onInform 处理上报：这是「纳管」的入口。
func (s *Server) onInform(w http.ResponseWriter, r *http.Request, sess *Session, env *Envelope, m *Node) {
	inf, err := ParseInform(m)
	if err != nil {
		s.log.Warn("Inform 解析失败", "session", sess.ID, "err", err)
		s.writeEnvelope(w, sess, env.ID, FaultBody(FaultInvalidArguments, err.Error()))
		return
	}

	// 新会话开始：清掉上一轮遗留的「推迟任务」标记（防御性，正常已在 endSession 清过）
	sess.clearDeferred()

	root := detectRoot(inf.Params)
	fields := deviceFieldsFromParams(inf.Params)

	dev := &store.Device{
		OUI:              inf.DeviceID.OUI,
		ProductClass:     inf.DeviceID.ProductClass,
		SerialNumber:     inf.DeviceID.SerialNumber,
		Manufacturer:     pick(fields.Manufacturer, inf.DeviceID.Manufacturer),
		ModelName:        fields.ModelName,
		DataModelRoot:    root,
		SoftwareVersion:  fields.SoftwareVersion,
		HardwareVersion:  fields.HardwareVersion,
		SpecVersion:      fields.SpecVersion,
		ProvisioningCode: fields.ProvisioningCode,
		ExternalIP:       fields.ExternalIP,
		ConnRequestURL:   fields.ConnRequestURL,
		PeriodicInterval: fields.PeriodicInterval,
		UserAgent:        r.UserAgent(),
		SourceIP:         clientIP(r),
		LastEvents:       strings.Join(inf.EventCodes(), ", "),
	}
	if inf.HasEvent("0 BOOTSTRAP") || inf.HasEvent("1 BOOT") || inf.HasEvent("M Reboot") {
		dev.LastBootAt = time.Now()
	}

	deviceID, created, err := s.store.UpsertDevice(dev)
	if err != nil {
		s.log.Error("写入设备失败", "serial", inf.DeviceID.SerialNumber, "err", err)
		s.writeEnvelope(w, sess, env.ID, FaultBody(FaultInternalError, "服务端存储失败"))
		return
	}
	s.sess.bindDevice(sess, deviceID)

	// 参数落库（Inform 只带部分参数，不能当全量）
	if len(inf.Params) > 0 {
		if err := s.store.UpsertParams(deviceID, toStoreParams(inf.Params), "inform"); err != nil {
			s.log.Warn("写入参数失败", "device_id", deviceID, "err", err)
		}
	}

	// 厂商把根节点名大小写写错的情况（真机见过：InternetGateWayDevice.），
	// 参数已原样存库，这里只提醒一句。
	if typos := findRootTypo(root, inf.Params); len(typos) > 0 {
		s.log.Warn("参数名根前缀大小写与标准不一致（按原文存库，未改写）",
			"device_id", deviceID, "names", typos)
	}

	// 事件流水
	_ = s.store.InsertInform(&store.InformRecord{
		DeviceID:    deviceID,
		Events:      strings.Join(inf.EventCodes(), ", "),
		CommandKey:  inf.CommandKeyOfEvent("7 TRANSFER COMPLETE"),
		RetryCount:  inf.RetryCount,
		CurrentTime: inf.CurrentTime,
		SourceIP:    clientIP(r),
		ParamCount:  len(inf.Params),
	})

	s.log.Info("收到 Inform",
		"device_id", deviceID,
		"serial", dev.SerialNumber,
		"oui", dev.OUI,
		"events", dev.LastEvents,
		"params", len(inf.Params),
		"new_device", created,
		"root", root)

	// 第一次见面 / 每轮 BOOTSTRAP：把「最基本的设备信息」取回来
	if s.cfg.AutoFetchDeviceInfo {
		if created || inf.HasEvent("0 BOOTSTRAP") || root == "" || !s.hasDeviceInfo(deviceID) {
			if _, err := s.EnqueueFetchDeviceInfo(deviceID); err != nil {
				s.log.Warn("入队取设备信息失败", "device_id", deviceID, "err", err)
			}
		}
	}

	// 主动唤醒的前提：设备上的 ConnectionRequest 账号密码得是我们知道的那套。
	// 设备不回读这两个参数（实测），所以得我们自己 provision。
	if s.cfg.ConnReqEnabled {
		s.EnsureConnReqCredentials(deviceID)
	}

	// 无线概况（看板上的 2.4G/5G 那一栏）：首次纳管 / BOOTSTRAP 时采一次。
	// 只取十几个摘要字段（SSID/开关/信道/标准/加密/终端数），不会把
	// 四百多个 WLAN 参数全拉回来，所以对设备负担很小。
	if s.cfg.AutoFetchWiFi && (created || inf.HasEvent("0 BOOTSTRAP")) {
		if _, err := s.EnqueueFetchWiFi(deviceID); err != nil {
			s.log.Warn("入队采集无线概况失败", "device_id", deviceID, "err", err)
		}
		// WAN 连接概况（详情页的「WAN 连接」区块），同样只取十几个摘要字段
		if _, err := s.EnqueueFetchWAN(deviceID); err != nil {
			s.log.Warn("入队采集 WAN 概况失败", "device_id", deviceID, "err", err)
		}
	}

	// 有诊断在等结果：设备这次 Inform 可能就带着结果回来了
	// （事件 8 DIAGNOSTICS COMPLETE），也可能什么都没说。
	// 两种情况都主动读一次 —— 不依赖事件码，因为不是所有设备都会发。
	if running, err := s.store.FindRunningTask(deviceID, TaskDiagnostics); err == nil && running != nil {
		s.log.Info("有诊断在等结果，本次会话读一次",
			"device_id", deviceID, "task_id", running.ID,
			"events", strings.Join(inf.EventCodes(), ", "))
		s.enqueueDiagResultRead(sess, running.ID)
	}

	// 能力探测（每台设备只做一次）：枚举顶层对象，看有没有 FTTR / 子设备对象。
	// 探测到会在同一个会话里顺手把那个子树拉回来（见 onGetParameterNamesResponse）。
	// 不探测到就永远不显示那个区块 —— 不要给用户看一个空区块。
	if s.cfg.ProbeCapabilities && (created || inf.HasEvent("0 BOOTSTRAP")) {
		if probed, err := s.store.HasObjectNodes(deviceID); err == nil && !probed {
			if _, err := s.EnqueueProbeCapabilities(deviceID, root); err != nil {
				s.log.Warn("入队能力探测失败", "device_id", deviceID, "err", err)
			}
		}
	}

	s.writeEnvelope(w, sess, env.ID, InformResponseBody())
}

// onGetParameterValuesResponse 处理我们下发的 GetParameterValues 的回执。
func (s *Server) onGetParameterValuesResponse(w http.ResponseWriter, sess *Session, env *Envelope, m *Node) {
	params := ParseParamValues(m.Child("ParameterList"))

	// 这条 GPV 可能是一次「读回核对」（带着期望值），先把载荷拿出来
	var p gpvPayload
	if t, ok := s.pendingTask(sess); ok && t.Kind == TaskGetParameterValues {
		_ = json.Unmarshal([]byte(t.Payload), &p)
	}

	s.warnIfPartialResponse(sess, len(params))
	if len(params) > 0 && sess.DeviceID != 0 {
		if err := s.store.UpsertParams(sess.DeviceID, toStoreParams(params), "getvalues"); err != nil {
			s.log.Warn("写入参数失败", "device_id", sess.DeviceID, "err", err)
		}
		// 把 DeviceInfo 里的型号/版本等补齐到设备行
		if fields := deviceFieldsFromParams(params); fields != nil {
			if root := detectRoot(params); root != "" {
				fields.DataModelRoot = root
			}
			if err := s.store.MergeDeviceFields(sess.DeviceID, fields); err != nil {
				s.log.Warn("合并设备信息失败", "device_id", sess.DeviceID, "err", err)
			}
		}
	}

	s.checkReadBack(sess, p, params)

	// ping 诊断：这条 GPV 可能是去读诊断结果的
	if p.DiagTask != 0 {
		s.handleDiagResult(sess, p.DiagTask, params)
	}

	s.finishTask(sess, fmt.Sprintf("收到 %d 个参数", len(params)))
	s.log.Info("取回参数", "device_id", sess.DeviceID, "count", len(params))
	s.dispatchNextTask(w, sess)
}

// checkReadBack 如果这条 GPV 是某次 SetParameterValues 的读回核对，就比对期望值。
//
// 分两种时机（见 gpvPayload.Deferred）：
//   - 同一次会话内的立即核对：对不上**先不判错**。真机的无线参数是异步生效的，
//     实测写入后同一会话读回仍是旧值，几十秒后才变；或本来就写不动的参数会一直不变。
//     分不清就先排到下一轮会话再核。
//   - 设备下一轮会话的延后核对：还对不上就是真的没生效，把原设置任务标为失败。
func (s *Server) checkReadBack(sess *Session, p gpvPayload, got []ParamValue) {
	if p.VerifyTask == 0 || len(p.Verify) == 0 {
		return
	}
	problems, unverifiable := verifyReadBack(p.Verify, p.Prev, got)

	if len(problems) == 0 {
		if len(unverifiable) > 0 {
			// 对上了、或者本来就核对不了（设备不回读，如密码）：直接结案，不用再等一轮
			s.noteUnverifiable(p.VerifyTask, unverifiable)
		}
		s.log.Info("读回核对通过",
			"device_id", sess.DeviceID, "set_task", p.VerifyTask,
			"params", len(p.Verify), "unverifiable", len(unverifiable))
		return
	}

	if !p.Deferred {
		s.log.Info("同会话读回仍是旧值，设备可能异步生效；排到下一轮会话再核对",
			"device_id", sess.DeviceID, "set_task", p.VerifyTask, "problems", problems)
		s.enqueueDeferredVerify(sess, p)
		return
	}

	// 已经等过一轮了：把确实没生效的和无法核对的分开结论
	if len(unverifiable) > 0 {
		s.noteUnverifiable(p.VerifyTask, unverifiable)
	}
	msg := "设备接受了写入（Status=0）但读回未生效：" + strings.Join(problems, "；")
	s.log.Warn("写入未生效（已在下一轮会话复核）",
		"device_id", sess.DeviceID, "set_task", p.VerifyTask, "problems", problems)
	if err := s.store.FailTask(p.VerifyTask, msg); err != nil {
		s.log.Warn("标记写入未生效失败", "task_id", p.VerifyTask, "err", err)
	}
}

// noteUnverifiable 把「已接受但设备不回读、无法核对」的参数记到任务结果里。
// 状态仍是成功 —— 因为确实没有证据表明失败（密码这类参数本来就读不出来）。
func (s *Server) noteUnverifiable(taskID int64, names []string) {
	if len(names) == 0 {
		return
	}
	msg := fmt.Sprintf("设置成功（Status=0）；%d 个参数设备不回读、无法核对：%s",
		len(names), strings.Join(names, "，"))
	if err := s.store.CompleteTask(taskID, msg); err != nil {
		s.log.Warn("更新任务结果失败", "task_id", taskID, "err", err)
	}
}

// enqueueDeferredVerify 把核对任务排到设备下一轮会话（本次会话不取它）。
func (s *Server) enqueueDeferredVerify(sess *Session, p gpvPayload) {
	names := make([]string, 0, len(p.Verify))
	for _, v := range p.Verify {
		names = append(names, v.Name)
	}
	payload, _ := json.Marshal(gpvPayload{
		Names:      names,
		VerifyTask: p.VerifyTask,
		Verify:     p.Verify,
		Prev:       p.Prev,
		Deferred:   true,
	})
	id, err := s.store.EnqueueTask(&store.Task{
		DeviceID: sess.DeviceID,
		Kind:     TaskGetParameterValues,
		Payload:  string(payload),
	})
	if err != nil {
		s.log.Warn("入队延后核对失败", "device_id", sess.DeviceID, "err", err)
		return
	}
	sess.deferTask(id)
}

// warnIfPartialResponse 对照任务载荷里的参数名个数，检查 CPE 是不是只回了一部分。
//
// 真机实测：华为 HN8145X6N 单次最多只回 256 个，多出来的静默丢弃。我们靠分批
// （Config.MaxParamsPerRequest）避免踩到上限；这里留个告警，是为了在遇到别的、
// 上限更低的设备时能立刻看出来，而不是默默少采集一堆参数。
func (s *Server) warnIfPartialResponse(sess *Session, got int) {
	t, ok := s.pendingTask(sess)
	if !ok || t.Kind != TaskGetParameterValues {
		return
	}
	var p gpvPayload
	if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
		return
	}
	if len(p.Names) > got {
		s.log.Warn("CPE 只回了一部分参数，可能触到了它的单次上限（本 ACS 会分批，若仍出现请调小每批数量）",
			"device_id", sess.DeviceID, "requested", len(p.Names), "returned", got)
	}
}

// onGetParameterNamesResponse 处理 GetParameterNames 的回执。
// 如果这条任务要求「枚举完顺便把值取回来」，就在这里接着入队一条 GPV ——
// 它会被下面的 dispatchNextTask 在**同一个会话**里马上发出去。
func (s *Server) onGetParameterNamesResponse(w http.ResponseWriter, sess *Session, env *Envelope, m *Node) {
	infos := ParseParameterInfoStructs(m.Child("ParameterList"))

	// 先把这条任务的载荷拿出来：它决定要不要把名字写库、要不要接着取值。
	p, havePayload := s.pendingGPNPayload(sess)

	if !p.SkipStore && len(infos) > 0 && sess.DeviceID != 0 {
		params := make([]store.Param, 0, len(infos))
		for _, in := range infos {
			params = append(params, store.Param{
				Name:     in.Name,
				Writable: in.Writable,
				Source:   "getnames",
			})
		}
		if err := s.store.UpsertParams(sess.DeviceID, params, "getnames"); err != nil {
			s.log.Warn("写入参数名失败", "device_id", sess.DeviceID, "err", err)
		}
	}

	s.log.Info("枚举参数名",
		"device_id", sess.DeviceID, "count", len(infos),
		"path", p.Path, "stored", !p.SkipStore, "have_payload", havePayload)

	if havePayload && p.ThenFetch {
		s.chainFetchAfterNames(sess, p, infos)
	}

	// 能力探测：只枚举了顶层对象，看看有没有 FTTR / 子设备对象。
	// 有的话**在同一个会话里**接着把那个子树拉了（不探测到就什么都不做，
	// 界面上的「FTTR 子设备」区块直接不显示）。
	if havePayload && p.ProbeFTTR && sess.DeviceID != 0 {
		names := make([]string, 0, len(infos))
		for _, in := range infos {
			names = append(names, in.Name)
		}
		if prefix, ok := detectFTTRPrefix(names); ok {
			s.log.Info("探测到 FTTR/子设备对象，接着采集", "device_id", sess.DeviceID, "prefix", prefix)
			if _, err := s.EnqueueFetchSubtree(sess.DeviceID, prefix, nil, 0); err != nil {
				s.log.Warn("入队采集 FTTR 子树失败", "device_id", sess.DeviceID, "err", err)
			}
		} else {
			s.log.Info("未探测到 FTTR/子设备对象（界面不显示该区块）", "device_id", sess.DeviceID)
		}
	}

	s.finishTask(sess, fmt.Sprintf("收到 %d 个参数名", len(infos)))
	s.dispatchNextTask(w, sess)
}

// pendingTaskKind 取出当前在途任务的类型与 ID（没有则返回空串与 0）。
func (s *Server) pendingTaskKind(sess *Session) (string, int64) {
	t, ok := s.pendingTask(sess)
	if !ok {
		return "", 0
	}
	return t.Kind, t.ID
}

// enqueueDiagResultRead 入队一条「读 ping 诊断结果」的任务。
//
// 结果往往要分几次才能拿到：设备刚收到请求时 DiagnosticsState 还是 Requested，
// 要等它跑完（会单独发一次 Inform）才变成 Complete。所以这个读会重复几轮，
// 直到状态明确为止。
func (s *Server) enqueueDiagResultRead(sess *Session, diagTaskID int64) {
	if sess.DeviceID == 0 {
		return
	}
	t, err := s.store.GetTask(diagTaskID)
	if err != nil || t == nil {
		return
	}
	var p diagPayload
	if json.Unmarshal([]byte(t.Payload), &p) != nil {
		return
	}
	prefix := p.Prefix
	if prefix == "" {
		d, err := s.store.GetDevice(sess.DeviceID)
		if err != nil {
			return
		}
		prefix = diagnosticsPrefix(d.DataModelRoot)
	}
	names := make([]string, 0, len(diagnosticsReadFields))
	for _, f := range diagnosticsReadFields {
		names = append(names, prefix+f)
	}
	payload, _ := json.Marshal(gpvPayload{Names: names, DiagTask: diagTaskID})
	if _, err := s.store.EnqueueTask(&store.Task{
		DeviceID: sess.DeviceID,
		Kind:     TaskGetParameterValues,
		Payload:  string(payload),
	}); err != nil {
		s.log.Warn("入队读诊断结果失败", "device_id", sess.DeviceID, "err", err)
	}
}

// handleDiagResult 处理一次诊断结果的读回。
//
//   - Complete  -> 汇总结果，任务算成功
//   - Error_*   -> 任务失败（设备把失败原因写在状态里）
//   - 其它       -> 还在跑，任务保持 running，等设备下一次上报再读
func (s *Server) handleDiagResult(sess *Session, diagTaskID int64, got []ParamValue) {
	vals := map[string]string{}
	for _, v := range got {
		vals[strings.ToLower(v.Name)] = strings.TrimSpace(v.Value)
	}
	pick := func(field string) string {
		want := strings.ToLower(field)
		for k, v := range vals {
			if strings.HasSuffix(k, want) {
				return v
			}
		}
		return ""
	}

	host := ""
	if t, err := s.store.GetTask(diagTaskID); err == nil && t != nil {
		var p diagPayload
		if json.Unmarshal([]byte(t.Payload), &p) == nil {
			host = p.Host
		}
	}

	state := pick("DiagnosticsState")
	switch {
	case state == "":
		s.log.Warn("读诊断结果：没拿到 DiagnosticsState", "device_id", sess.DeviceID, "task_id", diagTaskID)

	case state == "Complete":
		okN, _ := strconv.Atoi(pick("SuccessCount"))
		failN, _ := strconv.Atoi(pick("FailureCount"))
		summary := fmt.Sprintf("PING 目标 %s：发送包 %d，成功 %d，失败 %d；最小/平均/最大延时 = %s/%s/%s ms",
			host, okN+failN, okN, failN,
			pick("MinimumResponseTime"), pick("AverageResponseTime"), pick("MaximumResponseTime"))
		if err := s.store.SetTaskResult(diagTaskID, summary); err != nil {
			s.log.Warn("写诊断结果失败", "task_id", diagTaskID, "err", err)
		}
		if err := s.store.CompleteTask(diagTaskID, summary); err != nil {
			s.log.Warn("结束诊断任务失败", "task_id", diagTaskID, "err", err)
		}
		s.log.Info("诊断完成", "device_id", sess.DeviceID, "task_id", diagTaskID, "result", summary)

	case strings.HasPrefix(state, "Error"):
		msg := "设备报告诊断失败：" + state
		s.log.Warn("诊断失败", "device_id", sess.DeviceID, "task_id", diagTaskID, "state", state)
		if err := s.store.FailTask(diagTaskID, msg); err != nil {
			s.log.Warn("标记诊断失败出错", "task_id", diagTaskID, "err", err)
		}

	default:
		// 还在跑：保持 running，让状态接着蹦
		s.log.Info("诊断进行中", "device_id", sess.DeviceID, "task_id", diagTaskID, "state", state)
		if err := s.store.SetTaskResult(diagTaskID, "诊断进行中（设备状态 "+state+"）…"); err != nil {
			s.log.Warn("更新诊断任务状态失败", "task_id", diagTaskID, "err", err)
		}
	}
}

// pendingTask 取出当前在途任务。
func (s *Server) pendingTask(sess *Session) (*store.Task, bool) {
	if sess.pendingTask == 0 {
		return nil, false
	}
	t, err := s.store.GetTask(sess.pendingTask)
	if err != nil || t == nil {
		return nil, false
	}
	return t, true
}

// pendingGPNPayload 取出当前在途 GetParameterNames 任务的载荷。
func (s *Server) pendingGPNPayload(sess *Session) (gpnPayload, bool) {
	var p gpnPayload
	t, ok := s.pendingTask(sess)
	if !ok || t.Kind != TaskGetParameterNames {
		return p, false
	}
	if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
		return p, false
	}
	return p, true
}

// chainFetchAfterNames 按任务载荷里的白/黑名单，把要取值的参数名入队成 GPV。
// 入队的任务会被紧跟着的 dispatchNextTask 在**同一个会话**里发出去。
func (s *Server) chainFetchAfterNames(sess *Session, p gpnPayload, infos []ParamInfo) {
	if sess.DeviceID == 0 {
		return
	}
	names := filterLeafNames(infos, p.Include, p.Exclude, p.Max)
	if len(names) == 0 {
		s.log.Warn("枚举到 0 个可取值参数，跳过取值",
			"device_id", sess.DeviceID, "path", p.Path,
			"include", p.Include, "exclude", p.Exclude)
		return
	}
	batches, err := s.enqueueGPVDivided(sess.DeviceID, names)
	if err != nil {
		s.log.Warn("入队取值任务失败", "device_id", sess.DeviceID, "err", err)
		return
	}
	s.log.Info("枚举完成，同一会话内接着取值",
		"device_id", sess.DeviceID, "path", p.Path,
		"params", len(names), "batches", batches)
}

// onSimpleResponse 处理我们不特别关心的响应（如 SetParameterValuesResponse）。
func (s *Server) onSimpleResponse(w http.ResponseWriter, sess *Session, m *Node, note string) {
	if m.Local == "SetParameterValuesResponse" {
		s.handleSetParameterValuesResponse(w, sess, m)
		return
	}
	s.finishTask(sess, note)
	s.dispatchNextTask(w, sess)
}

// handleSetParameterValuesResponse 处理设置参数的回执。
func (s *Server) handleSetParameterValuesResponse(w http.ResponseWriter, sess *Session, m *Node) {
	status := strings.TrimSpace(m.ChildText("Status"))

	// 取出这次写了什么（下面要用它决定要不要重采无线概况）
	var vals []ParamValue
	kind, taskID := "", int64(0)
	if t, ok := s.pendingTask(sess); ok {
		kind, taskID = t.Kind, t.ID
		if t.Kind == TaskSetParameterValues {
			var spv spvPayload
			if json.Unmarshal([]byte(t.Payload), &spv) == nil {
				vals = spv.Values
			}
		}
	}

	if status != "" && status != "0" {
		msg := "CPE 报告设置失败，Status=" + status
		s.log.Warn("设置参数失败", "device_id", sess.DeviceID, "task_id", taskID, "status", status)
		s.failTask(sess, msg)
		s.dispatchNextTask(w, sess)
		return
	}

	// ping 诊断：SetParameterValues 只是让设备开始跑，任务**不能**在这里结束 ——
	// 结果要等设备回报，可能轮到下一次 Inform。
	if kind == TaskDiagnostics {
		if err := s.store.SetTaskResult(taskID, "诊断已下发，等设备回报结果…"); err != nil {
			s.log.Warn("更新诊断任务状态失败", "task_id", taskID, "err", err)
		}
		s.log.Info("诊断已下发，接着先读一次结果", "device_id", sess.DeviceID, "task_id", taskID)
		s.enqueueDiagResultRead(sess, taskID)
		s.dispatchNextTask(w, sess)
		return
	}

	// 写入成功不等于真的生效：把刚写的参数读回来核对。
	// 这一步是值得的 —— 有的 CPE 会默默接受写入但对某个参数不生效，
	// 只有读回来才能看出来；而且顺带把界面上的值刷新成实际值。
	s.enqueueReadBack(sess)

	// 写的是无线参数的话，顺手把整份无线概况也重采一遍。
	//
	// 为什么必须做：写入后我们只回读了改动的那一个参数，而界面上的
	// 状态/信道/终端数这些**相关联**的值就停在写入前了。
	// （这里真机上错过一次：5GHz 射频已经起来了、能搜到信号，
	// 界面却还显示 Disabled —— 因为 Status 是几十分钟前采集的。）
	s.refreshWiFiAfterWrite(sess, vals)

	s.finishTask(sess, "设置成功（Status="+statusOrZero(status)+"）")
	s.dispatchNextTask(w, sess)
}

// refreshWiFiAfterWrite 写入无线参数后，排一轮「重采无线概况」。
// 延后到下一轮会话：无线参数可能异步生效，等下一轮拿到的才是生效后的值。
func (s *Server) refreshWiFiAfterWrite(sess *Session, vals []ParamValue) {
	if sess.DeviceID == 0 || !containsWiFiParam(vals) {
		return
	}
	id, err := s.EnqueueFetchWiFi(sess.DeviceID)
	if err != nil {
		s.log.Warn("入队重采无线概况失败", "device_id", sess.DeviceID, "err", err)
		return
	}
	sess.deferTask(id)
	s.log.Info("写入的是无线参数，已排下一轮会话重采无线概况",
		"device_id", sess.DeviceID, "task_id", id)
}

// containsWiFiParam 判断这批参数里有没有无线相关的（TR-098 的 WLANConfiguration. /
// TR-181 的 WiFi. 都算）。
func containsWiFiParam(vals []ParamValue) bool {
	for _, v := range vals {
		l := strings.ToLower(v.Name)
		if strings.Contains(l, "wlanconfiguration.") || strings.Contains(l, "wifi.") {
			return true
		}
	}
	return false
}

func statusOrZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return s
}

// enqueueReadBack 把刚设置过的那批参数读回来，并带上“期望值”以供比对。
func (s *Server) enqueueReadBack(sess *Session) {
	if sess.pendingTask == 0 || sess.DeviceID == 0 {
		return
	}
	t, err := s.store.GetTask(sess.pendingTask)
	if err != nil || t == nil || t.Kind != TaskSetParameterValues {
		return
	}
	var p spvPayload
	if json.Unmarshal([]byte(t.Payload), &p) != nil || len(p.Values) == 0 {
		return
	}
	names := make([]string, 0, len(p.Values))
	for _, v := range p.Values {
		names = append(names, v.Name)
	}

	// 不用 enqueueGPVDivided：读回要带上期望值，所以自己分批。
	max := s.cfg.MaxParamsPerRequest
	if max <= 0 {
		max = defaultMaxParamsPerRequest
	}
	batches := 0
	for start := 0; start < len(names); start += max {
		end := start + max
		if end > len(names) {
			end = len(names)
		}
		payload, _ := json.Marshal(gpvPayload{
			Names:      names[start:end],
			VerifyTask: t.ID,
			Verify:     p.Values[start:end],
			Prev:       prevSlice(p.Prev, start, end),
		})
		if _, err := s.store.EnqueueTask(&store.Task{
			DeviceID: sess.DeviceID,
			Kind:     TaskGetParameterValues,
			Payload:  string(payload),
		}); err != nil {
			s.log.Warn("入队读回校验失败", "device_id", sess.DeviceID, "err", err)
			return
		}
		batches++
	}
	s.log.Info("设置成功，同一会话内读回核对",
		"device_id", sess.DeviceID, "params", len(names), "batches", batches)
}

// verifyReadBack 把读回的结果与期望值比对。
//
// 纯函数，便于单测。返回两组：
//   - problems：确实对不上的（设备没生效）
//   - unverifiable：“读回是空的，而且**写入前就是空的**” —— 这类参数设备根本
//     不回读内容（典型：WiFi 密码），属于无法核对，**不能当失败**。
//
// 比较时对布尔值宽容一点（true/1、false/0 视为一样）。
func verifyReadBack(expect, prev, got []ParamValue) (problems, unverifiable []string) {
	actual := make(map[string]string, len(got))
	for _, g := range got {
		actual[keyOf(g.Name)] = strings.TrimSpace(g.Value)
	}
	before := make(map[string]string, len(prev))
	for _, pv := range prev {
		before[keyOf(pv.Name)] = strings.TrimSpace(pv.Value)
	}

	for _, e := range expect {
		key := keyOf(e.Name)
		want := strings.TrimSpace(e.Value)
		// 写入前就是空的，且我们这次要写一个非空值 —— 这类参数很可能读不回来
		wasEmpty := before[key] == ""

		gotVal, ok := actual[key]
		if !ok {
			if wasEmpty {
				unverifiable = append(unverifiable, e.Name)
			} else {
				problems = append(problems, fmt.Sprintf("%s：读回里没有这个参数", e.Name))
			}
			continue
		}
		if sameValue(want, gotVal) {
			continue
		}
		if gotVal == "" && wasEmpty {
			// 写前写后都是空 → 设备不回读该参数（如 WiFi 密码），无法核对
			unverifiable = append(unverifiable, e.Name)
			continue
		}
		problems = append(problems, fmt.Sprintf("%s：期望 %q，读回 %q", e.Name, want, gotVal))
	}
	return problems, unverifiable
}

func keyOf(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// prevSlice 取 Prev 的 [start,end) 段（读回会被分批，Prev 要跟着切）。
func prevSlice(prev []ParamValue, start, end int) []ParamValue {
	if start >= len(prev) {
		return nil
	}
	if end > len(prev) {
		end = len(prev)
	}
	return prev[start:end]
}

func sameValue(a, b string) bool {
	if a == b {
		return true
	}
	return normBool(a) == normBool(b) && normBool(a) != ""
}

func normBool(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true":
		return "1"
	case "0", "false":
		return "0"
	}
	return ""
}

// onFault 处理 CPE 回的错误。
func (s *Server) onFault(w http.ResponseWriter, sess *Session, env *Envelope, m *Node) {
	f := ParseFault(m)
	code, msg := 0, ""
	if f != nil {
		code, msg = f.Code, f.String
	}
	s.log.Warn("CPE 返回 Fault", "device_id", sess.DeviceID, "code", code, "msg", msg)

	if sess.pendingTask != 0 {
		detail := fmt.Sprintf("CPE 返回错误 %d: %s", code, msg)
		for _, sf := range f.SetParamFaults {
			detail += fmt.Sprintf(" | %s -> %d %s", sf.Name, sf.Code, sf.String)
		}
		if err := s.store.FailTask(sess.pendingTask, detail); err != nil {
			s.log.Warn("标记任务失败出错", "task_id", sess.pendingTask, "err", err)
		}
		sess.pendingTask = 0
	}
	s.dispatchNextTask(w, sess)
}

// onMiscCPERequest 处理 TransferComplete 等「本期只记录」的请求。
func (s *Server) onMiscCPERequest(w http.ResponseWriter, sess *Session, env *Envelope, m *Node) {
	switch m.Local {
	case "TransferComplete":
		key := m.ChildText("CommandKey")
		fc := ""
		if fs := m.Child("FaultStruct"); fs != nil && fs.ChildText("FaultCode") != "" &&
			fs.ChildText("FaultCode") != "0" {
			fc = fmt.Sprintf("（失败 %s: %s）", fs.ChildText("FaultCode"), fs.ChildText("FaultString"))
		}
		s.log.Info("收到 TransferComplete",
			"device_id", sess.DeviceID, "command_key", key, "fault", fc)
		s.writeEnvelope(w, sess, env.ID, GenericResponseBody("TransferCompleteResponse"))
	default:
		s.log.Info("收到 CPE 请求", "device_id", sess.DeviceID, "method", m.Local)
		s.writeEnvelope(w, sess, env.ID, GenericResponseBody(m.Local+"Response"))
	}
}

// ---------- 任务下发 ----------

// handleCPEReady：CPE 发来空 POST，表示「有活就给我」。
func (s *Server) handleCPEReady(w http.ResponseWriter, sess *Session) {
	if sess.DeviceID == 0 {
		// 没经过 Inform 就来要活：不正常，直接结束会话
		s.endSession(w, sess)
		return
	}
	s.dispatchNextTask(w, sess)
}

// dispatchNextTask 取一条待办任务发下去；没有就 204 结束会话。
func (s *Server) dispatchNextTask(w http.ResponseWriter, sess *Session) {
	t, err := s.store.ClaimNextTask(sess.DeviceID, sess.deferredIDs())
	s.log.Debug("取任务", "device_id", sess.DeviceID, "session", sess.ID, "deferred", sess.deferredIDs(), "got", taskID(t))
	if err != nil {
		s.log.Error("取待办任务失败", "device_id", sess.DeviceID, "err", err)
		s.writeEnvelope(w, sess, newRPCID(), FaultBody(FaultInternalError, "取任务失败"))
		return
	}
	if t == nil {
		s.endSession(w, sess)
		return
	}

	body, err := buildTaskBody(t)
	if err != nil {
		s.log.Error("构造任务报文失败", "task_id", t.ID, "kind", t.Kind, "err", err)
		if ferr := s.store.FailTask(t.ID, "构造报文失败: "+err.Error()); ferr != nil {
			s.log.Warn("标记任务失败出错", "task_id", t.ID, "err", ferr)
		}
		s.dispatchNextTask(w, sess) // 跳过这条，继续下一条
		return
	}

	sess.pendingTask = t.ID
	s.log.Info("下发任务", "device_id", sess.DeviceID, "task_id", t.ID, "kind", t.Kind)
	s.writeEnvelope(w, sess, newRPCID(), body)
}

// finishTask 把当前在途任务标记成功。
func (s *Server) finishTask(sess *Session, note string) {
	if sess.pendingTask == 0 {
		return
	}
	if err := s.store.CompleteTask(sess.pendingTask, note); err != nil {
		s.log.Warn("标记任务完成出错", "task_id", sess.pendingTask, "err", err)
	}
	sess.pendingTask = 0
}

// failTask 把当前在途任务标记失败。
func (s *Server) failTask(sess *Session, note string) {
	if sess.pendingTask == 0 {
		return
	}
	if err := s.store.FailTask(sess.pendingTask, note); err != nil {
		s.log.Warn("标记任务失败出错", "task_id", sess.pendingTask, "err", err)
	}
	sess.pendingTask = 0
}

// buildTaskBody 把一条任务翻译成 CWMP 请求的 Body。
func buildTaskBody(t *store.Task) (string, error) {
	switch t.Kind {
	case TaskGetParameterValues:
		var p gpvPayload
		if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
			return "", err
		}
		if len(p.Names) == 0 {
			return "", fmt.Errorf("没有要查询的参数名")
		}
		return GetParameterValuesBody(p.Names), nil

	case TaskGetParameterNames:
		var p gpnPayload
		if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
			return "", err
		}
		return GetParameterNamesBody(p.Path, p.NextLevel), nil

	case TaskSetParameterValues:
		var p spvPayload
		if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
			return "", err
		}
		if len(p.Values) == 0 {
			return "", fmt.Errorf("没有要设置的参数")
		}
		key := p.ParameterKey
		if key == "" {
			key = newRPCID()
		}
		return SetParameterValuesBody(p.Values, key), nil

	case TaskDiagnostics:
		var p diagPayload
		if err := json.Unmarshal([]byte(t.Payload), &p); err != nil {
			return "", err
		}
		key := t.CommandKey
		if key == "" {
			key = newRPCID()
		}
		// 顺序很重要：DiagnosticsState=Requested 必须放在**最后**。
		// 设备看到 Requested 就会开始跑 ping，所以 Host / 次数得先就位；
		// 否则可能拿着空 Host 或默认次数去跑（本项目的模拟器就这么暴露了这个顺序问题，
		// 真机上有些设备也会这样）。
		vals := []ParamValue{}
		if p.Count > 0 {
			vals = append(vals, ParamValue{
				Name:  p.Prefix + "NumberOfRepetitions",
				Value: strconv.Itoa(p.Count),
				Type:  "unsignedInt",
			})
		}
		vals = append(vals,
			ParamValue{Name: p.Prefix + "Host", Value: p.Host, Type: "string"},
			ParamValue{Name: p.Prefix + "DiagnosticsState", Value: "Requested", Type: "string"},
		)
		return SetParameterValuesBody(vals, key), nil

	case TaskGetRPCMethods:
		return GetRPCMethodsBody(), nil

	case TaskReboot:
		key := t.CommandKey
		if key == "" {
			var p rebootPayload
			_ = json.Unmarshal([]byte(t.Payload), &p)
			key = p.CommandKey
		}
		if key == "" {
			key = newRPCID()
		}
		return RebootBody(key), nil
	}
	return "", fmt.Errorf("未实现的任务类型 %q", t.Kind)
}

// ---------- 输出辅助 ----------

func (s *Server) writeEnvelope(w http.ResponseWriter, sess *Session, id, body string) {
	ns := sess.cwmpNS
	if ns == "" {
		ns = cwmpNSBase + DefaultCWMPVersion
	}
	out := NewEnvelope(ns, id, body)
	if s.cfg.LogRawSOAP {
		s.log.Debug("发出报文", "session", sess.ID, "body", out)
	}
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, out)
}

// endSession 回 204（无内容）并结束会话 —— 这是 TR-069 里「我没活了」的标准表达。
func (s *Server) endSession(w http.ResponseWriter, sess *Session) {
	s.log.Debug("会话结束", "session", sess.ID, "device_id", sess.DeviceID)
	// 清掉推迟列表：本次会话要跳过的任务，下一轮会话就该放行了。
	// （会话对象会跨多次 HTTP 请求甚至跨会话复用，不清就会把任务永久跳过。）
	sess.clearDeferred()
	s.sess.end(sess)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- 小工具 ----------

func newRPCID() string { return newSessionID() }

func taskID(t *store.Task) int64 {
	if t == nil {
		return 0
	}
	return t.ID
}

func pick(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func toStoreParams(in []ParamValue) []store.Param {
	out := make([]store.Param, 0, len(in))
	for _, p := range in {
		out = append(out, store.Param{
			Name:      p.Name,
			Value:     p.Value,
			ValueType: p.Type,
		})
	}
	return out
}

// detectRoot 从参数名推断数据模型根。
//
// 比较是**大小写不敏感**的，因为真机确实会写错：华为 HN8145X6N 在 0 BOOTSTRAP 时
// 上报 InternetGateWayDevice.DeviceInfo.X_CT-ProvCode —— 根节点里的 W 是大写。
// 注意：这里只影响「识别」；存库时参数名原样保留，以后 SetParameterValues 必须用
// 设备自己的拼法（改写了就下发不会去）。
func detectRoot(params []ParamValue) string {
	for _, p := range params {
		lower := strings.ToLower(p.Name)
		switch {
		case strings.HasPrefix(lower, "device."):
			return "Device."
		case strings.HasPrefix(lower, "internetgatewaydevice."):
			return "InternetGatewayDevice."
		}
	}
	return ""
}

// findRootTypo 找出「根前缀大小写写错」的参数名。
//
// 这类参数能正常存下来（解析是容错的），但值得在日志里提一句：
// 一是提醒运维这批参数名和别的不是同一套拼法，二是以后做前缀查询时别把它漏掉。
func findRootTypo(root string, params []ParamValue) []string {
	if root == "" {
		return nil
	}
	lowerRoot := strings.ToLower(root)
	var out []string
	for _, p := range params {
		if strings.HasPrefix(p.Name, root) {
			continue
		}
		if strings.HasPrefix(strings.ToLower(p.Name), lowerRoot) {
			out = append(out, p.Name)
		}
	}
	return out
}

// deviceFieldsFromParams 从参数列表里挑出设备属性。
// 用后缀匹配（且忽略大小写），所以 TR-098 / TR-181 两种命名、
// 以及厂商把根写错的情况都能命中。
func deviceFieldsFromParams(params []ParamValue) *store.Device {
	d := &store.Device{}
	for _, p := range params {
		lower := strings.ToLower(p.Name)
		switch {
		case strings.HasSuffix(lower, ".deviceinfo.manufacturer"):
			d.Manufacturer = p.Value
		case strings.HasSuffix(lower, ".deviceinfo.modelname"):
			d.ModelName = p.Value
		case strings.HasSuffix(lower, ".deviceinfo.softwareversion"):
			d.SoftwareVersion = p.Value
		case strings.HasSuffix(lower, ".deviceinfo.hardwareversion"):
			d.HardwareVersion = p.Value
		case strings.HasSuffix(lower, ".deviceinfo.specversion"):
			d.SpecVersion = p.Value
		case strings.HasSuffix(lower, ".deviceinfo.provisioningcode"):
			d.ProvisioningCode = p.Value
		case strings.HasSuffix(lower, ".managementserver.connectionrequesturl"):
			d.ConnRequestURL = p.Value
		case strings.HasSuffix(lower, ".managementserver.periodicinforminterval"):
			if n, err := strconv.Atoi(strings.TrimSpace(p.Value)); err == nil {
				d.PeriodicInterval = n
			}
		case strings.HasSuffix(lower, ".externalipaddress"):
			d.ExternalIP = p.Value
		}
	}
	return d
}

// hasDeviceInfo 判断是否已经拿到过设备基本信息。
func (s *Server) hasDeviceInfo(deviceID int64) bool {
	d, err := s.store.GetDevice(deviceID)
	if err != nil {
		return false
	}
	if d.SoftwareVersion != "" || d.ModelName != "" {
		return true
	}
	prefix := d.DataModelRoot + "DeviceInfo."
	if d.DataModelRoot == "" {
		return false
	}
	ok, err := s.store.HasParamPrefix(deviceID, prefix)
	return err == nil && ok
}

// readBody 读请求体，顺带处理 gzip/deflate（不少 CPE 会压缩）。
func readBody(r *http.Request, max int64) ([]byte, error) {
	if max <= 0 {
		max = 4 << 20
	}
	var rd io.Reader = r.Body
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, fmt.Errorf("解压 gzip 失败: %w", err)
		}
		defer gz.Close()
		rd = gz
	case "deflate":
		zr, err := zlib.NewReader(r.Body)
		if err != nil {
			return nil, fmt.Errorf("解压 deflate 失败: %w", err)
		}
		defer zr.Close()
		rd = zr
	default:
		return nil, fmt.Errorf("不支持的 Content-Encoding: %s", r.Header.Get("Content-Encoding"))
	}

	b, err := io.ReadAll(io.LimitReader(rd, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("请求体超过上限 %d 字节", max)
	}
	return b, nil
}
