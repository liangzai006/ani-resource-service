//go:build sandboxintegration

package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
)

type sandboxFixture struct {
	repo    *data.Postgres
	runtime *pgxpool.Pool
	dsn     string
}

func sandboxDatabase(t *testing.T) *sandboxFixture {
	t.Helper()
	ctx := context.Background()
	raw, err := os.ReadFile(os.Getenv("SANDBOX_TEST_ADMIN_DSN_FILE"))
	if err != nil {
		t.Fatal("exclusive sandbox database DSN file required")
	}
	u, err := url.Parse(strings.TrimSpace(string(raw)))
	if err != nil || u.User == nil {
		t.Fatal("invalid fixture DSN")
	}
	password, ok := u.User.Password()
	if !ok || password == "" {
		t.Fatal("fixture password required")
	}
	admin, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal("admin unavailable")
	}
	t.Cleanup(admin.Close)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	name, owner, role := "sandbox_test_"+suffix, "sandbox_owner_"+suffix, "sandbox_runtime_"+suffix
	literal := "'" + strings.ReplaceAll(password, "'", "''") + "'"
	for _, r := range []string{owner, role} {
		if _, err = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{r}.Sanitize()+" LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD "+literal); err != nil {
			t.Fatal("role creation failed")
		}
		r := r
		t.Cleanup(func() {
			if _, err := admin.Exec(ctx, "DROP ROLE "+pgx.Identifier{r}.Sanitize()); err != nil {
				t.Error("role cleanup failed")
			}
		})
	}
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER "+pgx.Identifier{owner}.Sanitize()); err != nil {
		t.Fatal("database creation failed")
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error("database cleanup failed")
		}
	})
	u.Path = "/" + name
	u.User = url.UserPassword(owner, password)
	for range 2 {
		if err = data.Migrate(ctx, u.String(), role); err != nil {
			t.Fatal("sandbox migration failed", err)
		}
	}
	u.User = url.UserPassword(role, password)
	repo, err := data.OpenPostgres(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Close)
	runtime, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal("runtime unavailable")
	}
	t.Cleanup(runtime.Close)
	var identity, superuser, bypassRLS, createDB, createRole, createSchema bool
	if err = runtime.QueryRow(ctx, `SELECT current_user=$1,rolsuper,rolbypassrls,rolcreatedb,rolcreaterole,
		has_schema_privilege(current_user,'sandbox','CREATE') FROM pg_roles WHERE rolname=current_user`, role).
		Scan(&identity, &superuser, &bypassRLS, &createDB, &createRole, &createSchema); err != nil {
		t.Fatal("runtime role inspection failed", err)
	}
	if !identity || superuser || bypassRLS || createDB || createRole || createSchema {
		t.Fatal("fixture runtime has owner or elevated privileges")
	}
	return &sandboxFixture{repo: repo, runtime: runtime, dsn: u.String()}
}

func TestSandboxRegisteredImageIdentityPersistsAndReplayRemainsExact(t *testing.T) {
	f := sandboxDatabase(t)
	tenant, id, operation := uuid.NewString(), uuid.NewString(), uuid.NewString()
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "governance:user:42"})
	c := biz.Create{TenantID: tenant, ID: id, OperationID: operation, RequestDigest: strings.Repeat("a", 64), IntentDigest: strings.Repeat("b", 64), Spec: biz.CreateSpec{
		Name: "registered-image", ImageID: "img_" + strings.Repeat("c", 32), ImageScope: "platform",
		ImageReference: "registry.invalid/platform/image@sha256:" + strings.Repeat("d", 64), ImageDigest: "sha256:" + strings.Repeat("d", 64),
	}}
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err := f.repo.Get(ctx, tenant, id)
	if err != nil || got.Snapshot.ImageID != c.Spec.ImageID || got.Snapshot.ImageScope != c.Spec.ImageScope || got.Snapshot.ImageReference != c.Spec.ImageReference {
		t.Fatalf("registered image identity was not frozen: %#v %v", got.Snapshot, err)
	}
	ack, err := f.repo.AcceptCreate(ctx, c)
	if err != nil || !ack.Replayed {
		t.Fatalf("same command did not replay: %#v %v", ack, err)
	}
	c.Spec.ImageScope = "tenant"
	if _, err = f.repo.AcceptCreate(ctx, c); !errors.Is(err, biz.ErrConflict) {
		t.Fatalf("changed image scope did not conflict: %v", err)
	}
}

