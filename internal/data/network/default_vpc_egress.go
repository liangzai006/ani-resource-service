package data

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

type DefaultVPCEgressPostgres struct {
	base      *Postgres
	identity  biz.DefaultVPCIdentity
	provider  biz.DefaultEgressProvider
	freshness time.Duration
}

func NewDefaultVPCEgressRepository(p *Postgres, i biz.DefaultVPCIdentity, provider biz.DefaultEgressProvider, freshness time.Duration) (*DefaultVPCEgressPostgres, error) {
	if err := i.Validate(); err != nil {
		return nil, err
	}
	if p == nil || provider == nil || freshness <= 0 || i.ClusterID != p.placement.ClusterID {
		return nil, biz.Fail(biz.InvalidArgument, "fixed default VPC, matching placement and provider are required")
	}
	return &DefaultVPCEgressPostgres{p, i, provider, freshness}, nil
}
func defaultEgressSnapshot(r sqlcgen.NetworkDefaultVpcEgress, operationState string) biz.DefaultVPCEgress {
	v := biz.DefaultVPCEgress{EgressMetadata: biz.EgressMetadata{ID: r.ResourceID, Name: r.Name, State: biz.ResourceState(r.State), Reason: biz.Reason(r.Reason), Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, ObservedAt: r.ObservedAt, LastOperationID: r.LastOperationID}, PoolID: r.PoolID, Phase: r.Phase, OperationState: operationState, Address: r.Address, EIPID: r.EipID,
		Identity:        biz.DefaultVPCIdentity{ClusterID: r.ClusterID, ClusterUID: r.ClusterUid, Namespace: r.DefaultVpcNamespace, Name: r.DefaultVpcName, UID: r.DefaultVpcUid},
		EIPProviderName: r.EipProviderName, EIPProviderUID: r.EipProviderUid, SnatProviderName: r.SnatProviderName, SnatProviderUID: r.SnatProviderUid, PendingAction: r.PendingAction, PendingUnknown: r.PendingUnknown, DesiredEnabled: true, TargetGeneration: r.TargetGeneration}
	if r.AppliedEnabled.Valid {
		enabled := r.AppliedEnabled.Bool
		v.AppliedEnabled = &enabled
	}
	return v
}
func defaultEgressFingerprint(i biz.DefaultVPCEgressIntent) string {
	return contentHash(struct{ Name, PoolID string }{i.Name, i.PoolID})
}
func (p *DefaultVPCEgressPostgres) replay(ctx context.Context, q *sqlcgen.Queries, i biz.DefaultVPCEgressIntent) (biz.DefaultVPCEgress, bool, error) {
	r, err := q.GetDefaultEgressReplay(ctx, sqlcgen.GetDefaultEgressReplayParams{ClusterID: p.identity.ClusterID, IdempotencyKey: i.IdempotencyKey})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.DefaultVPCEgress{}, false, nil
	}
	if err != nil {
		return biz.DefaultVPCEgress{}, false, databaseFailure(err)
	}
	if r.Fingerprint != defaultEgressFingerprint(i) {
		return biz.DefaultVPCEgress{}, true, biz.Fail(biz.IdempotencyConflict, "key already accepted another default VPC egress")
	}
	var v biz.DefaultVPCEgress
	if err = json.Unmarshal(r.Response, &v); err != nil {
		return v, true, databaseFailure(err)
	}
	if v.ID != r.ResourceID || v.LastOperationID != r.OperationID {
		return v, true, biz.Fail(biz.DependencyUnavailable, "invalid platform egress acceptance snapshot")
	}
	return v, true, nil
}
func (p *DefaultVPCEgressPostgres) CreateDefaultVPCEgress(ctx context.Context, i biz.DefaultVPCEgressIntent, a biz.Attribution) (biz.DefaultVPCEgress, error) {
	// Exact accepted keys replay before current external prerequisites are read.
	if v, found, err := p.replay(ctx, p.base.queries, i); found || err != nil {
		return v, err
	}
	proof, err := p.provider.ValidateDefaultVPCEgress(ctx, p.identity, i.PoolID)
	if err != nil {
		return biz.DefaultVPCEgress{}, switchAdmissionFailure(err)
	}
	tx, err := p.base.pool.Begin(ctx)
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.base.queries.WithTx(tx)
	if err = q.LockPlatformCluster(ctx, sqlcgen.LockPlatformClusterParams{ClusterID: p.identity.ClusterID}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	if err = q.LockDefaultEgressKey(ctx, sqlcgen.LockDefaultEgressKeyParams{ClusterID: p.identity.ClusterID, IdempotencyKey: i.IdempotencyKey}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	if v, found, err := p.replay(ctx, q, i); found || err != nil {
		return v, err
	}
	pool, err := q.LockPublicPool(ctx, sqlcgen.LockPublicPoolParams{ClusterID: p.identity.ClusterID, ResourceID: i.PoolID})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	resource, err := q.GetPlatform(ctx, sqlcgen.GetPlatformParams{ClusterID: p.identity.ClusterID, Kind: "public_pool", ResourceID: i.PoolID})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	if pool.Scope != "public" || pool.Retired || proof.Revision != pool.ConfigRevision || proof.ProviderUID == "" || proof.ProviderUID != resource.ProviderUid || proof.ObservedAt.IsZero() || proof.ObservedAt.After(now) || now.Sub(proof.ObservedAt) > p.freshness || len(proof.Hash) != 64 {
		return biz.DefaultVPCEgress{}, biz.Fail(biz.PublicEgressNotReady, "public pool identity, physical proof or configuration changed")
	}
	// Platform pre-acceptance does not open or require tenant allocation.
	if err = p.base.poolReady(ctx, q, pool, now, p.freshness, false); err != nil {
		return biz.DefaultVPCEgress{}, err
	}
	id, op := newEgressID("dve"), uuid.NewString()
	suffix := strings.TrimPrefix(id, "dve_")
	err = q.InsertDefaultEgress(ctx, sqlcgen.InsertDefaultEgressParams{ResourceID: id, EipID: newEgressID("eip"), ClusterID: p.identity.ClusterID, ClusterUid: p.identity.ClusterUID, DefaultVpcNamespace: p.identity.Namespace, DefaultVpcName: p.identity.Name, DefaultVpcUid: p.identity.UID, Name: i.Name, PoolID: i.PoolID, PoolRevision: pool.ConfigRevision, PoolProviderUid: resource.ProviderUid, AdmissionHash: proof.Hash, AdmittedAt: now, LastOperationID: op, EipProviderName: "ani-default-eip-" + suffix, EipBindingID: uuid.NewString(), SnatProviderName: "ani-default-snat-" + suffix, SnatBindingID: uuid.NewString()})
	if err != nil {
		return biz.DefaultVPCEgress{}, switchAdmissionFailure(err)
	}
	if err = q.InsertDefaultEgressOperation(ctx, sqlcgen.InsertDefaultEgressOperationParams{OperationID: op, ResourceID: id, Kind: "create_default_vpc_egress", CreatedAt: now}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	r, err := q.GetDefaultEgress(ctx, sqlcgen.GetDefaultEgressParams{ClusterID: p.identity.ClusterID, ResourceID: id})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	v := defaultEgressSnapshot(r.NetworkDefaultVpcEgress, r.OperationState)
	body, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	if err = q.InsertDefaultEgressReplay(ctx, sqlcgen.InsertDefaultEgressReplayParams{ClusterID: p.identity.ClusterID, IdempotencyKey: i.IdempotencyKey, Fingerprint: defaultEgressFingerprint(i), ResourceID: id, OperationID: op, Response: body, CreatedAt: now}); err != nil {
		return v, databaseFailure(err)
	}
	if err = q.InsertDefaultEgressHistory(ctx, sqlcgen.InsertDefaultEgressHistoryParams{HistoryID: uuid.NewString(), ResourceID: id, OperationID: op, Event: "create_accepted", ActorRef: a.Actor, CallerRef: a.DirectCaller, Detail: body}); err != nil {
		return v, databaseFailure(err)
	}
	return v, databaseFailure(tx.Commit(ctx))
}
func (p *DefaultVPCEgressPostgres) GetDefaultVPCEgress(ctx context.Context, id string) (biz.DefaultVPCEgress, error) {
	r, err := p.base.queries.GetDefaultEgress(ctx, sqlcgen.GetDefaultEgressParams{ClusterID: p.identity.ClusterID, ResourceID: id})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	return p.observedDefaultEgress(ctx, r.NetworkDefaultVpcEgress, r.OperationState)
}
func (p *DefaultVPCEgressPostgres) observedDefaultEgress(ctx context.Context, row sqlcgen.NetworkDefaultVpcEgress, state string) (biz.DefaultVPCEgress, error) {
	v := defaultEgressSnapshot(row, state)
	now, err := p.base.queries.DatabaseTime(ctx)
	if err != nil {
		return v, databaseFailure(err)
	}
	v.ObservationStale = v.ObservedAt == nil || v.ObservedAt.After(now) || now.Sub(*v.ObservedAt) > p.freshness
	if v.State == biz.Available && v.ObservationStale {
		v.State, v.Reason = biz.Degraded, biz.ProviderNotReady
	}
	return v, nil
}
func (p *DefaultVPCEgressPostgres) GetCurrentDefaultVPCEgress(ctx context.Context) (biz.DefaultVPCEgress, error) {
	r, err := p.base.queries.GetCurrentDefaultEgress(ctx, sqlcgen.GetCurrentDefaultEgressParams{
		ClusterID: p.identity.ClusterID, DefaultVpcNamespace: p.identity.Namespace, DefaultVpcName: p.identity.Name,
	})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	return p.observedDefaultEgress(ctx, r.NetworkDefaultVpcEgress, r.OperationState)
}
func (p *DefaultVPCEgressPostgres) DeleteDefaultVPCEgress(ctx context.Context, id string, a biz.Attribution) (biz.DefaultVPCEgress, error) {
	tx, err := p.base.pool.Begin(ctx)
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.base.queries.WithTx(tx)
	if err = q.LockPlatformCluster(ctx, sqlcgen.LockPlatformClusterParams{ClusterID: p.identity.ClusterID}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	r, err := q.LockDefaultEgress(ctx, sqlcgen.LockDefaultEgressParams{ClusterID: p.identity.ClusterID, ResourceID: id})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	row := r.NetworkDefaultVpcEgress
	if row.State == "deleted" || row.State == "deleting" {
		return defaultEgressSnapshot(row, r.OperationState), nil
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	op := uuid.NewString()
	if err = q.SupersedeDefaultEgressOperation(ctx, sqlcgen.SupersedeDefaultEgressOperationParams{ResourceID: id, OperationID: row.LastOperationID}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	if err = q.InsertDefaultEgressOperation(ctx, sqlcgen.InsertDefaultEgressOperationParams{OperationID: op, ResourceID: id, Kind: "delete_default_vpc_egress", CreatedAt: now}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	changed, err := q.AdmitDefaultEgressDelete(ctx, sqlcgen.AdmitDefaultEgressDeleteParams{ClusterID: p.identity.ClusterID, ResourceID: id, Version: row.Version, OperationID: op})
	if err = affected(changed, err); err != nil {
		return biz.DefaultVPCEgress{}, err
	}
	if err = q.InsertDefaultEgressHistory(ctx, sqlcgen.InsertDefaultEgressHistoryParams{HistoryID: uuid.NewString(), ResourceID: id, OperationID: op, Event: "delete_accepted", ActorRef: a.Actor, CallerRef: a.DirectCaller, Detail: []byte(`{}`)}); err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	v, err := q.GetDefaultEgress(ctx, sqlcgen.GetDefaultEgressParams{ClusterID: p.identity.ClusterID, ResourceID: id})
	if err != nil {
		return biz.DefaultVPCEgress{}, databaseFailure(err)
	}
	return defaultEgressSnapshot(v.NetworkDefaultVpcEgress, v.OperationState), databaseFailure(tx.Commit(ctx))
}

func (p *DefaultVPCEgressPostgres) ClaimDefaultVPCEgress(ctx context.Context, owner string, duration time.Duration) (biz.DefaultEgressWork, bool, error) {
	tx, err := p.base.pool.Begin(ctx)
	if err != nil {
		return biz.DefaultEgressWork{}, false, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.base.queries.WithTx(tx)
	r, err := q.DueDefaultEgress(ctx, sqlcgen.DueDefaultEgressParams{ClusterID: p.identity.ClusterID})
	if errors.Is(err, pgx.ErrNoRows) {
		return biz.DefaultEgressWork{}, false, nil
	}
	if err != nil {
		return biz.DefaultEgressWork{}, false, databaseFailure(err)
	}
	epoch, err := q.ClaimDefaultEgressLease(ctx, sqlcgen.ClaimDefaultEgressLeaseParams{Owner: owner, LeaseSeconds: int64(duration / time.Second), ClusterID: p.identity.ClusterID, ResourceID: r.NetworkDefaultVpcEgress.ResourceID})
	if err != nil {
		return biz.DefaultEgressWork{}, false, databaseFailure(err)
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.DefaultEgressWork{}, false, databaseFailure(err)
	}
	x := biz.DefaultEgressWork{Egress: defaultEgressSnapshot(r.NetworkDefaultVpcEgress, r.OperationState), Owner: owner, Epoch: epoch, PoolRevision: r.NetworkDefaultVpcEgress.PoolRevision, PoolProviderUID: r.NetworkDefaultVpcEgress.PoolProviderUid, EIPBindingID: r.NetworkDefaultVpcEgress.EipBindingID, SnatBindingID: r.NetworkDefaultVpcEgress.SnatBindingID, Now: now}
	return x, true, databaseFailure(tx.Commit(ctx))
}
func (p *DefaultVPCEgressPostgres) AssertDefaultVPCEgressLease(ctx context.Context, x biz.DefaultEgressWork) error {
	ok, err := p.base.queries.AssertDefaultEgressLease(ctx, sqlcgen.AssertDefaultEgressLeaseParams{ClusterID: p.identity.ClusterID, ResourceID: x.Egress.ID, Version: x.Egress.Version, OperationID: x.Egress.LastOperationID, Owner: x.Owner, Epoch: x.Epoch})
	if err != nil {
		return databaseFailure(err)
	}
	if !ok {
		return biz.ErrLeaseLost
	}
	return nil
}
func (p *DefaultVPCEgressPostgres) BeginDefaultVPCEgressMutation(ctx context.Context, x biz.DefaultEgressWork, action string) error {
	n, err := p.base.queries.BeginDefaultEgressMutation(ctx, sqlcgen.BeginDefaultEgressMutationParams{Action: action, ClusterID: p.identity.ClusterID, ResourceID: x.Egress.ID, Version: x.Egress.Version, OperationID: x.Egress.LastOperationID, Owner: x.Owner, Epoch: x.Epoch})
	if err != nil {
		return databaseFailure(err)
	}
	if n != 1 {
		return biz.ErrLeaseLost
	}
	return nil
}
func (p *DefaultVPCEgressPostgres) FinishDefaultVPCEgress(ctx context.Context, x biz.DefaultEgressWork, v biz.DefaultEgressProgress) error {
	o := v.Observation
	if v.State == biz.Available && (!v.Observed || !o.Ready || o.EIPUID == "" || o.SnatUID == "" || o.Address == "" || o.TargetGeneration < 1 || o.AppliedEnabled == nil || !*o.AppliedEnabled) {
		return biz.Fail(biz.DependencyUnavailable, "default egress availability needs complete provider binding proof")
	}
	if v.State == biz.Deleted && (!v.Observed || o.EIPExists || o.SnatExists || !o.Unbound || !v.ClearPending || v.PendingUnknown) {
		return biz.Fail(biz.DependencyUnavailable, "default egress deletion needs final disappearance and unbinding proof")
	}
	tx, err := p.base.pool.Begin(ctx)
	if err != nil {
		return databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.base.queries.WithTx(tx)
	var observedAt *time.Time
	if v.Observed {
		observedAt = &o.ObservedAt
	}
	var applied pgtype.Bool
	if o.AppliedEnabled != nil {
		applied = pgtype.Bool{Bool: *o.AppliedEnabled, Valid: true}
	}
	n, err := q.FinishDefaultEgress(ctx, sqlcgen.FinishDefaultEgressParams{State: string(v.State), Phase: v.Phase, Reason: string(v.Reason), EipUid: o.EIPUID, SnatUid: o.SnatUID, Address: o.Address, AppliedEnabled: applied, TargetGeneration: o.TargetGeneration, Observed: v.Observed, ObservedAt: observedAt, ClearPending: v.ClearPending, PendingUnknown: v.PendingUnknown, DelayMs: v.Delay.Milliseconds(), ClusterID: p.identity.ClusterID, ResourceID: x.Egress.ID, Version: x.Egress.Version, OperationID: x.Egress.LastOperationID, Owner: x.Owner, Epoch: x.Epoch})
	if err != nil {
		return databaseFailure(err)
	}
	if n != 1 {
		return biz.ErrLeaseLost
	}
	if v.OperationState != "" {
		if err = q.FinishDefaultEgressOperation(ctx, sqlcgen.FinishDefaultEgressOperationParams{State: string(v.OperationState), Reason: string(v.Reason), ResourceID: x.Egress.ID, OperationID: x.Egress.LastOperationID}); err != nil {
			return databaseFailure(err)
		}
	}
	body, _ := json.Marshal(struct {
		State  biz.ResourceState
		Phase  string
		Reason biz.Reason
		Proof  string
	}{v.State, v.Phase, v.Reason, o.Hash})
	if err = q.InsertDefaultEgressHistory(ctx, sqlcgen.InsertDefaultEgressHistoryParams{HistoryID: uuid.NewString(), ResourceID: x.Egress.ID, OperationID: x.Egress.LastOperationID, Event: "worker_progress", Detail: body}); err != nil {
		return databaseFailure(err)
	}
	return databaseFailure(tx.Commit(ctx))
}
