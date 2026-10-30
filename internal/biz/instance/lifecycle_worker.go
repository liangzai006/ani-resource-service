package instance

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"time"
)

func cloneOperation(o *Operation) *Operation {
	if o == nil {
		return nil
	}
	raw, _ := json.Marshal(o)
	var out Operation
	_ = json.Unmarshal(raw, &out)
	return &out
}
func phaseTarget(e *Execution) string {
	switch e.Phase {
	case "validating":
		return e.BeforeRunState
	case "stopping", "waiting_stopped", "applying_mounts", "waiting_template", "committing_mounts", "recovering_stop", "verifying_safe_stop":
		return Stopped
	case "starting", "waiting_ready", "waiting_network":
		return Running
	default:
		return e.FinalRunState
	}
}
func advance(p *Progress, phase string) {
	e := p.Operation.Execution
	e.Phase = phase
	e.PhaseSequence++
	p.Instance.RuntimeDesired = phaseTarget(e)
	if p.Instance.Network != nil {
		ReconcileSubmission(&p.Instance, Observation{NotebookExists: p.Instance.NotebookUID != "", NotebookUID: p.Instance.NotebookUID, StatefulSetUID: p.Instance.StatefulSetUID, PodExists: p.Instance.PodUID != "", PodUID: p.Instance.PodUID, PodName: p.Instance.PodName})
	}
	p.Operation.State = "accepted"
	p.Operation.Reason = ""
	p.Instance.Reason = ""
}
func block(p *Progress, reason string) {
	p.Operation.State = "blocked"
	p.Operation.Reason = reason
	p.Instance.Reason = reason
}
func recovering(p *Progress, reason string) {
	e := p.Operation.Execution
	if e.FailureReason == "" {
		e.FailureReason = reason
	}
	if e.Phase != "recovering_stop" && e.Phase != "verifying_safe_stop" {
		advance(p, "recovering_stop")
	}
	block(p, reason)
}
func rememberObservation(p *Progress, o Observation) {
	p.Instance.TrackedPods = MergePodRefs(p.Instance.TrackedPods, o.Pods)
	if o.NotebookExists {
		p.Instance.NotebookUID = o.NotebookUID
		p.Instance.NotebookRV = o.NotebookRV
	}
	if o.StatefulSetUID != "" {
		p.Instance.StatefulSetUID = o.StatefulSetUID
	}
	p.Instance.PodUID = o.PodUID
	p.Instance.PodName = o.PodName
	if o.PVCUID != "" {
		p.Instance.PVCUID = o.PVCUID
	}
	if o.PodExists || o.NotebookExists || o.StatefulSetUID != "" {
		p.Instance.ExternalComputeStarted = true
		p.Instance.ComputeCleaned = false
	}
}
func (w *Worker) stepLifecycle(ctx context.Context, work Work, provider LifecycleProvider) (bool, error) {
	i := work.Instance
	p := Progress{Instance: i, Operation: cloneOperation(work.Operation), Next: 5 * time.Second}
	p.Instance.Network = CloneNetwork(i.Network)
	if p.Operation == nil || p.Operation.Execution == nil {
		return true, ErrInvalid
	}
	e := p.Operation.Execution
	finish := func() (bool, error) {
		p.Instance.ActiveOperation = p.Operation
		return true, w.repo.Finish(ctx, work, p)
	}
	obs, err := provider.ObserveIdentity(ctx, i)
	if err != nil {
		block(&p, ErrNetworkUnavailable.Error())
		if errors.Is(err, ErrOwnership) {
			block(&p, "INSTANCE_OWNERSHIP_CONFLICT")
		}
		return finish()
	}
	rememberObservation(&p, obs)
	if e.DeadlineAt != nil && !time.Now().Before(*e.DeadlineAt) && e.Phase != "done" {
		phase := e.Phase
		recovering(&p, "INSTANCE_OPERATION_DEADLINE_EXCEEDED")
		if e.Phase != phase {
			// Persist the recovery phase before a mutation or fence uses its CAS.
			return finish()
		}
	}
	if i.Pending != nil || i.RecoveryFence != nil {
		if err = w.resolveLifecycleWrites(ctx, work, &p, obs, provider); err != nil {
			return true, err
		}
		return finish()
	}
	if e.Phase == "done" {
		return finish()
	}
	if !obs.NotebookExists {
		recovering(&p, "NOTEBOOK_MISSING")
	}
	switch e.Phase {
	case "validating":
		proof, err := provider.VerifyMountTemplate(ctx, i, i.CurrentMounts, i.CurrentMounts)
		if err != nil || proof.Class != "target" {
			block(&p, "INSTANCE_MOUNT_TEMPLATE_UNVERIFIED")
			return finish()
		}
		if p.Operation.Action == "attach-pvc" && e.TargetConfiguration != nil {
			err := provider.VerifyAddedMounts(ctx, i, LoadMounts(e.TargetConfiguration.VolumeMounts))
			if err != nil {
				if errors.Is(err, ErrPVCNotFound) || errors.Is(err, ErrPVCReferenceChanged) || errors.Is(err, ErrInvalid) {
					e.FailureReason = err.Error()
					advance(&p, "done")
					p.Operation.State = "failed"
					p.Operation.Reason = e.FailureReason
					p.Instance.Reason = e.FailureReason
					p.Instance.Observed = e.BeforeRunState
				} else {
					block(&p, ErrNetworkUnavailable.Error())
				}
				return finish()
			}
		}
		if e.BeforeRunState == Running {
			runtime, err := provider.VerifyRuntime(ctx, i, obs)
			if err != nil || !runtime.PodReady || runtime.PodUID == "" {
				block(&p, "INSTANCE_RUNTIME_UNVERIFIED")
				return finish()
			}
			e.BeforePods = append([]PodRef(nil), runtime.Pods...)
			if len(e.BeforePods) == 0 {
				e.BeforePods = []PodRef{{i.Spec.ClusterID, i.Spec.Namespace, runtime.PodName, runtime.PodUID, Container}}
			}
			p.Instance.TrackedPods = MergePodRefs(p.Instance.TrackedPods, e.BeforePods)
			advance(&p, "stopping")
		} else if !obs.SafeStopped {
			block(&p, "INSTANCE_STOP_PROOF_REQUIRED")
		} else {
			advance(&p, "waiting_stopped")
		}
	case "stopping":
		if obs.Stopped {
			advance(&p, "waiting_stopped")
		} else {
			return w.lifecycleMutation(ctx, work, &p, obs, provider, true, nil)
		}
	case "waiting_stopped":
		if !obs.SafeStopped {
			block(&p, "WAITING_FOR_STOP_PROOF")
			break
		}
		if p.Operation.Action == "restart" {
			advance(&p, "starting")
		} else {
			advance(&p, "applying_mounts")
		}
	case "applying_mounts":
		if !obs.SafeStopped {
			recovering(&p, "INSTANCE_STOP_PROOF_LOST")
			break
		}
		if e.TargetConfiguration == nil {
			return true, ErrInvalid
		}
		target := LoadMounts(e.TargetConfiguration.VolumeMounts)
		return w.lifecycleMutation(ctx, work, &p, obs, provider, true, target)
	case "waiting_template":
		proof, err := provider.VerifyMountTemplate(ctx, i, LoadMounts(e.BeforeMounts), LoadMounts(e.TargetConfiguration.VolumeMounts))
		if err != nil || !proof.SafeStopped || proof.Class != "target" {
			block(&p, "WAITING_FOR_MOUNT_TEMPLATE")
			break
		}
		e.TemplateProof = &proof
		advance(&p, "committing_mounts")
	case "committing_mounts":
		// The owner transaction verifies and changes references with this config.
		proof, err := provider.VerifyMountTemplate(ctx, i, LoadMounts(e.BeforeMounts), LoadMounts(e.TargetConfiguration.VolumeMounts))
		if err != nil || !proof.SafeStopped || proof.Class != "target" {
			block(&p, "WAITING_FOR_MOUNT_TEMPLATE")
			break
		}
		e.TemplateProof = &proof
		e.TemplateCommitCompleted = true
		e.AppliedMountVersion = p.Operation.Version
		p.Instance.CurrentMounts = LoadMounts(e.TargetConfiguration.VolumeMounts)
		p.Instance.AppliedMountVersion = p.Operation.Version
		if e.FinalRunState == Stopped {
			e.CompletionScope = "template_applied"
			p.Operation.State = "completed"
			advance(&p, "done")
			p.Operation.State = "completed"
			p.Instance.Observed = Stopped
		} else {
			advance(&p, "starting")
		}
	case "starting":
		if !obs.Stopped {
			advance(&p, "waiting_ready")
		} else {
			return w.lifecycleMutation(ctx, work, &p, obs, provider, false, nil)
		}
	case "waiting_ready", "waiting_network":
		runtime, err := provider.VerifyRuntime(ctx, p.Instance, obs)
		if err != nil {
			if errors.Is(err, ErrPVCNotFound) || errors.Is(err, ErrPVCReferenceChanged) || errors.Is(err, ErrState) {
				recovering(&p, err.Error())
			} else {
				block(&p, "WAITING_FOR_RUNTIME")
			}
			break
		}
		if !runtime.PodReady || runtime.PodUID == "" {
			block(&p, "WAITING_FOR_NOTEBOOK")
			break
		}
		for _, before := range e.BeforePods {
			if before.UID == runtime.PodUID {
				block(&p, "WAITING_FOR_NEW_POD")
				return finish()
			}
		}
		rememberObservation(&p, runtime)
		if ReconcileSubmission(&p.Instance, runtime) {
			e.VerifiedPodUID = ""
			if e.Phase != "waiting_network" {
				advance(&p, "waiting_network")
			}
			return finish()
		}
		if e.Phase == "waiting_ready" {
			advance(&p, "waiting_network")
			return finish()
		}
		// Network's observer can advance its version between worker turns.
		// Refresh before the confirmation CAS; an unconfirmed snapshot is not
		// ready yet, but a failed observation must not reuse a cached version.
		networkReady := w.observeNetwork(ctx, &p.Instance)
		if p.Instance.Network != nil && p.Instance.Network.Reason != "" {
			block(&p, "WAITING_FOR_NETWORK")
			break
		}
		if changed, err := w.confirmNetwork(ctx, &p.Instance); changed || err != nil {
			return finish()
		}
		if !networkReady {
			block(&p, "WAITING_FOR_NETWORK")
			break
		}
		e.VerifiedPodUID = runtime.PodUID
		if p.Instance.Network != nil {
			e.NetworkSnapshotRevision = p.Instance.Network.SnapshotRevision
		}
		e.CompletionScope = "runtime_verified"
		advance(&p, "done")
		p.Operation.State = "completed"
		p.Instance.Observed = Running
		p.Next = 30 * time.Second
	case "recovering_stop":
		if obs.NotebookExists && !obs.Stopped {
			return w.lifecycleMutation(ctx, work, &p, obs, provider, true, nil)
		}
		if !obs.NotebookExists && (obs.PodExists || obs.StatefulSetUID != "") {
			block(&p, "WAITING_FOR_COMPUTE_CLEANUP")
			break
		}
		if obs.NotebookExists && !obs.SafeStopped {
			block(&p, "WAITING_FOR_SAFE_STOP")
			break
		}
		advance(&p, "verifying_safe_stop")
	case "verifying_safe_stop":
		if obs.NotebookExists && !obs.SafeStopped {
			block(&p, "WAITING_FOR_SAFE_STOP")
			break
		}
		if !obs.NotebookExists && (obs.PodExists || obs.StatefulSetUID != "") {
			block(&p, "WAITING_FOR_COMPUTE_CLEANUP")
			break
		}
		if e.TargetConfiguration != nil && obs.NotebookExists {
			proof, err := provider.VerifyMountTemplate(ctx, i, LoadMounts(e.BeforeMounts), LoadMounts(e.TargetConfiguration.VolumeMounts))
			if err != nil || !proof.SafeStopped || proof.Class == "mixed" {
				block(&p, "INSTANCE_MOUNT_TEMPLATE_UNVERIFIED")
				break
			}
			if proof.Class == "target" {
				e.TemplateProof = &proof
				e.TemplateCommitCompleted = true
				e.AppliedMountVersion = p.Operation.Version
				p.Instance.CurrentMounts = LoadMounts(e.TargetConfiguration.VolumeMounts)
				p.Instance.AppliedMountVersion = p.Operation.Version
			} else if e.TemplateCommitCompleted {
				block(&p, "INSTANCE_MOUNT_TEMPLATE_DRIFT")
				break
			}
		}
		p.Instance.Desired = Stopped
		p.Instance.RuntimeDesired = Stopped
		p.Instance.Observed = Stopped
		advance(&p, "done")
		p.Instance.RuntimeDesired = Stopped
		if p.Instance.Network != nil {
			ReconcileSubmission(&p.Instance, obs)
		}
		p.Operation.State = "failed"
		p.Operation.Reason = e.FailureReason
		p.Instance.Reason = e.FailureReason
		e.CompletionScope = ""
	default:
		return true, ErrInvalid
	}
	return finish()
}
func (w *Worker) lifecycleMutation(ctx context.Context, work Work, p *Progress, o Observation, provider LifecycleProvider, stopped bool, target []VolumeMount) (bool, error) {
	e := p.Operation.Execution
	m := Mutation{ID: uuid.NewString(), Kind: "set_stopped", OperationID: p.Operation.ID, Version: p.Operation.Version, Phase: e.Phase, PhaseSequence: e.PhaseSequence, TargetUID: o.NotebookUID, ResourceVersion: o.NotebookRV, StoppedTarget: &stopped, ProviderMarkerExpected: true}
	if target != nil {
		m.Kind = "update_mounts"
		m.TargetMountDigest = MountDigest(target)
	}
	work.Instance = p.Instance
	if err := w.repo.BeginMutation(ctx, work, m); err != nil {
		return true, err
	}
	p.Instance.Pending = &m
	var err error
	if target != nil {
		err = provider.UpdateMounts(ctx, p.Instance, m, target)
	} else {
		err = provider.ApplyMutation(ctx, p.Instance, m)
	}
	if err == nil {
		p.Instance.Pending = nil
		p.Receipt = &MutationReceipt{Mutation: m, Outcome: "succeeded"}
		if m.Kind == "update_mounts" {
			advance(p, "waiting_template")
		} else if e.Phase == "stopping" {
			advance(p, "waiting_stopped")
		} else if e.Phase == "starting" {
			advance(p, "waiting_ready")
		}
	} else if errors.Is(err, ErrUnknown) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		block(p, ErrUnknown.Error())
	} else {
		// A definitive conditional rejection cannot later apply. Persist its
		// receipt before a fresh observation can authorize another mutation.
		p.Instance.Pending = nil
		p.Receipt = &MutationReceipt{Mutation: m, Outcome: "definitive_rejection"}
		if errors.Is(err, ErrVersion) {
			// A rejected provider CAS cannot apply later. Keep this phase and
			// accepted intent so the next claim can bind a new mutation to a
			// fresh observation; status-only RV churn is not a root failure.
			block(p, ErrVersion.Error())
		} else {
			recovering(p, err.Error())
		}
	}
	return true, w.repo.Finish(ctx, work, *p)
}
func (w *Worker) resolveLifecycleWrites(ctx context.Context, work Work, p *Progress, o Observation, provider LifecycleProvider) error {
	recovery := p.Instance.Desired == Deleted || p.Operation.Execution.Phase == "recovering_stop" || p.Operation.Execution.Phase == "verifying_safe_stop"
	// Resolve a fence before issuing another write; its original identity stays frozen.
	if f := p.Instance.RecoveryFence; f != nil {
		if !o.NotebookExists || o.NotebookUID != f.TargetUID || o.NotebookRV != f.ResourceVersion {
			p.FenceReceipt = &MutationReceipt{Mutation: *f, Outcome: "condition_no_longer_possible"}
			p.Instance.RecoveryFence = nil
		} else {
			err := provider.ApplyMutation(ctx, p.Instance, *f)
			if err == nil {
				p.FenceReceipt = &MutationReceipt{Mutation: *f, Outcome: "succeeded"}
				p.Instance.RecoveryFence = nil
			} else {
				block(p, "RECOVERY_FENCE_RESULT_UNKNOWN")
			}
			return nil
		}
	}
	m := p.Instance.Pending
	if m == nil {
		return nil
	}
	applied := o.NotebookExists && o.NotebookUID == m.TargetUID && o.MutationID == m.ID
	if m.Kind == "set_stopped" && m.StoppedTarget != nil {
		applied = applied && o.Stopped == *m.StoppedTarget
	}
	if m.Kind == "update_mounts" {
		applied = false
		original, err := w.repo.GetOperation(ctx, p.Instance.TenantID, m.OperationID)
		if err != nil || original.Execution == nil || original.Execution.TargetConfiguration == nil {
			block(p, "INSTANCE_PENDING_TEMPLATE_UNVERIFIED")
			return nil
		}
		proof, err := provider.VerifyMountTemplate(ctx, p.Instance, LoadMounts(original.Execution.BeforeMounts), LoadMounts(original.Execution.TargetConfiguration.VolumeMounts))
		if o.NotebookExists && (err != nil || proof.Class == "mixed") {
			block(p, "INSTANCE_PENDING_TEMPLATE_UNVERIFIED")
			return nil
		}
		applied = err == nil && proof.Class == "target" && o.MutationID == m.ID
	}
	impossible := !o.NotebookExists || o.NotebookUID != m.TargetUID || o.NotebookRV != m.ResourceVersion
	if applied || impossible {
		// Template effects are classified again before any config/reference release.
		p.Receipt = &MutationReceipt{Mutation: *m, Outcome: "condition_observed"}
		p.Instance.Pending = nil
		if !recovery && applied {
			switch p.Operation.Execution.Phase {
			case "stopping":
				advance(p, "waiting_stopped")
			case "starting":
				advance(p, "waiting_ready")
			case "applying_mounts":
				advance(p, "waiting_template")
			}
		}
		return nil
	}
	if recovery && (m.Kind == "update_mounts" || (m.Kind == "set_stopped" && (m.StoppedTarget == nil || !*m.StoppedTarget))) {
		repo, ok := w.repo.(FenceRepository)
		if !ok {
			block(p, "INSTANCE_DEPENDENCY_UNAVAILABLE")
			return nil
		}
		yes := true
		f := Mutation{ID: uuid.NewString(), Kind: "set_stopped", OperationID: p.Operation.ID, Version: p.Operation.Version, Phase: p.Operation.Execution.Phase, PhaseSequence: p.Operation.Execution.PhaseSequence, TargetUID: o.NotebookUID, ResourceVersion: o.NotebookRV, StoppedTarget: &yes, FencedMutationID: m.ID, ProviderMarkerExpected: true}
		if err := repo.BeginRecoveryFence(ctx, work, f); err != nil {
			return err
		}
		p.Instance.RecoveryFence = &f
		err := provider.ApplyMutation(ctx, p.Instance, f)
		if err != nil {
			block(p, "RECOVERY_FENCE_RESULT_UNKNOWN")
		}
		return nil
	}
	var err error
	if m.Kind == "update_mounts" && p.Operation.Execution.TargetConfiguration != nil {
		err = provider.UpdateMounts(ctx, p.Instance, *m, LoadMounts(p.Operation.Execution.TargetConfiguration.VolumeMounts))
	} else {
		err = provider.ApplyMutation(ctx, p.Instance, *m)
	}
	if err != nil {
		block(p, ErrUnknown.Error())
	}
	return nil
}
