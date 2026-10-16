<div align="center">
  <h1 align="center">
    <img src="assets/felis-logo.png" alt="Felis logo" width="270"><br>
    Felis
  </h1>
  <p align="center">
    基于 Kubernetes 的 Minecraft 服务器托管平台<br>
    单条命令完成部署，自动管理服务器的启停、备份与玩家接入
    <br><br>
    简体中文 | <a href="README_EN.md">English</a>
    <br>
    <a href="https://felismc.com/">官网</a> | <a href="https://docs.felismc.com/">文档</a>
  </p>
</div>

> [!CAUTION]
> **当前仅提供开发快照，已撤下公开 Release。仅供开发者在隔离的测试环境中验证，不建议普通用户部署。**
> **此项目仍处于早期开发阶段，您不该在任何生产环境使用该项目。若产生任何问题，贵用户的使用行为与 FelisMC 团队无任何民事刑事法律关系。**<br>
> **THIS PROJECT IS STILL WIP, YOU SHOULD DO NOT USE THIS PROJECT IN ANY PRODUCTION USAGE. WE ARE NOT RESPOND FOR ANY LEGAL OR HUMANLY PROBLEM.**

Felis 是一个自托管的 Minecraft 服务器托管平台。一条安装命令即可在一台 Linux 主机上部署 k3s、PostgreSQL、控制面与 Velocity 游戏代理；此后服务器的创建、启停、备份与玩家接入均在 Web 面板中完成，主机的日常运维也可以在面板中执行。

<details>
<summary>目录</summary>

