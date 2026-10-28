package sandbox

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// UnknownCreateReview is a point-in-time diagnostic, not a provider receipt.
// It contains only target identity, observed metadata and credential principals.
type UnknownCreateReview struct {
	ReviewedAt               time.Time        `json:"reviewed_at"`
	DatabaseUser             string           `json:"database_user"`
	DatabaseSessionUser      string           `json:"database_session_user"`
	KubernetesUser           string           `json:"kubernetes_user,omitempty"`
	KubernetesIdentityStatus string           `json:"kubernetes_identity_status"`
	LogicalClusterID         string           `json:"logical_cluster_id"`
	ClusterIdentityStatus    string           `json:"cluster_identity_status"`
	AuditStatus              string           `json:"audit_status"`
	TenantID                 string           `json:"tenant_id"`
	SandboxID                string           `json:"sandbox_id"`
	InstanceID               string           `json:"instance_id"`
	Namespace                string           `json:"namespace"`
	Version                  int64            `json:"version"`
	Desired                  string           `json:"desired"`
	Observed                 string           `json:"observed"`
	Reason                   string           `json:"reason"`
	Pending                  *biz.Mutation    `json:"pending"`
	LastOperationID          string           `json:"last_operation_id"`
	LastOperationState       string           `json:"last_operation_state"`
	LastOperationReason      string           `json:"last_operation_reason"`
	LastOperationVersion     int64            `json:"last_operation_version"`
	LastOperationCreatedAt   time.Time        `json:"last_operation_created_at"`
	LeaseOwner               string           `json:"lease_owner"`
	LeaseEpoch               int64            `json:"lease_epoch"`
	LeaseUntil               string           `json:"lease_until"`
	Refs                     biz.ProviderRefs `json:"provider_refs"`
	Pod                      ReviewObject     `json:"pod"`
	PVC                      ReviewObject     `json:"pvc"`
	PV                       ReviewObject     `json:"pv"`
	Interpretation           string           `json:"interpretation"`
}
type ReviewObject struct {
	Status               string     `json:"status"`
	ObservedAt           *time.Time `json:"observed_at,omitempty"`
	Name                 string     `json:"name,omitempty"`
	UID                  string     `json:"uid,omitempty"`
	ResourceVersion      string     `json:"resource_version,omitempty"`
	Phase                string     `json:"phase,omitempty"`
	NodeName             string     `json:"node_name,omitempty"`
	Deleting             *bool      `json:"deleting,omitempty"`
	OwnershipMatches     *bool      `json:"ownership_matches,omitempty"`
	SpecMatches          *bool      `json:"provider_spec_checks_match,omitempty"`
	ClaimIdentityMatches *bool      `json:"claim_identity_matches,omitempty"`
	RecordedUIDMatches   *bool      `json:"recorded_uid_matches,omitempty"`
}

