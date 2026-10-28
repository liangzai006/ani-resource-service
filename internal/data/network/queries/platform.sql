-- Platform tables do not carry a synthetic tenant. Cluster scope is mandatory.
-- name: GetPlatform :one
SELECT * FROM network_platform_resources WHERE cluster_id=$1 AND kind=$2 AND resource_id=$3;

-- name: LockPlatform :one
SELECT * FROM network_platform_resources WHERE cluster_id=$1 AND kind=$2 AND resource_id=$3 FOR UPDATE;

-- name: ListPlatform :many
SELECT r.* FROM network_platform_resources r WHERE r.cluster_id=sqlc.arg(cluster_id) AND r.kind=sqlc.arg(kind)
 AND (r.kind<>'public_pool' OR EXISTS (SELECT 1 FROM network_public_pools pool WHERE pool.cluster_id=r.cluster_id AND pool.resource_id=r.resource_id AND pool.scope='public'))
 AND (sqlc.arg(name_filter)::text='' OR r.name=sqlc.arg(name_filter))
 AND ((sqlc.arg(state_filter)::text='' AND r.state<>'deleted') OR r.state=sqlc.arg(state_filter))
 AND (sqlc.arg(after_id)::text='' OR (r.created_at,r.resource_id)<(sqlc.arg(after_created_at)::timestamptz,sqlc.arg(after_id)::text))
ORDER BY r.created_at DESC,r.resource_id DESC LIMIT sqlc.arg(max_results)::integer;

-- name: GetPublicPool :one
SELECT * FROM network_public_pools WHERE cluster_id=$1 AND resource_id=$2;

-- name: LockPublicPool :one
SELECT * FROM network_public_pools WHERE cluster_id=$1 AND resource_id=$2 FOR UPDATE;

-- name: GetDeviceAdoption :one
SELECT * FROM network_device_adoptions WHERE cluster_id=$1 AND resource_id=$2;

-- name: GetVlanNetwork :one
SELECT * FROM network_vlan_networks WHERE cluster_id=$1 AND resource_id=$2;

-- name: GetDefaultPublicPool :one
SELECT * FROM network_default_public_pools WHERE cluster_id=$1;

-- name: LockPlatformCluster :exec
SELECT pg_advisory_xact_lock(hashtextextended('platform-cluster:'||sqlc.arg(cluster_id)::text,0));

-- name: LockPlatformKey :exec
SELECT pg_advisory_xact_lock(hashtextextended('platform:'||sqlc.arg(cluster_id)::text||':'||sqlc.arg(operation_kind)::text||':'||sqlc.arg(idempotency_key)::text,0));

-- name: GetPlatformIdempotency :one
SELECT * FROM network_platform_idempotency WHERE cluster_id=$1 AND operation_kind=$2 AND idempotency_key=$3;

-- name: InsertPlatformIdempotency :exec
INSERT INTO network_platform_idempotency(cluster_id,operation_kind,idempotency_key,fingerprint,resource_id,operation_id,response,created_at,fingerprint_version)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,sqlc.arg(fingerprint_version));

-- name: InsertPlatformResource :one
INSERT INTO network_platform_resources(resource_id,kind,cluster_id,name,description,state,created_at,updated_at,last_operation_id,provider_name,binding_id)
VALUES($1,$2,$3,$4,$5,'provisioning',$6,$6,$7,$8,$9) RETURNING *;

-- name: InsertDeviceAdoption :exec
INSERT INTO network_device_adoptions(resource_id,cluster_id,device_name,inventory_fingerprint,node_inventory) VALUES($1,$2,$3,$4,$5);

-- name: InsertVlanNetwork :exec
INSERT INTO network_vlan_networks(resource_id,cluster_id,device_id,vlan_id) VALUES($1,$2,$3,$4);

-- name: InsertEgressGateway :exec
INSERT INTO network_egress_gateways(resource_id,cluster_id) VALUES($1,$2);

