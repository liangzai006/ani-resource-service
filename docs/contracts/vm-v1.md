# VM v1 正式合同

日期：2026-10-23。新增设计，待实现。业务规则以[规格](../specs/vm-mvp.md)为准；本文件是名称、字段和跨模块接线的唯一来源。业务方法从现有约定做最小增量，不把本文件解释为要求一次实现所有功能。

## C01 包、服务与权限

| 项目 | 冻结值 |
|---|---|
| Resource owner | ani-resource-service |
| 业务 Proto | api/vm/v1/vm.proto；package vm.v1；service VMService；在 Resource 根 Go module |
| biz/data/service | internal/biz/vm、internal/data/vm、internal/service/vm |
| 装配 | cmd/ani-resource-service/vm.go，并接入现有两种主装配路径 |
| VNC 窄模块 | api/vm/console/go.mod；module github.com/zhangzhe-ctrl/ani-resource-service/api/vm/console |
| VNC Proto | api/vm/console/v1/console.proto；package vm.console.v1；VMConsoleService |
| compute usage | VIRTUAL_MACHINE=4 |
| 套餐功能 module | identity.service.v1.Module.VIRTUAL_MACHINE=17；存储字符串 VIRTUAL_MACHINE |
| VM canonical 版本 | vm_contract_version=1 |

17 在已审 Gov 基线未占用；实施前若新基线已占用，先记录合同冲突，不静默重号。同步 Module、PlanModule、Api.business_module、权限登记及前端模块映射；这是套餐功能准入，不增加资源配额。

权限固定：`vm:list`、`vm:read`、`vm:create`、`vm:delete`、`vm:power`、`vm:force-stop`、`vm:console`、`vm:disk:write`、`vm:media:write`、`vm:snapshot:read`、`vm:snapshot:create`、`vm:snapshot:delete`、`vm:snapshot:restore`、`vm:compute-preset:read`。列表/详情只读介质目录采用 vm:read；创建权限不自动替代目录/子网读取的既有授权。权限注册沿 Gov 已有机制。

VM 业务 API 沿现有 Instance/compute 的根 module 模式；只把 SG 必要纯协议拆成 console 子模块，其依赖限 grpc/protobuf 及必要间接依赖。Gov/SG 不通过本地 replace 作为正式交付。新增方法同时接入受信 listener、精确 method allowlist、caller 注入和 tenant 比对；禁用/未装配 VM 时拒绝新 VM 受理，不影响原实例。

## C02 Action 受理、版本与无 charge 合同

| owner | 正式 action | usage | codec |
|---|---|---|---|
| ani-resource-service | /instance.v1.InstanceService/AcceptCreate | CONTAINER_INSTANCE | 原 Instance create |
| ani-resource-service | /instance.v1.InstanceService/AcceptDelete | CONTAINER_INSTANCE | 原 Instance delete |
| ani-resource-service | /vm.v1.VMService/AcceptCreate | VIRTUAL_MACHINE | VM v1 create |
| ani-resource-service | /vm.v1.VMService/AcceptDelete | VIRTUAL_MACHINE | VM v1 delete |

commandcontract 维护一处编译期 action 定义，按(owner, action)选 resource kind、phase、usage、schema/版本与处理函数。Resource 仅注册一个聚合 adapter，容器与 VM 按同表分派；未知 owner/action、错 schema/usage 拒绝。保留原容器 action、已支持的原 canonical bytes、Inference 注册及 registry 的一 owner 一 adapter 约束，不重新开放已经停用的旧合同。

必改调用点：Usage、ValidateStoredCommand、DeleteSnapshot、validateDispatchCommand；GpuAcceptance/managed admission 共用流程中的 owner→Instance 断言、hash/usage 和 GPU action 能力选择也改为 action 定义。容器 DTO/codec 仍可独立存在，不在共用层留默认容器分支。[R02]

VM canonical 顶层固定为 `vm_contract_version`、`create_spec`，delete 只在原创建 canonical 上加入 `delete_expected_version`；未列出的删除控制字段拒绝。create_spec 不再另存第二个合同版本。canonical JSON/摘要复用现有规范化实现；vm_create_digest 不包含它自身或观察时间，request_digest 基于规范化用户意图而非重解析后的目录。重放不得覆盖首次冻结结果。

