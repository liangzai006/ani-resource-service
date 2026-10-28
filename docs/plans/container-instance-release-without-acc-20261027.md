# 容器实例在 ACC 下线 ObserveRelease 后的整改方案

日期：2026-10-27。用户已授权更新方案并开始整改，以下为本轮实施范围、顺序和验收条件。代码、测试、迁移及部署的实际结果只在 [执行状态](../execution/status.md#container-instance-owner-refund-2026-10-27) 维护；本文不维护第二份执行进度，也不代表真实实例验收通过。

## 1. 结论与完整主链

本轮按以下主链整改容器实例：**明确 DELETE → 封闭创建/启停写 → 清理 Notebook/StatefulSet/Pod → 回收专属工作盘 → 释放网络与借用引用 → 同事务固化最终 owner 清理事实和稳定 outbox → 独立通知 Gov → 原 CREATE 账本幂等退额**。

本方案以 ACC issue [#7](https://github.com/zhangzhe-ctrl/ani-accelerator-service/issues/7) 的下线方向为输入。Resource 不再请求 ACC 的 `ObserveRelease`，也不在本地构造 `ALLOCATION_RELEASED` 或恒为 true 的观察替身。Resource 报告其自有实例生命周期已终结；ACC 的物理占用、残留运行绑定和容量恢复仍是独立运行事实。

本方案沿用此前“明确 DELETE 且整个实例清理完成后报告”的业务边界。这会调整现行规格与代码中“计算清理后先退额、再删工作盘”的顺序，实施时必须同步修改正式规格，不能仅删除 ACC 门槛后保留旧的提前退额逻辑。

固定分析输入为 Resource `ece9d53037404b04034884e5c2d641ee95eb5729`。源码通过 `git show` / `git grep` 读取该版本，未把本地旧且有改动的 checkout 视为当前实现。

同时核对 Gov main `30a7e2d5c345ffa02a69758843cd3be7b5a726a2`：[现有四字段协议](https://github.com/zhangzhe-ctrl/ani-governance/blob/30a7e2d5c345ffa02a69758843cd3be7b5a726a2/api/protos/quota/service/v1/quota_release.proto#L57-L75)、[证书 owner 与请求校验](https://github.com/zhangzhe-ctrl/ani-governance/blob/30a7e2d5c345ffa02a69758843cd3be7b5a726a2/app/admin/service/internal/server/quota_internal_server.go#L315-L349)、[原账全量退额与事件幂等](https://github.com/zhangzhe-ctrl/ani-governance/blob/30a7e2d5c345ffa02a69758843cd3be7b5a726a2/app/admin/service/internal/data/quota_ledger_repo.go#L359-L471)已满足本方案，无须增加 Gov 接口。

ModelDev main `16064f6532eed2fc75b43fad8c9045f31b3eb610` 的[关闭投递代码](https://github.com/zhangzhe-ctrl/ani-modeldev-service/blob/16064f6532eed2fc75b43fad8c9045f31b3eb610/internal/biz/gpu_release.go#L110-L143)使用的也是 Gov `ReportResourceReleased`，没有新开退额接口，也没有经过 ACC 释放确认。Resource 中的 `PhysicalReleaseDigest` 是自己的客户端适配器在调用前执行的本地检查，摘要不在上述四字段请求中，从未传给 Gov。本次删除这层本地门槛，复用同一 owner 通知协议。

ModelDev 的关闭事实与 outbox 同事务、提交后通知及丢 ACK 恢复可作为实现参考；其 USER_STOP 是 execution 的终结关闭，不能照搬为容器 Stop 退额。ACC 本次整改移除 proto/service/auth 对外入口、保留 Resolve、Sync、Connect 与内部观测；本文引用 issue 的下线方向，不以本方案代替 ACC 实际合并或发布证据。

## 2. 整改前固定源码断点

| 位置 | 固定源码事实 | 整改含义 |
|---|---|---|
| [worker.go:176–189](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/ece9d53037404b04034884e5c2d641ee95eb5729/internal/biz/instance/worker.go#L176-L189) | GPU 删除在 Kubernetes 对象消失后仍等待 `ObserveRelease` 与 digest | ACC 接口移除后会停在 `WAITING_FOR_PHYSICAL_RELEASE` |
| [quota.go:26–44](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/ece9d53037404b04034884e5c2d641ee95eb5729/internal/data/instance/quota.go#L26-L44) | Resource 自己的 Gov 客户端适配器要求 `ComputeSealAt`、`PhysicalReleaseDigest`；Gov 请求不含该摘要 | 只删 worker 调用仍不能投递；需改 Resource 本地资格检查，无须改 Gov |
| [worker.go:193–238](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/ece9d53037404b04034884e5c2d641ee95eb5729/internal/biz/instance/worker.go#L193-L238) | quota 分支在 PVC/PV 与 Network 清理之前，Gov 失败即返回 | 既提前退额，又让 Gov 故障阻塞后续清理 |
| [quota.go:43–69](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/ece9d53037404b04034884e5c2d641ee95eb5729/internal/data/instance/quota.go#L43-L69) | `Report` 负责插入 outbox、调用 Gov、确认回执 | 最终清理事实与 outbox 尚未同事务提交；投递依赖实例 Step |
| [instance.go:35–48、175–188](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/ece9d53037404b04034884e5c2d641ee95eb5729/cmd/ani-resource-service/instance.go#L175-L188) | 启动时强制建立 ACC release 连接并构造观察器 | 连装配和配置也需要一起收尾 |
| [instance-mvp.md:328](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/ece9d53037404b04034884e5c2d641ee95eb5729/docs/specs/instance-mvp.md#L328-L332) | 正式规格仍要求 ACC 物理释放确认 | 与新边界冲突，必须改权威文档 |

上表记录整改前的固定源码断点，不作为当前实施状态；线上实例、数据库、账本与集群对象的检查或变更须按实际授权单独记录。

## 3. 复用能力与关闭事实

不重建实例状态机或任务系统。复用现有 PG admission、业务 version、worker lease/epoch、`BeginMutation`、`Pending`、`Finish` 和 outbox。`AcceptDelete` 会持久化终止意图；旧意图不能通过 `BeginMutation` 的版本/操作复核，迟到回执仍可归入原操作。[持久化与 fencing](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/ece9d53037404b04034884e5c2d641ee95eb5729/internal/data/instance/postgres.go#L365-L441)、[BeginMutation/Finish](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/ece9d53037404b04034884e5c2d641ee95eb5729/internal/data/instance/postgres.go#L548-L690)。

最终 owner 清理事实须同时满足以下条件：

1. 已持久受理明确 DELETE，实例保持终止意图，原 `tenant/resource/create_operation` 身份不变；新 Start/Stop 不能重新打开它。
2. 没有未知结果的外部写；已发送但结果未知的 CREATE 不能因 404、超时、重启或 ACC 不可用而清除 `Pending`。
3. 正常 UID/RV 条件删除后，Notebook、其 StatefulSet 和 Pod 均已消失；完整 API 观察成功且归属链可核实。处于 terminating 的对象仍算存在。复用保存的 UID/Pod 历史，不以某次标签查询为空替代所有权核查。
4. 实例专属 PVC 与绑定 PV 按现有 reclaim 规则完成回收；借用 PVC 只释放实例引用，绝不删除借用卷。
5. 若有网络接入，已有 submission 已 closed，Release 获得回执并观察到 released；不是仅发出 Release 请求。没有网络接入的实例不增加新依赖。
6. 在提交最终事实时重新复核当前意图、版本及 lease。事实与 outbox 同一 PG 事务落库，事务提交之后才发送 Gov RPC。

这些条件证明 Resource 拥有的实例已清理，**不证明 GPU 进程、驱动或物理容量已回收**。本方案不新增节点证明 RPC 或替代 ACC 的物理证据门槛；也不引入 force delete。保留正常 GC、不可变 UID 和未知写 fencing 即可。

已有适配器按 Notebook UID → StatefulSet ownerRef → Pod ownerRef 核对，删除 Notebook 使用 UID/RV 和 Foreground GC；Workspace 观察会继续确认绑定 PV，而非只看 PVC 404。[对象观察](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/ece9d53037404b04034884e5c2d641ee95eb5729/internal/data/instance/notebook.go#L204-L309)、[Workspace 回收](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/ece9d53037404b04034884e5c2d641ee95eb5729/internal/data/instance/notebook.go#L325-L418)、[条件删除](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/ece9d53037404b04034884e5c2d641ee95eb5729/internal/data/instance/notebook.go#L669-L690)。实施时定向核查历史 Pod UID 的缺失/归属处理，缺口只在该 adapter 内补齐。

## 4. 最小改法

### A. 先贯通正常 DELETE

- `internal/biz/instance/worker.go`：删除物理观察依赖和等待分支。先推进 compute、workspace、network、借用引用清理，再提交最终 owner 事实；Gov 通知不再阻挡这些清理。
- `internal/biz/instance/types.go`：移除 `PhysicalReleaseObserver` / `ReleaseEvidence` 业务端口；保留现有外部写标记和 Pod 历史。旧 `PhysicalReleaseDigest` 可留作历史解码/审计字段，不能作为新关闭资格或伪造 owner 证据。
- `internal/data/instance/postgres.go` 与 `quota.go`：将 outbox 创建移入最终清理事务；事件继续使用原稳定算法和 `RESOURCE_RELEASED`，不能为重试更换事件、CREATE 或 reason。报告器改为只投递已持久化且具备最终 owner 事实的事件。
- 最小本地证据附在现有持久记录上，保存 cleanup 完成时间、版本/关闭合同等必要字段；无需新增通用事件框架。只追加 `007_owner_cleanup_release.sql`，由 owner 显式迁移到 schema 7 并更新 `schema.go`，不改历史 checksum，也不在应用启动时迁移。

### B. 让未投递事件独立恢复

- 复用现有 `internal/server/instance_worker.go` 的进程生命周期，为已关闭未投递事件提供独立扫描/投递入口。它不重新依赖 Kubernetes/ACC Observe 成功，也不要求 Get/List 被调用。
- 保证有界处理并通过 `next_attempt_at` 调度失败重试，单个事件失败不能饿死其他清理或投递；Gov 收到同一稳定事件的重复发送由现有幂等处理承接。
- ACK 必须同时匹配 `release_event_id/create_operation_id/resource_id`，状态只能是 APPLIED 或 ALREADY_APPLIED；确认 outbox 与本地 `QuotaReturned`/删除操作完成标志须可崩溃恢复。资源清理完成和退额回执分别展示。
- 不扩 Gov RPC，也不由 Resource 持有配额金额、重算当前目录或改账；Gov 接收四字段 owner fact、按证书 owner 和原 CREATE 定位原账的契约保持不变。

### C. 移除装配依赖但保留运行身份能力

- 删除 `internal/data/instance/release.go` 及旧观察器专用测试；更新 `NewWorker` 的调用点。
- `cmd/ani-resource-service/instance.go` 移除 release connection、Release* 必填校验与 ACC release client。保留 Quota mTLS、`SigningKeyFile`、Pod owner 签名和运行身份标签；这些仍供运行观测使用。
- 配置读取使用 `DisallowUnknownFields`：直接删 Release* 结构字段会使旧配置启动失败。本次继续接受这些废弃字段但完全不使用、不建连接，新示例移除它们；正式删字段须另行发布兼容说明。不能偷偷启用旧 RPC fallback。
- 同步 runbook、spec §8/§10、相关验收与部署配置；若升级 ACC Go 依赖，固定已移除 RPC 的明确版本并编译核查。用户 API 无需新增退额参数。

## 5. 不同行为的退额边界

| 行为 | 本方案 |
|---|---|
| Stop / Start / Stop→Start（普通重启） | 保留实例与原账；不创建释放事件、不退额、不重新占额 |
| 运行中 Pod 退出、丢失、重调度或 ACC 来源缺项 | 运行异常/观察状态；不是最终关闭事实 |
| 创建 API 在持久受理前被拒绝 | 不是 Resource 已创建实例的正常释放；按 Gov 原有 admission 失败规则处理，不在本次新增逻辑 |
| 创建已受理后启动失败、一直 Pending | 不能仅凭失败退款；当前需要明确 DELETE，并走同一完整清理链 |
| DELETE-first / 从未开始外部创建的删除 | 保留 tombstone 与迟到 CREATE 拦截；满足剩余清理后提交原 CREATE 对应事实 |
| 已发 CREATE、结果未知 | 保留 `Pending` 与原账；取得可归因结果或原对象出现后再按最新 DELETE 清理 |
| 最终 DELETE 已清理、Gov 暂时不可用 | 关闭事实和 outbox 保留；独立重试，收到匹配 ACK 后结束退额阶段 |

本次不扩展“所有创建失败自动转终止并退额”的新产品行为，也不照搬其他服务把某种 Stop 定义为 execution 终结的规则。

## 6. 存量恢复与升级顺序

1. 只读盘点原 DELETE：`WAITING_FOR_PHYSICAL_RELEASE`、`QUOTA_RETURN_PENDING`、`Pending`、workspace/network 残项、outbox 是否已 ACK；保留原身份、操作、事件和来源证据。
2. 部署新 worker 前停止旧版本的实例执行/投递，避免旧流程继续提前上报；由 owner 显式执行 `007_owner_cleanup_release.sql`，匹配二进制与配置按同批变更准备，应用启动不迁移。具体环境切换仍须取得该环境发布授权。
3. `WAITING_FOR_PHYSICAL_RELEASE` 由原 DELETE 在新规则下重新观察并继续清理；不批量把 `ComputeCleaned/QuotaReturned` 改 true，不回填虚假 digest，不创建新退款操作。
4. **旧未 ACK outbox 可能在 PVC/network 完成前已创建**：禁止直接扫描即发。先完成原实例的全部清理并持久最终事实，再投递原事件。
5. 旧已 ACK 的事件保留已应用事实，只继续剩余实例清理；不回滚/重复退款。若 Gov 实际已退款但回执丢失，原事件重试取得 ALREADY_APPLIED。
6. 真实未知 CREATE、所有权冲突、正常 GC 或网络/存储回收未完成仍保留具体阻塞原因；仅取消已废弃的 ACC RPC 依赖，不清空真实未完成状态。

## 7. 有业务意义的定向验收

| 场景 | 必须观察到的结果 |
|---|---|
| 有 GPU 的真实实例正常 DELETE | Notebook/STS/Pod、专属 PVC/PV、网络/引用全部清理后才产生 owner fact/outbox；Gov 原 CREATE 全额只退一次；不调用 ObserveRelease |
| Gov 不可用 + 完整删除 | 前置清理继续完成，outbox 持久；恢复 Gov 后独立投递成功，无需恢复 Kubernetes 观察能力 |
| PVC finalizer/PV reclaim 或 Network release 暂阻塞 | 未完成清理前没有新的 Gov 报告；解除阻塞后从原操作继续 |
| 发送前/发送后进程崩溃，Gov 已提交但 ACK 丢失 | 同一事件与原 CREATE 恢复，匹配 ALREADY_APPLIED；无漏投、无重复退额 |
| Stop→Start、多次 Pod 替换、普通退出 | 不创建 release outbox、不退款，原规格/PVC/账本保持绑定 |
| DELETE-first、创建失败后显式 DELETE | 不允许迟到创建重新开启实例；完整清理后只报告原事件 |
| 未知 CREATE + 连续 NotFound、过期 worker、错误 UID/ACK、跨租户 | 分别保留未知/拒绝旧写/拒绝错误确认/拒绝越界，不能伪造关闭成功 |
| 旧 waiting、旧未 ACK、旧已 ACK 三类存量 | 原操作恢复；未 ACK 等最终清理事实；已 ACK 不重复退款 |
| CPU 实例与无网络 GPU 实例 | CPU 无 GPU 释放 RPC；未选网络不强加 Network 依赖；现有启停、卷回收行为不回归 |

先修改并复用 [真实 PG + outbox/Gov 回执测试](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/ece9d53037404b04034884e5c2d641ee95eb5729/internal/data/instance/postgres_integration_test.go#L536-L731)、Network vertical fixture 和 GPU execution fixture；将旧“物理观察放行”测试改为 owner 最终清理门槛测试，避免 fake 放行新业务逻辑。Gov/Kubernetes 可在受控测试中作为外部边界替身，Resource 权限、持久化、清理判断与 outbox 必须真实执行。随后在任务授权远端运行改动对应测试和提交前 `make verify`；真实实例链另列 LIVE 结果，不能由受控 fixture 推导。

## 8. 实施顺序与交付边界

按 DELETE 清理顺序与最终事务 → 独立 outbox 投递 → 装配/配置及正式规格收尾 → 定向行为和真实 PG 验证推进。外部边界替身不得替代 Resource 权限、持久化、未知写 fencing、完整清理判断及 outbox 业务逻辑。迁移只追加，不改历史 checksum；不改 Gov 协议，不新增配额金额或设备证明系统。

受控主链通过后，再按实际环境授权安排存量恢复及真实实例验收。静态修改、编译或受控 PASS 均不自动授权共享环境发布，不替代 LIVE 的完整删除、原账退额和独立设备容量核对。所有实施、执行主机、源码快照、命令、PASS/FAIL/NOT_RUN 与下一步统一写入 [执行状态](../execution/status.md#container-instance-owner-refund-2026-10-27)。
