-- Independent Sandbox listings are scoped by tenant, not by a parent instance.
-- Keep the historical instance_id column and its index for old rows and rollback.
CREATE INDEX sandbox_tenant_created ON sandbox.sandboxes(tenant_id,created_at DESC,sandbox_id DESC);
