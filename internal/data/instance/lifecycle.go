package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
)

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

const operationSelect = `SELECT tenant_id::text,operation_id::text,instance_id::text,action,idempotency_key,request_hash,version,ack,state,reason,created_at,execution FROM instance.instance_operations WHERE tenant_id=$1 AND operation_id=$2`

func scanOperation(row scanner) (biz.Operation, error) {
	var o biz.Operation
	var ack, execution []byte
	err := row.Scan(&o.TenantID, &o.ID, &o.InstanceID, &o.Action, &o.Key, &o.RequestHash, &o.Version, &ack, &o.State, &o.Reason, &o.CreatedAt, &execution)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, biz.ErrNotFound
	}
	if err != nil {
		return o, err
	}
	if err = json.Unmarshal(ack, &o.ACK); err != nil {
		return o, err
	}
	if len(execution) > 0 {
		o.Execution = new(biz.Execution)
		if err = json.Unmarshal(execution, o.Execution); err != nil {
			return o, err
		}
		if o.Execution.SchemaVersion != 1 {
			return o, biz.ErrInvalid
		}
	}
	return o, nil
}
func (p *Postgres) loadActiveOperation(ctx context.Context, q querier, i *biz.Instance) error {
	if i.LastOperationID == "" {
		return nil
	}
	o, err := scanOperation(q.QueryRow(ctx, operationSelect, i.TenantID, i.LastOperationID))
	if err != nil {
		return err
	}
	if !biz.TerminalOperation(o.State) {
		i.ActiveOperation = &o
	}
	return nil
}
func initialMounts(m []biz.VolumeMount) []biz.VolumeMount {
	out := append([]biz.VolumeMount{}, m...)
	for k := range out {
		out[k].ReferenceGeneration = 1
	}
	return out
}
func marshalMounts(m []biz.VolumeMount) []byte {
	raw, _ := json.Marshal(biz.StoreMounts(m))
	return raw
}
func decodeCurrentInstance(raw []byte, version int64, desired string, created, updated time.Time, mounts []byte, initialized bool, applied int64, runtime string, fence []byte) (biz.Instance, error) {
	i := biz.Instance{RuntimeDesired: runtime}
	if err := decodeSnapshot(raw, &i); err != nil {
		return i, err
	}
	if i.Desired != desired {
		return i, fmt.Errorf("instance desired snapshot mismatch")
	}
	i.Version = version
	i.Desired = desired
	i.CreatedAt = created
	i.UpdatedAt = updated
	i.RuntimeDesired = runtime
	i.MountsInitialized = initialized
	i.AppliedMountVersion = applied
	var stored []biz.StoredMount
	if len(mounts) == 0 || json.Unmarshal(mounts, &stored) != nil || stored == nil {
		return i, biz.ErrInvalid
	}
	i.CurrentMounts = biz.LoadMounts(stored)
	if applied < 0 || applied > version || (runtime != biz.Running && runtime != biz.Stopped && runtime != biz.Deleted) {
		return i, biz.ErrInvalid
	}
	if len(fence) > 0 {
		i.RecoveryFence = new(biz.Mutation)
		if err := json.Unmarshal(fence, i.RecoveryFence); err != nil {
			return i, err
		}
	}
	return i, nil
}
func saveIndependentColumns(ctx context.Context, tx pgx.Tx, i biz.Instance) error {
	var fence []byte
	var err error
	if i.RecoveryFence != nil {
		fence, err = json.Marshal(i.RecoveryFence)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE instance.instances SET desired=$3,runtime_desired=$4,current_mounts=$5,mounts_initialized=$6,applied_mount_version=$7,recovery_fence=$8 WHERE tenant_id=$1 AND instance_id=$2`, i.TenantID, i.ID, i.Desired, i.RuntimeDesired, marshalMounts(i.CurrentMounts), i.MountsInitialized, i.AppliedMountVersion, fence)
	return err
}
func newExecution(i biz.Instance, action string) *biz.Execution {
	now := time.Now().UTC()
	phase := map[string]string{"create": "creating", "start": "starting", "stop": "stopping", "delete": "deleting"}[action]
	e := &biz.Execution{SchemaVersion: 1, Phase: phase, PhaseSequence: 1, BeforeRunState: i.RuntimeDesired, FinalRunState: i.Desired, BeforeMounts: biz.StoreMounts(i.CurrentMounts), AppliedMountVersion: i.AppliedMountVersion, StartedAt: now}
	if biz.NewLifecycleAction(action) {
		e.Phase = "validating"
		deadline := now.Add(30 * time.Minute)
		e.DeadlineAt = &deadline
	}
	return e
}
func insertOperation(ctx context.Context, tx pgx.Tx, i biz.Instance, id, action, key, hash string, ack biz.ACK) error {
	raw, err := json.Marshal(ack)
	if err != nil {
		return err
	}
	execution := newExecution(i, action)
	if i.ActiveOperation != nil && i.ActiveOperation.ID == id && i.ActiveOperation.Execution != nil {
		execution = i.ActiveOperation.Execution
	}
	stored, err := json.Marshal(execution)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO instance.instance_operations(tenant_id,operation_id,instance_id,action,idempotency_key,request_hash,version,ack,execution) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, i.TenantID, id, i.ID, action, key, hash, i.Version, raw, stored)
	return err
}
func saveProgressOperation(ctx context.Context, tx pgx.Tx, w biz.Work, p biz.Progress) error {
	if p.Operation == nil {
		return nil
	}
	o := p.Operation
	if w.Operation == nil || o.ID != w.Operation.ID || o.Version != w.Operation.Version || o.Execution == nil || w.Operation.Execution == nil {
		return biz.ErrVersion
	}
	if biz.NewLifecycleAction(o.Action) && o.State == "completed" {
		e := o.Execution
		if e.Phase != "done" || p.Instance.Pending != nil || p.Instance.RecoveryFence != nil || p.Instance.Desired != e.FinalRunState || p.Instance.RuntimeDesired != e.FinalRunState {
			return biz.ErrState
		}
		if e.CompletionScope == "runtime_verified" {
			if e.VerifiedPodUID == "" || e.VerifiedPodUID != p.Instance.PodUID || p.Instance.Observed != biz.Running {
				return biz.ErrState
			}
			if n := p.Instance.Network; n != nil {
				if n.NetworkState != "attached" || n.Reason != "" || n.SnapshotRevision != n.ConfirmedSnapshotRevision || n.SnapshotRevision != e.NetworkSnapshotRevision || len(n.CurrentPods) != 1 || n.CurrentPods[0].Reference.UID != e.VerifiedPodUID {
					return biz.ErrState
				}
			}
		} else if e.CompletionScope == "template_applied" {
			if e.TargetConfiguration == nil || !e.TemplateCommitCompleted || e.TemplateProof == nil || !e.TemplateProof.SafeStopped || p.Instance.Observed != biz.Stopped || p.Instance.AppliedMountVersion != o.Version || biz.MountDigest(p.Instance.CurrentMounts) != e.TargetMountDigest {
				return biz.ErrState
			}
		} else {
			return biz.ErrState
		}
	}
	if o.Execution.TemplateCommitCompleted && !w.Operation.Execution.TemplateCommitCompleted {
		if o.Execution.TemplateProof == nil || o.Execution.TemplateProof.Class != "target" || !o.Execution.TemplateProof.SafeStopped || o.Execution.TargetConfiguration == nil {
			return biz.ErrState
		}
		if err := commitMountReferences(ctx, tx, *o); err != nil {
			return err
		}
	}
	if o.State == "failed" && !o.Execution.TemplateCommitCompleted && o.Execution.TargetConfiguration != nil {
		if err := finishUnappliedReferences(ctx, tx, *o); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(o.Execution)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE instance.instance_operations SET execution=$3,state=$4,reason=$5 WHERE tenant_id=$1 AND operation_id=$2 AND version=$6 AND state IN ('accepted','blocked') AND (execution->>'phase_sequence')::bigint=$7 AND execution->>'phase'=$8`, o.TenantID, o.ID, raw, o.State, o.Reason, o.Version, w.Operation.Execution.PhaseSequence, w.Operation.Execution.Phase)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return biz.ErrVersion
	}
	return nil
}
func lookupReplay(ctx context.Context, q querier, c biz.Command) (biz.ACK, error) {
	var raw []byte
	var hash string
	err := q.QueryRow(ctx, `SELECT ack,request_hash FROM instance.instance_operations WHERE tenant_id=$1 AND instance_id=$2 AND action=$3 AND idempotency_key=$4`, c.TenantID, c.ID, c.Action, c.Key).Scan(&raw, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.ACK{}, biz.ErrNotFound
	}
	if err != nil {
		return biz.ACK{}, err
	}
	if hash != c.RequestHash {
		return biz.ACK{}, biz.ErrConflict
	}
	var ack biz.ACK
	err = json.Unmarshal(raw, &ack)
	ack.Replayed = true
	return ack, err
}
func (p *Postgres) LookupCommandReplay(ctx context.Context, c biz.Command) (biz.ACK, error) {
	if err := biz.RequireTenant(ctx, c.TenantID); err != nil {
		return biz.ACK{}, err
	}
	return lookupReplay(ctx, p.pool, c)
}
func (p *Postgres) AcceptCommand(ctx context.Context, c biz.Command) (biz.ACK, error) {
	var ack biz.ACK
	if !biz.UUID(c.TenantID) || !biz.UUID(c.ID) || !biz.UUID(c.OperationID) || !biz.UUID(c.Key) || len(c.RequestHash) != 64 || c.ExpectedVersion < 1 {
		return ack, biz.ErrInvalid
	}
	if c.Action != "start" && c.Action != "stop" && !biz.NewLifecycleAction(c.Action) {
		return ack, biz.ErrInvalid
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return ack, err
	}
	defer tx.Rollback(ctx)
	i, err := scanInstance(tx.QueryRow(ctx, instanceSelect+" FOR UPDATE", c.TenantID, c.ID))
	if err != nil {
		return ack, err
	}
	ack, err = lookupReplay(ctx, tx, c)
	if err == nil {
		return ack, nil
	}
	if !errors.Is(err, biz.ErrNotFound) {
		return ack, err
	}
	if i.Version != c.ExpectedVersion {
		return ack, biz.ErrVersion
	}
	if i.Desired == biz.Deleted {
		return ack, biz.ErrDeleting
	}
	var active int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM instance.instance_operations WHERE tenant_id=$1 AND instance_id=$2 AND state IN ('accepted','blocked')`, i.TenantID, i.ID).Scan(&active); err != nil {
		return ack, err
	}
	if active != 0 || i.Pending != nil || i.RecoveryFence != nil {
		return ack, biz.ErrOperationInProgress
	}
	if c.Action == "start" && !i.MountsInitialized {
		return ack, biz.ErrState
	}
	if biz.NewLifecycleAction(c.Action) && (!i.MountsInitialized || i.AppliedMountVersion == 0 || i.Desired != i.RuntimeDesired || i.Desired != i.Observed || (i.Desired != biz.Running && i.Desired != biz.Stopped)) {
		return ack, biz.ErrState
	}
	if c.Action == "restart" && i.Desired != biz.Running {
		return ack, biz.ErrState
	}
	if (c.Action == "attach-pvc" || c.Action == "detach-pvc") && i.Desired == biz.Running && !c.RestartIfRunning {
		return ack, biz.ErrRequiresStop
	}
	e := newExecution(i, c.Action)
	e.CanonicalIntent = append([]byte(nil), c.CanonicalIntent...)
	mounts := append([]biz.VolumeMount{}, i.CurrentMounts...)
	if c.Action == "attach-pvc" {
		if c.Mount == nil || c.Mount.ClusterID != i.Spec.ClusterID || c.Mount.Namespace != i.Spec.Namespace || c.Mount.PVCUID == "" || c.Mount.Ownership != "borrowed" {
			return ack, biz.ErrInvalid
		}
		for _, m := range mounts {
			if m.MountPath == c.Mount.MountPath {
				return ack, biz.ErrMountPathInUse
			}
		}
		if err = lockMountGuards(ctx, tx, i.TenantID, []biz.VolumeMount{*c.Mount}); err != nil {
			return ack, err
		}
		generation := int64(1)
		err = tx.QueryRow(ctx, `SELECT reference_generation+1 FROM instance.borrowed_volume_references WHERE tenant_id=$1 AND instance_id=$2 AND mount_path=$3 AND state='released' FOR UPDATE`, i.TenantID, i.ID, c.Mount.MountPath).Scan(&generation)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return ack, err
		}
		m := *c.Mount
		m.ReferenceGeneration = generation
		mounts = append(mounts, m)
	} else if c.Action == "detach-pvc" {
		if c.MountPath == i.Spec.WorkspaceMountPath {
			return ack, biz.ErrWorkspaceDetach
		}
		found := false
		target := []biz.VolumeMount{}
		for _, m := range mounts {
			if m.MountPath == c.MountPath {
				found = true
			} else {
				target = append(target, m)
			}
		}
		if !found {
			return ack, biz.ErrMountNotFound
		}
		if err = lockMountGuards(ctx, tx, i.TenantID, mounts); err != nil {
			return ack, err
		}
		mounts = target
	}
	if c.Action == "attach-pvc" || c.Action == "detach-pvc" {
		e.TargetConfiguration = &biz.MountTarget{VolumeMounts: biz.StoreMounts(mounts)}
		e.TargetMountDigest = biz.MountDigest(mounts)
	}
	if biz.NewLifecycleAction(c.Action) {
		i.Observed = "pending"
	}
	i.Version++
	i.LastOperationID = c.OperationID
	i.Reason = ""
	if c.Action == "start" {
		i.Desired = biz.Running
		i.RuntimeDesired = biz.Running
	}
	if c.Action == "stop" {
		i.Desired = biz.Stopped
		i.RuntimeDesired = biz.Stopped
	}
	e.FinalRunState = i.Desired
	i.ActiveOperation = &biz.Operation{TenantID: i.TenantID, ID: c.OperationID, InstanceID: i.ID, Action: c.Action, Version: i.Version, State: "accepted", Execution: e}
	networkIntent(&i)
	ack = biz.ACK{OperationID: c.OperationID, ResourceID: c.ID, Accepted: true}
	if err = insertOperation(ctx, tx, i, c.OperationID, c.Action, c.Key, c.RequestHash, ack); err != nil {
		return ack, err
	}
	if c.Action == "attach-pvc" {
		m := mounts[len(mounts)-1]
		_, err = tx.Exec(ctx, `INSERT INTO instance.borrowed_volume_references(tenant_id,instance_id,mount_path,cluster_id,namespace,pvc_name,pvc_uid,read_only,state,reference_generation,operation_id,intent_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'pending',$9,$10,$11) ON CONFLICT(tenant_id,instance_id,mount_path) DO UPDATE SET cluster_id=EXCLUDED.cluster_id,namespace=EXCLUDED.namespace,pvc_name=EXCLUDED.pvc_name,pvc_uid=EXCLUDED.pvc_uid,read_only=EXCLUDED.read_only,state='pending',reference_generation=EXCLUDED.reference_generation,operation_id=EXCLUDED.operation_id,intent_version=EXCLUDED.intent_version WHERE borrowed_volume_references.state='released'`, i.TenantID, i.ID, m.MountPath, m.ClusterID, m.Namespace, m.PVCName, m.PVCUID, m.ReadOnly, m.ReferenceGeneration, c.OperationID, i.Version)
		i.VolumeReferencesReleased = false
	} else if c.Action == "detach-pvc" {
		var tag interface{ RowsAffected() int64 }
		tag, err = tx.Exec(ctx, `UPDATE instance.borrowed_volume_references SET state='retiring',operation_id=$4,intent_version=$5 WHERE tenant_id=$1 AND instance_id=$2 AND mount_path=$3 AND state='active'`, i.TenantID, i.ID, c.MountPath, c.OperationID, i.Version)
		if err == nil && tag.RowsAffected() != 1 {
			err = biz.ErrState
		}
	}
	if err != nil {
		return ack, err
	}
	raw, err := json.Marshal(i)
	if err != nil {
		return ack, err
	}
	if _, err = tx.Exec(ctx, `UPDATE instance.instances SET version=$3,desired=$4,snapshot=$5,updated_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2`, i.TenantID, i.ID, i.Version, i.Desired, raw); err != nil {
		return ack, err
	}
	if err = saveIndependentColumns(ctx, tx, i); err != nil {
		return ack, err
	}
	if err = saveNetworkSubmission(ctx, tx, i); err != nil {
		return ack, err
	}
	if _, err = tx.Exec(ctx, `UPDATE instance.instance_work SET due_at=clock_timestamp() WHERE tenant_id=$1 AND instance_id=$2`, i.TenantID, i.ID); err != nil {
		return ack, err
	}
	return ack, tx.Commit(ctx)
}
func lockMountGuards(ctx context.Context, tx pgx.Tx, tenant string, mounts []biz.VolumeMount) error {
	sorted := append([]biz.VolumeMount(nil), mounts...)
	sort.Slice(sorted, func(a, b int) bool {
		x, y := sorted[a], sorted[b]
		return x.ClusterID+"\x00"+x.Namespace+"\x00"+x.PVCName < y.ClusterID+"\x00"+y.Namespace+"\x00"+y.PVCName
	})
	for _, m := range sorted {
		if _, err := tx.Exec(ctx, `INSERT INTO instance.borrowed_pvc_guards(cluster_id,namespace,pvc_name,tenant_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, m.ClusterID, m.Namespace, m.PVCName, tenant); err != nil {
			return err
		}
		var owner, phase string
		if err := tx.QueryRow(ctx, `SELECT tenant_id::text,phase FROM instance.borrowed_pvc_guards WHERE cluster_id=$1 AND namespace=$2 AND pvc_name=$3 FOR UPDATE`, m.ClusterID, m.Namespace, m.PVCName).Scan(&owner, &phase); err != nil {
			return err
		}
		if owner != tenant {
			return biz.ErrDenied
		}
		if phase == "reserved" || phase == "deleting" || phase == "observing" {
			return biz.ErrPVCDeleting
		}
	}
	return nil
}

