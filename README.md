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
- **自动备份与恢复**：定时将世界打包存档，支持从任意备份点一键回滚。
- **智慧回收**：超过 15 天无人游玩的世界自动备份后删除，释放磁盘空间。
- **多核心支持**：兼容 Paper、Fabric、Forge、NeoForge，经由 Velocity 代理统一入口。
- **模组自助提交**：玩家自行上传模组包，服主审批通过后自动构建并部署。
- **Passkey 登录**：支持指纹、面容、硬件密钥等无密码认证方式。
- **零信任安全**：面板流量由 Cloudflare Access 保护，集群内 API 不暴露到公网。

## 使用方式

在准备好的 Linux 主机上执行：

```bash
curl -fsSL https://raw.githubusercontent.com/MliroLirrorsIngenuity/Felis/main/deploy/bootstrap.sh | sudo bash
```

脚本将自动安装 K3s、部署控制平面并启动设置向导。完成后浏览器访问已配置的域名进入控制面板即可使用。

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

本项目代码部分遵循 MIT License 开源协议。

### 协议注意事项

1. **保留版权声明**：在您分发本项目的副本或基于本项目衍生的软件中，必须包含原作者的版权声明和许可声明。
2. **免责声明**：本项目按"原样"提供，作者不承担任何因使用本项目而产生的法律责任。

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
