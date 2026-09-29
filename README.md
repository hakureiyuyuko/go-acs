# 轻量 TR-069 ACS

**简体中文** | [English](README-en.md)

单二进制、零外部中间件的 TR-069/CWMP ACS（Go + SQLite），用来管一批光猫 / FTTR 主机：
**纳管 → 看信息 → 改配置 → 诊断 → 子设备与终端管理**。

![CI](https://github.com/hakureiyuyuko/go-acs/actions/workflows/ci.yml/badge.svg)
![Go](https://img.shields.io/badge/Go-1.27-00ADD8)
![License](https://img.shields.io/badge/license-AGPL--3.0-blue)

## 特点

- **单文件部署**：一个静态二进制 + 一个 SQLite 文件，没有 Redis / MySQL / 消息队列
- **只用标准协议**：Inform、GetParameterValues / Names、SetParameterValues、GetRPCMethods、Reboot、
  IPPingDiagnostics、Connection Request（含 HTTP Digest）；不依赖任何厂商私有服务
- **先探测再显示**：能力探测决定界面出现哪些区块（WAN、FTTR 子设备…），探测不到就整块不显示，不摆空壳
- **如实呈现设备行为**：能改不能读的参数、异步生效的无线参数、设备没上报的字段（显示 `N/A`），不编数字
- **面板中英双语**：右上角一键切换（`?lang=` + cookie + 浏览器语言协商），漏翻有测试卡着
- **离线设备不乱给操作**：详情页里需要设备配合的按钮（重新获取、唤醒、重启、诊断）在离线时变灰，
  删除设备这类本地操作照旧可用
- **在线状态按设备自己的周期判**：超过`上报周期×2`没上报，先主动发 Connection Request 探三次，
  探不通才判离线（设备断电时 TR-069 不会通知 ACS，只能这样确认）
- **可运维**：任务队列持久化、一键唤醒设备、重启 / 删除设备、面板独立登录页（会话 cookie、可退出）、
  任务与上报记录的保留上限

## 截图

> 数据来自仓库自带的 CPE 模拟器，不含任何真实设备信息。

概览：设备列表（在线状态、序列号、软件版本、数据模型、最后上报时间、已采集参数条数、无线终端数，
可按在线 / 离线筛选，状态分「在线 / 探测中 / 离线」三态，右上角可开 5 秒自动刷新）+ 无线概况

![概览](docs/images/overview.png)

基本信息、WAN 连接、操作（唤醒 / 重启 / 删除）、备注；右上角同样有 5 秒自动刷新开关（与列表页共享）

![设备详情](docs/images/device.png)

FTTR 子设备与网络诊断：子设备的型号、组网模式、光功率与各自带了多少终端；诊断由设备自己发 ICMP

![FTTR 与诊断](docs/images/fttr.png)

终端列表：按「主机 / 子机」分组，每台终端带信号强度、主机名与 IP（设备没上报的字段写 `N/A`）

![终端列表](docs/images/clients.png)

设置：ACS 与面板各自的监听地址、面板登录账号密码（端口改动重启生效，账号密码立即生效）

![设置](docs/images/settings.png)

## 安装（Release 包）

从 [Releases](https://github.com/hakureiyuyuko/go-acs/releases) 下对应架构的包，解压后一条命令装成 systemd 服务：

```bash
VERSION=1.2.2                                   # 换成你下载的那个版本
tar xzf acs-$VERSION-linux-amd64.tar.gz
cd acs-$VERSION-linux-amd64
sudo ./install.sh                                                # 默认 CWMP 与面板都走 :7547
sudo ./install.sh --web-listen :8080 --web-user admin --web-pass '改成你自己的'
```

装完会打印面板地址与要填进光猫的 ACS URL（`http://<本机IP>:7547/acs`）。后续：

```bash
sudo ./update.sh         # 升级：校验 SHA256 → 停服务 → 备份 → 换新 → 健康检查，起不来自动回滚
sudo ./uninstall.sh      # 卸载：默认保留数据库与配置，--purge 连数据一起删
```

默认落点：主程序 `/usr/local/bin/acs`、数据 `/var/lib/acs`、配置 `/etc/default/acs`、
服务 `acs`。包内 `README.md` 有完整的选项与目录说明。

## 快速开始（源码）

```bash
go build -o acs ./cmd/acs        # Go 1.27+；CGO_ENABLED=0 可得到静态二进制
./acs -listen :9090 -db acs.db   # 面板 http://<IP>:9090/ ，CWMP http://<IP>:9090/acs
```

打发布包：

```bash
scripts/build-release.sh v1.2.2   # 产物在 dist/：amd64 + arm64 的 tar.gz 与 SHA256SUMS
```

设备侧的 ACS URL 填 `http://<IP>:9090/acs`；真机里也见过配成根路径 `/` 的，所以两者都收。
面板挪到独立端口（`-web-listen :8080`）时，CWMP 那侧**任何路径都受理**，不用担心运营商定制设备的路径写法。

没有设备也能玩：仓库自带模拟器（含 FTTR 子设备、能改不能读、异步诊断等开关）。

```bash
go build -o cpesim ./test/cpesim
./cpesim -acs http://127.0.0.1:9090/acs -serial DEMO0123 -fttr 3 -fttr-optical
```

## 配置

命令行参数与环境变量一一对应（环境变量名 = `ACS_` + 参数名大写、连字符换下划线）。

| 参数 | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `-listen` | `ACS_LISTEN` | `:7547` | CWMP 监听地址 |
| `-web-listen` | `ACS_WEB_LISTEN` | 空 | 面板监听地址；留空 = 与 CWMP 同端口 |
| `-path` | `ACS_PATH` | `/acs` | CWMP 端点路径（同端口时生效） |
| `-db` | `ACS_DB` | `acs.db` | SQLite 文件路径 |
| `-user` / `-password` | `ACS_USER` / `ACS_PASSWORD` | 空 | 设备侧 HTTP 认证（CPE 基本认证）|
| `-web-user` / `-web-pass` | `ACS_WEB_USER` / `ACS_WEB_PASS` | 空 | 面板登录账号密码（首次启动种入，之后以设置页为准）|
| `-max-params-per-request` | `ACS_MAX_PARAMS_PER_REQUEST` | `200` | 单次 GetParameterValues 带多少个参数名 |
| `-task-history-limit` | `ACS_TASK_HISTORY_LIMIT` | `500` | 每台设备保留多少条任务记录（`0` = 不限）|
| `-inform-history-limit` | `ACS_INFORM_HISTORY_LIMIT` | `500` | 每台设备保留多少条上报记录（`0` = 不限）|
| `-offline-after` | `ACS_OFFLINE_AFTER` | `10m` | 多久没上报算离线（设备**没**上报周期信息时的兜底）|
| `-offline-probe` | `ACS_OFFLINE_PROBE` | `true` | 判离线前先主动探测；`false` = 退回纯超时 |
| `-offline-probe-factor` | `ACS_OFFLINE_PROBE_FACTOR` | `2` | 超过 `上报周期 × 这个倍数` 没上报就开始探测 |
| `-offline-probe-attempts` | `ACS_OFFLINE_PROBE_ATTEMPTS` | `3` | 最多探测几次，都没回音才判离线 |
| `-offline-probe-interval` | `ACS_OFFLINE_PROBE_INTERVAL` | `15s` | 两次探测之间的间隔 |
| `-offline-probe-grace` | `ACS_OFFLINE_PROBE_GRACE` | `30s` | 最后一次探测后再等多久才判离线 |
| `-offline-probe-max` | `ACS_OFFLINE_PROBE_MAX` | `0` | `周期×倍数` 的上限（`0` = 不设；设备上报的周期很大时用得上）|
| `-offline-check-interval` | `ACS_OFFLINE_CHECK_INTERVAL` | `30s` | 后台多久巡检一次在线状态 |
| `-auto-fetch-wifi` | `ACS_AUTO_FETCH_WIFI` | `true` | 纳管时自动采集无线概况与主机列表 |
| `-auto-refresh-wifi` | `ACS_AUTO_REFRESH_WIFI` | `10m` | 无线 / 终端概况的自动刷新间隔（面板上的「采集」时间跟着动；`0` = 只在首次纳管时采一次）|
| `-probe-capabilities` | `ACS_PROBE_CAPABILITIES` | `true` | 纳管时做一次能力探测（决定界面区块）|
| `-log-level` | `ACS_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `-log-soap` | `ACS_LOG_SOAP` | `false` | 打印原始 SOAP 报文（排障用）|

### 面板登录

面板是**独立登录页 + 会话 cookie**（不是 HTTP Basic）：未登录访问任何页面都会跳到 `/login`，
带登录态后 7 天内有效、用着会自动续期，顶栏有「退出」按钮。登录态是 HMAC 签名的 cookie
（HttpOnly + SameSite=Lax，密钥存库、重启不掉线）；**改账号密码会把所有旧登录态立刻作废**。
接口（`/api/*`）未登录时回 401 JSON，静态资源不需要登录。

忘了密码：`ACS_WEB_AUTH=off` 起一次即可进去改，或清掉库里 `web_user`/`web_pass`。

### 在线状态怎么判

设备断电时 TR-069 **不会**给 ACS 发任何通知（只有设备主动连上来时 ACS 才知道它活着），
所以在线状态是按「多久没动静」推出来的，且跟设备自己上报的周期挂钩：

```
最后一次上报 + 上报周期×2            → 开始主动探测：发 Connection Request
  探测最多 3 次（间隔 15s）          → 有一次连上：判定设备还活着，保持在线
  3 次都连不上 + 再等 30s            → 标记离线
设备没上报周期信息                   → 退回 -offline-after（默认 10 分钟）纯超时
```

界面上有三种状态：**在线** / **探测中**（超期了、正在探）/ **离线**。
设备再次上报（含被唤醒后回连）就自动恢复在线并清掉探测进度。

## 验收

```bash
go test ./...                   # 单元测试：协议解析 / 存储 / Web
bash scripts/verify-s1.sh       # 端到端 353 项：模拟器打真实 HTTP + SOAP，逐条断言
bash scripts/verify-interop.sh  # 与 GenieACS 官方 JS 模拟器互通 8 项
```

## 压测

```bash
scripts/loadtest.sh                     # 阶梯 10/50/100/200/400，每个档位都用全新的库
scripts/loadtest.sh -n 200 -e 4484      # 只跑一档（-e = 每台 X_HW_APDevice 子树参数个数）
```

设备模板默认照真机华为 V271-20（TR-098、3 台 FTTR 子设备、`X_HW_APDevice` 子树 4484 个参数，
每台约 4700 个参数），全部设备同时发 `1 BOOT`，模拟**大规模断电恢复**。

本机（4 核）实测：吞吐约 **4 台设备/秒**（1.6–1.8 万参数行/秒）—— 100 台同时上电 26 秒完成，
200 台约 1 分钟、400 台约 2 分钟；数据都是全量入库，只是超过 CPE 30 秒超时的设备会在下一次
周期上报收尾。瓶颈是 SQLite 单连接串行写（**别调大连接池**，实测会丢写入）。
方法与完整数据见 `docs/notes/loadtest.md`。

## 目录

```
cmd/acs/            程序入口（监听、优雅退出、双端口路由）
internal/cwmp/      协议核心：SOAP 编解码、会话、任务、诊断、Connection Request
internal/store/     SQLite：设备 / 参数 / 任务 / 上报记录（迁移走 user_version）
internal/web/       Web UI 与 JSON API（模板 + 少量原生 JS，无前端框架）
test/cpesim/        自研 CPE 模拟器（大量开关，验收靠它）
deploy/             发布包里的安装 / 升级 / 卸载脚本与 systemd 单元模板
scripts/            打包、验收、压测、参考实现拉取、开发用起停脚本
docs/               需求文档、发布说明与实现笔记
```

## 更多

- `docs/requirements.md` —— 需求与实现进度
- `docs/notes/deploy.md` —— 安装包与部署脚本的实现笔记（systemd 加固、升级回滚、怎么验的）
- `docs/notes/i18n.md` —— 面板多语言方案（为什么用中文字面量当 key、怎么加一种语言）
- `docs/notes/loadtest.md` —— **压测记录**：集体上电的并发上限、瓶颈（SQLite 单连接）与实测数据
- `docs/notes/implementation-notes.md` —— **真机踩坑与实测记录**：协议边界（单次 GetParameterValues 上限、
  写回类型大小写、诊断要最后置 `Requested`）、设备怪癖（能改不能读、异步生效、身份键被元数据改写）、
  运维坑（挂载掉了不能乱删、任务别卡在 running）……修法与验证都在里面

仓库里的真机样本与文档**均已脱敏**（序列号、MAC、SSID、内网地址、终端名换成示例值）；
运行时数据库 `data/` 与日志不进仓库。

## 许可证

[AGPL-3.0](LICENSE)。可以自由使用、修改、分发（含商用）；但把**修改后**的版本作为网络服务提供给别人用时，
需要把对应源码以同样许可开放。

## 致谢

本项目由 **Deepseek V4.1 Flash** 辅助开发。

<img src="docs/images/thanks.png" alt="小肥鱼太棒了！" width="300">

## 赞助 Buy me a coffee

**TRC20 TJAHCw3UKdysUnkqDLZxPn39LJ6FigCdoB**
