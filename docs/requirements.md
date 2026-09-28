# 轻量 TR-069 ACS 服务器 — 需求文档

- 文档版本：v0.1（草案，待评审）
- 编写日期：2026-09-28
- 实现语言：Go
- 状态：**需求梳理阶段**，未开始编码

> **实现进度（2026-09-28 更新）**
> S1「纳管 + 查看设备信息」已实现并通过真实验收（42/42 断言），代码在仓库根目录。
> 验收方式：`scripts/verify-s1.sh`（端到端）与 `scripts/verify-interop.sh`（与 GenieACS 官方 CPE 模拟器的互通性）。
> **已接真机**：华为 OptiXstar HN8145X6N（FTTR 光猫）纳管成功，取回 14 个参数。
> 实现笔记与实测发现见 [notes/implementation-notes.md](notes/implementation-notes.md)。
> 实现中修正了本文档三处想法：① 取设备信息改为**下发显式参数名**而不是子树路径
> （有的实现不支持子树查询，见笔记 §2）；② ACS 侧错误码用 8xxx，9xxx 是 CPE 发给我们才用的；
> ③ **ACS URL 很可能配成根路径 `/` 而不是 `/acs`**（真机就是这么配的），两个路径都要接。

---

## 1. 项目概述

### 1.1 一句话定位

用 Go 实现一个**单二进制、嵌入式存储、零外部中间件**的轻量 TR-069/CWMP ACS（Auto Configuration Server），
足以接管一批 CPE（光猫 / 路由器 / 网关），完成「注册纳管 → 参数下发 → 采集 → 远程升级 → 重启」的闭环。

### 1.2 目标

| 编号 | 目标 | 说明 |
| --- | --- | --- |
| G1 | 协议正确 | 能正确收发 CWMP SOAP，兼容 TR-069 Amendment 1~6 的命名空间与常见厂商实现 |
| G2 | 能真正管住设备 | Inform / Get-Set / Connection Request / 任务队列 / 固件下载 全部可用 |
| G3 | 轻量 | 单个 Go 二进制 + 一个 SQLite 文件即可跑起来；无 Node/Java/Python/Redis/PG 强制依赖 |
| G4 | 可观测 | 有结构化日志 + 每台设备的会话/任务/参数历史可查 |
| G5 | 可移植 | 只在标准 Linux 上依赖 Go 与文件系统；不绑定具体机器事实（见 §2.4 原则） |

### 1.3 非目标（明确不做，避免范围膨胀）

- 不做多租户 / 计费 / 运营级 SaaS；
- 不做 TR-369 (USP) / MQTT / WebSocket 南向协议；
- 不做完整 TR-098/TR-181 数据模型字典（只存与管实际出现过的节点，见 §6.2）；
- 不做 STUN/XMPP NAT 穿透（Annex G），第一期只支持「CPE 有可达 ConnectionRequestURL」的场景；
- 不做前端构建链（不引入 npm/vite），界面用服务端模板 + 原生 JS 或 htmx；
- 不做告警/工单/报表等上层业务系统。

---

## 2. 术语与协议基础

### 2.1 术语

| 术语 | 含义 |
| --- | --- |
| ACS | Auto Configuration Server，本项目的服务端 |
| CPE | Customer Premises Equipment，被管的设备（光猫/路由） |
| CWMP | CPE WAN Management Protocol，即 TR-069 的协议名 |
| RPC | 远程过程调用，SOAP 信封里的一次方法调用 |
| Inform | CPE → ACS 的上报，一切会话的起点 |
| Connection Request | ACS 主动「踢」CPE 回连的 HTTP GET |
| Data Model | 参数树，TR-098 根为 `InternetGatewayDevice.`，TR-181 根为 `Device.` |
| 会话 Session | 一次 CPE 与 ACS 的完整报文交换过程，由 CPE 发起、CPE 结束 |

### 2.2 协议交互模型（关键，必须实现正确）