func TestSandboxCreateReplayAcrossWorkerProgress(t *testing.T) {
	f := sandboxDatabase(t)
	tenant, id, operation := uuid.NewString(), uuid.NewString(), uuid.NewString()
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "governance:user:42"})
	c := biz.Create{TenantID: tenant, ID: id, InstanceID: uuid.NewString(), OperationID: operation,
		RequestDigest: strings.Repeat("a", 64), IntentDigest: strings.Repeat("e", 64), Spec: biz.CreateSpec{
			Name: "sandbox-replay", ImageReference: "registry.invalid/platform/work@sha256:" + strings.Repeat("b", 64),
			ImageDigest: "sha256:" + strings.Repeat("b", 64), PresetID: uuid.NewString(), PresetCode: "cpu-small",
			PresetName: "CPU small", ComputeType: "cpu", CPUMillis: 200, MemoryMiB: 128,
			SelectionDigest: strings.Repeat("c", 64), SpecDigest: strings.Repeat("d", 64), ClusterID: uuid.NewString(),
			Namespace: "sandbox-replay", NamespaceUID: "original-namespace-uid", RuntimeClass: "kata", Workspace: biz.WorkspaceSpec{
				MountPath: "/workspace", Size: "1Gi", StorageClass: "sandbox-block", ReclaimPolicy: "Delete"},
		}}
	ack, err := f.repo.AcceptCreate(ctx, c)
	if err != nil || ack != (biz.ACK{OperationID: operation, ResourceID: id, Accepted: true}) {
		t.Fatal("create failed", ack, err)
	}
	var hasImageID, hasImageScope bool
	if err = f.runtime.QueryRow(ctx, `SELECT snapshot ? 'ImageID', snapshot ? 'ImageScope' FROM sandbox.sandboxes WHERE tenant_id=$1 AND sandbox_id=$2`, tenant, id).Scan(&hasImageID, &hasImageScope); err != nil || hasImageID || hasImageScope {
		t.Fatalf("legacy snapshot representation changed: id=%v scope=%v err=%v", hasImageID, hasImageScope, err)
	}
	assertReplay := func(t *testing.T) {
		t.Helper()
		before, err := f.repo.Get(ctx, tenant, id)
		if err != nil {
			t.Fatal(err)
		}
		if before.Snapshot.NamespaceUID != c.Spec.NamespaceUID {
			t.Fatalf("namespace UID was not durably frozen: %q", before.Snapshot.NamespaceUID)
		}
		var beforeWork, afterWork []byte
		if err = f.runtime.QueryRow(ctx, `SELECT to_jsonb(w) FROM sandbox.work w WHERE tenant_id=$1 AND sandbox_id=$2`, tenant, id).Scan(&beforeWork); err != nil {
			t.Fatal(err)
		}
		replay, err := f.repo.AcceptCreate(ctx, c)
		expected := ack
		expected.Replayed = true
		if err != nil || replay != expected {
			t.Fatal("identical create did not replay durable ACK", replay, err)
		}
		after, err := f.repo.Get(ctx, tenant, id)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("replay changed persisted sandbox", err)
		}
		if err = f.runtime.QueryRow(ctx, `SELECT to_jsonb(w) FROM sandbox.work w WHERE tenant_id=$1 AND sandbox_id=$2`, tenant, id).Scan(&afterWork); err != nil {
			t.Fatal(err)
		}
		if string(beforeWork) != string(afterWork) {
			t.Fatal("replay changed durable work")
		}
		var sandboxes, operations, works int
		if err = f.runtime.QueryRow(ctx, `SELECT (SELECT count(*) FROM sandbox.sandboxes),
			(SELECT count(*) FROM sandbox.operations),(SELECT count(*) FROM sandbox.work)`).Scan(&sandboxes, &operations, &works); err != nil {
			t.Fatal(err)
		}
		if sandboxes != 1 || operations != 1 || works != 1 || after.Version != 1 {
			t.Fatal("replay duplicated persisted intent", sandboxes, operations, works, after.Version)
		}
	}
	if !t.Run("immediate", assertReplay) {
		return
	}
	work, err := f.repo.Claim(ctx, "sandbox-replay-worker", 30*time.Second)
	if err != nil || work == nil || work.Sandbox.ID != id {
		t.Fatal("worker claim failed", err)
	}
	running := work.Sandbox
	running.Observed = biz.ObservedRunning
	running.ObservedAt = time.Now().UTC()
	running.ProviderRefs = biz.ProviderRefs{PVCName: "workspace-" + id, PVCUID: uuid.NewString(),
		PodName: "sandbox-" + id, PodUID: uuid.NewString(), PVCResourceVersion: "10", PodResourceVersion: "11"}
	if err = f.repo.Finish(ctx, *work, biz.Progress{Sandbox: running, Next: time.Hour}); err != nil {
		t.Fatal("worker finish failed", err)
	}
	persisted, err := f.repo.Get(ctx, tenant, id)
	if err != nil || persisted.Observed != biz.ObservedRunning || persisted.ProviderRefs != running.ProviderRefs {
		t.Fatal("worker did not persist running refs", err)
	}
	op, err := f.repo.GetOperation(ctx, tenant, operation)
	if err != nil || op.State != "succeeded" {
		t.Fatal("create operation did not succeed", op.State, err)
	}
	t.Run("after_worker_progress", assertReplay)
	for _, test := range []struct {
		name   string
		change func(*biz.Create)
	}{
		{"name", func(c *biz.Create) { c.Spec.Name += "-changed" }},
		{"image", func(c *biz.Create) { c.Spec.ImageReference = "registry.invalid/other@" + c.Spec.ImageDigest }},
		{"preset", func(c *biz.Create) { c.Spec.PresetID = uuid.NewString() }},
		{"cpu", func(c *biz.Create) { c.Spec.CPUMillis++ }},
		{"workspace", func(c *biz.Create) { c.Spec.Workspace.Size = "2Gi" }},
		{"digest", func(c *biz.Create) { c.RequestDigest = strings.Repeat("e", 64) }},
		{"create_intent", func(c *biz.Create) { c.IntentDigest = strings.Repeat("f", 64) }},
		{"parent", func(c *biz.Create) { c.InstanceID = uuid.NewString() }},
		{"operation", func(c *biz.Create) { c.OperationID = uuid.NewString() }},
		{"resource", func(c *biz.Create) { c.ID = uuid.NewString() }},
		{"intent_and_digest", func(c *biz.Create) { c.Spec.Name += "-changed"; c.RequestDigest = strings.Repeat("f", 64) }},
	} {
		t.Run("conflict_"+test.name, func(t *testing.T) {
			changed := c
			test.change(&changed)
			if _, err := f.repo.AcceptCreate(ctx, changed); !errors.Is(err, biz.ErrConflict) {
				t.Fatal("changed create replay was not rejected", err)
			}
		})
	}
	t.Run("after_rejected_conflicts", assertReplay)
	var storedSpec biz.CreateSpec
	var raw []byte
	if err = f.runtime.QueryRow(ctx, `SELECT snapshot FROM sandbox.sandboxes WHERE tenant_id=$1 AND sandbox_id=$2`, tenant, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &storedSpec); err != nil || storedSpec != c.Spec {
		t.Fatal("worker or rejected replay changed immutable spec", err)
	}
}

