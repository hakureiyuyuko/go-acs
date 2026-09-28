# 安装包与部署脚本（实现笔记）

面向维护者：为什么发布包长这样、脚本里哪些地方是刻意的。

## 打包形态

`scripts/build-release.sh vX.Y.Z` 产出 `dist/` 下两个包加一份校验和：

```
acs-X.Y.Z-linux-amd64.tar.gz
acs-X.Y.Z-linux-arm64.tar.gz
SHA256SUMS
```

包里就是「一个二进制 + 三个脚本 + systemd 单元模板」：

```
acs-X.Y.Z-linux-amd64/
├── acs                 静态二进制（CGO_ENABLED=0，modernc.org/sqlite 纯 Go）
├── install.sh          安装 / 原地升级
├── update.sh           从 GitHub 升级（校验、备份、回滚）
├── uninstall.sh        卸载（默认留数据）
├── acs.service         systemd 单元模板，install.sh 会填占位符
├── README.md           部署说明（= deploy/README.md）
├── LICENSE             AGPL-3.0
└── VERSION
```

编译用 `-trimpath -ldflags "-s -w"`：路径不进二进制，体积也小（amd64 包约 5.9 MB）。

## 落点与约定

| 项目 | 路径 |
|---|---|
| 主程序 | `/usr/local/bin/acs` |
| 数据（数据库、升级备份、pkg）| `/var/lib/acs`（0750，属主 `acs`）|
| 配置 | `/etc/default/acs`（0600，systemd EnvironmentFile）|
| 单元 | `/etc/systemd/system/acs.service` |
| 运行用户 | `acs`（系统用户、nologin）|

## 刻意的几处

1. **`install.sh` 重复运行 = 原地升级**：已存在的 `/etc/default/acs` 默认不覆盖（要覆盖加
   `--force`），数据库当然也不动。这样「下载新包 → 解压 → 跑 install.sh」就是升级路径，
   `update.sh` 是在此之上多了「自己下载 + 校验 + 回滚」。
2. **版本靠 `VERSION` 文件，不靠 `acs -version`**：二进制里没有版本常量，也不想为一个展示字段
   引入构建期 ldflags 依赖。`install.sh` 把包里的 `VERSION` 落到数据目录，`update.sh` 用它判断
   「已是最新」和给备份命名。以后真加了 `-version`，这两处一起改。
3. **`update.sh` 必须先停服务再换二进制**：Linux 上覆盖一个正在运行的可执行文件会 `ETXTBSY`。
   换完起服务做健康检查，不健康就把备份装回去再起一次（备份在 `/var/lib/acs/backups/`，
   默认留最近 3 个，`--keep` 可调）。数据库与配置全程不动。
4. **升级来源两条路**：默认走 GitHub API（curl 拉 JSON，压成一行后用 grep 抠字段——不依赖 jq，
   小机器上不用额外装东西）；`--file` 走本地包，内网 / 离线现场用。包用 release 里的
   `SHA256SUMS` 校验，校验不过直接拒绝安装。
5. **systemd 加固**：`ProtectSystem=strict` + `ReadWritePaths=<数据目录>`、`NoNewPrivileges`、
   `PrivateTmp`。**数据库挪到别处时，必须同步把新目录加进 `ReadWritePaths`**，否则服务起不来
   （会在安装脚本的健康检查里直接暴露出来，不会静默）。
6. **小于 1024 的端口**（比如 `:80`）需要 `AmbientCapabilities=CAP_NET_BIND_SERVICE`，
   单元里留了注释行，去掉注释即可。
7. **健康检查判据**：面板端口返回 2xx/3xx/401/403 都算正常（开了面板鉴权就是 401）。
   端口从 `/etc/default/acs` 里的 `ACS_WEB_LISTEN`、没有取 `ACS_LISTEN`。

## 这套脚本是怎么验的

- 打包：`scripts/build-release.sh v1.0.0`，检查两个包的内容与权限（755 保持得住，Linux 上 tar
  本来就带权限，不像 Windows 那样丢可执行位）。
- `install.sh`：① `--no-service` + 自定义目录（普通用户即可）→ 二进制、配置、VERSION 落位正确；
  ② 完整 systemd 分支用 `unshare -rm`（假 root）+ 一个假的 `systemctl` 驱动跑通：生成单元 →
  `daemon-reload`/`enable`/`restart` → 面板返回 401（鉴权生效）→ 健康检查通过。
- `update.sh`：`--file` 本地包升级 → 备份、`VERSION`、`/var/lib/acs/pkg` 刷新都对；
  坏包 / 包不存在 / release 还不存在三种失败都给明确错误与退出码 1；健康检查与回滚分支同样在假
  systemctl 下覆盖过。`--dry-run` 全程只打印。
- `uninstall.sh`：默认保留数据与配置；`--purge` 连数据、配置、服务用户一起删。

**还没在真机上跑过 `sudo ./install.sh`**（开发机 `mint` 没有免密 sudo）：真 root + 真 systemd 的
那一遍需要在有 sudo 的机器或干净容器里过一遍，重点看 `useradd`、单元加载、`ProtectSystem` 下能否
正常写库。
