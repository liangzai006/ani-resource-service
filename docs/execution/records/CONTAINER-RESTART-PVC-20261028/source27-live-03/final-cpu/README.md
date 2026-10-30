# CPU9b 最终正常删除与任务 scratch 回收

[state.safe.json](state.safe.json) 仅复制远端 `final-cpu9b-cleanup-live-01/state.json` 的既有安全投影。原始路径与 SHA256 见 `_evidence_source`；本次收证未重新执行业务请求或 PG/Kube 采样。

CPU `9b2e5371-5be3-4cac-8766-55147d8041fb` 在最终受理前为 Stopped、无 active operation、CurrentMounts=[]、Pending/Fence 空。唯一正常 DELETE 使用 expectedVersion 28，operation `97371a02-3b59-4af4-9628-a848a10174c4`，intentVersion **29**，实际 `completed/done`，三态 Deleted、active 为空、applied mount version 20。

- API `computeCleaned/workspaceCleaned/networkCleaned/volumeReferencesReleased/quotaReturned` 全为 true。
- 精确自身 NB、STS、Pod、workspace PVC 和原 PV 均 Gone；原 workspace PVC UID `d4f3d393-32dc-4a3c-ba10-624e2a06a79f`、PV UID `83072067-2c12-4605-a48b-a4d1e7fc0f42` 已保存在受理前摘要中。
- `unreleased_references=[]`；历史 released 引用按原记录保留。
- 原借卷 `ani-vol-168d2c1c17684c3f9b1eab83d963327d` 仍 Bound：PVC UID `fa56c965-5a9a-4bb1-b271-3960a7309e96`、PV UID `544dd33b-3330-48d7-a825-308c590d45f6` 未变。本脚本没有删除或写入原借卷。
- 原 CREATE `5a8123f1-d9ff-4bca-99b6-7c8d1b54af07` 摘要不变；原 charge 和实例所有 charge 集合均保持为空，没有新增费用。全部正常会话 logout 为 CONFIRMED。

[final-scratch-reclaim-01.safe.json](final-scratch-reclaim-01.safe.json) 原样收取远端同名安全回执：三个 A14/C19 任务 scratch 的原 claim UID 均无剩余 PV，`remaining_task_pvs=[]`、`PASS_NO_TASK_SCRATCH_PVS_REMAIN`，Kube writes/forced deletes 都为 0。没有 force 删除、SQL DML 或 lease 改写。