func (p *Postgres) BeginRecoveryFence(ctx context.Context, w biz.Work, m biz.Mutation) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	i, err := scanInstance(tx.QueryRow(ctx, instanceSelect+" FOR UPDATE", w.Instance.TenantID, w.Instance.ID))
	if err != nil {
		return err
	}
	if err = lockWork(ctx, tx, w); err != nil {
		return err
	}
	if i.Version != w.Instance.Version || i.LastOperationID != w.Instance.LastOperationID || i.Pending == nil || i.RecoveryFence != nil || m.FencedMutationID != i.Pending.ID || m.StoppedTarget == nil || !*m.StoppedTarget {
		return biz.ErrVersion
	}
	i.RecoveryFence = &m
	if err = saveIndependentColumns(ctx, tx, i); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func commitMountReferences(ctx context.Context, tx pgx.Tx, o biz.Operation) error {
	e := o.Execution
	if err := lockMountGuards(ctx, tx, o.TenantID, append(biz.LoadMounts(e.BeforeMounts), biz.LoadMounts(e.TargetConfiguration.VolumeMounts)...)); err != nil {
		return err
	}
	state := "active"
	expected := "pending"
	mounts := e.TargetConfiguration.VolumeMounts
	if o.Action == "detach-pvc" {
		state = "released"
		expected = "retiring"
		mounts = e.BeforeMounts
	}
	for _, m := range mounts {
		involved := false
		if o.Action == "attach-pvc" {
			involved = true
			for _, before := range e.BeforeMounts {
				if before.MountPath == m.MountPath {
					involved = false
					break
				}
			}
		} else {
			involved = true
			for _, target := range e.TargetConfiguration.VolumeMounts {
				if target.MountPath == m.MountPath {
					involved = false
					break
				}
			}
		}
		if !involved {
			continue
		}
		tag, err := tx.Exec(ctx, `UPDATE instance.borrowed_volume_references SET state=$7 WHERE tenant_id=$1 AND instance_id=$2 AND mount_path=$3 AND reference_generation=$4 AND operation_id=$5 AND state=$6`, o.TenantID, o.InstanceID, m.MountPath, m.ReferenceGeneration, o.ID, expected, state)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return biz.ErrVersion
		}
	}
	return nil
}

