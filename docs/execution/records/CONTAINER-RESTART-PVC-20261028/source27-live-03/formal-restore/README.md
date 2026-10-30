# 恢复正式 Source27 镜像

[result.safe.json](result.safe.json) 已收自远端 `resource-source27-fault-update-04/restore/result.safe.json`，结果为 **RESTORED_FORMAL_SOURCE27**。本目录只复制既有回执，没有重新查询 Deployment 或 Pod。

- 正式镜像：`10.10.1.66:30003/ani-gpu-platform/ani-resource-service@sha256:1846ff442cc31f9a645f6137fb8a70ebc3f8d76e8a72bf675f0c72893714909e`。
- Deployment generation/observedGeneration 83，Ready replicas 1；实际 Pod UID `b3c8184a-d0fa-426d-8b80-d294c8f9c39d`，容器 imageID 为上述正式 digest。
- 实际耗时 **10.525 秒**；`tests_run=false`、`release_gates_run=false`，database/business API writes 都为 0。

精确来源绑定、部署 UID、时间及原始远端文件 SHA256 保存在回执与 `_evidence_source` 中。未复制 private 或原始部署模板，未执行 SQL DML、lease 改写或 force 清理。
