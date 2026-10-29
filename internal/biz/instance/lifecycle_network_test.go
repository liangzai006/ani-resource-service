package instance

import (
	"context"
	"testing"
	"time"
)

func TestLifecycleRestartConfirmsFreshNetworkVersion(t *testing.T) {
	repo, network, worker := lifecycleNetworkFixture(t)
	stepCleanupWorker(t, worker)
	first := repo.instance.ActiveOperation
	if first.State == "completed" || first.Execution.Phase != "waiting_network" {
		t.Fatal("network confirmation must be persisted before runtime completion", first)
	}
	if network.confirmed != 1 || repo.instance.Network.ConfirmedSnapshotRevision != 5 {
		t.Fatalf("fresh attachment version was not confirmed: confirmed=%d revision=%d reason=%s", network.confirmed, repo.instance.Network.ConfirmedSnapshotRevision, repo.instance.Network.Reason)
	}

	// Network's independent observer advances the attachment between worker
	// turns, just as it does while the restarted Pod is already attached.
	network.version++
	stepCleanupWorker(t, worker)
	final := repo.instance.ActiveOperation
	if final.State != "completed" || final.Execution.Phase != "done" || final.Execution.CompletionScope != "runtime_verified" || final.Execution.VerifiedPodUID != "new-pod" || final.Execution.NetworkSnapshotRevision != 5 || repo.instance.Observed != Running {
		t.Fatal("restart did not complete against the current Pod and confirmed snapshot", final, repo.instance.Network)
	}
	if network.confirmed != 1 || repo.instance.Version != 7 {
		t.Fatal("re-observation repeated confirmation or changed the accepted command version")
	}
}

func TestLifecycleNetworkObservationFailureDoesNotConfirm(t *testing.T) {
	repo, network, worker := lifecycleNetworkFixture(t)
	// A cached version could still pass the CAS. Failed observation must not
	// authorize that write or seal runtime completion.
	network.version = repo.instance.Network.AttachmentVersion
	network.observeError = ErrNetworkUnavailable
	stepCleanupWorker(t, worker)
	operation := repo.instance.ActiveOperation
	if network.observes != 1 || network.confirms != 0 || repo.instance.Network.ConfirmedSnapshotRevision != 2 || operation.State != "blocked" || operation.Execution.Phase != "waiting_network" || operation.Execution.CompletionScope != "" {
		t.Fatal("failed observation allowed confirmation or completion", network, operation)
	}
}

func TestLifecycleNetworkCompletionRequiresCurrentPodAndSnapshot(t *testing.T) {
	for _, mismatch := range []string{"network-pod", "network-snapshot", "old-runtime-pod"} {
		t.Run(mismatch, func(t *testing.T) {
			repo, network, worker := lifecycleNetworkFixture(t)
			repo.instance.Network.ConfirmedSnapshotRevision = 5
			switch mismatch {
			case "network-pod":
				network.podUID = "another-pod"
			case "network-snapshot":
				network.snapshot = 4
			case "old-runtime-pod":
				worker.provider.(*lifecycleNetworkProvider).observation.PodUID = "old-pod"
			}
			for range 2 {
				network.version++
				stepCleanupWorker(t, worker)
			}
			operation := repo.instance.ActiveOperation
			if operation.State != "blocked" || operation.Execution.Phase != "waiting_network" || operation.Execution.CompletionScope != "" || operation.Execution.VerifiedPodUID != "" || network.confirms != 0 {
				t.Fatal("mismatched runtime/network evidence completed the restart", network, operation)
			}
		})
	}
}

func TestLifecycleNetworkPublishesChangedPodBeforeConfirmation(t *testing.T) {
	repo, network, worker := lifecycleNetworkFixture(t)
	repo.instance.Network.CurrentPods[0].Reference.UID = "old-pod"
	stepCleanupWorker(t, worker)
	operation := repo.instance.ActiveOperation
	if network.observes != 0 || network.confirms != 0 || repo.instance.Network.SnapshotRevision != 6 || repo.instance.Network.CurrentPods[0].Reference.UID != "new-pod" || operation.State == "completed" || operation.Execution.CompletionScope != "" {
		t.Fatal("network consumed a changed Pod before its submission was persisted", network, operation)
	}
}