func fixtureCreate() (context.Context, biz.Create) {
	tenant := uuid.NewString()
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "governance:user:42"})
	return ctx, biz.Create{TenantID: tenant, ID: uuid.NewString(), InstanceID: uuid.NewString(), OperationID: uuid.NewString(),
		RequestDigest: strings.Repeat("a", 64), IntentDigest: strings.Repeat("b", 64), Spec: biz.CreateSpec{
			Name: "sandbox-durable", ImageReference: "registry.invalid/work@sha256:" + strings.Repeat("a", 64),
			ImageDigest: "sha256:" + strings.Repeat("a", 64), PresetID: uuid.NewString(), PresetCode: "cpu-small", PresetName: "CPU Small",
			ComputeType: "cpu", CPUMillis: 200, MemoryMiB: 128, SelectionDigest: strings.Repeat("c", 64), SpecDigest: strings.Repeat("d", 64),
			ClusterID: uuid.NewString(), Namespace: "sandbox-durable", NamespaceUID: "original-namespace-uid", RuntimeClass: "sandbox-kata",
			Workspace: biz.WorkspaceSpec{MountPath: "/workspace", Size: "1Gi", StorageClass: "sandbox-rwo", ReclaimPolicy: "Delete"},
		}}
}

func acceptAndClaim(t *testing.T, f *sandboxFixture) (context.Context, biz.Create, *biz.Work) {
	t.Helper()
	ctx, c := fixtureCreate()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	return ctx, c, claimWork(t, f, ctx, "worker-1")
}

func claimWork(t *testing.T, f *sandboxFixture, ctx context.Context, owner string) *biz.Work {
	t.Helper()
	work, err := f.repo.Claim(ctx, owner, time.Minute)
	if err != nil || work == nil {
		t.Fatal("claim failed", err)
	}
	return work
}

func TestSandboxConcurrentCreateReplay(t *testing.T) {
	f := sandboxDatabase(t)
	ctx, c := fixtureCreate()
	const deliveries = 12
	start := make(chan struct{})
	results := make(chan biz.ACK, deliveries)
	errs := make(chan error, deliveries)
	var wg sync.WaitGroup
	for range deliveries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ack, err := f.repo.AcceptCreate(ctx, c)
			results <- ack
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("concurrent exact delivery failed", err)
		}
	}
	fresh, replayed := 0, 0
	for ack := range results {
		if !ack.Accepted || ack.OperationID != c.OperationID || ack.ResourceID != c.ID {
			t.Fatal("wrong ACK", ack)
		}
		if ack.Replayed {
			replayed++
		} else {
			fresh++
		}
	}
	if fresh != 1 || replayed != deliveries-1 {
		t.Fatal("expected one durable acceptance", fresh, replayed)
	}
	var resources, operations, works int
	if err := f.runtime.QueryRow(ctx, `SELECT (SELECT count(*) FROM sandbox.sandboxes),(SELECT count(*) FROM sandbox.operations),(SELECT count(*) FROM sandbox.work)`).Scan(&resources, &operations, &works); err != nil || resources != 1 || operations != 1 || works != 1 {
		t.Fatal("duplicate persistence", resources, operations, works, err)
	}
	changed := c
	changed.ID = uuid.NewString()
	if _, err := f.repo.AcceptCreate(ctx, changed); !errors.Is(err, biz.ErrConflict) {
		t.Fatal("operation reused for another resource", err)
	}
}

