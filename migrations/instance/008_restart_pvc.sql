-- Explicit forward-only migration. All incompatible Instance/Storage executors
-- must be drained before applying; startup only verifies checksums.
ALTER TABLE instance.instances
 ADD COLUMN current_mounts jsonb,
 ADD COLUMN mounts_initialized boolean NOT NULL DEFAULT false,
 ADD COLUMN applied_mount_version bigint NOT NULL DEFAULT 0,
 ADD COLUMN runtime_desired text,
 ADD COLUMN recovery_fence jsonb;
ALTER TABLE instance.instance_operations ADD COLUMN execution jsonb;
ALTER TABLE instance.borrowed_volume_references
 DROP CONSTRAINT borrowed_volume_references_state_check,
 ADD COLUMN reference_generation bigint NOT NULL DEFAULT 1 CHECK(reference_generation>0),
 ADD COLUMN operation_id uuid,
 ADD COLUMN intent_version bigint;
ALTER TABLE instance.borrowed_volume_references
 ADD CONSTRAINT borrowed_volume_references_state_check CHECK(state IN ('pending','active','retiring','released')),
 ADD CONSTRAINT borrowed_volume_reference_operation_fk FOREIGN KEY(tenant_id,operation_id) REFERENCES instance.instance_operations(tenant_id,operation_id);
DROP INDEX instance.borrowed_volume_references_active;
CREATE INDEX borrowed_volume_references_protected ON instance.borrowed_volume_references(cluster_id,namespace,pvc_name)
 WHERE state IN ('pending','active','retiring');
CREATE TABLE instance.borrowed_pvc_guards (
 cluster_id uuid NOT NULL, namespace text NOT NULL, pvc_name text NOT NULL,
 tenant_id uuid NOT NULL,
 guard_epoch bigint NOT NULL DEFAULT 0 CHECK(guard_epoch>=0),
 ticket_id uuid, target_uid text NOT NULL DEFAULT '',
 phase text NOT NULL DEFAULT 'completed' CHECK(phase IN ('reserved','deleting','observing','completed','cancelled')),
 write_started boolean NOT NULL DEFAULT false,
 lease_owner text NOT NULL DEFAULT '', lease_epoch bigint NOT NULL DEFAULT 0,
 lease_until timestamptz NOT NULL DEFAULT '-infinity',
 retry_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 last_write_outcome text NOT NULL DEFAULT '', last_observation jsonb,
 last_reason text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(cluster_id,namespace,pvc_name),
 CHECK((ticket_id IS NULL AND target_uid='' AND phase='completed') OR (ticket_id IS NOT NULL AND target_uid<>''))
);
CREATE INDEX borrowed_pvc_guards_recovery ON instance.borrowed_pvc_guards(retry_at,lease_until) WHERE phase IN ('reserved','deleting','observing');
-- Fail rather than manufacture missing protection or infer identity from a
-- current same-name PVC. The operator receives the concrete inconsistent row.
DO $$
DECLARE bad record;
BEGIN
 SELECT r.tenant_id,r.instance_id,r.mount_path INTO bad
 FROM instance.borrowed_volume_references r JOIN instance.instances i USING(tenant_id,instance_id)
 WHERE r.state='active' AND NOT EXISTS (
  SELECT 1 FROM jsonb_array_elements(COALESCE(NULLIF(i.snapshot->'Spec'->'VolumeMounts','null'::jsonb),'[]'::jsonb)) m
  WHERE m->>'MountPath'=r.mount_path AND m->>'PVCUID'=r.pvc_uid AND m->>'PVCName'=r.pvc_name
   AND m->>'ClusterID'=r.cluster_id::text AND m->>'Namespace'=r.namespace AND (m->>'ReadOnly')::boolean=r.read_only)
 LIMIT 1;
 IF FOUND THEN RAISE EXCEPTION 'borrowed reference inconsistent: tenant %, instance %, path %',bad.tenant_id,bad.instance_id,bad.mount_path; END IF;
 SELECT i.tenant_id,i.instance_id,m->>'MountPath' AS mount_path INTO bad
 FROM instance.instances i CROSS JOIN LATERAL jsonb_array_elements(COALESCE(NULLIF(i.snapshot->'Spec'->'VolumeMounts','null'::jsonb),'[]'::jsonb)) m
 WHERE NOT (i.desired='deleted' AND (COALESCE((i.snapshot->>'VolumeReferencesReleased')::boolean,false) OR COALESCE(i.snapshot->>'CreateRequestHash','')=''))
 AND NOT EXISTS (SELECT 1 FROM instance.borrowed_volume_references r WHERE r.tenant_id=i.tenant_id AND r.instance_id=i.instance_id AND r.mount_path=m->>'MountPath' AND r.pvc_uid=m->>'PVCUID' AND r.state='active') LIMIT 1;
 IF FOUND THEN RAISE EXCEPTION 'borrowed reference missing: tenant %, instance %, path %',bad.tenant_id,bad.instance_id,bad.mount_path; END IF;
 IF EXISTS (SELECT 1 FROM instance.borrowed_volume_references WHERE state='active' GROUP BY cluster_id,namespace,pvc_name HAVING count(DISTINCT tenant_id)>1 OR count(DISTINCT pvc_uid)>1) THEN RAISE EXCEPTION 'borrowed guard tenant or UID conflict'; END IF;
