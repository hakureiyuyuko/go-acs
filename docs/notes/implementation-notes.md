# 实现笔记（S1）

记录实现过程中的判断、实测结果，以及「为什么这么写」。
凡是数字都标注清楚是**本次实测值**还是**设计目标**。

## 环境

- 开发机：LMDE 7 (Debian 13)，内核 6.12，x86_64，4 核 / 3.8G 内存。
- Go 1.27.1（官方 tarball 装在 `~/.local/go`；`apt` 里的 `golang-go` 也可用，但那需要 sudo）。
- SQLite 驱动：`modernc.org/sqlite`（纯 Go，`CGO_ENABLED=0` 也能编译 ⇐ 这是选它的唯一理由：
  要保住「单静态二进制 + 交叉编译」这条底线）。
- **本机实测**：`acs` 二进制 20.4 MiB、`cpesim` 15.7 MiB（`CGO_ENABLED=0`，未 strip）。
  超过需求文档 NFR-1 里写的 20 MiB 目标一点 —— 主要来自 SQLite。压缩/symbol 剥离后还会小不少，
  但那是发布环节的事，先不优化。

## 协议相关的判断

### 会话怎么关联？（重要）

CWMP 每个 HTTP POST 只带一个 RPC，而 ACS 的 HTTP 是无状态的，所以「这几个 POST 属于同一次会话」
必须靠某种线索串起来。查了 GenieACS 的做法：它给 CPE 下发一个 `session=<id>` 的 cookie
（`lib/cwmp.ts`），下次请求靠 cookie 找回会话；**没有 cookie 就当成新会话**。

我们的做法是在它之上加了兜底：

1. 有 `session` cookie → 直接命中；
2. 没有 → 用 `来源IP | User-Agent | (Basic 用户名)` 做指纹。

第二层是必需的，因为不是所有 CPE 都回传 cookie。代价是同一 NAT 后、UA 又完全相同的两台设备
可能被误判成同一会话 —— 对轻量场景可接受，而且 cookie 路径会先兜住绝大多数情况。

cookie 解析特意写得宽松（`,` 和 `;` 都当分隔符、允许带引号），因为实测有设备不按规范的
`;` 来（GenieACS 的注释里也提到了同一个坑）。

### 「没活干了」用 204

GenieACS 的 `soap.response(null)` 返回 `{code: 204}`，所以我们也用 **204 No Content** 表达会话结束。
时序是：

```
Inform          → 200 InformResponse
空 POST         → 200 下一个任务 / 204 结束
任务响应        → 200 下一个任务 / 204 结束
```

注意第二行和第三行可以统一处理：**收到 CPE 的响应之后，响应体里可以直接带下一个请求**
（TR-069 允许一个 HTTP 请求/响应各承载一个 RPC）。这样一次会话能连着下发多条指令，不用来回空 POST。

### 错误码方向

- **9xxx 是 CPE 发给 ACS 的**（例如 CPE 回 `9000 Method not supported`）；
- **8xxx 是 ACS 发给 CPE 的**。

查 GenieACS `lib/cwmp.ts` 里对未知 RPC 的回复：`faultcode=Server` + `cwmp:Fault/FaultCode=8000`，
跟我们的一致。所以坏报文/未知方法我们回 8003 / 8000，不该回 9xxx。

### 数据模型根不能写死

`InternetGatewayDevice.`（TR-098）和 `Device.`（TR-181）两套命名必须都支持，而且**不能靠猜**。
做法：Inform 带的参数名里看前缀；看不到（比如某些 CPE 的 Inform 不带参数）就把两种命名的
同一批参数一起发给 CPE，谁存在谁回 —— 设备自己会告诉我们答案，这是探测。

## 实测发现的真问题

### 1. 命名空间回填（单元测试抓到）

`ParseEnvelope` 里写成了：

```go
if ns, ok := CWMPVersionOf(m.NS); ok {
    env.CWMPNS = ns   // ns 是 "1.0"，不是完整 URI！
}
```

