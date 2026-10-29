# Sandbox Kubernetes 权限

Sandbox 运行在 Resource 进程中，共用该进程的 Kubernetes 客户端；此清单不创建新的
Deployment 或 ServiceAccount。`rbac.yaml` 使用现有的 `ani-system/ani-resource-service`
身份。若实际部署使用不同 ServiceAccount 或显式 kubeconfig，须以该客户端的实际身份
配置和核验授权，不能假定示例绑定已经生效。

集群级规则供读取和初始化共享租户命名空间、读取 StorageClass、Kata RuntimeClass 和 PV 使用；其余 Pod、PVC、镜像拉取 Secret
权限只通过每个租户命名空间的 RoleBinding 授予。Secret 的 get/update 限定为该租户的
Sandbox 拉取 Secret；Kubernetes RBAC 无法按名称限制 create。Storage 的现有角色覆盖部分相同操作，
但其租户角色不包含 Sandbox 的 Pod 和 Secret 操作，集群角色也没有 PV list。
两套角色可以绑定到同一身份，Kubernetes 会合并权限。

## 开通

1. 由管理员应用 `deployments/sandbox/rbac.yaml`，并确认 Resource 实际使用的身份。
2. 租户初始化命名空间后，由负责租户开通的管理员在该命名空间创建绑定；下例中的
   `TENANT_ID` 是该租户的 Resource UUID，`NS` 是其已初始化的命名空间：

   ```bash
   TENANT_ID=<租户 UUID>
   NS=<租户命名空间>
   kubectl create rolebinding ani-sandbox-tenant \
     --clusterrole=ani-sandbox-tenant \
     --serviceaccount=ani-system:ani-resource-service \
     -n "$NS"
   kubectl create role ani-sandbox-pull-secret \
     --verb=get,update --resource=secrets \
     --resource-name="ani-sandbox-pull-$TENANT_ID" -n "$NS"
   kubectl create rolebinding ani-sandbox-pull-secret \
     --role=ani-sandbox-pull-secret \
     --serviceaccount=ani-system:ani-resource-service -n "$NS"
   ```

   对已有租户也要逐个核对。仅创建命名空间不会自动创建此 RoleBinding；当前清单不授予
   Resource 管理 RoleBinding 的权限。若租户开通流程已经自动管理绑定，应在该流程配置
   等价绑定并核验其实际生效。
3. 用实际身份逐项核验，以下以示例 ServiceAccount 为例：

   ```bash
   SA=system:serviceaccount:ani-system:ani-resource-service
   kubectl auth can-i get namespaces --as="$SA"
   kubectl auth can-i create namespaces --as="$SA"
   kubectl auth can-i get storageclasses.storage.k8s.io --as="$SA"
   kubectl auth can-i list storageclasses.storage.k8s.io --as="$SA"
   kubectl auth can-i get runtimeclasses.node.k8s.io/sandbox-kata --as="$SA"
   kubectl auth can-i get runtimeclasses.node.k8s.io/kata --as="$SA"
   kubectl auth can-i get persistentvolumes --as="$SA"
   kubectl auth can-i list persistentvolumes --as="$SA"
   for resource in pods persistentvolumeclaims; do
     for verb in get create delete; do
       kubectl auth can-i "$verb" "$resource" -n "$NS" --as="$SA"
     done
   done
   kubectl auth can-i create secrets -n "$NS" --as="$SA"
   kubectl auth can-i get "secret/ani-sandbox-pull-$TENANT_ID" -n "$NS" --as="$SA"
   kubectl auth can-i update "secret/ani-sandbox-pull-$TENANT_ID" -n "$NS" --as="$SA"
   ```

以上操作只描述待执行的部署步骤。权限检查不能替代真实创建、暂停、恢复和删除验收；
目标集群现有 ServiceAccount、其他角色、准入策略和实际有效权限需要现场核实。Secret
授权和 Pod 创建能力受 RoleBinding 所在租户命名空间约束，不能通过集群级绑定授予
`ani-sandbox-tenant`。Pod 创建能力可能间接访问命名空间中的其他 Secret；Kubernetes
RBAC 不能按 Pod 内容限制这项能力，需要结合实际准入策略评估。
