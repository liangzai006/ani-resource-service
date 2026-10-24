# Sandbox 方案复核与实施计划

**复核日期：** 2026-10-25
**范围：** 仅 Sandbox；不包含 VM、Notebook、推理服务和其他资源类型
**状态：** 方案方向有条件通过；当前仓库尚未实现 Sandbox 运行时
**审阅结论：** 旧 ANI 的行为和 live evidence 可以作为需求与验收输入，不能作为新 Resource/Runtime 架构或生产就绪证明。实施必须按本文修订后的阶段和退出门禁进行。

## 1. 审核结论

本轮对照了旧 ANI 的源码、OpenAPI、开发记录、脱敏 live evidence，以及 `ani-resource-service` 中保存的 Sandbox 设计。结论如下：

1. 2026-10-17 的 Sandbox 独立设计方向正确：Resource 持久化业务事实，Runtime 负责 Kata/Kubernetes Provider，SG 承担终端数据面，Higress 不承载 Sandbox API。
2. 旧 ANI 已经证明了多项产品行为，但这些行为是在不同批次、不同实现后端下分别验证的，不能拼接成“全部功能已经真实闭环”。
3. 新方案必须把“行为兼容”与“实现迁移”分开。不得复制旧的 `SandboxRuntime`、进程内 map、Gateway handler、旧表或 local fallback。
4. 第一阶段只批准 Kata CPU Sandbox 的持久生命周期和工作盘；文件、Exec、code-run、端口、过期、egress、checkpoint 必须分别通过门禁后再开放。
5. 当前状态不是实现完成，也不是生产 ready。正式开发前先完成 P0 契约冻结和 P1 持久事实模型；没有 Kata RuntimeClass、可用 StorageClass、SG 目标校验和 mTLS 身份时，只能做契约、fake provider 和预检。

## 2. 对照范围与证据等级

本次对照使用固定源码快照，避免把工作树上的未提交变化当成事实：

| 来源 | 快照 | 用途 |
|---|---|---|
| 新 Resource | `ani-resource-service` `main`，`63e138174831ba7cf32452e6216143a147dadc82` | 确认当前仓库没有 Sandbox 实现；确认现有分层、文档职责和运行约束 |
| 新 Resource 历史设计 | `198f6afe536e46be5acbf5acbcbfc17ce17787e6`，仅在 `backup/main-before-sync-20261018` | 复核 2026-10-17 的 Sandbox 独立设计和 VM/Sandbox 拆分设计；该提交不在当前 `main` |
| 旧 ANI | `ANI/repo` `138887aaa096cf38512b4c68a7bab58d8ec1ac72` | 对照真实实现、API、路由、安全边界和开发记录 |

证据按以下等级使用：

- **L3 live passed：** 旧项目在指定集群、指定镜像和指定网络条件下通过的 live gate，只证明记录中的那条行为链。
- **L2 local/logic verified：** 单元测试、静态验证或本地 provider 通过，不能替代真实 Provider 验收。
- **L1 design/contract：** 设计或契约约束，尚未证明实现和部署成立。

旧项目的 live evidence 均带有日期、镜像和环境边界；新项目不得把它们直接标记为新服务的验收结果。

## 3. 旧 ANI 能继承的行为清单

