package instance

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestLifecycleDefinitiveVersionConflictRetriesCurrentPhase(t *testing.T) {
	for _, phase := range []string{"stopping", "starting", "applying_mounts"} {
		t.Run(phase, func(t *testing.T) {
			repo, provider, worker := lifecycleVersionRetryFixture(t, phase)
			deadline := *repo.instance.ActiveOperation.Execution.DeadlineAt
			provider.applyError = fmt.Errorf("conditional update: %w", ErrVersion)
			stepCleanupWorker(t, worker)
			op := repo.instance.ActiveOperation
			if op.Execution.Phase != phase || op.Execution.PhaseSequence != 6 || op.Execution.FailureReason != "" || op.State != "blocked" || op.Reason != ErrVersion.Error() {
				t.Fatal("a rejected conditional write failed the accepted intent instead of retrying its phase", op)
			}
			if repo.instance.Pending != nil || len(repo.receipts) != 1 || repo.receipts[0].Outcome != "definitive_rejection" || repo.instance.Version != 7 || repo.instance.AppliedMountVersion != 1 || repo.instance.RuntimeDesired != phaseTarget(op.Execution) || !op.Execution.DeadlineAt.Equal(deadline) {
				t.Fatal("rejected write changed configuration, deadline or durable mutation ownership", repo.instance, repo.receipts)
			}
			first := provider.mutations[0]
			provider.applyError = nil
			provider.observation.NotebookRV = "fresh-notebook-rv"
			stepCleanupWorker(t, worker)
			want := map[string]string{"stopping": "waiting_stopped", "starting": "waiting_ready", "applying_mounts": "waiting_template"}[phase]
			op = repo.instance.ActiveOperation
			if op.Execution.Phase != want || op.Execution.FailureReason != "" || repo.instance.Pending != nil || len(provider.mutations) != 2 || len(repo.receipts) != 2 || repo.receipts[1].Outcome != "succeeded" {
				t.Fatal("the same accepted operation did not resume after a fresh observation", op, repo.receipts)
			}
			second := provider.mutations[1]
			if first.ID == second.ID || second.ResourceVersion != "fresh-notebook-rv" || first.OperationID != second.OperationID || first.Version != second.Version || first.Phase != second.Phase || first.PhaseSequence != second.PhaseSequence || first.TargetUID != second.TargetUID {
				t.Fatal("retry reused a rejected token or escaped the original durable intent", first, second)
			}
		})
	}
}

func TestLifecycleVersionRetryKeepsUnknownWritePending(t *testing.T) {
	repo, provider, worker := lifecycleVersionRetryFixture(t, "stopping")
	provider.applyError = ErrUnknown
	stepCleanupWorker(t, worker)
	first := *repo.instance.Pending
	stepCleanupWorker(t, worker)
	if len(repo.beginMutations) != 1 || len(repo.receipts) != 0 || !SameMutation(&first, repo.instance.Pending) || repo.instance.ActiveOperation.Execution.FailureReason != "" {
		t.Fatal("an unknown write was replaced without proving its original condition impossible", repo.instance, repo.beginMutations, repo.receipts)
	}
}

func TestLifecycleVersionRetryPreservesDefinitiveOwnershipFailure(t *testing.T) {
	repo, provider, worker := lifecycleVersionRetryFixture(t, "stopping")
	provider.applyError = ErrOwnership
	stepCleanupWorker(t, worker)
	op := repo.instance.ActiveOperation
	if op.Execution.Phase != "recovering_stop" || op.Execution.FailureReason != ErrOwnership.Error() || repo.instance.Pending != nil || len(repo.receipts) != 1 || repo.receipts[0].Outcome != "definitive_rejection" {
		t.Fatal("a non-version rejection lost its original safe-stop failure handling", op, repo.receipts)
	}
}

type lifecycleVersionRetryProvider struct{ *lifecycleDeadlineProvider }

func (p *lifecycleVersionRetryProvider) UpdateMounts(_ context.Context, _ Instance, m Mutation, _ []VolumeMount) error {
	p.mutations = append(p.mutations, *cloneLifecycleDeadlineMutation(&m))
	return p.applyError
}

func lifecycleVersionRetryFixture(t *testing.T, phase string) (*lifecycleDeadlineRepo, *lifecycleVersionRetryProvider, *Worker) {
	t.Helper()
	repo, base, _ := lifecycleDeadlineFixture(t)
	op := repo.instance.ActiveOperation
	deadline := time.Now().Add(time.Minute)
	op.Execution.DeadlineAt, op.Execution.Phase = &deadline, phase
	if phase != "stopping" {
		lifecycleDeadlineProveStopped(base)
	}
	if phase == "applying_mounts" {
		op.Action = "attach-pvc"
		op.Execution.TargetConfiguration = &MountTarget{VolumeMounts: []StoredMount{{PVCName: "borrowed", MountPath: "/data"}}}
	}
	repo.instance.RuntimeDesired = phaseTarget(op.Execution)
	provider := &lifecycleVersionRetryProvider{base}
	worker, err := NewWorker(repo, provider, "lifecycle-version-retry-worker")
	if err != nil {
		t.Fatal(err)
	}
	return repo, provider, worker
}