func finishUnappliedReferences(ctx context.Context, tx pgx.Tx, o biz.Operation) error {
	e := o.Execution
	if err := lockMountGuards(ctx, tx, o.TenantID, append(biz.LoadMounts(e.BeforeMounts), biz.LoadMounts(e.TargetConfiguration.VolumeMounts)...)); err != nil {
		return err
	}
	expected, next := "pending", "released"
	mounts := e.TargetConfiguration.VolumeMounts
	if o.Action == "detach-pvc" {
		expected, next = "retiring", "active"
		mounts = e.BeforeMounts
	}
	for _, m := range mounts {
		tag, err := tx.Exec(ctx, `UPDATE instance.borrowed_volume_references SET state=$7 WHERE tenant_id=$1 AND instance_id=$2 AND mount_path=$3 AND reference_generation=$4 AND operation_id=$5 AND state=$6`, o.TenantID, o.InstanceID, m.MountPath, m.ReferenceGeneration, o.ID, expected, next)
		if err != nil {
			return err
		}
		_ = tag
	}
	return nil
}

func (p *Postgres) TrackLegacyPending(ctx context.Context, w biz.Work) (biz.Mutation, error) {
	var zero biz.Mutation
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback(ctx)
	i, err := scanInstance(tx.QueryRow(ctx, instanceSelect+" FOR UPDATE", w.Instance.TenantID, w.Instance.ID))
	if err != nil {
		return zero, err
	}
	if i.Version != w.Instance.Version || i.LastOperationID != w.Instance.LastOperationID || !biz.SameMutation(i.Pending, w.Instance.Pending) || i.Pending == nil || i.Pending.Kind != "set_stopped" || i.Pending.ID != "" {
		return zero, biz.ErrVersion
	}
	if err = lockWork(ctx, tx, w); err != nil {
		return zero, err
	}
	m := *i.Pending
	m.ID = uuid.NewString()
	m.ProviderMarkerExpected = false
	i.Pending = &m
	raw, err := json.Marshal(i)
	if err != nil {
		return zero, err
	}
	if _, err = tx.Exec(ctx, "UPDATE instance.instances SET snapshot=$3 WHERE tenant_id=$1 AND instance_id=$2", i.TenantID, i.ID, raw); err != nil {
		return zero, err
	}
	if err = releaseMetadataWork(ctx, tx, w); err != nil {
		return zero, err
	}
	return m, tx.Commit(ctx)
}

