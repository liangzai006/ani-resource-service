-- name: InsertTenantRegistration :one
INSERT INTO image.registrations(image_id,space_id,owner_scope,tenant_id,display_name,description,repository,source_reference,digest,media_type,platforms,purposes,accelerator,created_by,updated_by)
VALUES(sqlc.arg(image_id),sqlc.arg(space_id),'tenant',sqlc.arg(tenant_id),sqlc.arg(display_name),sqlc.arg(description),sqlc.arg(repository),sqlc.arg(source_reference),sqlc.arg(digest),sqlc.arg(media_type),sqlc.arg(platforms),sqlc.arg(purposes),sqlc.arg(accelerator),sqlc.arg(actor),sqlc.arg(actor))
RETURNING *;

-- name: InsertPlatformRegistration :one
INSERT INTO image.registrations(image_id,space_id,owner_scope,tenant_id,display_name,description,repository,source_reference,digest,media_type,platforms,purposes,accelerator,created_by,updated_by)
VALUES(sqlc.arg(image_id),sqlc.arg(space_id),'platform',NULL,sqlc.arg(display_name),sqlc.arg(description),sqlc.arg(repository),sqlc.arg(source_reference),sqlc.arg(digest),sqlc.arg(media_type),sqlc.arg(platforms),sqlc.arg(purposes),sqlc.arg(accelerator),sqlc.arg(actor),sqlc.arg(actor))
RETURNING *;

-- name: GetTenantRegistration :one
SELECT * FROM image.registrations
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND image_id=sqlc.arg(image_id);

-- name: GetPlatformRegistration :one
SELECT * FROM image.registrations
WHERE owner_scope='platform' AND tenant_id IS NULL AND image_id=sqlc.arg(image_id);

-- name: FindTenantRegistrationsByReference :many
SELECT * FROM image.registrations
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND space_id=sqlc.arg(space_id)
 AND unregistered_at IS NULL AND repository=sqlc.arg(repository)
 AND ((sqlc.arg(is_digest)::boolean AND digest=sqlc.arg(reference)::text)
   OR (NOT sqlc.arg(is_digest)::boolean AND source_reference=sqlc.arg(reference)::text))
ORDER BY image_id LIMIT 2;

-- name: FindPlatformRegistrationsByReference :many
SELECT * FROM image.registrations
WHERE owner_scope='platform' AND tenant_id IS NULL AND space_id=sqlc.arg(space_id)
 AND unregistered_at IS NULL AND repository=sqlc.arg(repository)
 AND ((sqlc.arg(is_digest)::boolean AND digest=sqlc.arg(reference)::text)
   OR (NOT sqlc.arg(is_digest)::boolean AND source_reference=sqlc.arg(reference)::text))
ORDER BY image_id LIMIT 2;

-- name: ListTenantRegistrations :many
SELECT * FROM image.registrations
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND unregistered_at IS NULL
 AND (sqlc.arg(search_text)::text='' OR display_name ILIKE sqlc.arg(search_pattern)::text ESCAPE E'\\'
  OR repository ILIKE sqlc.arg(search_pattern)::text ESCAPE E'\\')
 AND (cardinality(sqlc.arg(purposes)::text[])=0 OR purposes && sqlc.arg(purposes)::text[])
 AND (sqlc.arg(accelerator)::text='' OR accelerator=sqlc.arg(accelerator))
 AND (NOT sqlc.arg(has_cursor)::boolean OR (created_at,image_id)<(sqlc.arg(after_created_at)::timestamptz,sqlc.arg(after_image_id)::text))
ORDER BY created_at DESC,image_id DESC LIMIT sqlc.arg(fetch_limit);

