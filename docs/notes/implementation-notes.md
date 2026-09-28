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
标准、加密、已连终端数；设备列表里有一列「无线终端」。
（顶部统计卡一开始放了 6 个：已纳管设备 / 在线 / 已采集参数 / 无线终端 / 待办任务 / 失败任务；
后来按用户要求**只留前两个** —— 看板只看「有多少台、在线几台」，其余的点进设备里看更准。）
顺带把设备列表那一列「无线终端」也统一成**主机 + 子光猫**之和：以前它只看主机自己的
`TotalAssociations`（设备 2 显示 6），详情页却已经按「合计」显示（2.4G 11 + 5G 2 = 13），
同一个词两个数字很容易让人觉得哪里算错了。现在两边都用同一套（残留行同样按条目数截断）。

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

### 浏览参数树：不猜路径，先让设备自己说出来

接新设备（尤其是厂商私有对象一大堆的光猫、FTTR 主机）时，盲猜参数路径代价很高 ——
真机上猜错一次就是一整轮上报周期（120 秒）。所以加了一个能力：

```bash
# 只枚举名字，不取值。next_level=true 表示只看直接子节点
POST /api/devices/2/names {"path":"InternetGatewayDevice.","next_level":true}
```

### 实战：从 FTTR 主机里读出子光猫

用户那台 V271-20 是 **FTTR 主机**，问能不能读到子光猫（从设备）信息。

**第一轮**：`next_level` 枚举 `InternetGatewayDevice.` -> 拿到 **72 个顶层对象**，
一眼就看到了几个可疑的：`X_HW_APDevice.`（AP 设备表）、`X_HW_SmartTopo.`（拓扑）、
`X_HW_WifiCoverService.`、`X_HW_EasyMeshSwitch.`。

**第二轮**：直接对 `X_HW_APDevice.` 做全量枚举 -> **337 个参数**，就是子光猫表。

两轮就拿到了，一共没用几次猜测。读出来的东西（真机数据）：

| 实例 | 型号 | 序列号 | MAC | 软件版本 | 硬件 | 在线 | 2.4G/5G 信道 | 在线时长 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | **K251e** | HWTCAA000001 | 02:73:E2:51:DA:EA | V5R023C10S326 | 3A17.A | 是 | 6 / 36 | 361:36:38 |
| 2 | **K251-20** | 48575443AA000002 | 02:16:C8:61:F4:BC | V5R023C10S300 | 3B78.A | 是 | 1 / 36 | 361:36:42 |
| 4 | **K251-20** | 48575443AA000003 | 02:40:08:CE:FB:7F | V5R023C10S300 | 3B78.A | 是 | 7 / 36 | 361:36:36 |

每个子光猫 21 个顶层字段：`SerialNumber` / `DeviceType` / `SoftwareVersion` /
`HardwareVersion` / `APMacAddr` / `ApOnlineFlag` / `DeviceStatus` / `UpTime` /
`CurrentChannel`（2.4G,5G 两个） / `SupportedRFBand` / `TransmitPower` / `SignalIntensity` /
`SyncStatus` / `WorkingMode` / `SupportedWorkingMode` / `InternetAccessMode` / `UUID` 等。

三个值得记的点：

1. **实例号又是 1 / 2 / 4**（缺 3）。加上之前无线实例是 1 和 5，可以确定：
   厂商的实例号真的随时会不连续，一律得枚举，绝不能写死。
2. **每个子光猫还带自己的 `WLANConfiguration.`（105 个参数）** —— 也就是说
   **每个房间那个 AP 的 WiFi 也能单独读、单独改**。它跟主设备自己的
   `LANDevice.1.WLANConfiguration.` 是两套路径，不要搞混。
3. `X_HW_SmartTopo.` 对象存在（7 个参数：RSSI 阈值、丢包率阈值、测量时长等），
   但**全是空的** —— 对象有不代表有数据，界面要能优雅地空着。

### 主时唤醒（Connection Request）：不用再等周期上报

需求原话是「ping 之类的信息能不能通过 ConnectionRequestURL 立刻拿结果，而不是等下一次上报」。
能 —— 这就是 TR-069 Annex A 的 Connection Request。

#### 真机实测（最有说服力的一段）

```
16:19:10.261  已入队：ping 诊断
16:19:10.272  主动唤醒成功
16:19:10.331  收到 Inform  events="6 CONNECTION REQUEST"   ← 59 毫秒后设备就回连了
16:19:10.338  下发任务 task_id=52 kind=Diagnostics         ← 立刻下发
16:19:14.471  诊断完成
```

**从点「诊断」到任务下发 77 毫秒，到出结果 4.2 秒。** 原来是等下一次周期上报，
真机上最多 120 秒。

#### 路上碰到的真问题：设备不回读 ConnectionRequest 账号密码

直接 GET 设备的 ConnectionRequestURL，得到的是一一 401 + Digest 挑战：

```
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Digest realm="HuaweiHomeGateway",nonce="…",qop="auth",algorithm="MD5"
```

要认证，但**设备就是不回读** `ConnectionRequestUsername` / `ConnectionRequestPassword`
（实测两个都是空串）。查了一下，这两个参数**可写** —— 所以答案就是：
**ACS 自己 provision 它们**。这也是标准做法（凭据本来就是 ACS 配的）。

实现上的两个坑：

1. **不能只在 BOOTSTRAP 时 provision**。已纳管的设备大多只发 `2 PERIODIC`，
   那样永远写不进去。改成每轮 Inform 都检查，但用**进程内标记**拦住重复下发：
   这两个参数设备不回读，我们无从从库里确认它已经生效，只能自己记。
2. **密码必须跳重启稳定**。不然每次重启 ACS 都会换一个密码，
   然后要把新凭据重新写进每台设备，中间那段时间唤醒必然 401。
   所以新增了一张 `settings` 表（迁移 #2），自动生成的密码存进去。