-- name: InsertPublicPool :exec
INSERT INTO network_public_pools(resource_id,cluster_id,mode,gateway_id,cidr,ovn_gateway_ip,excluded_ips,vlan_network_id,upstream_gateway_ip,managed_gateway)
VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF(sqlc.arg(vlan_network_id)::text,''),NULLIF(sqlc.arg(upstream_gateway_ip)::text,''),sqlc.arg(managed_gateway));

-- name: ManagedGatewayOwner :one
SELECT resource_id FROM network_public_pools WHERE cluster_id=$1 AND gateway_id=$2 AND managed_gateway;

-- Only an explicit owner deletion may supersede an unfinished create. The
-- resource version and lease are fenced by SetPlatformIntent/SchedulePlatform.
-- name: SupersedePlatformCreate :execrows
UPDATE network_platform_operations SET state='failed',reason='SUPERSEDED_BY_DELETE',
 completed_at=clock_timestamp(),updated_at=clock_timestamp(),next_attempt_at=NULL
WHERE resource_id=$1 AND operation_id=$2 AND kind LIKE 'create_%' AND completed_at IS NULL;

-- name: PublicPoolOverlaps :one
SELECT count(*)::bigint FROM network_public_pools p JOIN network_platform_resources r ON r.cluster_id=p.cluster_id AND r.resource_id=p.resource_id
WHERE p.cluster_id=$1 AND r.state<>'deleted' AND p.cidr::cidr && sqlc.arg(cidr)::cidr;

-- name: CountPoolEIPs :one
SELECT count(*)::bigint FROM network_eips WHERE cluster_id=$1 AND pool_id=$2 AND state<>'deleted';

-- name: CountGatewayPools :one
SELECT count(*)::bigint FROM network_public_pools p JOIN network_platform_resources r ON r.cluster_id=p.cluster_id AND r.resource_id=p.resource_id
WHERE p.cluster_id=$1 AND p.gateway_id=$2 AND r.state<>'deleted';

-- name: CountVlanPools :one
SELECT count(*)::bigint FROM network_public_pools p JOIN network_platform_resources r ON r.cluster_id=p.cluster_id AND r.resource_id=p.resource_id
WHERE p.cluster_id=$1 AND p.vlan_network_id=$2 AND r.state<>'deleted';

-- name: SetPoolAllocation :execrows
UPDATE network_public_pools SET allocation_enabled=$3 WHERE cluster_id=$1 AND resource_id=$2;

-- name: SavePoolVerification :execrows
UPDATE network_public_pools SET verification=$3,verification_expires_at=$4 WHERE cluster_id=$1 AND resource_id=$2;

-- name: SetDefaultPublicPool :exec
INSERT INTO network_default_public_pools(cluster_id,pool_id) VALUES($1,$2)
ON CONFLICT(cluster_id) DO UPDATE SET pool_id=excluded.pool_id,version=network_default_public_pools.version+1;

-- name: InsertPlatformOperation :exec
INSERT INTO network_platform_operations(operation_id,resource_id,kind,state,created_at,updated_at,completed_at,next_attempt_at)
VALUES(sqlc.arg(operation_id),sqlc.arg(resource_id),sqlc.arg(kind),sqlc.arg(state),sqlc.arg(created_at),sqlc.arg(created_at),sqlc.narg(completed_at),sqlc.narg(next_attempt_at));

-- name: GetPlatformOperation :one
SELECT o.* FROM network_platform_operations o JOIN network_platform_resources r ON r.resource_id=o.resource_id
WHERE r.cluster_id=$1 AND o.operation_id=$2;

-- name: InsertPlatformReconciliation :exec
INSERT INTO network_platform_reconciliations(resource_id,next_run_at) VALUES($1,$2);

-- name: InsertPlatformHistory :exec
INSERT INTO network_platform_history(history_id,resource_id,operation_id,event,resource_state,operation_state,reason,actor_ref,caller_ref,created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10);