既有账本列 InstanceContractVersion 保留：VM 复用的持久受理 DTO 中该旧字段传0，由现有数据代码落为默认兼容值1；VM delete 继承该占位。VM codec 不读取该列判断 VM 合同，只验 canonical.vm_contract_version。Instance codec 仍按其现有字段及当前允许版本严格校验；不得把未来 VM 版本数字复制到旧列，不重命名全库。

合法 VM create/delete：items=[]、charges=[]，仍持久保存 Gov operation/投递。本期新增的空 charge 支持仅限已注册且合法的 VM 合同；现有 Instance CPU 空 charge 与容器 GPU 占退额合同保持，未知 owner/action 或不合法 canonical 不放行。VM 删除不调用空账项 QuotaRelease。

创建幂等作用域沿既有 Gov 的 tenant/actor/action/key，摘要包含完整用户意图。普通 Resource 变更键至少绑定 tenant/vm/action/key；同键先比对首次请求摘要，原请求重放返回同一 operation，不因数据库当前 version 已变化而失败；请求中的 expected_version 或其他意图字段变化属于异意图冲突，新键必须校验当前 version。durable ACK 只保持 operation_id、resource_id、accepted=true 的原校验语义，不把业务详情塞入严格 ACK JSON。

## C03 公开入口与 Resource RPC

服务间仅 gRPC；下表 HTTP 是 Gov 已有公开 BFF 的新增路由，不给 Resource 新开业务 REST 服务。tenant 从可信身份取得，前端不得提供 owner/action、namespace、原生名称/UID、冻结规格或 charges。

| Gov 公开入口 | VMService RPC | 主要权限 |
|---|---|---|
| GET /api/v1/virtual-machines | ListVMs | vm:list |
| POST /api/v1/virtual-machines | ResolveCreateSpec → 既有投递 AcceptCreate | vm:create |
| GET /api/v1/virtual-machines/{vm_id} | GetVM | vm:read |
| DELETE /api/v1/virtual-machines/{vm_id} | 既有投递 AcceptDelete | vm:delete |
| POST .../{vm_id}/start | Start | vm:power |
| POST .../{vm_id}/stop | Stop | vm:power |
| POST .../{vm_id}/force-stop | ForceStop | vm:force-stop |
| POST .../{vm_id}/restart | Restart | vm:power |
| GET .../{vm_id}/disks | ListDisks | vm:read |
| POST .../{vm_id}/disks | CreateDisk | vm:disk:write |
| POST .../{vm_id}/disks/{disk_id}/attach | AttachDisk | vm:disk:write |
| POST .../{vm_id}/disks/{disk_id}/detach | DetachDisk | vm:disk:write |
| DELETE .../{vm_id}/disks/{disk_id} | DeleteDisk | vm:disk:write |
| PUT .../{vm_id}/media/{slot} | SetMedia | vm:media:write |
| DELETE .../{vm_id}/media/{slot} | EjectMedia | vm:media:write |
| PUT .../{vm_id}/boot | SetBootDevice | vm:media:write |
| GET /api/v1/virtual-machine-media | ListMedia | vm:read |
| GET .../{vm_id}/snapshots | ListSnapshots | vm:snapshot:read |
| GET .../{vm_id}/snapshots/{snapshot_id} | GetSnapshot | vm:snapshot:read |
| POST .../{vm_id}/snapshots | CreateSnapshot | vm:snapshot:create |
| DELETE .../{vm_id}/snapshots/{snapshot_id} | DeleteSnapshot | vm:snapshot:delete |
| POST .../{vm_id}/snapshots/{snapshot_id}/restore-preview | PreviewRestore | vm:snapshot:restore |
| POST .../{vm_id}/snapshots/{snapshot_id}/restore | RestoreSnapshot | vm:snapshot:restore |
| POST .../{vm_id}/console-sessions | ResolveConsoleTarget → SG CreateSession | vm:console |
| GET .../{vm_id}/operations/{operation_id} | GetOperation（先查 Gov 受理状态） | vm:read |
| GET /api/v1/compute-presets?usage=VIRTUAL_MACHINE | 既有 compute 目录 RPC | vm:compute-preset:read |

