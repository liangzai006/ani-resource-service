# VPC 基础内网、公网出站与租户 LB 统一方案

初始日期：2026-09-14；后端合同更新：2026-10-10。状态：实现范围与实际验收分别见执行状态；用户已确认的约束见 [ADR-0005](../adr/0005-separate-vpc-connectivity-and-exclusive-eip-bindings.md)。本文定义目标行为，实施任务见[执行计划](../plans/vpc-connectivity-lb.md)，当前进度只看[执行状态](../execution/status.md)。历史实施结果保留各自时点；本次固定三规格、多监听器和 VPC 预设只改后端，未修改前端或共享平台预置。

## 1. 固定输入和变化范围

Network 实施基础为 `d8835a22d905e358b7f60756d3113baa97d7c762`（既有 EIP/SNAT 分支），不是只有 VPC/Subnet 的 main `e481e968...`。保留其租户隔离、操作、幂等、UID、fencing 和 SharedInformer→持久唤醒→worker 模型。不能把手工 kc 实验直接并入产品数据库。

本方案覆盖：平台基础网络准备、已有 VPC 创建/删除/观察的调整、既有 Public EIP/SNAT 的调整、三种类型租户 LB 的新生命周期，以及 API/界面接入任务。Network 保持唯一网络产品写入者；ANI Gateway/Console 只调用契约和展示结果；实例 owner 仍创建 Pod/VM；kc 与 Envoy Gateway 各自负责其数据面资源。

本批 LB 交付范围为 IPv4、单集群、三种入口类型、1～64 个独立 HTTP/HTTPS/TCP/UDP/SSL 监听器、固定 small/medium/large、同 VPC 的 IP 后端、权重与监听参数。旧单监听器默认 HTTP/8080 输入继续兼容。复杂路由、证书管理、在线切换 LB 类型/规格、HA/容量和 Underlay 物理验收仍属后续单独任务。