func (p *Postgres) InitializeLegacyDeleteExecution(ctx context.Context, w biz.Work) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	i, err := scanInstance(tx.QueryRow(ctx, instanceSelect+" FOR UPDATE", w.Instance.TenantID, w.Instance.ID))
	if err != nil {
		return err
	}
	if i.Version != w.Instance.Version || i.LastOperationID != w.Instance.LastOperationID || i.Desired != biz.Deleted {
		return biz.ErrVersion
	}
	o, err := scanOperation(tx.QueryRow(ctx, operationSelect+" FOR UPDATE", i.TenantID, i.LastOperationID))
	if err != nil {
		return err
	}
	if o.Action != "delete" || biz.TerminalOperation(o.State) || o.Execution != nil {
		return biz.ErrVersion
	}
	if err = lockWork(ctx, tx, w); err != nil {
		return err
	}
	raw, err := json.Marshal(&biz.Execution{SchemaVersion: 1, Phase: "deleting", PhaseSequence: 1, FinalRunState: biz.Deleted})
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE instance.instance_operations SET execution=$3 WHERE tenant_id=$1 AND operation_id=$2 AND execution IS NULL", i.TenantID, o.ID, raw); err != nil {
		return err
	}
	if err = releaseMetadataWork(ctx, tx, w); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// The metadata-only step has finished under lockWork. Let a fresh claim read
