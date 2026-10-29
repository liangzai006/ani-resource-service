# Sandbox Runtime 完整设计方案

**版本：** 1.1（设计合同；实际实现、部署与验收仅见[执行状态](../execution/status.md)）

**范围：** `ani-resource-service` 内的独立 Sandbox 业务组件；Gov 仍是唯一治理网关和入站链路，使用 `/api/v1/sandbox`，创建不依赖其他实例。Sandbox 通过本进程内的 Compute、Image、Storage 能力和 Resource 共享配置获取创建所需资源，不依赖另行部署的业务服务或父实例。GPU、内存快照和公网端口不在一期承诺内。

2026-10-29 业务澄清：旧业务的 Sandbox 自身是一种实例，旧数据面路径中的实例 ID 不是另一个父实例。新入口直接创建 Sandbox；这一修正不表示旧前端全部参数已经迁移。当前创建字段仍需对照模板、镜像引用、自动启动、超时和出网策略的来源与实际能力，不能把字段存在当作策略生效证据。共享资源接线及镜像目录登记限制见下文；新接线在真实集群的完整生命周期尚未验证，实际进度以[执行状态](../execution/status.md)为准。

**职责：** 将旧 ANI 的行为输入转换为符合新版 `ani-resource-service` 分层约束的独立组件设计，定义目标行为和分期门禁；当前实现、部署及验收仅见[执行状态](../execution/status.md)。

本文是 Sandbox 后续开发、联调和验收的唯一设计方案。旧 ANI 的源码、OpenAPI 和 live evidence 只提供行为要求及验收样例；新版 Resource 的规格只提供本仓库的分层、数据、资源解析和恢复约束。Gov 负责认证、授权、公网 DTO 和路由，Resource 负责 Sandbox 业务事实和执行；Sandbox 不反向调用 Gov 或其他业务服务。

## 1. 设计依据与合并原则

### 1.1 依据

| 来源 | 作用 | 使用边界 |
|---|---|---|
| 旧 ANI `138887aaa096cf38512b4c68a7bab58d8ec1ac72` | 生命周期、工作盘、文件安全、code-run、端口、token、checkpoint、过期和 egress 行为 | 只作为兼容性和 live gate 输入，不作为新运行时实现或生产就绪证明 |
| Resource `docs/specs/compute-presets.md` | `preset_id`、`ResolveComputePreset`、`ComputeSelection`、`cpu_millis`、`memory_mib`、`selection_digest` | 作为 Sandbox 算力解析和冻结的唯一来源；用途绑定以该合同登记的 `SANDBOX` 为准 |
| Resource `docs/specs/instance-mvp.md` | 资源单位、CPU/内存单位、requests/limits 相等、冻结执行快照和清理语义 | 只复用通用资源语义，不复制 Notebook 专用生命周期和表结构 |
| Resource `AGENTS.md`、`CONTEXT.md` 和模块化 ADR | `cmd` 组合根、`internal/biz`、`internal/service`、`internal/data`、`internal/server` 的依赖方向，显式租户条件、持久事实和 Provider 观察 | 作为本组件唯一实现约束；Gov 只作为入站适配，不创建跨服务运行时模块 |
| Resource `docs/specs/instance-configuration-network.md`、[ADR-0002](../adr/0002-use-tenant-owned-data-without-rls.md)、[ADR-0004](../adr/0004-observe-cr-with-durable-reconciliation.md)、[ADR-0006](../adr/0006-evolve-resource-service-preserving-network.md) | 显式租户条件、PostgreSQL 事实源、Provider 观察和模块化边界 | 只复用架构原则，不把 Network 或 Instance 的私有字段、调用或数据表接入 Sandbox |
| `ani-inference-service` 的 `inference/v1.ResourceSpec` 与 managed contract | CPU/内存 Quantity、requests/limits 校验、GPU 分离原则 | 仅作历史语义参考；不作为依赖、不导入 protobuf、不访问该服务 |

### 1.2 旧 ANI 复核结论

旧 ANI 的能力按证据等级使用：L3 表示指定环境的 live gate，L2 表示本地逻辑/单元验证，L1 表示设计或契约。旧证据不等于新 Resource 已实现或生产 ready。

| 能力 | 旧 ANI 已确认行为 | 新方案结论 |
|---|---|---|
| Kata 生命周期 | create、Deployment 观察、pause、resume、delete，默认使用 `sandbox-kata` | 保留行为，改为 Sandbox 持久事实 + 本地 Kubernetes Provider；选取集群已有 Kata RuntimeClass，P2 真实集群验收 |
| 重启和幂等 | Gateway 重启后可查任务，Redis 幂等重放，指纹冲突返回 409 | 保留语义，使用 DB operation/idempotency/lease；禁止迁移旧进程内 map |
| 工作盘 | 专属 PVC 挂载 `/workspace`；无默认 StorageClass 时会 Pending | StorageClass、容量、reclaim 必须冻结并预检；P2 验收 |
| 文件安全 | 规范化路径、symlink、hard-link、rename-out 越界写入被拒绝 | 保留安全性质，由 Sandbox 自己的 file adapter 重写并做 fd/容器边界测试 |
| code-run | Python/JavaScript Ready Pod 执行，输出有上限，暂停或非 Running 时拒绝 | 后置到 P3；由 Sandbox 自己的 exec adapter 执行，不依赖 Gateway 或其他服务 |
| 端口 | NodePort 在实验环境可访问，但租户 VPC/边界策略有限制 | Sandbox 自己创建受控 Service/endpoint；NodePort 仅实验开关，P4 验收 |
| token | HMAC 绑定 tenant/instance/scope/expiry/jti；缺密钥时存在进程内随机 fallback | 保留 scope 绑定，移除 fallback，由 Sandbox 自己的 signer 签发 |
| checkpoint | CSI filesystem snapshot 可用；无 memory/CRIU，存在 RBAC/等待预算约束 | P5 独立开放，只承诺 filesystem crash-consistent |
| 过期 | 30 秒扫描和 session/idle 逻辑只到 LOCAL_VERIFIED，未部署 ready | 改为持久 worker、lease、重启恢复和真实 gate；P1/P4 |
| egress | 旧实现可回显 allowlist，真实网络生效受平台能力限制 | Sandbox 自己渲染和观察 NetworkPolicy；回显不算生效 |
| API | 正式路径是 `/api/v1/instances/{id}/sandbox/...`，旧 `/sandboxes*` 为 deprecated stub | Gov 使用独立 `/api/v1/sandbox` 路径；旧路径中的实例 ID 是 Sandbox 本身，不表示另一个父实例 |

### 1.3 旧实现必须重写的风险

