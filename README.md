# 轻量 TR-069 (CWMP) ACS

用 Go 写的**单二进制、无外部中间件**的 TR-069 Auto Configuration Server。
目标是把一批 CPE（光猫 / 路由器 / 网关）管起来：**注册纳管 → 查看设备信息 → 参数下发/采集 → 远程升级 → 重启**。

需求文档见 [`docs/requirements.md`](docs/requirements.md)。

**当前进度：S1（最小可用：纳管 + 查看设备信息）** ✅
已通过：端到端验收 42/42、独立实现互通验证 8/8、**真机（华为 OptiXstar HN8145X6N）纳管成功**。

---

## 快速开始

```bash
# 1) 准备 Go（本机放在 ~/.local/go，没装的话用官方 tarball）
export PATH=$HOME/.local/go/bin:$PATH

# 2) 编译
CGO_ENABLED=0 go build -o acs ./cmd/acs
CGO_ENABLED=0 go build -o cpesim ./test/cpesim

# 3) 启动（默认监听 :7547，CWMP 端点 /acs，数据存 acs.db）
./acs

# 4) 打开界面
#    http://127.0.0.1:7547/
```

长期联调（真机随时会上报，需要后台常驻）用这个脚本，带 pidfile 管理：

```bash
scripts/dev-server.sh start     # 编译 + 后台启动，默认端口 9090
scripts/dev-server.sh status    # 状态 + 最近日志
scripts/dev-server.sh log       # 跟踪日志
scripts/dev-server.sh stop

# 排障时同时看「收到的报文」和「发出的报文」：
ACS_LOG_LEVEL=debug ACS_LOG_SOAP=1 scripts/dev-server.sh restart
```

把 CPE 的 ACS URL 指到 `http://<本机IP>:端口/acs`，**也可以直接写 `http://<本机IP>:端口/`**
（两个都接）。账号密码见下面的配置。设备下次 Inform 就会出现在界面里。

> 真机实测：华为 OptiXstar HN8145X6N 配的就是**根路径 `/`** —— 只监听 `/acs` 会直接收不到上报。

没有真机也能验证 —— 用自带的 CPE 模拟器：

```bash
# 模拟一台 TR-098 设备接入一次
./cpesim -acs http://127.0.0.1:7547/acs -serial MYDEV001 -once

# 模拟一台 TR-181 设备，每 30 秒上报一次
./cpesim -acs http://127.0.0.1:7547/acs -serial MYDEV002 -dm 181 -interval 30s
```

## 配置

命令行参数与环境变量（环境变量优先，命令行参数优先级最高）：

| 参数 | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `-listen` | `ACS_LISTEN` | `:7547` | HTTP 监听地址 |
| `-path` | `ACS_PATH` | `/acs` | CWMP 端点路径 |
| `-db` | `ACS_DB` | `acs.db` | SQLite 文件 |
| `-acs-url` | `ACS_URL` | — | 本机对外的 ACS URL（展示/写回 CPE 用） |
| `-user` / `-password` | `ACS_USER` / `ACS_PASSWORD` | 空 | CPE→ACS 的 Basic 认证，留空则不校验 |
| `-session-timeout` | `ACS_SESSION_TIMEOUT` | `60s` | 会话空闲超时 |
| `-offline-after` | `ACS_OFFLINE_AFTER` | `10m` | 多久没上报算离线 |
| `-max-body` | `ACS_MAX_BODY` | `4MiB` | 单请求体上限 |
| `-max-params-per-request` | `ACS_MAX_PARAMS_PER_REQUEST` | `200` | 单次 GetParameterValues 带多少个参数名（真机单次上限可能只有 256，见下文）|

## 界面

- 设备详情页的**参数 / 任务历史 / Inform 记录**三个区块默认折叠，点击展开
  （原生 `<details>`，不需要 JS，键盘与读屏也可用）
- 三个表以及概览页的设备列表**默认 20 条/页**，可切换 20 / 50 / 100 / 全部；
  浏览器端分页，和「参数名过滤」是配合关系（过滤后重新分页）；
  行数本来不到一页时自动不显示分页条

## 数据库迁移

库里记着 `PRAGMA user_version`，`internal/store/store.go` 里有一个 `migrations` 列表。

