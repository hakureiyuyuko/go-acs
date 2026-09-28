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
| `-auto-fetch-info` | `ACS_AUTO_FETCH_INFO` | `true` | Inform 后自动取设备基本信息 |
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
- **JSON API**：`/api/devices`、`/api/devices/{id}`
- **读取任意参数子树**：界面上的「读取参数子树」表单，或
  `POST /api/devices/{id}/fetch` `{"path":"...","exclude":["..."],"max":200}` ——
  先 `GetParameterNames` 枚举再 `GetParameterValues` 取值，两步在**同一个会话**里完成
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

## 实现过程中踩到/修掉的四个真问题

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

## 相关笔记

- `docs/requirements.md` —— 需求与设计（含协议时序、数据模型、里程碑）
- `docs/notes/implementation-notes.md` —— 实现笔记与实测记录
