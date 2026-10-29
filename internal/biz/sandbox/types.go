package sandbox

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
)

var (
	ErrInvalid               = errors.New("INVALID_SANDBOX_REQUEST")
	ErrDenied                = errors.New("SANDBOX_SCOPE_DENIED")
	ErrNotFound              = errors.New("SANDBOX_NOT_FOUND")
	ErrConflict              = errors.New("SANDBOX_IDEMPOTENCY_CONFLICT")
	ErrVersion               = errors.New("SANDBOX_VERSION_CONFLICT")
	ErrUnsupported           = errors.New("SANDBOX_CAPABILITY_UNSUPPORTED")
	ErrOwnership             = errors.New("SANDBOX_PROVIDER_OWNERSHIP_CONFLICT")
	ErrLease                 = errors.New("SANDBOX_WORK_LEASE_LOST")
	ErrClusterMismatch       = errors.New("SANDBOX_PROVIDER_CLUSTER_MISMATCH")
	ErrDependencyUnavailable = errors.New("SANDBOX_DEPENDENCY_UNAVAILABLE")
)

const (
	DesiredRunning       = "running"
	DesiredPaused        = "paused"
	DesiredDeleted       = "deleted"
	ObservedProvisioning = "provisioning"
	ObservedRunning      = "running"
	ObservedPaused       = "paused"
	ObservedDeleting     = "deleting"
	ObservedDeleted      = "deleted"
	ObservedUnknown      = "unknown"

	MutationCreateWorkspace = "create_workspace"
	MutationCreatePod       = "create_pod"
	MutationDeletePod       = "delete_pod"
	MutationDeleteWorkspace = "delete_workspace"
	MutationApplied         = "applied"
	MutationRejected        = "rejected"
	MutationNotSent         = "not_sent"
	MutationUnknown         = "unknown"
)

type Caller struct{ TenantID, Actor string }
type callerKey struct{}

