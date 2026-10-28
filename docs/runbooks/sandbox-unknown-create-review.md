# Sandbox 未知创建只读诊断

此命令帮助定位 `create_pod` 或 `create_workspace` 的未决 mutation，输出供运维留存的 JSON 诊断报告。它不恢复资源、不修改数据库或业务 operation，不创建/删除 Pod、PVC 或 PV。原 mutation 仍是屏障；Kubernetes 短暂 NotFound、权限失败和等待时间都不能证明原请求未执行，也不能证明曾创建的 Pod 已停止。即使后来不存在，强删或节点分区也可能留下仍在运行的进程。

使用具有 Sandbox 目标记录只读权限的私有 DSN 文件，以及对应集群的只读 kubeconfig；SelfSubjectReview 是 Kubernetes 的非持久化身份查询，除此以外命令仅 GET 目标 Pod/PVC 以及已有明确名称的 PV。缺少只读权限时报告 `forbidden` 等状态，不将其视为对象不存在。访问数据库和 Kubernetes 所需权限由平台运维配置；命令记录各自实际凭据身份，不能把它们自动等同于平台管理员或某个自然人，共享凭据尤其不能区分操作人。

```sh
ani-resource-service -sandbox-review-unknown-create \
  -sandbox-review-dsn-file <private-dsn-file> \
  -sandbox-review-kubeconfig <operator-kubeconfig> \
  -sandbox-review-cluster-id <configured-logical-cluster-uuid> \
  -sandbox-review-tenant-id <tenant-uuid> \
  -sandbox-review-id <sandbox-uuid>
```

输出包含时间、目标、版本、状态、pending、最新 operation、lease、已记录的 Provider 引用、数据库用户、Kubernetes API 看到的用户名、Pod/PVC/PV 的只读观察及解释。`provider_spec_checks_match` 仅代表通过现有 Provider 的部分规格检查，不是对对象全部字段的逐项比较，也不证明 Pod 已停止。数据库读取是一次只读一致快照，Kubernetes 对象逐个读取，时间点可能不同，不构成跨系统原子快照；若身份查询失败会单独标出。`physical_cluster_not_authenticated` 表明仅核对输入的逻辑 cluster ID 与冻结记录一致，并没有验证 kubeconfig 所指物理集群；逻辑 ID 不一致时不查询 Kubernetes。是否对应真实集群需由独立可信资产和身份机制确认。

对象观察中，检查过的布尔事实会明确输出 `true` 或 `false`；字段缺失表示未检查或缺少比对身份，不表示 `false`。例如对象找不到时不会声称它未在删除中，也不会伪造归属检查结果。

`audit_status: not_recorded` 表示结果只输出至标准输出；命令不建立持久或防篡改审计。运维须按现有受控流程保存报告，并保护输出中的租户、实例、操作、节点与 Provider 身份。持久审计、个人身份归属和不可篡改留存依赖外部认证/日志机制；没有相应可信机制时不能将该报告当作已经审计通过的证据。原 mutation 不会因诊断解除；只有明确可归属对象出现及现有 worker 安全识别，或未来接入经验证的原请求结果和节点终止证据，才能按各自合同推进。