1. **Provider 写入不等于状态事实。** 旧 Runtime apply 后仍从进程内 map 返回 Get/List，不能支撑重启、Provider 漂移和多副本。这里的“进程内 map”是类似 `map[sandboxID]SandboxRecord` 的临时内存表：进程重启就丢失，不能作为 PVC、Pod 或 Sandbox 的事实来源。新 Runtime 的 `Apply` 只返回 mutation receipt，`Observe` 必须查询 Provider UID、resourceVersion、phase、Ready、RuntimeClass、PVC UID 及 Service/Network 引用。
2. **Token fallback 不可进入生产。** 没有稳定密钥时必须拒绝启用；claims 要绑定 tenant、sandbox、generation/version、scope、expiry、jti，并定义撤销和轮换。
3. **过期代码不能视为已部署能力。** 到期动作必须有持久 `expires_at`、`last_activity_at`、`on_timeout`、worker lease、条件更新、重试和人工介入状态。
4. **NodePort 和 Gateway 内 kubectl 不构成产品架构。** 端口合同返回受控 endpoint、协议、TTL 和撤销状态；Sandbox 的 Kubernetes adapter 使用受信 client-go/remotecommand，不引入 Gateway 或外部数据面服务。
5. **Storage 和 checkpoint 必须区分创建成功与回收成功。** snapshot/PVC UID、StorageClass、reclaim policy、RBAC、等待预算和 cleanup receipt 都要进入快照或操作事实；不能以 HTTP 2xx 直接标记完成。

### 1.4 合并后的硬规则

1. **旧需求决定行为，新版 res 决定事实和资源来源。** 例如旧 ANI 要求 pause 后工作盘保留；新版 Resource 决定必须保存 PVC 引用、Provider UID、操作和回收 receipt，不能靠进程内 map 复现。
2. **所有执行规格在受理时冻结。** 规格目录后续禁用、修改或删除，不重算已受理 Sandbox；重试和恢复只消费冻结快照。
3. **用户不能直接提交 Kubernetes 资源。** 镜像、资源、RuntimeClass、Namespace、端口、网络和特权能力都要经过 Resource 的业务校验和白名单渲染。
4. **旧证据按能力逐项迁移。** 旧项目的 `LOCAL_VERIFIED`、回显、NodePort 可达或单次 apply 不能直接升格为新服务 live 通过。
5. **Sandbox 是独立业务切片。** 领域、数据、服务、运行时适配器和 worker 由 Sandbox 自己持有；不把 Sandbox 做成通用 YAML 注册表或共享运行时模块。
6. **禁止本地状态作为事实源。** 新方案不得使用进程内 map、local 文件、JSON/YAML 状态文件、SQLite/嵌入式临时库或 `/tmp` 文件保存 Sandbox、operation、幂等、PVC 或过期状态。内存缓存如有必要只能是可丢弃缓存，不能参与正确性判断，重启后必须从 PostgreSQL 和 Kubernetes Provider 重建。

### 1.5 独立组件边界

“独立”指 Sandbox 在 `ani-resource-service` 进程和仓库内自包含，不指脱离 Kubernetes 或 PostgreSQL 这类基础设施。实现必须满足：

- 组合根只有 `cmd/ani-resource-service`；由它装配 Sandbox 的本地 use case、Repository、worker、`SelectionResolver` 和 Kubernetes Provider adapter；
- 领域代码位于 `internal/biz/sandbox`，只依赖领域类型和本组件端口；服务适配位于 `internal/service/sandbox`；数据库、Kubernetes、token signer 和文件/exec 适配位于 `internal/data/sandbox`；HTTP/gRPC、鉴权中间件和日志位于 `internal/server`；
- Gov 是唯一允许的入站治理适配：它把认证后的 tenant、actor、request_id 和业务 DTO 通过已登记的 Resource RPC 传入；Sandbox 不反向调用 Gov，也不在领域层依赖 Gov protobuf 或客户端；
- Sandbox 不依赖另一个 Instance；新建 Sandbox 不查询 Instance，历史 `instance_id` 只保留为兼容数据。共享目录或镜像能力应通过本进程内适当的端口接入，不要求独立部署，也不直接访问其他领域的存储表；
- Sandbox 不依赖另一个实例或外部工作负载服务预先创建 workspace、network、token 或 operation；镜像使用本 Resource 进程中的 Image 目录校验，必须已有可授权的登记记录。工作盘与其持久状态由 Sandbox 自己持有和清理；
- 资源目录只通过同一 Resource 进程内的 `SelectionResolver` 端口使用，不发起远程 `ResolveComputePreset` RPC。该 port 由进程内 `internal/biz/compute.Resolver` 实现，复用其 preset 校验和选择逻辑；Sandbox 不依赖 compute 的传输层或 protobuf；
- Runtime 不是独立的 `ani-workload-runtime-service`。Kubernetes Provider adapter 直接属于 `internal/data/sandbox`，通过本地 `Runtime` port 被 worker 调用；
- Gov 是可替换的传输入口，不进入 Sandbox 的领域依赖图；Resource 的 RPC listener 只负责身份、allowlist、deadline 和 DTO 适配。没有 Gov 请求重放时，Sandbox 仍可由自己的持久 worker 和 Provider 完成已受理生命周期。

Sandbox 的外部基础设施边界包括 Kubernetes API/RuntimeClass/CSI、PostgreSQL 和 Image 所使用的镜像仓库；均通过本地 adapter 或本进程内 Image 能力访问。Provider 的 unknown、超时和回收结果须按状态机持久化。

## 2. 产品范围与兼容目标

### 2.1 一期范围

一期只实现 Kata CPU Sandbox 的持久生命周期和专属工作盘：

- 创建、观察、暂停、恢复、删除；
- 持久化 Sandbox 意图、冻结资源、Provider 引用、operation、幂等和观察状态；
- 使用集群中受控的 Kata RuntimeClass；
- 专属 Workspace PVC，明确 StorageClass、容量和回收策略；
- 可恢复 worker、持久化单步 Provider 写入意图和回执；
- 面向本组件 file/exec/port adapter 的目标校验合同。

文件、Exec/code-run、端口、token、egress、checkpoint 都属于 Sandbox 的完整目标能力，但分别作为独立 action 设计和验收；创建、暂停、恢复、删除等生命周期接口不自动授予这些能力。对应门禁未通过时，接口必须明确返回 `UNSUPPORTED` 或 `FAILED_PRECONDITION`，不能静默成功或伪造已生效。

### 2.1.1 交付分期

| 版本 | 包含内容 | 不包含内容 |
|---|---|---|
| **一期：核心 Sandbox** | Gov 入站链路和 Sandbox RPC、租户/Sandbox ID、`preset_id` 资源解析、CPU/内存冻结快照、Kata CPU create/observe/pause/resume/delete、PVC 工作盘、PostgreSQL 事实、幂等、operation、worker lease、Provider unknown 恢复和删除回收 | 文件、Exec/code-run、端口、Token、egress 实际策略、checkpoint/restore/clone 的可用能力 |
| **二期：Sandbox 数据面** | 文件安全、Exec/code-run、受控端口、Sandbox 自有 Token、egress NetworkPolicy、filesystem checkpoint/restore/clone；每项仍作为独立 action 和独立验收门禁 | GPU、memory/CRIU checkpoint、任意 Pod YAML、宿主机路径、任意 NodePort 和跨服务业务编排 |

`expires_at`、`last_activity_at`、session/idle timeout 和 `on_timeout` 的字段、迁移及自动过期动作统一属于二期 P4；一期 API、模型和表结构不承诺这些字段。P4 必须先补齐持久化与恢复合同并通过真实 Provider gate，再开放过期能力。

### 2.2 目标架构

