// Command cpesim 是一个最小可用的 TR-069 CPE 模拟器，用来在没有真机的情况下
// 端到端验收 ACS。
//
// 它做的事：按 CWMP 的时序向 ACS 上报 Inform，然后不断用空 POST 领取任务、
// 执行、把结果回给 ACS，直到 ACS 回 204 表示会话结束。
//
// 用法示例：
//
//	go run ./test/cpesim -acs http://127.0.0.1:7547/acs -once
//	go run ./test/cpesim -dm 181 -serial XXX -interval 30
package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hakureiyuyuko/go-acs/internal/cwmp"
)

type simulator struct {
	acsURL      string
	user        string
	pass        string
	cwmpVersion string
	event       string
	once        bool
	interval    time.Duration
	// stallAfter > 0 时：启动这么多秒之后**不再**做周期上报，但仍然响应
	// Connection Request 与任务。用来验「设备没按周期报但还活着」这条路径
	// （ACS 应该探测成功、不判离线）。
	stallAfter time.Duration
	stalled    bool

	// 设备身份
	manufacturer string
	oui          string
	productClass string
	serial       string
	model        string

	// 参数表（TR-069 的数据模型）
	params map[string]string
	types  map[string]string

	crCh chan struct{}

	// ignoreSet 里的子串命中某个 SetParameterValues 参数时，模拟器会返回
	// Status=0（“我接受了”）但**不真的应用**这个值。
	// 用来复现真机上的那个行为（写入被接受但静默失效），验证 ACS 的读回核对能抓出来。
	ignoreSet []string

	// writeOnly 里的子串命中时，模拟“能改不能读”的参数（典型：WiFi 密码）：
	// 接受写入（回 Status=0），但**读回永远是空串**。
	// 真机上华为的 PreSharedKey.1.KeyPassphrase 就是这样。
	writeOnly []string

	// diagDelay 模拟「设备异步跑 ping」：收到 DiagnosticsState=Requested 时不当场出结果，
	// 而是在**下一次会话**的 Inform 里带事件 8 DIAGNOSTICS COMPLETE 一起报回来。
	// 真机就是这个行为（所以 ACS 不能指望在同一会话里拿到结果）。
	diagDelay bool

	// pingNeedIface 非空时，模拟「设备自己选的出口出不去、必须显式指定承载接口」：
	// 诊断时 IPPingDiagnostics.Interface 不含这个子串，就全部失败（秒失败、延时全 0）。
	// 真机案例：华为 V271-20（FTTR 主机）的 INTERNET WAN 是桥接、没有默认路由，
	// ACS 不指定 Interface 时设备一发包就 no route。
	pingNeedIface string

	// optical：给主机加上 PON 光功率参数（模拟光猫自己上报收/发光）。
	optical bool
	// fttrOptical：给 FTTR 子设备加上光功率参数（模拟光纤组网子机）。
	fttrOptical bool
	// fttrWireless / fttrWired：把这些实例（1-based 子设备序号）做成无线 / 有线组网，
	// 用来验证「无线、有线组网不显示光功率」那条规则。
	fttrWireless map[int]bool
	fttrWired    map[int]bool

	// pendingDiag 记录“已经开始跑、等着下次会话报结果”的诊断
	pendingDiag string

	// rootPrefix 当前数据模型根（用于查 ManagementServer 下的参数）
	rootPrefix string
	// crNonce 是 Connection Request 监听用的 Digest nonce（进程内固定即可）
	crNonce string

	// fttr 子设备（FTTR 从光猫/子 AP）的数量。
	// 默认 0 —— 这样能同时验证「探测不到就不显示区块」那条。
	fttr int

	// noWAN 为真时不建 WAN 连接对象（验证“没这类参数就不显示 WAN 区块”）
	noWAN bool

	// crUser/crPass 非空时，Connection Request 监听强制要求这套凭据
	// （不依赖设备参数，测试里可以确定性地验证 Digest 实现）
	crUser, crPass string

	// specVersion 是数据模型的 Spec 版本（TR-098 = 1.0，TR-181 = 2.0）。
	specVersion string

	// srcIP 是本设备发起连接时绑定的**源 IP**（留空 = 系统默认）。
	//
	// 为什么压测要用它：ACS 用「来源 IP + User-Agent + 账号」做会话兜底指纹
	// （CPE 还没拿到 cookie 时的第一次 Inform 只能靠它）。一个进程里跑 N 台模拟设备
	// 时它们共用 127.0.0.1 和同一个 UA，会被 ACS 当成**同一个会话**，
	// 于是任务被串到别的设备上、大部分设备直接被 204 结束 —— 压出来的数字全是假的。
	// 真机各有各的管理 IP，所以压测里给每台设备绑一个 127.0.0.0/8 里的不同地址。
	srcIP string

	// cookie 是 ACS 回给我们的 session cookie（形如 session=xxx）。
	//
	// 真机（以及 TR-069 规范）的 CPE 会把 Set-Cookie 回传，ACS 靠它把一次会话里的
	// 多个 POST 串起来；不回传时 ACS 只能退回「来源 IP + User-Agent」指纹，
	// 于是同一台机器上并发的多台模拟设备会被当成**同一个会话**（压测时踩到过：
	// 10 台设备共用一个会话，大部分拿不到任务直接被 204 结束）。
	cookie string

	// extraParams > 0 时，在 X_HW_APDevice 子树下再合成这么多参数 ——
	// 用来mock 真机（华为 V271-20）那种「子设备子树有几千个参数」的负载：
	// 压测时要看的正是 ACS 枚举 + 写库的规模效应。
	extraParams int
}