结果回给 CPE 的信封变成 `xmlns:cwmp="1.0"`。**端到端测试完全没发现**，因为自研模拟器按
本地名（local name）匹配方法，不看命名空间 URI。单元测试断言完整 URI 才暴露出来。

→ 教训写进 README：**只跟自己的实现对打，会两边一起犯同一个错**。所以引入了
`scripts/verify-interop.sh`（用 GenieACS 的 JS 模拟器）。

### 2. 子树路径的 GetParameterValues 不能指望（交叉验证抓到）

第一版取设备信息是下发子树路径 `InternetGatewayDevice.DeviceInfo.`。用 GenieACS 官方
CPE 模拟器打过来时它直接崩：

```
TypeError: Cannot read properties of undefined (reading 'replace')
    at methods.js:240  (GetParameterValues)
```

看它的实现：`methods.js` 的 `GetParameterValues` 对每个请求名直接 `device[name][1]` 查表，
没有做「部分路径展开」；而它的 `GetParameterNames` 反倒支持 `startsWith(parameterPath)`。
真机大多支持子树（是规范允许的），但既然有实现会在这里栽，就没必要赌。

改成下发 14 个显式参数名（`DeviceInfo.*` + `ManagementServer.*`），根未知时两种命名都发。
好处：
- 兼容性最好（GenieACS 自己也这么干）；
- 一轮 RPC 就拿到基本信息，不用 `GetParameterNames` + `GetParameterValues` 两轮；
- 顺带完成数据模型根探测。

### 3. 交叉验证的收获

改完之后用 GenieACS 官方模拟器（华为 BM632w 数据模型）打我们的 ACS，结果：

| 字段 | 取到的值 |
| --- | --- |
| 厂商 | Huawei Technologies Co., Ltd. |
| 型号 | BM632w |
| OUI | 202BC1 |
| 软件版本 | V100R001IRQC56B017 |
| 硬件版本 | 40501 |
| 数据模型根 | InternetGatewayDevice. |
| 参数数 | 15 |

这证明我们的 XML 解析对**第三方实现的报文**是真的可用，而不只是能读自己生成的东西。

## 存储相关的判断

- **时间统一存 RFC3339(UTC) 文本**：定长、字典序即时间序，可以直接用 `<` 比较、也能按序排列。
- **`SetMaxOpenConns(1)`**：轻量场景最省心的做法，彻底绕开 `SQLITE_BUSY`。代价是读写全串行，
  设备量上来（NFR-3 目标 1000+）需要换成「写队列 + 多读连接」，届时要改这里。
- **合并规则：新值非空才覆盖**。因为 Inform 每次只带部分参数，不能因为某次没带某字段
  就把已经知道的信息抹掉（需求文档 R-INF-1）。
- **`writable` 用 `MAX()` 合并**：Inform 不带可写信息，不能把 `GetParameterNames` 探到的
  `writable=true` 覆盖成 false。
- **`MergeDeviceFields` 与 `UpsertDevice` 分开**：处理 `GetParameterValuesResponse` 时补齐型号版本，
  但**不该刷新 `last_inform_at`**（那会让「最后上报时间」失真）。

## 待办 / 已知取舍

- 任务失败**不自动重试**（`retry_count` 字段已预留）；需求里 NFR-6 只要求「重启不丢」，
  这个已满足（启动时 `ResetRunningTasks`）。
- 任务去重只按 `(device_id, kind)`，粒度偏粗：以后同设备可能有「查 WiFi」和「查 WAN」两条
  `GetParameterValues`，需要按 payload 摘要去重。
- 会话状态只在内存里，没落 `sessions` 表（需求文档 §5 里有，S1 先不做）。
- 没有做 `AddObject`/`DeleteObject`，所以还没有多实例对象的增删能力。
- schema 目前是「一次性建表 + IF NOT EXISTS」，**加字段前必须先引入版本迁移**，
  否则老库升不上去。

