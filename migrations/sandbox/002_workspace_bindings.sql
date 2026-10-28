CREATE TABLE sandbox.workspace_bindings (
 tenant_id uuid NOT NULL,
 sandbox_id uuid NOT NULL,
 namespace text NOT NULL,
 pvc_name text NOT NULL,
 pvc_uid text NOT NULL DEFAULT '',
 cleanup_receipt text NOT NULL DEFAULT '',
 PRIMARY KEY(tenant_id,sandbox_id),
 FOREIGN KEY(tenant_id,sandbox_id) REFERENCES sandbox.sandboxes(tenant_id,sandbox_id)
);
