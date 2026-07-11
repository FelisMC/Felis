# Felis

一个 Kubernetes 驱动的 Minecraft 服务器托管平台，一行命令部署，自动管理服务器启动、备份与安全。  
A Kubernetes-driven Minecraft server hosting platform — one command to deploy, automatic lifecycle, backup, and security.

[简体中文](README.md) | [English](README_EN.md)

## 目录

- [特性](#特性)
- [使用方式](#使用方式)
- [从源码构建](#从源码构建)
- [开源协议](#开源协议)
- [致谢](#致谢)

## 特性

- **即开即玩**：玩家连接服务器时自动启动，空闲后自动停止，像休眠一样省资源。
- **Web 控制面板**：浏览器中查看服务器状态、管理备份、审批模组包、管理玩家账号。
- **自动备份与恢复**：定时打包世界存档，支持从任意备份点一键恢复。
- **智慧回收**：超过 15 天无人游玩的世界自动备份后删除，不浪费磁盘。
- **多核心支持**：兼容 Paper、Fabric、Forge、NeoForge，通过 Velocity 代理统一入口。
- **模组自助提交**：玩家可自行上传模组包，服主审批通过后自动构建并部署。
- **Passkey 登录**：支持指纹、面容、硬件密钥等无密码认证方式。
- **零信任安全**：基于 Cloudflare Zero Trust 架构，面板流量由 Cloudflare Access 保护，集群内 API 不暴露到公网。

## 使用方式

### 环境要求

- Linux 主机（amd64，Kernel ≥ 5.4）
- 4 GB 内存起步，推荐 8 GB
- 20 GB 磁盘起步
- root 或 sudo 权限

### 一键安装

在准备好的 Linux 主机上执行：

```bash
curl -fsSL https://raw.githubusercontent.com/MliroLirrorsIngenuity/Felis/main/deploy/bootstrap.sh | sudo bash
```

脚本会自动安装 K3s 集群，部署 Felis 控制平面，并启动 TUI 设置向导引导你完成域名、管理员账号等初始配置。

### 创建服务器

1. 打开浏览器访问配置的域名，登录控制面板
2. 进入「服务器」页面，点击「创建服务器」
3. 选择核心类型和版本，设置内存上限和子域名
4. 点击创建，系统自动构建镜像并启动

### 玩家入驻

玩家在 Minecraft 客户端中添加 Velocity 代理地址即可进入大厅。使用 `/link` 命令获取绑定码，在面板中完成账号绑定。

## 从源码构建

本项目基于 Go 和 Node.js 开发：

```bash
# 后端（Go 1.26+）
go build -o felis ./cmd/felis

# 前端面板（Node.js 22+）
cd panel
npm ci
npm run build

# Docker 镜像
docker build -t felis:custom .
```

更多开发细节请参阅 [CONTRIBUTING.md](./CONTRIBUTING.md)。

## 开源协议

本项目代码部分遵循 MIT License 开源协议。

### 协议注意事项

1. **保留版权声明**：在您分发本项目的副本或基于本项目衍生的软件中，必须包含原作者的版权声明和许可声明。
2. **免责声明**：本项目按"原样"提供，作者不承担任何因使用本项目而产生的法律责任。

## 致谢

- [Kubernetes](https://kubernetes.io/) / [K3s](https://k3s.io/) —— 底层编排引擎
- [Cloudflare Zero Trust](https://www.cloudflare.com/zero-trust/) —— 零信任安全基础设施
- [PostgreSQL](https://www.postgresql.org/) —— 数据持久化
- [React](https://react.dev/) + [Vite](https://vitejs.dev/) + [TailwindCSS](https://tailwindcss.com/) —— 前端技术栈
- [Bubble Tea](https://github.com/charmbracelet/bubbletea) —— TUI 框架
- [Minecraft](https://www.minecraft.net/) —— 让这一切值得做