#### Digest 实现是用标准测试向量验证的

Digest 算错了**不会报错，只会一直 401** —— 这是最难 debug 的一类 bug。
所以直接拿 **RFC 2617 第 3.5 节的例子**当测试向量，把 HA1/HA2/response 三步都对一遍。
另加用例：引号内含逗号的挑战、无 qop 的老式挑战、Basic。

> 模拟器那边**独立实现**了一遍 Digest 校验（不调 ACS 的代码）。
> 两边各写一遍才能互相验证 —— 只用自己的实现在自己身上试，算错了一起错。

#### 不依赖厂商：非标准化的部分

- 认证方式：Digest 与 Basic 都支持（设备用 `WWW-Authenticate` 告诉我们要哪个）。
- 只接受 http/https：ConnectionRequestURL 是设备给的，不能让它把我们指向别的协议。
- 唤醒失败不影响正事：任务仍然会在下一次周期上报时下发，只是慢一点。

### WAN 连接信息

跟 WiFi 同一套做法：枚举子树拿到真实实例号，再按后缀白名单只取十几个字段
（真机整棵 WAN 子树有 458 个参数，摘要只取 19 个）。

读出来的东西（真机）：

| 连接 | 状态 | IP / 掩码 | 网关 | MAC | 寻址 | 业务 | VLAN |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1_INTERNET_R_VID_ | Connected | 192.168.10.21 / 255.255.252.0 | 192.168.10.1 | 02:A0:BE:48:50:9B | DHCP | INTERNET | 0 |
| 2_TR069_R_VID_ | Connected | 192.168.10.22 / 255.255.252.0 | 192.168.10.1 | 02:A0:BE:48:50:9C | DHCP | TR069 | 0 |

两个如实展示、不装作有的地方：

- **上行时长**：标准参数是 `WANIPConnection.Uptime`，但**两台真机都返回空**，
  所以这列显示 `-`（商用 ACS 截图里那个「1天15小时…」在你这台设备上取不到）。
- **VLAN**：标准模型里没有这个概念，只有厂商私有的 `X_HW_VLAN`，探测到才显示。

### FTTR 子设备：先探测能力，没有就整块不显示

需求是「详情页加子设备区块，但先探测设备有没有这个能力，**没有的不显示**」。
所以先解决了「怎么知道设备有没有」。

#### 探测方式：枚举顶层对象（一次 GPN，不会误报错）

首次纳管时下发一次 `GetParameterNames(<根>, NextLevel=1)`，只拿顶层对象。
真机上这一次给出了 **72 个顶层对象**，一眼就能看出有没有子设备类对象。

为什么不用「逐个试候选路径」：试不存在的路径，有的设备会直接回错误（9005/9007），
而我们也不能凭报错就断定“没这个能力”。枚举顶层不会误报错，而且顺便拿到了
一份完整的**能力地图**（比如还能看到 DownloadDiagnostics / TraceRouteDiagnostics
/ X_HW_WifiCoverService 等等，以后做别的功能能直接用）。

只做一次：库里已经有对象节点（名字以 `.` 结尾）就说明探过了，不会每轮 BOOTSTRAP 都探。

#### 探到了就顺手采回来（同一个会话）

探测的 GPN 响应回来后，我们在**同一个会话**里接着入队那个子树的枚举+取值，
所以少等一轮上报周期（真机 120 秒）：

```
Inform → 空 POST → GPN(根, NextLevel=1)     ← 探测
        ↑ 响应里直接带上 GPN(X_HW_APDevice.) + GPV  ← 采集
        204（会话结束）
```

#### 两个方向都测

验收里同时盖住两边：

- **有能力的**（模拟器 `-fttr 2`）：区块出现，报「共 2 台子设备」，
  序列号/实例号都对，而且实例号是**故意做成不连续的 1 / 4**（真机是 1/2/4，
  写死连续编号的代码在真机上一定踩坑）。
- **没有能力的**（普通光猫模拟）：断言详情页 HTML 里**根本不含**「FTTR 子设备」这几个字。

真机结果：HN8145X6N（普通光猫）不显示；V271-20（FTTR 主机）显示 **3 台**
（K251e + 2 台 K251-20，全在线，含版本/信道/信号/时长）。

#### 认哪些对象

候选写到两张表里（`cwmp.fttrProbeCandidates` 与 `web.fttrCandidates`，
web 不反向依赖 cwmp 所以抄了一份，两边注释互相指向）：

| 对象 | 说明 |
| --- | --- |
| `InternetGatewayDevice.X_HW_APDevice.` | 华为 FTTR（实测就是子光猫表）|
| `Device.WiFi.DataElements.Network.Device.` | 标准 TR-181 Multi-AP（Wi-Fi Data Elements）|

二者结构不同：华为的对象直接是 `X_HW_APDevice.{i}.`，而标准那边多了
`Device.` 一级（`Network.Device.{i}.`），所以候选写成
「探测前缀 + 实例前缀」两个字段，不能当同一个。

> 顺带：每个子设备还带自己的 `WLANConfiguration.`，也就是说**每个房间那个 AP 的 WiFi
> 也能单独读改**（它跟主设备的无线配置是两套路径）。这一轮只做了展示，没做修改入口。

### ping 诊断：一个「任务保持 running 等设备回报」的流程

商用 ACS 那个「输入域名 → 诊断 → 显示发送/成功/失败与延时」的功能（见图）。
用的是**标准对象** `IPPingDiagnostics`，不需要任何厂商私有扩展。

#### 流程

```
用户点「诊断」
  → 入队 TaskDiagnostics{host, count}
  → 下次 Inform 时下发 SetParameterValues(次数, Host, DiagnosticsState=Requested)
  → 同一会话内先读一次结果（通常还是 Requested）
  → 设备跑完，**单独发一次 Inform**（事件 8 DIAGNOSTICS COMPLETE）
  → 我们借这次 Inform 再读一次 → Complete → 汇总结果、任务结束
```

