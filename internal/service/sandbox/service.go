package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	computepb "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/api/resource"
)

type Config struct {
	ClusterID, Namespace, NamespacePrefix, RuntimeClass, StorageClass, WorkspaceSize string
	ImagePrefixes                                                                    []string
	ImageResolver                                                                    biz.ImageResolver
	StorageClassResolver                                                             biz.StorageClassResolver
	RuntimeClassResolver                                                             biz.RuntimeClassResolver
	NamespaceResolver                                                                biz.NamespaceResolver
}
type Service struct {
	pb.UnimplementedSandboxServiceServer
	repo    biz.Repository
	compute biz.SelectionResolver
	config  Config
}

func NewService(repo biz.Repository, resolver biz.SelectionResolver, c Config) (*Service, error) {
	q, qerr := resource.ParseQuantity(c.WorkspaceSize)
	if repo == nil || resolver == nil || c.NamespaceResolver == nil || !biz.UUID(c.ClusterID) || (c.NamespacePrefix == "" && c.Namespace == "") || (c.RuntimeClass == "" && c.RuntimeClassResolver == nil) || c.WorkspaceSize == "" || qerr != nil || q.Sign() <= 0 {
		return nil, biz.ErrInvalid
	}
	if c.NamespacePrefix != "" {
		// Validate the configured prefix independently of future requests.
		if _, err := biz.TenantNamespace("11111111-1111-4111-8111-111111111111", c.NamespacePrefix); err != nil {
			return nil, biz.ErrInvalid
		}
	}
	return &Service{repo: repo, compute: resolver, config: c}, nil
}

func (s *Service) namespace(tenant string) (string, error) {
	if s.config.NamespacePrefix != "" {
		return biz.TenantNamespace(tenant, s.config.NamespacePrefix)
	}
	// Compatibility for existing deployments until the composition root passes
	// the shared namespace prefix. Never derive this value from a request.
	return s.config.Namespace, nil
}

func validWorkspaceSize(size string) bool {
	q, err := resource.ParseQuantity(size)
	return size != "" && err == nil && q.Sign() > 0
}
func (s *Service) trustedImage(ref string) bool {
	for _, prefix := range s.config.ImagePrefixes {
		if prefix != "" && strings.HasPrefix(ref, prefix) {
			return true
		}
	}
	return false
}

func (s *Service) resolveImage(ctx context.Context, tenant, requested string) (string, error) {
	if requested == "" {
		return "", biz.ErrInvalid
	}
	if s.config.ImageResolver != nil {
		ref, err := s.config.ImageResolver.ResolveSandboxImage(ctx, tenant, requested)
		if err != nil {
			return "", err
		}
		if _, ok := biz.ImageDigest(ref); !ok || ref != strings.TrimSpace(ref) {
			return "", biz.ErrDependencyUnavailable
		}
		return ref, nil
	}
	// Compatibility with existing Sandbox deployments; the new composition
	// root supplies an authorized Image resolver instead of a prefix list.
	if len(s.config.ImagePrefixes) == 0 {
		return "", biz.ErrDependencyUnavailable
	}
	if _, ok := biz.ImageDigest(requested); !ok || !s.trustedImage(requested) {
		return "", biz.ErrInvalid
	}
	return requested, nil
}

func (s *Service) storageClass(ctx context.Context, tenant string) (string, error) {
	if s.config.StorageClassResolver != nil {
		class, err := s.config.StorageClassResolver.ResolveSandboxStorageClass(ctx, tenant)
		if err != nil {
			return "", err
		}
		if class == "" || class != strings.TrimSpace(class) {
			return "", biz.ErrDependencyUnavailable
		}
		return class, nil
	}
	if s.config.StorageClass == "" {
		return "", biz.ErrDependencyUnavailable
	}
	return s.config.StorageClass, nil
}

