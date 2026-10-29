# 容器实例重启与 PVC 挂卸实施方案

**版本：v2.0 · 后端实施版**  
**修订日期：2026-10-28（Asia/Shanghai）**  
**范围：Resource + Governance；本批不对接 Console。**

本文替代同日原版实施计划。当前交付物是方案修订；本文中的新增字段、RPC、阶段、迁移和测试均为实施要求，不表示已经实现、执行或验收。本文不构成 push、merge、数据库迁移、集群发布或共享组件变更的执行授权。

实施进度只维护在 Resource `docs/execution/status.md` 的 `container-restart-pvc-20261028` 条目；原始证据放入 `docs/execution/records/CONTAINER-RESTART-PVC-20261028/`。本文不另设进度台账。实现后将稳定合同归入正式 specs/contracts，由本文链接，避免两个权威状态机长期并存。

## 0. 本次修订与执行规则

### 0.1 相对原计划的变更

1. 删除 Console 开发、前端请求重试、页面轮询、按钮、弹窗、浏览器验收等工作；后端接口及查询合同仍在本批交付。
2. 固定公开 JSON 命名、RPC、权限、幂等摘要、查询结果和错误分类，不把这些选择留给各执行 agent。
3. 固定用户运行目标与阶段运行目标的区别、操作终态、条件写恢复和安全停止的规则。
4. 把 Gov 删除明确拒绝、只读命令重放、CPU/GPU 拒绝结果查询前移到 CP-01。
5. 把 Storage 删除入口与持久卷引用协调纳入 CP-02；同时覆盖原 CREATE 的 borrowed 引用受理。
6. 补齐迁移、旧进程退出、空集合、同路径重挂代际、失效卷卸载以及后端故障注入验收。

### 0.2 约束等级

- “必须”“固定”“禁止”是本批实施约束，不能由执行 agent 自行改为可选项。
- 文件内的字段名、动作名、状态名、HTTP JSON 名、错误 reason 和阶段名按本文固定；已有 Proto 字段编号不得变更，新字段编号由集成人在最新文件末尾追加并记录。
- 本文指定的行为不能通过删除数据、替换用户 PVC、强制删除 Pod/finalizer、关闭鉴权或绕过 PG 实现。
- 遇到当前代码或真实环境与本文假设不符，先提交具体反例和最小修订建议；停止受影响切片，继续不受影响的已授权工作。不能暗自扩大为通用工作流、通用锁服务或共享 runtime 重构。
- CPU 与 GPU 都属于后端交付范围；CPU 通过不代替 GPU。后端交付状态不依赖前端是否开发。

## 1. 来源、当前事实与目标

### 1.1 固定源码基线

| 来源 | 本轮确认 HEAD | 使用规则 |
|---|---|---|
| Resource `main` | `55092dbf05c284875555d422e75eeb5691cb9cfd` | 从该 HEAD 或实施时重新核定的 main 建立隔离工作目录 |
| Governance `main` | `4ad9cbff15bda6afc57ac99218d8f49ae197ccc6` | 同上；正式消费本批 Resource API 不可变 module ref |
| Notebook Controller 源码参照 | Kubeflow v1.10.0，`90e987bf87d3e7c900926310b00bfa16b59e41eb` | 仅用于证明源码能力；部署镜像版本须在 LIVE 前核定 |