## 真机联调（2026-09-28，已跑通）

接了一台**真实 CPE** 指向本机 `9090` 端口上报，完整跑通了纳管 + 取信息。

### 真机信息

| 项 | 值 |
| --- | --- |
| 设备 | 华为 OptiXstar **HN8145X6N**（FTTR 光猫）|
| User-Agent | `HW_WAP_CWMP_V02` |
| OUI / 序列号 | `00259E` / `48575443AA000001` |
| 软 / 硬件版本 | `V5R023C00S120` / `35A0.E` |
| 数据模型 | `InternetGatewayDevice:1.4`（TR-098 Amendment 4）|
| 上报周期 | 120 秒 |

`DeviceSummary` 里还带了它支持的完整能力集，很有用（以后按能力置配策略靠它）：

```
InternetGatewayDevice:1.4[](Baseline:1, EthernetLAN:1, WiFiLAN:2, Time:1, IPPing:1, DeviceAssociation:1),
VoiceService:1.0[1](Endpoint:1, SIPEndpoint:1)
```

### 真机暴露出来的坑（逐条）

1. **它 POST 到根路径 `/`，不是 `/acs`**
   很多 CPE / 运维就是把 ACS URL 配成 `http://host:port/`。之前只监听 `/acs`，会直接 404 收不到。
   → 现在 `POST /` 也接（`mux.Handle("POST /{$}", srv)`）。**这是本次最有价值的一个修改。**

2. **报文用大写前缀 `SOAP-ENV:` / `SOAP-ENC:`**
   我们的解析只按本地名匹配，天然不受影响（单测里有这条断言）。

3. **自闭合空元素**：`<CommandKey/>`、`<Value xsi:type="xsd:string"/>`
   必须解出空字符串而不是报错。`ProvisioningCode` 在真机上就是空的，现在存的是 `""`。

4. **`CurrentTime` 用 `2026-09-28T06:58:51+00:00`**（时区写成 `+00:00` 而不是 `Z`）。
   我们只存不解析，所以没影响；但如果以后要解析，不能只按 RFC3339 的 `Z` 形式写。

5. **它原样回填我们下发的 `cwmp:ID`**（例：`0abee7239d3f8942`）。
   说明我们生成的 ID 它是认的，以后可以拿它做请求/响应配对。

6. **`GetParameterValues` 只返回请求里存在的参数**，不存在的既不返回也不报错。
   这正好是我们探测数据模型根所依赖的行为 —— 真机上验证过了。

7. **厂商笔误：根节点名大小写写错**
   0 BOOTSTRAP 时它上报 `InternetGateWay**D**evice.DeviceInfo.X_CT-ProvCode`（根里的 `W` 是大写），
   配合值 `00/0`（华为自己的 provisioning code）。其他参数都是正确的 `InternetGatewayDevice.`。
   → 解析和存库本来就是容错的（原样保留）；但根探测改成**大小写不敏感**，
   并在日志里 Warn 提一句（`findRootTypo`）。**参数名绝不改写** —— 以后 `SetParameterValues`
   必须用设备自己的拼法。

8. **Inform 的 ParameterList 里没有 `DeviceInfo.Manufacturer`**
   厂商只在 `DeviceId` 里给。这再次印证了「Inform 不能当全量」的设计，
   也说明 `pick(fields.Manufacturer, DeviceId.Manufacturer)` 这种兜底是必要的。

9. **`ConnectionRequestURL` 是可直连的**
   `http://192.168.10.22:7547/0123456789abcdef0123456789abcdef` —— 虽然是私网地址、
   还带一长串随机路径，但设备就跟我们在同一个局域网，**从 ACS 直接可达**。
   也就是说下一步做 Connection Request（主动唤醒）时，**可以用这台真机做真实验收**，
   不像很多部署那样被 NAT 挡住。

### 看板上的 WiFi 概览（2.4G / 5G + 终端数）