-- name: SetPlatformIntent :one
UPDATE network_platform_resources SET state=sqlc.arg(state),reason='',last_operation_id=sqlc.arg(operation_id),version=version+1,updated_at=clock_timestamp()
WHERE cluster_id=sqlc.arg(cluster_id) AND resource_id=sqlc.arg(resource_id) AND version=sqlc.arg(version) RETURNING *;

-- name: SchedulePlatform :execrows
UPDATE network_platform_reconciliations SET next_run_at=clock_timestamp(),lease_owner=NULL,lease_until=NULL,lease_epoch=lease_epoch+1 WHERE resource_id=$1;

-- Shared worker's platform lane; this is not a second executor.
-- name: DuePlatformCandidate :one
SELECT p.* FROM network_platform_resources p JOIN network_platform_reconciliations r ON r.resource_id=p.resource_id
WHERE p.cluster_id=sqlc.arg(cluster_id) AND r.next_run_at<=clock_timestamp() AND (r.lease_until IS NULL OR r.lease_until<=clock_timestamp())
 AND NOT EXISTS(SELECT 1 FROM network_device_adoptions d WHERE d.resource_id=p.resource_id AND d.registration_closed)
ORDER BY r.next_run_at,p.resource_id LIMIT 1 FOR UPDATE OF p SKIP LOCKED;

-- name: AcquirePlatformLease :one
UPDATE network_platform_reconciliations SET lease_owner=sqlc.arg(owner)::uuid,lease_epoch=lease_epoch+1,
 lease_until=clock_timestamp()+sqlc.arg(lease_micros)::bigint*interval '1 microsecond'
WHERE resource_id=sqlc.arg(resource_id) AND (lease_until IS NULL OR lease_until<=clock_timestamp()) RETURNING *;

-- name: CheckPlatformLease :one
SELECT * FROM network_platform_reconciliations WHERE resource_id=sqlc.arg(resource_id) AND lease_owner=sqlc.arg(owner)::uuid
 AND lease_epoch=sqlc.arg(epoch) AND lease_until>clock_timestamp() FOR UPDATE;

-- name: RunPlatformOperation :one
UPDATE network_platform_operations SET state='running',attempt=attempt+1,execution_epoch=sqlc.arg(epoch),updated_at=clock_timestamp()
WHERE operation_id=sqlc.arg(operation_id) AND resource_id=sqlc.arg(resource_id) AND completed_at IS NULL RETURNING *;

-- name: BeginPlatformMutation :execrows
UPDATE network_platform_resources SET pending_action=sqlc.arg(action),pending_since=clock_timestamp(),
 provider_uid=CASE WHEN provider_uid='' THEN sqlc.arg(identity)::text ELSE provider_uid END
WHERE resource_id=sqlc.arg(resource_id) AND binding_id=sqlc.arg(binding_id) AND (provider_uid='' OR provider_uid=sqlc.arg(identity))
 AND ((sqlc.arg(action)::text='create' AND pending_action='') OR
 (sqlc.arg(action)='delete' AND (pending_action IN ('','delete') OR sqlc.arg(identity)::text<>'')));

-- name: AdvancePlatform :one
UPDATE network_platform_resources SET state=sqlc.arg(state),reason=sqlc.arg(reason),version=version+1,updated_at=clock_timestamp(),
 provider_uid=CASE WHEN provider_uid='' THEN sqlc.arg(identity)::text ELSE provider_uid END,
 observed_at=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.narg(observed_at)::timestamptz ELSE observed_at END,
 pending_action=CASE WHEN sqlc.arg(clear_pending)::boolean THEN '' ELSE pending_action END,
 pending_since=CASE WHEN sqlc.arg(clear_pending)::boolean THEN NULL ELSE pending_since END
