//go:build instanceintegration

package instance_test

import (
	"context"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	computedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/compute"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
)

// The existing database helper creates both task-owned login roles with the
// same private fixture password. Resolve the actual owner from PG metadata;
// never guess a deployment role or emit either DSN.
func computeMigrationOwner(t *testing.T, f *fixture) (*pgxpool.Pool, string, string) {
	t.Helper()
	ctx := context.Background()
	runtime, err := url.Parse(f.dsn)
	if err != nil {
		t.Fatal("invalid fixture runtime DSN")
	}
	role := runtime.User.Username()
	if !strings.HasPrefix(role, "instance_runtime_") {
		t.Fatal("refuse a role outside the task-owned instance fixture")
	}
	var owner, version string
	if err = f.runtime.QueryRow(ctx, `SELECT pg_get_userbyid(nspowner),current_setting('server_version_num') FROM pg_namespace WHERE nspname='instance'`).Scan(&owner, &version); err != nil {
		t.Fatal(err)
	}
	if version != "170011" {
		t.Fatalf("PG17.11 rehearsal requires server_version_num=170011; actual=%s", version)
	}
	if owner == role || !strings.HasPrefix(owner, "instance_owner_") {
		t.Fatal("fixture owner/runtime separation differs")
	}
	raw, err := os.ReadFile(os.Getenv("INSTANCE_TEST_ADMIN_DSN_FILE"))
	if err != nil {
		t.Fatal("explicit fixture admin DSN file required")
	}
	admin, err := pgxpool.New(ctx, strings.TrimSpace(string(raw)))
	clear(raw)
	if err != nil {
		t.Fatal("fixture admin unavailable")
	}
	if _, err = admin.Exec(ctx, "ALTER ROLE "+pgx.Identifier{role}.Sanitize()+" NOINHERIT"); err != nil {
		admin.Close()
		t.Fatal("cannot set task-owned runtime NOINHERIT")
	}
	admin.Close()
	password, _ := runtime.User.Password()
	runtime.User = url.UserPassword(owner, password)
	dsn := runtime.String()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("fixture owner unavailable")
	}
	t.Cleanup(pool.Close)
	t.Log("actual PostgreSQL server_version_num=170011; task-owned runtime NOINHERIT")
	return pool, dsn, role
}

func computeMigrationRows(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	tables := map[string]string{
		"instance.instances":                     "tenant_id,instance_id",
		"instance.instance_operations":           "tenant_id,operation_id",
		"instance.instance_work":                 "tenant_id,instance_id",
		"instance.workspace_bindings":            "tenant_id,instance_id",
		"instance.instance_quota_release_outbox": "tenant_id,instance_id",
		"instance.borrowed_volume_references":    "tenant_id,instance_id,mount_path",
		"instance.network_submissions":           "tenant_id,instance_id",
		"instance.gpu_cutover_audit":             "tenant_id,instance_id",
		"instance.schema_version":                "version",
	}
	out := map[string]string{}
	for table, order := range tables {
		var value string
		if err := pool.QueryRow(context.Background(), "SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY "+order+"),'[]'::jsonb)::text FROM "+table+" r").Scan(&value); err != nil {
			t.Fatal(table, err)
		}
		out[table] = value
	}
	return out
}