首页（`/`）新增「WiFi 概览」表，每台设备的每个频段一行：频段、SSID、射频开关、信道、
标准、加密、已连终端数；顶部统计卡里加了「无线终端」总数，设备列表里也有一列。

采集方式（重要）：**不是把整棵 WLAN 子树拉回来**。先用 `GetParameterNames` 枚举子树，
把真实实例号圈出来（这台机器是 1 和 5），再按**后缀白名单**（`wifiSummarySuffixes`，
只有 `.SSID` / `.Channel` / `.TotalAssociations` … 共 19 项）筛出十几个名字去取值。
对比：整棵子树是 376 个叶子参数，摘要只要 22 个 —— 轻了一个数量级。

两个细节：

- 枚举出来的几百个名字**不写库**（GPN 任务载荷里的 `skip_store`），否则参数表会被几百个空值刷屏。
- 频段**以设备自报为准**（`X_HW_RFBand` / `OperatingFrequencyBand`）；设备没报就显示「实例 N」，
  不根据实例号猜（真机上 5G 是实例 5，猜成「实例 2 就是 5G」会错）。

采集时机：首次纳管或收到 `0 BOOTSTRAP` 时自动做一次（`-auto-fetch-wifi`，默认开）；
界面上每个设备都有「重新采集」按钮，也可 `POST /api/devices/{id}/wifi`。

### 修改 WiFi：表单 → SetParameterValues → 读回核对

实现了商用 ACS 那个「点 SSID 改配置」的流程：看板或详情页点 SSID → 编辑表单 → 提交。

#### 表单字段不写死，全部从设备实报的参数推导

每个字段是一条声明（`wifiFieldDefs`），带**叶子名候选**（如 `ssid`、`radioenabled`、
`ieee11iencryptionmodes`），按优先级取第一个在设备参数里存在的；设备没这个参数，
**这个字段就不出现**——而不是给个空框。好处：

- TR-098（`WLANConfiguration.{i}.`）与 TR-181（`WiFi.Radio/SSID/AccessPoint`）都能用，
  因为索引用的是叶子名而不是完整路径；
- 同一功能厂商私有参数和标准参数成对存在时，也能按优先级选中。

下拉框的候选值也是设备给的（这才是 ACS 该有的做法，比界面里写死列表靠谱）：

| 字段 | 候选来源 | 真机的值 |
| --- | --- | --- |
| 无线信道 | `PossibleChannels` | `1,2,…,13` |
| 发射功率 | `TransmitPowerSupported` | `20,40,60,80,100` |
| 加密方式 / 算法 / 标准 | 标准枚举（BeaconType / EncryptionModes / Standard） | 预选中当前值 |

当前值不在候选里时会把当前值补进去（选不中就尴尬了）。真机上就碰上了：
`TransmitPower=200`，而 `TransmitPowerSupported=20,40,60,80,100` —— 设备自己的两个值对不上，
界面会显示「200（当前）」。拿不到任何候选值时就退回普通文本框，不给空下拉框。

#### 只下发改动过的字段

提交时逐个字段跟当前值比对，没变的不入队（密码则「留空 = 不修改」——很多 CPE
本来就不返回明文密码，无法“改成一样”）。好处是写入面最小，而且不会把没动过的参数重写一遍。

#### 下发后自动读回核对

收到 `SetParameterValuesResponse` 且 `Status=0` 后，我们**在同一会话里把刚写的参数再读一遍**，
而且只多一次 HTTP 往返（不额外等设备轮询）。理由：写入成功不等于真的生效，
有的 CPE 会默默接受写入但某个参数不生效 —— 不读回来根本发现不了；顺便把界面上的值刷成实际值。

失败路径也分得很清楚：`Status!=0` 记失败；CPE 回 Fault 时把逐条的 `SetParameterValuesFault`
（哪个参数、什么错误码）都写进任务结果。

#### 顺手修掉的一个真问题：xsd 类型名不能小写

