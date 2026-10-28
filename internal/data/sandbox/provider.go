package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var pvcGVR = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
var podGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
var pvGVR = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
var storageClassGVR = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}

const (
	providerOwnerLabel   = "ani.io/sandbox-owner"
	providerTenantLabel  = "ani.io/tenant-id"
	providerServiceLabel = "ani.io/owner-service"
)

type KubernetesProvider struct {
	client    dynamic.Interface
	clusterID string
}

func NewKubernetesProvider(client dynamic.Interface, clusterID string) (*KubernetesProvider, error) {
	if client == nil || !biz.UUID(clusterID) {
		return nil, biz.ErrInvalid
	}
	return &KubernetesProvider{client: client, clusterID: clusterID}, nil
}
func (p *KubernetesProvider) requireCluster(s biz.Sandbox) error {
	if s.Snapshot.ClusterID != p.clusterID {
		return biz.ErrClusterMismatch
	}
	return nil
}

func names(s biz.Sandbox) (string, string) { return "workspace-" + s.ID, "sandbox-" + s.ID }
func labels(s biz.Sandbox) map[string]string {
	return map[string]string{providerOwnerLabel: s.ID, providerTenantLabel: s.TenantID, providerServiceLabel: "ani-resource-service"}
}
func owned(o *unstructured.Unstructured, s biz.Sandbox) bool {
	if o == nil {
		return false
	}
	l := o.GetLabels()
	return o.GetNamespace() == s.Snapshot.Namespace && l[providerOwnerLabel] == s.ID && l[providerTenantLabel] == s.TenantID && l[providerServiceLabel] == "ani-resource-service"
}

func pvcMatches(o *unstructured.Unstructured, s biz.Sandbox) bool {
	storageClass, found, err := unstructured.NestedString(o.Object, "spec", "storageClassName")
	if err != nil || !found || storageClass != s.Snapshot.Workspace.StorageClass {
		return false
	}
	volumeMode, found, err := unstructured.NestedString(o.Object, "spec", "volumeMode")
	if err != nil || (found && volumeMode != "Filesystem") {
		return false
	}
	modes, found, err := unstructured.NestedStringSlice(o.Object, "spec", "accessModes")
	if err != nil || !found || len(modes) != 1 || modes[0] != "ReadWriteOnce" {
		return false
	}
	requested, found, err := unstructured.NestedString(o.Object, "spec", "resources", "requests", "storage")
	if err != nil || !found {
		return false
	}
	want, wantErr := resource.ParseQuantity(s.Snapshot.Workspace.Size)
	got, gotErr := resource.ParseQuantity(requested)
	return wantErr == nil && gotErr == nil && want.Equal(got)
}