func main() {
	s := &simulator{params: map[string]string{}, types: map[string]string{}, crCh: make(chan struct{}, 1)}

	var root string
	var crPort int
	var count, extraParams int
	var ignoreSet string
	var writeOnly string
	var diagDelay bool
	var noWAN bool
	var crUser, crPass string
	flag.StringVar(&s.acsURL, "acs", "http://127.0.0.1:7547/acs", "ACS 的 CWMP 地址")
	flag.StringVar(&s.user, "user", "", "CPE→ACS 认证账号")
	flag.StringVar(&s.pass, "pass", "", "CPE→ACS 认证密码")
	flag.StringVar(&s.cwmpVersion, "cwmp", "1.0", "CWMP 版本 1.0/1.1/1.2/1.3")
	flag.StringVar(&s.event, "event", "0 BOOTSTRAP", "首次上报的事件码")
	flag.BoolVar(&s.once, "once", false, "跑完一次会话就退出（适合脚本验收）")
	flag.DurationVar(&s.interval, "interval", 30*time.Second, "周期上报间隔（0 表示会话结束就退出）")
	flag.DurationVar(&s.stallAfter, "stall-after", 0,
		"启动这么多秒后停止周期上报（但仍响应 Connection Request），模拟「周期上报坏了但设备还在」")
	flag.StringVar(&s.manufacturer, "manufacturer", "SimVendor", "厂商")
	flag.StringVar(&s.oui, "oui", "001122", "OUI（6 位十六进制）")
	flag.StringVar(&s.productClass, "product-class", "SimRouter", "产品类")
	flag.StringVar(&s.serial, "serial", "ACSIM0000001", "序列号")
	flag.StringVar(&s.model, "model", "SimModel-X1", "型号名")
	flag.StringVar(&root, "dm", "098", "数据模型：098(TR-098) 或 181(TR-181)")
	flag.IntVar(&crPort, "cr-port", 0, "ConnectionRequest 监听端口（0 = 随机）")
	flag.StringVar(&ignoreSet, "ignore-set", "", "模拟“接受写入但不生效”的参数名子串（逗号分隔）")
	flag.StringVar(&writeOnly, "write-only", "", "模拟“能改不能读”的参数名子串（逗号分隔，写接受但读回为空）")
	flag.BoolVar(&diagDelay, "diag-delay", false, "ping 诊断改为异步：下次会话才出结果并带事件 8 DIAGNOSTICS COMPLETE")
	var pingNeedIface string
	flag.StringVar(&pingNeedIface, "ping-need-iface", "", "模拟「必须指定承载接口才出得去」：诊断时 Interface 不含该子串就全失败（留空则不启用）")
	var fttrOptical bool
	var fttrWireless, fttrWired string
	flag.BoolVar(&fttrOptical, "fttr-optical", false, "给 FTTR 子设备加光功率参数（模拟光纤组网子机）")
	var optical bool
	flag.BoolVar(&optical, "optical", false, "给主机加 PON 光功率参数（模拟光猫自己上报收/发光）")
	flag.StringVar(&fttrWireless, "fttr-wifi", "", "把哪些 FTTR 子设备做成无线组网（子设备序号，逗号分隔，如 1,3）")
	flag.StringVar(&fttrWired, "fttr-eth", "", "把哪些 FTTR 子设备做成有线组网（子设备序号，逗号分隔）")
	flag.IntVar(&s.fttr, "fttr", 0, "模拟 FTTR 子设备（从光猫）数量，0 表示没有")
	flag.IntVar(&count, "count", 1, "压测：一个进程模拟多少台设备（>1 时全部同时上报，序列号自动加序号）")
	flag.IntVar(&extraParams, "extra-params", 0,
		"压测：在 X_HW_APDevice 子树下再合成这么多参数（真机 V271-20 这里是 4484 个）")
	flag.BoolVar(&noWAN, "no-wan", false, "不模拟 WAN 连接对象（用于验证“没有就不显示”）")
	flag.StringVar(&crUser, "cr-user", "", "Connection Request 监听要求的用户名（留空则用设备参数里的）")
	flag.StringVar(&crPass, "cr-pass", "", "Connection Request 监听要求的密码")
	flag.Parse()
	s.diagDelay = diagDelay
	s.pingNeedIface = pingNeedIface
	s.fttrOptical = fttrOptical
	s.optical = optical
	s.fttrWireless = parseIntSet(fttrWireless)
	s.fttrWired = parseIntSet(fttrWired)
	s.noWAN = noWAN
	s.crUser, s.crPass = crUser, crPass
	s.extraParams = extraParams

	for _, part := range strings.Split(ignoreSet, ",") {
		if p := strings.TrimSpace(part); p != "" {
			s.ignoreSet = append(s.ignoreSet, p)
		}
	}
	for _, part := range strings.Split(writeOnly, ",") {
		if p := strings.TrimSpace(part); p != "" {
			s.writeOnly = append(s.writeOnly, p)
		}
	}

	rootPrefix := s.prepare(root, crPort)

	// 多设备模式：一个进程模拟 N 台设备（压测用）
	if count > 1 {
		runMany(s, count, crPort)
		return
	}

	log.Printf("CPE 模拟器启动 serial=%s dm=%s acs=%s 参数=%d 条",
		s.serial, rootPrefix, s.acsURL, len(s.params))
	s.runLoop()
}

// prepare 把数据模型根、参数表、ConnectionRequest 监听都准备好。
// 单设备与多设备模式共用（多设备模式里每台设备都会走一遍）。
func (s *simulator) prepare(root string, crPort int) string {
	rootPrefix := "InternetGatewayDevice."
	s.specVersion = "1.0"
	if strings.Contains(root, "181") {
		rootPrefix = "Device."
		s.specVersion = "2.0"
	}
	s.rootPrefix = rootPrefix
	if s.crNonce == "" {
		s.crNonce = fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	s.buildParams(rootPrefix, s.specVersion)

	// 起一个本地 HTTP 服务当 ConnectionRequestURL，并把它写进参数表
	crURL, err := s.startConnectionRequestServer(crPort)
	if err != nil {
		log.Printf("注意：无法启动 ConnectionRequest 监听：%v", err)
	}
	if crURL != "" {
		s.params[rootPrefix+"ManagementServer.ConnectionRequestURL"] = crURL
		s.types[rootPrefix+"ManagementServer.ConnectionRequestURL"] = "string"
	}
	return rootPrefix
}

// cloneDevice 复制一份设备：只换序列号，参数表重建（每台设备一份，互不干扰）。
// 其他配置（型号、数据模型、FTTR 子设备数…）原样继承。
func cloneDevice(tmpl *simulator, serial string) *simulator {
	dev := *tmpl
	dev.serial = serial
	dev.params = map[string]string{}
	dev.types = map[string]string{}
	dev.crCh = make(chan struct{}, 1)
	dev.stalled = false
	dev.pendingDiag = ""
	dev.cookie = "" // 每台设备一份会话 cookie
	dev.crNonce = fmt.Sprintf("%016x", time.Now().UnixNano()+int64(len(serial)))
	dev.buildParams(dev.rootPrefix, dev.specVersion)
	dev.addExtraParams()
	return &dev
}

// runLoop 是单设备的主循环（周期上报 + 响应 Connection Request），
// 与多设备模式里的单台设备共用。
func (s *simulator) runLoop() {
	startedAt := time.Now()
	event := s.event
	for round := 0; ; round++ {
		if err := s.runSession(event); err != nil {
			log.Printf("会话出错：%v", err)
		}
		if s.once || s.interval <= 0 {
			log.Printf("（-once/interval=0）结束")
			return
		}
		// 等下一个周期，或者被 Connection Request 唤醒
		// -stall-after：到点后不再做周期上报（只等 Connection Request），
		// 用来验证 ACS 的离线探测确实能区分「设备死了」和「只是不按周期报了」
		if s.stallAfter > 0 && !s.stalled && time.Since(startedAt) >= s.stallAfter {
			s.stalled = true
			log.Printf("到达 -stall-after（%s）：停止周期上报，但仍会响应 Connection Request",
				s.stallAfter)
		}
		wait := s.interval
		if s.stalled {
			wait = 24 * time.Hour // 相当于不再周期上报
		}
		select {
		case <-s.crCh:
			event = "6 CONNECTION REQUEST"
			log.Printf("收到连接请求，立刻回连（event=%s）", event)
			time.Sleep(200 * time.Millisecond)
		case <-time.After(wait):
			if s.stalled {
				// 停报状态：不推进运行时长、也不上报，只挂着等 Connection Request
				continue
			}
			s.tick(s.rootPrefix)
			event = "2 PERIODIC"
		}
	}
}

// runMany 是压测模式：一个进程里并发跑 count 台设备。
//
// 「大规模断电恢复」的场景就是所有设备**同时**发 1 BOOT：这里 N 台设备在同一瞬间
// 起步（只在起 goroutine 上有一点调度抖动），跑完各自统计耗时分布，
// 用来观察 ACS 的并发上限在哪。
//
// -once / interval<=0 时每台只跑一次会话（上电报文）；否则按周期一直跑（持续负载），
// 由外部脚本控制跑多久。
func runMany(tmpl *simulator, count, crPort int) {
	log.Printf("压测模式：%d 台设备同时上报（事件 %q，dm=%s，FTTR 子设备 %d，额外参数 %d）",
		count, tmpl.event, tmpl.rootPrefix, tmpl.fttr, tmpl.extraParams)
	if crPort != 0 {
		log.Printf("注意：多设备模式不为每台设备起 ConnectionRequest 监听（%d 个监听没必要），"+
			"压测里设备是主动上报的", count)
	}

	durs := make([]time.Duration, count)
	errs := make([]error, count)
	devices := make([]*simulator, count)
	for i := 0; i < count; i++ {
		devices[i] = cloneDevice(tmpl, fmt.Sprintf("%s%05d", tmpl.serial, i+1))
		// 每台设备一个不同的源 IP（127.0.0.1 起，够用 6 万多台）
		devices[i].srcIP = fmt.Sprintf("127.0.%d.%d", (i/254)%256, i%254+1)
		// 多设备模式不起 ConnectionRequest 监听，但真机一定会报 ConnectionRequestURL；
		// 补一个（指向它自己的源 IP，没人监听）—— 压测里设备是主动上报的，用不到它
		if _, ok := devices[i].params[devices[i].rootPrefix+"ManagementServer.ConnectionRequestURL"]; !ok {
			devices[i].params[devices[i].rootPrefix+"ManagementServer.ConnectionRequestURL"] =
				fmt.Sprintf("http://%s:7547/", devices[i].srcIP)
			devices[i].types[devices[i].rootPrefix+"ManagementServer.ConnectionRequestURL"] = "string"
		}
	}

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			dev := devices[idx]
			t0 := time.Now()
			if tmpl.once || tmpl.interval <= 0 {
				errs[idx] = dev.runSession(dev.event)
			} else {
				dev.runLoop()
			}
			durs[idx] = time.Since(t0)
		}(i)
	}
	wg.Wait()

	ok := 0
	var sum, worst, best time.Duration
	best = time.Hour
	for i := range durs {
		if errs[i] == nil {
			ok++
		}
		sum += durs[i]
		if durs[i] > worst {
			worst = durs[i]
		}
		if durs[i] < best {
			best = durs[i]
		}
	}
	sorted := append([]time.Duration(nil), durs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	median := sorted[len(sorted)/2]
	log.Printf("压测结果：%d/%d 台成功｜总耗时 %s｜单台 最快 %s / 中位 %s / 最慢 %s｜平均 %s",
		ok, count, time.Since(start).Round(time.Millisecond),
		best.Round(time.Millisecond), median.Round(time.Millisecond), worst.Round(time.Millisecond),
		(sum / time.Duration(count)).Round(time.Millisecond))

	failed := 0
	for i, err := range errs {
		if err != nil {
			if failed < 5 {
				log.Printf("  失败样例：%s → %v", devices[i].serial, err)
			}
			failed++
		}
	}
	if failed > 0 {
		log.Printf("  失败合计 %d 台", failed)
	}
}

