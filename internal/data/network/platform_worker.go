package data

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func (p *Postgres) claimPlatform(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, owner string, duration time.Duration) (biz.Work, bool, error) {
	r, err := q.DuePlatformCandidate(ctx, sqlcgen.DuePlatformCandidateParams{ClusterID: p.placement.ClusterID})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.Work{}, false, nil
	}
	if err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	if r.ClusterID != p.placement.ClusterID {
		return biz.Work{}, false, biz.Fail(biz.DependencyUnavailable, "platform placement does not match executor")
	}
	lease, err := q.AcquirePlatformLease(ctx, sqlcgen.AcquirePlatformLeaseParams{ResourceID: r.ResourceID, Owner: owner, LeaseMicros: duration.Microseconds()})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.Work{}, false, nil
	}
	if err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	op, err := q.GetPlatformOperation(ctx, sqlcgen.GetPlatformOperationParams{ClusterID: p.placement.ClusterID, OperationID: r.LastOperationID})
	if err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	active := op.CompletedAt == nil
	if active {
		op, err = q.RunPlatformOperation(ctx, sqlcgen.RunPlatformOperationParams{ResourceID: r.ResourceID, OperationID: op.OperationID, Epoch: lease.LeaseEpoch})
		if err != nil {
			return biz.Work{}, false, databaseFailure(err)
		}
	}
	value, err := platformSnapshot(ctx, q, r)
	if err != nil {
		return biz.Work{}, false, err
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	work := biz.Work{Resource: biz.ResourceWork{ID: r.ResourceID, Kind: r.Kind, State: biz.ResourceState(r.State), Reason: biz.Reason(r.Reason), Version: r.Version, ObservedAt: r.ObservedAt, UpdatedAt: r.UpdatedAt, LastOperationID: r.LastOperationID, Egress: &biz.EgressWorkSpec{Platform: &value}}, Operation: biz.Operation{ID: op.OperationID, ResourceID: r.ResourceID, ResourceType: r.Kind, Kind: op.Kind, State: biz.OperationState(op.State), Reason: biz.Reason(op.Reason)}, ActiveOperation: active, BindingID: r.BindingID, KnownIdentity: r.ProviderUid, PendingAction: r.PendingAction, Owner: owner, Epoch: lease.LeaseEpoch, Attempt: op.Attempt, Now: now, Requirement: biz.ObservationRequirement{RequestedGeneration: lease.RequestedGeneration, AppliedAt: lease.EvidenceAppliedAt, Hash: lease.EvidenceHash}}
	if r.ObservedAt != nil {
		work.Requirement.PreviousCollectedAt = *r.ObservedAt
	}
	if r.Kind == "public_pool" && r.State == "provisioning" && active {
		pool, err := q.GetPublicPool(ctx, sqlcgen.GetPublicPoolParams{ClusterID: r.ClusterID, ResourceID: r.ResourceID})
		if err != nil {
			return biz.Work{}, false, databaseFailure(err)
		}
		if pool.ManagedGateway {
			gateway, err := q.GetPlatform(ctx, sqlcgen.GetPlatformParams{ClusterID: r.ClusterID, Kind: "egress_gateway", ResourceID: textValue(pool.GatewayID)})
			if err != nil {
				return biz.Work{}, false, databaseFailure(err)
			}
			if !freshResource(gateway.State, gateway.ObservedAt, now, p.baseFreshnessLimit()) {
				work.GateReason = biz.ProviderNotReady
			}
			if gateway.State == "failed" || gateway.State == "deleted" || gateway.State == "deleting" {
				work.DependencyFailed = true
			}
		}
	}
	if value.Vlan != nil && value.Vlan.VirtualSwitchID != "" && r.State != "deleting" && r.State != "deleted" {
		s, err := q.GetVirtualSwitch(ctx, sqlcgen.GetVirtualSwitchParams{ResourceID: value.Vlan.VirtualSwitchID})
		if err != nil {
			return biz.Work{}, false, databaseFailure(err)
		}
		if s.State != "available" || s.BindingState != "configured" {
			work.GateReason = biz.ProviderNotReady
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return biz.Work{}, false, databaseFailure(err)
	}
	return work, true, nil
}
func (p *Postgres) lockedPlatform(ctx context.Context, q *sqlcgen.Queries, w biz.Work) error {
	r, err := q.LockPlatform(ctx, sqlcgen.LockPlatformParams{ClusterID: p.placement.ClusterID, Kind: w.Resource.Kind, ResourceID: w.Resource.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.ErrLeaseLost
	}
	if err != nil {
		return databaseFailure(err)
	}
	if r.Version != w.Resource.Version || r.BindingID != w.BindingID {
		return biz.ErrLeaseLost
	}
	_, err = q.CheckPlatformLease(ctx, sqlcgen.CheckPlatformLeaseParams{ResourceID: r.ResourceID, Owner: w.Owner, Epoch: w.Epoch})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.ErrLeaseLost
	}
	return databaseFailure(err)
}
func (p *Postgres) beginPlatformMutation(ctx context.Context, w biz.Work, action, identity string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if err = p.lockedPlatform(ctx, q, w); err != nil {
		return err
	}
	if err = affected(q.BeginPlatformMutation(ctx, sqlcgen.BeginPlatformMutationParams{ResourceID: w.Resource.ID, BindingID: w.BindingID, Action: action, Identity: identity})); err != nil {
		return err
	}
	return databaseFailure(tx.Commit(ctx))
}
func (p *Postgres) finishPlatform(ctx context.Context, w biz.Work, progress biz.Progress) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if err = p.lockedPlatform(ctx, q, w); err != nil {
		return err
	}
	if progress.Observed && !progress.Proof.Covers(w.Requirement) {
		return biz.ErrLeaseLost
	}
	if w.Resource.Kind == "public_pool" && progress.State == biz.Deleted {
		progress, err = p.finishManagedPoolCleanup(ctx, q, w, progress)
		if err != nil {
			return err
		}
	}
	r, err := q.AdvancePlatform(ctx, sqlcgen.AdvancePlatformParams{ResourceID: w.Resource.ID, Version: w.Resource.Version, State: string(progress.State), Reason: string(progress.Reason), Identity: progress.Identity, Observed: progress.Observed, ObservedAt: proofTime(progress.Observed, progress.Proof), ClearPending: progress.ClearPending})
	if err != nil {
		return databaseFailure(err)
	}
	if progress.Observed && r.Kind == "public_pool" && progress.Egress != nil {
		images := progress.Egress.ProviderImages
		if images == nil {
			images = []string{}
		}
		if err = affected(q.SavePlatformImages(ctx, sqlcgen.SavePlatformImagesParams{ClusterID: r.ClusterID, ResourceID: r.ResourceID, ProviderImages: images})); err != nil {
			return err
		}
	}
	if progress.State == biz.Deleted {
		switch r.Kind {
		case "vlan":
			err = affected(q.RetireVlanSlot(ctx, sqlcgen.RetireVlanSlotParams{ClusterID: r.ClusterID, ResourceID: r.ResourceID}))
		case "public_pool":
			err = affected(q.RetirePublicPoolSlot(ctx, sqlcgen.RetirePublicPoolSlotParams{ClusterID: r.ClusterID, ResourceID: r.ResourceID}))
		}
		if err != nil {
			return err
		}
	}
	if progress.Egress != nil && len(progress.Egress.DeviceNodes) > 0 && r.Kind == "device" {
		body, err := json.Marshal(progress.Egress.DeviceNodes)
		if err != nil {
			return err
		}
		if err = affected(q.SaveDeviceProgress(ctx, sqlcgen.SaveDeviceProgressParams{ClusterID: r.ClusterID, ResourceID: r.ResourceID, NodeProgress: body})); err != nil {
			return err
		}
	}
	if w.ActiveOperation {
		if err = affected(q.CompletePlatformAttempt(ctx, sqlcgen.CompletePlatformAttemptParams{ResourceID: r.ResourceID, OperationID: w.Operation.ID, Epoch: w.Epoch, State: string(progress.OperationState), Reason: string(progress.Reason), DelayMicros: progress.NextDelay.Microseconds()})); err != nil {
			return err
		}
	}
	if w.ActiveOperation || w.Resource.State != progress.State || w.Resource.Reason != progress.Reason {
		record := sqlcgen.InsertPlatformHistoryParams{HistoryID: uuid.NewString(), ResourceID: r.ResourceID, Event: "reconciled", ResourceState: r.State, Reason: r.Reason, CreatedAt: r.UpdatedAt}
		if w.ActiveOperation {
			record.OperationID = &w.Operation.ID
			record.OperationState = string(progress.OperationState)
		}
		if err = q.InsertPlatformHistory(ctx, record); err != nil {
			return databaseFailure(err)
		}
	}
	if err = affected(q.ReleasePlatformLease(ctx, sqlcgen.ReleasePlatformLeaseParams{ResourceID: r.ResourceID, Owner: w.Owner, Epoch: w.Epoch, DelayMicros: progress.NextDelay.Microseconds(), CoveredGeneration: coveredGeneration(progress.Observed, progress.Proof, w.Requirement), Observed: progress.Observed, EvidenceHash: progress.Proof.Hash, Backoff: progress.Backoff})); err != nil {
		return err
	}
	return databaseFailure(tx.Commit(ctx))
}
