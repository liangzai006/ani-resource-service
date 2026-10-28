//go:build instanceintegration

package vm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

func TestVMPostgresAcceptanceReplayVersionAndTenant(t *testing.T) {
	ctx := context.Background()
	db := testenv.NewDatabase(t)
	if e := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	p, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	tenant, id, op := uuid.NewString(), uuid.NewString(), uuid.NewString()
	a := biz.Acceptance{TenantID: tenant, VMID: id, OperationID: op, RequestDigest: stringDigest('a'), Spec: biz.Spec{Canonical: []byte(`{"immutable":"repository-fixture"}`), Digest: stringDigest('b'), Name: "test", InitialDisks: []biz.Disk{biz.NewInitialDisk(id, "system", "system", "system", 8)}}}
	a.Spec.DNS = &biz.DNS{Mode: "PLATFORM_DEFAULT", Nameservers: []string{"223.5.5.5"}}
	const count = 8
	var wg sync.WaitGroup
	wg.Add(count)
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			defer wg.Done()
			o, e := p.AcceptCreate(ctx, a)
			if e == nil && o.ID != op {
				e = errors.New("replay changed operation")
			}
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	var n int
	if e = p.pool.QueryRow(ctx, "SELECT count(*) FROM vm.operations WHERE tenant_id=$1 AND vm_id=$2", tenant, id).Scan(&n); e != nil || n != 1 {
		t.Fatalf("operation count %d: %v", n, e)
	}
	if _, e = p.Get(ctx, uuid.NewString(), id); !errors.Is(e, biz.ErrNotFound) {
		t.Fatalf("cross tenant: %v", e)
	}
	if _, e = db.Owner.Exec(ctx, "INSERT INTO vm.disks(tenant_id,vm_id,disk_id,volume_name,serial,record) VALUES($1,$2,$3,'foreign','foreign','{}')", uuid.NewString(), id, uuid.NewString()); e == nil {
		t.Fatal("cross tenant FK accepted")
	}
	bad := a
	bad.RequestDigest = stringDigest('c')
	if _, e = p.AcceptCreate(ctx, bad); !errors.Is(e, biz.ErrIdempotency) {
		t.Fatalf("changed create intent: %v", e)
	}
	o, v, e := p.Claim(ctx, uuid.NewString(), time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	o.State = "succeeded"
	if e = p.Save(ctx, o, v); e != nil {
		t.Fatal(e)
	}
	c := biz.Command{TenantID: tenant, VMID: id, Action: "start", Key: "same-key", RequestDigest: stringDigest('d'), Intent: []byte(`{"expected_version":1}`), ExpectedVersion: 1}
	first, e := p.AcceptCommand(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := p.AcceptCommand(ctx, c)
	if e != nil || first.ID != replay.ID {
		t.Fatalf("old version replay changed: %+v %v", replay, e)
	}
	changed := c
	changed.ExpectedVersion = 2
	changed.Intent = []byte(`{"expected_version":2}`)
	changed.RequestDigest = stringDigest('e')
	if _, e = p.AcceptCommand(ctx, changed); !errors.Is(e, biz.ErrIdempotency) {
		t.Fatalf("same key changed version: %v", e)
	}
	del := biz.Acceptance{TenantID: tenant, VMID: id, OperationID: uuid.NewString(), RequestDigest: stringDigest('f'), CreateOperationID: op, ExpectedVersion: 1}
	if _, e = p.AcceptDelete(ctx, del); !errors.Is(e, biz.ErrVersion) {
		t.Fatalf("delete version overwritten: %v", e)
	}
	del.ExpectedVersion = 2
	active, activeVM, e := p.Claim(ctx, uuid.NewString(), time.Minute)
	if e != nil || active.ID != first.ID {
		t.Fatalf("claim before delete: %+v %v", active, e)
	}
	deleteOp, e := p.AcceptDelete(ctx, del)
	if e != nil {
		t.Fatal(e)
	}
	c.Key = "new-after-close"
	c.ExpectedVersion = 3
	if _, e = p.AcceptCommand(ctx, c); !errors.Is(e, biz.ErrBusy) {
		t.Fatalf("closing admitted command: %v", e)
	}
	original := c
	original.Key = "same-key"
	original.ExpectedVersion = 1
	original.Intent = []byte(`{"expected_version":1}`)
	original.RequestDigest = stringDigest('d')
	if o, e = p.LookupReplay(ctx, original); e != nil || o.ID != first.ID {
		t.Fatalf("closing lost original replay: %v", e)
	}
	if o, e = p.AcceptDelete(ctx, del); e != nil || o.ID != deleteOp.ID {
		t.Fatalf("delete replay changed: %v", e)
	}
	// A separate connection observes accepted state after the request process exits.
	p2, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer p2.Close()
	if persisted, e := p2.Get(ctx, tenant, id); e != nil || persisted.Spec.DNS == nil || persisted.Spec.DNS.Mode != "PLATFORM_DEFAULT" || len(persisted.Spec.DNS.Nameservers) != 1 || persisted.Spec.DNS.Nameservers[0] != "223.5.5.5" {
		t.Fatalf("restart lost frozen DNS: %+v %v", persisted.Spec.DNS, e)
	}
	if o, e = p2.GetOperation(ctx, tenant, id, deleteOp.ID); e != nil || o.State != "accepted" {
		t.Fatalf("restart lost durable accept: %+v %v", o, e)
	}
	if _, _, e = p.Claim(ctx, uuid.NewString(), time.Minute); !errors.Is(e, biz.ErrNotFound) {
		t.Fatalf("delete raced live native worker lease: %v", e)
	}
	if e = p.Save(ctx, active, activeVM); !errors.Is(e, biz.ErrVersion) {
		t.Fatalf("old worker overwrote closing version: %v", e)
	}
	if _, e = db.Owner.Exec(ctx, "UPDATE vm.operations SET lease_until=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2", tenant, active.ID); e != nil {
		t.Fatal(e)
	}
	deleting, closed, e := p.Claim(ctx, uuid.NewString(), time.Minute)
	if e != nil || deleting.ID != deleteOp.ID {
		t.Fatalf("delete did not resume after older lease: %+v %v", deleting, e)
	}
	deleting.State = "succeeded"
	closed.Observed = "Deleted"
	if e = p.Save(ctx, deleting, closed); e != nil {
		t.Fatal(e)
	}
	if o, e = p.LookupReplay(ctx, original); e != nil || o.ID != first.ID || o.State != "failed" || o.Reason != "VM_SUPERSEDED_BY_DELETE" {
		t.Fatalf("old replay remained permanently running after delete: %+v %v", o, e)
	}
}
func stringDigest(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