```text
Governance BFF
  -> /api/v1/sandbox/...
  -> Resource sandbox.v1 RPC (TLS/mTLS + exact allowlist)
       internal/server/sandbox
  -> internal/service/sandbox
       request validation / operation admission
  -> internal/biz/sandbox
       intent / frozen snapshot / state machine / ports
  -> internal/data/sandbox
       PostgreSQL repository + worker lease
       Kubernetes Provider + file/exec/port/checkpoint adapters
  -> Deployment/Pod + Kata RuntimeClass + Workspace PVC + optional Service/NetworkPolicy
```

Gov 只负责入口治理和协议适配，不承载 Sandbox 状态机；Resource 的业务事实源是 PostgreSQL，Kubernetes adapter 是 Provider 事实源，Sandbox 自己负责数据面 action。Sandbox 不新增 Gov client、Inference client、SG client、Network client 或 `ani-workload-runtime-service`。

### 2.2.1 一期装配与迁移合同

P0～P2 的 Resource 纵切包含：`api/sandbox/v1` 自有 RPC、Gov 精确方法白名单、`SANDBOX` compute usage、`internal/biz/service/data/server` Sandbox 分层、PostgreSQL schema/operation/idempotency/lease、Kubernetes PVC/Pod Provider，以及 Kata CPU 的 create/observe/pause/resume/delete worker。Sandbox 与其他 Resource 模块共用主入口，不需要 `ANI_SANDBOX_CONFIG_FILE` 或专用 kubeconfig。当前组合根使用 Compute 已配置的受管数据库和 cluster，使用 Resource 的 Kubernetes 配置与租户命名空间前缀；缺少共享依赖时，Sandbox RPC 返回依赖不可用，不影响其他 Resource 模块启动。

创建的具体来源：

- **算力和租户命名空间：** 通过本进程 Compute 目录解析 `SANDBOX` 用途。首次解析创建规格时，按 Resource 共享前缀和受信租户 ID 推导命名空间；不存在时按 Network 的既有名称和归属标签初始化，不依赖创建 VPC。并发创建或写入结果不确定时只重新读取并检查名称、UID、标签和删除状态，不接管不匹配对象。规格冻结 UID；受理、重放及 worker 的既有边界不因命名空间消失而重新创建它。
- **镜像：** 通过本进程 Image 目录按租户授权解析 tag 或 digest，冻结不可变 digest 引用并在受理新命令前再次校验。当前只接受已在租户或平台镜像空间登记、可授权且可解析的镜像引用；任意未登记镜像不能直接创建。这是当前实现限制，并非声称旧前端镜像流程已全部兼容。
- **工作盘：** 请求可提供正数 `workspace_size`；省略时当前采用旧 Sandbox 工作盘的 `5Gi` 默认值。StorageClass 由 Storage 目录选择唯一平台默认类，且实时对象必须未删除、明确使用 `Delete` 回收策略；没有唯一、安全的默认类时创建失败，不自行猜测类名。
- **运行时：** Resource 使用共享 Kubernetes 客户端按 `sandbox-kata`、`kata` 的顺序读取 RuntimeClass，仅在前一项不存在时尝试下一项；选择项必须使用 `kata` 或 `kata-` 开头的 handler，且不得正在删除。不存在、权限不足或对象不符合要求时拒绝新的创建，不回退到普通容器。解析规格时冻结选中的名称，新建受理时独立核对冻结的名称仍在许可名单且有效，不因新出现优先级更高的 RuntimeClass 改写该选择；已受理命令按持久快照重放。当前新创建接口不提供由请求任意选择 RuntimeClass 的能力。目标集群运行时和 StorageClass 的实际可用性仍需真实环境验证。

这些内容描述当前工作区的接线和目标约束，不代表已发布或通过真实集群验收。镜像权限、拉取凭据、默认存储类、租户命名空间和 Kata RuntimeClass 均属于实际验收前置；详见[执行状态](../execution/status.md)。

从旧版本升级必须先停止所有旧 Sandbox worker（包括独立候选），再以 owner 应用增量 migration003、配置 Provider 只读权限并启动新 worker；禁止旧 worker 与新 journal 写入者共用同库并行运行。旧版本不理解 pending 栅栏，不能直接滚动混用；回退前必须独立评估未决写入和在途请求，不能丢弃 journal。

Sandbox schema 由独立的 `-sandbox-migrate` 运维模式创建，使用 `ANI_SANDBOX_OWNER_DSN_FILE` 与 `ANI_SANDBOX_RUNTIME_ROLE`。`-instance-migrate` 不再隐式创建 Sandbox 表；这样 Instance 和 Sandbox 可以分别迁移、启停和回滚，运行时仍只使用受限角色。

二期 P3～P5 的 file、Exec/code-run、端口、token、egress 和 checkpoint/restore/clone RPC 分别随对应切片引入，不预留默认开放的空实现。实现及验收状态仅见[执行状态](../execution/status.md)。

### 2.3 旧 ANI 行为到新版实现的映射

| 旧行为要求 | 新版保留的语义 | 新版实现要求 | 开放门禁 |
|---|---|---|---|
| Kata create/ready/pause/resume/delete | 生命周期动作和状态可观察 | Sandbox 持久意图 + Kubernetes Provider Apply/Observe；不使用本地 map | P2 真实集群 |
| 重启后查询任务、重复请求不重复创建 | 幂等和恢复 | DB operation/idempotency + lease + Provider UID/名称观察 | P1 fake，P2 live |
| `/workspace` 专属 PVC | 工作盘隔离、pause 后保留 | 冻结 PVC/StorageClass/reclaim，删除等回收 receipt | P2 |
| 路径、symlink、hard-link、rename-out 防护 | 工作区边界和文件安全性质 | Sandbox 自己的 file adapter 做 fd/容器边界测试，不能复制旧脚本 | P3 |
| Python/JavaScript code-run、输出上限 | 明确语言、超时、工作目录、结果状态 | Sandbox 自己的 exec adapter + operation；不依赖 Gateway 或其他服务 | P3 |
| 预览端口 | 受控 endpoint、TTL、撤销和清理 | Sandbox 自己创建受控 Service/endpoint；NodePort 只能实验开关 | P4 |
| HMAC token 绑定 tenant/instance/scope/expiry/jti | scope 和目标绑定 | Sandbox 自己的 signer 签发，稳定密钥和撤销/版本检查 | P3 |
| CSI filesystem checkpoint | 文件系统 crash-consistent 快照 | 独立 action，记录 Snapshot/PVC UID；不承诺内存/CRIU | P5 |
| 30 秒过期扫描和 session/idle 判定 | 到期语义 | 持久 `expires_at`/`last_activity_at` + lease/条件更新 worker | P4 |
| egress allowlist | 策略意图 | Sandbox 自己渲染和观察 NetworkPolicy；回显不算成功 | P4 |
| `/api/v1/sandbox/...` | Gov 的独立 Sandbox 入口 | 以租户和 Sandbox ID 确定范围，不依赖其他 Instance | P0 |

## 3. 资源解析与推理资源语义对齐

### 3.1 资源来源

Sandbox 创建请求必须携带 `preset_id`。Sandbox 在受理新请求时通过同一 `ani-resource-service` 进程内的 `SelectionResolver` port 调用算力目录的只读解析，不发起跨服务 RPC：

