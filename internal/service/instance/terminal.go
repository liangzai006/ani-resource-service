package instance

import (
	"context"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/terminal/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type TerminalValidation struct {
	pb.UnimplementedInstanceTerminalServiceServer
	repo     biz.Repository
	provider biz.Provider
}

func NewTerminalValidation(r biz.Repository, p biz.Provider) *TerminalValidation {
	return &TerminalValidation{repo: r, provider: p}
}
func (s *TerminalValidation) ValidateTerminalTarget(ctx context.Context, t *pb.TerminalTarget) (*pb.TerminalValidation, error) {
	if t == nil || !biz.UUID(t.TenantId) || !biz.UUID(t.InstanceId) || t.Version < 1 {
		return nil, status.Error(codes.InvalidArgument, "INVALID_TERMINAL_TARGET")
	}
	i, e := s.repo.Get(ctx, t.TenantId, t.InstanceId)
	if e != nil {
		return nil, rpcError(e)
	}
	valid := func(i biz.Instance) bool {
		return i.Desired == biz.Running && i.RuntimeDesired == biz.Running && i.ActiveOperation == nil && i.RecoveryFence == nil && i.Observed == biz.Running && i.Version == t.Version && i.Pending == nil && i.Spec.ClusterID == t.ClusterId && i.Spec.Namespace == t.Namespace && i.Spec.NotebookName == t.NotebookName && i.NotebookUID == t.NotebookUid && i.PodName == t.PodName && i.PodUID == t.PodUid && t.ContainerName == biz.Container && i.Spec.Shell == t.Shell && t.PodUid != "" && t.NotebookUid != ""
	}
	if !valid(i) {
		return nil, status.Error(codes.FailedPrecondition, "INSTANCE_TERMINAL_TARGET_STALE")
	}
	obs, e := s.provider.Observe(ctx, i)
	if e != nil {
		return nil, rpcError(e)
	}
	if !obs.NotebookExists || !obs.PodReady || obs.Stopped || obs.NotebookUID != t.NotebookUid || obs.PodUID != t.PodUid || obs.PodName != t.PodName || obs.IntentVersion != t.Version {
		return nil, status.Error(codes.FailedPrecondition, "INSTANCE_TERMINAL_TARGET_STALE")
	}
	// Re-read after external observation so a newly accepted Stop/Delete revokes
	// the pending connection instead of trusting the earlier runtime snapshot.
	i, e = s.repo.Get(ctx, t.TenantId, t.InstanceId)
	if e != nil {
		return nil, rpcError(e)
	}
	if !valid(i) {
		return nil, status.Error(codes.FailedPrecondition, "INSTANCE_TERMINAL_TARGET_STALE")
	}
	return &pb.TerminalValidation{Valid: true}, nil
}
