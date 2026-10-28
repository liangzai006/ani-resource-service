# Image runtime 接入定向证据

基线：`origin/main=10b9ad8813206a62fa100659363f940815a2fd7f`，已通过 Git SSH 443 刷新。
任务分支：`codex/image-registry-resource-20261026`。已有 `api/image/v1` 两 RPC 不改协议，
消费者可锁正式根模块 `github.com/zhangzhe-ctrl/ani-resource-service@v0.0.0-20261026034642-10b9ad881320`。
新 listener 只接 Inference，原 Instance 同进程 RuntimeImages 保留；配置/证书合同见
[部署接线](../../../../deployments/image/README.md#inference-运行入口)。

Fedora 源目录：`/home/chabking/task-runs/image-registry-20261026/src/resource`。
Go 1.26.10，`GOWORK=off`，独立任务缓存/TMPDIR，既有 `net05a-heavy.lock`，
systemd scope `CPUQuota=200%/MemoryMax=2300M/MemorySwapMax=0`。
仅对本次八个 Go 文件执行 gofmt；未改 protobuf，无生成差异。
`source-code.sha256` 固定最终返回的已测代码快照。

必要用例与实际结果：

| 用例 | 结果 | 覆盖 |
|---|---|---|
| TestImageGovernanceAllowlistAndScope | PASS | 目录身份仍不能取 runtime 凭据 |
| TestImageRequestAndCredentialReplyNotLogged | PASS | Image 入参脱敏、响应凭据不入日志 |
| TestImageRuntimeIdentityAndTenantBoundary | PASS | 两方法、单一 Inference 身份、可信 tenant、拒 actor/重复 metadata/越租户 |
| TestImageRuntimeConfigurationFailsClosed | PASS | 默认关闭、部分 TLS/未启用 Image 拒绝启动 |
| TestImageRuntimeMTLSListener | PASS（修复测试启动后） | 真 TLS 1.3 listener，两真实客户端调用、拒错误证书/目录调用/越租户、无凭据日志 |

首次命令：

```text
go test ./internal/server ./cmd/ani-resource-service -run '^(TestImageRuntimeIdentityAndTenantBoundary|TestImageRuntimeMTLSListener|TestImageRuntimeConfigurationFailsClosed|TestImageGovernanceAllowlistAndScope|TestImageRequestAndCredentialReplyNotLogged)$' -count=1 -v
```

[首次原始日志](runtime-test-attempt1.txt) 中其他四项通过；mTLS 用例因测试直接调用阻塞式
Kratos.Start 挂起，仅中断该任务明确 PID，记录该次整体 FAIL。测试改为 goroutine 启动及有界
停止后，只重跑失败用例：

```text
go test ./internal/server -run '^TestImageRuntimeMTLSListener$' -count=1 -timeout=45s -v
```

[第二次原始日志](runtime-mtls-attempt2.txt) 为 PASS；没有重跑已通过且未改的用例。
测试 probe 只验证 RPC/身份边界，不能代替真实 Harbor、数据库、Secret 或产品推理验收。

CODE：实现完成；Fedora：上述定向行为通过；LIVE/PRODUCT：NOT_RUN。
全量 `make verify/integration/race/audit`：NOT_RUN（用户禁止全量）。
CI 对所有 push/PR（包括 tag push）在 Ubuntu 触发这些重任务，与本轮约束冲突，
因此没有 push、PR、tag 发布，也未修改/关闭/绕过 CI。候选保留本地；实际部署须等待合法交付路径。