读取与预览无副作用；创建后的所有公开业务变更使用 idempotency_key 和 expected_version。创建仅用 idempotency_key；删除亦需原创建关联，由 Gov/Resource 可信记录取得。控制台会话复用 SG 自己的短时幂等，不写 VM 变更 operation。ForceStop、运行中 DetachDisk、RestoreSnapshot 需明确产品确认；确认不能替代服务端授权/版本检查。响应复用既有 202/错误封装；权限失败、范围不存在、版本/幂等冲突按当前 Gov 合同映射。

### C03.1 用户输入字段

| 消息 | 冻结字段与校验 |
|---|---|
| CreateVMData | name；preset_id；source_kind=DISK_IMAGE或INSTALL_ISO；media_key；system_disk_size_gb；data_disks[]；network={vpc_id,subnet_id}；start_after_create（必填布尔）；可选 guest_initialization（由 Gov 规范化，见下）；可选 dns={mode,nameservers[]} |
| InitialDataDisk | client_disk_key（同一创建内唯一）；name；size_gb；全部为 blank，初始挂载；不接受任意 PVC |
| ChangeVMRequest | tenant_id（仅受信gRPC）；vm_id；idempotency_key；expected_version |
| ForceStopRequest | ChangeVMRequest字段；destructive_confirmation=true，缺失/false拒绝；确认纳入意图摘要；原生StopOptions.GracePeriod=0，不开放任意宽限时间 |
| CreateDiskRequest | ChangeVMRequest字段；name；size_gb；创建 blank 自有盘，初始未挂载 |
| DiskChangeRequest | ChangeVMRequest字段；disk_id；运行中Detach另须guest_prepared_confirmation=true，停止态不要求；确认纳入意图摘要 |
| SetMediaRequest | ChangeVMRequest字段；slot=INSTALL或DRIVER；media_key（与该VM冻结OS设备合同兼容）；本次来源与副本身份按S05在首次受理冻结 |
| SetBootDeviceRequest | ChangeVMRequest字段；boot_device=INSTALL_MEDIA或SYSTEM_DISK |
| SnapshotCreateRequest | ChangeVMRequest字段；name；在线/离线由当前实际运行态决定，不自动关机 |
| PreviewRestoreRequest | tenant_id；vm_id；snapshot_id；只读，无幂等键 |
| RestoreSnapshotRequest | ChangeVMRequest字段；snapshot_id；preview_digest；destructive_confirmation=true |
| CreateConsoleSessionRequest（Gov公开DTO） | vm_id；idempotency_key；expected_version；复用SG短时会话幂等，不创建VM业务operation，也不递增VM version |
| ResolveConsoleTargetRequest | tenant_id；vm_id；expected_version；来自Gov已授权请求 |
| GetOperationRequest | tenant_id；vm_id；operation_id（须属于该VM） |

size_gb 均为正整数十进制 GB；公开输入不接受 volume_mode/StorageClass/磁盘总线/原始 YAML。平台 root/data 固定 Block/RWO，介质与设备按受管目录。data_disks 保留数组，不以“一块系统盘”为产品上限。合理请求大小和分页限额沿现有接口治理，不新增租户磁盘配额。具体 Proto tag 在 VM-01A 首次生成时按该消息字段顺序冻结，不改任何旧消息 tag。

DNSSelection 的 mode 为 PLATFORM_DEFAULT 或 CUSTOM；省略、空 mode 或显式 PLATFORM_DEFAULT 在规范化意图中等价为默认选择，不可夹带 nameservers。CUSTOM 接受 1–3 个 IP 地址，规范化 IPv4/IPv6 并保序去重。新增 Resource GetDNSDefaults(tenant_id) 只读返回平台 nameservers；Gov 以当前 tenant、vm:create 和 VIRTUAL_MACHINE 套餐授权公开 GET /api/v1/virtual-machine-dns-defaults。CreateVMData.dns 追加 tag 10，CreateSpec.dns 追加 tag 15，FrozenDNS 固定 mode 和实际 nameservers；其余旧 tag 与无 DNS 的历史摘要不变。新建时实际 DNS 必须完整冻结，平台默认变化不修改既有冻结规格。GetVM 返回该冻结值，不提供任意原生 YAML 或可信网络身份输入。

