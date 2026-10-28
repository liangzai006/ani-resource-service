package service

import (
	"context"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type PlatformDefaultEgressService struct {
	networkv1.UnimplementedPlatformDefaultEgressServiceServer
	usecase *biz.DefaultVPCEgressUseCase
}

func NewPlatformDefaultEgressService(u *biz.DefaultVPCEgressUseCase) *PlatformDefaultEgressService {
	return &PlatformDefaultEgressService{usecase: u}
}
func wireDefaultVPCEgress(v biz.DefaultVPCEgress) *networkv1.DefaultVPCEgress {
	return &networkv1.DefaultVPCEgress{Id: v.ID, EipId: v.EIPID, Name: v.Name, PoolId: v.PoolID, State: string(v.State), Reason: string(v.Reason), Version: v.Version, LastOperationId: v.LastOperationID, OperationState: v.OperationState, Phase: v.Phase, PendingUnknown: v.PendingUnknown, PendingAction: v.PendingAction, Address: v.Address, EipProviderName: v.EIPProviderName, EipProviderUid: v.EIPProviderUID, SnatProviderName: v.SnatProviderName, SnatProviderUid: v.SnatProviderUID, DesiredEnabled: v.DesiredEnabled, AppliedEnabled: v.AppliedEnabled, ClusterId: v.Identity.ClusterID, ClusterUid: v.Identity.ClusterUID, DefaultVpcNamespace: v.Identity.Namespace, DefaultVpcName: v.Identity.Name, DefaultVpcUid: v.Identity.UID, CreatedAt: timestamppb.New(v.CreatedAt), UpdatedAt: timestamppb.New(v.UpdatedAt), ObservedAt: optionalTime(v.ObservedAt), TargetGeneration: v.TargetGeneration, ObservationStale: v.ObservationStale}
}
func (s *PlatformDefaultEgressService) CreateDefaultVPCEgress(ctx context.Context, r *networkv1.CreateDefaultVPCEgressRequest) (*networkv1.CreateDefaultVPCEgressResponse, error) {
	v, err := s.usecase.Create(ctx, biz.DefaultVPCEgressIntent{Name: r.GetName(), PoolID: r.GetPoolId(), IdempotencyKey: r.GetIdempotencyKey()})
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.CreateDefaultVPCEgressResponse{Egress: wireDefaultVPCEgress(v)}, nil
}
func (s *PlatformDefaultEgressService) GetDefaultVPCEgress(ctx context.Context, r *networkv1.GetDefaultVPCEgressRequest) (*networkv1.GetDefaultVPCEgressResponse, error) {
	v, err := s.usecase.Get(ctx, r.GetEgressId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetDefaultVPCEgressResponse{Egress: wireDefaultVPCEgress(v)}, nil
}
func (s *PlatformDefaultEgressService) DeleteDefaultVPCEgress(ctx context.Context, r *networkv1.DeleteDefaultVPCEgressRequest) (*networkv1.DeleteDefaultVPCEgressResponse, error) {
	v, err := s.usecase.Delete(ctx, r.GetEgressId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.DeleteDefaultVPCEgressResponse{Egress: wireDefaultVPCEgress(v)}, nil
}
func (s *PlatformDefaultEgressService) GetCurrentDefaultVPCEgress(ctx context.Context, _ *networkv1.GetCurrentDefaultVPCEgressRequest) (*networkv1.GetCurrentDefaultVPCEgressResponse, error) {
	v, err := s.usecase.GetCurrent(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	return &networkv1.GetCurrentDefaultVPCEgressResponse{Egress: wireDefaultVPCEgress(v)}, nil
}
