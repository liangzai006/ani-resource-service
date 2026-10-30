// Package instance owns the lifecycle of fixed-specification container instances.
package instance

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrOperationInProgress = errors.New("INSTANCE_OPERATION_IN_PROGRESS")
	ErrState               = errors.New("INSTANCE_STATE_CONFLICT")
	ErrRequiresStop        = errors.New("INSTANCE_REQUIRES_STOP")
	ErrDeleting            = errors.New("INSTANCE_DELETING")
	ErrMountPathInUse      = errors.New("INSTANCE_MOUNT_PATH_IN_USE")
	ErrMountNotFound       = errors.New("INSTANCE_MOUNT_NOT_FOUND")
	ErrWorkspaceDetach     = errors.New("INSTANCE_WORKSPACE_DETACH_FORBIDDEN")
	ErrPVCDeleting         = errors.New("INSTANCE_PVC_DELETION_IN_PROGRESS")
	ErrActionUnavailable   = errors.New("INSTANCE_ACTION_UNAVAILABLE")
	ErrInvalid             = errors.New("INVALID_INSTANCE_REQUEST")
	ErrDenied              = errors.New("INSTANCE_SCOPE_DENIED")
	ErrNotFound            = errors.New("INSTANCE_NOT_FOUND")
	ErrConflict            = errors.New("INSTANCE_IDEMPOTENCY_CONFLICT")
	ErrVersion             = errors.New("INSTANCE_VERSION_CONFLICT")
	ErrLease               = errors.New("INSTANCE_LEASE_LOST")
	ErrUnknown             = errors.New("INSTANCE_EXTERNAL_RESULT_UNKNOWN")
	ErrOwnership           = errors.New("INSTANCE_OBJECT_OWNERSHIP_CONFLICT")
	ErrDiagnosticTarget    = errors.New("INSTANCE_DIAGNOSTIC_TARGET_UNAVAILABLE")
	ErrPodChanged          = errors.New("INSTANCE_LOG_POD_CHANGED")
	ErrLogLimit            = errors.New("INSTANCE_LOG_CONNECTION_LIMIT")
	ErrLogSlow             = errors.New("INSTANCE_LOG_CLIENT_TOO_SLOW")
	ErrSchedulingConflict  = errors.New("SCHEDULING_FIELD_CONFLICT")
	ErrPVCNotFound         = errors.New("INSTANCE_PVC_NOT_FOUND")
	ErrPVCReferenceChanged = errors.New("INSTANCE_PVC_REFERENCE_CHANGED")
	ErrPVCCatalogLimit     = errors.New("INSTANCE_PVC_CATALOG_LIMIT")
	ErrReservedConfig      = errors.New("INSTANCE_RESERVED_CONFIG")
	ErrNetworkNotFound     = errors.New("INSTANCE_NETWORK_NOT_FOUND")
	ErrNetworkConflict     = errors.New("INSTANCE_NETWORK_CONFLICT")
	ErrNetworkUnavailable  = errors.New("INSTANCE_DEPENDENCY_UNAVAILABLE")
)

const Owner = "ani-resource-service"
const Container = "workspace"
const (
	KindContainer = "container"
	TypeCPU       = "cpu"
	TypeGPU       = "gpu"
	Running       = "running"
	Stopped       = "stopped"
	Deleted       = "deleted"
)

type Caller struct {
	TenantID, Actor string
	Platform        bool
}
type callerKey struct{}

func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}
func CallerFromContext(ctx context.Context) (Caller, error) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	if !ok || c.Platform || c.Actor == "" || !UUID(c.TenantID) {
		return Caller{}, ErrDenied
	}
	return c, nil
}
func RequireTenant(ctx context.Context, tenant string) error {
	c, ok := ctx.Value(callerKey{}).(Caller)
	if !ok || c.Platform || !UUID(tenant) || c.TenantID != tenant || c.Actor == "" {
		return ErrDenied
	}
	return nil
}
func UUID(v string) bool { u, e := uuid.Parse(v); return e == nil && u != uuid.Nil && u.String() == v }