| 能力 | 旧项目证据 | 已确认行为 | 新方案决策 |
|---|---|---|---|
| Kata 生命周期 | `development-records/instance-sandbox-adapter-a.md`、`live-evidence/instance-sandbox-live-20260801.json`，L3 | 创建、Deployment 观察、pause、resume、delete；使用 `sandbox-kata` | **保留行为，重写实现。** Resource 保存事实和操作，Runtime 只执行和观察 |
| 重启后状态/异步任务 | `instance-sandbox-stateless-a.md`、`live-evidence/instance-sandbox-stateless-live-20260802.json`，L3 | Gateway 重启后可查询任务；Redis 幂等键重放，指纹冲突返回 409 | **保留语义。** 新 Resource 使用自己的持久 operation/idempotency 模型，不迁移旧 Redis/Gateway 代码 |
| 工作盘 | `instance-sandbox-kata-storage-a.md`、checkpoint live record，L3 | Sandbox 使用专属 PVC 挂载 `/workspace`；没有默认 StorageClass 时 PVC 会 Pending | **保留约束。** StorageClass 必须是冻结配置或预检选择结果，不能硬编码 `ani-block`，也不能假定集群有默认值 |
| 文件安全 | `instance-sandbox-file-safety-live-gate-a.md`、`instance-sandbox-file-safety-a.md`，L2/L3 | 规范化路径、工作区边界、symlink、hard-link、rename-out 等越界写入被拒绝 | **保留安全性质，重写适配器。** 新 Runtime/SG 必须有 fd/容器边界安全测试，不复制旧 Python 脚本作为业务层实现 |
| code-run | `instance-sandbox-coderun-a.md`、`instance-sandbox-coderun-kubectl-a.md`、live evidence，L3 | Python/JavaScript 在 Ready Pod 中执行；输出有上限；暂停或 Pod 非 Running 时拒绝 | **后置开放。** 执行通过受信 SG/Runtime action；禁止让 Gateway 依赖镜像内的 `kubectl` shell-out |
| 预览端口 | `instance-sandbox-ports-a.md`、live evidence，L3 | 创建真实 NodePort Service；有 Endpoints 时可访问；Kata 不兼容 port-forward，租户 VPC 会阻断外部 NodePort | **不迁移 NodePort 作为产品合同。** 由 SG/受控代理或明确的内部 Service 暴露；NodePort 只能作为受控实验实现并有环境开关 |
| Sandbox token | `instance-sandbox-token-a.md`、live evidence，L3 | HMAC token 绑定 tenant/instance/scope/expiry/jti；files、ports、code-runs 可用；tokens、checkpoints、lifecycle 不能用 Sandbox token | **保留绑定和 scope 语义，替换签发方式。** 由 SG/受信会话服务签发，必须有稳定密钥和撤销/版本检查，禁止进程内随机密钥 fallback |
| checkpoint | `instance-sandbox-checkpoint-a.md`、CSI live evidence，L3 | PVC + CSI VolumeSnapshot；创建时 scale 0，ready 后恢复；restore 重建同名 PVC；clone 从 snapshot-backed PVC 创建；只支持 filesystem crash-consistent，不支持 memory | **最后开放。** 只实现明确的 filesystem snapshot；CSI/权限/回收未通过前返回 `UNSUPPORTED`，不伪造 CRIU 或 memory checkpoint |
| 过期 | `sandbox-expiration-egress-a.md`，L2；状态为 `LOCAL_VERIFIED`，未部署 | 30 秒扫描、session/idle 判定、pause/kill 映射、落 `expired` 状态的代码和单测存在；真实 Provider/部署门禁未完成 | **重新设计为持久 worker。** 需要 claim/lease、DB 条件更新、重启恢复和真实 provider gate，不能把旧本地扫描器当成已验证能力 |
| egress | `sandbox-expiration-egress-a.md`，L2 | 详情可回显 allowlist；真实 NetworkPolicy/租户 VPC 出网 B 部分被平台能力阻塞 | **拆开交付。** 回显不等于网络生效；egress 由 Network owner 提供明确 Prepare/Observe/Release 结果后再开放 |
| API 路径 | 旧 `api/openapi/v1.yaml`、`instances.go`，L1/L3 | 正式路径为 `/api/v1/instances/{instance_id}/sandbox/...`；旧 `/sandboxes*` 是 deprecated stub | **只采用实例子资源路径。** 不恢复旧 `/sandboxes*`，不把旧占位路由迁入 Resource |

## 4. 必须修正的旧实现风险

### 4.1 Provider 真实写入不等于状态真实来源

旧 `KubernetesSandboxRuntime` 在 apply 模式下会创建 Deployment、Pod、PVC 和 Service，但 `Get`/`List` 仍委托进程内 `LocalSandboxRuntime` 的 map；`hydrateExecution` 还会把调用方传入的执行上下文重新灌回本地 map。这样只能说明一次写入适配存在，不能说明 Resource 重启、Provider 漂移或多副本场景下状态可恢复。

新方案必须满足：

- Resource PostgreSQL 是租户、实例、Sandbox 配置、期望/观察状态、operation、幂等和资源引用的事实来源；
- Runtime 的 `Apply` 只返回 mutation receipt 和 opaque refs，`Observe` 必须从 Provider 查询 UID、resourceVersion、phase、Ready、RuntimeClass、PVC UID 和 Service/Network refs；
- unknown 结果只能通过原 UID、名称和 resourceVersion 观察恢复，不能重复 create，也不能直接标记成功；
- Resource 和 Runtime 都不能因请求进程重启而丢失 Sandbox 身份或动作结果。

