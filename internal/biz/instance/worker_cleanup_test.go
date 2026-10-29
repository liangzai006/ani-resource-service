package instance

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeleteCleansEntireInstanceBeforeQuotaDelivery(t *testing.T) {
	repo := &cleanupWorkerRepo{instance: Instance{
		Spec:           Spec{ComputeType: TypeGPU, NotebookName: "notebook"},
		RuntimeDesired: Deleted, Desired: Deleted, Version: 2, LastOperationID: "delete-operation",
		ExternalComputeStarted: true,
	}}
	provider := &cleanupWorkerProvider{observation: Observation{
		NotebookExists: true, NotebookUID: "notebook-uid", PodExists: true,
		StatefulSetUID: "controller-uid", PVCExists: true, PVCUID: "workspace-uid", VolumeExists: true,
	}}
	w, err := NewWorker(repo, &identityCleanupProvider{cleanupWorkerProvider: provider}, "cleanup-worker")
	if err != nil {
		t.Fatal(err)
	}
	stepCleanupWorker(t, w)
	if provider.notebookDeletes != 1 || provider.workspaceDeletes != 0 || repo.instance.ComputeCleaned {
		t.Fatal("delete did not begin with Notebook cleanup")
	}
	stepCleanupWorker(t, w)
	if repo.instance.ComputeCleaned || repo.instance.QuotaReturned || repo.instance.Reason != "WAITING_FOR_COMPUTE_CLEANUP" {
		t.Fatal("remaining Pod allowed compute cleanup or quota return")
	}
	provider.observation.PodExists = false
	stepCleanupWorker(t, w)
	if repo.instance.ComputeCleaned || provider.workspaceDeletes != 0 {
		t.Fatal("remaining StatefulSet allowed compute or workspace cleanup")
	}
	provider.observation.StatefulSetUID = ""
	stepCleanupWorker(t, w)
	if !repo.instance.ComputeCleaned || !repo.instance.VolumeReferencesReleased || repo.instance.QuotaReturned {
		t.Fatal("compute cleanup was not persisted before workspace cleanup")
	}
	stepCleanupWorker(t, w)
	if provider.workspaceDeletes != 1 || repo.instance.QuotaReturned || repo.instance.WorkspaceCleaned {
		t.Fatal("unreturned quota blocked workspace deletion or completed it prematurely")
	}
	stepCleanupWorker(t, w)
	if repo.instance.WorkspaceCleaned || repo.instance.Reason != "WAITING_FOR_WORKSPACE_RECLAIM" {
		t.Fatal("remaining PV allowed owner cleanup")
	}
	provider.observation.VolumeExists = false
	stepCleanupWorker(t, w)
	if !OwnerCleanupReady(repo.instance) || !repo.progress.CleanupComplete || repo.instance.Observed != Deleted || repo.instance.QuotaReturned || repo.instance.Reason != "QUOTA_RETURN_PENDING" || repo.progress.Next != 0 {
		t.Fatalf("GPU cleanup did not finish independently of Gov delivery: %+v", repo.instance)
	}
}

func TestDeleteWaitsForNetworkReleaseBeforeOwnerCleanup(t *testing.T) {
	repo := &cleanupWorkerRepo{instance: Instance{
		Spec: Spec{ComputeType: TypeGPU, NotebookName: "notebook"}, RuntimeDesired: Deleted, Desired: Deleted,
		ComputeCleaned: true, WorkspaceCleaned: true, VolumeReferencesReleased: true,
		Network: &NetworkRuntime{State: "closed", DesiredRunState: Deleted, AttachmentID: "attachment", ReleaseStarted: true},
	}}
	provider := &cleanupWorkerProvider{}
	network := &cleanupWorkerNetwork{state: "attached"}
	w, err := NewWorker(repo, provider, "network-cleanup-worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.UseNetwork(network); err != nil {
		t.Fatal(err)
	}
	stepCleanupWorker(t, w)
	if OwnerCleanupReady(repo.instance) || repo.instance.Observed == Deleted || network.releases != 1 || repo.instance.Reason != "WAITING_FOR_NETWORK_RELEASE" {
		t.Fatal("network release request was treated as complete cleanup")
	}
	network.state = "released"
	stepCleanupWorker(t, w)
	if !OwnerCleanupReady(repo.instance) || repo.instance.Observed != Deleted || repo.instance.QuotaReturned || repo.instance.Reason != "QUOTA_RETURN_PENDING" {
		t.Fatal("released network did not permit independent Gov delivery")
	}
}

