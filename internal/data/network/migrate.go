package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zhangzhe-ctrl/ani-resource-service/migrations"
)

// Migrate is an explicit owner-only entry point. The runtime never calls it.
func Migrate(ctx context.Context, owner *pgxpool.Pool, runtimeRole string) error {
	if runtimeRole == "" {
		return fmt.Errorf("runtime role is required")
	}
	tx, err := owner.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(84013001)"); err != nil {
		return err
	}
	var ownRole string
	if err := tx.QueryRow(ctx, "SELECT current_user").Scan(&ownRole); err != nil {
		return err
	}
	if ownRole == runtimeRole {
		return fmt.Errorf("migration owner and runtime role must differ")
	}
	var foreignTables int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON c.relnamespace=n.oid
 WHERE n.nspname='public' AND c.relkind IN ('r','p') AND c.relname<>ALL($1::text[])`, append(append([]string{}, networkTables...), "network_schema_version")).Scan(&foreignTables); err != nil {
		return err
	}
	if foreignTables > 0 {
		return fmt.Errorf("migration requires an exclusive Network database")
	}
	var databaseName string
	if err := tx.QueryRow(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "REVOKE CREATE, TEMPORARY ON DATABASE "+pgx.Identifier{databaseName}.Sanitize()+" FROM PUBLIC"); err != nil {
		return err
	}
	var present bool
	if err := tx.QueryRow(ctx, "SELECT to_regclass('public.network_schema_version') IS NOT NULL").Scan(&present); err != nil {
		return err
	}
	if !present {
		var residue int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname = ANY($1::text[])",
			networkTables).Scan(&residue); err != nil {
			return err
		}
		if residue != 0 {
			return fmt.Errorf("unversioned Network schema already exists")
		}
	}
	if _, err := tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS network_schema_version (version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT clock_timestamp())"); err != nil {
		return err
	}
	files, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for index, name := range files {
		body, err := migrations.Files.ReadFile(name)
		if err != nil {
			return err
		}
		checksum := sha256.Sum256(body)
		want := hex.EncodeToString(checksum[:])
		var actual string
		err = tx.QueryRow(ctx, "SELECT checksum FROM network_schema_version WHERE version=$1", index+1).Scan(&actual)
		if err == nil {
			if actual != want {
				return fmt.Errorf("migration %s checksum differs from applied schema", name)
			}
			continue
		}
		if err != pgx.ErrNoRows {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO network_schema_version(version,checksum) VALUES ($1,$2)", index+1, want); err != nil {
			return err
		}
	}
	role := pgx.Identifier{runtimeRole}.Sanitize()
	if _, err := tx.Exec(ctx, "REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE ON SCHEMA public TO "+role); err != nil {
		return err
	}
	for _, table := range networkTables {
		name := pgx.Identifier{"public", table}.Sanitize()
		permissions := "SELECT, INSERT, UPDATE"
		if table == "network_lb_retired_bindings" || table == "network_idempotency" || table == "network_resource_history" || table == "network_platform_idempotency" || table == "network_platform_history" || table == "network_virtual_switch_idempotency" || table == "network_virtual_switch_history" || table == "network_legacy_device_retirement_idempotency" || table == "network_legacy_device_retirement_history" || table == "network_default_vpc_egress_idempotency" || table == "network_default_vpc_egress_history" {
			permissions = "SELECT, INSERT"
		}
		if _, err := tx.Exec(ctx, "REVOKE ALL ON "+name+" FROM PUBLIC; REVOKE ALL ON "+name+" FROM "+role+"; GRANT "+permissions+" ON "+name+" TO "+role); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "GRANT SELECT ON network_schema_version TO "+role); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var networkTables = []string{
	"network_vm_namespaces",
	"network_default_vpc_egresses", "network_default_vpc_egress_operations", "network_default_vpc_egress_idempotency", "network_default_vpc_egress_history",
	"network_legacy_device_retirements", "network_legacy_device_retirement_idempotency", "network_legacy_device_retirement_history",
	"network_virtual_switches", "network_virtual_switch_operations", "network_virtual_switch_idempotency", "network_virtual_switch_reconciliations", "network_virtual_switch_history",
	"network_lb_retired_bindings", "network_lb_configuration_listeners", "network_lb_listener_members",
	"network_lb_listeners", "network_lb_configurations", "network_lb_members", "network_lb_configuration_members", "network_lb_subnet_refs", "network_lb_components", "network_lb_generated_resources", "network_lb_capabilities",
	"network_default_intranet_pools", "network_load_balancers", "network_eip_claims", "network_lb_vip_intents", "network_vpc_base_connectivity", "network_connectivity_rollout", "network_base_backfill_runs", "network_base_backfill_candidates",
	"network_tenant_namespaces", "network_platform_resources", "network_device_adoptions", "network_vlan_networks", "network_egress_gateways", "network_public_pools", "network_default_public_pools", "network_platform_operations", "network_platform_idempotency", "network_platform_reconciliations", "network_platform_history", "network_eips", "network_snat_bindings",
	"network_attachment_consumer_snapshots", "network_subnets", "network_attachments", "network_attachment_history", "network_vpcs", "network_operations", "network_reconciliations",
	"network_idempotency", "network_provider_bindings", "network_resource_history",
}