-- name: ListPlatformRegistrations :many
SELECT * FROM image.registrations
WHERE owner_scope='platform' AND tenant_id IS NULL AND unregistered_at IS NULL
 AND (sqlc.arg(search_text)::text='' OR display_name ILIKE sqlc.arg(search_pattern)::text ESCAPE E'\\'
  OR repository ILIKE sqlc.arg(search_pattern)::text ESCAPE E'\\')
 AND (cardinality(sqlc.arg(purposes)::text[])=0 OR purposes && sqlc.arg(purposes)::text[])
 AND (sqlc.arg(accelerator)::text='' OR accelerator=sqlc.arg(accelerator))
 AND (NOT sqlc.arg(has_cursor)::boolean OR (created_at,image_id)<(sqlc.arg(after_created_at)::timestamptz,sqlc.arg(after_image_id)::text))
ORDER BY created_at DESC,image_id DESC LIMIT sqlc.arg(fetch_limit);

-- name: UpdateTenantMetadata :one
UPDATE image.registrations SET display_name=sqlc.arg(display_name),description=sqlc.arg(description),
 purposes=sqlc.arg(purposes),accelerator=sqlc.arg(accelerator),updated_by=sqlc.arg(actor),updated_at=clock_timestamp(),version=version+1
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND image_id=sqlc.arg(image_id)
 AND unregistered_at IS NULL AND version=sqlc.arg(expected_version)
RETURNING *;

-- name: UpdatePlatformMetadata :one
UPDATE image.registrations SET display_name=sqlc.arg(display_name),description=sqlc.arg(description),
 purposes=sqlc.arg(purposes),accelerator=sqlc.arg(accelerator),updated_by=sqlc.arg(actor),updated_at=clock_timestamp(),version=version+1
WHERE owner_scope='platform' AND tenant_id IS NULL AND image_id=sqlc.arg(image_id)
 AND unregistered_at IS NULL AND version=sqlc.arg(expected_version)
RETURNING *;

-- name: UnregisterTenantRegistration :one
UPDATE image.registrations SET unregistered_at=clock_timestamp(),updated_at=clock_timestamp(),updated_by=sqlc.arg(actor),version=version+1
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND image_id=sqlc.arg(image_id)
 AND unregistered_at IS NULL AND version=sqlc.arg(expected_version)
RETURNING *;

-- name: UnregisterPlatformRegistration :one
UPDATE image.registrations SET unregistered_at=clock_timestamp(),updated_at=clock_timestamp(),updated_by=sqlc.arg(actor),version=version+1
WHERE owner_scope='platform' AND tenant_id IS NULL AND image_id=sqlc.arg(image_id)
 AND unregistered_at IS NULL AND version=sqlc.arg(expected_version)
RETURNING *;

-- name: CountTenantRegistrations :one
SELECT count(*) FROM image.registrations
WHERE owner_scope='tenant' AND tenant_id=sqlc.arg(tenant_id) AND unregistered_at IS NULL
 AND (sqlc.arg(search_text)::text='' OR display_name ILIKE sqlc.arg(search_pattern)::text ESCAPE E'\\'
  OR repository ILIKE sqlc.arg(search_pattern)::text ESCAPE E'\\')
 AND (cardinality(sqlc.arg(purposes)::text[])=0 OR purposes && sqlc.arg(purposes)::text[])
 AND (sqlc.arg(accelerator)::text='' OR accelerator=sqlc.arg(accelerator))
;

-- name: CountPlatformRegistrations :one
SELECT count(*) FROM image.registrations
WHERE owner_scope='platform' AND tenant_id IS NULL AND unregistered_at IS NULL
 AND (sqlc.arg(search_text)::text='' OR display_name ILIKE sqlc.arg(search_pattern)::text ESCAPE E'\\'
  OR repository ILIKE sqlc.arg(search_pattern)::text ESCAPE E'\\')
 AND (cardinality(sqlc.arg(purposes)::text[])=0 OR purposes && sqlc.arg(purposes)::text[])
 AND (sqlc.arg(accelerator)::text='' OR accelerator=sqlc.arg(accelerator))
;