func TestSandboxListPaginationAndSnapshot(t *testing.T) {
	f := sandboxDatabase(t)
	ctx, template := fixtureCreate()
	var ids []string
	for range 22 {
		c := template
		c.ID, c.OperationID = uuid.NewString(), uuid.NewString()
		if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	if _, err := f.runtime.Exec(ctx, "UPDATE sandbox.sandboxes SET created_at='2026-10-27T00:00:00Z' WHERE tenant_id=$1", template.TenantID); err != nil {
		t.Fatal(err)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	for _, test := range []struct {
		name         string
		page, size   uint32
		start, count int
	}{
		{"defaults", 0, 0, 0, 20}, {"first", 1, 5, 0, 5}, {"zero_defaults_first", 0, 5, 0, 5},
		{"second", 2, 5, 5, 5}, {"last", 5, 5, 20, 2}, {"past_end", 6, 5, 22, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			items, total, err := f.repo.List(ctx, template.TenantID, biz.ListFilter{InstanceID: template.InstanceID, Page: test.page, PageSize: test.size})
			if err != nil || total != 22 || len(items) != test.count {
				t.Fatal("pagination", len(items), total, err)
			}
			for i, item := range items {
				if item.ID != ids[test.start+i] || item.CreateIntentDigest != template.IntentDigest || item.CreateRequestDigest != template.RequestDigest {
					t.Fatal("unstable ordering or missing frozen command", item)
				}
			}
		})
	}
	if _, _, err := f.repo.List(ctx, template.TenantID, biz.ListFilter{InstanceID: template.InstanceID, PageSize: 101}); !errors.Is(err, biz.ErrInvalid) {
		t.Fatal("oversized page was accepted", err)
	}
	writer := make(chan error, 1)
	go func() {
		for range 30 {
			c := template
			c.ID, c.OperationID = uuid.NewString(), uuid.NewString()
			if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
				writer <- err
				return
			}
		}
		writer <- nil
	}()
	for range 60 {
		items, total, err := f.repo.List(ctx, template.TenantID, biz.ListFilter{InstanceID: template.InstanceID, Page: 1, PageSize: 100})
		if err != nil || int64(len(items)) != total {
			t.Fatal("items/count did not share a snapshot", len(items), total, err)
		}
	}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
}

func TestSandboxOperationSuccessRemainsTerminal(t *testing.T) {
	f := sandboxDatabase(t)
	ctx, c, work := acceptAndClaim(t, f)
	progress := work.Sandbox
	progress.Observed, progress.ObservedAt = biz.ObservedRunning, time.Now().UTC()
	if err := f.repo.Finish(ctx, *work, biz.Progress{Sandbox: progress, Next: time.Nanosecond}); err != nil {
		t.Fatal(err)
	}
	for _, observed := range []string{biz.ObservedUnknown, biz.ObservedProvisioning} {
		work = claimWork(t, f, ctx, "worker-2")
		progress = work.Sandbox
		progress.Observed, progress.Reason = observed, "PROVIDER_OBSERVATION_FAILED"
		if err := f.repo.Finish(ctx, *work, biz.Progress{Sandbox: progress, Next: time.Nanosecond}); err != nil {
			t.Fatal(err)
		}
		op, err := f.repo.GetOperation(ctx, c.TenantID, c.OperationID)
		if err != nil || op.State != "succeeded" || op.Reason != "" {
			t.Fatal("terminal success changed with later observation", op, err)
		}
	}
}

