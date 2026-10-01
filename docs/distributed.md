# A 主控与多机 worker

分布式模式默认关闭。A 运行唯一 Felis API/operator、k3s server、PostgreSQL、Registry、归档服务和系统服；Velocity 继续使用 A 的 systemd 服务。B、C 等节点只运行 k3s-agent/containerd、游戏 Pod 和 A 创建的维护 Job。节点必须与 A 同架构、同 k3s 版本，宿主机由管理员信任并维护。

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

节点批准后，管理员可在面板创建服务器时选择它，或在创建请求中传 `nodeName`。普通服主不能选节点或指定 PVC。失联节点拒绝新任务；operator 撤销该服务器 Service 的后端和 Ready 状态，Velocity 进入原有 fallback 流程，不换机。

## 停服迁移

先在面板停服并等待 `Stopped` 和游戏 Pod 退出。打开“停服迁移”，选择在线且已批准的目标 worker。面板从 CR 的持久化记录读取最新操作，刷新页面或重启 A 后仍可查进度。

```bash
# A 的 root 运维入口，区别于数据库 felis migrate：
felis server-migrate start --name survival --target-node c
felis server-migrate status --name survival
felis server-migrate retry --name survival --id <操作ID>
```

管理员 API：

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/api/v1/nodes` | 执行节点列表 |
| POST | `/api/v1/servers/{name}/migrations` | `{ "targetNode": "c" }`，返回操作及 ID |
| GET | `/api/v1/servers/{name}/migrations` | 最新迁移记录 |
| GET | `/api/v1/servers/{name}/migrations/{id}` | 当前操作进度 |
| POST | `/api/v1/servers/{name}/migrations/{id}/retry` | 重试失败阶段 |

阶段为 `backing_up` → `restoring` → `switching` → `succeeded`。锁是 `migration@<开始时间>`，不按临时维护锁的两分钟规则过期。失败保存阶段与原因，保持锁和停服状态；相同目标的重复请求返回已有操作，其他迁移被拒绝。恢复 Job 校验下载 SHA-256，恢复后逐文件读回校验；成功后才在一次乐观锁 CR 更新中切换节点、活动 PVC 和进度。停服 StatefulSet 会重建以使用新的 PVC，CR、Service、ClusterIP、域名及归属保持不变。

迁移成功仍不自动启动。检查目标世界后手动启动。记录中的 `sourcePVC` 保留，不被回收流程顺带删除；确认不再需要后由管理员显式清理。失败不要手动删除迁移注解或锁；排除源节点失联、目标磁盘不足、传输/校验错误后使用重试。已提交的切换不会自动回退到源世界。

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

当前本地验证使用一台已有 Felis 的 ARM64 CentOS Stream 9 VM，通过临时测试程序验证归档和迁移逻辑；`deploy/test-node-firewall.sh` 在独立网络命名空间中实测 resident-node、NodePort DNAT 和提前 ACCEPT 的防护，不改变其现有集群网络。三机网络验收仍是上线前必要步骤。A 继续是控制面和公网入口单点，首版没有主控 HA 或自动故障迁移。

相关上游说明：[k3s 跨公网组网](https://docs.k3s.io/networking/distributed-multicloud)、[限时 bootstrap token](https://docs.k3s.io/cli/token)、[NodeRestriction 标签](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#node-isolationrestriction)、[NetworkPolicy 的节点边界](https://kubernetes.io/docs/concepts/services-networking/network-policies/)。
