//go:build instanceintegration

package instance_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	quotapb "github.com/zhangzhe-ctrl/ani-governance/api/quota/gen/go/quota/service/v1"
	terminal "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/terminal/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	computedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/compute"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/instance"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type fixture struct {
	repo        *data.Postgres
	presets     *computedata.Postgres
	runtime     *pgxpool.Pool
	dsn         string
	ownerDSN    string
	runtimeRole string
}

func database(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	raw, e := os.ReadFile(os.Getenv("INSTANCE_TEST_ADMIN_DSN_FILE"))
	if e != nil {
		t.Fatal("exclusive instance database DSN required")
	}
	u, e := url.Parse(string(raw))
	if e != nil {
		t.Fatal("invalid fixture DSN")
	}
	admin, e := pgxpool.New(ctx, string(raw))
	if e != nil {
		t.Fatal("admin unavailable")
	}
	t.Cleanup(admin.Close)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	name := "instance_test_" + suffix
	owner := "instance_owner_" + suffix
	role := "instance_runtime_" + suffix
	password, _ := u.User.Password()
	literal := "'" + strings.ReplaceAll(password, "'", "''") + "'"
	for _, r := range []string{owner, role} {
		if _, e = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{r}.Sanitize()+" LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD "+literal); e != nil {
			t.Fatal("role creation failed")
		}
		r := r
		t.Cleanup(func() {
			if _, e := admin.Exec(ctx, "DROP ROLE "+pgx.Identifier{r}.Sanitize()); e != nil {
				t.Error("role cleanup failed")
			}
		})
	}
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER "+pgx.Identifier{owner}.Sanitize()); e != nil {
		t.Fatal("database creation failed")
	}
	t.Cleanup(func() {
		if _, e := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); e != nil {
			t.Error("database cleanup failed")
		}
	})
	u.Path = "/" + name
	u.User = url.UserPassword(owner, password)
	if e = data.Migrate(ctx, u.String(), role); e != nil {
		t.Fatal(e)
	}
	if e = data.Migrate(ctx, u.String(), role); e != nil {
		t.Fatal("migration replay", e)
	}
	if e = computedata.Migrate(ctx, u.String(), role); e != nil {
		t.Fatal(e)
	}
	ownerDSN := u.String()
	u.User = url.UserPassword(role, password)
	repo, e := data.OpenPostgres(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(repo.Close)
	presets, e := computedata.OpenPostgres(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(presets.Close)
	runtime, e := pgxpool.New(ctx, u.String())
	if e != nil {
		t.Fatal("runtime unavailable")
	}
	t.Cleanup(runtime.Close)
	return &fixture{repo: repo, presets: presets, runtime: runtime, dsn: u.String(), ownerDSN: ownerDSN, runtimeRole: role}
}
func create() biz.Create {
	tenant, id, op := uuid.NewString(), uuid.NewString(), uuid.NewString()
	return biz.Create{TenantID: tenant, ID: id, OperationID: op, RequestHash: strings.Repeat("a", 64), BusinessDigest: strings.Repeat("b", 64), Spec: biz.Spec{ContractVersion: 2, WorkspaceMountPath: "/workspace", InstanceKind: biz.KindContainer, ComputeType: biz.TypeGPU, Scheduling: biz.Scheduling{PodPlacement: "NONE"}, Name: "workspace", ImageID: uuid.NewString(), ImageScope: "platform", ImageReference: "registry.invalid/platform/workspace@sha256:" + strings.Repeat("c", 64), ImageDigest: "sha256:" + strings.Repeat("c", 64), PresetID: "shared", CPU: "1", Memory: "2Gi", UID: 1000, GID: 1000, Shell: "/bin/bash", ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", ClusterID: uuid.NewString(), Namespace: "tenant-" + tenant, NotebookName: "instance-" + id, PVCName: "workspace-" + id, GPU: biz.GPU{Scheduler: "volcano", RuntimeClass: "nvidia", Queue: "default", Digest: strings.Repeat("d", 64), NodeLabels: map[string]string{"accelerator.ani.io/supply-group": "test"}, Annotations: map[string]string{"volcano.sh/vgpu-mode": "hami-core"}, Limits: map[string]string{"volcano.sh/vgpu-number": "1", "volcano.sh/vgpu-memory": "6144", "volcano.sh/vgpu-cores": "25"}}}}
}

func cpuCreate() biz.Create {
	c := create()
	c.Spec.ComputeType = biz.TypeCPU
	c.Spec.PresetID = "cpu-small"
	c.Spec.GPU = biz.GPU{}
	return c
}

func TestCPUInstancePersistentLifecycleAndTerminal(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := cpuCreate()
	ack, err := f.repo.AcceptCreate(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := f.repo.AcceptCreate(ctx, c); err != nil || replay != ack {
		t.Fatal("CPU admission did not replay its durable ACK", replay, err)
	}
	if _, err := f.repo.Get(ctx, uuid.NewString(), c.ID); !errors.Is(err, biz.ErrNotFound) {
		t.Fatal("CPU instance leaked across tenants", err)
	}
	p := &provider{}
	remote := &releaseEndpoint{}
	reporter, err := data.NewQuota(f.repo, remote)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := biz.NewWorker(f.repo, p, "cpu-worker")
	var i biz.Instance
	for n := 0; n < 3; n++ {
		i = step(t, f, w, c)
	}
	if i.Observed != biz.Running || i.Spec.ComputeType != biz.TypeCPU || !i.Spec.GPU.Empty() {
		t.Fatal("CPU instance did not reach running with its fixed specification", i.Reason)
	}
	authority := service.NewTerminalValidation(f.repo, p)
	target := func(i biz.Instance) *terminal.TerminalTarget {
		return &terminal.TerminalTarget{TenantId: i.TenantID, InstanceId: i.ID, Version: i.Version, ClusterId: i.Spec.ClusterID, Namespace: i.Spec.Namespace, NotebookName: i.Spec.NotebookName, NotebookUid: i.NotebookUID, PodName: i.PodName, PodUid: i.PodUID, ContainerName: biz.Container, Shell: i.Spec.Shell}
	}
	old := target(i)
	if _, err := authority.ValidateTerminalTarget(ctx, old); err != nil {
		t.Fatal("running CPU terminal rejected", err)
	}
	frozen, _ := json.Marshal(i.Spec)
	command(t, f, c, i, "stop")
	if _, err := authority.ValidateTerminalTarget(ctx, old); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("CPU stop intent retained old terminal authority", err)
	}
	for n := 0; n < 2; n++ {
		i = step(t, f, w, c)
	}
	if i.Observed != biz.Stopped || i.PVCUID != "pvc-original" {
		t.Fatal("CPU stop lost workspace binding", i)
	}
	command(t, f, c, i, "start")
	for n := 0; n < 2; n++ {
		i = step(t, f, w, c)
	}
	current, _ := json.Marshal(i.Spec)
	if i.Observed != biz.Running || i.PVCUID != "pvc-original" || i.CreateOperationID != c.OperationID || string(frozen) != string(current) || p.creates != 1 {
		t.Fatal("CPU restart changed the frozen create or original workspace", i)
	}
	if _, err := authority.ValidateTerminalTarget(ctx, old); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("old CPU ticket survived restart", err)
	}
	if _, err := authority.ValidateTerminalTarget(ctx, target(i)); err != nil {
		t.Fatal("new CPU terminal rejected", err)
	}
	d := biz.Delete{InstanceKind: biz.KindContainer, ComputeType: biz.TypeCPU, TenantID: c.TenantID, ID: c.ID, OperationID: uuid.NewString(), CreateOperationID: c.OperationID, RequestHash: strings.Repeat("e", 64), ExpectedVersion: i.Version}
	if _, err := f.repo.AcceptDelete(ctx, d); err != nil {
		t.Fatal("CPU delete required a GPU plan or ledger", err)
	}
	p.failReclaim = true
	for n := 0; n < 6; n++ {
		i = step(t, f, w, c)
	}
	if !i.ComputeCleaned || i.QuotaReturned || i.WorkspaceCleaned || i.Observed == biz.Deleted || i.Reason != "WAITING_FOR_WORKSPACE_RECLAIM" {
		t.Fatal("CPU deletion completed before its original volume was reclaimed", i)
	}
	p.obs.VolumeExists = false
	i = step(t, f, w, c)
	if i.Observed != biz.Deleted || !i.WorkspaceCleaned || remote.calls != 0 {
		t.Fatal("CPU cleanup failed or called the GPU ledger", i, remote.calls)
	}
	if err := reporter.Report(ctx, i); err != nil {
		t.Fatal("CPU reporter unexpectedly required GPU charges", err)
	}
	var releases int
	if err := f.runtime.QueryRow(ctx, "SELECT count(*) FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2", c.TenantID, c.ID).Scan(&releases); err != nil || releases != 0 || remote.calls != 0 {
		t.Fatal("CPU lifecycle created a GPU outbox event", releases, remote.calls, err)
	}
	op, err := f.repo.GetOperation(ctx, c.TenantID, d.OperationID)
	if err != nil || op.State != "completed" {
		t.Fatal("CPU delete did not complete durably", op, err)
	}
}

func TestCPUDeleteFirstAndUnknownCreate(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := cpuCreate()
	d := biz.Delete{InstanceKind: biz.KindContainer, ComputeType: biz.TypeCPU, TenantID: c.TenantID, ID: c.ID, OperationID: uuid.NewString(), CreateOperationID: c.OperationID, RequestHash: strings.Repeat("e", 64)}
	ack, err := f.repo.AcceptDelete(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := f.repo.AcceptDelete(ctx, d); err != nil || replay != ack {
		t.Fatal("CPU delete-first replay failed", err)
	}
	conflicting := create()
	conflicting.TenantID, conflicting.ID, conflicting.OperationID = c.TenantID, c.ID, c.OperationID
	if _, err := f.repo.AcceptCreate(ctx, conflicting); !errors.Is(err, biz.ErrConflict) {
		t.Fatal("CPU tombstone admitted a GPU create", err)
	}
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal("late CPU create did not receive its superseded ACK", err)
	}
	p := &provider{}
	w, _ := biz.NewWorker(f.repo, p, "cpu-delete-first")
	var i biz.Instance
	for n := 0; n < 3; n++ {
		i = step(t, f, w, c)
	}
	if i.Observed != biz.Deleted || p.creates != 0 {
		t.Fatal("CPU tombstone created a workload or released GPU quota", i, p.creates)
	}
	c = cpuCreate()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	p = &provider{unknown: true}
	w, _ = biz.NewWorker(f.repo, p, "cpu-before-restart")
	step(t, f, w, c)
	i = step(t, f, w, c)
	d = biz.Delete{InstanceKind: biz.KindContainer, ComputeType: biz.TypeCPU, TenantID: c.TenantID, ID: c.ID, OperationID: uuid.NewString(), CreateOperationID: c.OperationID, RequestHash: strings.Repeat("e", 64), ExpectedVersion: i.Version}
	if _, err := f.repo.AcceptDelete(ctx, d); err != nil {
		t.Fatal(err)
	}
	w, _ = biz.NewWorker(f.repo, p, "cpu-after-restart")
	for n := 0; n < 3; n++ {
		i = step(t, f, w, c)
	}
	if i.Pending == nil || i.Reason != "CREATE_RESULT_UNKNOWN" || p.creates != 1 || i.ComputeCleaned || i.WorkspaceCleaned {
		t.Fatal("unknown CPU create was retried or prematurely cleaned", i, p.creates)
	}
}

// The only substitutes are the external Kubernetes and Gov boundaries. All
// admission, versions, pending actions, leasing and cleanup progress use PG.
type provider struct {
	mu               sync.Mutex
	obs              biz.Observation
	unknown          bool
	failPVC          bool
	failReclaim      bool
	failUpdate       bool
	observeErr       error
	creates, updates int
}

func (p *provider) Observe(context.Context, biz.Instance) (biz.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.obs, p.observeErr
}
func (p *provider) EnsureWorkspace(context.Context, biz.Instance) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.obs.PVCExists = true
	p.obs.PVCUID = "pvc-original"
	p.obs.VolumeExists = true
	p.obs.VolumeName = "volume-original"
	p.obs.VolumeUID = "volume-original-uid"
	return p.obs.PVCUID, nil
}
func (p *provider) CreateNotebook(_ context.Context, i biz.Instance) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates++
	if p.unknown {
		return "", biz.ErrUnknown
	}
	p.obs.NotebookExists = true
	p.obs.NotebookUID = "notebook-original"
	p.obs.NotebookRV = "1"
	p.obs.IntentVersion = i.Version
	p.obs.PodExists = true
	p.obs.PodReady = true
	p.obs.PodUID = "pod-1"
	p.obs.PodName = "notebook-0"
	return p.obs.NotebookUID, nil
}
func (p *provider) SetStopped(_ context.Context, i biz.Instance, _ biz.Observation, stop bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failUpdate {
		return errors.New("external conditional update rejected")
	}
	p.updates++
	p.obs.Stopped = stop
	p.obs.IntentVersion = i.Version
	p.obs.PodExists = !stop
	p.obs.PodReady = !stop
	if !stop {
		p.obs.PodUID = "pod-restarted"
	}
	return nil
}
func (p *provider) DeleteNotebook(context.Context, biz.Instance, biz.Observation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.obs.NotebookExists = false
	p.obs.PodExists = false
	p.obs.PodReady = false
	return nil
}
func (p *provider) DeleteWorkspace(context.Context, biz.Instance, biz.Observation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failPVC {
		return errors.New("external PVC delete forbidden")
	}
	p.obs.PVCExists = false
	if !p.failReclaim {
		p.obs.VolumeExists = false
	}
	return nil
}