func TestSandboxMutationJournalRestartAndLateIntent(t *testing.T) {
	f := sandboxDatabase(t)
	ctx, c, work := acceptAndClaim(t, f)
	mutation := biz.Mutation{ID: uuid.NewString(), Kind: biz.MutationCreateWorkspace, OperationID: c.OperationID, Version: work.Sandbox.Version}
	if err := f.repo.PrepareMutation(ctx, *work, mutation); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.PrepareMutation(ctx, *work, mutation); err != nil {
		t.Fatal("identical preparation was not idempotent", err)
	}
	f.repo.Close()
	reopened, err := data.OpenPostgres(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	f.repo = reopened
	t.Cleanup(reopened.Close)
	persisted, err := f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil || persisted.Pending == nil || *persisted.Pending != mutation || persisted.CreateIntentDigest != c.IntentDigest || persisted.CreateRequestDigest != c.RequestDigest {
		t.Fatal("journal/frozen command lost across reopen", persisted, err)
	}
	deleteID := uuid.NewString()
	if _, err = f.repo.Change(ctx, biz.Command{TenantID: c.TenantID, ID: c.ID, InstanceID: c.InstanceID, OperationID: deleteID, Action: "delete", IdempotencyKey: "delete", RequestDigest: strings.Repeat("f", 64), ExpectedVersion: 1}); err != nil {
		t.Fatal(err)
	}
	progress := work.Sandbox
	progress.Observed, progress.Reason = biz.ObservedRunning, "late stale observation"
	progress.Snapshot.Name = "must-not-rewrite-frozen-snapshot"
	refs := biz.ProviderRefs{PVCName: "workspace-" + c.ID, PVCUID: uuid.NewString(), PVName: "pv-" + c.ID, PVUID: uuid.NewString()}
	if err = f.repo.Finish(ctx, *work, biz.Progress{Sandbox: progress, Next: time.Hour, Receipt: &biz.MutationResult{Mutation: mutation, Outcome: biz.MutationApplied, Refs: refs}}); err != nil {
		t.Fatal("late attributable result lost", err)
	}
	persisted, err = f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil || persisted.Pending != nil || persisted.Version != 2 || persisted.Desired != biz.DesiredDeleted || persisted.LastOperationID != deleteID || persisted.Observed != biz.ObservedProvisioning || persisted.Snapshot.Name != c.Spec.Name || persisted.ProviderRefs != refs {
		t.Fatal("late result overwrote current intent or lost UID", persisted, err)
	}
	oldOp, err := f.repo.GetOperation(ctx, c.TenantID, c.OperationID)
	if err != nil || oldOp.State != "superseded" {
		t.Fatal("old operation changed", oldOp, err)
	}
	newOp, err := f.repo.GetOperation(ctx, c.TenantID, deleteID)
	if err != nil || newOp.State != "queued" {
		t.Fatal("new operation completed from old result", newOp, err)
	}
	next := claimWork(t, f, ctx, "delete-worker")
	if next.Sandbox.Version != 2 || next.Sandbox.Pending != nil || next.Sandbox.ProviderRefs != refs {
		t.Fatal("new intent not immediately recoverable", next)
	}
}

func TestSandboxUnknownMutationAndLeaseTakeover(t *testing.T) {
	f := sandboxDatabase(t)
	ctx, c, first := acceptAndClaim(t, f)
	mutation := biz.Mutation{ID: uuid.NewString(), Kind: biz.MutationCreateWorkspace, OperationID: c.OperationID, Version: first.Sandbox.Version}
	if err := f.repo.PrepareMutation(ctx, *first, mutation); err != nil {
		t.Fatal(err)
	}
	progress := first.Sandbox
	progress.Observed = biz.ObservedUnknown
	if err := f.repo.Finish(ctx, *first, biz.Progress{Sandbox: progress, Next: time.Nanosecond, Receipt: &biz.MutationResult{Mutation: mutation, Outcome: biz.MutationUnknown}}); err != nil {
		t.Fatal(err)
	}
	persisted, err := f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil || persisted.Pending == nil || *persisted.Pending != mutation {
		t.Fatal("unknown mutation journal disappeared", persisted, err)
	}
	successor := claimWork(t, f, ctx, "successor")
	if err = f.repo.PrepareMutation(ctx, *first, mutation); !errors.Is(err, biz.ErrLease) {
		t.Fatal("stale worker prepared Provider write", err)
	}
	replacement := mutation
	replacement.ID = uuid.NewString()
	if err = f.repo.PrepareMutation(ctx, *successor, replacement); !errors.Is(err, biz.ErrConflict) {
		t.Fatal("unresolved journal was overwritten", err)
	}
	refs := biz.ProviderRefs{PVCName: "workspace-" + c.ID, PVCUID: uuid.NewString(), PVName: "pv-" + c.ID, PVUID: uuid.NewString()}
	if err = f.repo.Finish(ctx, *first, biz.Progress{Sandbox: progress, Next: time.Hour, Receipt: &biz.MutationResult{Mutation: mutation, Outcome: biz.MutationApplied, Refs: refs}}); err != nil {
		t.Fatal("attributable receipt after takeover lost", err)
	}
	var owner string
	var epoch int64
	if err = f.runtime.QueryRow(ctx, "SELECT lease_owner,lease_epoch FROM sandbox.work WHERE tenant_id=$1 AND sandbox_id=$2", c.TenantID, c.ID).Scan(&owner, &epoch); err != nil || owner != successor.LeaseOwner || epoch != successor.LeaseEpoch {
		t.Fatal("late receipt released successor lease", owner, epoch, err)
	}
	// The successor claimed before the late receipt, so its original observed
	// refs are empty. Finishing that old read must preserve the merged UID.
	progress = successor.Sandbox
	progress.Observed = biz.ObservedRunning
	if err = f.repo.Finish(ctx, *successor, biz.Progress{Sandbox: progress, Next: time.Hour}); err != nil {
		t.Fatal(err)
	}
	persisted, err = f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil || persisted.Pending != nil || persisted.ProviderRefs != refs {
		t.Fatal("successor's stale observation erased late refs", persisted, err)
	}
}

func TestSandboxMutationPreparationRequiresActiveLease(t *testing.T) {
	f := sandboxDatabase(t)
	ctx, c, work := acceptAndClaim(t, f)
	if _, err := f.runtime.Exec(ctx, "UPDATE sandbox.work SET lease_until='-infinity' WHERE tenant_id=$1 AND sandbox_id=$2", c.TenantID, c.ID); err != nil {
		t.Fatal(err)
	}
	mutation := biz.Mutation{ID: uuid.NewString(), Kind: biz.MutationCreatePod, OperationID: c.OperationID, Version: 1}
	if err := f.repo.PrepareMutation(ctx, *work, mutation); !errors.Is(err, biz.ErrLease) {
		t.Fatal("expired lease prepared Provider mutation", err)
	}
	persisted, err := f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil || persisted.Pending != nil {
		t.Fatal("expired preparation created a journal", persisted, err)
	}
}

func TestSandboxMutationReceiptOutcomes(t *testing.T) {
	for _, outcome := range []string{biz.MutationApplied, biz.MutationRejected, biz.MutationNotSent, biz.MutationUnknown} {
		t.Run(outcome, func(t *testing.T) {
			f := sandboxDatabase(t)
			ctx, c, work := acceptAndClaim(t, f)
			mutation := biz.Mutation{ID: uuid.NewString(), Kind: biz.MutationCreatePod, OperationID: c.OperationID, Version: 1}
			if err := f.repo.PrepareMutation(ctx, *work, mutation); err != nil {
				t.Fatal(err)
			}
			wrong := mutation
			wrong.ID = uuid.NewString()
			if err := f.repo.Finish(ctx, *work, biz.Progress{Sandbox: work.Sandbox, Receipt: &biz.MutationResult{Mutation: wrong, Outcome: outcome}}); !errors.Is(err, biz.ErrConflict) {
				t.Fatal("unattributed receipt was accepted", err)
			}
			if err := f.repo.Finish(ctx, *work, biz.Progress{Sandbox: work.Sandbox, Receipt: &biz.MutationResult{Mutation: mutation, Outcome: outcome}, Next: time.Hour}); err != nil {
				t.Fatal(err)
			}
			persisted, err := f.repo.Get(ctx, c.TenantID, c.ID)
			if err != nil || (persisted.Pending != nil) != (outcome == biz.MutationUnknown) {
				t.Fatal("wrong journal outcome", persisted, err)
			}
		})
	}
}

func TestSandboxDeletedWaitsForPendingMutation(t *testing.T) {
	f := sandboxDatabase(t)
	ctx, c := fixtureCreate()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	deleteID := uuid.NewString()
	if _, err := f.repo.Change(ctx, biz.Command{TenantID: c.TenantID, ID: c.ID, InstanceID: c.InstanceID, OperationID: deleteID, Action: "delete", IdempotencyKey: "delete", RequestDigest: strings.Repeat("f", 64), ExpectedVersion: 1}); err != nil {
		t.Fatal(err)
	}
	work := claimWork(t, f, ctx, "delete-worker")
	mutation := biz.Mutation{ID: uuid.NewString(), Kind: biz.MutationDeleteWorkspace, OperationID: deleteID, Version: 2, TargetUID: uuid.NewString()}
	if err := f.repo.PrepareMutation(ctx, *work, mutation); err != nil {
		t.Fatal(err)
	}
	progress := work.Sandbox
	progress.Observed = biz.ObservedDeleted
	if err := f.repo.Finish(ctx, *work, biz.Progress{Sandbox: progress, Next: time.Nanosecond, Receipt: &biz.MutationResult{Mutation: mutation, Outcome: biz.MutationUnknown}}); err != nil {
		t.Fatal(err)
	}
	var terminal bool
	if err := f.runtime.QueryRow(ctx, "SELECT due_at='infinity'::timestamptz FROM sandbox.work WHERE tenant_id=$1 AND sandbox_id=$2", c.TenantID, c.ID).Scan(&terminal); err != nil || terminal {
		t.Fatal("unresolved mutation stopped reconciliation", terminal, err)
	}
	work = claimWork(t, f, ctx, "delete-worker-2")
	if err := f.repo.Finish(ctx, *work, biz.Progress{Sandbox: work.Sandbox, Receipt: &biz.MutationResult{Mutation: mutation, Outcome: biz.MutationNotSent}}); err != nil {
		t.Fatal(err)
	}
	if err := f.runtime.QueryRow(ctx, "SELECT due_at='infinity'::timestamptz FROM sandbox.work WHERE tenant_id=$1 AND sandbox_id=$2", c.TenantID, c.ID).Scan(&terminal); err != nil || !terminal {
		t.Fatal("settled deletion was not terminal", terminal, err)
	}
}

func forceSandboxWorkDue(t *testing.T, f *sandboxFixture, ctx context.Context, c biz.Create, expire bool) {
	t.Helper()
	query := "UPDATE sandbox.work SET due_at='-infinity' WHERE tenant_id=$1 AND sandbox_id=$2"
	if expire {
		query = "UPDATE sandbox.work SET due_at='-infinity',lease_until='-infinity' WHERE tenant_id=$1 AND sandbox_id=$2"
	}
	if _, err := f.runtime.Exec(ctx, query, c.TenantID, c.ID); err != nil {
		t.Fatal(err)
	}
}

func prepareSandboxUnknownDelete(t *testing.T, f *sandboxFixture) (context.Context, biz.Create, *biz.Work, biz.Mutation) {
	t.Helper()
	ctx, c, work := acceptAndClaim(t, f)
	mutation := biz.Mutation{ID: uuid.NewString(), Kind: biz.MutationDeletePod, OperationID: c.OperationID, Version: work.Sandbox.Version, TargetUID: "pod-original"}
	if err := f.repo.PrepareMutation(ctx, *work, mutation); err != nil {
		t.Fatal(err)
	}
	return ctx, c, work, mutation
}

func TestSandboxDeleteRetryAttemptFenceAndNegativeReceipts(t *testing.T) {
	for _, negative := range []string{biz.MutationNotSent, biz.MutationRejected} {
		t.Run(negative, func(t *testing.T) {
			f := sandboxDatabase(t)
			ctx, c, first, original := prepareSandboxUnknownDelete(t, f)
			forceSandboxWorkDue(t, f, ctx, c, true)
			successor := claimWork(t, f, ctx, "successor")
			retry := original
			retry.Attempt++
			retry.PreviousUncertain = true
			if err := f.repo.PrepareMutation(ctx, *successor, retry); err != nil {
				t.Fatal("retry preparation failed", err)
			}
			progress := successor.Sandbox
			progress.Observed = biz.ObservedUnknown
			if err := f.repo.Finish(ctx, *successor, biz.Progress{Sandbox: progress, Receipt: &biz.MutationResult{Mutation: retry, Outcome: biz.MutationUnknown}}); err != nil {
				t.Fatal(err)
			}
			if err := f.repo.Finish(ctx, *first, biz.Progress{Sandbox: first.Sandbox, Receipt: &biz.MutationResult{Mutation: original, Outcome: negative}}); !errors.Is(err, biz.ErrConflict) {
				t.Fatal("old-attempt negative was accepted", err)
			}
			stored, err := f.repo.Get(ctx, c.TenantID, c.ID)
			if err != nil || stored.Pending == nil || *stored.Pending != retry {
				t.Fatal("old receipt erased successor uncertainty", stored, err)
			}
			forceSandboxWorkDue(t, f, ctx, c, false)
			latest := claimWork(t, f, ctx, "latest")
			progress = latest.Sandbox
			progress.Observed = biz.ObservedRunning
			if err := f.repo.Finish(ctx, *latest, biz.Progress{Sandbox: progress, Receipt: &biz.MutationResult{Mutation: retry, Outcome: negative}}); err != nil {
				t.Fatal(err)
			}
			stored, err = f.repo.Get(ctx, c.TenantID, c.ID)
			if err != nil || stored.Pending == nil || *stored.Pending != retry {
				t.Fatal("successor negative erased earlier possible dispatch", stored, err)
			}
			op, err := f.repo.GetOperation(ctx, c.TenantID, c.OperationID)
			if err != nil || op.State != "unknown" {
				t.Fatal("pending uncertainty completed operation", op, err)
			}
			forceSandboxWorkDue(t, f, ctx, c, false)
			observer := claimWork(t, f, ctx, "observer")
			if err := f.repo.Finish(ctx, *observer, biz.Progress{Sandbox: observer.Sandbox, Receipt: &biz.MutationResult{Mutation: retry, Outcome: biz.MutationApplied}}); err != nil {
				t.Fatal("attributed current-attempt resolution rejected", err)
			}
			stored, err = f.repo.Get(ctx, c.TenantID, c.ID)
			if err != nil || stored.Pending != nil {
				t.Fatal("positive resolution did not clear pending", stored, err)
			}
		})
	}
}

func TestSandboxDeleteRetryPreparationFencesCurrentWorkAndOrigin(t *testing.T) {
	f := sandboxDatabase(t)
	ctx, c, first, original := prepareSandboxUnknownDelete(t, f)
	forceSandboxWorkDue(t, f, ctx, c, true)
	successor := claimWork(t, f, ctx, "successor")
	retry := original
	retry.Attempt = 1
	retry.PreviousUncertain = true
	cases := map[string]biz.Mutation{}
	invalid := retry
	invalid.PreviousUncertain = false
	cases["cleared uncertainty"] = invalid
	invalid = retry
	invalid.Attempt = 2
	cases["skipped attempt"] = invalid
	invalid = retry
	invalid.TargetUID = "replacement-pod"
	cases["target changed"] = invalid
	invalid = retry
	invalid.OperationID = uuid.NewString()
	cases["origin changed"] = invalid
	invalid = retry
	invalid.ID = uuid.NewString()
	cases["mutation identity changed"] = invalid
	invalid = retry
	invalid.Kind = biz.MutationCreatePod
	cases["create retry"] = invalid
	for name, mutation := range cases {
		if err := f.repo.PrepareMutation(ctx, *successor, mutation); !errors.Is(err, biz.ErrConflict) {
			t.Fatal(name, err)
		}
	}
	if err := f.repo.PrepareMutation(ctx, *first, retry); !errors.Is(err, biz.ErrLease) {
		t.Fatal("old lease prepared retry", err)
	}
	// A late negative can legitimately clear an original attempt if no successor
	// has advanced/dispatched. The already-claimed successor must not resurrect it.
	if err := f.repo.Finish(ctx, *first, biz.Progress{Sandbox: first.Sandbox, Receipt: &biz.MutationResult{Mutation: original, Outcome: biz.MutationNotSent}}); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.PrepareMutation(ctx, *successor, retry); !errors.Is(err, biz.ErrConflict) {
		t.Fatal("cleared journal resurrected", err)
	}
	stored, err := f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil || stored.Pending != nil {
		t.Fatal(stored, err)
	}
}

func TestSandboxDeleteRetryNewIntentAndAdvancedJournalFences(t *testing.T) {
	f := sandboxDatabase(t)
	ctx, c, _, original := prepareSandboxUnknownDelete(t, f)
	forceSandboxWorkDue(t, f, ctx, c, true)
	stale := claimWork(t, f, ctx, "stale-before-new-intent")
	deleteID := uuid.NewString()
	if _, err := f.repo.Change(ctx, biz.Command{TenantID: c.TenantID, ID: c.ID, InstanceID: c.InstanceID, OperationID: deleteID, Action: "delete", IdempotencyKey: "delete", RequestDigest: strings.Repeat("f", 64), ExpectedVersion: 1}); err != nil {
		t.Fatal(err)
	}
	retry := original
	retry.Attempt = 1
	retry.PreviousUncertain = true
	if err := f.repo.PrepareMutation(ctx, *stale, retry); !errors.Is(err, biz.ErrLease) {
		t.Fatal("old current-intent snapshot prepared retry", err)
	}
	forceSandboxWorkDue(t, f, ctx, c, true)
	fresh := claimWork(t, f, ctx, "fresh-new-intent")
	if err := f.repo.PrepareMutation(ctx, *fresh, retry); err != nil {
		t.Fatal("current work could not retry old-origin deletion", err)
	}
	next := retry
	next.Attempt++
	if err := f.repo.PrepareMutation(ctx, *fresh, next); !errors.Is(err, biz.ErrConflict) {
		t.Fatal("claimed old pending advanced twice", err)
	}
	for _, outcome := range []string{biz.MutationApplied, biz.MutationUnknown, biz.MutationNotSent} {
		if err := f.repo.Finish(ctx, *fresh, biz.Progress{Sandbox: fresh.Sandbox, Receipt: &biz.MutationResult{Mutation: original, Outcome: outcome}}); !errors.Is(err, biz.ErrConflict) {
			t.Fatal("old-attempt receipt accepted", outcome, err)
		}
	}
	stored, err := f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil || stored.Pending == nil || *stored.Pending != retry {
		t.Fatal("advanced journal changed", stored, err)
	}
}

func TestIndependentSandboxAndHistoricalRows(t *testing.T) {
	f := sandboxDatabase(t)
	ctx, legacy := fixtureCreate()
	legacy.InstanceID = "old-parent"
	if _, err := f.repo.AcceptCreate(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	independent := legacy
	independent.ID, independent.OperationID, independent.InstanceID = uuid.NewString(), uuid.NewString(), ""
	if _, err := f.repo.AcceptCreate(ctx, independent); err != nil {
		t.Fatal("independent create:", err)
	}
	if ack, err := f.repo.AcceptCreate(ctx, independent); err != nil || !ack.Replayed {
		t.Fatal("independent replay:", ack, err)
	}
	items, total, err := f.repo.List(ctx, legacy.TenantID, biz.ListFilter{})
	if err != nil || total != 2 || len(items) != 2 {
		t.Fatal("tenant list:", len(items), total, err)
	}
	for _, c := range []biz.Create{legacy, independent} {
		if _, err := f.repo.Change(ctx, biz.Command{TenantID: c.TenantID, ID: c.ID, OperationID: uuid.NewString(), Action: "pause", IdempotencyKey: "pause", RequestDigest: biz.ActionDigest("", 1, "pause"), ExpectedVersion: 1}); err != nil {
			t.Fatal("action without parent:", c.ID, err)
		}
	}
	// Convert one persisted action to its pre-upgrade digest, then replay it
	// using the standalone contract. The old parent is read from the row only.
	if _, err := f.runtime.Exec(ctx, "UPDATE sandbox.operations SET request_digest=$3 WHERE tenant_id=$1 AND sandbox_id=$2 AND action='pause'", legacy.TenantID, legacy.ID, biz.ActionDigest(legacy.InstanceID, 1, "pause")); err != nil {
		t.Fatal(err)
	}
	retry := biz.Command{TenantID: legacy.TenantID, ID: legacy.ID, OperationID: uuid.NewString(), Action: "pause", IdempotencyKey: "pause", RequestDigest: biz.ActionDigest("", 1, "pause"), ExpectedVersion: 1}
	if ack, err := f.repo.Change(ctx, retry); err != nil || !ack.Replayed {
		t.Fatal("historical action replay:", ack, err)
	}
	retry.ExpectedVersion = 2
	retry.RequestDigest = biz.ActionDigest("", 2, "pause")
	if _, err := f.repo.Change(ctx, retry); !errors.Is(err, biz.ErrConflict) {
		t.Fatal("changed historical action replay:", err)
	}
	otherTenant := uuid.NewString()
	otherCtx := biz.WithCaller(context.Background(), biz.Caller{TenantID: otherTenant, Actor: "governance:user:other"})
	if _, err := f.repo.Get(otherCtx, otherTenant, legacy.ID); !errors.Is(err, biz.ErrNotFound) {
		t.Fatal("cross tenant get:", err)
	}
	if _, total, err := f.repo.List(otherCtx, otherTenant, biz.ListFilter{}); err != nil || total != 0 {
		t.Fatal("cross tenant list:", total, err)
	}
	if _, err := f.repo.Change(otherCtx, biz.Command{TenantID: otherTenant, ID: legacy.ID, OperationID: uuid.NewString(), Action: "delete", IdempotencyKey: "delete", RequestDigest: strings.Repeat("f", 64)}); !errors.Is(err, biz.ErrNotFound) {
		t.Fatal("cross tenant change:", err)
	}
}

func TestTenantListingMigrationIsAppliedAndReady(t *testing.T) {
	f := sandboxDatabase(t) // runs Migrate twice and opens with CheckReady
	ctx := context.Background()
	var registered, indexExists bool
	if err := f.runtime.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sandbox.schema_version WHERE version=4), to_regclass('sandbox.sandbox_tenant_created') IS NOT NULL`).Scan(&registered, &indexExists); err != nil {
		t.Fatal(err)
	}
	if !registered || !indexExists {
		t.Fatalf("tenant listing migration missing: version=%v index=%v", registered, indexExists)
	}
	if err := f.repo.CheckReady(ctx); err != nil {
		t.Fatal("migrated schema not ready:", err)
	}
}
