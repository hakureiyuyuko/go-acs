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
)

// 任务载荷（JSON）。
type gpvPayload struct {
	Names []string `json:"names"`
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
	// Max：取值名单的最大条数（0 = 不限制）。
	Max int `json:"max,omitempty"`
}

type spvPayload struct {
	Values       []ParamValue `json:"values"`
	ParameterKey string       `json:"parameter_key"`
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
	MaxBodyBytes        int64
	LogRawSOAP          bool
	OfflineAfter        time.Duration // 超过多久没上报就算离线

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
	"ManagementServer.PeriodicInformInterval",
	"ManagementServer.ParameterKey",
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
	payload, _ := json.Marshal(gpnPayload{
		Path:      path,
		NextLevel: false,
		ThenFetch: true,
		Exclude:   exclude,
		Max:       max,
	})
	id, err := s.store.EnqueueTask(&store.Task{
		DeviceID: deviceID,
		Kind:     TaskGetParameterNames,
		Payload:  string(payload),
	})
	if err == nil {
		s.log.Info("已入队：枚举参数子树", "device_id", deviceID, "path", path, "max", max)
	}
	return id, err
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

// filterLeafNames 从枚举结果里挑出可以取值的叶子参数名。
//   - 对象节点（以 "." 结尾）跳过，因为它们不是叶子；
//   - 名字包含 exclude 里任一子串的跳过；
//   - 超过 max 就截断。
func filterLeafNames(infos []ParamInfo, exclude []string, max int) []string {
	out := make([]string, 0, len(infos))
	for _, in := range infos {
		n := strings.TrimSpace(in.Name)
		if n == "" || strings.HasSuffix(n, ".") {
			continue
		}
		skip := false
		for _, e := range exclude {
			if e != "" && strings.Contains(n, e) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		out = append(out, n)
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

	s.writeEnvelope(w, sess, env.ID, InformResponseBody())
}

// onGetParameterValuesResponse 处理我们下发的 GetParameterValues 的回执。
func (s *Server) onGetParameterValuesResponse(w http.ResponseWriter, sess *Session, env *Envelope, m *Node) {
	params := ParseParamValues(m.Child("ParameterList"))
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
	s.finishTask(sess, fmt.Sprintf("收到 %d 个参数", len(params)))
	s.log.Info("取回参数", "device_id", sess.DeviceID, "count", len(params))
	s.dispatchNextTask(w, sess)
}

// warnIfPartialResponse 对照任务载荷里的参数名个数，检查 CPE 是不是只回了一部分。
//
// 真机实测：华为 HN8145X6N 单次最多只回 256 个，多出来的静默丢弃。我们靠分批
// （Config.MaxParamsPerRequest）避免踩到上限；这里留个告警，是为了在遇到别的、
// 上限更低的设备时能立刻看出来，而不是默默少采集一堆参数。
func (s *Server) warnIfPartialResponse(sess *Session, got int) {
	if sess.pendingTask == 0 {
		return
	}
	t, err := s.store.GetTask(sess.pendingTask)
	if err != nil || t == nil || t.Kind != TaskGetParameterValues {
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
	if len(infos) > 0 && sess.DeviceID != 0 {
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

	s.chainFetchAfterNames(sess, infos)

	s.finishTask(sess, fmt.Sprintf("收到 %d 个参数名", len(infos)))
	s.log.Info("枚举参数名", "device_id", sess.DeviceID, "count", len(infos))
	s.dispatchNextTask(w, sess)
}

// chainFetchAfterNames 根据 GetParameterNames 任务的载荷，决定要不要接着取值。
func (s *Server) chainFetchAfterNames(sess *Session, infos []ParamInfo) {
	if sess.pendingTask == 0 || sess.DeviceID == 0 {
		return
	}
	t, err := s.store.GetTask(sess.pendingTask)
	if err != nil || t == nil {
		return
	}
	var p gpnPayload
	if err := json.Unmarshal([]byte(t.Payload), &p); err != nil || !p.ThenFetch {
		return
	}

	names := filterLeafNames(infos, p.Exclude, p.Max)
	if len(names) == 0 {
		s.log.Warn("枚举到 0 个可取值参数，跳过取值",
			"device_id", sess.DeviceID, "path", p.Path, "exclude", p.Exclude)
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
		status := m.ChildText("Status")
		if status != "" && status != "0" {
			s.finishTask(sess, "CPE 报告设置失败，Status="+status)
			s.log.Warn("CPE 设置参数失败", "device_id", sess.DeviceID, "status", status)
			s.dispatchNextTask(w, sess)
			return
		}
	}
	s.finishTask(sess, note)
	s.dispatchNextTask(w, sess)
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
	t, err := s.store.ClaimNextTask(sess.DeviceID)
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
	s.sess.end(sess)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- 小工具 ----------

func newRPCID() string { return newSessionID() }

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
