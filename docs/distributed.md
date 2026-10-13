# 分布式部署：主控、worker、故障切换与主控高可用

分布式模式默认关闭。主控 A 运行 Felis API/operator、k3s server、PostgreSQL、Registry、归档服务和系统服；Velocity 继续使用主控的 systemd 服务。B、C 等 worker 只运行 k3s-agent/containerd、游戏 Pod 和主控创建的维护 Job。节点必须与 A 同架构、同 k3s 版本，宿主机由管理员信任并维护。

worker 失联时，可将其上的世界按最新完好备份恢复到其他节点（见“故障切换”）。启用主控高可用后，可增加更多主控；控制面在承载它的主控失联时自动转移到其他主控（见“主控高可用”）。

## 面板管理

Owner 可在「平台设置 → 分布式」标签页的「节点操作」卡片中启用分布式部署、通过 SSH 接入 worker、批准已接入的 worker、更新节点、启用主控高可用、加入新主控，以及查询或重试失败任务。「执行节点」列表显示各节点的在线、批准、隔离及版本状态，并提供失联节点的故障切换入口和隔离主控的恢复入口；「故障切换」卡片设置自动故障切换与世界副本间隔。服务器的节点选择和停止后的世界迁移继续使用服务器管理页面。以下 CLI 流程保留为维护及恢复入口。