关键设计：这个任务**不能在 SetParameterValues 成功后就算完成** ——
它要一直保持 `running` 等结果，可能跨越好几轮 Inform。所以：

- 设备回报不需要依赖事件 8：**只要这个设备有诊断在跑，每次 Inform 都读一次**"
  （诊断短命，多读一次代价很小；而且不是所有设备都会发事件 8）。
- 兜底：janitor 把 `running` 超过 5 分钟的诊断判为失败
  （“设备未在 5 分钟内回报诊断结果”），不让它永远挂着。
- 防重：已有诊断在**排队或进行中**时不允许再发起（不只是 running，连 pending 也要拦）。

#### 真机实测（华为 HN8145X6N）

```
16:06:52.443  诊断已下发，接着先读一次结果
16:06:52.462  诊断进行中 state=Requested          ← 同会话读回还是“进行中”
16:06:56.479  有诊断在等结果 events="8 DIAGNOSTICS COMPLETE"   ← 4 秒后设备单独回报
16:06:56.504  诊断完成
```

结果：`PING 目标 www.baidu.com：发送包 4，成功 4，失败 0；最小/平均/最大延时 = 30/31/32 ms`

两个值得记的细节：

1. **不能指望读回 `Host`**：HN8145X6N 读回来是空串（V271-20 倒是保留了最后一次的值），
   所以域名必须存在任务载荷里，否则结果里就不知道 ping 的是谁了。
2. 两台真机都有完整的 12 个标准参数，字段名与规范完全一致。

#### 踩到的坑：`DiagnosticsState=Requested` 必须放在最后

第一版把它放在了参数列表**最前面**。设备看到 Requested 就立刻开始跑 ping，
而此时 `Host` / `NumberOfRepetitions` 还没写进去 —— 实际是拿**空 Host + 默认次数**在跑，
结果就变成了「发送包 4，成功 0，失败 4」。

TR-069 虽然要求一次 SetParameterValues 里的参数原子生效，但不能指望设备真这么做。
改成「次数 → Host → **最后**才 Requested」之后立刻正常。

> 这个坑是自写模拟器先暴露出来的（它按顺序处理），真机上有些设备也会这样。

### 面板设置页：监听地址与账号密码（两个端口、可选鉴权）

需求：能单独改 **Web GUI 端口** 与 **ACS 监听端口**，面板加**账号密码保护**，并且能在设置页里改账号密码。

**两个端口**：`ACS_LISTEN` 管 CWMP，`ACS_WEB_LISTEN` 管面板。
留空（或与 ACS 相同）= 一个 listener、一套合并路由（默认，保持轻量）；
分开写就是两个 listener：CWMP 那侧只挂 `/acs`（以及根路径的 POST，真机会往根路径发），
面板那侧挂页面 / `/api` / 静态资源。验收里专门验了「两边互不串门」：
CWMP 端口拿不到面板首页，面板端口也不认 `/acs`。

**设置存哪**：`settings` 表（跟自动生成的 ConnectionRequest 密码同一个地方），
启动时 **DB 里的值优先于启动参数** —— 管理员在界面上改完重启就该按它跑。
首次启动可用 `ACS_WEB_USER/ACS_WEB_PASS` 种一次初始账号（写个 `web_seeded` 标记，只种一次），
之后一律以面板为准，免得环境变量把界面上的改动悄悄盖掉。

**生效时机**（刻意分成两种，页面上只给结果不给解释）：

| 改什么 | 何时生效 | 为什么 |
| --- | --- | --- |
| 账号 / 密码 | **立即** | 凭据改成运行中可更新的 `web.Creds`（带读写锁），中间件**每次请求**读一遍；改完马上按新的校验 |
| 监听地址 | **重启 ACS** | 换端口会把当前这条连接和在线设备的上报一起打断，没法在线安全切换 |

一开始我把凭据也做成「重启才生效」，验收立刻打脸：改了密码、旧密码还能进（因为中间件持有的是启动时那份值）。
这种「界面说改了、其实没生效」正是最该避免的，所以改成上面那样。

**ACS 端口对路径不挑（健壮性）**：运营商定制设备的 ACS URL 五花八门 —— 见过配 `/` 的、配 `/tr069` 的、
还带一串随机路径的。所以**独享端口时**（面板在别的端口）那条端口上的路由挂成兜底
（`mux.Handle("/", srv)`）：任何路径的 POST 都交给 CWMP 处理器，不因为路径不同就丢掉上报。
与之配套的两点：① 会话身份是 cookie + (IP|UA|user) 指纹，**跟路径无关**，
回给设备的会话 cookie 也是 `Path=/`，所以设备换路径也不会丢会话；② **共用端口时不能兜底** ——
否则面板页面会被 CWMP 抢走，这时只认配置的路径加根路径的 POST。
验收里两边都验了：独享端口上 POST `/`、`/tr069`、`/cwmp/ACS`、带查询串的随机路径都受理，
共用端口上陌生路径仍是 404（面板路由安全）。

**鉴权**：HTTP Basic + PBKDF2-HMAC-SHA256（Go 1.24+ 的 `crypto/pbkdf2`，不引入第三方依赖），
入库格式 `pbkdf2-sha256$迭代次数$盐$散列`，校验用常数时间比较，账号与密码都只接受配置完整的组合
（有账号没散列 = 一律拒绝）。静态资源与 `/api` 同样受保护（接口也是管理面）。
**CWMP 端点不受面板鉴权影响** —— 设备侧有自己的一套认证（`ACS_USER/ACS_PASSWORD`），两者别混。

