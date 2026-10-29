package instance

import (
	"context"
	"errors"
	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"github.com/zhangzhe-ctrl/ani-resource-service/pkg/instanceconfig"
)

func (s *Service) LookupCommandReplay(ctx context.Context, r *pb.LookupCommandReplayRequest) (*pb.OperationResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	intent, err := pb.ParseCommandIntent(r.Action, r.InstanceId, r.CanonicalIntent)
	if err != nil || !biz.UUID(r.IdempotencyKey) {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.replay(ctx, biz.Command{TenantID: r.TenantId, ID: r.InstanceId, Action: intent.Action, Key: r.IdempotencyKey, ExpectedVersion: intent.ExpectedVersion, RequestHash: pb.CommandIntentDigest(r.CanonicalIntent), CanonicalIntent: r.CanonicalIntent})
}
func (s *Service) replay(ctx context.Context, c biz.Command) (*pb.OperationResponse, error) {
	repo, ok := s.repo.(biz.CommandReplayRepository)
	if !ok {
		return nil, rpcError(biz.ErrNetworkUnavailable)
	}
	a, err := repo.LookupCommandReplay(ctx, c)
	return ack(a), rpcError(err)
}
func (s *Service) lifecycleCommand(ctx context.Context, c biz.Command) (*pb.OperationResponse, error) {
	if err := biz.RequireTenant(ctx, c.TenantID); err != nil {
		return nil, rpcError(err)
	}
	if !biz.UUID(c.ID) || !biz.UUID(c.Key) || c.ExpectedVersion < 1 {
		return nil, rpcError(biz.ErrInvalid)
	}
	raw, err := pb.CanonicalCommandIntent(c.Action, c.ID, c.ExpectedVersion, func() string {
		if c.Mount != nil {
			return c.Mount.PVCName
		}
		return ""
	}(), c.MountPath, func() bool { return c.Mount != nil && c.Mount.ReadOnly }(), c.RestartIfRunning)
	if err != nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	c.CanonicalIntent = raw
	c.RequestHash = pb.CommandIntentDigest(raw)
	// Only an explicit NotFound permits a new intent. This happens before gate,
	// current version/state and any Kubernetes PVC resolution.
	repo, ok := s.repo.(biz.CommandReplayRepository)
	if !ok {
		return nil, rpcError(biz.ErrNetworkUnavailable)
	}
	a, err := repo.LookupCommandReplay(ctx, c)
	if err == nil {
		return ack(a), nil
	}
	if !errors.Is(err, biz.ErrNotFound) {
		return nil, rpcError(err)
	}
	if !s.config.NewIntentsEnabled || (c.Action == "restart" && !s.config.RestartEnabled) || ((c.Action == "attach-pvc" || c.Action == "detach-pvc") && !s.config.PVCMutationsEnabled) {
		return nil, rpcError(biz.ErrActionUnavailable)
	}
	if c.Action == "attach-pvc" || c.Action == "detach-pvc" {
		i, err := s.repo.Get(ctx, c.TenantID, c.ID)
		if err != nil {
			return nil, rpcError(err)
		}
		if i.Spec.ClusterID != s.config.ClusterID {
			return nil, rpcError(biz.ErrNetworkUnavailable)
		}
		if c.MountPath == i.Spec.WorkspaceMountPath && c.Action == "detach-pvc" {
			return nil, rpcError(biz.ErrWorkspaceDetach)
		}
		paths := []string{}
		for _, m := range i.CurrentMounts {
			if c.Action == "attach-pvc" || m.MountPath != c.MountPath {
				paths = append(paths, m.MountPath)
			}
		}
		if c.Action == "attach-pvc" {
			paths = append(paths, c.MountPath)
		}
		if err = instanceconfig.Validate(i.Spec.WorkspaceMountPath, paths, nil, nil, nil, nil); err != nil {
			return nil, rpcError(biz.ErrInvalid)
		}
		if c.Action == "attach-pvc" {
			if s.volumes == nil {
				return nil, rpcError(biz.ErrNetworkUnavailable)
			}
			mounts, err := s.volumes.ResolveVolumes(ctx, c.TenantID, i.Spec.ClusterID, i.Spec.Namespace, []biz.VolumeMount{*c.Mount})
			if err != nil {
				return nil, rpcError(err)
			}
			if len(mounts) != 1 {
				return nil, rpcError(biz.ErrNetworkUnavailable)
			}
			c.Mount = &mounts[0]
		}
	}
	c.OperationID = uuid.NewString()
	a, err = s.repo.AcceptCommand(ctx, c)
	return ack(a), rpcError(err)
}
func (s *Service) Restart(ctx context.Context, r *pb.ChangeInstanceRequest) (*pb.OperationResponse, error) {
	if r == nil || len(r.ProtoReflect().GetUnknown()) > 0 {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.lifecycleCommand(ctx, biz.Command{TenantID: r.TenantId, ID: r.InstanceId, Key: r.IdempotencyKey, ExpectedVersion: r.ExpectedVersion, Action: "restart"})
}
func (s *Service) AttachPVC(ctx context.Context, r *pb.AttachPVCRequest) (*pb.OperationResponse, error) {
	if r == nil || len(r.ProtoReflect().GetUnknown()) > 0 {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.lifecycleCommand(ctx, biz.Command{TenantID: r.TenantId, ID: r.InstanceId, Key: r.IdempotencyKey, ExpectedVersion: r.ExpectedVersion, Action: "attach-pvc", MountPath: r.MountPath, Mount: &biz.VolumeMount{PVCName: r.PvcName, MountPath: r.MountPath, ReadOnly: r.ReadOnly}, RestartIfRunning: r.RestartIfRunning})
}
func (s *Service) DetachPVC(ctx context.Context, r *pb.DetachPVCRequest) (*pb.OperationResponse, error) {
	if r == nil || len(r.ProtoReflect().GetUnknown()) > 0 {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.lifecycleCommand(ctx, biz.Command{TenantID: r.TenantId, ID: r.InstanceId, Key: r.IdempotencyKey, ExpectedVersion: r.ExpectedVersion, Action: "detach-pvc", MountPath: r.MountPath, RestartIfRunning: r.RestartIfRunning})
}
func wireMountTarget(target *biz.MountTarget) *pb.MountConfiguration {
	if target == nil {
		return nil
	}
	out := &pb.MountConfiguration{VolumeMounts: []*pb.VolumeMount{}}
	for _, m := range target.VolumeMounts {
		out.VolumeMounts = append(out.VolumeMounts, &pb.VolumeMount{PvcName: m.PVCName, MountPath: m.MountPath, ReadOnly: m.ReadOnly})
	}
	return out
}
func wireOperation(o biz.Operation) *pb.Operation {
	out := &pb.Operation{Id: o.ID, InstanceId: o.InstanceID, Action: o.Action, IntentVersion: o.Version, State: o.State, Reason: o.Reason}
	if e := o.Execution; e != nil {
		out.Phase = e.Phase
		out.TargetConfiguration = wireMountTarget(e.TargetConfiguration)
		out.FinalRunState = e.FinalRunState
		out.AppliedMountVersion = e.AppliedMountVersion
		out.CompletionScope = e.CompletionScope
		out.FailureReason = e.FailureReason
	}
	return out
}
func (s *Service) ListInstanceMountablePVCs(ctx context.Context, r *pb.ListInstanceMountablePVCsRequest) (*pb.ListMountablePVCsResponse, error) {
	if r == nil || len(r.ProtoReflect().GetUnknown()) > 0 || !biz.UUID(r.InstanceId) {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	i, err := s.repo.Get(ctx, r.TenantId, r.InstanceId)
	if err != nil {
		return nil, rpcError(err)
	}
	if i.Spec.ClusterID != s.config.ClusterID || s.volumes == nil {
		return nil, rpcError(biz.ErrNetworkUnavailable)
	}
	items, total, err := s.volumes.ListMountablePVCs(ctx, r.TenantId, i.Spec.ClusterID, i.Spec.Namespace, r.Page, r.PageSize, r.Name)
	if err != nil {
		return nil, rpcError(err)
	}
	guard, ok := s.repo.(biz.PVCGuardReader)
	if !ok {
		return nil, rpcError(biz.ErrNetworkUnavailable)
	}
	out := &pb.ListMountablePVCsResponse{Items: []*pb.MountablePVC{}, Total: total}
	for _, m := range items {
		deleting, err := guard.PVCDeleting(ctx, r.TenantId, i.Spec.ClusterID, i.Spec.Namespace, m.PVCName)
		if err != nil {
			return nil, rpcError(err)
		}
		if deleting {
			m.Selectable = false
			m.UnavailableReason = "PVC_DELETION_IN_PROGRESS"
		}
		out.Items = append(out.Items, &pb.MountablePVC{PvcName: m.PVCName, Phase: m.Phase, RequestedCapacity: m.RequestedCapacity, AccessModes: m.AccessModes, VolumeMode: m.VolumeMode, Selectable: m.Selectable, UnavailableReason: m.UnavailableReason})
	}
	return out, nil
}
