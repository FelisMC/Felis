# Felis

一款 Kubernetes 驱动的 Minecraft 服务器托管平台，一行命令部署，自动管理生命周期与安全。  
A Kubernetes-driven Minecraft server hosting platform — one command to deploy, automatic lifecycle, backup, and security.

[简体中文](README.md) | [English](README_EN.md)

目录

- [特性](#特性)
- [使用方式](#使用方式)
- [从源码构建](#从源码构建)
- [开源协议](#开源协议)
- [致谢](#致谢)

## 特性

- **即开即玩**：玩家尝试连接时自动唤醒服务器，空闲后自动休眠，像游戏主机一样省资源。
- **Web 控制面板**：浏览器中查看服务器状态、在线玩家与资源用量，管理备份与恢复。
- **备份与恢复**：一键把整服数据（世界、配置、插件/模组，即整个 /data 卷）打包进集群内的归档库，支持从任意备份点回滚；默认安装就已启用（归档 PVC 与路径由安装器一并生成）。
- **控制面数据库备份**：账号、服务器归属、配额与存档索引所在的数据库每天自动备份，每次升级迁移前先快照，出错可用 `felis db restore` 整库原子回滚；面板「维护与备份」页显示备份是否新鲜（见 [故障排查 §16](docs/troubleshooting.md)）。
- **智慧回收（可选开启）**：超过 15 天无人游玩的世界自动备份后删除，释放磁盘空间；安装时设置 `FELIS_WORLDS_HOST_PATH`（k3s 默认 `/var/lib/rancher/k3s/storage`）即启用每日回收，不设置则不删任何世界。
- **多核心支持**：兼容 Paper、Fabric、Forge、NeoForge，经由 Velocity 代理统一入口。
- **模组自助提交**：玩家自行上传模组包，服主审批通过后自动构建；构建产物进入镜像白名单，可直接选用为服务器镜像完成部署。
- **Passkey 登录**：支持指纹、面容、硬件密钥等无密码认证方式。
- **零信任安全**：面板流量由 Cloudflare Access 保护，集群内 API 不暴露到公网。

## 使用方式

在准备好的 Linux 主机上执行：

```bash
curl -fsSL https://raw.githubusercontent.com/FelisMC/Felis/main/deploy/bootstrap.sh | sudo bash
```

脚本将自动安装 K3s、部署控制平面并启动设置向导。完成后浏览器访问已配置的域名进入控制面板即可使用。

> **本仓库当前为私有**，上面这条会返回 404。请改用带凭据的形式；安装器自身也需要同一个 token
> 去解析并下载 release，所以用 `sudo -E` 把它带进去：
>
> ```bash
> export FELIS_GITHUB_TOKEN=<对本仓库有读权限的 token>
> printf 'header = "Authorization: Bearer %s"\n' "$FELIS_GITHUB_TOKEN" \
>   | curl -fsSL --config - -H "Accept: application/vnd.github.raw" \
>       https://api.github.com/repos/FelisMC/Felis/contents/deploy/bootstrap.sh \
>   | sudo -E bash
> ```
>
> token 经 stdin 交给 `curl --config -`，不放在命令行上：argv 在 `/proc` 下对本机任意用户可读，
> 而这正是安装器内部 `github_api` 采用同一写法的原因。

重跑这条命令也是把 felis-api 升到新版本的方式（`felis setup` 做不到，它用的是本机已有的二进制）。
重跑会沿用已安装的根域名，但**不会**沿用通道：若本机跟随 main，需一并 `export FELIS_VERSION_BOOTSTRAP=dev`。

## 从源码构建

本项目基于 Go 和 Node.js 开发：

```bash
# 后端（Go 1.26+）
go build -o felis ./cmd/felis

# 前端（Node.js 22+）
cd panel
npm ci
npm run build

# Docker 镜像
docker build -t felis:custom .
```

## 开源协议

本项目遵循 [AGPL-3.0-only](LICENSE) 开源协议。

### 协议注意事项

1. **衍生作品同样是 AGPL**：分发本项目的副本或基于本项目衍生的软件时，必须以 AGPL-3.0 开源，并保留原作者的版权声明和许可声明。
2. **通过网络提供服务同样要开源**（AGPL 第 13 条）：如果你把修改过的 Felis 架起来给别人用，即使从不分发任何二进制，也必须向这些用户提供你那份修改后的完整源码。这是 AGPL 相对 GPL 的唯一实质区别，而 Felis 正是一个跑在网络上的托管平台，所以这一条基本总会触发。
3. **免责声明**：本项目按"原样"提供，作者不承担任何因使用本项目而产生的法律责任。

## 致谢

- [Kubernetes](https://kubernetes.io/)：底层容器编排引擎
- [K3s](https://k3s.io/)：轻量级 Kubernetes 发行版
- [Cloudflare Zero Trust](https://www.cloudflare.com/zero-trust/)：零信任安全基础设施
- [PostgreSQL](https://www.postgresql.org/)：数据持久化
- [React](https://react.dev/)：前端用户界面框架
- [Vite](https://vitejs.dev/)：前端构建工具
- [TailwindCSS](https://tailwindcss.com/)：CSS 框架
- [Bubble Tea](https://github.com/charmbracelet/bubbletea)：TUI 框架
- [Minecraft](https://www.minecraft.net/)：让这一切值得做