**验收**（`verify-s1.sh` 里用 `ACS_WEB_USER/PASS` 打开鉴权、并把凭据传给 python：
只有面板请求带 Basic，CWMP 报文不带）：
- 不带凭据 / 错凭据 → 401；对了 → 200；`/api` 与静态资源同样 401；**CWMP 报文照旧 200**；
- 设置页：顶部状态表（ACS / 面板监听 + 访问控制）、四类输入框、保存后写库
  （`listen` / `web_listen` / `web_user`）、非法端口与两次密码不一致被拒；
- **页面上不放说明性文档**：只有标签、字段、按钮，加上「重启服务后生效」这类状态提示；
  为什么这么设计、忘记了密码怎么救，都写在文档里（README 与这份笔记），不占界面。
- **改密码后旧密码立刻 401、新密码 200**，且库里只存散列；
- 另起一个双端口实例（`:17561` CWMP + `:17562` 面板），验「两边互不串门」。

### 历史保留上限：任务历史与上报记录各留最近 500 条（控制库大小）

`tasks` 表原来只增不减：跑久了库会越来越大，而任务历史只有最近的才有用。
现在每台设备**最多保留 500 条**（`-task-history-limit` / `ACS_TASK_HISTORY_LIMIT`，`0` = 不限）。

实现上有两个刻意的选择：

1. **只裁已结束的任务**（`pending` / `running` 一条都不删）。排队的和正在执行的是「还没做的事」，
   丢了就是真丢事。用窗口函数按设备编号排序：
   `ROW_NUMBER() OVER (PARTITION BY device_id ORDER BY id DESC) > 上限` 且状态是 done/failed 才删。
   副作用是对的：某台设备排了 600 个任务时总数会超过 500，这是应该的。
2. **每次入队顺手裁一次**，启动时再裁一次（下次把上限调小，重启就生效）。
   裁剪失败不影响这次入队（任务已经写进去了，清理只是管家活），启动那次会记日志。
   上限走 store 字段（`SetTaskHistoryLimit`），所以测试里能调到很小来验证边界。

界面上「任务历史」标题写着「最近 N 条」，让人知道为什么老记录会消失（截图里出现过这个疑问）。

顺带修了一处**被裁剪暴露出来的脆弱验收**：原来「凭据 provisioning」那条是去**任务历史里找**
那条 `SetParameterValues` 任务 —— 任务一被裁就找不到了。现在改成看**设备侧的实际结果**
（新设备上来时模拟器默认给 `cpe-cr`，ACS 会把它改写成配置的 `acs`，读回来能对上），
再加上日志里的下发动作，跟任务历史有没有被裁无关。

**上报记录同样留 500 条**（`-inform-history-limit` / `ACS_INFORM_HISTORY_LIMIT`）。
它是增长最快的表：设备每 120 秒一条 Inform，**一台设备一天 720 条**，两年就是 50 万条。
跟任务历史不同，上报记录都是流水日志、没有“还没做完”的状态，所以直接按设备保留最近 N 条
（不需要护着 pending/running）。界面上的「上报记录」标题同样写着「最近 N 条」。

### 界面文案：给使用者看的，不是给开发者看的（2026-09-28 整理）

用户提了一句话：**「所有内部说明性质的文案去掉，像正式产品那样」** —— 起因是 WAN 区块底下那句
「数据来自标准的 WANIPConnection / WANPPPConnection 对象；业务模式与 VLAN 在真机上是厂商私有参数
（X_HW_*），探测到才显示」。这类话是我们自己关心的实现细节，使用者不需要，也不该出现在产品界面上。

这一轮清掉/改写的东西（原则：**实现理由写进文档与提交信息，界面上只留使用者要的信息**）：

| 原来 | 现在 |
| --- | --- |
| WAN 区块那段「数据来自标准对象…厂商私有参数…」 | 只留「共 N 条连接，已连接 M 条」 |
| FTTR 区块「实例号由枚举得到…每个子设备还带自己的无线配置…」 | 只留「共 N 台子设备，在线 M 台」 |
| 「组网」列整段口径说明（WorkingMode / SignalIntensity / 光功率怎么判） | 删掉；悬停里改成「设备上报：repeater · 上行 DHCP」 |
| Ping 区「让设备自己发 ICMP（标准的 IPPingDiagnostics）…」+ 承载接口四行说明 | 删掉（输入框 placeholder 已说明「留空＝设备自选」） |
| 无线区「只采集…摘要字段，负担很小」「终端数是主机 + 子光猫的合计…」 | 只留一句「设备一般不返回明文无线密码，该字段为空属正常」 |
| 「读取参数子树」底下的「做法：先 GetParameterNames 再 …」 | 删掉；区块改名「高级：读取参数子树」 |
| WiFi 编辑表单每个字段底下那行参数全路径 | 挪到输入框的 title（悬停可见），表单本身清爽 |
| 顶栏「CWMP 端点 /acs」（只有路径、没有主机，对使用者没用） | 删掉 |
| 「Inform 记录」这个小节名 | 改成「上报记录」 |

后端串到界面上的字串也一并改了（它们出现在「任务历史」的结果列）：
「设置成功（Status=0）」→「设置成功」、「收到 6 个参数」→「已采集 6 个参数」、
「CPE 报告设置失败，Status=x」→「设备拒绝写入（错误码 x）」、「诊断进行中（设备状态 Requested）…」→「诊断进行中…」。
状态码仍在日志里记着，排障不受影响。

**保持不变**：数据类的悬停提示（终端信号 `RSSI -58 dBm / 设备自报质量 51`、组网悬停里的设备自报值）、
采集时间、操作确认框里的后果说明（「重启期间无法上网」「只删本地记录 / 下次上报会重新纳管」）——
这些是使用者真的要读的信息。顺带把「数据模型根」显示成 **TR-098 / TR-181**（认不出来才原样显示）。

### 删除设备：说清「删的是什么」

