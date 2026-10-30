CREATE TABLE sandbox.file_operations (
 tenant_id uuid NOT NULL,
 sandbox_id uuid NOT NULL,
 operation_id uuid NOT NULL,
 idempotency_key text NOT NULL,
 request_digest text NOT NULL,
 target jsonb NOT NULL,
 state text NOT NULL CHECK (state IN ('accepted','dispatching','succeeded','failed','unknown','cancelled')),
 reason text NOT NULL DEFAULT '',
 result jsonb NOT NULL DEFAULT '{}'::jsonb,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id, operation_id),
 UNIQUE (tenant_id, sandbox_id, idempotency_key),
 FOREIGN KEY (tenant_id, sandbox_id) REFERENCES sandbox.sandboxes(tenant_id, sandbox_id)
);