func TestLifecycleDeadlinePersistsRecoveryBeforeMutation(t *testing.T) {
	repo, provider, worker := lifecycleDeadlineFixture(t)
	deadline := *repo.instance.ActiveOperation.Execution.DeadlineAt
	stepCleanupWorker(t, worker)
	operation := repo.instance.ActiveOperation
	if repo.finishes != 1 || len(repo.beginMutations) != 0 || len(provider.mutations) != 0 || operation.Execution.Phase != "recovering_stop" || operation.Execution.PhaseSequence != 7 || operation.Execution.FailureReason != "INSTANCE_OPERATION_DEADLINE_EXCEEDED" {
		t.Fatal("deadline recovery must become durable before a stop write", repo, operation, provider.mutations)
	}
	if repo.instance.Version != 7 || operation.Version != 7 || !operation.Execution.DeadlineAt.Equal(deadline) || operation.Execution.FinalRunState != Running {
		t.Fatal("recovery changed the accepted intent or renewed its deadline", operation)
	}

	// The persisted phase now authorizes the stop CAS. Keeping the expired
	// deadline must not make recovering_stop return before doing its work.
	stepCleanupWorker(t, worker)
	if len(repo.beginMutations) != 1 || len(provider.mutations) != 1 || repo.instance.Pending != nil || repo.instance.ActiveOperation.Execution.Phase != "recovering_stop" {
		t.Fatal("durable recovery did not perform its stop write", repo, provider.mutations)
	}
	stop := provider.mutations[0]
	if stop.Phase != "recovering_stop" || stop.PhaseSequence != 7 || stop.Version != 7 || stop.TargetUID != "notebook-uid" || stop.ResourceVersion != "notebook-rv" || stop.StoppedTarget == nil || !*stop.StoppedTarget {
		t.Fatal("recovery stop was not bound to its durable phase and observed identity", stop)
	}
	stepCleanupWorker(t, worker)
	if repo.instance.ActiveOperation.Execution.Phase != "recovering_stop" || TerminalOperation(repo.instance.ActiveOperation.State) {
		t.Fatal("the stopped annotation bypassed the safe-stop proof", repo.instance.ActiveOperation)
	}
	lifecycleDeadlineProveStopped(provider)
	stepCleanupWorker(t, worker)
	if repo.instance.ActiveOperation.Execution.Phase != "verifying_safe_stop" || TerminalOperation(repo.instance.ActiveOperation.State) {
		t.Fatal("safe-stop verification was not persisted separately", repo.instance.ActiveOperation)
	}
	stepCleanupWorker(t, worker)
	assertLifecycleDeadlineFailed(t, repo)
	if len(repo.beginMutations) != 1 || len(provider.mutations) != 1 {
		t.Fatal("expired recovery repeated an already resolved stop", repo.beginMutations, provider.mutations)
	}
}

