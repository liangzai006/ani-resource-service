-- name: LockDefaultEgressKey :exec
SELECT pg_advisory_xact_lock(hashtextextended('default-vpc-egress:'||sqlc.arg(cluster_id)::text||':'||sqlc.arg(idempotency_key)::text,0));

-- name: GetDefaultEgressReplay :one
SELECT * FROM network_default_vpc_egress_idempotency WHERE cluster_id=$1 AND idempotency_key=$2;

-- name: InsertDefaultEgressReplay :exec
INSERT INTO network_default_vpc_egress_idempotency(cluster_id,idempotency_key,fingerprint,resource_id,operation_id,response,created_at) VALUES($1,$2,$3,$4,$5,$6,$7);

-- name: InsertDefaultEgress :exec
INSERT INTO network_default_vpc_egresses(resource_id,cluster_id,cluster_uid,default_vpc_namespace,default_vpc_name,default_vpc_uid,name,pool_id,pool_revision,pool_provider_uid,admission_hash,admitted_at,last_operation_id,eip_provider_name,eip_binding_id,snat_provider_name,snat_binding_id,next_run_at,created_at,updated_at,eip_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$12,$12,$12,$18);

-- name: GetDefaultEgress :one
SELECT sqlc.embed(r),o.state AS operation_state FROM network_default_vpc_egresses r JOIN network_default_vpc_egress_operations o ON o.resource_id=r.resource_id AND o.operation_id=r.last_operation_id WHERE r.cluster_id=$1 AND r.resource_id=$2;

-- name: GetCurrentDefaultEgress :one
SELECT sqlc.embed(r),o.state AS operation_state FROM network_default_vpc_egresses r
JOIN network_default_vpc_egress_operations o ON o.resource_id=r.resource_id AND o.operation_id=r.last_operation_id
WHERE r.cluster_id=$1 AND r.default_vpc_namespace=$2 AND r.default_vpc_name=$3 AND r.retired_at IS NULL;

-- name: LockDefaultEgress :one
SELECT sqlc.embed(r),o.state AS operation_state FROM network_default_vpc_egresses r JOIN network_default_vpc_egress_operations o ON o.resource_id=r.resource_id AND o.operation_id=r.last_operation_id WHERE r.cluster_id=$1 AND r.resource_id=$2 FOR UPDATE OF r;

-- name: InsertDefaultEgressOperation :exec
INSERT INTO network_default_vpc_egress_operations(operation_id,resource_id,kind,state,created_at,updated_at) VALUES($1,$2,$3,'queued',$4,$4);

-- name: InsertDefaultEgressHistory :exec
INSERT INTO network_default_vpc_egress_history(history_id,resource_id,operation_id,event,actor_ref,caller_ref,detail,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,clock_timestamp());

-- name: SupersedeDefaultEgressOperation :exec
UPDATE network_default_vpc_egress_operations SET state='failed',reason='DELETE_REQUESTED',updated_at=clock_timestamp(),completed_at=clock_timestamp() WHERE resource_id=$1 AND operation_id=$2 AND completed_at IS NULL;

-- name: AdmitDefaultEgressDelete :execrows
UPDATE network_default_vpc_egresses SET state='deleting',phase='delete_snat',reason='',version=version+1,last_operation_id=sqlc.arg(operation_id),lease_owner=NULL,lease_until=NULL,lease_epoch=lease_epoch+1,next_run_at=clock_timestamp(),updated_at=clock_timestamp() WHERE cluster_id=sqlc.arg(cluster_id) AND resource_id=sqlc.arg(resource_id) AND version=sqlc.arg(version);

-- Pool deletion must call this while holding the same platform-cluster lock.
-- name: CountDefaultEgressPoolReferences :one
SELECT count(*)::bigint FROM network_default_vpc_egresses WHERE cluster_id=$1 AND pool_id=$2 AND retired_at IS NULL;

-- name: DueDefaultEgress :one
SELECT sqlc.embed(r),o.state AS operation_state FROM network_default_vpc_egresses r JOIN network_default_vpc_egress_operations o ON o.resource_id=r.resource_id AND o.operation_id=r.last_operation_id
WHERE r.cluster_id=$1 AND r.retired_at IS NULL AND r.next_run_at<=clock_timestamp() AND (r.lease_until IS NULL OR r.lease_until<=clock_timestamp()) ORDER BY r.next_run_at,r.resource_id FOR UPDATE OF r SKIP LOCKED LIMIT 1;