WHERE resource_id=sqlc.arg(resource_id) AND version=sqlc.arg(version) AND (provider_uid='' OR provider_uid=sqlc.arg(identity)) RETURNING *;

-- name: CompletePlatformAttempt :execrows
UPDATE network_platform_operations SET state=sqlc.arg(state),reason=sqlc.arg(reason),updated_at=clock_timestamp(),
 completed_at=CASE WHEN sqlc.arg(state)::text IN ('succeeded','failed') THEN clock_timestamp() ELSE NULL END,
 next_attempt_at=CASE WHEN sqlc.arg(state)::text IN ('succeeded','failed') THEN NULL ELSE clock_timestamp()+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' END
WHERE operation_id=sqlc.arg(operation_id) AND resource_id=sqlc.arg(resource_id) AND execution_epoch=sqlc.arg(epoch) AND completed_at IS NULL;

-- name: ReleasePlatformLease :execrows
UPDATE network_platform_reconciliations SET lease_owner=NULL,lease_until=NULL,
 processed_generation=greatest(processed_generation,sqlc.arg(covered_generation)::bigint),
 evidence_hash=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.arg(evidence_hash)::text ELSE evidence_hash END,
 evidence_applied_at=CASE WHEN sqlc.arg(observed)::boolean THEN clock_timestamp() ELSE evidence_applied_at END,
 retry_not_before=CASE WHEN sqlc.arg(backoff)::boolean THEN clock_timestamp()+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' ELSE '1970-01-01 UTC'::timestamptz END,
 next_run_at=CASE WHEN requested_generation>sqlc.arg(covered_generation)::bigint AND NOT sqlc.arg(backoff)::boolean THEN clock_timestamp() ELSE clock_timestamp()+sqlc.arg(delay_micros)::bigint*interval '1 microsecond' END
WHERE resource_id=sqlc.arg(resource_id) AND lease_owner=sqlc.arg(owner)::uuid AND lease_epoch=sqlc.arg(epoch) AND lease_until>clock_timestamp();

-- name: NotifyPlatform :execrows
UPDATE network_platform_reconciliations SET requested_generation=requested_generation+1,
 next_run_at=greatest(retry_not_before,least(next_run_at,clock_timestamp())) WHERE resource_id=$1;

-- name: SaveDeviceProgress :execrows
UPDATE network_device_adoptions SET node_progress=$3 WHERE cluster_id=$1 AND resource_id=$2;

-- name: ObservationPlatformResources :many
SELECT p.*,r.requested_generation FROM network_platform_resources p JOIN network_platform_reconciliations r ON r.resource_id=p.resource_id WHERE p.cluster_id=$1;

-- name: PublicPoolTopology :one
SELECT p.resource_id,p.cluster_id,p.config_revision,p.mode,p.gateway_id,p.cidr,p.ovn_gateway_ip,p.excluded_ips,p.vlan_network_id,p.upstream_gateway_ip,
 r.provider_uid AS pool_uid,r.provider_images,COALESCE(g.provider_uid,'')::text AS gateway_uid,COALESCE(v.provider_uid,'')::text AS vlan_uid,
 COALESCE(d.provider_uid,'')::text AS device_config_uid
FROM network_public_pools p
JOIN network_platform_resources r ON r.cluster_id=p.cluster_id AND r.resource_id=p.resource_id
LEFT JOIN network_platform_resources g ON g.cluster_id=p.cluster_id AND g.resource_id=p.gateway_id
LEFT JOIN network_platform_resources v ON v.cluster_id=p.cluster_id AND v.resource_id=p.vlan_network_id
LEFT JOIN network_vlan_networks vl ON vl.cluster_id=p.cluster_id AND vl.resource_id=p.vlan_network_id
LEFT JOIN network_platform_resources d ON d.cluster_id=vl.cluster_id AND d.resource_id=vl.device_id
WHERE p.cluster_id=$1 AND p.resource_id=$2;