空监听器保存、独立 Provider 目标版本、下发集合与清空/再激活的增量合同由 [LB 延迟下发规格](lb-deferred-activation.md) §§2～6 定义；实际验证范围只见 [LB-DEFERRED-01](../execution/status.md#lb-deferred-01)。

已有 VPC/Subnet/Attachment 的未冲突规则、Public EIP 的独立申请/保留/释放语义、平台 Public 网络模式与当前准入规则继续适用。本文替代旧方案中“每 VPC 总共一个 SNAT”“VPC Ready 即整个创建完成”“任何 SNAT 都直接阻塞 VPC 删除”“EIP 只能用于 SNAT”的相关限制。

### 1.1 已有证据与边界

- 独立 Network EIP/SNAT 代码已有受控 Provider、真实 PostgreSQL、恢复及观察验证；其历史公网原生 Overlay 验收存在外部 kc 阻塞，不能因新 LB 成功而自动关闭。
- 2026-09-14 手工 CR 实验补齐 Intranet Subnet→EIP→Snat，并由用户重启 kc 控制器后绑定成功；三种 HTTP LB 的 kind 私网/外部 EIP 入口成功，配套 30 项检查 pass。
- 手工 CR、kind 节点作为外部客户端、实验 Public 地址不等于产品 API 创建链路、真实互联网、生产可用性或高可用证明。
- 控制器初始化缓存问题的修复由 kc 团队承担；Network 不自动重启控制器、改 OVN 路由或用 NodePort 代替 EIP。

## 2. 产品模型和固定规则

术语统一见 [CONTEXT](../../CONTEXT.md)。本节是规则权威来源，任务文档只引用。

| 对象 | 管理者 | 创建及释放方式 |
|---|---|---|
| Intranet / Public Address Pool、出口网关、必要的二层网络 | 平台管理员 | 管理员独立初始化，租户只消费能力 |
| VPC | 租户 | 创建同时编排基础内网连接；删除时清理系统子资源 |
| 基础 Intranet EIP/SNAT | 系统，资源归属仍为对应租户 | 随 VPC 创建、观察、修复、释放；不提供租户独立解绑 |
| Public EIP | 租户 | 默认从平台 Public 池申请；未绑定时可保留或释放 |
| Public VPC SNAT Binding | 租户 | 将独立 Public EIP 用于整个 VPC 出站；可启停/解绑 |
| LoadBalancer | 租户 | 属于单一 VPC/Subnet；可有私网入口、公网入口或两者 |
| LB 的 EIP Binding | 租户资源操作产生，系统维护占用 | 占用一个已申请 Public EIP；LB 删除后归还 EIP，不自动释放地址 |

规则：

1. 同 VPC 最多一个未释放的 Intranet SNAT 和一个未释放的 Public SNAT；停用、退化、删除中、结果未知均继续占用自己的位置。
2. 一个 EIP 同时只有一个有效目标：`vpc_snat` 或 `load_balancer`，从受理预留开始即排他。目标修改必须先完成解绑；不得两份资源各自声明唯一却一起占用。
3. Intranet EIP 是 system-managed，只能用于本 VPC 的基础 SNAT；Public EIP 可以用于 Public SNAT 或一个有公网入口的 LB。地址池类型、管理归属与用途必须匹配。
4. 申请租户 EIP API 默认且只申请 Public；基础地址不混入租户公网 EIP 列表，猜测内部 ID 也不能 GET/启停/解绑/删除它。VPC 只读显示 `base_connectivity` 摘要，管理员诊断视图可查基础资源。
5. 纯私网 LB 不占用 Public EIP；三个类型均依赖 VPC 基础内网连接。创建公网 LB 不要求该 VPC 已有 Public SNAT。
6. Public SNAT 关闭、解绑、换绑只作用于公网绑定，Intranet EIP/Snat 的身份、地址和期望保持不变。删除 LB 不解绑 VPC 的任一 SNAT。
7. 跨租户绑定统一按不存在处理；所有关系验证 tenant、cluster、namespace、产品 ID、Provider 名称及 UID。Provider 命名用稳定产品 ID，不使用租户显示名。
8. 资源状态、当前配置应用状态与数据面探测证据分开。`available` 表示本节后续定义的配置完成条件，不能独自证明所有流量健康；观测过期明确显示 unknown/stale。

## 3. 平台初始化流程

管理员按环境准备所需二层网络、Public EIPGateway/Public Pool、Intranet Pool、`intranetNetworks` 和 LB 运行组件。Overlay 无 `underlayConfig`，不强制创建 VLAN；Underlay 需物理网卡、VLAN、上游网关和单独验收。保留既有网卡接管保护和 Public 池资源就绪检查；公有网络不再以手工出网证据作为准入条件，详见 [Public 分配规则](vpc-snat.md#44-出口验收开放分配和默认池)。

安装器已经将物理口加入 `managedDevices` 并由 kc 接管时，应核验并登记该现成设备，再创建二层网络；不能将“已接管”本身判作环境阻塞。空闲接管与已有设备登记共用管理员接口，身份、并发与退役规则统一见[Public 方案 4.1](vpc-snat.md#41-underlay-的网卡发现与二层网络)。现场连通性和完整产品 API 数据面仍分别验证。

每个部署集群分别配置默认 Intranet 池和默认 Public 池，池的 scope 创建后不可变。默认池选择只影响后续新申请，已受理操作持久固定池 ID、配置版本和 Provider placement；重试不能改选新默认池。关闭新分配不删除或解绑已有地址。

Intranet Pool 映射为平台 namespace 的 `Subnet(type: Intranet)`，显式识别默认 VPC 网关，不能创建 scope Public 的 EIPGateway 来假冒。平台负责内网目标网段（含实际 DNS/xDS/Service 目标所需范围）的配置和验证，租户请求不得修改集群路由范围。Public 池沿用现有 Overlay/Underlay 与网关配置。

平台暴露三个独立能力摘要：`base_connectivity_ready`、`public_address_ready`、`load_balancer_ready`。前者失败影响新 VPC 基础连接，Public 失败不阻止纯私网场景；LB 组件未就绪不应阻止没有 LB 的 VPC 创建。既有能力的健康、证据时效、新分配开关分别记录，不合并成一个含糊的“网络正常”开关。

LB 初始化严格消费附件 `install/` 配置：GatewayNamespace、Backend API、所需 RBAC/TokenReview、GatewayClass/EnvoyProxy 映射和固定镜像来源。规格由平台目录映射，租户传 `flavor`，不能传任意 GatewayClass、镜像、patch、Kubernetes annotation。固定规格映射见下表，运行核对按对应型号的副本与资源执行；缺失的 Class/EnvoyProxy 不由 Resource 补装，dynamic 不开放。

## 4. 租户 VPC 流程调整

### 4.1 首次受理与分步创建

CreateVPC 的租户输入保持名称/CIDR/描述/幂等键，不让用户选择基础 EIP 或 SNAT。单次受理事务保存 VPC、create operation、稳定的基础 EIP/SNAT 子资源 ID、固定 Intranet 池版本、Provider placement、关系和待执行工作；外部 Kubernetes 调用在事务外进行。

按以下顺序推进：

```text
受理并持久化意图
  → 创建/确认 VPC CR 身份及 Provider Ready
  → 从固定 Intranet 池分配基础 EIP
  → 为该 VPC 创建 Intranet Snat
  → 同时核对 Snat 与 EIP 的身份、代次和实际绑定
  → VPC 基础连接就绪，create_vpc operation 成功
```

关键：内部基础 SNAT 只依赖 `VPC Provider Ready + 稳定 UID`，**不能调用要求 VPC 产品 available 的公开 Public BindSnat 流程**。VPC 聚合 available 又依赖基础 SNAT，如果不分层会永久互相等待。

基础子资源使用内部持久步骤/子资源工作记录，只有 VPC 创建 operation 对外代表本次整体请求。每步具有稳定身份、pending mutation、lease/fence、重试和结果归因；不靠串行 RPC 请求栈保存进度，也不创建跨数据库事务。

### 4.2 就绪与持续观察

新 VPC 创建完成同时满足：VPC 同 UID/期望 spec、当前代次 Ready；基础 EIP 池/地址/UID 正确；Snat 与 EIP 双向绑定正确、期望代次应用、启用状态正确；观察新鲜；无未知外部变更。Provider `Snat.spec.vpc` 写短名；EIP `status.boundResource.vpc` 核对 `namespace/name`。

只读响应新增 `base_connectivity: {state, reason, observed_at, observation_stale}`，不暴露可由租户修改的内部 EIP/SNAT ID。已有 VPC 状态机继续使用 provisioning/available/degraded/deleting/deleted/failed；基础连接失败使新 VPC 保持 provisioning/明确 operation 原因，已完成 VPC 的依赖后来失效则 degraded。恢复只更新当前状态，不能重写历史成功 operation。

受理时默认 Intranet 池不存在/不开放/状态过期，返回明确 `BASE_CONNECTIVITY_NOT_READY`；受理后池耗尽、绑定暂时失败保持原 ID 和操作，提供具体 reason。已分配地址但请求结果未知时不能改名/切池再次申请。

### 4.3 删除与创建失败终止

删除准入先阻止用户所有的 Subnet/Attachment、Public SNAT、LB 等占用；基础 Intranet 子资源是本 VPC 的系统子资源，不能成为永远无法满足的“必须先由租户解绑”条件。

进入删除后封闭新 Subnet/LB/Public 绑定受理，持久清理顺序：基础 Snat 删除并确认 → EIP boundResource 清空 → 释放基础 EIP并确认不存在 → 删除 VPC CR并确认不存在 → VPC deleted。任何步骤未知/失败保留占用和待执行工作。Public EIP 是独立租户资源，不因 VPC 删除级联释放。

允许用户终止 failed/blocked/provisioning 的 VPC 创建，并复用此删除编排；终止意图与创建结果落库受同一资源锁和 fence 保护。迟到创建结果仍需识别并清理，不能释放 pending-create 身份后宣告无残留。

清理必须区分步骤发送状态：有持久证据确认从未发送 Provider POST 的基础子资源，可直接取消该步骤并退休其意图，不要求先取得不存在的父/子 UID；已发送、待响应或发送状态不明的步骤保留身份并核验清理。测试须覆盖“VPC 尚未产生任何 CR 即终止”，不能机械调用要求父 UID 的删除 adapter 导致永久阻塞。

## 5. Public EIP 与 VPC 公网出站调整

保留已有“申请 EIP → 分配 → 绑定 Public SNAT → 停用/启用 → 解绑 → 可选释放 EIP”的用户操作顺序。以下是必须修改的部分：

- 原租户 EIP 查询/申请保持 Public 语义；基础 Intranet 地址使用系统用例，不允许租户伪造 scope 或 managed_by。
- 原按 VPC GetSnat/Bind/Toggle/Unbind API 明确只操作 **Public** SNAT，查询与唯一性都加用途，不返回任意一条未删除的 SNAT。
- 绑定事务同时预留 `(tenant, vpc, public)` 位置和 EIP 的唯一目标位置，LB 与 SNAT 必须走同一套地址占用事务。
- 绑定/启用要求 VPC 基础连接已就绪且观察新鲜、Public 池通过其自身出口能力检查；不能因内网验证通过就绕过公网准入。
- 停用保留 Public EIP 和绑定占用；解绑确认 Snat 消失且 EIP 无绑定后才释放 claim；释放 EIP 要同时检查产品和 Provider 的 SNAT/LB/外来绑定。
- EIP 响应增加 `binding_target: {kind, id, state}`，区分 `vpc_snat` 与 `load_balancer`。旧 `binding_id` 保持“SNAT ID”语义：SNAT 目标时返回其 ID，LB 目标时为空；旧 `binding_state` 从统一 claim 投影为 unbound/reserved/bound，LB 占用时绝不能返回 unbound。新客户端必须根据 binding_state/target 判断占用，不能根据空旧 ID 判断地址可用；完成相关客户端适配是开放 LB 绑定的门槛。字段移除另行版本升级。

同 VPC 内网与公网路由按实际 `intranetNetworks` 和 Public 默认出口分别生效。验收必须测试 Public 启停/解绑前后内网路径和私网 LB 不变；公网访问声明仍需独立真实请求和源地址证据。

## 6. 新增租户 LB 生命周期

### 6.1 资源和租户输入

LB 归属于一个租户/VPC/Subnet，包含 1～64 个 HTTP/HTTPS/TCP/UDP/SSL Listener，每个监听器拥有稳定 ID/name、端口、独立 Backend Members 和健康检查配置；HTTP/HTTPS 使用默认 `/` 前缀规则。一期不开放任意 Provider YAML。建议创建输入：

| 字段 | 规则 |
|---|---|
| name、description、idempotency_key | 沿用现有校验与持久重放规则 |
| vpc_id、subnet_id | 同 tenant/cluster/namespace，Subnet 属于 VPC，基础内网连接就绪 |
| exposure | `private` / `public` / `public_private`，创建后一期不可改 |
| flavor | 固定 small/medium/large；空值兼容 small，未知值拒绝，不能直接传 Class 名 |
| public_eip_id | public/public_private 必填，private 禁止；已分配且未占用的本租户 Public EIP |
| private_ip | private/public_private 必填，属于所选 Subnet 且非网关/保留地址；public 禁止 |
| listeners | `{items:[...]}`；1～64 个监听器，protocol 为 HTTP/HTTPS/TCP/UDP/SSL；name 遵守安装 CRD 的 253 字符 DNS 名称限制，name/port 不重复，端口范围 1–65535 |
| listener | 旧单监听器输入仍兼容，规范化为 `http`；不得与 listeners 同时提供 |
| backends | 普通监听器允许空集合，等待添加后端时仅保存数据库；HTTP 重定向监听器必须为空，其激活依赖固定 HTTPS 目标。下发规则见 [延迟下发 §2](lb-deferred-activation.md#2-用户配置与下发集合)。非空成员含 subnet_id、IPv4、port、weight，须验证归属本 VPC 的已分配业务地址，禁止直接指定平台/节点/跨租户地址；顶层字段仅用于旧输入 |
| health_check | TCP/HTTP；UDP 仅 TCP。port 缺失或 0 使用后端业务端口，非零 1–65535 可以独立于业务端口；HTTP 要求 path，可选 DNS hostname。默认 interval=5s、timeout=3s、unhealthy=3、healthy=1，时间与阈值分别为正数；panicThreshold=0 |
| algorithm | 每监听器 RoundRobin（旧请求默认）、LeastRequest、SourceIP ConsistentHash，详情返回保存的实际算法 |
| certificate_ref | HTTPS/SSL 必填 TLS Secret name，可选 namespace 必须等于资源绑定 namespace；Secret 需具有相同租户标签。只引用，不签发或上传 |
| session_persistence | HTTP/HTTPS 可停用、COOKIE_TIMEOUT（正 cookie_expiry_seconds）、COOKIE_SESSION；关闭删除旧字段 |
| max_connections | 可选正数，0/省略关闭；HTTP/HTTPS/TCP/SSL 映射 CTP；UDP 不接受该字段 |
| gzip_enabled | HTTP/HTTPS 开启 Gzip；false 清除 compressor |
| https_redirect_status_code | HTTP 的 301/302，0 关闭；与 backends 互斥，HTTPS 目标优先 443，否则必须唯一，重定向包含目标端口 |

Backend 地址的归属校验结合已持久化 Attachment 与 Provider VNicIP/UID 事实，不仅用 CIDR 判断业务归属。Network 不创建或删除业务 Pod/VM；实例 owner 返回的接入信息用于地址确认。后端删除/地址变化触发成员退化与配置更新，固定 IP 成员不会静默转发到复用同一 IP 的另一身份。需要未纳管静态后端时另行定义管理员准入，不默认为任意 IP 放行。

持续观察校验 Attachment 时，以读取该记录后的数据库时间判断新鲜度。LB 领取任务后、等待 Provider 审计期间完成的正常 Attachment 续报不能被误判为未来时间；实际未来、过期或缺失的观察仍拒绝。此时效判断不替代 Pod/VNic/VNicIP 的 UID、owner 链、Subnet/namespace 身份与审计 fence 校验。

一期指定私网 VIP，由 kc IPAM 做最终冲突与预留。Network 先在本 VPC 范围保留地址意图，防止自身并发重复申请；不以自己的表替代 kc 对 Pod/VNicIP 等全部分配的权威。自动 VIP 分配不是本轮前置，可在确认 kc 契约后独立增加。

### 6.2 Provider 映射

| LB exposure | Class 映射（small 示例） | lb_vip_address | lb_eips | 数据面 Service |
|---|---|---|---|---|
| private | lb-small-noeip | 指定 VIP | 不填 | ClusterIP |
| public | lb-small | disable | Public EIP CR短名 | LoadBalancer |
| public_private | lb-small | 指定 VIP | Public EIP CR短名 | LoadBalancer |

固定规格、安装能力检查与数据面观察共用同一份常量：

| flavor | 副本 | 每副本 requests | 每副本 limits | public/public_private Class | private Class |
| --- | ---: | --- | --- | --- | --- |
| small | 2 | 1 CPU / 1Gi | 2 CPU / 2Gi | lb-small | lb-small-noeip |
| medium | 4 | 2 CPU / 2Gi | 4 CPU / 4Gi | lb-medium | lb-medium-noeip |
| large | 6 | 4 CPU / 4Gi | 8 CPU / 8Gi | lb-large | lb-large-noeip |

Gateway `spec.infrastructure.annotations` 的 `lb_vpc`/`subnet` 使用 namespace/name。Gateway、各协议 Route、Backend、BackendTrafficPolicy 及所需 ClientTrafficPolicy/EnvoyPatchPolicy 放在固定租户 namespace；GatewayNamespace 模式由 Envoy Gateway 在同 namespace 生成 Service/Deployment/Pod。Network 只写自己拥有的 Gateway/Route/Backend/Policy，不直接成为生成 Service/Deployment 的第二个 spec 写入者。

监听参数随配置版本持久化，旧行参数默认为空对象，旧回执与 fingerprint 不重写。健康端口缺失/0 时省略 `overrides.port`，非零独立端口显式写入；不再要求与后端端口相同或 timeout 小于 interval。

| 协议/参数 | 生成对象及字段 |
|---|---|
| HTTP / HTTPS | Gateway HTTP / HTTPS（TLS Terminate）+ HTTPRoute |
| TCP / UDP / SSL | Gateway TCP / UDP / TLS（TLS Terminate）+ TCPRoute / UDPRoute / TCPRoute |
| algorithm | BackendTrafficPolicy `loadBalancer`；SourceIP 使用 `ConsistentHash` + `consistentHash.type=SourceIP` |
| Cookie | HTTPRoute `rules[].sessionPersistence`，Permanent + absoluteTimeout 或 Session |
| 健康检查 | 非 UDP：BackendTrafficPolicy `healthCheck.active`；UDP：EnvoyPatchPolicy TCP health_checks、可选 endpoint 检查端口及 panic=0 |
| 最大连接数 | ClientTrafficPolicy 指向 Gateway 与 listener sectionName，`connection.connectionLimit.value` |
| Gzip / HTTPS 重定向 | BackendTrafficPolicy compressor / HTTPRoute RequestRedirect，无 backendRefs |

HTTPRoute rule 名固定 `default-rule`，HTTP BackendTrafficPolicy 的 sectionName 指向该 rule；Gateway parentRef 与 ClientTrafficPolicy 的 sectionName 指向 listener name。TCP/UDP policy 不填写不存在的 Route rule sectionName。每个 listener 继续拥有独立 Route/Policy 组件，协议切换与关闭参数只清理对应自有组件。生成 Service/Deployment 的归属仍通过 Gateway UID/owner 链核对，不直接写这些对象。

最大连接数是 Envoy 每 Pod 的限制，不是全局协调额度。ClientTrafficPolicy 只写所需 connectionLimit，保留平台已有可信代理/clientIPDetection 以及 HTTP2/WebSocket 默认行为。TLS 引用需要资源绑定 namespace 内对选定 Secret 的 `get` 权限，可通过带 resourceNames 的 Role/RoleBinding 授予，不扩大为全局 Secret list/watch。UDP 健康策略要求底层开启 `extensionApis.enableEnvoyPatchPolicy`；缺失时保留失败/未就绪证据，由底层处理，应用不更改安装配置。


本次 Fedora 具名验证：`TestLBMultipleListenersMainFlow`、`TestLBListenerParametersMainFlow`，以及两仓 `TestNetworkJointHTTP/multiple_listeners` 均通过。测试使用真实 admission、PostgreSQL、Worker、RPC/HTTP/auth；Kubernetes/控制器边界为 fixture。未运行全量测试。真实集群/数据面验收仍为 `not_verified`，不以 fixture Accepted/Programmed 代替。

### 6.3 创建事务和执行顺序

同一事务锁定父 VPC/Subnet、必要的 EIP，确认均未删除/封闭，保存 LB 及其配置版本、稳定成员/Listener ID、operation、Provider bindings、EIP claim 和私网地址意图。LB 一旦受理即占用父资源，不能等 Gateway Ready 后才补占用。

Backend Member 引用同VPC内其他Subnet时，也登记该Subnet的网络引用占用；删除该Subnet前先移除相关成员或LB并确认配置释放，不能只保护LB入口所在Subnet。成员关系不取得业务实例生命周期所有权，实例owner仍可删除业务实例，Network观察后标记成员不可用。

worker 按稳定依赖推进 Backend → Gateway → 各协议 Route/Policy，观察控制器生成的数据面和 kc VIP/EIP 绑定。独立步骤重试不能重新申请另一个 Public EIP或另一个 VIP，不能留下未登记 CR。

LB 配置完成需：自有 CR 身份与期望一致，Gateway Accepted/Programmed，Route Accepted/ResolvedRefs，Policy Accepted且代次匹配；数据面存在并关联正确；Service 类型、端口、EndpointSlice、VIP分配或Public EIP绑定符合 exposure；EIP claim 目标和 Provider实际 Service一致；父基础连接和观测新鲜。原附件关闭数据面探针，Running 不证明监听器真正加载，因此响应还需分别记录 `configuration_state`、`data_plane_state`、`observed_at`，无实际健康来源时 data_plane_state=unknown。

产品创建 operation 成功表达“配置已完成”；Console 文案显示“已配置”，不得将无健康证据的情况显示成“流量健康”。部署/功能验收必须额外从正确来源实际访问各入口并断言业务响应。后续具备受控主动探测/Envoy运行观测源时可提高 data_plane_state 的证据等级，但不默认开放 Envoy admin 接口或把日志无报错作为证明。

### 6.4 更新与删除

本批允许更新名称/描述、增删监听器、修改监听端口和各监听器的后端/健康检查；沿用 expected_version + 幂等键、desired/applied 配置版本和同一 LB 的 lease。新更新使用 `{data:{...},update_mask:...}`；监听器集合遗漏时保持原值，提供时整体替换，空集合拒绝。保留的监听器 ID/name 稳定；协议及适用参数可以更新，VPC、Subnet、exposure、Public EIP、VIP、flavor 保持不可变。更新部分完成/响应未知不得抹掉旧 applied 状态，退化和失败原因可查询。

监听器变更使用 Gateway UID/resourceVersion CAS，只允许 listener 集合和对应版本标记变化；Class、placement、VIP/EIP 等不可变条件继续核对。新增先准备后端，再更新 Gateway、Route/Policy 并确认观察。移除先撤转发和 Route/Policy，确认 Gateway/Service 对应端口已撤除后，才能释放不再被任何监听器引用的 Backend 和子网占用。不能删除整个 Gateway 来删除一个监听器。

全部目标监听器、Route/Policy 和完整 Service/EndpointSlice 端口集合均确认后才推进 applied_version。Service 的 targetPort 可以不同于监听端口，按实际 Envoy Pod 端口解析并核对 EndpointSlice。单个后端身份失效仅撤除引用它的监听器转发；部分应用保持旧 applied_version。追加迁移 0009/0010 保留旧 listener/component ID、CR 名称/UID、旧 fingerprint 和受理回执，关系继续使用 tenant/placement 复合外键。

删除受理后封闭新更新，先撤除 Route/Policy，再删除 Gateway并观察其生成 Service/Deployment/Pod/EndpointSlice 释放，最后清理自有 Backend。仅在实际 Service消失、Public EIP解绑、VIP kc预留释放已确认后，释放 EIP claim、VIP意图和父资源占用并标记 deleted。Public EIP 保留供租户继续使用；基础 Intranet SNAT和业务后端都保留。

删除期间重启 worker、Provider 迟到响应和 finalizer 卡住均进入持久恢复；不强删 finalizer、不清数据库伪造完成。仅自有 Backend由LB删除，业务实例通过既有 owner生命周期间接影响成员状态。

## 7. 持久化、并发与观察

### 7.1 目标数据变化

这是目标模型，具体 SQL在任务实现时落地；历史 `0005` 不修改，使用新 migration。

| 结构 | 必须表达的规则 |
|---|---|
| 平台地址池 | 在现有 Public 池基础上演进为有 `scope=intranet/public` 的地址池，分别默认池；保持旧 ID、版本、Public字段及权限语义；按 scope 检查不同网关/模式字段 |
| EIP | scope、managed_by=system/tenant、固定池/版本、可选 system_owner_vpc；scope与池匹配；system资源不可进入租户Public操作面 |
| SNAT binding | purpose=intranet/public，唯一 `(tenant_id,vpc_id,purpose)`（未deleted）；EIP scope与purpose匹配，保留启停/占用 |
| EIP claim | 全部 SNAT/LB 共用；未释放 `(tenant_id,eip_id)` 唯一，target_kind与snat_id/lb_id严格一一对应；已存在Public绑定迁移为claim |
| VPC 基础连接 | 固定子资源ID、池版本、依赖步骤、desired/applied状态、version、reason、观察时效与终止意图 |
| LB、Listener、Backend Member、配置版本 | tenant/vpc/subnet归属、稳定身份、期望配置和applied版本；版本化 Listener 集合及独立成员关系；不把任意YAML当权威产品记录 |
| VIP意图与父占用 | `(tenant_id,cluster_id,vpc_id,address)` 未释放唯一；Subnet/VPC删除与受理使用共同锁/占用判断；kc依然负责最终地址分配 |

所有租户关系继续显式 tenant_id、租户限定查询与保持 tenant/cluster/namespace 的复合 FK。EIP claim 不能仅保存无法建立外键的自由字符串 target；可用可空 typedFK 加 num_nonnulls=1、target_kind校验。平台配置不用伪造租户。

固定锁顺序以现有 VPC→Subnet 为基础扩展：父 VPC → 相关 Subnet → 按 ID 排序的 EIP → 绑定/claim/LB → operation；无父VPC的EIP申请使用池自身事务，不在持有EIP锁时反向等待VPC。纯读查询和Informer回调不持锁执行Kubernetes操作。不同绑定目标竞争同一EIP由数据库唯一性兜底，错误映射为稳定的EIP_IN_USE。

### 7.2 观察与恢复

沿用共享Informer、关系索引、PostgreSQL持久唤醒与定期核验；新增依赖：Intranet池/EIP/Snat→基础连接→VPC→LB，LB Gateway/Route/Backend/Policy→LB，生成Service/Deployment/EndpointSlice→LB与EIP claim。仅增加GVR不构成生命周期闭环。

合法预期 LB Service占用应由新adapter核对并接受；其他Service/Nat/Snat仍是冲突。旧逻辑“看到任何Service引用EIP都冲突”必须替换。删除事件、UID替换、LIST/WATCH过期、重启relist和无CR的待创建意图均纳入恢复。

无论SNAT还是LB，未知Provider创建/删除结果保留claim、地址意图、父占用和pending_action。不得超时释放后让另一目标抢占，不得由每个worker建立独立的“私有EIP占用表”。

## 8. 存量升级与契约接入

### 8.1 已有记录和旧接口

数据迁移将已知从Public池创建的EIP/SNAT标为public/tenant-managed，保留资源ID、Provider UID、历史operation、幂等指纹和已返回快照；不能按“VPC唯一SNAT”猜用途。发现不符合旧约束/来源不明数据输出冲突清单，由独立处置解决，不能静默收养外部CR。

租户原Public EIP/SNAT API语义保持；增加绑定目标类型和VPC基础连接摘要。平台新增Intranet池/默认池管理。LB新增 Create/Get/List/Update/Delete 和operation查询，公开租户body不允许覆盖tenant/namespace/Provider字段。

### 8.2 既有 VPC 补齐

数据库升级不自动批量写集群。先生成可审核的VPC补齐清单和固定默认Intranet池版本，再通过独立的系统 `ensure_vpc_base_connectivity` 持久操作逐个推进，幂等、限速、可暂停和恢复；不复用或重写历史create_vpc成功operation。

旧VPC在补齐前返回基础连接 missing/unknown，旧同VPC工作负载保持；不因数据库加字段删除、重建或自动停用旧资源。补齐开关分别控制新建VPC路径和旧VPC的完成状态语义切换：新VPC从启用起使用聚合条件，旧VPC仍保留旧状态并显式显示base summary；新增LB/Public绑定以基础连接就绪为准。全部存量通过补齐及核验后，再启用统一聚合退化规则。这个有期限的升级阶段必须在执行状态中记账，不能形成长期双权威。

删除/补齐/新绑定竞争通过同VPC封闭标记与锁串行；默认池切换不改已受理补齐意图。补齐失败保留已分配地址与步骤，用户可按既有删除流程终止。

### 8.3 ANI 接入与界面

Network 定义并生成契约，ANI Gateway以可信租户上下文调用；不恢复Core的网络DB/worker/Provider写入。界面在VPC创建页不增加基础EIP输入，VPC详情显示基础连接摘要；公网EIP列表只显示tenant-managed Public地址及SNAT/LB绑定目标；绑定弹窗区分“VPC出站”和“LB入口”，过滤已有claim的EIP；LB表单按exposure显示VIP/EIP字段，提交后展示operation、配置状态和健康证据。

可信身份适配沿用既有服务边界，不能把未验证header当管理员权限。当前身份/配额/Core大规模重构各有独立任务，本方案只接入已有可用授权能力；未完成的部分在集成任务明确标记，不以临时dev身份冒充上线验收。

## 9. 验收要求

| 编号 | 必须证明的行为 |
|---|---|
| U-V01 | 无默认Intranet池拒绝新建；有池时VPC→EIP→Snat完成且不会available循环等待 |
| U-V02 | 三步各处崩溃/超时/迟到成功恢复，地址不重复分配、claim不提前释放；VPC尚未产生任何CR即终止可完成 |
| U-V03 | 同VPC可同时有一个Intranet和一个Public SNAT；第二个同用途拒绝；猜system EIP ID不能操作 |
| U-V04 | 同一EIP并发绑定SNAT/LB，仅一个受理；跨租户/同名异UID/错误namespace均拒绝 |
| U-V05 | Public申请/启停/解绑/释放期间Intranet身份和内网流量不变；Public真实出站和源地址单独验证 |
| U-V06 | private/public/public_private真实入口请求到两个后端；Public从VPC外请求，不能仅NodePort/port-forward |
| U-V07 | LB生成资源正确归属，Service占用被合法识别；外来Service占用仍阻止绑定/释放 |
| U-V08 | LB更新版本与部分失败可恢复；业务后端消失或IP身份变化不静默转发错误目标 |
| U-V09 | 删除LB释放EIP绑定但保留EIP；删除VPC自动清理基础资源，用户资源依赖仍阻止删除 |
| U-V10 | 旧Public记录和幂等重放不漂移；补齐不中断旧业务、不会收养手工CR、与删除竞争安全 |
| U-V11 | Informer重启/断流/过期/UID替换触发正确退化与恢复，Get/List不承担修复 |
| U-V12 | API/Console正确区分配置完成与流量健康；缺少证据显示unknown，三类型表单校验正确 |

Network构建/真实PG/恢复/契约/观察门禁在ubuntu执行；环境和源码manifest固定，按包限制资源。真实数据面在合格kc/Envoy版本与明确拓扑下，经产品API创建资源后验证；故障只作用本run对象，保留既有手工现场，测试清理走产品生命周期。生产发布、迁移执行和真实上游配置不是编写方案的隐含授权。

## 列表总数

负载均衡列表返回 `items,next_cursor,total`。`total` 统计当前租户、name/state/vpc/subnet/exposure 筛选下已受理的产品资源，不包含尚未受理的占位记录，不应用 cursor/limit；总数与本页数据来自同一只读数据库快照。跨页不承诺总数固定。
