# Governance → Resource 调用登记

本登记覆盖 Network/Image/Instance/Sandbox 用户业务委托，统一规则见 [ANI 服务间调用规范](https://github.com/zhangzhe-ctrl/ani-governance/blob/8724a32ab7dda903e1c67def28ad206a06d09c44/docs/contracts/service-to-service-calls.md)。平台网络及实例使用原业务委托身份；实例 owner 退额另走既有 QuotaReleaseService 的受管 owner 身份。

Sandbox 条目定义 Resource RPC 合同和精确接收方 allowlist。Governance BFF 已接入 HTTP 客户端、路由和权限目录，并固定使用包含 `api/sandbox/v1` 的正式 Resource API module；不使用本地 `replace` 或复制生成代码。实现、部署与真实跨仓验收分别记录，实际验证范围见[执行状态](../execution/status.md)。

## 身份、范围和入口

- 调用服务为 `ani-governance`；客户端证书只有一个 DNS SAN `ani-governance`，无 URI、IP、email 或其他 DNS 身份。接收服务为 Resource，沿用登记的服务器字面 DNS SAN `ani-network-service`，不随仓库名称改名。
- TLS 1.3、私有 CA、双向证书验证；服务器证书禁止通配符 SAN。三种互斥装配模式共用同一业务 gRPC 的安全构造：默认/full、governance 和 vpc-read 都不提供明文 fallback。地址仅决定连接位置，不能决定权限。
- 服务器必需 `ANI_NETWORK_CLIENT_CA/TLS_CERT/TLS_KEY`；Gov 的 Network/Image 使用各自 `ANI_NETWORK_*` / `ANI_IMAGE_*` 受管客户端配置。缺证书、错误身份或配置失败直接报错。证书在构造时读取，续期后须重启重载；本次源码修改不代表共享环境已经换证书或部署。
- Gov 先完成 JWT/AK 认证、当前动作权限和套餐闸门，再将内部租户主键经持久映射转换为 Resource 租户 UUID。Resource 仅接受受信委托中的该租户，业务查询、关联与写入继续核验资源归属。
- metadata 只由可信值重建：单值 `x-ani-tenant-id`、`x-ani-actor`、`x-ani-request-id`。tenant 和 request ID 必须是规范、非零 UUID；actor 是 `governance:user:<uint32>` 或 `governance:access-key:<uint32>`，ID 必须规范、非零。报文中的 tenant、actor、direct caller 和 correlation 必须与委托一致。
- Network 的可选旧 `x-ani-operator` 只能是 actor 推导的 `gov-user-<id>` / `gov-ak-<id>`，不能覆盖 request ID。用户 Authorization、AK/SK 和外来身份 header 不转发。
- 下列完整方法清单在 handler 之前授权。`vpc-read` 进一步只允许 GetVPC；其他模式按已注册服务及清单授权。Attachment、ImageRuntime、health/reflection 和未登记的 stream 未获 Gov 授权，默认拒绝；唯一日志服务端流按下节独立授权。健康检查使用既有 admin `/readyz`。

## 完整 RPC 清单

所有条目都是用户业务委托：Network 和 Image 写入只操作当前可信租户所属资源；Image 的 GetImage/ListImages 另保留既有 `scope=platform` 只读目录，仍使用已授权租户上下文及原业务过滤。下表租户方法不授予平台管理、后台 owner 或其他租户访问权限。Network RPC 使用 `ANI_NETWORK_TIMEOUT`（默认 3s）；Image RPC 使用 `ANI_IMAGE_TIMEOUT`（默认 30s，最多 45s）。每次继承上游取消并设置正 deadline，部署可在原边界内调整超时。

“原键”指 API 已有的 `idempotency_key` 与原请求内容；“资源删除”沿用现有资源/删除操作状态机，不新增报文字段；查询没有业务幂等键。`x-ani-request-id` 仅用于追踪，不能替代这些业务标识。

| 完整 RPC | 幂等约束 |
|---|---|
| `/network.v1.NetworkService/GetVPC` | 查询 |
| `/network.v1.NetworkService/ListVPCs` | 查询 |
| `/network.v1.NetworkService/ListVPCCIDRPresets` | 查询 |
| `/network.v1.NetworkService/CreateVPC` | 原键 |
| `/network.v1.NetworkService/DeleteVPC` | 资源删除 |
| `/network.v1.NetworkService/GetSubnet` | 查询 |
| `/network.v1.NetworkService/ListSubnets` | 查询 |
| `/network.v1.NetworkService/CreateSubnet` | 原键 |
| `/network.v1.NetworkService/DeleteSubnet` | 资源删除 |
| `/network.v1.NetworkService/GetOperation` | 查询 |
| `/network.v1.TenantEgressService/CreateEIP` | 原键 |
| `/network.v1.TenantEgressService/GetEIP` | 查询 |
| `/network.v1.TenantEgressService/ListEIPs` | 查询 |
| `/network.v1.TenantEgressService/DeleteEIP` | 资源删除 |
| `/network.v1.TenantEgressService/BindVPCSnat` | 原键 |
| `/network.v1.TenantEgressService/GetVPCSnat` | 查询 |
| `/network.v1.TenantEgressService/GetVPCSnatBinding` | 查询 |
| `/network.v1.TenantEgressService/SetVPCSnatEnabled` | 原键及 expected_version |
| `/network.v1.TenantEgressService/DeleteVPCSnatBinding` | 资源删除 |
| `/network.v1.TenantLoadBalancerService/CreateLoadBalancer` | 原键 |
| `/network.v1.TenantLoadBalancerService/GetLoadBalancer` | 查询 |
| `/network.v1.TenantLoadBalancerService/ListLoadBalancers` | 查询 |
| `/network.v1.TenantLoadBalancerService/UpdateLoadBalancer` | 原键及 expected_version |
| `/network.v1.TenantLoadBalancerService/DeleteLoadBalancer` | 资源删除 |
| `/network.v1.TenantLoadBalancerService/GetLoadBalancerOperation` | 查询 |
| `/image.v1.TenantImageService/EnsureImageSpace` | 原键 |
| `/image.v1.TenantImageService/GetImageSpace` | 查询 |
| `/image.v1.TenantImageService/GetPublisherCredential` | 查询 |
| `/image.v1.TenantImageService/IssuePublisherCredential` | 原键 |
| `/image.v1.TenantImageService/ResetPublisherCredential` | 原键 |
| `/image.v1.TenantImageService/DisablePublisherCredential` | 原键 |
| `/image.v1.TenantImageService/RegisterImage` | 原键 |
| `/image.v1.TenantImageService/GetImage` | 查询，租户镜像或既有平台只读目录 |
| `/image.v1.TenantImageService/ListImages` | 查询，租户镜像或既有平台只读目录 |
| `/image.v1.TenantImageService/UpdateImage` | 原键 |
| `/image.v1.TenantImageService/UnregisterImage` | 原键 |
| `/compute.v1.ComputePresetService/ListPresets` | 明确usage；page/page_size、items/total；启用项与可信GPU授权交集后分页 |
| `/compute.v1.ComputePresetService/ResolveComputePreset` | 明确usage、单次一致读取；候选/确定性摘要；无受理/占额/资源副作用，默认3s |
| `/instance.v1.InstanceService/ResolveCreateSpec` | 仅新公开请求解析，首次冻结 |
| `/instance.v1.InstanceService/AcceptCreate` | 原 operation/ref/plan/charges 与持久 ACK |
| `/instance.v1.InstanceService/AcceptDelete` | 原 CREATE + delete operation，expected_version |
| `/instance.v1.InstanceService/Start` | 原键 + expected_version |
| `/instance.v1.InstanceService/Stop` | 原键 + expected_version |
| `/instance.v1.InstanceService/Restart` | 独立持久 operation；原键 + expected_version，默认能力关闭 |
| `/instance.v1.InstanceService/AttachPVC` | borrowed PVC/path/readonly + 显式 restart_if_running；原键 + expected_version |
| `/instance.v1.InstanceService/DetachPVC` | 当前 mount_path + 显式 restart_if_running；原键 + expected_version |
| `/instance.v1.InstanceService/ListInstanceMountablePVCs` | 持久实例 placement 的只读候选；删除 guard 活跃时不可选 |
| `/instance.v1.InstanceService/LookupCommandReplay` | 当前授权后只读原命令 ACK；固定 canonical 数组、无 Kubernetes 解析、无副作用 |
| `/instance.v1.InstanceService/GetInstance` | 查询 |
| `/instance.v1.InstanceService/ResolveInstanceMetricScope` | 仅持久化范围读取；历史查询不要求当前运行对象 |
| `/instance.v1.InstanceService/ListInstances` | page/page_size，默认1/20、最大100，items/total；同RR快照Count/List |
| `/instance.v1.InstanceService/GetOperation` | 查询 |
| `/instance.v1.InstanceService/GetInstanceRuntime` | 只读当前owner UID链，不调用Observe/签名 |
| `/instance.v1.InstanceService/ListInstanceEvents` | 当前Pod UID的core/v1 Events，有限读取，无持久化 |
| `/instance.v1.InstanceService/ResolveTerminalTarget` | 保存位置 + 当前业务版本与真实 UID |
| `/sandbox.v1.SandboxService/ResolveCreateSpec` | 解析冻结规格；preset 用途固定 SANDBOX |
| `/sandbox.v1.SandboxService/AcceptCreate` | operation/resource_id 与持久 ACK；实例 ID 仅作 opaque owner reference |
| `/sandbox.v1.SandboxService/GetSandbox` | 查询持久快照与 Provider 观察 |
| `/sandbox.v1.SandboxService/ListSandboxes` | tenant + `instance_id` 父资源范围分页查询 |
| `/sandbox.v1.SandboxService/GetOperation` | tenant + `instance_id` 父资源范围查询持久操作状态 |
| `/sandbox.v1.SandboxService/Pause` | `instance_id` + 原键 + expected_version |
| `/sandbox.v1.SandboxService/Resume` | `instance_id` + 原键 + expected_version |
| `/sandbox.v1.SandboxService/Delete` | `instance_id` + 原键 + expected_version；等待真实 Provider 清理 |

接收方允许清单实现为 [governance.go](../../internal/server/governance.go)、[image_identity.go](../../internal/server/image_identity.go)、[compute_identity.go](../../internal/server/compute_identity.go) 与 [instance_identity.go](../../internal/server/instance_identity.go) 与 [sandbox_identity.go](../../internal/server/sandbox_identity.go)。Instance 普通 unary 调用上限 30s，后台投递继承其更短 deadline；StreamInstanceLogs 使用独立有限生命周期，不套此总期限。修改调用时同时修改登记、允许清单和真实链路测试，不使用 RPC 前缀或通配符授权。

`/instance.terminal.v1.InstanceTerminalService/ValidateTerminalTarget` 不在 Gov 委托清单中。它由 Resource 同进程独立 mTLS listener 仅授权唯一 SG URI `spiffe://ani.internal/service/ani-session-gateway`，核对持久意图/版本、保存位置、Notebook/Pod UID、容器及当前 provider 观察。详见 [运行与恢复](../runbooks/instance-mvp-fedora.md)。

## 结果和重试

### 实例监控历史范围

`ResolveInstanceMetricScope(GetInstanceRequest)` 复用规范非零 UUID 的 `tenant_id`、`instance_id`；只读流程为请求校验、可信租户校验、现有 `Repository.Get` 和持久化位置校验。响应 `InstanceMetricScope` 的 `tenant_id`、`instance_id`、`cluster_id`、`namespace` 分别来自记录的 `TenantID`、`ID`、`Spec.ClusterID`、`Spec.Namespace`，`container_name` 固定使用领域 `Container`（`workspace`）。不接受调用者指定的集群、namespace 或 Pod，不使用进程配置填补缺失范围。

停止或正常删除后的记录，只要仍保留完整位置即可解析；不要求 Running/Ready、不调用 Kubernetes、Observe 或 runtime，也不更新持久化、配额或生命周期，不用 UpdatedAt 推算历史终点。不存在沿用 `NotFound`；delete-first tombstone、缺少或非法集群/namespace 返回 `FailedPrecondition`，并携带 `google.rpc.ErrorInfo { domain: "instance.ani.io", reason: "INSTANCE_METRIC_SCOPE_UNAVAILABLE" }`，Gov 对应 HTTP 409。跨租户和不受信调用仍按上述 mTLS、精确方法清单、单值 metadata 和请求 tenant 匹配规则拒绝。

一期完整契约由 `ani-observability-service/docs/specs/observability-phase1/CONTRACT.md` 维护，本仓不另复制任务包。历史数据是否仍在 Prometheus 保留期内由指标查询判定；位置可解析不代表必有样本。

Gov 关闭未审查的 gRPC retry/service config；调用超时不证明操作未执行。用户重放沿用原业务键，创建报文 Attribution 和 metadata 共用本次追踪 ID。接收方的持久受理、幂等、worker 和租户约束沿用既有业务实现，不在传输层伪造成功。

客户端保留原 RPC status 与领域 details，不能根据瞬时连接状态把 DeadlineExceeded 改成其他状态。Gov 对公网继续进行既有安全错误映射，隐藏内部地址、证书和私有错误详情。异步受理不代表资源已可用或删除完成，调用方通过已登记查询观察实际结果。


## 平台网络用户委托

PlatformNetworkService 的完整 27 方法由 [平台允许清单](../../internal/server/governance.go) 逐项登记，平台 Public/Intranet 池和 VLAN 原业务合同仍适用。Gov 的 HTTP DTO、角色授权和分页流程由配套仓库 `docs/contracts/platform-network.md` 维护，本批发布前不生成不存在的远端来源链接；该合同不是本仓运行依赖或部署已完成证据。

同一业务 mTLS listener 允许独立平台范围：单值 x-ani-scope=platform、governance:user:<非零规范 uint32> actor、非零规范 UUID request_id，并且 x-ani-tenant-id 必须完全不存在。Gov 先完成平台用户认证、当前动作权限与 Casbin 授权，不发送虚构租户，也不接受 API Key 平台 actor。Resource 验证证书/允许方法/互斥范围后才安装 PlatformAdministrator 和原 actor/caller/request 归因；混合或重复范围拒绝。平台委托不能调用租户/Image 方法，租户委托不能调用平台方法。vpc-read 的 GetVPC 边界保持。

创建和池配置命令沿用原业务幂等键，池配置保留 expected_version，删除仍由原持久 worker 完成。平台 Get/List 和 total 保持原集群、资源种类及筛选约束，不扩为跨租户查询。真实 KC/网卡/数据面验证独立于 Gov 受控联调。


## 公共算力与平台规格

平台规格方法为 `/compute.v1.ComputePresetService/AdminListPresets`、`AdminGetPreset`、`AdminCreatePreset`、`AdminUpdatePreset`、`AdminSetPresetEnabled`、`AdminDeletePreset`（后五项完整前缀均为 `/compute.v1.ComputePresetService/`）。逐完整方法授权，使用同一 mTLS 的平台互斥委托，不发 tenant_id；接收后注入 compute 专用 Scope。CRUD 仅写 Resource 的 compute.presets，不创建或修改 ACC Profile。Gov 入口 owner=ComputePresetService，既有SYSTEM分类，精确 compute:preset:admin 五码；目录拆分不修改用户套餐或自动补权。

公共 ListPresets/ResolveComputePreset 使用可信租户、actor与request_id，以及明确的 CONTAINER_INSTANCE、TRAINING、INFERENCE、VIRTUAL_MACHINE 或 SANDBOX；未知/UNSPECIFIED及未登记调用者用途拒绝。当前唯一直接调用服务是经证书验证的 Gov。受信Scope记录完整RPC、服务身份与用途；报文tenant须与委托一致，平台范围不能调用租户解析。容器完整ResolveCreateSpec内部适配同一范围并固定CONTAINER_INSTANCE；VM完整ResolveCreateSpec固定VIRTUAL_MACHINE（4）；Sandbox 完整 ResolveCreateSpec 固定 SANDBOX（5），三者方法/用途不可互借；compute不依赖实例Caller或业务环境。字段/单位/确定性摘要及错误边界的唯一来源为[算力目录合同](compute-presets.md)。

TRAINING供普通训练与微调共享，INFERENCE只声明公共提供方支持。本次不装配训练或推理创建消费，不修改ModelDev/ACC/Inference，不增加Gov推理路由或依赖。VM执行及冻结合同见[VM v1](../contracts/vm-v1.md)；Sandbox 一期生命周期合同见[统一设计方案](../plans/sandbox-runtime-design-20261025.md)，并固定由 Gov 的实例子资源入口承接。

## 实例日志

唯一获准流为 `/instance.v1.InstanceService/StreamInstanceLogs`：必须 server-streaming、非client-streaming，受信 Gov mTLS + 单值 tenant/actor/request_id，报文 tenant 再与委托比较。`vpc-read` 拒绝该流，其余流全部拒绝。handler 再按可信租户/实例从 DB 读取保存位置与当前 owner UID链，不接受任意 Namespace、Pod、container/path 或 previous。

日志开流前及 HTTP 开流后分别 GET 同名 Pod 确认 UID；完成后才发送 READY，之后 DATA bytes / END / ERROR。流不跟随替换Pod；观察到变化时以 POD_CHANGED 结束。普通 Kubernetes client 保留15秒整体超时，独立日志client取消整体超时但保留15秒开流等待、10分钟连接期限及逐层取消。单帧16KiB、有界256KiB、发送等待10秒，进程/租户/实例100/10/3并发上限，可经受管 Service.LogLimits 调整。最小只读权限见 [诊断RBAC](../../deployments/instance/README.md)。

静态详情/列表独立读取数据库快照，集群故障只影响诊断读取；诊断不 Patch/Update、不签名、不调用占额/usage或清理。runtime/events 的安全字段、null与事件截断语义见 [实例合同](instance-mvp.md)。Kubernetes GET/LIST单元测试及HTTP pods/log适配测试与真实集群/公开SSE验收分开记录。