完整安装会安装 `felis-node-control.service`。面板 API 保持非 root 身份，仅挂载该服务的只读 Unix socket 目录；宿主机服务只接受固定的节点操作与主机维护操作（见 [运维手册第 9 节](operations.md#9-running-the-host-from-the-panel-go-tested-sh-tested)），不提供通用命令执行。SELinux 主机为专用 socket 目录与文件配置独立类型；API 通过文件权限及独占挂载访问该服务。当前策略同时允许容器域连接以 `unconfined_service_t` 运行的服务所监听的 Unix 流套接字，实际可达范围由文件标签和挂载范围限定；为该服务定义专用进程域以收紧此规则的工作，尚待在 SELinux 主机上验证。HTTP 接口仅限 Owner，并沿用敏感操作的身份复核与审计要求。SSH 私钥、bootstrap token 和宿主机 kubeconfig 不返回浏览器。

首次接入前，在 A 的 root SSH 配置中准备免交互认证并核验主机指纹。SSH 账户须为 root 或具有 `sudo -n` 权限；已接入 worker 的 SSH 别名须与节点名称一致，以便增加节点时更新所有对等防火墙。系统不会接受未知主机指纹，也不会重新安装已经加入集群的主机。安装中断后，若 worker 已加入集群，应改用批准操作。

启用部署前须停止全部游戏及维护任务。服务暂停 operator 后再次检查停止状态，备份数据库，并停止 k3s 保存 `db`、`token`、`tls` 快照后重新启动 k3s；快照保存在 `/var/lib/felis/cluster-backups`。随后复用当前二进制的安装器配置 WireGuard、主控身份与平台组件。期间面板可能暂时无法访问，宿主机任务继续执行。worker 批准复用 CLI 的镜像拉取、网络隔离及节点身份验收；失败时不解除隔离。手动 cordon 的节点不会自动解除 cordon。

任务状态及日志持久化在 `/var/lib/felis/node-control`，由 root 保护。节点操作的执行超时上限为 45 分钟，主机维护操作各有上限，中断后执行必要恢复动作；保留最近 100 条记录；单项日志文件限制为 4 MiB，面板读取最近 64 KiB。服务重启会将未完成任务标记为失败，恢复前应核对主机状态，不自动重复执行安装。配置了主机服务的 API 在节点操作，以及数据库恢复、整机重建、凭据轮换和更换域名等会中断控制面的主机维护操作期间，阻止启动、维护及迁移；主机服务不可用时这些操作返回明确错误，停止入口仍保留。

主机服务异常时，宿主机管理员可执行 `systemctl status felis-node-control`、`journalctl -u felis-node-control` 和 `systemctl restart felis-node-control`。首次升级至包含该功能的版本，需要通过完整安装流程安装服务及 socket 挂载；仅替换网页资源不能启用宿主机操作。

## 先准备 A

在维护窗口停服，使用现有数据库备份流程备份 PostgreSQL，并离线保存 k3s 状态、server token 和当前安装配置。SQLite k3s 的状态目录是 `/var/lib/rancher/k3s/server/db`；使用其他 datastore 时按对应备份流程操作。不要把这些文件复制到 worker。

记录 A 的现有节点名称，后续安装必须沿用。先把当前控制工作负载固定到 A，再启用 WireGuard；已有游戏 PVC 不迁移。

```bash
# 在 A，以 root 执行；替换节点名和所有固定节点地址。
A_NODE=existing-node-name
PEERS=192.0.2.10/32,192.0.2.11/32,192.0.2.12/32
k3s kubectl label node "$A_NODE" \
  felis.node-restriction.kubernetes.io/identity="$A_NODE" \
  felis.node-restriction.kubernetes.io/role=controller --overwrite

# 按实际部署名称执行，均使用受保护身份标签。
for d in felis-api felis-operator felis-postgres registry; do
  k3s kubectl -n felis patch deployment "$d" --type merge \
    -p "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":{\"felis.node-restriction.kubernetes.io/identity\":\"$A_NODE\"}}}}}"
done
```

用包含此功能的 Felis 安装器在 A 重跑安装：`FELIS_DISTRIBUTED=1`、`FELIS_NODE_EXTERNAL_IP=<A 固定公网 IP>`、`FELIS_PEER_CIDRS="$PEERS"`，保留现有安装参数。该步骤会启用 `wireguard-native`、`flannel-external-ip`、NodeRestriction 和独立 agent token，并安装归档服务、最小 RBAC 和宿主机隔离规则。WireGuard 更换需要停服维护窗口。已有 worker 的对等地址列表也必须提前更新。

仅推送 main 不会自动发布 release。还未使用包含这些变更的发布资产时，在新版源码目录中以 root 执行下面的命令，显式从 main 构建；其他原安装参数继续保留。只更新安装脚本、仍使用默认 release 通道，可能下载到不支持分布式命令的旧二进制。

```bash
FELIS_REF=main FELIS_DISTRIBUTED=1 \
  FELIS_NODE_EXTERNAL_IP=<A固定公网IP> \
  FELIS_PEER_CIDRS="$PEERS" bash deploy/bootstrap.sh
```

直接生成部署清单时，增加：

```bash
felis manifests --felis-image <现有逻辑镜像引用> \
  --distributed --controller-node "$A_NODE" \
  --egress-probe felis-api.felis.svc:443 \
  --velocity-cidr <A精确来源IP/32> \
  --archive-local-path <原archive.local_path> --backup-pvc felis-backups
```

其他已有参数继续保留。安装器也把 CoreDNS、local-path-provisioner 固定在 A；手动部署时同样给这些 Deployment 设置 A 的受保护节点选择器。手动部署须把同一随机密钥保存到 `felis` 和 `minecraft` 命名空间的 `felis-archive-key` Secret（字段 `key`，至少 32 字符）；只有 API、Reaper 和归档服务获得密钥，operator 不获得。归档 PVC、Registry 和数据库均留在 A。

## 接入 B，再接入 C

先在所有现有节点执行 `felis node firewall --peers "$PEERS" --controller-ip <A地址>` 更新完整对等地址列表；A 增加 `--controller`。只允许精确 `/32` 或 `/128` 地址，不能用整个节点/Pod 网段充当 Velocity 或 Registry 来源。固定公网节点之间允许 WireGuard UDP 51820–51821，k3s 6443 只允许已知对等节点。

```bash
# A：每台 worker 独立创建，默认 10 分钟；输出文件 root-only，令牌不打印。
felis node token --name b --ttl 10m --out /root/b.bootstrap
k3s kubectl -n felis get svc registry
# 通过可信 SSH/SCP 把该文件和同版本 felis 二进制交给 B。

# B：不需要 server token、管理员 kubeconfig、数据库或 Registry 写入凭据。
felis node join --name b --server https://<A公网IP>:6443 \
  --external-ip <B公网IP> --token-file /root/b.bootstrap \
  --registry-ip <Registry ClusterIP> --peers "$PEERS"

# A：SSH 使用既有主机密钥校验；目标账号须能 sudo -n。
felis node approve --name b --ssh-target <B的SSH别名> \
  --image <Felis逻辑镜像引用>
felis node list
```

安装 worker 不安装数据库、Velocity、API 或 operator，不删除本地卷或 node-password；重复安装会拒绝改名或更换集群。mirror 使用 Registry ClusterIP，关闭默认镜像源回退，保留原镜像引用。

批准前 worker 带 `NoSchedule` 隔离 taint，且没有受保护的 approved 标签。批准命令检查架构、版本、节点在线状态、WireGuard、宿主机隔离、kubelet 修改受保护标签被拒绝，删除指定镜像后执行真实 `crictl pull`。随后创建临时探针，检查跨节点 Service、控制服务的正向可达性和游戏标签 Pod 的拒绝路径。A 从宿主机连接每个测试 Service，读取后端实际观察到的源地址，只将属于 A 的精确地址写入游戏策略。任何检查失败都保留隔离状态。批准过程中会删除指定缓存镜像，须在该节点尚无游戏任务时执行。

Owner 可在面板“平台设置 → 多节点分布式部署”查看部署模式、节点在线及批准状态、接入流程，并进入服务器节点选择与迁移。部署模式可由面板「节点操作」或安装器配置；两种入口的节点批准执行同一套完整校验。

节点批准后，管理员可在面板创建服务器时选择它，或在创建请求中传 `nodeName`。普通服主不能选节点或指定 PVC。失联节点拒绝新任务；operator 撤销该服务器 Service 的后端和 Ready 状态，Velocity 进入原有 fallback 流程。世界只在 Owner 发起故障切换或已开启自动故障切换时转移到其他节点（见“故障切换”）。

## 更新节点

主控升级后，「执行节点」列表将 k3s 版本或 felis 构建与主控不一致的 worker 标记为“需要更新”。在「节点操作」选择“更新节点”，填写节点名称与 SSH 目标并确认维护条件。任务先确认该节点上没有游戏与维护任务，再通过 SSH 传输与主控相同的 felis 程序，在 worker 上以安装器的 worker-upgrade 模式（`felis node upgrade`）把 felis 与 k3s-agent 更新到主控的版本，重新安装对等防火墙，等待节点就绪后记录新版本。k3s-agent 会重启，因此该节点上的服务器须先停止。

## 停服迁移

先在面板停服并等待 `Stopped` 和游戏 Pod 退出。打开“停服迁移”，选择在线、已批准且未被隔离的目标节点；主控也可作为目标。面板从 CR 的持久化记录读取最新操作，刷新页面或重启 A 后仍可查进度。

```bash
# A 的 root 运维入口，区别于数据库 felis migrate：
felis server-migrate start --name survival --target-node c
felis server-migrate status --name survival
felis server-migrate retry --name survival --id <操作ID>
felis server-migrate abort --name survival --id <操作ID>
```

管理员 API：

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/api/v1/nodes` | 执行节点列表 |
| POST | `/api/v1/servers/{name}/migrations` | `{ "targetNode": "c" }`，返回操作及 ID |
| GET | `/api/v1/servers/{name}/migrations` | 最新迁移记录 |
| GET | `/api/v1/servers/{name}/migrations/{id}` | 当前操作进度 |
| POST | `/api/v1/servers/{name}/migrations/{id}/retry` | 重试失败阶段 |
| POST | `/api/v1/servers/{name}/migrations/{id}/abort` | 放弃尚未切换的失败迁移 |

阶段为 `backing_up` → `restoring` → `switching` → `succeeded`。锁是 `migration@<开始时间>`，不按临时维护锁的两分钟规则过期。失败保存阶段与原因，保持锁和停服状态；相同目标的重复请求返回已有操作，其他迁移被拒绝。恢复 Job 校验下载 SHA-256，恢复后逐文件读回校验；成功后才在一次乐观锁 CR 更新中切换节点、活动 PVC 和进度。停服 StatefulSet 会重建以使用新的 PVC，CR、Service、ClusterIP、域名及归属保持不变。

迁移成功仍不自动启动。检查目标世界后手动启动。记录中的 `sourcePVC` 保留，不被回收流程顺带删除；确认不再需要后由管理员显式清理。失败不要手动删除迁移注解或锁；排除源节点失联、目标磁盘不足、传输/校验错误后使用重试。已提交的切换不会自动回退到源世界。

目标节点永久不可用时，在迁移对话框中使用“放弃迁移”。放弃仅适用于失败且尚未切换的操作：服务器留在源节点并恢复可用，目标节点上未完成的副本被删除，迁移前的安全备份保留为恢复点；目标节点无需在线。

## 故障切换

worker 失联后，其上的游戏服务器无法运行。故障切换使用每台服务器最新的完好备份，在另一个在线节点上恢复这些世界。代价是该备份之后的全部变化；面板在确认前逐台列出将使用的备份时间与大小。

### 手动故障切换

「执行节点」列表为失联节点显示失联时长，并向 Owner 提供“故障切换”按钮。对话框列出该节点上的用户服务器：每台服务器将恢复的备份、切换后自动启动还是保持停止（沿用切换前的期望状态）；没有完好备份的服务器留在原节点。Owner 选择目标节点（在线、可调度的已批准 worker 或主控），输入节点名称确认，并完成敏感操作的身份复核。

开始后，每台服务器记录一项 `kind` 为 `failover` 的迁移操作，持有世界锁，期望状态改为停止。节点操作服务随即隔离失联节点：设为不可调度，撤销批准标签，并加上 Kubernetes 的 `node.kubernetes.io/out-of-service=nodeshutdown:NoExecute` taint，使 Kubernetes 不等待其 kubelet 即释放该节点上的 Pod 与卷，仍记录在该节点上的 Pod 随后被直接删除。隔离任务未能启动时，felis-api 每两分钟在后台重试；隔离完成前不恢复任何世界。阶段依次为 `fencing` → `restoring` → `switching` → `succeeded`：等待隔离完成并删除该服务器在失联节点上未完成的维护 Job，在目标节点创建新卷，恢复并逐文件读回校验备份，最后在一次 CR 更新中切换节点与活动卷，并恢复原期望状态。原世界卷保留在失联节点的磁盘上。

失联节点恢复上线后保持隔离：其上的 Pod 已被删除，节点不再承载世界。worker 须在「节点操作」中执行“批准已有 worker”，重新通过完整验收后才解除隔离；主控（仅在启用主控高可用时可被隔离）在「执行节点」列表中使用“恢复使用”解除隔离并恢复调度。旧世界卷不会自动清理。

### 自动故障切换与世界副本

「平台设置 → 故障切换」卡片保存以下设置：

| 设置 | 取值 | 默认 |
| --- | --- | --- |
| 自动故障切换 | 开或关 | 关 |
| 失联多久后自动切换 | 10–1440 分钟 | 15 |
| 世界副本间隔 | 0（关闭）或 15–1440 分钟 | 60 |

开启自动故障切换后，felis-api 每 30 秒检查一次：仅当恰好一个节点失联、该节点尚未隔离且至少一个主控在线，并且失联时间超过设定值时，才把该节点上的世界切换到负载最低的在线节点（用户服务器最少者优先，相同时优先主控）。多个节点同时失联更可能是主控自身的网络故障，此时仍等待 Owner 处理。自动切换失败的服务器保持停止，交由 Owner 处理，系统不自动重试。

世界副本缩小故障切换丢失的进度：运行在 worker 上（启用主控高可用后也包括主控上）的服务器每隔设定间隔做一次不停服备份，记录原因为 `replica`。副本采用与手动备份相同的不停服流程（RCON `save-off`、`save-all flush`，在同一节点复制世界后立即 `save-on`），因此要求服务器的控制台可用。系统每次最多启动一项副本，并在其他备份或恢复 Job 运行时等待；每台服务器只保留最新 2 份副本，保留期 48 小时；副本与 Owner 的手动备份和定时还原点分开计数，互不挤占。

API（仅 Owner）：

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/api/v1/nodes/{node}/failover` | 故障切换计划：失联状态、各服务器将恢复的备份、可用目标 |
| POST | `/api/v1/nodes/{node}/failover` | `{ "targetNode": "c", "confirm": "<节点名>" }`，开始故障切换并请求隔离 |
| GET | `/api/v1/settings/failover` | 读取故障切换设置 |
| PUT | `/api/v1/settings/failover` | 保存故障切换设置（按 revision 比较后写入） |

未启用主控高可用时，主控失联即控制面整体不可用，主控上的世界无法故障切换。

## 主控高可用

主控高可用把集群状态迁到 k3s 嵌入式 etcd，把控制面数据迁到 Longhorn 复制卷，使控制面可在多台主控之间转移。

### 架构

- **集群状态**：k3s 以嵌入式 etcd 运行。etcd 需要多数成员在线，因此应部署三台或更多奇数台主控；三台主控可容忍一台故障，两台主控在任一台失联时即失去多数。主控之间应为同一地域的低延迟网络。
- **控制面数据**：PostgreSQL、上传文件、Registry、归档及登录空间与正式大厅的世界存放在 `felis-replicated` StorageClass 的 Longhorn 卷上（3 副本，回收策略 Retain），卷名为原名加 `-ha` 后缀。Longhorn 的组件与副本只放在主控上。
- **控制面工作负载**：felis-api、operator、PostgreSQL、Registry、归档服务与回收任务只调度到主控，并随 PostgreSQL 运行在同一台主控上。该主控失联后，这些 Pod 在约 30 秒的容忍期后被驱逐；Longhorn 删除失联节点上的 Pod 并把复制卷挂载到另一台主控，控制面在那里重新启动。期间面板短时不可用。
- **不重复执行**：felis-api 的后台循环由租约 `felis-api-loops` 保护，operator 由租约 `felis-operator` 保护；新的持有者只在旧持有者停止续约后接管。
- **宿主机任务**：数据库备份、异地副本、更新检查和构建工具镜像同步等 systemd 任务只在“宿主机主节点”执行，即当前运行 PostgreSQL Pod 的主控；`felis ha leader` 判定本机是否为主节点。watchdog 在主节点上运行，主节点不可达时也在其他主控上运行，以便报告故障。
- **入口**：每台主控都安装 Velocity。可选的 VRRP 浮动地址（keepalived）在主控之间漂移，并优先停留在宿主机主节点上；该地址要求各主控处于同一二层网络。主控分布在不同网络时，改用 DNS 或云服务商的浮动地址指向各主控。
- **系统服**：登录空间与正式大厅不再固定在某台主控上，可在任一主控上运行。
- **用户世界**：主控本地卷上的用户世界在其主控失联时，可像 worker 上的世界一样故障切换（须另有主控在线）。

### 启用

在「节点操作」选择“启用主控高可用”并确认维护条件。可同时填写浮动地址、网卡与 VRRP 路由 ID，分别对应下表的 `FELIS_HA_VIP`、`FELIS_HA_VIP_INTERFACE` 与 `FELIS_HA_VRID`；网卡与路由 ID 留空时由安装脚本确定。任务依次执行：

1. 备份数据库（`pre-migrate` 标签）。
2. 保存集群状态快照（`pre-ha`，位于 `/var/lib/felis/cluster-backups`）。
3. 以 `FELIS_CONTROLLER_HA=1` 重新运行安装器：k3s 以 `cluster-init` 迁移到嵌入式 etcd；安装 open-iscsi 与 Longhorn（版本由 `FELIS_LONGHORN_VERSION` 指定，默认 `v1.8.1`，清单从 GitHub 下载）；执行 `felis ha migrate-storage`，停止系统服与控制面，把数据库目录、上传文件、Registry、归档和系统服世界逐一复制到对应的复制卷，原卷保留；最后应用高可用部署清单，控制面在复制卷上重新启动。

迁移期间登录空间与正式大厅暂停，面板短时不可访问；其他游戏服务器继续运行。`felis ha migrate-storage` 记录每个卷的进度，中断后重跑只复制尚未完成的卷。设置写入 `/etc/felis/ha.env`，此后重新运行安装器或 `felis update` 均保留高可用模式。

命令行入口为在主控上以 root 重跑安装器，并保留原有安装参数：

```bash
FELIS_DISTRIBUTED=1 FELIS_CONTROLLER_HA=1 \
  FELIS_NODE_EXTERNAL_IP=<主控固定IP> \
  FELIS_HA_VIP=192.0.2.50/24 bash deploy/bootstrap.sh
```

| 变量 | 含义 |
| --- | --- |
| `FELIS_CONTROLLER_HA=1` | 启用主控高可用，须同时为分布式模式 |
| `FELIS_HA_VIP` | 可选，IPv4 浮动地址及前缀，如 `192.0.2.50/24` |
| `FELIS_HA_VIP_INTERFACE` | 浮动地址所在网卡，默认取承载节点地址的网卡 |
| `FELIS_HA_VRID` | VRRP 虚拟路由 ID，1–255，默认 51 |
| `FELIS_LONGHORN_VERSION` | Longhorn 版本标签，默认 `v1.8.1` |

启用后无法通过面板关闭。需要回退时，使用第 1、2 步保存的数据库备份与集群快照手动恢复。

### 加入新主控

在「节点操作」选择“加入新主控”，填写节点名称、SSH 目标和新主控的固定 IP，可选填写额外对等地址。新主控须与现有主控同架构，且尚未安装 k3s；SSH 要求与接入 worker 相同。任务依次执行：

1. 用包含新主控地址的对等列表更新所有现有节点的防火墙。
2. 通过 SSH 标准输入向新主控传输集群的 server token、agent token、本主控 `/etc/felis` 的打包文件以及当前 felis 程序。这些内容不经过 API，也不写入命令行、任务记录或日志。
3. 在新主控上执行 `felis node join-controller`，即安装器的 controller-join 模式：以与现有主控相同的 k3s 版本加入嵌入式 etcd，安装 open-iscsi、节点操作服务、Velocity，以及数据库备份、异地副本、更新检查与 watchdog 定时任务；配置了浮动地址时同时安装 keepalived。
4. 等待节点就绪后，为其加上主控角色、节点身份及 Longhorn 磁盘标签，使 Longhorn 可在其上放置副本。

`/etc/felis` 中包含数据库凭据、转发密钥、邮件中继设置与 VRRP 口令。每台主控都持有完整的平台机密，应与 A 同等对待。

面板暂不提供移除主控的操作。永久下线一台主控时，先确认其余主控仍构成 etcd 多数，再按 k3s 文档移除其 etcd 成员并删除节点。

### 主控失联与恢复

承载控制面的主控失联后，控制面按上文自动转移，无需人工操作。该主控本地卷上的用户世界需要故障切换（手动或自动），规则与 worker 相同。失联主控被隔离后重新上线时，先确认它已重新加入 etcd、Longhorn 副本开始重建，再在「执行节点」列表中对其使用“恢复使用”。节点操作服务拒绝隔离本机，也拒绝隔离最后一台在线主控。

## 隔离与验收

游戏进程沿用非 root、禁止提权、drop ALL、无 SA token 和宿主机命名空间的限制。维护 Job 只挂一个世界 PVC：传输 Job 仅访问归档服务和 DNS，文件/导出 Job 仅额外访问既有 API 传输入口。归档服务无数据库配置和 Kubernetes 身份；只有完整归档原子落盘后 A 才记录成功，现有 tarLocal 路径、保留与 offsite 流程继续使用。

宿主机 INPUT/FORWARD 规则封闭 resident-node 路径，raw PREROUTING 在 DNAT 前封闭 worker NodePort；A 的受信控制 Pod 精确地址可以访问 apiserver。raw 规则也封闭游戏 Pod 向宿主机发起的新连接，避免 kube-router 的提前 ACCEPT 绕过 filter 规则；Velocity/RCON 的已建立连接回复保留。规则由 systemd 安装，控制 Pod 地址定期更新。节点地址、防火墙或 CNI 配置变动后，先停服并重新执行批准检查，再运行不可信代码。游戏 egress gate 同时检查允许的 DNS TCP 路径和拒绝路径，分布式模式超时拒绝启动。

必须在 A/B/C 三台 Linux 机器完成上线验收，不能用单机单元测试代替：

- Velocity `ClusterIP:25565` 跨节点连接及实际源地址、RCON、休眠唤醒、缓存清除后镜像拉取。
- B 上备份恢复，B→C 迁移，确认 Service IP/归属不变、源 PVC 保留、目标仍停服。
- 迁移期间唤醒、文件写入、导出、回收及重复请求互斥；A 重启继续协调。
- 源节点失联、传输中断、目标/归档磁盘满和读回校验失败，确认源世界可恢复。
- 游戏 Pod 对各服、主控、Registry、宿主机端口、kubelet、元数据的请求均拒绝；各拒绝目标在受信正向探针中确实可达。
- 过期、重放、跨服和错误操作的归档令牌拒绝；kubelet 伪造受保护标签拒绝。
- 故障切换：关闭 B 后按面板计划恢复到 C，确认隔离生效、丢失进度与计划一致、恢复后自动启动状态正确；B 重新上线后旧 Pod 被停止，重新批准前不承载世界。自动故障切换只在单节点失联时触发。
- 世界副本：运行中的服务器按间隔生成副本，期间玩家仅感知保存暂停，副本数量与保留期符合设定。
- 主控高可用：三台主控下关闭承载控制面的主控，确认控制面在另一台主控上恢复、后台循环与 operator 不重复执行、宿主机定时任务随 PostgreSQL 转移、浮动地址漂移；失联主控重新上线后恢复使用；从单主控启用高可用的迁移在真实数据上完成且原卷保留。

当前本地验证使用一台已有 Felis 的 ARM64 CentOS Stream 9 VM，通过临时测试程序验证归档和迁移逻辑；`deploy/test-node-firewall.sh` 在独立网络命名空间中实测 resident-node、NodePort DNAT 和提前 ACCEPT 的防护，不改变其现有集群网络。三机网络验收仍是上线前必要步骤。故障切换与主控高可用的逻辑由单元测试和安装脚本测试覆盖，尚未在多台真实机器上完成上述验收；Longhorn 的默认版本 `v1.8.1` 亦待在目标系统上确认。未启用主控高可用时，A 仍是控制面和公网入口的单点。

相关上游说明：[k3s 跨公网组网](https://docs.k3s.io/networking/distributed-multicloud)、[限时 bootstrap token](https://docs.k3s.io/cli/token)、[NodeRestriction 标签](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#node-isolationrestriction)、[NetworkPolicy 的节点边界](https://kubernetes.io/docs/concepts/services-networking/network-policies/)。
