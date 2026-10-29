package instance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"time"
)

// StoredMount is deliberately separate from immutable CREATE's historical JSON.
type StoredMount struct {
	ClusterID           string `json:"cluster_id"`
	Namespace           string `json:"namespace"`
	PVCName             string `json:"pvc_name"`
	PVCUID              string `json:"pvc_uid"`
	MountPath           string `json:"mount_path"`
	ReadOnly            bool   `json:"read_only"`
	Ownership           string `json:"ownership"`
	ReferenceGeneration int64  `json:"reference_generation"`
}

func StoreMounts(mounts []VolumeMount) []StoredMount {
	out := make([]StoredMount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, StoredMount{m.ClusterID, m.Namespace, m.PVCName, m.PVCUID, m.MountPath, m.ReadOnly, m.Ownership, m.ReferenceGeneration})
	}
	return out
}
func LoadMounts(mounts []StoredMount) []VolumeMount {
	out := make([]VolumeMount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, VolumeMount{ClusterID: m.ClusterID, Namespace: m.Namespace, PVCName: m.PVCName, PVCUID: m.PVCUID, MountPath: m.MountPath, ReadOnly: m.ReadOnly, Ownership: m.Ownership, ReferenceGeneration: m.ReferenceGeneration})
	}
	return out
}
func MountDigest(mounts []VolumeMount) string {
	raw, _ := json.Marshal(StoreMounts(mounts))
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

type MountTarget struct {
	VolumeMounts []StoredMount `json:"volume_mounts"`
}
type MountTemplateProof struct {
	Class, NotebookUID, NotebookRV, StatefulSetUID                           string
	NotebookGeneration, StatefulSetGeneration, StatefulSetObservedGeneration int64
	SafeStopped                                                              bool
}
type Execution struct {
	SchemaVersion           int                 `json:"schema_version"`
	Phase                   string              `json:"phase"`
	PhaseSequence           int64               `json:"phase_sequence"`
	CanonicalIntent         json.RawMessage     `json:"canonical_intent,omitempty"`
	BeforeRunState          string              `json:"before_run_state"`
	FinalRunState           string              `json:"final_run_state"`
	BeforeMounts            []StoredMount       `json:"before_mounts"`
	TargetConfiguration     *MountTarget        `json:"target_configuration"`
	TargetMountDigest       string              `json:"target_mount_digest,omitempty"`
	BeforePods              []PodRef            `json:"before_pods"`
	TemplateProof           *MountTemplateProof `json:"template_proof,omitempty"`
	TemplateCommitCompleted bool                `json:"template_commit_completed"`
	AppliedMountVersion     int64               `json:"applied_mount_version"`
	StartedAt               time.Time           `json:"started_at"`
	DeadlineAt              *time.Time          `json:"deadline_at,omitempty"`
	FailureReason           string              `json:"failure_reason"`
	CompletionScope         string              `json:"completion_scope"`
	VerifiedPodUID          string              `json:"verified_pod_uid,omitempty"`
	NetworkSnapshotRevision int64               `json:"network_snapshot_revision,omitempty"`
}

func TerminalOperation(state string) bool {
	return state == "completed" || state == "failed" || state == "superseded"
}
func NewLifecycleAction(action string) bool {
	return action == "restart" || action == "attach-pvc" || action == "detach-pvc"
}
func SameMutation(a, b *Mutation) bool { return reflect.DeepEqual(a, b) }

// RuntimeTarget has no fallback: migrated and newly accepted rows always load it.
func RuntimeTarget(i Instance) string { return i.RuntimeDesired }

type LifecycleProvider interface {
	VerifyAddedMounts(context.Context, Instance, []VolumeMount) error
	ObserveIdentity(context.Context, Instance) (Observation, error)
	VerifyRuntime(context.Context, Instance, Observation) (Observation, error)
	VerifyMountTemplate(context.Context, Instance, []VolumeMount, []VolumeMount) (MountTemplateProof, error)
	ApplyMutation(context.Context, Instance, Mutation) error
	UpdateMounts(context.Context, Instance, Mutation, []VolumeMount) error
}
type CommandReplayRepository interface {
	LookupCommandReplay(context.Context, Command) (ACK, error)
}
type FenceRepository interface {
	BeginRecoveryFence(context.Context, Work, Mutation) error
}

// Legacy tracking adds an internal receipt identity without inferring the old
// stopped payload or claiming the old provider request carried a marker.
type LegacyPendingRepository interface {
	TrackLegacyPending(context.Context, Work) (Mutation, error)
}

type PVCGuardReader interface {
	PVCDeleting(context.Context, string, string, string, string) (bool, error)
}

type LegacyDeleteRepository interface {
	InitializeLegacyDeleteExecution(context.Context, Work) error
}