func podMatches(o *unstructured.Unstructured, s biz.Sandbox) bool {
	automount, found, err := unstructured.NestedBool(o.Object, "spec", "automountServiceAccountToken")
	if err != nil || !found || automount {
		return false
	}
	for field := range map[string]struct{}{"hostNetwork": {}, "hostPID": {}, "hostIPC": {}} {
		value, found, fieldErr := unstructured.NestedBool(o.Object, "spec", field)
		if fieldErr != nil || (found && value) {
			return false
		}
	}
	security, found, err := unstructured.NestedMap(o.Object, "spec", "securityContext")
	if err != nil || !found || security["runAsNonRoot"] != true || security["runAsUser"] != int64(10001) || security["runAsGroup"] != int64(10001) || security["fsGroup"] != int64(10001) || security["fsGroupChangePolicy"] != "OnRootMismatch" {
		return false
	}
	seccomp, found, err := unstructured.NestedMap(o.Object, "spec", "securityContext", "seccompProfile")
	if err != nil || !found || seccomp["type"] != "RuntimeDefault" {
		return false
	}
	runtimeClass, found, err := unstructured.NestedString(o.Object, "spec", "runtimeClassName")
	if err != nil || !found || runtimeClass != s.Snapshot.RuntimeClass {
		return false
	}
	restartPolicy, found, err := unstructured.NestedString(o.Object, "spec", "restartPolicy")
	if err != nil || !found || restartPolicy != "Never" {
		return false
	}
	containers, found, err := unstructured.NestedSlice(o.Object, "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		return false
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		return false
	}
	containerName, nameOK := container["name"].(string)
	if !nameOK || containerName != "sandbox" {
		return false
	}
	containerSecurity, ok := container["securityContext"].(map[string]any)
	if !ok || containerSecurity["allowPrivilegeEscalation"] != false || containerSecurity["privileged"] != false || containerSecurity["readOnlyRootFilesystem"] != true {
		return false
	}
	capabilities, ok := containerSecurity["capabilities"].(map[string]any)
	if !ok {
		return false
	}
	drop, ok := capabilities["drop"].([]any)
	if !ok || len(drop) != 1 || drop[0] != "ALL" {
		return false
	}
	image, ok := container["image"].(string)
	if !ok || image != s.Snapshot.ImageReference {
		return false
	}
	wantCPU, wantMemory := fmt.Sprintf("%dm", s.Snapshot.CPUMillis), fmt.Sprintf("%dMi", s.Snapshot.MemoryMiB)
	expectedCPU, expectedCPUErr := resource.ParseQuantity(wantCPU)
	expectedMemory, expectedMemoryErr := resource.ParseQuantity(wantMemory)
	if expectedCPUErr != nil || expectedMemoryErr != nil {
		return false
	}
	for _, scope := range []string{"requests", "limits"} {
		cpu, cpuFound, cpuErr := unstructured.NestedString(container, "resources", scope, "cpu")
		memory, memoryFound, memoryErr := unstructured.NestedString(container, "resources", scope, "memory")
		if cpuErr != nil || memoryErr != nil || !cpuFound || !memoryFound {
			return false
		}
		actualCPU, cpuParseErr := resource.ParseQuantity(cpu)
		actualMemory, memoryParseErr := resource.ParseQuantity(memory)
		if cpuParseErr != nil || memoryParseErr != nil || !actualCPU.Equal(expectedCPU) || !actualMemory.Equal(expectedMemory) {
			return false
		}
	}
	mounts, ok := container["volumeMounts"].([]any)
	if !ok || len(mounts) != 1 {
		return false
	}
	mount, ok := mounts[0].(map[string]any)
	mountName, mountNameOK := mount["name"].(string)
	mountPath, mountPathOK := mount["mountPath"].(string)
	if !ok || !mountNameOK || !mountPathOK || mountName != "workspace" || mountPath != s.Snapshot.Workspace.MountPath {
		return false
	}
	volumes, found, err := unstructured.NestedSlice(o.Object, "spec", "volumes")
	if err != nil || !found || len(volumes) != 1 {
		return false
	}
	volume, ok := volumes[0].(map[string]any)
	if !ok {
		return false
	}
	volumeName, volumeNameOK := volume["name"].(string)
	claim, ok := volume["persistentVolumeClaim"].(map[string]any)
	claimName, claimOK := claim["claimName"].(string)
	return volumeNameOK && volumeName == "workspace" && ok && claimOK && claimName == "workspace-"+s.ID
}

func podTerminal(o *unstructured.Unstructured) bool {
	phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
	return phase == "Failed" || phase == "Succeeded"
}

