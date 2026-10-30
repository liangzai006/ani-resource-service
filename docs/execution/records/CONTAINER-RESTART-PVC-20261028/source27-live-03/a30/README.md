# A30：Storage 删除阶段退出、发送后退出与旧代回执

本目录仅收取远端已经存在的安全 state 和进程回执；整理期间没有重新查询业务、PG 或集群。各 JSON 的 `_evidence_source` 保存原始远端路径、原文件 SHA256 和脱敏记录。未复制 private、凭据、原输入文件或原始 Deployment 模板。

| 真实场景 | 原 ticket | 实际结果与入口 |
|---|---|---|
| reserve 提交后、usage 检查前退出 | `01bccc0b-2272-4853-a620-dbff69d444a2` | [observe](stage-exit/reserve-before-usage/observe/state.safe.json) 证明真实 exit86、同 Pod 容器重启；[recover](stage-exit/reserve-before-usage/recover/state.safe.json) 证明原 ticket 正常恢复到 completed、原 PVC/PV Gone、保护引用为空 |
| write_started 提交后、发送前退出 | `5d6837ab-f783-4df8-bb54-0bb69aaeb11e` | [observe](stage-exit/mark-before-send/observe/state.safe.json) 证明真实 exit86；[recover](stage-exit/mark-before-send/recover/state.safe.json) 证明原 ticket 正常恢复到 completed、原 PVC/PV Gone、保护引用为空 |
| 真实 DELETE 2xx 已返回、观察前退出 | `0203f1b2-47f6-40e6-9deb-b8cc6645c21e` | [run](postsend-exit/run/state.safe.json) 保存真实 200 被截住及 Resource 进程丢失；[recover](postsend-exit/recover/state.safe.json) 证明原 ticket completed、PVC/PV Gone、保护引用为空 |
| 旧 UID 请求迟到、同名新 UID 已存在 | `b4c538c1-34ad-42fd-a66a-87c7e98aa274` | [run](old-generation/run/state.safe.json) 保存原代 completed、正常 Storage 同名重建新 UID，以及迟到原请求真实 HTTP409；新 UID 保留，[cleanup](old-generation/cleanup/state.safe.json) 记录两代任务对象正常清理/PV 回收 |

两处 stage 的完整请求与控制记录分别位于 `stage-exit/reserve-before-usage/` 和 `stage-exit/mark-before-send/`，各有 `create/arm/delete/observe/recover/clear/state.safe.json`。首次业务请求中的 `STORAGE_BACKEND_UNAVAILABLE` 是退出造成的实际响应，未改写为成功；随后沿原 ticket、原请求体正常续接，保存自然 lease 过期或正常新 epoch 的观察。两个 clear 都已记录撤销 arm，保留阶段 marker。

发送后退出的 [resource-process.safe.json](postsend-exit/recover/resource-process.safe.json) 绑定同一诊断镜像的实际 Resource replacement。其恢复 state 保留 `failure.code=ORIGINAL_DELETE_TICKET_NOT_COMPLETED`：这是首次看到 PVC Gone 就立即要求 guard completed 的 harness 过早断言。Root 改为正常等待原 ticket 完成后，原 ticket 在 phase completed、lease 已释放时得到 PASS；旧 failure 未删除，没有 SQL DML、lease 改写或新 ticket 冒充恢复。

旧代场景实际旧 PVC UID 为 `98ed3256-ac55-4e43-a149-b0a7a2b2aaca`，正常重建的同名新 UID 为 `8364596a-5bd2-4238-9a46-3fe1f1147e26`。迟到请求按旧 UID 返回 409，不能删除新代。`create/run/cleanup` state 保留完整正常业务和清理证据。

以上为真实 Gov/Resource/Kube/只读 PG 的既有证据；没有用伪空 LIST、直接 SQL 改状态、手工改 lease 或 force 删除业务对象制造结果。