原来我们把类型名一律转小写存，写回去就发 `xsi:type="xsd:unsignedint"`。
XML Schema 类型**大小写敏感**，`xsd:unsignedint` 不合法，严格的 CPE 会拒收（9008）。
因为只影响写入路径，读写展示一直看不出来。已加 `canonicalType()` 在解析与写出去时都归一化。

#### 界面上的一个取舍

表单里每个字段下面都把**实际要写入的 TR-069 参数名**印出来。商用 ACS 一般不显示，
但对我们这种要拿真机练手/排障的场景很有用 —— 一眼能看出到底在改哪个参数。

另外加了「高级：直接设置任意参数」，把表单没列出来的项（例如这台设备没有
`OperatingChannelBandwidth`，所以「信道带宽」一栏不出现）留给用户自己填。

### 实战：读真机的 WiFi 信息（顺带挖出一个真 bug）

用界面上的「读取参数子树」或 `POST /api/devices/1/fetch` 对
`InternetGatewayDevice.LANDevice.1.WLANConfiguration.` 做一次读取。

#### 做法：枚举 + 取值，在同一个会话里完成

不能直接下发子树路径给 `GetParameterValues`（见前面「两个真问题」的第 2 条），所以走两条命令：

```
Inform                          → 200 InformResponse
空 POST                         → 200 GetParameterNames(<子树>)
GetParameterNamesResponse (437)  → 200 GetParameterValues(<前 200 个名>)
…Response (200)                  → 200 GetParameterValues(<后 176 个名>)
…Response (176)                  → 204 结束
```

关键点：**收到 GetParameterNamesResponse 的那个 HTTP 响应里就直接带上第一条 GPV 请求**，
所以整个枚举 + 取值只花一次会话（实测 15:10:52.401 → 15:10:53.449，**约 1 秒**），
不用再等设备下一次轮询（否则要等 120 秒）。

#### 挖出来的真 bug：CPE 单次只回 256 个参数，超出**静默丢弃**

第一次读的时候我们向设备请求了 376 个参数名，它**只回了 256 个（正好 2^8）**，
不报错、不告知。上一轮请求 300 个，也是回 256。

当时我们的代码会把「设备回了一批」直接当作任务成功 —— 结果是**默默少采集了 120 个参数**，
而且没有任何错误信号。这比协议报错危险得多。

→ 修法：新增 `MaxParamsPerRequest`（默认 200，`-max-params-per-request` / `ACS_MAX_PARAMS_PER_REQUEST` 可调），
入队 GPV 时自动分批（`enqueueGPVDivided`）。分批不会变慢，因为各批还是同一个会话里依次下发。
修完后同一批 376 个参数拆成 **200 + 176** 两批，全部取回，比对结果：**376/376，一条不丢**。

另外加了一道防御 `warnIfPartialResponse`：任何时候发现「回的比请求的少」就记 WARN，
以后遇到上限更低的设备能立刻看出来。

> 这个坑单靠自研模拟器永远发现不了 —— 我们的模拟器会老老实实把 376 个全返回。
> 又一次说明：**必须拿真机（和真实报文）验收**。

#### 读出来的 WiFi 信息

| | 2.4GHz | 5GHz |
| --- | --- | --- |
| 实例号 | `WLANConfiguration.**1**` | `WLANConfiguration.**5**` |
| SSID | `WirelessNet` | `WirelessNet-5G` |
| 频段（`X_HW_RFBand`）| 2.4GHz | 5GHz |
| 无线标准 | 11ax | 11ax |
| 信道 | 6（自动）| 0 |
| 认证 / 加密 | WPA/WPA2-PSK / TKIPandAES | 同左 |
| 射频开关 | 开 | **关**（`RadioEnabled=0`，`Status=Disabled`）|
| BSSID | `02:A0:BE:48:50:A0` | `02:A0:BE:48:50:A4` |
| 已连设备数 | 1 | 0 |

值得记的两个细节：