### 4.2 local token fallback 不能进入生产路径

旧 token 的环境变量是 `SANDBOX_TOKEN_SIGNING_KEY`；未设置时会生成进程内随机 HMAC key。这样跨 Gateway 重启、跨副本、密钥轮换和撤销都不成立。新方案不保留该降级：

- token 由 SG 或受信 session service 签发；
- claims 至少绑定 `tenant_id`、`instance_id`、scope、sandbox version、expiry、jti；
- 删除、暂停、generation/version 变化后旧目标必须失效；
- signing key 来自受管密钥服务或明确的 Secret，并定义轮换；没有稳定密钥时拒绝启用，而不是生成本地随机 key。

### 4.3 过期代码不能被误称为已部署能力

旧过期记录明确写明“`LOCAL_VERIFIED`，未部署 ani-test2，不标 runtime ready”，并列出没有 Pod `ActiveDeadlineSeconds`、没有 DB 级过期过滤/锁、没有 `remain_seconds` 的边界。新实现至少需要：

- `expires_at`、`last_activity_at`、`on_timeout` 作为持久 Sandbox snapshot 的字段；
- worker 以租户范围枚举非终态记录，使用 lease/条件更新抢占到期处理权；
- pause/kill 与 `expired` 状态落库分成可恢复步骤，重复扫描不会重复删除或重复记账；
- Provider unknown、服务重启、worker 超时和暂停后恢复都有明确重试/人工介入状态；
- 真实集群 live gate 证明“创建短时 Sandbox -> 重启 worker/服务 -> 到期动作 -> 计算对象和状态一致”。

### 4.4 NodePort 和 Gateway 内 kubectl 不是可迁移架构

旧端口 live gate 证明了 NodePort 在实验环境的可达性，不证明租户 VPC、边界防火墙和生产暴露策略成立。旧 code-run 的 hotfix 还必须把 `kubectl` 放进 Gateway 镜像，说明调用链把 Kubernetes 管理客户端放进了业务进程。

新方案明确：

- 端口合同返回受控 endpoint、协议、TTL 和撤销状态，不让用户指定任意 NodePort/Service；优先由 SG 或受控内部代理承载流量；
- Runtime 使用受信 client-go/remotecommand adapter 或 SG 数据面，不依赖 Gateway 镜像内的 `kubectl` 二进制；
- Resource 只做目标校验和授权，不把 Pod exec、命令、Kubernetes YAML 暴露给前端；
- 若实验阶段保留 NodePort，必须标记为实验能力，绑定允许的网络环境、端口范围、TTL 和清理 receipt，不能进入通用产品承诺。

### 4.5 Storage 和 checkpoint 必须区分“能创建”与“能回收”

旧 checkpoint live gate 已通过 PVC/CSI 快照，但也记录了 `volumesnapshotcontents/status` RBAC 补缺、60 秒 provider 等待预算、历史 `emptyDir` 不能在线迁移和删除级联清理等条件。新方案必须在数据模型中保留 PVC UID、StorageClass、reclaim policy、snapshot UID 和清理 receipt；删除完成前不能只看 API 返回 2xx。

checkpoint 只代表 filesystem crash-consistent 状态，不包含内存、进程和连接。CRIU 或 memory checkpoint 不得由同名接口暗中承诺。

## 5. 修订后的目标架构

```text
Governance BFF
  -> ani-resource-service
       Sandbox facts / desired-observed / idempotency / expiry / operation
       workspace and network references / target validation result
  -> ani-workload-runtime-service (TLS 1.3 + mTLS)
       Kata/Kubernetes apply / observe / pause / resume / delete
       optional files / code-run / ports / checkpoint actions
  -> Deployment/Pod + sandbox-kata + Workspace PVC + controlled Service

SG <-> Resource target validation <-> Sandbox Pod Exec/WebSocket
Higress: only OpenAI-compatible inference calls; no Sandbox or /api/v1 routing
```

### Resource owner

Resource 负责租户边界、实例关联、冻结 Sandbox snapshot、期望和观察状态、operation/idempotency、过期 worker、资源引用、回收 receipt、删除完成条件和对 SG 的目标校验。Resource 不保存可复用的长期 Sandbox token，不直接执行 Kubernetes 命令，不暴露 Sandbox 公网 HTTP。

### Runtime owner