-- name: ClaimDefaultEgressLease :one
UPDATE network_default_vpc_egresses SET lease_owner=sqlc.arg(owner)::uuid,lease_until=clock_timestamp()+sqlc.arg(lease_seconds)::bigint*interval '1 second',lease_epoch=lease_epoch+1 WHERE cluster_id=sqlc.arg(cluster_id) AND resource_id=sqlc.arg(resource_id) RETURNING lease_epoch;

-- name: AssertDefaultEgressLease :one
SELECT EXISTS(SELECT 1 FROM network_default_vpc_egresses WHERE cluster_id=sqlc.arg(cluster_id) AND resource_id=sqlc.arg(resource_id) AND version=sqlc.arg(version) AND last_operation_id=sqlc.arg(operation_id) AND lease_owner=sqlc.arg(owner)::uuid AND lease_epoch=sqlc.arg(epoch) AND lease_until>clock_timestamp())::boolean;

-- name: BeginDefaultEgressMutation :execrows
UPDATE network_default_vpc_egresses SET pending_action=sqlc.arg(action),pending_unknown=true,pending_since=coalesce(pending_since,clock_timestamp())
WHERE cluster_id=sqlc.arg(cluster_id) AND resource_id=sqlc.arg(resource_id) AND version=sqlc.arg(version) AND last_operation_id=sqlc.arg(operation_id) AND lease_owner=sqlc.arg(owner)::uuid AND lease_epoch=sqlc.arg(epoch) AND lease_until>clock_timestamp()
AND (pending_action='' OR (pending_action=sqlc.arg(action) AND pending_action IN('delete_snat','delete_eip')));

-- name: FinishDefaultEgress :execrows
UPDATE network_default_vpc_egresses SET state=sqlc.arg(state),phase=sqlc.arg(phase),reason=sqlc.arg(reason),version=version+1,
 eip_provider_uid=CASE WHEN eip_provider_uid='' THEN sqlc.arg(eip_uid) ELSE eip_provider_uid END,
 snat_provider_uid=CASE WHEN snat_provider_uid='' THEN sqlc.arg(snat_uid) ELSE snat_provider_uid END,
 address=CASE WHEN sqlc.arg(address)::text<>'' THEN sqlc.arg(address) ELSE address END,
 applied_enabled=sqlc.narg(applied_enabled)::boolean,target_generation=greatest(target_generation,sqlc.arg(target_generation)::bigint),
 observed_at=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.narg(observed_at)::timestamptz ELSE observed_at END,
 pending_action=CASE WHEN sqlc.arg(clear_pending)::boolean THEN '' ELSE pending_action END,
 pending_since=CASE WHEN sqlc.arg(clear_pending)::boolean THEN NULL ELSE pending_since END,
 pending_unknown=CASE WHEN sqlc.arg(clear_pending)::boolean THEN false ELSE sqlc.arg(pending_unknown)::boolean END,
 lease_owner=NULL,lease_until=NULL,next_run_at=clock_timestamp()+sqlc.arg(delay_ms)::bigint*interval '1 millisecond',updated_at=clock_timestamp(),
 retired_at=CASE WHEN sqlc.arg(state)='deleted' THEN clock_timestamp() ELSE retired_at END
WHERE cluster_id=sqlc.arg(cluster_id) AND resource_id=sqlc.arg(resource_id) AND version=sqlc.arg(version) AND last_operation_id=sqlc.arg(operation_id) AND lease_owner=sqlc.arg(owner)::uuid AND lease_epoch=sqlc.arg(epoch) AND lease_until>clock_timestamp()
 AND (eip_provider_uid='' OR sqlc.arg(eip_uid)::text='' OR eip_provider_uid=sqlc.arg(eip_uid)) AND (snat_provider_uid='' OR sqlc.arg(snat_uid)::text='' OR snat_provider_uid=sqlc.arg(snat_uid));

-- name: FinishDefaultEgressOperation :exec
UPDATE network_default_vpc_egress_operations SET state=sqlc.arg(state),reason=sqlc.arg(reason),updated_at=clock_timestamp(),completed_at=CASE WHEN sqlc.arg(state) IN('succeeded','failed') THEN clock_timestamp() ELSE NULL END WHERE resource_id=sqlc.arg(resource_id) AND operation_id=sqlc.arg(operation_id) AND completed_at IS NULL;
