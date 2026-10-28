package biz

import (
	"context"
	"net/netip"
	"strings"
)

type PlatformEIP struct {
	EIP
	PoolID, PoolName, ClusterID string
}
type PlatformEIPFilter struct {
	PageWindow
	TenantID, Name, Address, PoolID, State, Scope string
}
type PlatformEIPRepository interface {
	ListPlatformEIPs(context.Context, PlatformEIPFilter) ([]PlatformEIP, int64, error)
	GetPlatformEIP(context.Context, string) (PlatformEIP, error)
}

func (e *Egress) platformEIPRepository(ctx context.Context) (PlatformEIPRepository, Attribution, error) {
	a, err := e.authorization.Platform(ctx)
	if err != nil {
		return nil, a, err
	}
	r, ok := e.repository.(PlatformEIPRepository)
	if !ok {
		return nil, a, Fail(DependencyUnavailable, "platform EIP inventory is unavailable")
	}
	return r, a, nil
}
func (e *Egress) ListPlatformEIPs(ctx context.Context, f PlatformEIPFilter) ([]PlatformEIP, int64, error) {
	r, _, err := e.platformEIPRepository(ctx)
	if err != nil {
		return nil, 0, err
	}
	if f.Limit < 1 || f.Limit > 100 || f.Offset < 0 {
		return nil, 0, Fail(InvalidArgument, "invalid page window")
	}
	if f.TenantID != "" {
		if f.TenantID, err = ParseTenant(f.TenantID); err != nil {
			return nil, 0, err
		}
	}
	f.Name = strings.TrimSpace(f.Name)
	if len(f.Name) > 128 {
		return nil, 0, Fail(InvalidArgument, "name filter is too long")
	}
	if f.Address != "" {
		a, parseErr := netip.ParseAddr(f.Address)
		if parseErr != nil || !a.Is4() || a.String() != f.Address {
			return nil, 0, Fail(InvalidArgument, "address must be canonical IPv4")
		}
	}
	if f.PoolID != "" && !validEgressID(f.PoolID, "pool") {
		return nil, 0, Fail(InvalidArgument, "invalid address pool ID")
	}
	if f.Scope != "" && f.Scope != "public" && f.Scope != "intranet" {
		return nil, 0, Fail(InvalidArgument, "invalid EIP scope")
	}
	if _, err = normalizeList("", f.State, 20); err != nil {
		return nil, 0, err
	}
	rows, total, err := r.ListPlatformEIPs(ctx, f)
	for i := range rows {
		rows[i].EIP = e.eip(rows[i].EIP)
	}
	return rows, total, err
}
func (e *Egress) GetPlatformEIP(ctx context.Context, id string) (PlatformEIP, error) {
	r, _, err := e.platformEIPRepository(ctx)
	if err != nil {
		return PlatformEIP{}, err
	}
	if !validEgressID(id, "eip") {
		return PlatformEIP{}, Fail(ResourceNotFound, "EIP not found")
	}
	v, err := r.GetPlatformEIP(ctx, id)
	v.EIP = e.eip(v.EIP)
	return v, err
}
func (e *Egress) ReleasePlatformEIP(ctx context.Context, id string) (PlatformEIP, error) {
	r, a, err := e.platformEIPRepository(ctx)
	if err != nil {
		return PlatformEIP{}, err
	}
	if !validEgressID(id, "eip") {
		return PlatformEIP{}, Fail(ResourceNotFound, "EIP not found")
	}
	v, err := r.GetPlatformEIP(ctx, id)
	if err != nil {
		return v, err
	}
	if v.Scope != "public" || v.ManagedBy != "tenant" {
		return v, Fail(PermissionDenied, "system-managed EIPs must be released by their owner lifecycle")
	}
	// Resolve the real owner from inventory, never from request-supplied tenant
	// identity. DeleteEIP locks that tenant's row and checks active claims again.
	v.EIP, err = e.repository.DeleteEIP(ctx, v.TenantID, id, a)
	v.EIP = e.eip(v.EIP)
	return v, err
}