func podReady(o *unstructured.Unstructured) bool {
	if o == nil || o.GetDeletionTimestamp() != nil {
		return false
	}
	phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
	if phase != "Running" {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	ready := false
	for _, item := range conditions {
		condition, _ := item.(map[string]any)
		if condition["type"] == "Ready" && condition["status"] == "True" {
			ready = true
		}
	}
	statuses, _, _ := unstructured.NestedSlice(o.Object, "status", "containerStatuses")
	for _, item := range statuses {
		status, _ := item.(map[string]any)
		if ready && status["name"] == "sandbox" && status["ready"] == true {
			return true
		}
	}
	return false
}

// observeVolume follows the original claim binding even after the PVC is gone.
// A PV is never owned through labels alone: its immutable UID and claimRef must
// refer to the recorded workspace claim.
func (p *KubernetesProvider) observeVolume(ctx context.Context, s biz.Sandbox, pvc *unstructured.Unstructured, refs biz.ProviderRefs) (biz.ProviderRefs, bool, string, error) {
	if pvc != nil {
		name, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeName")
		if name != "" {
			if refs.PVName != "" && refs.PVName != name {
				return refs, false, "", biz.ErrOwnership
			}
			refs.PVName = name
		}
	}
	if pvc == nil && refs.PVName == "" && refs.PVCUID != "" {
		// Recover a binding lost between PVC deletion and the first successful
		// observation. A failed or incomplete list is never a reclaim receipt.
		originalRefs := refs
		continuation := ""
		for {
			list, err := p.client.Resource(pvGVR).List(ctx, metav1.ListOptions{Limit: 200, Continue: continuation})
			if err != nil {
				// An incomplete scan has not ruled out duplicate bindings. Do not
				// persist a first-page identity that would bypass the next scan.
				return originalRefs, false, "", err
			}
			for i := range list.Items {
				pv := &list.Items[i]
				claimUID, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "uid")
				if claimUID != refs.PVCUID {
					continue
				}
				claimNS, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "namespace")
				claimName, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "name")
				if refs.PVName != "" || claimNS != s.Snapshot.Namespace || claimName != refs.PVCName || (refs.PVUID != "" && refs.PVUID != string(pv.GetUID())) {
					return refs, true, "", biz.ErrOwnership
				}
				refs.PVName = pv.GetName()
				refs.PVUID = string(pv.GetUID())
			}
			continuation = list.GetContinue()
			if continuation == "" {
				break
			}
		}
	}
	if refs.PVName == "" {
		return refs, false, "", nil
	}
	pv, err := p.client.Resource(pvGVR).Get(ctx, refs.PVName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return refs, false, "", nil
	}
	if err != nil {
		return refs, false, "", err
	}
	claimNS, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "namespace")
	claimName, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "name")
	claimUID, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "uid")
	if refs.PVCUID == "" || claimNS != s.Snapshot.Namespace || claimName != refs.PVCName || claimUID != refs.PVCUID || (refs.PVUID != "" && refs.PVUID != string(pv.GetUID())) {
		return refs, true, "", biz.ErrOwnership
	}
	refs.PVUID = string(pv.GetUID())
	policy, _, _ := unstructured.NestedString(pv.Object, "spec", "persistentVolumeReclaimPolicy")
	if policy != "Delete" {
		return refs, true, "WORKSPACE_RECLAIM_POLICY_MISMATCH", nil
	}
	return refs, true, "", nil
}