```text
tenant_id + preset_id + usage=SANDBOX
        -> ComputeSelection
        -> Sandbox frozen resource snapshot
        -> Runtime render
```

请求不能同时把任意 `cpu`/`memory` 数量作为另一套权威来源。若产品需要“直接输入资源”模式，必须另立版本化合同和准入规则，不能在 Sandbox 内绕过 compute 目录。

与当前代码的对应关系如下：

```text
internal/biz/compute.NewResolver(presetRepository, clusterID)
        -> *compute.Resolver
        -> Resolve(ctx, tenantID, presetID, usage)
        -> compute.Selection{CPUMillis, MemoryMiB, GPU, ...}
        -> Sandbox local selection + canonical selection_digest
```

`SelectionResolver` 是 Sandbox 内部 port，不是新服务；进程内 `*compute.Resolver` 直接实现该 port，返回 `compute.Selection`。入站 service adapter 按 `compute-selection-v1` 生成和校验 canonical digest，再冻结 Sandbox spec；protobuf 不进入 `internal/biz/sandbox`。`internal/service/compute` 暴露的 gRPC 仍是现有 Gov 入口，Sandbox 不通过该 gRPC 回调自己。可信 scope 来自 Gov 入站身份链，usage 按[算力目录合同](../specs/compute-presets.md)校验。


### 3.2 `SANDBOX` 用途

Sandbox 固定使用[算力目录合同](../specs/compute-presets.md)登记的 `SANDBOX` usage，一期只接受 CPU 选择；不得发送 `UNSPECIFIED`、静默复用 `CONTAINER_INSTANCE` 或把未知目标默认当作容器。候选资格、停用和绑定规则由该合同定义。

### 3.3 ComputeSelection 到 Runtime 资源

`ComputeSelection` 是一次只读候选，不创建资源、不占额、不产生受理 ACK。Resource 必须：

1. 校验 `schema_version=1`、`preset_id`、`compute_type`、正数 `cpu_millis`/`memory_mib` 和 `selection_digest`；
2. 把 `preset_id`、code、name、compute_type、CPU、内存、GPU（一期必须为 null）和 digest 原样写入 Sandbox snapshot；
3. 通过本地 canonical digest 校验候选，不能只相信展示字段；
4. 将 CPU 转为 `<cpu_millis>m`，内存转为 `<memory_mib>Mi`；
5. 一期渲染 `requests.cpu/memory` 与 `limits.cpu/memory` 为相同值，避免出现“目录显示值”和实际调度值不一致；
6. 不用 GPU 显存、profile 展示字段或推理模型参数替代主机内存；GPU 未来扩展必须有独立的 ACC/计费/调度合同。

例如 `cpu_millis=1500`、`memory_mib=2048` 只生成：

```yaml
resources:
  requests:
    cpu: "1500m"
    memory: "2048Mi"
  limits:
    cpu: "1500m"
    memory: "2048Mi"
```

渲染器不接受前端传入的原始 Pod resources，也不从默认值补齐缺失 CPU/内存。解析失败、digest 不匹配、usage 不合法、preset 不存在或已禁用都必须返回明确错误，不能回退默认规格。

### 3.4 资源语义的本地实现

推理服务的 `ResourceSpec` 只作为历史参考：CPU/内存使用 Kubernetes Quantity，managed 资源要求 requests==limits，GPU 是独立资源。Sandbox 在本地实现同样的语义，但所有类型、校验、快照和渲染代码都属于本仓库：

- 不导入推理 protobuf，不依赖推理服务运行、数据库或部署状态；
- `cpu_millis`、`memory_mib` 和 `selection_digest` 由 res 内部 `SelectionResolver` 提供；
- Sandbox snapshot 直接转换为自身 Kubernetes adapter 所需的 Quantity 或 typed fields；
- 模型、引擎、拓扑、副本、推理 API 和推理调度均不进入 Sandbox。

## 4. 对外调用与内部边界

### 4.1 Gov 入站与 Resource Sandbox RPC

公网入口由 Gov 提供，直接创建和管理 Sandbox：

```text
/api/v1/sandbox
/api/v1/sandbox/{sandbox_id}
/api/v1/sandbox/operations/{operation_id}
```

Gov 完成公网认证、产品授权、租户映射和 DTO 校验，再通过 Resource 已登记的 `sandbox.v1.SandboxService` RPC 传入 `tenant_id`、`actor` 和业务请求；`request_id` 由受信 metadata/context 校验并用于请求链路关联。Resource 的 `internal/server` 只做 TLS/mTLS、精确方法 allowlist、metadata 校验、deadline 和传输适配，不承载 Sandbox 状态机。具体 HTTP DTO、版本号和 RPC 名称在 P0 冻结。

| 方法 | 语义 | 幂等/权限 |
|---|---|---|
| `ResolveCreateSpec` | 校验 Gov 传入的 tenant，以及 Sandbox 自身的 image digest、preset、工作盘、策略和运行时参数，生成未受理的候选规格 | 认证创建路径可初始化共享租户 Namespace；不创建 Sandbox Pod/PVC、不占额 |
| `AcceptCreate` | 持久化 Sandbox 意图、冻结 snapshot、创建 operation | 原 `idempotency_key`；新请求才解析和冻结 |
| `GetSandbox`/`ListSandboxes` | 读取 DB 快照和观察状态 | 租户 + Sandbox ID 范围查询 |
| `GetOperation` | 读取持久 operation | 租户 + Sandbox ID 范围查询 |
| `Pause`/`Resume`/`Delete` | 产生可恢复 action | 租户 + Sandbox ID + 原键 + expected_version；不以请求返回代表 Provider 完成 |
| `ResolveDataPlaneTarget` | 向本组件自身的 file/exec/port adapter 返回当前有效 target、版本和 Pod UID | 进程内 port；不依赖 SG 或其他服务 |

Resource RPC 的身份由本服务的 transport middleware 注入并校验；Sandbox 不调用认证、授权或租户服务。请求中的 tenant/actor/request_id 只能来自 Gov 受信委托和本服务认可的 transport context，不能由业务字段覆盖。Gov 路径不经 Higress；Sandbox RPC 必须登记在 Resource 的 Gov 调用清单中。

### 4.1.1 创建请求和响应的最小字段

最终 HTTP/Proto 字段在 P0 冻结，但语义必须保持以下边界：

