# Image API 规格

契约起稿在 [Resource Proto](../../api/image/v1/image.proto)、[内部运行 Proto](../../api/image/v1/runtime.proto)、[公开 DTO](https://github.com/zhangzhe-ctrl/ani-governance/blob/d64d6ee478801795afadb4573ba25f8c2de6b7fc/api/protos/catalog/service/v1/image.proto)、[HTTP 绑定](https://github.com/zhangzhe-ctrl/ani-governance/blob/d64d6ee478801795afadb4573ba25f8c2de6b7fc/api/protos/admin/service/v1/i_image.proto)。实际源码是唯一契约；生成和验证状态仅见[执行账本](../execution/status.md#image-mvp)，不维护第二份副本。

## 1. 通用规则

Resource 只提供 gRPC。浏览器 REST 由 Governance 实现，不增加 Image HTTP BFF。前端 DTO **没有 tenant_id、Harbor project_id、namespace、role、robot permission、upstream URL、management secret**。请求里添加身份字段/重复身份头不得改变作用域，按现有严格入口风格拒绝不支持字段。

公开 scope 为字符串 `tenant` 或 `platform`，必须明确传值；purposes 为 `container/development/inference/finetuning/training` 去重数组；accelerator 为 `undeclared/none/nvidia/amd/ascend/other`。Resource 使用对应枚举，0 scope/purpose 拒绝；0 accelerator 表示未声明，列表 0 表示不筛选。平台硬件选择只是声明。查询不提供任意 tenant、scope=all 或跨空间游标。

协议字段下表用 Proto snake_case；实际 JSON 字段命名遵循 Governance 现有编码器和生成 OpenAPI，不另改全站序列化配置；IMG-06 用真实 HTTP 测试冻结编码行为。版本 int64 在 JSON 遵从现有 Proto 编码约定。

请求 tenant_id 由 Governance 解析并与受信 metadata 同值；用户 actor 由 Principal.Actor() 构造，不能来自 body。Image 新客户端必须用 NewOutgoingContext 重建 metadata，不复制/追加外部头。Resource 启用精确 RPC 白名单并生成 ImageCaller；无 caller 的 biz 用例拒绝。

平台 CLI 不走下面租户方法。运行内部接口不进入公开 OpenAPI/Swagger，也不允许浏览器 JWT 直接调用。

## 2. HTTP—RPC—权限

| HTTP | TenantImageService 方法 | 权限 |
|---|---|---|
| POST `/api/v1/images/space:enable` | EnsureImageSpace | image:space:enable |
| GET `/api/v1/images/space` | GetImageSpace | image:space:get |
| GET `/api/v1/images/publisher-credential` | GetPublisherCredential | image:credential:get |
| POST `/api/v1/images/publisher-credential:issue` | IssuePublisherCredential | image:credential:issue |
| POST `/api/v1/images/publisher-credential:reset` | ResetPublisherCredential | image:credential:reset |
| POST `/api/v1/images/publisher-credential:disable` | DisablePublisherCredential | image:credential:disable |
| POST `/api/v1/images/registrations` | RegisterImage | image:registration:create |
| GET `/api/v1/images/registrations/{image_id}` | GetImage | image:registration:get |
| GET `/api/v1/images/registrations` | ListImages | image:registration:list |
| PATCH `/api/v1/images/registrations/{image_id}` | UpdateImage | image:registration:update |
| POST `/api/v1/images/registrations/{image_id}:unregister` | UnregisterImage | image:registration:unregister |

沿用当前模块订阅/权限框架，建议追加 IMAGE=14，先检查实际枚举是否冲突；不要改现有编号，也不要借 NETWORK 订阅冒充 Image。普通成员默认只读；空间和凭证权限只授予租户管理员/指定发布者。是否授权某个 AK 使用敏感签发方法走现有 permission policy，不自动给全部 AK 开权。

## 3. 请求及返回语义（精确字段见 Proto）

### EnsureImageSpace

输入 `slug` 与 `idempotency_key`。slug 3～40 字符，正则 `^[a-z][a-z0-9-]{1,38}[a-z0-9]$`，禁保留词，Project=`t-<slug>`。幂等键 8～128 ASCII 字符，允许 `[A-Za-z0-9._:-]`。每租户只有一个空间；同 slug 已 available 返回当前空间，不生成新 Robot；同租户不同 slug 返回 SPACE_NAME_IMMUTABLE；并发同名租户冲突返回 SPACE_NAME_CONFLICT。

返回 `space_id/registry_authority/project_name/state/reason/version/pull_credential_generation/created_at/updated_at`。Secret 不在此返回。available 表示 Project+运行身份就绪，不等于 Namespace/Pod 已创建。暂态返回 Unavailable 并保留 durable command；客户端同键重试，不靠 202+后台任务假装实现异步。

Project POST 的完整确定拒绝响应（Harbor 400/401/403）保留原业务错误，并持久化同一 command 的 `retryable/project_rejected`，修正原因后可以原 key 重试。重试仍先读取已完成结果与 Project 绑定，且不认领未知同名 Project。超时、断链、5xx、409、成功回执/ID 丢失或 `project_sent` 未绑定均不构成安全重发依据，即使单次查询返回404也不得重复 POST；继续使用受控归属恢复。历史 blocked command 没有确定拒绝凭据时，不根据当前404推断过去没有创建。

### GetImageSpace / GetPublisherCredential

Governance 的空间开通及 GET 详情直接返回 ImageSpace，凭证停用及 GET 详情直接返回 PublisherCredential 的公开字段，不包裹 `space` 或 `credential`。签发/重置保留 `credential + secret + replay_until`。内部 Resource gRPC 响应保留；HTTP 绑定与实际验收见 [Governance 接口登记](https://github.com/zhangzhe-ctrl/ani-governance/blob/25a171cc676e8dc5f2a614da08f64c0dc5daff06/docs/interface-integration-register.md#2026-10-09租户-network-补齐与响应合同)。

无用户可指定身份。前者未启用返回 SPACE_NOT_FOUND。后者空间存在但未签发时返回 `state=not_issued, generation=0, version=0, username=""`，不隐式签发；已签发返回元数据，**永不带 secret**。

### Issue / Reset / Disable

都有幂等键和 expected_version。Issue 首次版本为0；停用后重发使用当前 metadata version；已 active 不允许 Issue 暗中换密钥，返回 CREDENTIAL_ALREADY_ACTIVE。Reset 只对已存在代次使用，expected_version 必须>0；包括从过期状态恢复。Disable 对有效身份停用，重复同键返回原结果；已停用的新键返回当前状态，无外部多余写。

空间已开通但 publisher 尚未签发时，Disable（expected_version=0）返回 `CREDENTIAL_NOT_ISSUED` / FailedPrecondition / HTTP409，不预留或完成成功停用 command，不返回 `not_issued` 成功响应。新请求仍先执行版本 CAS；已有 command 的 actor/kind/指纹及成功结果重放检查先于当前凭证状态检查，后续重新签发不会遮蔽已完成停用的原结果。

Issue/Reset 返回 `credential + secret + replay_until`。secret 只允许成功交付/受限10分钟重放，不在普通查询恢复。metadata version 每次状态写变化，generation 只在新 Robot 代次激活时增加。客户端版本冲突409，不自动覆盖。新的 Secret 不能使用登录密码/AKSK 代替。

所有凭证相关响应设置 `Cache-Control: no-store`、`Pragma: no-cache`，不记录 request/response body、不放 URL/浏览器持久存储。Secret 用 password-stdin 或交互登录，不在命令行 -p 参数、shell history、截图或证据 JSON 中出现。

### RegisterImage

字段 `image_reference/display_name/description/purposes/accelerator/idempotency_key`。image_reference 最长512，必须显式 tag 或 sha256 digest，registry authority 必须等于固定值，project 必须当前租户，Repository 首期一个段且满足 `^[a-z0-9]+(?:[._-][a-z0-9]+)*$`、长度<=128。Tag 符合 `[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}`；Digest 固定 `sha256:`+64小写hex。拒绝带 scheme/userinfo/空格/控制字符/编码斜线/.. 段/多个 @/缺引用的输入。

display_name 1～100 Unicode字符；description 0～2000字符；purposes 1～5个，去重排序后保存；accelerator 未声明可接受。查询镜像并固定 Digest，平台事实由 provider 返回，不能靠用户填写名称猜架构。用户不能通过 scopes/标签登记 platform 写资产。

返回 ImageRegistration：id形如 `img_`+32小写hex，固定 repository/source_reference/digest/resolved_reference，平台数组、用途声明和元数据。有效同一空间同repo同digest重复登记返回 IMAGE_ALREADY_REGISTERED；不会悄悄修改已有登记的说明。

### GetImage / ListImages

Governance 的登记、更新、取消登记及 GET 镜像登记详情直接返回 ImageRegistration 的公开字段，不包裹 `image`；列表和签发/重置的多字段响应保留原结构。

必须提供 scope；Get 给 image_id；List 给 search(<=100字符)、purposes(匹配任一所选用途)、accelerator(undeclared/空不筛)、limit(默认20，1～100)、cursor(<=4096字节)。默认只列有效登记。Get 可返回自己已取消登记的记录，便于显示历史状态；Resolve 必须拒绝取消登记。

search 只匹配 display_name/repository，不开放任意 SQL/JSON filter。同scope排序 `created_at DESC,image_id DESC`，基于签名 keyset cursor；cursor绑定tenant、scope、所有筛选、limit，不能跨租户/筛选重用。不返回“全库真实存储量/仓库总数”之类未实现指标。Image API 空列表不代表 Harbor 仓库为空。列表返回 `items,next_cursor,total`；`total` 是当前 scope、租户、search/purposes/accelerator 条件下未取消登记的镜像登记条数，不应用 cursor/limit。总数、本页登记和镜像空间来自同一只读数据库快照；跨页允许总数随登记变化。

### UpdateImage

仅替换 display_name/description/purposes/accelerator 四类元数据；这不是任意 FieldMask。带 image_id/expected_version/idempotency_key；同tenant active记录 + versionCAS。source_reference/digest/platforms/owner/repository不能变；要换版本就 Register。跨租户/平台ID通过租户写路径一律 NotFound。取消记录不得更新。

### UnregisterImage

带 image_id/expected_version/idempotency_key，写 unregistered_at、version+1。重复已取消记录返回当前 tombstone，不能复活；按同键重放原结果。只取消目录登记，不调用 Harbor DELETE，不删除 layer，不撤销该租户对 Harbor repo 的 Pull。已经运行的 Pod 或已保存的业务镜像意图不因取消登记改变。

## 4. 内部 ImageRuntimeService

| 方法 | 输入 | 返回/限制 |
|---|---|---|
| ResolveImageForWorkload | 可信创建方断言的 tenant_id、scope、image_id、target_platform | 固定完整引用、根digest、登记version、匹配平台；无Secret；租户/平台只读分开查询，取消登记拒绝；Harbor当前依赖异常503 |
| GetTenantPullMaterial | 可信tenant_id | 该租户registry/username/secret/generation/expiry；空间须可用；仅受信创建方可调用，不接受任意 namespace/upstream/perms |

同进程优先窄 Go 接口；仅真实独立创建方存在时注册专用内部mTLS listener。证书必须是 IMG-00 绑定的唯一身份、方法白名单仅这两项；不复用 Governance 人工 actor，不接受用户头。调用方须验证业务意图属于tenant、写入自己的租户namespace，不允许转发任意用户提供的tenant_id。

独立入口通过 `ANI_IMAGE_RUNTIME_ADDR/CA/CERT/KEY` 显式启用，默认不监听。
TLS 1.3 客户端证书只允许唯一 DNS SAN `ani-inference-service` 或 `ani-modeldev-service`，不能带额外 DNS、URI、IP、
email 或通配符身份；服务端证书沿用 `ani-network-service` DNS SAN。Inference 保持上表两方法权限；
ModelDev 仅允许 `GetTenantPullMaterial`，拒绝 `ResolveImageForWorkload` 与任何未登记方法。
该入口与 Governance listener 的方法、证书白名单分离，不注册 HTTP、reflection、health 或 streaming。
调用方必须先验证受信业务命令的身份、tenant、摘要及重放状态，且只写自己验证归属的租户 namespace；
ModelDev 只能为其已冻结租户环境预置拉取凭据，不能把该入口透传给用户。
两种调用方都用 `metadata.NewOutgoingContext` 新建且只发送各一项
`x-ani-tenant-id` 和 `x-ani-request-id`；两者都是规范小写、非零 UUID，tenant 必须等于请求体。
禁止传递 `x-ani-actor`、`x-ani-operator`、`x-ani-scope`。受信适配器构造
`RuntimeCaller{Subject: 证书的唯一受信 DNS SAN, TenantID: tenant}`，`Actor` 始终为空。
租户目录方法不能交付 pull material，Governance 身份不能访问运行入口；Instance 保留原同进程窄接口。

## 5. 错误码

所有 Resource 错误使用 gRPC status + `google.rpc.ErrorInfo{domain:"image.ani.io",reason:<code>}`，message不含密码/DSN/Harbor内部敏感URL。

| reason示例 | gRPC | HTTP |
|---|---|---|
| INVALID_ARGUMENT / INVALID_REFERENCE / INVALID_CURSOR | InvalidArgument | 400 |
| TRUSTED_CALLER_REQUIRED | Unauthenticated | 401 |
| PERMISSION_DENIED / IMAGE_PROJECT_DENIED | PermissionDenied | 403 |
| IMAGE_NOT_FOUND / SPACE_NOT_FOUND | NotFound | 404 |
| IMAGE_ALREADY_REGISTERED / SPACE_NAME_CONFLICT / IDEMPOTENCY_CONFLICT | AlreadyExists | 409 |
| VERSION_CONFLICT | Aborted | 409 |
| SPACE_NOT_READY / CREDENTIAL_ALREADY_ACTIVE / CREDENTIAL_NOT_ISSUED / CREDENTIAL_DELIVERY_EXPIRED / UNSUPPORTED_ARTIFACT / PLATFORM_MISMATCH / SPACE_OWNERSHIP_UNCONFIRMED | FailedPrecondition | 409 |
| DEPENDENCY_UNAVAILABLE / REQUEST_IN_PROGRESS | Unavailable | 503 |
| DEADLINE_EXCEEDED | DeadlineExceeded | 504 |
| INTERNAL_ERROR | Internal | 500 |

上游断开引发 deadline可按当前客户端风格映射依赖503；已建立连接的慢调用保留504。不要把 Harbor403伪装成镜像不存在；先通过业务归属判断，不泄露他租户制品。

## 6. 幂等

唯一键 `(space_id,idempotency_key)`，command包括kind、actor、规范化请求SHA256。相同键不同kind/actor/参数返回409；不得仅匹配key就回放别人的签发结果。所有DB写与对应command最终结果在同一个短事务提交。

登记/更新/取消：先查已完成command再发Harbor读取或版本检查，重放已提交快照，不再应用副作用；新的key是新的操作。签发Secret重放还校验当前有效generation和expiry/window。外部副作用command可以pending/retryable；没有后台处理承诺，客户端在同请求上下文外重新发同key。

## 7. 受控平台命令

同一 `cmd/ani-resource-service` 二进制新增 `-image-admin=<action>`；输入JSON经stdin或0600文件，不从租户API传平台scope。actions：`init-platform`、`issue-platform-publisher`、`register-platform`、`update-platform`、`unregister-platform`、`inspect-space`、`recover-project`、`purge-expired-secrets`。不支持任意SQL/HTTP透传。

新命令只是同一模块的运维入口；不要新建常驻管理服务。操作者身份由受控运行配置注入并记录，不能假装它是Governance普通user。输出默认不含Secret；签发命令必须显式指定0600 Secret输出文件并禁止覆写任意路径。所有生产/共享恢复操作仍需当前授权。
