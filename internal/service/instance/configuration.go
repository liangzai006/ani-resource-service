package instance

import (
	"context"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) ListMountablePVCs(ctx context.Context, r *pb.ListMountablePVCsRequest) (*pb.ListMountablePVCsResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	if s.volumes == nil {
		return nil, status.Error(codes.Unavailable, "INSTANCE_DEPENDENCY_UNAVAILABLE")
	}
	ns, e := biz.ResolveNamespace(r.TenantId, "", s.config.NamespacePrefix)
	if e != nil {
		return nil, rpcError(e)
	}
	items, total, e := s.volumes.ListMountablePVCs(ctx, r.TenantId, s.config.ClusterID, ns, r.Page, r.PageSize, r.Name)
	if e != nil {
		return nil, rpcError(e)
	}
	out := &pb.ListMountablePVCsResponse{Items: []*pb.MountablePVC{}, Total: total}
	for _, v := range items {
		out.Items = append(out.Items, &pb.MountablePVC{PvcName: v.PVCName, Phase: v.Phase, RequestedCapacity: v.RequestedCapacity, AccessModes: v.AccessModes, VolumeMode: v.VolumeMode, Selectable: v.Selectable, UnavailableReason: v.UnavailableReason})
	}
	return out, nil
}
func (s *Service) GetInstanceConfiguration(ctx context.Context, r *pb.GetInstanceRequest) (*pb.InstanceConfiguration, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	i, e := s.repo.Get(ctx, r.TenantId, r.InstanceId)
	if e != nil {
		return nil, rpcError(e)
	}
	if e := biz.ValidateConfiguration(i.Spec); e != nil {
		return nil, rpcError(e)
	}
	out := &pb.InstanceConfiguration{InstanceId: i.ID, InstanceContractVersion: i.Spec.ContractVersion, WorkspaceMountPath: i.Spec.WorkspaceMountPath, Labels: i.Spec.Labels, Annotations: i.Spec.Annotations}
	out.Version = i.Version
	out.AppliedMountVersion = i.AppliedMountVersion
	if o := i.ActiveOperation; o != nil && o.Execution != nil && (o.Action == "attach-pvc" || o.Action == "detach-pvc") {
		out.PendingChange = &pb.PendingMountChange{OperationId: o.ID, IntentVersion: o.Version, Phase: o.Execution.Phase, TargetConfiguration: wireMountTarget(o.Execution.TargetConfiguration)}
	}
	for _, m := range i.CurrentMounts {
		out.VolumeMounts = append(out.VolumeMounts, &pb.VolumeMount{PvcName: m.PVCName, MountPath: m.MountPath, ReadOnly: m.ReadOnly})
	}
	for _, v := range i.Spec.Env {
		out.Env = append(out.Env, &pb.InstanceEnvVar{Name: v.Name, Value: v.Value})
	}
	if i.Spec.Network != nil {
		out.Network = &pb.NetworkSelection{VpcId: i.Spec.Network.VPCID, SubnetId: i.Spec.Network.SubnetID}
	}
	return out, nil
}
