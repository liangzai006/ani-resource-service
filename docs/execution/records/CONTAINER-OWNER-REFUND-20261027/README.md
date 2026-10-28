# 容器实例 owner 退额整改验证

日期：2026-10-27。唯一当前状态见[执行状态](../../status.md#container-instance-owner-refund-2026-10-27)，合同见[实例规格](../../../specs/instance-mvp.md#10-完整释放及-gov-原账退额)，实施范围见[方案](../../../plans/container-instance-release-without-acc-20261027.md)。

## 固定输入与执行位置

- Resource 基线：`ece9d53037404b04034884e5c2d641ee95eb5729`；任务分支 `codex/container-owner-refund-20261027`，本文随提交固定验证输入。
- 本地仅编辑、Git/轻量检查和传输；格式化、编译、测试、PostgreSQL 均在 `ssh fedora`。
- Fedora 独占根：`/home/chabking/workspace/ani-resource-service-runs/container-owner-refund-20261027-01a17b4e`，有效源码目录 `src-r2`。源码归档排除私钥、DSN、kubeconfig、缓存与无关历史证据；首次完整代码归档 SHA256 `a699f8bbe0eaf7932a0a627497c29f30e41ec77ae7b6442d1f40164a283cf433`，其后两个修补清单及最终源码清单附于本目录。
- 最终运行源码清单 [source-code-final.sha256](source-code-final.sha256) 的 SHA256 为 `df2a3221da0f30c5c5ae19981a84d32e3538fb57d2f26ac26e64b2526b167334`；Fedora 返回格式化结果经输入哈希防漂移后应用，本地全部运行源码与该清单逐文件一致。后续文档记录变化不改变已验证运行源码。
- Go：`go1.26.10 linux/amd64`；`GOWORK=off`、`GOTOOLCHAIN=local`、`GOMAXPROCS=2`、`GOFLAGS=-p=2`、`GOMEMLIMIT=1536MiB`。
- PostgreSQL：脚本固定 `docker.io/library/postgres@sha256:4ef4dbc939d61acea57712655ddb4b4ab27419c913f94cca0cd57cb3ea3c2280`，实际 `18.6`。每次 fixture 独占容器、数据库及 owner/runtime 角色，runtime 为受限身份；两条纵向测试使用同一任务 PG 服务中的独立库。
- 临时路径：`/tmp/res-owner-01a17b4e` 仅是本任务 `tmp` 的短路径 symlink；避免共享 tmpfs 用户配额不足及 Unix socket 过长。未清理其他任务缓存或临时文件。

## 已执行检查

| 检查 | 命令/入口 | 结果与边界 |
|---|---|---|
| 相关单元与装配 | `go test -count=1 ./internal/biz/instance ./internal/data/instance ./internal/service/instance ./internal/server ./cmd/ani-resource-service` | PASS，见 `unit-r2.log` |
| 历史 UID 适配器与 Compute | `go test -count=1 ./internal/data/instance ./internal/data/compute` | PASS，见 `unit-r4-adapter-compute.log`；Compute 普通构建无独立 unit，用以下 PG 验证行为 |
| 实例/迁移 PG | `INSTANCE_TEST_RUN='^Test(Instance(OwnerCleanup|Legacy|PersistentLifecycle|DeleteFirst|ReleaseRetry|Superseded|Network)|CPU|ResourceRelease|GPU(ExternalScope|CreatedWithout)|BorrowedVolume|ComputeMigration(PreservesRelationAndRecovers|FailureIsAtomic))' scripts/instance-integration -v ./internal/data/instance ./internal/data/compute` | PASS，见 `pg-r4.log`。两条 Network 纵向测试初次缺少 Network fixture 环境而 skip，已按下一行实际补跑 |
| 两条 Network 纵向恢复及 ACK 回写 | `INSTANCE_TEST_RUN='^TestInstance(NetworkVerticalPGNotebookTwoRestartsAndRecreation|NetworkVerticalConfirmRetryRequiresDurableAck|WorkerFinishPreservesCommittedReleaseACK)$' scripts/instance-integration -v ./internal/data/instance` | 全部 PASS，无 skip，见 `pg-r5-vertical-ack.log` |
| 静态检查 | `go vet ./internal/biz/instance ./internal/data/instance ./internal/data/compute ./internal/service/instance ./internal/server ./cmd/ani-resource-service` | PASS，见 `vet-r5.log` |
| 服务构建 | `go build -trimpath -o <任务根>/bin/ani-resource-service ./cmd/ani-resource-service` | PASS，二进制 SHA256 `4c91db9ce2f5186ff4a2422d967098cc3520d2fdd5b0e8fbcc513bc6fb7688da`，见 `binary.sha256` |
| 提交前完整门禁 | `make verify` | PASS，见 `verify-precommit-r5.log`；生成/分层/故障构建/tidy、全包测试、vet、build、模块校验及 diff 检查全部通过 |

业务用例实际覆盖：完整清理前无新退额事件、Gov 丢回执后保持原事件并返回 ALREADY_APPLIED、重建通知适配器后无需 Kubernetes 恢复、错误 ACK 拒绝、旧 pending/acknowledged 事件、旧清理标志遇到观察失败不误封存、Stop/Start 和未知 CREATE 不退额、DELETE-first/迟到与租约 fencing、workspace/网络等待、借用卷保留以及 worker 不覆盖已提交 ACK。生产 Notebook 适配器另外核对历史 UID、标签丢失、同名新 UID 和查询失败；未用恒真释放替身代替本地业务判断。

## 早期失败及恢复

1. 首次尝试传输包含无关历史证据的归档过大，已中止；一次过早解包得到 EOF，该 `src-r1` 从未作为测试输入。随后仅传正式源码与所需文档的 823 文件清单，在 `src-r2` 校验全部通过。
2. PG 首轮链接因 Fedora `/tmp` 的现有用户配额不足报 `disk quota exceeded`，行为 NOT_RUN，见 `pg-r2.log`。改用本任务 home 文件系统临时目录的短 symlink 后继续，未删其他任务文件。
3. 新 instance migration 007 使 Compute 原固定六版历史校验报 `unexpected instance migration history`，见 `pg-r3-home-tmp.log`。同步完整历史校验到七版后，同一 instance/Compute fixture、幂等迁移和失败原子性均通过；历史 DDL 未修改。
4. Network 纵向用例需要 `NETWORK_TEST_ADMIN_DSN`，原实例脚本只提供 instance 的私有 DSN 文件，因此早期 skip。复用同一任务 PG、提供该测试 fixture 环境后，两条用例及新增 ACK 回写用例实际 PASS；未放宽业务授权或清理门槛。
5. 完整门禁先遇到共享模块缓存权限（r1）和公开 proxy sumdb EOF（r2）；改用本任务独占 `GOMODCACHE` 并复用已验证的公开 sumdb 缓存。r3/r4 的 socket 测试拒绝 symlink 临时路径，最终 `TMPDIR=GOTMPDIR=/home/chabking/t/arv-01a` 使用真实短目录后 r5 全部通过。未关闭模块校验或修改测试断言。

## 未执行与运行切换

完整 `make verify` 已 PASS。用户授权的 PR 正常合并、镜像发布、`100.64.128.12` owner migration 和只读核验结果见唯一[执行状态](../../status.md#container-instance-owner-refund-2026-10-27)与[脱敏发布回执](release-10064.json)。PG 纵向 fixture 中只有外部 Kubernetes/Gov/KC 边界使用替身，Resource 持久化、权限、版本/租约、清理决定与 outbox 是真实实现；不能据此宣称新实例的生产账本或设备容量验收。

上线采用 owner 显式 `-instance-migrate`，先停旧实例 worker/投递，再迁移 schema 7 和切换匹配二进制。旧未 ACK 事件不能直接投递：必须完成原实例清理，保留原事件及 CREATE；旧 ACK 保留，不重复退款。新快照与关闭事实已写入后不得直接混跑旧 reader/worker。目标已有 PR #17 的 ImageRuntime 能力将随固定来源组合保留，本次退额 PR 单独合入 main。