- [特性](#特性)
- [工作方式](#工作方式)
- [快速开始](#快速开始)
- [文档](#文档)
- [从源码构建](#从源码构建)
- [开源协议](#开源协议)
- [致谢](#致谢)

</details>

## 特性

- **一条命令部署**：安装器在修改主机之前检查内存、磁盘、端口与网络，随后安装 k3s、数据库、控制面与游戏代理；设置向导引导选择面板访问方式与存储，并创建首位所有者。
- **按需启停**：玩家连接时自动唤醒目标服务器，启动期间在大厅等待，服务器就绪后自动传送；无人在线的服务器按设定时长休眠，释放内存。
- **Web 控制面板**：在浏览器中管理控制台（RCON）、白名单、封禁、OP 与 LuckPerms 权限；文件管理支持分片上传、下载与导入世界；计划任务可定时执行命令、重启、启停或备份，并提前在游戏内提醒玩家。
- **玩家接入**：所有玩家经由同一个 Velocity 游戏代理进入，支持 Mojang 正版与第三方 Yggdrasil 认证源；登录空间负责身份验证与账户关联，正式大厅提供服务器菜单，玩家进入策略可在面板中配置。
- **备份与恢复**：手动备份、不停服备份与每日恢复点默认启用，支持下载备份与导出世界；控制面数据库每日备份，备份可加密同步到 S3 兼容存储，并用于在新主机上整机重建。
- **服务端软件**：服务器可运行 Paper、Purpur、Fabric、Quilt、Forge、NeoForge、原版或自定义 jar，并自选游戏版本、构建与 Java 版本；服务端从官方来源下载并校验哈希值，同一版本在集群内只下载一次，能够接入身份转发的版本由平台自动配置。所有者可开放自助建服，用户在配额内创建自己的服务器。
- **镜像与模组包**：管理员也可以使用镜像白名单中的服务端镜像；用户可提交模组包，经管理员审核后自动构建，并通过 Trivy 安全扫描后加入白名单。
- **运维与告警**：面板「维护与备份」集中展示主机状态、未解决的告警、健康检查、诊断包与版本更新；访问方式、根域名、凭据轮换、异地副本等主机操作可在面板中执行，并记入审计日志。看门狗定时巡检，异常时向所有者发送邮件，支持外部心跳与 Prometheus 指标。
- **安全**：通行密钥与邮箱验证码登录，敏感操作要求重新验证身份并记入审计日志；游戏服务器之间网络隔离，内部 API 不对公网开放；面板可经由自有反向代理或 Cloudflare Tunnel 与 Access 访问。
- **多机部署（实验性）**：主控节点统一调度多台工作节点，支持停服迁移、故障切换与主控高可用。

## 工作方式

- **服务器**：每台 Minecraft 服务器以 Kubernetes 资源的形式运行在 k3s 中，世界与服务端保存在独立的数据卷上。管理员创建服务器供用户认领，有配额的用户也可以自行创建；用户关联游戏身份后管理自己的服务器。
- **玩家**：玩家连接游戏代理后先进入登录空间完成身份验证，再进入目标服务器；目标服务器休眠时，系统自动唤醒并让玩家在大厅等待。
- **数据**：账户、服务器归属、配额与备份索引保存在 k3s 中运行的 PostgreSQL；世界备份保存在集群内的归档存储，可选同步到异地存储桶。
- **主机**：`felis` 命令与若干 systemd 服务负责数据库备份、看门狗、异地同步与主机运维；面板中的主机操作由主机上的 root 服务以对应命令执行。

## 快速开始

**环境要求**

- 64 位 Linux 主机（`x86_64` 或 `aarch64`），使用 systemd，具有 root 权限；
- 支持 CentOS Stream 9、RHEL 9、Rocky Linux 9、AlmaLinux 9、Fedora、Ubuntu、Debian 12、openSUSE 与 Arch Linux；参考平台为 CentOS Stream 9 与 Ubuntu 24.04 LTS；
- 至少 2 vCPU、4 GB 内存（另加 2 GB 交换空间）与 40 GB 磁盘，可承载约 20 名同时在线玩家。

完整要求见[环境要求](https://docs.felismc.com/guide/requirements)。

**安装**

当前仅提供开发通道（跟随 `main` 分支，从源码构建）。在隔离的测试主机上执行：

```bash
curl -fsSL https://raw.githubusercontent.com/FelisMC/Felis/main/deploy/bootstrap.sh | sudo FELIS_VERSION_BOOTSTRAP=dev bash
```

安装器询问安装类型时选择 `[1] Felis`。安装完成后，设置向导引导选择面板访问方式与存储，并在终端给出首位所有者的一次性网页设置链接；在浏览器中打开该链接，登记邮箱并创建通行密钥即可进入面板。安装变量、离线安装与安装后的检查见[安装与部署](https://docs.felismc.com/guide/deployment)。

## 文档

完整文档位于 [docs.felismc.com](https://docs.felismc.com/)，提供简体中文与英文版本。

- **开始使用**：[环境要求](https://docs.felismc.com/guide/requirements) · [安装与部署](https://docs.felismc.com/guide/deployment)
- **使用指南**：[管理服务器](https://docs.felismc.com/guide/servers) · [服务端软件](https://docs.felismc.com/guide/software) · [玩家接入](https://docs.felismc.com/guide/players) · [平台管理](https://docs.felismc.com/guide/administration)
- **运维**：[运维概览](https://docs.felismc.com/operations/) · [备份与恢复](https://docs.felismc.com/operations/backup) · [升级](https://docs.felismc.com/operations/upgrade) · [监控与告警](https://docs.felismc.com/operations/monitoring) · [故障排查](https://docs.felismc.com/operations/troubleshooting) · [多机部署与高可用](https://docs.felismc.com/guide/distributed)
- **参考**：[命令行](https://docs.felismc.com/reference/cli) · [配置](https://docs.felismc.com/reference/configuration) · [API](https://docs.felismc.com/reference/api) · [系统架构](https://docs.felismc.com/reference/architecture) · [已知限制](https://docs.felismc.com/reference/limitations)
- **开发**：[从源码构建](https://docs.felismc.com/guide/building) · [贡献指南](https://docs.felismc.com/reference/contributing)

## 从源码构建

后端使用 Go，面板使用 React 与 Vite：

```bash
# 后端
go build -o felis ./cmd/felis

# 面板
cd panel
npm ci
npm run build
```

工具链版本、镜像与插件的构建以及测试的运行方式见[从源码构建](https://docs.felismc.com/guide/building)。

## 开源协议

本项目采用 [AGPL-3.0-only](LICENSE) 许可证。

1. **衍生作品须采用 AGPL**：分发本项目的副本或基于本项目的衍生软件时，须以 AGPL-3.0 开源，并保留原作者的版权声明与许可声明。
2. **网络服务同样须提供源码**（AGPL 第 13 条）：通过网络向他人提供经修改的 Felis 服务时，即使未分发任何二进制文件，也须向这些用户提供修改后的完整源码。Felis 是通过网络访问的托管平台，几乎所有部署场景都适用此条款。
3. **免责声明**：本项目按“原样”提供，作者不承担因使用本项目而产生的任何法律责任。

详见[开源协议](https://docs.felismc.com/reference/license)。

## 致谢

- [Kubernetes](https://kubernetes.io/)：容器编排引擎
- [k3s](https://k3s.io/)：轻量级 Kubernetes 发行版
- [Velocity](https://papermc.io/software/velocity)：Minecraft 代理服务端
- [PostgreSQL](https://www.postgresql.org/)：数据持久化
- [Cloudflare Zero Trust](https://www.cloudflare.com/zero-trust/)：零信任访问
- [React](https://react.dev/)、[Vite](https://vitejs.dev/)、[Tailwind CSS](https://tailwindcss.com/)：面板前端
- [Bubble Tea](https://github.com/charmbracelet/bubbletea)：终端界面框架
- [Minecraft](https://www.minecraft.net/)：本项目服务的游戏
