package data

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func platformEIP(row sqlcgen.NetworkEip, poolName, bindingID, bindingState, targetKind, targetID string) biz.PlatformEIP {
	v := biz.PlatformEIP{EIP: eipResource(row), PoolID: row.PoolID, PoolName: poolName, ClusterID: row.ClusterID}
	v.BindingID, v.BindingState = bindingID, bindingState
	v.BindingTarget = eipBindingTarget(targetKind, targetID, bindingState)
	return v
}
func platformSystemEIP(row sqlcgen.NetworkDefaultVpcEgress, poolName string) biz.PlatformEIP {
	return biz.PlatformEIP{EIP: biz.EIP{
		EgressMetadata: biz.EgressMetadata{ID: row.EipID, Name: row.Name, State: biz.ResourceState(row.State), Reason: biz.Reason(row.Reason), Version: row.Version, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ObservedAt: row.ObservedAt, LastOperationID: row.LastOperationID},
		Address:        row.Address, Scope: "public", ManagedBy: "system", BindingState: "bound",
		BindingTarget: &biz.EIPBindingTarget{Kind: "platform_default_egress", ID: row.ResourceID, State: row.State},
	}, PoolID: row.PoolID, PoolName: poolName, ClusterID: row.ClusterID}
}
func (p *Postgres) ListPlatformEIPs(ctx context.Context, f biz.PlatformEIPFilter) ([]biz.PlatformEIP, int64, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	total, err := q.CountPlatformEIPs(ctx, sqlcgen.CountPlatformEIPsParams{ClusterID: p.placement.ClusterID, TenantFilter: f.TenantID, NameFilter: f.Name, AddressFilter: f.Address, PoolFilter: f.PoolID, ScopeFilter: f.Scope, StateFilter: f.State})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	rows, err := q.ListPlatformEIPs(ctx, sqlcgen.ListPlatformEIPsParams{ClusterID: p.placement.ClusterID, TenantFilter: f.TenantID, NameFilter: f.Name, AddressFilter: f.Address, PoolFilter: f.PoolID, ScopeFilter: f.Scope, StateFilter: f.State, MaxResults: f.Limit, PageOffset: f.Offset})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.EipID)
	}
	tenantRows, err := q.ListPlatformTenantEIPsByIDs(ctx, sqlcgen.ListPlatformTenantEIPsByIDsParams{ClusterID: p.placement.ClusterID, EipIds: ids})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	systemRows, err := q.ListPlatformSystemEIPsByIDs(ctx, sqlcgen.ListPlatformSystemEIPsByIDsParams{ClusterID: p.placement.ClusterID, EipIds: ids})
	if err != nil {
		return nil, 0, databaseFailure(err)
	}
	byID := make(map[string]biz.PlatformEIP, len(rows))
	for _, r := range tenantRows {
		v := platformEIP(r.NetworkEip, r.PoolName, r.BindingID, r.BindingState, r.BindingTargetKind, r.BindingTargetID)
		byID[v.ID] = v
	}
	for _, r := range systemRows {
		v := platformSystemEIP(r.NetworkDefaultVpcEgress, r.PoolName)
		byID[v.ID] = v
	}
	values := make([]biz.PlatformEIP, 0, len(rows))
	for _, id := range ids {
		values = append(values, byID[id])
	}
	return values, total, nil
}
func (p *Postgres) GetPlatformEIP(ctx context.Context, id string) (biz.PlatformEIP, error) {
	r, err := p.queries.GetPlatformEIP(ctx, sqlcgen.GetPlatformEIPParams{ClusterID: p.placement.ClusterID, EipID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		rows, readErr := p.queries.ListPlatformSystemEIPsByIDs(ctx, sqlcgen.ListPlatformSystemEIPsByIDsParams{ClusterID: p.placement.ClusterID, EipIds: []string{id}})
		if readErr != nil {
			return biz.PlatformEIP{}, databaseFailure(readErr)
		}
		if len(rows) == 1 {
			return platformSystemEIP(rows[0].NetworkDefaultVpcEgress, rows[0].PoolName), nil
		}
	}
	if err != nil {
		return biz.PlatformEIP{}, databaseFailure(err)
	}
	return platformEIP(r.NetworkEip, r.PoolName, r.BindingID, r.BindingState, r.BindingTargetKind, r.BindingTargetID), nil
}
