package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

type workerRepo struct {
	work             *Work
	progress         Progress
	prepared         []Mutation
	prepareErr       error
	finishContextErr error
}

func (r *workerRepo) Claim(context.Context, string, time.Duration) (*Work, error) {
	v := *r.work
	return &v, nil
}
func (r *workerRepo) PrepareMutation(_ context.Context, _ Work, m Mutation) error {
	if r.prepareErr != nil {
		return r.prepareErr
	}
	r.prepared = append(r.prepared, m)
	r.work.Sandbox.Pending = &m
	return nil
}
func (r *workerRepo) Finish(ctx context.Context, _ Work, p Progress) error {
	r.finishContextErr = ctx.Err()
	r.progress = p
	r.work.Sandbox = p.Sandbox
	if p.Receipt != nil && p.Receipt.Outcome != MutationUnknown && (!p.Receipt.Mutation.PreviousUncertain || p.Receipt.Outcome == MutationApplied) {
		r.work.Sandbox.Pending = nil
	}
	return nil
}

type workerProvider struct {
	repo        *workerRepo
	observe     Observation
	observeErr  error
	observeCall func(context.Context, Sandbox) (Observation, error)
	apply       func(context.Context, Sandbox, Mutation) (MutationResult, error)
	calls       []Mutation
}