// the normalized record rather than retaining a lease for work we no longer do.
func releaseMetadataWork(ctx context.Context, tx pgx.Tx, w biz.Work) error {
	_, err := tx.Exec(ctx, `UPDATE instance.instance_work SET lease_owner='',lease_until='-infinity',due_at=clock_timestamp()+interval '5 seconds' WHERE tenant_id=$1 AND instance_id=$2`, w.Instance.TenantID, w.Instance.ID)
	return err
}

func lockReleaseGuards(ctx context.Context, tx pgx.Tx, tenant, id string) error {
	rows, err := tx.Query(ctx, `SELECT DISTINCT cluster_id::text,namespace,pvc_name FROM instance.borrowed_volume_references WHERE tenant_id=$1 AND instance_id=$2 AND state IN ('pending','active','retiring') ORDER BY cluster_id::text,namespace,pvc_name`, tenant, id)
	if err != nil {
		return err
	}
	var identities [][3]string
	for rows.Next() {
		var x [3]string
		if err = rows.Scan(&x[0], &x[1], &x[2]); err != nil {
			rows.Close()
			return err
		}
		identities = append(identities, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, x := range identities {
		var owner string
		if err = tx.QueryRow(ctx, `SELECT tenant_id::text FROM instance.borrowed_pvc_guards WHERE cluster_id=$1 AND namespace=$2 AND pvc_name=$3 FOR UPDATE`, x[0], x[1], x[2]).Scan(&owner); err != nil {
			return err
		}
		if owner != tenant {
			return biz.ErrDenied
		}
	}
	return nil
}
