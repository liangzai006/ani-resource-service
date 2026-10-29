package instance

import (
	"context"
	"errors"
	"fmt"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"sync"

	command "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/command/v1"
	terminal "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/terminal/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Config struct {
	NamespacePrefix, ClusterID, ServiceAccount, StorageClass, WorkspaceSize, Shell string
	LogLimits                                                                      biz.LogLimits
	UID, GID                                                                       int64
	NetworkV2Writes                                                                bool
	NewIntentsEnabled, RestartEnabled, PVCMutationsEnabled                         bool
}
type Service struct {
	pb.UnimplementedInstanceServiceServer
	repo                     biz.Repository
	compute                  *compute.Resolver
	diagnostics              biz.Diagnostics
	logMu                    sync.Mutex
	logTotal                 int
	logTenants, logInstances map[string]int
	images                   biz.ImageResolver
	environment              biz.EnvironmentValidator
	network                  biz.NetworkPort
	volumes                  biz.VolumeResolver
	config                   Config
	vmScope                  func(context.Context, string, string) (string, string, bool, error)
}

func NewService(r biz.Repository, resolver *compute.Resolver, images biz.ImageResolver, environment biz.EnvironmentValidator, c Config) (*Service, error) {
	// An empty supply catalog is a valid closed experiment. Query and recovery
	// stay available; new creates still require a configured, frozen preset.
	if biz.ValidateLogLimits(c.LogLimits) != nil {
		return nil, biz.ErrInvalid
	}
	if r == nil || resolver == nil || images == nil || environment == nil || !biz.UUID(c.ClusterID) || c.ServiceAccount == "" || c.StorageClass == "" || c.WorkspaceSize == "" || c.Shell == "" || c.UID < 0 || c.GID < 0 {
		return nil, biz.ErrInvalid
	}
	diagnostics, _ := environment.(biz.Diagnostics)
	volumes, _ := environment.(biz.VolumeResolver)
	return &Service{repo: r, compute: resolver, images: images, environment: environment, volumes: volumes, diagnostics: diagnostics, config: c, logTenants: map[string]int{}, logInstances: map[string]int{}}, nil
}