func (p *workerProvider) Observe(ctx context.Context, s Sandbox) (Observation, error) {
	if p.observeCall != nil {
		return p.observeCall(ctx, s)
	}
	return p.observe, p.observeErr
}
func (p *workerProvider) Apply(ctx context.Context, s Sandbox, m Mutation) (MutationResult, error) {
	if p.repo.work.Sandbox.Pending == nil || *p.repo.work.Sandbox.Pending != m {
		panic("provider called without durable journal")
	}
	p.calls = append(p.calls, m)
	if p.apply != nil {
		return p.apply(ctx, s, m)
	}
	return MutationResult{Mutation: m, Outcome: MutationApplied, Refs: s.ProviderRefs}, nil
}
func testWorker(t *testing.T, desired string) (*Worker, *workerRepo, *workerProvider) {
	t.Helper()
	r := &workerRepo{work: &Work{Sandbox: Sandbox{Desired: desired, Version: 1, LastOperationID: "11111111-1111-4111-8111-111111111111"}}}
	p := &workerProvider{repo: r}
	w, err := NewWorker(r, p, "worker", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return w, r, p
}
func step(t *testing.T, w *Worker) {
	t.Helper()
	worked, err := w.Step(context.Background())
	if err != nil || !worked {
		t.Fatalf("step=%v err=%v", worked, err)
	}
}
func TestWorkerPersistsUnknownProviderState(t *testing.T) {
	w, r, p := testWorker(t, DesiredRunning)
	p.observeErr = errors.New("timeout")
	step(t, w)
	if r.progress.Sandbox.Observed != ObservedUnknown || len(p.calls) != 0 {
		t.Fatalf("progress=%+v calls=%v", r.progress, p.calls)
	}
}
func TestWorkerPauseKeepsWorkspaceAndJournalsOneChild(t *testing.T) {
	w, r, p := testWorker(t, DesiredPaused)
	p.observe = Observation{Observed: ObservedRunning, WorkspaceExists: true, WorkspaceBound: true, PodExists: true, ProviderRefs: ProviderRefs{PVCName: "workspace", PVCUID: "pvc-1", PodUID: "pod-1"}}
	step(t, w)
	if len(p.calls) != 1 || p.calls[0].Kind != MutationDeletePod || p.calls[0].TargetUID != "pod-1" || r.progress.Sandbox.ProviderRefs.PVCUID != "pvc-1" {
		t.Fatalf("calls=%v progress=%+v", p.calls, r.progress)
	}
}

func TestWorkerRetainsTerminatedPodUntilExplicitPause(t *testing.T) {
	w, r, p := testWorker(t, DesiredRunning)
	p.observe = Observation{Observed: ObservedProvisioning, Reason: "POD_TERMINATED_EXIT_CODE_127", WorkspaceExists: true, WorkspaceBound: true, PodExists: true, PodTerminated: true, ProviderRefs: ProviderRefs{PVCUID: "pvc-1", PodUID: "pod-1"}}
	step(t, w)
	if len(p.calls) != 0 || len(r.prepared) != 0 || r.progress.Sandbox.Observed != ObservedUnknown || r.progress.Sandbox.Reason != "POD_TERMINATED_EXIT_CODE_127" || r.progress.Sandbox.ProviderRefs.PodUID != "pod-1" || r.progress.Next != 30*time.Second {
		t.Fatalf("terminated workload was not retained: progress=%+v mutations=%v", r.progress, p.calls)
	}
	r.work.Sandbox.Desired = DesiredPaused
	step(t, w)
	if len(p.calls) != 1 || p.calls[0].Kind != MutationDeletePod || p.calls[0].TargetUID != "pod-1" {
		t.Fatalf("explicit pause did not stop retained pod: %v", p.calls)
	}
}
func TestWorkerDoesNotReplaceMissingOriginalWorkspace(t *testing.T) {
	w, r, p := testWorker(t, DesiredRunning)
	r.work.Sandbox.ProviderRefs.PVCUID = "original"
	p.observe = Observation{Observed: ObservedUnknown, Reason: "WORKSPACE_MISSING", ProviderRefs: r.work.Sandbox.ProviderRefs}
	step(t, w)
	if len(p.calls) != 0 || r.progress.Sandbox.ProviderRefs.PVCUID != "original" {
		t.Fatalf("replaced original: %+v", r.progress)
	}
}
func TestWorkerUnknownCreateSurvivesDeleteAndRecoversLateObject(t *testing.T) {
	w, r, p := testWorker(t, DesiredRunning)
	p.observe = Observation{Observed: ObservedProvisioning}
	p.apply = func(_ context.Context, _ Sandbox, m Mutation) (MutationResult, error) {
		return MutationResult{Mutation: m, Outcome: MutationUnknown}, context.DeadlineExceeded
	}
	step(t, w)
	if r.work.Sandbox.Pending == nil || r.work.Sandbox.Pending.Kind != MutationCreateWorkspace {
		t.Fatal("unknown create lost its barrier")
	}
	// Delete arrives while the original create's server-side execution is delayed.
	r.work.Sandbox.Desired = DesiredDeleted
	r.work.Sandbox.Version = 2
	p.observe = Observation{Observed: ObservedDeleting}
	step(t, w)
	if r.progress.Sandbox.Observed == ObservedDeleted || r.progress.Receipt != nil || len(p.calls) != 1 {
		t.Fatal("absence incorrectly completed delayed create")
	}
	// The original object finally becomes visible; commit recovery separately.
	p.observe = Observation{Observed: ObservedDeleting, WorkspaceExists: true, ProviderRefs: ProviderRefs{PVCUID: "late-original"}}
	step(t, w)
	if r.work.Sandbox.Pending != nil || r.progress.Receipt == nil || len(p.calls) != 1 {
		t.Fatal("late object not recovered before cleanup")
	}
	p.apply = nil
	step(t, w)
	if len(p.calls) != 2 || p.calls[1].Kind != MutationDeleteWorkspace || p.calls[1].TargetUID != "late-original" {
		t.Fatalf("cleanup=%+v", p.calls)
	}
	p.observe = Observation{Observed: ObservedDeleted}
	step(t, w)
	if r.progress.Sandbox.Observed != ObservedDeleted {
		t.Fatal("cleanup never completed")
	}
}
func TestWorkerWaitsForVolumeReclamation(t *testing.T) {
	w, r, p := testWorker(t, DesiredDeleted)
	p.observe = Observation{Observed: ObservedDeleting, VolumeExists: true}
	step(t, w)
	if r.progress.Sandbox.Observed == ObservedDeleted || len(p.calls) != 0 {
		t.Fatal("volume still exists")
	}
}
func TestWorkerLeaseLossPreventsProviderMutation(t *testing.T) {
	w, r, p := testWorker(t, DesiredRunning)
	r.prepareErr = ErrLease
	if _, err := w.Step(context.Background()); !errors.Is(err, ErrLease) {
		t.Fatalf("err=%v", err)
	}
	if len(p.calls) != 0 {
		t.Fatal("mutation sent after lease loss")
	}
}
func TestWorkerRecordsTimeoutWithUsablePersistenceContext(t *testing.T) {
	w, r, p := testWorker(t, DesiredRunning)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.apply = func(context.Context, Sandbox, Mutation) (MutationResult, error) {
		cancel()
		return MutationResult{Mutation: *r.work.Sandbox.Pending, Outcome: MutationUnknown}, context.Canceled
	}
	if _, err := w.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if r.progress.Receipt == nil || r.progress.Receipt.Outcome != MutationUnknown || r.finishContextErr != nil {
		t.Fatalf("timeout result not recorded with usable context: %+v err=%v", r.progress, r.finishContextErr)
	}
}

func TestWorkerCreatesConsumerForPendingWorkspace(t *testing.T) {
	w, _, p := testWorker(t, DesiredRunning)
	p.observe = Observation{Observed: ObservedProvisioning, WorkspaceExists: true, ProviderRefs: ProviderRefs{PVCUID: "pending-pvc"}}
	step(t, w)
	if len(p.calls) != 1 || p.calls[0].Kind != MutationCreatePod {
		t.Fatalf("WaitForFirstConsumer cannot bind: calls=%v", p.calls)
	}
}

func TestWorkerRetriesUnknownDeleteWithOriginalUID(t *testing.T) {
	w, r, p := testWorker(t, DesiredPaused)
	pending := Mutation{ID: "original-delete", Kind: MutationDeletePod, OperationID: "previous-operation", Version: 1, TargetUID: "pod-original"}
	r.work.Sandbox.Pending = &pending
	p.observe = Observation{Observed: ObservedRunning, PodExists: true, WorkspaceExists: true, ProviderRefs: ProviderRefs{PodUID: "pod-original", PVCUID: "workspace-original"}}
	step(t, w)
	retry := pending
	retry.Attempt++
	retry.PreviousUncertain = true
	if len(r.prepared) != 1 || len(p.calls) != 1 || p.calls[0] != retry || r.work.Sandbox.Pending != nil {
		t.Fatalf("existing delete journal was not safely retried: calls=%v progress=%+v", p.calls, r.progress)
	}
}

func TestWorkerPauseStopsKnownPodDespiteWorkspaceFault(t *testing.T) {
	for _, reason := range []string{"WORKSPACE_LOST", "WORKSPACE_MISSING", "WORKSPACE_RECLAIM_POLICY_MISMATCH"} {
		t.Run(reason, func(t *testing.T) {
			w, r, p := testWorker(t, DesiredPaused)
			p.observe = Observation{Observed: ObservedUnknown, Reason: reason, PodExists: true, ProviderRefs: ProviderRefs{PVCName: "original-workspace", PVCUID: "original-pvc", PodName: "original-pod", PodUID: "original-pod-uid"}}
			step(t, w)
			if len(p.calls) != 1 || p.calls[0].Kind != MutationDeletePod || p.calls[0].TargetUID != "original-pod-uid" {
				t.Fatalf("accepted pause left known Pod running: calls=%v progress=%+v", p.calls, r.progress)
			}
			if r.progress.Sandbox.ProviderRefs.PVCUID != "original-pvc" {
				t.Fatal("pause discarded workspace identity")
			}
		})
	}
}

func TestWorkerDeleteRetryKeepsEarlierUncertaintyAfterNegativeResult(t *testing.T) {
	for _, outcome := range []string{MutationNotSent, MutationRejected} {
		t.Run(outcome, func(t *testing.T) {
			w, r, p := testWorker(t, DesiredRunning)
			pending := Mutation{ID: "pending-delete", Kind: MutationDeletePod, OperationID: "original-pause", Version: 1, TargetUID: "original-pod"}
			r.work.Sandbox.Pending = &pending
			p.observe = Observation{Observed: ObservedRunning, PodExists: true, WorkspaceExists: true, WorkspaceBound: true, ProviderRefs: ProviderRefs{PodUID: "original-pod", PVCUID: "original-workspace"}}
			p.apply = func(_ context.Context, _ Sandbox, m Mutation) (MutationResult, error) {
				return MutationResult{Mutation: m, Outcome: outcome}, errors.New("retry was not dispatched")
			}
			step(t, w)
			if len(p.calls) != 1 || len(r.prepared) != 1 || p.calls[0].Attempt != 1 || !p.calls[0].PreviousUncertain {
				t.Fatalf("retry not durably fenced: prepared=%v calls=%v", r.prepared, p.calls)
			}
			if r.work.Sandbox.Pending == nil || r.progress.Sandbox.Observed != ObservedUnknown {
				t.Fatal("negative retry erased older unknown deletion or completed Resume")
			}
			// A later conclusive UID-fenced delete can resolve the same logical command.
			p.apply = nil
			step(t, w)
			if len(p.calls) != 2 || p.calls[1].Attempt != 2 || r.work.Sandbox.Pending != nil {
				t.Fatalf("later conclusive attempt did not recover: calls=%v progress=%+v", p.calls, r.progress)
			}
		})
	}
}

func TestWorkerPauseCannotCompleteWhileCreateIsUnknown(t *testing.T) {
	w, r, p := testWorker(t, DesiredPaused)
	pending := Mutation{ID: "unknown-create", Kind: MutationCreateWorkspace, OperationID: "original-create", Version: 1}
	r.work.Sandbox.Pending = &pending
	p.observe = Observation{Observed: ObservedPaused}
	step(t, w)
	if r.progress.Sandbox.Observed != ObservedUnknown || r.work.Sandbox.Pending == nil || len(p.calls) != 0 {
		t.Fatal("stopped Pod incorrectly canceled unknown workspace creation")
	}
}

func TestWorkerVolumeReadFailureOnlyRetriesOriginalPodDeletion(t *testing.T) {
	for _, kind := range []string{MutationDeletePod, MutationDeleteWorkspace} {
		t.Run(kind, func(t *testing.T) {
			w, r, p := testWorker(t, DesiredDeleted)
			target := "original-pod"
			if kind == MutationDeleteWorkspace {
				target = "original-workspace"
			}
			pending := Mutation{ID: "pending-delete", Kind: kind, OperationID: "original-delete", Version: 1, TargetUID: target}
			r.work.Sandbox.Pending = &pending
			p.observe = Observation{Observed: ObservedUnknown, Reason: "WORKSPACE_VOLUME_OBSERVATION_FAILED", VolumeObservationFailed: true, WorkspaceExists: true, PodExists: kind == MutationDeletePod, ProviderRefs: ProviderRefs{PodUID: "original-pod", PVCUID: "original-workspace", PVUID: "original-volume"}}
			step(t, w)
			if r.progress.Sandbox.Reason != p.observe.Reason || r.progress.Sandbox.ProviderRefs.PVCUID != "original-workspace" || r.progress.Sandbox.ProviderRefs.PVUID != "original-volume" {
				t.Fatalf("lost volume diagnosis or identities: %+v", r.progress)
			}
			if kind == MutationDeletePod {
				if len(p.calls) != 1 || p.calls[0].Kind != MutationDeletePod || p.calls[0].TargetUID != target || p.calls[0].Attempt != 1 || !p.calls[0].PreviousUncertain || r.work.Sandbox.Pending != nil || r.progress.Sandbox.Observed == ObservedDeleted {
					t.Fatalf("known Pod delete was not safely retried: calls=%v progress=%+v", p.calls, r.progress)
				}
			} else if len(p.calls) != 0 || len(r.prepared) != 0 || r.work.Sandbox.Pending == nil || *r.work.Sandbox.Pending != pending || r.progress.Sandbox.Observed != ObservedUnknown {
				t.Fatalf("unknown volume read permitted workspace deletion: calls=%v progress=%+v", p.calls, r.progress)
			}
		})
	}
}

func TestWorkerVolumeObservationDeadlineLeavesBudgetToStopPod(t *testing.T) {
	w, r, p := testWorker(t, DesiredPaused)
	w.lease = 7 * time.Second
	var observationDeadline time.Time
	p.observeCall = func(ctx context.Context, _ Sandbox) (Observation, error) {
		observationDeadline, _ = ctx.Deadline()
		// Model a PV read that uses all of its allotted budget after the Pod
		// has already been verified, rather than an immediate Timeout response.
		<-ctx.Done()
		return Observation{Observed: ObservedUnknown, Reason: "WORKSPACE_VOLUME_OBSERVATION_FAILED", VolumeObservationFailed: true, PodExists: true, WorkspaceExists: true, ProviderRefs: ProviderRefs{PodUID: "original-pod", PVCUID: "original-workspace"}}, nil
	}
	p.apply = func(ctx context.Context, s Sandbox, m Mutation) (MutationResult, error) {
		deadline, ok := ctx.Deadline()
		if ctx.Err() != nil || !ok || !deadline.After(observationDeadline) {
			t.Fatalf("PV timeout exhausted the Pod stop budget: err=%v deadline=%v observation=%v", ctx.Err(), deadline, observationDeadline)
		}
		return MutationResult{Mutation: m, Outcome: MutationApplied, Refs: s.ProviderRefs}, nil
	}
	step(t, w)
	if len(p.calls) != 1 || p.calls[0].Kind != MutationDeletePod || p.calls[0].TargetUID != "original-pod" || r.progress.Sandbox.Reason != "WORKSPACE_VOLUME_OBSERVATION_FAILED" || r.work.Sandbox.Pending != nil {
		t.Fatalf("deadline-bound PV failure prevented the original Pod stop: calls=%v progress=%+v", p.calls, r.progress)
	}
}