| 对象 | 必填字段 | 规则 |
|---|---|---|
| `ResolveCreateSpecRequest` | `tenant_id`、`preset_id`、`image_reference`；`workspace_size` 可选 | tenant 来自 Gov 受信委托；Resource 不查询其他 Instance。镜像须先在租户或平台 Image 目录登记，tag 或 digest 经本地 Image 能力授权并解析为不可变 digest；工作盘大小须为正数，省略时采用当前默认值。不接受原始 Pod YAML、CPU/内存 Quantity 或任意 Namespace；当前不支持通过请求选择 RuntimeClass |
| `CreateSpec` | tenant、Sandbox、image digest、`ComputeSelection`、workspace、egress、runtime、timeout、spec digest | 未受理候选；解析可初始化共享租户 Namespace，不创建 Sandbox Pod/PVC、不占额、不产生长期 token |
| `AcceptCreateRequest` | `create_spec`、`spec_digest`、`idempotency_key`、`expected_version`（如适用） | `request_digest` 必须是 `sandbox-create-v1` 对 tenant/resource/operation/空历史 instance 字段/spec 的 SHA-256；校验冻结规格摘要、原始请求摘要与候选一致；新键才冻结和持久化，原键返回原 operation/快照 |
| `Sandbox` | `sandbox_id`、tenant、generation、desired/observed、snapshot、operation 状态、Provider refs | 历史 `instance_id` 仅为兼容保留，不用于新资源归属；所有读取带显式 tenant 条件；Provider 状态和业务状态分开表达 |
| Action 请求 | `sandbox_id`、`idempotency_key`、`expected_version` | Resource 按 `sandbox-action-v1` 对空历史 instance 字段/expected_version/action 计算并持久化 request digest；同键不同请求返回冲突。Pause/Resume/Delete 都是异步 action；调用返回 accepted operation，不代表 Runtime 已完成 |
| `ResolveDataPlaneTarget` | tenant、sandbox、generation、action/scope | 只给本组件 adapter 当前有效目标、Pod UID、容器和短时授权信息；pause/delete/version 变化后旧目标失效 |

业务错误至少区分：`INVALID_ARGUMENT`（结构/单位/digest）、`UNAUTHENTICATED`（transport identity/metadata）、`PERMISSION_DENIED`（租户或 action）、`NOT_FOUND`（资源/镜像/preset）、`FAILED_PRECONDITION`（能力或状态不满足）、`ALREADY_EXISTS`（幂等冲突）、`UNAVAILABLE`/`DEADLINE_EXCEEDED`（基础设施故障）、`UNKNOWN`（Provider 结果不确定）。不能把 Provider 超时直接改写成成功或已删除。

### 4.1.2 摘要计算

Sandbox 不接受任意字符串作为 `request_digest`。新创建和动作的摘要使用原有版本和字段格式，但历史 `instance_id` 字段固定为空；请求按租户、Sandbox ID、operation、原始创建意图和冻结规格校验，同一幂等键的不同请求返回冲突。历史记录的摘要和引用不被改写：已受理的旧创建仅在原命令完全匹配时重放；旧动作按保存的历史摘要验证。尚未被 Resource 受理的旧父实例创建不能当作成功或自动改写，应保留待确认记录并单独核查。旧客户端带非空 `instance_id` 的普通查询和动作必须拒绝，避免按旧父资源授权的客户端误读整个租户的列表。

列表采用一基页码，默认 `page=1/pageSize=20`，pageSize 只允许 1–100；HTTP 显式 0、重复或非规范数字为 400，不做静默夹取。同次 count/items 必须使用一致数据库快照，按 created_at DESC、sandbox_id DESC 排序。已完成的 operation 结果保持 succeeded/superseded，后续健康漂移只更新资源观察。公共错误保持稳定 reason：缺失 404，幂等/版本/前置条件冲突 409，依赖不可用 503；RPC 原因需映射到同样语义。

### 4.2 Sandbox -> Kubernetes Provider

Kubernetes adapter 只接受 Sandbox 已冻结的业务 snapshot 和 opaque refs：

```text
PrepareMutation(work lease, mutation ID/kind/operation/version/target UID/attempt) -> durable pending
Apply(snapshot, one mutation) -> applied/rejected/not_sent/unknown receipt + opaque refs
Observe(refs) -> provider facts / UNKNOWN
GetCapabilities() -> supported runtime/storage/action matrix
```

每次 Apply 只允许一个子操作：create workspace、create pod、delete pod 或 delete workspace。worker 必须先持久化 pending，再调用 Provider，随后保存匹配回执。超时或传输结果不确定时保留 pending；创建对象暂时不存在不能关闭未知创建。只能通过归属和规格匹配的对象观察恢复该创建，再按最新 desired intent 回收。迟到回执只能合并匹配 journal 的身份事实，不能覆盖新意图、快照、operation 或接管者 lease。

UID 受限删除可重试，但每次发送前必须递增并持久化 `Attempt`，同时保留 `PreviousUncertain`。回执必须匹配当前完整 attempt；旧 attempt 的回执不能清除后继屏障。即使当前 attempt 明确未发送或被拒绝，只要仍有前次不确定性，就继续保留 pending；当前匹配的 Applied 或原目标 Gone 观察才可解除。已被清除或推进的 journal 不得由旧 Work 复活。成功 Apply 也不得复用写入前的 Ready/Gone 完成当前 operation：写入后进入等待观察，下一次 Observe 确认当前目标才可成功。

Provider 在构造时绑定受管配置的非零规范 ClusterID，每次 Observe/Apply 必须先与冻结 snapshot 的 ClusterID 相等；不匹配时返回 `SANDBOX_PROVIDER_CLUSTER_MISMATCH`，不发任何 Kubernetes 请求，不生成回收回执。kubeconfig 与逻辑 ClusterID 的物理映射仍由受信部署配置负责；此检查不提供物理集群指纹认证，也不宣称已验证误配 endpoint 的所有情形。

Provider adapter 不重新解析 preset、不读取其他服务数据库、不接受用户 Pod YAML、不决定外部租户配额。`Apply` 的成功只表示 Provider mutation receipt 已取得；最终状态以 `Observe` 的 UID、resourceVersion、phase、Ready、RuntimeClass、PVC 和本组件创建的 Service/NetworkPolicy refs 为准。

共享 Kubernetes 凭据除租户 namespace 内的 Pod/PVC 操作外，还需要命名空间 `get/create` 以初始化并校验租户归属，以及 `storageclasses.get/list` 选择唯一默认存储类；Kubernetes 不支持按名称限制 namespace 的 create，代码须严格限定推导名称和归属标签。回收观察需要 `persistentvolumes.get/list`，镜像拉取凭据还需要租户 namespace 内 Secret 的相应操作权限。不需要 PV/StorageClass 写权限。缺少观察权限时不能声明回收完成；权限和新增 migration 必须与代码发布配套，源码或模拟测试通过不代表目标集群已具备这些前置条件。

### 4.3 本组件内部 owner

| 组件 | 负责 | 不负责 |
|---|---|---|
| `internal/service/sandbox` | 入站请求适配、字段校验、use case 调用和响应映射 | 业务规则、Kubernetes、数据库细节 |
| `internal/biz/sandbox` | 租户边界、compute 解析端口、冻结 snapshot、状态机、operation、token/file/exec/port 端口 | Kratos、protobuf、HTTP、Kubernetes、SQL |
| `internal/data/sandbox` | PostgreSQL facts、worker lease、Kubernetes Apply/Observe、workspace、file/exec/port/checkpoint、token signer | 对外 API、业务授权决策 |
| `internal/server` | Sandbox HTTP/gRPC、transport identity、deadline、日志和健康检查 | Sandbox 状态机和 Provider 规则 |
| `cmd/ani-resource-service` | 装配本组件所有依赖和 worker | 跨服务编排或远程业务调用 |

### 4.4 res 仓库内的实现布局