func (p *KubernetesProvider) Observe(ctx context.Context, s biz.Sandbox) (biz.Observation, error) {
	if err := p.requireCluster(s); err != nil {
		return biz.Observation{}, err
	}
	pvcName, podName := names(s)
	ns := s.Snapshot.Namespace
	pvc, err := p.client.Resource(pvcGVR).Namespace(ns).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return biz.Observation{}, err
	}
	pod, err := p.client.Resource(podGVR).Namespace(ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return biz.Observation{}, err
	}
	refs := s.ProviderRefs
	refs.PVCName, refs.PodName = pvcName, podName
	refs.ResourceVersion = ""
	if pvc == nil {
		// Retain the original claim identity. Losing an existing workspace is
		// not permission to provision an empty replacement.
		refs.PVCResourceVersion = ""
	} else {
		if !owned(pvc, s) || !pvcMatches(pvc, s) || (refs.PVCUID != "" && refs.PVCUID != string(pvc.GetUID())) {
			return biz.Observation{}, biz.ErrOwnership
		}
		refs.PVCUID = string(pvc.GetUID())
		refs.PVCResourceVersion = pvc.GetResourceVersion()
		refs.ResourceVersion = pvc.GetResourceVersion()
	}
	if pod == nil {
		refs.PodUID, refs.PodResourceVersion = "", ""
	} else {
		if !owned(pod, s) || !podMatches(pod, s) || (refs.PodUID != "" && refs.PodUID != string(pod.GetUID())) {
			return biz.Observation{}, biz.ErrOwnership
		}
		refs.PodUID = string(pod.GetUID())
		refs.PodResourceVersion = pod.GetResourceVersion()
		if refs.ResourceVersion == "" {
			refs.ResourceVersion = pod.GetResourceVersion()
		}
	}
	refs, volumeExists, volumeReason, err := p.observeVolume(ctx, s, pvc, refs)
	if errors.Is(err, biz.ErrOwnership) {
		return biz.Observation{}, err
	}
	// PVC and Pod identity checks above are independent of the read-only PV
	// query. A failed PV read must not prevent UID-fenced stopping of a known
	// Pod, but cannot establish volume absence or authorize workspace cleanup.
	volumeObservationFailed := err != nil
	if volumeObservationFailed {
		volumeReason = "WORKSPACE_VOLUME_OBSERVATION_FAILED"
	}
	obs := biz.Observation{Observed: biz.ObservedProvisioning, ProviderRefs: refs, At: time.Now().UTC(), WorkspaceExists: pvc != nil, PodExists: pod != nil, VolumeExists: volumeExists, VolumeObservationFailed: volumeObservationFailed}
	if pod != nil {
		obs.PodTerminated = podTerminal(pod)
	}
	phase := ""
	if pvc != nil {
		phase, _, _ = unstructured.NestedString(pvc.Object, "status", "phase")
		obs.WorkspaceBound = phase == "Bound" && pvc.GetDeletionTimestamp() == nil && volumeExists && volumeReason == "" && !volumeObservationFailed
	}
	if s.Desired == biz.DesiredDeleted {
		obs.Observed = biz.ObservedDeleting
		switch {
		case volumeObservationFailed:
			obs.Observed, obs.Reason = biz.ObservedUnknown, volumeReason
		case pvc != nil || pod != nil:
			obs.Reason = volumeReason
		case s.Pending != nil:
			obs.Reason = "PROVIDER_MUTATION_PENDING"
		case volumeExists:
			obs.Reason = "WAITING_FOR_WORKSPACE_RECLAIM"
			if volumeReason != "" {
				obs.Reason = volumeReason
			}
		default:
			obs.Observed = biz.ObservedDeleted
			if refs.CleanupReceipt == "" {
				obs.ProviderRefs.CleanupReceipt = fmt.Sprintf("k8s-delete:%s:%d:pvc=%s:pv=%s", s.ID, s.Version, refs.PVCUID, refs.PVUID)
			}
		}
		return obs, nil
	}
	switch {
	case volumeObservationFailed:
		obs.Observed, obs.Reason = biz.ObservedUnknown, volumeReason
	case pvc == nil && refs.PVCUID != "":
		obs.Observed, obs.Reason = biz.ObservedUnknown, "WORKSPACE_MISSING"
	case phase == "Lost":
		obs.Observed, obs.Reason = biz.ObservedUnknown, "WORKSPACE_LOST"
	case volumeReason != "":
		obs.Observed, obs.Reason = biz.ObservedUnknown, volumeReason
	case phase == "Bound" && refs.PVName == "":
		obs.Observed, obs.Reason = biz.ObservedUnknown, "WORKSPACE_VOLUME_IDENTITY_MISSING"
	case phase == "Bound" && !volumeExists:
		obs.Observed, obs.Reason = biz.ObservedUnknown, "WORKSPACE_VOLUME_MISSING"
	case !obs.WorkspaceBound:
		if pvc != nil && pvc.GetDeletionTimestamp() != nil {
			obs.Reason = "WORKSPACE_DELETING"
		} else if pvc != nil {
			obs.Reason = "WORKSPACE_PENDING"
		}
	case podReady(pod):
		obs.Observed = biz.ObservedRunning
	}
	if s.Desired == biz.DesiredPaused && pod == nil {
		// Pause confirms the workload is stopped; it does not assert that the
		// retained workspace is Bound or healthy. A WFFC claim cannot bind until
		// resume creates its consumer. Preserve storage facts/reasons separately.
		if s.Pending == nil {
			obs.Observed = biz.ObservedPaused
		} else {
			// An unknown create can still produce a late Pod. Absence alone must
			// not complete pause before the mutation journal has been resolved.
			obs.Observed = biz.ObservedUnknown
			if obs.Reason == "" || obs.Reason == "WORKSPACE_PENDING" {
				obs.Reason = "PROVIDER_MUTATION_PENDING"
			}
		}
	}
	return obs, nil
}