func TestUnknownWorkspaceCreateUsesWorkspaceObservation(t *testing.T) {
	repo := &cleanupWorkerRepo{instance: Instance{
		Spec:    Spec{ComputeType: TypeCPU, NotebookName: "notebook"},
		Desired: Running, RuntimeDesired: Running, Pending: &Mutation{Kind: "create_workspace"},
	}}
	provider := &cleanupWorkerProvider{observation: Observation{PVCExists: true, PVCUID: "workspace-uid"}}
	w, err := NewWorker(repo, &identityCleanupProvider{cleanupWorkerProvider: provider}, "workspace-recovery-worker")
	if err != nil {
		t.Fatal(err)
	}
	stepCleanupWorker(t, w)
	if repo.instance.Pending != nil || repo.instance.PVCUID != "workspace-uid" || repo.progress.Receipt == nil || repo.progress.Receipt.Outcome != "recovered_by_observation" {
		t.Fatal("an existing workspace was hidden by identity-only observation", repo.instance, repo.progress.Receipt)
	}
}

func TestDurableOwnerCleanupSkipsExternalObservation(t *testing.T) {
	repo := &cleanupWorkerRepo{instance: Instance{
		Spec: Spec{ComputeType: TypeGPU, NotebookName: "notebook"}, RuntimeDesired: Deleted, Desired: Deleted,
		ComputeCleaned: true, WorkspaceCleaned: true, VolumeReferencesReleased: true,
		CleanupCompletedAt: time.Now().UTC(), Network: &NetworkRuntime{Cleaned: true},
	}}
	provider := &cleanupWorkerProvider{observeError: errors.New("Kubernetes unavailable")}
	w, err := NewWorker(repo, provider, "resumed-cleanup-worker")
	if err != nil {
		t.Fatal(err)
	}
	stepCleanupWorker(t, w)
	if provider.observes != 0 || !repo.progress.CleanupComplete || repo.instance.Observed != Deleted || repo.instance.Reason != "QUOTA_RETURN_PENDING" || repo.progress.Next != 0 {
		t.Fatal("durable cleanup became dependent on an external observation")
	}
}

func TestFailedObservationCannotSealStoredCleanupFlags(t *testing.T) {
	repo := &cleanupWorkerRepo{instance: Instance{
		Spec: Spec{ComputeType: TypeGPU, NotebookName: "notebook"}, RuntimeDesired: Deleted, Desired: Deleted,
		ComputeCleaned: true, WorkspaceCleaned: true, VolumeReferencesReleased: true,
	}}
	provider := &cleanupWorkerProvider{observeError: errors.New("Kubernetes unavailable")}
	w, err := NewWorker(repo, provider, "unsealed-cleanup-worker")
	if err != nil {
		t.Fatal(err)
	}
	stepCleanupWorker(t, w)
	if !OwnerCleanupReady(repo.instance) || repo.progress.CleanupComplete || repo.instance.Reason != "PROVIDER_OBSERVATION_FAILED" {
		t.Fatal("failed observation was allowed to seal historic cleanup flags")
	}
}

func TestUnknownCreateStillBlocksOwnerCleanup(t *testing.T) {
	for _, kind := range []string{"create_notebook", "create_workspace"} {
		t.Run(kind, func(t *testing.T) {
			repo := &cleanupWorkerRepo{instance: Instance{
				Spec: Spec{ComputeType: TypeGPU, NotebookName: "notebook"}, RuntimeDesired: Deleted, Desired: Deleted,
				Pending: &Mutation{Kind: kind}, CleanupCompletedAt: time.Now().UTC(),
				ComputeCleaned: true, WorkspaceCleaned: true, VolumeReferencesReleased: true,
			}}
			provider := &cleanupWorkerProvider{}
			w, err := NewWorker(repo, provider, "unknown-create-worker")
			if err != nil {
				t.Fatal(err)
			}
			stepCleanupWorker(t, w)
			if provider.observes != 1 || repo.instance.Pending == nil || OwnerCleanupReady(repo.instance) || repo.instance.Observed == Deleted || repo.instance.QuotaReturned {
				t.Fatal("unknown CREATE was treated as absence of resources")
			}
		})
	}
}