// addExtraParams 在 X_HW_APDevice 子树下合成 extraParams 个参数。
//
// 为什么要这个：真机 V271-20（FTTR 主机）的 X_HW_APDevice 子树里有 4484 个参数，
// ACS 纳管时要枚举+取值+写库。默认的模拟器只有几十个参数，
// 压不出「几千参数 × N 台设备同时上报」这种真实负载。
func (s *simulator) addExtraParams() {
	if s.extraParams <= 0 {
		return
	}
	root := s.rootPrefix + "X_HW_APDevice."
	// 名字形状照真机那套（子设备 → 下面的对象 → 参数），让 ACS 的枚举走同样的层级
	perSub := 600
	for i := 0; i < s.extraParams; i++ {
		sub := (i / perSub) + 1
		k := i % perSub
		name := fmt.Sprintf("%s%d.SubDevice.%d.Param%04d", root, sub, sub, k)
		s.params[name] = fmt.Sprintf("v%d", i)
		s.types[name] = "string"
	}
}

// tick 模拟运行时长在增长。
func (s *simulator) tick(root string) {
	up := root + "DeviceInfo.UpTime"
	if v, ok := s.params[up]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			s.params[up] = strconv.Itoa(n + int(s.interval.Seconds()))
		}
	}
}

// buildParams 造一份「最基本的设备信息」。
func (s *simulator) buildParams(root, specVersion string) {
	di := root + "DeviceInfo."
	ms := root + "ManagementServer."
	wan := root + "WANDevice.1.WANConnectionDevice.1.WANIPConnection.1."

	set := func(name, val, typ string) {
		s.params[name] = val
		s.types[name] = typ
	}

	set(di+"Manufacturer", s.manufacturer, "string")
	set(di+"ManufacturerOUI", s.oui, "string")
	set(di+"ModelName", s.model, "string")
	set(di+"Description", "TR-069 CPE Simulator", "string")
	set(di+"ProductClass", s.productClass, "string")
	set(di+"SerialNumber", s.serial, "string")
	set(di+"HardwareVersion", "V1.0", "string")
	set(di+"SoftwareVersion", "1.0.0-sim", "string")
	set(di+"SpecVersion", specVersion, "string")
	set(di+"ProvisioningCode", "0000", "string")
	set(di+"UpTime", "3600", "unsignedInt")
	set(di+"FirstUseDate", "2026-01-01T00:00:00Z", "dateTime")

	set(ms+"URL", s.acsURL, "string")
	set(ms+"Username", s.user, "string")
	set(ms+"PeriodicInformEnable", "1", "boolean")
	set(ms+"PeriodicInformInterval", strconv.Itoa(int(s.interval.Seconds())), "unsignedInt")
	set(ms+"ConnectionRequestUsername", "cpe-cr", "string")
	// 真机上这个参数存在、但设备不回读（返回空串），所以留空
	set(ms+"ConnectionRequestPassword", "", "string")
	set(ms+"ParameterKey", "", "string")

	if !s.noWAN {
		set(wan+"ExternalIPAddress", "203.0.113.7", "string")
		set(wan+"ConnectionStatus", "Connected", "string")
		set(wan+"Name", "1_INTERNET_R_VID_", "string")
		set(wan+"SubnetMask", "255.255.255.0", "string")
		set(wan+"DefaultGateway", "203.0.113.1", "string")
		set(wan+"MACAddress", "00:11:22:33:44:AA", "string")
		set(wan+"AddressingType", "DHCP", "string")
		set(wan+"NATEnabled", "1", "boolean")
		set(wan+"ConnectionType", "IP_Routed", "string")
		set(wan+"Uptime", "12345", "unsignedInt")
		set(wan+"X_HW_VLAN", "41", "unsignedInt")
		set(wan+"X_HW_SERVICELIST", "INTERNET", "string")
	}

	// PON 接入的光功率（可选，-optical 打开）：模拟真机 V271-20（PON、联通定制）的怪样子 ——
	// 读数在一个**名字拼错的私有对象**下（X_GponInterafceConfig），
	// 而旁边另一个对象报的是没换算的原始值（254 / 10000）。
	// 界面必须选中前者，后者不能被当成 dBm 显示。
	// 名字与真机实测一致（2026-09-30）。
	if s.optical {
		pon := root + "WANDevice.1.X_GponInterafceConfig."
		set(pon+"RXPower", "-15", "int")
		set(pon+"TXPower", "2", "int")
		set(pon+"TransceiverTemperature", "43", "int")
		set(root+"WANDevice.1.X_CU_WANEdgeONTPONInterfaceConfig.OpticalTransceiver.RXPower", "254", "unsignedInt")
		set(root+"WANDevice.1.X_CU_WANEdgeONTPONInterfaceConfig.OpticalTransceiver.TXPower", "10000", "unsignedInt")
	}

	// 无线参数。
	// 特意把实例号做成 **1 和 5**（不是 1 和 2）—— 真机（华为 HN8145X6N）就是这么编号的，
	// 写死 1/2 会读空。
	if root == "Device." {
		// TR-181：Radio / SSID / AccessPoint 分开在不同对象下
		set("Device.WiFi.Radio.1.Enable", "1", "boolean")
		set("Device.WiFi.Radio.1.Status", "Up", "string")
		set("Device.WiFi.Radio.1.Channel", "36", "unsignedInt")
		set("Device.WiFi.Radio.1.OperatingFrequencyBand", "5GHz", "string")
		set("Device.WiFi.SSID.1.SSID", "SimWiFi", "string")
		set("Device.WiFi.AccessPoint.1.AssociatedDeviceNumberOfEntries", "2", "unsignedInt")
		set("Device.WiFi.AccessPoint.1.AssociatedDevice.1.MACAddress", "02:00:00:00:00:A1", "string")
		set("Device.WiFi.AccessPoint.1.AssociatedDevice.1.IPAddress", "192.168.1.101", "string")
		set("Device.WiFi.AccessPoint.1.AssociatedDevice.1.SignalStrength", "-41", "string")
		set("Device.WiFi.AccessPoint.1.AssociatedDevice.2.MACAddress", "02:00:00:00:00:A2", "string")
		set("Device.WiFi.AccessPoint.1.AssociatedDevice.2.IPAddress", "192.168.1.102", "string")
		set("Device.WiFi.AccessPoint.1.AssociatedDevice.2.SignalStrength", "-55", "string")
		// 主机列表（TR-181）：终端名只能从这里按 MAC 对出来
		set("Device.Hosts.Host.1.MACAddress", "02:00:00:00:00:A1", "string")
		set("Device.Hosts.Host.1.IPAddress", "192.168.1.101", "string")
		set("Device.Hosts.Host.1.HostName", "Sim-Dev-A1", "string")
		set("Device.Hosts.Host.1.Active", "1", "boolean")
		set("Device.Hosts.HostNumberOfEntries", "1", "unsignedInt")
		set("Device.WiFi.AccessPoint.1.SSIDAdvertisementEnabled", "1", "boolean")
	} else {
		wlan := root + "LANDevice.1.WLANConfiguration."
		set(wlan+"1.SSID", "SimWiFi", "string")
		set(wlan+"1.Enable", "1", "boolean")
		set(wlan+"1.RadioEnabled", "1", "boolean")
		set(wlan+"1.Status", "Up", "string")
		set(wlan+"1.Channel", "6", "unsignedInt")
		set(wlan+"1.Standard", "11ax", "string")
		set(wlan+"1.BSSID", "00:11:22:33:44:55", "string")
		set(wlan+"1.BeaconType", "11i", "string")
		set(wlan+"1.WPAEncryptionModes", "AESEncryption", "string")
		set(wlan+"1.TotalAssociations", "2", "unsignedInt")
		set(wlan+"1.X_HW_RFBand", "2.4GHz", "string")
		// 关联终端：主机自己这 2 台（同时给一条**残留空行**：网关真机会留下上一次读的空行，
		// 条目数必须以 AssociatedDeviceNumberOfEntries 为准，不然界面上会多出幽灵终端）。
		set(wlan+"1.AssociatedDeviceNumberOfEntries", "2", "unsignedInt")
		set(wlan+"1.AssociatedDevice.1.AssociatedDeviceMACAddress", "02:00:00:00:00:B1", "string")
		set(wlan+"1.AssociatedDevice.1.AssociatedDeviceIPAddress", "192.168.1.11", "string")
		set(wlan+"1.AssociatedDevice.1.RSSI", "-41", "string")
		set(wlan+"1.AssociatedDevice.1.SNR", "43", "string")
		set(wlan+"1.AssociatedDevice.1.RxRate", "72", "string")
		set(wlan+"1.AssociatedDevice.1.TxRate", "65", "string")
		set(wlan+"1.AssociatedDevice.1.Uptime", "3600", "string")
		set(wlan+"1.AssociatedDevice.2.AssociatedDeviceMACAddress", "02:00:00:00:00:B2", "string")
		set(wlan+"1.AssociatedDevice.2.AssociatedDeviceIPAddress", "192.168.1.12", "string")
		set(wlan+"1.AssociatedDevice.2.RSSI", "-58", "string")
		set(wlan+"1.AssociatedDevice.2.FrequencyWidth", "40MHz", "string")
		set(wlan+"1.AssociatedDevice.3.AssociatedDeviceMACAddress", "02:00:00:00:00:FF", "string")
		set(wlan+"1.AssociatedDevice.3.AssociatedDeviceIPAddress", "0.0.0.0", "string")
		// 下面这几个是「可编辑 / 给下拉框提供候选值」用的
		set(wlan+"1.KeyPassphrase", "", "string")
		// WPA/WPA2-PSK 真正的密码位；两都存在时 ACS 应该优先选这个（真机验证过）
		set(wlan+"1.PreSharedKey.1.KeyPassphrase", "", "string")
		set(wlan+"1.IEEE11iEncryptionModes", "AESEncryption", "string")
		set(wlan+"1.IEEE11iAuthenticationMode", "PSKAuthentication", "string")
		set(wlan+"1.TransmitPower", "100", "unsignedInt")
		set(wlan+"1.TransmitPowerSupported", "20,40,60,80,100", "string")
		set(wlan+"1.PossibleChannels", "1,2,3,4,5,6,7,8,9,10,11,12,13", "string")

		set(wlan+"5.SSID", "SimWiFi-5G", "string")
		set(wlan+"5.Enable", "1", "boolean")
		set(wlan+"5.RadioEnabled", "0", "boolean")
		set(wlan+"5.Status", "Disabled", "string")
		set(wlan+"5.Channel", "0", "unsignedInt")
		set(wlan+"5.Standard", "11ax", "string")
		set(wlan+"5.BSSID", "00:11:22:33:44:56", "string")
		set(wlan+"5.BeaconType", "11i", "string")
		set(wlan+"5.WPAEncryptionModes", "AESEncryption", "string")
		set(wlan+"5.TotalAssociations", "1", "unsignedInt")
		set(wlan+"5.X_HW_RFBand", "5GHz", "string")
		set(wlan+"5.AssociatedDeviceNumberOfEntries", "1", "unsignedInt")
		set(wlan+"5.AssociatedDevice.1.AssociatedDeviceMACAddress", "02:00:00:00:00:C1", "string")
		set(wlan+"5.AssociatedDevice.1.AssociatedDeviceIPAddress", "192.168.1.21", "string")
		set(wlan+"5.AssociatedDevice.1.RSSI", "-52", "string")
		set(wlan+"5.AssociatedDevice.1.FrequencyWidth", "160MHz", "string")

		// 射频对象（真机华为 HN8145X6N / V271-20 都有）：`LANDevice.1.WiFi.Radio.{i}`，
		// 编号是 1/2，跟 SSID 实例号（1/5）**对不上**。以前按实例号硬合并，
		// 会凭空多出一行「5G ｜ - ｜ 开 ｜ -」的无效显示（用户 2026-09-30 截图就是这个）。
		radio := root + "LANDevice.1.WiFi.Radio."
		set(radio+"1.Enable", "1", "boolean")
		set(radio+"1.OperatingFrequencyBand", "2.4GHz", "string")
		set(radio+"2.Enable", "1", "boolean")
		set(radio+"2.OperatingFrequencyBand", "5GHz", "string")

		// 主机列表（TR-098）：终端名靠它按 MAC 对出来（关联终端表里通常没有名字）
		hosts := root + "LANDevice.1.Hosts."
		set(hosts+"Host.1.MACAddress", "02:00:00:00:00:B1", "string")
		set(hosts+"Host.1.IPAddress", "192.168.1.11", "string")
		set(hosts+"Host.1.HostName", "Sim-Laptop", "string")
		set(hosts+"Host.1.Active", "1", "boolean")
		set(hosts+"Host.1.AddressSource", "DHCP", "string")
		set(hosts+"Host.2.MACAddress", "02:00:01:00:00:01", "string")
		set(hosts+"Host.2.IPAddress", "10.0.1.1", "string")
		set(hosts+"Host.2.HostName", "Sim-Phone", "string")
		set(hosts+"Host.2.Active", "1", "boolean")
		set(hosts+"HostNumberOfEntries", "2", "unsignedInt")
		// 另一台终端把名字写在自己的行上（验证「行自带名字」这条路）
		set(wlan+"1.AssociatedDevice.2.X_HW_AssociatedDevicedescriptions", "Sim-Camera", "string")
	}

	// ping 诊断对象（TR-069 标准的 IPPingDiagnostics）。
	// 真机上两台光猫都有这 12 个参数，字段名与标准完全一致。
	ping := root + "IPPingDiagnostics."
	set(ping+"DiagnosticsState", "None", "string")
	set(ping+"Host", "", "string")
	set(ping+"NumberOfRepetitions", "4", "unsignedInt")
	set(ping+"Timeout", "10000", "unsignedInt")
	set(ping+"DataBlockSize", "56", "unsignedInt")
	set(ping+"DSCP", "0", "unsignedInt")
	set(ping+"Interface", "", "string")
	set(ping+"SuccessCount", "0", "unsignedInt")
	set(ping+"FailureCount", "0", "unsignedInt")
	set(ping+"MinimumResponseTime", "0", "unsignedInt")
	set(ping+"AverageResponseTime", "0", "unsignedInt")
	set(ping+"MaximumResponseTime", "0", "unsignedInt")

	// FTTR 子设备（可选）。
	//
	// 实例号**故意做成不连续的**（1/4/7...）—— 真机上就是 1/2/4，
	// 写死连续编号的代码在真机上是会踩坑的。
	for i := 1; i <= s.fttr; i++ {
		inst := i*3 - 2
		ap := root + fmt.Sprintf("X_HW_APDevice.%d.", inst)
		set(ap+"SerialNumber", fmt.Sprintf("SUBSN%06d", i), "string")
		set(ap+"DeviceType", "K251-20", "string")
		set(ap+"APMacAddr", fmt.Sprintf("02:00:00:00:00:%02X", i), "string")
		set(ap+"ApOnlineFlag", "1", "string")
		set(ap+"DeviceStatus", "OK", "string")
		set(ap+"ManufacturerOUI", "00259E", "string")
		set(ap+"SoftwareVersion", "V5R023C10S300", "string")
		set(ap+"HardwareVersion", "3B78.A", "string")
		set(ap+"CurrentChannel", "1,36", "string")
		set(ap+"SupportedRFBand", "2.4G,5G", "string")
		set(ap+"SignalIntensity", "0", "string")
		set(ap+"SyncStatus", "3", "string")
		set(ap+"UpTime", "361:36:38", "string")
		set(ap+"WorkingMode", "repeater", "string")

		// 组网方式（验证「无线/有线组网不显示光功率」）：
		// 无线 → WorkingMode 写成 wifi、SignalIntensity 给个真实信号强度；
		// 有线 → WorkingMode 写成 eth。默认（repeater + 信号 0）留给真机那种
		// “设备自报的取值我们归不了一类”的情形。
		if s.fttrWireless[i] {
			set(ap+"WorkingMode", "wifi", "string")
			set(ap+"SignalIntensity", "-45", "string")
			set(ap+"SupportedWorkingMode", "wifi,eth", "string")
		} else if s.fttrWired[i] {
			set(ap+"WorkingMode", "eth", "string")
			set(ap+"SupportedWorkingMode", "eth,wifi", "string")
		} else {
			set(ap+"SupportedWorkingMode", "repeater", "string")
		}
		set(ap+"InternetAccessMode", "DHCP", "string")

		// 光功率（可选）：注意**无论哪种组网都加上**，这样才能验证
		// “无线组网即使有参数也不显示光功率”那条规则。
		if s.fttrOptical {
			set(ap+"X_HW_RxPower", fmt.Sprintf("-%.1f", 18.0+float64(i)), "string")
			set(ap+"X_HW_TxPower", "2.5", "string")
		}

		// 子设备各自的无线配置 + 关联终端（真机上每台子光猫都带自己的 WLANConfiguration.，
		// 终端就挂在这里 —— 主机那边一台都没有）。
		// 台数按子设备序号递减，方便验收断言：子机 1 → 2.4G 2 台 / 5G 1 台，
		// 子机 2 → 2.4G 1 台，子机 3 → 0 台。
		n24 := 3 - i
		if n24 < 0 {
			n24 = 0
		}
		n5 := 0
		if i == 1 {
			n5 = 1
		}
		subWlan := func(j int, band, ssid string, n int) {
			w := ap + fmt.Sprintf("WLANConfiguration.%d.", j)
			set(w+"SSID", ssid, "string")
			set(w+"Enable", "1", "boolean")
			ch := "6"
			if band == "5G" {
				ch = "36"
			}
			set(w+"Channel", ch, "unsignedInt")
			set(w+"Standard", "11be", "string")
			set(w+"X_HW_RFBand", band, "string")
			set(w+"AssociatedDeviceNumberOfEntries", strconv.Itoa(n), "unsignedInt")
			for k := 1; k <= n; k++ {
				c := w + fmt.Sprintf("AssociatedDevice.%d.", k)
				set(c+"AssociatedDeviceMACAddress", fmt.Sprintf("02:00:%02X:00:00:%02X", i, k), "string")
				set(c+"AssociatedDeviceIPAddress", fmt.Sprintf("10.0.%d.%d", i, k), "string")
				set(c+"RSSI", strconv.Itoa(-(50 + 8*k + 4*i)), "string")
				// 设备自报的信号质量（0..100）：界面上的百分比优先用它，
				// 而不是我们拿 RSSI 换算 —— 各家对“几格”的口径不一样。
				set(c+"SingalQuality", strconv.Itoa(20+20*k+15*i), "string")
				set(c+"SNR", strconv.Itoa(40-i), "string")
				set(c+"RxRate", "573", "string")
				set(c+"TxRate", "433", "string")
				set(c+"FrequencyWidth", "80MHz", "string")
				set(c+"Uptime", "86400", "string")
			}
		}
		subWlan(1, "2.4G", fmt.Sprintf("SimWiFi-SUB%d", i), n24)
		subWlan(2, "5G", fmt.Sprintf("SimWiFi-SUB%d-5G", i), n5)
	}
}