CWMP 是一条**长轮询式的、CPE 单向发起的** SOAP over HTTP 通道，**每个 HTTP POST 只承载一个 RPC**：

```
  CPE                                             ACS
   |  POST  Inform                              |
   | -----------------------------------------> |
   |  200   InformResponse (MaxEnvelopes=1)     |
   | <----------------------------------------- |
   |  POST  (空信封：我还有事，你说吧)            |
   | -----------------------------------------> |
   |  200   GetParameterValues (ACS 下发的任务)  |
   | <----------------------------------------- |
   |  POST  GetParameterValuesResponse          |
   | -----------------------------------------> |
   |  200   (空 body)                           |
   | <----------------------------------------- |
   |  POST  (空信封)                            |
   | -----------------------------------------> |
   |  204   No Content (会话结束)                |
   | <----------------------------------------- |
```

必须遵守的规则：

1. **CPE 永远是发起方**；ACS 不能主动建连接，只能靠 Connection Request 让 CPE 回连（带回事件码 `6 CONNECTION REQUEST`）。
2. **同一时刻一台 CPE 只允许一个会话**；ACS 侧必须对同一设备串行化（见 FR-6 / NFR-2）。
3. 一个 SOAP Envelope 的 `Header` 中 **`cwmp:ID` 必须回填同一个值**（`mustUnderstand="1"`）。
4. ACS 响应中的 `MaxEnvelopes` 应回 `1`（表示「我一次只处理一个请求」）。
5. 命名空间需与 CPE 声明的版本一致（`urn:dslforum-org:cwmp-1-0` ~ `cwmp-1-4`），**不要写死 1-0**。
6. ACS 无任务可下发时，对该次空 POST 返回 `204 No Content`（部分实现用 200 + 空 body，需兼容）。

### 2.3 Inform 事件码

| 事件码 | 含义 | 附带内容 | 备注 |
| --- | --- | --- | --- |
| `0 BOOTSTRAP` | 首次连上本 ACS 或 ACS URL 变更 | DeviceId 全量 | 纳管入口，需下发基础配置 |
| `1 BOOT` | 每次上电启动 | — | |
| `2 PERIODIC` | 周期上报 | — | 由 `PeriodicInformInterval` 控制 |
| `3 SCHEDULED` | ScheduleInform 触发 | CommandKey | |
| `4 VALUE CHANGE` | 被通知参数发生变化 | **仅带变化的 ParameterList** | |
| `5 KICKED` | 被 ACS 断开后跟随 | — | |
| `6 CONNECTION REQUEST` | 收到 ACS 连接请求后回连 | — | Connection Request 成功的标志 |
| `7 TRANSFER COMPLETE` | 下载/上传完成 | CommandKey | 与 Download 任务关联 |
| `8 DIAGNOSTICS COMPLETE` | 诊断完成 | CommandKey | 第二期 |
| `M Reboot` | 重启之后 | — | |
| `X CT-*` | 厂商自定义 | 视厂商而定 | 只记录、不报错 |

> **需求约束 R-INF-1**：不得假设 Inform 一定携带全量 ParameterList。`0 BOOTSTRAP`/`1 BOOT` 可能只带部分节点，`4 VALUE CHANGE` 只带变化节点。ACS 必须能按需补齐。

### 2.4 实现原则（重要）

沿用本项目一贯风格，**不为某一台机器/某一型号设备写死事实**：

- 数据模型根（`InternetGatewayDevice.` / `Device.`）、认证方式、周期、参数类型，一律**运行时探测 + 配置项可覆盖**；
- 探测失败时**优雅降级**（例如未知参数类型先按 string 存并在日志标注），不得 panic、不得中断会话；
- 文档中出现的任何性能/容量数字必须标注为「实例测量值，非项目常量」；
- 厂商私有节点差异只在「适配层」处理，不污染核心会话逻辑。

---

## 3. 需求范围与优先级（MoSCoW）

