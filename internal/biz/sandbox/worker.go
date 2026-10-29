package sandbox

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
)

type Worker struct {
	repo     WorkerRepository
	provider Provider
	owner    string
	lease    time.Duration
}

func NewWorker(repo WorkerRepository, provider Provider, owner string, lease time.Duration) (*Worker, error) {
	if repo == nil || provider == nil || owner == "" || lease <= 5*time.Second {
		return nil, ErrInvalid
	}
	return &Worker{repo: repo, provider: provider, owner: owner, lease: lease}, nil
}
func (w *Worker) Step(ctx context.Context) (bool, error) {
	work, err := w.repo.Claim(ctx, w.owner, w.lease)
	if err != nil || work == nil {
		return false, err
	}
	// Reserve the last five seconds for recording the result, including an
	// unknown result when the provider call exhausts its own deadline.
	callCtx, cancel := context.WithTimeout(ctx, w.lease-5*time.Second)
	defer cancel()
	p := Progress{Sandbox: work.Sandbox, Next: 5 * time.Second}
	finish := func() (bool, error) {
		persistCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		return true, w.repo.Finish(persistCtx, *work, p)
	}
	// A PV read can consume its entire observation deadline after the Pod has
	// already been verified. Reserve a separate part of the lease budget for
	// journaling and stopping that Pod with an unexpired mutation context.
	observeCtx, observeCancel := context.WithTimeout(callCtx, (w.lease-5*time.Second)/2)
	obs, observeErr := w.provider.Observe(observeCtx, work.Sandbox)
	observeCancel()
	if observeErr != nil {
		p.Sandbox.Observed, p.Sandbox.Reason = ObservedUnknown, "PROVIDER_OBSERVATION_FAILED"
		if errors.Is(observeErr, ErrClusterMismatch) {
			p.Sandbox.Reason = ErrClusterMismatch.Error()
		}
		return finish()
	}
	p.Sandbox.Observed, p.Sandbox.Reason, p.Sandbox.ProviderRefs, p.Sandbox.ObservedAt = obs.Observed, obs.Reason, obs.ProviderRefs, obs.At
	apply := func(mutation Mutation) {
		result, mutateErr := w.provider.Apply(callCtx, p.Sandbox, mutation)
		// A malformed/empty adapter result cannot release a durable barrier.
		if result.Mutation != mutation || (result.Outcome != MutationApplied && result.Outcome != MutationRejected && result.Outcome != MutationNotSent && result.Outcome != MutationUnknown) {
			result = MutationResult{Mutation: mutation, Outcome: MutationUnknown, Reason: "INVALID_PROVIDER_RECEIPT"}
		}
		p.Receipt = &result
		if mutateErr == nil && result.Outcome == MutationApplied {
			// The observation predates this write. In particular, deleting an
			// old Pause target cannot complete a newer Resume from its old Ready
			// state. Only a subsequent observation can prove the current goal.
			p.Sandbox.Observed = ObservedProvisioning
			if p.Sandbox.Desired == DesiredDeleted {
				p.Sandbox.Observed = ObservedDeleting
			}
			p.Sandbox.Reason = "WAITING_FOR_PROVIDER_OBSERVATION"
			if obs.VolumeObservationFailed {
				p.Sandbox.Reason = obs.Reason
			}
		}
		if mutateErr != nil || result.Outcome != MutationApplied {
			p.Sandbox.Observed, p.Sandbox.Reason = ObservedUnknown, result.Reason
			if p.Sandbox.Reason == "" {
				p.Sandbox.Reason = "PROVIDER_MUTATION_FAILED"
			}
			if errors.Is(mutateErr, ErrUnsupported) {
				p.Sandbox.Reason = "UNSUPPORTED"
			}
		}
	}
	if pending := work.Sandbox.Pending; pending != nil {
		// Absence cannot prove a timed-out create was canceled. An attributable
		// object resolves it; cleanup then follows the latest desired intent.
		resolved := false
		switch pending.Kind {
		case MutationCreateWorkspace:
			resolved = obs.WorkspaceExists
		case MutationCreatePod:
			resolved = obs.PodExists
		case MutationDeletePod:
			resolved = !obs.PodExists
		case MutationDeleteWorkspace:
			resolved = !obs.WorkspaceExists
		}
		if resolved {
			p.Receipt = &MutationResult{Mutation: *pending, Outcome: MutationApplied, Refs: obs.ProviderRefs}
		} else if (pending.Kind == MutationDeletePod && obs.ProviderRefs.PodUID == pending.TargetUID && pending.TargetUID != "") || (pending.Kind == MutationDeleteWorkspace && !obs.VolumeObservationFailed && obs.ProviderRefs.PVCUID == pending.TargetUID && pending.TargetUID != "") {
			// Each retry gets its own durable attempt fence. A negative result
			// for this retry cannot cancel an older potentially dispatched call.
			if pending.Attempt == math.MaxInt64 {
				return true, ErrInvalid
			}
			retry := *pending
			retry.Attempt++
			retry.PreviousUncertain = true
			if err = w.repo.PrepareMutation(callCtx, *work, retry); err != nil {
				return true, err
			}
			p.Sandbox.Pending = &retry
			apply(retry)
		} else {
			p.Sandbox.Observed = ObservedUnknown
			if !obs.VolumeObservationFailed {
				p.Sandbox.Reason = "PROVIDER_MUTATION_RESULT_UNKNOWN"
			}
		}
		// Commit recovery before preparing another provider command.
		return finish()
	}
	if obs.VolumeObservationFailed && (work.Sandbox.Desired == DesiredRunning || (work.Sandbox.Desired == DesiredDeleted && !obs.PodExists)) {
		// A failed storage read permits stopping an attributable Pod, never
		// creating a consumer, deleting the workspace, or declaring reclamation.
		p.Sandbox.Observed = ObservedUnknown
		return finish()
	}
	if obs.Observed == ObservedUnknown && work.Sandbox.Desired != DesiredDeleted && work.Sandbox.Desired != DesiredPaused {
		if p.Sandbox.Reason == "" {
			p.Sandbox.Reason = "PROVIDER_STATE_UNKNOWN"
		}
		return finish()
	}
	kind, target := "", ""
	switch work.Sandbox.Desired {
	case DesiredRunning:
		switch {
		case !obs.WorkspaceExists && work.Sandbox.ProviderRefs.PVCUID == "":
			kind = MutationCreateWorkspace
		case !obs.WorkspaceExists:
			p.Sandbox.Observed, p.Sandbox.Reason = ObservedUnknown, "WORKSPACE_MISSING"
		case !obs.PodExists:
			kind = MutationCreatePod
		case obs.PodTerminated:
			// Preserve the failed workload and its diagnostic facts. Repeatedly
			// replacing a Pod with a deterministic image/command failure hides
			// the cause and creates unbounded churn. A later pause followed by
			// resume explicitly stops the old Pod and starts a new attempt.
			p.Sandbox.Observed = ObservedUnknown
			if p.Sandbox.Reason == "" {
				p.Sandbox.Reason = "POD_TERMINATED"
			}
			p.Next = 30 * time.Second
		case !obs.WorkspaceBound:
		}
	case DesiredPaused:
		if obs.PodExists {
			kind, target = MutationDeletePod, obs.ProviderRefs.PodUID
		}
	case DesiredDeleted:
		switch {
		case obs.PodExists:
			kind, target = MutationDeletePod, obs.ProviderRefs.PodUID
		case obs.WorkspaceExists:
			kind, target = MutationDeleteWorkspace, obs.ProviderRefs.PVCUID
		case obs.VolumeExists:
			p.Sandbox.Observed, p.Sandbox.Reason = ObservedDeleting, "VOLUME_RECLAIM_PENDING"
		}
	default:
		return true, ErrInvalid
	}
	if kind == "" {
		return finish()
	}
	mutation := Mutation{ID: uuid.NewString(), Kind: kind, OperationID: work.Sandbox.LastOperationID, Version: work.Sandbox.Version, TargetUID: target}
	if err = w.repo.PrepareMutation(callCtx, *work, mutation); err != nil {
		return true, err
	}
	p.Sandbox.Pending = &mutation
	apply(mutation)
	return finish()
}