// startConnectionRequestServer 起一个假的 CPE 侧 HTTP 服务。
// 返回可用的 URL（用与 ACS 通信时实际使用的本机 IP 拼出来，避免写死 127.0.0.1）。
func (s *simulator) startConnectionRequestServer(port int) (string, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return "", err
	}
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 模仿真机：只要设备上配了 ConnectionRequest 账号密码，就要求 HTTP Digest。
		// 注意这里是**独立实现**一遍校验（不调 ACS 那边的代码），
		// 这样才真的能验证客户端的 Digest 算得对不对。
		user, pass := s.crUser, s.crPass
		if user == "" {
			user = s.params[s.rootPrefix+"ManagementServer.ConnectionRequestUsername"]
			pass = s.params[s.rootPrefix+"ManagementServer.ConnectionRequestPassword"]
		}
		if user != "" {
			if !checkDigest(r.Header.Get("Authorization"), r.Method, r.URL.RequestURI(), user, pass, s.crNonce) {
				w.Header().Set("WWW-Authenticate",
					`Digest realm="SimHomeGateway",nonce="`+s.crNonce+`",qop="auth",algorithm="MD5"`)
				w.WriteHeader(http.StatusUnauthorized)
				log.Printf("Connection Request 认证失败，已回 401 挑战")
				return
			}
			log.Printf("Connection Request 认证通过（Digest，用户 %s）", user)
		} else {
			log.Printf("收到 Connection Request（设备没配账号，不要求认证）")
		}
		w.WriteHeader(http.StatusOK)
		select {
		case s.crCh <- struct{}{}:
		default:
		}
	})
	go func() { _ = http.Serve(ln, httpMux) }()

	ip := localIPFor(s.acsURL)
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	return fmt.Sprintf("http://%s:%s/", ip, p), nil
}