公开创建的可选 `guestInitialization={username,password?,sshPublicKey?}` 仅用于[S05.1](../specs/vm-mvp.md#s051-linux-镜像的可选账户初始化)。启用时 username 必填，密码或公钥至少一项；用户名匹配 `^[a-z_][a-z0-9_-]{0,31}$` 且不能为 root；密码为 8–128 UTF-8 bytes，不含 NUL 或换行。公钥最多 16384 bytes，须为单行、无 options 的 OpenSSH 公钥；拒绝私钥，不由平台生成 keypair。默认不启用 SSH 密码登录，密码用于控制台登录。

Gov 生成可信 `GuestInitialization={method=CLOUD_INIT_NOCLOUD,username,ssh_public_key,password_set,credential_intent_digest}`，写入 `CreateVMData.guest_initialization`；`password_set` 和 `credential_intent_digest` 由 Gov 生成，后者为绑定完整凭据意图的 Gov HMAC，不接受浏览器伪造。明文密码不在 create_spec 内；仅经 mTLS 的 `AcceptCreate.guest_credentials` 单独传递。Gov outbox 使用 AEAD 保护凭据；Resource 在受理 ACK 的同一事务内写入独立 AEAD 私密表。普通 spec、查询、operation、日志及公开证据均不返回密码或密文。

`MediaReference.initialization_capability` 与 `initialization_verified` 为受管目录的只读能力。仅 capability=CLOUD_INIT_NOCLOUD、verified=true，且已验证 source UID/hash 的 Linux DISK_IMAGE 可接受初始化；其余介质明确拒绝。创建冻结这组能力及规范化初始化意图，合法同意图重放不二次受理或重置账户。原生专属 Secret 绑定真实 VM 所有权，最终按精确 UID 清理。固定 firmware UUID=业务 VM ID，保持 Stop/Start/Restart 和同一 VM 恢复的 NoCloud instance-id；客体实测须证明停开不重置自定义账户，恢复后保留快照内账户状态，不重新用初始凭据覆盖。

### C03.2 可信创建与操作消息

ResolveCreateSpec 输入：tenant_id、CreateVMData、可信 namespace/namespace_uid、当前协议版本；返回完整 create_spec、vm_contract_version=1、selection_digest、vm_create_digest。cluster 从可信目标配置/compute解析取得并校验，不接受浏览器任意选择。此时只校验 Network 选择，不 Prepare。

create_spec 至少包含：原意图（含可选规范化 guest_initialization，不含明文密码或密文）、冻结 compute 结果、CPU/内存/拓扑、cluster/namespace身份、服务账号、固定 root/data StorageClass与mode/accessMode、OS/固件/总线/TPM要求、完整源引用/校验/容量与初始化能力、network业务选择及已核验受管NAD身份、默认介质/启动顺序。VM Prepare 时重核这些冻结网络身份，不能接到后来变化的 NAD。

AcceptCreate：CommandContext={tenant_id,resource_id,operation_id,request_digest}、vm_contract_version、create_spec、vm_create_digest。原子持久写入后 ACK。
AcceptDelete：同 CommandContext、create_operation_id、expected_version、vm_contract_version。canonical delete 验证保留原冻结创建内容；实际清理使用当前资源清单，不按初始盘清单删。
普通变更由 Resource 生成/保存 operation_id，Gov 返回相同 resource_id/operation_id；不得每次网络重试生成新的业务操作。

GetVM/ListVMs：id/name/version、OS、冻结规格摘要、desired_state/observed_state/reason/observed_at、当前网络地址、last_operation_id、closing。
GetVM 另含当前盘/介质/boot/device配置摘要；原生UID不作为浏览器可写参数。
Operation：id/vm_id/action/intent_version/state/reason/observed_at；state沿域内既有 accepted/running/succeeded/failed/unknown 等映射，不新增产品运行代次。
Disk：disk_id/role/name/size_gb/requested_bytes/observed_capacity_bytes/volume_name/serial、attached、preparation_state、reason；挂载成功与guest格式化分开。
Snapshot：snapshot_id/source_vm_id/source_vm_uid、ready/completeness/included/excluded、guest_agent/quiesce indications、disk inventory、observed_at/reason。
Media 操作结果：desired_media、observed_media、effective_now、pending_restart。若仅持久模板可更新，配置operation在模板与逻辑关系核对后可结束，同时返回effective_now=false、pending_restart=true；不能标记实际已弹出，也不能占住变更互斥等待后续人工Start。下一次启动使用已保存当前配置。

业务 error reason 固定最小集合：VM_SCOPE_DENIED、VM_NOT_FOUND、VM_INVALID_REQUEST、VM_IDEMPOTENCY_CONFLICT、VM_VERSION_CONFLICT、VM_OPERATION_CONFLICT、VM_DEPENDENCY_NOT_READY、VM_DEPENDENCY_CHANGED、VM_OBJECT_OWNERSHIP_CONFLICT、VM_EXTERNAL_RESULT_UNKNOWN、VM_SNAPSHOT_INCOMPLETE、VM_RESTORE_PREVIEW_CHANGED、VM_CONSOLE_TARGET_CHANGED。沿现有 Kratos/gRPC错误封装；原生失败原因另保留受控摘要，不暴露票据/凭据。

### C03.3 Gov 删除持久受理栅栏

Gov 在每次当前主体、租户、动作授权后，沿现有容器 canOperate 机制，检查该 VM 原 CREATE 关联的已持久受理 DELETE。按正式 VM action、resource kind、tenant/resource/create_operation_id 精确识别，不能把同一 Resource owner 下的容器命令视为 VM。

删除已被 Gov 受理、尚未投递到 Resource 时也必须拒绝新的 Start/Stop/ForceStop/Restart、盘、介质、启动顺序、快照创建/删除/恢复及 console 会话意图，返回既有冲突封装和 VM_OPERATION_CONFLICT；只读列表、详情、快照读取/恢复预览及原 operation 查询沿各自当前授权规则继续。该栅栏不新增全局锁或另一个工作流。

幂等查找仍限定当前 tenant/actor/action/key（Resource 直达变更至少 tenant/vm/action/key）并比对首次请求摘要。DELETE 之前已持久受理的原请求同意图重放返回原 operation；已签发 console 的同键重放只在 SG 既有允许重放状态及原 binding 下按 C06 处理，不能重新签发、重开流或绑定新 VMI。不能把旧操作换绑成新操作，也不能以新 key 绕过删除。原 DELETE 同键重放复用原持久命令及 ID。

对检查栅栏前已在途的直达请求，由 Resource 原子 version/closing 与操作互斥处理：先受理者的意图和结果保持可查；DELETE 不得覆盖 expected_version，也不得把排队中或冲突命令标完成。投递时版本已冲突，沿持久投递规则明确记录冲突/失败并让原 operation 可查询；如需使用新版本重新删除，必须是当前授权、用户新确认的新意图，不能静默改写旧 DELETE。原生结果未知先沿原身份查证，再按现有关闭/清理规则继续。

必须验证：暂停删除投递→Gov 返回删除已持久受理→新的直达变更和控制台会话均拒绝；原查询和合法幂等重放仍指向同一操作/绑定；恢复投递后原 DELETE 继续且不增加 VM/盘/原生副作用；并覆盖在途请求导致 expected_version 冲突时的可观察结果。验收归 V05，不把排队删除长期悬挂当作成功。

### C03.4 已受理删除期间的原变更查询

VMService 新增受信只读 LookupCommandReplay(LookupCommandReplayRequest) returns Operation；request 字段/tag 固定：tenant_id=1、vm_id=2、action=3、idempotency_key=4、bytes canonical_intent=5。

该方法只在精确 Gov 受信 listener/method allowlist 上提供；Gov 每次先做当前主体/租户/动作授权，Resource 校验可信 tenant。按 tenant/vm/action/key 查询并比对规范化的原用户意图，canonical_intent 必须包含 expected_version 及该动作全部用户字段。同键同意图返回原 Operation，不读取当前 VM version 作新受理准入；同键异意图返回幂等冲突，不存在返回 NotFound。查询不改变数据库、业务版本、operation 或原生对象，不重新执行变更。

Gov 删除栅栏生效时只允许通过该方法恢复先前变更的合法幂等结果；NotFound 则拒绝新意图。删除前已受理操作保持原 ID 和绑定，不能用查询把新意图伪装成重放。按 V05 覆盖原 operation、异意图、未命中、当前版本变化及无副作用断言。

## C04 VM 内部窄端口与持久化

biz 不导入 protobuf、Kubernetes、Kratos 或数据库驱动。最小端口与方法：

- ComputeResolver.ResolveSelection：复用 compute 一次解析及用途校验。
- MediaCatalog.Get/List：只读受管配置，返回冻结来源与设备规则。
- NetworkPort.ValidateSelection/Prepare/Observe/Confirm/Release：C05规定。
- NativeVMPort.CreateVM/GetVM/Start/Stop/Restart/DeleteVM、EnsureDataVolume/GetVolume/DeleteOwnedVolume、ApplyDiskConfiguration/ApplyMediaConfiguration、Create/Get/DeleteSnapshot、Create/GetRestore：返回领域引用/观察，不向biz透传unstructured。
- Repository.AcceptCreate/AcceptDelete/AcceptCommand/Get/List/GetOperation、已有模式的工作领取/完成：一个事务持久输入、版本、幂等及待执行意图；未知结果保存pending mutation后查证。
- ConsoleResolver.Resolve/Validate：只读当前身份与关闭意图，C06规定。

不把上述端口拆成新的微服务或万能Provider。每个原生mutation前持久目标kind/namespace/name/已知UID/operation；有UID的删改使用precondition，首次create确定名称、冲突时核对归属和意图，不盲目接管同名对象。

最小表和字段见S11；新迁移只追加，不能改已发布迁移字节。真PG覆盖tenant复合约束、幂等、并发version、受理后进程退出恢复；不要只依赖内存替身。

## C05 Network v3 合同

vm_managed 使用 consumer protocol_version=3；single_pod/空模式v1、controller_managed Notebook v2保持当前格式与行为，不全量迁移。现有InstanceID/instance_id字段号暂留，VM分支值为平台VM ID，必须走VMReader，不能误查Instance表。

新 VMPrimaryPlan format_version=1，字段：namespace、nad_name、nad_uid、nad_config_digest、kcn_subnet_ref、受信labels。Attachment既有binding_revision继续固定KCN Subnet UID。NAD spec.config摘要采用与冻结配置一致的规范化JSON摘要；不是随意读取任意NAD后认可为受管。

原生模板投影固定为：VM与spec.template.metadata写入该计划的受信labels；spec.template.metadata.annotations的`networking.kubercloud.com/subnet`取kcn_subnet_ref；spec.template.spec.networks仅一项name=default、multus.default=true、multus.networkName=namespace+'/'+nad_name；对应domain.devices.interfaces使用name=default、bridge={}及冻结OS网卡型号。NAD引用与该KCN注解值分别来源于各自字段，不互换；VMI/launcher传播结果须在真实环境核验。不得由前端提供这些原生标签或注解。

PrepareAttachmentRequest在既有1–11号字段后追加VMNetworkExpectation vm_expectation=12，字段为nad_name、nad_uid、nad_config_digest；仅vm_managed必填，来源为已冻结create_spec，不能由公开JSON直接提交。首次Prepare严格核对受管NAD与该期望一致再持久受理；同键重放先读原记录并按原冻结计划返回，执行/Start仍检查依赖漂移。旧模式该字段为空，旧canonical/指纹编码保持原样（不因新增零值字段改变摘要）。

在已审Network Attachment的现有25号字段之后追加 `VMPrimaryPlan vm_primary=26`、`repeated VMCurrentAddress vm_current_addresses=27`。旧pod_primary=15在VM模式为空，旧模式继续原样。实施基线若这些新tag已被占用，VM-00记录协议冲突再统一修订，不静默重用。
VMCurrentAddress字段：nic_name、address、launcher_pod_uid、vmi_uid；父Attachment已有observed_at。只返回当前核验有效的地址，Stopped/过期/冲突返回空及具体reason。

新consumer沿用现有submission/generation/attachment/snapshot_revision/finalization结构：根VM UID登记后固定；VMI/launcher可更换并单调登记，current launcher最多一个，retired UID沿现有规则保留。原VMRestore根UID不变，不把旧snapshot中的Pod/IP回填。

新增 VMNetworkConsumer 路由；两个主装配都接入，VM adapter不可用时拒绝新VM。沿既有Network worker处理Prepare/Confirm/Release，不复制worker。不放宽Notebook owner链，不任意接受GVK。核对原生NAD、VMI binding、launcher主网注解和KCN VNic/VNicIP/Subnet归属；label不能替代owner/UID。

关闭顺序固定为VM原生根/运行子对象退出→consumer closed→请求Network Release→Network自行证明网络依赖退出并released；整VM删除还等待S10的盘/快照清理。不得让consumer closed等待Network released，否则会构成相互等待。

修改落点：biz/network/attachment.go、attachment_managed.go、attachment_worker.go；data/network/attachment*.go、kc_attachment*.go、kc_observed_provider.go；service/network/attachment.go；api/network/v1/network.proto；新增biz/vm/network.go、data/vm/network*.go和composition。具体代码参见[R03](../references/vm-mvp-sources.md)。

## C06 VNC 窄协议与接线

console module禁止依赖Resource根module、Kubernetes、biz/data。以下字段/tag为新协议冻结值：

```proto
syntax = "proto3";
package vm.console.v1;
option go_package = "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/console/v1;consolev1";

service VMConsoleService {
  rpc ValidateConsoleSession(VMConsoleBinding) returns (ConsoleSessionValidation);
}
message VMConsoleBinding {
  string tenant_id = 1;
  string vm_id = 2;
  int64 version = 3;
  string cluster_id = 4;
  string namespace = 5;
  string vm_name = 6;
  string vm_uid = 7;
  string vmi_name = 8;
  string vmi_uid = 9;
  string namespace_uid = 10;
}
message ConsoleSessionValidation { bool valid = 1; }
```

全部绑定身份字段必填；version为VM业务版本，非Kubernetes resourceVersion。业务VMService.ResolveConsoleTarget返回该绑定。
SG Target保留1–4，新5号字段为vm_binding；instance_binding与vm_binding不能同时存在。vm_binding只能用于WorkloadVM+VNC，且Target.instance_id=vm_id、Target.workload_name=vm_name、Principal.tenant_id=tenant_id，namespace使用冻结值，cluster匹配SG受管集群。

SG CreateSession复用精确Gov mTLS身份校验并调用VMConsoleService；session fingerprint与持久store纳入全部绑定字段。SG→Res复用既有SG专用mTLS listener，新增精确方法 `/vm.console.v1.VMConsoleService/ValidateConsoleSession`，保留原InstanceTerminal校验方法。SG主体仍为 `spiffe://ani.internal/service/ani-session-gateway`；不开放任意调用者。

Gov 的 CreateConsoleSessionRequest.expected_version 传给 ResolveConsoleTarget，idempotency_key 传给 SG CreateSession；稳定请求意图绑定该次解析出的完整目标，SG 同键异目标沿既有冲突规则拒绝。目标改变后前端重新发起新的会话意图/键，不把旧票据自动换绑到新 VMI。

签发、连接前均校验：DB归属/version/关闭意图；namespace UID；VM UID；当前VMI UID及owner；原生运行条件。开原生VNC流后、转发浏览器数据前再核对VMI UID与业务关闭意图；变化则关流报目标变化，不连接到新VMI。校验先观察后复读业务行以发现并发Stop/Delete。

SG已有VNC字节转发可复用，但当前name-only路径不能作为本期受管VM回退。VNC准入与guestIP、GuestAgent、OS安装及整个create operation完成解耦。旧连接由原VMI流结束关闭，未用旧票据在业务version或VMI变化后失效；不新增session_epoch或全局撤销服务。


### C06.1 已受理删除期间的 console 原会话查询

为 C03.3 删除受理栅栏保留已有短时幂等，SG SessionService 新增纯查询 GetSessionReplay(GetSessionReplayRequest) returns GetSessionReplayResponse；request 字段/tag 固定：idempotency_key=1、Principal principal=2、instance_id（VM ID）=3、expected_version=4。GetSessionReplayResponse 字段/tag 为 session_id=1、connect_url=2、google.protobuf.Timestamp expires_at=3、bool replayed=4，沿原会话响应语义。

Gov 当前动作授权后，若删除已持久受理，仅调用该只读查询取得同主体/租户、同 VM/version/key 的原 managed VM/VNC 会话；SG 仍校验精确 Gov mTLS。仅原 issued 且未过期、未 claim 的会话可返回同票据/URL，不调用 ResolveConsoleTarget、ValidateConsoleSession、Issue/CreateOrGet，不创建或刷新票据、不改绑定。VM/version 等意图变化按既有幂等冲突拒绝；不存在时返回 NotFound，由 Gov 删除栅栏拒绝新会话；claimed/expired/closed 按既有 FailedPrecondition 拒绝。原票据后续连接仍执行 C06 的完整目标/closing 校验，查询成功不授予绕过删除的开流能力。