func renderWorkspace(s biz.Sandbox, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": name, "namespace": s.Snapshot.Namespace, "labels": stringMap(labels(s))},
		"spec": map[string]any{
			"accessModes":      []any{"ReadWriteOnce"},
			"resources":        map[string]any{"requests": map[string]any{"storage": s.Snapshot.Workspace.Size}},
			"storageClassName": s.Snapshot.Workspace.StorageClass, "volumeMode": "Filesystem",
		},
	}}
}

// mutationOutcome only calls a response rejected when the API definitively
// refused the write. Transport failures, cancellation after dispatch, timeouts,
// and server errors remain unknown and must be recovered by observation.
func mutationOutcome(err error) string {
	if err == nil {
		return biz.MutationApplied
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		code := status.Status().Code
		if code >= 400 && code < 500 && code != 408 {
			return biz.MutationRejected
		}
	}
	return biz.MutationUnknown
}

func (p *KubernetesProvider) Apply(ctx context.Context, s biz.Sandbox, mutation biz.Mutation) (biz.MutationResult, error) {
	if err := p.requireCluster(s); err != nil {
		return biz.MutationResult{Mutation: mutation, Outcome: biz.MutationNotSent, Reason: "SANDBOX_PROVIDER_CLUSTER_MISMATCH"}, err
	}
	result := biz.MutationResult{Mutation: mutation, Outcome: biz.MutationNotSent, Refs: s.ProviderRefs}
	fail := func(reason string, err error) (biz.MutationResult, error) {
		result.Reason = reason
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return fail("PROVIDER_REQUEST_NOT_SENT", err)
	}
	if mutation.ID == "" || mutation.OperationID == "" || mutation.Version <= 0 {
		return fail("INVALID_PROVIDER_MUTATION", biz.ErrInvalid)
	}
	pvcName, podName := names(s)
	ns := s.Snapshot.Namespace
	result.Refs.PVCName, result.Refs.PodName = pvcName, podName
	var gvr schema.GroupVersionResource
	var name, expectedUID string
	var create bool
	switch mutation.Kind {
	case biz.MutationCreateWorkspace:
		gvr, name, expectedUID, create = pvcGVR, pvcName, s.ProviderRefs.PVCUID, true
	case biz.MutationCreatePod:
		gvr, name, expectedUID, create = podGVR, podName, s.ProviderRefs.PodUID, true
	case biz.MutationDeleteWorkspace:
		gvr, name, expectedUID = pvcGVR, pvcName, mutation.TargetUID
	case biz.MutationDeletePod:
		gvr, name, expectedUID = podGVR, podName, mutation.TargetUID
	default:
		return fail("INVALID_PROVIDER_MUTATION", biz.ErrInvalid)
	}
	if !create && expectedUID == "" {
		return fail("PROVIDER_TARGET_UID_REQUIRED", biz.ErrInvalid)
	}
	if !create && ((gvr == pvcGVR && s.ProviderRefs.PVCUID != "" && s.ProviderRefs.PVCUID != expectedUID) || (gvr == podGVR && s.ProviderRefs.PodUID != "" && s.ProviderRefs.PodUID != expectedUID)) {
		return fail("PROVIDER_TARGET_UID_MISMATCH", biz.ErrOwnership)
	}
	o, err := p.getOwned(ctx, gvr, ns, name, s, expectedUID)
	if err != nil && !apierrors.IsNotFound(err) {
		return fail("PROVIDER_TARGET_READ_FAILED", err)
	}
	if o != nil {
		if gvr == pvcGVR {
			result.Refs.PVCUID, result.Refs.PVCResourceVersion = string(o.GetUID()), o.GetResourceVersion()
		} else {
			result.Refs.PodUID, result.Refs.PodResourceVersion = string(o.GetUID()), o.GetResourceVersion()
		}
	}
	if create {
		if o != nil {
			result.Outcome = biz.MutationApplied
			return result, nil
		}
		if gvr == pvcGVR {
			if s.ProviderRefs.PVCUID != "" {
				return fail("WORKSPACE_MISSING", biz.ErrOwnership)
			}
			sc, readErr := p.client.Resource(storageClassGVR).Get(ctx, s.Snapshot.Workspace.StorageClass, metav1.GetOptions{})
			if readErr != nil {
				return fail("WORKSPACE_STORAGE_CLASS_READ_FAILED", readErr)
			}
			policy, _, _ := unstructured.NestedString(sc.Object, "reclaimPolicy")
			if policy != "Delete" || sc.GetDeletionTimestamp() != nil || s.Snapshot.Workspace.ReclaimPolicy != "Delete" {
				return fail("WORKSPACE_RECLAIM_POLICY_MISMATCH", biz.ErrUnsupported)
			}
			o = renderWorkspace(s, name)
		} else {
			// A Pod may be needed to bind a WaitForFirstConsumer claim, but it
			// must never be started against an absent or replaced workspace.
			workspace, readErr := p.getOwned(ctx, pvcGVR, ns, pvcName, s, s.ProviderRefs.PVCUID)
			if readErr != nil {
				return fail("WORKSPACE_READ_FAILED", readErr)
			}
			if workspace.GetDeletionTimestamp() != nil {
				return fail("WORKSPACE_DELETING", biz.ErrOwnership)
			}
			result.Refs.PVCUID, result.Refs.PVCResourceVersion = string(workspace.GetUID()), workspace.GetResourceVersion()
			o = renderPod(s, name, labels(s))
		}
		o.SetAnnotations(map[string]string{"ani.io/sandbox-mutation-id": mutation.ID})
		if err := ctx.Err(); err != nil {
			return fail("PROVIDER_REQUEST_NOT_SENT", err)
		}
		created, createErr := p.client.Resource(gvr).Namespace(ns).Create(ctx, o, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(createErr) {
			// The competing object is safe to adopt only after verifying its
			// ownership, frozen spec, and any previously recorded UID.
			created, err = p.getOwned(ctx, gvr, ns, name, s, expectedUID)
			if err != nil {
				result.Outcome = biz.MutationRejected
				return fail("PROVIDER_EXISTING_TARGET_REJECTED", err)
			}
			createErr = nil
		}
		result.Outcome = mutationOutcome(createErr)
		if createErr != nil {
			return fail("PROVIDER_CREATE_FAILED", createErr)
		}
		if !owned(created, s) || (gvr == pvcGVR && !pvcMatches(created, s)) || (gvr == podGVR && !podMatches(created, s)) || created.GetUID() == "" {
			result.Outcome = biz.MutationUnknown
			return fail("PROVIDER_CREATE_RECEIPT_INVALID", biz.ErrOwnership)
		}
		if gvr == pvcGVR {
			result.Refs.PVCUID, result.Refs.PVCResourceVersion = string(created.GetUID()), created.GetResourceVersion()
		} else {
			result.Refs.PodUID, result.Refs.PodResourceVersion = string(created.GetUID()), created.GetResourceVersion()
		}
		return result, nil
	}
	if o == nil {
		result.Outcome = biz.MutationApplied
		if gvr == podGVR {
			result.Refs.PodUID, result.Refs.PodResourceVersion = "", ""
		}
		return result, nil
	}
	if gvr == pvcGVR {
		pod, readErr := p.getOwned(ctx, podGVR, ns, podName, s, s.ProviderRefs.PodUID)
		if readErr != nil && !apierrors.IsNotFound(readErr) {
			return fail("PROVIDER_POD_READ_FAILED", readErr)
		}
		if pod != nil {
			return fail("WAITING_FOR_POD_DELETION", biz.ErrOwnership)
		}
		refs, _, _, readErr := p.observeVolume(ctx, s, o, result.Refs)
		if readErr != nil {
			return fail("WORKSPACE_VOLUME_READ_FAILED", readErr)
		}
		result.Refs = refs
	}
	if err := ctx.Err(); err != nil {
		return fail("PROVIDER_REQUEST_NOT_SENT", err)
	}
	err = p.client.Resource(gvr).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: ptrUID(expectedUID)}})
	if apierrors.IsNotFound(err) {
		err = nil
	}
	result.Outcome = mutationOutcome(err)
	if err != nil {
		return fail("PROVIDER_DELETE_FAILED", err)
	}
	return result, nil
}