| 优先级 | 含义 | 内容摘要 |
| --- | --- | --- |
| **P0 必须有** | 缺了就不算 ACS | HTTP/SOAP 端点、Inform 处理、会话状态机、Get/SetParameterValues、GetParameterNames、任务队列、SQLite 持久化、结构化日志 |
| **P1 应该有** | 生产可用 | Connection Request、Add/DeleteObject、参数属性（Get/SetParameterAttributes）、预设(Preset)、固件 Download + TransferComplete、最小 Web 界面、REST API、基础认证 |
| **P2 可以有** | 锦上添花 | Upload、ScheduleInform、FactoryReset、Reboot 策略控制、HoldRequests 长轮询、STUN、mTLS、PostgreSQL 后端、Prometheus 指标 |
| **不做** | — | USP/MQTT、多租户、报表、告警、NAT 穿透(第一期) |

---

## 4. 功能需求

### FR-1 CWMP 端点与传输层

- `POST /acs`（路径可配置）接收 SOAP；必须支持 `text/xml; charset=utf-8` 与 `Content-Type: application/soap+xml`。
- 必须容忍 CPE 常见的脏报文：多余空白、`Transfer-Encoding: chunked`、无 `Content-Length`、gzip、非标准 `charset`、未知 SOAP 命名空间前缀。
- 支持 HTTP 与 HTTPS（证书路径配置化，未配置则只监听 HTTP）。
- 单请求体默认上限（可配置，默认 4 MiB）防 DoS。
- **R-FR1-1**：解析失败时返回合法 SOAP Fault，而不是 500 裸错误，便于定位。

### FR-2 认证与安全

| 编号 | 需求 | 优先级 |
| --- | --- | --- |
| FR-2.1 | CPE → ACS 认证：HTTP Digest（首选）/ Basic（兼容）；账号可来自配置或数据库 | P1 |
| FR-2.2 | 可选 mTLS（Pre-Shared Certificate，TR-069 Annex C） | P2 |
| FR-2.3 | 密码**不得明文入库**（bcrypt/scrypt 或 AES-GCM 加密存储） | P1 |
| FR-2.4 | Connection Request 使用 `InternetGatewayDevice.ManagementServer`（或 `Device.ManagementServer`）下的 `ConnectionRequestUsername/Password`，HTTP Digest 认证 | P1 |
| FR-2.5 | 日志中屏蔽密码、Authorization 头、敏感参数值 | P1 |
| FR-2.6 | 可选按源 IP/白名单限制 `/acs` | P2 |

### FR-3 SOAP 编解码

- 统一 Envelope 构造：`Envelope` / `Header(cwmp:ID)` / `Body`。
- 命名空间随 CPE 请求版本回填（见 §2.2 第 5 条）。
- **R-FR3-1**：编解码必须是「容忍未知元素」的：遇到不认识的子元素/厂商扩展，跳过而不是报错。
- **R-FR3-2**：`ParameterValueStruct` 的 `xsi:type` 需覆盖 `string / int / unsignedInt / boolean / dateTime / base64`。
- **R-FR3-3**：数字类型在 XML 里可能带前导零、`+` 号、科学计数法，解析需容错。
- SOAP Fault 输出需符合 CWMP 的 `cwmp:Fault`（含 `FaultCode` / `FaultString` / `SetParameterValuesFault`）。

### FR-4 CPE → ACS RPC（南向接收）

| RPC | 处理要求 | 优先级 |
| --- | --- | --- |
| `Inform` | 解析 DeviceId/Event/ParameterList/MaxEnvelopes/RetryCount/CurrentTime；落库；建会话；回 `InformResponse` | P0 |
| `TransferComplete` | 按 CommandKey 关联下载任务的成败，更新任务状态与固件版本 | P1 |
| `AutonomousTransferComplete` | 自主升级完成，按策略决定是否拉取参数 | P2 |
| `GetRPCMethods` | 返回本 ACS 支持的 RPC 名列表 | P1 |
| `RequestDownload` | 罕见，返回 `RequestDownloadResponse`（可先空实现并记录） | P2 |
| `Kicked` | 记录（TR-069 A.3.2.2，含 `OldACSURL`） | P2 |
| `Fault` | 记录为设备侧错误，不视为传输失败 | P0 |