详情页「操作」里加了红色「删除设备」（跟「重启设备」同级，带二次确认）。

语义必须先想清楚，否则这个按钮就是个坑：

- **只删本地记录**：设备行 + 它的参数、任务、上报历史（外键 `ON DELETE CASCADE`，一条 SQL 删干净，
  验收里直接查库文件确认三张表都没残留）。
- **不会动设备本身**，也**不阻断再次纳管**：设备那边还配着我们的 ACS 地址，下次上报就回来了
  （身份键是 OUI / ProductClass / SerialNumber）。所以确认框和提示里都写明这一点 ——
  不然用户会以为“删了就再也不来了”，或者反过来担心“把光猫删坏了”。
- 真要做「不许再来」需要另加黑名单（按 OUI+SN 拒收 Inform），本期不做。
- 删完回**首页**而不是留在详情页（那页已经 404 了），首页会显示一行成功提示
  （首页原来没有提示条，顺手加上了）。

顺带把本机开发库里的**模拟设备**清掉了（用这个新接口删的，顺便当了一次真实验收）：
设备 3/4（`CLI-DEMO`、`CLI-DEMO2`）删除后，库里只剩两台真机（HN8145X6N / V271-20），
参数 3347 / 4522 条未受影响，三张关联表都没有孤儿行。

### 关联终端：谁连主机、谁连子机（弹窗）

需求来自用户的截图（商用 ACS 上点「终端」弹出一个列表：MAC + IP 地址），
并且明确要求：**无线概况里的终端数要加上子设备上的终端**，
还要能看出**哪些设备连主机、哪些连哪台从机**。

真机现实（华为 V271-20 FTTR 主机 + 3 台子光猫）：主机自己的 WLAN 上有 5 台终端，
三台子光猫上另有 3 / 4 / 0 台 —— **只看主机就是 5，实际全网 11 台**。

数据来源（都是标准对象，不猜）：

| 谁 | 路径 |
| --- | --- |
| 主机 | `<root>LANDevice.1.WLANConfiguration.{i}.AssociatedDevice.{k}.*` |
| 子机 | `<root>X_HW_APDevice.{inst}.WLANConfiguration.{j}.AssociatedDevice.{k}.*` |
| 子机（TR-181 Multi-AP） | `<root>WiFi.DataElements.Network.Device.{inst}.…` |

几个必须记住的点：

1. **条目数以 `AssociatedDeviceNumberOfEntries` 为准**。真机上这张表是快照式的，
   会残留上一次读的空行（实测：`NumberOfEntries=1` 却有 2 行，第 2 行 MAC/IP/RSSI 全空）。
   界面必须按条目数截断，否则会多出「幽灵终端」（模拟器里专门留了一条**带 MAC 的残留行**来守这条）。
2. **字段名各家不同**：子机的表用标准名（`RSSI` / `SNR` / `RxRate` / `TxRate` / `FrequencyWidth` / `Uptime`），
   主机的表用厂商私有名（`X_HW_RSSI` / `X_HW_SNR` / `X_HW_RxRate` / `X_HW_TxRate` / `X_HW_FrequencyWidth` / `X_HW_Uptime`），
   TR-181 又是 `MACAddress` / `IPAddress`。按后缀别名认全。
3. **主机的终端不回 IP**：真机上主机的 `AssociatedDeviceIPAddress` 是空串（子机那边倒是有，
   例如 192.168.10.121）。界面就显示 `-` —— 不编。主机的 `LANDevice.1.Hosts.` 表里也没有这些 WiFi 终端
   （只有三台子光猫），所以补不出来。
4. **终端数 = 主机 + 子机**（用户要求）。同一频段有多行时（真机 5G 有一行是没 SSID 的空实例），
   子机数量只算在**有 SSID 的那一行**，否则同一台子设备会被重复计入。
5. **WiFi 摘要采集要带上终端字段**，否则主机自己的终端根本不在库里（弹窗里只剩子机的）。
   白名单里加了 `AssociatedDeviceMACAddress` / `…IPAddress` / `RSSI` / `SNR` / `RxRate` / `TxRate` /
   `FrequencyWidth` / `LastDataTransmitRate` / `Uptime`（TR-181 的 `MACAddress` / `IPAddress` 也认）。
   已纳管的设备点一下「重新采集无线概况」就能补上。

**顺手修掉一个会误导人的真 bug**：子设备也带 `WLANConfiguration`（实例号同样从 1 开始），
而 `store.WifiParams()` 一条 `LIKE '%WLANConfiguration.%'` 会把它们全捞进来，
于是**主机的「2.4G」那一行显示成了子光猫的 SSID / 信道**（真机上真的会这样：
主机 2.4G 那行显示的是 LabWifi，其实是子机的值）。
现在 `WifiOverview` / `wifiInstanceParams` / `WifiInstances` 都会跳过子设备自己的 WLAN
（`isSubDeviceWifi`），子设备的终端只在「FTTR 子设备」区块里看。

**主机名**：终端条目里加了一行「主机名：」，拿不到就显示 **N/A**（不编、也不留空）。

来源有两个，按顺序取：

1. 终端行自己带的名字字段（华为是 `X_HW_AssociatedDevicedescriptions`，也认 `HostName` /
   `AssociatedDeviceHostName` / `X_HW_DeviceName`）；
2. 设备的**主机列表**（TR-098 `LANDevice.1.Hosts.Host.{i}.HostName`，TR-181 `Hosts.Host.{i}.HostName`），
   按 MAC 对 —— 大小写不一致也能对上；各 AP 的终端行里带的名字也会并进这张索引，
   所以**子设备的终端也能借到主机列表里的名字**。

