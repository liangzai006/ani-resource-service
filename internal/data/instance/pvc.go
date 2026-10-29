package instance

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"github.com/zhangzhe-ctrl/ani-resource-service/pkg/instanceconfig"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

var namespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

const pvcListPage = 200
const pvcListMaximum = 2000

// ValidateNotebookConfiguration applies Kubernetes-specific input constraints.
// It deliberately reports field names and safe reasons, never literal values.
func ValidateNotebookConfiguration(s biz.Spec) error {
	if err := biz.ValidateConfiguration(s); err != nil {
		return err
	}
	if err := validateMountPath(s.WorkspaceMountPath); err != nil {
		return err
	}
	for _, mount := range s.VolumeMounts {
		if err := validateMountPath(mount.MountPath); err != nil {
			return err
		}
		if len(validation.IsDNS1123Subdomain(mount.PVCName)) != 0 {
			return fmt.Errorf("%w: volume_mounts.pvc_name", biz.ErrInvalid)
		}
	}
	for _, metadata := range []map[string]string{s.Labels, s.Annotations} {
		for key := range metadata {
			if instanceconfig.ReservedKey(key) {
				return fmt.Errorf("%w: metadata key", biz.ErrReservedConfig)
			}
			if len(validation.IsQualifiedName(key)) != 0 {
				return fmt.Errorf("%w: metadata key", biz.ErrInvalid)
			}
		}
	}
	for _, value := range s.Labels {
		if len(validation.IsValidLabelValue(value)) != 0 {
			return fmt.Errorf("%w: labels value format", biz.ErrInvalid)
		}
	}
	for _, env := range s.Env {
		if len(validation.IsEnvVarName(env.Name)) != 0 {
			return fmt.Errorf("%w: env.name", biz.ErrInvalid)
		}
		if instanceconfig.ReservedEnv(env.Name) {
			return fmt.Errorf("%w: env.name", biz.ErrReservedConfig)
		}
	}
	return nil
}

func validateMountPath(value string) error {
	if !instanceconfig.MountPath(value) {
		return fmt.Errorf("%w: mount path must be a supported canonical absolute directory", biz.ErrInvalid)
	}
	return nil
}

func (n *Notebook) trustedVolumeNamespace(ctx context.Context, tenant, cluster, namespace string) error {
	if n.clusterID != "" && n.clusterID != cluster {
		return biz.ErrNetworkUnavailable
	}
	if !biz.UUID(tenant) || !biz.UUID(cluster) || len(validation.IsDNS1123Label(namespace)) != 0 {
		return biz.ErrInvalid
	}
	ns, err := n.lifecycleGet(ctx, namespaceGVR, "", namespace)
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			return biz.ErrPVCNotFound
		}
		return err
	}
	if ns.GetDeletionTimestamp() != nil || ns.GetLabels()["network.ani.io/tenant-id"] != tenant || ns.GetLabels()["network.ani.io/managed-by"] != "ani-network-service" {
		return biz.ErrPVCNotFound
	}
	return nil
}

func dedicatedPVC(pvc *unstructured.Unstructured) bool {
	labels := pvc.GetLabels()
	if labels[resourceLabel] != "" || labels[ownerLabel] != "" || labels["ani.io/instance-id"] != "" {
		return true
	}
	for _, owner := range pvc.GetOwnerReferences() {
		if owner.Kind == "Notebook" && strings.HasPrefix(owner.APIVersion, "kubeflow.org/") {
			return true
		}
	}
	return false
}

func mountablePVC(pvc *unstructured.Unstructured) (biz.MountablePVC, string) {
	phase, _, _ := unstructured.NestedString(pvc.Object, "status", "phase")
	mode, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeMode")
	if mode == "" {
		mode = "Filesystem"
	}
	capacity, _, _ := unstructured.NestedString(pvc.Object, "spec", "resources", "requests", "storage")
	access, _, _ := unstructured.NestedStringSlice(pvc.Object, "spec", "accessModes")
	out := biz.MountablePVC{PVCName: pvc.GetName(), Phase: phase, RequestedCapacity: capacity, AccessModes: access, VolumeMode: mode, Selectable: true}
	switch {
	case pvc.GetDeletionTimestamp() != nil:
		out.UnavailableReason = "PVC_TERMINATING"
	case pvc.GetUID() == "":
		out.UnavailableReason = "PVC_IDENTITY_MISSING"
	case mode != "Filesystem":
		out.UnavailableReason = "PVC_FILESYSTEM_REQUIRED"
	case phase == "Lost":
		out.UnavailableReason = "PVC_LOST"
	}
	out.Selectable = out.UnavailableReason == ""
	return out, out.UnavailableReason
}