### FR-5 ACS → CPE RPC（北向下发）

| RPC | 关键点 | 优先级 |
| --- | --- | --- |
| `GetParameterValues` | 参数名数组；可含单参数与对象路径（`.` 结尾） | P0 |
| `SetParameterValues` | `ParameterKey` 必须原样回传；失败项逐条落库 | P0 |
| `GetParameterNames` | `NextLevelOnly` 语义；返回 `writable` 标记 | P0 |
| `AddObject` | 返回新 `InstanceNumber`，需回填本地模型；失败回滚 | P1 |
| `DeleteObject` | 对象名需以实例号 + `.` 结尾 | P1 |
| `GetParameterAttributes` | `Notification` / `AccessList` | P1 |
| `SetParameterAttributes` | 同上，写权限校验 | P1 |
| `Download` | FileType / URL / Username / Password / FileSize / DelaySeconds / SuccessURL / FailureURL / CommandKey | P1 |
| `Upload` | CPE 上传到 ACS 端 URL（需文件接收端点） | P2 |
| `Reboot` | CommandKey 关联；需策略控制（避免误重启） | P1 |
| 〃 | **已实现**：详情页红按钮 + 二次确认；同一设备不重复下发；设备回 RebootResponse 即完成 | ✅ |
| `FactoryReset` | 高危，默认关闭，需显式白名单 | P2 |
| `ScheduleInform` | DelaySeconds / CommandKey | P2 |

> **R-FR5-1**：所有下发 RPC 都必须带 `CommandKey`（可配置是否生成），以便和后续 Inform/TransferComplete 关联。
> **R-FR5-2**：CPE 返回 Fault 时要区分「重试有意义」（9005 Retry request / 8005）与「重试无意义」（9003 Invalid arguments 等），决定任务是否重试。

### FR-6 会话状态机与并发

- 会话由 CPE 的 POST 驱动，ACS 侧维护内存态（可选落库做审计）。
- **R-FR6-1**：同一设备（键：OUI+ProductClass+SerialNumber）的会话必须**串行**；并发到达时排队或直接拒绝（返回 503/空响应），不得交叉下发任务导致报文错配。
- **R-FR6-2**：会话有超时（默认 60s，可配置），超时释放锁并清理上下文。
- **R-FR6-3**：`cwmp:ID` 与设备绑定，用于校验「这个响应是回应我哪次下发」。
- 支持 `HoldRequests`（CPE 声明支持时可长轮询，减少空 POST 往返）—— P2。
- 会话结束（204）后必须归还设备锁、落盘会话记录。

### FR-7 设备与参数存储

- 设备身份唯一键：`(OUI, ProductClass, SerialNumber)`；`0 BOOTSTRAP` 时自动注册新设备。
- 参数以「路径 + 值 + 类型 + 可写 + 属性」形式存储，支持按前缀批量查询（对象树）。
- **R-FR7-1**：ACS 需要区分「ACS 想下发的期望值」与「CPE 实际上报值」吗？→ 第一期只存实际值 + 待下发任务，不做 last-known-good 影子模型（记入 §12 开放问题）。
- 提供参数历史（可选，按需开启，避免无限增长）。

### FR-8 任务队列

- 每设备一条 FIFO 待办队列，任务类型见 FR-5。
- 状态机：`pending → running → done | failed | expired`。
- 支持：延迟执行（`not_before`）、过期时间、重试次数与退避、去重（同设备同类同参任务合并）。
- 设备上线（Inform）时取出到期任务依次下发，一次会话内可连续下发多条。
- 提供 REST/CLI 手工入队能力（运维刚需）。

### FR-9 Connection Request（主动唤醒）