**约定：`baseSchema` 永远是「第 0 版」，新加字段一律走迁移。**
不要直接改 `baseSchema` —— 那样已存在的库升不上来，而新建的库又会因为重复建列而报错。
第一次加字段（设备备注）时就是这么做的，迁移是幂等的：重复启动不会出错，
库版本比程序新时会明确报错而不是继续跑。
| `-auto-fetch-info` | `ACS_AUTO_FETCH_INFO` | `true` | Inform 后自动取设备基本信息 |
| `-auto-fetch-wifi` | `ACS_AUTO_FETCH_WIFI` | `true` | 首次纳管/BOOTSTRAP 时自动采集无线概况（看板用）|
| `-probe-capabilities` | `ACS_PROBE_CAPABILITIES` | `true` | 首次纳管时探测设备能力（如有没有 FTTR 子设备）|
| `-log-level` / `-log-json` | `ACS_LOG_LEVEL` / `ACS_LOG_JSON` | `info` / 否 | 日志 |
| `-log-soap` | `ACS_LOG_SOAP` | 否 | 打印原始 SOAP 报文（排障用） |

所有跟运行环境绑定的值都是可配置的，代码里不写死「某台机器的事实」。

## 目录结构

```
cmd/acs/            程序入口
internal/config/    配置加载
internal/cwmp/      CWMP 协议：XML 解析、SOAP 编解码、会话、HTTP 端点、任务下发
internal/cwmp/testdata/  真机报文的回归样本（实际抓下来的，不是手写的）
internal/store/     SQLite 持久化（devices / device_params / informs / tasks）
internal/web/       Web 界面 + JSON API（模板内嵌，无前端构建链）
test/cpesim/        用 Go 写的 CPE 模拟器（真机替代品）
scripts/            验收脚本 + dev-server.sh（后台起 ACS 给真机联调）
reference/          第三方参考实现（不进仓库，见 scripts/fetch-reference.sh）
docs/               需求文档与笔记
```

## 已实现（S1）

- **CWMP 端点** `/acs`：容忍各种脏报文（非标准前缀、未知厂商扩展节点、gzip/deflate、iso-8859-1、标签不闭合时优雅报错）
- **SOAP 编解码**：命名空间按 CPE 声明的版本回填（1.0~1.3），`cwmp:ID` 原样回填
- **Inform 处理**：设备自动登记（身份 = OUI + ProductClass + SerialNumber）、事件码解析、参数落库、Inform 流水
- **自研会话管理**：cookie + 「IP/UA」指纹双层关联；同一设备串行化（并发请求排队而非交叉）
- **任务队列**：DB 持久化、去重、`pending → running → done/failed`、重启后 `running` 自动退回待办
- **ACS→CPE 下发**：`GetParameterValues` / `GetParameterNames` / `SetParameterValues` / `Reboot` / `GetRPCMethods`
- **CPE→ACS 接收**：`Inform` / `Fault` / 各类 `*Response` / `TransferComplete`（先记录）
- **Web 界面**：设备列表 + 设备详情（基本信息 / 参数表带过滤 / 任务历史 / Inform 记录）+ 一键「重新获取设备信息」
- **修改无线设置**：看板或详情页点 SSID（或频段）进入编辑表单，提交后下发 `SetParameterValues`。
  字段是否出现、写向哪个参数、下拉候选值（信道/功率来自设备的 `PossibleChannels`、
  `TransmitPowerSupported`）**全部从设备实报参数推导**，不写死；只下发真正改动过的字段；
  下发成功后自动把参数读回来核对（写入成功不等于真的生效，而且设备可能**异步生效**，
  所以同会话对不上先不判错、到下一轮会话再定性）；写入无线参数后会**自动重采一次无线概况**，
  避免界面上的状态/信道停在写入前
- **设备备注**：设备详情页可写备注（不会下发给设备、也不会被 Inform 冲掉），概览页展示并参与搜索
- **概览页搜索**：按 序列号 / 备注 / 名称 / 产品类 / OUI / SSID 搜索（服务端过滤，
  结果 URL 可分享；输入后自动搜索，不依赖 JS 也能用搜索按钮）
- **FTTR 子设备**：详情页展示子光猫 / 子 AP（型号、序列号、MAC、在线、版本、信道、信号、时长）。
  **先探测能力，没有就整块不显示** —— 不给用户看空区块（详见下文「探测」一节）
- **网络诊断（Ping）**：详情页填 IP/域名与包数，下发标准的 `IPPingDiagnostics`，
  等设备自己跑完回报结果（支持同步与「事件 8 DIAGNOSTICS COMPLETE」异步两种节奏）
- **日间 / 夜间模式**：右上角一键切换，选择记在 localStorage；
  没选过时跟随系统的 `prefers-color-scheme`；主题在样式生效前就定好，刷新不闪
- **JSON API**：`/api/devices`、`/api/devices/{id}`
- **看板 WiFi 概览**：首页按设备分频段展示 2.4G/5G 的 SSID、射频开关、信道、标准、加密与**已连终端数**，
  并给出终端总数；首次纳管 / `0 BOOTSTRAP` 时**自动采集**（先枚举子树圈出真实实例号，
  再按后缀白名单只取十几个摘要字段，不会把四百多个 WLAN 参数全拉回来）