主机列表原来不在任何默认采集里（无线摘要只枚举 WLAN 子树），所以加了一条
`EnqueueFetchHosts`（枚举 `LANDevice.1.Hosts.` 后按后缀只取 MAC / IP / HostName / Active /
InterfaceType / AddressSource / VendorClassID / 条目数），挂在「采集无线概况」一起下发
（点界面上的「重新采集无线概况」也会刷新它）。终端名同理，也加进了无线摘要白名单。

**两台真机上的实测差异（很说明问题）**：

- 设备 1（HN8145X6N 光猫）：主机列表里有那台终端 —— 界面直接显示 **Android-Phone**；
- 设备 2（V271-20 FTTR 主机）：主机列表里只有 3 台子光猫（`InterfaceType=PON`），
  WiFi 终端一台都没有 → 那一列全是 **N/A**。

这和之前「主机终端没有 IP」是同一个根因：这台 FTTR 主机的 INTERNET 业务是桥接的，
主机的三层视角里基本没有这些终端（它既拿不到 IP，也拿不到名字），
而子光猫自己有 ARP/转发视角，所以子机那边的 IP 是齐的。**设备不知道的就显示 N/A，不猜。**

**信号图标（四格小柱 + 百分比）**，口径定成两条，避免“同一个信号两种数字”：

1. **优先用设备自报的质量**（华为 `X_HW_SingalQuality`，0..100；也认 `SignalQuality`）——
   各家对「几格」的换算不一样，设备自己算的更可信。
2. 没有质量值才用 RSSI 换算：**-50 dBm 及以上算满格、-100 dBm 及以下算 0**（中间线性）。
   格数按 `round(百分比/25)`，有一点信号至少给一格（不然看着像“没连上”）。
3. 两者都拿不到 → **不渲染这个图标**（不编）。悬停提示里把原始值都列出来
   （设备自报质量 / RSSI / SNR），免得百分比被当成设备事实。

**踩了一个坑（值得记）**：模板里调用方法时**只能有一个返回值**（或 `(值, error)`）。
我一开始写了 `SignalPct() (int, bool)` 给模板用，`html/template` 直接报
`invalid function signature: second return value should be error; is bool` ——
更糟的是这个错误发生在**渲染中途**：页面被截断，页尾还混进一行 template 错误文本，
因为原来是把模板直接写到 `ResponseWriter` 的。
→ 两处都改了：① 给模板的方法都改成单返回值（内部另有一个 `signalPct() (int, bool)`）；
② 新增 `Server.render()`，**先渲染到内存、成功后再写出**，出错就干干净净回 500。
另外补了个回归用例 `TestDevicePageRendersCompletely`（断言页尾 `</html>` 在、
且信号图标真的渲染出来），这类“渲染到一半出错”的问题以后会被直接抓出来。

界面做法：弹窗内容**由服务端一起渲染在页面里**（`hidden`），`app.js` 只负责开/关
（`data-modal` 打开，点遮罩 / ✕ / Esc 关闭）—— 不额外发请求，也不依赖前端拼内容。
频段那一行点开的是**该频段**的完整列表（主机 + 各子机分组，分组头写明「主机 · SSID」/「子机 1（K251e）· SSID」），
子设备那一行点开的是**这台子机**的列表。

### FTTR 子设备的「组网模式」与「光功率」：先探测，别硬做

需求：子光猫那里想看**组网模式**和**光功率**（无线 / 有线组网就不显示光功率）。

**先说结论（真机探测，2026-09-28）**：

- **组网模式能看**（间接的）：子设备表里跟组网有关的字段就这几个 ——
  `WorkingMode` / `SupportedWorkingMode` / `InternetAccessMode` / `SignalIntensity` / `SyncStatus`。
  真机三台子光猫都是 `WorkingMode=repeater`、`SignalIntensity=0`。
- **光功率拿不到**：把两台真机的**全树参数名**拉下来扫了一遍（根级 GetParameterNames：
  V271-20 **4484** 个名字、HN8145X6N **3315** 个），搜 `optic|rxpower|txpower|pon|olt|wavelength`
  全部落空；主网关上 `X_HW_PonQualityMonitor.` 只有 `BERThreshold/Enable/MonitorInterval`
  三个**配置项**，没有测量值。子设备 `X_HW_APDevice.{i}.` 的直属字段一共 20 个，也没有光功率。

怎么探测的（可复用）：把 `ACS_LOG_SOAP=1` 打开（debug 级别会把收到的报文全打日志），
发一条 `POST /api/devices/{id}/names {"path":"InternetGatewayDevice."}`（只枚举名字不取值），
然后在日志里正则抠 `<Name>`。比盲猜路径靠谱得多。

**界面上的做法**（按上面的现实来设计）：

- 新增「组网」列。判定顺序：`SignalIntensity` 非 0 → 无线（带信号值）；
  再看 `WorkingMode` 里明确的写法（`wifi/wireless` → 无线，`eth/wire/bridge/lan` → 有线，
  `fttr/fiber/optical/pon` → 光纤）。**归不了一类就原样显示设备自报的值**（真机就是这样，显示 `repeater`），
  并把 `WorkingMode / SupportedWorkingMode / InternetAccessMode / SignalIntensity / SyncStatus`
  放在悬停提示里 —— 我们归一化后的字样不能被当成设备事实。
- 新增「光功率」列，两个条件同时满足才显示值：① 这台子设备确实上报了光功率；
  ② 组网不是无线 / 有线（没光口，有些固件反而会回 0 或无效值）。
- 整列是否渲染看「有没有任何一台子设备真的读到光功率」—— 没读到就整列不出现（跟 WAN / FTTR 一个规矩）。
  所以**当前两台真机都不会看到这一列**，哪天固件/型号给出了就自动出现。
- 参数名按**叶子名后缀**认，容忍各家的不同写法：`RxPower / RxPowerDbm / OpticalRxPower /
  X_HW_RxPower`…，并且允许中间多一层（`…{i}.Optical.RxPower`）。
  注意 `TransmitPower` 是**无线**发射功率，不能当成光发射功率（真机上子设备就有 `TransmitPower=100,100`）。
