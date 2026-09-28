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

## 验收记录（本次实测）

| 项目 | 结果 |
| --- | --- |
| `go test ./...` | 全部通过（`internal/cwmp` 18 个用例、`internal/store` 6 个用例） |
| `scripts/verify-s1.sh` | **通过 42 / 失败 0** |
| `scripts/verify-interop.sh` | 通过（GenieACS 官方模拟器可完整纳管） |
| 界面渲染 | 用 headless Chrome 截图确认（列表页 + 详情页） |

> 以上都是**本次实例的实测值，不是项目常量**。