END $$;
UPDATE instance.instances i SET runtime_desired=desired,
 current_mounts=CASE WHEN desired='deleted' AND (COALESCE((snapshot->>'VolumeReferencesReleased')::boolean,false) OR snapshot->>'CreateRequestHash'='') THEN '[]'::jsonb ELSE
  COALESCE((SELECT jsonb_agg(jsonb_build_object('cluster_id',m->>'ClusterID','namespace',m->>'Namespace','pvc_name',m->>'PVCName','pvc_uid',m->>'PVCUID','mount_path',m->>'MountPath','read_only',(m->>'ReadOnly')::boolean,'ownership','borrowed','reference_generation',1) ORDER BY ord)
  FROM jsonb_array_elements(COALESCE(NULLIF(i.snapshot->'Spec'->'VolumeMounts','null'::jsonb),'[]'::jsonb)) WITH ORDINALITY x(m,ord)),'[]'::jsonb) END,
 mounts_initialized=true;
INSERT INTO instance.borrowed_pvc_guards(cluster_id,namespace,pvc_name,tenant_id)
 SELECT DISTINCT cluster_id,namespace,pvc_name,tenant_id FROM instance.borrowed_volume_references WHERE state='active';
UPDATE instance.borrowed_volume_references r SET operation_id=i.create_operation_id,intent_version=1 FROM instance.instances i WHERE i.tenant_id=r.tenant_id AND i.instance_id=r.instance_id;
ALTER TABLE instance.instances
 ALTER COLUMN current_mounts SET NOT NULL, ALTER COLUMN current_mounts SET DEFAULT '[]'::jsonb,
 ALTER COLUMN runtime_desired SET NOT NULL,
 ADD CONSTRAINT current_mounts_array CHECK(jsonb_typeof(current_mounts)='array'),
 ADD CONSTRAINT runtime_desired_state CHECK(runtime_desired IN ('running','stopped','deleted')),
 ADD CONSTRAINT applied_mount_version_range CHECK(applied_mount_version>=0 AND applied_mount_version<=version),
 ADD CONSTRAINT recovery_fence_object CHECK(recovery_fence IS NULL OR jsonb_typeof(recovery_fence)='object');
ALTER TABLE instance.instance_operations ADD CONSTRAINT instance_execution_object CHECK(execution IS NULL OR (jsonb_typeof(execution)='object' AND execution->>'schema_version'='1'));
