//go:build instanceintegration

package instance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	api "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Only the Kubernetes/Controller boundary is replaced. The real Worker owns
// all phases, Pending, versions, current configuration and reference commits.
type restartGuardBoundary struct {
	*provider
	mounts []biz.VolumeMount
	podSeq int
	rv     int
}

func newRestartGuardBoundary() *restartGuardBoundary {
	return &restartGuardBoundary{provider: &provider{}, podSeq: 1, rv: 1}
}

func (p *restartGuardBoundary) CreateNotebook(ctx context.Context, i biz.Instance) (string, error) {
	uid, err := p.provider.CreateNotebook(ctx, i)
	if err != nil {
		return uid, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mounts = append([]biz.VolumeMount{}, i.CurrentMounts...)
	return uid, nil
}

func (p *restartGuardBoundary) SetStopped(ctx context.Context, i biz.Instance, o biz.Observation, stopped bool) error {
	if err := p.provider.SetStopped(ctx, i, o, stopped); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rv++
	p.obs.NotebookRV = strconv.Itoa(p.rv)
	if i.Pending != nil {
		p.obs.MutationID = i.Pending.ID
	}
	if stopped {
		p.obs.PodName, p.obs.PodUID = "", ""
	} else {
		p.podSeq++
		p.obs.PodUID, p.obs.PodName = fmt.Sprintf("pod-%d", p.podSeq), "notebook-0"
	}
	return nil
}

func (p *restartGuardBoundary) ObserveIdentity(ctx context.Context, i biz.Instance) (biz.Observation, error) {
	out, err := p.provider.Observe(ctx, i)
	out.SafeStopped = out.NotebookExists && out.Stopped && !out.PodExists
	if out.NotebookExists {
		out.NotebookGeneration = 1
		out.StatefulSetExists, out.StatefulSetUID = true, "sts-original"
		out.StatefulSetGeneration, out.StatefulSetObservedGeneration = 1, 1
		if !out.Stopped {
			out.StatefulSetReplicas = 1
		}
	}
	if out.PodExists {
		out.Pods = []biz.PodRef{{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, Name: out.PodName, UID: out.PodUID, ContainerName: biz.Container}}
	}
	return out, err
}

func (p *restartGuardBoundary) VerifyRuntime(ctx context.Context, i biz.Instance, _ biz.Observation) (biz.Observation, error) {
	out, err := p.ObserveIdentity(ctx, i)
	if err != nil {
		return out, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if biz.MountDigest(p.mounts) != biz.MountDigest(i.CurrentMounts) {
		return out, biz.ErrState
	}
	return out, nil
}

func (p *restartGuardBoundary) VerifyMountTemplate(ctx context.Context, i biz.Instance, before, target []biz.VolumeMount) (biz.MountTemplateProof, error) {
	out, err := p.ObserveIdentity(ctx, i)
	proof := biz.MountTemplateProof{Class: "mixed", NotebookUID: out.NotebookUID, NotebookRV: out.NotebookRV, StatefulSetUID: out.StatefulSetUID, SafeStopped: out.SafeStopped, NotebookGeneration: 1, StatefulSetGeneration: 1, StatefulSetObservedGeneration: 1}
	if err != nil || !out.NotebookExists {
		return proof, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if biz.MountDigest(p.mounts) == biz.MountDigest(target) {
		proof.Class = "target"
	} else if biz.MountDigest(p.mounts) == biz.MountDigest(before) {
		proof.Class = "old"
	}
	return proof, nil
}

func (p *restartGuardBoundary) ApplyMutation(ctx context.Context, i biz.Instance, m biz.Mutation) error {
	p.mu.Lock()
	valid := p.obs.NotebookUID == m.TargetUID && p.obs.NotebookRV == m.ResourceVersion
	p.mu.Unlock()
	if !valid || m.StoppedTarget == nil {
		return biz.ErrVersion
	}
	i.Pending, i.Version = &m, m.Version
	return p.SetStopped(ctx, i, biz.Observation{}, *m.StoppedTarget)
}

func (p *restartGuardBoundary) UpdateMounts(_ context.Context, _ biz.Instance, m biz.Mutation, target []biz.VolumeMount) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.obs.Stopped || p.obs.NotebookUID != m.TargetUID || p.obs.NotebookRV != m.ResourceVersion || biz.MountDigest(target) != m.TargetMountDigest {
		return biz.ErrVersion
	}
	p.mounts = append([]biz.VolumeMount{}, target...)
	p.rv++
	p.obs.NotebookRV, p.obs.MutationID = strconv.Itoa(p.rv), m.ID
	return nil
}

func restartGuardCreate() biz.Create {
	c := cpuCreate()
	c.Spec.VolumeMounts = []biz.VolumeMount{{ClusterID: c.Spec.ClusterID, Namespace: c.Spec.Namespace, PVCName: "borrowed-data", PVCUID: uuid.NewString(), MountPath: "/data", ReadOnly: true, Ownership: "borrowed"}}
	return c
}

func awaitRestartGuardOperation(t *testing.T, f *fixture, w *biz.Worker, c biz.Create, operation string) biz.Instance {
	t.Helper()
	var i biz.Instance
	for turn := 0; turn < 40; turn++ {
		i = step(t, f, w, c)
		o, err := f.repo.GetOperation(context.Background(), c.TenantID, operation)
		if err != nil {
			t.Fatal(err)
		}
		if biz.TerminalOperation(o.State) {
			if o.State != "completed" {
				t.Fatalf("operation stopped at %s/%s: %s", o.State, o.Execution.Phase, o.Reason)
			}
			return i
		}
	}
	t.Fatalf("operation did not finish through real Worker: instance reason %s", i.Reason)
	return i
}

func acceptRestartGuardCommand(t *testing.T, f *fixture, c biz.Create, i biz.Instance, action, path string) biz.Command {
	t.Helper()
	raw, err := api.CanonicalCommandIntent(action, c.ID, i.Version, "", path, false, false)
	if err != nil {
		t.Fatal(err)
	}
	command := biz.Command{TenantID: c.TenantID, ID: c.ID, OperationID: uuid.NewString(), Key: uuid.NewString(), Action: action, ExpectedVersion: i.Version, RequestHash: api.CommandIntentDigest(raw), CanonicalIntent: raw, MountPath: path}
	if _, err := f.repo.AcceptCommand(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	return command
}

func TestRestartGuardPGCurrentMountsAndAllProtectionStates(t *testing.T) {
	f := database(t)
	f.repo.UseIntentGate(true)
	ctx := context.Background()
	c := restartGuardCreate()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	mount := c.Spec.VolumeMounts[0]
	id := storage.PVCIdentity{TenantID: c.TenantID, ClusterID: mount.ClusterID, Namespace: mount.Namespace, PVCName: mount.PVCName, TargetUID: mount.PVCUID}
	assertProtected := func(want string) {
		t.Helper()
		var state string
		if err := f.runtime.QueryRow(ctx, `SELECT state FROM instance.borrowed_volume_references WHERE tenant_id=$1 AND instance_id=$2 AND mount_path=$3`, c.TenantID, c.ID, mount.MountPath).Scan(&state); err != nil || state != want {
			t.Fatal("reference not in required business state", state, want, err)
		}
		if _, _, err := f.repo.ReserveDelete(ctx, id, "storage-owner", 45*time.Second); !errors.Is(err, storage.ErrVolumeInUse) {
			t.Fatal("protected reference permitted Storage deletion", want, err)
		}
	}
	assertProtected("pending")
	p := newRestartGuardBoundary()
	w, _ := biz.NewWorker(f.repo, p, "restart-guard-worker")
	i := awaitRestartGuardOperation(t, f, w, c, c.OperationID)
	assertProtected("active")
	stop := acceptRestartGuardCommand(t, f, c, i, "stop", "")
	i = awaitRestartGuardOperation(t, f, w, c, stop.OperationID)
	detach := acceptRestartGuardCommand(t, f, c, i, "detach-pvc", mount.MountPath)
	assertProtected("retiring")
	i = awaitRestartGuardOperation(t, f, w, c, detach.OperationID)
	if i.CurrentMounts == nil || len(i.CurrentMounts) != 0 || len(i.Spec.VolumeMounts) != 1 || i.AppliedMountVersion != i.Version || i.RuntimeDesired != biz.Stopped {
		t.Fatal("detach lost empty/current/immutable CREATE distinction", i)
	}
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal("original CREATE replay failed after detach", err)
	}
	reopened, err := data.OpenPostgres(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.Get(ctx, c.TenantID, c.ID)
	if err != nil || restored.CurrentMounts == nil || len(restored.CurrentMounts) != 0 || len(restored.Spec.VolumeMounts) != 1 {
		t.Fatal("restart of repository restored CREATE mounts", restored, err)
	}
	notebook, err := data.RenderNotebook(restored)
	if err != nil {
		t.Fatal(err)
	}
	volumes, _, _ := unstructured.NestedSlice(notebook.Object, "spec", "template", "spec", "volumes")
	if len(volumes) != 1 {
		t.Fatal("render restored removed borrowed mounts", volumes)
	}
	var raw []byte
	var runtime string
	var initialized bool
	var applied int64
	if err := f.runtime.QueryRow(ctx, `SELECT current_mounts,runtime_desired,mounts_initialized,applied_mount_version FROM instance.instances WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID).Scan(&raw, &runtime, &initialized, &applied); err != nil || string(raw) != "[]" || runtime != biz.Stopped || !initialized || applied != i.Version {
		t.Fatal("independent columns did not retain authoritative empty mounts", string(raw), runtime, initialized, applied, err)
	}
	start := acceptRestartGuardCommand(t, f, c, i, "start", "")
	i = awaitRestartGuardOperation(t, f, w, c, start.OperationID)
	if len(i.CurrentMounts) != 0 || i.AppliedMountVersion != applied || i.Observed != biz.Running {
		t.Fatal("Start reset mounts or misrepresented applied version", i)
	}
	if _, acquired, err := f.repo.ReserveDelete(ctx, id, "storage-owner", 45*time.Second); err != nil || !acquired {
		t.Fatal("released last path still protected Storage deletion", acquired, err)
	}
}

func TestRestartGuardPGRuntimeDesiredSurvivesGetListClaim(t *testing.T) {
	f := database(t)
	f.repo.UseIntentGate(true)
	ctx := context.Background()
	c := restartGuardCreate()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	p := newRestartGuardBoundary()
	w, _ := biz.NewWorker(f.repo, p, "restart-runtime-worker")
	i := awaitRestartGuardOperation(t, f, w, c, c.OperationID)
	beforeUID, beforeApplied := i.PodUID, i.AppliedMountVersion
	restart := acceptRestartGuardCommand(t, f, c, i, "restart", "")
	i = step(t, f, w, c)
	if i.Desired != biz.Running || i.RuntimeDesired != biz.Stopped || i.Version != restart.ExpectedVersion+1 {
		t.Fatal("Restart did not persist its separate stopping target", i)
	}
	var desired, runtime string
	var snapshot []byte
	if err := f.runtime.QueryRow(ctx, `SELECT desired,runtime_desired,snapshot FROM instance.instances WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID).Scan(&desired, &runtime, &snapshot); err != nil || desired != biz.Running || runtime != biz.Stopped {
		t.Fatal("SQL phase target differs from Get", desired, runtime, err)
	}
	var stored map[string]json.RawMessage
	_ = json.Unmarshal(snapshot, &stored)
	if string(stored["Desired"]) != `"running"` || stored["RuntimeDesired"] != nil || stored["CurrentMounts"] != nil {
		t.Fatal("snapshot changed independent-column or CREATE encoding")
	}
	items, total, err := f.repo.List(biz.WithCaller(ctx, biz.Caller{TenantID: c.TenantID, Actor: "governance:user:42"}), c.TenantID, biz.ListFilter{})
	if err != nil || total != 1 || items[0].RuntimeDesired != biz.Stopped || !reflect.DeepEqual(items[0].CurrentMounts, i.CurrentMounts) {
		t.Fatal("List lost runtime/current fields", items, total, err)
	}
	// Only the due time is advanced, as in the existing Step fixture. Business
	// state is read and returned through the real Claim/Finish lease protocol.
	if _, err := f.runtime.Exec(ctx, `UPDATE instance.instance_work SET due_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2`, c.TenantID, c.ID); err != nil {
		t.Fatal(err)
	}
	work, err := f.repo.Claim(ctx, "roundtrip-claim", 45*time.Second)
	if err != nil || work == nil || work.Instance.RuntimeDesired != biz.Stopped || work.Operation == nil || work.Operation.Execution.Phase != "stopping" {
		t.Fatal("Claim lost staged operation", work, err)
	}
	if err := f.repo.Finish(ctx, *work, biz.Progress{Instance: work.Instance, Operation: work.Operation, Next: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	i = awaitRestartGuardOperation(t, f, w, c, restart.OperationID)
	if i.PodUID == beforeUID || i.RuntimeDesired != biz.Running || i.Desired != biz.Running || i.AppliedMountVersion != beforeApplied || i.Version != restart.ExpectedVersion+1 {
		t.Fatal("Restart changed mounts/version or retained original Pod", i)
	}
}

func TestRestartGuardPGCreateAndStorageShareNameLock(t *testing.T) {
	f := database(t)
	f.repo.UseIntentGate(true)
	for attempt := 0; attempt < 6; attempt++ {
		c := restartGuardCreate()
		mount := c.Spec.VolumeMounts[0]
		id := storage.PVCIdentity{TenantID: c.TenantID, ClusterID: mount.ClusterID, Namespace: mount.Namespace, PVCName: mount.PVCName, TargetUID: mount.PVCUID}
		start := make(chan struct{})
		var createErr, deleteErr error
		var acquired bool
		var wait sync.WaitGroup
		wait.Add(2)
		go func() { defer wait.Done(); <-start; _, createErr = f.repo.AcceptCreate(context.Background(), c) }()
		go func() {
			defer wait.Done()
			<-start
			_, acquired, deleteErr = f.repo.ReserveDelete(context.Background(), id, "racing-storage", 45*time.Second)
		}()
		close(start)
		wait.Wait()
		if createErr == nil {
			if acquired || !errors.Is(deleteErr, storage.ErrVolumeInUse) {
				t.Fatal("CREATE and Storage both won the same name", acquired, deleteErr)
			}
		} else if deleteErr != nil || !acquired || !errors.Is(createErr, biz.ErrPVCDeleting) {
			t.Fatal("name guard race did not produce exactly one winner", createErr, deleteErr, acquired)
		}
	}
}

func TestRestartGuardPGDeleteLeaseAndTerminalIdentityCAS(t *testing.T) {
	f := database(t)
	f.repo.UseIntentGate(true)
	ctx := context.Background()
	c := restartGuardCreate()
	mount := c.Spec.VolumeMounts[0]
	id := storage.PVCIdentity{TenantID: c.TenantID, ClusterID: mount.ClusterID, Namespace: mount.Namespace, PVCName: mount.PVCName, TargetUID: mount.PVCUID}
	first, acquired, err := f.repo.ReserveDelete(ctx, id, "storage-first", 45*time.Second)
	if err != nil || !acquired {
		t.Fatal(first, acquired, err)
	}
	replay, acquired, err := f.repo.ReserveDelete(ctx, id, "storage-second", 45*time.Second)
	if err != nil || acquired || replay.Token != first.Token {
		t.Fatal("same UID retry stole live lease", replay, acquired, err)
	}
	if err := f.repo.MarkDeleteWriteStarted(ctx, id, first.Token); err != nil {
		t.Fatal(err)
	}
	if err := f.repo.CancelUnsentDelete(ctx, id, first.Token, "external-use"); !errors.Is(err, storage.ErrDeleteLease) {
		t.Fatal("possibly sent DELETE cancelled its barrier", err)
	}
	if err := f.repo.RecordDeleteObservation(ctx, id, first.Token, storage.DeleteObservation{State: "present", ObservedUID: id.TargetUID, WriteOutcome: "unknown", RetryAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	second, err := f.repo.ClaimDeleteRecovery(ctx, "storage-recovery", 45*time.Second)
	if err != nil || second == nil || second.Token.TicketID != first.Token.TicketID || second.Token.GuardEpoch != first.Token.GuardEpoch || second.Token.LeaseEpoch <= first.Token.LeaseEpoch || !second.WriteStarted {
		t.Fatal("recovery replaced identity instead of taking lease", second, err)
	}
	terminal := storage.DeleteObservation{State: "identity_changed", ObservedUID: "same-name-new-uid", WriteOutcome: "unknown"}
	if err := f.repo.CompleteDelete(ctx, id, first.Token, terminal); !errors.Is(err, storage.ErrDeleteLease) {
		t.Fatal("old generation completed a later recovery lease", err)
	}
	expired := second.Token
	expired.LeaseUntil = time.Now().Add(-time.Second)
	if err := f.repo.CompleteDelete(ctx, id, expired, terminal); !errors.Is(err, storage.ErrDeleteLease) {
		t.Fatal("expired token completed a protected ticket", err)
	}
	if err := f.repo.RecordDeleteObservation(ctx, id, second.Token, terminal); !errors.Is(err, storage.ErrInvalid) {
		t.Fatal("terminal observation released lease before Complete", err)
	}
	if err := f.repo.CompleteDelete(ctx, id, second.Token, terminal); err != nil {
		t.Fatal("same-name replacement did not complete old UID ticket", err)
	}
	replay, acquired, err = f.repo.ReserveDelete(ctx, id, "after-complete", 45*time.Second)
	if err != nil || acquired || replay.Phase != "completed" || replay.Identity.TargetUID != id.TargetUID || replay.ObservedUID != terminal.ObservedUID {
		t.Fatal("completed old UID ticket lost immutable target", replay, acquired, err)
	}
}

var _ biz.LifecycleProvider = (*restartGuardBoundary)(nil)

func (p *restartGuardBoundary) VerifyAddedMounts(context.Context, biz.Instance, []biz.VolumeMount) error {
	return nil
}