- 从设备参数读取 `ConnectionRequestURL`（可能带 `?auth=...` 查询串，需原样保留）。
- 用设备侧的 `ConnectionRequestUsername/Password` 发 HTTP GET，完成 Digest 握手。
- 结果分类：`成功 / 401 认证失败 / 连接超时 / 无 URL / 被 NAT 挡（URL 为私网且不可达）`，分类落库并在 UI 展示。
- 成功后 CPE 会在数秒内以 `6 CONNECTION REQUEST` 发起会话，此时任务才会真正下发。
- 超时可配置（默认 10s），失败不阻塞调用方（异步）。

### FR-10 预设 / 策略（Preset）

- 触发条件（至少支持）：
  - 事件码（如 `0 BOOTSTRAP`、`1 BOOT`、`2 PERIODIC`、`7 TRANSFER COMPLETE`）；
  - 首次纳管（新设备）；
  - 参数值变化（`4 VALUE CHANGE` 命中某个参数）。
- 动作序列：`SetParameterValues` / `GetParameterValues` / `AddObject` / `Download` 等，按序入队。
- 支持条件表达式（对设备已有参数做判断，如「型号 = X 且 软件版本 < Y」）——可先做简单比较。
- 预设与动作需可在不改代码的前提下增删（配置或界面）。

### FR-11 固件/文件管理

- 内置静态文件服务，供 CPE 直接 `Download`（CPE 是**自己拉取** URL，不经 ACS 代理）。
- 文件元数据：名称、类型（Firmware/Config/Web）、大小、SHA-256、适用型号、版本号。
- 下发 `Download` 时生成 `CommandKey`，等待 `TransferComplete` 回执关联成败。
- 支持 `DelaySeconds`（错峰，避免批量升级打垮链路）。
- 校验：CPE 上报的版本与目标版本比对，避免重复升级。

### FR-12 Web 界面（最小）

服务端模板渲染，无构建步骤。至少包含：

1. 设备列表（在线态、型号、软件版本、最后 Inform 时间、IP、事件码），支持搜索/过滤；
2. 设备详情：参数树浏览 + 单参数编辑下发 + 任务历史 + 会话/Inform 历史；
3. 任务中心：待办/失败任务、重试、手工入队；
4. 预设管理：查看/启停；
5. 日志/事件流；
6. Connection Request 一键触发。

### FR-13 REST API

- 供自动化/脚本调用：设备 CRUD、参数读写、任务入队、预设管理、统计。
- 鉴权：Token（配置生成）或 Basic；默认只监听本机或需显式开启。
- 响应 JSON，风格与 Web UI 共用同一套内部服务层。

### FR-14 日志与可观测

- 结构化日志（`log/slog`，JSON 与文本两种格式可切）。
- 每条日志尽量带 `device` 维度（OUI/序列号）。
- 会话级报文记录（可选开关，注意隐私与体积）：原始 XML 可按需开启，用于排障。
- 关键计数：在线设备数、会话数、任务成功率、平均会话时长、Fault 计数。
- （P2）Prometheus `/metrics` 端点。

---

## 5. 数据模型与存储设计（草案）

选型见 §8；下表以关系型（SQLite/PG 通用）描述。

