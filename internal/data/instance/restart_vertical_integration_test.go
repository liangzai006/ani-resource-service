//go:build instanceintegration

package instance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"testing"
)

// Only the Kubernetes/controller/CSI boundary is substituted. Admission,
// phases, leases, versioning, replay and recovery use the real PostgreSQL owner.
type lifecycleBoundary struct {
	provider
	mounts       []biz.VolumeMount
	generation   int
	lostStartACK bool
}

func (p *lifecycleBoundary) CreateNotebook(ctx context.Context, i biz.Instance) (string, error) {
	uid, err := p.provider.CreateNotebook(ctx, i)
	p.mounts = append([]biz.VolumeMount{}, i.CurrentMounts...)
	return uid, err
}
func (p *lifecycleBoundary) ObserveIdentity(ctx context.Context, i biz.Instance) (biz.Observation, error) {
	o, err := p.provider.Observe(ctx, i)
	if err != nil {
		return o, err
	}
	o.SafeStopped = o.NotebookExists && o.Stopped && !o.PodExists
	o.StatefulSetUID = "set-original"
	o.StatefulSetExists = o.NotebookExists
	o.StatefulSetReplicas = 1
	if o.Stopped {
		o.StatefulSetReplicas = 0
	}
	if !o.NotebookExists {
		o.StatefulSetUID = ""
	}
	o.NotebookGeneration = 1
	o.StatefulSetGeneration = 1
	o.StatefulSetObservedGeneration = 1
	if o.PodExists {
		o.Pods = []biz.PodRef{{i.Spec.ClusterID, i.Spec.Namespace, o.PodName, o.PodUID, biz.Container}}
	} else {
		o.PodUID = ""
		o.PodName = ""
	}
	return o, nil
}
func (p *lifecycleBoundary) Observe(ctx context.Context, i biz.Instance) (biz.Observation, error) {
	return p.ObserveIdentity(ctx, i)
}
func (p *lifecycleBoundary) VerifyRuntime(ctx context.Context, i biz.Instance, previous biz.Observation) (biz.Observation, error) {
	return p.ObserveIdentity(ctx, i)
}
func (p *lifecycleBoundary) VerifyMountTemplate(ctx context.Context, i biz.Instance, before, target []biz.VolumeMount) (biz.MountTemplateProof, error) {
	o, err := p.ObserveIdentity(ctx, i)
	proof := biz.MountTemplateProof{Class: "mixed", NotebookUID: o.NotebookUID, NotebookRV: o.NotebookRV, StatefulSetUID: o.StatefulSetUID, SafeStopped: o.SafeStopped}
	if o.NotebookExists && biz.MountDigest(p.mounts) == biz.MountDigest(target) {
		proof.Class = "target"
	} else if o.NotebookExists && biz.MountDigest(p.mounts) == biz.MountDigest(before) {
		proof.Class = "old"
	}
	return proof, err
}
func (p *lifecycleBoundary) ApplyMutation(ctx context.Context, i biz.Instance, m biz.Mutation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.obs.NotebookUID != m.TargetUID || p.obs.NotebookRV != m.ResourceVersion {
		return biz.ErrVersion
	}
	if m.StoppedTarget == nil {
		return biz.ErrInvalid
	}
	p.updates++
	p.obs.Stopped = *m.StoppedTarget
	p.obs.MutationID = m.ID
	p.obs.IntentVersion = m.Version
	p.obs.NotebookRV = uuid.NewString()
	p.obs.PodExists = !*m.StoppedTarget
	p.obs.PodReady = !*m.StoppedTarget
	if !*m.StoppedTarget {
		p.generation++
		p.obs.PodUID = fmt.Sprintf("replacement-%d", p.generation)
		p.obs.PodName = "notebook-0"
		if p.lostStartACK {
			p.lostStartACK = false
			return biz.ErrUnknown
		}
	}
	return nil
}
func (p *lifecycleBoundary) SetStopped(ctx context.Context, i biz.Instance, o biz.Observation, stop bool) error {
	if i.Pending != nil && i.Pending.ID != "" {
		return p.ApplyMutation(ctx, i, *i.Pending)
	}
	return p.provider.SetStopped(ctx, i, o, stop)
}
func (p *lifecycleBoundary) UpdateMounts(ctx context.Context, i biz.Instance, m biz.Mutation, target []biz.VolumeMount) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.obs.Stopped || p.obs.NotebookRV != m.ResourceVersion {
		return biz.ErrVersion
	}
	p.mounts = append([]biz.VolumeMount{}, target...)
	p.obs.NotebookRV = uuid.NewString()
	p.obs.MutationID = m.ID
	return nil
}
func TestRestartVerticalPGPhasesReplayAndLostStartACK(t *testing.T) {
	for _, lostACK := range []bool{false, true} {
		t.Run(fmt.Sprint(lostACK), func(t *testing.T) {
			f := database(t)
			c := cpuCreate()
			ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: c.TenantID, Actor: "test-user"})
			if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
				t.Fatal(err)
			}
			boundary := &lifecycleBoundary{lostStartACK: lostACK}
			worker, _ := biz.NewWorker(f.repo, boundary, "restart-pg")
			var i biz.Instance
			for n := 0; n < 8; n++ {
				i = step(t, f, worker, c)
				if i.Observed == biz.Running && i.AppliedMountVersion == 1 && i.ActiveOperation == nil {
					break
				}
			}
			if i.Observed != biz.Running || i.AppliedMountVersion != 1 || i.ActiveOperation != nil {
				t.Fatalf("create not initialized: observed=%s applied=%d reason=%s", i.Observed, i.AppliedMountVersion, i.Reason)
			}
			beforeSpec, _ := json.Marshal(i.Spec)
			oldPod := i.PodUID
			canonical, _ := pb.CanonicalCommandIntent("restart", i.ID, i.Version, "", "", false, false)
			cmd := biz.Command{TenantID: i.TenantID, ID: i.ID, OperationID: uuid.NewString(), Key: uuid.NewString(), Action: "restart", ExpectedVersion: i.Version, CanonicalIntent: canonical, RequestHash: pb.CommandIntentDigest(canonical)}
			ack, err := f.repo.AcceptCommand(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := f.repo.AcceptCommand(ctx, cmd)
			if err != nil || !replay.Replayed || replay.OperationID != ack.OperationID {
				t.Fatal("replay lost original ACK", err)
			}
			stop := biz.Command{TenantID: i.TenantID, ID: i.ID, OperationID: uuid.NewString(), Key: uuid.NewString(), Action: "stop", ExpectedVersion: i.Version + 1, RequestHash: cmd.RequestHash}
			if _, err = f.repo.AcceptCommand(ctx, stop); !errors.Is(err, biz.ErrOperationInProgress) {
				t.Fatalf("stop superseded restart: %v", err)
			}
			stoppedPhase := false
			for n := 0; n < 22; n++ {
				i = step(t, f, worker, c)
				op, err := f.repo.GetOperation(ctx, c.TenantID, ack.OperationID)
				if err != nil {
					t.Fatal(err)
				}
				if i.Version != 2 || i.Desired != biz.Running || i.AppliedMountVersion != 1 {
					t.Fatalf("phase changed public intent/version: %+v", i)
				}
				if op.Execution.Phase == "stopping" || op.Execution.Phase == "waiting_stopped" {
					stoppedPhase = true
					if i.RuntimeDesired != biz.Stopped {
						t.Fatal("runtime desired not persisted")
					}
				}
				if op.State == "completed" {
					break
				}
			}
			op, err := f.repo.GetOperation(ctx, c.TenantID, ack.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if !stoppedPhase || op.State != "completed" || op.Execution.Phase != "done" || op.Execution.CompletionScope != "runtime_verified" || i.PodUID == oldPod || boundary.updates != 2 {
				t.Fatalf("restart did not finish exactly once: state=%s phase=%s reason=%s writes=%d", op.State, op.Execution.Phase, op.Reason, boundary.updates)
			}
			afterSpec, _ := json.Marshal(i.Spec)
			if string(beforeSpec) != string(afterSpec) {
				t.Fatal("restart changed CREATE")
			}
			if a, err := f.repo.LookupCommandReplay(ctx, cmd); err != nil || !a.Replayed || a.OperationID != ack.OperationID {
				t.Fatal("terminal replay failed", err)
			}
			for n := 0; n < 2; n++ {
				step(t, f, worker, c)
			}
			terminal, _ := f.repo.GetOperation(ctx, c.TenantID, ack.OperationID)
			if terminal.State != op.State || terminal.Reason != op.Reason || boundary.updates != 2 {
				t.Fatal("terminal observation restarted operation")
			}
			var outbox int
			if err = f.runtime.QueryRow(ctx, "SELECT count(*) FROM instance.instance_quota_release_outbox WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID).Scan(&outbox); err != nil || outbox != 0 {
				t.Fatal("restart emitted refund", err)
			}
		})
	}
}

