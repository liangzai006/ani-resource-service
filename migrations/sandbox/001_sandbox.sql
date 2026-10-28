CREATE SCHEMA sandbox;
CREATE TABLE sandbox.schema_version (version integer PRIMARY KEY, checksum text NOT NULL);
CREATE TABLE sandbox.sandboxes (
 tenant_id uuid NOT NULL,
 sandbox_id uuid NOT NULL,
 instance_id text NOT NULL,
 version bigint NOT NULL CHECK(version > 0),
 desired text NOT NULL CHECK(desired IN ('running','paused','deleted')),
 observed text NOT NULL,
 reason text NOT NULL DEFAULT '',
 create_operation_id uuid NOT NULL,
 last_operation_id uuid NOT NULL,
 snapshot jsonb NOT NULL,
 provider_refs jsonb NOT NULL DEFAULT '{}'::jsonb,
 observed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,sandbox_id),
 UNIQUE(tenant_id,create_operation_id)
);
CREATE TABLE sandbox.operations (
 tenant_id uuid NOT NULL,
 operation_id uuid NOT NULL,
 sandbox_id uuid NOT NULL,
 action text NOT NULL,
 idempotency_key text NOT NULL,
 request_digest text NOT NULL DEFAULT '',
 intent_version bigint NOT NULL,
 state text NOT NULL,
 reason text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,operation_id),
 UNIQUE(tenant_id,sandbox_id,action,idempotency_key),
 FOREIGN KEY(tenant_id,sandbox_id) REFERENCES sandbox.sandboxes(tenant_id,sandbox_id)
);
CREATE TABLE sandbox.work (
 tenant_id uuid NOT NULL,
 sandbox_id uuid NOT NULL,
 due_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 lease_owner text NOT NULL DEFAULT '',
 lease_epoch bigint NOT NULL DEFAULT 0,
 lease_until timestamptz NOT NULL DEFAULT '-infinity',
 PRIMARY KEY(tenant_id,sandbox_id),
 FOREIGN KEY(tenant_id,sandbox_id) REFERENCES sandbox.sandboxes(tenant_id,sandbox_id)
);
CREATE INDEX sandbox_work_due ON sandbox.work(due_at,lease_until);
CREATE INDEX sandbox_instance_created ON sandbox.sandboxes(tenant_id,instance_id,created_at DESC);