- **读取任意参数子树**：界面上的「读取参数子树」表单，或
  `POST /api/devices/{id}/fetch` `{"path":"...","exclude":["..."],"max":200}` ——
  先 `GetParameterNames` 枚举再 `GetParameterValues` 取值，两步在**同一个会话**里完成
- **浏览参数树**：`POST /api/devices/{id}/names` `{"path":"InternetGatewayDevice.","next_level":true}` ——
  只枚举名字不取值。接新设备/找厂商私有对象时先用它把结构列出来，
  比盲猜路径实用得多（真机上猜错一次就是一整轮上报周期）
- **CPE 模拟器**：TR-098 / TR-181 两种数据模型，支持 Connection Request 触发

## 未实现（后续）

`AddObject` / `DeleteObject`、参数属性（Get/SetParameterAttributes）、Connection Request（**发送**侧）、预设/策略、固件 Download/Upload、ScheduleInform、FactoryReset、mTLS、PG 后端、指标。
详见 `docs/requirements.md` §3 的优先级表。

## 验收

```bash
# 单元测试
go test ./...

# 端到端验收（编译真二进制、起真 HTTP + 真 SQLite、发真报文，42 条断言）
scripts/verify-s1.sh

# 与独立实现的互通性验证（需要 node）
scripts/fetch-reference.sh      # 先拉参考项目
scripts/verify-interop.sh
```

`verify-s1.sh` 覆盖：服务存活、空 POST/204 语义、坏报文容错、未知 RPC 报错码、命名空间回填、
TR-098 与 TR-181 两种设备的纳管与信息采集、重复上报不产生重复设备、周期上报不重复入队、
手工刷新、界面渲染。

真机报文已固化为回归样本（`internal/cwmp/testdata/`，不是手写的，是实际抓下来的字节），
对应 `internal/cwmp/realdevice_test.go` 里的 4 个用例 —— 以后重构解析逻辑时，
真机的那些怪癖（大写前缀、自闭合空元素、根节点名大小写写错）会被测到。

## 实现过程中踩到/修掉的十个真问题

1. **CWMP 命名空间回填写成了版本号**（单测抓到）
   `ParseEnvelope` 一度把 `env.CWMPNS` 设成 `"1.0"` 而不是完整的 `urn:dslforum-org:cwmp-1-0`，
   回给 CPE 的信封就成了 `xmlns:cwmp="1.0"`。因为本地模拟器是按本地名匹配的，端到端测试
   完全没暴露它。**教训：只跟自己的模拟器对打会掩盖错误。**

2. **不要指望 CPE 支持子树路径的 GetParameterValues**（交叉验证抓到）
   最初我们下发 `InternetGatewayDevice.DeviceInfo.`（`.` 结尾 = 子树）去取信息，
   结果 GenieACS 官方的 genieacs-sim 直接崩 —— 它的 `GetParameterValues` 只按完整参数名
   做 map 查表，不支持部分路径（虽然它的 `GetParameterNames` 支持）。
   真机大多支持子树，但既然有实现会栽在这里，就改成**下发显式参数名**（GenieACS 自己也这么做）：
   根未知时把 TR-098 与 TR-181 两种命名的同一批参数都发过去，CPE 只回它有的那些 ——
   这本身就把「数据模型根」探测出来了，而且一轮 RPC 搞定。

3. **真机的 ACS URL 配的是根路径 `/`，不是 `/acs`**（真机联调抓到）
   华为这台光猫把上报 POST 到了 `http://192.168.10.158:9090/`。我们当时只监听 `/acs`，
   结果就是回 404、设备永远上不来，而日志里又什么都不会显示。
   现在 `POST /{$}` 也接；并且新增的请求日志中间件会把这类「打进来了但没被处理」的 404/405
   在 **WARN** 级别记下来 —— 以后接新设备时一眼就能看出是路径配错了。

4. **CPE 单次只回 256 个参数，超出静默丢弃**（真机联调 + 读 WiFi 时抓到）
   我们请求 376 个参数名，华为这台只回了 **256 个（正好 2^8）**，不报错、不告知。
   旧代码会把「回了一批」当成任务成功 → **默默少采集 120 个参数，没有任何错误信号**。
   这比协议报错危险得多。
   → 新增 `MaxParamsPerRequest`（默认 200）自动分批（`enqueueGPVDivided`）；分批不会变慢，
   因为各批还是同一个会话里依次下发。修完后 376 个参数拆成 200+176 两批，**一条不丢**。
   另加 `warnIfPartialResponse` 防御：发现「回的比请求的少」就记 WARN。
   > 这个坑自研模拟器永远发现不了（它会老老实实全返回）—— 又一次说明必须拿真机验收。