// localIPFor 通过一次 UDP「连接」探测出到达 ACS 时用的本机 IP。
func localIPFor(acsURL string) string {
	u := strings.TrimPrefix(strings.TrimPrefix(acsURL, "http://"), "https://")
	if i := strings.IndexAny(u, "/"); i >= 0 {
		u = u[:i]
	}
	host, port, err := net.SplitHostPort(u)
	if err != nil {
		host, port = u, "80"
	}
	conn, err := net.Dial("udp", net.JoinHostPort(host, port))
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return "127.0.0.1"
}

// ---------- 会话流程 ----------

func (s *simulator) runSession(event string) error {
	// 上一轮请求过的 ping 诊断：真机是跑完之后**单独发一次 Inform** 把结果带回来的，
	// 所以这里把结果补上，并把事件改成 8 DIAGNOSTICS COMPLETE。
	if s.pendingDiag != "" {
		prefix := s.pendingDiag
		s.pendingDiag = ""
		n := 4
		if v, err := strconv.Atoi(s.params[prefix+"NumberOfRepetitions"]); err == nil && v > 0 {
			n = v
		}
		s.finishDiagnostic(prefix, n)
		event = "8 DIAGNOSTICS COMPLETE"
	}

	// 1) Inform
	resp, status, err := s.post(s.envelope(randID(), s.informBody(event)))
	if err != nil {
		return fmt.Errorf("发送 Inform 失败: %w", err)
	}
	if status/100 != 2 {
		return fmt.Errorf("Inform 返回了 %d: %s", status, truncate(resp))
	}
	log.Printf("已上报 Inform（event=%s），收到 %d 字节响应", event, len(resp))

	// 2) 空 POST 领任务 → 执行 → 回结果 → 直到 204
	body := ""
	for round := 0; round < 200; round++ {
		resp, status, err = s.post(body)
		if err != nil {
			return fmt.Errorf("会话中断: %w", err)
		}
		if status == http.StatusNoContent || len(bytes.TrimSpace(resp)) == 0 {
			log.Printf("会话结束（HTTP %d）", status)
			return nil
		}

		env, err := parseEnvelope(resp)
		if err != nil {
			return fmt.Errorf("解析 ACS 报文失败: %w（原文: %s）", err, truncate(resp))
		}
		if env.Method == nil {
			return fmt.Errorf("ACS 报文里没有 RPC")
		}
		log.Printf("ACS 下发：%s", env.Method.Local)

		reply, err := s.handle(env.Method)
		if err != nil {
			return err
		}
		if reply == "" {
			log.Printf("会话由 CPE 侧结束")
			return nil
		}
		body = s.envelope(env.ID, reply)
	}
	return fmt.Errorf("会话轮次过多，疑似死循环")
}

