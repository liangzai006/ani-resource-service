package sandbox

import (
	"context"
	"errors"
)

// ErrDataPlaneUnavailable means the persisted Sandbox state does not permit
// data-plane access. It does not assert anything about the current Pod.
var ErrDataPlaneUnavailable = errors.New("SANDBOX_DATA_PLANE_UNAVAILABLE")

const (
	DataPlaneFiles   = "files"
	DataPlaneExec    = "exec"
	DataPlaneCodeRun = "code-run"
)

// DataPlaneTarget identifies the persisted workload that an action intends to
// use. It is not an authorization token or proof that the Pod is still ready.
type DataPlaneTarget struct {
	TenantID, SandboxID, Action string
	Version                     int64
	ClusterID, Namespace        string
	NamespaceUID, PodName       string
	PodUID, Container           string
}

// DataPlaneReader is the tenant-scoped persisted Sandbox read port.
type DataPlaneReader interface {
	Get(context.Context, string, string) (Sandbox, error)
}

// ResolveDataPlaneTarget checks persisted intent and identity. The adapter
// must additionally verify the live namespace and Pod, and fence execution
// against a concurrently replaced Pod and pause/delete before exposing it.
func ResolveDataPlaneTarget(ctx context.Context, reader DataPlaneReader, tenant, id, action string) (DataPlaneTarget, error) {
	if err := RequireTenant(ctx, tenant); err != nil {
		return DataPlaneTarget{}, err
	}
	if reader == nil || !UUID(id) || (action != DataPlaneFiles && action != DataPlaneExec && action != DataPlaneCodeRun) {
		return DataPlaneTarget{}, ErrInvalid
	}
	s, err := reader.Get(ctx, tenant, id)
	if err != nil {
		return DataPlaneTarget{}, err
	}
	if s.TenantID != tenant || s.ID != id {
		return DataPlaneTarget{}, ErrDataPlaneUnavailable
	}
	return DataPlaneTargetForSandbox(s, action)
}

// DataPlaneTargetForSandbox extracts a target from an already tenant-scoped
// row. Callers must enforce the caller's tenant before reading that row.
func DataPlaneTargetForSandbox(s Sandbox, action string) (DataPlaneTarget, error) {
	if !UUID(s.TenantID) || !UUID(s.ID) || (action != DataPlaneFiles && action != DataPlaneExec && action != DataPlaneCodeRun) {
		return DataPlaneTarget{}, ErrInvalid
	}
	if s.Version < 1 || s.Desired != DesiredRunning || s.Observed != ObservedRunning || s.Pending != nil ||
		s.Snapshot.ClusterID == "" || s.Snapshot.Namespace == "" || len(s.Snapshot.Namespace) > 63 || !namespaceRE.MatchString(s.Snapshot.Namespace) ||
		s.Snapshot.NamespaceUID == "" || s.ProviderRefs.PodName == "" || s.ProviderRefs.PodUID == "" {
		return DataPlaneTarget{}, ErrDataPlaneUnavailable
	}
	return DataPlaneTarget{
		TenantID: s.TenantID, SandboxID: s.ID, Action: action, Version: s.Version,
		ClusterID: s.Snapshot.ClusterID, Namespace: s.Snapshot.Namespace,
		NamespaceUID: s.Snapshot.NamespaceUID, PodName: s.ProviderRefs.PodName,
		PodUID: s.ProviderRefs.PodUID, Container: "sandbox",
	}, nil
}

// RevalidateDataPlaneTarget rejects targets invalidated by persisted lifecycle
// or identity changes. It does not make a subsequent Provider call atomic.
func RevalidateDataPlaneTarget(ctx context.Context, reader DataPlaneReader, target DataPlaneTarget) error {
	current, err := ResolveDataPlaneTarget(ctx, reader, target.TenantID, target.SandboxID, target.Action)
	if err != nil {
		return err
	}
	if current != target {
		return ErrDataPlaneUnavailable
	}
	return nil
}