```text
cmd/ani-resource-service/
  sandbox composition root
internal/biz/sandbox/
  types.go          # Sandbox、Snapshot、Operation、Provider facts 与 ports
  worker.go         # journal、恢复、单步生命周期推进
internal/service/sandbox/
  service.go        # transport DTO -> use case -> response
internal/data/sandbox/
  postgres.go       # facts、operation、idempotency、lease、cleanup receipt
  provider.go       # Kata/PVC/Pod/PV 的 Apply/Observe
  # 以下 adapter 随二期切片引入：
  filesystem.go     # /workspace 文件边界和 upload action
  exec.go           # Pod exec/command adapter 和输出限制
  endpoint.go       # Service/endpoint/TTL 以及撤销
  token.go          # 稳定密钥 signer 和 claims 校验
internal/server/
  sandbox_identity.go / sandbox_worker.go
  # 入站 gRPC 注册复用 governance.go，公共 HTTP 属于 Gov
```

`internal/biz/sandbox` 不得导入 Kratos、protobuf、Kubernetes client、SQL driver 或其他服务包；`internal/service/sandbox` 不得把业务规则写进 handler；`internal/data/sandbox` 不得从请求重新解释资源或绕过 use case；`internal/server/governance.go` 与 `sandbox_identity.go` 只处理 Gov 入站传输合同，不把 Gov DTO 带入 biz；`cmd` 只负责构造依赖、启动 worker 和关闭资源。这样 Sandbox 可以在同一 res 进程内独立启停、测试和恢复，而不会形成跨服务业务耦合。

## 5. Sandbox 冻结快照

下表区分一期与后续目标；创建输入保持冻结，观察、引用和操作事实独立更新。二期专属字段随各自切片增加，不能据此推断一期已提供该能力：

| 类别 | 字段 |
|---|---|
| 归属和版本 | `tenant_id`、`sandbox_id`、`generation`、`operation_id`、`idempotency_key` 摘要、`request_digest`；`request_id` 保留在受信请求上下文 |
| 算力 | `preset_id`、preset code/name、`compute_type`、`cpu_millis`、`memory_mib`、GPU selection（一期 null）、`selection_digest` |
| 镜像和运行时 | image ID/digest、允许的 RuntimeClass、cluster、namespace、service account、Provider spec digest |
| 工作盘 | volume ID、PVC name/UID、容量、StorageClass、volume mode、reclaim policy、mount path `/workspace` |
| 生命周期 | 一期：desired/observed state、created/deleted timestamps；P4：`expires_at`、`last_activity_at`、session/idle timeout、`on_timeout` |
| 网络和数据面 | egress policy 摘要、Sandbox 自己创建的 NetworkPolicy/Service refs、端口声明、data-plane target version；不保存长期 token |
| 安全和环境 | 受控 Secret/env 引用、非特权安全上下文、允许 action/scope、资源和操作版本 |
| Provider 事实 | Deployment/Pod/PVC/Service/Network opaque refs、UID、resourceVersion、phase、Ready、last observed error |
| 回收 | delete intent、各子资源清理状态、snapshot/volume/network cleanup receipt、unknown/retry reason |

快照 digest 应覆盖所有影响执行的字段。重试、恢复、Start/Stop 或 Resume 不重新读取当前 preset、镜像别名、Namespace 或网络默认值。

### 5.1 工作盘与 PVC 的关系

工作盘和 PVC 是同一条存储链路的两个层次：

```text
Sandbox workspace（业务概念）
  -> PVC（Kubernetes 持久卷声明，Sandbox 绑定的 Provider 对象）
      -> PV / Storage backend（集群实际分配的持久存储）
```

- **工作盘**描述 Sandbox 看到的逻辑文件系统，例如挂载到 `/workspace`、容量、读写模式和生命周期；
- **PVC**是工作盘在 Kubernetes 中的持久化绑定对象，负责请求容量、StorageClass、访问模式和回收策略；
- **PV/存储后端**是集群实际提供数据的对象，通常不由 Sandbox 直接创建或操作；
- 一期一个 Sandbox 的工作盘对应一个由 Sandbox 管理的 PVC。pause 只停止运行负载，保留同一个 PVC；resume 重新挂载原 PVC，不能新建空 PVC；
- 暂停以归属明确的 Pod 已停止且没有未决 mutation 为完成条件，不要求保留盘 Bound。WFFC Pending 可在无 Pod 时暂停，恢复时由同一 PVC 的 consumer 促成绑定；Lost/Missing/Deleting 等存储诊断和原 UID 保留。工作盘异常或 PV 只读查询失败不阻止对已确认原 Pod 的 UID 受限停止，但不允许 Resume 用空盘替代缺失的原盘；未知创建仍必须先恢复归属，不能因为暂时无 Pod 就完成暂停。PV 查询失败时保留原存储身份及诊断，删除仍须等待后续存储观察和回收证据，不能将读取失败视为 Gone；Pod 读取失败、归属或 UID 冲突及 cluster 不匹配仍拒绝写入；
- restore/clone 可以产生新的 PVC，但必须记录 source PVC/Snapshot UID 和新 PVC UID；
- 删除 Sandbox 先删除或释放运行负载，再按冻结的 reclaim 策略处理 PVC，并等待 PVC/PV/快照的回收 receipt；不能因为删除接口返回成功就认为工作盘已经清理。

因此，snapshot 同时保存工作盘的业务字段和 PVC 的 Provider 引用：`mount_path`、容量、StorageClass、access mode、reclaim policy、PVC name/UID、PV/VolumeSnapshot 引用。工作盘状态由 Resource 持久化意图和 Kubernetes Observe 结果共同决定。

Pod 消失后可清空活动 Pod UID，以便恢复工作负载。已知 PVC 的 UID 和原 PV name/UID 必须保留；非删除意图下丢失原 PVC 返回 `WORKSPACE_MISSING`，不得创建空盘替代。`workspace_bindings` 保留原 PVC 身份和 cleanup receipt；一期没有工作盘替换动作。删除必须等待原 Pod、PVC、PV 全部 Gone 且没有未定 mutation，才可终止工作项。StorageClass 和实际绑定 PV 的 reclaim policy 均需为 `Delete`；PV Gone 是 Kubernetes 回收证据，不等同于存储后端字节级擦除证明。

### 5.2 持久化对象

数据库表名和迁移编号在实现阶段确定，但至少要有以下逻辑对象，且每个对象都带 `tenant_id` 或通过受保护的 Sandbox 外键归属租户：

| 对象 | 关键字段 | 作用 |
|---|---|---|
| Sandbox intent | ID、tenant、generation、desired state、create request digest、frozen snapshot、created/deleted time | 保存用户意图和不可变执行输入；不建立 Instance 外键 |
| Sandbox observation | sandbox ID、Provider refs/UID/resourceVersion、phase、ready、runtime/storage/network facts、last error | 保存 Provider 观察，不由请求线程伪造 |
| Operation | operation ID、action、state（queued/running/retrying/succeeded/unknown/superseded）、attempt、lease、deadline、error detail、started/finished time | 支撑异步动作、重试、查询和重启恢复；新动作替代仍在执行的旧动作时，旧 operation 标记 `superseded` |
| Idempotency record | tenant、scope/action、key、request digest、operation ID、response digest、expiry | 原键返回原结果；同键不同请求返回冲突 |
| Workspace binding | volume ID、PVC name/UID、StorageClass、size、reclaim、mount、cleanup receipt | 绑定工作盘和回收证据 |
| Runtime reference | provider kind/name/UID、cluster、namespace、resourceVersion、owner generation | 通过 Observe 恢复资源身份，避免重复 create |
| Provider mutation journal | mutation ID、单一 kind、operation/version、目标 UID、pending 与回执 | 写前持久化；unknown 保留屏障，防止迟到创建泄漏 |
| Expiry lease（P4） | sandbox、expires_at、last_activity_at、on_timeout、lease owner/version、last decision | 支撑过期 worker 的抢占和幂等处理 |