-- name: SavePlatformImages :execrows
UPDATE network_platform_resources SET provider_images=$3 WHERE cluster_id=$1 AND resource_id=$2;

-- name: RetireVlanSlot :execrows
UPDATE network_vlan_networks SET retired=true WHERE cluster_id=$1 AND resource_id=$2;

-- name: RetirePublicPoolSlot :execrows
UPDATE network_public_pools SET retired=true WHERE cluster_id=$1 AND resource_id=$2;

-- name: InsertIntranetPool :exec
INSERT INTO network_public_pools(resource_id,cluster_id,scope,mode,gateway_id,cidr,ovn_gateway_ip,excluded_ips,default_vpc_name,default_vpc_uid,intranet_networks)
VALUES(sqlc.arg(resource_id),sqlc.arg(cluster_id),'intranet','overlay',NULL,sqlc.arg(cidr),sqlc.arg(ovn_gateway_ip),sqlc.arg(excluded_ips),sqlc.arg(default_vpc_name),sqlc.arg(default_vpc_uid),sqlc.arg(intranet_networks));

-- name: GetDefaultIntranetPool :one
SELECT * FROM network_default_intranet_pools WHERE cluster_id=$1;

-- name: SetDefaultIntranetPool :exec
INSERT INTO network_default_intranet_pools(cluster_id,pool_id) VALUES($1,$2)
ON CONFLICT(cluster_id) DO UPDATE SET pool_id=excluded.pool_id,version=network_default_intranet_pools.version+1;

-- name: ListIntranetPools :many
SELECT r.* FROM network_platform_resources r JOIN network_public_pools p ON p.cluster_id=r.cluster_id AND p.resource_id=r.resource_id
WHERE r.cluster_id=sqlc.arg(cluster_id) AND p.scope='intranet'
 AND (sqlc.arg(name_filter)::text='' OR r.name=sqlc.arg(name_filter))
 AND ((sqlc.arg(state_filter)::text='' AND r.state<>'deleted') OR r.state=sqlc.arg(state_filter))
 AND (sqlc.arg(after_id)::text='' OR (r.created_at,r.resource_id)<(sqlc.arg(after_created_at)::timestamptz,sqlc.arg(after_id)::text))
ORDER BY r.created_at DESC,r.resource_id DESC LIMIT sqlc.arg(max_results)::integer;

-- name: CountPlatform :one
SELECT count(*) FROM network_platform_resources r WHERE r.cluster_id=sqlc.arg(cluster_id) AND r.kind=sqlc.arg(kind)
 AND (r.kind<>'public_pool' OR EXISTS (SELECT 1 FROM network_public_pools pool WHERE pool.cluster_id=r.cluster_id AND pool.resource_id=r.resource_id AND pool.scope='public'))
 AND (sqlc.arg(name_filter)::text='' OR r.name=sqlc.arg(name_filter))
 AND ((sqlc.arg(state_filter)::text='' AND r.state<>'deleted') OR r.state=sqlc.arg(state_filter));

-- name: CountIntranetPools :one
SELECT count(*) FROM network_platform_resources r JOIN network_public_pools p ON p.cluster_id=r.cluster_id AND p.resource_id=r.resource_id WHERE r.cluster_id=sqlc.arg(cluster_id) AND p.scope='intranet'
 AND (sqlc.arg(name_filter)::text='' OR r.name=sqlc.arg(name_filter))
 AND ((sqlc.arg(state_filter)::text='' AND r.state<>'deleted') OR r.state=sqlc.arg(state_filter));

-- name: PlatformPoolVPCOverlaps :one
SELECT count(*)::bigint FROM network_vpcs v JOIN network_provider_bindings b ON b.tenant_id=v.tenant_id AND b.vpc_id=v.vpc_id AND b.resource_kind='vpc'
WHERE b.cluster_id=$1 AND v.state<>'deleted' AND v.cidr::cidr && sqlc.arg(cidr)::cidr;