- **实例号是 1 和 5，不是 1 和 2**。2.4G 用 1、5G 用 5 是光猫厂商的常见习惯，
  所以获取 WiFi 必须先枚举（`GetParameterNames`），不能写死 `WLANConfiguration.1` 和 `.2`。
- **WPA 密码读不到**：`KeyPassphrase`、`PreSharedKey.1.*`、`WEPKey.*.WEPKey` 全部返回**空串**，
  但 `Writable=true` —— 也就是**能改不能读**。这是厂商故意的（TR-069 常在未鉴权的 LAN 口上开着，
  回明文 PSK 等于把 WiFi 密码送出去）。做界面时不要把它当成“没读到”，要标明“设备不允许读取”。

#### 连在 2.4G 上的客户端

`AssociatedDevice.1` 下 29 个参数，华为还附了私有的信号质量字段：

| 字段 | 值 |
| --- | --- |
| `X_HW_AssociatedDevicedescriptions` | `Android-Phone` |
| `AssociatedDeviceMACAddress` | `02:76:B9:B7:C3:4A`（随机化 MAC）|
| `AssociatedDeviceIPAddress` | `192.168.30.3` |
| `X_HW_WorkingMode` | `11ax` |
| `X_HW_RSSI` / `X_HW_SNR` / `X_HW_SingalQuality` | `-26` / `60` / `68` |
| `X_HW_TxRate` / `X_HW_RxRate` | `243` / `286` |
| `X_HW_Uptime` | `1428` 秒 |

注意客户端 IP 是 `192.168.30.3`（光猫的 LAN 侧），而光猫自己对我们呈现的是 `192.168.10.22`
（它作为 CPE 的 WAN/管理 IP）—— 两张网是由它 NAT 隔开的。

### 真机写入实验：三个值得记的行为（2026-09-28）

在真机（华为 OptiXstar HN8145X6N）上实际跑了几次写入，碰到三个之前想不到的行为：

#### 1. 无线参数是**异步生效**的：立即读回会误报

改了 `WLANConfiguration.5.RadioEnabled`（开 5GHz 射频）：

```
15:26:52.165  下发 SetParameterValues
15:26:57.691  设备回 Status=0（接受了，耗时 5.5 秒）
15:26:57.764  同一会话内读回：RadioEnabled 还是 0
…
几分钟后再查：RadioEnabled = 1          ← 真的生效了，只是晚
```

所以第一版的「读回对不上就判失败」会**误报**。改成两级：

- **同会话立即读回**：对得上就直接结案；对不上**先不判错**
  （可能就是还没生效），把核对任务排到**设备下一轮会话**再跑；
- **下一轮会话的延后核对**：还对不上才算真没生效，把设置任务标为失败。

实现上不需要加延时列：`ClaimNextTask` 支持一个 `exclude` 列表，
会话对象记住「本轮要跳过的任务 ID」，会话结束时清空 —— 任务自然就落到下一轮了。

#### 2. 密码要写到 `PreSharedKey.1.KeyPassphrase`，不是 `KeyPassphrase`

往 `WLANConfiguration.{i}.KeyPassphrase`（同实例下那个空的）写密码，设备直接拒：

```
CPE 返回错误 9003: Invalid arguments
  | InternetGatewayDevice.LANDevice.1.WLANConfiguration.5.KeyPassphrase
    -> 9007 Invalid parameter value
```

因为 WPA/WPA2-PSK 的密码在 `PreSharedKey.1.KeyPassphrase` 下，
而 `WLANConfiguration.{i}.KeyPassphrase` 是给 WEP 的。

顺带说明：**设备把逐参数错误报得很清楚**，我们的 Fault 处理把
`SetParameterValuesFault`（哪个参数、什么码、什么原因）完整记进了任务结果。

→ 修法：表单字段的候选从「只比最后一段」改成「支持多段尾部路径」，
密码字段的候选顺序改为 `presharedkey.1.keypassphrase` → `keypassphrase`。
（原来靠叶子名索引，这两个参数的叶子名一样，永远选不到正确的那个。）