- 顺带修了个静默串位：`SupportedWorkingMode` 以前也会被当成 `WorkingMode`（后缀匹配的坑）。

**踩了一个大坑（已修，记在这里）**：搞探测时用根级 `GetParameterNames` 浏览参数树，
结果**把设备已有的参数值全刷成了空串** —— 详情页瞬间“没数据了”（设备那边好好的）。
原因是 `UpsertParams` 一律 `value = excluded.value`，而名字枚举根本不带值。
现在改了：`source='getnames'` 的行**不改动 value / value_type，也不刷采集时间**，
只用来记录“这个参数存在、可不可写”；而真正取到的空值（设备就是这么回的）仍然如实落库 ——
这两种情况必须分得开（前者是“没问”，后者是“问了是空”）。补了单测
`TestParamsUpsertNamesDoesNotWipeValue`。本机真机库用**只读全树重采**恢复，
顺带把两台设备的参数采全了（V271-20 4522 个、HN8145X6N 3347 个）。

验证靠模拟器新增的三个开关（真机没有这些参数，不这么做就没法验收）：
`-fttr-optical`（子设备带光功率）、`-fttr-wifi 1,3` / `-fttr-eth 1,3`（做成无线 / 有线组网）。
注意模拟器给**所有**子设备都加了光功率参数 —— 这样才能验证「无线组网即使有参数也不显示光功率」。

### 重启设备（Reboot）：把护栏写在后端，而不是只靠前端的确认框

`Reboot` 很早就在 `buildTaskBody` 里构造好了报文，但一直没接界面 —— 因为它和别的任务不同，
**它把设备搞断线**：正在跑的业务会中断，光纤/宽带用户直接感受到「网断了」。所以接它的方式得讲究一点。

**护栏分两层**：

1. 前端：按钮是**红的**（`button.danger`，用主题里的 `--bad`），提交前 `window.confirm` 二次确认，
   确认文案里写明设备名与后果（「会立刻断开重启，重启期间无法上网」）。
   确认文字写在 form 的 `data-confirm` 属性上，由 `app.js` 统一接管 —— **不把设备名拼进 JS 里**：
   属性由模板做转义，设备名里带引号也搞不坏脚本。
2. 后端：**不依赖前端的确认框**。`EnqueueReboot` 先查 `FindOpenTask(deviceID, TaskReboot)`，
   同一台设备上有重启在排队/进行中就直接拒绝（任务 #N 报给用户）；
   设备不存在也不建任务（不然会堆出孤儿任务）。验收里专门测了这两条。

**流程上的一个细节**：和删诊断一样，点完顺手发一次 Connection Request —— 否则这条指令
最多要等 120 秒的周期上报才发得下去；唤醒之后几秒内设备就回连接走了。

**完成时机**：设备回 `RebootResponse` 就算「已接受」，任务标 `done`，结果文案写
「设备已接受重启指令，正在重启…」。之后它自己断线、重启、重连（带 `1 BOOT`），
`LastBootAt` 会刷新 —— 但不能把「回执」当成「真的重启完了」，真正确认重启完要看重连。
（本项目的模拟器只回回执、不真的断线，所以验收只测到回执这一步。）

### ping 诊断的承载接口（Interface）：为什么真机上是「成功 0、0ms」秒失败

**现象**：设备 2（华为 V271-20，FTTR 主机）上跑 ping，不管目标是域名还是公网 IP，
结果一律是「发送包 4，成功 0，失败 4；最小/平均/最大延时 = 0/0/0 ms」，
而且**秒回**（`Timeout=10000`，真发包超时不可能 4 秒就出结果）。
同一套代码在设备 1（HN8145X6N）上 4/4 成功、30ms。

**排查（全程只读，一次写入都没加）**：

1. 把 `IPPingDiagnostics.` 整对象重读一遍 → `Host` 保留着 `baidu.com`
   （正是我们下发的那次），`DiagnosticsState=Complete`，`NumberOfRepetitions=4`。
   **说明 SetParameterValues 是生效的，任务带对了目标**，不是入队/编码的问题。
2. 换成纯 IP `119.29.29.29` 也一样全失败 → 与 DNS 无关。
3. 读设备自报的路由表 `Layer3Forwarding.X_HW_CurrentForwarding`：
   只有 `192.168.3.0/24 dev br0` 和 `192.168.20.0/22 dev wan2` 两条直连路由，
   **没有默认路由**（`X_HW_AutoDefaultGatewayEnable=0`）。
4. 它的 WAN：`1_INTERNET_B_VID_` 是 `IP_Bridged`（`ExternalIPAddress` 空、
   `LastConnectionError=ERROR_USER_DISCONNECT`），唯一有 IP 的
   `2_TR069_R_VID_`（192.168.10.23）`X_HW_SERVICELIST` 只有 `TR069`。

结论：**这台设备的 ICMP 根本没有可用的出口**（互联网那侧是桥接的，主机自己不持有上网 IP）。
包一进协议栈就是 no route，立刻计一次失败 —— 这就是「0 成功 + 延时全 0 + 秒回」的来源。

**教训（比这条 bug 本身重要）**：

- 「0 成功 + 延时全 0 + 秒回」= **没出去**，而不是「出去被丢了」。看**延时**就能分开这两种情况。
- 别把设备侧的现实当成 ACS 的 bug，也别反过来：先用「另一台设备同代码能不能通」把
  变量锁在设备/网络上，再往深了查。
- 模块化写法（同样还有路由表、WAN 列表）能自证：`Host` 读回值 = 我们下发的值，
  一句话就排除了整条入队/编码链路。

**改动：诊断支持可选承载接口**（对应标准的 `IPPingDiagnostics.Interface`）