一期 Provider 资源名称使用受控前缀加 Sandbox UUID（`workspace-<id>`、`sandbox-<id>`），不直接拼接用户名称；Sandbox version/generation 和 Provider UID 仍写入事实，用于动作隔离和迟到对象校验。二期 clone/restore 若引入新 generation，必须为新资源分配新名称。删除和回收 receipt 要保留足够时间支持审计和迟到对象清理。

## 6. 状态、持久化与恢复

### 6.1 状态建议

业务状态和 Provider 观察状态分开保存：

```text
PENDING -> CREATING -> READY -> PAUSED -> READY
                    └-> FAILED
PENDING/CREATING -> PAUSED（无未决 mutation、运行负载已停止；工作盘允许 Pending）
READY/PAUSED -> DELETING -> DELETED
任意非终态 -> UNKNOWN（Provider 或 worker 结果不确定）
```

`expired` 是业务结果或 reason，不替代 Provider 状态；到期动作（pause/kill）完成并核对资源后，才更新最终业务状态。Provider 2xx 不等于已 Ready，删除请求返回不等于资源已回收。

### 6.2 DB 和 worker

- PostgreSQL 保存 intent、snapshot、desired/observed、operation、idempotency、lease、mutation journal 和 cleanup receipt；expiry 在 P4 引入；
- worker 通过租户范围查询非终态记录，以 lease/条件更新抢占，不使用进程内扫描状态；
- PVC、Pod、Deployment、Service、NetworkPolicy、VolumeSnapshot 的真实状态只来自 Kubernetes Provider 的查询结果，并按 name/UID/resourceVersion 恢复；不能从本地文件、内存对象或上一次请求结果推断 `Bound`、`Ready`、`Deleted` 或 snapshot 完成；
- Provider unknown 时保留原 refs、请求摘要和最后错误，下一次 Observe 先恢复身份，再决定重试或人工介入；
- 同一幂等元组 `tenant_id + sandbox_id + version + action + idempotency_key` 不产生第二个 Sandbox 或第二套 Provider 资源；
- 只要 Pod/PVC/Service/Network 任一回收条件未确认，业务状态不能标记 `DELETED`；
- 所有查询和写入带显式 tenant predicate，禁止依靠 RLS、默认租户或进程配置填补归属。

## 7. 安全和数据面约束

1. 目标校验至少比较 tenant、sandbox generation/version、Pod UID、container 和允许 action；pause/delete 后旧 data-plane target 必须失效。
2. 文件操作限制在 `/workspace`，覆盖规范化路径、symlink、hard-link、rename-out、父目录替换、大小上限和上传幂等。
3. code-run 只接受白名单语言、超时、输出上限和工作目录；容器不具备宿主机权限，Sandbox 进程不依赖 `kubectl` 二进制。
4. token 由 Sandbox 自己的 signer 签发，claims 至少包含 tenant、sandbox version、scope、expiry、jti；没有稳定签名密钥时拒绝启用，不生成进程内随机 fallback。
5. 一期 CPU-only；GPU 需要 ACC profile、设备数、cluster/pool、计费和 runtime 调度合同，不能把 `gpu` 字段或显存展示值直接拼到 Sandbox Pod。
6. egress 的“策略已保存/已回显”与“数据面已生效”分开记录；Kubernetes Provider 无法下发时返回明确失败，不伪造 allowlist 生效。

## 8. 分阶段验收门禁

P0～P2 对应一期核心 Sandbox；P3～P5 对应二期数据面能力。每个二期 action 都可以独立发布和回滚，不要求一次性开放全部数据面。

### P0：契约审核

- 审核并冻结本文、Sandbox API、Sandbox -> Kubernetes Provider 合同和 snapshot digest；
- 决定 `SANDBOX` usage 或显式复用 `CONTAINER_INSTANCE`，同步 res 内部 compute 合同、transport identity 和测试；
- 在 `docs/specs/governance-calls.md` 和 Resource 精确 allowlist 登记 Sandbox RPC；使用 Gov 公网路径 `/api/v1/sandbox/...`，验证 tenant/actor/request_id、Sandbox ID 和 deadline；
- 明确 image、workspace、Kata、StorageClass、CSI、NetworkPolicy 和本地 data-plane adapter 的 capability matrix；
- 规定旧 ANI 每条 evidence 的 L1/L2/L3 等级和新 live gate 格式。

Proto 生成、Resource 精确 allowlist、SANDBOX compute usage、租户边界、幂等摘要和错误映射，以及 Gov -> Resource 真实 mTLS/metadata 联调和目标集群 capability matrix 均属于退出条件；执行结果按唯一状态记录。

### P1：Sandbox 事实和恢复骨架

- 新建 `internal/biz/sandbox`、`internal/data/sandbox`、`internal/service/sandbox`，保持分层边界；
- 实现 DB intent/snapshot/operation/idempotency、workspace binding、lease 和 cleanup receipt；
- 接 Runtime fake，覆盖重复 create、跨租户、unknown、重启、删除重试和过期条件更新；
- P1 只建立生命周期和恢复骨架；端口、code-run、egress、checkpoint 暂不开放，调用时返回明确的 `UNSUPPORTED` 或 `FAILED_PRECONDITION`。

状态模型、幂等、lease、unknown 和恢复代码需要对应测试；真实 PostgreSQL/Kubernetes 重启恢复、删除回收 receipt 和多副本门禁须分别记录实际结果，不能由代码存在推断通过。

### P2：Kata CPU 生命周期

真实 gate：

```text
create -> Kata Pod Ready -> local data-plane target validation
-> write /workspace -> pause -> resume -> file retained
-> delete -> Pod/Deployment absent -> PVC/reclaim receipt
```

还必须验证租户隔离、重复请求不产生第二个 Deployment、Provider 超时可恢复、旧目标在 pause/delete 后失效。

退出条件：Kata Pod Ready、PVC Bound、RuntimeClass、workspace 写入保留、pause/resume 和删除回收均有 L3 证据；实际验收需记录 image digest、cluster、namespace、UID、RBAC、网络和清理结果，并链接到唯一执行状态。

### P3：文件、Exec、code-run 和 token

通过安全测试和真实 Pod gate 后，才开放 Sandbox 自己的数据面 adapter。验证路径穿越、symlink/hard-link、输出上限、语言/超时、token scope、过期、版本变化、跨租户和密钥轮换。

退出条件：文件边界测试覆盖父目录替换和 rename-out；code-run 在 Ready Pod 中成功并能处理暂停、超时、输出超限和缺少语言运行时；token 不接受跨租户、跨 Sandbox、过期或旧 generation。

