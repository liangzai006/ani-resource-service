# 实例后端运行与恢复

行为合同见 [v1.5 规格](../specs/instance-mvp.md)，实际执行结果只看 [当前状态](../execution/status.md#gpu-container-instance-mvp-v15)。本说明提供配置和恢复步骤，不代表共享集群已部署或真实 GPU 已验收。

当前配置、已有卷及显式 VPC 接入使用[增量合同](../specs/instance-configuration-network.md)，实际结果见[配置与网络执行状态](../execution/status.md#instance-configuration-and-network-2026-10-19)。只支持当前实例配置合同2；此前从旧配置合同切换的清理前置不适用于本次算力目录改名。本次保留迁移前实例、原账及运行对象，维护时关闭新的受理与规格写入，按[目录迁移手册](compute-presets-migration.md)停止旧SQL/RPC进程、切换匹配版本及验证恢复；不部署旧配置reader。

Restart 与 borrowed PVC 挂卸遵循[权威方案 v2.0](../plans/container-instance-restart-pvc-20261028.md)，本批结果统一见[当前执行状态](../execution/status.md#container-restart-pvc-20261028)。本批按用户新增授权，经重任务机 `root@192.168.25.40` 的内网 SSH `kubercloud@10.10.1.66` 管理原集群，主机及集群身份绑定见当前执行状态；下文旧 CPU/隔离实验目录是历史恢复参考。本文不表示新版本已部署或 LIVE 已通过。

## 源码与验证

本批 Resource 验证优先复用 `ssh fedora` 的暖缓存，发生 I/O 阻塞后使用用户新增重任务机；该机也承接 Gov 和只读探针编译，使用任务独占源码、缓存和 TMPDIR，不转本地执行。实际主机、互斥锁、资源预算、固定基线及快照见当前状态。按 [远程执行约定](../remote-execution.md) 同步带文件哈希的源码，不同步私钥、DSN 或 kubeconfig。

Resource 的 [实例 API 生成器](../../scripts/generate-instance-api) 使用固定 Buf/Go 插件及正式 Acc proto；sqlc 使用根 `sqlc.yaml`。实例定向 PG 验证入口为 [instance-integration](../../scripts/instance-integration)，只创建、迁移和清理带任务标签的独占 PostgreSQL；`INSTANCE_TEST_RUN` 可限制相关场景。提交前仍执行仓库要求的 `make verify`；实例真实 PG 门禁单独执行，不能由普通单元测试代替。

终端验证协议单独发布为 `github.com/zhangzhe-ctrl/ani-resource-service/api/instance/terminal`，只依赖 protobuf/gRPC，SG 不依赖 Resource 根模块及其 Kubernetes SDK。正式版本固定在各消费者 `go.mod`；升级先发布 provider，核验下载来源 SHA/sums，再用 `GOWORK=off` 构建消费者。早期受控联调使用任务私有 go.work；2026-10-13 发布前已改为正式下载并验证 Resource/SG 独立构建，不提交兄弟目录 replace。

## 环境与启动

写入前核对实际 API、cluster UID、节点地址与工作负载；只能使用本任务确认的集群与实验租户。冻结真实 Acc group/profile/version、Queue、受支持的 Volcano/HAMi 基线和镜像 digest。镜像须提供长驻入口、指定 shell、UID/GID 与可写的冻结工作目录；不在启动时修理镜像或临时放宽 Acc 基线。

环境预先提供保存 Namespace 和普通 SA。Namespace 兼容现有 Network 标签 `network.ani.io/tenant-id=<完整资源租户 UUID>`、`network.ani.io/managed-by=ani-network-service`；缺省 Namespace 取当前 Network 配置前缀加完整 UUID。工作盘 StorageClass 必须采用 Delete reclaim policy。服务使用已有受管 Kubernetes 身份，实例 SA 不挂载 API token；本任务不是不互信租户隔离认证。

Resource 的 Kubernetes 身份还需要 PV 的 `get/list` 只读权限，以记录 PVC 的实际绑定和核对回收；实例普通 SA 不需要这项权限。删除只针对保存 UID 的专属 PVC，不直接删除 PV。PVC 消失后仍存在的原 PV、Retain 回收策略或归属不符都不能完成工作盘清理；PV 绑定按 claimRef 的完整 UID/Namespace/name 及保存的 PV UID 核验。

显式 VPC 的 controller-managed 释放路径还需要 Resource 服务身份在 `kcn-system` 对 `networking.kubercloud.com/nics` 的 `list` 权限，以确认系统网卡已消失。这项读取不在 14 类 full observer 的 LIST/WATCH 检查内，应使用实际运行服务账号单独验证；缺少时会保持 `PROVIDER_UNAVAILABLE` 并等待释放。仅向 Resource SA 绑定所需命名空间 Role，不向实例工作负载授予该权限。GET/UID 检查不能代替外部防同名替换保护；本批 A36 的用户暂缓边界见下文，不借此放松平台内部 guard。

Resource 使用 `ANI_NETWORK_MODE=governance`，保留现有业务 mTLS listener 与服务器 DNS SAN `ani-network-service`，启用现有 Image runtime 后才可启用实例。私有 `ANI_INSTANCE_CONFIG_FILE` 是严格 JSON，字段以 [instanceSettings](../../cmd/ani-resource-service/instance.go) 为准：

```json
{
  "DatabaseDSNFile": "/managed/instance-runtime.dsn",
  "SigningKeyFile": "/managed/instance-ed25519.key",
  "QuotaAddress": "governance.internal:9445",
  "QuotaCAFile": "/managed/quota-ca.pem",
  "QuotaCertFile": "/managed/resource-client.pem",
  "QuotaKeyFile": "/managed/resource-client.key",
  "QuotaServerName": "ani-governance",
  "TerminalAddress": ":9446",
  "Service": {
    "ClusterID": "<Acc 的真实 cluster UUID>",
    "ServiceAccount": "workload",
    "StorageClass": "ani-block",
    "WorkspaceSize": "10Gi",
    "Shell": "/bin/bash",
    "UID": 1000,
    "GID": 1000,
    "NetworkV2Writes": true
  }
}
```

示例中的 UUID、地址与私有路径必须替换；未配置默认拒绝。NamespacePrefix 始终从当前 Network 配置读入，不由该 JSON 改写。SigningKeyFile 为 64 字节原始 Ed25519 私钥，不放入源码、普通归档或日志。

显式 VPC 创建还要求私有 JSON 的 `Service.NetworkV2Writes=true`；缺失时默认关闭，ResolveCreateSpec 返回 `INSTANCE_NETWORK_NOT_ENABLED`。该项从私有配置读取，修改后正常滚动 Resource 载入。

三个能力闸门 `ANI_INSTANCE_NEW_INTENTS_ENABLED`、`ANI_INSTANCE_RESTART_ENABLED`、`ANI_INSTANCE_PVC_MUTATIONS_ENABLED` 均默认 `false`，仅精确值 `true` 启用；以[实际装配](../../cmd/ani-resource-service/instance.go)为准，不在私有 JSON 中另设来源。维护闸门关闭时拒绝新 CREATE、Start/Stop/Restart/PVC 命令和新 Storage 删除 ticket；当前授权后的原键重放、查询、已受理恢复及合法 `AcceptDelete` 不被这三个开关阻断。Storage 删除缺少 guard 不回退直删；[恢复 worker](../../internal/server/storage_worker.go)未就绪或其数据库健康失败时，应保持新入口关闭，不能只看普通服务健康就开放挂卸。

Resource 不再建立 ACC release 连接或调用 `ObserveRelease`。旧私有配置中的 `ReleaseAddress/ReleaseCAFile/ReleaseCertFile/ReleaseKeyFile/ReleaseServerName` 仅为过渡解码兼容保留，不读取文件、不校验或连接地址；新配置应移除它们。`SigningKeyFile` 和 Pod owner 签名仍用于实际运行投影，必须保留；Gov Quota mTLS 继续校验受管 CA、客户端身份、TLS 1.3 和精确 DNS SAN。

创建 Notebook 的外部调用前，同一实例/lease 事务先持久 `ExternalComputeStarted` 与完整已见 Pod 历史；进程重启、Stop/Start 和并发删除均保留原 UID。普通 Notebook 删除采用 UID/RV 条件和 Foreground GC，不强制删除 Pod。明确 DELETE 后依次清理 Notebook/StatefulSet/Pod、专属 PVC/原 PV、网络和借用引用，再在同一事务中保存最终 owner 清理事实和稳定 outbox。未知 CREATE 保持阻塞；借用卷仅解除引用，原数据保留。

Resource 与 ModelDev 共用 Gov 现有 `ReportResourceReleased` 四字段协议；ACC 摘要是旧 Resource 本地门槛，从未传给 Gov。清理事实提交后，独立投递循环重试 Gov，不依赖再次观察 Kubernetes 或 ACC。匹配 APPLIED/ALREADY_APPLIED 才确认本地 outbox；Gov 退额与 ACC 设备容量恢复分别核验。CPU 实例没有 GPU 释放事件，普通 Stop/Start 不退额。

GPU RuntimeClass 必须由 ACC 受管配置写入唯一 `GpuExecution.runtime_class_name`；Res 没有第二个配置来源。只有历史004从F256/百分比迁往绝对显存合同的首次切换才要求先结束不兼容GPU活动并确认outbox；不得将旧F256状态交给当前reader。本次目录迁移校验已应用004且不重放，保留当前合同的GPU活动、冻结执行和原账，不要求重新发布ACC Profile。

规格唯一来源为 Resource compute 的 `compute.presets`，配置中的 `Presets` 已删除，严格配置解码会拒绝该旧字段。规格通过 Gov 的 ComputePresetService 平台管理入口创建，CPU 创建不依赖 ACC；GPU 新建/替换引用必须使用可信 ACC 目录的完整事实。独立算力配置与原地迁移见[算力目录合同](../specs/compute-presets.md)及[维护/逆向恢复手册](compute-presets-migration.md)。

迁移由独立 owner 身份显式运行 `ANI_INSTANCE_OWNER_DSN_FILE=<私有文件> ANI_INSTANCE_RUNTIME_ROLE=<受限角色> ./ani-resource-service -instance-migrate`，按 instance 阶段再进入独立 compute 阶段。历史001..007只验证checksum，不重放004；本次追加[008_restart_pvc.sql](../../migrations/instance/008_restart_pvc.sql)至schema 8，Compute元数据检查同步匹配完整历史，保留原表、实例、GPU活动和原账。迁移必须先完成下文旧进程排空与旧直删盘点；缺失或冲突的借用引用会失败，不能用当前同名 PVC 猜填旧 UID。前文旧F256切换前置不适用于本次增量。应用只用受限角色，不在启动时迁移，不用超级用户。启动方式沿用现有 Resource 的 `-conf`、Image及`ANI_INSTANCE_CONFIG_FILE`，并提供显式`ANI_COMPUTE_CONFIG_FILE`；两者使用同一受管实例/规格数据库及cluster。

Governance 配置 `ANI_INSTANCE_ADDR/CA/CERT/KEY`（现有 Resource DNS-only mTLS），登记 INSTANCE 能力、当前动作 API 和角色权限。Restart/PVC 只使用 Governance 仓的 `scripts/ops/sql/register-instance-restart-pvc-permissions.sql` 做本批四个 API、三个 permission 及目标角色/INSTANCE 关联的增量登记：默认 dry-run，核对差异后明确 apply，再刷新各 Gov 策略实例；不以全局同步删除其他模块权限。精确 Resource RPC 见[调用登记](../specs/governance-calls.md)。退额 listener 的 `ANI_QUOTA_RESOURCE_DNS_SAN=<Resource 客户端精确 DNS SAN>` 固定映射 `ani-resource-service`，不能与 Inference 名称复用。

Acc 在独立恢复点之后由迁移 owner 显式执行 `0003_resource_instance_owner.sql`，runtime 要求 schema 3；旧 schema 2 不接受 Resource owner。迁移入口/CI 准备已登记 0003，应用不自动迁移。`resource_owner_public_key_file` 指向本实例签名密钥对应的受管 Base64 公钥，独立于 Inference 公钥。实际 Pod 的签名与投影关联后才显示租户 Binding；缺钥或签名不匹配仍计实际容量。Gov 的原账同步产生 ENDED，不能据此判断设备物理空闲。恢复旧 Acc 二进制还需匹配的数据库恢复点，不能手改版本记录。

Gov → SG 配置 `ANI_INSTANCE_SG_ADDR/SERVER_NAME/CA_FILE/CERT_FILE/KEY_FILE`，客户端证书唯一 URI 为 `spiffe://ani.internal/service/ani-governance`。SG 启用 `RESOURCE_INSTANCE_ADDR/SERVER_NAME/CA_FILE/CERT_FILE/KEY_FILE` 和 `RESOURCE_INSTANCE_CLUSTER_ID`；它连接 Resource 的同进程终端验证 listener，证书唯一 URI 为 `spiffe://ani.internal/service/ani-session-gateway`。SG 控制口同时要求 `INSTANCE_CONTROL_CA_FILE/CERT_FILE/KEY_FILE` 和 TLS 1.3。两个 listener 均按方法和服务身份授权，不能把 Gov 业务委托证书当作 SG 终端权限。

SG 继续使用现有 Redis 持久票据、票据密钥、Origin/WS 及 Kubernetes exec 配置。票据冻结 Namespace、业务版本、Notebook/Pod UID、容器和 shell；签发与连接均复核 Resource。Pod 中 `ANI_INSTANCE_POD_UID` 来自 downward API，exec 再校验它，以拒绝查询后出现的同名替换 Pod。前端由专人接手；提供原 DTO/API，不另外创建页面工程。

Fedora 隔离 SG 可以设置绝对路径 `KUBECONFIG`，使用既有 CA 校验的 HTTPS API 与实验 Namespace 内的普通 SA Token；禁止 insecure、ExecProvider/AuthProvider。未设置则继续使用 InClusterConfig。只需要本 Namespace 的 Pods get/list、pods/exec create。显式配置 `INSTANCE_CONTROL_CA_FILE/CERT_FILE/KEY_FILE` 时，即使没有启用实例 validator，控制口也启用 mTLS；三个字段必须完整。

Notebook 1.10 的标签/注解放在 Notebook 顶层 metadata，`spec.template` 只有 spec。Resource 按保存 Notebook UID 核对同名 StatefulSet 的 controller ownerRef，再核对 Pod 的 STS UID；没有 STS 标签不能证明计算已清理。实际来源限制见 [Notebook 投影核验](../execution/records/INSTANCE-MVP-V15/notebook-v1.10-projection.md)。普通 Stop/Start 使用原生停止注解，不修改控制器生成的子对象。

## CPU 实例与前端联调

实例内外统一为 `instance_kind=container` 与来自数据库规格的 `compute_type=cpu|gpu`。无空类型推断，不接受 `instance_type` 或外层 `preset_version`。

先经平台入口创建规格，再使用返回的 UUID `preset_id` 创建租户实例：

```json
{"data":{"code":"cpu-1c-2g","name":"CPU 1核 / 2Gi","compute_type":"cpu","cpu_millis":"1000","memory_mib":"2048","enabled":true}}
```

```json
{"data":{"idempotency_key":"<新UUID>","name":"cpu-dev","image_id":"<已登记镜像ID>","image_scope":"platform","preset_id":"<规格UUID>"}}
```

CPU 不带 GPU request/plan/charges。CPU/内存精确转换为 Quantity 并冻结；规格修改、禁用、删除都不改变已经持久受理的命令或已有实例。调度原始缺失与实际 `NONE` 分别保留。已受理重试使用原命令；Start/Stop/Delete 继续使用新的幂等键和最新 `expected_version`。

普通列表使用 `page`/`pageSize`、`items`/`total`。静态详情返回冻结 CPU/内存、规格名称/编码、实际调度、安全 GPU 摘要及真实创建时间，不查询当前规格、ACC 或 Kubernetes。CPU `quota_returned=true` 表示无待退 GPU 配额。

Exec 返回一次性 `ws_url`，通过 SG 的 `ani.terminal.v1` WebSocket 子协议传输终端帧；不要保存票据到普通日志或在停止/重启后复用旧票据。停止后保留 `/workspace`，最终删除会清除专属工作目录。CPU 不展示“仍占 GPU 配额”。

以下 2026-10-15 隔离环境记录属于旧版运行证据，不是新合同启动配置；升级必须先完成本批迁移和配置切换。旧运行根：`/home/chabking/workspace/ani-resource-service-runs/instance-cpu-mvp-20261015`。`private/cpu-runtime.sh start|stop|status [resource|gov|sg]` 管理任务专用 Docker 服务，SSH 退出后继续运行；不启动 Acc、不改共享部署。原持久 PG/Redis 与私有身份材料保留，任务容器按 label 核对归属。配置、二进制、Image receipt 只在该任务目录，登录密码/token 不进入仓库。

前端开发机可建立专用隧道：

```bash
ssh -N -o ExitOnForwardFailure=yes \
  -L 18080:127.0.0.1:52673 -L 18081:127.0.0.1:47157 fedora
```

Gov 地址为 `http://127.0.0.1:18080`，SG 票据的公开地址为 `ws://127.0.0.1:18081/api/v1/realtime`。前端开发服务器代理 `/api` 到 Gov；SG 已允许 `http://localhost:5173`、`http://127.0.0.1:5173` 与实验 driver Origin `http://instance-lab`。其他 Origin 需在该任务私有配置中明确登记并重启 SG，不放开通配。

手动刷新 SG token 并只重启该任务 SG：

```bash
ssh fedora 'python3 /home/chabking/workspace/ani-resource-service-runs/instance-cpu-mvp-20261015/private/refresh-session-token.py'
```

脚本核对 cluster/Namespace/SA UID，只申请原 namespaced 权限的 token，以 API 返回的到期时间为准；不安装定时任务。TLS 证书到期前使用保存 CA/CSR 续签。普通用户按当前 Gov 正式合同完成租户密码登录，沿用 INSTANCE/IMAGE 当前角色和套餐授权。实验租户名 `instance-lab-v15`、账号 `instance-lab-user`；`POST /api/v1/auth/password/login` 接受 `tenant_name`、`username`、`password`。密码位于 Fedora 原任务根 `private/instance-lab/tenant.password`，只通过受管私有渠道读取；无需管理员账号。实际镜像 ID、运行结果、有效期和精确快照统一见 [CPU 当前状态](../execution/status.md#cpu-instance-backend-20261015)。

## 本任务隔离实验的恢复

固定远端根为 `/home/chabking/workspace/ani-resource-service-runs/instance-mvp-v15-20261013`。私有配置与运行入口在 `private/instance-lab/` 和 `private/lab-*.{py,sh}`；ID、源码哈希与实际结果见 [真实实验记录](../execution/records/INSTANCE-MVP-V15/live-lab.md)。PG/Redis 和 Harbor 项目保留，四个服务已停止。不要在恢复时删除数据库卷、角色密钥、原账或恢复标记。

1. 在持续 SSH 连接中运行 `bash <根>/private/lab-session.sh`。Fedora 最后一个 SSH 退出会停止 user manager；该入口仅保持一个 20 分钟任务会话，退出时停止本任务四个 unit，不修改账号 linger 策略。结束可创建 `private/instance-lab/session.stop`；再次运行前移除这个任务停止标记。
2. 在另一条 SSH 连接中运行 `python3 <根>/private/lab-prepare.py` 刷新实验 Namespace 的 SG Token，再运行 `python3 <根>/private/lab-configure.py`。Token 有效期 7200 秒；任务 TLS 证书为 30 天，过期前使用保存 CA/CSR 续签，不能禁用校验。Resource/Acc 复用保存的受管集群凭据，SG 只用 namespaced SA。
3. 运行 `bash <根>/private/lab-services.sh start all`；查询或停止使用 `status all` / `stop all`。unit 名为 `instance-mvp-v15-lab-{acc,gov,resource,sg}`，全部监听随机冻结 loopback 端口，并分别限制 CPU/内存；日志仍只保留在私有目录。不能重用共享服务端口或停止其他 unit。
4. `private/lab-api.py captcha` 返回正常登录挑战，使用图中答案调用 `login <答案>` 或 `tenant-login <答案>`；密码与 token 由驱动读取/保存到私有文件。`query` 验证当前租户权限→Gov→Resource 的真实空列表；`register` 重放原实验集群/池登记键，不能生成新键来掩盖未知结果。驱动是 API 客户端，不写产品操作或完成状态 SQL。

五域分别使用 `lab_{gov,acc,net,image,inst}_{owner,runtime}`，每个角色有独立凭据；runtime 非超级用户、非 owner、没有 owner 角色成员资格。新实验首次准备由 `lab-db-prepare.sh` 调用受控迁移及官方 Gov CLI；已有数据库凭据修复/配置恢复使用 `lab-db-credentials.py`，不得把 owner DSN 配给应用。Harbor `init-platform` 已由生产 Image 生命周期完成，只建立本任务 InstallationID 对应的项目；当前没有 GPU 工作镜像或 publisher credential，后续需要实际镜像合同和产品凭证验收。

## Restart/PVC 升级切换

1. 关闭新意图及新能力闸门，保存数据库恢复点、原配置与镜像身份；停止全部旧 Instance writer/worker、严格 snapshot 读取进程及旧 Storage DELETE 执行者。确认原执行 lease 已归还或失效；不能混跑仍按原 `Spec` 渲染挂载的旧程序。
2. 盘点旧 Storage 已出站或结果未知的直删，保留原授权请求、tenant/cluster/Namespace/PVC name/UID 及出站证据。当前[guard 实现](../../internal/data/instance/pvc_guard.go)没有历史请求导入接口；不能仅凭对象身份调用普通 ReserveDelete 或手造 `write_started` ticket，授予恢复 worker 新的删除权限。无法从真实请求证明收敛时继续禁用该旧名称的挂载；HTTP 超时、进程退出和 lease 失效均不能证明外部删除已结束。
3. 显式执行008并核对数据后，启动匹配的新 Instance、Storage guard 与 recovery 组合，保持新意图关闭。先恢复旧 Pending、DELETE 与已有 ticket，验证查询、初始模板证明和恢复健康，再按权威方案及实际授权开放能力；旧 Gov 仍在运行也不能越过 Resource gate。

A35 切换盘点中的四个既有 PVC 为两个专属 workspace、两个 Block 盘，borrowed 候选为 0；历史直删收敛仍为 `NOT_PROVEN`。仅在旧 writer 确认退出、旧删除确实绑定原 UID precondition 的前提下，经正常 Storage API 新建的唯一名称、新 UID、Filesystem 卷可作为本批新候选；不重用旧名或以业务 SQL 补成功。具体身份与来源链见[切换取证](../execution/records/CONTAINER-RESTART-PVC-20261028/deployment-prepare.md)，后续开放范围只见[当前状态](../execution/status.md#container-restart-pvc-20261028)，这里不另建执行账。

A36 外部 PVC 删除旁路保护由用户暂缓，记为 `DEFERRED_USER_SCOPE`，不记 PASS；内部名称 guard、引用与恢复要求保持。开放范围与实际结果仍以当前执行状态为准。

## 恢复与回退

- 重启后由同一受限 PG、原冻结 CREATE 与唯一 GPU execution、待定 mutation 和 lease epoch 恢复。`CurrentMounts` 是独立于原 CREATE `Spec` 的当前配置，空集合也有实际含义，不回退旧卷；`Desired` 保存最终目标，`RuntimeDesired` 保存当前阶段目标，内部停启和 Network 均读后者。见[持久化读取/提交](../../internal/data/instance/lifecycle.go)与[worker](../../internal/biz/instance/worker.go)。普通 Stop/Start/Restart 不重新占额或解析原镜像，不更换 workspace PVC。
- Pending/Fence 保留原 mutation、操作、phase 及 Notebook UID/RV，推进仍须当前有效 lease；未知写不能换新 RV 重发，也不能靠超时、进程重启或无归因的当前观察清除。`CREATE_RESULT_UNKNOWN` / `WORKSPACE_CREATE_RESULT_UNKNOWN` 保留原待定动作与原账，只有可归因的确定外部结果才可推进。唯一安全停止 Fence 仍约束原未决写；明确 DELETE 抢占后保留归因并向前清理，不恢复旧 Start 或旧目标模板。恢复逻辑见[lifecycle worker](../../internal/biz/instance/lifecycle_worker.go)；不提供手改状态或强制完成解阻。
- 原 CREATE/Attach 与 Storage 删除共用 cluster/Namespace/PVC name guard；`pending/active/retiring` 全部保护名称，不能只检查 active。引用按 path/generation/operation 比较并提交，released 后重挂的新 generation 不得被旧回执释放；最终 DELETE 释放本实例全部保护态并确认无剩余引用才记录完成。见[引用事务](../../internal/data/instance/lifecycle.go)与[Storage 恢复路径](../../internal/biz/storage/delete_recovery.go)。过期 lease 或旧 worker 回执只能补所属事实，不能清新 Pending/Fence、释放新引用或改新运行目标。
- `WAITING_FOR_PHYSICAL_RELEASE` 的旧 DELETE 按当前清理规则重新观察并向前恢复；不补假摘要，不批量改 `ComputeCleaned/QuotaReturned`。007 的 owner cleanup 规则继续适用，本批迁移与旧进程切换按上文008步骤执行；原规则背景见[owner 退额执行状态](../execution/status.md#container-instance-owner-refund-2026-10-27)。
- `QUOTA_RETURN_PENDING` 使用持久 outbox 中的原事件、原 CREATE、resource 与 reason 重试。旧未 ACK 事件须先核实全部清理并固化最终事实，不能直接扫描即发；Gov 已入账但回执丢失仍重试原事件。旧已 ACK 事实保留，只继续剩余清理，不重复退额。
- `WORKSPACE_OBSERVATION_FAILED` 保留磁盘重试；计算已清理仍须等工作盘、网络和引用完整清理后才投递原 GPU 退额。`WAITING_FOR_WORKSPACE_RECLAIM` 表示 PVC 已消失、原 PV 尚未回收；不通过手改状态、移除存储 finalizer 或直接删 PV 解阻。
- 出现问题先关闭新意图，保留兼容新状态的 worker 向前恢复。已经写入新 execution、改变 `CurrentMounts` 或存在 guard ticket 时，不直接部署旧二进制，不把 `Spec` 覆盖回当前配置或删除新字段。保留实例账本、待定动作、guard、引用和退额 outbox；不得删除共享 Namespace、租户 Image pull secret 或其他任务对象。

本任务快照、测试二进制、私有运行配置与文件哈希按当前状态记录的远端目录保留。临时 fixture PG/Redis 由标签核对后自动清理；隔离实验的持久 PG/Redis、卷和角色密钥保留用于恢复。CPU 基础设施探针已按原 UID 回收，资源清单记录留存；真实 GPU 产品对象必须通过产品删除并核对 compute、专属 PVC 与账本，再清理对应任务 SA/Namespace。共享平台组件缺失时记录准确断点和最小接入条件，不安装设备插件或重建集群来扩展本任务。
