# 贡献指南（Contributing）

> ## ⚠️ 本仓库原则上**只在中华人民共和国工作日**处理 issue 与 Pull Request
>
> 具体地说：以**北京时间（UTC+8）**为准，周六、周日、法定节假日（含国务院办公厅公布的
> 放假与调休安排）**不保证**响应；节后第一个工作日会集中处理积压。
>
> 这是维护者一个人维护的开源项目，请按这个节奏预期回复时间 —— 不是不理会，
> 而是攒到工作日一起看。**安全相关问题也请走 issue**，我会尽快在工作日处理。

Issue 与 PR 用**中文或英文**都可以。欢迎任何形式的反馈：报 bug、提需求、改文档、
补测试、加新机型的参数映射（见下文「厂商参数映射」）。

---

## 目录

- [先说清楚这个项目在意什么](#先说清楚这个项目在意什么)
- [报告问题（Issue）](#报告问题issue)
- [提交代码（Pull Request）](#提交代码pull-request)
- [开发环境](#开发环境)
- [代码与文档约定](#代码与文档约定)
- [验收：提交前必须跑的](#验收提交前必须跑的)
- [提交信息规范](#提交信息规范)
- [厂商参数映射：加一台新光猫](#厂商参数映射加一台新光猫)
- [审查与合并](#审查与合并)
- [许可](#许可)

---

## 先说清楚这个项目在意什么

这个 ACS 是「单二进制 + 一个 SQLite 文件」的管理工具，跑在光猫/FTTR 机房里。
下面这些原则决定了什么样的改动会被接受，**它们比"功能多"更重要**：

1. **不编数字**。设备没上报就显示 `-` 或整块不显示，绝不猜一个看起来合理的值。
   探测不到的能力（WAN、FTTR、光功率…）就不要在界面上摆空壳。
2. **真机说了算**。能力是否存在必须**在真机上真跑一次**（或写一个能复现的模拟器场景），
   不能只看 `ffmpeg -encoders` 之类的清单，也不能只凭文档。验收里要给出证据。
3. **零外部中间件**。不引入 Redis / MQ / 前端构建链（npm、vite…）。新增 Go 依赖要说明必要性。
4. **界面文案像正式产品**。实现细节、协议内部解释不要出现在界面上 —— 写进
   `docs/notes/implementation-notes.md`。
5. **如实呈现设备的怪脾气**。能改不能读、异步生效、参数名拼错、单位不统一……
   都如实记录（真机踩坑一律写进实现笔记），而不是在界面上抹平。

## 报告问题（Issue）

请带上这些信息，能省掉好几轮来回：

- **版本**：`VERSION` 文件、`git describe --tags`，或者是从哪个 Release 装的
- **设备**：厂商 / 型号 / 软件版本 / 数据模型（TR-098 还是 TR-181）/ 大体参数条数
- **现象**：期望什么、实际什么；界面问题最好附截图
- **证据**：`data/acs.log` 相关片段（`-log-soap` 打开后的原始 SOAP 报文更佳）、
  任务列表里那条任务的结果、相关参数名与值

**脱敏**：序列号、MAC、SSID、公网/内网地址、终端名请替换成示例值再贴。仓库里所有
真机样本都是脱敏过的，请保持一致。

## 提交代码（Pull Request）

- **一个 PR 只做一件事**。顺手的重构、格式化、无关修复请另开。
- **先说明为什么**（对应什么现场问题、哪台设备上复现），再说改了什么。
- **附上验收证据**：跑了哪些检查、结果如何；真机验证请写清机型与结论。
  「我看代码觉得没问题」不算证据。
- **改动行为要说清楚**：改了既有页面的展示口径、任务结果文案、数据库结构，
  都要在 PR 描述里点出来，并同步更新文档。
- **别改仓库归属**：请勿把自己的 fork 地址（`github.com/<你>/go-acs`）写进
  `README`、`deploy/README.md`、`deploy/acs.service`、`deploy/update.sh` 等文件 ——
  合并到上游后那些地方必须指向上游仓库。
- **不要动这些底线**（改了要单独说明理由）：
  - `store.Open` 里的 `SetMaxOpenConns(1)`：压测证明调大连接会 `SQLITE_BUSY` 丢写入；
  - 迁移只能**追加在末尾**（按 `PRAGMA user_version` 顺序执行，插在中间会让老库跳过）；
  - i18n 用**中文字面量当 key**（`{{T "状态"}}`），不要改成 `overview.title` 这类键名。
- **不要 `git add -A`**：只 stage 你真正改动的文件（本机可能同时存在别的工作目录）。
- **界面文案有英文**：新增中文文案要同时在 `internal/i18n/strings_en.go` 里给出译文
  （漏翻会有测试卡住你，见下）。

## 开发环境

只需要 Go（1.27+）与一个可选的模拟器，不需要数据库服务、不需要 npm：

```bash
git clone https://github.com/hakureiyuyuko/go-acs.git && cd go-acs

export PATH=$HOME/.local/go/bin:$PATH   # 若 Go 装在自定义路径
CGO_ENABLED=0                            # 纯 Go 的 modernc.org/sqlite

go build -o acs ./cmd/acs                # 服务端
go build -o cpesim ./test/cpesim         # 自研 CPE 模拟器（验收靠它）
```

起一台本地实例（后台跑、带 pidfile、日志在 `data/acs.log`）：

```bash
scripts/dev-server.sh start|stop|restart|status|log
```

没有真机也能玩 —— 模拟器带一堆开关（FTTR 子设备、光功率、能改不能读、异步诊断…）：

```bash
./cpesim -acs http://127.0.0.1:9090/acs -serial DEMO0123 -fttr 3 -fttr-optical -optical
```

## 代码与文档约定

- **Go**：`gofmt` 必须干净；注释写**为什么**这么做（尤其是踩过的坑），不要复述代码。
  一个包一个文件干一件事，纯逻辑抽成可单测的函数。
- **前端**：模板 + 一点原生 JS（`internal/web/static/app.js`），不引入框架与构建步骤。
- **测试**：
  - 纯函数、模板渲染、存储层尽量写单测（`go test ./...`）；
  - 端到端走 `scripts/verify-s1.sh`（真 HTTP + 真 SOAP + 真 SQLite），
    新增功能请**补断言**，断言要能被反向验证（把修复去掉时它会红）；
  - 与其它实现互通走 `scripts/verify-interop.sh`。
- **文档**：
  - 真机发现、协议边界、设备怪癖 → `docs/notes/implementation-notes.md`；
  - 面向前端的约定（i18n、压测、部署）→ `docs/notes/` 下对应文档；
  - 发版说明 → `docs/releases/vX.Y.Z.md`（只由维护者在发版时添加）；
  - 需求与进度 → `docs/requirements.md`。

## 验收：提交前必须跑的

```bash
export PATH=$HOME/.local/go/bin:$PATH
CGO_ENABLED=0

gofmt -l . | grep -v '^reference/'     # 期望：无输出
go vet ./...                           # 期望：无输出
go test ./...                          # 期望：全部 ok
bash scripts/verify-s1.sh              # 期望：全部通过（当前 373 项）
bash scripts/verify-interop.sh         # 期望：8 / 0（需要 node 与 reference/genieacs-sim）
```

PR 描述里请附上这几条的实际输出（或结果摘要）。CI 也会跑其中的大部分。

## 提交信息规范

- 用**中文**写，第一行说清「做了什么」，正文说清「为什么」与**真机发现**（有就写）；
- 建议前缀：`feat:` / `fix:` / `docs:` / `refactor:` / `test:` / `ci:`;
- 一次提交只做一件事；不要提交 `dist/`、`data/`、日志、编辑器配置等产物。

示例：

```
fix(web): 无线概况不再显示射频对象造出的假实例（那行「5G ｜ - ｜ 开 ｜ -」）

根因：SSID 一级的 WLANConfiguration.{i} 真机实例号是 1/5，射频一级的
WiFi.Radio.{i} 是 1/2，按实例号硬合并就凭空多出一行。真机三台验证已回归。
```

## 厂商参数映射：加一台新光猫

各家的光功率 / 温度 / 电压等私有参数名、位置、单位都不一样，同一台设备上还可能同时存在
「直接报真实值」与「按光模块寄存器（SFF-8472）编码的原始值」两种形态。这些映射存在数据库
表 `param_aliases` 里，**加机型不需要改代码**：

```bash
acs alias kinds                                     # 看支持的换算规则
acs alias ls  --db /var/lib/acs/acs.db              # 看现有映射
acs alias add --db /var/lib/acs/acs.db \
    --field rx_power --contains optical.interface. --suffix .rxpower \
    --decode identity --priority 5 --vendor 中兴 --note "ZXHN F610GV9 实测 -23.4 dBm"
acs alias rm  --db /var/lib/acs/acs.db 12
```

如果这条路走不通（新字段、新换算方式），那就需要改代码：字段定义在
`internal/web/params_map.go` 的 `panelFields`，换算规则在同文件的 `decodeRaw`。
请在 PR 里说明**真机实测值**（设备自己页面上显示多少、你映射出来多少）。

## 审查与合并

- 维护者会在**工作日**看你的 PR（见顶部说明），通常会给出「必改项 + 讨论项」两档意见；
- 合入前会复跑上面那套验收；行为变化会顺带更新文档与发布说明；
- 合并方式以 squash 为主，提交信息会保留你的署名（**请在提交里保留你自己的名字与邮箱**）。

## 许可

本项目以 [AGPL-3.0](LICENSE) 发布。你提交的任何贡献都视为**按同一许可授权**
（即同意以 AGPL-3.0 分发，并允许维护者随项目一起再分发）。

---

还拿不准怎么改？先开一个 issue 描述现场问题，我们**在工作日**一起看。