var digest64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var dnsName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func ResolveNamespace(tenant, trusted, prefix string) (string, error) {
	if !UUID(tenant) {
		return "", ErrInvalid
	}
	ns := trusted
	if ns == "" {
		ns = prefix + tenant
	}
	if len(ns) > 63 || !dnsName.MatchString(ns) {
		return "", fmt.Errorf("%w: invalid namespace", ErrInvalid)
	}
	return ns, nil
}

// GPU is the sole frozen execution authority. Limits retain provider units,
// with memory-block metadata frozen in Annotations.
type GPU struct {
	Scheduler, Queue, RuntimeClass, Digest string
	NodeLabels, Annotations, Limits        map[string]string
}

func (g GPU) Empty() bool {
	return g.Scheduler == "" && g.Queue == "" && g.RuntimeClass == "" && g.Digest == "" && len(g.NodeLabels) == 0 && len(g.Annotations) == 0 && len(g.Limits) == 0
}

type VolumeMount struct {
	ReferenceGeneration                              int64 `json:"-"`
	ClusterID, Namespace, PVCName, PVCUID, MountPath string
	ReadOnly                                         bool
	Ownership                                        string
}
type EnvVar struct{ Name, Value string }
type NetworkSelection struct{ VPCID, SubnetID string }
type MountablePVC struct {
	PVCName, Phase, RequestedCapacity string
	AccessModes                       []string
	VolumeMode                        string
	Selectable                        bool
	UnavailableReason                 string
}
type VolumeResolver interface {
	ResolveVolumes(context.Context, string, string, string, []VolumeMount) ([]VolumeMount, error)
	ListMountablePVCs(context.Context, string, string, string, uint32, uint32, string) ([]MountablePVC, int64, error)
}
type Spec struct {
	ContractVersion                                        uint32
	WorkspaceMountPath                                     string
	VolumeMounts                                           []VolumeMount
	Network                                                *NetworkSelection
	Labels, Annotations                                    map[string]string
	Env                                                    []EnvVar
	InstanceKind                                           string `json:"instance_kind"`
	ComputeType                                            string `json:"compute_type"`
	Name, ImageID, ImageScope, ImageReference, ImageDigest string
	PresetID                                               string
	PresetName, PresetCode                                 string
	Scheduling                                             Scheduling
	GPUSummary                                             *GPUSummary
	CPU, Memory                                            string
	UID, GID                                               int64
	Shell, ServiceAccount, StorageClass, WorkspaceSize     string
	ImagePullSecret                                        string
	ClusterID, Namespace, NotebookName, PVCName            string
	GPU                                                    GPU
}

type Scheduling struct {
	PodPlacement      string
	PriorityClassName *string
}
type GPUSummary struct {
	ProfileID                   string
	ProfileVersion              uint64
	ProfileName, Mode, ModelKey string
}

type Create struct {
	TenantID, ID, OperationID, RequestHash, BusinessDigest string
	Spec                                                   Spec
}
type Delete struct {
	InstanceKind                                              string `json:"instance_kind"`
	ComputeType                                               string `json:"compute_type"`
	TenantID, ID, OperationID, CreateOperationID, RequestHash string
	ExpectedVersion                                           int64
}

