# 容器实例挂载、配置与 VPC 接入合同

日期：2026-10-19。状态：**增量实现合同；实际交付与未验证范围只见[执行状态](../execution/status.md#instance-configuration-and-network-2026-10-19)**。原固定输入的哈希保留在任务源清单中。

2026-10-28 增量以[已审 Restart/PVC v2.0 方案](../plans/container-instance-restart-pvc-20261028.md)为输入；以下当前挂载、引用代际和 Storage 删除协调规则替代旧的创建后卷配置不变与双态引用。当前实施/真实验收只见[唯一状态](../execution/status.md#container-restart-pvc-20261028)，迁移和向前恢复见[运行手册](../runbooks/instance-mvp-fedora.md)。

目标是让用户通过 Gov 创建容器实例时选择已有 PVC 和挂载路径、VPC/Subnet，以及标签、注解和环境变量；配置经过授权、校验、冻结和持久化后进入真实 Notebook/Pod，启停和重建继续生效，最终删除正确处理卷引用和网络关联。

本文只定义新增行为。现有实例、GPU 执行和原账退额规则继续引用[实例规格](instance-mvp.md)，网络资源所有权继续引用 [ADR-0001](../adr/0001-own-network-lifecycle.md)，租户数据约束继续引用 [ADR-0002](../adr/0002-use-tenant-owned-data-without-rls.md)。实施顺序见[改造计划](../plans/instance-configuration-network.md)，实际交付结果只记入[执行状态](../execution/status.md)。

本合同定点替代原实例规格中“工作盘固定路径、不引入已有卷”和“仅复用现有网络”的范围限制；实现、正式模块交付和真实环境验收分别确认。用户于 2026-10-19 后续明确授权升级现有测试环境 Gov/Res，并要求先清理旧实例及其业务记录、删除旧实例配置兼容代码。后续真实 GPU 验收统一走现有 `tenant-a`，复用既有 ACC 目录、供给组和 T4；不另建隔离 GPU 后端。kcn/ACC/SG 源码及相应共享组件保持既有版本。

## 1. 业务主链和当前断点

```mermaid
flowchart LR
  U[用户选择卷 网络与配置] --> G[Gov 当前授权和幂等受理]
  G --> R[Res 解析生效配置]
  R --> F[Gov 冻结命令与持久投递]
  F --> D[Res 持久接受与后台执行]
  D --> A[准备网络关联和卷引用]
  A --> N[创建 Notebook 和 Pod]
  N --> V[核验实际挂载 配置和网络]
  V --> S[终端使用 启停与重建]
  S --> X[删除计算 释放网络与卷引用]
```

首个可观察结果：正式 Gov 创建一个 CPU 实例，两个已有 PVC 挂载到指定路径，自定义环境变量可在容器读取，标签/注解出现在实际 Pod；启停后数据和配置保留；删除后用户 PVC 保留、系统工作盘回收。下一条纵向链在此基础上指定 VPC/Subnet，验证取址、通信、Pod 自动重建及最终网络释放。GPU 继续走同一配置实现，并单独验证执行参数和原账退额。

原设计核查快照：Res `f7459f7ae71c5573f53a50c5d20908b129c17e0c`；Gov `90b1ecc8cfa31b96ac43a8e4ff3aa18d41f03812`；用户提供的 kcn 本地 checkout `2b9467c66dc9024fbf321a4b3a98a9e5f3fef377`。下表记录实施前断点，当前实现及部署/数据面结论见执行状态。

| 断点 | 固定源码落点 | 对改造的影响 |
|---|---|---|
| 创建没有用户卷、网络或 metadata/env 字段 | [Res Proto](../../api/instance/v1/instance.proto)、[Gov Proto](https://github.com/zhangzhe-ctrl/ani-governance/blob/90b1ecc8cfa31b96ac43a8e4ff3aa18d41f03812/api/protos/instance/service/v1/instance.proto#L13-L23) | 两端合同、解析及冻结必须一起演进 |
| 工作盘和工作目录固定 `/workspace`，Pod 校验也检查该路径 | [Notebook adapter](../../internal/data/instance/notebook.go)，RenderNotebook、verifyPod | 不能只修改渲染，必须同步运行详情、日志和终端目标核验 |
| 专属工作盘会被最终删除 | [工作盘实现](../../internal/data/instance/notebook.go)，EnsureWorkspace、observeWorkspace、DeleteWorkspace | 用户已有卷不能冒用专属工作盘字段和清理流程 |
| Res Attachment 拒绝不同 Pod UID | [Confirm](../../internal/data/network/attachment.go)、[worker](../../internal/biz/network/attachment_worker.go)、[kc adapter](../../internal/data/network/kc_attachment.go) | 普通 Notebook 启停和自动重建需要受控替换合同 |
| kcn 有新 Pod 创建网卡及旧 sandbox 清理路径 | [Pod controller](https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/2b9467c66dc9024fbf321a4b3a98a9e5f3fef377/internal/controller/core/pod_controller.go#L472-500)、[CNI handler](https://gitlab.changqingyun.cn/kubercloud/sdn/kc-networking/-/blob/2b9467c66dc9024fbf321a4b3a98a9e5f3fef377/internal/daemon/cniserver/handler.go#L299-355) | 本次 UID 限制的修改对象是 Res；保留 kcn 的对象归属和迟到 DEL 保护 |
| 当前规范化使用 EmitUnpopulated，命令摘要显式列举字段 | [business.go](../../api/instance/v1/business.go) | 直接追加字段会改变旧摘要，需要按保存版本解释请求和冻结载荷 |
| 实际 governance 装配没有实例网络关联闭环 | [governance composition](../../cmd/ani-resource-service/governance.go)、[full composition](../../cmd/ani-resource-service/app.go) | 两种支持实例的运行路径均需装配 Attachment worker 和实例 Consumer |

## 2. 范围和所有权

CPU/GPU 共用创建配置；原 CREATE 始终冻结。已有 borrowed PVC 支持停止态挂卸，以及显式 `restart_if_running=true` 的运行态同操作停止/改模板/恢复。工作盘路径、镜像、规格、网络、标签/注解/env 仍不提供修改入口；不停机热挂载、扩盘、卷创建/快照/克隆不属于实例挂卸范围。

| 所有者 | 本次职责 |
|---|---|
| Gov | 公开入参、当前权限检查、可信租户映射、幂等受理、冻结命令及前端合同；不直接读 Kubernetes 或管理网络表 |
| Res Instance | 配置解析、PVC 引用、Notebook/StatefulSet/Pod 身份、启停和最终封闭、实例状态及恢复 |
| Res Network | VPC/Subnet 准入、Attachment、网络投影、独立观察、地址资格和真实释放 |
| kcn | 按受管网络投影建立/回收 VNic、VNicIP、Nic 和底层数据面；当前方案不要求修改其源码 |
| ACC/Gov 原账 | GPU 执行及原始计费规则继续使用现有合同，不将用户 metadata 或网络观察混入 GPU Execution |
| 前端 owner | 卷候选、VPC/Subnet 选择、键值输入和状态展示；本次方案提供合同，不修改前端仓库 |

Instance 在 `internal/biz` 声明自己需要的窄 Network 和卷查询 Interface；Adapter 在 `internal/data`，composition root 注入现有 Network 用例。Instance 不导入 Network repository、sqlc 或 Provider，不跨域读表。Network 的反向 Consumer 通过实例公开只读用例取得提交事实。

## 3. 创建输入和查询合同

### 3.1 公开字段

保留现有 `POST /api/v1/instances` 和 `data` 外壳。以下为新增字段合同；`idempotency_key/name/image_id/image_scope/preset_id/scheduling` 沿用原合同。

```json
{
  "data": {
    "idempotency_key": "10000000-0000-4000-8000-000000000001",
    "name": "demo",
    "image_id": "img_example",
    "image_scope": "tenant",
    "preset_id": "10000000-0000-4000-8000-000000000002",
    "workspace_mount_path": "/work",
    "volume_mounts": [
      {"pvc_name": "datasets", "mount_path": "/data", "read_only": true},
      {"pvc_name": "outputs", "mount_path": "/outputs", "read_only": false}
    ],
    "network": {
      "vpc_id": "vpc_10000000000040008000000000000003",
      "subnet_id": "subnet_10000000000040008000000000000004"
    },
    "labels": {"team": "ml"},
    "annotations": {"example.com/purpose": "training"},
    "env": [{"name": "APP_MODE", "value": "dev"}]
  }
}
```

示例 ID 是占位值，实际请求必须使用可选目录返回的镜像、规格、卷和网络。

| 字段 | 缺省及验证 |
|---|---|
| `workspace_mount_path` | 缺省 `/workspace`；显式值须为规范绝对路径，并同时成为主容器 workingDir |
| `volume_mounts[]` | 缺省空；服务器生成 volume name，客户端不提供 namespace、cluster、UID 或卷删除策略 |
| `network` | 缺省沿用当前集群网络行为；提供时 vpc_id/subnet_id 均必填，解析为显式 VPC 接入，失败不退回默认网络 |
| `labels`、`annotations` | 缺省空；字符串 map，校验 Kubernetes 格式及本方案保留键 |
| `env[]` | 缺省空；有序 name/value 数组，允许空字符串，拒绝重名；首版是字面值，不开放任意 valueFrom/envFrom |

显式 `null`、未知字段、无效类型和重复 JSON key 均拒绝。v2 内缺省与空集合视为相同意图；`network: {}` 和空路径非法。路径不自动清洗，环境值不 trim、不展开。环境数组保持输入顺序，`$(VAR)` 也是字面值；Notebook 渲染必须处理 kubelet 的变量展开规则，容器内仍得到用户原值。

首版上限：附加卷 8 个、环境变量 64 个、用户 labels/annotations 各 32 项，环境值总计 16 KiB、用户 metadata 总计 16 KiB；保留现有 64 KiB 请求体上限。两端验证和前端提示使用同一限制，不能只在页面限制。

### 3.2 可选目录与配置回显

已实现 `ListMountablePVCs` 及批量解析端口，Gov 公开候选入口为 `GET /api/v1/instances/mountable-pvcs?page=1&pageSize=20&name=...`。静态路由先于 `{instance_id}` 路由匹配。分页沿用 `page/pageSize → items,total`，最大 pageSize=100；Kubernetes LIST 使用一次完整一致的分页快照，完成租户过滤、确定性名称排序后计算真实 total 并切页。内部每页 200、单次最多 2,000 个 PVC；continue 过期重启本次查询，超限返回明确容量错误，不返回截断集合或伪造 total。

候选返回 `pvc_name、phase、requested_capacity、access_modes、volume_mode、selectable、unavailable_reason`；容量使用 Kubernetes quantity 字符串，例如 `20Gi`。UID 由 Res 内部冻结，不作为公开可写字段。候选只在可信租户 Namespace 内查询，过滤其他实例专属工作盘及非授权资源。目录展示不能替代创建时重查。

VPC/Subnet 继续复用现有租户列表，前端按 VPC 筛选 Subnet；CPU/GPU 规格解析出的 cluster 必须与所选网络 placement 一致。用户看不到或不能使用的卷/网络不作为可猜测的跨租户详情返回。

授权配置查询 `GET /api/v1/instances/{instance_id}/configuration` 返回当前 `CurrentMounts`、`version/applied_mount_version/pending_change`，以及原冻结的工作目录、网络产品 ID、用户标签/注解/env；待提交目标不能提前覆盖当前挂载。采用实例详情读取权限，该权限可以读取字面环境值。列表、操作状态、审计和错误不回显 env 值；运行诊断只返回环境变量名称/来源。字段及映射见 [Proto](../../api/instance/v1/instance.proto)和[配置查询](../../internal/service/instance/configuration.go)。

创建范围候选沿用原入口；实例范围候选 `GET /api/v1/instances/{instance_id}/mountable-pvcs` 只使用实例已保存的 cluster/namespace，活跃删除 guard 对应名称不可选。三个新动作和候选的权限、五个 Resource RPC 及精确 mTLS 登记见[实例 API 合同](instance-mvp.md#111-用户-api)；只读 Lookup 不访问 Kubernetes、不产生受理或占额副作用。

实例详情增加独立的 `network_state/network_reason/network_cleaned` 和卷引用状态；无网络选择为 `not_requested`。显式联网实例只有当前 Pod Ready 且 Network 核验 attached 后才展示整体 running；部分就绪保留 pending/reason，暂停后不保留旧 IP 的地址资格。

## 4. PVC 引用和工作目录

### 4.1 两类卷分开管理

系统工作盘继续由实例创建、保存 PVC/PV 身份和最终回收。用户已有卷以 borrowed 引用挂载：停止保留引用，删除实例只解除引用，不删除其 PVC/PV，不给用户卷添加指向 Notebook 的可回收 ownerReference。用户卷丢失不触发自动补造，也不阻挡已终止实例解除该引用。

当前冻结挂载项包含 `cluster_id、namespace、pvc_name、pvc_uid、mount_path、read_only、ownership=borrowed`，引用另有内部 `reference_generation`；UID/代际不是公开可写字段。重放及 Start 使用当前配置已冻结的身份，不重选同名卷；原 CREATE 中的挂载仍只表示首次受理内容。

### 4.2 校验与竞态

1. 先解析可信租户 Namespace，再检查 PVC 的 Filesystem 模式、归属、未删除及非 Lost；不接受其他实例专属工作盘。
2. 合法 Pending PVC 可以使用，包含 WaitForFirstConsumer；不能要求所有候选先 Bound。绑定、调度及 CSI 挂载问题作为真实状态和原因返回。
3. 挂载路径必须绝对、规范，拒绝 `/`、系统关键目录、重复路径及与工作目录相互遮盖的路径；具体受保护目录集中在 adapter 校验，不提供任意 volume source 或 hostPath。
4. 创建 Notebook 前、Start 前检查当前冻结 PVC UID，运行观察核对当前 claimName/mountPath/readOnly。运行态 Attach 首次停止前只复核新增冻结身份；Detach 不因剩余失效 PVC 阻塞安全移除。卷替换或访问失败明确返回原因，不重选数据。
5. RWO/RWOP/RWX 按实际存储和调度能力解释，不用无锁计数宣称独占。候选可选不保证并发挂载成功；访问模式及占用原因需可见。

Pod PVC 引用只有 claimName，没有 UID 前置条件。应用层 UID 冻结、GET 和平台内部 name guard 不能消除绕过平台删除后同名重建的竞态，不能宣称严格防替换。当前 Goal 的 A36 外部准入/RBAC 保护按用户决定为 **DEFERRED_USER_SCOPE**，不记 PASS；这不削减本节 UID 校验及以下平台内部引用/删除保护。

### 4.3 持久化和投影

`Spec` 和 CREATE 摘要保持原 JSON；`current_mounts/mounts_initialized/applied_mount_version` 是独立列。渲染、运行验证、配置查询使用 `CurrentMounts`，显式 `[]` 绝不回退 `Spec.VolumeMounts`；未初始化不能渲染成猜测配置。迁移只冻结旧 UID/顺序/readonly，不凭当前同名卷重新选择；`applied_mount_version=0` 仍需 worker 取得模板基线证明。

引用键为 `(tenant_id, instance_id, mount_path)`，保存 PVC UID、operation/intent_version 和 `reference_generation`，对实例/操作使用租户复合 FK。CREATE/Attach 受理产生 `pending`，模板提交后为 `active`；Detach 受理先置 `retiring`，旧模板/Pod 不再可能消费后才 `released`。`pending/active/retiring` 全部保护名称。同路径重新挂载递增代际，旧回执不能释放新引用。当前配置、应用版本、引用状态及对应操作在同一事务提交；系统 `workspace_bindings` 只管理专属盘。

挂卸的模板证明须覆盖 Notebook 与仍存在的 StatefulSet 的目标卷、replicas/observedGeneration 和安全停止；StatefulSet 不存在时，仍须证明 Notebook 已停止、旧控制链/Pod 已退出且后续重建只能来自目标 Notebook 模板。old/target/mixed 分类不确定时保留保护，不能因一次 Update 成功释放 retiring。运行态启动失败后的安全停止保留已提交配置，不回滚 CREATE。持久化落点为[008](../../migrations/instance/008_restart_pvc.sql)及[阶段提交事务](../../internal/data/instance/lifecycle.go)，外部证明见[Notebook adapter](../../internal/data/instance/notebook_lifecycle.go)。主容器仍名 `workspace`，SG、日志与诊断继续核验 owner/Pod UID。

### 4.4 Storage 名称 guard 与删除恢复

原 CREATE、Attach 和 Storage DELETE 共用 `(cluster_id, namespace, pvc_name)` 物理名称 guard，键不含 UID；guard 固定 tenant，不能以跨租户相同物理名绕过。名称正处于删除时拒绝新引用；三个保护态中的任一引用都拒绝 Storage 删除。Instance 提供[窄 BorrowedPVCGuard 端口](../../internal/biz/storage/delete_guard.go)，Storage 不跨域查询 Instance 私表。

Storage 前台与恢复 worker 共用[有限推进](../../internal/biz/storage/delete_recovery.go)：先持久 ticket 和原 UID，再在外部 DELETE 前提交 `write_started`；Kubernetes 删除始终带该 UID precondition。同 UID 有效 lease 的重放没有第二份推进权，过期 token 不能完成；write_started 后不能当作未发送取消。结果未知保留名称屏障和原身份；直接观察 absent 或同名新 UID 只完成旧 ticket，不删除新对象。[guard CAS](../../internal/data/instance/pvc_guard.go)约束 ticket/guard epoch/lease，缺 guard 或依赖失败时不放行删除，绝不回退旧直删；开放入口前还须验证[恢复 worker readiness](../../internal/server/storage_worker.go)。

## 5. 标签 注解和环境变量

用户配置与 ANI/GPU/Network 管理字段分别保存。渲染时先验证键名和保留键，再合并；发生控制字段冲突直接拒绝，不静默覆盖。首版保留范围如下，模式匹配按 key 的域和名称解析，不能用容易绕过的任意子串匹配。

| 保留范围 | 控制用途 |
|---|---|
| `ani.io/*`、任意 `*.ani.io/*` | 租户、实例、owner、意图版本、GPU 执行和签名 |
| `kubeflow-resource-stopped`、`notebook-name`、`controller-revision-hash`、`statefulset.kubernetes.io/*`、`apps.kubernetes.io/pod-index` | Notebook 生命周期和控制器选择 |
| `networking.kubercloud.com/*`、任意 `*.networking.kubercloud.com/*`、`k8s.v1.cni.cncf.io/*`、`ovn.kubernetes.io/*` | 网络选择、网卡/IP 引用和关联 |
| `volcano.sh/*`、`scheduling.volcano.sh/*`、`scheduling.k8s.io/group-name` | 受管队列、PodGroup 和设备调度 |

当前部署中具有 admission 注入语义的附加键，在开放写入前按实际配置补入 adapter 的精确保留集合和测试，避免用户标签触发未经授权的配置注入；普通业务 key 仍可自定义。这里不引入通用 YAML 异常注册表。

`ANI_INSTANCE_POD_UID` 及其他实际平台/设备运行时保留环境变量不可由用户覆盖。普通 env 值按原样保序下发；业务校验和审计不输出值。用户标签/注解不进入 ACC `GpuExecution`，不改变其执行摘要或原 charges。

Notebook v1.10 的用户标签/注解写入 Notebook 顶层 metadata，由 controller 传播到 Pod；PodSpec 继续写 `spec.template.spec`，不能写入会被 CRD 丢弃的 `spec.template.metadata`。最终读取实际 Pod 核对用户配置及系统字段都存在，不能只断言 RenderNotebook 的输出。

## 6. Res Network 支持 Notebook 的 Pod 替换

### 6.1 明确关联模式

Attachment 是 Res 的网络消费记录，区别于 kcn 的 VPC/TransitRouter `VpcAttachment` CRD。现有 `single_pod` 模式继续保留固定 Pod UID 和原释放语义；新增内部可信 `workload_mode=controller_managed`、Consumer 协议 v2，显式登记给实例 owner。公开用户仅选 VPC/Subnet，不能提交模式、owner 身份或 UID 来绕过校验。

逻辑 Attachment 固定关联 tenant/instance/submission/generation/cluster/namespace 和原 VPC/Subnet，正常 Pod 重建不创建新的逻辑网络意图，也不自动换网。网络选择不承诺跨重启保留 Pod IP。

### 6.2 提交快照和独立核验

GetSubmission v2 保留 `open/closing/closed、finalization_id`，增加以下可信事实：

| 字段 | 含义 |
|---|---|
| `snapshot_revision` | 实例提交快照的单调版本，和实例 version、lease epoch、观察 generation 分别解释 |
| `desired_run_state` | running/stopped/deleted；停止不是永久 closed |
| `root_controller` | Notebook 的 GVK、namespace、name、UID；首次记录后同 Submission 不静默换根 |
| `controllers` | 已登记的 StatefulSet 身份和父引用 |
| `current_pods` | 当前候选 Pod 的完整引用及 owner 链；单实例最多一个提供有效地址的 Pod |
| `retired_pods` | 曾登记的旧 Pod 身份，仅用于清理核验，不能提供地址资格 |
| `closed_at` | 不再可能创建工作负载、未知创建已查明且控制器已消失后的最终封闭时点 |

Instance 持久登记自己创建和观察到的身份；Network 自己通过 Kubernetes GET/LIST 逐跳复核 Pod→StatefulSet→Notebook 的 Group/Kind/name/namespace/UID、规范化版本、controller ownerReference、租户和关联标签、网络投影。Consumer 的 UID 是待核验声明，不是直接授权。

Notebook ownerReference 仅接受已核实的 `kubeflow.org/v1` 和 `kubeflow.org/v1beta1`，统一规范为 `kubeflow.org/v1`。这只处理同一对象的已知 served 版本别名；仍要求唯一 `controller=true` 引用，并独立 GET StatefulSet 和 Notebook，核对 namespace/name/UID 及完整父链。未知版本、错误 Group/Kind/UID 或非 controller 引用不能借规范化通过，见 [managed Provider](../../internal/data/network/kc_attachment_managed.go)。

Network 比 Instance 快照先看到新 Pod 时，保存待核验候选并等待新快照，不仅因 UID 变化就永久 ProtocolBlocked；候选和历史资源均进入清理观察，但只有核验通过的当前 Pod 可以提供地址和 LB 后端资格。错误租户、矛盾 owner 链、根 UID 偷换或不符合单实例策略的并发 live Pod 仍是协议冲突。不能单纯移除现有所有 UID 检查。

Network 持久保存最高已接受 snapshot_revision、固定根 UID、相应快照摘要和 finalization_id。低版本响应只忽略并重试，不回退当前 Pod、生命周期或地址资格；同版本异内容判协议冲突。Confirm 携带 submission/generation、Pod UID 和 expected_snapshot_revision，按前置版本条件更新，重复同值幂等；旧代迟到 Confirm 不能覆盖新代，closing/closed 不能被旧 open 快照恢复。进程重启后继续使用已保存的栅栏。

已有 `ProtocolBlocked` 的可信重证恢复仅适用于 `controller_managed`、Attachment 为 reserved/attached、提交为 open/running 且无 finalization 的情形，并须同时满足：

- 当前 Consumer 匹配持久化的最高已接受快照；revision、内容摘要和固定根 UID 均不变，保存的快照内容也须匹配该摘要，不能用新 revision 本身解除阻塞。
- 唯一当前 Pod 的 name/UID 匹配独立观察；Network 已持久保存该 revision 和 Pod UID 的 Confirm 回执。Instance 展示整体 running 仍要求自己的持久 Confirm 回执匹配。
- 重新采集的完整 owner 链和当前 VNic/VNicIP 地址事实核验成功，无待核验候选，fresh proof 覆盖本次观察要求；冲突前的地址缓存不能单独用于恢复。

[managed worker](../../internal/biz/network/attachment_managed.go) 仅据上述证明请求恢复；[持久化事务](../../internal/data/network/attachment_worker.go) 在原 version、lease owner/epoch 和未过期 lease 条件下再次核对冻结快照、Confirm 回执及 proof，才清除阻塞。低版本、同版异内容、根 UID 更换、未知关系或 closing/closed 后迟到的 open 不具备该恢复资格；不能手工清标志或修改冻结快照代替重证。

### 6.3 创建 启停 重建和删除

| 场景 | Instance 动作 | Network 收敛规则 |
|---|---|---|
| 创建 | 本地事务保存网络选择、稳定 request_key/submission/generation；Prepare 后保存 attachment、binding revision 和投影，再创建 Notebook，登记真实 owner 链并 Confirm | Prepare 复用现有同租户、VPC/Subnet 关系及 placement 校验；独立核验后 attached |
| Stop | 持久停止意图，条件停止 Notebook；保留逻辑网络选择，报告 stopped 和实际 Pod 集合 | 无 Pod 是正常暂停；撤销旧地址资格，继续观察旧 VNic/IP 清理，保留 Attachment 占用 |
| Start | 使用冻结规格、CurrentMounts 和网络投影，登记新 Pod；不重占 GPU 原账 | 验证同一根 owner 链后更新当前 Pod，旧关系进入历史清理 |
| Pod 自动重建 | Notebook/StatefulSet 自行造新 Pod，Instance 更新提交快照 | 未确认候选先等待；通过 owner 链后接替，不依赖每代提前改模板 |
| 删除或放弃创建 | 先保存 closing 和禁止继续发送；删除已知 Notebook，查明未知 CREATE；确认控制器退出后最终 closed，再持久请求 Release | 独立确认当前、历史和待核验候选 Pod/VNic/VNicIP 等关系全部消失后 released |

同一动作使用稳定幂等身份。Prepare/Confirm/Release 丢回执或进程重启后由持久步骤重试，不依赖内存调用链。发出但结果未知的 Notebook CREATE 不能因为一次 404 就 closed 或释放网络；未知关系也不能被重置为“无资源”。

Stop 完成使用[完整安全停止证明](instance-mvp.md#82-普通停止与启动)，旧网络清理独立显示。组合挂卸时 Network 消费阶段 `RuntimeDesired`，不能误用最终 `Desired=running` 保留旧地址资格。最终删除与 GPU 退额时机遵循[实例规格 §8.3](instance-mvp.md#83-最终删除)和[完整释放规则](instance-mvp.md#10-完整释放及-gov-原账退额)；失败保留原操作与原因，Gov 通知独立重试。

### 6.4 数据与运行装配

Instance 侧新增 submission/网络绑定记录，保存网络选择、步骤、attachment、投影、controller/Pod 身份、snapshot_revision 和 finalization。Network 侧新增模式、根工作负载引用、当前/历史 Pod 绑定，并使 managed 模式的 Provider 关系以 kind/namespace/name/UID 识别多代对象，不能继续把同名新 UID 一概判冲突。

新关系均带 tenant_id，本域关系使用租户复合 FK；Instance 与 Network 各自事务，不跨域数据库 FK、联表或全局事务。现有 lease/epoch、观察新鲜度、关系索引、持久通知及释放证明继续复用。

composition root 在 `governance`、`full` 实际实例路径装配：Instance Network Adapter→Network Attachments、Network Consumer→Instance 只读提交查询、Attachment worker 和 Provider。既有外部 Consumer 按登记的 owner/协议路由，不因新本地 owner 消费者而丢失。内部调用不通过 loopback gRPC，也不因此放开 Gov 的 Attachment 写 RPC allowlist；已有业务 mTLS、租户身份与方法授权保持有效。

没有显式选择 VPC/Subnet 时不建立 Attachment，也不等待 Network 的 attached/released；保留原计算、工作盘和原账流程。显式选择时，Instance 通过窄端口读取 Network 的业务结果，用于整体就绪及删除完成判断。Network 自己负责 Kubernetes 观察和网络释放证明，Instance 不跨域查询网络表。

Attachment 的独立地址证明只消费 Pod、VNic、VNicIP 和 EIP 事实；同属一个 Notebook 的 Service 更新不能使该证明永久失效。相关对象的旧/新 UID、删除事件以及全局 source continuity/overflow 仍使旧证明失效，Network/LB 原有观察范围不变。

## 7. 当前配置合同与原账

新创建仍只接受显式 `instance_contract_version=2`，缺省、0 和 1 均拒绝；版本由服务端确定，用户不选择。正常新请求的缺省配置只在 Resolve 时解析一次。008 对历史持久结构的有证据前向转换不重新开放旧公开请求、旧 CREATE 摘要或旧版本解码。

| 数据 | 当前规则 |
|---|---|
| 原始请求意图 | 包括用户输入的全部新增字段；metadata 稳定按键编码，挂载按规范路径排序，env 保持原顺序；固定缺省/空值规则 |
| 冻结 CreateSpec | 保存原工作目录、首次 PVC UID、网络选择、用户 metadata/env 和受管字段；默认只解析一次 |
| 当前配置 | 独立 CurrentMounts；挂卸提交不修改 CREATE、原镜像、GPU execution 或 charges |
| 命令摘要 | 使用独立 `resource-create-v2` 域和显式字段集合覆盖完整冻结配置；GPU Execution 仍用它原有摘要 |
| 动态观察 | Attachment 投影、当前 Pod、地址和清理进展单独保存，不能回写并改变原 CREATE 摘要 |

Gov 先做当前授权，再按租户/主体/action/幂等键查已受理的当前合同操作。重放比较完整原始请求意图，返回原 operation/resource/charges，不重新 Resolve；配置不同必须冲突。新键才解析默认值及外部引用，不能丢弃未知字段后误判为同一次请求。

首次受理把当前版本、请求意图摘要、冻结载荷和原有占额记录在同一事务中确定。两个并发请求争用同一幂等键时，未胜出的请求按胜出记录重新比较。新写维护闸门关闭时直接拒绝新受理，不能退回生成旧版本命令。

CPU frozen decoder 只校验当前合同的规范编码及原 business_payload 字节摘要，不做旧字段投影、HTML 反转义或旧 envelope 恢复。GPU 当前唯一冻结 Execution、原始 charges 与稳定退额事件保持原合同；它们自身的协议版本不因实例配置清理而改写。关闭资格使用[实例完整释放规则](instance-mvp.md#10-完整释放及-gov-原账退额)，不依赖 ACC ObserveRelease 或历史物理摘要。

切换先排空入口并退出不兼容旧 Instance/Storage writer、worker 和严格读取进程，再由 owner 显式执行[008](../../migrations/instance/008_restart_pvc.sql)，应用启动只校验。历史 checksum、CREATE、version、ACK 和 replay 保留，不以清库重建迁就新结构；缺引用、身份冲突会阻止迁移，不能猜补保护。旧 Pending 不猜停止 payload，旧 DELETE 由真实 worker 补齐最小执行元数据；恢复规则见[运行手册](../runbooks/instance-mvp-fedora.md)。

`ANI_INSTANCE_NEW_INTENTS_ENABLED`、`ANI_INSTANCE_RESTART_ENABLED`、`ANI_INSTANCE_PVC_MUTATIONS_ENABLED` 缺省均为 false，装配见[instance composition](../../cmd/ani-resource-service/instance.go)。关闭闸门不抹除原键回执，也不停止已受理任务/真实删除 ticket 的恢复；查询、候选和 Lookup 保持只读。启用挂卸前须完成平台内部 guard、恢复 worker 和模板传播前提，不能把 Ready 或候选可选当成真实挂载验收。

例：T0 创建时冻结 PVC UID=U1；平台修改缺省后原 key 仍返回原结果。同名变为 U2 时 Start 不重选；原 key 增加 env 返回冲突。挂卸必须使用新键和当前版本，更换网络/env 没有本批入口；不能通过重放修改旧 CREATE。

## 8. 错误 状态和前端交接

以下 reason 是新增合同，两端错误映射及 OpenAPI 同步维护，不让依赖错误一律变成 503，也不让新资源上限误映射成日志连接错误。

| HTTP | reason | 处理 |
|---|---|---|
| 400 | INVALID_INSTANCE_REQUEST、INSTANCE_RESERVED_CONFIG | 显示字段路径和安全原因；保留键、路径或重复 env 等输入问题 |
| 403 | INSTANCE_SCOPE_DENIED | 当前动作无权限；不回显环境值或他租户资源 |
| 404 | INSTANCE_PVC_NOT_FOUND、INSTANCE_NETWORK_NOT_FOUND | 不存在或不可见的资源统一语义，避免跨租户枚举 |
| 409 | INSTANCE_IDEMPOTENCY_CONFLICT、INSTANCE_PVC_REFERENCE_CHANGED、INSTANCE_NETWORK_CONFLICT | 需要新意图或修正选择；不得自动降级/重选 |
| 422 | INSTANCE_PVC_CATALOG_LIMIT | 候选集合超过受支持的有界查询容量，明确提示；不伪造分页成功 |
| 503 | INSTANCE_DEPENDENCY_UNAVAILABLE | 依赖不可读；同键重试，已受理操作继续由 worker 恢复 |

异步挂载、网络等待及清理失败保存在实例/操作 reason 中，不把一次接受成功显示为运行成功。前端需要展示工作盘与已有卷的删除差异、只读标记、网络准备/暂停/释放状态，以及 PVC/CSI/网络的实际失败原因；敏感环境值只在授权配置视图读取。

## 9. 验收条件

| 编号 | 必须观察到的结果 |
|---|---|
| A1 | 正式 Gov 创建 CPU 实例，两个已有 PVC 挂到指定路径，其中只读卷写入被拒绝；终端实际读写验证，不只检查 YAML |
| A2 | 自定义工作目录、标签、注解、env 在实际 Pod/容器生效；系统 UID guard、GPU/网络控制字段保留，恶意覆盖被拒绝 |
| A3 | 两轮 Stop/Start 后原 PVC UID 和文件保留；删除后系统工作盘/PV 按原规则回收，用户 PVC/PV 与数据保留 |
| A4 | 卷跨租户/专属盘、同名替换、路径冲突被拒绝；合法 Pending 不被错误排除；实际挂载冲突可归因 |
| B1 | 指定 VPC/Subnet 创建，真实子网取址及同 VPC 对照服务通信通过；错误租户/placement 被拒绝 |
| B2 | Stop/Start 和删除 Pod 触发自动重建均能重新联网；新旧 UID 交接无永久误冲突，旧地址不再被作为有效后端 |
| B3 | Prepare/Confirm/Release 丢回执、Instance/Network 重启、Consumer 快照落后、未知 CREATE 均可恢复或明确等待；覆盖快照/Confirm 乱序、closing 后迟到 open、旧 Pod Confirm 迟到，不能回退状态或提前释放 |
| B4 | 最终删除等待当前/历史 Pod、VNic/VNicIP/Nic 等依赖清理及 Network released；完整清理后才提交原 GPU 退额，恢复与重试只作用于原事件 |
| C1 | 旧实例和终结记录已清理；0/1/缺省配置版本明确拒绝；当前 CPU/GPU 创建、重放、恢复、Start/Delete、原账与重复退额保持，配置差异触发幂等冲突 |
| C2 | GPU 模式验证用户配置不改变冻结执行、额度占退及系统注解；真实硬件运行与仅字段传播分别报告 |
| C3 | 实际 governance/full 装配可推进网络，旧 single_pod Consumer 定向回归；真实前端页面验收独立记录 |

## 10. 方案取舍与外部前提

首版保留一个系统工作盘，另允许最多 8 个已有卷；用户卷默认保留；配置创建后冻结；未选网络保持现有行为；显式 VPC 使用一个 primary Subnet。这些取舍由本 Goal 的固定输入采用；是否已通过真实环境验收仍以执行状态为准。

Notebook 原生 PodSpec 支持 PVC 与挂载路径，见 [Notebook v1.10 API](https://github.com/kubeflow/kubeflow/blob/v1.10.0/components/notebook-controller/api/v1/notebook_types.go#L23-L31)。卷同 Namespace、访问模式和延迟绑定以 [Kubernetes 存储合同](https://kubernetes.io/docs/concepts/storage/persistent-volumes/)为基础。实际支持的镜像目录权限、CSI、PVC 删除保护、kcn/OVN 及 admission 保留键需要在真实验收环境核对；源码路径存在不等于该组合已验收。
