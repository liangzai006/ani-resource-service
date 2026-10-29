-- name: GetInstance :one
SELECT snapshot,version,desired,created_at,updated_at,instance_contract_version,current_mounts,mounts_initialized,applied_mount_version,runtime_desired,recovery_fence
FROM instance.instances
WHERE tenant_id=sqlc.arg(tenant_id) AND instance_id=sqlc.arg(instance_id);
