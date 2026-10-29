# ANI Resource 文档导航

GPU 容器实例 v1.5：[规格](specs/instance-mvp.md)、[实施计划](plans/instance-mvp-v1.5.md)、[运行与恢复](runbooks/instance-mvp-fedora.md)、[当前执行状态](execution/status.md#gpu-container-instance-mvp-v15)。本轮按用户调整只负责后端，前端接入交由专人。

容器最终删除与退额：[2026-10-27 整改方案](plans/container-instance-release-without-acc-20261027.md)移除 Resource 对 ACC 释放确认的依赖，以完整 owner 清理事实和持久 outbox 复用 Gov 现有退额接口；正式行为见[实例规格 §10](specs/instance-mvp.md#10-完整释放及-gov-原账退额)，实际实施与验证只见[执行状态](execution/status.md#container-instance-owner-refund-2026-10-27)。

容器实例配置与 VPC 接入：[增量设计](specs/instance-configuration-network.md)、[IC-01～IC-05 计划](plans/instance-configuration-network.md)；实施与真实验收只看[本任务状态](execution/status.md#instance-configuration-and-network-2026-10-19)。

这是本仓库唯一正式文档导航。项目入口为 [README](../README.md)，工程约定为 [AGENTS](../AGENTS.md)。
当前实施与验证进度只在 [执行状态](execution/status.md) 维护，本文不复制进度表。

[算力目录与公共解析](specs/compute-presets.md)定义 compute 管理、列表、用途绑定的只读候选及与各消费者的组合边界；[原地迁移与恢复](runbooks/compute-presets-migration.md)定义规格表/账本与匹配 Gov 权限维护步骤。实例 v2、配置与生命周期合同继续有效。

[ani-resource-service 改名与模块整理计划](plans/resource-service-modularization.md)：原R0–R5沿用现有仓库和Kratos分层整理Network，并验证旧客户端、旧数据、资源接管和回退；本次Compute增量以[算力目录合同](specs/compute-presets.md)为准，Storage仍待后续切片。

容器重启与已有 PVC 挂卸：[生命周期](specs/instance-mvp.md#84-restart-与挂卸操作)、[五个 RPC 与动作权限](specs/instance-mvp.md#111-用户-api)、[当前配置/引用代际/Storage 删除保护](specs/instance-configuration-network.md#43-持久化和投影)、[008 切换与向前恢复](runbooks/instance-mvp-fedora.md)、[精确服务调用登记](specs/governance-calls.md)。实施输入仍为[已审 v2.0 方案](plans/container-instance-restart-pvc-20261028.md)，来源 SHA-256 `b6dfb17aef74f3d99cc969ecd2ef51b19c77b0a984f82fbe6afd28aa94d41f7a`；部署、A35/A36 边界和实际验收只见[唯一执行状态](execution/status.md#container-restart-pvc-20261028)。

## 阅读顺序

1. [领域词汇](../CONTEXT.md)：网络资源、消费者、操作、资源状态与身份角色。
2. [首片规格：VPC/Subnet](specs/vpc-subnet.md)：产品规则、REST/RPC、数据、状态、恢复、接入和验收。
3. [架构决定](#架构决定)：已由用户确认的所有权、数据和身份接入取舍。
4. [纵向实施计划](plans/vpc-subnet.md)：NET-01 至 NET-06（含新增 NET-05A）与 NET-AUTH 的依赖和退出证据；[NET-05A 专案](plans/cr-observation.md)安排持续观察改造。
5. [执行状态](execution/status.md)：当前工作包、下一步、验证范围与记录索引。

公网出网增量：[租户 VPC SNAT 方案](specs/vpc-snat.md)定义 Overlay/Underlay、平台与租户接口及生命周期；[实施计划](plans/vpc-snat.md)安排依赖和后续 Underlay 验收；[操作手册](kc-public-egress-manual.md)提供产品 RPC 顺序及 kc CR/YAML 核对参考；[出网部署前置](../deployments/egress/README.md)说明额外 RBAC 和只读节点事实来源。

2026-09-14 新统一设计：[VPC 基础内网、公网出站与 LB 方案](specs/vpc-connectivity-lb.md)定义新规则及对旧流程的调整；[13 个可执行任务](plans/vpc-connectivity-lb.md)给出依赖、修改范围与验收条件；[方案交付与来源核对](execution/records/2026-09-14-vpc-lb-plan.md)记录固定代码和手工实测的证据边界。本文档交付不代表新流程已实现。

[LB 监听器延迟下发](specs/lb-deferred-activation.md)定义空监听器保存、运行投影、清空与再激活合同；实际进度只见 [LB-DEFERRED-01](execution/status.md#lb-deferred-01)，证据入口为 [工作包索引](execution/records/LB-DEFERRED-01/README.md)。

[历史未知创建恢复提案](plans/provider-unknown-create-recovery.md)区分退出结果落库修正与旧回执缺失的恢复前提；本次实验例外已按用户授权恢复并复测；通用管理接口未实施，实际结果见执行状态。

[Sandbox 未知创建只读诊断](runbooks/sandbox-unknown-create-review.md)输出目标、凭据身份及 Provider 观察，保留未知屏障；报告本身不是持久审计或恢复证明。

[Sandbox Runtime 完整设计方案](plans/sandbox-runtime-design-20261025.md)是 Sandbox 唯一设计入口，已把旧 ANI 行为和 live evidence、新版 Resource 的 compute 解析/冻结快照/租户边界/Provider 恢复模型，以及推理服务资源语义合并在一份文档中。一期核心链和二期数据面能力分别按方案门禁推进；实际实现、部署和验收仅见[当前执行状态](execution/status.md)。

## 架构决定

| 记录 | 决定 |
|---|---|
| [ADR-0001](adr/0001-own-network-lifecycle.md) | Network 自己拥有资源、operation、worker、状态、恢复和真实删除，Core 不兜底；实例 owner 仍拥有 Pod/VM。 |
| [ADR-0002](adr/0002-use-tenant-owned-data-without-rls.md) | 独立 PostgreSQL + sqlc/pgx；租户资源显式 tenant 约束，无 RLS、共享写表或跨服务 FK。 |
| [ADR-0003](adr/0003-defer-workload-authentication.md) | 本期暂缓服务间身份验证，IAM 就绪后单独接入；租户业务边界继续实现和测试。 |
| [ADR-0004](adr/0004-observe-cr-with-durable-reconciliation.md) | 共享 CR 观察与 Network 持久执行，NET-05A 位于 NET-06 前；兄弟服务框架自主选择，配额延后到 Core 重构之后。 |
| [ADR-0005](adr/0005-separate-vpc-connectivity-and-exclusive-eip-bindings.md) | 用户确认 EIP 目标独占，VPC 内网/公网 SNAT 分用途限制，基础内网资源随 VPC 管理。 |
| [ADR-0006](adr/0006-evolve-resource-service-preserving-network.md) | 原历史上演进 Resource 单进程，保留 Network 数据、契约、资源身份与回退边界。 |

ADR 记录已确认的方向及理由；规格中本轮补齐的数值、字段和协议细节是工程设计，不冒充已经逐项人工批准或实际验收。
当前用户明确决定优先。规格、ADR、代码或证据出现差异时，标明是待实现目标、过期材料还是需要变更的决定，并更新对应权威材料，不能静默挑选有利版本。

NET-05A 的观察、时效、调度与增量验收统一见[持续观察规格](specs/cr-observation.md)；既有资源与 Attachment 协议仍以首片规格为准。实际源码、分层验收和容量边界见 [NET-05A 实施记录](execution/records/NET-05A-implementation.md)，当前结果只看执行状态。

管理员逐条手工复现三项 Public 转发问题：[命令与独立 YAML](runbooks/public-forwarding-manual-20260917/README.md)。此入口是手工 Provider 诊断，准备时未执行新资源创建或流量复验，不替代产品验收。

## 文档职责

| 位置 | 唯一职责 |
|---|---|
| 根 `CONTEXT.md` | 领域词汇定义，保持简短，不放 SQL、传输字段或执行步骤 |
| `docs/specs/` | 应有行为、契约、数据和验收条件；不维护实施进度 |
| `docs/adr/` | 重要取舍的上下文、决定和理由；不另复制整份规格 |
| `docs/plans/` | 工作包、依赖、实施顺序和退出证据 |
| `docs/execution/status.md` | 唯一当前状态及下一步 |
| `docs/execution/records/` | 带日期/工作包的来源与执行证据，保留时点与限制 |

正式材料集中在 docs；不新增另一个根 evidence 目录或 .scratch 当前规格。
源码契约、SQL、migration 和测试保留在其实际源码位置，由规格链接而非复制。
目录只在有实际文件时创建。过期规范注明替代关系；历史执行记录不随当前状态变化而重写成新的证据。

## 运行与来源

- [Governance VPC 只读接入](specs/governance-vpc-read.md)：用户/API Key 的受信 mTLS 查询入口；原始验证见 [2026-09-22 记录](execution/records/governance-aksk-vpc-20260922/README.md)。
- [Governance → Resource 调用登记](specs/governance-calls.md)：现有 Network/Image 方法、唯一证书身份、租户范围、超时和幂等，以及各装配模式共用业务 mTLS 的边界。

- [NET-VPC-LB-02 实现与分层验收](execution/records/NET-VPC-LB-02/README.md)：固定输入、LB 持久生命周期及受控/真实数据面边界；[隔离产品 API 操作](../deployments/load-balancer/README.md)；[Public 转发失败排查交接](execution/records/NET-VPC-LB-02/device-registration-20260915/public-forwarding-followup.md)。

- [NET-VPC-BASE-01 实施与受控验收](execution/records/NET-VPC-BASE-01/README.md)：六卡实现、固定源码、分层验收、补齐工具及下一批输入；[Intranet 平台运行前提](../deployments/egress/intranet.md)。

- [VPC SNAT 本仓实现与验证](execution/records/VPC-SNAT-IMPLEMENTATION/README.md)：固定设计输入、接口/事务/worker 落点、远端必要门禁、kc 外部阻塞与清理证据。

- [2026-09-10 kind Overlay EIP/Snat 实测](execution/records/KC-OVERLAY-20260910T114200Z/README.md)：原始流程空下一跳失败、测试路由对照、实际流量与清理差异。
- [NET-05 普通容器真实网络验收](execution/records/NET-05-implementation.md)：实际 main、数据面、故障、权限与清理证据。
- [运行说明](runtime.md)
- [运行验证](runtime-verification.md)：已有通用骨架门禁的范围。
- [远程执行约定](remote-execution.md)：通用规则与本轮 Goal 的更严格边界；Resource 改名本轮重任务必须在 Fedora，不自动回退本地。
- [生成溯源](scaffold/provenance.md) 与 [运行依赖 SBOM](scaffold/bom.cdx.json)
- [2026-09-09 源码评估](execution/records/2026-09-09-source-assessment.md)：ANI、IAM、Notification、kc 的设计输入与快照。
- [2026-09-09 文档交付检查](execution/records/2026-09-09-design-verification.md)
- [2026-09-10 CR 持续观察选型](execution/records/2026-09-10-cr-observation-selection.md)：选型研究与社区主源；已由 ADR-0004 和持续观察规格承接设计，配额讨论仅保留为后续输入。
- [2026-09-10 NET-05A 方案检查](execution/records/2026-09-10-cr-observation-plan.md)：文档交付、编排与静态检查，不代表实施验收。

本仓库独立拥有生成源码，不在构建或运行时依赖 layout 或 ANI 内部目录。
旧版 START-HERE 中的初始化交接及待讨论问题由上述词汇、ADR、规格和来源记录取代；历史版本仍可从 Git 查询。

## VM MVP

- [虚拟机一期规格 v2](specs/vm-mvp.md)：Linux/Windows、ISO 与 qcow2/raw、Block/RWO 自有盘、多盘、VNC、原生热插拔、在线/离线快照恢复及租户主网的唯一业务规则。
- [VM v1 正式合同](contracts/vm-v1.md)：API、action/usage/codec、compute、Network v3、可信 console binding 及 Gov 删除持久受理栅栏的唯一字段/接线规则。
- [纵向实施计划 v2](plans/vm-mvp.md)：VM-00 至 VM-05B 共16卡；先贯通真实Linux主链，再补齐一期范围。
- [整批执行 Goal](plans/vm-mvp-complete-goal-20261023.md)：本轮完整范围和授权边界；以当前用户指令补充。重任务仅ssh fedora、集群仅172.16.101.10～12、Git传输仅SSH；按依赖连续执行16卡，不逐卡询问。共享runtime/组件变更和正式发布仍须取得具体批准；旧环境Block缺项须在10～12重新核验。
- [验收矩阵 V01–V25](verification/vm-mvp-acceptance.md)：CODE/PG/BOUNDARY/LIVE/GUEST/BROWSER及双系统必需格子，含Gov删除投递暂停期间的受理栅栏；本文件不维护执行结果。
- [制包来源](references/vm-mvp-sources.md)、[设计复核历史](execution/records/VM-MVP/design-review-20261023.md)、[制包校验历史](execution/records/VM-MVP/package-validation-20261023.md)：固定输入与历史证据，不作当前环境或实施状态。
- [首卡 Goal](plans/vm-mvp-goals/VM-00.md)、[维护窗口 Goal](plans/vm-mvp-goals/VM-00R.md)；其余16卡文件在同目录。单卡范围按完整Goal连续执行授权组合。
- 空白模板：[环境输入](plans/vm-mvp-templates/environment-inputs.yaml)、[runtime变更准备单](plans/vm-mvp-templates/runtime-change-plan.md)、[任务报告](plans/vm-mvp-templates/task-report.md)。填写后的输入、变更单、报告统一放入docs/execution/records/VM-MVP/。
- [VM候选部署交接](execution/records/VM-MVP/vm-candidate-deployment-handoff.md)：实际候选来源、依赖顺序、迁移/密钥和部署/回退边界；Resource/Gov/SG实现任务ref及三个Harbor镜像已发布并核对digest，实际引用和未验/未部署边界以交接回执为准，不复制验收矩阵。
- 当前结果仅见[唯一执行状态](execution/status.md#vm-mvp)。

### 文档接入与唯一状态

不可变输入包为ani-vm-mvp-v2-20261023.zip，SHA-256为ef781d20e31f02ecdd6e3e0f98dcd3bd77ee38a3b790b0a69b0ef86ecd49a10d。VM-00按下表将正式内容按差异接入，替换旧VM规格和计划中的相反规则，保留无关内容；不覆盖根README，不修改AGENTS或remote-execution。迁移后的Markdown链接及Goal文字路径均指向仓内目标；包README引用统一指本VM入口。

| 包内文件 | Resource目标位置 |
|---|---|
| docs/specs/vm-mvp.md | [docs/specs/vm-mvp.md](specs/vm-mvp.md) |
| docs/contracts/vm-v1.md | [docs/contracts/vm-v1.md](contracts/vm-v1.md) |
| docs/plans/vm-mvp.md | [docs/plans/vm-mvp.md](plans/vm-mvp.md) |
| docs/verification/acceptance.md | [docs/verification/vm-mvp-acceptance.md](verification/vm-mvp-acceptance.md) |
| docs/references/sources.md | [docs/references/vm-mvp-sources.md](references/vm-mvp-sources.md) |
| docs/review/consistency-review.md | [docs/execution/records/VM-MVP/design-review-20261023.md](execution/records/VM-MVP/design-review-20261023.md) |
| docs/review/package-validation.md | [docs/execution/records/VM-MVP/package-validation-20261023.md](execution/records/VM-MVP/package-validation-20261023.md) |
| goals/*.md | docs/plans/vm-mvp-goals/，保留16个原文件名 |
| templates/* | docs/plans/vm-mvp-templates/，保留3个原文件名，模板保持空白 |
| README.md | 不覆盖仓库README；由本VM入口承接映射及接入规则 |

唯一当前状态始终为docs/execution/status.md#vm-mvp；证据仅以docs/execution/records/VM-MVP/为本批索引。Gov、SG和前端记录链接到该索引，不再建立第二套总状态。两份历史复核仅保留制包时事实，不重写为实施通过声明；目标行为和计划也不作为产品验收证据。

## Image MVP

- [2026-10-13 tenant-a 前端演示手动操作](execution/records/IMAGE-MVP/frontend-demo-tenant-a-20261013/README.md)：真实租户空间、两类镜像推送/登记和恢复记录。
- [2026-10-08 后端交接](execution/records/IMAGE-MVP/HANDOFF-20261008.md)：业务主链断点、代码入口、验证范围与后续接入条件。
- [2026-09-30 节点缓存补充清理](execution/records/IMAGE-MVP/img-cleanup-20260930T0344Z/README.md)：历史测试资源复查、三节点精确镜像删除和校验和。
- [规格](specs/image-mvp.md)、[API](specs/image-api.md)、[数据](specs/image-data.md)、[方法](specs/image-methods.md)、[验收](specs/image-acceptance.md)
- [逐批计划](plans/image-mvp.md)、[执行授权](plans/image-mvp-goal.md)、[来源](plans/image-mvp-sources.md)、[设计包](plans/image-mvp-package.md)
- [ADR-0007](adr/0007-image-domain-and-private-registry.md)、[Fedora 手册](runbooks/image-mvp-fedora.md)
- 当前结果仍以[唯一执行状态](execution/status.md#image-mvp)为准。
