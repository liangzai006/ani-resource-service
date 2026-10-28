# 虚拟机一期规格 v2

日期：2026-10-23。本文件定义目标行为；当前实施结果仅见[唯一状态](../execution/status.md#vm-mvp)。本文件定义业务规则；接口见[正式合同](../contracts/vm-v1.md)，实施顺序见[执行计划](../plans/vm-mvp.md)，验收见[验收矩阵](../verification/vm-mvp-acceptance.md)。本包内新增 API、表和方法均为拟实施设计，不能当作仓库已有能力。

## S01 目标、范围与模块边界

在 ani-resource-service 内新增独立 vm 领域，复用 KubeVirt、CDI、Multus 和现有 Network/KCN。首条纵向结果是：当前租户授权→选择规格/预置 Linux 系统镜像/租户子网→持久受理→独立 Block 系统盘→原生 VM→浏览器 VNC 与来宾网络→停开保盘→产品删除及资源回收。

| 一期交付 | 确定边界 |
|---|---|
| Linux 与 Windows | 各固定至少一个正式 OS 版本；CirrOS 只作探针 |
| qcow2/raw 系统镜像与 ISO 安装 | 管理员预置只读目录；每台 VM 独立可写系统盘 |
| CPU/内存规格 | 复用 compute 目录，新增 VIRTUAL_MACHINE usage，不建 VM 规格表 |
| 电源与 VNC | Manual；Start/Stop/Restart/显式强制停止；浏览器 noVNC |
| 数据盘 | 0 到多块，稳定盘身份；创建、冷挂卸、在线原生热插拔、删除 |
| 网络 | 一块租户主网卡；Multus default + bridge + KCN；动态 IP |
| 快照和恢复 | 在线/离线原生 VM 磁盘快照；关机恢复到同一原 VM |
| 删除 | 清理全部专属盘、介质副本、原生状态卷、快照及网络占用 |

本期不建设 VM 镜像产品、独立云盘、GPU/PCI 直通、CPU/内存热调整、热迁移、HA、自动安装、备份调度、多网卡入口、任意 YAML 或自定义虚拟化/网络/CSI 控制器。前端排除带 GPU 的规格；沿用通用权限、用途、引用和一致性校验，不增加第二套 VM 专属 GPU 禁令，也不实现 VM GPU 执行。

一个业务 VM 独占其盘；CDI importer/cloner、launcher、hotplug helper 可按原生流程接触同一盘。不得把业务独占实现为“只许一个 Pod”或擅改为 RWOP。

## S02 职责、配额与持久受理

- Gov：当前主体/租户/动作授权、套餐功能资格、公开入口和既有创建/删除持久投递。容器与 VM 在共用路径中统一按正式 action 分派。
- Resource vm：创建规格组合、业务记录与 operation、原生请求、观察投影、盘/介质/快照关系、Network consumer。
- Resource compute：唯一规格目录与解析。Resource network：租户子网、Attachment、原生投影与地址/释放证明。跨域仅走 biz 窄端口，不直接写另一域 SQL。
- SG：沿既有短时票据、会话存储、WebSocket 与 VNC 流代理；新增可信 VM/VMI 身份绑定。
- KubeVirt/CDI/CSI/KCN：执行其原生生命周期；平台不重复实现这些控制器。

**本期不接 CPU、内存、磁盘配额余额校验、占用、扣减或归还。** 合法 VM 创建/删除保留既有 Gov operation 和投递，items/charges 为空，不写对应 QuotaCharge、不改账户余额，也不向有账项退款接口发送空 charges。已有容器 GPU 语义保持。

开关机、盘、介质、快照/恢复由 Gov 当前授权并执行[删除持久受理栅栏](../contracts/vm-v1.md#c033-gov-删除持久受理栅栏)后直接调用 Resource，后者原子持久受理；不为这些动作新增 Gov 配额投递。公开调用每次先授权，再查本租户幂等记录。原键同意图返回原结果，异意图冲突。受理 ACK 不代表原生或业务动作完成。

新 vm 领域可以有自己的 operation/work 行与恢复扫描，采用现有 Instance/Network 持久受理、租约、投递、回执模式；不得直接写 Instance 表，也不抽取新通用 outbox/工作流平台。一个 VM 同时只执行一个配置/电源/盘/快照/恢复变更，复用其业务 version 与原子受理检查；读与符合条件的 VNC 不受此一概阻断。Delete 可原子置 closing，停止新变更，接管既有操作的查证/清理；结果未知的原生创建先查明，再继续删除，不另发冲突变更。

## S03 环境前置与本次授权

核对目标版本 KubeVirt 1.9.0、CDI 1.66.1；附件中的 Kubernetes 1.35.8、Multus 4.3.1-thick 和 KCN 现场记录仅为既有探测信息，须在实施时读取实际版本、镜像 digest、CRD served version 与节点能力，不自动升级。

任务包记录的 `device_ownership_from_security_context` 缺项与 Block 导入权限错误来自此前环境，只作线索。本轮唯一目标为 172.16.101.10、172.16.101.11、172.16.101.12，先只读固定集群与节点身份，再核验有效 runtime 配置和 Block 路径；不得沿用 100.64.128.12 / 10.10.1.66～68 的结论。若无需变更，记录当前事实并验证原生 Block 能力；若确有缺项才登记 ENV-BLOCK，并按下述具体批准执行。该配置与 CDI 官方说明相关，不宣称它是唯一可能原因。[U01](../references/vm-mvp-sources.md)

VM-00 只读取获准访问的环境、准备具体配置差异、备份/恢复方式和节点顺序。VM-00R 只在以下两个条件都成立后写入：

1. 用户确认集群无人使用、给出维护窗口及受影响节点范围；
2. 用户届时明确授权该具体 runtime 变更、相关服务重启与限定探针。

查看负载、存在本任务包、历史探测授权都不能代替上述授权。agent 不得为“腾空集群”自行停止用户工作负载。

按实际 runtime 选择配置源：containerd v1/v2 与 CRI-O 配置段不同；k3s/RKE2 管理的文件应改受支持的生成输入，不能只改会被覆盖的生成文件。批准后逐节点执行最小配置变更及验证，已运行的 CDI Pod 按批准范围重建；保留原配置及恢复记录。失败按批准的恢复方案处理，不改 root/privileged、宿主机设备权限、Filesystem 或共享 CDI 策略来伪造通过。

代码与隔离测试不依赖 VM-00R；真实 Block 验收依赖当前目标 Block 能力已验证，只有确需 runtime 变更时才依赖获批 VM-00R。NAD、快照组件、feature gate、物料缺项独立记录，只阻断依赖能力。VM-00R 不自动授权 KCN/CSI/KubeVirt 升级、共享 gate 改动或 installer 重构。

## S04 规格、容量与创建冻结

Resource 的 ResolveCreateSpec 只读组合 compute 一次，再校验介质/磁盘/Network 业务引用；本 RPC 不创建 Attachment、DV、VM。保存用户意图摘要、ComputeSelection/selection_digest、来源固定身份、完整冻结创建规格与 vm_create_digest。重放及 Start 不重新解析当前目录；不虚构 compute preset_version。

- cpu_millis 保留原值用于 CPU request/limit；来宾拓扑为 1 socket、ceil(cpu_millis/1000) cores、1 thread。1500m 对应 2 vCPU 与 1500m CPU 限额，不称为两个独占物理核。
- memory_mib 写入 guest/request；QEMU/launcher 调度开销交给 KubeVirt 加算，不把来宾内存直接作为整个 Pod 的硬上限。
- 系统/数据盘 size_gb 为正整数、步长 1 GB，1 GB=1,000,000,000 bytes；原生写 NG，如 20G。memory_mib 仍为 MiB，不能因磁盘 GB 规则改内存单位。
- 保存 requested_bytes 和观察到的实际容量；实际分配可向上取整，须至少满足请求。来宾分区/文件系统空闲量不承诺恰为 N GB。
- 镜像最小系统盘要求采用虚拟容量及目标 CDI 路径的原生限制，不使用压缩 qcow2 文件大小。Block 源克隆也遵守源 PVC 请求容量约束；跨模式克隆按指定 CDI 路径验证，不假设所有模式都按同一容量公式。
- cluster、namespace 及 namespace_uid 从可信平台定位冻结。当前配置修订与初始创建审计快照分别保存；修改/恢复不能回写旧初始配置。

来源与原生资源规则依据 [U02/U03](../references/vm-mvp-sources.md)。

## S05 预置介质与设备默认值

管理员维护受管只读配置目录；不提供上传、发布、登记、删除或版本管理页面。每项包含 media_key、name、kind（DISK_IMAGE/INSTALL_ISO/DRIVER_ISO）、source cluster/namespace/PVC name/UID、content checksum、source volumeMode/contentType、实际/虚拟容量、os_family/os_version、architecture、firmware、root/data bus、网卡型号、TPM/UEFI 持久要求、driver_media_key。

来源内容必须按管理约定冻结；同名换 UID 或校验值变化不能被静默采用。Resolve 校验并冻结来源，真正克隆前重新校验已冻结身份；已完成系统盘的 Start 不依赖管理员源仍存在。重新挂载未保留的介质副本只能使用该冻结来源，来源失效则报依赖变化。

显式 SetMedia 可以选择与该 VM 冻结 OS/设备合同兼容的新 media_key。首次持久受理前确定本次来源与副本身份，将 source cluster/namespace/PVC name/UID、checksum、mode/contentType 等冻结到本次 operation；该次配置结果进入 current configuration，不回写初始 create_spec。重放先返回原操作，执行重试用该次冻结来源，克隆前重核身份；复用已保留专属副本则使用其已登记身份。Start 不重新解析目录。

首条 Linux 路径使用已经验收的 Block 系统镜像源；若实际受管源为 Filesystem，VM-00 固定该指定 FS→Block 原生克隆路径，单独验证，不改变目标 Block。CDI 能按条件使用原生 host-assisted，不自行运行 qemu-img，不改全局 cloneStrategyOverride。[U02]

每台 VM 克隆独立系统盘。ISO/驱动在租户 namespace 内创建 VM 专属副本，以 SATA 只读 CD-ROM 呈现给来宾；Filesystem 副本的 launcher PVC 挂载须允许 KubeVirt 初始化目录属主，不能把整个宿主文件系统挂为只读而阻断启动。该初始化只作用于 VM 自有副本，来宾设备始终只读；来源不直接作为可写盘挂载，不给共享源加 VM owner。ISO/驱动目标模式按目录冻结，本期物料优先选择已验证 Filesystem，不能套用 root/data Block 规则。跨 namespace 克隆使用 CDI 原生权限，不扩大 cluster 权限规避拒绝。

ISO 路径为空白 Block 根盘+预定义安装光驱/必要驱动光驱，VNC 手动安装。boot_device 仅 INSTALL_MEDIA/SYSTEM_DISK。运行中介质变化只在目标版本支持的原生能力下尝试；若只修改持久模板，配置operation核对持久结果后结束，返回明确“下次启动生效”，不占住互斥等待后续Start，不隐式重启，不伪报实际已弹出。替换或弹出的 VM 专属介质副本保留到最终删除，避免运行态旧引用仍在时提前删除。

Windows 固定至少一个明确版本及 VirtIO 驱动，驱动光驱 SATA/只读，验证识盘、NetKVM、键鼠、SCSI 数据盘及 Guest Agent。没有自动驱动注入、激活或 Installing/Installed 自定义状态。要求 TPM/UEFI 的所选 OS 必须使用原生持久状态，KubeVirt backend PVC 为 Filesystem，并验证对应存储的快照能力；不能用关闭系统要求替代实现。[U07]

原生默认配置仅由已冻结的 `os_family=windows` 选择，采用 [KubeVirt v1.9 Windows 示例](https://github.com/kubevirt/kubevirt/blob/v1.9.0/examples/vmi-windows.yaml#L10-L35)中的 ACPI/APIC、Hyper-V relaxed/vapic/spinlocks（8191），以及 UTC、禁用 HPET、PIT delay、RTC catchup 和 Hyper-V clock。新建时渲染；旧模板只在正常 Start 的已停止、无 VMI/launcher/原生待处理请求阶段，以归属、UID 和 resourceVersion 守卫补齐缺项。保留其他配置，显式冲突报错，运行态不改模板，Linux 不应用这些默认值；配置存在不替代真实 Windows 可用性验收。

### S05.1 Linux 镜像的可选账户初始化

仅对目录中明确标记、且已由真实客体验证原生 NoCloud 的 Linux `DISK_IMAGE` 提供可选账户初始化。能力证据绑定同一 source PVC UID、内容校验值和实际 OS 版本；不能按 OS 名称推断支持，也不能把仅创建 Secret、VMI Running 或另一镜像的结果标为 verified。Windows、ISO 安装路径及未验证介质不接受此输入。

公开输入限用户名、控制台登录密码和用户提供的 SSH 公钥。按合同校验格式与限额；不接受私钥、任意 user-data/YAML、来宾脚本或命令。通过每台 VM 专属原生 NoCloud Secret/volume 初始化独立克隆盘；不修改共享源镜像，不制作派生镜像，也不把这项能力扩展为自动安装。

初始化意图与来源能力在创建受理时冻结，参与完整幂等比较；合法重放返回原操作。NoCloud 的实例身份在同一业务 VM 内保持稳定，Stop/Start/Restart 不重新设置账户或密码。密码与明文 user-data 不进入读 API、操作结果、错误、日志、公开审计或验收材料；只通过受保护的受理/执行状态和原生 Secret 使用，不提供读回密码的入口。

该初始化路径同时提供平台固定的单主网卡 DHCP network-data，由同一 VM 专属 Secret 的原生 `networkDataSecretRef` 引用；仅匹配 Ethernet 名称，不固定首个 VMI 的 MAC、IP 或默认路由，不接收用户网络配置。验证镜像能力时包含该网络配置的兼容性及停开后 DHCP。NoCloud 实例身份保持稳定，不能靠改变 instance-id 重置账户来重新生成网络规则。

## S06 磁盘声明与创建时序

唯一新建策略：**独立 DV，不使用 dataVolumeTemplates**。不留两套可由 agent 任选的盘生命周期。

1. 受理时持久保存 VM ID、所有初始盘 disk_id/volume_name/serial、operation 与确定的原生名称；每个名称在该 VM 生命周期内不复用给另一块逻辑盘。
2. Network Prepare 返回并保存 Attachment/冻结网络投影。
3. 创建 `runStrategy: Manual` 根 VM，模板引用确定的 DV 名称；登记真实 VM UID。此时允许所引用 DV 尚未创建。
4. 创建独立 DV，owner 指向该 VM UID；PVC 使用 CDI 原生 ownership。系统/数据 DV 显式 volumeMode=Block、accessModes=[ReadWriteOnce]，固定已验证 StorageClass。不能给同一对象追加两个 controller owner。
5. start_after_create=true 时按原生流程发 Start，使 VMI 作为消费者参与调度和盘准备；**不得先等待 DV Succeeded 才创建消费者**。
6. start_after_create=false 时，根 VM、DV 声明和身份已持久登记即可完成创建 operation；磁盘仍可显示 WFFC/待准备。首次 Start 再驱动原生准备，不能暗中启动停止态 VM。

WFFC/PendingPopulation 的观察与错误如实展示；不为提前 Ready 修改 StorageClass 或 CDI gate。[U02/U04]

数据盘从首次配置起采用 SCSI、稳定 serial 与 hotpluggable=true；系统盘不开放在线摘除。CreateDisk 在独立 DV 声明及身份持久登记后即可完成，允许 preparation=WaitForFirstConsumer/PendingPopulation；AttachDisk 不以 DV Succeeded 为受理前提。冷挂卸在持久 VM 模板与逻辑关系核对后完成，实际挂载等下次 Start；在线挂卸用 DeclarativeHotplugVolumes，完成时才同时核对持久模板与 VMI volumeStatus。不得为等待磁盘准备占住只有后续 Attach/Start 才能推进的 operation。guest 是否已格式化/挂载由用户操作。运行中卸载需要用户先在 guest 卸载文件系统/离线磁盘，平台不执行来宾命令，也不能宣称仅凭 native API 已证明 guest 不忙。

卸载只解除 VM/VMI 的卷和设备引用，保留 DV/PVC 与所有权。再挂载引用当前盘，不重新 blank/clone。单盘删除要求当前已卸载且原生引用已退出，带 UID 前置删除精确自有 DV/PVC，并确认回收；不提供独立云盘或跨 VM 引用。

旧 HotplugVolumes gate 必须未启用，否则目标版本可能跳过声明式路径；DeclarativeHotplugVolumes 按实际配置确认。共享 gate 缺失单独阻塞热插拔卡，不允许 agent 自行改动。[U05]

## S07 Network 与动态地址

接口与协议固定见合同 C05。网络路径为一个主网 `Multus default + bridge + KCN`。NIC 内部为集合，一期仅 nic_name=default。

新增 vm_managed/v3 consumer，严格验证 VM→VMI→launcher Pod 的 GVK、namespace/name/UID、owner、租户与冻结网络选择。旧 single_pod/v1 和 Notebook/v2 路径保留；不把旧校验泛化为任意 GVK。

参与租户 namespace 的受管 NAD 和 workload SA 由 Resource Network 现有持久 worker 按真实租户、集群及 namespace 自动准备；每个租户网络 namespace 复用一个 KCN 单插件 NAD，多子网仍使用各自独立的 kcn_subnet_ref。准备意图先持久化，Kubernetes 创建回执的 Namespace/NAD/SA UID 与模板摘要随后固定；不接管同名异主对象，也不在已登记 UID 丢失后重建或认可新对象。失败只阻止 VM 使用该依赖，不改变普通 VPC/Subnet 可用性。已存在租户网络同样进入周期准备，无需逐租户改配置或重启服务。VM ResolveCreateSpec 保持只读，按真实网络绑定读取并核验持久 NAD 身份；兼容既有明确部署映射。NAD 不是 KCN Subnet，二者不可共用一个字段。

NAD/来源依赖失效只阻止需要该依赖的新建、启动和当前地址授权，不能阻止按已登记原生 UID 执行 Stop/Delete。Network 依据冻结关系的实际退出证明完成释放，不要求 NAD 此时仍健康。

| 动作 | 固定网络行为 |
|---|---|
| 停止态创建 | Attachment reserved；无当前 IP 正常 |
| Start | 原 Attachment、原子网和原 NAD 身份；核验新 VMI/launcher 后产生当前地址 |
| Stop | consumer 仍 open，Attachment 回 reserved；撤销旧 launcher 地址资格，不 Release |
| Restart | 旧 launcher 退休，新 launcher 重新核验；不换子网 |
| Restore | 原 VM UID/Attachment/网络选择不变；不恢复旧 IP 或 launcher 身份 |
| 最终删除/放弃创建 | closing 阻止新提交；查明未知创建并确认原 VM/VMI/launcher 退出后，将 consumer 置 closed 并以稳定 finalization_id 请求 Release；Network 独立证明其 VNic/VNicIP/Nic 等依赖退出后标记 released |

CurrentPods 仅为当前 launcher，最多一个；CDI/hotplug helper 不作 VM 网络端点。地址由 Network 已核验结果通过正式 DTO 返回，VM 不跨域读 provider_relations。停止、观察过期、身份冲突时不显示旧地址为当前可用；显示 observed_at/reason，允许启停后 IP 改变。

控制台可使用不依赖所有盘或 CSI 健康的窄运行身份观察，但须保持相同 namespace/UID/owner、唯一 launcher、业务 version 与 closing 校验；普通生命周期仍使用完整存储观察。

租户网络验收使用真实 VPC/Subnet，覆盖跨节点通信、DNS、出站和按现有网络规则跨租户拒绝。旧共享默认子网探测不证明租户隔离。secondary KCN 已知问题、固定 IP、多网卡产品和 NetworkPolicy 对 guest 的自动覆盖均不在本期实现范围。[R03/U08]

创建 VM 提供“平台默认 DNS”（省略时默认）和“自定义 DNS”。自定义接受 1–3 个有效 IPv4/IPv6 地址，规范化并保序去重；平台默认不附自定义地址，由可信部署配置提供。只读默认值接口用于创建页展示实际配置。首次解析将生效 DNS 随创建规格持久冻结，Start、重放与恢复不重新读取平台默认；原生 VM 使用 dnsPolicy=None 和冻结的 dnsConfig.nameservers。缺失默认配置明确拒绝默认模式的新建，自定义模式仍按输入校验；旧无 DNS 冻结字段的 VM 保持原合同。此选择不自动建立 VPC 出口或内部 DNS 路由，真实 Guest 必须分别验收实际 DNS 与外部访问。

## S08 电源与 VNC

默认 runStrategy=Manual。操作含义固定：

- Start：使用当前生效配置调用原生 Start；已达到运行目标时按当前 operation 语义收敛，不另建盘。
- Stop：先尝试正常关机，宽限期届满可进入 KubeVirt 原生终止流程。不能承诺 guest 拒绝时永远不终止。
- ForceStop：显式权限/用户确认后调用原生强制停止参数；不承诺断连节点立即物理断电。
- Restart：原生停止当前 VMI 并创建新 VMI。复用 operation，提交前记录 old_vmi_uid。

Restart 传输结果未知时先观察原 VM、stateChangeRequests、旧/新 VMI UID；看到新 VMI 且身份、盘网、运行目标一致后完成。证据不足保留原操作继续观察，按现有超时/未知结果机制报告；不因仍看到旧 UID、队列已空、进程重启或上游重试而盲目再发 Restart。old_vmi_uid 是观察线索，不是原生 exactly-once token，不新增运行代次表/API。[U06]

Stop 完成须确认 VMI 退出、launcher 不再运行且网络旧地址资格已撤销；原 VM、盘、Attachment 保留。删除另按 S10。节点/控制面失联无法确认时显示未知，不新造 HA/fencing 系统，也不伪造释放。

VNC 路径：Gov 当前授权→ResolveConsoleTarget→SG CreateSession→noVNC/SG→KubeVirt VNC。固定原 VM/VMI UID 和业务 version，连接前及原生开流后、向浏览器转发前再校验，防止同名新 VMI 接走旧票据。target 来自可信冻结身份，不能由浏览器提交 namespace/UID/地址。

VNC 不依赖 guest IP、Network attached、AgentConnected、OS 安装完成或整个创建 operation 已成功；安装/网络排障期间，可信且可开 VNC 的运行 VMI 可以使用控制台。关闭/替换目标的冲突意图仍拒绝新会话。

原生 VM 模板固定提供 USB tablet 绝对定位输入，保证 Linux/Windows 安装介质阶段可用鼠标；不依赖预装 VirtIO input 驱动。既有模板缺少该设备时，只在正常 Start 经 namespace/VM UID 与停止态核验后，以 UID/resourceVersion 条件补齐，保留其他设备；不在线改运行 VMI 或自动重启。此项是固定执行配置，不增加用户设备管理接口。[KubeVirt v1.9 输入定义](https://github.com/kubevirt/api/blob/v1.9.0/core/v1/schema.go#L507-L530)

复用 SG 原票据 TTL、单次 claim、Origin 白名单和流关闭机制；noVNC 使用既有 ani.vnc.v1 二进制协议。Stop/Restart/Restore/Delete 后旧未使用票据因 version/VMI UID 不符而失效。Resource 完成条件依据旧 VMI/launcher 退出及旧 binding 失效；SG 随原生流结束关闭 WebSocket 并释放会话名额，真实端到端验收必须验证旧连接结束。operation 不等待不存在的 SG 会话关闭回执。不开新全局撤销服务，不承诺 Stop 受理瞬间踢掉所有旧流。[R04]

## S09 快照与原 VM 恢复

只创建/读取/删除 KubeVirt VirtualMachineSnapshot/Restore；CSI 由原生 controller 管理，不逐盘拼装快照。前置为实际可用 Snapshot API/controller、匹配 driver 的 VolumeSnapshotClass、所需 gate。持久状态卷如启用也必须在原生支持与验收范围。

快照覆盖“该快照时当前持久挂载的 root/data”，记录 disk_id↔volume_name/serial、role/size、必要介质/设备配置、source VM UID、完整性与原生 indications。当前未挂载盘不宣称受保护。必要卷未 included 或原生未 Ready，不能标记完整可恢复快照；本期 Restore 只接受必要卷完整且原生可用的快照。Guest Agent 参与与 freeze 结果按实际展示，不承诺数据库/外部系统事务一致性；不提供可恢复 RAM 状态，也不称多卷快照为原子 group snapshot。

恢复只针对同租户、同一根 VM UID；要求已停止且 VMI 对象不存在，targetReadinessPolicy=FailImmediate、volumeRestorePolicy=RandomizeNames、volumeOwnershipPolicy=Vm。不隐式关机/开机，不提供策略选择。

### 恢复盘集合与确认

PreviewRestore 返回当前 vm.version、snapshot UID、完整影响清单和 preview_digest。提交携带同版本及摘要，服务端重新核对并取得 VM 变更互斥；变更则要求重新预览。

| 逻辑盘位置 | 本期恢复行为 |
|---|---|
| 在快照卷集合内，目前仍挂载 | 恢复快照内容及配置，映射到原 disk_id；旧物理卷成功后清理 |
| 在快照卷集合内，目前已卸载 | 恢复内容并按快照重新挂载；旧物理卷成功后清理 |
| 在快照卷集合内，目前已删除 | 依据快照内盘清单恢复该逻辑 disk_id 与新 PVC，不依赖当前未删除盘表 |
| 不在快照内、目前仍挂载的新增数据盘 | 明确列入破坏性清单；恢复成功后删除该 VM 自有盘 |
| 不在快照内、目前已卸载的数据盘 | 保留内容、身份、所有权和卸载关系，不进入本次删除清单 |

快照行保存逻辑盘清单；盘记录保留 tombstone/旧引用直到相关清理已证明，volume_name 不被复用给另一逻辑盘。不新增独立盘历史系统。

### 恢复完成与旧卷清理

既有 operation 只保存三个必要进展点：native_restore_completed、mapping_committed、cleanup_completed。

1. 原生 Restore 完成并核对新映射前，平台不主动删除旧 root/data 自有卷；失败或结果未知时保留其 cleanup refs 并继续查证。backend state 的切换及旧卷清理由 KubeVirt 原生 Restore 管理，不承诺失败时自动回滚或保留全部原生状态卷。
2. 原生完成后读取 status.restores、实际 VM/PVC 与 UID，核对保存新的 current configuration/逻辑盘映射；恢复到 PVC 引用是合法结果。
3. 目标仍保持停止态，按已冻结数据盘设备合同补回 PVC hotpluggable=true、SCSI/serial 等必要字段；不能整份覆盖模板或改回初始旧 DV。
4. 映射持久后，按预览冻结清单、所有权与 UID 精确删除被替换旧盘及新增挂载盘。独立 DV 不依赖 deletedDataVolumes 自动列出旧卷。
5. 清理未确认时 operation 保持处理中，reason 明确“恢复完成，旧卷清理中”；保留新映射及旧 cleanup refs，继续同一操作，不重复 Restore。
6. 新映射、必要原生配置和清理都完成后才结束 operation，用户随后显式 Start。恢复后 Start 不重解析 compute 或重写创建快照。

恢复后的 WFFC PVC 可保持 Pending；完成核验不要求先 Bound/Succeeded，也不启动消费者，后续用户显式 Start 再驱动绑定与准备。

以上映射/清理是 VM 自有资源管理，不是另建 CSI 恢复编排。恢复后的在线摘挂必须再验，防止原生 DV→PVC 转换丢失 hotpluggable。[U09]

## S10 删除、所有权与可观察完成

新建链为 VM→独立 DV→CDI PVC；恢复 PVC 与 backend state 采用原生 VM 所有权。共享预置源、租户 NAD 不加用户 VM owner，也不在用户 VM 删除范围。

删除先持久 closing，拒绝新副作用/新 VNC，处理已投递但未知的原生动作。枚举所有当前、卸载、已替换待清理盘、ISO副本、backend state、快照及操作创建的原生资源；不得只看当前 template.volumes。

使用 OwnerReference/原生 GC 为主，必要显式删除只针对已登记且重新核验的自身 UID；无授权不得强删 finalizer 或顺手清理同 namespace 其他对象。selected SC/PV reclaimPolicy 和 VolumeSnapshotClass deletionPolicy 必须能兑现本期回收目标；采用已验证 Delete 策略，不静默改共享策略，也不把 PVC 消失当作后端卷已回收。[U10]

VM operation 删除完成需：目标 VM/VMI/launcher 已退出，自有 DV/PVC/介质/backend state/快照及关联持久资源按原生策略完成清理，Network released，旧 VNC binding 已无效；SG 旧流关闭的实现与验收按 S08，不新增关闭回执依赖。没有 CPU/内存/磁盘退款前置。若某环节未知/残留，保留 closing、cleanup refs 和具体原因，不靠删除业务行宣告成功。

原始共享源、租户 NAD、其他 VM/租户资源必须保持不变。导入中、停止态、多盘热插拔后、恢复清理中删除都纳入验收。

## S11 数据与观察

新增 vm schema 及显式增量迁移；各表租户复合键/约束，禁止跨域 SQL/FK。最小逻辑数据：

- virtual_machines：tenant/id/name、cluster/namespace/namespace_uid、native VM name/UID、初始意图/摘要、冻结 spec、current configuration/version、Attachment、closing、观察摘要。
- disks：tenant/vm/disk_id、role/size_gb/requested_bytes、volume_name/serial、挂载关系、当前 DV/PVC 引用、tombstone 与未完成 cleanup refs。
- snapshots：tenant/vm/snapshot_id、native UID/content、snapshot disk inventory、完整性/一致性/观察结果。
- operations：复用域内持久受理模式，保存 action/幂等摘要/目标/意图version、pending mutation、native结果和必要上下文；lease/work 可依现有模式实现，不能做另一套总线。
- 介质副本及 NIC 数据在 VM 配置中；不预建镜像表、独立云盘表或 provider 框架。

观察投影区分 desired/observed、operation、reason、observed_at、disk preparation、media effective/pending、snapshot completeness。断开/过期为 Unknown/信息过期；VMI Running 不等于 OS 安装完成或来宾健康。对外查询若 Gov 已受理、Res 尚未持久接收，返回原排队/投递状态，不能误报资源从未存在。

## S12 实施边界与规则变更

正常开发按[计划](../plans/vm-mvp.md)、[本轮完整 Goal](../plans/vm-mvp-complete-goal-20261023.md)与当前用户指令执行。生成、编译、测试、PG、镜像、服务和 API/集群驱动仅使用 ssh fedora，禁止 Ubuntu 或本地重任务回退；本地限阅读、编辑、轻量检查、Git/转运和现有浏览器。Git clone/fetch/pull/push 均用 SSH。使用任务独占目录并保留无关改动；固定实际 host、源码和集群身份。16 卡按依赖连续推进，环境批准只暂停依赖步骤，不逐卡询问继续。

不要求重写注册框架、通用 operation 平台、Network worker、整个 API module 结构或 installer。运行时、共享 gate、KCN、存储策略、生产部署各按实际明确授权范围执行。

若本版固定选择与实际能力不符，记录具体失败与受影响卡；先完成不受影响工作，再由用户决定范围变更。不得把失败功能自动降级为下一期、改网络/模式绕过，或宣称未跑的 Linux/Windows/热插拔/快照已通过。