本轮比较确认：Resource 相对初审 `8ede17c` 新增 Sandbox 相关实现，Instance 核心生命周期/卷/PG 实现未变；Gov 相对 `5046a0a` 新增 Inference Key 相关实现，Instance/VM 命令链未变。共享 composition、权限映射、生成输出和 OpenAPI 已有其他变更，实施时必须保留。证据：[Resource 差异](https://github.com/zhangzhe-ctrl/ani-resource-service/compare/8ede17c74beee9366a00b20ccd77cdfe63097892...55092dbf05c284875555d422e75eeb5691cb9cfd)、[Gov 差异](https://github.com/zhangzhe-ctrl/ani-governance/compare/5046a0af00304adfd1bdf82273ff3c3a1d24a528...4ad9cbff15bda6afc57ac99218d8f49ae197ccc6)。

实施前记录实际 HEAD、dirty 文件、源快照哈希和交付模块版本；不覆盖原有工作树。来源变化时只复核相关 diff，不重做无关整仓审计。代码链接是点时证据，不是跨仓目录依赖。

### 1.2 必须承认的当前代码事实

| 事实 | 对本批实现的直接要求 |
|---|---|
| Instance 普通命令当前主要更新 desired，并可 supersede 旧 accepted | 增加真实互斥；新 Start/Stop 不能盖过未完成重启/挂卸 |
| `Finish` 当前按 Desired/Observed 一致自动写 completed | 操作结果改为阶段驱动，并封存终态 |
| snapshot 读取严格拒绝未知字段，SQL desired 还会覆盖 JSON Desired | 新持久字段显式读取；禁止仅改内存/JSON 阶段目标；禁止混跑旧执行者 |
| `Observe` 把 owner、配置、就绪及 GPU 签名相关处理交织 | 分离安全身份/退出证明与运行资格，允许异常配置安全停止 |
| Storage 删除仅查 Pod/Notebook 使用方 | 增加持久引用/删除资格协调，不能只增加一次 SELECT |
| Gov Instance 先删除闸门后 Resource Change；VM 已有 Lookup 先行 | 补 Instance 只读重放并调整顺序 |
| Gov DELETE 明确版本拒绝仅为 VM 完整处理 | 补 Instance 专用拒绝、解闸、重放、查询闭环 |

源码证据：[Instance 命令受理与 Finish](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/55092dbf05c284875555d422e75eeb5691cb9cfd/internal/data/instance/postgres.go)、[Notebook 观察](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/55092dbf05c284875555d422e75eeb5691cb9cfd/internal/data/instance/notebook.go)、[Storage 删除](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/55092dbf05c284875555d422e75eeb5691cb9cfd/internal/biz/storage/usecase.go)、[Gov Instance](https://github.com/zhangzhe-ctrl/ani-governance/blob/4ad9cbff15bda6afc57ac99218d8f49ae197ccc6/app/admin/service/internal/service/instance_service.go)、[VM 删除参照](https://github.com/zhangzhe-ctrl/ani-governance/blob/4ad9cbff15bda6afc57ac99218d8f49ae197ccc6/app/admin/service/internal/data/vm_delete_guard.go)。

### 1.3 用户主链及完成定义

主链：授权客户端调用 Gov HTTP → Gov 授权/重放/删除闸门 → Resource gRPC 持久受理 → 实例 worker 操作 Notebook → controller 更新 StatefulSet/Pod → Gov 查询取得持久阶段和最终结果。

| 操作 | 允许的新意图 | 成功完成条件 |
|---|---|---|
| Restart | 稳定 running 实例 | 旧 Pod 已退出，新 Pod UID 与保存的旧 UID 不同，Ready、当前挂载、GPU 资格及网络核验通过 |
| 停止态 AttachPVC | 稳定 stopped；新增已有 PVC 的一个路径引用 | Notebook 和受控 STS 模板匹配目标；无 Pod；当前配置/引用已提交；保持 stopped |
| 停止态 DetachPVC | 稳定 stopped；移除一个当前 borrowed 路径 | 同上，目标路径已移除；借用 PVC/PV 不删除；保持 stopped |
| 运行态 AttachPVC/DetachPVC | 稳定 running 且 `restart_if_running=true` | 单 operation 停止、改模板、提交配置、启动并验证；完成时恢复 running |

运行态挂卸缺省或显式 `restart_if_running=false`，返回需停止冲突，不受理新 operation。运行态修改会终止容器进程，PVC 之外的临时文件不保证保留。重启/挂卸不重新占 GPU 额度、不退额；只有最终实例 DELETE 完整清理后按原账退额。

### 1.4 范围边界

交付 Resource/Gov 真实实现、正式 API 模块、权限登记、显式迁移、恢复说明和 CODE/PG/BOUNDARY/LIVE 证据。保留现有 gRPC 服务间通信和 Gov `/api/v1` 业务入口。

本批不开发 Console、不执行浏览器验收；不引入不停机热挂载、卷创建/扩容/快照/克隆的新能力、任意 hostPath/Block、实例变配/换网/换镜像/env/labels 修改、系统 workspace 卸载或通用工作流服务。Storage 仅修改与本批引用保护直接相关的现有删除路径及恢复装配。

## 2. 全局不变量

| 编号 | 必须始终成立的规则 |
|---|---|
| I01 | 原 CREATE canonical、Spec、请求/业务摘要、创建 operation、ACC 冻结执行快照和原账不变；原 CREATE 重放不重置当前挂载 |
| I02 | CurrentMounts 已初始化后始终权威；空 `[]` 表示没有附加挂载，不回退原 Spec |
| I03 | 每次新命令受理使实例 version 加 1；幂等重放、worker 阶段推进、应用版本回写不增加实例 version |
| I04 | 普通 Start/Stop/Restart/AttachPVC/DetachPVC 同一实例最多一个非终态操作；Resource DELETE 可以接管，之后禁止新普通变更 |
| I05 | 当前授权先于重放；重放先于当前版本、状态、删除闸门和外部 PVC 解析；命中同键异意图必须冲突 |
| I06 | Notebook 生命周期/卷模板写前持久记录 Pending；条件写绑定 UID、resourceVersion、operation、意图版本和阶段序号 |
| I07 | completed/failed/superseded 为终态，不因实例常态观察重新变为 accepted/blocked/completed |
| I08 | 外部结果未知、身份不明或安全退出证明不足时保留 blocked 及保护引用；时间到达不是清理证明 |
| I09 | 新引用先保护后写模板；旧引用在全部受控模板和旧 Pod 不再使用且未决写收敛后释放 |
| I10 | PVC 挂载受理和平台 PVC 删除受理使用同一个持久 guard；不通过一次 GET/LIST/SELECT 声称严格互斥 |
| I11 | Borrowed PVC/PV 不获得 Notebook ownerReference；实例挂卸和最终 DELETE 不删除借用数据 |
| I12 | 当前配置、应用版本、引用推进、phase 及运行目标相关变更，在同一 Instance owner 事务提交 |
| I13 | public Desired 保存最终运行目标；RuntimeDesired 保存当前阶段执行目标；空闲时二者一致，网络消费 RuntimeDesired |
| I14 | 只有最新 intent/lease/phase 有权推进；迟到结果可保存归因证据，不能复建资源、覆盖新配置或重新激活引用 |
| I15 | 退额前必须计算、workspace、全部保护态引用、网络及未知写均已完成清理；CPU 不进入 GPU 占退额调用 |

I06 的范围是本批 Notebook 生命周期与卷模板控制写。已有 GPU Pod 签名继续采用实际 Pod UID 绑定的幂等方式，在运行资格核验阶段调用；不将它误写成纯只读，也不为本批创建独立签名工作流。其失败不阻挡已证明 owner 的停止/删除。

## 3. 固定后端接口合同

### 3.1 Gov 公开入口和权限

| 方法与路径 | Gov RPC | 请求 | 权限 |
|---|---|---|---|
| `POST /api/v1/instances/{instance_id}:restart` | `RestartInstance` | `ChangeInstanceRequest` | `instance:restart` |
| `POST /api/v1/instances/{instance_id}:attach-pvc` | `AttachInstancePVC` | `AttachInstancePVCRequest` | `instance:pvc:attach` |
| `POST /api/v1/instances/{instance_id}:detach-pvc` | `DetachInstancePVC` | `DetachInstancePVCRequest` | `instance:pvc:detach` |
| `GET /api/v1/instances/{instance_id}/mountable-pvcs` | `ListInstanceMountablePVCs` | 专用只读请求 | `instance:pvc:attach` |
| 现有 instance/configuration 查询 | 保留 | 保留 | 保留实例读取权限 |
| `GET /api/v1/instance-operations/{operation_id}` | 保留 | 保留 | `instance:operation:read` |

前三个动作在 Proto HTTP annotation、手写 router 和权限目录中的路径模板沿用 `{data.instance_id}`；实际 URL 仍只是实例 ID。实例范围候选用顶层 `{instance_id}`。创建范围 `/api/v1/instances/mountable-pvcs` 保留原用途及 `instance:create` 权限。

运行态挂卸的内部停止/启动由相应 PVC 动作权限和显式中断参数授权，不额外要求独立 Start/Stop/Restart 权限；独立 Restart 仍要求 `instance:restart`。所有读写仍受可信租户、资源范围和现有 INSTANCE 套餐闸门限制。

### 3.2 公开 JSON 名与校验

保留现有命名：基础字段 `instanceId/idempotencyKey/expectedVersion` 为 camelCase；卷字段 `pvc_name/mount_path/read_only` 沿用显式 snake_case；新增中断字段固定 `restart_if_running`。本批不进行全局命名迁移。

Restart 请求示例：

```json
{
  "data": {
    "idempotencyKey": "10000000-0000-4000-8000-000000000001",
    "expectedVersion": "7"
  }
}
```

Attach 请求示例：

```json
{
  "data": {
    "idempotencyKey": "10000000-0000-4000-8000-000000000002",
    "expectedVersion": "7",
    "pvc_name": "research-data",
    "mount_path": "/datasets",
    "read_only": false,
    "restart_if_running": true
  }
}
```

Detach 请求示例：

```json
{
  "data": {
    "idempotencyKey": "10000000-0000-4000-8000-000000000003",
    "expectedVersion": "8",
    "mount_path": "/datasets",
    "restart_if_running": false
  }
}
```

固定校验规则：

1. `data` 必须是对象。`instanceId` 可省略并由路径绑定；若提供，必须与路径一致。路径 ID、幂等 key 使用现有规范 UUID 规则。
2. 新动作 `expectedVersion` 必须是正整数的十进制字符串，且不溢出 int64。不能通过 JS 数值、浮点、科学计数、空串或 null 传递。
3. `read_only/restart_if_running` 缺省 false；省略与显式 false 是同一业务意图。显式 null、字符串布尔和数值布尔均拒绝。只有 JSON `true` 表示允许运行中中断。
4. 新动作只接受本节列出的 JSON 名，不另接受相同字段的 snake/camel 别名；旧 Start/Stop 的既有兼容输入不在本批收窄。
5. 新动作拒绝未知字段、重复键、非法类型、额外 query 参数及客户端提供的 tenant/actor/cluster/namespace/PVC UID/ownership。沿用现有 64 KiB 和 Content-Type 限制及可信 header 防伪规则。
6. Restart 复用现有 Change 消息，但新路由增加专用原始 JSON 校验；不能因为复用消息就接受 PVC 参数。Attach/Detach 用独立消息，不给通用 Change 增加无关字段。
7. `mount_path` 必须已规范化的绝对路径，拒绝通过静默 clean 改变用户意图。沿用受保护目录、路径相互遮盖和最多 8 个附加挂载规则。

对新路由先做轻量原始 JSON 键/类型/null 校验，再进入现有严格 ProtoJSON 解码和路径绑定；不能用宽松解码器吞掉未知参数。基础命名来源见 [Gov Instance Proto](https://github.com/zhangzhe-ctrl/ani-governance/blob/4ad9cbff15bda6afc57ac99218d8f49ae197ccc6/api/protos/instance/service/v1/instance.proto)。

### 3.3 受理响应与重试

新的三个动作只有获得 Resource durable ACK 才返回 HTTP 202。使用现有 Acceptance 对象，响应不再包一层 `data`：

```json
{
  "operationId": "20000000-0000-4000-8000-000000000001",
  "resourceId": "30000000-0000-4000-8000-000000000001",
  "replayed": false,
  "result": "ACCEPTED"
}
```

Resource `OperationResponse` 保留 `durable_owner_ack=1`，追加 `replayed=2`。并发相同请求在事务内命中重放也必须正确标识 true。响应丢失后 API 调用方用原 key、原版本、原正文重试；后端不得要求调用方先改版本才能查询原受理。

新动作不进入 Gov 配额命令持久队列；Gov 未收到 durable ACK 时返回现有错误封装和适当失败状态，不伪造 202。重放 202 仍表示原受理；终态由 operation 查询取得。CREATE/DELETE 原投递协议保留。

### 3.4 实例范围候选

只接受 `page/pageSize/name`；page 缺省 1，pageSize 缺省 20、范围 1..100，显式 0、重复或未知 query 拒绝。总数为十进制字符串。复用现有候选响应字段：`pvc_name/phase/requested_capacity/access_modes/volume_mode/selectable/unavailable_reason`。

Resource 先按可信 tenant 和 instance ID 读取持久实例 placement，再从相应 cluster/Namespace 查询。禁止用当前默认 placement 覆盖已有实例的位置。单集群 client 与保存 cluster 不匹配时返回依赖不可用，不能暗中改查另一个集群。

专属 workspace、非本租户和不允许暴露的对象继续过滤；Filesystem、Lost、terminating 等资格继续显示。持久删除 guard 激活时返回不可选原因 `PVC_DELETION_IN_PROGRESS`。合法 Pending/WFFC 不因未 Bound 被一律拒绝。候选资格不保证之后受理或调度一定成功。

### 3.5 查询字段及空集合语义

Resource 与 Gov DTO 同步追加字段；新字段仅追加，不改原编号/JSON 名。保留 Gov 当前默认 ProtoJSON `EmitUnpopulated=true` 编码行为：没有对象的 message 输出 null；存在的挂载对象即使列表为空，也必须明确序列化 `volume_mounts: []`，不以 null 代替空集合。本批不修改共享全局 codec。[当前依赖的 Kratos 编码器](https://github.com/go-kratos/kratos/blob/v2.9.2/encoding/json/json.go)。

| 对象 | 新字段及 JSON 名 | 定义 |
|---|---|---|
| Instance | `active_operation_id` → `activeOperationId` | 当前普通/删除执行操作 ID；无活动操作为空 |
| Instance | `execution_run_state` → `executionRunState` | RuntimeDesired 的公开只读投影 |
| Instance | `applied_mount_version` → `appliedMountVersion` | 当前已证明应用的挂载版本；0 表示尚未证明 |
| InstanceConfiguration | `version` | 当前实例命令 version，十进制字符串 |
| InstanceConfiguration | `applied_mount_version`，显式同名 JSON | 同上；不等于最新命令 version 的情况合法 |
| InstanceConfiguration | `pending_change`，显式同名 JSON | 仅当前非终态挂卸的 operation_id、intent_version、phase、target_configuration；没有则为 null |
| Operation | `phase` | 第 6 节固定阶段名 |
| Operation | `target_configuration`，显式同名 JSON | 只包含 `volume_mounts`；Restart 为 null，卸空时对象存在且列表为空 |
| Operation | `final_run_state`，显式同名 JSON | 本操作成功目标 running/stopped；DELETE 为 deleted；失败安全停止不改写这份原始目标；历史无执行记录时为空 |
| Operation | `applied_mount_version`，显式同名 JSON | 本操作最后确认/提交的应用版本；终态后封存 |
| Operation | `completion_scope`，显式同名 JSON | 成功时 `template_applied` 或 `runtime_verified`，未成功为空 |
| Operation | `failure_reason`，显式同名 JSON | 进入失败收尾时保存的初始原因；最终 failed 的 reason 使用该值 |

`pending_change` 内部 `operation_id/intent_version/phase/target_configuration` 也显式固定同名 snake_case JSON；两处 `target_configuration` 使用相同消息，内部列表固定 `volume_mounts`，元素沿用现有公开卷字段。所有 int64 版本字段，包括 `appliedMountVersion/applied_mount_version/intent_version`，公开 JSON 都是十进制字符串。

禁止只靠 repeated 字段区分“没有配置变更”与“目标明确为空”：前者为 `"target_configuration": null`，卸空目标为 `"target_configuration": {"volume_mounts": []}`。现有 configuration 的 env 值仍仅在已有授权配置查询中返回；列表、operation、日志不扩大环境值/Secret/PVC UID 暴露。

CurrentMounts 表示当前权威配置集合。新建/迁移初始化时可先登记集合、`applied_mount_version=0`，此时查询明确未验证；之后的挂卸只有模板证明成立并提交事务后才切换 CurrentMounts。实际运行挂载仍以当前 Pod 核验结果为准。

### 3.6 Resource gRPC

在现有 `instance.v1.InstanceService` 新增并固定五个方法：

```proto
rpc Restart(ChangeInstanceRequest) returns (OperationResponse);
rpc AttachPVC(AttachPVCRequest) returns (OperationResponse);
rpc DetachPVC(DetachPVCRequest) returns (OperationResponse);
rpc ListInstanceMountablePVCs(ListInstanceMountablePVCsRequest)
    returns (ListMountablePVCsResponse);
rpc LookupCommandReplay(LookupCommandReplayRequest) returns (OperationResponse);
```

AttachPVC 请求：`tenant_id, instance_id, idempotency_key, expected_version, pvc_name, mount_path, read_only, restart_if_running`。DetachPVC 请求去掉 `pvc_name/read_only`。候选请求：`tenant_id, instance_id, page, page_size, name`。Lookup 请求：`tenant_id, instance_id, action, idempotency_key, canonical_intent`。

这些请求顶层保留 `tenant_id`，沿用 Gov mTLS 主体及可信租户/actor 校验。`internal/server/instance_identity.go` 精确增加五个方法，更新方法矩阵与 `docs/specs/governance-calls.md`；不能放开整个服务前缀。公开 HTTP 不接受这些内部身份字段。

Gov 和 Resource 均以服务现有配置方式新增本批能力开关，缺省关闭；只控制新 Restart/Attach/Detach 受理，不阻止授权的只读查询/候选/Lookup。先完成当前身份/动作授权和输入规范化，再做只读重放；开关检查在这些步骤之后、任何外部 PVC 解析或持久副作用之前。Resource 不能只相信 Gov 已关入口；worker 对已受理任务的恢复不受开关影响。

切换期间另用同一配置机制的维护受理门禁，覆盖新 CREATE/Start/Stop/Restart/Attach/Detach 及 Storage PVC DELETE；不阻止已受理任务恢复、原键重放和 Instance 最终 DELETE 清理。新版本首次启动该门禁保持关闭，确认迁移/guard/身份健康后按对象资格开放。第 8 节进程维护窗口本身可能暂时不可用，与健康新版本中“关闭新能力仍保留重放”分开处理；不新建通用开关服务。

### 3.7 规范意图、幂等范围及摘要

幂等查询键固定 `(tenant_id, instance_id, action, idempotency_key)`，action 使用 `start/stop/restart/attach-pvc/detach-pvc`。原 CREATE/DELETE 保持各自既有合同。

规范字节由 Resource API 模块的公共 helper 唯一产生，Gov 正式依赖同一 helper。固定采用 Go `encoding/json.Marshal` 的数组编码、无尾随换行、int64 为 JSON 整数、bool 为布尔、字符串采用该编码器默认转义；hash 为字节的 SHA-256 小写十六进制：

| action | 规范数组，元素顺序不可变 |
|---|---|
| start / stop / restart | `[instanceID, expectedVersionInt64, action]` |
| attach-pvc | `[instanceID, expectedVersionInt64, "attach-pvc", pvcName, mountPath, readOnly, restartIfRunning]` |
| detach-pvc | `[instanceID, expectedVersionInt64, "detach-pvc", mountPath, restartIfRunning]` |

Start/Stop 必须逐字保持当前历史 hash 算法，不加 tenant、actor、schema 前缀或改成对象。规范字节中的数值与 HTTP 的字符串版本属于不同层，不能混淆。UID、placement、phase、当前实例版本及解析结果不进入用户意图摘要。

Lookup 严格验证 action、数组结构和类型，并重编码核对规范字节；版本用 `[]json.RawMessage` 逐槽解码为 int64，或 UseNumber 后 ParseInt，禁止经过 float64。包括 2^53+1 和 int64 上界在内的合法版本必须无精度损失。不接收任意客户端提供的 hash 作为可信结果。Lookup 只读数据库，不访问 Kubernetes，不验证当前运行状态，不要求目标 PVC 当前仍存在；同键异意图返回冲突，未命中明确 NotFound。受理事务仍须再次查重，不能把 Lookup miss 当作独占资格。

### 3.8 错误与状态封装

| 场景 | reason | Gov HTTP |
|---|---|---|
| 新动作格式/类型/路径无效 | `INVALID_INSTANCE_REQUEST` | 400 |
| 当前动作无权限/租户不允许 | 沿用现有授权 reason | 403；跨租户对象按既有不可见策略处理 |
| 实例或可见 PVC 不存在 | `INSTANCE_NOT_FOUND` / `INSTANCE_PVC_NOT_FOUND` | 404 |
| 同键异意图 | `INSTANCE_IDEMPOTENCY_CONFLICT` | 409 |
| 版本不符 | `INSTANCE_VERSION_CONFLICT` | 409 |
| 已有非终态普通操作 | `INSTANCE_OPERATION_IN_PROGRESS` | 409 |
| 不满足新动作稳定状态 | `INSTANCE_STATE_CONFLICT` | 409 |
| running 挂卸未允许中断 | `INSTANCE_REQUIRES_STOP` | 409 |
| DELETE 闸门关闭 | `INSTANCE_DELETING` | 409 |
| 路径已挂载/待卸路径不存在 | `INSTANCE_MOUNT_PATH_IN_USE` / `INSTANCE_MOUNT_NOT_FOUND` | 409 / 404 |
| 尝试卸载 workspace | `INSTANCE_WORKSPACE_DETACH_FORBIDDEN` | 409 |
| PVC 身份变更/删除 guard 激活 | `INSTANCE_PVC_REFERENCE_CHANGED` / `INSTANCE_PVC_DELETION_IN_PROGRESS` | 409 |
| 保护端口或依赖不可用 | `INSTANCE_DEPENDENCY_UNAVAILABLE` | 503 |
| 新动作尚未开放 | `INSTANCE_ACTION_UNAVAILABLE` | 503 |

已有错误映射不顺带重命名；新 reason 在 Resource domain、RPC 映射、Gov HTTP 映射和合同测试中同步。gRPC 的新业务冲突用 FailedPrecondition，幂等冲突保持 AlreadyExists，依赖故障用 Unavailable。已受理后的执行失败通过 operation state/reason 查询，不把查询成功的 HTTP 200 误当操作成功。

## 4. 持久化模型和版本规则

### 4.1 Instance 当前配置与运行目标

在 `instance.instances` 追加以下独立列；领域新增字段使用 `json:"-"` 或独立持久 DTO，避免无意改变原 Spec 的 JSON 表达：

| 列 | 类型/约束 | 含义 |
|---|---|---|
| `current_mounts` | jsonb、必须是 array | 当前权威 frozen mount 集合；空集合为 `[]` |
| `mounts_initialized` | boolean | 已完成一次明确初始化；正常新实例为 true |
| `applied_mount_version` | bigint，`0 <= value <= version` | 0=未证明；其余为应用配置所对应的实例意图版本 |
| `runtime_desired` | text，running/stopped/deleted | 当前执行阶段对 Notebook 的目标 |
| `recovery_fence` | jsonb，可 NULL | 第 4.4 节唯一恢复停止尝试的独立持久槽；NULL 表示没有 |

CurrentMounts 内部记录固定保存 cluster_id、namespace、pvc_name、pvc_uid、mount_path、read_only、ownership=borrowed、reference_generation。使用独立带明确 JSON tag 的存储 DTO；不把 UID/generation 投影到公开卷字段。保留既有挂载顺序；新增项追加，卸载保留其余项相对顺序，避免迁移时无故改变生成的 borrowed volume 名。

原 `Spec.VolumeMounts` 永不更新，仍用于原创建事实和摘要。当前渲染、校验、配置查询、诊断、Start 和 Network/终端运行目标统一消费 CurrentMounts。`mounts_initialized=false` 不是“空列表”，此时新重启/挂卸不可受理，先按第 8 节完成初始化；不存在按 `len==0` 回退的路径。

保留 `LastOperationID` 作为操作入口，不新增第二个活动任务持久指针。公开 `activeOperationId` 从 LastOperationID 对应操作是否非终态计算。Claim 同时加载实例与相关操作执行记录；受理仍在实例行锁下查询非终态操作，验证与 LastOperationID 一致，异常多操作不得静默任选一条。

### 4.2 两种运行目标

必须同时满足：

```text
SQL instances.desired == snapshot.Desired
SQL instances.runtime_desired == Instance.RuntimeDesired
Network.DesiredRunState == Instance.RuntimeDesired（有 Network 时）
没有活动操作且没有未决恢复写时：Desired == RuntimeDesired
```

- Desired 表示该操作最终希望达到的运行目标，公开 `desiredState` 保持此含义。
- RuntimeDesired 是阶段目标，仅由第 6 节映射表设置；公开为只读 `executionRunState`。
- 重启和运行态挂卸的中间停止阶段，Desired=running、RuntimeDesired=stopped；稳定性不能再只看 Desired/Observed 相等。
- 操作安全停止并失败时，同一事务把 Desired、SQL desired、RuntimeDesired 都设为 stopped；原 operation 的 final_run_state 仍保存原始成功目标，便于解释失败。
- Resource DELETE 接管后两者固定 deleted。恢复停止栅栏只是删除过程的控制写，不把 runtime_desired 从 deleted 改回 stopped。

必须同步修改 `decodeSnapshot/scanInstance/decodeInstance/Get/List/Claim` 的装载与一致性检查、`networkIntent/ReconcileSubmission`、Network observe/confirm/read-only consumer 和 worker 的运行目标判断。先加载独立列再做跨字段校验，不能保留原 decoder 的 `Network.DesiredRunState == Desired` 断言。

### 4.3 Operation 执行记录

沿用 `instance.instance_operations` 的操作身份、action、key、request_hash、version、ack、state、reason。增加一个版本化 `execution` JSONB（内部 schema_version=1），保存下列必要事实；state/reason 仍以原列为唯一权威，不在 JSON 中另存可漂移副本：

- phase、phase_sequence；phase_sequence 每次阶段转换加 1，不向用户开放配置 revision。
- canonical_intent；新命令受理后不可变，历史 Start/Stop 用原 hash 兼容。
- before_run_state、final_run_state；受理时冻结。
- before_mounts、可空的 target_configuration、对应确定性摘要；只保存挂载差异所需事实，不复制整个 CREATE。
- 本次涉及引用的 path/generation，以及模板提交是否完成。
- before_pods（cluster/namespace/name/UID/controller UID）；在首次控制写前冻结，不在重试时换成新 Pod。
- 已验证的 Notebook/STS 身份、模板证明及本次最后确认的 applied_mount_version。
- started_at、deadline_at、failure_reason、completion_scope。
- 与本操作有关的外部回执归因信息；不能把旧操作回执当作当前资源结果覆盖。

固定操作状态：

| state | 是否终态 | 行为 |
|---|---|---|
| accepted | 否 | 已受理且推进/等待；phase 表示进度 |
| blocked | 否 | 未知外部结果、身份不明或安全证明不足；保留互斥与引用，后台继续可证明的恢复 |
| completed | 是 | 达成本操作规定的成功完成证据 |
| failed | 是 | 确定未执行即拒绝，或已经安全停止且实际配置/引用收敛；必须符合第 6.6 节 |
| superseded | 是 | Resource DELETE 已接管；旧未决写和引用不因此消失 |

本批不新增 public `running/unknown/succeeded` operation 状态。自然语言“结果未知”对应 blocked 及稳定 reason；`Pending` 是持久外部写记录，不是 operation state。Gov CREATE/DELETE 未收到 owner ACK 的大写投递状态继续属于另一层，不在本批全局重命名。

终态 state、reason、completion_scope、结果版本不可被常态资源观察覆盖。允许在独立归因字段追加迟到回执，但不改终态结果。旧 `Finish` 中 `Desired == Observed → completed` 的总规则必须移除，以 action/phase 的合法终结转换替代。

### 4.4 Pending 与恢复停止栅栏

沿用 Instance 持久 Pending 概念，扩展每次 Notebook 控制写的记录：

| 字段 | 规则 |
|---|---|
| mutation_id | 每次持久写尝试的唯一 UUID；同一未知尝试重发保持相同 ID |
| operation_id / intent_version | 发起该写的已受理操作及版本 |
| phase / phase_sequence | 精确绑定本次阶段；同一个 operation 的 stop 与 start 不能混用 |
| kind | `set_stopped` 或 `update_mounts`；既有 CREATE/DELETE 类型保留 |
| target_uid / resource_version | 外部条件写使用的原值；重发不能自行刷新 |
| stopped_target | set_stopped 的目标布尔值 |
| target_mount_digest | update_mounts 的目标 frozen 集合摘要；完整目标在该 operation 中 |

新增最多一个 `RecoveryFence`，存入独立 `recovery_fence` 列；保存该写尝试的 mutation_id、不可变 operation/version/phase/phase_sequence、目标 Notebook UID/RV、stopped=true、所围住的原 Pending mutation_id，以及固定 intent-version/mutation 注解值和可重建 payload 摘要。原 Pending 和 RecoveryFence 同时存在是允许的唯一双控制写恢复情形，见第 6.5 节；禁止无限叠加恢复尝试。

“写尝试身份”和“当前推进权限”分开：DELETE 接管时继承原 fence 及其原 payload，不改原 mutation/operation/version 后冒称同一请求；新 DELETE 通过当前 LastOperationID/lease 获得观察和收敛权限。只有旧尝试已证明不再可能生效，才可归档该尝试并记录一个绑定当前操作的新 fence。

Pending/RecoveryFence 均由 Instance owner 保存，所有更新比较完整身份。新 Notebook CREATE/UPDATE 附带受管注解 `instance.ani.io/mutation-id`，继续写现有 `instance.ani.io/intent-version`；DELETE 请求不能附对象注解，使用持久 mutation 身份、既有 UID 条件和消失证明归因，不为补注解额外 patch。标记只用于 Notebook 写恢复，不假定传播到 STS Pod metadata。

### 4.5 引用关系与 guard

保留 `borrowed_volume_references` 按 `(tenant_id, instance_id, mount_path)` 的主键，扩展：

- state 为 pending/active/retiring/released；前三者全部属于保护态。
- `reference_generation > 0`；已 released 的路径重挂时加 1。
- 保存当前变更 operation_id/intent_version；引用推进必须匹配 path、generation 及所归属操作。
- 保护态索引覆盖 pending/active/retiring，不只覆盖 active。
- CREATE/Attach 新建 pending 的同事务设置 VolumeReferencesReleased=false；释放后的布尔值按当前全部保护态 NOT EXISTS 计算，不能从历史 Spec 是否为空推断。

新增 Instance-owned `borrowed_pvc_guards`，物理唯一键为 `(cluster_id, namespace, pvc_name)`；保存不可变的可信 tenant 归属，所有访问附 tenant 条件并校验 Namespace 归属。guard 表示该名字的使用/删除资格；不能只按 PVC UID 分锁，因为工作负载实际按 claimName 引用。

guard 保存 guard_epoch、当前删除 ticket/target_uid、删除 phase、write_started、外部结果、恢复 lease/epoch、retry_at 和安全原因。该表是有限的 PVC 删除资格及恢复记录，不是通用作业系统。具体协议见第 7 节。

新增同域实例/操作关系使用 tenant 复合 FK；涉及 cluster/PVC 的外部身份不伪造跨域 FK。迁移同时补权限 GRANT、schema 校验及全部读取路径。

## 5. 受理、互斥和 Gov 删除闭环

### 5.1 新变更的固定调用顺序

1. Gov 验证当前身份、租户和动作权限，解析原始输入为规范意图。
2. 调用 Resource LookupCommandReplay；命中返回原 ACK，异意图冲突。仅明确未命中才继续。
3. 检查 Gov Instance 专用 DELETE 闸门；功能开放开关也只阻止新意图，不阻止授权的原操作重放。
4. Resource 再次验证可信 tenant，先做只读 replay，再检查自身能力/维护门禁；新 Attach 只解析新增那一项 PVC，冻结 placement、UID 和资格；不为原键重新解析。
5. 进入 Instance owner 事务：锁实例 → 再次重放检查 → 检查 expected_version、删除、活动操作、稳定状态 → 按固定顺序锁涉及的 PVC guards → 保存引用、操作和资源版本 → 唤醒 work。
6. 同一事务提交成功后返回 durable ACK。任何中途失败均无半条受理；Kubernetes GET 不放在长持锁事务里，外部写前再复核身份。

固定锁序：实例身份锁/实例行 → 当前操作行 → 按 `(cluster, namespace, name)` 排序的 guard 行 → 引用行 → work 行。Storage guard 方法不反向锁实例行；多 guard 一律排序。

稳定 running：没有活动操作或未决控制写、Desired=RuntimeDesired=Observed=running，已初始化且应用版本非 0；worker 首次写前再核实真实 Pod/控制链及运行资格。稳定 stopped：同类持久前提且三状态为 stopped，worker 写前仍需第 6.3 节的真实停止证明。新动作不把 stale 数据库状态当作外部安全证明。

Restart 不允许 stopped 实例。Attach 已存在相同 mount_path，无论是否同一 PVC，都返回路径占用；修改只读方式按显式卸载后重新挂载处理。本批不增加原地改挂载入口。Detach 按当前 path 唯一定位，禁止 workspace，找不到返回明确错误。

Start/Stop 保留原公开请求和摘要行为，增加相同互斥及 DELETE 资格检查；不能 supersede 半途重启/挂卸。原有同目标幂等业务行为不借本批任意收紧。原 CREATE 重放和 DELETE 重放保持各自历史路径，但都不能重新创建 borrowed 使用。

### 5.2 Gov 已受理 DELETE 的判定

新增 Instance 专用 `HasAcceptedInstanceDelete`，按 tenant、Resource owner、container CREATE/DELETE action、resource ID、resource tenant 和原 CREATE 关联查询。排除明确 REJECTED 的 DELETE；已排队、正在投递、已 ACK 和结果未知的 DELETE 均关闭新意图闸门。不要修改共享 GPU projection 的 hasDelete 含义来替代专用判断。

在 Gov 原 CREATE 行锁内保证同一资源最多一条未拒绝的 DELETE；同键重放先于该检查。另一个新键在已有未拒绝 DELETE 时返回 INSTANCE_DELETING。CPU/GPU 两条入口采用一致规则。

### 5.3 Resource 明确拒绝 DELETE

只有以下全部成立，Gov 才能记录 REJECTED：

1. 是 container Instance DELETE，原 CREATE 关联正确；不是 VM/Sandbox/其他 owner 动作。
2. 当前 dispatcher 持有对应 lease generation，记录仍为 DISPATCHING，且从未收到有效 owner ACK。
3. Resource 明确返回 FailedPrecondition / `INSTANCE_VERSION_CONFLICT`，表示本次冻结删除意图没有被 owner 受理。

记录拒绝时保留原 key、expected_version、canonical、operation ID，不修改原账，不生成退额，不用更新版本后继续派发同一命令。传输失败、超时、ACK 丢失、无法确认 owner 是否受理均不符合上述条件，仍保持未知与闸门。

必须同时改 dispatcher、专用 guard、`createDeleteOperationTx` 的 Instance 分支、CPU/GPU 删除受理投影和 operation 查询。只改其中一处不算完成。

同键重放被拒绝的 DELETE：返回原 operation/resource，`replayed=true`、`result=REJECTED_VERSION_CONFLICT`，不重新派发。首次排队仍按原 DELETE 202 语义返回；后续重放沿用该受理响应形状，最终原因通过查询取得。

CPU/GPU 的 Gov operation 查询统一支持本地拒绝投影，避免去 Resource 查询不存在的 operation 而返回 404：

```json
{
  "id": "20000000-0000-4000-8000-000000000009",
  "instanceId": "30000000-0000-4000-8000-000000000001",
  "action": "delete",
  "intentVersion": "0",
  "state": "failed",
  "reason": "INSTANCE_VERSION_CONFLICT"
}
```

这里 intentVersion=0 只表示 Resource 未产生该删除操作版本；不能用请求 expectedVersion 冒充。该 Gov 本地拒绝不改变 Instance Desired/RuntimeDesired，也不适用“执行失败后安全停止”的 owner 规则。Resource 已接受的新动作 intentVersion 始终大于 0。

### 5.4 Resource DELETE 接管

Resource 按既有 DELETE 重放和 expected_version 规则受理；它可以越过普通操作互斥，但不绕过请求版本检查。成功受理事务中：

- version 加 1，Desired=RuntimeDesired=deleted，Network submission 按既有协议进入 closing。
- 原非终态普通操作成为 superseded；新 DELETE 成为 LastOperationID。
- 保留原 Pending、RecoveryFence、历史 Pod 身份和全部保护态引用，不因 superseded 清空。
- 封闭新的普通控制写；既有 lease 到期或归还后，当前 DELETE worker 才领取并收敛未决结果。

过期 worker 不能推进任何当前 phase、运行目标、CurrentMounts 或引用；旧 receipt 即使内容看似成功，也只能追加准确 mutation 归因。由当前拥有租约的 worker 根据事实决定解除 Pending，不允许旧 Finish 用 receipt 无条件清空当前 Pending。

DELETE 不重新验证 borrowed PVC 的存在性来决定是否可清理自身资源。所有计算对象/控制器退出、未知写收敛后，同事务释放该实例 pending/active/retiring，执行 NOT EXISTS 保护态查询，证明成立才设置 VolumeReferencesReleased=true。随后继续 workspace/Network/owner outbox 完整清理边界；不得仅执行过 UPDATE 就提前退额。

## 6. 固定执行阶段、证明和失败收尾

### 6.1 阶段及合法路径

新操作受理时 state=accepted、phase=validating、phase_sequence=1。每次阶段转换在 owner 事务中将 sequence 加 1；仅更新等待原因、观察结果或 lease 不加 sequence。不依据“实例版本相同”认定阶段相同。

| phase | RuntimeDesired | 唯一职责及退出条件 |
|---|---|---|
| validating | before_run_state | 验证持久前提和 owner 身份；运行态保存旧 Pod 身份；不得在本阶段静默修复配置 |
| stopping | stopped | 先提交阶段目标和 Network submission，再持久记录并执行 stopped=true 条件写 |
| waiting_stopped | stopped | 满足第 6.3 节完整停止证明；仅 Notebook 存在或 Pod LIST 为空不足以退出 |
| applying_mounts | stopped | 只对挂卸：按 frozen target 执行一次受管卷字段条件写，保持 stopped=true |
| waiting_template | stopped | Notebook 与受控 STS 模板均匹配目标；未决写收敛，停止证明仍有效 |
| committing_mounts | stopped | 原子提交 CurrentMounts、应用版本、引用状态与下一阶段；不调用外部写 |
| starting | running | 先提交阶段目标和 Network submission，核验全部当前卷，然后持久执行 stopped=false 条件写 |
| waiting_ready | running | 新 Pod 身份、UID 更换、Ready、实际挂载、GPU 资格验证通过 |
| waiting_network | running | 每轮重新验证当前 Pod 的运行资格，并在同一 UID 下完成当前 Network snapshot/Confirm/附着验证；无 Network 也必须重核运行资格 |
| recovering_stop | stopped | 已记录 failure_reason；收敛旧 Pending，必要时使用第 6.5 节恢复停止栅栏，保持/设置停止 |
| verifying_safe_stop | stopped | 停止证明成立，实际模板已归类并与 CurrentMounts/引用收敛；没有未决控制写 |
| done | 按结果固定 | completed 使用 final_run_state；安全停止后 failed 为 stopped；写前确定失败保持 before_run_state |

普通路径固定如下，表内重复阶段不得省略其证明：

| action/受理状态 | 路径 |
|---|---|
| restart/running | validating → stopping → waiting_stopped → starting → waiting_ready → waiting_network → done |
| attach-pvc 或 detach-pvc/stopped | validating → waiting_stopped → applying_mounts → waiting_template → committing_mounts → done |
| attach-pvc 或 detach-pvc/running | validating → stopping → waiting_stopped → applying_mounts → waiting_template → committing_mounts → starting → waiting_ready → waiting_network → done |
| 中断后的执行失败 | 原阶段 → recovering_stop → verifying_safe_stop → done；证据不足停留相应阶段并 state=blocked |

停止态操作若 validating 发现实际 Notebook 未停止、出现 Pod 或有控制器恢复趋势，不悄悄替用户停机并继续改卷；按确定前提失败或事实不明分类。运行态首次写之前保存的是本次旧 Pod 集合，后续新 Pod 不能覆盖该集合。恢复过程中遇到的新 Pod 另记到安全退出跟踪集合，包含 CPU，不只记录 GPUPods。

成功的停止态挂卸 completion_scope=template_applied；Restart 和运行态挂卸为 runtime_verified。Restart 不改变 CurrentMounts、引用或 applied_mount_version。挂卸提交应用版本为该操作 intent_version；即使后续启动失败，该应用版本仍保留。失败、superseded 不写成功 completion_scope。

已有 CREATE/Start/Stop/DELETE 不套用本表全部阶段，保留各自业务合同，但新版本受理时同样生成最小 execution：create=creating、start=starting、stop=stopping、delete=deleting，phase_sequence=1，满足各自原有明确完成证明后进入 done。阶段转移按 action+phase 共同解释，不能把 Restart 的 starting 路径套给独立 Start。DELETE 最终目标为 deleted，沿原清理顺序推进，不进入本表普通恢复阶段，不套用新三动作的 30 分钟预算。

历史无 execution 的已终结 operation 保持空 phase；新操作禁止通过旧的统一 `Desired == Observed` 分支自动完成。升级后接续旧 DELETE 的最小恢复记录见第 8.2 节，不伪造历史执行或修改原受理身份。

### 6.2 分离身份、配置和运行资格

Provider 在本批新增三类明确职责，允许内部复用现有 helper，但调用边界必须可单测：

1. `ObserveIdentity`：读取可信 placement、Notebook UID、STS owner 链、Pod owner 链、generation、停止状态和已知对象存在性。不能因 borrowed PVC 缺失、挂载内容不符、GPU 签名暂不可写或网络未就绪而拒绝返回已验证的 owner 身份。
2. `VerifyMountTemplate`：比较受管卷/容器挂载字段，保留 workspace 和其他非本批字段；输出 old/target/mixed 三类事实及差异，不以单个 annotation 代替内容比较。
3. `VerifyRuntime`：只在启动/运行完成资格中检查实际 Pod 挂载、当前 PVC UID、Ready、GPU owner 签名及网络关联。已有签名写仍绑定真实 Pod UID；签名成功不代替卷或网络验证。

如果 owner 链本身不匹配，不对该外部对象执行停止、更新或删除；记录 INSTANCE_OWNERSHIP_CONFLICT，state=blocked。若 owner 正确而配置错误，仍允许原操作安全停止和最终 DELETE。所有错误都必须区分“无法证明属于本实例”和“属于本实例但运行资格不满足”。

Notebook 控制写保持 Notebook UID；不删除重建 Notebook 来实施重启，不直接 patch STS，不强制删 Pod/finalizer。只修改本批受管 stopped/mutation 注解或卷模板字段，保留 image、compute、ACC、env、labels、Network 选择和 workspace 身份。

### 6.3 完整停止证明

普通修改模板、恢复后释放互斥、释放卸载引用都必须同时满足：

- Notebook 为已记录 UID、owner 正确且 stopped=true；若原 Notebook 已消失，普通挂卸不重建，转入失败收尾/最终 DELETE 的专门处理。
- 对仍存在的受控 STS，`spec.replicas=0`，`status.observedGeneration >= metadata.generation`；generation 对应本次观察的停止模板。已记录 STS 的直接 GET 返回 404 可以证明该 UID 不再存在；同名不同 UID 必须重新校验控制链，不能直接忽略。
- 对持久保存的旧 Pod 和恢复期间见到的 Pod，按 namespace/name 直接 GET 并比较 UID；原 UID 不存在才算退出，deletionTimestamp 非空不算退出。
- 成功 LIST 当前 Notebook/STS 控制链下的 Pod，结果没有仍存在的受控 Pod；列表失败、权限失败、仅按可改标签筛选得到空集合都不能当证明。
- Pod 检查后再次确认 Notebook 停止与 owner、STS generation/replicas 条件未变化；未决 set_stopped/update_mounts 已按精确身份收敛，旧启动请求不能随后成功。

只要 Notebook/STS 仍可能按旧模板生成 Pod，就不得释放 retiring 引用。模板完成证明须包含 Notebook 和 STS 两端；STS 不存在时仅当 Notebook 已停止、已知 STS/Pod 均消失且后续重建只能来自目标 Notebook 模板，才允许停止态 template_applied。没有权限读所需对象时保持 blocked。

### 6.4 模板应用与当前配置提交

Attach 受理解析新增一项；写前再查该 PVC 的 UID、Namespace 归属和 terminating 状态。不能将本次候选解析结果替换为同名新 UID。Detach 通过 CurrentMounts 定位和构造目标；停止态卸载不重新解析全部 target PVC，剩余某个 PVC 缺失也不能阻止移除本次选中的失效引用。所有 Start（含内部恢复运行）须重新验证完整 CurrentMounts，缺失项不启动。

模板更新以保存的目标集合生成受管 volumes/volumeMounts，保持其他字段；条件写绑定 Notebook UID/RV 与 mutation_id。每个 frozen mount 生成确定性受管卷名，必须保持既有当前集合的顺序规则；测试覆盖删除中间项和同 PVC 多路径，不让旧残留字段混入目标模板。

上游参照明确支持停止 Notebook 后复制 PodSpec 到 STS，但其 helper 不自动复制 Pod template metadata。因此用模板内容、控制器 generation 和 Pod UID 证明效果，不要求 Pod 拥有新 mutation 注解。[Notebook 停止及模板处理](https://github.com/kubeflow/kubeflow/blob/90e987bf87d3e7c900926310b00bfa16b59e41eb/components/notebook-controller/controllers/notebook_controller.go)、[STS 复制 helper](https://github.com/kubeflow/kubeflow/blob/90e987bf87d3e7c900926310b00bfa16b59e41eb/components/common/reconcilehelper/util.go)。

`committing_mounts` 的单事务必须校验 operation/version/phase_sequence/lease，锁相关 guard/reference，并一并完成：

1. CurrentMounts=target，mounts_initialized=true，applied_mount_version=operation.intent_version。
2. Attach 本次 pending→active；Detach 本次 retiring→released；匹配 generation 和 operation，不能批量按 PVC 名释放别人的引用。
3. execution 保存模板证明、提交标记及结果版本；更新下一阶段与 RuntimeDesired；有 Network 时同步提交对应 submission。
4. 停止态直接封存 completed/template_applied；运行态进入 starting。失败收尾中确认 target 已应用时，执行同样配置/引用提交，但下一阶段仍为 verifying_safe_stop，不转入启动。

提交前后崩溃均由事务和执行记录区分，不再写一次模板或增加 version。明确未应用不提高应用版本；只有 annotation 匹配、只有 Notebook 匹配而 STS 仍旧、或两端混合，都不算已经完成配置提交。

### 6.5 未知条件写与恢复停止栅栏

这是本批必须实现的有限恢复协议，不得简化成“看到 Notebook 就清 Pending”。普通控制写始终最多一个 Pending。

| 重新观察到的事实 | 允许的处理 |
|---|---|
| 原 UID 存在，mutation_id、目标停止值/卷内容均匹配 | 保存该写已应用证据，按完整身份收敛 Pending；继续等待控制器/Pod 的阶段证明 |
| 原 UID 存在，RV 仍是原值、无本次标记 | 原请求仍可能在途；正常重试只能沿用同一 UID/RV/mutation 和同一 payload，不能刷新 RV 后当成同一次尝试 |
| 原 UID 存在，RV 已变化 | 原条件写已不能再基于旧 RV 成功，但内容是否曾应用仍需核对；不能仅凭 RV 变化记成功 |
| 原 UID 已消失，或同名对象为其他 UID | 旧 UID 条件写不再可能作用于新对象；保留证明，不接管未经身份验证的新对象 |
| GET 超时、owner 不明、混合模板无法解释 | 保留 Pending、blocked 和保护引用，继续观察；不按时间清空 |

若旧停止写或模板写尚未知，正常操作可以重发完全相同条件请求。若已进入失败停止或 DELETE 接管，禁止重发旧 `stopped=false` 或目标模板写；使用下面的唯一例外打破等待旧启动结果的死锁：

1. 当前 lease holder 读取原 Notebook，验证 UID/owner，记录其当前 RV。owner 不明时不发送任何恢复写。
2. 在 Instance owner 事务中保留原 Pending，新增唯一 RecoveryFence，绑定首次发起该 fence 的恢复/删除 operation、phase sequence、原 mutation_id、目标 UID/RV；payload 仅设置 stopped=true 及该尝试冻结的 intent-version/mutation 注解，不修改挂载。若继承已有 fence，则保留其全部原始写身份和 payload。
3. 提交后发送该 UID/RV 条件写。若原请求先成功，栅栏因 RV 冲突不能误覆盖，重新观察；若栅栏先成功，旧条件请求之后因旧 RV 失效不能再成功。
4. 栅栏结果未知时只观察或重发相同条件写，不生成第三个未决写。若已明确因其他更新使该 RV 失效，可保存该尝试“不再可能生效”的证据，再在同一 fence 槽换新尝试；不得仅以 lease/TTL 到期换槽。
5. 原 UID 仍存在时，只有证明原 Pending 不再可能迟到生效，并确认其卷效果为 old/target 中某一集合，才能收敛原 Pending。stopped=true 不证明模板从未改变。原 UID 及其受控对象已全部消失时，允许独立 absent 证明：UID 条件写已失去目标、没有未知 CREATE、全链退出后，按第 6.6 节缺失 Notebook 分支处理，不要求不存在的模板再匹配 old/target。随后收敛栅栏自身结果并完成对应停止/清理证明。

RecoveryFence 是只减小运行风险的停止写，不是新的用户 Stop，不增加实例 version，也不受普通动作互斥阻止。DELETE 中使用栅栏不改变 Desired/RuntimeDesired=deleted。旧 worker 的迟到回执只能与原 mutation 对号，不清除 fence、不恢复 running、不更改 CurrentMounts。

上述 RV 证明只适用于带 UID/RV 前置条件的更新。既有 `create_notebook/create_workspace` 仍按原 CREATE 恢复规则处理；不能用“当前 GET 404”证明在途 CREATE 永远不会出现，也不能套用更新栅栏无条件释放最终删除引用。

### 6.6 失败、期限和可继续操作的条件

failure_reason 在首次进入失败收尾时固定；后续暂时性原因放 operation.reason/实例 reason，不覆盖根因。最终 failed.reason=failure_reason。保留安全错误分类，不记录令牌、env 明文或驱动原始凭据。

只允许两类 failed：

**A. 写前确定失败。** 没有发起任何可能生效的控制写，没有 Pending/Fence，现有配置/引用和原运行目标可证一致。例如 Attach 新 PVC 在首次停止前已明确被替换。释放仅本次未使用 pending 引用，或恢复仅本次 retiring→active，原运行状态保持；封存 failed/done。若外部配置已漂移且不能归类，不能以此例外直接释放互斥。

**B. 中断后安全失败。** 曾发起停止/模板/启动控制写，或无法证明完全未写；必须进入 recovering_stop，最终证明无 Pod、无未决控制写，且模板/CurrentMounts/引用已收敛。原子设 Desired=RuntimeDesired=SQL desired=stopped，并更新 Network，再封存 failed/done。旧成功目标 final_run_state 保留。

失败收尾按实际模板分类，固定处理如下：

| 已证明的实际配置 | CurrentMounts/引用处理 |
|---|---|
| before_mounts，目标写明确未生效或已被条件栅栏排除，且 template_commit_completed=false、CurrentMounts 仍为 before | 保持原 CurrentMounts/应用版本；Attach 本次 pending→released；Detach 本次 retiring→active |
| target，且 Notebook/STS 与停止证明均通过 | 按第 6.4 节提交目标与引用；保留已应用的新版本，然后以 stopped/failed 结束 |
| Notebook/STS 混合、非 old/target、身份不明 | state=blocked；不释放关联引用，不静默回滚，不宣告失败已安全完成 |

一旦目标已提交，之后观察到 before/mixed 视为外部漂移并 blocked；不能再走“未应用”分支倒退 CurrentMounts 或 applied_mount_version。

缺失的 Notebook 不能按模板完成处理。原 UID 和所有受控 STS/Pod 确实消失、无未知 CREATE/控制写时，可以按安全停止失败结束并保留最后已提交 CurrentMounts/其 active 引用；尚未使用的 pending 可释放，retiring 恢复 active。此时新 Start 仍按已有 NOTEBOOK_MISSING 规则拒绝复建，用户可最终 DELETE；不宣称普通挂卸恢复了实例。

新 Restart/Attach/Detach 的执行预算固定为受理时起 30 分钟，deadline_at 持久化，重试/重启不续期。合法 WFFC/CSI/调度暂态在预算内等待；确定不可继续或到期进入同一操作的安全停止流程。该预算不用于清空未知写、回收引用或证明删除完成；恢复可以超过预算，查询持续返回 blocked/阶段/根因。原 CREATE/DELETE 的期限合同不在本批改写。

保留既有 worker lease=45 秒、推进间隔 5 秒和常态观察 30 秒。单外部请求上限 15 秒，每次 Step 工作 context 上限 30 秒；多次调用耗尽预算则持久返回，不能持 lease 无限阻塞。deadline/取消后的写结果按未知处理，租约失效只失去推进资格，不代表外部请求撤销成功。

### 6.7 网络、GPU 和常态观察

沿用 Network 的 prepare、持久 submission、observe、confirm、release 协议；所有决定运行目标的消费者改读 RuntimeDesired。内部停启只更新已有 submission 的运行目标、Pod 集合和 snapshot revision；不因重启分配新 VPC、换子网或错误进入最终 closing。只有 DELETE 走最终 release。

新 Pod 身份先通过 Instance owner 持久发布，再按该 snapshot revision Confirm；不能用未提交内存里的新 Pod UID 确认。waiting_network 每轮重做当前 Pod 的 VerifyRuntime，Ready、挂载或 UID 任一变化即作废旧运行资格证明；重新发布/确认当前 UID，不为此再发一次停止/启动。最终事务必须核对同一 UID 的运行证明、Network snapshot revision、已确认 revision 及可用状态，不能拼接旧 Pod 运行证据与新 Pod 网络证据。旧 Pod/IP/terminal ticket 不转接到新 Pod；终端和日志目标保持现有实时身份校验。

GPU 的新 Pod 重新完成现有 owner 签名、ACC/资源契约、Pod Ready 和 Network 运行资格；沿用原 CREATE 账与执行快照。实际 CUDA 程序是 CP-04 LIVE 验收，不是每次产品 Restart 自动执行的探针，本批不为 worker 新增 exec 权限。CPU 不调用 GPU 签名或配额。常态 Observe 可以更新实例运行事实和诊断，但不能修改任何已终结 operation 的 state/reason/result version/completion_scope，或为一次 Restart 再制造第二次停启。

## 7. PVC 引用和 Storage 删除的完整协议

### 7.1 挂载资格与边界

沿用租户 Namespace、placement、Filesystem、非专属 workspace、合法路径/8 项上限和 UID 冻结；新增 guard 检查不替代现有资格。显式拒绝 Lost/terminating/Block、非本租户或同名不同 UID。Bound 和合法 Pending/WFFC 均可受理，后者的实际可用性在启动阶段验证。

只读同时写入 PVC volume source 与容器 volumeMount 的现有对应字段，实际只读写入必须失败。若 accessModes 仅允许 ReadOnlyMany，读写请求拒绝；RWO 不解释为单 Pod 锁，RWOP/RWX 的实际能力由 CSI/Kubernetes 约束。本批不实现跨实例 CSI 调度器或虚构一次候选检查等于一定能挂载。访问模式的真实验收覆盖目标环境实际支持的模式，不支持的模式明确记为未验证。

同一 PVC 可在多个实例或多个路径被引用，每个 path 有独立 reference_generation。卸载一个 path 不移除其他 path；name guard 只有不存在任何 pending/active/retiring 使用者时才允许平台删除。

### 7.2 引用转换表

| 事件 | 本次引用变化 | 必须同时成立 |
|---|---|---|
| 新 CREATE borrowed 受理 | 插入 pending，generation=1 | 同事务锁名称 guard，禁止已有删除 ticket |
| Attach 受理 | 无行则 pending/1；released 行则 pending/旧 generation+1 | 本次 operation/version 冻结；同路径保护态不覆盖 |
| 新 CREATE/Attach 模板已应用 | pending→active | 相应创建/应用证明及 owner 事务提交 |
| Detach 受理 | active→retiring，generation 不变 | 将本次 operation/version 绑定到该引用；旧绑定记录在 execution 的 before 事实 |
| Detach 模板移除并完整退出 | retiring→released | 所有受控模板和旧 Pod 不再使用，未决写收敛 |
| Attach 确定未应用 | 本次 pending→released | 精确 operation/path/generation，不能释放已应用引用 |
| Detach 确定未应用 | 本次 retiring→active | generation 不变；保留最后处理 operation 归因，原配置仍权威 |
| 模板已应用但恢复运行失败 | 保持目标提交后的 active/released | 不把失败等同配置回滚 |
| 实例 DELETE 最终计算清理 | 本实例全部 pending/active/retiring→released | 清理证明与 NOT EXISTS 检查同事务，不影响别的实例 |

所有 CAS 使用 tenant、instance、mount_path、reference_generation、预期 state 和当前负责该转换的 operation。DELETE 接管可在同一删除事务登记接管归因，随后仅由 DELETE 操作释放原代际；旧操作不再有转换权限。released 旧回执不得命中新代际重挂行。

### 7.3 跨领域窄端口及装配

guard/references 由 Instance owner repository 管理；Storage 通过注入的 `BorrowedPVCGuard` 窄端口调用 owner 方法。端口只接受可信 tenant/cluster/namespace/name/UID 和 ticket/lease 身份，返回删除资格及恢复结果，不暴露 pgx.Tx、数据库表名或 Instance 内部模型。

固定端口职责为 `ReserveDelete`、`ClaimDeleteRecovery`、`MarkDeleteWriteStarted`、`RecordDeleteObservation`、`CancelUnsentDelete`、`CompleteDelete`。DTO 与语义固定如下，Go 类型名可按仓库命名习惯落地，不能改变字段含义：

| DTO/方法 | 必要字段或事务语义 |
|---|---|
| PVCIdentity | tenant_id、cluster_id、namespace、pvc_name、target_uid；target_uid 一旦建 ticket 不再变化 |
| DeleteToken | ticket_id、guard_epoch、lease_owner、lease_epoch、lease_until |
| DeleteRecord | 上述身份/token；phase、write_started、last_write_outcome、last_observation、last_reason、retry_at、created_at、updated_at |
| ReserveDelete(identity, executor, ttl) | 同 UID 非终态 ticket先重放；新 ticket/过期 lease接管才签发token；有效lease不被重试抢占；不同UID未完成ticket不覆盖；cancelled后重试可重新建ticket，epoch+1并重查引用 |
| ClaimDeleteRecovery(executor, ttl) | 只领取 retry 到期且无有效 lease 的非终态记录 |
| MarkDeleteWriteStarted(token, targetUID) | 完整身份 CAS，记录可能已发送；sticky true |
| RecordDeleteObservation(token, observation) | 保存观察/原因/重试时间，释放本轮 lease；observedUID 独立保存，不能改 targetUID |
| CancelUnsentDelete(token) | 仅 write_started=false 且有确定不应删除原因，结束本次 barrier |
| CompleteDelete(token, evidence) | 原 UID 已消失的证明成立，同事务保存终态观察、结束 barrier并保留目标身份/结果；不先调用会释放lease的Record |

phase 固定为 reserved/deleting/observing/completed/cancelled，前三者关闭该名称的新挂载。所有修改 CAS ticket、guard_epoch、lease_owner/epoch 和 targetUID；lease=45 秒，单 Step 上限30秒，单外部请求15秒，重试间隔5秒。前台 HTTP 和后台 worker 复用同一有限推进函数，前台不能无 token 直删。

同 UID 的 completed ticket 不再发送 DELETE，公开结果仍按第7.5节的404/同名换UID规则返回；cancelled 表示先前未发送且已取消的尝试，允许解除占用后重新reserve并重做refs/usage。终态观察直接 Complete；只有非终态观察调用 Record 并释放本轮lease。

原 ticket 有有效 lease 时，重试请求可读取已有肯定结果，但无权执行外部写；若尚无确定提交结果，返回503等待原执行者/恢复，不仅凭 ticket 存在就返回 SUBMITTED。已明确提交且有安全观察时按原结果返回。每个结果区分保护引用冲突、同 UID ticket、UID 不符、依赖不可用和 lease 无效。

原 CREATE/Attach 在 Instance owner 自己的受理事务内使用相同 guard 行锁和引用检查，不经跨领域端口另开事务。Storage 不跨域读写私有 SQL，Instance 不接管 Storage 的 Kubernetes 删除执行。`cmd/ani-resource-service/storage.go` 和对应 composition 显式注入 owner 端口，并装配 Storage 删除恢复 worker 的启动、关闭和健康检查。

Storage 当前只装配 K8s/S3，不能假定它已有 PG 或恢复 worker。[当前 Storage 装配](https://github.com/zhangzhe-ctrl/ani-resource-service/blob/55092dbf05c284875555d422e75eeb5691cb9cfd/cmd/ani-resource-service/storage.go)。guard 端口缺失/不可用时，受影响的 Storage PVC DELETE 返回依赖不可用；不能 fallback 到旧直删。S3 及无关 Storage 动作保持原职责。

### 7.4 平台 PVC 删除与挂载的互斥

1. Storage 按原合同验证 tenant、Namespace、名称和 expected UID。同名 GET 404 返回 GONE；同名不同 UID 保留现有身份冲突响应。若有未完成 ticket，先按该 ticket 的身份收敛，不能误清同名新对象的 barrier。
2. `ReserveDelete` 在事务里锁名称 guard，同 UID 已有非终态 ticket 按第7.3节重放，completed/cancelled按各自规则处理；新 ticket 检查全部保护态引用，有任意引用则拒绝。无引用时冻结 target_uid、生成 ticket、guard_epoch 加1，phase=reserved、write_started=false，提交删除barrier。
3. 此后原 CREATE/Attach 获取同一 guard 必须拒绝。Storage 持有效 token 继续做已有 Pod/Notebook 使用方 LIST，以覆盖未登记的外部消费者；LIST 失败不跳过检查。HTTP 或 worker 恢复到 reserved/write_started=false，都必须重新核验 Namespace/UID/usage，不能从领取任务直接跳到 DELETE。若发现占用且从未可能发送 DELETE，取消该 ticket 的 barrier 并返回已有占用冲突。
4. 外部 DELETE 前，owner 事务 CAS 当前 ticket/epoch/lease，写 phase=deleting、write_started=true。提交后调用 Kubernetes UID-precondition DELETE；数据库事务不跨越外部请求。
5. 明确提交成功后 phase=observing，保留 barrier，直到直接 GET 证明原 UID 不存在。HTTP 超时、取消、断连或未知响应也保留 barrier，记录原因，由同一恢复 worker 接管。
6. 恢复 worker 只重发同名同 target_uid 的 UID-precondition DELETE 或观察。任何重试不得改成最新 PVC UID。原 UID 已消失（404，或同名为新 UID）时可完成原 ticket；完成 CAS 仍须匹配 ticket/target_uid/guard_epoch，旧回执不能解除后来的 barrier。

`write_started=true` 表示可能已发送，不是已成功；服务在该标记之后、真正发送之前崩溃，恢复仍按可能发送处理。lease 超时仅允许接管；从不自动解除 barrier。观察到原 UID 仍存在，也不能证明旧 DELETE 不会迟到。

`CancelUnsentDelete` 仅允许当前 ticket 且 write_started=false；一旦为 true，不通过该入口撤销。确定拒绝、权限恢复或 Kubernetes 暂时不可用均保留可审计原因并继续恢复；不以清 barrier 换取表面可用性。名称被新 UID 复用时完成的是原 UID 删除资格，不删除新 UID。

### 7.5 Storage 公开结果与恢复责任

保留现有 `SUBMITTED/TERMINATING/GONE` 状态，不引入用户可见的通用删除任务系统。成功提交/观察到终止中仍返回对应现有结果；响应未知或依赖不可用按现有 `STORAGE_BACKEND_UNAVAILABLE` 错误封装返回503，barrier 和后台恢复独立持续。客户端重试相同名称/expected UID 复用已有 ticket，不产生第二个删除目标。同名换 UID 可以作为内部原 ticket 已无旧目标的完成证据，但公开 DELETE 仍返回现有 VOLUME_IDENTITY_CHANGED，不能把新 PVC 当删除结果；保护引用占用沿用 VOLUME_IN_USE。现有 GET 继续读取当前对象，不把它改造成按删除 ticket 查询的接口。

恢复 worker 的 lease/retry 同样采用有界调用和 CAS，指标至少覆盖未完成 ticket 数、最老年龄、保护引用冲突、未知写次数和恢复失败原因。日志包含 ticket/epoch、tenant/实例或 PVC 业务名的安全标识；不包含 kubeconfig、token、env 或 Secret。

此协议保证本平台 Instance 受理与 Storage 删除入口之间的竞态受控。Kubernetes claimName 本身不能附带 UID 前置条件，其他控制器/管理员直接删除 PVC 不自动受 PG guard 管理。LIVE 前必须核实当前 Namespace 的删除准入/RBAC/使用保护；缺失时记录并关闭受影响能力的发布门禁，提出最小保护补齐，不将应用 GET 检查宣称为防止外部换卷的完整保证。

## 8. 增量迁移与升级兼容

### 8.1 显式迁移内容

在最新 `migrations/instance/` 目录分配下一个序号，新增迁移文件，不改已执行迁移/checksum。迁移覆盖第 4 节列、operation execution、引用代际/状态、名称 guard、索引、检查约束、tenant 复合关系、owner GRANT 与 schema 验证；应用启动不自动执行 DDL。

先加允许回填的列，再幂等回填，最后收紧 NOT NULL/check。runtime_desired 从持久 desired 初始化，同时保持 snapshot/network 的已有一致性。current_mounts 从原冻结 Spec 的内部完整记录初始化，保留名称、UID、顺序、readonly 和 placement；不能通过重新 Resolve 当前同名 PVC 偷换 UID。所有初始化设置 mounts_initialized=true，applied_mount_version=0，等待真实证明。

| 既有记录 | 回填规则 |
|---|---|
| 存活、非 DELETE 实例 | 当前集合取原冻结 borrowed 挂载；核对并保留已有 active 引用，赋 generation=1；缺失或与现存引用不一致时列入异常，不在正常迁移中自动补齐 |
| 已受理 DELETE、清理未完成 | 保留最后保护引用及真实 Pending；集合可初始化用于审计，不能因迁移重新允许启动 |
| 清理已完成的 deleted 实例/DELETE-first tombstone | CurrentMounts=[]，不新建保护引用；保持已证明释放/清理结果 |
| Spec 无附加卷 | 显式 []，不是 null；仍设置已初始化，防止以后回退 |
| 历史已终结 operation | 不伪造 execution 历史阶段；保留状态/摘要/ACK |

同一名称 guard 回填从 Namespace/实例 placement 可信信息建立租户归属。多个 tenant 被映射到相同物理 Namespace/name、UID冲突、保护引用缺失或不一致均生成逐项异常清单；受影响实例和名称保持关闭，不从候选列表猜测修复。引用缺失的受控重建只能作为单独恢复步骤，依据原冻结 Spec、已验证 owner/模板和名称 guard 在 owner 事务中完成，保留修复证据；不能让普通回填自动覆盖异常。

### 8.2 应用版本和旧活动操作

SQL 回填不证明 Kubernetes 已应用，不能把 applied_mount_version 直接设成当前 version。新 worker 验证 owner 身份及既有 CurrentMounts 与 Notebook/受控 STS 挂载模板一致后，写入基线版本1；停止态还须完整无 Pod 证明。该证明不要求 borrowed PVC 当前仍存在、Pod Ready、GPU/Network 就绪；这些属于单独的运行资格。失效卷错误继续显示，但不能因此永久停在0而无法显式 Detach。历史多次 Start/Stop 只增实例意图版本，不代表发生过挂载修订。新 CREATE 在其真实模板/创建证明后同样将初始应用版本从0设为1。

升级前列出所有非终态普通操作、Pending、Network submission 和 DELETE 清理任务。旧活动 CREATE/Start/Stop 先用原版本完成/安全收敛；若不能在切换窗口前完成，保持受影响对象的新动作关闭并形成专门恢复清单，不能批量标 completed、清 Pending 或改为新的 Restart。DELETE 可以由兼容的新执行者继续原清理协议，所有未决证据必须可读。

接续旧 DELETE 时，在 owner 事务中补最小 execution：schema_version=1、phase=deleting、phase_sequence=1、final_run_state=deleted，保留原 operation ID/version/ACK/hash/LastOperationID/SQL desired；不增加实例version、不伪造canonical、不添加30分钟预算。历史 Pending 允许缺少新字段，读取为 legacy 格式；旧 set_stopped 也可能没有 stopped_target，不能根据现在 deleted 目标反推原payload。可为这份旧记录一次性分配内部 tracking ID，并标记 provider_marker_expected=false，供新 Fence 关联；不声称旧请求已带新 mutation 注解。旧条件更新通过保存的 UID/RV 和新停止 Fence 收敛，未知 CREATE 继续原恢复规则。

应用版本0时新 Restart/挂卸返回 INSTANCE_STATE_CONFLICT；允许既有任务恢复和只读查询。初始化未完成不开放新 Start；对已初始化且 owner 可证、没有活动普通操作/未决控制写的对象，保留既有 Stop 的安全路径，Stop 不要求借用PVC存在或Ready。模板无法证明的对象保持对象级异常，并保留安全Stop/最终DELETE；不得以解除门禁代替事实证明，也不要求全体实例健康才开放其他已证明对象。健康新版本中，当前授权的原键重放先于这些门禁。

### 8.3 必须采用的发布顺序

1. 安排维护窗口，停止相关旧写入口的新流量并排空请求，按第 8.2 节收敛旧普通操作。旧二进制没有本批开关时，通过现有运维入口隔离/停服务实施，不假定旧代码认识新开关；窗口内相关请求、重放和查询可能503，不承诺零停机。可用的旧worker继续已受理恢复，直到退出步骤。
2. 发布正式 Resource API module，Gov 以不可变版本生成和编译；此时新路由保持关闭，不能调用尚不存在的服务方法。
3. 停止全部旧 Instance writer/worker/严格 snapshot 读取进程及旧 Storage DELETE 执行者，确认执行lease归还/失效，并盘点所有已出站/结果未知的旧 Storage DELETE。对未收敛目标按原 tenant/placement/name/UID 导入 write_started=true 的 guard ticket；无法准确建立身份的对象保持名称级挂载禁用。HTTP超时、进程退出或lease失效不证明外部写已结束。执行显式迁移和数据核对，禁止迁移期间旧写继续入库。
4. 启动兼容新列/新 execution 的 Resource Instance 与 Storage guard/recovery 组合；Resource 自己的新意图开关保持关闭，先验证查询、旧 DELETE/Storage ticket 恢复、初始配置证明和跨领域健康。旧 Gov 尚未升级也不能绕过 Resource gate 写入新变更。
5. 发布 Gov 对应版本，核验 mTLS 精确方法白名单、生成合同、权限与 API 关联，运行 BOUNDARY。
6. 在迁移/共同安全底座就绪后，按 CP-01 至 CP-04 的能力门禁开放新意图并执行 LIVE；健康新版本功能关闭不阻止当前授权用户重放已有 key 或查询。新能力缺省关闭，Gov开但Resource关时新键仍被拒绝且无任何受理副作用。

不能混跑旧 Instance worker，也不能以“只加列、旧 snapshot 兼容”为依据继续让旧渲染器读取原 Spec。当前挂载一旦改变，旧二进制会使用过时配置，属于行为不兼容。已有其他业务共享进程的退出/恢复范围应在具体发布单中列明，不能通过绕过新读取逻辑留旧执行者。

### 8.4 回退与修复

出现问题先关闭新意图，保留能读取新状态的 worker 完成恢复；保存当前配置、操作、guard 和引用证据。若已经写入新 execution、改变 CurrentMounts 或存在新 guard ticket，不直接部署旧二进制，不把 Spec 覆盖回 CurrentMounts，也不删除新字段“恢复兼容”。

本批提供向前修复 runbook：按 mutation/ticket 身份观察、恢复停止、收敛实际模板、保留数据、最终 DELETE 清理。只有完全证明没有新语义记录、无新控制写且旧程序可正确消费数据后，才能在独立发布审批中考虑旧版回退；本方案不预先授权破坏性回滚。

## 9. 工作包、文件责任和交付顺序

### 9.1 工作包与门禁

CP 是实现和验证里程碑；不是四次自动生产发布授权。跨领域 guard、迁移兼容和权限闭环未就绪前，不对不受控用户开放挂卸。隔离测试环境可以按里程碑验证已具备的子集。

| 包 | 必须交付的实现 | 可运行退出条件 | 前置/限制 |
|---|---|---|---|
| CP-01：重启和命令闭环 | 当前配置/RuntimeDesired 基础持久化与读取；Restart 完整阶段、Pending/Fence、失败单调性、DELETE接管和过期worker保护；Start/Stop 摘要兼容与互斥；五个新 RPC 的冻结合同及必要服务注册；Gov 授权→Lookup→闸门；Instance DELETE 明确拒绝/查询闭环；重启权限 | 正式 Gov HTTP→Resource gRPC→真实 PG→Notebook/Pod 完成 CPU Restart，旧 UID 退出、新 UID 运行、workspace/现有 borrowed 数据保持；响应丢失、进程恢复、明确失败安全停止、DELETE接管及拒绝解闸均有证据 | 首先交付这条真实主链，不先长期建设纯框架；PVC 写路由关闭；第 8 节初始化与旧进程边界适用 |
| CP-02：停止态 PVC 与删除保护 | CurrentMounts 模板写/验证/原子提交；四态引用、同路径generation CAS、失败引用收尾、Instance DELETE释放全保护态及NOT EXISTS；原 CREATE 与 Attach 共用 name guard；Storage durable 删除及恢复装配；实例范围候选、Attach/Detach HTTP/权限/查询；增量迁移 | 停止态挂卸完成后仍无 Pod；再次 Start 挂载正确；卸空不回退；失效卷可安全卸载；真实 PG 交错证明挂载/Storage 删除互斥；A28/A32必须通过 | CP-01 通过；Storage guard/恢复 worker/代际及完整释放缺失时不能开放Attach；最先核实controller模板传播 |
| CP-03：运行态、异常与抢占 | 组合既定停止/模板/启动阶段；显式中断参数；在运行态强化未知模板/启动回执、双worker/旧lease、DELETE接管、同路径代际和完整释放的故障矩阵 | 运行态挂卸各用一个 operation 完成；未同意中断拒绝；关键阶段进程退出/ACK 丢失可恢复；失败不退款、不复活引用；DELETE 后无迟到复建 | CP-01/02共同安全机制已经实现；本包扩组合和验证，不后置底层恢复保护 |
| CP-04：GPU/网络/权限及正式交付 | GPU 新 Pod 签名/CUDA/原账；VPC/SG/Network/terminal 回归；两租户/套餐/动作权限负例；正式 module/生成/权限登记；迁移和恢复 runbook；证据索引 | CPU/GPU 后端矩阵完成，原账不变，最终 DELETE 仅退一次且 borrowed PVC/PV/文件保持；所有必需 CODE/PG/BOUNDARY/LIVE 有结果 | 前端和 BROWSER 不属于本包，不作为后端完成条件；真实 GPU/CSI 环境不足的项明确保留未验证 |

### 9.2 单一文件责任

| 负责人 | 独占修改范围 | 可并行边界 |
|---|---|---|
| Resource 集成人 | `api/instance/v1/{instance.proto,business.go,configuration.go}`、Instance types/PG/worker、migrations/schema、共享 composition 和运行目标/Network 接线 | 其他 agent 提交 provider/Storage adapter 或测试补丁，集成人串行合入上述共享文件 |
| Resource provider/Storage owner | `internal/data/instance/{notebook.go,pvc.go}` 的指定切片，Storage usecase/adapter/recovery、相关测试 | 先与集成人冻结内部 DTO/窄端口；不得自行改状态名、事务归属或 API |
| Gov owner | Instance Proto/HTTP/service/client、专用删除 guard/dispatcher/ledger 对应分支、权限源表/登记脚本、生成/OpenAPI、模块依赖和合同 | 不修改 Resource 私有源码、不用本地 replace；共享配额路径只改精确 Instance 分支并回归 VM |
| 独立审阅者 | 合同/状态机/竞态/证据审阅 | 不在别人持有的文件并行编辑；指出具体反例，由负责者修复 |

一个文件同一时间只由一名 agent 编辑。前端仓库不分派开发任务，不要求修改 Console Hook/API/UI。角色名不要求固定人员；开始每个切片时记录实际负责人和文件清单。

### 9.3 必须核对的代码落点

| 仓库/层 | 本批核对和修改范围 |
|---|---|
| Resource API/service | Instance Proto、规范化 helper、配置 DTO、service 错误转换、新五 RPC、精确 identity allowlist、Gov 调用矩阵 |
| Resource biz/data | 受理事务、全部 scan/decode/Get/List/Claim、Finish/BeginMutation、phase/Fence、Notebook/Pod/STS 观察、CurrentMounts 渲染、引用/guard、Network 运行目标与持久发布、终端/日志目标 |
| Resource Storage | 删除 usecase、UID 条件 adapter、BorrowedPVCGuard adapter、持久恢复 worker、composition/关闭/健康、必要只读 RBAC；不改 S3 主链 |
| Resource schema | 显式迁移、schema/version 检查、约束/索引/GRANT、升级异常扫描、旧执行者退出和回退说明 |
| Gov 接口与授权 | `api/protos/instance/service/v1/instance.proto`、`api/protos/admin/service/v1/i_instance.proto`、`instance_http.go/instance_input.go`、service/client、公开错误映射和 OpenAPI |
| Gov DELETE | `instance_service.go` 及 CPU/GPU 分支、Instance 专用 guard、`quota_dispatch_worker.go`、`createDeleteOperationTx`、GetInstanceOperation 本地拒绝投影；共享 VM/其他 owner 不改变语义 |
| Gov 权限源 | `docs/contracts/permission-code-capability-map.tsv`、`scripts/permcode/gen-go-maps.py` 生成结果、`scripts/ops/sql/register-instance-permissions.sql`、API/permission/role/INSTANCE capability 关联与策略刷新 |
| 正式文档 | Resource `docs/specs/instance-configuration-network.md` 的创建后冻结/当前配置段、`governance-calls.md`；Gov `container-instances.md` 及实际承载这些 HTTP 合同的文档；唯一状态条目和证据索引 |

Gov 权限登记须先对目标 API 目录做 dry-run/diff，确认新增三条动作与候选 GET 的方法/路径、permission code、角色和 INSTANCE 套餐能力映射正确。生成文件由源表生成，不手改 `pkg/permcode/capability_gen.go`。不执行会删除其他模块权限的全局 SyncPermissions 来“补齐”；使用本批明确增量登记并刷新策略，直接 API 验证生效。

### 9.4 每包必须提交的说明

每个包提交简洁 PR/变更说明：当前问题、该包改变的行为、状态和接口兼容、迁移要求、已运行验证、仍未验证项。共享 SQL/API 先列清合同再实施；不能以“其他 agent 后续补”跨过本包退出条件。实际代码偏离本文固定设计时，先修订同一方案并完成对应审阅，不维护私有平行版本。

## 10. 必需验收矩阵

### 10.1 证据分层

| 层 | 有效证据 | 不能替代它的结果 |
|---|---|---|
| CODE | 相关 Go/生成/静态门禁、规范字节 golden、状态转换和 provider 边界行为测试 | 仅编译成功、方法存在 |
| PG | 真实 PostgreSQL 事务/约束/并发交错、lease/进程恢复、迁移与数据往返 | 内存 repository 或 mock transaction |
| BOUNDARY | 真实 Gov HTTP 路由/授权/ProtoJSON、mTLS Resource 方法、正式 module、错误/重放/查询合同 | 直接调用私有 Go 方法或绕过 Gov |
| LIVE | 真实 Notebook Controller、Pod/STS/PVC/CSI/Network/GPU 的身份、数据和清理证据 | operation accepted、单独 Pod Ready、旧环境截图 |

不设置前端或 BROWSER 验收层。每条证据记代码 SHA/module ref、环境/镜像/controller/CSI、时间、输入业务意图、operation/实例身份、关键前后状态和安全化日志；不要收集 token、凭据或 env 明文。阶段故障注入只停本批隔离服务/测试实例，不干扰已有业务。

### 10.2 接口、授权、重放及 Gov

| 编号 | 层 | 场景与必须结果 |
|---|---|---|
| A01 | CODE/BOUNDARY | 三个动作 JSON 精确样例通过；null/重复键/未知键/别名/数值版本/科学计数/伪 tenant/错误路径 ID/额外 query 均按 §3 拒绝；旧 Start/Stop 兼容输入仍通过 |
| A02 | CODE/BOUNDARY | omitted 与 false 摘要相同，true 不同；旧 Start/Stop 历史 golden hash 逐字不变；变更 PVC/path/只读/中断/expectedVersion 任一项产生不同规范意图；2^53+1与int64上界在canonical解析/重编码中无浮点失真 |
| A03 | PG/BOUNDARY | 同键并发只创建一个 operation；事务内晚命中 replay=true；响应丢失后原 key/版本/正文返回原 ACK；不能因当前版本已前进而失去重放 |
| A04 | BOUNDARY | 当前无权限用户不能通过 Lookup 泄露结果；健康新版本中旧已受理操作在DELETE闸门/功能关/PVC消失后仍可重放；Gov开而Resource关时新键无version/operation/引用副作用，旧键可重放；worker恢复不受开关阻止；异意图仍冲突 |
| A05 | BOUNDARY/LIVE | 两租户、动作权限、INSTANCE 套餐、伪 actor/header、跨 placement 负例；有 attach 权但无 create 权者可读本实例候选；运行态挂卸无需额外独立 Start/Stop 权限 |
| A06 | PG | Gov 原 CREATE 锁下只有一个未 REJECTED Instance DELETE；双新 key/CPU/GPU 并发一致；VM 既有行为不回归 |
| A07 | PG/BOUNDARY | DELETE 明确版本拒绝只由当前 lease 且未 ACK 记录 REJECTED；无退额；解闸后新意图可受理；原 key 返回同一 REJECTED_VERSION_CONFLICT，不改版本重发 |
| A08 | BOUNDARY | CPU/GPU 本地被拒绝 DELETE 的 operation 查询均 200/state=failed/intentVersion="0"/正确 reason，无 Resource 404；已 ACK 的正常 DELETE 仍走原 owner 查询 |
| A09 | PG/BOUNDARY | DELETE 超时/ACK 丢失保持未知和闸门，不能误 REJECTED；陈旧 dispatcher generation 的拒绝回执不能覆盖新状态 |
| A10 | CODE/BOUNDARY | 目标配置null与对象存在但volume_mounts=[]可区分；pending_change子字段精确snake/int64字符串；当前配置/应用版本/activeOperationId/phase正确投影，终态重复查询不变化 |

### 10.3 执行、状态和恢复

| 编号 | 层 | 场景与必须结果 |
|---|---|---|
| A11 | LIVE | CPU Restart：实例/Notebook/workspace/PVC UID 保持；旧 Pod 退出、新 Pod UID 不同；workspace 与每个当前 borrowed 文件内容/哈希保持 |
| A12 | PG/LIVE | Restart 中 Desired=running、RuntimeDesired=stopped，SQL/snapshot/独立列/Network 同步；Get/List/Claim/进程重启均不解码失败，不提前宣告完成 |
| A13 | PG | Start/Stop/Restart/Attach/Detach 双请求互斥；普通 Stop 不能 supersede 活动变更；实例 version 只在新命令受理时加 1，phase推进/重放/应用提交不加 |
| A14 | PG/LIVE | 在受理、Pending提交后写前、停止写后、旧 Pod 退出后、模板写后、配置提交后、启动后分别退出/丢响应；从保存阶段恢复，已完成 Restart 不再停启一次 |
| A15 | PG/LIVE | 未知 stopped=false 后进入失败/DELETE；恢复停止栅栏覆盖原请求先成功、栅栏先成功、栅栏响应未知、RV被其他更新推进四种顺序；无第三个未决写，无复活 Pod |
| A16 | PG | old intent/phase/lease/mutation 回执只能追加归因，不能清除新 Pending/Fence、恢复引用或改运行目标；相同 version 的 stop/start 不混认 |
| A17 | LIVE | Pod terminating、LIST失败、仅 label空结果、STS replicas尚未归零/observedGeneration落后均不算停止；保存的旧 Pod 失去标签后仍按 name/UID 检查 |
| A18 | PG/LIVE | owner匹配但卷缺失/GPU签名/网络未就绪仍能安全停止；owner不匹配则 blocked 且不修改别人的对象 |
| A19 | PG/LIVE | 写前明确失败保持 before 状态并只释放本次未使用引用；中断后失败必须安全停止；实际 target 已应用则保留目标和应用版本，mixed 保持 blocked |
| A20 | PG | 超过30分钟触发原操作安全停止，重启不续期；未知写/身份不明时期限和lease到期都不释放引用；failed经常态观察仍failed，root failure不被等待原因覆盖 |
| A21 | PG/LIVE | Resource DELETE 以正确版本接管，旧操作 superseded但Pending/refs保留；错误版本拒绝不接管；迟到worker不复建Notebook/Pod或重新激活引用 |
| A22 | LIVE | 缺失 Notebook 的普通操作不重建；原 UID/STS/Pod全部消失后按安全失败规则保持配置，Start不偷建；最终DELETE可完成 |

### 10.4 卷、并发删除与迁移

| 编号 | 层 | 场景与必须结果 |
|---|---|---|
| A23 | LIVE | 停止态 Attach/Detach：Notebook和STS目标模板一致、全程保持停止；完成为template_applied，之后Start使用目标配置 |
| A24 | LIVE | 运行态挂/卸：缺省/false拒绝，无版本/引用副作用；true只产生一条操作，恢复running并runtime_verified |
| A25 | PG/LIVE | 卸载全部卷后CurrentMounts=[]，进程重启、configuration查询、Start、原CREATE重放都不恢复Spec旧卷 |
| A26 | CODE/LIVE | 同PVC多路径、删除中间项、只读、路径遮盖/超限、Block/Lost/terminating/专属workspace均按规则；未改路径和workspace保持 |
| A27 | LIVE | 失效PVC、同名新UID不得挂回；停止态可卸载选定失效路径；剩余其他失效卷不阻挡本次Detach，但后续Start必须拒绝缺失当前卷 |
| A28 | PG | pending/active/retiring均阻止Storage删除；首次Attach同事务把VolumeReferencesReleased置false；同PVC其他实例/路径仍保护；released路径重挂generation+1，旧receipt不能释放新行 |
| A29 | PG/LIVE | 分别强制交错“Attach先锁guard/Storage先reserve”和“原CREATE先锁guard/Storage先reserve”；只允许一方通过，无检查到写之间无保护窗口 |
| A30 | PG/LIVE | Storage在reserve后usage前、mark-write后发送前、DELETE响应丢失、观察前崩溃均恢复；reserved恢复先重做usage；lease过期不清barrier；同名新UID不被重试删除 |
| A31 | PG/BOUNDARY | Storage发现外部占用时仅write_started=false可取消；取消后同UID新请求可重新reserve；同UID非终态重试不抢lease；终态观察直接Complete不先释放token；guard缺失503不fallback；公开UID冲突/删除状态保持 |
| A32 | PG/LIVE | DELETE最终释放本实例所有保护态且NOT EXISTS通过才标VolumeReferencesReleased；不误释别实例；borrowed PVC/PV/文件保留，系统workspace正常回收 |
| A33 | PG | 从真实历史结构迁移，完整current_mounts/顺序/UID/readonly往返；null与[]、未初始化与空配置区分；已清理tombstone不回填保护引用；可重入回填不改变checksum |
| A34 | PG/LIVE | applied_mount_version初始0禁止新Restart/挂卸；owner/模板证明后基线1，不依赖PVC存在/Ready；迁移后失效卷可经安全Stop和基线证明进入A27；restart不改应用版本，Start/Stop版本不冒充挂载版本 |
| A35 | PG/BOUNDARY | 旧Instance/Storage执行者均退出；旧Storage在途/未知直删导入ticket或保持对象禁用；legacy Pending与旧DELETE正确恢复，不批量清除；拒绝旧Spec覆盖；健康新版本功能关后原key可重放 |
| A36 | LIVE | 核验实际Namespace PVC删除准入/RBAC/使用保护；记录边界，外部旁路保护不成立时相关发布门禁不通过，不能以平台guard测试冒充覆盖 |

### 10.5 GPU、网络与原账

| 编号 | 层 | 场景与必须结果 |
|---|---|---|
| A37 | LIVE | GPU Restart及停止态/运行态挂卸：新UID签名、实际GPU/CUDA可运行、当前卷读写/只读通过；CPU结果不代替 |
| A38 | PG/LIVE | 内部停启Network读RuntimeDesired，使用当前持久Pod/snapshot Confirm；原VPC/SG保持，新网络可用，旧IP/终端票据不转接，新终端/日志目标正确 |
| A39 | PG/LIVE | 重启/挂卸/失败前后CREATE canonical、Spec、ACC快照、原charge、占额不变，无新release outbox；CPU无GPU调用 |
| A40 | PG/LIVE | 正常最终DELETE在compute/workspace/全部refs/network/未知写全部清理后按原账退一次；Gov旧文档中若仅写“compute后退额”须同步纠正 |
| A41 | BOUNDARY | 正式Resource module与GOWORK=off消费通过；新RPC精确mTLS白名单、权限源生成和角色关联生效，保留最新Sandbox/Inference共享注册 |

### 10.6 首条真实路径

先用隔离 CPU 实例贯通：创建 → 写 workspace 标记 → Restart 读回 → Stop → Attach 已有测试 PVC → Start 写卷标记 → Restart 读回 → Stop → Detach → Start 确认该路径无卷挂载 → Stop/重新 Attach → Start 读回原卷文件 → 正常最终 DELETE，核验 borrowed PVC/PV/文件保留。每步从 Gov 取得 ACK/operation，记录对应版本，等待明确结果后再发下一意图。

然后以相同接口补运行态挂卸、GPU、Network 和故障注入，不以私有 SQL 修改状态、绕过授权、直接改 STS 或 force delete 代替产品路径。用于验收的预置 PVC 不因此成为实例所有；清理前先核对保护清单。

## 11. 执行环境、登记和收尾

1. 本地仅编辑、阅读、Git 和轻量静态检查；生成、依赖工具、编译、行为/PG 测试、镜像和 API 驱动使用 `ssh fedora`。本批不采用本地重任务回退；按 Resource `docs/remote-execution.md` 的隔离快照、工具固定、凭据排除要求执行，不覆盖其他任务目录。
2. 实施前确认真实目标 cluster、Namespace/tenant、CPU/GPU 规格、可用测试 PVC及访问模式、Notebook Controller/CSI/Network镜像和现有保护策略。源码参照版本不代替部署事实；历史其他环境证据不算本批LIVE。
3. Resource 相关修改运行针对性的行为/PG/恢复测试和工程规定的 `make verify`；Gov 执行其当前生成/模块/权限/行为门禁。只因具体失败或跨模块风险扩大验证，不为文档整理重复无关全量测试。
4. Resource API发布正式不可变module ref；Gov使用`GOWORK=off`，不留本地replace，不在服务仓复制Resource私有实现。生成的Proto/OpenAPI/权限映射与源文件一并提交并核验无遗漏。
5. 环境权限登记、迁移、镜像发布和服务部署按照届时明确范围执行；本次只是方案修订。遇到自动审批或既定环境门禁阻挡，记录准确动作与原因，继续不受影响的工作，不能以关闭鉴权/绕过PG等方式通过。
6. 模板不传播、CSI无法满足、Network换代或PVC外部保护缺口，保留原身份和证据，实施本主链最小必要修复。未经扩展任务不修改 kcn/ACC/SG 或共享 runtime；不自动换卷、换网、换规格或退额。
7. 用正常产品API清理本批测试实例；保留预先登记的 borrowed PVC/PV/文件和已有业务资源。核对无本批遗留活动operation、孤立控制对象或未解释删除ticket；结果未知时明确留档继续恢复，不伪报已清理。

## 12. 最终一致性核对与完成标准

### 12.1 实施前后都必须逐项核对

| 主题 | 唯一合同 | 不允许出现的第二种解释 |
|---|---|---|
| 范围 | Resource+Gov后端；Storage相关保护在Resource内 | 把Console/BROWSER重新列为本批工作或完成门禁 |
| 意图/应用版本 | 新命令受理增version；挂载证明提交才改applied；restart不改挂载版本 | 每阶段加version，或以最新Start/Stop版本冒充挂载应用版本 |
| 原始/当前/目标配置 | Spec不变；CurrentMounts权威；operation保存目标 | 空当前集合回退Spec，或受理即把目标当当前 |
| 运行目标 | Desired最终目标；RuntimeDesired阶段目标；Network读后者 | 数据库加载把阶段目标覆盖回最终目标 |
| 操作结果 | accepted/blocked非终态；completed/failed/superseded单调 | 自造unknown/succeeded/running状态，或常态观察洗掉failed |
| 幂等 | 当前授权→只读重放→新意图闸门；原键保留原版本 | 已删/已变版本/功能关闭后失去原受理，或换hash破坏旧Start/Stop |
| 未知写 | 精确Pending；唯一停止Fence例外；事实收敛 | 看到对象即清Pending，或先清Pending才能安全Stop而形成死锁 |
| 失败 | 写前确定未执行，或中断后完整安全停止 | 超时直接failed/释放引用；目标已应用却静默回滚 |
| 删除 | Gov持久闸门与精确REJECTED；Resource接管保留证据 | owner未受理的DELETE查询404，或迟到worker复建 |
| PVC保护 | 原CREATE/Attach与Storage同name guard；pending/active/retiring全保护 | 一次SELECT/GET当互斥，UID分锁绕过claimName，漏原CREATE |
| 引用代际 | path/generation/operation精确CAS；DELETE释放全保护态 | released重挂被旧回执释放，或只释放active后宣告清理完成 |
| 升级 | 显式迁移、真实初始证明、退出旧执行者 | SQL猜测已应用、tombstone复活、混跑旧Spec渲染器 |

### 12.2 仍需在真实环境取证的事实

本文已固定设计选择；以下是实施前/验收时必须采集的环境事实，不能由 agent 自行假定：部署中的 Notebook Controller 是否具备已引用源码的传播行为；可用 CSI/访问模式和 WFFC；PVC 删除准入及 RBAC；GPU/ACC签名和CUDA；Network及终端换代；最终目标集群/租户和正式模块版本。每一项写明已验证/失败/未验证及证据，不用“预计支持”替代。

### 12.3 后端完成的定义

交付两仓实现、正式API模块及生成合同、权限增量登记、显式迁移与异常清单、失败/未知写/删除恢复说明，以及 A01–A41 对应分层证据。允许将环境不可用的项目明确标为未验证，但不能在存在必需 BOUNDARY/LIVE 未验证时宣布整批后端已经验收完成。

本版不要求前端实现。后端进度查询合同、重放身份和测试API驱动属于本批；页面如何触发、缓存或轮询由未来前端任务使用该合同另行对接。