func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}
func RequireTenant(ctx context.Context, tenant string) error {
	c, ok := ctx.Value(callerKey{}).(Caller)
	if !ok || c.Actor == "" || !UUID(c.TenantID) || c.TenantID != tenant || !UUID(tenant) {
		return ErrDenied
	}
	return nil
}
func UUID(v string) bool {
	id, err := uuid.Parse(v)
	return err == nil && id != uuid.Nil && id.String() == v
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var namespaceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// TenantNamespace follows Resource's tenant namespace convention. The prefix
// comes from trusted platform configuration, never from a Sandbox request.
func TenantNamespace(tenant, prefix string) (string, error) {
	if !UUID(tenant) {
		return "", ErrInvalid
	}
	namespace := prefix + tenant
	if len(namespace) > 63 || !namespaceRE.MatchString(namespace) {
		return "", ErrInvalid
	}
	return namespace, nil
}

func ImageDigest(reference string) (string, bool) {
	i := strings.LastIndex(reference, "@")
	if i < 1 || !digestRE.MatchString(reference[i+1:]) {
		return "", false
	}
	return reference[i+1:], true
}

type WorkspaceSpec struct{ MountPath, Size, StorageClass, ReclaimPolicy string }

// ImageResolver authorizes a tenant's requested image and returns its immutable
// runtime reference. It also accepts a frozen reference for reauthorization.
type ImageResolver interface {
	ResolveSandboxImage(ctx context.Context, tenantID, requestedReference string) (string, error)
}

// StorageClassResolver selects the platform's currently permitted default
// workspace storage class for a tenant.
type StorageClassResolver interface {
	ResolveSandboxStorageClass(ctx context.Context, tenantID string) (string, error)
}

// RuntimeClassResolver selects an approved isolation runtime from the cluster.
type RuntimeClassResolver interface {
	ResolveSandboxRuntimeClass(context.Context) (string, error)
	ValidateSandboxRuntimeClass(context.Context, string) error
}

type CreateSpec struct {
	Name, ImageReference, ImageDigest, PresetID, PresetCode, PresetName, ComputeType string
	CPUMillis, MemoryMiB                                                             int64
	SelectionDigest, SpecDigest, ClusterID, Namespace, NamespaceUID, RuntimeClass    string
	Workspace                                                                        WorkspaceSpec
}
type Snapshot struct {
	Name, ImageReference, PresetID, PresetCode, PresetName, ImageDigest, ComputeType, ClusterID, Namespace, NamespaceUID, RuntimeClass, SelectionDigest, SpecDigest string
	CPUMillis, MemoryMiB                                                                                                                                            int64
	Workspace                                                                                                                                                       WorkspaceSpec
}
type ProviderRefs struct {
	PVCName, PVCUID, PodName, PodUID, PVName, PVUID string
	PVCResourceVersion, PodResourceVersion          string
	// ResourceVersion is retained for wire compatibility; new code should use
	// the resource-specific versions above.
	ResourceVersion, CleanupReceipt string
}
type Sandbox struct {
	ID, TenantID, InstanceID                                      string
	Version                                                       int64
	Desired, Observed, Reason, CreateOperationID, LastOperationID string
	Snapshot                                                      Snapshot
	ProviderRefs                                                  ProviderRefs
	ObservedAt                                                    time.Time
	CreateIntentDigest, CreateRequestDigest                       string
	Pending                                                       *Mutation
}
type Operation struct {
	ID, TenantID, SandboxID, InstanceID, Action, IdempotencyKey, State, Reason string
	IntentVersion                                                              int64
	CreatedAt                                                                  time.Time
}
type Create struct {
	TenantID, ID, InstanceID, OperationID, RequestDigest, IntentDigest string
	Spec                                                               CreateSpec
}
type Command struct {
	TenantID, ID, InstanceID, OperationID, Action, IdempotencyKey string
	RequestDigest                                                 string
	ExpectedVersion                                               int64
}
type ACK struct {
	OperationID, ResourceID string
	Accepted                bool
	Replayed                bool
}
type ListFilter struct {
	InstanceID     string
	Page, PageSize uint32
}

// Mutation is persisted before a single provider call. Unknown outcomes retain
// this journal until an attributable observation resolves the call.
type Mutation struct {
	ID, Kind, OperationID string
	Version               int64
	TargetUID             string
	// Attempt fences responses from earlier dispatches of a retried UID deletion.
	Attempt int64 `json:",omitempty"`
	// Once an earlier dispatch may have been sent, a later negative receipt
	// cannot prove that the logical mutation had no Provider side effect.
	PreviousUncertain bool `json:",omitempty"`
}
type MutationResult struct {
	Mutation Mutation
	Outcome  string
	Refs     ProviderRefs
	Reason   string
}
type Observation struct {
	WorkspaceExists, WorkspaceBound, PodExists, VolumeExists, PodTerminated bool
	// VolumeObservationFailed leaves volume existence/reclamation unknown while
	// preserving independently verified workspace and Pod identities. Only the
	// known Pod may be stopped until volume observation succeeds again.
	VolumeObservationFailed bool
	Observed                string
	Reason                  string
	ProviderRefs            ProviderRefs
	At                      time.Time
}
type Work struct {
	Sandbox    Sandbox
	LeaseOwner string
	LeaseEpoch int64
}
type Progress struct {
	Sandbox Sandbox
	Next    time.Duration
	Receipt *MutationResult
}
type WorkerRepository interface {
	Claim(context.Context, string, time.Duration) (*Work, error)
	PrepareMutation(context.Context, Work, Mutation) error
	Finish(context.Context, Work, Progress) error
}
type Provider interface {
	Observe(context.Context, Sandbox) (Observation, error)
	Apply(context.Context, Sandbox, Mutation) (MutationResult, error)
}

// SelectionResolver is the in-process compute port used to freeze Sandbox
// resources. Its implementation is the Resource process' ComputeResolver;
// Sandbox never calls a Compute RPC or imports a storage adapter.
type SelectionResolver interface {
	Resolve(context.Context, string, string, compute.Usage) (compute.Selection, error)
}

// NamespaceResolver prepares a namespace only for initial resolution. Accepted
// commands must use the read-only method to check their frozen UID.
type NamespaceResolver interface {
	EnsureSandboxNamespaceUID(context.Context, string) (string, error)
	ResolveSandboxNamespaceUID(context.Context, string) (string, error)
}

type Repository interface {
	AcceptCreate(context.Context, Create) (ACK, error)
	Get(context.Context, string, string) (Sandbox, error)
	List(context.Context, string, ListFilter) ([]Sandbox, int64, error)
	GetOperation(context.Context, string, string) (Operation, error)
	Change(context.Context, Command) (ACK, error)
}
