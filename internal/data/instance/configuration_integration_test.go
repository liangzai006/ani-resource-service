//go:build instanceintegration

package instance_test

import (
	"context"
	"errors"
	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"testing"
)

func TestBorrowedVolumeReferencesPersistAndReleaseWithCompute(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := cpuCreate()
	c.Spec.ContractVersion = 2
	c.Spec.WorkspaceMountPath = "/work"
	c.Spec.VolumeMounts = []biz.VolumeMount{{ClusterID: c.Spec.ClusterID, Namespace: c.Spec.Namespace, PVCName: "datasets", PVCUID: uuid.NewString(), MountPath: "/data", ReadOnly: true, Ownership: "borrowed"}}
	c.Spec.Env = []biz.EnvVar{{Name: "APP_MODE", Value: "  literal  "}}
	if _, e := f.repo.AcceptCreate(ctx, c); e != nil {
		t.Fatal(e)
	}
	got, e := f.repo.Get(ctx, c.TenantID, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	if got.Spec.ContractVersion != 2 || got.Spec.VolumeMounts[0].PVCUID != c.Spec.VolumeMounts[0].PVCUID || got.Spec.Env[0].Value != "  literal  " {
		t.Fatal("frozen configuration changed")
	}
	var version int
	var state string
	if e = f.runtime.QueryRow(ctx, "SELECT instance_contract_version FROM instance.instances WHERE tenant_id=$1 AND instance_id=$2", c.TenantID, c.ID).Scan(&version); e != nil || version != 2 {
		t.Fatal("version not persisted", e)
	}
	check := func(want string) {
		t.Helper()
		if e := f.runtime.QueryRow(ctx, "SELECT state FROM instance.borrowed_volume_references WHERE tenant_id=$1 AND instance_id=$2 AND mount_path='/data'", c.TenantID, c.ID).Scan(&state); e != nil || state != want {
			t.Fatal("reference state", state, e)
		}
	}
	check("pending")
	if _, e = f.runtime.Exec(ctx, "INSERT INTO instance.borrowed_volume_references(tenant_id,instance_id,mount_path,cluster_id,namespace,pvc_name,pvc_uid,read_only) VALUES($1,$2,'/other',$3,$4,'other',$5,false)", uuid.NewString(), c.ID, c.Spec.ClusterID, c.Spec.Namespace, uuid.NewString()); e == nil {
		t.Fatal("cross-tenant reference accepted")
	}
	if _, e = f.repo.Get(ctx, uuid.NewString(), c.ID); !errors.Is(e, biz.ErrNotFound) {
		t.Fatal("cross-tenant instance visible", e)
	}
	// Only the Kubernetes/controller boundary is substituted. The real worker
	// must prove the CREATE template before PostgreSQL activates its references.
	boundary := &lifecycleBoundary{}
	worker, e := biz.NewWorker(f.repo, boundary, "config-test")
	if e != nil {
		t.Fatal(e)
	}
	for n := 0; n < 8; n++ {
		got = step(t, f, worker, c)
		if got.Observed == biz.Running && got.AppliedMountVersion == 1 && got.ActiveOperation == nil {
			break
		}
	}
	if got.Observed != biz.Running || got.AppliedMountVersion != 1 || got.ActiveOperation != nil {
		t.Fatalf("CREATE template was not proven: state=%s applied=%d reason=%s", got.Observed, got.AppliedMountVersion, got.Reason)
	}
	check("active")
	command(t, f, c, got, "delete")
	check("active")
	for n := 0; n < 8; n++ {
		got = step(t, f, worker, c)
		observation, err := boundary.Observe(ctx, got)
		if err != nil {
			t.Fatal(err)
		}
		if !observation.NotebookExists {
			break
		}
	}
	// The accepted DELETE and external deletion call cannot release references
	// before the following worker observation commits compute cleanup.
	if got.ComputeCleaned {
		t.Fatal("compute cleanup was claimed without the later observation")
	}
	check("active")
	for n := 0; n < 12; n++ {
		got = step(t, f, worker, c)
		if got.Observed == biz.Deleted && got.VolumeReferencesReleased && got.WorkspaceCleaned {
			break
		}
	}
	if got.Observed != biz.Deleted || !got.ComputeCleaned || !got.WorkspaceCleaned || !got.VolumeReferencesReleased {
		t.Fatal("DELETE did not release through real worker cleanup", got.Reason)
	}
	observation, e := boundary.Observe(ctx, got)
	if e != nil || observation.NotebookExists || observation.PodExists || observation.PVCExists || observation.VolumeExists {
		t.Fatal("DELETE left external compute or workspace behind", observation, e)
	}
	check("released")
	got, e = f.repo.Get(ctx, c.TenantID, c.ID)
	if e != nil || !got.VolumeReferencesReleased || got.Spec.VolumeMounts[0].PVCUID != c.Spec.VolumeMounts[0].PVCUID {
		t.Fatal("release lost frozen evidence", e)
	}
}