type Command struct {
	CanonicalIntent                                     []byte
	Mount                                               *VolumeMount
	MountPath                                           string
	RestartIfRunning                                    bool
	TenantID, ID, OperationID, Key, RequestHash, Action string
	ExpectedVersion                                     int64
}
type ACK struct {
	Replayed                bool `json:"-"`
	OperationID, ResourceID string
	Accepted                bool
}
type Operation struct {
	Execution                                                         *Execution
	TenantID, ID, InstanceID, Action, RequestHash, Key, State, Reason string
	Version                                                           int64
	ACK                                                               ACK
	CreatedAt                                                         time.Time
}
type Instance struct {
	CurrentMounts                                                      []VolumeMount `json:"-"`
	MountsInitialized                                                  bool          `json:"-"`
	AppliedMountVersion                                                int64         `json:"-"`
	RuntimeDesired                                                     string        `json:"-"`
	RecoveryFence                                                      *Mutation     `json:"-"`
	ActiveOperation                                                    *Operation    `json:"-"`
	TrackedPods                                                        []PodRef
	Network                                                            *NetworkRuntime
	TenantID, ID, CreateOperationID, CreateRequestHash, BusinessDigest string
	Spec                                                               Spec
	Version                                                            int64
	Desired, Observed, Reason, LastOperationID                         string
	NotebookUID, NotebookRV, PVCUID                                    string
	StatefulSetUID                                                     string
	VolumeName, VolumeUID                                              string
	PodUID, PodName                                                    string
	ComputeCleaned, WorkspaceCleaned, QuotaReturned                    bool
	VolumeReferencesReleased                                           bool
	CleanupCompletedAt                                                 time.Time
	// The scope is durable before any external Notebook creation or mutation.
	ExternalComputeStarted bool
	GPUPods                []PodRef
	// Historical ACC evidence is retained for audit only. Owner cleanup no
	// longer requires an ACC observation or release digest.
	ComputeSealAt         time.Time
	PhysicalReleaseDigest string
	// Pending is written before sending. Absence of a target never clears an unknown CREATE.
	Pending              *Mutation
	CreatedAt, UpdatedAt time.Time
}
type Mutation struct {
	ID, Phase                                     string
	PhaseSequence                                 int64
	StoppedTarget                                 *bool
	TargetMountDigest, FencedMutationID           string
	ProviderMarkerExpected                        bool
	Kind, OperationID, TargetUID, ResourceVersion string
	Version                                       int64
}
type Work struct {
	Operation  *Operation
	Instance   Instance
	Owner      string
	Epoch      int64
	LeaseUntil time.Time
}
type Observation struct {
	Pods                                                    []PodRef
	SafeStopped, StatefulSetExists                          bool
	NotebookGeneration                                      int64
	StatefulSetGeneration, StatefulSetObservedGeneration    int64
	StatefulSetReplicas                                     int32
	MutationID                                              string
	NotebookExists, PVCExists, PodExists, PodReady, Stopped bool
	NotebookUID, NotebookRV, PVCUID, PodUID, PodName        string
	StatefulSetUID                                          string
	IntentVersion                                           int64
	VolumeExists                                            bool
	VolumeName, VolumeUID, WorkspaceReason                  string
	GPUPods                                                 []PodRef
}
type PodRef struct {
	ClusterID, Namespace, Name, UID, ContainerName string
}

// OwnerCleanupReady is the local release boundary. The repository persists
// this fact and the stable Gov notification in the same transaction.
func OwnerCleanupReady(i Instance) bool {
	return (i.Spec.ComputeType == TypeCPU || i.Spec.ComputeType == TypeGPU) &&
		i.Desired == Deleted && i.Pending == nil && i.RecoveryFence == nil && i.ComputeCleaned &&
		i.WorkspaceCleaned && i.VolumeReferencesReleased &&
		(i.Network == nil || i.Network.Cleaned)
}

func MergePodRefs(a, b []PodRef) []PodRef {
	out := append([]PodRef(nil), a...)
	for _, p := range b {
		found := false
		for _, old := range out {
			if old == p {
				found = true
				break
			}
		}
		if !found {
			out = append(out, p)
		}
	}
	return out
}

type MutationReceipt struct {
	Mutation Mutation
	Outcome  string
}
type Progress struct {
	Operation    *Operation
	FenceReceipt *MutationReceipt
	Instance     Instance
	Next         time.Duration
	Receipt      *MutationReceipt
	// CleanupComplete records this turn's successful owner cleanup decision.
	// Stored cleanup flags alone are insufficient after a failed observation.
	CleanupComplete bool
}
type Repository interface {
	AcceptCreate(context.Context, Create) (ACK, error)
	AcceptDelete(context.Context, Delete) (ACK, error)
	AcceptCommand(context.Context, Command) (ACK, error)
	Get(context.Context, string, string) (Instance, error)
	List(context.Context, string, ListFilter) ([]Instance, int64, error)
	GetOperation(context.Context, string, string) (Operation, error)
	Claim(context.Context, string, time.Duration) (*Work, error)
	BeginMutation(context.Context, Work, Mutation) error
	Finish(context.Context, Work, Progress) error
}
type Provider interface {
	Observe(context.Context, Instance) (Observation, error)
	EnsureWorkspace(context.Context, Instance) (string, error)
	CreateNotebook(context.Context, Instance) (string, error)
	SetStopped(context.Context, Instance, Observation, bool) error
	DeleteNotebook(context.Context, Instance, Observation) error
	DeleteWorkspace(context.Context, Instance, Observation) error
}
type QuotaReporter interface {
	Report(context.Context, Instance) error
}