#### 3. 写 `SSID` 是同步生效的

改 2.4G 的 SSID（`WirelessNet` → `HomeWifi`）：下发后同一会话读回就是新值，一次到位。
说明不是所有参数都异步 —— 所以「同会话能对上就不多跑一轮」这个优化是值得的。

#### 未解的一个疑问

5GHz 的 `RadioEnabled` 已经是 `1` 了，但 `Status` 一直是 `Disabled`、`Channel` 是 `0`。
可能是还要动别的（厂商私有参数 / 重启生效），也可能设备就是不上报。
先记下来，不猜。

#### 4. 只看库里的值会把人带坑里（被用户当场发现）

改完 5GHz 射频开关后，我查库里看到 `Status=Disabled`，就报告说「5G 没起来」。
用户回：**能搜到信号啊**。

一查时间戳就知道错了：

```
Status / Channel / ChannelsInUse   最后更新 15:10:53   ← 开射频之前采的
RadioEnabled                       最后更新 15:26:57   ← 写入后回读的
```

写入只回读了改动的那**一个**参数，而界面上的状态/信道/终端数这些**相关联**的值
就停在写入前了。重新采一次就真相大白：`Status=Up`、`Channel=36`。

> 教训：**别拿库里的值下结论**，先看它的更新时间（或直接重采一次）。

→ 两个改动：

1. 写入无线参数后，自动排一轮「重采无线概况」（**延后一轮**，因为无线参数异步生效），
   这样界面上相关联的值会自己跟上；
2. 界面上把**每个参数的更新时间**和**每个频段的采集时间**直接显示出来 ——
   让「这是新的还是旧的」一眼可见，而不是靠人记。

#### 接入第二台真机（零改动）

后来又接了一台 **华为 V271-20**（FTTR 主网关，软件 `V5R023C10S326`）到同一台 ACS，
**一行代码都没改**就自动纳管了：

| | HN8145X6N（第一台） | V271-20（第二台） |
| --- | --- | --- |
| 无线标准 | 11ax (Wi-Fi 6) | **11be (Wi-Fi 7)** |
| 2.4G | 信道 5，11ax | 信道 11，11be，SSID `LabWifi`，**5 台终端** |
| 5G | 信道 36，11ax | 信道 36，11be，SSID `LabWifi-5G` |
| 无线实例号 | 1 / 5 | 1 / 5 |
| 无线参数名个数 | 437 | **561** |
| 总采集参数 | 454（注） | **68** |

> 注：第一台的 454 是因为我手工拉过整棵 WLAN 子树；只算自动采集的话两台量级一样，
> 都是 「Inform + DeviceInfo 14 个 + 无线摘要 26×2」 —— 第二台的 **68** 才是「无人干预」下的常态。
> 这反过来证明「只取摘要 + 枚举名不写库」的设计是有效的。

两个顺带验证到的点：

- 无线实例号又是 **1 和 5**（不同型号但同一厂商习惯一致），
  再次说明不能写死 `.1`/`.2`，必须枚举；
- 第二台 5G 的 `BeaconType` 是 **`WPA2/WPA3`** —— 不在我们写死的加密方式选项里。
  因为做了「当前值不在选项里就补进去」这个兵库，下拉框照样能正确预选，
  不会出现“选不中”。

### 设备备注与概览页搜索

概览页（`/`）加了搜索框、设备列表加了「备注」列；详情页可以写备注。

**搜索是服务端过滤**（`GET /?q=...`），不是纯前端过滤。两个原因：

- 结果 URL 可以分享（「把那台有问题的设备的搜索结果发我」）；
- 不依赖 JS 也能用（搜索按钮照常提交）。

输入框上挂了个 350ms 防抖的自动提交，所以感觉上跟即时过滤一样；
刷新后会把光标放回输入框末尾，连续输入不被打断。