type envelopeInfo struct {
	ID     string
	Method *cwmp.Node
}

func parseEnvelope(b []byte) (*envelopeInfo, error) {
	root, err := cwmp.ParseXML(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	env, err := cwmp.ParseEnvelope(root)
	if err != nil {
		return nil, err
	}
	return &envelopeInfo{ID: env.ID, Method: env.Method}, nil
}

func (s *simulator) post(body string) ([]byte, int, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, s.acsURL, rd)
	if s != nil && s.cookie != "" {
		req.Header.Set("Cookie", s.cookie)
	}
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("User-Agent", "cpesim/1.0 UPnP/1.0")
	if s.user != "" {
		req.SetBasicAuth(s.user, s.pass)
	}
	if body != "" {
		req.ContentLength = int64(len(body))
	} else {
		req.ContentLength = 0
	}

	client := &http.Client{Timeout: 30 * time.Second}
	if s != nil && s.srcIP != "" {
		if ip := net.ParseIP(s.srcIP); ip != nil {
			client = &http.Client{
				Timeout: 30 * time.Second,
				Transport: &http.Transport{
					DialContext: (&net.Dialer{
						Timeout:   10 * time.Second,
						LocalAddr: &net.TCPAddr{IP: ip},
					}).DialContext,
				},
			}
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	// 记下会话 cookie，下一次 POST 带上（真机就是这么做的）
	if c := resp.Header.Get("Set-Cookie"); c != "" {
		if kv := strings.SplitN(strings.TrimSpace(strings.Split(c, ";")[0]), "=", 2); len(kv) == 2 && kv[1] != "" {
			s.cookie = kv[0] + "=" + kv[1]
		}
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// ---------- 处理 ACS 下发的 RPC ----------

func (s *simulator) handle(m *cwmp.Node) (string, error) {
	switch m.Local {
	case "Fault":
		f := cwmp.ParseFault(m)
		return "", fmt.Errorf("ACS 返回 Fault: code=%d msg=%s", f.Code, f.String)
	case "GetParameterValues":
		return s.getParameterValues(m), nil
	case "GetParameterNames":
		return s.getParameterNames(m), nil
	case "SetParameterValues":
		return s.setParameterValues(m), nil
	case "GetRPCMethods":
		return s.getRPCMethods(), nil
	case "Reboot":
		log.Printf("收到 Reboot（CommandKey=%s），模拟重启", m.ChildText("CommandKey"))
		return `<cwmp:RebootResponse/>`, nil
	case "Download":
		log.Printf("收到 Download（URL=%s），本模拟器不支持", m.ChildText("URL"))
		return faultBody(9001, "Download not supported by simulator"), nil
	default:
		log.Printf("不支持的 RPC：%s，回 9000", m.Local)
		return faultBody(9000, "Method not supported"), nil
	}
}

// getParameterValues 支持两种情况：完整参数名，以及以 "." 结尾的部分路径（返回整棵子树）。
func (s *simulator) getParameterValues(m *cwmp.Node) string {
	reqs := m.Child("ParameterNames")
	var out []cwmp.ParamValue
	seen := map[string]bool{}

	if reqs != nil {
		for _, child := range reqs.Kids {
			name := child.Trimmed()
			if name == "" {
				continue
			}
			if strings.HasSuffix(name, ".") {
				for k := range s.params {
					if strings.HasPrefix(k, name) && !seen[k] {
						seen[k] = true
						out = append(out, s.pv(k))
					}
				}
				continue
			}
			if _, ok := s.params[name]; ok && !seen[name] {
				seen[name] = true
				out = append(out, s.pv(name))
			}
		}
	}

	var b strings.Builder
	b.WriteString(`<cwmp:GetParameterValuesResponse><ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[`)
	b.WriteString(strconv.Itoa(len(out)))
	b.WriteString(`]">`)
	for _, p := range out {
		t := p.Type
		if t == "" {
			t = "string"
		}
		b.WriteString(`<ParameterValueStruct><Name>` + esc(p.Name) + `</Name>`)
		b.WriteString(`<Value xsi:type="xsd:` + esc(t) + `">` + esc(p.Value) + `</Value>`)
		b.WriteString(`</ParameterValueStruct>`)
	}
	b.WriteString(`</ParameterList></cwmp:GetParameterValuesResponse>`)
	return b.String()
}

func (s *simulator) pv(name string) cwmp.ParamValue {
	t := s.types[name]
	if t == "" {
		t = "string"
	}
	return cwmp.ParamValue{Name: name, Value: s.params[name], Type: t}
}

func (s *simulator) getParameterNames(m *cwmp.Node) string {
	path := m.ChildText("ParameterPath")
	nextLevel := m.ChildText("NextLevel") == "1"

	var names []string
	seen := map[string]bool{}
	for k := range s.params {
		if path != "" && !strings.HasPrefix(k, path) {
			continue
		}
		var n string
		if nextLevel {
			// 只返回下一层：把剩下的部分截到第一个 "."
			rest := strings.TrimPrefix(k, path)
			if i := strings.Index(rest, "."); i >= 0 {
				n = path + rest[:i+1]
			} else {
				n = k
			}
		} else {
			n = k
		}
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}

	var b strings.Builder
	b.WriteString(`<cwmp:GetParameterNamesResponse><ParameterList soap-enc:arrayType="cwmp:ParameterInfoStruct[`)
	b.WriteString(strconv.Itoa(len(names)))
	b.WriteString(`]">`)
	for _, n := range names {
		writable := "0"
		if !strings.HasSuffix(n, ".") {
			writable = "1"
		}
		b.WriteString(`<ParameterInfoStruct><Name>` + esc(n) + `</Name><Writable>` + writable + `</Writable></ParameterInfoStruct>`)
	}
	b.WriteString(`</ParameterList></cwmp:GetParameterNamesResponse>`)
	return b.String()
}

func (s *simulator) setParameterValues(m *cwmp.Node) string {
	key := m.ChildText("ParameterKey")
	vals := cwmp.ParseParamValues(m.Child("ParameterList"))
	for _, v := range vals {
		if s.shouldIgnore(v.Name) {
			// 真机行为：回 Status=0 说“接受了”，但值不变
			log.Printf("  设置 %s = %s （本次故意不生效，模拟真机静默失效）", v.Name, v.Value)
			continue
		}
		if s.matchAny(s.writeOnly, v.Name) {
			// 真机行为（如 WiFi 密码）：接受写入，但读回永远是空串
			log.Printf("  设置 %s （能改不能读，接受但不落入可读值）", v.Name)
			continue
		}
		log.Printf("  设置 %s = %s", v.Name, v.Value)
		s.params[v.Name] = v.Value
		s.types[v.Name] = v.Type

		// ping 诊断：标准行为是把 DiagnosticsState 置 Requested，设备自己去跑
		if strings.HasSuffix(v.Name, "IPPingDiagnostics.DiagnosticsState") && v.Value == "Requested" {
			s.startDiagnostic(v.Name)
		}
	}
	_ = key
	return `<cwmp:SetParameterValuesResponse><Status>0</Status></cwmp:SetParameterValuesResponse>`
}

// startDiagnostic 响应一次 ping 诊断请求。
//
// 同步模式（默认）：当场算出结果，DiagnosticsState 直接置 Complete ——
// ACS 在同一次会话里读就能拿到，流程最短。
// 异步模式（-diag-delay）：保持 Requested，下次会话再置 Complete 并带事件 8，
// 这才是真机的节奏。
func (s *simulator) startDiagnostic(name string) {
	prefix := strings.TrimSuffix(name, "DiagnosticsState")
	host := s.params[prefix+"Host"]
	n := 4
	if v, err := strconv.Atoi(s.params[prefix+"NumberOfRepetitions"]); err == nil && v > 0 {
		n = v
	}
	log.Printf("开始 ping 诊断：host=%s count=%d interface=%q（diag-delay=%v）",
		host, n, s.params[prefix+"Interface"], s.diagDelay)
	if s.diagDelay {
		s.pendingDiag = prefix
		return
	}
	s.finishDiagnostic(prefix, n)
}

// finishDiagnostic 把诊断结果写进参数（模拟 ping 已完成）。
//
// 为了让“成功/失败/延时可重现”，结果由 host 的字符和哈希决定，而不是随机数 ——
// 验收脚本需要能断言具体数字。
func (s *simulator) finishDiagnostic(prefix string, n int) {
	fail := 0
	if strings.Contains(prefix, "never") || s.params[prefix+"Host"] == "" {
		fail = n
	}
	// 模拟「设备自己选的出口出不去」：没指定承载接口（或指定得不对）就是全失败，
	// 而且**当场**返回 —— 真机上就是这样（没有路由，包根本没发出去）。
	if s.pingNeedIface != "" && !strings.Contains(s.params[prefix+"Interface"], s.pingNeedIface) {
		log.Printf("  ping 全失败：承载接口 %q 不含 %q", s.params[prefix+"Interface"], s.pingNeedIface)
		fail = n
	}
	ok := n - fail
	rtt := 10 + len(s.params[prefix+"Host"])%20 // 10..29，可重现
	if ok == 0 {
		rtt = 0 // 一个包都没通：延时没有意义（真机就是 0）
	}
	s.params[prefix+"DiagnosticsState"] = "Complete"
	s.params[prefix+"SuccessCount"] = strconv.Itoa(ok)
	s.params[prefix+"FailureCount"] = strconv.Itoa(fail)
	s.params[prefix+"MinimumResponseTime"] = strconv.Itoa(rtt)
	s.params[prefix+"AverageResponseTime"] = strconv.Itoa(rtt)
	s.params[prefix+"MaximumResponseTime"] = strconv.Itoa(rtt)
	log.Printf("诊断完成：host=%s 成功=%d 失败=%d rtt=%dms", s.params[prefix+"Host"], ok, fail, rtt)
}

func (s *simulator) shouldIgnore(name string) bool {
	return s.matchAny(s.ignoreSet, name)
}

// parseIntSet 把 "1,3" 这种列表转成集合（空串就是空集合）。
func parseIntSet(v string) map[int]bool {
	out := map[int]bool{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			log.Printf("忽略看不懂的子设备序号: %q", part)
			continue
		}
		out[n] = true
	}
	return out
}

func (s *simulator) matchAny(subs []string, name string) bool {
	for _, sub := range subs {
		if strings.Contains(name, sub) {
			return true
		}
	}
	return false
}

func (s *simulator) getRPCMethods() string {
	methods := []string{
		"Inform", "GetRPCMethods", "TransferComplete", "GetParameterValues",
		"SetParameterValues", "GetParameterNames", "SetParameterAttributes",
		"GetParameterAttributes", "AddObject", "DeleteObject", "Reboot",
	}
	var b strings.Builder
	b.WriteString(`<cwmp:GetRPCMethodsResponse><MethodList soap-enc:arrayType="xsd:string[`)
	b.WriteString(strconv.Itoa(len(methods)))
	b.WriteString(`]">`)
	for _, m := range methods {
		b.WriteString(`<string>` + m + `</string>`)
	}
	b.WriteString(`</MethodList></cwmp:GetRPCMethodsResponse>`)
	return b.String()
}

// ---------- 报文拼装 ----------

func (s *simulator) informBody(event string) string {
	var b strings.Builder
	b.WriteString(`<cwmp:Inform>`)
	b.WriteString(`<DeviceId>`)
	b.WriteString(`<Manufacturer>` + esc(s.manufacturer) + `</Manufacturer>`)
	b.WriteString(`<OUI>` + esc(s.oui) + `</OUI>`)
	b.WriteString(`<ProductClass>` + esc(s.productClass) + `</ProductClass>`)
	b.WriteString(`<SerialNumber>` + esc(s.serial) + `</SerialNumber>`)
	b.WriteString(`</DeviceId>`)

	b.WriteString(`<Event soap-enc:arrayType="cwmp:EventStruct[1]">`)
	b.WriteString(`<EventStruct><EventCode>` + esc(event) + `</EventCode><CommandKey></CommandKey></EventStruct>`)
	b.WriteString(`</Event>`)

	b.WriteString(`<MaxEnvelopes>1</MaxEnvelopes>`)
	b.WriteString(`<CurrentTime>` + time.Now().UTC().Format("2006-01-02T15:04:05.000Z") + `</CurrentTime>`)
	b.WriteString(`<RetryCount>0</RetryCount>`)

	// 真实 CPE 的 Inform 只带一小部分参数，这里也一样
	names := s.informParamNames()
	b.WriteString(`<ParameterList soap-enc:arrayType="cwmp:ParameterValueStruct[` + strconv.Itoa(len(names)) + `]">`)
	for _, n := range names {
		v, ok := s.params[n]
		if !ok {
			continue
		}
		t := s.types[n]
		if t == "" {
			t = "string"
		}
		b.WriteString(`<ParameterValueStruct><Name>` + esc(n) + `</Name>`)
		b.WriteString(`<Value xsi:type="xsd:` + esc(t) + `">` + esc(v) + `</Value>`)
		b.WriteString(`</ParameterValueStruct>`)
	}
	b.WriteString(`</ParameterList>`)
	b.WriteString(`</cwmp:Inform>`)
	return b.String()
}

func (s *simulator) informParamNames() []string {
	var out []string
	for _, suffix := range []string{
		"DeviceInfo.SpecVersion", "DeviceInfo.HardwareVersion", "DeviceInfo.SoftwareVersion",
		"DeviceInfo.ProvisioningCode", "ManagementServer.ParameterKey",
		"ManagementServer.ConnectionRequestURL",
	} {
		for k := range s.params {
			if strings.HasSuffix(k, suffix) {
				out = append(out, k)
			}
		}
	}
	return out
}

func (s *simulator) envelope(id, body string) string {
	ns := "urn:dslforum-org:cwmp-" + s.cwmpVersion
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<soap-env:Envelope xmlns:soap-env="http://schemas.xmlsoap.org/soap/envelope/"` +
		` xmlns:soap-enc="http://schemas.xmlsoap.org/soap/encoding/"` +
		` xmlns:xsd="http://www.w3.org/2001/XMLSchema"` +
		` xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"` +
		` xmlns:cwmp="` + ns + `">` +
		`<soap-env:Header><cwmp:ID soap-env:mustUnderstand="1">` + esc(id) + `</cwmp:ID></soap-env:Header>` +
		`<soap-env:Body>` + body + `</soap-env:Body>` +
		`</soap-env:Envelope>`
}

func faultBody(code int, msg string) string {
	return `<soap-env:Fault><faultcode>Client</faultcode><faultstring>CWMP fault</faultstring>` +
		`<detail><cwmp:Fault><FaultCode>` + strconv.Itoa(code) + `</FaultCode>` +
		`<FaultString>` + esc(msg) + `</FaultString></cwmp:Fault></detail></soap-env:Fault>`
}

// ---------- 小工具 ----------

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func randID() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// ---------- Connection Request 的 Digest 校验（独立实现） ----------

// checkDigest 按 RFC 2617 校验一个 Digest 头。
// 这里刻意不复用 ACS 那边的代码：两套独立实现才能互相验证。
func checkDigest(header, method, uri, user, pass, nonce string) bool {
	if !strings.HasPrefix(strings.ToLower(header), "digest ") {
		return false
	}
	ps := parseDigestParams(header[len("Digest "):])
	if ps["username"] != user || ps["nonce"] != nonce {
		return false
	}
	gotURI := ps["uri"]
	if gotURI == "" {
		gotURI = uri
	}
	ha1 := md5hexSim(user + ":" + ps["realm"] + ":" + pass)
	ha2 := md5hexSim(method + ":" + gotURI)
	var want string
	if ps["qop"] == "auth" {
		want = md5hexSim(ha1 + ":" + nonce + ":" + ps["nc"] + ":" + ps["cnonce"] + ":auth:" + ha2)
	} else {
		want = md5hexSim(ha1 + ":" + nonce + ":" + ha2)
	}
	return want == ps["response"]
}

// parseDigestParams 解析 `k=v, k="v"` 形式（引号内的逗号不切）。
func parseDigestParams(s string) map[string]string {
	out := map[string]string{}
	var cur strings.Builder
	inQuote := false
	flush := func() {
		part := strings.TrimSpace(cur.String())
		cur.Reset()
		if part == "" {
			return
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return
		}
		k := strings.ToLower(strings.TrimSpace(kv[0]))
		v := strings.TrimSpace(kv[1])
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case r == ',' && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

func md5hexSim(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