// These are pre-move durable Resource samples, built by the existing real
// repository and worker. Kubernetes and the remote Gov receipt are the existing explicit external boundaries. This proves PG
// state preservation; it makes no claim of live Pod/GPU or Gov-ledger proof.
func TestComputeMigrationPG17FrozenLifecycleRowsSurvive(t *testing.T) {
	for _, kind := range []string{biz.TypeCPU, biz.TypeGPU} {
		for _, phase := range []string{"pending", "running", "stopped", "cleanup"} {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				f := database(t)
				owner, ownerDSN, role := computeMigrationOwner(t, f)
				ctx := context.Background()
				if err := computedata.Restore(ctx, ownerDSN, role); err != nil {
					t.Fatal("prepare pre-move schema", err)
				}
				var originalOID uint32
				if err := owner.QueryRow(ctx, `SELECT 'instance.compute_presets'::regclass::oid`).Scan(&originalOID); err != nil {
					t.Fatal(err)
				}
				c := create()
				if kind == biz.TypeCPU {
					c = cpuCreate()
				}
				c.Spec.Labels = map[string]string{"example.com/migration": "preserved"}
				c.Spec.Annotations = map[string]string{"example.com/note": "frozen before metadata move"}
				c.Spec.Env = []biz.EnvVar{{Name: "MIGRATION_FIXTURE", Value: "preserved"}}
				c.Spec.WorkspaceMountPath = "/home/project"
				c.Spec.VolumeMounts = []biz.VolumeMount{{ClusterID: c.Spec.ClusterID, Namespace: c.Spec.Namespace, PVCName: "borrowed-data", PVCUID: "borrowed-pvc-original", MountPath: "/data", Ownership: "borrowed"}}
				ack, err := f.repo.AcceptCreate(ctx, c)
				if err != nil || !ack.Accepted {
					t.Fatal("real persistent admission", ack, err)
				}
				p, remote := &provider{}, &releaseEndpoint{}
				reporter, err := data.NewQuota(f.repo, remote)
				if err != nil {
					t.Fatal(err)
				}
				worker, err := biz.NewWorker(f.repo, p, "compute-migration-baseline")
				if err != nil {
					t.Fatal(err)
				}
				current, err := f.repo.Get(ctx, c.TenantID, c.ID)
				if err != nil {
					t.Fatal(err)
				}
				if phase != "pending" {
					for n := 0; n < 3; n++ {
						current = step(t, f, worker, c)
					}
					if current.Observed != biz.Running {
						t.Fatal("external-boundary fixture did not reach Running", current.Reason)
					}
				}
				if phase == "stopped" {
					command(t, f, c, current, "stop")
					for n := 0; n < 2; n++ {
						current = step(t, f, worker, c)
					}
					if current.Observed != biz.Stopped {
						t.Fatal("real stop intent did not persist", current.Reason)
					}
				}
				if phase == "cleanup" {
					command(t, f, c, current, "delete")
					for n := 0; n < 8; n++ {
						current = step(t, f, worker, c)
						if current.Observed == biz.Deleted && kind == biz.TypeGPU {
							if _, err := reporter.Dispatch(ctx); err == nil {
								t.Fatal("lost Gov receipt unexpectedly accepted")
							}
						}
						if kind == biz.TypeGPU && remote.calls == 1 {
							break
						}
						if kind == biz.TypeCPU && current.ComputeCleaned && current.QuotaReturned {
							break
						}
					}
					if !current.ComputeCleaned || !current.WorkspaceCleaned || current.CleanupCompletedAt.IsZero() {
						t.Fatal("sample lacks durably completed owner cleanup", current.Reason)
					}
					var events int
					var acknowledged bool
					if kind == biz.TypeGPU {
						if err = owner.QueryRow(ctx, `SELECT count(*),bool_or(acknowledged) FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2 AND original_create_operation_id=$3`, c.TenantID, c.ID, c.OperationID).Scan(&events, &acknowledged); err != nil || events != 1 || acknowledged || current.QuotaReturned || remote.calls != 1 {
							t.Fatal("real original-create outbox was not left awaiting receipt", events, acknowledged, err)
						}
					} else {
						if err = owner.QueryRow(ctx, `SELECT count(*) FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID).Scan(&events); err != nil || events != 0 || remote.calls != 0 {
							t.Fatal("CPU touched GPU release boundary", events, err)
						}
					}
				}
				before := computeMigrationRows(t, owner)
				baseline := current
				assertUnchanged := func(name string) {
					t.Helper()
					if got := computeMigrationRows(t, owner); !reflect.DeepEqual(before, got) {
						for table, want := range before {
							if got[table] != want {
								t.Error(name + " changed " + table)
							}
						}
						t.FailNow()
					}
					got, err := f.repo.Get(ctx, c.TenantID, c.ID)
					if err != nil || !reflect.DeepEqual(baseline, got) {
						t.Fatal(name+" changed decoded frozen instance", err)
					}
					replay, err := f.repo.AcceptCreate(ctx, c)
					if err != nil || replay != ack {
						t.Fatal(name+" changed original durable ACK", err)
					}
					if got := computeMigrationRows(t, owner); !reflect.DeepEqual(before, got) {
						t.Fatal(name + " replay mutated persistent rows")
					}
				}
				for _, stage := range []struct {
					name  string
					run   func(context.Context, string, string) error
					table string
				}{
					{"forward", computedata.Migrate, "compute.presets"},
					{"forward replay", computedata.Migrate, "compute.presets"},
					{"historical checksum replay", data.Migrate, "compute.presets"},
					{"inverse metadata recovery", computedata.Restore, "instance.compute_presets"},
					{"forward after recovery", computedata.Migrate, "compute.presets"},
				} {
					if err = stage.run(ctx, ownerDSN, role); err != nil {
						t.Fatal(stage.name, err)
					}
					var oid uint32
					if err = owner.QueryRow(ctx, "SELECT $1::regclass::oid", stage.table).Scan(&oid); err != nil || oid != originalOID {
						t.Fatal(stage.name+" changed preset relation identity", err)
					}
					assertUnchanged(stage.name)
				}
				if err = f.presets.CheckReady(ctx); err != nil {
					t.Fatal("new compute readiness after recovery", err)
				}
				if kind == biz.TypeGPU && phase == "cleanup" && (remote.calls != 1 || remote.applied != 1) {
					t.Fatal("metadata migration retried the external release")
				}
			})
		}
	}
}
