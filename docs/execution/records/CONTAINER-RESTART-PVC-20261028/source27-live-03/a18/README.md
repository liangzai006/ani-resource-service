# A18：真实 owner 拒绝、GPU 签名缺失及 Network 未就绪时的停止

本目录仅收取远端已经保存的 state；整理期间未查询业务、PG 或集群，未执行新的写操作。每份投影的 `_evidence_source` 保留原始远端路径、原文件 SHA256 和敏感值替换记录；未复制凭据、private 文件或单独的 CREATE 输入文件。

## 实际操作

| 场景 | 实例 | intentVersion | operation ID | 实际结果 |
|---|---|---:|---|---|
| CPU owner label 不匹配后的原 Restart | `9b2e5371-5be3-4cac-8766-55147d8041fb` | 15 | `37afeee6-3a00-4d39-bd33-9c43864079fb` | 先 `blocked/validating/INSTANCE_OWNERSHIP_CONFLICT`；恢复同 Notebook 标签后原操作 `completed/done/runtime_verified` |
| 新任务 GPU CREATE | `9567dacb-ffb4-4bf9-80eb-9b5c13011c4a` | 1 | `f43b6cdf-d5ec-472d-b536-0f22528a22a1` | `completed/done`，真实 Running |
| GPU 签名移除后普通 Stop | 同上 | 2 | `8fedabd2-8776-42fe-8beb-cb55d05513bc` | `completed/done`，完整物理 Stopped |
| GPU 正常 Start | 同上 | 3 | `ee26add5-3630-47f1-ba28-8f3bbbf3f9d0` | `completed/done`，真实 Running |
| GPU Network 未就绪时普通 Stop | 同上 | 4 | `f702275a-c229-4cb1-a5d0-2b79661db36b` | `completed/done`，完整物理 Stopped |
| GPU 最终 DELETE | 同上 | 5 | `d7e82ae0-f32a-4aec-9592-34b91d4a415d` | `completed`，API 原始 phase 为 `deleting`；五项 cleanup 均 true，任务对象及原 PV 全无 |

[owner-state.safe.json](owner-state.safe.json) 保存 owner 标签的 UID/RV 条件变更、两次 blocked 样本、Pending/Fence 为空、Notebook spec/关键注解保持，以及恢复原标签后仅续查同一 Restart 的记录。原 workspace marker 只读验证通过，applied mount version 保持 14；STS/Pod 变化按实际记录，不归因为 Resource 越权写入。

[gpu-network-state.safe.json](gpu-network-state.safe.json) 保存两项普通 Stop 及最终清理：

- 签名：精确任务 Pod `8f070577-ac38-4e10-b8d4-937a328238de` 在条件 PATCH 回执中 `signature_present=false`，随后立即提交原 Stop2。此证据证明移除签名后仍可正常停止；不声称 signer 始终不可用或签名持续缺失，期间实际快照原样保留。
- Network：仅普通删除本任务 Pod，让现有控制器自然替换。Stop4 前实际新 Pod 与持久 snapshot UID 均为 `e5fe7f03-fa2a-4064-97e7-0694846a35af`；`runtime_desired=running`、无 active operation，Network 为 `open/reserved`、reason `PROVIDER_OBJECT_MISSING`，snapshot revision 8、confirmed revision 6。未改共享组件、KCN 或绑定标签。
- DELETE5：`computeCleaned/workspaceCleaned/networkCleaned/volumeReferencesReleased/quotaReturned` 均为 true；精确 NB、STS、Pod、workspace PVC 和已保存的原 PV 均不存在。所有会话 logout 为 `CONFIRMED`。

## 保留的 harness 失败字段

owner state 中首次 `fault-label` 的 `TASK_KUBE_CALL_FAILED_PRIVATE_OUTPUT_WITHHELD` 是驱动 Kube 调用失败的历史记录；之后真实条件变更、blocked 与原操作恢复均有独立成功证据。该失败字段未删除，不把它解释为产品 owner 检查失败。

GPU state 中 `step_failures.delete.code=AttributeError` 来自清理读取器将“全部命名对象已不存在”的空输出当作字典读取。业务 DELETE5 已完成；Root 将空输出归一为空集合后，沿原 ACK 只查询收尾并得到完整清理证据，没有提交新 DELETE，也未 force 清理。历史字段继续保留。