func TestLifecycleDeadlinePreservesAndResolvesUnknownWrites(t *testing.T) {
	for _, kind := range []string{"pending-stop", "pending-start", "recovery-fence"} {
		t.Run(kind, func(t *testing.T) {
			repo, provider, worker := lifecycleDeadlineFixture(t)
			stopped := kind == "pending-stop"
			phase := "starting"
			if stopped {
				phase = "stopping"
			}
			operation := repo.instance.ActiveOperation
			operation.Execution.Phase = phase
			pending := Mutation{ID: "original-pending", Kind: "set_stopped", OperationID: operation.ID, Version: operation.Version, Phase: phase, PhaseSequence: 6, TargetUID: "notebook-uid", ResourceVersion: "notebook-rv", StoppedTarget: &stopped, ProviderMarkerExpected: true}
			repo.instance.Pending = cloneLifecycleDeadlineMutation(&pending)
			var originalFence *Mutation
			if kind == "recovery-fence" {
				operation.Execution.Phase, operation.Execution.PhaseSequence = "recovering_stop", 7
				operation.Execution.FailureReason = "INSTANCE_OPERATION_DEADLINE_EXCEEDED"
				stop := true
				originalFence = &Mutation{ID: "original-fence", Kind: "set_stopped", OperationID: operation.ID, Version: operation.Version, Phase: "recovering_stop", PhaseSequence: 7, TargetUID: pending.TargetUID, ResourceVersion: pending.ResourceVersion, StoppedTarget: &stop, FencedMutationID: pending.ID, ProviderMarkerExpected: true}
				repo.instance.RecoveryFence = cloneLifecycleDeadlineMutation(originalFence)
			}
			provider.applyError = ErrUnknown
			stepCleanupWorker(t, worker)
			if !SameMutation(repo.instance.Pending, &pending) || !SameMutation(repo.instance.RecoveryFence, originalFence) || len(repo.beginMutations) != 0 || len(repo.beginFences) != 0 {
				t.Fatal("deadline transition rewrote or discarded unknown-write evidence", repo.instance.Pending, repo.instance.RecoveryFence)
			}
			if kind != "recovery-fence" && len(provider.mutations) != 0 {
				t.Fatal("deadline transition attempted recovery before persisting its phase", provider.mutations)
			}
			stepCleanupWorker(t, worker)
			if !SameMutation(repo.instance.Pending, &pending) || len(repo.beginMutations) != 0 || len(provider.mutations) == 0 {
				t.Fatal("the existing unknown write was not resolved through its original evidence", repo.instance.Pending, provider.mutations)
			}
			replayed := provider.mutations[len(provider.mutations)-1]
			switch kind {
			case "pending-stop":
				if !SameMutation(&replayed, &pending) || repo.instance.RecoveryFence != nil {
					t.Fatal("safe pending stop was replaced instead of replayed exactly", replayed)
				}
			case "pending-start":
				if len(repo.beginFences) != 1 || repo.instance.RecoveryFence == nil || !SameMutation(&replayed, repo.instance.RecoveryFence) || replayed.ID == pending.ID || replayed.FencedMutationID != pending.ID || replayed.TargetUID != pending.TargetUID || replayed.ResourceVersion != pending.ResourceVersion || replayed.StoppedTarget == nil || !*replayed.StoppedTarget || replayed.Phase != "recovering_stop" || replayed.PhaseSequence != 7 {
					t.Fatal("pending start did not acquire its unique durable stop fence", replayed, repo.instance.RecoveryFence)
				}
			case "recovery-fence":
				if len(repo.beginFences) != 0 || !SameMutation(&replayed, originalFence) || !SameMutation(repo.instance.RecoveryFence, originalFence) {
					t.Fatal("inherited fence identity or payload was refreshed", replayed, repo.instance.RecoveryFence)
				}
			}

			// A later RV proves these original conditional writes impossible; it
			// does not permit reissuing them with a new RV or sealing failure yet.
			writes := len(provider.mutations)
			provider.observation.NotebookRV = "later-notebook-rv"
			provider.observation.Stopped = true
			stepCleanupWorker(t, worker)
			if repo.instance.Pending != nil || repo.instance.RecoveryFence != nil || len(provider.mutations) != writes || len(repo.receipts) != 1 || !SameMutation(&repo.receipts[0].Mutation, &pending) || TerminalOperation(repo.instance.ActiveOperation.State) {
				t.Fatal("changed RV refreshed an unknown write or skipped safe stop", repo, provider.mutations)
			}
			stepCleanupWorker(t, worker)
			if repo.instance.ActiveOperation.Execution.Phase != "recovering_stop" || TerminalOperation(repo.instance.ActiveOperation.State) {
				t.Fatal("resolved writes bypassed the safe-stop proof", repo.instance.ActiveOperation)
			}
			lifecycleDeadlineProveStopped(provider)
			stepCleanupWorker(t, worker)
			stepCleanupWorker(t, worker)
			assertLifecycleDeadlineFailed(t, repo)
		})
	}
}

func assertLifecycleDeadlineFailed(t *testing.T, repo *lifecycleDeadlineRepo) {
	t.Helper()
	o := repo.instance.ActiveOperation
	if o.State != "failed" || o.Execution.Phase != "done" || o.Reason != "INSTANCE_OPERATION_DEADLINE_EXCEEDED" || o.Execution.FailureReason != o.Reason || o.Execution.CompletionScope != "" || repo.instance.Desired != Stopped || repo.instance.RuntimeDesired != Stopped || repo.instance.Observed != Stopped || repo.instance.Pending != nil || repo.instance.RecoveryFence != nil || repo.instance.Version != 7 {
		t.Fatal("expired operation did not converge to its original safe failure", o, repo.instance)
	}
}

// This fixture keeps claimed objects separate from durable state and applies
// the production phase/version CAS before granting any external mutation.
type lifecycleDeadlineRepo struct {
	*lifecycleNetworkRepo
	finishes                    int
	beginMutations, beginFences []Mutation
	receipts, fenceReceipts     []MutationReceipt
}

func (r *lifecycleDeadlineRepo) Claim(context.Context, string, time.Duration) (*Work, error) {
	i := cloneLifecycleDeadlineInstance(r.instance)
	return &Work{Instance: i, Operation: cloneOperation(r.instance.ActiveOperation)}, nil
}

func (r *lifecycleDeadlineRepo) accepts(m Mutation) bool {
	o := r.instance.ActiveOperation
	return o != nil && o.Execution != nil && !TerminalOperation(o.State) && r.instance.Version == m.Version && r.instance.LastOperationID == m.OperationID && o.ID == m.OperationID && o.Version == m.Version && o.Execution.Phase == m.Phase && o.Execution.PhaseSequence == m.PhaseSequence
}