type ResolvedImage struct{ Reference, Digest, RegistryAuthority string }
type ImageResolver interface {
	Resolve(context.Context, string, string, string) (ResolvedImage, error)
}

// PullSecretEscape is the instance path's handle on the unified Secret writer.
// Instance owns neither the naming rule nor the credential; it only freezes the
// derived name into its spec after Ensure succeeds.
type PullSecretEscape interface {
	EnsureInstancePullSecret(context.Context, string, string, string) (string, error)
}
type EnvironmentValidator interface {
	ValidateEnvironment(context.Context, string, string, string, string) error
}

func ValidateCreate(c Create) error {
	if !UUID(c.TenantID) || !UUID(c.ID) || !UUID(c.OperationID) || len(c.RequestHash) != 64 || len(c.BusinessDigest) != 64 {
		return ErrInvalid
	}
	s := c.Spec
	if e := ValidateConfiguration(s); e != nil {
		return e
	}
	if s.InstanceKind != KindContainer || ValidateScheduling(s.Scheduling) != nil {
		return ErrInvalid
	}
	if len(s.ImageDigest) != 71 || !strings.HasPrefix(s.ImageDigest, "sha256:") || !strings.HasSuffix(s.ImageReference, "@"+s.ImageDigest) {
		return ErrInvalid
	}
	if !UUID(s.ClusterID) || len(s.Namespace) > 63 || !dnsName.MatchString(s.Namespace) || s.NotebookName == "" || s.PVCName == "" || s.ImageReference == "" || s.ImageDigest == "" || s.CPU == "" || s.Memory == "" || s.UID < 0 || s.GID < 0 || s.ServiceAccount == "" || s.StorageClass == "" || s.WorkspaceSize == "" || s.Shell == "" {
		return ErrInvalid
	}
	if s.ComputeType == TypeCPU && (s.GPUSummary != nil) {
		return ErrInvalid
	}
	if err := validateResources(s.ComputeType, s.GPU); err != nil {
		return err
	}
	if s.ComputeType == TypeGPU && (s.GPU.Scheduler != "volcano" || s.GPU.Queue == "" || len(s.GPU.Limits) == 0 || len(s.GPU.NodeLabels) == 0) {
		return ErrInvalid
	}
	return nil
}

func ValidateDelete(d Delete) error {
	if !UUID(d.TenantID) || !UUID(d.ID) || !UUID(d.OperationID) || !UUID(d.CreateOperationID) || len(d.RequestHash) != 64 || d.ExpectedVersion < 0 {
		return ErrInvalid
	}
	if d.InstanceKind != KindContainer {
		return ErrInvalid
	}
	if d.ComputeType != TypeCPU && d.ComputeType != TypeGPU {
		return ErrInvalid
	}
	return nil
}

func validateResources(kind string, gpu GPU) error {
	switch kind {
	case TypeCPU:
		if !gpu.Empty() {
			return ErrInvalid
		}
	case TypeGPU:
		if len(gpu.Digest) != 64 || gpu.Scheduler != "volcano" || gpu.Queue == "" || gpu.RuntimeClass == "" || len(gpu.Limits) != 3 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type ListFilter struct {
	Page, PageSize                                 uint32
	InstanceKind, ComputeType, ObservedState, Name string
}

func PageWindow(page, size uint32) (uint32, uint32, int64, error) {
	if page == 0 {
		page = 1
	}
	if size == 0 {
		size = 20
	}
	if size > 100 {
		return 0, 0, 0, ErrInvalid
	}
	return page, size, int64(page-1) * int64(size), nil
}
func ValidateScheduling(s Scheduling) error {
	if s.PodPlacement != "NONE" && s.PodPlacement != "COLOCATE_PREFERRED" && s.PodPlacement != "SPREAD_PREFERRED" {
		return ErrInvalid
	}
	if s.PriorityClassName != nil && (*s.PriorityClassName == "" || strings.TrimSpace(*s.PriorityClassName) != *s.PriorityClassName) {
		return ErrInvalid
	}
	return nil
}
