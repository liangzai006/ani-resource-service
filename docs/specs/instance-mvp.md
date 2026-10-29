# Res 容器实例合同

> 2026-10-19 增量合同：[挂载、配置与 VPC 接入](instance-configuration-network.md)替代下文固定工作目录、排除已有用户卷及仅复用默认网络的限制。专属工作盘仍最终回收；新增 borrowed 卷只解除引用并保留原卷与数据。显式 VPC 采用受控工作负载合同，当前真实验收结论只见[执行状态](../execution/status.md#instance-configuration-and-network-2026-10-19)。

> 2026-10-27 释放合同调整：明确 DELETE 后，由 Resource 完成整个实例的计算、专属工作盘、网络与借用引用清理，再持久化最终 owner 清理事实和退额 outbox。Resource 与 ModelDev 共用 Gov 现有 `ReportResourceReleased`，不调用 ACC `ObserveRelease`；旧 ACC 摘要是 Resource 本地门槛，从未作为 Gov 请求字段。以下 §8.3、§9.1 和 §10 是当前释放规则；实施顺序见[整改方案](../plans/container-instance-release-without-acc-20261027.md)，实际结果只见[执行状态](../execution/status.md#container-instance-owner-refund-2026-10-27)。

> 2026-10-28 生命周期增量：以下 §8.4、§9.2 与[当前挂载及 Storage 协调合同](instance-configuration-network.md#43-持久化和投影)替代历史的创建后卷配置不变、双态引用和简单启停恢复说法。实施输入为[已审 v2.0 方案](../plans/container-instance-restart-pvc-20261028.md)，迁移与恢复见[运行手册](../runbooks/instance-mvp-fedora.md)，部署和 LIVE 结果只见[唯一状态](../execution/status.md#container-restart-pvc-20261028)。

> 本版基于 v1.4 和后续讨论修订，面向当前受控实验环境。文中接口、字段及工作包是实施约定，不代表已经修改代码、部署或通过验收。

**主线保持不变：Res 管实例，Notebook 承载运行，SG 提供浏览器终端；Gov 创建时占额一次，普通启停不动账本，最终由 Res 决定何时通知退额。** 这沿用 v1.4 已明确的配额生命周期与服务分工。[1]

本版进一步收敛四处：**Namespace 使用可信传入值或现有规则生成的缺省值；不扩建实验环境的租户网络体系；兼容镜像由平台制定规范；实例专属工作盘随最终删除回收。**

参考创建页《华孚AI算力工厂》仅保留为交互参考，不据此推断后端实现，也不将页面全部能力纳入首期。[2]

> 2026-10-13 执行范围调整：用户要求本轮只完成后端，前端由专人接手。本文页面条目作为接口交接要求；本轮执行范围、实际结果和环境断点只见 [执行状态](../execution/status.md#gpu-container-instance-mvp-v15)。

> 2026-10-15 用户追加 CPU 产品入口：分别创建 CPU、GPU 实例，创建后类型与规格固定，不支持互转。以下 CPU 规则替代原纯 CPU 排除项；GPU 的原账配额、唯一执行快照与实机验收要求继续有效，CPU 验收不替代 GPU 验收。

### 2026-10-16 联合合同

本节是当前有效合同；下文保留历史生命周期依据，冲突处以本节为准。

本节取代下文历史方案中的单卡、配置规格、旧类型/版本及旧分页约定。完整规格由[算力目录](compute-presets.md)拥有，唯一实表为 `compute.presets`；管理、列表、引用、FieldMask与解析合同在该文维护。实例保留已受理时冻结的规格来源和资源值，不依赖当前目录继续存在。

实例内外只有 `instance_kind=container`、`compute_type=cpu|gpu`，类型来自数据库规格并冻结。删除 `instance_type`、外层 `preset_version`、空类型默认 GPU、旧 canonical 分支及 `Config.Presets`。当前 schema 缺必填类型或实际调度对象直接拒绝。profile_version 与 expected_version 保留现有语义。

Gov 首先检查原始幂等记录，然后解析规格和默认值，一次持久受理/占额后投递。Res Resolve 只解析，不创建/占额；Accept 只消费单一 context/spec，不重读当前目录。CPU 不调用 ACC；GPU 只持久化 `GpuExecution` 与简短展示引用，原占额留在 Gov。共享设备数为 1；整卡按已解析资源执行，实例副本始终 1。002/003 历史迁移不改写，004 显式切换到绝对显存及无账目 owner 合同。

规格 CPU/内存采用精确整数，分别转换为 `<cpu_millis>m` 与 `<memory_mib>Mi`，requests=limits。内存值必须可表达为合法 Kubernetes Quantity，超出 int64 字节上限拒绝。实例独立保存规格 ID/名称/编码、两维类型、CPU/内存、GPU 展示引用、唯一执行快照、镜像/环境和实际调度。修改、禁用或删除规格不影响已受理重试、静态展示、启停及清理。

GPU 运行参数只来自 ACC 的 `GpuExecution`。Resource 保存一次执行快照及 Profile 展示引用；RuntimeClass 不再由本服务配置或回退。完整释放使用 `ReportResourceReleased`，不接收、保存或回传 Gov 的额度类型、数量和账目明细。新命令使用单一 context/spec；旧附件、Plan、百分比字段和签名格式不能进入新执行链。

原始 CreateInstanceData.scheduling 可缺失；出现时拒绝空对象、非法枚举或空优先级。FrozenSpec.scheduling 必须存在且 placement 明确，按用户值 > 本次合法默认 > NONE，priority 无来源保持缺失。接收端再次校验原始明确值与实际值相等，正确摘要不能免除这项检查。原始哈希只包含原始意图；业务摘要覆盖完整冻结对象。

执行摘要只覆盖 scheduler、queue、RuntimeClass、node selector、annotations、resources 六个明确字段，使用排序 canonical JSON 与 `acc-execution-v1` 域前缀。整命令摘要覆盖身份、业务配置、执行摘要及展示；未知字段与旧附件被拒绝。Pod v2 签名另外绑定实际 UID 及执行摘要，不能用摘要代替服务认证。

`ApplySchedulingOptions` 只消费冻结 scheduling，COLOCATE/SPREAD 分别合并 weight=100、hostname 拓扑的软亲和/软反亲和，selector 固定可信 tenant+instance 并明确当前 namespace；NONE 不新增规则。节点约束、GPU Runtime 和不相关 affinity 保留，重复渲染去重；固定 priority 或相反受管关系冲突拒绝，不整体覆盖 affinity。Notebook v1.10 的 labels/annotations 从 Notebook metadata 传播，PodSpec 从 spec.template.spec 传播；最终 Pod 再核对所选规则与 priority。GPU Pod 引用现有 Volcano PodGroup 且本次选了 priority 时，只读核对同 namespace Group 的 priorityClassName，冲突回显 SCHEDULING_FIELD_CONFLICT，不重写 Group。

ComputePresetService 管理与列表使用[独立compute范围](compute-presets.md#身份用途与时效)，实例方法继续使用实例可信租户。容器 ResolveCreateSpec 内部固定 CONTAINER_INSTANCE，用同一 compute resolver 读取一次规格，再组合完整容器条件；Gov 不重复调用公共算力解析。目录与业务创建分别授权，新 GPU 使用准入不要求 Profile 目录读权限。原键仍先检查当前业务授权，再返回原冻结结果。

ListInstances 直接改为 page/page_size 与 items/total；删除 keyset 双轨。规格与实例 Count/List 在同一只读 Repeatable Read 事务、相同租户/筛选/授权 WHERE 中执行，按 created_at DESC 和唯一 ID DESC 排序。name 大小写不敏感字面子串，%/_ 不作为通配符。静态列表/详情同一安全映射，不查 Kubernetes/ACC/当前规格；返回真实 createdAt、workspaceSize、两维类型、CPU/内存、preset_name/code、gpu_summary、实际 scheduling。操作状态与清理标志沿原合同。

CPU 最小运行验收仍为 Gov 正式入口 → 真实 PG → Notebook/控制器/Pod；GPU 需读取 Whole1/Shared1/Whole2 实际 Pod 字段。Pending 仅可作为字段证据，不是运行/硬件/隔离/释放通过。软件和外部边界替身单独报告。运行详情、事件与日志通过纯GET/LIST目标解析：可信租户/实例DB记录 → 保存cluster/namespace → Notebook/STS/Pod owner UID链。诊断不调用带签名写入的Observe、不清理/占额/usage同步。runtime仅返回安全状态、来源类别、挂载类别，不返回环境值/Secret内容/宿主路径；缺对象为null、读取失败为错误。事件只取当前Pod UID的core/v1 Event，每页200、最多1000、真实truncated，时间与计数缺失保持null。日志仅固定workspace stdout/stderr，开流前后GET同UID、READY后才传bytes，替换Pod终止；鉴权、取消和保护上限见[调用登记](governance-calls.md#实例日志)。真实公开日志/SSE、长连接和Pod传播必须另行实测，软件测试不替代现场验收。

## 1. 本轮交付与明确边界

### 1.1 用户闭环

```text
创建 GPU 实例，Gov 占额一次
          ↓
Notebook 运行，通过 SG 浏览器终端使用
          ↓
文件写入 /workspace
          ↓
停止：保留工作盘，保留原配额
          ↓
启动：原镜像、原规格、原工作盘，不再次占额
          ↓
最终删除：清理计算与专属工作盘，归还原配额
```

| 项目 | 本轮范围 |
|---|---|
| 实例操作 | 创建、列表、详情、操作查询、启动、停止、重启、已有 PVC 挂卸、删除 |
| GPU | 已开放的共享规格和整卡规格；一个主容器、一个 GPU 设备 |
| 规格 | 固定 CPU／内存／GPU 配置，创建后不变配 |
| 镜像 | 平台预置镜像、已登记且遵守平台约定的租户自定义镜像 |
| 入口 | 浏览器终端，底层 Kubernetes exec；复用 SG |
| 存储 | 一个固定容量的实例专属工作盘；自定义路径及已有用户卷按[增量合同](instance-configuration-network.md) |
| 观察 | Res 持久 worker 定点轮询 |
| 配额 | Gov 首次占一次、最终归还一次；网络通知允许幂等重试 |
| 环境 | 一个受管集群、一种架构、受控测试用户 |

**共享 GPU 不能后置，也不能用“CPU 路径通过”代替产品验收。**

不做额外容器日志产品、JupyterLab／VS Code 网页、原生 SSH、任意应用端口、变配、不停机热挂载、多集群、批量和定时回收。已有 PVC 的受控挂卸按增量合同执行；服务日志、只读诊断及实例失败原因保留。

### 1.2 两条必须展示的产品提示

> **停止只停止运行，仍占用本实例原 GPU 配额。保留配额不保证原设备或立即重新调度。**

> **删除将同时删除实例专属工作目录，本功能不提供数据恢复入口。需要保留的数据请在删除前另行保存。**

“停止”和“最终删除”必须分开，不能使用同一个含糊的操作文案。

## 2. 服务分工与信任边界

| 服务／角色 | 负责 | 不新增的职责 |
|---|---|---|
| Gov | 当前用户认证授权、可信租户上下文、占额、命令投递、幂等退额 | 不重新检查 Notebook／Pod 判断业务能否退额 |
| Res／instance | 实例生命周期、Notebook 适配、工作盘、终端目标、本业务退额决定 | 不重建 IAM、配额账本或 GPU 调度器 |
| Res／image | 镜像登记、可见范围、固定 digest、拉取材料 | 不在线修理任意镜像 |
| Acc | GPU 规格、最终执行快照、供给与实际占用观察、使用关联 | 不将配额已退等同物理设备空闲 |
| SG | 终端票据、WebSocket 与 exec | 不管理实例生命周期，不承载新增 IDE 网页 |
| 平台部署／镜像维护者 | 实验环境、基础镜像规范、现有网络与存储配置 | 本轮不建设通用租户环境平台 |
| ModelDev | 本轮不修改 | 不作为 Notebook 请求转发层 |

**Res 信任经现有服务身份机制确认的 Gov 租户上下文，不重新认证用户。** 但它仍按该 tenant_id 查询自己的实例、操作和工作盘；这属于使用可信身份，不是重复建立身份权威。

退额遵循已确定的统一规则：合法 owner 决定时机，Gov 校验身份、原占额归属、数量、幂等并原子记账。即使 owner 错误地提前通知，Gov 也不查询资源后再审批；误报、漏报和实际清理由对应业务服务负责。Acc 继续保留实际观察结果，不自动清空占用。

## 3. Namespace：缺省规则，不建设映射服务

### 3.1 首次创建时解析一次

```text
Gov 可信内部上下文提供 namespace
    → 使用提供值

没有提供 namespace
    → Network 同配置前缀 + 规范化 tenant_uuid
```

本次读取的 Network 实现确实使用 `p.placement.NamespacePrefix + intent.TenantID`，然后保存并读取对应绑定。实例模块沿用这个命名方式，不另发明前缀或截短 UUID。[3]

建议集中为一个实例领域的小函数：

```text
ResolveNamespace(tenantID, trustedNamespace, namespacePrefix)
```

实际前缀取当前部署中 Network 使用的配置值，不在方案中假定一定是 `tenant-` 或 `ani-tenant-`。非空但不合法的传入值明确报错，不悄悄回退到其他 Namespace。

### 3.2 保存结果，不反复推导

首次创建解析出的 `cluster_id + namespace` 进入冻结创建快照，并随命令持久保存。Res 接受后将其写入实例记录。

后续 Notebook、PVC、拉取 Secret、启停、删除和终端全部使用保存值。**创建重放也返回原值，不受当前前缀配置变化影响。**

Namespace 不是公开创建参数；普通浏览器请求不能自行指定它。Gov 内部字段的正式接线位置在实施时确定，不声称已有该字段。

### 3.3 谁准备 Namespace

本轮沿用现有 Network／实验环境部署流程准备 Namespace 和普通工作负载 ServiceAccount。实例模块负责解析和使用，不另做租户开通业务，不读写 Network 私有映射表，也不为复用命名而导入其数据库实现。

已有环境直接使用；缺少 Namespace 或必要配置时给出明确环境错误，由部署步骤按解析后的名称准备。**不把全平台命名统一作为本轮前置。**

Namespace 可能被同租户多个实例和其他服务共用，删除实例绝不顺带删除 Namespace。

## 4. 实验环境的网络与安全范围

**本轮不新增完整多租户网络隔离能力，也不将全套跨租户网络矩阵列为实例 MVP 的交付门槛。** 直接复用实际集群已有网络与安全配置，不为让实例运行而关闭已有规则。

仍保留已有用户权限、可信服务调用、Res 数据查询的租户条件、实例／卷／终端目标归属检查。工作容器不注入控制面管理凭据，不新增任意特权、宿主机挂载或宿主机网络参数入口。

不新增动态 NetworkPolicy 管理、跨 CNI 验证、租户网络产品或新的环境控制服务。

交付说明固定为：

> **已验证受控实验环境中的 GPU 实例功能闭环；沿用现有网络配置，未完成面向不互信租户的完整网络隔离认证。**

不同 Namespace 本身不等于网络隔离；策略是否生效还取决于网络配置与插件支持。因此上述范围限制不能在生产交付中被写成“完整隔离已验证”。[4]

## 5. GPU 执行快照：正确映射，正常联调

### 5.1 执行职责

```text
Acc 生成 execution／quota_items／selection
    ↓
Gov 只将 execution 和展示随受管 spec 传给 Res
    ↓
Res 填入 Notebook 对应位置
    ↓
已有控制器、Volcano／设备插件执行
```

Res 负责正确消费唯一执行快照，不重复开发调度逻辑，也不在每次启动时重新推导 GPU 配置。

| 内容 | Res 的实现约定 |
|---|---|
| GPU limits | 使用 execution.resources 中已经解析好的运行资源值，不用业务展示显存替代 |
| scheduler／Queue | 按固定适配写入正确对象位置，不根据字段名称猜测层级 |
| 节点限制、受管标签与注解 | 准确映射，不被镜像或普通业务配置覆盖 |
| 主容器身份与 GPU 数量 | 与冻结请求一致，不增加额外 GPU 容器或副本 |
| 启停重放 | 保留原 execution、Provider 资源值、冻结单位元数据和主容器名称 |

GPU 执行的显存单位由冻结元数据解释：旧 F=1 执行不携带单位注解，`volcano.sh/vgpu-memory` 数值按 MiB；F=10 执行由 ACC 在 `execution.pod_annotations` 冻结 `accelerator.ani.io/memory-block-mib=10`，资源数值为显存块数。Res 始终原样渲染资源和注解，仅展示层使用 ACC 正式协议 helper 将冻结执行投影为绝对 MiB，不重新查询当前 Profile、基线或重编码旧执行摘要。共享和整卡均使用正整数显存块，整卡使用全部可用显存及 100 核心；不生成或接受显存百分比键。Res 不重新计算 F，不渲染 Profile。

### 5.2 验收只做应有的测试

单元测试检查字段映射和防覆盖；联调时检查实际 Notebook 与生成 Pod 的关键字段，并执行真实 GPU 程序。共享和整卡分别验证。

这不是每次创建前增加一轮人工审批，也不新建验证服务。底层版本或模板变化时回归相应接入测试；发现字段未传递就定位适配或部署兼容问题，不能用“渲染成功”代替实际支持。

已观察到的 Pod 身份关联继续按现有 Acc 合同处理，作用是租户／实例关联，不作为 Gov 退额审批条件。

## 6. 镜像规范由平台维护

本轮采用“平台规定运行约定，Res 按约定启动”的方式，不由 Res 每次创建时检查和修理镜像内部全部软件。

| 约定 | 平台发布时明确的内容 |
|---|---|
| 架构 | 当前实验环境支持的一个目标架构 |
| 长驻入口 | 镜像自带可持续运行的入口；不能只执行一次后退出 |
| 终端 | 约定的 shell 存在，SG 可以用它打开交互终端 |
| 用户与目录 | 批准的 UID/GID、工作目录与新建 PVC 写权限相容 |
| GPU 软件环境 | 在本集群驱动、设备插件和共享配置上做过对应运行验证 |
| 发布身份 | 使用固定 digest，保留本次验证的镜像版本记录 |

UID/GID、shell 和镜像 digest 在 MVP-01 根据实际基础镜像填写，不将猜测的数值写成既有规范。

平台预置镜像由维护者发布前验证；用户自定义镜像遵守同一约定，建议从基础镜像扩展。Image 模块负责登记、归属和拉取材料，不承担在线兼容认证。Res 负责受管配置及明确的失败原因，不通过放开权限或临时改装软件来掩盖不兼容。

创建后保存原 digest；再次启动或请求重放不解析到新 tag 内容。

## 7. 工作盘：停止保留，最终删除一起回收

### 7.1 生命周期

| 动作 | 专属工作盘 |
|---|---|
| 创建 | 创建一个受管 PVC，固定容量，挂载冻结的 `workspace_mount_path` |
| 停止 | 保留 |
| 再次启动 | 挂载原 PVC，保留原文件 |
| 最终删除 | 删除这个实例专属的 PVC，并由存储系统按策略回收 |

**本版替代 v1.4 的“实例删除后默认保留卷”。** 不做保留卷列表、独立卷产品、重新挂载入口或回收站。本段回收规则仅限实例自己创建、专属使用的工作盘；新增用户已有卷遵守[借用卷合同](instance-configuration-network.md#4-pvc-引用和工作目录)并在实例删除后保留。

### 7.2 回收责任与失败处理

Res 保存 PVC 名称、UID、租户和实例绑定，在计算停止并确认不再使用后删除原 PVC。只删除自己记录的目标，不清空整个 Namespace，也不删除租户共享的拉取凭证等对象。

平台选择符合回收语义的 StorageClass。PVC 删除后的实际回收受 PV reclaim policy 和存储插件影响；`Retain` 不等于自动删除底层数据，采用 `Delete` 也必须由受支持的存储插件执行。此项在所选存储上做一次真实验证，不改存储驱动。[6]

**最终删除必须完成计算、专属工作盘及其他实例占用清理后，才按 §10 持久化 GPU 退额通知。** 工作盘回收失败由实例 worker 保留原绑定继续重试，通知 Gov 不阻塞这些前置清理。

创建中的 PVC、正常工作盘和删除未完成的 PVC 都继续计入现有存储／对象限制。配额不足或创建被拒绝如实返回，不为本 MVP 新建存储账本。并发限制优先复用已有的平台或 Namespace 资源上限，不用一次无锁计数冒充强限制。

## 8. 创建、启停和删除流程

### 8.1 创建与重放顺序

```text
Gov 当前认证授权
        ↓
按 tenant + 原公开请求 + 幂等键查已受理记录
        ├── 同键同内容：返回原冻结受理结果
        ├── 同键异内容：冲突
        └── 新请求：继续
                    ↓
解析镜像、固定规格、Namespace 与环境
                    ↓
确定 instance_id、Notebook／主容器名称
                    ↓
Gov 调 Acc 得到 execution、quota_items、selection
                    ↓
Gov 一次事务占额并保存命令
                    ↓
Res 持久接受 context/spec 中唯一 execution 和展示，返回 ACK
                    ↓
Res worker 准备工作盘和拉取材料，创建 Notebook
```

**重放已受理请求不重新解析镜像、Namespace 或 GPU execution。** 镜像下架或 Harbor 暂时故障不能阻止返回原受理结果；但当前调用授权仍要执行。这不等于批准一个新的创建，也不保证原资源后续拉取永远成功。

Res 对同一命令也返回原持久 ACK，不重复建资源。已接受的后续停止／删除意图不被迟到 CREATE 重放覆盖。

### 8.2 普通停止与启动

保留一个 Notebook。Stop 使用它的原生停止机制，Start 恢复同一个 Notebook 并消费当前挂载配置；实际 Pod 可以变化，但 instance_id、原 CREATE、execution、Gov 原账和工作盘不变。

沿用 v1.4 所选的 `kubeflow-resource-stopped` 注解适配：停止添加，启动移除，不将值写为 `false` 冒充移除。该机制的既有来源是 v1.4 引用的上游版本，仍须以实验集群实际控制器验证，不假设部署版本。[1]

停止证明由 [ObserveIdentity](../../internal/data/instance/notebook_lifecycle.go) 提供：Notebook 有停止注解，StatefulSet 若存在则须 replicas=0 且 observedGeneration 覆盖 generation；持久化历史 Pod 的直接 GET 和完整 controller 链 LIST 均确认无旧 Pod，再复核 Notebook/StatefulSet 身份或确认 StatefulSet 仍不存在。terminating 对象仍算存在；卷缺失、挂载资格或 GPU 运行资格不能阻挡已证 owner 的安全停止。启动须取得当前 Pod 的运行与网络证明后才可申请终端；启停均不占退额。

### 8.3 最终删除

```text
保存最终删除意图，拒绝新 Start 和新终端
        ↓
处理本实例未确定的外部写结果
        ↓
前台删除 Notebook，检查其控制器及关联 Pod
        ↓
仍有对象：继续处理；查询失败：重试
        ↓
计算对象已清理：封闭计算创建，保存清理进度
        ↓
清理专属工作盘并核对原 PV 回收
        ↓
释放网络占用和借用卷引用，保留借用卷与原数据
        ↓
同一事务保存最终 owner 清理事实和原 GPU 退额 outbox
        ↓
独立通知 Gov；收到匹配回执后结束退额阶段与删除操作
```

最终事实提交前再次核对当前 DELETE 意图、业务版本和 lease；没有待确认的外部写，且迟到 CREATE 与 Start 已被封闭。只有完整对象观察与归属校验成功，才能确认 Notebook、其 StatefulSet 与 Pod 消失；terminating 对象仍算存在。专属 PVC 消失后仍须核对原绑定 PV 回收；借用卷只解除实例引用。网络 Release 请求须取得回执并观察到 released，不能仅凭请求已发送完成清理。

Res 的正常退额规则是上述自有生命周期检查。Gov 不重新执行清理，也不接收 PVC、Kubernetes 或 ACC 证明。Gov 不可用只使退额等待重试，不阻塞计算、存储、网络和引用清理。

本服务仅在明确最终 DELETE 且完整清理后退额。创建已受理但启动失败、一直 Pending 时仍需明确 DELETE；从未开始外部创建的取消可以使用内部未执行事实，但仍须完成已经准备的其他资源清理。普通运行阶段查不到 Pod，不能直接退款；保留实例与占额，显示异常并进入相应业务操作。CPU 实例不创建 GPU 释放事件，不调用 Gov GPU 退额 RPC。

### 8.4 Restart 与挂卸操作

`Spec`、原 CREATE 摘要、GPU execution 与原账保持冻结；当前 borrowed 挂载独立保存为 `CurrentMounts`，空数组不能回退到 `Spec.VolumeMounts`。`AppliedMountVersion` 只记录已证明应用的模板版本，初始化和原子提交规则见[配置合同 §4.3](instance-configuration-network.md#43-持久化和投影)。`Desired` 表示最终目标，`RuntimeDesired` 表示当前阶段运行目标；Network 使用后者，不能在组合操作的停止阶段继续发布旧 Running 资格。

独立 Restart 只在稳定 Running 受理。停止态挂卸应用模板后保持停止；运行态必须显式 `restart_if_running=true`，由同一 operation 完成停止、应用和恢复运行。Gov 不串联两个独立 Stop/Start。只有新意图持久受理才增加一次业务 version；phase、重放、观察和模板提交不增加。当前授权先于只读重放，重放先于当前版本、状态、维护/能力闸门和 PVC 解析；原键须携带原版本与原正文。

`accepted/blocked` 操作、Pending 或 RecoveryFence 保持互斥，普通 Start/Stop 也不能覆盖它们；DELETE 可接管未终态操作，但保留其执行和未知写证据。`completed/failed/superseded` 终态单调。失败只有在未知写已封闭、当前模板已分类并证明安全停止后才可结束并释放互斥；已提交配置不静默回滚，无安全证明则保持 blocked。新键修复使用当前版本与当前配置。[阶段执行](../../internal/biz/instance/lifecycle_worker.go)和[受理/提交事务](../../internal/data/instance/lifecycle.go)是对应实现落点；停止态结果为 `template_applied`，运行态须 `runtime_verified`，均不能单凭 ACK 判断完成。

## 9. worker 不改架构，只补必要的请求正确性

### 9.1 执行组织

Res 进程内新增实例 worker，沿用：

```text
PG 持久工作 → Step → Claim → Observe / BeginMutation → Finish
```

不修改 Network worker，不增加独立 worker 服务，不引入事件关系图。业务变化中可从 5–10 秒轮询、稳定运行从约 30 秒开始；这些只是初始参数，按小规模实测调整。

操作状态、运行状态、工作盘清理进度、最终 owner 清理事实和退额通知回执分别记录。清理由实例 worker 推进，已提交的 outbox 在同一进程生命周期内独立扫描和投递；投递不依赖新的 Kubernetes/ACC 观察或 Get/List 请求，不增加跨服务事务。单个事件失败不能阻塞其他实例清理或饿死其他待投递事件。

### 9.2 迟到启停请求：使用条件更新

Notebook 条件写使用持久 Mutation：原 operation/version、mutation ID、phase/phase_sequence、目标 UID/RV 和明确停止 payload；卷写另绑定目标摘要。先由 `BeginMutation` 复核当前意图和 lease，再调用 [LifecycleProvider](../../internal/biz/instance/lifecycle.go)；不直接 patch StatefulSet。

一次外部写的顺序是：读取 Notebook 及 resourceVersion，随后在 `BeginMutation` 复核当前数据库意图和执行资格，再提交带条件的更新。将本次业务意图版本与停止状态一起写入 Notebook。

结果未知时保持原 UID/RV/mutation/payload；Pending 与唯一 RecoveryFence 不因到期或进程退出清空，也不能刷新 RV 冒充原尝试。只有取得可归因的确定回执、匹配原写标记，或证明原条件不再可能生效，才能记录回执并继续。DELETE 继承原证据与 Pod 历史，由当前操作取得恢复推进资格，不改写旧尝试的身份。

Kubernetes resourceVersion 条件与数据库版本/lease 同时约束外部顺序；具体未知写分类与 fence 收敛见 [resolveLifecycleWrites](../../internal/biz/instance/lifecycle_worker.go)。单条注解或一次 NotFound 不能替代这组证明。[7]

### 9.3 未知创建结果：允许阻塞，不假装自动恢复

| 记录情况 | 本轮行为 |
|---|---|
| 有确定依据表明尚未发送 | 最终删除可取消发送，清理已准备的附属资源 |
| 请求已发送、结果未知 | 保存待定动作，只核对原目标；不能因一次 NotFound 宣布最终删除成功 |
| 原对象出现且归属正确 | 记录 UID，再按当前最新意图继续；当前已删除就删除它 |
| 收到可归因的明确最终拒绝 | 记录原调用结果，解除该次不确定性并继续清理 |
| 无法确定 | 显示阻塞原因，保留任务和原账，由负责人核验，不自动清空记录 |

如果进程在“记录准备发送”和“实际发送”之间退出而无法区分，应归入未知，不能当作确定未发送。Network 现有未知创建规则也保留这一边界；本版只借用原则，不复制其多资源观察体系。[8]

恢复记录至少包含实例、原操作、目标身份、待定动作及所取得的确定结果。**等待一段时间、重复多次 404、重启服务，都不能单独充当恢复依据。** 无依据时允许人工介入和持续阻塞，本轮不承诺解决全部不确定请求的自动恢复。

## 10. 完整释放及 Gov 原账退额

Res 完成 §8.3 的全部清理后，在同一 PG 事务中持久化最终 owner 清理事实和释放 outbox；事务提交后才发送 Gov 现有 `ReportResourceReleased`。Resource 和 ModelDev 使用同一个 Gov RPC，没有新增退额接口。请求只有稳定 `release_event_id`、`create_operation_id`、`resource_id`、`reason` 四字段；原 `PhysicalReleaseDigest` 是 Resource 本地遗留检查，从未传给 Gov，当前不再参与关闭资格判断，也不伪造摘要代替清理事实。

Resource 不请求 ACC `ObserveRelease`。保留外部创建前的持久开始标记、完整已见 Pod UID 历史、正常 UID/RV 条件删除、Foreground GC 及未知外部写 fencing。API absence、节点离线、超时、重启或 ACC 不可用不能单独清除未知 CREATE。Stop、Start、普通重启、Pod 退出与 ACC 来源缺项均不产生最终释放事件。

Gov 从已认证 owner 与原创建操作查原账，验证归属后幂等全退；Res 不接受或存储 charge/code/units，也不根据当前目录重算。响应必须匹配事件、原操作、资源且为 APPLIED／ALREADY_APPLIED，Res 才确认 outbox。丢回执重试相同内容；同事件异内容拒绝，同资源换事件不得重复退额。

旧未 ACK outbox 可能产生于工作盘或网络清理之前：必须先完成原实例全部清理并持久最终事实，才能投递原事件。旧已 ACK 事件保留原退额事实，只继续剩余清理；不得重复退款。`WAITING_FOR_PHYSICAL_RELEASE` 的原 DELETE 按当前规则继续，不回填虚假摘要或批量改完成标志。

Gov 独立异步同步 ACC 的薄 Usage 投影；失败重试不回滚已经完成退额。Usage ENDED 不能清除真实活绑定，物理释放与容量恢复仍由 ACC 的运行采集确认。各项实际验收状态仅见执行状态，协议和受控测试不代表真实 GPU 通过。

## 11. API、数据与代码落点

### 11.1 用户 API

| API | 用途 | 配额影响 |
|---|---|---|
| `GET /api/v1/compute-presets?usage=CONTAINER_INSTANCE` | 查询固定规格 | 无 |
| `POST /api/v1/instances` | 创建并启动 | Gov 首次占额 |
| `GET /api/v1/instances` | 租户列表 | 无 |
| `GET /api/v1/instances/{id}` | 详情、失败原因与清理进度 | 无 |
| `POST /api/v1/instances/{id}:start` | 原配置启动 | 无 |
| `POST /api/v1/instances/{id}:stop` | 停止 | 无 |
| `POST /api/v1/instances/{id}:restart` | 独立持久重启 | 无 |
| `POST /api/v1/instances/{id}:attach-pvc` | 挂载已有 borrowed PVC | 无 |
| `POST /api/v1/instances/{id}:detach-pvc` | 卸载当前 borrowed 路径 | 无 |
| `GET /api/v1/instances/{id}/mountable-pvcs` | 实例保存 placement 下的候选 | 无 |
| `GET /api/v1/instances/{id}/configuration` | 当前配置、应用版本与待提交变更 | 无 |
| `DELETE /api/v1/instances/{id}` | 删除实例和专属工作盘 | Res 按规则通知最终归还 |
| `GET /api/v1/instance-operations/{id}` | 异步操作状态 | 无 |
| `POST /api/v1/instances/{id}/exec` | 申请浏览器终端 | 无 |

正式字段以 [Instance Proto](../../api/instance/v1/instance.proto)及[配置输入](instance-configuration-network.md#3-创建输入和查询合同)为准。新增五个 Resource RPC 为 `Restart/AttachPVC/DetachPVC/ListInstanceMountablePVCs/LookupCommandReplay`，精确受信方法登记见[调用合同](governance-calls.md)及[接收方白名单](../../internal/server/instance_identity.go)。Gov 三个动作权限为 `instance:restart`、`instance:pvc:attach`、`instance:pvc:detach`，实例候选使用 attach 权限；运行态组合操作不额外要求独立 Start/Stop 权限。权限源表、增量注册及 INSTANCE 能力关联按[已审方案 §3.1/代码落点](../plans/container-instance-restart-pvc-20261028.md)核对，不以全局同步删除其他模块权限。启停、重启、挂卸及删除均使用原幂等身份和期望版本；namespace 只来自可信内部上下文。

删除接口默认删除专属盘，不另开 `retain_workspace` 分支；UI 必须明确数据删除提示。本文是在设计接口，不授权执行任何现有资源删除。

### 11.2 数据最小集合

| 记录 | 保存内容 |
|---|---|
| `instances` | 原 CREATE 与冻结规格；独立 current_mounts/mounts_initialized/applied_mount_version/runtime_desired/recovery_fence；外部身份和观察 |
| `instance_operations` | 请求指纹、幂等 ACK、命令意图版本、execution 阶段/模板证明和外部待定结果 |
| `borrowed_volume_references` / `borrowed_pvc_guards` | 挂载代际及四态引用、物理名称 guard 与 Storage 持久删除票据 |
| `instance_work` | 到期时间、租约、Owner/Epoch、失败与重试信息 |
| `workspace_bindings` | 实例专属 PVC 身份和删除进度，不提供独立保留卷生命周期 |
| `instance_quota_release_outbox` | 稳定事件、原 CREATE、resource、reason、最终 owner 清理资格与匹配 Gov ACK |

无独立 Run 表、无第二套账户余额、无 Namespace 映射新表。业务版本只随有效命令变化，不随普通观察刷新变化。最终删除后保留必要操作与审计记录，不靠物理删除数据库行表达幂等终态。

### 11.3 方法与目录

创建、命令及重放接口以 Proto 为准；[受理/阶段提交](../../internal/data/instance/lifecycle.go)、[Notebook 安全身份与条件写](../../internal/data/instance/notebook_lifecycle.go)、[Storage 删除窄端口](../../internal/biz/storage/delete_guard.go)及[008 迁移](../../migrations/instance/008_restart_pvc.sql)分别拥有其持久化与适配规则。SG 仍通过原目标解析接入。

```text
api/instance/v1/instance.proto
internal/biz/instance/        # 规则、端口、生命周期、worker
internal/data/instance/       # PG/sqlc、Notebook、Image、PVC、退额适配
internal/service/instance/    # 查询、命令、终端目标的 gRPC 适配
internal/server/instance_worker.go
cmd/ani-resource-service/instance.go
migrations/instance/
```

Namespace 函数不导入 Network 的数据库包，Image 通过公开用例协作。`biz` 不依赖 Proto、Kratos、client-go 或存储驱动，保持现有分层和手工装配；这与 Res 的工程规则一致。[9]

SG 复用原 terminal／exec；只补当前实例目标和申请／建连时检查，不另做持续会话系统。目标绑定可信 tenant_id、instance_id、业务版本、保存的 Namespace、Notebook／Pod UID 和容器；旧票据不自动转接重启后的新 Pod。

## 12. 实施顺序

基础实例工作包见 [v1.5 实施计划](../plans/instance-mvp-v1.5.md)；ACC 释放确认依赖移除、完整清理事务与独立 Gov 投递见[2026-10-27 整改方案](../plans/container-instance-release-without-acc-20261027.md)。

## 13. 最小验收清单

| 场景 | 预期结果 |
|---|---|
| Gov 提供合法 Namespace | 使用并保存传入值 |
| Gov 未提供 Namespace | 使用当前 Network 前缀＋完整租户 UUID |
| 前缀变化后重放／启停／删除 | 仍操作首次保存的 Namespace |
| 已受理创建，镜像解析服务不可用 | 当前授权通过后返回原受理结果，不再次占额 |
| GPU execution 经 Notebook 到 Pod | 关键运行字段正确，共享和整卡实际可用 |
| 多次停止启动 | 配额不变，原工作盘文件保留 |
| 旧 Start 请求最后到达 | 条件冲突或被最新意图拒绝，不让旧动作覆盖新状态 |
| 创建结果未知，此时查不到对象 | 不错误宣告完成，保留待定动作和清晰原因 |
| Res 删除检查有对象／查询失败 | 按自身规则等待或重试 |
| 最终删除 | 计算、专属 PVC/PV、网络和借用引用全部清理后，同事务固化最终事实与原 GPU outbox |
| 工作盘/PV 或网络释放阻塞 | 保留原绑定与具体原因；清理未完整前不投递 Gov 退额 |
| Gov 不可用 | 实例清理完整完成；持久 outbox 独立重试，恢复后按原账退额 |
| 退额回执丢失或投递进程重启 | 原事件恢复重试，匹配 ALREADY_APPLIED 后确认，不重复退额 |
| ACC 释放接口不可用或配置缺失 | 不调用 ObserveRelease，不妨碍完整清理后的 Gov 退额；设备容量事实独立验证 |
| CPU 实例最终删除 | 走原清理链，不创建 GPU 释放事件，不调用 GPU 退额 RPC |
| 合法 owner 提前通知 | Gov 按账本合同归还，不查询业务；Acc 保留实际观察 |
| 跨租户实例／操作／终端请求 | 现有权限与归属检查拒绝 |
| 不同 Namespace 的容器网络通信 | 本轮不据此承诺完整隔离，不扩建网络验收矩阵 |
| Namespace、共享凭据与其他实例卷 | 不被实例删除流程误删 |

**先做可用闭环，保留请求与账本的基本正确性；不把受控实验 MVP 扩大成完整生产多租户平台。**

## 14. 文档与交付方式

本版覆盖 v1.4 的冲突规则，尤其是 Namespace 解析、实验环境范围、工作盘删除和前移的纵向联调。公共退额职责、一次占退、Res／Notebook／SG 分工保持不变。

仓库落点继续使用现有规则：`docs/specs/instance-mvp.md` 放业务与接口合同，`docs/plans/instance-mvp-v1.5.md` 放工作包，进度只维护在 `docs/execution/status.md`，证据进入对应 records。旧文档标明被替代，不改写历史验收记录。

历史 v1.5 方案的成文记录不代表后续代码、部署或验收状态。执行时记录真实版本；当前增量只链接[唯一执行状态](../execution/status.md#container-restart-pvc-20261028)，本文不维护第二份结果账。

**v1.5 的交付对象是：使用缺省 Namespace 规则、平台兼容镜像、单个专属工作盘的 GPU 容器；通过 Notebook 启停、SG 终端使用，实例最终删除时回收工作盘，Gov 完成一次原配额归还。**


---

## 附录：依据与核验边界

本文依据 v1.4 附件、用户后续决定、对审核意见的讨论，以及上列必要技术资料形成。新增方法和流程均为本版实施约定。未重新审计全部仓库，未验证部署环境。Git blob SHA 是文件内容标识，不是部署 commit；`main` 链接可能变化，实施时需锁定真实提交与正式依赖版本。

### [1] v1.4 底稿

本次对话附件《01-Res-GPU-容器实例-MVP-v1.4.md》；参考其中服务职责、一次占退、Notebook 原生启停及公共合同改动。v1.5 与其冲突的范围以本文为准。

### [2] 离线创建页

用户提供《华孚AI算力工厂 (2026_10_12 10：59：46).html》；仅作交互参考，不认证其后端实现。本包不包含原网页。

### [3] Network Namespace 命名

`ani-resource-service/internal/data/network/postgres.go`，本次读取 150–185 行；Git blob SHA：`42557c269e955cdd6db94921dd5e5348a7d35ad9`。

来源：<https://github.com/zhangzhe-ctrl/ani-resource-service/blob/main/internal/data/network/postgres.go>

### [4] Kubernetes Network Policies

只用于说明 Namespace 与实际网络隔离不是同一保证；不将完整网络体系引入本轮实施。

来源：<https://kubernetes.io/docs/concepts/services-networking/network-policies/>

### [5] Acc GPU 计划与计量

`ani-accelerator-service/docs/contracts/accelerator-consumer-contract.md`，本次读取 40–57 行；Git blob SHA：`90a2dc3668bf1933f8ea8e780f617b6625dcc2e5`。仅引用本次所需的规格、单位与冻结计划约定，不将该文档所有其他条款重新引入本 MVP。

来源：<https://github.com/zhangzhe-ctrl/ani-accelerator-service/blob/main/docs/contracts/accelerator-consumer-contract.md>

### [6] Kubernetes Persistent Volumes

用于核对 PVC／PV 回收、Retain 与 Delete 策略；实际存储部署未在本次验证。

来源：<https://kubernetes.io/docs/concepts/storage/persistent-volumes/>

### [7] Kubernetes API Concepts

Updates to existing resources：resourceVersion 条件更新与冲突处理。本文应用到 Res 的顺序规则为拟实施设计，并非 Kubernetes 自动理解业务意图。

来源：<https://kubernetes.io/docs/reference/using-api/api-concepts/#updates-to-existing-resources>

### [8] Network 未知创建规则

`ani-resource-service/docs/specs/vpc-subnet.md`，本次读取 360–375 行；Git blob SHA：`46442836fe9eec1739315fe8b284310b14012e01`。参考待定请求处理原则，不复用 Network 业务模型。

来源：<https://github.com/zhangzhe-ctrl/ani-resource-service/blob/main/docs/specs/vpc-subnet.md>

### [9] Res 工程规范

`ani-resource-service/AGENTS.md`，本次读取 1–32 行；Git blob SHA：`716676aabc0e16272a514b77c74383d4febab7f7`。

来源：<https://github.com/zhangzhe-ctrl/ani-resource-service/blob/main/AGENTS.md>
