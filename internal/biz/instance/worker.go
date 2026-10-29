package instance

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"time"
)

type Worker struct {
	repo     Repository
	provider Provider
	owner    string
	network  NetworkPort
}

func NewWorker(r Repository, p Provider, owner string) (*Worker, error) {
	if r == nil || p == nil || owner == "" {
		return nil, ErrInvalid
	}
	return &Worker{repo: r, provider: p, owner: owner}, nil
}
func (w *Worker) Step(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	work, err := w.repo.Claim(ctx, w.owner, 45*time.Second)
	if err != nil || work == nil {
		return false, err
	}
	i := work.Instance
	if work.Operation != nil && work.Operation.Action == "delete" && work.Operation.Execution == nil {
		if repo, ok := w.repo.(LegacyDeleteRepository); ok {
			return true, repo.InitializeLegacyDeleteExecution(ctx, *work)
		}
	}
	if i.Pending != nil && i.Pending.Kind == "set_stopped" && i.Pending.ID == "" {
		if repo, ok := w.repo.(LegacyPendingRepository); ok {
			_, err := repo.TrackLegacyPending(ctx, *work)
			return true, err
		}
	}
	if lp, ok := w.provider.(LifecycleProvider); ok {
		if work.Operation != nil && NewLifecycleAction(work.Operation.Action) {
			return w.stepLifecycle(ctx, *work, lp)
		}
		if i.MountsInitialized && i.AppliedMountVersion == 0 && i.Desired != Deleted && i.NotebookUID != "" {
			proof, e := lp.VerifyMountTemplate(ctx, i, i.CurrentMounts, i.CurrentMounts)
			if e == nil && proof.Class == "target" && (i.RuntimeDesired != Stopped || proof.SafeStopped) {
				i.AppliedMountVersion = 1
				return true, w.repo.Finish(ctx, *work, Progress{Instance: i, Next: time.Millisecond})
			}
		}
	}
	p := Progress{Instance: i, Next: 5 * time.Second}
	p.Instance.Network = CloneNetwork(i.Network)
	if !i.CleanupCompletedAt.IsZero() && OwnerCleanupReady(i) {
		// Local cleanup is already durable. Gov delivery is independent and
		// must not become dependent on Kubernetes again after a restart.
		completeOwnerCleanup(&p)
		return true, w.repo.Finish(ctx, *work, p)
	}
	var obs Observation
	// A DELETE-first tombstone has never accepted an external creation. Its
	// admission lock prevents any later CREATE from enabling external writes.
	if i.Spec.NotebookName != "" {
		// Stopping must remain possible without runtime eligibility. DELETE
		// additionally requires fresh workspace/PV evidence before it may seal
		// cleanup; identity-only observation deliberately omits those fields.
		workspaceRecovery := i.Pending != nil && i.Pending.Kind == "create_workspace"
		if lp, ok := w.provider.(LifecycleProvider); ok && i.Desired != Deleted && !workspaceRecovery && (i.Pending != nil || i.RecoveryFence != nil || i.RuntimeDesired != Running) {
			obs, err = lp.ObserveIdentity(ctx, i)
		} else {
			obs, err = w.provider.Observe(ctx, i)
		}
	}
	p.Instance.GPUPods = MergePodRefs(i.GPUPods, obs.GPUPods)
	if i.Spec.ComputeType == TypeGPU && obs.PodUID != "" {
		p.Instance.GPUPods = MergePodRefs(p.Instance.GPUPods, []PodRef{{i.Spec.ClusterID, i.Spec.Namespace, obs.PodName, obs.PodUID, Container}})
	}
	if obs.NotebookExists || obs.PodExists || obs.StatefulSetUID != "" {
		p.Instance.ExternalComputeStarted = true
		p.Instance.ComputeCleaned = false
	}
	if err != nil {
		p.Instance.Reason = "PROVIDER_OBSERVATION_FAILED"
		if errors.Is(err, ErrSchedulingConflict) {
			p.Instance.Reason = "SCHEDULING_FIELD_CONFLICT"
		}
		if errors.Is(err, ErrPVCReferenceChanged) {
			p.Instance.Reason = "INSTANCE_PVC_REFERENCE_CHANGED"
		}
		if errors.Is(err, ErrPVCNotFound) {
			p.Instance.Reason = "INSTANCE_PVC_NOT_FOUND"
		}
		return true, w.repo.Finish(ctx, *work, p)
	}
	if obs.NotebookExists {
		p.Instance.NotebookUID = obs.NotebookUID
		p.Instance.NotebookRV = obs.NotebookRV
	}
	if obs.PVCExists {
		p.Instance.PVCUID = obs.PVCUID
	}
	if obs.StatefulSetUID != "" {
		p.Instance.StatefulSetUID = obs.StatefulSetUID
	}
	if obs.VolumeName != "" {
		p.Instance.VolumeName = obs.VolumeName
		p.Instance.VolumeUID = obs.VolumeUID
	}
	p.Instance.PodUID = obs.PodUID
	p.Instance.PodName = obs.PodName
	p.Instance.TrackedPods = MergePodRefs(p.Instance.TrackedPods, obs.Pods)
	if lp, ok := w.provider.(LifecycleProvider); ok && (i.RecoveryFence != nil || (i.Pending != nil && i.Pending.Kind == "set_stopped" && i.Pending.ID != "") || (i.Pending != nil && i.Pending.Kind == "update_mounts")) {
		p.Operation = cloneOperation(work.Operation)
		if p.Operation == nil || p.Operation.Execution == nil {
			p.Instance.Reason = "LEGACY_EXECUTION_RECOVERY_REQUIRED"
			return true, w.repo.Finish(ctx, *work, p)
		}
		if err := w.resolveLifecycleWrites(ctx, *work, &p, obs, lp); err != nil {
			return true, err
		}
		return true, w.repo.Finish(ctx, *work, p)
	}
	if i.Pending != nil {
		switch i.Pending.Kind {
		case "create_notebook":
			if !obs.NotebookExists {
				p.Instance.Reason = "CREATE_RESULT_UNKNOWN"
				return true, w.repo.Finish(ctx, *work, p)
			}
		case "create_workspace":
			if !obs.PVCExists {
				p.Instance.Reason = "WORKSPACE_CREATE_RESULT_UNKNOWN"
				return true, w.repo.Finish(ctx, *work, p)
			}
		// Conditional updates and UID-preconditioned deletes can safely be re-observed.
		case "set_stopped":
			if !obs.NotebookExists {
				p.Instance.Reason = "NOTEBOOK_MISSING"
				return true, w.repo.Finish(ctx, *work, p)
			}
		}
		p.Instance.Pending = nil
		p.Receipt = &MutationReceipt{Mutation: *i.Pending, Outcome: "recovered_by_observation"}
		p.Instance.Reason = ""
		ReconcileSubmission(&p.Instance, obs)
		// Persist recovered evidence before any next mutation.
		return true, w.repo.Finish(ctx, *work, p)
	}
	if ReconcileSubmission(&p.Instance, obs) {
		if i.RuntimeDesired == Running {
			p.Instance.Observed = "pending"
			p.Instance.Reason = "WAITING_FOR_NETWORK"
		}
		return true, w.repo.Finish(ctx, *work, p)
	}
	if i.Desired != Deleted {
		if changed, e := w.prepareNetwork(ctx, &p.Instance); changed || e != nil {
			return true, w.repo.Finish(ctx, *work, p)
		}
	}
	networkReady := w.observeNetwork(ctx, &p.Instance)
	// Later mutation/Confirm paths can return before the final readiness branch.
	// Revoke a previous Running state before any such durable return.
	if !networkReady && p.Instance.Network != nil && p.Instance.Observed == Running {
		p.Instance.Observed = "pending"
		p.Instance.Reason = "WAITING_FOR_NETWORK"
	}
	if i.RuntimeDesired == Running && i.Network != nil {
		if changed, e := w.confirmNetwork(ctx, &p.Instance); changed || e != nil {
			return true, w.repo.Finish(ctx, *work, p)
		}
	}
	mutate := func(kind string, call func() error) (bool, error) {
		m := Mutation{Kind: kind, OperationID: i.LastOperationID, Version: i.Version, TargetUID: obs.NotebookUID, ResourceVersion: obs.NotebookRV}
		if work.Operation != nil && work.Operation.Execution != nil {
			m.ID = uuid.NewString()
			m.Phase = work.Operation.Execution.Phase
			m.PhaseSequence = work.Operation.Execution.PhaseSequence
			m.ProviderMarkerExpected = true
			if kind == "set_stopped" {
				stopped := i.RuntimeDesired == Stopped
				m.StoppedTarget = &stopped
			}
		}
		if kind == "delete_workspace" {
			m.TargetUID = obs.PVCUID
		}
		if kind == "create_notebook" {
			p.Instance.ExternalComputeStarted = true
		}
		// The admission/lease transaction commits both the external-write marker
		// and every observed Pod before this call can reach Kubernetes.
		work.Instance = p.Instance
		if err := w.repo.BeginMutation(ctx, *work, m); err != nil {
			if errors.Is(err, ErrVersion) || errors.Is(err, ErrLease) {
				return true, nil
			}
			return true, err
		}
		p.Instance.Pending = &m
		err := call()
		if err == nil {
			p.Instance.Pending = nil
			p.Instance.Reason = ""
			p.Receipt = &MutationReceipt{Mutation: m, Outcome: "succeeded"}
		} else if errors.Is(err, ErrUnknown) {
			p.Instance.Reason = "EXTERNAL_RESULT_UNKNOWN"
		} else {
			p.Instance.Pending = nil
			p.Instance.Reason = "PROVIDER_MUTATION_FAILED"
			if errors.Is(err, ErrPVCReferenceChanged) {
				p.Instance.Reason = "INSTANCE_PVC_REFERENCE_CHANGED"
			}
			if errors.Is(err, ErrPVCNotFound) {
				p.Instance.Reason = "INSTANCE_PVC_NOT_FOUND"
			}
			if errors.Is(err, ErrReservedConfig) {
				p.Instance.Reason = "INSTANCE_RESERVED_CONFIG"
			}
			p.Receipt = &MutationReceipt{Mutation: m, Outcome: "definitive_rejection"}
		}
		return true, w.repo.Finish(ctx, *work, p)
	}
	if i.Desired == Deleted {
		if obs.NotebookExists {
			return mutate("delete_notebook", func() error { return w.provider.DeleteNotebook(ctx, p.Instance, obs) })
		}
		if obs.PodExists || obs.StatefulSetUID != "" {
			p.Instance.Reason = "WAITING_FOR_COMPUTE_CLEANUP"
			return true, w.repo.Finish(ctx, *work, p)
		}
		p.Instance.ComputeCleaned = true
		if !i.ComputeCleaned {
			return true, w.repo.Finish(ctx, *work, p)
		}
		if obs.PVCExists {
			if obs.WorkspaceReason != "" {
				p.Instance.Reason = obs.WorkspaceReason
				return true, w.repo.Finish(ctx, *work, p)
			}
			return mutate("delete_workspace", func() error { return w.provider.DeleteWorkspace(ctx, p.Instance, obs) })
		}
		if obs.WorkspaceReason != "" {
			p.Instance.Reason = obs.WorkspaceReason
			return true, w.repo.Finish(ctx, *work, p)
		}
		if obs.VolumeExists {
			p.Instance.Reason = "WAITING_FOR_WORKSPACE_RECLAIM"
			return true, w.repo.Finish(ctx, *work, p)
		}
		p.Instance.WorkspaceCleaned = true
		if changed, e := w.prepareNetwork(ctx, &p.Instance); changed || e != nil {
			return true, w.repo.Finish(ctx, *work, p)
		}
		if !w.releaseNetwork(ctx, &p.Instance) {
			p.Instance.Reason = "WAITING_FOR_NETWORK_RELEASE"
			return true, w.repo.Finish(ctx, *work, p)
		}
		if !p.Instance.VolumeReferencesReleased {
			p.Instance.Reason = "WAITING_FOR_VOLUME_REFERENCE_RELEASE"
			return true, w.repo.Finish(ctx, *work, p)
		}
		if !OwnerCleanupReady(p.Instance) {
			p.Instance.Reason = ErrInvalid.Error()
			return true, w.repo.Finish(ctx, *work, p)
		}
		completeOwnerCleanup(&p)
		return true, w.repo.Finish(ctx, *work, p)
	}
	if i.RuntimeDesired == Stopped && obs.NotebookExists {
		if !obs.Stopped || obs.IntentVersion != i.Version {
			return mutate("set_stopped", func() error { return w.provider.SetStopped(ctx, p.Instance, obs, true) })
		}
		if !obs.PodExists && stoppedProof(w.provider, obs) {
			p.Instance.Observed = Stopped
			p.Instance.Reason = ""
			p.Next = 30 * time.Second
			return true, w.repo.Finish(ctx, *work, p)
		}
	}
	if obs.WorkspaceReason != "" {
		p.Instance.Reason = obs.WorkspaceReason
		return true, w.repo.Finish(ctx, *work, p)
	}
	if !obs.PVCExists {
		if i.PVCUID != "" {
			p.Instance.Reason = "WORKSPACE_MISSING"
			return true, w.repo.Finish(ctx, *work, p)
		}
		return mutate("create_workspace", func() error { uid, e := w.provider.EnsureWorkspace(ctx, p.Instance); p.Instance.PVCUID = uid; return e })
	}
	if !obs.NotebookExists {
		// A recorded UID proves a Notebook previously existed. Losing it is not a new CREATE.
		if i.NotebookUID != "" {
			p.Instance.Reason = "NOTEBOOK_MISSING"
			return true, w.repo.Finish(ctx, *work, p)
		}
		return mutate("create_notebook", func() error {
			uid, e := w.provider.CreateNotebook(ctx, p.Instance)
			p.Instance.NotebookUID = uid
			return e
		})
	}
	stopped := i.RuntimeDesired == Stopped
	if obs.Stopped != stopped || obs.IntentVersion != i.Version {
		return mutate("set_stopped", func() error { return w.provider.SetStopped(ctx, p.Instance, obs, stopped) })
	}
	if stopped && !obs.PodExists && stoppedProof(w.provider, obs) {
		p.Instance.Observed = Stopped
		p.Instance.Reason = ""
		p.Next = 30 * time.Second
	} else if !stopped && obs.PodReady && networkReady {
		p.Instance.Observed = Running
		p.Instance.Reason = ""
		p.Next = 30 * time.Second
	} else {
		p.Instance.Observed = "pending"
		p.Instance.Reason = "WAITING_FOR_NOTEBOOK"
		if obs.PodReady && !networkReady {
			p.Instance.Reason = "WAITING_FOR_NETWORK"
		}
	}
	return true, w.repo.Finish(ctx, *work, p)
}

func completeOwnerCleanup(p *Progress) {
	p.CleanupComplete = true
	p.Instance.Observed = Deleted
	p.Instance.Reason = ""
	if p.Instance.Spec.ComputeType == TypeCPU {
		p.Instance.QuotaReturned = true
	} else if !p.Instance.QuotaReturned {
		p.Instance.Reason = "QUOTA_RETURN_PENDING"
	}
	p.Next = 0
}

func stoppedProof(provider Provider, obs Observation) bool {
	if _, ok := provider.(LifecycleProvider); ok {
		return obs.SafeStopped
	}
	return !obs.PodExists
}