func (s *Service) runtimeClass(ctx context.Context) (string, error) {
	if s.config.RuntimeClassResolver == nil {
		return s.config.RuntimeClass, nil
	}
	class, err := s.config.RuntimeClassResolver.ResolveSandboxRuntimeClass(ctx)
	if err != nil {
		return "", err
	}
	if class == "" || class != strings.TrimSpace(class) {
		return "", biz.ErrDependencyUnavailable
	}
	return class, nil
}

func (s *Service) validateRuntimeClass(ctx context.Context, class string) error {
	if s.config.RuntimeClassResolver != nil {
		return s.config.RuntimeClassResolver.ValidateSandboxRuntimeClass(ctx, class)
	}
	if class != s.config.RuntimeClass {
		return biz.ErrInvalid
	}
	return nil
}
func rpcError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, biz.ErrDenied):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, biz.ErrNotFound), errors.Is(err, compute.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, biz.ErrConflict):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, biz.ErrVersion), errors.Is(err, compute.ErrPresetDisabled):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, biz.ErrUnsupported):
		return status.Error(codes.Unimplemented, err.Error())
	case errors.Is(err, compute.ErrDenied):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, biz.ErrOwnership):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, biz.ErrLease):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, biz.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, compute.ErrInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, computepb.ErrInvalidSelection):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	default:
		return status.Error(codes.Unavailable, "SANDBOX_DEPENDENCY_UNAVAILABLE")
	}
}
func selectionDigest(s compute.Selection) (string, error) {
	if s.ComputeType != "cpu" || s.GPU != nil {
		return "", biz.ErrInvalid
	}
	return computepb.SelectionDigest(&computepb.ComputeSelection{
		SchemaVersion: computepb.SelectionSchemaVersion,
		PresetId:      s.PresetID, Code: s.Code, Name: s.Name, ComputeType: s.ComputeType,
		CpuMillis: s.CPUMillis, MemoryMib: s.MemoryMiB,
	})
}
func frozenSpecDigest(s *pb.ResolveCreateSpecResponse) (string, error) {
	if s == nil || s.Workspace == nil {
		return "", biz.ErrInvalid
	}
	canonical := struct {
		Name, ImageReference, ImageDigest, PresetID, PresetCode, PresetName, ComputeType                                string
		CPUMillis, MemoryMiB                                                                                            int64
		SelectionDigest, ClusterID, Namespace, NamespaceUID, RuntimeClass, MountPath, Size, StorageClass, ReclaimPolicy string
		ContractVersion                                                                                                 uint32
	}{s.Name, s.ImageReference, s.ImageDigest, s.PresetId, s.PresetCode, s.PresetName, s.ComputeType, s.CpuMillis, s.MemoryMib, s.SelectionDigest, s.ClusterId, s.Namespace, s.NamespaceUid, s.RuntimeClass, s.Workspace.MountPath, s.Workspace.Size, s.Workspace.StorageClass, s.Workspace.ReclaimPolicy, s.ContractVersion}
	b, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	d := sha256.Sum256(append([]byte("sandbox-spec-v1\n"), b...))
	return hex.EncodeToString(d[:]), nil
}
func commandDigest(c *pb.CommandContext, instanceID string, s *pb.ResolveCreateSpecResponse) (string, error) {
	return pb.CommandDigest(c, instanceID, s, "")
}
func hasUnknown(m protoreflect.Message) bool {
	if !m.IsValid() || len(m.GetUnknown()) != 0 {
		return true
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.IsMap() {
			if fd.MapValue().Kind() == protoreflect.MessageKind {
				v.Map().Range(func(_ protoreflect.MapKey, nested protoreflect.Value) bool {
					found = hasUnknown(nested.Message())
					return !found
				})
			}
		} else if fd.IsList() {
			if fd.Kind() == protoreflect.MessageKind {
				list := v.List()
				for i := 0; i < list.Len() && !found; i++ {
					found = hasUnknown(list.Get(i).Message())
				}
			}
		} else if fd.Kind() == protoreflect.MessageKind {
			found = hasUnknown(v.Message())
		}
		return !found
	})
	return found
}
func (s *Service) ResolveCreateSpec(ctx context.Context, r *pb.ResolveCreateSpecRequest) (*pb.ResolveCreateSpecResponse, error) {
	if r == nil || hasUnknown(r.ProtoReflect()) || r.Data == nil || hasUnknown(r.Data.ProtoReflect()) || r.ContractVersion != 1 || r.InstanceId != "" || strings.TrimSpace(r.Data.Name) == "" || len([]rune(r.Data.Name)) > 80 || !biz.UUID(r.Data.PresetId) {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	namespace, err := s.namespace(r.TenantId)
	if err != nil {
		return nil, rpcError(err)
	}
	name := strings.TrimSpace(r.Data.Name)
	imageRef, err := s.resolveImage(ctx, r.TenantId, strings.TrimSpace(r.Data.ImageReference))
	if err != nil {
		return nil, rpcError(err)
	}
	digest, _ := biz.ImageDigest(imageRef)
	runtimeClass, err := s.runtimeClass(ctx)
	if err != nil {
		return nil, rpcError(err)
	}
	sel, err := s.compute.Resolve(ctx, r.TenantId, r.Data.PresetId, compute.Sandbox)
	if err != nil {
		return nil, rpcError(err)
	}
	workspace := r.Data.WorkspaceSize
	if workspace == "" {
		workspace = s.config.WorkspaceSize
	}
	if !validWorkspaceSize(workspace) || sel.PresetID != r.Data.PresetId || sel.ComputeType != "cpu" || sel.GPU != nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	selDigest, err := selectionDigest(sel)
	if err != nil {
		return nil, rpcError(err)
	}
	// Namespace initialization follows all checks that do not require it.
	namespaceUID, err := s.config.NamespaceResolver.EnsureSandboxNamespaceUID(ctx, r.TenantId)
	if err != nil {
		return nil, rpcError(err)
	}
	if namespaceUID == "" {
		return nil, rpcError(biz.ErrDependencyUnavailable)
	}
	storageClass, err := s.storageClass(ctx, r.TenantId)
	if err != nil {
		return nil, rpcError(err)
	}
	out := &pb.ResolveCreateSpecResponse{Name: name, ImageReference: imageRef, ImageDigest: digest, PresetId: sel.PresetID, PresetCode: sel.Code, PresetName: sel.Name, ComputeType: sel.ComputeType, CpuMillis: sel.CPUMillis, MemoryMib: sel.MemoryMiB, SelectionDigest: selDigest, ClusterId: s.config.ClusterID, Namespace: namespace, NamespaceUid: namespaceUID, RuntimeClass: runtimeClass, Workspace: &pb.WorkspaceSpec{MountPath: "/workspace", Size: workspace, StorageClass: storageClass, ReclaimPolicy: "Delete"}, ContractVersion: 1}
	out.SpecDigest, err = frozenSpecDigest(out)
	if err != nil {
		return nil, rpcError(err)
	}
	return out, nil
}
func specValue(v *pb.ResolveCreateSpecResponse) biz.CreateSpec {
	return biz.CreateSpec{Name: v.Name, ImageReference: v.ImageReference, ImageDigest: v.ImageDigest, PresetID: v.PresetId, PresetCode: v.PresetCode, PresetName: v.PresetName, ComputeType: v.ComputeType, CPUMillis: v.CpuMillis, MemoryMiB: v.MemoryMib, SelectionDigest: v.SelectionDigest, SpecDigest: v.SpecDigest, ClusterID: v.ClusterId, Namespace: v.Namespace, NamespaceUID: v.NamespaceUid, RuntimeClass: v.RuntimeClass, Workspace: biz.WorkspaceSpec{MountPath: v.Workspace.MountPath, Size: v.Workspace.Size, StorageClass: v.Workspace.StorageClass, ReclaimPolicy: v.Workspace.ReclaimPolicy}}
}
func frozenSnapshot(v biz.CreateSpec) biz.Snapshot {
	return biz.Snapshot{Name: v.Name, ImageReference: v.ImageReference, ImageDigest: v.ImageDigest, PresetID: v.PresetID, PresetCode: v.PresetCode, PresetName: v.PresetName, ComputeType: v.ComputeType, CPUMillis: v.CPUMillis, MemoryMiB: v.MemoryMiB, SelectionDigest: v.SelectionDigest, SpecDigest: v.SpecDigest, ClusterID: v.ClusterID, Namespace: v.Namespace, NamespaceUID: v.NamespaceUID, RuntimeClass: v.RuntimeClass, Workspace: v.Workspace}
}
func (s *Service) AcceptCreate(ctx context.Context, r *pb.AcceptCreateRequest) (*pb.AcceptCreateResponse, error) {
	if r == nil || hasUnknown(r.ProtoReflect()) || r.Context == nil || r.Spec == nil || !biz.UUID(r.Context.ResourceId) || !biz.UUID(r.Context.OperationId) || len([]rune(r.InstanceId)) > 128 || r.Spec.ContractVersion != 1 || r.Spec.Workspace == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.Context.TenantId); err != nil {
		return nil, rpcError(err)
	}
	if r.CreateIntentDigest != "" {
		decoded, err := hex.DecodeString(r.CreateIntentDigest)
		if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != r.CreateIntentDigest {
			return nil, rpcError(biz.ErrInvalid)
		}
	}
	requestDigest, digestErr := pb.CommandDigest(r.Context, r.InstanceId, r.Spec, r.CreateIntentDigest)
	selDigest, selErr := selectionDigest(compute.Selection{PresetID: r.Spec.PresetId, Code: r.Spec.PresetCode, Name: r.Spec.PresetName, ComputeType: r.Spec.ComputeType, CPUMillis: r.Spec.CpuMillis, MemoryMiB: r.Spec.MemoryMib})
	specDigest, specErr := frozenSpecDigest(r.Spec)
	if got, ok := biz.ImageDigest(r.Spec.ImageReference); !ok || got != r.Spec.ImageDigest || r.Spec.NamespaceUid == "" || strings.TrimSpace(r.Spec.ImageReference) != r.Spec.ImageReference || strings.TrimSpace(r.Spec.Name) == "" || len([]rune(r.Spec.Name)) > 80 || !biz.UUID(r.Spec.PresetId) || !biz.UUID(r.Spec.ClusterId) || r.Spec.CpuMillis <= 0 || r.Spec.MemoryMib <= 0 || digestErr != nil || requestDigest != r.Context.RequestDigest || selErr != nil || r.Spec.SelectionDigest != selDigest || specErr != nil || r.Spec.SpecDigest != specDigest {
		return nil, rpcError(biz.ErrInvalid)
	}
	create := biz.Create{TenantID: r.Context.TenantId, ID: r.Context.ResourceId, InstanceID: r.InstanceId, OperationID: r.Context.OperationId, RequestDigest: r.Context.RequestDigest, IntentDigest: r.CreateIntentDigest, Spec: specValue(r.Spec)}
	// Exact accepted commands replay against their original facts before any
	// validation of today's deployment policy or catalog configuration.
	existing, err := s.repo.Get(ctx, create.TenantID, create.ID)
	if err == nil {
		if existing.InstanceID != create.InstanceID || existing.CreateOperationID != create.OperationID || existing.CreateRequestDigest != create.RequestDigest || existing.CreateIntentDigest != create.IntentDigest || existing.Snapshot != frozenSnapshot(create.Spec) {
			return nil, rpcError(biz.ErrConflict)
		}
		return &pb.AcceptCreateResponse{Ack: ackProto(biz.ACK{OperationID: existing.CreateOperationID, ResourceID: existing.ID, Accepted: true, Replayed: true})}, nil
	}
	if !errors.Is(err, biz.ErrNotFound) {
		return nil, rpcError(err)
	}
	if r.InstanceId != "" {
		// The former parent field is accepted only to replay an already persisted
		// historical command above. It cannot create new linked resources.
		return nil, rpcError(biz.ErrInvalid)
	}
	namespaceUID, namespaceUIDErr := s.config.NamespaceResolver.ResolveSandboxNamespaceUID(ctx, r.Context.TenantId)
	if namespaceUIDErr != nil {
		return nil, rpcError(namespaceUIDErr)
	}
	if namespaceUID == "" || namespaceUID != r.Spec.NamespaceUid {
		return nil, rpcError(biz.ErrOwnership)
	}
	imageRef, imageErr := s.resolveImage(ctx, r.Context.TenantId, r.Spec.ImageReference)
	if imageErr != nil {
		return nil, rpcError(imageErr)
	}
	storageClass, storageErr := s.storageClass(ctx, r.Context.TenantId)
	if storageErr != nil {
		return nil, rpcError(storageErr)
	}
	if err := s.validateRuntimeClass(ctx, r.Spec.RuntimeClass); err != nil {
		return nil, rpcError(err)
	}
	namespace, namespaceErr := s.namespace(r.Context.TenantId)
	if namespaceErr != nil || imageRef != r.Spec.ImageReference || r.Spec.ClusterId != s.config.ClusterID || r.Spec.Namespace != namespace || r.Spec.Workspace.MountPath != "/workspace" || !validWorkspaceSize(r.Spec.Workspace.Size) || r.Spec.Workspace.StorageClass != storageClass || r.Spec.Workspace.ReclaimPolicy != "Delete" {
		return nil, rpcError(biz.ErrInvalid)
	}
	ack, err := s.repo.AcceptCreate(ctx, create)
	return &pb.AcceptCreateResponse{Ack: ackProto(ack)}, rpcError(err)
}
func (s *Service) GetSandbox(ctx context.Context, r *pb.GetSandboxRequest) (*pb.GetSandboxResponse, error) {
	if r == nil || hasUnknown(r.ProtoReflect()) || !biz.UUID(r.SandboxId) || r.InstanceId != "" {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	v, err := s.repo.Get(ctx, r.TenantId, r.SandboxId)
	return &pb.GetSandboxResponse{Sandbox: toProto(v)}, rpcError(err)
}
func (s *Service) ListSandboxes(ctx context.Context, r *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
	if r == nil || hasUnknown(r.ProtoReflect()) || r.InstanceId != "" {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	xs, total, err := s.repo.List(ctx, r.TenantId, biz.ListFilter{Page: r.Page, PageSize: r.PageSize})
	out := &pb.ListSandboxesResponse{Total: total}
	for _, x := range xs {
		out.Items = append(out.Items, toProto(x))
	}
	return out, rpcError(err)
}
func (s *Service) GetOperation(ctx context.Context, r *pb.GetOperationRequest) (*pb.GetOperationResponse, error) {
	if r == nil || hasUnknown(r.ProtoReflect()) || !biz.UUID(r.OperationId) || r.InstanceId != "" {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	op, err := s.repo.GetOperation(ctx, r.TenantId, r.OperationId)
	return &pb.GetOperationResponse{Operation: operationProto(op)}, rpcError(err)
}
func (s *Service) Pause(ctx context.Context, r *pb.PauseRequest) (*pb.PauseResponse, error) {
	if r == nil || hasUnknown(r.ProtoReflect()) {
		return nil, rpcError(biz.ErrInvalid)
	}
	a, err := s.change(ctx, r.GetTenantId(), r.GetSandboxId(), r.GetInstanceId(), r.GetIdempotencyKey(), r.GetExpectedVersion(), "pause")
	return &pb.PauseResponse{Ack: ackProto(a)}, rpcError(err)
}
func (s *Service) Resume(ctx context.Context, r *pb.ResumeRequest) (*pb.ResumeResponse, error) {
	if r == nil || hasUnknown(r.ProtoReflect()) {
		return nil, rpcError(biz.ErrInvalid)
	}
	a, err := s.change(ctx, r.GetTenantId(), r.GetSandboxId(), r.GetInstanceId(), r.GetIdempotencyKey(), r.GetExpectedVersion(), "resume")
	return &pb.ResumeResponse{Ack: ackProto(a)}, rpcError(err)
}
func (s *Service) Delete(ctx context.Context, r *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	if r == nil || hasUnknown(r.ProtoReflect()) {
		return nil, rpcError(biz.ErrInvalid)
	}
	a, err := s.change(ctx, r.GetTenantId(), r.GetSandboxId(), r.GetInstanceId(), r.GetIdempotencyKey(), r.GetExpectedVersion(), "delete")
	return &pb.DeleteResponse{Ack: ackProto(a)}, rpcError(err)
}
func (s *Service) change(ctx context.Context, tenant, id, instance, key string, version int64, action string) (biz.ACK, error) {
	if err := biz.RequireTenant(ctx, tenant); err != nil {
		return biz.ACK{}, err
	}
	if !biz.UUID(id) || instance != "" || strings.TrimSpace(key) == "" || len([]rune(key)) > 256 || version < 0 {
		return biz.ACK{}, biz.ErrInvalid
	}
	return s.repo.Change(ctx, biz.Command{TenantID: tenant, ID: id, InstanceID: "", OperationID: uuid.NewString(), Action: action, IdempotencyKey: key, RequestDigest: biz.ActionDigest("", version, action), ExpectedVersion: version})
}
func ackProto(a biz.ACK) *pb.OperationAck {
	return &pb.OperationAck{OperationId: a.OperationID, ResourceId: a.ResourceID, Accepted: a.Accepted, Replayed: a.Replayed}
}
func toProto(x biz.Sandbox) *pb.Sandbox {
	return &pb.Sandbox{Id: x.ID, TenantId: x.TenantID, InstanceId: x.InstanceID, CreateIntentDigest: x.CreateIntentDigest, Version: x.Version, DesiredState: x.Desired, ObservedState: x.Observed, Reason: x.Reason, CreateOperationId: x.CreateOperationID, LastOperationId: x.LastOperationID, Snapshot: &pb.SandboxSnapshot{Name: x.Snapshot.Name, ImageReference: x.Snapshot.ImageReference, PresetId: x.Snapshot.PresetID, PresetCode: x.Snapshot.PresetCode, PresetName: x.Snapshot.PresetName, ImageDigest: x.Snapshot.ImageDigest, CpuMillis: x.Snapshot.CPUMillis, MemoryMib: x.Snapshot.MemoryMiB, ComputeType: x.Snapshot.ComputeType, ClusterId: x.Snapshot.ClusterID, Namespace: x.Snapshot.Namespace, NamespaceUid: x.Snapshot.NamespaceUID, RuntimeClass: x.Snapshot.RuntimeClass, SelectionDigest: x.Snapshot.SelectionDigest, SpecDigest: x.Snapshot.SpecDigest, Workspace: &pb.WorkspaceSpec{MountPath: x.Snapshot.Workspace.MountPath, Size: x.Snapshot.Workspace.Size, StorageClass: x.Snapshot.Workspace.StorageClass, ReclaimPolicy: x.Snapshot.Workspace.ReclaimPolicy}}, ProviderRefs: &pb.ProviderRefs{PvName: x.ProviderRefs.PVName, PvUid: x.ProviderRefs.PVUID, PvcName: x.ProviderRefs.PVCName, PvcUid: x.ProviderRefs.PVCUID, PodName: x.ProviderRefs.PodName, PodUid: x.ProviderRefs.PodUID, ResourceVersion: x.ProviderRefs.ResourceVersion, CleanupReceipt: x.ProviderRefs.CleanupReceipt, PvcResourceVersion: x.ProviderRefs.PVCResourceVersion, PodResourceVersion: x.ProviderRefs.PodResourceVersion}, ObservedAt: timestamppb.New(x.ObservedAt)}
}
func operationProto(x biz.Operation) *pb.Operation {
	return &pb.Operation{Id: x.ID, SandboxId: x.SandboxID, InstanceId: x.InstanceID, Action: x.Action, IntentVersion: x.IntentVersion, State: x.State, Reason: x.Reason, CreatedAt: timestamppb.New(x.CreatedAt)}
}
