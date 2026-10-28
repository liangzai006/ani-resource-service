# Image 部署接线

本目录是 Image MVP 的配置说明，不是部署或 live 验收记录。当前结果只看
[执行状态](../../docs/execution/status.md#image-mvp)；真实写入先满足
[Fedora 手册的 live profile](../../docs/runbooks/image-mvp-fedora.md)。
现有 Network 配置、数据库、证书身份及迁移入口保持各自职责。

## Resource 服务

将 [image.example.yaml](image.example.yaml) 的 `image` 段追加到部署方已有的完整、
有效配置中。示例默认关闭；启用前替换所有占位符，使用运行身份可读的绝对路径。
`ANI_NETWORK_MODE=governance` 或 `full` 注册 Image 租户 RPC；vpc-read 模式不注册。
Governance 模式仍装配现有 Network worker，并需要原 Network 配置及 Provider 权限，
不能把仅有 `image` 段的文件当成完整服务配置。

Resource 入站继续使用 `ANI_NETWORK_CLIENT_CA`、`ANI_NETWORK_TLS_CERT`、
`ANI_NETWORK_TLS_KEY`。服务端证书的 DNS SAN 包含既有 `ani-network-service`，
受信 Governance 客户端证书包含 `ani-governance`；TLS 最低1.3。
这些服务间证书与 Harbor HTTPS CA 是不同的配置用途。
Image 仅增加固定11项租户操作；平台管理和运行拉取材料没有公共浏览器入口。

### Inference 运行入口

Inference 已作为独立工作负载 owner 接入时，在 Resource `governance/full` 部署显式设置：

```text
ANI_IMAGE_RUNTIME_ADDR=:19094
ANI_IMAGE_RUNTIME_CA=/run/ani-image-runtime/ca.pem
ANI_IMAGE_RUNTIME_CERT=/run/ani-image-runtime/tls.crt
ANI_IMAGE_RUNTIME_KEY=/run/ani-image-runtime/tls.key
```

地址为空默认不监听；仅配置 TLS 而没有地址、Image 未启用、证书缺失均拒绝启动，
不退回明文或 Governance 凭证。端口仅加入已有 Resource 内部 Service 和对应 NetworkPolicy，
允许 Inference 工作负载访问；不加入公网入口、Gov BFF 或浏览器 OpenAPI。
现有 Service selector、namespace、NetworkPolicy 标签需从实际部署读取，不能套用猜测值。

沿用部署的内部服务 CA 签发方式，给 Inference **单独**签发 clientAuth 证书，唯一 DNS SAN
为 `ani-inference-service`，不带 URI/IP/email/额外 DNS，不复用 `ani-governance` 客户端密钥。
Resource 端 `CERT/KEY` 为 serverAuth 证书，DNS SAN 包含 `ani-network-service`；Inference
客户端固定 serverName 为该值并校验 CA。TLS 只接受 1.3。证书通过既有 Kubernetes Secret
只读挂载到相应服务，不将私钥写 ConfigMap、源码、证据或命令行；沿用已有签发/轮换责任。

运行协议和受信 tenant metadata 见[内部合同](../../docs/specs/image-api.md#4-内部-imageruntimeservice)。
Resource 仍只用原 Image 数据库权限读取凭据，不新增 Secret 写入 RBAC。Inference 的已有
控制器身份是其共享 pull Secret 的唯一写入 owner，只在已授权 namespace 获得 Secrets 的
get/create/update 权限；普通 runtime SA 不增加 API 权限，Pod 显式引用本租户 Secret。
Instance 原 `ani-instance-image-pull` 及 `ani-resource-instance` owner 不受此入口接管。

API 使用根模块 `github.com/zhangzhe-ctrl/ani-resource-service` 的正式版本及其
`api/image/v1` 生成合同，当前两 RPC 不需要变更 protobuf。不得复制 DTO 或用本地 replace/go.work
替代发布。仓库 CI 对所有 push/PR 在 Ubuntu 运行全量 verify/integration/race；在用户禁止
全量及非 Fedora 重任务的本轮，push、PR 和新 tag 发布均停止，保留本地候选与 Fedora 定向证据，
不修改或绕过 CI。已存在正式基线版本可供消费者锁定。

镜像外部调用有界；配置 `server.grpc.timeout` 时需给空间/凭证写入留下足够时间，
例如45秒，Governance Image 客户端超时默认30秒、最多45秒。
超时后保留原幂等键与请求内容重试，不将超时视为未发生副作用。
Harbor 暂不可用不导致 Network 因 Image 启动探测而退出；Image 配置或数据库检查失败
会拒绝启动。启动时数据库检查通过也不代表持续 Harbor/数据面可用。

## 文件与数据库

- 数据库 DSN、Harbor 管理密码、AES-GCM keyring、游标签名密钥均通过独立私有文件注入。
  文件须为普通文件、非符号链接、其他用户不可读写，建议目录0700、文件0600；
  每文件最多64 KiB。不得把内容放到命令行、源码、ConfigMap或公开日志。
- Kubernetes Secret volume 的投射文件通常是符号链接，不能直接传给私有文件读取器。
  部署方须在启动前将所需文件复制到该 Pod 独占的受限目录，设置服务 UID 和0600，
  再使用普通文件路径；保留原 Secret 管理与轮换责任，不修改读取器来忽略检查。
- `harbor_ca_file` 是验证 Harbor 服务端的 PEM CA。`harbor_url` 必须为 HTTPS，
  当前同时提供管理 API 与镜像 registry authority；不接受任意 URL path 或关闭证书校验。
  `robot_name_prefix` 必须与实际 Harbor 配置一致，不根据测试前缀猜测生产配置。
- keyring JSON 的形状为 `{"active_key_id":"<id>","keys":{"<id>":"<base64 AES key>"}}`。
  保留解密已有密文所需的旧 key；游标文件为至少32字节随机密钥的严格 base64。
  安装 ID、registry authority 和空间绑定不可通过更换配置重认领。
- Image 使用单独 runtime DSN；运行角色只有 `image` 业务表 SELECT/INSERT/UPDATE、
  `image.schema_version` SELECT，无 owner/超级用户/BYPASSRLS/建库/建角色/跨域表权限。
  不复用 Network runtime DSN。表关系与约束见[数据契约](../../docs/specs/image-data.md)。

迁移使用同一二进制的 `-image-migrate`，通过 `ANI_IMAGE_MIGRATION_DSN_FILE`
和 `ANI_IMAGE_RUNTIME_ROLE` 明确 owner DSN 私有文件及待授权运行角色。
owner 只用于该独立迁移入口；原 `-migrate` 继续只处理 Network。
执行迁移前按实际数据库备份/恢复流程冻结输入；本说明不授权操作共享数据库。

## Governance 接线

设置 `ANI_IMAGE_ADDR`、`ANI_IMAGE_CA`、`ANI_IMAGE_CERT`、`ANI_IMAGE_KEY`，
可选 `ANI_IMAGE_TIMEOUT`。目标是上述受信 Resource listener；配置缺失不建立
明文或默认身份连接。客户端只使用固定版本的生成契约，复用既有租户 UUID 映射。

部署 Governance 后先按其已有 OpenAPI catalog 同步流程同步 Image 路由，再由管理员
按 Governance `sql/data/README.md` 执行 Image 权限 catalog seed。
catalog 不自动给角色、API Key 或套餐授权。按实际角色显式配置只读四项权限与
需要的发布权限，并给相应套餐启用 IMAGE 模块；浏览器按钮状态不替代服务端权限检查。
不重置现有账号密码或绕过现有 Cookie/JWT/AK 身份流程。

## 验证与回退边界

先验证关闭 Image 时原 Network 行为，再在隔离数据库上验证 Image 配置、schema、
受信证书和精确方法边界。真实 Harbor 的版本、CA、Project范围、测试租户、允许清理
对象须来自已批准 profile；看到可访问的 Harbor 或 Kubernetes 不构成写入授权。

关闭 Image 可停止新的 Image API 入站，不会删除 Harbor Project/Robot、登记数据或
调用方拥有的 Namespace/Secret。已创建工作负载保留其固定 Digest 意图；凭证及对象
清理由相应所有者按实际 ID/UID 和本 run 的 ownership 处理。没有自动全局清理或迁移回滚。

## 本机受控平台命令

同一二进制使用 `-image-admin=<action>`，不启动服务 listener/Network worker。
必须给 `-image-operator-config` 一个0600普通文件，其中固定 `subject`、`actor`、
`installation_id`、`secret_output_directory`。安装 ID 必须等于 `-conf` 中启用的
Image 安装 ID；平台 actor 不冒充 Governance 用户。
`secret_output_directory` 必须为绝对路径、非符号链接、其他用户不可访问的目录。
operator 文件只交给已授权的后台运维执行身份，不给浏览器或工作负载。

请求为严格 JSON，默认从 stdin 读取，或给 `-image-admin-input` 一个0600普通文件；
未知字段、重复顶层字段、额外 JSON、任意 tenant/scope/SQL/URL 字段均拒绝。
以下是各动作输入字段；所有幂等键8～128字符，同一次重试必须保持 actor/key/body。

| action | 输入 |
|---|---|
| `init-platform` | `idempotency_key`；Project 名只来自配置 |
| `issue-platform-publisher` | `idempotency_key`、`expected_version`、`rotate`（首次false；显式轮换true） |
| `register-platform` | `idempotency_key`、`image_reference`、`display_name`、`description`、`purposes`、`accelerator` |
| `update-platform` | `idempotency_key`、`image_id`、`expected_version`及上行四项可变元数据；没有镜像引用字段 |
| `unregister-platform` | `idempotency_key`、`image_id`、`expected_version` |
| `inspect-space` | `space_id`；只输出空间事实，不输出 Robot Secret |
| `recover-project` | `space_id`、`project_id`、`evidence_sha256`；先确认实际私有 Project ID/名称及所有权证据 |
| `purge-expired-secrets` | `{}`；只清除已成功且交付窗口到期的 command 密文 |

签发必须额外指定 `-image-secret-output`，其绝对路径必须位于上述输出目录且文件名
仅含字母数字、点、短横线或下划线，以字母数字开头。命令在外部调用前以0600和
exclusive-create 保留新文件，存在的文件或符号链接一律失败。普通 stdout 只含
动作、操作者、元数据；Secret 仅写到此文件的 `secret` 字段，用户名在
`credential.Username`。首次签发 `expected_version=0`；后续使用返回的实际
`CredentialInfo.Version`，不要把 generation 当 version。

例如，在批准的 Fedora run 中，对已准备的私有 operator/runtime 配置执行：

```bash
"$IMAGE_BINARY" -conf "$IMAGE_RUNTIME_CONFIG" \
  -image-operator-config "$IMAGE_OPERATOR_CONFIG" \
  -image-admin=init-platform -image-admin-input "$IMAGE_INIT_REQUEST"
"$IMAGE_BINARY" -conf "$IMAGE_RUNTIME_CONFIG" \
  -image-operator-config "$IMAGE_OPERATOR_CONFIG" \
  -image-admin=issue-platform-publisher -image-admin-input "$IMAGE_ISSUE_REQUEST" \
  -image-secret-output "$IMAGE_NEW_CREDENTIAL_FILE"
```

`init-platform` 不认领未知同名 Project；没有已落库绑定时会阻塞，必须经明确的
`recover-project` 和外部所有权证据恢复后再重试原请求。新建空间不签发发布身份。
同 key 的 Secret 交付重放最多10分钟，且当前有效代次/version必须仍匹配；已轮换或
已过期交付不会返回旧 Secret。文件写入失败时保留原请求并使用一个新文件名重放，
不要不检查现状就改 key。只有显式 `rotate=true` 才替换已有发布身份并停用旧 Robot。
取消登记不删除 Harbor 内容；本命令没有任意对象删除或 HTTP 透传能力。

## Harbor 技术验收驱动

`scripts/image-smoke` 执行 `init → push → register → resolve`，没有Pod创建或产品owner
替身。它使用真实平台CLI、Governance租户HTTP、skopeo registry客户端，以及独立的
`scripts/image-smoke-runtime` 只读技术helper；后者无listener，不注册公共Runtime RPC。
技术helper直接调用已实现的 `ResolveImageForWorkload/GetTenantPullMaterial`，显式使用
profile允许的技术身份，只输出固定Digest元数据，拉取认证材料只写新0600文件。
它证明的范围是技术链路，普通容器创建方身份认证、Namespace/Secret归属和运行结果仍须
由IMG-08/10实测。

[smoke.example.json](smoke.example.json) 扩展了live profile，默认不能执行。
只能由有权批准该环境的操作者填写真实引用并批准，不能将示例改为approved来绕过授权。
运行前准备如下输入，全部构建/工具检查/驱动操作仍只在Fedora本run预算scope和既有锁内：

- 从profile的完整Resource SHA干净副本构建两个二进制：
  `go build -trimpath -o <private-resource-binary> ./cmd/ani-resource-service` 和
  `go build -trimpath -o <private-runtime-binary> ./scripts/image-smoke-runtime`。
  `go version -m` 必须含相同 `vcs.revision` 及 `vcs.modified=false`。
  实际Governance部署SHA另填profile；驱动记录此批准绑定，**不声称HTTP已证明部署SHA**。
- `runtime_config` 是单独0600 **JSON** 文件（与既有Bootstrap相同snake_case字段，
  `request_timeout` 如 `"10s"`）。CLI和helper读取同一个文件；数据库是已迁移的独立
  Image runtime DSN。不得借此复用Network DSN或改变既有服务配置。
- `operator_config.secret_output_directory` 必须等于 `<work_directory>/private`。
  work_directory为批准的、尚不存在的绝对路径；驱动创建0700目录。重复运行同目录会失败，
  失败后必须先依据事件中的幂等键/ID核对实际状态，不用新key盲目重试或认领已有Project。
- run_id为8～32位小写字母数字/短横线；平台Project精确为 `<run_id>-platform`，
  两tenant的slug精确为 `<run_id>-a`、`<run_id>-b`，Project为 `t-<slug>`。
  `allowed_project_names` 仅含这三个名字；三者事前都必须不存在，两个测试tenant事前
  都未开通Image空间。平台写入现有Project不在本驱动范围。
- `user_credential_files` 按A/B顺序引用两份私有JWT文件，账号需通过既有登录方式取得，
  具备本次明确授予的Image权限/模块及可信tenant映射；驱动不修改密码或授权。
  `harbor.management_credential_file` 引用runtime的Harbor密码文件，只供后端与事前
  检查；skopeo仅接收签发的受限Robot认证文件。
- `file_sha256` 以绝对文件路径为键，固定runtime/operator配置、kubeconfig、Harbor密码、
  两JWT、Harbor/Governance CA、两个二进制、runtime DSN/keyring/cursor文件的实际SHA256。
  `tool_version_sha256` 是 `kubectl version --client=true -o json` 与 `skopeo --version`
  的原始stdout SHA256。工具须事先在本run按已核对版本准备，不自动全局安装。
- cluster需固定context、API、kubeconfig内嵌CA PEM字节SHA256及kube-system Namespace UID。
  驱动只读取该UID，不创建/修改Namespace；不接受insecure TLS或proxy-url。
  Harbor实际 `harbor_version`、HTTPS authority/CA、Governance HTTPS origin/CA均固定。
- base_image_digest指向已批准、匿名可读、最多32 MiB的单平台OCI/Docker schema2镜像。
  index/list在本驱动返回明确阻塞，不能悄悄挑选平台改变根Digest。预读根manifest核对
  SHA和各layer/config总大小后，用 `skopeo copy --preserve-digests` 直接Push三份；
  保留现有客户端签名策略及registry门禁，TLS验证显式开启。

运行方式：在已限资源且持有既有锁的Fedora执行上下文中执行
`./scripts/image-smoke --profile <private-profile> --workdir <approved-new-directory>`。
脚本有最长20分钟截止时间；超过预算或依赖错误即非零退出，不把HTTP 404/5xx/网络错误当成
Registry越权拒绝。A/B各自成功读后，才接受A读B的401/403为负例；两tenant都以自身只读
凭证读平台和自身固定Digest，并再次拒绝另一tenant内容。登记HTTP使用实际公共DTO字段，
包括 `image_id` 与小写scope/purposes/accelerator。

事件日志 `events.jsonl` 仅写命令标签、退出码、版本/指纹、幂等键、空间/Project/Image ID
及固定引用。第三方原始stdout/stderr和Secret不进入证据。所有本次临时交付/auth文件在
成功或失败后删除；CA文件及脱敏事件保留。Harbor内容、Robot、空间和登记**不会自动删除**，
无全局GC，也不对外部对象推断清理授权。使用实际ID/安装ownership与批准清单逐项核验清理；
丢响应时可能存在尚无ID的资源，先用原key恢复/核对。事件中的retained必须进入交付报告。

离线驱动检查：`python3 -B -m unittest discover -s scripts -p image_smoke_test.py -v`，
以及 `go test -race ./scripts/image-smoke-runtime`。受控协议fixture只验证驱动边界和
字段/阶段编排，不能作为真实Harbor、真实Governance部署或产品验收证据。

## 到期、故障恢复与清理交接

发布凭证到期/丢失时，先读取当前metadata version，再以新幂等键执行租户
`publisher-credential:reset`（平台使用 `issue-platform-publisher` 且 `rotate=true`）。
仅在受控交付窗口保存Secret，更新发布客户端后销毁本次交付文件。重置使旧Robot停用，
不保证已经签出的Registry Token立即失效；实际Token TTL必须在获批Harbor上另测。
普通GET不会恢复Secret；窗口到期必须显式重置，不能直接解密数据库或复用旧command。

**当前没有运行pull凭证更换/Namespace Secret同步的已接通产品操作。**
代码读取并校验 `expires_at`，已过期材料会拒绝交付；再次enable同一空间不能越过此检查。
上线方须把该能力作为接入前门禁，结合真实owner和唯一tenant Namespace完成受控更换：
准备并验证新材料、通过owner更新/确认其Secret generation、再停用旧身份。
这段顺序是待接入约束，不是声称已经存在的CLI；不得直接改Image行、在Harbor单独改密钥，
或宣称365天有效期等于永久可用。没有该更换链路时，应在到期前阻止新的创建尝试并报告
需要owner修复；不能承诺已有工作负载重建或换节点时仍能拉取。

空间/Robot创建请求超时后，先保留原actor、幂等键和完全相同的输入。查 `inspect-space`
的实际绑定，再重试原操作；不要用新key覆盖未决command。已落库候选ID按其代次恢复，
旧command不得刷新新代次。未知同名Project只允许在人工核对私有Project ID、安装归属与
外部证据后使用 `recover-project`；证据SHA256是核对记录的引用，不是自动归属证明。
缺少证据保持blocked。密钥文件缺失/错误AAD/密文损坏同样保持失败，不能丢弃行后重建身份。

取消登记只停止后续基于该登记的新解析，不触碰Harbor内容或已持久化的业务镜像意图。
停用Image或回退应用不会替你删除schema/Project/Robot/Namespace/Secret。
物理清理必须按当前run创建的实际ID/UID、安装/owner标识和清单逐项核对；包含不明对象
或存储时保留并报告。测试进程被强杀或主机中断后，私有交付目录可能仍有文件，先确认本run
归属再清理；任何自动退出路径都不能替代恢复后的独立资源核验。