func (r *lifecycleDeadlineRepo) BeginMutation(_ context.Context, _ Work, m Mutation) error {
	r.beginMutations = append(r.beginMutations, m)
	if !r.accepts(m) || r.instance.Pending != nil {
		return ErrVersion
	}
	r.instance.Pending = cloneLifecycleDeadlineMutation(&m)
	return nil
}

func (r *lifecycleDeadlineRepo) BeginRecoveryFence(_ context.Context, _ Work, m Mutation) error {
	r.beginFences = append(r.beginFences, m)
	if !r.accepts(m) || r.instance.Pending == nil || r.instance.RecoveryFence != nil || m.FencedMutationID != r.instance.Pending.ID || m.StoppedTarget == nil || !*m.StoppedTarget {
		return ErrVersion
	}
	r.instance.RecoveryFence = cloneLifecycleDeadlineMutation(&m)
	return nil
}

func (r *lifecycleDeadlineRepo) Finish(_ context.Context, w Work, p Progress) error {
	if w.Operation == nil || w.Operation.Execution == nil || !r.accepts(Mutation{OperationID: w.Operation.ID, Version: w.Operation.Version, Phase: w.Operation.Execution.Phase, PhaseSequence: w.Operation.Execution.PhaseSequence}) || p.Instance.Version != r.instance.Version {
		return ErrVersion
	}
	if p.Receipt != nil {
		if !SameMutation(r.instance.Pending, &p.Receipt.Mutation) {
			return ErrVersion
		}
		r.receipts = append(r.receipts, *p.Receipt)
	} else if r.instance.Pending != nil && !SameMutation(r.instance.Pending, p.Instance.Pending) {
		return ErrVersion
	}
	if p.FenceReceipt != nil {
		if !SameMutation(r.instance.RecoveryFence, &p.FenceReceipt.Mutation) {
			return ErrVersion
		}
		r.fenceReceipts = append(r.fenceReceipts, *p.FenceReceipt)
	} else if r.instance.RecoveryFence != nil && !SameMutation(r.instance.RecoveryFence, p.Instance.RecoveryFence) {
		return ErrVersion
	}
	r.instance = cloneLifecycleDeadlineInstance(p.Instance)
	r.instance.ActiveOperation = cloneOperation(p.Operation)
	r.finishes++
	return nil
}

func cloneLifecycleDeadlineMutation(m *Mutation) *Mutation {
	if m == nil {
		return nil
	}
	out := *m
	if m.StoppedTarget != nil {
		stopped := *m.StoppedTarget
		out.StoppedTarget = &stopped
	}
	return &out
}

func cloneLifecycleDeadlineInstance(i Instance) Instance {
	i.ActiveOperation = cloneOperation(i.ActiveOperation)
	i.Pending = cloneLifecycleDeadlineMutation(i.Pending)
	i.RecoveryFence = cloneLifecycleDeadlineMutation(i.RecoveryFence)
	i.Network = CloneNetwork(i.Network)
	i.CurrentMounts = append([]VolumeMount(nil), i.CurrentMounts...)
	i.TrackedPods = append([]PodRef(nil), i.TrackedPods...)
	return i
}

type lifecycleDeadlineProvider struct {
	*lifecycleNetworkProvider
	mutations  []Mutation
	applyError error
}

func (p *lifecycleDeadlineProvider) ApplyMutation(_ context.Context, _ Instance, m Mutation) error {
	p.mutations = append(p.mutations, *cloneLifecycleDeadlineMutation(&m))
	if p.applyError != nil {
		return p.applyError
	}
	if m.TargetUID != p.observation.NotebookUID || m.ResourceVersion != p.observation.NotebookRV || m.StoppedTarget == nil {
		return ErrState
	}
	p.observation.Stopped = *m.StoppedTarget
	p.observation.MutationID = m.ID
	return nil
}

func lifecycleDeadlineProveStopped(p *lifecycleDeadlineProvider) {
	p.observation.Stopped, p.observation.SafeStopped = true, true
	p.observation.PodExists, p.observation.PodReady = false, false
	p.observation.PodUID, p.observation.PodName = "", ""
}