- 任务载荷加 `interface` 字段（可空）；报文里排在 `Host` **之前**，
  `DiagnosticsState=Requested` 仍然最后（同一个道理：出口得先就位）。
- 留空 = 设备自选出口（保持原来的行为，报文里连这个元素都不出现）。
- 值会直接进 SOAP 报文，所以限字符集（字母/数字/`.`/`-`/`_`，不许空段），
  但**不限定必须是哪一类对象** —— 标准举的例子是 WAN 连接路径，厂商也接受别的写法。
- 界面：诊断表单加了「承载接口（留空＝设备自选）」输入框 + `datalist` 备选；
  备选就是这台设备**已经采集到的 WAN 连接**（含 TR069 那条管理连接 —— 用它去 Ping
  管理网/内网正是这个选项的用途）。手填也行，不强限。
- 结果里留痕：诊断结论会带上「承载接口 xxx」，全失败且延时为 0 时补一句
  「设备可能根本没有可用的出网路径，可试试指定承载接口」；
  最近一次诊断的承载接口也在结果框上回显 —— 免得以后「一会儿通一会儿不通」没法复盘。
- 模拟器新增 `-ping-need-iface <子串>`：模拟「必须指定承载接口才出得去」的设备
  （不指定就全失败、延时全 0），验收里就是这么对比的（不带接口 0/2，带接口 2/2 通）。
- 顺带修了模拟器的一个不自洽：一个包都没通时延时也报 0（原来会报一个假的 10–29ms）。

### 日间模式

整体换成 CSS 变量方案：规则里不再写死颜色，只换 `:root[data-theme="light"]` 那一组变量。

- 主题在页面 `<head>` 的内联脚本里**先于样式定好**，避免刷新时先闪一下暗色
- 没选过时跟随系统的 `prefers-color-scheme`；点过之后选择记在 `localStorage`
- 切换按钮的文案由 JS 写成「切换到日间 / 夜间」，不会让人猜哪个是当前值

#### 顺手抓到一个真 bug：两个页面根本没引入 app.js

做主题按钮时发现首页那个按钮的文案没被 JS 改（还是初始的“主题”）。
一查：`index.html` 与 `wifi_edit.html` **从来没有引入 `/static/app.js`**，
只有详情页引了。也就是说首页的**搜索防抖、表格分页**一直在静默失效 ——
搜索按钮还能用（它就是普通表单提交），所以之前一直没被发现。

已补上，并加了一条验收：三个页面都必须包含 `static/app.js`；
另外在 headless 浏览器里断言**主题按钮被 JS 初始化过** ——
这比“引了没引”强，它证明脚本真的执行了。

### 折叠与分页（界面细节）

设备一多、参数一多，详情页不折叠就没人看了：一台设备现在能到 **923 个参数**、
任务历史几十条、上报记录天天在涨（每 120 秒一条，一天 720 条）。所以：

- 参数 / 任务历史 / 上报记录三个区块用原生 `<details>/<summary>`，**默认折叠**；
- 四个表（含概览页设备列表）默认 **20 条/页**，可切 20 / 50 / 100 / 全部。

两个实现上的取舍：

1. **分页放在浏览器端**。数据本来就全部渲染在 DOM 里了（参数 923 行也就 200KB 左右），
   在浏览器里分页不用往返服务端，而且能和「参数名过滤」直接配合 ——
   过滤出子集后重新分页，而不是两套逻辑各干各的。过滤时会重置回第 1 页
   （过滤完还停在第 47 页会很困惑）。
2. **行数不到一页就不显示分页条**。只有两三台设备时“共 2 条 · 第 1/1 页”纯属噪音，
   所以直接不渲染。

为了服务端多取一些给前端分页，详情页任务/上报记录的上限从 30/20 提到 100。

### 「能改不能读」的参数：核对要分三种结果

真机上试改 5G 密码时发现的一个坑。往 `WLANConfiguration.5.PreSharedKey.1.KeyPassphrase`
写密码，设备回 `Status=0`（说接受了），但**读回永远是空串** —— 它跟 `KeyPassphrase` 一样
属于「能改不能读」（厂商故意不回明文 PSK，因为 TR-069 常在未鉴权的 LAN 口上开着）。

上一版的核对只看「期望值 vs 读回值」，于是会把一次**成功的**改密码报成「未生效」。

→ 核对时多比一个东西：**写入前的值**。三种结果分开处理：

| 写入前 | 读回 | 结论 |
| --- | --- | --- |
| 非空 | 变成期望值 | ✅ 核对通过 |
| 非空 | 没变 | ❌ 未生效（真没生效）|
| **空** | **还是空** | ⚠️ **无法核对**（设备不回读该参数），不判失败 |

第三种情况的措辞很重要：任务状态仍算成功，但结果里写明
`设置成功（Status=0）；1 个参数设备不回读、无法核对：<参数名>` ——
既不骗人说“成功了”，也不冤枉设备。

实现上就是在写入任务的载荷里多存一份 `prev`（写入前从本地库拿的值），
核对时一起带过去。

模拟器也加了 `-write-only <子串>` 开关模拟这个行为，验收里有一条完整的
「提交密码 → 设备接受但不回读 → 不该被判失败」流程。

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
| `go test ./...` | 全部通过（`internal/cwmp` 41 个用例、`internal/store` 12 个用例、`internal/web` 33 个用例）|
| `scripts/verify-s1.sh` | **通过 293 / 失败 0** |
| `scripts/verify-interop.sh` | 通过（GenieACS 官方模拟器可完整纳管） |
| 真机（华为 HN8145X6N + V271-20） | **两台不同型号均自动纳管成功**；实测过 SSID 改名、开 5GHz 射频、写密码（后者发现参数选错）|
| 界面渲染 | 用 headless Chrome 截图确认（列表页 + 详情页） |

> 以上都是**本次实例的实测值，不是项目常量**。