Runtime 负责 Kata/Kubernetes 资源渲染、Apply、Observe、生命周期、资源 UID、Provider 错误映射和可选 action。Runtime 只接受 Resource 已冻结的业务 snapshot 和 opaque refs，不重新解释用户的镜像、租户、配额或原始 Pod YAML。

### 受信 API 边界

Resource 内部合同至少包含：

- `ApplyCreate`、`Observe`、`Pause`、`Resume`、`Delete`、`GetCapabilities`；
- 幂等键元组：`tenant_id + instance_id + generation + action + idempotency_key`；
- Provider UID、resourceVersion、workspace PVC UID、RuntimeClass、phase、Ready、endpoint 和错误原因；
- `UNSUPPORTED`、`FAILED_PRECONDITION`、`UNKNOWN`、`NOT_FOUND` 等可恢复错误映射。

后续文件、code-run、端口和 checkpoint 作为独立 action，不把所有数据面功能塞进第一版生命周期接口。

### Sandbox snapshot

创建时冻结并持久化：

- tenant、instance、generation、operation 和请求摘要；
- image ID/digest、CPU、memory、允许的 RuntimeClass、目标 Cluster/Namespace；
- session/idle timeout、`expires_at`、`last_activity_at`、`on_timeout`；
- deny-all/allowlist/internet egress policy 及 Network owner 引用；
- 环境变量的受控 Secret 引用和初始端口声明；
- Workspace volume ID、容量、StorageClass、reclaim policy；
- Provider spec digest、资源 refs、UID 和最后观察结果。

前端不得提交任意 Pod YAML、宿主机路径、特权开关、任意 RuntimeClass、长期 token 或任意 NodePort。

## 6. 分阶段实施计划

### P0：契约、证据和平台预检

**工作内容**

- 冻结 Governance -> Resource 的 Sandbox 版本化合同和实例子资源路径；
- 冻结 Resource -> Runtime 的 `workload.runtime.v1` 合同、错误码、幂等元组和 unknown recovery；
- 建立 capability matrix：Kata、PVC、Service、NetworkPolicy、SG、mTLS、CSI Snapshot；
- 定义 Sandbox snapshot、workspace 引用、Provider refs、过期字段和删除 receipt；
- 明确 old ANI 每一条证据是 L1/L2/L3，建立新项目 live gate 骨架。

**退出条件**

- Proto 通过 pinned generation workflow；
- 租户隔离、幂等、版本/generation、旧 token 失效和错误映射有测试；
- 目标集群预检明确列出 `sandbox-kata`、StorageClass、RBAC、SG/mTLS 和 Network owner 的结果；
- 预检失败时只阻止后续 live gate，不通过 fake 测试伪造真实通过。

### P1：Resource 持久事实、工作盘和恢复骨架

**工作内容**

- 在 `internal/biz/sandbox` 定义领域类型和 ports；
- 在 `internal/data` 实现 PostgreSQL facts、operation、idempotency、workspace binding、worker lease 和回收 receipt；
- 复用现有 Resource 的租户和 operation 语义，但不把 Sandbox 字段塞进 Notebook 专用结构；
- 先接 Runtime fake，覆盖重复 create、Provider unknown、服务/worker 重启、删除重试和跨租户拒绝；
- 实现持久 `expires_at`/`last_activity_at` 和条件更新的过期 worker，暂不开放端口和 checkpoint。

**退出条件**

- `go test`、分层依赖和 `make verify` 通过；
- 同一幂等请求不会产生第二条 Sandbox 或第二个 operation；
- worker 失去 lease 后可由新 worker 继续，过期动作和 `expired` 状态不会重复执行；
- 删除只有在计算对象、工作盘和回收 receipt 达成条件后才标记 `deleted`。

### P2：Kata CPU 生命周期真实闭环

**工作内容**

- 新 Runtime Provider 渲染 Deployment/Pod、`runtimeClassName=sandbox-kata`、专属 Workspace PVC、最小 ServiceAccount 和网络对象；
- Resource 只发送冻结 snapshot；Runtime 返回并持续观察 Deployment/Pod/PVC UID；
- 实现 create、observe、pause、resume、delete；pause 保留 PVC，resume 使用原 snapshot；
- SG 目标校验包含 tenant、instance、generation/version、Pod UID、container 和允许的会话类型。

**退出条件：真实 live gate**