func TestOnlyFinalCPUDeleteCompletesWithoutQuotaDelivery(t *testing.T) {
	for _, desired := range []string{Running, Stopped, Deleted} {
		t.Run(desired, func(t *testing.T) {
			repo := &cleanupWorkerRepo{instance: Instance{
				Spec: Spec{ComputeType: TypeCPU, NotebookName: "notebook"}, RuntimeDesired: desired, Desired: desired,
				ComputeCleaned: true, WorkspaceCleaned: true, VolumeReferencesReleased: true,
			}}
			provider := &cleanupWorkerProvider{}
			if desired != Deleted {
				provider.observation = Observation{NotebookExists: true, PVCExists: true, PodReady: true, Stopped: desired == Stopped}
			}
			w, err := NewWorker(repo, provider, "cpu-worker")
			if err != nil {
				t.Fatal(err)
			}
			stepCleanupWorker(t, w)
			if repo.instance.QuotaReturned != (desired == Deleted) || repo.instance.Observed != desired {
				t.Fatalf("unexpected quota lifecycle for %s: %+v", desired, repo.instance)
			}
		})
	}
}

func stepCleanupWorker(t *testing.T, w *Worker) {
	t.Helper()
	if worked, err := w.Step(context.Background()); err != nil || !worked {
		t.Fatalf("worker did not advance: worked=%v error=%v", worked, err)
	}
}

type cleanupWorkerRepo struct {
	Repository
	instance Instance
	progress Progress
}

func (r *cleanupWorkerRepo) Claim(context.Context, string, time.Duration) (*Work, error) {
	return &Work{Instance: r.instance}, nil
}
func (r *cleanupWorkerRepo) BeginMutation(_ context.Context, w Work, m Mutation) error {
	r.instance = w.Instance
	r.instance.Pending = &m
	return nil
}
func (r *cleanupWorkerRepo) Finish(_ context.Context, _ Work, p Progress) error {
	r.progress = p
	r.instance = p.Instance
	// The repository owns the transaction that releases borrowed references.
	if r.instance.Desired == Deleted && r.instance.ComputeCleaned {
		r.instance.VolumeReferencesReleased = true
	}
	return nil
}

type cleanupWorkerProvider struct {
	Provider
	observation                                 Observation
	observeError                                error
	observes, notebookDeletes, workspaceDeletes int
}

// The real lifecycle adapter keeps identity-only stopping independent from
// workspace availability. DELETE must use its full workspace observation.
type identityCleanupProvider struct {
	*cleanupWorkerProvider
	LifecycleProvider
}

func (p *identityCleanupProvider) ObserveIdentity(ctx context.Context, i Instance) (Observation, error) {
	out, err := p.cleanupWorkerProvider.Observe(ctx, i)
	out.PVCExists, out.VolumeExists = false, false
	out.PVCUID, out.VolumeName, out.VolumeUID, out.WorkspaceReason = "", "", "", ""
	return out, err
}

func (p *cleanupWorkerProvider) Observe(context.Context, Instance) (Observation, error) {
	p.observes++
	return p.observation, p.observeError
}
func (p *cleanupWorkerProvider) DeleteNotebook(context.Context, Instance, Observation) error {
	p.notebookDeletes++
	p.observation.NotebookExists = false
	return nil
}
func (p *cleanupWorkerProvider) DeleteWorkspace(context.Context, Instance, Observation) error {
	p.workspaceDeletes++
	p.observation.PVCExists = false
	return nil
}

type cleanupWorkerNetwork struct {
	NetworkPort
	state    string
	releases int
}

func (n *cleanupWorkerNetwork) Observe(context.Context, Instance) (NetworkObservation, error) {
	return NetworkObservation{State: n.state}, nil
}
func (n *cleanupWorkerNetwork) Release(context.Context, Instance) error {
	n.releases++
	return nil
}