func step(t *testing.T, f *fixture, w *biz.Worker, c biz.Create) biz.Instance {
	t.Helper()
	ctx := context.Background()
	if _, e := f.runtime.Exec(ctx, "UPDATE instance.instance_work SET due_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2", c.TenantID, c.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := w.Step(ctx); e != nil {
		t.Fatal(e)
	}
	i, e := f.repo.Get(ctx, c.TenantID, c.ID)
	if e != nil {
		t.Fatal(e)
	}
	return i
}
func command(t *testing.T, f *fixture, c biz.Create, i biz.Instance, action string) biz.Command {
	t.Helper()
	r := biz.Command{TenantID: c.TenantID, ID: c.ID, OperationID: uuid.NewString(), Key: uuid.NewString(), RequestHash: strings.Repeat("e", 64), Action: action, ExpectedVersion: i.Version}
	var a biz.ACK
	var e error
	if action == "delete" {
		a, e = f.repo.AcceptDelete(context.Background(), biz.Delete{TenantID: r.TenantID, ID: r.ID, OperationID: r.OperationID, CreateOperationID: c.OperationID, RequestHash: r.RequestHash, ExpectedVersion: r.ExpectedVersion, InstanceKind: c.Spec.InstanceKind, ComputeType: c.Spec.ComputeType})
	} else {
		a, e = f.repo.AcceptCommand(context.Background(), r)
	}
	if e != nil || !a.Accepted {
		t.Fatal(a, e)
	}
	return r
}
func TestInstancePersistentLifecycle(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := create()
	a, e := f.repo.AcceptCreate(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := f.repo.AcceptCreate(ctx, c)
	if e != nil || replay != a {
		t.Fatal("non-durable ACK replay", replay, e)
	}
	changed := c
	changed.BusinessDigest = strings.Repeat("f", 64)
	if _, e = f.repo.AcceptCreate(ctx, changed); !errors.Is(e, biz.ErrConflict) {
		t.Fatal("changed create accepted", e)
	}
	p := &provider{}
	remote := &releaseEndpoint{immediateReceipt: true}
	reporter, e := data.NewQuota(f.repo, remote)
	if e != nil {
		t.Fatal(e)
	}
	w, _ := biz.NewWorker(f.repo, p, "worker-1")
	var i biz.Instance
	for n := 0; n < 3; n++ {
		i = step(t, f, w, c)
	}
	if i.Observed != biz.Running {
		t.Fatal("not running", i.Reason)
	}
	for n := 0; n < 2; n++ {
		stop := command(t, f, c, i, "stop")
		i = step(t, f, w, c)
		i = step(t, f, w, c)
		if i.Observed != biz.Stopped || i.PVCUID != "pvc-original" || remote.calls != 0 {
			t.Fatal("stop changed quota/workspace")
		}
		// Same-value Stop must advance the durable intent even if projection is
		// temporarily rejected. Its previous observed state cannot complete it.
		if n == 0 {
			repeated := command(t, f, c, i, "stop")
			p.failUpdate = true
			i = step(t, f, w, c)
			op, e := f.repo.GetOperation(ctx, c.TenantID, repeated.OperationID)
			if e != nil || op.State == "completed" || i.Reason != "PROVIDER_MUTATION_FAILED" {
				t.Fatal("old observation completed rejected new Stop", e)
			}
			p.failUpdate = false
			i = step(t, f, w, c)
			i = step(t, f, w, c)
			if i.Version != repeated.ExpectedVersion+1 || p.obs.IntentVersion != i.Version {
				t.Fatal("same-value Stop lost intent version")
			}
		}
		same, e := f.repo.AcceptCommand(ctx, stop)
		if e != nil || same.OperationID != stop.OperationID {
			t.Fatal("stop replay", e)
		}
		command(t, f, c, i, "start")
		i = step(t, f, w, c)
		i = step(t, f, w, c)
		if i.CreateOperationID != c.OperationID || !sameSnapshot(i, c) || i.Observed != biz.Running || p.creates != 1 {
			t.Fatal("start changed original freeze")
		}
		if dispatched, err := reporter.Dispatch(ctx); err != nil || dispatched {
			t.Fatal("Stop/Start created an original refund event", dispatched, err)
		}
		var events int
		if err := f.runtime.QueryRow(ctx, `SELECT count(*) FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID).Scan(&events); err != nil || events != 0 {
			t.Fatal("Stop/Start persisted a refund event", events, err)
		}
	}
	command(t, f, c, i, "delete")
	p.obs.WorkspaceReason = "WORKSPACE_OBSERVATION_FAILED"
	for n := 0; n < 6; n++ {
		i = step(t, f, w, c)
	}
	if !i.ComputeCleaned || i.QuotaReturned || i.WorkspaceCleaned || remote.calls != 0 || i.Reason != "WORKSPACE_OBSERVATION_FAILED" {
		t.Fatal("workspace observation failure allowed early refund", i.Reason, remote.calls)
	}
	p.obs.WorkspaceReason = ""
	p.failPVC = true
	i = step(t, f, w, c)
	if !i.ComputeCleaned || i.QuotaReturned || i.WorkspaceCleaned || i.Observed == biz.Deleted || remote.calls != 0 {
		t.Fatal("PVC failure lost deletion progress", i.Reason, remote.calls)
	}
	p.failPVC = false
	p.failReclaim = true
	for n := 0; n < 3; n++ {
		i = step(t, f, w, c)
	}
	if i.WorkspaceCleaned || i.Observed == biz.Deleted || i.QuotaReturned || i.Reason != "WAITING_FOR_WORKSPACE_RECLAIM" {
		t.Fatal("PVC absence completed before volume reclaim", i.Reason)
	}
	p.obs.VolumeExists = false
	p.failReclaim = false
	for n := 0; n < 3; n++ {
		i = step(t, f, w, c)
	}
	if dispatched, err := reporter.Dispatch(ctx); err != nil || !dispatched {
		t.Fatal("cleanup event dispatch", dispatched, err)
	}
	i, e = f.repo.Get(ctx, c.TenantID, c.ID)
	if e != nil || i.Observed != biz.Deleted || !i.WorkspaceCleaned || !i.QuotaReturned || remote.calls != 1 {
		t.Fatal("cleanup recovery", i, remote.calls, e)
	}
	// A new instance with unknown CREATE stays pending across DELETE and worker restart.
	c2 := create()
	if _, e = f.repo.AcceptCreate(ctx, c2); e != nil {
		t.Fatal(e)
	}
	p2 := &provider{unknown: true}
	w2, _ := biz.NewWorker(f.repo, p2, "before-restart")
	step(t, f, w2, c2)
	i = step(t, f, w2, c2)
	command(t, f, c2, i, "delete")
	w2, _ = biz.NewWorker(f.repo, p2, "after-restart")
	for n := 0; n < 3; n++ {
		i = step(t, f, w2, c2)
	}
	if i.Pending == nil || i.Reason != "CREATE_RESULT_UNKNOWN" || p2.creates != 1 {
		t.Fatal("unknown CREATE incorrectly closed", i.Reason)
	}
	if dispatched, err := reporter.Dispatch(ctx); err != nil || dispatched || remote.calls != 1 {
		t.Fatal("unknown CREATE released original quota", dispatched, err, remote.calls)
	}
	var events int
	if err := f.runtime.QueryRow(ctx, `SELECT count(*) FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2`, c2.TenantID, c2.ID).Scan(&events); err != nil || events != 0 || !i.CleanupCompletedAt.IsZero() {
		t.Fatal("unknown CREATE gained a durable cleanup fact", events, i, err)
	}
}
func sameSnapshot(i biz.Instance, c biz.Create) bool {
	a, _ := json.Marshal(i.Spec)
	b, _ := json.Marshal(c.Spec)
	return string(a) == string(b)
}
func TestInstanceDeleteFirstAndTenant(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := create()
	d := biz.Delete{InstanceKind: biz.KindContainer, ComputeType: biz.TypeGPU, TenantID: c.TenantID, ID: c.ID, OperationID: uuid.NewString(), CreateOperationID: c.OperationID, RequestHash: strings.Repeat("e", 64)}
	a, e := f.repo.AcceptDelete(ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	replay, e := f.repo.AcceptDelete(ctx, d)
	if e != nil || replay != a {
		t.Fatal("delete replay", e)
	}
	if _, e = f.repo.AcceptCreate(ctx, c); e != nil {
		t.Fatal("late create ACK", e)
	}
	i, e := f.repo.Get(ctx, c.TenantID, c.ID)
	if e != nil || i.Desired != biz.Deleted {
		t.Fatal("late create reopened tombstone", e)
	}
	p := &provider{}
	remote := &releaseEndpoint{immediateReceipt: true}
	reporter, e := data.NewQuota(f.repo, remote)
	if e != nil {
		t.Fatal(e)
	}
	w, _ := biz.NewWorker(f.repo, p, "delete-first")
	for n := 0; n < 3; n++ {
		i = step(t, f, w, c)
	}
	if dispatched, err := reporter.Dispatch(ctx); err != nil || !dispatched {
		t.Fatal("delete-first refund event dispatch", dispatched, err)
	}
	if p.creates != 0 || remote.calls != 1 || i.Observed != biz.Deleted {
		t.Fatal("delete first created resources")
	}
	if _, e = f.repo.Get(ctx, uuid.NewString(), c.ID); !errors.Is(e, biz.ErrNotFound) {
		t.Fatal("cross tenant instance leak", e)
	}
	if _, e = f.repo.GetOperation(ctx, uuid.NewString(), d.OperationID); !errors.Is(e, biz.ErrNotFound) {
		t.Fatal("cross tenant operation leak", e)
	}
	if e = biz.RequireTenant(ctx, c.TenantID); !errors.Is(e, biz.ErrDenied) {
		t.Fatal("missing trusted principal accepted")
	}
}

// This substitutes only the remote Gov boundary. The production Resource
// outbox, worker and terminal authority all share the restricted real PG repo.
type releaseEndpoint struct {
	request          *quotapb.ReportResourceReleasedRequest
	calls, applied   int
	wrongReceipt     bool
	immediateReceipt bool
}

func (q *releaseEndpoint) ReportResourceReleased(_ context.Context, r *quotapb.ReportResourceReleasedRequest, _ ...grpc.CallOption) (*quotapb.ReportResourceReleasedResponse, error) {
	q.calls++
	if q.request == nil {
		q.request = proto.Clone(r).(*quotapb.ReportResourceReleasedRequest)
		q.applied++
		if !q.immediateReceipt {
			return nil, status.Error(codes.Unavailable, "response lost after commit")
		}
	}
	if !proto.Equal(q.request, r) {
		return nil, errors.New("release retry changed original event")
	}
	reply := &quotapb.ReportResourceReleasedResponse{ReleaseEventId: r.ReleaseEventId, CreateOperationId: r.CreateOperationId, ResourceId: r.ResourceId, Status: quotapb.ResourceReleaseStatus_ALREADY_APPLIED}
	if q.calls == 1 && q.immediateReceipt {
		reply.Status = quotapb.ResourceReleaseStatus_APPLIED
	}
	if q.wrongReceipt {
		reply.ResourceId = uuid.NewString()
	}
	return reply, nil
}
func TestInstanceReleaseRetryAndTerminalRevocation(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := create()
	if _, e := f.repo.AcceptCreate(ctx, c); e != nil {
		t.Fatal(e)
	}
	p := &provider{}
	remote := &releaseEndpoint{}
	reporter, e := data.NewQuota(f.repo, remote)
	if e != nil {
		t.Fatal(e)
	}
	worker, _ := biz.NewWorker(f.repo, p, "initial-worker")
	var i biz.Instance
	for n := 0; n < 3; n++ {
		i = step(t, f, worker, c)
	}
	authority := service.NewTerminalValidation(f.repo, p)
	target := func(i biz.Instance) *terminal.TerminalTarget {
		return &terminal.TerminalTarget{TenantId: i.TenantID, InstanceId: i.ID, Version: i.Version, ClusterId: i.Spec.ClusterID, Namespace: i.Spec.Namespace, NotebookName: i.Spec.NotebookName, NotebookUid: i.NotebookUID, PodName: i.PodName, PodUid: i.PodUID, ContainerName: biz.Container, Shell: i.Spec.Shell}
	}
	old := target(i)
	if _, e = authority.ValidateTerminalTarget(ctx, old); e != nil {
		t.Fatal("running target rejected", e)
	}
	if e = reporter.Report(ctx, i); !errors.Is(e, biz.ErrInvalid) {
		t.Fatal("release permitted before cleanup", e)
	}
	command(t, f, c, i, "stop")
	if _, e = authority.ValidateTerminalTarget(ctx, old); status.Code(e) != codes.FailedPrecondition {
		t.Fatal("new stop intent did not revoke terminal", e)
	}
	for n := 0; n < 2; n++ {
		i = step(t, f, worker, c)
	}
	command(t, f, c, i, "start")
	for n := 0; n < 2; n++ {
		i = step(t, f, worker, c)
	}
	if _, e = authority.ValidateTerminalTarget(ctx, old); status.Code(e) != codes.FailedPrecondition {
		t.Fatal("old ticket targeted restarted Pod", e)
	}
	if _, e = authority.ValidateTerminalTarget(ctx, target(i)); e != nil {
		t.Fatal("new running target rejected", e)
	}
	command(t, f, c, i, "delete")
	p.failPVC = true
	for n := 0; n < 8; n++ {
		i = step(t, f, worker, c)
	}
	if dispatched, err := reporter.Dispatch(ctx); err != nil || dispatched || remote.calls != 0 || i.QuotaReturned || !i.ComputeCleaned || i.WorkspaceCleaned {
		t.Fatal("PVC failure permitted early Gov delivery", dispatched, err, i)
	}
	p.failPVC = false
	for n := 0; n < 6; n++ {
		i = step(t, f, worker, c)
	}
	if i.Observed != biz.Deleted || i.CleanupCompletedAt.IsZero() || i.QuotaReturned {
		t.Fatal("complete owner cleanup did not persist before Gov", i)
	}
	if dispatched, err := reporter.Dispatch(ctx); err == nil || !dispatched || remote.calls != 1 {
		t.Fatal("lost Gov receipt was accepted", dispatched, err, remote.calls)
	}
	reporter, e = data.NewQuota(f.repo, remote)
	if e != nil {
		t.Fatal(e)
	}
	forceReleaseDue(t, f, c)
	if dispatched, err := reporter.Dispatch(ctx); err != nil || !dispatched {
		t.Fatal("restarted dispatcher did not recover persisted event", dispatched, err)
	}
	i, e = f.repo.Get(ctx, c.TenantID, c.ID)
	if e != nil || remote.calls != 2 || remote.applied != 1 || !i.QuotaReturned || !i.WorkspaceCleaned || i.Observed != biz.Deleted {
		t.Fatal("durable release retry failed", i, e)
	}
	if remote.request.CreateOperationId != c.OperationID || remote.request.ResourceId != c.ID {
		t.Fatal("release lost original ledger binding")
	}
	if dispatched, err := reporter.Dispatch(ctx); err != nil || dispatched || remote.calls != 2 {
		t.Fatal("settled quota retransmitted", dispatched, err, remote.calls)
	}
}

func TestInstanceSupersededMutationAndLeaseFencing(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := create()
	if _, e := f.repo.AcceptCreate(ctx, c); e != nil {
		t.Fatal(e)
	}
	work, e := f.repo.Claim(ctx, "original-worker", time.Minute)
	if e != nil || work == nil {
		t.Fatal("claim", e)
	}
	mutation := biz.Mutation{Kind: "create_notebook", OperationID: c.OperationID, Version: work.Instance.Version}
	if e = f.repo.BeginMutation(ctx, *work, mutation); e != nil {
		t.Fatal(e)
	}
	stopRequest := biz.Command{TenantID: c.TenantID, ID: c.ID, OperationID: uuid.NewString(), Key: uuid.NewString(), RequestHash: strings.Repeat("f", 64), Action: "stop", ExpectedVersion: work.Instance.Version}
	if _, err := f.repo.AcceptCommand(ctx, stopRequest); !errors.Is(err, biz.ErrOperationInProgress) {
		t.Fatal("Stop superseded an unfinished CREATE mutation", err)
	}
	deletion := command(t, f, c, work.Instance, "delete")
	result := work.Instance
	result.NotebookUID = "notebook-created-before-delete-takeover"
	result.Observed = biz.Running
	if e = f.repo.Finish(ctx, *work, biz.Progress{Instance: result, Receipt: &biz.MutationReceipt{Mutation: mutation, Outcome: "succeeded"}, Next: time.Millisecond}); e != nil {
		t.Fatal("attributable late result lost", e)
	}
	current, e := f.repo.Get(ctx, c.TenantID, c.ID)
	if e != nil || current.Desired != biz.Deleted || current.Version != 2 || current.LastOperationID != deletion.OperationID || current.Observed == biz.Running || !biz.SameMutation(current.Pending, &mutation) || current.NotebookUID != result.NotebookUID {
		t.Fatalf("late result lost identity or overwrote DELETE/Pending: desired=%s version=%d pending=%+v notebook=%s error=%v", current.Desired, current.Version, current.Pending, current.NotebookUID, e)
	}
	var receipt []byte
	if e = f.runtime.QueryRow(ctx, "SELECT external_result FROM instance.instance_operations WHERE tenant_id=$1 AND operation_id=$2", c.TenantID, c.OperationID).Scan(&receipt); e != nil || !strings.Contains(string(receipt), "succeeded") {
		t.Fatal("original operation lost external receipt", e)
	}
	if _, e = f.runtime.Exec(ctx, "UPDATE instance.instance_work SET due_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2", c.TenantID, c.ID); e != nil {
		t.Fatal(e)
	}
	old, e := f.repo.Claim(ctx, "expiring-worker", time.Minute)
	if e != nil || old == nil {
		t.Fatal(e)
	}
	command(t, f, c, old.Instance, "delete")
	if e = f.repo.BeginMutation(ctx, *old, biz.Mutation{Kind: "set_stopped", OperationID: old.Instance.LastOperationID, Version: old.Instance.Version}); !errors.Is(e, biz.ErrVersion) {
		t.Fatal("old mutation crossed intent fence", e)
	}
	// Only the lease clock is advanced in this failure fixture; business state
	// is changed exclusively by admission and Finish.
	if _, e = f.runtime.Exec(ctx, "UPDATE instance.instance_work SET lease_until='-infinity',due_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2", c.TenantID, c.ID); e != nil {
		t.Fatal(e)
	}
	newer, e := f.repo.Claim(ctx, "replacement-worker", time.Minute)
	if e != nil || newer == nil || newer.Epoch <= old.Epoch {
		t.Fatal("lease takeover", e)
	}
	if e = f.repo.Finish(ctx, *old, biz.Progress{Instance: old.Instance, Next: time.Millisecond}); !errors.Is(e, biz.ErrLease) {
		t.Fatal("expired worker overwrote current state", e)
	}
	current, e = f.repo.Get(ctx, c.TenantID, c.ID)
	if e != nil || current.Desired != biz.Deleted || current.Version != 3 {
		t.Fatal("lease fence lost newest intent", e)
	}
}

func TestResourceReleaseRequiresMatchingAck(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := create()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	p := &provider{}
	worker, err := biz.NewWorker(f.repo, p, "wrong-ack-owner")
	if err != nil {
		t.Fatal(err)
	}
	i := step(t, f, worker, c)
	command(t, f, c, i, "delete")
	for n := 0; n < 8; n++ {
		i = step(t, f, worker, c)
	}
	remote := &releaseEndpoint{wrongReceipt: true, immediateReceipt: true}
	reporter, err := data.NewQuota(f.repo, remote)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched, err := reporter.Dispatch(ctx); err == nil || !dispatched || remote.calls != 1 {
		t.Fatal("mismatched resource ACK accepted", dispatched, err, remote.calls)
	}
	var acknowledged bool
	if err := f.runtime.QueryRow(ctx, "SELECT acknowledged FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID).Scan(&acknowledged); err != nil || acknowledged {
		t.Fatal("mismatched outbox settled", acknowledged, err)
	}
	i, err = f.repo.Get(ctx, c.TenantID, c.ID)
	if err != nil || i.QuotaReturned {
		t.Fatal("invalid ACK modified the instance ledger", i, err)
	}
	remote.wrongReceipt = false
	forceReleaseDue(t, f, c)
	if dispatched, err := reporter.Dispatch(ctx); err != nil || !dispatched {
		t.Fatal("matching ACK not accepted", dispatched, err)
	}
	if dispatched, err := reporter.Dispatch(ctx); err != nil || dispatched || remote.calls != 2 || remote.applied != 1 {
		t.Fatal("confirmed event retransmitted", dispatched, err, remote.calls)
	}
}

func TestGPUExternalScopeIsCommittedBeforeNotebookCall(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := create()
	if _, e := f.repo.AcceptCreate(ctx, c); e != nil {
		t.Fatal(e)
	}
	work, e := f.repo.Claim(ctx, "crashed-before-external-call", time.Minute)
	if e != nil || work == nil {
		t.Fatal(e)
	}
	work.Instance.GPUPods = []biz.PodRef{{ClusterID: c.Spec.ClusterID, Namespace: c.Spec.Namespace, Name: "pod", UID: "retired-pod", ContainerName: biz.Container}}
	m := biz.Mutation{Kind: "create_notebook", OperationID: work.Instance.LastOperationID, Version: work.Instance.Version}
	if e = f.repo.BeginMutation(ctx, *work, m); e != nil {
		t.Fatal(e)
	}
	i, e := f.repo.Get(ctx, c.TenantID, c.ID)
	if e != nil || !i.ExternalComputeStarted || i.Pending == nil || len(i.GPUPods) != 1 {
		t.Fatal("process death before Finish lost external write scope", e)
	}
	remote := &releaseEndpoint{}
	reporter, _ := data.NewQuota(f.repo, remote)
	i.Pending = nil
	i.Desired = biz.Deleted
	i.ComputeCleaned = true
	if e = reporter.Report(ctx, i); e == nil || remote.calls != 0 {
		t.Fatal("caller-supplied cleanup bypassed persisted owner cleanup fact")
	}
}

func TestGPUCreatedWithoutSeenPodClosesOnOwnerCleanup(t *testing.T) {
	f := database(t)
	ctx := context.Background()
	c := create()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	p := &provider{}
	worker, err := biz.NewWorker(f.repo, p, "unallocated-owner")
	if err != nil {
		t.Fatal(err)
	}
	_ = step(t, f, worker, c)
	i := step(t, f, worker, c)
	p.obs.PodExists = false
	p.obs.PodReady = false
	p.obs.PodName = ""
	p.obs.PodUID = ""
	command(t, f, c, i, "delete")
	for n := 0; n < 8; n++ {
		i = step(t, f, worker, c)
	}
	if len(i.GPUPods) != 0 || !i.ExternalComputeStarted || !i.ComputeCleaned || !i.WorkspaceCleaned || i.CleanupCompletedAt.IsZero() || i.Observed != biz.Deleted {
		t.Fatal("unallocated workload could not close from owner cleanup", i)
	}
	remote := &releaseEndpoint{immediateReceipt: true}
	reporter, err := data.NewQuota(f.repo, remote)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched, err := reporter.Dispatch(ctx); err != nil || !dispatched || remote.calls != 1 {
		t.Fatal("unallocated workload original refund failed", dispatched, err, remote.calls)
	}
}

func computeResolver(t *testing.T, f *fixture, cluster string) *compute.Resolver {
	t.Helper()
	r, e := compute.NewResolver(f.presets, cluster)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func containerScope(ctx context.Context, tenant string) context.Context {
	return compute.WithScope(ctx, compute.Scope{TenantID: tenant, Actor: "governance:user:42", RequestID: "33333333-3333-4333-8333-333333333333", Workload: "ani-governance", Method: compute.ContainerResolveMethod, Usage: compute.ContainerInstance})
}
func presetTenantScope(tenant string) context.Context {
	return compute.WithScope(context.Background(), compute.Scope{TenantID: tenant, Actor: "governance:user:42", RequestID: "33333333-3333-4333-8333-333333333333", Workload: "ani-governance", Method: compute.ListMethod, Usage: compute.ContainerInstance})
}
