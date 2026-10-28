package service

import (
	"context"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

type PlatformEIPService struct {
	networkv1.UnimplementedPlatformEIPServiceServer
	egress *biz.Egress
}

func NewPlatformEIPService(e *biz.Egress) *PlatformEIPService { return &PlatformEIPService{egress: e} }
func wirePlatformEIP(v biz.PlatformEIP) *networkv1.PlatformEIP {
	return &networkv1.PlatformEIP{Eip: wireEIP(v.EIP), PoolId: v.PoolID, PoolName: v.PoolName, ClusterId: v.ClusterID}
}
func (s *PlatformEIPService) ListPlatformEIPs(ctx context.Context, r *networkv1.ListPlatformEIPsRequest) (*networkv1.ListPlatformEIPsResponse, error) {
	if r == nil {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "request is required"))
	}
	window, err := biz.ResolvePage(r.Page, r.PageSize)
	if err != nil {
		return nil, rpcError(err)
	}
	state, ok := statesFromWire[r.GetState()]
	if !ok {
		return nil, rpcError(biz.Fail(biz.InvalidArgument, "invalid EIP state"))
	}
	rows, total, err := s.egress.ListPlatformEIPs(ctx, biz.PlatformEIPFilter{PageWindow: window, TenantID: r.GetTenantId(), Name: r.GetName(), Address: r.GetAddress(), PoolID: r.GetPoolId(), Scope: r.GetScope(), State: string(state)})
	if err != nil {
		return nil, rpcError(err)
	}
	out := &networkv1.ListPlatformEIPsResponse{Items: make([]*networkv1.PlatformEIP, 0, len(rows)), Total: total}
	for _, v := range rows {
		out.Items = append(out.Items, wirePlatformEIP(v))
	}
	return out, nil
}
func (s *PlatformEIPService) GetPlatformEIP(ctx context.Context, r *networkv1.GetPlatformEIPRequest) (*networkv1.GetPlatformEIPResponse, error) {
	v, err := s.egress.GetPlatformEIP(ctx, r.GetEipId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetPlatformEIPResponse{Resource: wirePlatformEIP(v)}, nil
}
func (s *PlatformEIPService) ReleasePlatformEIP(ctx context.Context, r *networkv1.ReleasePlatformEIPRequest) (*networkv1.ReleasePlatformEIPResponse, error) {
	v, err := s.egress.ReleasePlatformEIP(ctx, r.GetEipId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.ReleasePlatformEIPResponse{Resource: wirePlatformEIP(v)}, nil
}