```text
create -> Kata guest/Pod Ready -> SG target validation
-> write /workspace -> pause -> resume -> file retained
-> delete -> Deployment/Pod absent -> PVC/PV reclaim receipt
```

同时必须验证租户 A 不能观察或操作租户 B、重复请求不产生第二个 Deployment、Provider 超时可恢复、旧 Exec 目标在 pause/delete 后失效。

### P3：受信数据面、文件安全、code-run 和 token

**工作内容**

- SG 承载 WebSocket/Exec；Resource 只返回短时目标授权；
- 文件 list/write/delete 实现规范化路径、工作区根、symlink/hard-link/rename-out 防护、内容大小和 upload idempotency；
- code-run 只允许明确语言、超时、输出大小和工作目录，结果写入 operation/task；
- token 由受信 session/SG 签发，scope 映射保持旧行为：files、ports、exec；tokens、checkpoints、lifecycle 需要平台身份；
- 不引入进程内 signing key、local instance map 或 Gateway `kubectl` 依赖。

**退出条件**

- 文件安全测试覆盖正常读写、路径穿越、symlink、hard-link 和父目录替换；
- code-run 在 Ready Pod 中通过，暂停、超时、输出超限和镜像缺少运行时均有明确错误；
- token 跨实例、跨租户、过期、版本变化、删除和密钥轮换测试通过；
- 真实 gate 的证据记录环境、镜像 digest、RBAC、网络和清理结果。

### P4：受控端口、egress 和生产过期 gate

**工作内容**

- 选择 SG/内部代理或受控 Service 作为预览端口承载，固定端口范围、协议、TTL、租户标签和清理 receipt；
- egress policy 通过 Network owner 的 Prepare/Observe/Release 合同落地；详情回显不能作为网络生效证明；
- 在真实集群验证 session/idle expiry、worker 重启、pause/kill、Provider unknown 和最终状态一致性；
- NodePort 仅可在明确的实验环境开关下启用，不能作为通用 API 默认行为。

**退出条件**

- 端口能创建、访问、撤销和自动清理；租户 VPC/边界策略的可达性已单独记录；
- egress deny/allowlist/internet 三种策略分别有 Provider 观察结果，无法下发时返回可识别错误；
- 到期动作在服务重启后仍能执行，重复扫描不会重复资源操作；
- 不把旧 `LOCAL_VERIFIED` 记录升级成新项目的 live 通过。

### P5：filesystem checkpoint/restore/clone

**前置条件**

- CSI VolumeSnapshot CRD/controller、snapshot class、权限和回收策略已通过独立预检；
- Workspace PVC 的 StorageClass、RWO/挂载约束、VolumeSnapshotContent 状态权限已固定；
- P2 的 pause/resume、P1 的 durable operation 和 P4 的删除/回收 receipt 已通过。

**工作内容**

- 创建前 scale 0 并等待 Pod 消失，再创建带 tenant/instance/checkpoint 标签的 VolumeSnapshot；
- 只接受 filesystem crash-consistent 语义；`keep_memory=true` 明确返回 `UNSUPPORTED`；
- restore 删除并从 snapshot 重建受 Resource 所有的同名 PVC；paused/stopped 保持原期望状态；
- clone 创建新 Sandbox 和 snapshot-backed PVC，完整记录 source snapshot UID 和新 PVC UID；
- 删除源 Sandbox 时按 Resource 自己的 UID/标签清理 managed snapshot，不能删除共享卷。

**退出条件**

- 真实顺序通过：写入 `version-1` -> snapshot ready -> 写入 `version-2` -> restore -> 读回 `version-1` -> clone -> 新实例读回 `version-1` -> 删除并确认所有受管对象归零；
- API server、CSI controller leader、RBAC、60 秒等待预算和未知结果都有失败/恢复证据；
- 文档明确不包含 memory/process/connection 状态，不宣称 CRIU。

## 7. 统一验收门禁

所有阶段都必须留下可追溯证据，至少包括请求摘要、tenant/instance、generation、idempotency key、Provider UID、镜像 digest、集群、namespace、时间和清理结果。门禁分为：