// ResolveVolumes freezes identities only. Borrowed PVCs never acquire owner
// references or lifecycle mutations from Instance.
func (n *Notebook) ResolveVolumes(ctx context.Context, tenant, cluster, namespace string, mounts []biz.VolumeMount) ([]biz.VolumeMount, error) {
	if len(mounts) > 8 {
		return nil, biz.ErrInvalid
	}
	if err := n.trustedVolumeNamespace(ctx, tenant, cluster, namespace); err != nil {
		return nil, err
	}
	out := make([]biz.VolumeMount, 0, len(mounts))
	seen := map[string]bool{}
	for _, mount := range mounts {
		if mount.PVCUID != "" || mount.ClusterID != "" || mount.Namespace != "" || mount.Ownership != "" || len(validation.IsDNS1123Subdomain(mount.PVCName)) != 0 {
			return nil, biz.ErrInvalid
		}
		if err := validateMountPath(mount.MountPath); err != nil {
			return nil, err
		}
		if seen[mount.MountPath] {
			return nil, biz.ErrInvalid
		}
		seen[mount.MountPath] = true
		pvc, err := n.lifecycleGet(ctx, pvcGVR, namespace, mount.PVCName)
		if err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
				return nil, biz.ErrPVCNotFound
			}
			return nil, err
		}
		if pvc.GetNamespace() != namespace || dedicatedPVC(pvc) || (pvc.GetLabels()[tenantLabel] != "" && pvc.GetLabels()[tenantLabel] != tenant) {
			return nil, biz.ErrPVCNotFound
		}
		if _, reason := mountablePVC(pvc); reason != "" {
			return nil, fmt.Errorf("%w: %s", biz.ErrInvalid, reason)
		}
		if !mount.ReadOnly && pvcReadOnlyOnly(pvc) {
			return nil, fmt.Errorf("%w: read-only PVC requires read_only", biz.ErrInvalid)
		}
		mount.ClusterID, mount.Namespace, mount.PVCUID, mount.Ownership = cluster, namespace, string(pvc.GetUID()), "borrowed"
		out = append(out, mount)
	}
	return out, nil
}

// verifyBorrowedVolumes is a fresh pre-write/read identity check. Kubernetes
// claimName has no UID precondition: supported deployments must separately
// enforce PVC deletion/admission protection for active borrowed references.
func (n *Notebook) verifyBorrowedVolumes(ctx context.Context, i biz.Instance) error {
	if !i.MountsInitialized {
		return biz.ErrInvalid
	}
	return n.verifyBorrowedMounts(ctx, i, i.CurrentMounts)
}

func (n *Notebook) verifyBorrowedMounts(ctx context.Context, i biz.Instance, mounts []biz.VolumeMount) error {
	if len(mounts) == 0 {
		return nil
	}
	if err := n.trustedVolumeNamespace(ctx, i.TenantID, i.Spec.ClusterID, i.Spec.Namespace); err != nil {
		return err
	}
	for _, mount := range mounts {
		if mount.ClusterID != i.Spec.ClusterID || mount.Namespace != i.Spec.Namespace || mount.Ownership != "borrowed" || mount.PVCUID == "" || mount.PVCName == i.Spec.PVCName {
			return biz.ErrPVCReferenceChanged
		}
		pvc, err := n.lifecycleGet(ctx, pvcGVR, i.Spec.Namespace, mount.PVCName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return biz.ErrPVCReferenceChanged
			}
			return err
		}
		if string(pvc.GetUID()) != mount.PVCUID || pvc.GetNamespace() != mount.Namespace || dedicatedPVC(pvc) || (pvc.GetLabels()[tenantLabel] != "" && pvc.GetLabels()[tenantLabel] != i.TenantID) {
			return biz.ErrPVCReferenceChanged
		}
		if _, reason := mountablePVC(pvc); reason != "" {
			return biz.ErrPVCReferenceChanged
		}
		if !mount.ReadOnly && pvcReadOnlyOnly(pvc) {
			return biz.ErrPVCReferenceChanged
		}
	}
	return nil
}