// ReviewUnknownCreate performs no database state mutation or provider resource
// write. SelfSubjectReview is a non-persisted Kubernetes authentication review.
func (p *Postgres) ReviewUnknownCreate(ctx context.Context, provider *KubernetesProvider, tenant, id string) (UnknownCreateReview, error) {
	var result UnknownCreateReview
	if provider == nil || !biz.UUID(tenant) || !biz.UUID(id) {
		return result, biz.ErrInvalid
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	s, err := scanSandbox(tx.QueryRow(ctx, sandboxSelect+" WHERE s.tenant_id=$1 AND s.sandbox_id=$2", tenant, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return result, biz.ErrNotFound
	}
	if err != nil {
		return result, err
	}
	if s.Pending == nil || (s.Pending.Kind != biz.MutationCreatePod && s.Pending.Kind != biz.MutationCreateWorkspace) {
		return result, biz.ErrConflict
	}
	result = UnknownCreateReview{ReviewedAt: time.Now().UTC(), TenantID: s.TenantID, SandboxID: s.ID, InstanceID: s.InstanceID, Namespace: s.Snapshot.Namespace, Version: s.Version, Desired: s.Desired, Observed: s.Observed, Reason: s.Reason, Pending: s.Pending, LastOperationID: s.LastOperationID, Refs: s.ProviderRefs, LogicalClusterID: s.Snapshot.ClusterID, ClusterIdentityStatus: "physical_cluster_not_authenticated", AuditStatus: "not_recorded", Interpretation: "No observation proves that the original create was never executed or that a previously created workload has stopped; the pending mutation remains unchanged."}
	if err = tx.QueryRow(ctx, `SELECT current_user,session_user`).Scan(&result.DatabaseUser, &result.DatabaseSessionUser); err != nil {
		return UnknownCreateReview{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT lease_owner,lease_epoch,lease_until::text FROM sandbox.work WHERE tenant_id=$1 AND sandbox_id=$2`, tenant, id).Scan(&result.LeaseOwner, &result.LeaseEpoch, &result.LeaseUntil); err != nil {
		return UnknownCreateReview{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT state,reason,intent_version,created_at FROM sandbox.operations WHERE tenant_id=$1 AND sandbox_id=$2 AND operation_id=$3`, tenant, id, s.LastOperationID).Scan(&result.LastOperationState, &result.LastOperationReason, &result.LastOperationVersion, &result.LastOperationCreatedAt); err != nil {
		return UnknownCreateReview{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return UnknownCreateReview{}, err
	}
	if provider.requireCluster(s) != nil {
		result.ClusterIdentityStatus = "logical_cluster_id_mismatch_physical_cluster_not_authenticated"
		result.KubernetesIdentityStatus = "not_observed_cluster_mismatch"
		result.Pod.Status = "not_observed_cluster_mismatch"
		result.PVC.Status = result.Pod.Status
		result.PV.Status = result.Pod.Status
		return result, nil
	}
	reviewGVR := schema.GroupVersionResource{Group: "authentication.k8s.io", Version: "v1", Resource: "selfsubjectreviews"}
	review, err := provider.client.Resource(reviewGVR).Create(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview"}}, metav1.CreateOptions{})
	if err != nil {
		result.KubernetesIdentityStatus = reviewErrorStatus(err)
	} else {
		name, found, e := unstructured.NestedString(review.Object, "status", "userInfo", "username")
		if e != nil || !found || name == "" {
			result.KubernetesIdentityStatus = "unavailable"
		} else {
			result.KubernetesIdentityStatus = "observed"
			result.KubernetesUser = name
		}
	}
	pvcName, podName := names(s)
	result.Pod = provider.reviewObject(ctx, podGVR, s.Snapshot.Namespace, podName, s)
	result.PVC = provider.reviewObject(ctx, pvcGVR, s.Snapshot.Namespace, pvcName, s)
	if s.ProviderRefs.PVName == "" {
		result.PV.Status = "identity_not_recorded"
	} else {
		result.PV = provider.reviewObject(ctx, pvGVR, "", s.ProviderRefs.PVName, s)
	}
	return result, nil
}
func reviewErrorStatus(err error) string {
	switch {
	case apierrors.IsNotFound(err):
		return "not_found"
	case apierrors.IsForbidden(err):
		return "forbidden"
	case apierrors.IsUnauthorized(err):
		return "unauthorized"
	default:
		return "unavailable"
	}
}
func (p *KubernetesProvider) reviewObject(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, s biz.Sandbox) ReviewObject {
	result := ReviewObject{Name: name}
	r := p.client.Resource(gvr)
	var o *unstructured.Unstructured
	var err error
	if namespace == "" {
		o, err = r.Get(ctx, name, metav1.GetOptions{})
	} else {
		o, err = r.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	observedAt := time.Now().UTC()
	result.ObservedAt = &observedAt
	if err != nil {
		result.Status = reviewErrorStatus(err)
		return result
	}
	if o == nil {
		result.Status = "unavailable"
		return result
	}
	result.Status = "found"
	result.UID = string(o.GetUID())
	result.ResourceVersion = o.GetResourceVersion()
	deleting := o.GetDeletionTimestamp() != nil
	result.Deleting = &deleting
	result.Phase, _, _ = unstructured.NestedString(o.Object, "status", "phase")
	switch gvr {
	case podGVR:
		if s.ProviderRefs.PodUID != "" {
			matches := s.ProviderRefs.PodUID == result.UID
			result.RecordedUIDMatches = &matches
		}
		result.NodeName, _, _ = unstructured.NestedString(o.Object, "spec", "nodeName")
		ownership := owned(o, s)
		result.OwnershipMatches = &ownership
		if ownership {
			matches := podMatches(o, s)
			result.SpecMatches = &matches
		}
	case pvcGVR:
		if s.ProviderRefs.PVCUID != "" {
			matches := s.ProviderRefs.PVCUID == result.UID
			result.RecordedUIDMatches = &matches
		}
		ownership := owned(o, s)
		result.OwnershipMatches = &ownership
		if ownership {
			matches := pvcMatches(o, s)
			result.SpecMatches = &matches
		}
	case pvGVR:
		if s.ProviderRefs.PVUID != "" {
			matches := s.ProviderRefs.PVUID == result.UID
			result.RecordedUIDMatches = &matches
		}
		uid, _, _ := unstructured.NestedString(o.Object, "spec", "claimRef", "uid")
		ns, _, _ := unstructured.NestedString(o.Object, "spec", "claimRef", "namespace")
		claim, _, _ := unstructured.NestedString(o.Object, "spec", "claimRef", "name")
		if s.ProviderRefs.PVCUID != "" {
			matches := uid == s.ProviderRefs.PVCUID && ns == s.Snapshot.Namespace && claim == "workspace-"+s.ID && (s.ProviderRefs.PVUID == "" || s.ProviderRefs.PVUID == result.UID)
			result.ClaimIdentityMatches = &matches
		}
	}
	return result
}
