# 指定测试环境只读盘点

2026-10-28，经 `ssh kubercloud@100.64.128.12` 读取；未写入对象、未登录平台、未读取 Secret。实际对象身份和安全化部署/controller/CSI/GPU/Network/PVC 清单见 [cluster](environment-cluster-safe.json)，删除权限及准入匹配明细见 [PVC protection](environment-pvc-protection-safe.json)。历史环境证据没有用于本批验收。

当前入口 context 为 `kubernetes-admin@ani-kcn-full`，API 为 `https://lb.kubesphere.local:6443`，node1–3 内网地址 `10.10.1.66–68`。源码参照 Kubeflow v1.10 不代替部署实际传播验证。

A36：目标租户 Namespace 仅有 Kubernetes PVC 使用保护，未发现覆盖停止态 Notebook 模板引用的 PVC DELETE 准入。Argo/CDI/KubeVirt/Rook 等现有 ClusterRole 绑定含 PVC delete，Resource 身份亦有 delete/patch；平台 guard 之外的直接删除与同名重建风险没有闭合。此项属于环境删除保护配置缺口，不能冒称 CSI/节点不可用或已测出的 Restart 代码故障。盘点时按原 v2.0 规则记录为发布缺口。用户后续明确暂缓外部旁路的准入/RBAC 保护：A36 此部分 DEFERRED_USER_SCOPE，不记 PASS、不宣称严格外部防替换、不阻塞本批挂卸发布和验收；平台内部 name guard、四态保护、引用代际、删除恢复和 UID 复核继续完整实现。

本批必要部署前须基于精确候选记录维护入口、备份、影响和恢复步骤。现阶段没有执行迁移、权限登记或部署；未清理/修改既有负载。已有 workload/PVC 清单均在保护范围，不能因本批容量需要删除。
