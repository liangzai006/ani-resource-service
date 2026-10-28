-- name: GetVMNamespace :one
SELECT * FROM network_vm_namespaces WHERE tenant_id=$1 AND cluster_id=$2;

-- name: ReserveVMNamespace :exec
INSERT INTO network_vm_namespaces
 (tenant_id,cluster_id,namespace,namespace_uid,management_id,template_id,nad_name,nad_config,nad_config_digest,service_account)
VALUES ($1,$2,$3,$4,$5,'kcn-single-plugin-v1',$6,$7,$8,$9)
ON CONFLICT (tenant_id,cluster_id) DO NOTHING;

-- name: SaveVMNamespaceObjectIdentity :one
UPDATE network_vm_namespaces
SET nad_uid=CASE WHEN sqlc.arg(object_kind)::text='nad' THEN sqlc.arg(object_uid)::text ELSE nad_uid END,
 service_account_uid=CASE WHEN sqlc.arg(object_kind)::text='service_account' THEN sqlc.arg(object_uid)::text ELSE service_account_uid END,
 ready=(CASE WHEN sqlc.arg(object_kind)::text='nad' THEN sqlc.arg(object_uid)::text ELSE nad_uid END<>''
 AND CASE WHEN sqlc.arg(object_kind)::text='service_account' THEN sqlc.arg(object_uid)::text ELSE service_account_uid END<>''),
 reason='',updated_at=clock_timestamp()
WHERE tenant_id=sqlc.arg(tenant_id) AND cluster_id=sqlc.arg(cluster_id) AND management_id=sqlc.arg(management_id)
 AND sqlc.arg(object_uid)::text<>'' AND sqlc.arg(object_kind)::text IN ('nad','service_account')
 AND (sqlc.arg(object_kind)::text<>'nad' OR nad_uid='' OR nad_uid=sqlc.arg(object_uid))
 AND (sqlc.arg(object_kind)::text<>'service_account' OR service_account_uid='' OR service_account_uid=sqlc.arg(object_uid))
RETURNING *;

-- name: FailVMNamespacePreparation :exec
UPDATE network_vm_namespaces SET ready=false,reason=$4,updated_at=clock_timestamp()
WHERE tenant_id=$1 AND cluster_id=$2 AND management_id=$3;