func pvcReadOnlyOnly(pvc *unstructured.Unstructured) bool {
	modes, _, _ := unstructured.NestedStringSlice(pvc.Object, "spec", "accessModes")
	return len(modes) == 1 && modes[0] == "ReadOnlyMany"
}

// Only borrowed volume fields are replaced. Workspace, image, compute, env,
// labels and provider-injected non-PVC volumes remain exactly as observed.
func replaceManagedMounts(notebook *unstructured.Unstructured, spec biz.Spec, target []biz.VolumeMount) error {
	spec.VolumeMounts = target
	if err := ValidateNotebookConfiguration(spec); err != nil {
		return err
	}
	podSpec, present, err := unstructured.NestedMap(notebook.Object, "spec", "template", "spec")
	if err != nil || !present {
		return biz.ErrOwnership
	}
	containers, _, err := unstructured.NestedSlice(podSpec, "containers")
	if err != nil || len(containers) != 1 {
		return biz.ErrOwnership
	}
	container, ok := containers[0].(map[string]any)
	if !ok || container["name"] != biz.Container {
		return biz.ErrOwnership
	}
	volumes, _, err := unstructured.NestedSlice(podSpec, "volumes")
	if err != nil {
		return err
	}
	mounts, _, err := unstructured.NestedSlice(container, "volumeMounts")
	if err != nil {
		return err
	}
	keptVolumes, keptMounts := []any{}, []any{}
	for _, value := range volumes {
		volume, _ := value.(map[string]any)
		name, _ := volume["name"].(string)
		if !managedBorrowedVolumeName(name) {
			keptVolumes = append(keptVolumes, value)
		}
	}
	for _, value := range mounts {
		mount, _ := value.(map[string]any)
		name, _ := mount["name"].(string)
		if !managedBorrowedVolumeName(name) {
			keptMounts = append(keptMounts, value)
		}
	}
	for index, mount := range target {
		name := borrowedVolumeName(index)
		keptVolumes = append(keptVolumes, map[string]any{"name": name, "persistentVolumeClaim": map[string]any{"claimName": mount.PVCName, "readOnly": mount.ReadOnly}})
		keptMounts = append(keptMounts, map[string]any{"name": name, "mountPath": mount.MountPath, "readOnly": mount.ReadOnly})
	}
	podSpec["volumes"], container["volumeMounts"] = keptVolumes, keptMounts
	podSpec["containers"] = containers
	pod := &unstructured.Unstructured{Object: map[string]any{"spec": podSpec}}
	if err := verifyFrozenMounts(pod, container, spec); err != nil {
		return err
	}
	return unstructured.SetNestedMap(notebook.Object, podSpec, "spec", "template", "spec")
}

func managedBorrowedVolumeName(name string) bool {
	suffix, found := strings.CutPrefix(name, "borrowed-")
	index, err := strconv.Atoi(suffix)
	return found && err == nil && index >= 0 && index < 8 && suffix == strconv.Itoa(index)
}

func (n *Notebook) ListMountablePVCs(ctx context.Context, tenant, cluster, namespace string, page, size uint32, name string) ([]biz.MountablePVC, int64, error) {
	_, size, offset, err := biz.PageWindow(page, size)
	if err != nil || len(name) > 253 {
		return nil, 0, biz.ErrInvalid
	}
	if err := n.trustedVolumeNamespace(ctx, tenant, cluster, namespace); err != nil {
		return nil, 0, err
	}
	// A continue expiration invalidates every earlier page. Restart from an
	// empty accumulator, with a bounded number of attempts under churn.
	for attempt := 0; attempt < 3; attempt++ {
		items := []biz.MountablePVC{}
		continuation, version := "", ""
		count := 0
		restart := false
		for {
			list, err := n.client.Resource(pvcGVR).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: pvcListPage, Continue: continuation})
			if apierrors.IsResourceExpired(err) {
				restart = true
				break
			}
			if err != nil {
				return nil, 0, err
			}
			if continuation != "" && version != list.GetResourceVersion() {
				return nil, 0, fmt.Errorf("PVC catalog snapshot changed")
			}
			version = list.GetResourceVersion()
			count += len(list.Items)
			if count > pvcListMaximum || (count == pvcListMaximum && list.GetContinue() != "") {
				return nil, 0, biz.ErrPVCCatalogLimit
			}
			for k := range list.Items {
				pvc := &list.Items[k]
				if pvc.GetNamespace() != namespace || dedicatedPVC(pvc) || (pvc.GetLabels()[tenantLabel] != "" && pvc.GetLabels()[tenantLabel] != tenant) || !strings.Contains(pvc.GetName(), name) {
					continue
				}
				item, _ := mountablePVC(pvc)
				items = append(items, item)
			}
			continuation = list.GetContinue()
			if continuation == "" {
				break
			}
		}
		if restart {
			continue
		}
		sort.Slice(items, func(a, b int) bool { return items[a].PVCName < items[b].PVCName })
		total := int64(len(items))
		if offset >= total {
			return []biz.MountablePVC{}, total, nil
		}
		end := offset + int64(size)
		if end > total {
			end = total
		}
		return items[offset:end], total, nil
	}
	return nil, 0, fmt.Errorf("PVC catalog snapshot expired repeatedly")
}