### P4：端口、egress 和生产过期

端口由 Sandbox 自己创建和观察受控 Service/endpoint；NodePort 只能实验开关。NetworkPolicy 由同一 Kubernetes adapter Prepare/Observe/Release。重启 worker/Resource 后仍能完成 session/idle expiry，且重复扫描不重复操作。

退出条件：端口创建、访问、撤销、TTL 到期和清理有独立证据；egress deny/allowlist/internet 分别有 Provider 观察结果；到期动作在服务重启和 Provider unknown 后仍能恢复。

### P5：filesystem checkpoint/restore/clone

前置条件是 CSI VolumeSnapshot CRD/controller、SnapshotClass、RBAC、Workspace StorageClass 和回收策略已通过预检，且 P1 durable operation、P2 pause/resume、P4 cleanup receipt 已通过。

验证顺序为：写入 `version-1` -> snapshot ready -> 写入 `version-2` -> restore -> 读回 `version-1` -> clone -> 新 Sandbox 读回 `version-1` -> 删除并确认受管对象归零。只承诺 CSI filesystem crash-consistent；记录 PVC/Snapshot UID、RBAC、等待预算、失败和回收 receipt。没有 memory/CRIU 证据时，接口返回 `UNSUPPORTED`，不做隐含承诺。

### 8.1 所有阶段通用门禁

1. **契约门禁：** pinned protobuf 生成、API compatibility、错误映射、分层依赖和 `make verify`。
2. **状态门禁：** desired/observed、operation、lease、unknown recovery、重启后查询和幂等 replay。
3. **事实源门禁：** 清空内存、删除临时文件并重启服务后，业务状态从 PostgreSQL 恢复，PVC/Pod/Service/Snapshot 状态从 Kubernetes Provider 恢复；任何 local map/file 变化都不得改变结果。
4. **安全门禁：** 跨租户拒绝、错误 kind/state/provider 的明确错误、token scope/version、目标 UID、路径和端口边界。
5. **Provider 门禁：** Kata RuntimeClass、Pod Ready、PVC Bound、Service/Network 观察、UID ownership、删除和回收 receipt。
6. **故障门禁：** 请求超时、Provider unknown、worker 重启、CSI 未 ready、RBAC 不足和迟到对象；任何未知写入不得自动重复 create。
7. **证据门禁：** 每项记录区分 L2 local/logic 与 L3 live；旧 ANI evidence 只能作为输入，不替代 Sandbox/Provider gate。

## 9. 非目标与实施前检查

- 公网 Sandbox API 由 Gov 的 `/api/v1/sandbox/...` 提供，Resource 只提供本组件自己的 Sandbox RPC，不放到 Higress；
- 不把旧 Gateway handler、旧表、local runtime map、随机 token key 或镜像内 `kubectl` 搬入本组件；
- 不用 local 文件、JSON/YAML 状态文件、SQLite、嵌入式 KV 或 `/tmp` 文件替代 PostgreSQL 和 Kubernetes Provider；`/workspace` 只能来自 PVC 挂载，不能使用宿主机路径；
- 不把 Notebook Instance 的字段和生命周期直接当成 Sandbox 合同；
- 不把 Sandbox 业务规则、状态机或数据库模型塞入 Instance；新建和管理均不依赖其他 Instance；
- 不通过远程 compute、auth、image、network、session、runtime 或 inference RPC 获取创建前置条件；compute 只走本进程内 port；Gov 仅负责入站治理和 DTO 委托；
- 不把推理服务的 protobuf、模型/引擎/副本组合引入 Resource；
- 不在本规格中修改远端仓库、提交、推送或创建 PR；远端动作须另行获得授权。

上线或进入 Gov 联调前必须完成：

1. `SANDBOX` usage 方案和兼容版本评审；
2. 本组件 transport identity、deadline 和精确 API 行为测试；
3. compute selection digest/CPU/内存转换测试；
4. `make verify`、生成代码检查和跨租户/幂等测试；
5. 目标集群 capability matrix 及第一个 P2 live gate 的清理证据。

## 10. 设计审核结论与来源

| 项目 | 设计约束与来源 |
|---|---|
| 方案方向 | 独立 Sandbox 组件、冻结输入与 Provider 事实分离；按 P0～P5 门禁逐项验收 |
| 新版 `ani-resource-service` | 实现、部署和验收状态仅见[执行状态](../execution/status.md)，本文不维护第二份进度 |
| 旧 ANI | `138887aaa096cf38512b4c68a7bab58d8ec1ac72`；仅作为行为和验收输入 |
| 新 Resource 历史 Sandbox 设计 | `198f6afe536e46be5acbf5acbcbfc17ce17787e6`，仅在 `backup/main-before-sync-20261018` |
| 生产开放条件 | 对应阶段 L3 live gate、租户/幂等/恢复/删除回收证据齐全；旧 `LOCAL_VERIFIED` 不可替代 |

新版 Resource 正式依据：

- `docs/specs/compute-presets.md`：算力 preset、`ResolveComputePreset`、`ComputeSelection`、CPU/内存单位和 selection digest；
- `docs/specs/instance-mvp.md`：只复用冻结执行资源、requests/limits 和清理语义；
- `docs/specs/governance-calls.md`：仅用于确认 Sandbox 不继承 Gov 的跨服务调用边界，不作为 Sandbox 运行时依赖；
- `docs/specs/instance-configuration-network.md`：只复用 workspace、配置和 Provider 观察原则，不接入 Instance/Network 服务；
- `docs/adr/0002-use-tenant-owned-data-without-rls.md`：显式 tenant predicate；
- `docs/adr/0004-observe-cr-with-durable-reconciliation.md`：PostgreSQL 事实、Provider Observe、unknown 和恢复；
- `docs/adr/0006-evolve-resource-service-preserving-network.md`：按业务域模块化，不做共享运行时模型。

推理资源语义参考（外部仓库，不作为 Resource 运行时依赖）：

- `/root/kubercon/ani-inference-service/api/inference/v1/inference.proto` 的 `ResourceSpec`；
- `/root/kubercon/ani-inference-service/api/inference/contract/v1/managed_contract.go`；
- `/root/kubercon/ani-inference-service/docs/contracts/compute-spec-scheduling-handoff.md`。

旧 ANI 行为证据来源：

- `development-records/instance-sandbox-adapter-a.md`、`instance-sandbox-stateless-a.md`、`instance-sandbox-kata-storage-a.md`；
- `development-records/instance-sandbox-file-safety-a.md`、`instance-sandbox-file-safety-live-gate-a.md`；
- `development-records/instance-sandbox-coderun-a.md`、`instance-sandbox-ports-a.md`、`instance-sandbox-token-a.md`；
- `development-records/instance-sandbox-checkpoint-a.md`、`sandbox-expiration-egress-a.md`；
- `development-records/live-evidence/instance-sandbox-*.json`；
- `pkg/adapters/runtime/kubernetes_sandbox_runtime.go`、`sandbox_expiration_controller.go`、`pkg/security/sandboxtoken/token.go` 和旧 Gateway Sandbox 路由。

本文不授权任何远端提交、推送或 PR 操作；后续远端动作须单独获得授权。