// SetVMMetricScope wires the VM fallback for ResolveInstanceMetricScope. The
// resolver returns the persisted VM cluster/namespace and whether the VM exists
// in the caller tenant. Container instances always win; VM is only consulted on
// a container-instance NotFound.
func (s *Service) SetVMMetricScope(resolve func(context.Context, string, string) (string, string, bool, error)) {
	s.vmScope = resolve
}
func rpcError(e error) error {
	if e == nil {
		return nil
	}
	switch {
	case errors.Is(e, biz.ErrReservedConfig), errors.Is(e, pb.ErrReservedConfig):
		return status.Error(codes.InvalidArgument, "INSTANCE_RESERVED_CONFIG")
	case errors.Is(e, biz.ErrNetworkNotFound):
		return status.Error(codes.NotFound, "INSTANCE_NETWORK_NOT_FOUND")
	case errors.Is(e, biz.ErrNetworkConflict):
		return status.Error(codes.FailedPrecondition, "INSTANCE_NETWORK_CONFLICT")
	case errors.Is(e, biz.ErrNetworkUnavailable):
		return status.Error(codes.Unavailable, "INSTANCE_DEPENDENCY_UNAVAILABLE")
	case errors.Is(e, biz.ErrPVCNotFound):
		return status.Error(codes.NotFound, "INSTANCE_PVC_NOT_FOUND")
	case errors.Is(e, biz.ErrPVCReferenceChanged):
		return status.Error(codes.FailedPrecondition, "INSTANCE_PVC_REFERENCE_CHANGED")
	case errors.Is(e, biz.ErrPVCCatalogLimit):
		return status.Error(codes.ResourceExhausted, "INSTANCE_PVC_CATALOG_LIMIT")
	case errors.Is(e, pb.ErrInvalidScheduling):
		return status.Error(codes.InvalidArgument, "INVALID_SCHEDULING_OPTIONS")
	case errors.Is(e, pb.ErrSchedulingConflict), errors.Is(e, biz.ErrSchedulingConflict):
		return status.Error(codes.FailedPrecondition, "SCHEDULING_FIELD_CONFLICT")
	case errors.Is(e, compute.ErrPresetDisabled):
		return status.Error(codes.FailedPrecondition, "COMPUTE_PRESET_DISABLED")
	case errors.Is(e, biz.ErrOwnership):
		return status.Error(codes.FailedPrecondition, "INSTANCE_OBJECT_OWNERSHIP_CONFLICT")
	case errors.Is(e, biz.ErrDiagnosticTarget), errors.Is(e, biz.ErrPodChanged):
		return status.Error(codes.Unavailable, "INSTANCE_DIAGNOSTIC_TARGET_UNAVAILABLE")
	case errors.Is(e, biz.ErrLogLimit), errors.Is(e, biz.ErrLogSlow):
		return status.Error(codes.ResourceExhausted, e.Error())
	case errors.Is(e, context.Canceled), errors.Is(e, context.DeadlineExceeded):
		return status.FromContextError(e).Err()
	case errors.Is(e, biz.ErrDenied), errors.Is(e, compute.ErrDenied):
		return status.Error(codes.PermissionDenied, "INSTANCE_SCOPE_DENIED")
	case errors.Is(e, biz.ErrInvalid), errors.Is(e, compute.ErrInvalid), errors.Is(e, pb.ErrInvalidBusiness):
		return status.Error(codes.InvalidArgument, "INVALID_INSTANCE_REQUEST")
	case errors.Is(e, biz.ErrNotFound), errors.Is(e, compute.ErrNotFound):
		return status.Error(codes.NotFound, "INSTANCE_NOT_FOUND")
	case errors.Is(e, biz.ErrConflict):
		return status.Error(codes.AlreadyExists, "INSTANCE_IDEMPOTENCY_CONFLICT")
	case errors.Is(e, biz.ErrActionUnavailable):
		return status.Error(codes.Unavailable, e.Error())
	case errors.Is(e, biz.ErrOperationInProgress), errors.Is(e, biz.ErrState), errors.Is(e, biz.ErrRequiresStop), errors.Is(e, biz.ErrDeleting), errors.Is(e, biz.ErrMountPathInUse), errors.Is(e, biz.ErrWorkspaceDetach), errors.Is(e, biz.ErrPVCDeleting):
		return status.Error(codes.FailedPrecondition, e.Error())
	case errors.Is(e, biz.ErrMountNotFound):
		return status.Error(codes.NotFound, e.Error())
	case errors.Is(e, biz.ErrVersion):
		return status.Error(codes.FailedPrecondition, "INSTANCE_VERSION_CONFLICT")
	default:
		return status.Error(codes.Unavailable, "INSTANCE_DEPENDENCY_UNAVAILABLE")
	}
}
func (s *Service) ResolveCreateSpec(ctx context.Context, r *pb.ResolveCreateSpecRequest) (*pb.CreateSpec, error) {
	if r == nil || r.Data == nil || strings.TrimSpace(r.Data.Name) == "" || len([]rune(r.Data.Name)) > 80 || (r.Data.ImageScope != "tenant" && r.Data.ImageScope != "platform") || !biz.UUID(r.Data.PresetId) {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	if _, e := imagebiz.ParseImageID(r.Data.ImageId); e != nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	version := r.InstanceContractVersion
	if version != pb.ContractV2 {
		return nil, rpcError(biz.ErrInvalid)
	}
	if _, e := pb.CanonicalBusinessPayloadVersion(r.Data, version); e != nil {
		return nil, rpcError(e)
	}
	scheduling, e := pb.ResolveSchedulingOptions(r.Data.Scheduling, r.SchedulingDefaults)
	if e != nil {
		return nil, rpcError(e)
	}
	preset, e := s.compute.Resolve(ctx, r.TenantId, r.Data.PresetId, compute.ContainerInstance)
	if e != nil {
		return nil, rpcError(e)
	}
	ns, e := biz.ResolveNamespace(r.TenantId, r.Namespace, s.config.NamespacePrefix)
	if e != nil {
		return nil, rpcError(e)
	}
	if e = s.environment.ValidateEnvironment(ctx, r.TenantId, ns, s.config.ServiceAccount, s.config.StorageClass); e != nil {
		return nil, rpcError(e)
	}
	image, e := s.images.Resolve(ctx, r.TenantId, r.Data.ImageId, r.Data.ImageScope)
	if e != nil {
		return nil, rpcError(e)
	}
	c := s.config
	out := &pb.CreateSpec{Name: r.Data.Name, ImageId: r.Data.ImageId, ImageScope: r.Data.ImageScope, PresetId: r.Data.PresetId, InstanceKind: biz.KindContainer, ComputeType: preset.ComputeType, ImageReference: image.Reference, Cpu: fmt.Sprintf("%dm", preset.CPUMillis), Memory: fmt.Sprintf("%dMi", preset.MemoryMiB), ClusterId: c.ClusterID, Namespace: ns, ServiceAccount: c.ServiceAccount, StorageClass: c.StorageClass, WorkspaceSize: c.WorkspaceSize, Shell: c.Shell, Uid: c.UID, Gid: c.GID, Scheduling: scheduling, PresetName: preset.Name, PresetCode: preset.Code, GpuSelection: wireReference(preset.GPU)}
	out.InstanceContractVersion = version
	out.WorkspaceMountPath = "/workspace"
	if r.Data.WorkspaceMountPath != nil {
		out.WorkspaceMountPath = *r.Data.WorkspaceMountPath
	}
	out.Labels = r.Data.Labels
	out.Annotations = r.Data.Annotations
	out.Env = r.Data.Env
	if r.Data.Network != nil {
		if !s.config.NetworkV2Writes || s.network == nil {
			return nil, status.Error(codes.Unavailable, "INSTANCE_NETWORK_NOT_ENABLED")
		}
		selection := biz.NetworkSelection{VPCID: r.Data.Network.VpcId, SubnetID: r.Data.Network.SubnetId}
		if e := s.network.ValidateSelection(ctx, r.TenantId, c.ClusterID, ns, selection); e != nil {
			return nil, rpcError(e)
		}
		out.Network = &pb.NetworkSelection{VpcId: selection.VPCID, SubnetId: selection.SubnetID}
	}
	if len(r.Data.VolumeMounts) > 0 {
		if s.volumes == nil {
			return nil, status.Error(codes.Unavailable, "INSTANCE_DEPENDENCY_UNAVAILABLE")
		}
		mounts := make([]biz.VolumeMount, 0, len(r.Data.VolumeMounts))
		for _, m := range r.Data.VolumeMounts {
			mounts = append(mounts, biz.VolumeMount{PVCName: m.PvcName, MountPath: m.MountPath, ReadOnly: m.ReadOnly})
		}
		frozen, e := s.volumes.ResolveVolumes(ctx, r.TenantId, c.ClusterID, ns, mounts)
		if e != nil {
			return nil, rpcError(e)
		}
		for _, m := range frozen {
			out.VolumeMounts = append(out.VolumeMounts, &pb.FrozenVolumeMount{ClusterId: m.ClusterID, Namespace: m.Namespace, PvcName: m.PVCName, PvcUid: m.PVCUID, MountPath: m.MountPath, ReadOnly: m.ReadOnly, Ownership: m.Ownership})
		}
	}

	if e = pb.ValidateCreateSpec(out, false); e != nil {
		return nil, rpcError(e)
	}
	return out, nil
}
func ack(a biz.ACK) *pb.OperationResponse {
	return &pb.OperationResponse{Replayed: a.Replayed, DurableOwnerAck: &command.DurableOwnerAck{OperationId: a.OperationID, ResourceId: a.ResourceID, Accepted: a.Accepted}}
}
func (s *Service) AcceptCreate(ctx context.Context, r *pb.AcceptCreateRequest) (*pb.OperationResponse, error) {
	if r == nil || r.Context == nil || r.Spec == nil || len(r.ProtoReflect().GetUnknown()) != 0 {
		return nil, rpcError(biz.ErrInvalid)
	}
	c, f := r.Context, r.Spec
	if e := biz.RequireTenant(ctx, c.TenantId); e != nil {
		return nil, rpcError(e)
	}
	if e := pb.ValidateCreateSpec(f, true); e != nil {
		return nil, rpcError(e)
	}
	digest, e := pb.CommandDigest(c, f)
	if e != nil || digest != c.RequestDigest || f.ClusterId != s.config.ClusterID {
		return nil, rpcError(biz.ErrInvalid)
	}
	if !s.config.NewIntentsEnabled {
		original, err := s.repo.GetOperation(ctx, c.TenantId, c.OperationId)
		if err == nil {
			if original.Action != "create" || original.InstanceID != c.ResourceId || original.RequestHash != c.RequestDigest {
				return nil, rpcError(biz.ErrConflict)
			}
			a := original.ACK
			a.Replayed = true
			return ack(a), nil
		}
		if !errors.Is(err, biz.ErrNotFound) {
			return nil, rpcError(err)
		}
		return nil, rpcError(biz.ErrActionUnavailable)
	}
	image := strings.LastIndex(f.ImageReference, "@sha256:")
	if image < 1 {
		return nil, rpcError(biz.ErrInvalid)
	}
	spec := biz.Spec{InstanceKind: f.InstanceKind, ComputeType: f.ComputeType, Name: f.Name, ImageID: f.ImageId, ImageScope: f.ImageScope, ImageReference: f.ImageReference, ImageDigest: f.ImageReference[image+1:], PresetID: f.PresetId, PresetName: f.PresetName, PresetCode: f.PresetCode, Scheduling: domainScheduling(f.Scheduling), CPU: f.Cpu, Memory: f.Memory, UID: f.Uid, GID: f.Gid, Shell: f.Shell, ServiceAccount: f.ServiceAccount, StorageClass: f.StorageClass, WorkspaceSize: f.WorkspaceSize, ClusterID: f.ClusterId, Namespace: f.Namespace, NotebookName: "instance-" + c.ResourceId, PVCName: "workspace-" + c.ResourceId, ImagePullSecret: "ani-instance-image-pull"}
	spec.ContractVersion = f.InstanceContractVersion
	spec.WorkspaceMountPath = f.WorkspaceMountPath
	spec.Labels = f.Labels
	spec.Annotations = f.Annotations
	for _, m := range f.VolumeMounts {
		spec.VolumeMounts = append(spec.VolumeMounts, biz.VolumeMount{ClusterID: m.ClusterId, Namespace: m.Namespace, PVCName: m.PvcName, PVCUID: m.PvcUid, MountPath: m.MountPath, ReadOnly: m.ReadOnly, Ownership: m.Ownership})
	}
	for _, v := range f.Env {
		spec.Env = append(spec.Env, biz.EnvVar{Name: v.Name, Value: v.Value})
	}
	if f.Network != nil {
		spec.Network = &biz.NetworkSelection{VPCID: f.Network.VpcId, SubnetID: f.Network.SubnetId}
	}
	if g := f.GpuExecution; g != nil {
		spec.GPU = biz.GPU{Scheduler: g.SchedulerName, Queue: g.QueueName, RuntimeClass: g.RuntimeClassName, Digest: g.ExecutionDigest, NodeLabels: g.NodeSelector, Annotations: g.PodAnnotations, Limits: g.Resources}
		d := f.GpuDisplay
		spec.GPUSummary = &biz.GPUSummary{ProfileID: d.ProfileId, ProfileVersion: d.ProfileVersion, ProfileName: d.ProfileName, Mode: d.Mode.String(), ModelKey: d.ModelKey}
	}
	accepted, e := s.repo.AcceptCreate(ctx, biz.Create{TenantID: c.TenantId, ID: c.ResourceId, OperationID: c.OperationId, RequestHash: c.RequestDigest, BusinessDigest: digest, Spec: spec})
	return ack(accepted), rpcError(e)
}
func (s *Service) AcceptDelete(ctx context.Context, r *pb.AcceptDeleteRequest) (*pb.OperationResponse, error) {
	if r == nil || r.Context == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	c := r.Context
	if e := biz.RequireTenant(ctx, c.TenantId); e != nil {
		return nil, rpcError(e)
	}
	digest, e := pb.DeleteCommandDigest(r)
	if e != nil || digest != c.RequestDigest {
		return nil, rpcError(biz.ErrInvalid)
	}
	accepted, e := s.repo.AcceptDelete(ctx, biz.Delete{InstanceKind: r.InstanceKind, ComputeType: r.ComputeType, TenantID: c.TenantId, ID: c.ResourceId, OperationID: c.OperationId, CreateOperationID: r.CreateOperationId, RequestHash: c.RequestDigest, ExpectedVersion: r.ExpectedVersion})
	return ack(accepted), rpcError(e)
}
func (s *Service) change(ctx context.Context, r *pb.ChangeInstanceRequest, action string) (*pb.OperationResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	return s.lifecycleCommand(ctx, biz.Command{TenantID: r.TenantId, ID: r.InstanceId, Key: r.IdempotencyKey, ExpectedVersion: r.ExpectedVersion, Action: action})
}
func (s *Service) Start(ctx context.Context, r *pb.ChangeInstanceRequest) (*pb.OperationResponse, error) {
	return s.change(ctx, r, "start")
}
func (s *Service) Stop(ctx context.Context, r *pb.ChangeInstanceRequest) (*pb.OperationResponse, error) {
	return s.change(ctx, r, "stop")
}
func wire(i biz.Instance) *pb.Instance {
	out := &pb.Instance{InstanceKind: i.Spec.InstanceKind, ComputeType: i.Spec.ComputeType, Id: i.ID, Name: i.Spec.Name, Version: i.Version, DesiredState: i.Desired, ObservedState: i.Observed, Reason: i.Reason, ImageId: i.Spec.ImageID, ImageDigest: i.Spec.ImageDigest, PresetId: i.Spec.PresetID, CreateOperationId: i.CreateOperationID, LastOperationId: i.LastOperationID, ComputeCleaned: i.ComputeCleaned, WorkspaceCleaned: i.WorkspaceCleaned, QuotaReturned: i.QuotaReturned, Cpu: i.Spec.CPU, Memory: i.Spec.Memory, CreatedAt: timestamppb.New(i.CreatedAt), WorkspaceSize: i.Spec.WorkspaceSize, PresetName: i.Spec.PresetName, PresetCode: i.Spec.PresetCode, GpuSummary: wireSummary(i.Spec.GPUSummary, i.Spec.GPU), Scheduling: wireScheduling(i.Spec.Scheduling)}
	out.ExecutionRunState = i.RuntimeDesired
	out.AppliedMountVersion = i.AppliedMountVersion
	if i.ActiveOperation != nil && !biz.TerminalOperation(i.ActiveOperation.State) {
		out.ActiveOperationId = i.ActiveOperation.ID
	}
	out.NetworkState = "not_requested"
	out.NetworkCleaned = true
	out.VolumeReferencesReleased = i.VolumeReferencesReleased
	if i.Network != nil {
		out.NetworkState = i.Network.NetworkState
		out.NetworkReason = i.Network.Reason
		out.NetworkCleaned = i.Network.Cleaned
	}
	return out
}
func (s *Service) GetInstance(ctx context.Context, r *pb.GetInstanceRequest) (*pb.Instance, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	i, e := s.repo.Get(ctx, r.TenantId, r.InstanceId)
	return wire(i), rpcError(e)
}
func (s *Service) ListInstances(ctx context.Context, r *pb.ListInstancesRequest) (*pb.ListInstancesResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	items, total, e := s.repo.List(ctx, r.TenantId, biz.ListFilter{Page: r.Page, PageSize: r.PageSize, InstanceKind: r.InstanceKind, ComputeType: r.ComputeType, ObservedState: r.ObservedState, Name: r.Name})
	if e != nil {
		return nil, rpcError(e)
	}
	out := &pb.ListInstancesResponse{Items: []*pb.Instance{}, Total: total}
	for _, i := range items {
		out.Items = append(out.Items, wire(i))
	}
	return out, nil
}

func (s *Service) GetOperation(ctx context.Context, r *pb.GetOperationRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	o, e := s.repo.GetOperation(ctx, r.TenantId, r.OperationId)
	return wireOperation(o), rpcError(e)
}
func (s *Service) ResolveTerminalTarget(ctx context.Context, r *pb.ResolveTerminalTargetRequest) (*terminal.TerminalTarget, error) {
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
	if i.Desired != biz.Running || i.RuntimeDesired != biz.Running || i.ActiveOperation != nil || i.RecoveryFence != nil || i.Observed != biz.Running || i.Version != r.ExpectedVersion || i.NotebookUID == "" || i.PodUID == "" || i.Pending != nil {
		return nil, status.Error(codes.FailedPrecondition, "INSTANCE_TERMINAL_NOT_READY")
	}
	return &terminal.TerminalTarget{TenantId: i.TenantID, InstanceId: i.ID, Version: i.Version, ClusterId: i.Spec.ClusterID, Namespace: i.Spec.Namespace, NotebookName: i.Spec.NotebookName, NotebookUid: i.NotebookUID, PodName: i.PodName, PodUid: i.PodUID, ContainerName: biz.Container, Shell: i.Spec.Shell}, nil
}

func (s *Service) UseNetwork(network biz.NetworkPort) error {
	if network == nil {
		return biz.ErrInvalid
	}
	s.network = network
	return nil
}