var _ biz.VolumeResolver = (*Notebook)(nil)

func borrowedVolumeName(index int) string { return fmt.Sprintf("borrowed-%d", index) }

func verifyFrozenEnvironment(actual []any, expected []biz.EnvVar) error {
	previous := -1
	for _, frozen := range expected {
		found := -1
		for index, value := range actual {
			variable, _ := value.(map[string]any)
			if variable["name"] != frozen.Name {
				continue
			}
			if found != -1 || variable["valueFrom"] != nil {
				return biz.ErrOwnership
			}
			literal, _, err := unstructured.NestedString(variable, "value")
			if err != nil || literal != strings.ReplaceAll(frozen.Value, "$", "$$") {
				return biz.ErrOwnership
			}
			found = index
		}
		if found <= previous {
			return biz.ErrOwnership
		}
		previous = found
	}
	return nil
}

func verifyFrozenMounts(pod *unstructured.Unstructured, container map[string]any, s biz.Spec) error {
	type projection struct {
		claim, path string
		readOnly    bool
	}
	expected := map[string]projection{"workspace": {claim: s.PVCName, path: s.WorkspaceMountPath}}
	for index, mount := range s.VolumeMounts {
		expected[borrowedVolumeName(index)] = projection{claim: mount.PVCName, path: mount.MountPath, readOnly: mount.ReadOnly}
	}
	volumeCounts, mountCounts := map[string]int{}, map[string]int{}
	volumes, _, _ := unstructured.NestedSlice(pod.Object, "spec", "volumes")
	for _, value := range volumes {
		volume, _ := value.(map[string]any)
		name, _ := volume["name"].(string)
		frozen, managed := expected[name]
		if !managed {
			if volume["persistentVolumeClaim"] != nil {
				return biz.ErrOwnership
			}
			continue
		}
		volumeCounts[name]++
		claim, _, err := unstructured.NestedString(volume, "persistentVolumeClaim", "claimName")
		readOnly, _, roErr := unstructured.NestedBool(volume, "persistentVolumeClaim", "readOnly")
		if err != nil || roErr != nil || claim != frozen.claim || readOnly != frozen.readOnly {
			return biz.ErrOwnership
		}
	}
	mounts, _, _ := unstructured.NestedSlice(container, "volumeMounts")
	for _, value := range mounts {
		mount, _ := value.(map[string]any)
		name, _ := mount["name"].(string)
		mountPath, _ := mount["mountPath"].(string)
		frozen, managed := expected[name]
		if !managed {
			for _, required := range expected {
				if mountPath == required.path || strings.HasPrefix(mountPath, required.path+"/") || strings.HasPrefix(required.path, mountPath+"/") {
					return biz.ErrOwnership
				}
			}
			continue
		}
		mountCounts[name]++
		readOnly, _, err := unstructured.NestedBool(mount, "readOnly")
		if err != nil || mountPath != frozen.path || readOnly != frozen.readOnly || mount["subPath"] != nil || mount["subPathExpr"] != nil {
			return biz.ErrOwnership
		}
	}
	for name := range expected {
		if volumeCounts[name] != 1 || mountCounts[name] != 1 {
			return biz.ErrOwnership
		}
	}
	return nil
}