1. **契约门禁：** Buf 生成、API compatibility、错误映射、架构分层和 `make verify`。
2. **状态门禁：** desired/observed、operation、lease、unknown recovery、重启后查询和幂等 replay。
3. **安全门禁：** 跨租户 404、错误 kind/state/provider 的 422、token scope/version、目标 UID、路径和端口边界。
4. **Provider 门禁：** Kata RuntimeClass、Pod Ready、PVC Bound、Service/Network 观察、UID ownership、删除和回收 receipt。
5. **故障门禁：** 请求超时、Provider unknown、worker 重启、CSI 未 ready、RBAC 不足和迟到对象；任何未知写入不得自动重复 create。
6. **范围门禁：** 每项记录区分 L2 local/logic 与 L3 live；旧 ANI evidence 只能作为输入，不替代新 Resource/Runtime gate。

## 8. 明确不迁移和不承诺的内容

- 旧 ANI 的 `LocalSandboxRuntime`、进程内 instance/token/port/file/checkpoint map；
- 旧 Gateway handler、旧数据库表、旧 Redis 组织方式和 `sandboxExecutionForRecord` 适配套路；
- `/api/v1/sandboxes*` deprecated stub 路由；正式入口只使用实例及其 Sandbox 子资源；
- 未设置密钥时的进程内 token fallback；
- Gateway 镜像内安装 `kubectl` 并 shell-out 到 Pod 的执行方式；
- 任意 Pod YAML、任意 RuntimeClass、宿主机路径、特权容器、任意 SSH、任意 NodePort 和跨集群调度；
- 没有 CSI/CRIU 证明时的 checkpoint、memory checkpoint 或“进程恢复”承诺；
- 把旧的 `LOCAL_VERIFIED` 过期 worker 或仅详情回显的 egress allowlist 记为生产能力；
- 把旧实验环境的 NodePort 可达性、default 网络或单一 StorageClass 直接外推到所有租户 VPC 和集群。

## 9. 当前审核后的执行状态

| 项目 | 结论 |
|---|---|
| 方案方向 | 有条件通过，按本文阶段修订后实施 |
| 当前 `ani-resource-service` Sandbox 代码 | 未实现 |
| 当前 `main` 是否包含 2026-10-17 设计文档 | 不包含；设计提交只在 `backup/main-before-sync-20261018` |
| 旧 ANI 生命周期 live evidence | 可作行为参考，不能作为新项目验收 |
| 旧 ANI 文件/端口/code-run/token/checkpoint evidence | 分批 live 通过，后端和网络边界各不相同 |
| 旧 ANI 过期 worker | 仅 local verified，未部署 runtime ready |
| 真实开发前置 | P0 契约、平台预检、P1 持久事实模型 |
| 生产开放条件 | 对应阶段 L3 live gate、删除/回收证据、故障恢复证据全部具备 |

## 10. 复核来源索引

新 Resource 历史设计：

- `docs/superpowers/specs/2026-10-17-sandbox-runtime-design.md`
- `docs/superpowers/specs/2026-10-17-workload-runtime-split-design.md`

旧 ANI 关键实现：

- `pkg/ports/sandbox_runtime.go`
- `pkg/adapters/runtime/local_sandbox_runtime.go`
- `pkg/adapters/runtime/kubernetes_sandbox_runtime.go`
- `pkg/adapters/runtime/kubernetes_sandbox_files.go`
- `pkg/adapters/runtime/kubernetes_sandbox_ports.go`
- `pkg/adapters/runtime/kubernetes_sandbox_checkpoints.go`
- `pkg/adapters/runtime/kubernetes_sandbox_pod_exec.go`
- `pkg/adapters/runtime/sandbox_expiration_controller.go`
- `pkg/security/sandboxtoken/token.go`
- `services/ani-gateway/internal/middleware/sandbox_auth.go`
- `services/ani-gateway/internal/router/instances.go`
- `services/ani-gateway/internal/router/sandbox_resources.go`（deprecated stub，不迁移）

旧 ANI 开发记录和 evidence：

- `development-records/instance-sandbox-adapter-a.md`
- `development-records/instance-sandbox-stateless-a.md`
- `development-records/instance-sandbox-file-safety-a.md`
- `development-records/instance-sandbox-file-safety-live-gate-a.md`
- `development-records/instance-sandbox-coderun-a.md`
- `development-records/instance-sandbox-coderun-kubectl-a.md`
- `development-records/instance-sandbox-ports-a.md`
- `development-records/instance-sandbox-token-a.md`
- `development-records/instance-sandbox-checkpoint-a.md`
- `development-records/instance-sandbox-kata-storage-a.md`
- `development-records/sandbox-expiration-egress-a.md`
- `development-records/live-evidence/instance-sandbox-*.json`
