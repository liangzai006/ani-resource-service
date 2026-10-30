package instance

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var notebookGVR = schema.GroupVersionResource{Group: "kubeflow.org", Version: "v1", Resource: "notebooks"}
var pvcGVR = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
var pvGVR = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
var podGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
var stsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}

const stoppedAnnotation = "kubeflow-resource-stopped"
const versionAnnotation = "instance.ani.io/intent-version"
const tenantLabel = "accelerator.ani.io/resource-tenant-id"
const resourceLabel = "accelerator.ani.io/resource-id"
const ownerLabel = "accelerator.ani.io/owner-service"
const createLabel = "accelerator.ani.io/create-operation-id"

type Notebook struct {
	client     dynamic.Interface
	clusterID  string
	signingKey ed25519.PrivateKey
	images     imagebiz.RuntimeImages
	logSource  podLogSource
	logLimits  biz.LogLimits
}

func (n *Notebook) ConfigureClusterID(id string) error {
	if !biz.UUID(id) {
		return biz.ErrInvalid
	}
	n.clusterID = id
	return nil
}

func NewNotebook(client dynamic.Interface) (*Notebook, error) {
	if client == nil {
		return nil, biz.ErrInvalid
	}
	return &Notebook{client: client}, nil
}
func (n *Notebook) ConfigureSigningKey(key ed25519.PrivateKey) error {
	if len(key) != ed25519.PrivateKeySize {
		return biz.ErrInvalid
	}
	n.signingKey = append(ed25519.PrivateKey(nil), key...)
	return nil
}
func (n *Notebook) ConfigureImages(images imagebiz.RuntimeImages) error {
	if images == nil {
		return biz.ErrInvalid
	}
	n.images = images
	return nil
}
func (n *Notebook) ValidateEnvironment(ctx context.Context, tenant, namespace, sa, storageClass string) error {
	ns, e := n.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(ctx, namespace, metav1.GetOptions{})
	if e != nil {
		return e
	}
	// Preserve Network's existing Namespace ownership contract without its DB.
	if ns.GetDeletionTimestamp() != nil || ns.GetLabels()["network.ani.io/tenant-id"] != tenant || ns.GetLabels()["network.ani.io/managed-by"] != "ani-network-service" {
		return biz.ErrOwnership
	}
	account, e := n.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}).Namespace(namespace).Get(ctx, sa, metav1.GetOptions{})
	if e != nil {
		return e
	}
	if account.GetDeletionTimestamp() != nil {
		return biz.ErrOwnership
	}
	sc, e := n.client.Resource(schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}).Get(ctx, storageClass, metav1.GetOptions{})
	if e != nil {
		return e
	}
	policy, _, _ := unstructured.NestedString(sc.Object, "reclaimPolicy")
	if policy != "Delete" {
		return fmt.Errorf("workspace StorageClass requires Delete reclaim policy")
	}
	return nil
}
func labels(i biz.Instance) map[string]string {
	return map[string]string{tenantLabel: i.TenantID, resourceLabel: i.ID, ownerLabel: biz.Owner, createLabel: i.CreateOperationID, "ani.io/tenant-id": i.TenantID, "ani.io/instance-id": i.ID}
}
func owned(o *unstructured.Unstructured, i biz.Instance, uid string) bool {
	if o == nil || o.GetNamespace() != i.Spec.Namespace || (uid != "" && string(o.GetUID()) != uid) {
		return false
	}
	for k, v := range labels(i) {
		if o.GetLabels()[k] != v {
			return false
		}
	}
	return true
}
func stringMap(in map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// RenderNotebook consumes the frozen instance specification. User images cannot change the
// managed container count, replicas, selectors, queue, or resource limits.
func RenderNotebook(i biz.Instance) (*unstructured.Unstructured, error) {
	if e := biz.ValidateCreate(biz.Create{TenantID: i.TenantID, ID: i.ID, OperationID: i.CreateOperationID, RequestHash: i.CreateRequestHash, BusinessDigest: i.BusinessDigest, Spec: i.Spec}); e != nil {
		return nil, e
	}
	s, err := runtimeSpec(i)
	if err != nil {
		return nil, err
	}
	if s.Network != nil && (i.Network == nil || i.Network.AttachmentID == "" || i.Network.PrimaryNetworkRef == "" || i.Network.BindingRevision == "") {
		return nil, biz.ErrNetworkUnavailable
	}
	if err := ValidateNotebookConfiguration(s); err != nil {
		return nil, err
	}
	limits := map[string]any{"cpu": s.CPU, "memory": s.Memory}
	for k, v := range s.GPU.Limits {
		if !strings.HasPrefix(k, "volcano.sh/vgpu-") {
			return nil, biz.ErrInvalid
		}
		limits[k] = v
	}
	ann := map[string]string{}
	for k, v := range s.Annotations {
		ann[k] = v
	}
	if s.Network != nil {
		ann["networking.kubercloud.com/subnet"] = i.Network.PrimaryNetworkRef
	}
	for k, v := range s.GPU.Annotations {
		ann[k] = v
	}
	mounts := []any{map[string]any{"name": "workspace", "mountPath": s.WorkspaceMountPath}}
	volumes := []any{map[string]any{"name": "workspace", "persistentVolumeClaim": map[string]any{"claimName": s.PVCName}}}
	for index, mount := range s.VolumeMounts {
		name := borrowedVolumeName(index)
		mounts = append(mounts, map[string]any{"name": name, "mountPath": mount.MountPath, "readOnly": mount.ReadOnly})
		volumes = append(volumes, map[string]any{"name": name, "persistentVolumeClaim": map[string]any{"claimName": mount.PVCName, "readOnly": mount.ReadOnly}})
	}
	pod := map[string]any{"serviceAccountName": s.ServiceAccount, "automountServiceAccountToken": false, "securityContext": map[string]any{"runAsUser": s.UID, "runAsGroup": s.GID, "fsGroup": s.GID}, "containers": []any{map[string]any{"name": biz.Container, "image": s.ImageReference, "imagePullPolicy": "Always", "workingDir": s.WorkspaceMountPath, "securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}}}, "resources": map[string]any{"requests": map[string]any{"cpu": s.CPU, "memory": s.Memory}, "limits": limits}, "volumeMounts": mounts}}, "volumes": volumes}
	if s.ComputeType == biz.TypeGPU {
		ann["scheduling.volcano.sh/queue-name"] = s.GPU.Queue
		ann["accelerator.ani.io/execution-digest"] = s.GPU.Digest
		pod["schedulerName"] = s.GPU.Scheduler
		pod["nodeSelector"] = stringMap(s.GPU.NodeLabels)
	}
	if s.GPU.RuntimeClass != "" {
		pod["runtimeClassName"] = s.GPU.RuntimeClass
	}
	cs := pod["containers"].([]any)
	env := make([]any, 0, len(s.Env)+1)
	// Kubelet expands previous-env references; doubling every dollar preserves literal values.
	// https://kubernetes.io/docs/reference/kubernetes-api/core/pod-v1/
	for _, variable := range s.Env {
		env = append(env, map[string]any{"name": variable.Name, "value": strings.ReplaceAll(variable.Value, "$", "$$")})
	}
	env = append(env, map[string]any{"name": "ANI_INSTANCE_POD_UID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.uid"}}})
	cs[0].(map[string]any)["env"] = env
	if s.ImagePullSecret != "" {
		pod["imagePullSecrets"] = []any{map[string]any{"name": s.ImagePullSecret}}
	}
	if err := ApplySchedulingOptions(pod, i); err != nil {
		return nil, err
	}
	// NotebookTemplateSpec is a PodSpec wrapper, without metadata. The v1.10
	// controller copies Notebook metadata to the new StatefulSet Pod template.
	metaAnn := ann
	metaAnn[versionAnnotation] = strconv.FormatInt(i.Version, 10)
	if i.Pending != nil && i.Pending.ID != "" && i.Pending.Kind == "create_notebook" {
		metaAnn[mutationAnnotation] = i.Pending.ID
	}
	if executionTarget(i) == biz.Stopped {
		metaAnn[stoppedAnnotation] = "true"
	}
	metadataLabels := labels(i)
	for key, value := range instanceNetworkLabels(i) {
		metadataLabels[key] = value
	}
	for k, v := range s.Labels {
		metadataLabels[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubeflow.org/v1", "kind": "Notebook", "metadata": map[string]any{"name": s.NotebookName, "namespace": s.Namespace, "labels": stringMap(metadataLabels), "annotations": stringMap(metaAnn)}, "spec": map[string]any{"template": map[string]any{"spec": pod}}}}, nil
}
func (n *Notebook) Observe(ctx context.Context, i biz.Instance) (biz.Observation, error) {
	out, err := n.ObserveIdentity(ctx, i)
	if err != nil {
		return out, err
	}
	n.observeWorkspace(ctx, i, &out)
	if executionTarget(i) == biz.Running {
		runtime, err := n.VerifyRuntime(ctx, i, out)
		runtime.PVCExists, runtime.PVCUID = out.PVCExists, out.PVCUID
		runtime.VolumeExists, runtime.VolumeName, runtime.VolumeUID = out.VolumeExists, out.VolumeName, out.VolumeUID
		runtime.WorkspaceReason = out.WorkspaceReason
		return runtime, err
	}
	return out, nil
}

func controlledBy(o *unstructured.Unstructured, group, kind, name, uid string) bool {
	if uid == "" {
		return false
	}
	for _, r := range o.GetOwnerReferences() {
		apiGroup, _, ok := strings.Cut(r.APIVersion, "/")
		if ok && apiGroup == group && r.Kind == kind && r.Name == name && string(r.UID) == uid && r.Controller != nil && *r.Controller {
			return true
		}
	}
	return false
}

// PVC absence is not the volume-reclamation receipt. Read only the recorded
// binding (or recover it by exact claim UID); never delete a PV directly.
func (n *Notebook) observeWorkspace(ctx context.Context, i biz.Instance, out *biz.Observation) {
	fail := func() { out.WorkspaceReason = "WORKSPACE_OBSERVATION_FAILED" }
	pvc, e := n.client.Resource(pvcGVR).Namespace(i.Spec.Namespace).Get(ctx, i.Spec.PVCName, metav1.GetOptions{})
	name, uid := i.VolumeName, i.PVCUID
	if e == nil {
		if !owned(pvc, i, i.PVCUID) {
			out.WorkspaceReason = "WORKSPACE_OWNERSHIP_MISMATCH"
			return
		}
		out.PVCExists = true
		out.PVCUID = string(pvc.GetUID())
		uid = out.PVCUID
		bound, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeName")
		if name != "" && bound != "" && name != bound {
			out.WorkspaceReason = "WORKSPACE_OWNERSHIP_MISMATCH"
			return
		}
		if bound != "" {
			name = bound
		}
	}
	if e != nil && !apierrors.IsNotFound(e) {
		fail()
		return
	}
	check := func(pv *unstructured.Unstructured) bool {
		claim, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "uid")
		ns, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "namespace")
		pvcName, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "name")
		policy, _, _ := unstructured.NestedString(pv.Object, "spec", "persistentVolumeReclaimPolicy")
		if uid == "" || claim != uid || ns != i.Spec.Namespace || pvcName != i.Spec.PVCName || (i.VolumeUID != "" && string(pv.GetUID()) != i.VolumeUID) {
			out.WorkspaceReason = "WORKSPACE_OWNERSHIP_MISMATCH"
			return false
		}
		out.VolumeExists = true
		out.VolumeName = pv.GetName()
		out.VolumeUID = string(pv.GetUID())
		if policy != "Delete" {
			out.WorkspaceReason = "WORKSPACE_RECLAIM_POLICY_MISMATCH"
		}
		return true
	}
	if name != "" {
		pv, e := n.client.Resource(pvGVR).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(e) {
			if out.PVCExists {
				out.WorkspaceReason = "WORKSPACE_VOLUME_MISSING"
			}
			return
		}
		if e != nil {
			fail()
			return
		}
		check(pv)
		return
	}
	if out.PVCExists || uid == "" {
		return
	}
	// Recovery when a PVC disappears before the first binding observation.
	continuation := ""
	for {
		list, e := n.client.Resource(pvGVR).List(ctx, metav1.ListOptions{Limit: 200, Continue: continuation})
		if e != nil {
			fail()
			return
		}
		for j := range list.Items {
			pv := &list.Items[j]
			claim, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "uid")
			if claim == uid {
				if out.VolumeExists {
					out.WorkspaceReason = "WORKSPACE_OWNERSHIP_MISMATCH"
					return
				}
				if !check(pv) {
					return
				}
			}
		}
		continuation = list.GetContinue()
		if continuation == "" {
			return
		}
	}
}
func verifyPod(p *unstructured.Unstructured, i biz.Instance) error {
	spec, err := runtimeSpec(i)
	if err != nil {
		return err
	}
	i.Spec = spec
	if i.Spec.Network != nil {
		if i.Network == nil || i.Network.PrimaryNetworkRef == "" || p.GetAnnotations()["networking.kubercloud.com/subnet"] != i.Network.PrimaryNetworkRef {
			return biz.ErrOwnership
		}
		for key, value := range instanceNetworkLabels(i) {
			if p.GetLabels()[key] != value {
				return biz.ErrOwnership
			}
		}
	}
	if err := verifySchedulingOptions(p, i); err != nil {
		return err
	}
	for _, field := range []string{"initContainers", "ephemeralContainers"} {
		containers, _, _ := unstructured.NestedSlice(p.Object, "spec", field)
		for _, v := range containers {
			container, _ := v.(map[string]any)
			for _, resourceField := range []string{"requests", "limits"} {
				resources, _, _ := unstructured.NestedStringMap(container, "resources", resourceField)
				for k := range resources {
					if strings.Contains(k, "gpu") || (i.Spec.ComputeType == biz.TypeCPU && strings.Contains(k, "/")) {
						return biz.ErrOwnership
					}
				}
			}
		}
	}
	cs, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
	if len(cs) != 1 {
		return biz.ErrOwnership
	}
	c, _ := cs[0].(map[string]any)
	if c["name"] != biz.Container || c["image"] != i.Spec.ImageReference {
		return biz.ErrOwnership
	}
	if c["workingDir"] != i.Spec.WorkspaceMountPath {
		return biz.ErrOwnership
	}
	for key, value := range i.Spec.Labels {
		actual, exists := p.GetLabels()[key]
		if !exists || actual != value {
			return biz.ErrOwnership
		}
	}
	for key, value := range i.Spec.Annotations {
		actual, exists := p.GetAnnotations()[key]
		if !exists || actual != value {
			return biz.ErrOwnership
		}
	}
	serviceAccount, _, _ := unstructured.NestedString(p.Object, "spec", "serviceAccountName")
	automount, present, err := unstructured.NestedBool(p.Object, "spec", "automountServiceAccountToken")
	if serviceAccount != i.Spec.ServiceAccount || err != nil || !present || automount {
		return biz.ErrOwnership
	}
	for field, expected := range map[string]int64{"runAsUser": i.Spec.UID, "runAsGroup": i.Spec.GID, "fsGroup": i.Spec.GID} {
		actual, exists, err := unstructured.NestedInt64(p.Object, "spec", "securityContext", field)
		if err != nil || !exists || actual != expected {
			return biz.ErrOwnership
		}
	}
	for field, expected := range map[string]int64{"runAsUser": i.Spec.UID, "runAsGroup": i.Spec.GID} {
		actual, exists, err := unstructured.NestedInt64(c, "securityContext", field)
		if err != nil || (exists && actual != expected) {
			return biz.ErrOwnership
		}
	}
	for _, field := range []string{"requests", "limits"} {
		resources, _, _ := unstructured.NestedStringMap(c, "resources", field)
		for name, expected := range map[string]string{"cpu": i.Spec.CPU, "memory": i.Spec.Memory} {
			actualQuantity, actualErr := resource.ParseQuantity(resources[name])
			expectedQuantity, expectedErr := resource.ParseQuantity(expected)
			if actualErr != nil || expectedErr != nil || actualQuantity.Cmp(expectedQuantity) != 0 {
				return biz.ErrOwnership
			}
		}
		if i.Spec.ComputeType == biz.TypeCPU && len(resources) != 2 {
			return biz.ErrOwnership
		}
	}
	// The exec UID guard and workspace target must survive the controller's
	// projection. Otherwise this Pod is not the frozen instance target.
	guard := 0
	env, _, _ := unstructured.NestedSlice(c, "env")
	for _, value := range env {
		variable, _ := value.(map[string]any)
		if variable["name"] == "ANI_INSTANCE_POD_UID" {
			guard++
			field, _, _ := unstructured.NestedString(variable, "valueFrom", "fieldRef", "fieldPath")
			if field != "metadata.uid" || variable["value"] != nil {
				return biz.ErrOwnership
			}
		}
	}
	if guard != 1 {
		return biz.ErrOwnership
	}
	if err := verifyFrozenEnvironment(env, i.Spec.Env); err != nil {
		return err
	}
	if err := verifyFrozenMounts(p, c, i.Spec); err != nil {
		return err
	}
	limits, _, _ := unstructured.NestedStringMap(c, "resources", "limits")
	for k, v := range i.Spec.GPU.Limits {
		if limits[k] != v {
			return fmt.Errorf("GPU execution was not projected to Pod")
		}
	}
	for k := range limits {
		if strings.Contains(k, "gpu") && i.Spec.GPU.Limits[k] == "" {
			return biz.ErrOwnership
		}
	}
	scheduler, _, _ := unstructured.NestedString(p.Object, "spec", "schedulerName")
	if i.Spec.ComputeType == biz.TypeCPU {
		if scheduler != "" && scheduler != "default-scheduler" {
			return biz.ErrOwnership
		}
		nodes, _, _ := unstructured.NestedStringMap(p.Object, "spec", "nodeSelector")
		runtimeClass, _, _ := unstructured.NestedString(p.Object, "spec", "runtimeClassName")
		if len(nodes) != 0 || runtimeClass != "" {
			return biz.ErrOwnership
		}
		for key := range p.GetAnnotations() {
			if strings.HasPrefix(key, "accelerator.ani.io/") || strings.HasPrefix(key, "volcano.sh/") || strings.HasPrefix(key, "scheduling.volcano.sh/") {
				return biz.ErrOwnership
			}
		}
		return nil
	}
	if p.GetAnnotations()["accelerator.ani.io/execution-digest"] != i.Spec.GPU.Digest {
		return biz.ErrOwnership
	}
	if scheduler != i.Spec.GPU.Scheduler {
		return biz.ErrOwnership
	}
	nodes, _, _ := unstructured.NestedStringMap(p.Object, "spec", "nodeSelector")
	for k, v := range i.Spec.GPU.NodeLabels {
		if nodes[k] != v {
			return biz.ErrOwnership
		}
	}
	for k, v := range i.Spec.GPU.Annotations {
		if p.GetAnnotations()[k] != v {
			return biz.ErrOwnership
		}
	}
	if p.GetAnnotations()["scheduling.volcano.sh/queue-name"] != i.Spec.GPU.Queue {
		return biz.ErrOwnership
	}
	if i.Spec.GPU.RuntimeClass != "" {
		runtime, _, _ := unstructured.NestedString(p.Object, "spec", "runtimeClassName")
		if runtime != i.Spec.GPU.RuntimeClass {
			return biz.ErrOwnership
		}
	}
	return nil
}
func (n *Notebook) signPod(ctx context.Context, p *unstructured.Unstructured, i biz.Instance) error {
	if i.Spec.ComputeType != biz.TypeGPU {
		return biz.ErrInvalid
	}
	payload, e := json.Marshal(map[string]any{"pod": map[string]string{"cluster_id": i.Spec.ClusterID, "namespace": p.GetNamespace(), "name": p.GetName(), "uid": string(p.GetUID()), "container_name": biz.Container}, "ref": map[string]string{"tenant_id": i.TenantID, "owner_service": biz.Owner, "resource_id": i.ID, "create_operation_id": i.CreateOperationID}, "execution_digest": i.Spec.GPU.Digest})
	if e != nil {
		return e
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(n.signingKey, append([]byte("acc-owner-pod-v2\n"), payload...)))
	ann := p.GetAnnotations()
	if ann["accelerator.ani.io/owner-signature"] == sig {
		return nil
	}
	ann["accelerator.ani.io/owner-signature"] = sig
	p.SetAnnotations(ann)
	_, e = n.client.Resource(podGVR).Namespace(p.GetNamespace()).Update(ctx, p, metav1.UpdateOptions{})
	return e
}
func (n *Notebook) EnsureWorkspace(ctx context.Context, i biz.Instance) (string, error) {
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": i.Spec.PVCName, "namespace": i.Spec.Namespace, "labels": stringMap(labels(i))}, "spec": map[string]any{"accessModes": []any{"ReadWriteOnce"}, "storageClassName": i.Spec.StorageClass, "resources": map[string]any{"requests": map[string]any{"storage": i.Spec.WorkspaceSize}}}}}
	created, e := n.client.Resource(pvcGVR).Namespace(i.Spec.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	if e != nil {
		return "", mutationError(e)
	}
	return string(created.GetUID()), nil
}
func (n *Notebook) CreateNotebook(ctx context.Context, i biz.Instance) (string, error) {
	if err := n.verifyBorrowedVolumes(ctx, i); err != nil {
		return "", err
	}
	// The unified writer derives the name; the instance never invents one, and
	// nothing is created when Ensure is unavailable.
	name, e := n.ensurePullSecret(ctx, i)
	if e != nil {
		return "", e
	}
	i.Spec.ImagePullSecret = name
	obj, e := RenderNotebook(i)
	if e != nil {
		return "", e
	}
	created, e := n.client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Create(ctx, obj, metav1.CreateOptions{})
	if e != nil {
		return "", mutationError(e)
	}
	return string(created.GetUID()), nil
}

// ensurePullSecret delegates to the single Image Ensure use case, so instances,
// inference and training share one naming rule, one generation and one writer.
func (n *Notebook) ensurePullSecret(ctx context.Context, i biz.Instance) (string, error) {
	if n.images == nil {
		return "", biz.ErrInvalid
	}
	call := imagebiz.WithCaller(ctx, imagebiz.Caller{Kind: imagebiz.RuntimeCaller, Subject: biz.Owner, TenantID: i.TenantID})
	ensured, e := n.images.EnsureTenantPullSecret(call, imagebiz.EnsurePullSecret{PullSecretScope: imagebiz.PullSecretScope{TenantID: i.TenantID, ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace}})
	if e != nil {
		return "", e
	}
	if ensured.SecretRef == "" || ensured.RegistryAuthority == "" || ensured.Generation < 1 {
		return "", biz.ErrInvalid
	}
	return ensured.SecretRef, nil
}
func (n *Notebook) SetStopped(ctx context.Context, i biz.Instance, o biz.Observation, stopped bool) error {
	if i.Pending != nil && i.Pending.Kind == "set_stopped" && i.Pending.ID != "" {
		if i.Pending.StoppedTarget == nil || *i.Pending.StoppedTarget != stopped {
			return biz.ErrInvalid
		}
		return n.ApplyMutation(ctx, i, *i.Pending)
	}
	if !stopped {
		if err := n.verifyBorrowedVolumes(ctx, i); err != nil {
			return err
		}
	}
	obj, e := n.client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Get(ctx, i.Spec.NotebookName, metav1.GetOptions{})
	if e != nil {
		return e
	}
	if !owned(obj, i, o.NotebookUID) || obj.GetResourceVersion() != o.NotebookRV {
		return biz.ErrVersion
	}
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	current, _ := strconv.ParseInt(ann[versionAnnotation], 10, 64)
	if current > i.Version {
		return biz.ErrVersion
	}
	ann[versionAnnotation] = strconv.FormatInt(i.Version, 10)
	if i.Pending != nil && i.Pending.ID != "" && i.Pending.Kind == "set_stopped" {
		ann[mutationAnnotation] = i.Pending.ID
	}
	if stopped {
		ann[stoppedAnnotation] = "true"
	} else {
		delete(ann, stoppedAnnotation)
	}
	obj.SetAnnotations(ann)
	_, e = n.client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Update(ctx, obj, metav1.UpdateOptions{})
	return mutationError(e)
}
func (n *Notebook) DeleteNotebook(ctx context.Context, i biz.Instance, o biz.Observation) error {
	if o.NotebookUID == "" {
		return biz.ErrInvalid
	}
	uid := types.UID(o.NotebookUID)
	rv := o.NotebookRV
	policy := metav1.DeletePropagationForeground
	e := n.client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Delete(ctx, i.Spec.NotebookName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}, PropagationPolicy: &policy})
	return mutationError(e)
}
func (n *Notebook) DeleteWorkspace(ctx context.Context, i biz.Instance, o biz.Observation) error {
	if o.PVCUID == "" || o.PodExists || o.NotebookExists {
		return biz.ErrInvalid
	}
	for _, mount := range i.CurrentMounts {
		if mount.PVCName == i.Spec.PVCName || mount.PVCUID == o.PVCUID {
			return biz.ErrOwnership
		}
	}
	uid := types.UID(o.PVCUID)
	e := n.client.Resource(pvcGVR).Namespace(i.Spec.Namespace).Delete(ctx, i.Spec.PVCName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	return mutationError(e)
}
func mutationError(e error) error {
	if e == nil {
		return nil
	}
	if apierrors.IsConflict(e) {
		return biz.ErrVersion
	}
	if apierrors.IsInvalid(e) || apierrors.IsForbidden(e) || apierrors.IsUnauthorized(e) || apierrors.IsNotFound(e) {
		return e
	}
	return fmt.Errorf("%w: provider write has no definitive result", biz.ErrUnknown)
}

var _ biz.Provider = (*Notebook)(nil)