```sql
-- 设备
CREATE TABLE devices (
  id                    INTEGER PRIMARY KEY,
  oui                   TEXT NOT NULL,
  product_class         TEXT NOT NULL,
  serial_number         TEXT NOT NULL,
  manufacturer          TEXT,
  model_name            TEXT,
  data_model_root       TEXT,            -- 'InternetGatewayDevice.' | 'Device.'
  software_version      TEXT,
  hardware_version      TEXT,
  ip_address            TEXT,
  conn_request_url      TEXT,
  conn_request_username TEXT,
  conn_request_password TEXT,            -- 加密存储
  periodic_interval     INTEGER,
  last_inform_at        TIMESTAMP,
  last_boot_at          TIMESTAMP,
  last_contact_at       TIMESTAMP,
  online                BOOLEAN DEFAULT 0,
  created_at            TIMESTAMP,
  updated_at            TIMESTAMP,
  UNIQUE (oui, product_class, serial_number)
);

-- 参数（一棵扁平的树，按 name 前缀查对象）
CREATE TABLE device_params (
  device_id    INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,            -- 全路径，如 Device.WiFi.SSID.1.SSID
  value        TEXT,
  value_type   TEXT,                     -- string|int|unsignedInt|boolean|dateTime|base64
  writable     BOOLEAN,
  notification INTEGER DEFAULT 0,        -- 0 none / 1 passive / 2 active
  access_list  TEXT,
  updated_at   TIMESTAMP,
  PRIMARY KEY (device_id, name)
);

-- 任务（每设备 FIFO）
CREATE TABLE tasks (
  id           INTEGER PRIMARY KEY,
  device_id    INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL,            -- SetParameterValues / Download / ...
  payload      TEXT,                     -- JSON
  command_key  TEXT,
  status       TEXT NOT NULL,            -- pending|running|done|failed|expired
  result       TEXT,                     -- JSON（含 FaultCode）
  retry_count  INTEGER DEFAULT 0,
  max_retries  INTEGER DEFAULT 3,
  not_before   TIMESTAMP,
  expire_at    TIMESTAMP,
  created_at   TIMESTAMP,
  started_at   TIMESTAMP,
  finished_at  TIMESTAMP
);

-- Inform / 事件流水
CREATE TABLE informs (
  id          INTEGER PRIMARY KEY,
  device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  event_codes TEXT,                      -- JSON 数组
  command_key TEXT,
  retry_count INTEGER,
  current_time TIMESTAMP,
  raw         TEXT,                      -- JSON 摘要（可选）
  created_at  TIMESTAMP
);

-- 会话审计
CREATE TABLE sessions (
  id            INTEGER PRIMARY KEY,
  device_id     INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  cwmp_id       TEXT,
  source_ip     TEXT,
  envelopes     INTEGER DEFAULT 0,
  started_at    TIMESTAMP,
  ended_at      TIMESTAMP,
  end_reason    TEXT
);

-- 文件/固件
CREATE TABLE files (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  file_type  INTEGER NOT NULL,           -- 1 Firmware / 2 Web / 3 Config
  path       TEXT NOT NULL,
  url_path   TEXT NOT NULL,
  size       INTEGER,
  sha256     TEXT,
  version    TEXT,
  model      TEXT,                       -- 适用型号（可空=通用）
  created_at TIMESTAMP
);

-- 预设
CREATE TABLE presets (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  trigger    TEXT NOT NULL,              -- JSON
  actions    TEXT NOT NULL,              -- JSON 数组
  priority   INTEGER DEFAULT 0,
  enabled    BOOLEAN DEFAULT 1,
  created_at TIMESTAMP
);

-- 日志（可选，或仅 stdout）
CREATE TABLE logs (
  id        INTEGER PRIMARY KEY,
  ts        TIMESTAMP,
  level     TEXT,
  device_id INTEGER,
  msg       TEXT,
  fields    TEXT
);
```

索引：`device_params(device_id, name)`、`tasks(device_id, status)`、`informs(device_id, created_at)`、`devices(serial_number)`。

---

## 6. 配置项（全部可覆盖，默认值合理）

以文件（TOML/YAML 二选一）+ 环境变量覆盖的方式提供：

```toml
[server]
listen       = ":7547"                     # 可被 ACS_LISTEN 覆盖
acs_url      = "http://10.0.0.1:7547/acs"  # 下发/写回 CPE 的 URL
path         = "/acs"
max_body     = "4MiB"
tls_cert     = ""                          # 为空则只跑 HTTP
tls_key      = ""

[storage]
driver = "sqlite"                          # sqlite | postgres
dsn    = "file:acs.db?_pragma=busy_timeout(5000)"  # PG 用连接串

[auth]
cpe_realm  = "acs"
# 账号优先从 DB 取；此项为内置/兜底账号
cpe_users  = "acs:secret"

[session]
timeout      = "60s"
max_envelopes = 1
serialize_per_device = true

[connection_request]
enabled = true
timeout = "10s"

[inform]
# 会话结束后保留参数快照的时长（历史清理）
history_retention = "720h"

[log]
level  = "info"
format = "json"                            # json | text
raw_soap = false                           # 是否记录原始报文（排障用）

[security]
log_redact = true
```

