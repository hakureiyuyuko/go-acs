# 轻量 TR-069 ACS —— 安装包

单二进制、零外部中间件的 TR-069/CWMP ACS（管理光猫 / FTTR 主机）。
项目与源码：https://github.com/hakureiyuyuko/go-acs

## 包里有什么

| 文件 | 说明 |
|---|---|
| `acs` | 主程序（Linux 静态二进制，无动态库依赖）|
| `install.sh` | 安装（装 systemd 服务，可重复运行 = 原地升级）|
| `update.sh` | 升级（从 GitHub 拉新版，校验、备份、失败自动回滚）|
| `uninstall.sh` | 卸载（默认保留数据，`--purge` 连数据一起删）|
| `acs.service` | systemd 单元模板（`install.sh` 会按参数填好）|
| `VERSION` | 版本号 |

## 安装

```bash
VERSION=1.2.2                                  # 换成你下载的那个版本
tar xzf acs-$VERSION-linux-amd64.tar.gz
cd acs-$VERSION-linux-amd64
sudo ./install.sh
```

装完会打印面板地址与要填进光猫的 ACS URL。常用选项：

```bash
sudo ./install.sh --web-listen :8080                 # 面板单独一个端口
sudo ./install.sh --web-user admin --web-pass 'xxx'  # 顺便设面板账号密码
sudo ./install.sh --dry-run                          # 先看它要做什么，不动系统
sudo ./install.sh --help                             # 全部选项
```

默认落点：

| 项目 | 路径 |
|---|---|
| 主程序 | `/usr/local/bin/acs` |
| 数据目录（数据库、升级备份）| `/var/lib/acs` |
| 配置文件 | `/etc/default/acs` |
| systemd 单元 | `/etc/systemd/system/acs.service` |
| 运行用户 | `acs`（系统用户，不可登录）|

## 给光猫配 ACS URL

- CWMP 端点：`http://<本机IP>:7547/acs`
- 面板：`http://<本机IP>:7547/`
- 光猫上只允许填根路径的设备也支持：`http://<本机IP>:7547/`

面板用 `--web-listen` 挪到独立端口后，CWMP 那个端口**任何路径都受理**，不用担心运营商定制固件的路径写法。

## 升级

```bash
sudo ./update.sh                # 拉最新版
sudo ./update.sh --check        # 只看有没有新版本
sudo ./update.sh --tag v1.2.2   # 升到指定版本
sudo ./update.sh --file acs-$VERSION-linux-amd64.tar.gz   # 内网/离线，用本地包
```

升级流程：下载 → 校验 SHA256 → 停服务 → 旧二进制备份到 `/var/lib/acs/backups/`
（默认留最近 3 个）→ 换新 → 起服务 → 健康检查，**起不来自动回滚**。
数据库与配置不动。

> 也可以直接用新版安装包里的 `install.sh` 原地升级：它保留 `/etc/default/acs` 与数据库。

## 卸载

```bash
sudo ./uninstall.sh          # 只拆服务与主程序，保留数据库、配置、用户
sudo ./uninstall.sh --purge  # 连数据库、配置、服务用户一起删
```

## 端口

| 端口 | 用途 |
|---|---|
| `7547` | CWMP（TR-069），光猫连进来的口 |
| 面板端口 | 默认与 7547 同一个；`--web-listen` 可分开 |

要监听 1024 以下的端口（如 `:80`），把 `acs.service` 里那两行
`AmbientCapabilities` / `CapabilityBoundingSet` 的注释去掉再重装。

## 配置

配置全在 `/etc/default/acs`（环境变量），改完 `sudo systemctl restart acs`。
变量名与命令行参数一一对应，完整列表见 README 的「配置」一节。

面板的监听地址改动**重启生效**；面板登录账号密码保存后**立即生效**（其中密码以 PBKDF2 散列存在数据库里）。
面板走**独立登录页 + 会话 cookie**（不是 HTTP Basic），顶栏有「退出」；忘了密码用 `ACS_WEB_AUTH=off` 起一次即可。

## 两个提醒

- 单元里开了 `ProtectSystem=strict`，服务只允许写数据目录；把数据库挪到别处时，记得把它加进 `ReadWritePaths`。
- 面板是登录页 + 会话 cookie，但整个面板走的是**明文 HTTP**（登录密码也是明文传输）。
  TR-069 那一侧基本只能用 HTTP（光猫普遍不支持 HTTPS），所以**别把端口直接暴露到公网**：
  放内网，或前面挂 nginx 做 TLS 与来源限制（走了 HTTPS 时登录 cookie 会自动带 Secure）。
