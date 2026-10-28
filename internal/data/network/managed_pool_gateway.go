package data

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

// Called in the owner's admission transaction, under the platform cluster/key
// locks. No provider calls or independent child idempotency key are needed.
func (p *Postgres) acceptManagedGateway(ctx context.Context, q *sqlcgen.Queries, owner sqlcgen.NetworkPlatformResource, a biz.Attribution, now time.Time) (string, error) {
	id, op := newEgressID("egw"), uuid.NewString()
	_, err := q.InsertPlatformResource(ctx, sqlcgen.InsertPlatformResourceParams{ResourceID: id, Kind: "egress_gateway", ClusterID: owner.ClusterID, Name: id, Description: "Managed by public pool " + owner.ResourceID, CreatedAt: now, LastOperationID: op, ProviderName: strings.Replace(id, "_", "-", 1), BindingID: uuid.NewString()})
	if err != nil {
		return "", databaseFailure(err)
	}
	if err = q.InsertEgressGateway(ctx, sqlcgen.InsertEgressGatewayParams{ResourceID: id, ClusterID: owner.ClusterID}); err != nil {
		return "", databaseFailure(err)
	}
	if err = q.InsertPlatformOperation(ctx, sqlcgen.InsertPlatformOperationParams{ResourceID: id, OperationID: op, Kind: "create_egress_gateway", State: string(biz.Queued), CreatedAt: now, NextAttemptAt: &now}); err != nil {
		return "", databaseFailure(err)
	}
	if err = q.InsertPlatformReconciliation(ctx, sqlcgen.InsertPlatformReconciliationParams{ResourceID: id, NextRunAt: now}); err != nil {
		return "", databaseFailure(err)
	}
	err = q.InsertPlatformHistory(ctx, sqlcgen.InsertPlatformHistoryParams{HistoryID: uuid.NewString(), ResourceID: id, OperationID: &op, Event: "managed_gateway_accepted", ResourceState: string(biz.Provisioning), OperationState: string(biz.Queued), ActorRef: a.Actor, CallerRef: a.DirectCaller, CreatedAt: now})
	return id, databaseFailure(err)
}

// The owner can finish deletion only after both provider objects have gone.
// The shared worker calls this only after direct observation proved pool absence.
// All child intent and owner progress commit together and survive a restart.
func (p *Postgres) finishManagedPoolCleanup(ctx context.Context, q *sqlcgen.Queries, w biz.Work, progress biz.Progress) (biz.Progress, error) {
	pool, err := q.GetPublicPool(ctx, sqlcgen.GetPublicPoolParams{ClusterID: p.placement.ClusterID, ResourceID: w.Resource.ID})
	if err != nil {
		return progress, databaseFailure(err)
	}
	if !pool.ManagedGateway {
		return progress, nil
	}
	gateway, err := q.LockPlatform(ctx, sqlcgen.LockPlatformParams{ClusterID: pool.ClusterID, Kind: "egress_gateway", ResourceID: textValue(pool.GatewayID)})
	if err != nil {
		return progress, databaseFailure(err)
	}
	if gateway.State == "deleted" {
		return progress, nil
	}
	if gateway.State != "deleting" {
		old, err := q.GetPlatformOperation(ctx, sqlcgen.GetPlatformOperationParams{ClusterID: gateway.ClusterID, OperationID: gateway.LastOperationID})
		if err != nil {
			return progress, databaseFailure(err)
		}
		if old.CompletedAt == nil {
			if err = affected(q.SupersedePlatformCreate(ctx, sqlcgen.SupersedePlatformCreateParams{ResourceID: gateway.ResourceID, OperationID: old.OperationID})); err != nil {
				return progress, err
			}
		}
		now, err := q.DatabaseTime(ctx)
		if err != nil {
			return progress, databaseFailure(err)
		}
		op := uuid.NewString()
		if _, err = q.SetPlatformIntent(ctx, sqlcgen.SetPlatformIntentParams{ClusterID: gateway.ClusterID, ResourceID: gateway.ResourceID, Version: gateway.Version, State: string(biz.Deleting), OperationID: op}); err != nil {
			return progress, databaseFailure(err)
		}
		if err = q.InsertPlatformOperation(ctx, sqlcgen.InsertPlatformOperationParams{ResourceID: gateway.ResourceID, OperationID: op, Kind: "delete_egress_gateway", State: string(biz.Queued), CreatedAt: now, NextAttemptAt: &now}); err != nil {
			return progress, databaseFailure(err)
		}
		if err = affected(q.SchedulePlatform(ctx, sqlcgen.SchedulePlatformParams{ResourceID: gateway.ResourceID})); err != nil {
			return progress, err
		}
		if err = q.InsertPlatformHistory(ctx, sqlcgen.InsertPlatformHistoryParams{HistoryID: uuid.NewString(), ResourceID: gateway.ResourceID, OperationID: &op, Event: "owner_cleanup_accepted", ResourceState: string(biz.Deleting), OperationState: string(biz.Queued), CreatedAt: now}); err != nil {
			return progress, databaseFailure(err)
		}
	}
	progress.State, progress.OperationState, progress.Reason = biz.Deleting, biz.Retrying, biz.CleanupPending
	progress.Backoff = true
	return progress, nil
}
