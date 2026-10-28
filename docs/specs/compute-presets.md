# 算力目录与公共解析合同

算力规格由 Resource 的 compute 模块拥有：定义、校验、CRUD、启停、列表、持久化与只读解析均归此模块。实例通过依赖 compute 组合完整容器配置。实现、正式 API 发布、环境切换与业务验收的实际状态只见[执行状态](../execution/status.md#compute-preset-resolution)。

唯一实表为 `compute.presets`，无租户私有副本或 Gov Preset 表。CPU 使用正整数 `cpu_millis` 和 `memory_mib`；内存不能超过 int64 字节范围。GPU 还包含 ACC 精确 Profile ID、版本、digest、cluster、pool、模式、型号、单设备显存、核心限制及设备数。规格没有递增版本；GPU Profile 版本有独立语义。规格修改、禁用或删除不改写已受理实例。

## 管理、目录与容器边界

`api/compute/v1` 的 `compute.v1.ComputePresetService` 拥有六个管理 RPC：`AdminListPresets`、`AdminGetPreset`、`AdminCreatePreset`、`AdminUpdatePreset`、`AdminSetPresetEnabled`、`AdminDeletePreset`。管理要求可信平台用户委托，不使用虚构租户。Gov 的现有 `/api/v1/admin/compute-presets…` 路径由独立 ComputePresetService 接管；SYSTEM 仅为平台管理分类，五个精确权限是 `compute:preset:admin:list/read/create/update/delete`，set-enabled 使用 update。

管理对象的 code 创建后不可修改；创建默认禁用。更新要求非空 FieldMask，在锁行事务中合并并校验最终对象；`gpu` 整体替换或显式清空。删除不级联实例或 ACC。请求的 `data`、分页、201/200/204 和字段语义由 Gov 的前端合同维护。

`ListPresets` 面向可信租户范围，显式传入 usage。只返回 enabled 规格；GPU 按受信 `allowed_gpu_profiles` 的 profile_id/version/spec_digest/cluster_id/pool_id 精确交集过滤，再执行 count/page。空允许集合严格排除 GPU。CPU 不依赖 ACC；`has_gpu_candidates` 仅供 Gov 安全处理目录依赖故障。列表/计数使用同一只读 Repeatable Read 快照，按 created_at DESC、id DESC 排序，分页默认 1/20、最多100；name 为大小写不敏感字面子串。

Gov 唯一租户目录为 `GET /api/v1/compute-presets`，usage 必填。CONTAINER_INSTANCE 要求 `instance:preset:read`/INSTANCE；TRAINING（含微调）要求 `modeldev:compute-preset:read`/MODEL；INFERENCE 要求 `inference:compute-preset:read`/INFERENCE；VIRTUAL_MACHINE 要求 `vm:compute-preset:read`/VIRTUAL_MACHINE（module=17）；SANDBOX 由 Sandbox 生命周期合同固定使用，不把它当作普通容器目录。旧 `/api/v1/instance-presets` 删除，无兼容入口。各用途分别检查当前业务授权，不因共享 URL 互相放行。创建仍只调用一次活跃的 `instance.v1.InstanceService/ResolveCreateSpec`：服务端复用 compute resolver 的单次读取，再检查镜像、Namespace、SA、存储、调度、网络及卷。Gov 不串行调用公共解析与容器解析来重复读取规格。

InstanceService 原七个目录 RPC 直接删除，不保留转发、别名、旧方法失败后的回退或双目录实现。活跃实例 v2、GpuPresetReference、冻结字节/hash、AcceptCreate 和生命周期合同见[实例合同](instance-mvp.md)及[配置与网络合同](instance-configuration-network.md)。目录中的 `devices_per_instance` 存储 JSON 和实例 v2 字段保持原语义，公共解析显式映射为单执行单元设备数。

## 公共解析

完整方法为 `/compute.v1.ComputePresetService/ResolveComputePreset`。请求字段固定：`tenant_id=1`、`preset_id=2`、`usage=3`。用途枚举 UNSPECIFIED=0（拒绝）、CONTAINER_INSTANCE=1、TRAINING=2、INFERENCE=3、VIRTUAL_MACHINE=4、SANDBOX=5；普通训练与微调共用 TRAINING。Sandbox 一期只接受 `compute_type=cpu` 且 `gpu=null`。

成功结果为一次只读候选，不创建资源、不占额、不产生持久受理 ACK。schema_version 固定1；ComputeSelection 的字段编号为：

| 编号 | 字段 | 含义 |
|---|---|---|
| 1 | schema_version | 内容编码版本 |
| 2–4 | preset_id、code、name | 来源与历史展示 |
| 5 | compute_type | cpu/gpu |
| 6 | cpu_millis | 单执行单元 CPU 毫核 |
| 7 | memory_mib | 单执行单元主机内存 MiB |
| 8 | gpu | CPU 为 null；GPU 为精确引用及设备数 |
| 9 | selection_digest | 内容摘要 |

公共 GPU 结果字段编号依次为 profile_id=1、profile_version=2、spec_digest=3、devices_per_unit=4、cluster_id=5、pool_id=6、mode=7、model_key=8、memory_mib_per_device=9、core_limit_percent=10。设备数量按一个执行容器解释，不乘 replicas、worker 或 LWS 拓扑。

摘要为小写 SHA-256 十六进制：先写入域前缀 `compute-selection-v1\n`（末尾是一个换行字节），再写固定字段顺序、无空白的 UTF-8 JSON。顶层字段按上述1..8顺序，GPU 字段按1..10顺序；CPU `gpu:null`。整数使用 JSON 十进制整数，字符串使用 Go encoding/json 的转义规则；不包含 digest 自身、usage、enabled、时间或审计字段。四种合法用途对同一份内容产生相同资源值与摘要。调用方使用正式 API 的摘要校验函数核对候选，再纳入自己的完整快照。

## 身份、用途与时效

调用登记见[Governance 调用合同](governance-calls.md#公共算力与平台规格)。沿用同一内部业务 TLS1.3/mTLS listener、唯一精确 Gov DNS SAN、完整方法允许清单及单值 tenant/actor/request_id。compute 使用独立可信 Scope，不依赖 instance.Caller。平台与租户范围互斥；非法服务、方法、混合 metadata、跨租户、不规范 actor/request_id 和未获准用途均拒绝。容器内部调用由已验证范围适配为固定 CONTAINER_INSTANCE；VM 的 `/vm.v1.VMService/ResolveCreateSpec` 内部调用由同一受信范围固定 VIRTUAL_MACHINE；Sandbox 的 `/sandbox.v1.SandboxService/ResolveCreateSpec` 固定 SANDBOX，三者不能互借方法或用途。公共调用不能伪造实例/VM/Sandbox上下文绕过用途校验。

Res 校验可信服务/RPC/租户/actor/用途，以及规格存在、enabled、资源组合、精确引用和目标 cluster。Gov 校验当前用户、业务动作、角色和套餐；新 GPU 请求组合已经获准的本业务创建动作与 ACCELERATOR 资格，不要求 Profile 目录读权限。ACC 仍负责既有委托、Profile/供给/数量/状态及执行计划。目录可见性不是 GPU 创建授权。

当前业务授权在每次请求时先执行，随后查原幂等记录；原键返回原冻结结果，不重新读 Preset、不重新执行新 GPU 资格或 ACC Resolve。新键才解析及准入，随后完整冻结/占额/持久投递。worker 与 owner 消费原命令；原账退款不按当前规格重算。

解析默认3秒期限且尊重更短的上游期限。非法 ID/用途/结构为 InvalidArgument；无可信身份/用途未授权为 Unauthenticated/PermissionDenied；不存在为 NotFound；disabled 为 FailedPrecondition；依赖故障/超时为 Unavailable/DeadlineExceeded。不得回退默认规格或公开内部依赖细节。enabled 只在本次读取检查：解析后禁用不追溯撤销已完成的这次解析，AcceptCreate 不重新查目录；本合同没有跨服务 reservation。

## 后续消费者

TRAINING/INFERENCE 已提供 Gov 统一目录查询和 Resource 用途支持，尚未接通创建消费者。训练/微调以后须组合 ModelDev 的 Release、程序、镜像、Runtime 和最终 Snapshot；`compute_preset_id` 必须区别于训练 preset_id，不能修改 owner 快照后沿用旧 hash。本次不增加训练创建算力消费，不修改 ModelDev 或 Gov 训练受理/worker。

INFERENCE 解析不需要模型、引擎、Namespace 或拓扑；未来消费者负责单元到副本/总量的组合与对应业务准入。本次只增加统一算力目录的 INFERENCE 查询与资格，不增加推理管理、创建受理/执行链、Inference API 依赖或部署。VM 创建消费者按[VM正式合同](../contracts/vm-v1.md)组合一次只读compute结果并冻结；公共目录与解析保持通用GPU一致性、引用及cluster校验，不新增VM专属GPU禁令，前端按VM一期范围排除GPU规格。Sandbox 由[统一设计方案](../plans/sandbox-runtime-design-20261025.md)冻结 CPU 规格和工作盘，不把未知目标默认作容器。

## 数据与运行

领域、handler、存取分别位于 `internal/biz/compute`、`internal/service/compute`、`internal/data/compute`；依赖方向 instance→compute。compute 构造不要求镜像、Notebook 或实例运行环境。显式 `ANI_COMPUTE_CONFIG_FILE` 提供 DatabaseDSNFile 与 ClusterID；启用实例时须与实例配置使用同一托管 DSN 文件及 cluster。两种支持实例的 composition 均装配同一 compute resolver。

该配置是受管私有JSON文件，示例值须替换为已核对环境，文件权限沿用现有受管私有文件约束：

```json
{"DatabaseDSNFile":"/managed/instance-runtime.dsn","ClusterID":"<已核对的cluster UUID>"}
```

不把DSN内容写入此文件或日志；配置不接受Preset列表、实例镜像、Namespace、Notebook或GPU执行参数。

`compute.schema_version` 独立维护 compute 版本/checksum，readiness 只检查自己的版本和目标结构。实例历史001..006和 `instance.schema_version` 保留。原地移动、最小 runtime 授权、维护与逆向恢复见[迁移手册](../runbooks/compute-presets-migration.md)；发布新 API 不代表允许先部署删除旧 RPC 的 Res。
