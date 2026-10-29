package compute

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	migrations "github.com/zhangzhe-ctrl/ani-resource-service/migrations/compute"
	historical "github.com/zhangzhe-ctrl/ani-resource-service/migrations/instance"
)

type Postgres struct{ pool *pgxpool.Pool }

func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid compute database configuration")
	}
	p := &Postgres{pool: pool}
	if err = p.CheckReady(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}
func (p *Postgres) Close() { p.pool.Close() }

func migrationChecksum() (string, error) {
	body, err := migrations.Files.ReadFile("001_presets.sql")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// CheckReady checks only the compute ledger and relation, never instance versions.
func (p *Postgres) CheckReady(ctx context.Context) error {
	return checkReady(ctx, p.pool)
}

type schemaReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func checkReady(ctx context.Context, db schemaReader) error {
	checksum, err := migrationChecksum()
	if err != nil {
		return err
	}
	var ready bool
	err = db.QueryRow(ctx, `SELECT
  (SELECT count(*)=1 AND bool_and(version=1 AND checksum=$1) FROM compute.schema_version)
  AND EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='compute' AND c.relname='presets' AND c.relkind='r')
  AND NOT EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='instance' AND c.relname='compute_presets')
  AND (SELECT count(*)=13 FROM pg_attribute WHERE attrelid='compute.presets'::regclass AND attnum>0 AND NOT attisdropped)
  AND (SELECT bool_and(attname||':'||format_type(atttypid,atttypmod)=ANY(ARRAY['id:uuid','code:text','name:text','description:text','compute_type:text','cpu_millis:bigint','memory_mib:bigint','enabled:boolean','gpu:jsonb','created_at:timestamp with time zone','updated_at:timestamp with time zone','created_by:text','updated_by:text'])) FROM pg_attribute WHERE attrelid='compute.presets'::regclass AND attnum>0 AND NOT attisdropped)
  AND EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='compute.presets'::regclass AND conname='presets_pkey' AND contype='p')
  AND EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='compute.presets'::regclass AND conname='presets_code_key' AND contype='u')
  AND EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='compute.presets'::regclass AND conname='presets_check' AND contype='c')
  AND to_regclass('compute.presets_listing') IS NOT NULL`, checksum).Scan(&ready)
	if err != nil || !ready {
		return fmt.Errorf("compute schema not migrated or target structure/checksum differs")
	}
	return nil
}

var historyFiles = []string{"001_instance.sql", "002_compute_presets.sql", "003_whole_gpu_quantity.sql", "004_gpu_execution.sql", "005_instance_configuration.sql", "006_instance_network_submissions.sql", "007_owner_cleanup_release.sql", "008_restart_pvc.sql"}

// Migrate is explicit; it never executes historical instance SQL.
func Migrate(ctx context.Context, dsn, runtimeRole string) error {
	return migrate(ctx, dsn, runtimeRole, false)
}

// Restore reverses only the metadata migration, preserving the same relation OID.
func Restore(ctx context.Context, dsn, runtimeRole string) error {
	return migrate(ctx, dsn, runtimeRole, true)
}

func migrate(ctx context.Context, dsn, role string, restore bool) error {
	if role == "" {
		return fmt.Errorf("compute runtime role required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("invalid compute migration configuration")
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='45s'; SELECT pg_advisory_xact_lock(84015001)`); err != nil {
		return err
	}
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1 AND rolname<>current_user AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolbypassrls AND NOT pg_has_role(oid,current_user,'MEMBER'))`, role).Scan(&valid); err != nil || !valid {
		return fmt.Errorf("distinct existing restricted compute runtime role required")
	}
	// Validate the complete immutable history before moving any metadata.
	var versions int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM instance.schema_version`).Scan(&versions); err != nil || versions != len(historyFiles) {
		return fmt.Errorf("unexpected instance migration history")
	}
	for i, name := range historyFiles {
		body, e := historical.Files.ReadFile(name)
		if e != nil {
			return e
		}
		sum := sha256.Sum256(body)
		var old string
		if e = tx.QueryRow(ctx, `SELECT checksum FROM instance.schema_version WHERE version=$1`, i+1).Scan(&old); e != nil || old != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("instance migration %s checksum differs", name)
		}
	}
	var oldTable, newTable, schema bool
	if err = tx.QueryRow(ctx, `SELECT to_regclass('instance.compute_presets') IS NOT NULL,to_regclass('compute.presets') IS NOT NULL,EXISTS(SELECT 1 FROM pg_namespace WHERE nspname='compute')`).Scan(&oldTable, &newTable, &schema); err != nil {
		return err
	}
	checksum, err := migrationChecksum()
	if err != nil {
		return err
	}
	if schema {
		if oldTable || !newTable {
			return fmt.Errorf("compute target conflict")
		}
		var known bool
		if err = tx.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(version=1 AND checksum=$1) FROM compute.schema_version`, checksum).Scan(&known); err != nil || !known {
			return fmt.Errorf("compute migration checksum differs")
		}
		if err = tx.QueryRow(ctx, `SELECT nspowner=(SELECT oid FROM pg_roles WHERE rolname=current_user) FROM pg_namespace WHERE nspname='compute'`).Scan(&valid); err != nil || !valid {
			return fmt.Errorf("compute schema belongs to another owner")
		}
	} else if restore {
		return fmt.Errorf("compute migration is not applied")
	} else if !oldTable || newTable {
		return fmt.Errorf("compute source/target conflict")
	}
	relation := "instance.compute_presets"
	if schema {
		relation = "compute.presets"
	}
	// Unknown FK/view/trigger/RLS dependencies require explicit operator review.
	if err = tx.QueryRow(ctx, `SELECT c.relkind='r' AND c.relowner=(SELECT oid FROM pg_roles WHERE rolname=current_user) AND NOT c.relrowsecurity AND NOT c.relforcerowsecurity
  AND NOT EXISTS(SELECT 1 FROM pg_constraint WHERE (conrelid=c.oid OR confrelid=c.oid) AND contype='f')
  AND NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid=c.oid AND NOT tgisinternal)
  AND NOT EXISTS(SELECT 1 FROM pg_depend d WHERE d.refobjid=c.oid AND d.classid='pg_rewrite'::regclass)
  FROM pg_class c WHERE c.oid=$1::regclass`, relation).Scan(&valid); err != nil || !valid {
		return fmt.Errorf("compute relation ownership or dependencies differ")
	}
	if restore || !schema {
		filename := "001_presets.sql"
		if restore {
			filename = "recovery/001_presets.sql"
		}
		body, e := migrations.Files.ReadFile(filename)
		if e != nil {
			return e
		}
		if _, err = tx.Exec(ctx, string(body)); err != nil {
			return err
		}
		if !restore {
			if _, err = tx.Exec(ctx, `INSERT INTO compute.schema_version(version,checksum) VALUES(1,$1)`, checksum); err != nil {
				return err
			}
		}
	}
	quoted := pgx.Identifier{role}.Sanitize()
	if restore {
		_, err = tx.Exec(ctx, `GRANT USAGE ON SCHEMA instance TO `+quoted+`; GRANT SELECT,INSERT,UPDATE,DELETE ON instance.compute_presets TO `+quoted)
	} else {
		_, err = tx.Exec(ctx, `REVOKE ALL ON SCHEMA compute FROM PUBLIC; REVOKE CREATE ON SCHEMA compute FROM `+quoted+`; GRANT USAGE ON SCHEMA compute TO `+quoted+`; GRANT SELECT ON compute.schema_version TO `+quoted+`; GRANT SELECT,INSERT,UPDATE,DELETE ON compute.presets TO `+quoted)
	}
	if err != nil {
		return err
	}
	if !restore {
		if err = checkReady(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