func (p *KubernetesProvider) getOwned(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, s biz.Sandbox, expectedUID string) (*unstructured.Unstructured, error) {
	o, err := p.client.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if !owned(o, s) || (gvr == pvcGVR && !pvcMatches(o, s)) || (gvr == podGVR && !podMatches(o, s)) || (expectedUID != "" && expectedUID != string(o.GetUID())) {
		return nil, biz.ErrOwnership
	}
	return o, nil
}
func renderPod(s biz.Sandbox, name string, ls map[string]string) *unstructured.Unstructured {
	cpu := fmt.Sprintf("%dm", s.Snapshot.CPUMillis)
	memory := fmt.Sprintf("%dMi", s.Snapshot.MemoryMiB)
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": s.Snapshot.Namespace, "labels": stringMap(ls)},
		"spec": map[string]any{
			"runtimeClassName": s.Snapshot.RuntimeClass, "restartPolicy": "Never",
			"automountServiceAccountToken": false,
			"hostNetwork":                  false, "hostPID": false, "hostIPC": false,
			"securityContext": map[string]any{
				"runAsNonRoot": true, "runAsUser": int64(10001), "runAsGroup": int64(10001),
				"fsGroup": int64(10001), "fsGroupChangePolicy": "OnRootMismatch",
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"containers": []any{map[string]any{
				"name": "sandbox", "image": s.Snapshot.ImageReference,
				"command": []any{"/bin/sh", "-c", "trap : TERM INT; sleep infinity & wait"},
				"securityContext": map[string]any{
					"allowPrivilegeEscalation": false, "privileged": false, "readOnlyRootFilesystem": true,
					"capabilities": map[string]any{"drop": []any{"ALL"}},
				},
				"resources":    map[string]any{"requests": map[string]any{"cpu": cpu, "memory": memory}, "limits": map[string]any{"cpu": cpu, "memory": memory}},
				"volumeMounts": []any{map[string]any{"name": "workspace", "mountPath": s.Snapshot.Workspace.MountPath}},
			}},
			"volumes": []any{map[string]any{"name": "workspace", "persistentVolumeClaim": map[string]any{"claimName": "workspace-" + s.ID}}},
		},
	}}
}
func ptrUID(v string) *types.UID {
	if v == "" {
		return nil
	}
	u := types.UID(v)
	return &u
}
func stringMap(m map[string]string) map[string]any {
	r := map[string]any{}
	for k, v := range m {
		r[k] = v
	}
	return r
}