5. **写回参数时类型名的大小写必须规范**（做「修改 WiFi」时想到并修掉）
   我们原来把 CPE 报的类型名一律转小写存（`unsignedInt` -> `unsignedint`），
   于是写回去就变成 `xsi:type="xsd:unsignedint"`。XML Schema 类型是**大小写敏感**的，
   `xsd:unsignedint` 不合法，严格的 CPE 会直接拒收（9008 invalid parameter type）。
   因为只影响写入路径，读/展示一直看不出来。
   → 新增 `canonicalType()`：归一化成规范写法（`string` / `int` / `unsignedInt` /
   `boolean` / `dateTime` / `base64`…），解析时和**写出去时**都过一遍（后者能顺便
   修正库里已有的旧值），不认识的类型原样保留不瞎改。

6. **无线参数是异步生效的，写完立即读回会误报**（真机写入时抓到）
   改 5GHz 的 `RadioEnabled`：设备回 `Status=0`（接受了），但同一会话里读回来还是 `0`；
   几分钟后再查已经是 `1` —— **真的生效了，只是晚**。
   第一版的「读回对不上就判失败」会因此误报。
   → 改成两级核对：同会话读回对得上就直接结案；对不上先不判错，把核对任务排到
   **设备下一轮会话**再跑；那时还对不上才算真没生效。
   （不用加延时列：`ClaimNextTask` 支持一个 `exclude` 列表，会话记住「本轮跳过的任务」，
   会话结束时清空，任务自然落到下一轮。）

7. **密码写错了参数**（真机报错告诉我们的）
   往 `WLANConfiguration.{i}.KeyPassphrase` 写 WPA2 密码，设备回
   `9003 Invalid arguments` + `...KeyPassphrase -> 9007 Invalid parameter value`。
   因为 WPA/WPA2-PSK 的密码在 `PreSharedKey.1.KeyPassphrase` 下，前者是给 WEP 的。
   （两个参数的叶子名都是 `KeyPassphrase`，原来靠叶子名索引永远选不到对的那个。）
   → 字段候选改成支持**多段尾部路径**，密码优先选 `presharedkey.1.keypassphrase`。
   > 顺带印证：把设备的逐参数错误完整记进任务结果是值得的 —— 它直接指路了。

8. **只回读改动的参数，会让「相关字段」停在写入前**（被用户当场发现）
   改完 5GHz 射频开关后，我看库里 `Status=Disabled` 就说「5GHz 没起来」；
   实际上设备**已经起来了、能搜到信号**。看时间戳才发现：`Status/Channel`
   最后一次采集是 23 分钟前（写入只回读了 `RadioEnabled` 一个参数）。
   > 教训：**别拿库里的值下结论** —— 先看它的更新时间（或者重新采一次）。
   → 两个改动：① 写入无线参数后自动重采一份无线概况（延后一轮，拿生效后的值）；
   ② 界面上把**每个参数的更新时间**和**每个频段的采集时间**显示出来，
   让「这是新的还是旧的」一眼可见，不再靠人记得。

9. **「写 only」参数不能被读回核对当成失败**（你真机上改 5G 密码时抓到）
   往 `WLANConfiguration.5.PreSharedKey.1.KeyPassphrase` 写密码，设备回 `Status=0`，
   但读回永远是**空串** —— 因为它跟 `KeyPassphrase` 一样属于**能改不能读**。
   上一版的核对只看「期望值 vs 读回值」，就会把一次成功的改密码报成「未生效」。
   → 核对时多比一个东西：**写入前的值**。
   - 写前非空、写后变了 → 未生效（真没生效）
   - 写前写后**都是空** → 设备不回读该参数，归为「无法核对」，**不判失败**，
     但会在任务结果里写明哪几个参数没核对上
   为此写入任务里多存一份 `prev`（写入前的值）。

10. **`DiagnosticsState=Requested` 必须放在 SetParameterValues 的最后**（做 ping 诊断时模拟器暴露的）
    第一次把 `DiagnosticsState=Requested` 放在了列表**最前面**，结果设备看到它就开始跑 ping，
    而此刻 `Host` 与 `NumberOfRepetitions` 还没写进去 —— 实际拿到的是**空 Host + 默认次数**，
    结果是「发送包 4，成功 0，失败 4」。
    真机上有些设备也会这么干（TR-069 虽然要求一次写入原子生效，但不能指望）。
    → 顺序改成：次数、Host、**最后**才置 Requested。改完立刻变成「发送包 3，成功 3，失败 0」。

## 相关笔记

- `docs/requirements.md` —— 需求与设计（含协议时序、数据模型、里程碑）
- `docs/notes/implementation-notes.md` —— 实现笔记与实测记录
