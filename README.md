<div align="center">
  <h1 align="center">
    <img src="docs/assets/felis-logo.png" alt="Felis logo" width="270"><br>
    Felis
  </h1>
  <p align="center">
    基于 Kubernetes 的 Minecraft 服务器托管平台<br>
    单条命令完成部署，自动管理服务器生命周期、备份与安全
    <br><br>
    <a href="README.md">简体中文</a> | <a href="README_EN.md">English</a>
    <br>
    <a href="https://felismc.com/">官网</a> | <a href="https://docs.felismc.com/">文档</a>
  </p>
</div>

> [!CAUTION]
> **此项目仍处于早期开发阶段，您不该在任何生产环境使用该项目。若产生任何问题，贵用户的使用行为与 FelisMC 团队无任何民事刑事法律关系。**<br>
> **THIS PROJECT IS STILL WIP, YOU SHOULD DO NOT USE THIS PROJECT IN ANY PRODUCTION USAGE. WE ARE NOT RESPOND FOR ANY LEGAL OR HUMANLY PROBLEM.**

<details>
<summary>目录</summary>

- [特性](#特性)
- [使用方式](#使用方式)
- [从源码构建](#从源码构建)
- [开源协议](#开源协议)
- [致谢](#致谢)

</details>

## 特性

* **按需启停**：玩家连接代理时自动启动目标服务器，启动期间玩家进入等待队列，服务器就绪后自动传送；服务器空闲后自动停止，释放内存。

* **Web 控制面板**：在浏览器中查看服务器状态、在线玩家与资源用量。
  * 控制台（RCON）、白名单、封禁、OP 与 LuckPerms 权限管理
  * 文件管理：新建、删除、重命名、分片上传、下载，以及停服状态下解压 zip，可用于导入世界
  * 计划任务：按星期与时区定时执行命令、重启、停止、启动或备份，执行前在游戏内向玩家发送提醒

* **备份与恢复**：默认启用，归档 PVC 及其路径由安装器生成。
  * 手动备份：将服务器的完整数据卷（`/data`，含世界、配置、插件与模组）归档至集群内的归档存储，可回滚至任一备份点
  * 每日恢复点：当天有玩家进入过的服务器在停止后自动生成恢复点，默认保留 7 个，保存期限 90 天；恢复点单独轮换，不影响手动备份
  * 下载与导出：支持下载单个备份（附 sha256 校验）、删除单个备份及导出完整世界
  * 异地副本（可选）：备份在主机上加密后同步至 S3 兼容存储（AWS S3、Cloudflare R2、Backblaze B2、MinIO 等）
  * 控制面数据库：存放账号、服务器归属、配额与存档索引的数据库每日自动备份，每次升级迁移前额外创建快照，故障时可通过 `felis db restore` 整库原子回滚；面板「维护与备份」页显示最近一次备份的时效（参见 [故障排查 §16](docs/troubleshooting.md)）

* **运维诊断**
  * `sudo felis status`：汇总显示节点、控制面、游戏代理、各服务器、备份及未解决的告警
  * `sudo felis doctor`：执行全部健康检查，按模块列出问题及排查方向，执行过程中不发送邮件
  * `sudo felis support-bundle`：生成已脱敏的诊断包，供提交问题时附带（参见 [故障排查 §0](docs/troubleshooting.md)）
  * 看门狗：每 2 分钟执行一次巡检，异常持续时向平台所有者发送邮件告警，支持外部心跳监测

* **世界回收（可选）**：超过 15 天无人游玩的世界在备份后删除，以释放磁盘空间。安装时设置 `FELIS_WORLDS_HOST_PATH`（k3s 默认为 `/var/lib/rancher/k3s/storage`）即启用每日回收；未设置时不删除任何世界。过期备份的每日清理与此设置无关，始终执行。

* **多核心支持**：兼容 Paper、Fabric、Forge 与 NeoForge，统一经由 Velocity 代理接入。

* **模组包投稿**：玩家可上传模组包，经服主审批后自动构建并通过 Trivy 安全扫描；构建产物加入镜像白名单后，可直接选作服务器镜像。

* **安全**
  * Passkey 登录：支持指纹、面容识别及硬件密钥等无密码认证方式
  * 零信任访问：面板流量经 Cloudflare Access 保护，集群内部 API 不对公网开放

* **多机部署（实验性，默认关闭）**：由一台主控节点统一下发指令，其余节点仅运行游戏服务器，已停止的服务器可迁移至其他节点。该功能目前仅位于 main 分支，尚未完成三机验收（参见 [多机部署](docs/distributed.md)）。

## 使用方式

在已准备好的 Linux 主机上执行：

```bash
curl -fsSL https://raw.githubusercontent.com/FelisMC/Felis/main/deploy/bootstrap.sh | sudo bash
```

脚本将安装 K3s，在 K3s 中部署 PostgreSQL 与控制平面，随后启动设置向导。设置完成后，通过浏览器访问所配置的域名即可进入控制面板。

* **设置向导**：先完成面板访问方式与存储配置，再由主机管理员创建首位 Owner，终端会给出一次性网页设置链接（30 分钟内有效）。用浏览器打开链接、登记邮箱并创建通行密钥，即可进入面板，无需启动 Minecraft。角色关联可稍后在“账户”页选择认证源、输入角色名或 UUID 并确认；普通玩家继续通过游戏绑定码验证身份。未完成网页登录或链接过期时，再次执行 `sudo felis setup` 会提供新链接；已设置登录凭证的账号不会被重置。登录服或大厅故障不会阻止面板初始化。“账户”页会解释世界树（Yggdrasil）认证、显示进服地址与登录服/大厅所需的 Java 版客户端版本；安装器会把实际构建版本写入 `[velocity].game_version`，自定义镜像需自行填写，未配置时不会猜测版本。标准世界树接口可直接查询角色；非标准 `hasJoined` 地址可通过 `[[auth_source]].api_url` 指定认证站 API 根地址，游戏绑定码仍可作为替代方式。安装器仅在交互式终端中自动启动向导；输出重定向至日志或经由 cloud-init 安装时，请在安装结束后执行 `sudo felis setup`。设置 `FELIS_NO_SETUP=1` 时，安装器在输出摘要后直接结束。

* **支持的系统**：CentOS Stream 9（aarch64）已在实机上验证；Ubuntu 24.04（x86_64）在每次推送时由 CI 执行全新安装、重复安装、升级及上述安装命令（参见 [运维手册 §1](docs/operations.md#1-supported-hosts)）。

* **安装前检查**：安装器在修改主机之前检查内存、磁盘、端口、网段冲突、已有的 Kubernetes 及外网连通性。发现问题时一次性列出全部问题并退出，主机保持原状（检查项参见 [运维手册 §1](docs/operations.md#1-supported-hosts)）。

* **升级**：重新执行安装命令即可将 felis-api 升级至新版本；`felis setup` 仅使用本机已安装的二进制，无法用于升级。重新执行时沿用已安装的根域名，发布通道需重新指定：跟随 main 分支的主机须同时设置 `export FELIS_VERSION_BOOTSTRAP=dev`。早期版本安装在宿主机上的 PostgreSQL 会在重新执行时整库迁入 K3s，宿主机上的原实例停用并保留，以便回退（参见 [运维手册 §4](docs/operations.md#4-upgrading-the-pieces-around-felis)）。

<details>
<summary>安装来源与受限网络环境下的安装</summary>
<br>

安装发布版时，二进制文件、全部镜像及 Velocity 插件均取自该版本由 CI 预构建的 release 附件，逐一校验 `SHA256SUMS` 后导入。主机无需安装 Docker、Gradle 或 Go，也无需访问 Docker Hub。若某个附件缺失或校验失败，仅该镜像回退为本机构建，并输出提示（参见 [故障排查 §15c](docs/troubleshooting.md)）。

也可将附件预先复制到主机，再通过 `FELIS_ARTIFACT_DIR=<绝对路径>` 安装，此时 Felis 自身的二进制、镜像与插件均从该目录读取。k3s 及其镜像、JRE、cloudflared、Velocity 与 Via 插件仍从 GitHub 和 PaperMC 下载；RHEL、Fedora、openSUSE Leap 等启用 SELinux 的主机还需从 rpm.rancher.io 安装 k3s-selinux；系统软件包来自发行版软件源。

因此，出站网络受限的主机须放行上述地址的 HTTPS 访问，或设置 `https_proxy`。preflight 会在修改主机之前逐一探测这些地址。目前暂不支持完全离线安装（地址清单参见 [运维手册 §1](docs/operations.md#1-supported-hosts)）。

</details>

## 从源码构建

本项目基于 Go 与 Node.js 开发：

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

本项目采用 [AGPL-3.0-only](LICENSE) 许可证。

### 协议注意事项

1. **衍生作品须采用 AGPL**：分发本项目副本或基于本项目的衍生软件时，须以 AGPL-3.0 开源，并保留原作者的版权声明与许可声明。
2. **网络服务同样须提供源码**（AGPL 第 13 条）：通过网络向他人提供经修改的 Felis 服务时，即使未分发任何二进制文件，也须向这些用户提供修改后的完整源码。这是 AGPL 与 GPL 唯一的实质区别；Felis 作为通过网络访问的托管平台，几乎所有部署场景都适用此条款。
3. **免责声明**：本项目按"原样"提供，作者不承担因使用本项目而产生的任何法律责任。

## 致谢

* [Kubernetes](https://kubernetes.io/)：容器编排引擎
* [K3s](https://k3s.io/)：轻量级 Kubernetes 发行版
* [Cloudflare Zero Trust](https://www.cloudflare.com/zero-trust/)：零信任安全基础设施
* [PostgreSQL](https://www.postgresql.org/)：数据持久化
* [React](https://react.dev/)：前端用户界面框架
* [Vite](https://vitejs.dev/)：前端构建工具
* [TailwindCSS](https://tailwindcss.com/)：CSS 框架
* [Bubble Tea](https://github.com/charmbracelet/bubbletea)：TUI 框架
* [Minecraft](https://www.minecraft.net/)：本项目服务的游戏
