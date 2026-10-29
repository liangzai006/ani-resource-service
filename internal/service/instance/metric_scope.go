package instance

import (
	"context"
	"errors"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func metricScopeUnavailable() error {
	const reason = "INSTANCE_METRIC_SCOPE_UNAVAILABLE"
	detailed, err := status.New(codes.FailedPrecondition, reason).WithDetails(&errdetails.ErrorInfo{Domain: "instance.ani.io", Reason: reason})
	if err != nil {
		return status.Error(codes.Internal, "failed to encode instance metric scope error")
	}
	return detailed.Err()
}

// ResolveInstanceMetricScope reads the original persisted location only. A
// stopped or deleted runtime does not revoke access to retained metric history.
// A VM (KubeVirt) id is resolved through the wired VM reader after the container
// instance table misses; its metric scope uses the virt-launcher compute
// container while keeping the same HTTP contract.
func (s *Service) ResolveInstanceMetricScope(ctx context.Context, r *pb.GetInstanceRequest) (*pb.InstanceMetricScope, error) {
	if r == nil || !biz.UUID(r.TenantId) || !biz.UUID(r.InstanceId) {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	i, err := s.repo.Get(ctx, r.TenantId, r.InstanceId)
	if err != nil {
		if !errors.Is(err, biz.ErrNotFound) || s.vmScope == nil {
			return nil, rpcError(err)
		}
		cluster, namespace, found, vmErr := s.vmScope(ctx, r.TenantId, r.InstanceId)
		if vmErr != nil {
			return nil, rpcError(vmErr)
		}
		if !found {
			return nil, rpcError(err)
		}
		if _, namespaceErr := biz.ResolveNamespace(r.TenantId, namespace, ""); !biz.UUID(cluster) || namespace == "" || namespaceErr != nil {
			return nil, metricScopeUnavailable()
		}
		return &pb.InstanceMetricScope{TenantId: r.TenantId, InstanceId: r.InstanceId, ClusterId: cluster, Namespace: namespace, ContainerName: "compute"}, nil
	}
	if i.TenantID != r.TenantId || i.ID != r.InstanceId {
		return nil, rpcError(biz.ErrDenied)
	}
	// ResolveNamespace normally supplies a create-time default. An absent
	// persisted namespace must never take that fallback on historical reads.
	_, namespaceErr := biz.ResolveNamespace(i.TenantID, i.Spec.Namespace, "")
	if !biz.UUID(i.Spec.ClusterID) || i.Spec.Namespace == "" || namespaceErr != nil {
		return nil, metricScopeUnavailable()
	}
	return &pb.InstanceMetricScope{TenantId: i.TenantID, InstanceId: i.ID, ClusterId: i.Spec.ClusterID, Namespace: i.Spec.Namespace, ContainerName: biz.Container}, nil
}
