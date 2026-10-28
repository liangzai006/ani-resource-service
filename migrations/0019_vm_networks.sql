-- One tenant network namespace owns its reusable VM primary NAD and workload
-- identity. Intents precede Kubernetes writes; UIDs are pinned on first receipt.
CREATE TABLE network_vm_namespaces (
 tenant_id uuid NOT NULL,
 cluster_id text NOT NULL,
 namespace text NOT NULL,
 namespace_uid text NOT NULL CHECK (namespace_uid<>''),
 management_id uuid NOT NULL UNIQUE,
 template_id text NOT NULL CHECK (template_id='kcn-single-plugin-v1'),
 nad_name text NOT NULL CHECK (nad_name<>''),
 nad_uid text NOT NULL DEFAULT '',
 nad_config text NOT NULL CHECK (jsonb_typeof(nad_config::jsonb)='object'),
 nad_config_digest text NOT NULL CHECK (nad_config_digest ~ '^[0-9a-f]{64}$'),
 service_account text NOT NULL CHECK (service_account<>''),
 service_account_uid text NOT NULL DEFAULT '',
 ready boolean NOT NULL DEFAULT false,
 reason text NOT NULL DEFAULT '',
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY (tenant_id,cluster_id),
 UNIQUE (cluster_id,namespace,nad_name),
 FOREIGN KEY (tenant_id,cluster_id,namespace)
  REFERENCES network_tenant_namespaces(tenant_id,cluster_id,namespace),
 CHECK (NOT ready OR (nad_uid<>'' AND service_account_uid<>'' AND reason=''))
);
