-- name: ListPlatformEIPs :many
SELECT e.eip_id,e.created_at FROM network_eips e
WHERE e.cluster_id=sqlc.arg(cluster_id)
 AND (sqlc.arg(tenant_filter)::text='' OR e.tenant_id=NULLIF(sqlc.arg(tenant_filter)::text,'')::uuid)
 AND (sqlc.arg(name_filter)::text='' OR position(lower(sqlc.arg(name_filter)) in lower(e.name))>0)
 AND (sqlc.arg(address_filter)::text='' OR e.address=sqlc.arg(address_filter))
 AND (sqlc.arg(pool_filter)::text='' OR e.pool_id=sqlc.arg(pool_filter))
 AND (sqlc.arg(scope_filter)::text='' OR e.scope=sqlc.arg(scope_filter))
 AND ((sqlc.arg(state_filter)::text='' AND e.state<>'deleted') OR e.state=sqlc.arg(state_filter))
UNION ALL
SELECT r.eip_id,r.created_at FROM network_default_vpc_egresses r
WHERE r.cluster_id=sqlc.arg(cluster_id)
 AND sqlc.arg(tenant_filter)::text=''
 AND (sqlc.arg(name_filter)::text='' OR position(lower(sqlc.arg(name_filter)) in lower(r.name))>0)
 AND (sqlc.arg(address_filter)::text='' OR r.address=sqlc.arg(address_filter))
 AND (sqlc.arg(pool_filter)::text='' OR r.pool_id=sqlc.arg(pool_filter))
 AND (sqlc.arg(scope_filter)::text='' OR sqlc.arg(scope_filter)::text='public')
 AND ((sqlc.arg(state_filter)::text='' AND r.state<>'deleted') OR r.state=sqlc.arg(state_filter))
ORDER BY created_at DESC,eip_id DESC LIMIT sqlc.arg(max_results)::integer OFFSET sqlc.arg(page_offset)::bigint;

-- name: CountPlatformEIPs :one
SELECT ((SELECT count(*) FROM network_eips e
WHERE e.cluster_id=sqlc.arg(cluster_id)
 AND (sqlc.arg(tenant_filter)::text='' OR e.tenant_id=NULLIF(sqlc.arg(tenant_filter)::text,'')::uuid)
 AND (sqlc.arg(name_filter)::text='' OR position(lower(sqlc.arg(name_filter)) in lower(e.name))>0)
 AND (sqlc.arg(address_filter)::text='' OR e.address=sqlc.arg(address_filter))
 AND (sqlc.arg(pool_filter)::text='' OR e.pool_id=sqlc.arg(pool_filter))
 AND (sqlc.arg(scope_filter)::text='' OR e.scope=sqlc.arg(scope_filter))
 AND ((sqlc.arg(state_filter)::text='' AND e.state<>'deleted') OR e.state=sqlc.arg(state_filter))
) + (SELECT count(*) FROM network_default_vpc_egresses r
WHERE r.cluster_id=sqlc.arg(cluster_id)
 AND sqlc.arg(tenant_filter)::text=''
 AND (sqlc.arg(name_filter)::text='' OR position(lower(sqlc.arg(name_filter)) in lower(r.name))>0)
 AND (sqlc.arg(address_filter)::text='' OR r.address=sqlc.arg(address_filter))
 AND (sqlc.arg(pool_filter)::text='' OR r.pool_id=sqlc.arg(pool_filter))
 AND (sqlc.arg(scope_filter)::text='' OR sqlc.arg(scope_filter)::text='public')
 AND ((sqlc.arg(state_filter)::text='' AND r.state<>'deleted') OR r.state=sqlc.arg(state_filter))
))::bigint;

-- name: ListPlatformTenantEIPsByIDs :many
SELECT sqlc.embed(e),p.name AS pool_name,
 coalesce(c.snat_id,'')::text AS binding_id,coalesce(c.state,'unbound')::text AS binding_state,
 coalesce(c.target_kind,'')::text AS binding_target_kind,coalesce(c.snat_id,c.lb_id,'')::text AS binding_target_id
FROM network_eips e JOIN network_platform_resources p ON p.cluster_id=e.cluster_id AND p.resource_id=e.pool_id
LEFT JOIN network_eip_claims c ON c.tenant_id=e.tenant_id AND c.eip_id=e.eip_id AND c.released_at IS NULL
WHERE e.cluster_id=sqlc.arg(cluster_id) AND e.eip_id=ANY(sqlc.arg(eip_ids)::text[]);

-- name: GetPlatformEIP :one
SELECT sqlc.embed(e),p.name AS pool_name,
 coalesce(c.snat_id,'')::text AS binding_id,coalesce(c.state,'unbound')::text AS binding_state,
 coalesce(c.target_kind,'')::text AS binding_target_kind,coalesce(c.snat_id,c.lb_id,'')::text AS binding_target_id
FROM network_eips e JOIN network_platform_resources p ON p.cluster_id=e.cluster_id AND p.resource_id=e.pool_id
LEFT JOIN network_eip_claims c ON c.tenant_id=e.tenant_id AND c.eip_id=e.eip_id AND c.released_at IS NULL
WHERE e.cluster_id=sqlc.arg(cluster_id) AND e.eip_id=sqlc.arg(eip_id);

-- name: ListPlatformSystemEIPsByIDs :many
SELECT sqlc.embed(r),p.name AS pool_name FROM network_default_vpc_egresses r
JOIN network_platform_resources p ON p.cluster_id=r.cluster_id AND p.resource_id=r.pool_id
WHERE r.cluster_id=sqlc.arg(cluster_id) AND r.eip_id=ANY(sqlc.arg(eip_ids)::text[]);