搜的字段：序列号 / 备注 / 名称（厂商+型号）/ 产品类 / OUI / 广播的 SSID。
按 SSID 找设备其实很常用（“那个叫 LabWifi 的是哪台”）。

备注存在 `devices.note` 里：

- **设备上报不会动它**（`UpsertDevice` / `MergeDeviceFields` 的 UPDATE 语句里都没有它，
  这是有意的：人工写的东西不能被设备冲掉）；
- 限长 200 字符（备注是给人看的，防止把整篇文章塞进来）；
- 只在本地保存，**不会下发给设备**（不是 TR-069 参数）。

顺带把设备列表里的「OUI」列去掉了 —— 表太宽会折行，而 OUI 在详情页的
基本信息里本来就有，也仍然能搜。

### 顺带：终于有了正经的数据库迁移

加备注字段需要改表结构，于是把迁移机制补上了：库里记 `PRAGMA user_version`，
`migrations` 是一个按序执行的 SQL 列表。

**约定：`baseSchema` 永远是「第 0 版」，新加字段一律走迁移。**
不要直接改 `baseSchema` —— 那样已存在的库升不上来（列不会自己出现），
而新建的库又会因为重复建列而失败。

这是本项目第一个迁移，所以特意：

1. 先在**真库的副本**上跑一遍，确认 `user_version 0 -> 1`、`note` 列加上、
   两台真机数据完好；
2. 再验证**重复启动幂等**（第二次启动不会再建列）；
3. 最后才动真库（动之前先备份了一份）。

单测 `TestDeviceNoteAndMigrationIdempotent` 把「备注不被上报冲掉」和
「同一个库连续打开三次都不出错」都盖住了。

### 抓到的真机报文已固化成回归样本

放在 `internal/cwmp/testdata/`（不是手写的，是这次联调实际抓下来的原始字节）：

| 文件 | 内容 |
| --- | --- |
| `huawei-hn8145x6n-inform.xml` | 真机 Inform（大写前缀、自闭合空元素、8 个参数）|
| `huawei-hn8145x6n-gpv-response.xml` | 真机对 GetParameterValues 的响应（14 个参数）|
| `acs-getparametervalues-request.xml` | **我们发出去的**请求（用于回归保护：必须全是显式参数名）|

对应 `internal/cwmp/realdevice_test.go` 里的 4 个用例。
手写样本容易「按自己的理解写」而掩盖真机怪癖，用真机报文做回归才守得住。

### 联调时的运维细节

- 用 `scripts/dev-server.sh {start|stop|status|log}` 管理后台进程（带 pidfile）。
- **别用 `pkill -f 'bin/acs'`**：命令包装器自己的命令行里就含 `bin/acs`，会把自己一起杀掉。
  用 `pkill -x acs` 或脚本里的 pidfile。
- 排障时这样起：`ACS_LOG_LEVEL=debug ACS_LOG_SOAP=1 scripts/dev-server.sh restart`，
  日志里能同时看到收到的报文（`CPE 报文`）和发出的报文（`发出报文`）。
- 新增的请求日志中间件会把「打进来但没被处理」（404/405）在 **WARN** 级别记下来 ——
  真机接不上时，这是第一眼要看的信号。

---

## 验收记录（本次实测）

| 项目 | 结果 |
| --- | --- |
| `go test ./...` | 全部通过（`internal/cwmp` 27 个用例、`internal/store` 6 个用例、`internal/web` 9 个用例）|
| `scripts/verify-s1.sh` | **通过 77 / 失败 0** |
| `scripts/verify-interop.sh` | 通过（GenieACS 官方模拟器可完整纳管） |
| 真机（华为 HN8145X6N + V271-20） | **两台不同型号均自动纳管成功**；实测过 SSID 改名、开 5GHz 射频、写密码（后者发现参数选错）|
| 界面渲染 | 用 headless Chrome 截图确认（列表页 + 详情页） |

> 以上都是**本次实例的实测值，不是项目常量**。