func lifecycleDeadlineFixture(t *testing.T) (*lifecycleDeadlineRepo, *lifecycleDeadlineProvider, *Worker) {
	t.Helper()
	base, _, networkWorker := lifecycleNetworkFixture(t)
	repo := &lifecycleDeadlineRepo{lifecycleNetworkRepo: base}
	deadline := time.Now().Add(-time.Minute)
	repo.instance.ActiveOperation.Execution.DeadlineAt = &deadline
	repo.instance.ActiveOperation.Execution.PhaseSequence = 6
	provider := &lifecycleDeadlineProvider{lifecycleNetworkProvider: networkWorker.provider.(*lifecycleNetworkProvider)}
	worker, err := NewWorker(repo, provider, "lifecycle-deadline-worker")
	if err != nil {
		t.Fatal(err)
	}
	return repo, provider, worker
}

type lifecycleNetworkRepo struct{ cleanupWorkerRepo }

func (r *lifecycleNetworkRepo) Claim(context.Context, string, time.Duration) (*Work, error) {
	return &Work{Instance: r.instance, Operation: r.instance.ActiveOperation}, nil
}

type lifecycleNetworkProvider struct {
	*cleanupWorkerProvider
	LifecycleProvider
}

func (p *lifecycleNetworkProvider) ObserveIdentity(ctx context.Context, i Instance) (Observation, error) {
	return p.Observe(ctx, i)
}

func (p *lifecycleNetworkProvider) VerifyRuntime(_ context.Context, _ Instance, o Observation) (Observation, error) {
	return o, nil
}

type lifecycleNetworkPort struct {
	NetworkPort
	version, snapshot             int64
	podUID                        string
	observeError                  error
	observes, confirms, confirmed int
}

func (n *lifecycleNetworkPort) Observe(context.Context, Instance) (NetworkObservation, error) {
	n.observes++
	if n.observeError != nil {
		return NetworkObservation{}, n.observeError
	}
	return NetworkObservation{State: "attached", Version: n.version, SnapshotRevision: n.snapshot, PodUID: n.podUID}, nil
}

func (n *lifecycleNetworkPort) Confirm(_ context.Context, i Instance) error {
	n.confirms++
	if i.Network.AttachmentVersion != n.version || i.Network.SnapshotRevision != n.snapshot || i.PodUID != n.podUID {
		return ErrNetworkConflict
	}
	n.confirmed++
	n.version++
	return nil
}

func lifecycleNetworkFixture(t *testing.T) (*lifecycleNetworkRepo, *lifecycleNetworkPort, *Worker) {
	t.Helper()
	root := WorkloadReference{"kubeflow.org/v1", "Notebook", "tenant", "notebook", "notebook-uid"}
	controller := WorkloadReference{"apps/v1", "StatefulSet", "tenant", "notebook", "controller-uid"}
	pod := WorkloadReference{"v1", "Pod", "tenant", "notebook-0", "new-pod"}
	repo := &lifecycleNetworkRepo{cleanupWorkerRepo: cleanupWorkerRepo{instance: Instance{
		Spec:    Spec{ComputeType: TypeCPU, ClusterID: "cluster", Namespace: "tenant", NotebookName: "notebook"},
		Desired: Running, RuntimeDesired: Running, Observed: Running, Version: 7,
		NotebookUID: "notebook-uid", StatefulSetUID: "controller-uid", PodUID: "new-pod", PodName: "notebook-0",
		MountsInitialized: true, AppliedMountVersion: 1, LastOperationID: "restart-operation",
		ActiveOperation: &Operation{ID: "restart-operation", Action: "restart", State: "accepted", Version: 7, Execution: &Execution{
			SchemaVersion: 1, Phase: "waiting_network", PhaseSequence: 7, BeforeRunState: Running, FinalRunState: Running,
			BeforePods: []PodRef{{"cluster", "tenant", "notebook-0", "old-pod", Container}},
		}},
		Network: &NetworkRuntime{
			State: "open", DesiredRunState: Running, SubmissionID: "submission", Generation: 1,
			SnapshotRevision: 5, ConfirmedSnapshotRevision: 2, AttachmentID: "attachment", AttachmentVersion: 475,
			RootController: root, Controllers: []ControllerReference{{controller, root}}, CurrentPods: []PodReference{{pod, controller}},
		},
	}}}
	provider := &lifecycleNetworkProvider{cleanupWorkerProvider: &cleanupWorkerProvider{observation: Observation{
		NotebookExists: true, NotebookUID: "notebook-uid", NotebookRV: "notebook-rv", StatefulSetUID: "controller-uid",
		PodExists: true, PodReady: true, PodUID: "new-pod", PodName: "notebook-0",
	}}}
	network := &lifecycleNetworkPort{version: 561, snapshot: 5, podUID: "new-pod"}
	worker, err := NewWorker(repo, provider, "lifecycle-network-worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.UseNetwork(network); err != nil {
		t.Fatal(err)
	}
	return repo, network, worker
}