func (p *lifecycleBoundary) VerifyAddedMounts(context.Context, biz.Instance, []biz.VolumeMount) error {
	return nil
}

// delayedLifecycleBoundary holds one real conditional start at the external
// boundary. Each ordering preserves its original UID/RV/mutation payload.
type delayedLifecycleBoundary struct {
	lifecycleBoundary
	queued     *biz.Mutation
	fenceOrder string
}

func (p *delayedLifecycleBoundary) ApplyMutation(ctx context.Context, i biz.Instance, m biz.Mutation) error {
	if m.StoppedTarget != nil && !*m.StoppedTarget && m.FencedMutationID == "" {
		original := m
		p.queued = &original
		return biz.ErrUnknown
	}
	if m.FencedMutationID != "" && p.queued != nil {
		switch p.fenceOrder {
		case "original_first":
			if err := p.lifecycleBoundary.ApplyMutation(ctx, i, *p.queued); err != nil {
				return err
			}
			p.fenceOrder = "resolved"
			return biz.ErrVersion
		case "other_rv_first":
			p.mu.Lock()
			p.obs.NotebookRV = uuid.NewString()
			p.mu.Unlock()
			p.fenceOrder = "resolved"
			return biz.ErrVersion
		case "fence_ack_lost":
			err := p.lifecycleBoundary.ApplyMutation(ctx, i, m)
			p.fenceOrder = "resolved"
			if err != nil {
				return err
			}
			return biz.ErrUnknown
		}
	}
	return p.lifecycleBoundary.ApplyMutation(ctx, i, m)
}
func TestRestartVerticalPGDeleteInheritsUnknownStartAndUniqueFence(t *testing.T) {
	for _, order := range []string{"fence_first", "original_first", "fence_ack_lost", "other_rv_first"} {
		t.Run(order, func(t *testing.T) {
			f := database(t)
			c := cpuCreate()
			ctx := context.Background()
			if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
				t.Fatal(err)
			}
			boundary := &delayedLifecycleBoundary{fenceOrder: order}
			worker, _ := biz.NewWorker(f.repo, boundary, "delete-fence-pg")
			var i biz.Instance
			for n := 0; n < 8; n++ {
				i = step(t, f, worker, c)
				if i.ActiveOperation == nil && i.AppliedMountVersion == 1 {
					break
				}
			}
			raw, _ := pb.CanonicalCommandIntent("restart", i.ID, i.Version, "", "", false, false)
			restart := biz.Command{TenantID: i.TenantID, ID: i.ID, OperationID: uuid.NewString(), Key: uuid.NewString(), Action: "restart", ExpectedVersion: i.Version, CanonicalIntent: raw, RequestHash: pb.CommandIntentDigest(raw)}
			if _, err := f.repo.AcceptCommand(ctx, restart); err != nil {
				t.Fatal(err)
			}
			for n := 0; n < 15; n++ {
				i = step(t, f, worker, c)
				if boundary.queued != nil && i.Pending != nil {
					break
				}
			}
			if i.Pending == nil || i.Pending.StoppedTarget == nil || *i.Pending.StoppedTarget {
				t.Fatal("unknown start was not durable")
			}
			original := *i.Pending
			deleted := biz.Delete{InstanceKind: biz.KindContainer, ComputeType: biz.TypeCPU, TenantID: i.TenantID, ID: i.ID, OperationID: uuid.NewString(), CreateOperationID: c.OperationID, ExpectedVersion: i.Version, RequestHash: pb.CommandIntentDigest([]byte("delete"))}
			if _, err := f.repo.AcceptDelete(ctx, deleted); err != nil {
				t.Fatal(err)
			}
			i, err := f.repo.Get(ctx, c.TenantID, c.ID)
			if err != nil || !biz.SameMutation(i.Pending, &original) {
				t.Fatal("DELETE discarded unknown write", err)
			}
			op, _ := f.repo.GetOperation(ctx, c.TenantID, restart.OperationID)
			if op.State != "superseded" {
				t.Fatal("DELETE did not take over restart")
			}
			seenFenceIDs := map[string]bool{}
			for n := 0; n < 22; n++ {
				i = step(t, f, worker, c)
				if i.RuntimeDesired != biz.Deleted || i.Desired != biz.Deleted {
					t.Fatal("recovery changed DELETE target")
				}
				if i.RecoveryFence != nil {
					seenFenceIDs[i.RecoveryFence.ID] = true
				}
				if i.Observed == biz.Deleted {
					break
				}
			}
			if i.Observed != biz.Deleted || i.Pending != nil || i.RecoveryFence != nil || !biz.OwnerCleanupReady(i) || len(seenFenceIDs) != 1 {
				t.Fatalf("DELETE/fence did not converge: order=%s reason=%s pending=%v fence=%v IDs=%d", order, i.Reason, i.Pending != nil, i.RecoveryFence != nil, len(seenFenceIDs))
			}
			if err := boundary.lifecycleBoundary.ApplyMutation(ctx, i, original); !errors.Is(err, biz.ErrVersion) {
				t.Fatal("late original start could apply", err)
			}
			op, _ = f.repo.GetOperation(ctx, c.TenantID, restart.OperationID)
			if op.State != "superseded" {
				t.Fatal("late receipt changed terminal restart")
			}
		})
	}
}