**R-CFG-1**：所有与运行环境相关的值（监听地址、ACS URL、数据库、TLS、认证凭证）必须可配置，不得硬编码到代码；
**R-CFG-2**：配置缺失时给出明确默认或明确报错，不静默使用「某台机器的事实」。

---

## 7. 非功能需求（NFR）

| 编号 | 需求 | 目标值（**待实测确认，非项目常量**） |
| --- | --- | --- |
| NFR-1 轻量 | 单二进制、无外部中间件；静态链接可跨平台交叉编译 | 二进制 < 20 MiB |
| NFR-2 并发 | 多设备并行、同设备串行；无数据竞争 | `go test -race` 全过 |
| NFR-3 容量 | 单实例纳管设备数 | 目标 1000+（待压测） |
| NFR-4 性能 | Inform 处理耗时（不含 CPE 侧网络） | P95 < 50 ms（待压测） |
| NFR-5 内存 | 1000 设备常驻内存 | < 150 MiB（待实测） |
| NFR-6 可靠性 | 进程重启后任务不丢；设备锁不残留 | 重启后 pending 任务可继续下发 |
| NFR-7 幂等 | 重复的 Inform/TransferComplete 不产生重复副作用 | — |
| NFR-8 安全 | 密码加密存储、日志脱敏、Fault 不回泄漏内部细节 | — |
| NFR-9 容错 | 脏报文/半截连接/超时不影响其他设备 | 单设备异常隔离 |
| NFR-10 可维护 | 核心协议逻辑可单测覆盖 | 关键路径单测覆盖 |
| NFR-11 可移植 | Linux 为主，代码不绑定特定宿主机 | `GOOS/GOARCH` 可交叉编译 |

---

## 8. 技术选型（建议）

| 方面 | 建议 | 理由 |
| --- | --- | --- |
| Go 版本 | 1.24+（Debian 13 仓库有 `golang-go` 1.24/1.26；或官方 tarball 1.26） | 需要 `log/slog`、`net/http` 新路由等 |
| HTTP | 标准库 `net/http`（1.22+ 的 `ServeMux` 方法+通配符路由） | 零依赖，够用 |
| XML | `encoding/xml` + 自封装容忍未知节点的编解码层 | 标准库即可，避免重型框架 |
| 存储 | **SQLite（`modernc.org/sqlite`，纯 Go，免 cgo）**；可选 `PostgreSQL`（`pgx`） | 纯 Go 才能保住「单二进制、交叉编译」 |
| 迁移 | 内嵌 SQL 迁移（自研极简 或 `pressly/goose`） | 升级不丢数据 |
| 模板/前端 | `html/template` + 原生 JS（或 htmx CDN） | 无构建链，符合轻量目标 |
| 日志 | `log/slog` | 标准库，结构化 |
| 依赖控制 | 核心协议层零第三方依赖；第三方仅限驱动/工具 | 便于审计与长期维护 |

---

## 9. 里程碑（建议迭代）

| 里程碑 | 内容 | 交付标志 |
| --- | --- | --- |
| **M0 骨架** | 项目结构、配置加载、HTTP 端点、SOAP 编解码、SQLite 迁移 | 能收发一个空 Envelope 并回 204 |
| **M1 最小闭环** | Inform + 会话状态机 + InformResponse + Get/SetParameterValues + GetParameterNames + 设备/参数落库 | 模拟 CPE 跑通「上报 → 下发设置 → 回读」 |
| **M2 可控** | 任务队列（重试/过期/去重）+ Connection Request + 设备串行锁 | 能主动唤醒设备并下发任务 |
| **M3 好用** | 预设/策略 + 最小 Web 界面 + REST API + 日志脱敏 + 结构化日志 | 不写代码也能纳管、采集、排障 |
| **M4 运维能力** | 固件 Download + TransferComplete + 文件管理 + Reboot 策略 + 指标 | 完成一次端到端远程升级 |
| **M5 加固** | mTLS、HoldRequests、PG 后端、Upload、压测与性能达标 | NFR 达标 |

---

## 10. 验收标准（可测）

1. **纳管**：全新（模拟）CPE 以 `0 BOOTSTRAP` 接入 → 设备自动登记，参数入库，AI 界面可见。
2. **下发**：对某设备设置 `...ManagementServer.PeriodicInformInterval` → CPE 侧回读值一致，`ParameterKey` 被原样回传。
3. **采集**：对对象路径 `GetParameterNames(NextLevelOnly=true)` 与 `GetParameterValues` 均返回结构正确的树。
4. **唤醒**：点「Connection Request」→ 设备以 `6 CONNECTION REQUEST` 回连并在同一会话内执行任务。
5. **任务可靠性**：断网/超时/CPE 返回 Fault 时，任务按策略重试或失败并留下可读原因；进程重启后 pending 任务继续下发。
6. **升级**：下发 `Download`(Firmware) → CPE 从内置文件服务拉取 → `TransferComplete` 回执，任务置成功且版本号更新。
7. **并发**：>= 若干台模拟 CPE 同时 Inform，无报文错配、`-race` 无告警、无锁死。
8. **脏报文**：非法 XML / 空 body / 未知 RPC / 未知命名空间 → 返回合法 Fault 或 204，服务不崩。
9. **安全**：抓取日志确认无明文密码；数据库内密码非明文。

> 验收建议用**自研 Go CPE 模拟器**（`test/cpesim`）驱动，可在 CI 里全自动跑；同时保留用真实设备/厂商工具做兼容性抽检。

---

## 11. 风险与开放问题

### 风险

| 风险 | 影响 | 缓解 |
| --- | --- | --- |
| 厂商 CWMP 实现不规范（命名空间、空 POST、MaxEnvelopes 语义、脏 XML） | 兼容性差 | 编解码层「容错优先」，保留原始报文开关用于排障 |
| CPE 的 ConnectionRequestURL 是私网地址 | 无法主动唤醒 | 第一期接受限制，文档说明；P2 再考虑 STUN |
| 固件文件大、批量升级打垮链路 | 网络拥塞 | `DelaySeconds` 错峰 + 并发上限 |
| 参数树规模大，全量刷新开销高 | 性能 | 只抓关注节点 + 增量（`4 VALUE CHANGE` 通知） |
| 无真实 CPE 验证 | 上不了生产 | 自研模拟器 + 找一台真实设备抽检 |

### 需要你拍板的开放问题

1. **存储**：只做 SQLite，还是一开始就抽象出 PostgreSQL？（影响驱动与迁移设计）
2. **规模**：预期纳管设备量级（几十 / 几百 / 几千）？决定是否需要 PG 与压测强度。
3. **目标设备**：主要管什么型号（光猫？自家路由？），支持 TR-098 还是 TR-181？有没有可拿到的真机？
4. **认证**：CPE→ACS 用 Digest/Basic 就够，还是要 mTLS？
5. **Web 界面**：是否要（P1）？还是只做 REST API + CLI？
6. **NAT 穿透**：第一期是否明确放弃 STUN 唤醒？
7. **Go 版本**：用 Debian 仓库的 1.24/1.26，还是官方最新（1.26）tarball？
8. **是否需要**「参数期望值 vs 实际值」的影子模型（做配置漂移检测）？

---

## 12. 当前环境备注

- 工作目录 `/path/to/Desktop/ACS` 为空，未初始化 git。
- 本机（LMDE 7 / Debian 13）**尚未安装 Go**；`apt` 中有 `golang-go`（1.24/1.26 backports）可选。
- 编码前需先：安装 Go、`git init`、确定模块路径（如 `github.com/xxx/acs`）。
