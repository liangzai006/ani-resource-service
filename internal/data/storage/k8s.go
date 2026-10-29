// Package storage implements the Storage use-case outbound ports with the
// existing dynamic Kubernetes client and a SigV4 S3 client.
package storage

import (
	"context"
	"fmt"
	"sort"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

var (
	namespaceGVR    = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	pvcGVR          = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	pvGVR           = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
	podGVR          = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	eventGVR        = schema.GroupVersionResource{Version: "v1", Resource: "events"}
	storageClassGVR = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	notebookGVR     = schema.GroupVersionResource{Group: "kubeflow.org", Version: "v1", Resource: "notebooks"}
)

// Kubernetes 是 VolumePort 的 Kubernetes 实现。
type Kubernetes struct {
	client dynamic.Interface
}

func NewKubernetes(client dynamic.Interface) (*Kubernetes, error) {
	if client == nil {
		return nil, fmt.Errorf("Kubernetes client required")
	}
	return &Kubernetes{client: client}, nil
}

// backendError 保留分类，不把 Kubernetes 原始对象细节当作页面文案。
func backendError(err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return biz.ErrNotFound
	case apierrors.IsForbidden(err):
		return biz.ErrAccessDenied
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		return biz.ErrConflict
	default:
		return fmt.Errorf("%w: %v", biz.ErrBackendUnavailable, err)
	}
}

func (k *Kubernetes) GetNamespace(ctx context.Context, name string) (biz.Namespace, error) {
	ns, err := k.client.Resource(namespaceGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return biz.Namespace{}, biz.ErrNotFound
		}
		return biz.Namespace{}, backendError(err)
	}
	return biz.Namespace{
		Name:      ns.GetName(),
		Deleting:  ns.GetDeletionTimestamp() != nil,
		ManagedBy: ns.GetLabels()[biz.NamespaceManagedBy],
		TenantID:  ns.GetLabels()[biz.NamespaceTenantID],
	}, nil
}

func (k *Kubernetes) GetVolume(ctx context.Context, namespace, name string) (*biz.VolumeRecord, error) {
	pvc, err := k.client.Resource(pvcGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, backendError(err)
	}
	record := volumeRecord(pvc)
	if record.StorageClassName != "" {
		if class, err := k.GetStorageClass(ctx, record.StorageClassName); err == nil {
			record.ClassReclaim, record.ClassKnown = class.ReclaimPolicy, true
		}
	}
	return record, nil
}

func (k *Kubernetes) CreateVolume(ctx context.Context, namespace string, spec biz.CreateSpec) (*biz.VolumeRecord, error) {
	object := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":        spec.Name,
			"namespace":   namespace,
			"labels":      stringMap(spec.Labels),
			"annotations": stringMap(spec.Annotations),
		},
		"spec": map[string]any{
			"accessModes":      stringList(spec.AccessModes),
			"volumeMode":       spec.VolumeMode,
			"storageClassName": spec.StorageClassName,
			"resources": map[string]any{
				"requests": map[string]any{"storage": spec.RequestedQuantity},
			},
		},
	}
	created, err := k.client.Resource(pvcGVR).Namespace(namespace).Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{})
	if err != nil {
		return nil, backendError(err)
	}
	return volumeRecord(created), nil
}

func (k *Kubernetes) ListVolumes(ctx context.Context, namespace, tenantID string) ([]*biz.VolumeRecord, error) {
	selector := fmt.Sprintf("%s=%s,%s=%s", biz.LabelManagedBy, biz.ManagedByValue, biz.LabelTenantID, tenantID)
	list, err := k.client.Resource(pvcGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, backendError(err)
	}
	out := make([]*biz.VolumeRecord, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, volumeRecord(&list.Items[i]))
	}
	return out, nil
}

// ResizeVolume 以读到的 resourceVersion 作为更新前置条件，只改申请容量。
func (k *Kubernetes) ResizeVolume(ctx context.Context, namespace, name, expectedUID, expectedResourceVersion, quantity string, capacityBytes int64) (*biz.VolumeRecord, error) {
	current, err := k.client.Resource(pvcGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, backendError(err)
	}
	if string(current.GetUID()) != expectedUID {
		return nil, biz.ErrIdentityChanged
	}
	if current.GetResourceVersion() != expectedResourceVersion {
		return nil, biz.ErrConflict
	}
	if err := unstructured.SetNestedField(current.Object, quantity, "spec", "resources", "requests", "storage"); err != nil {
		return nil, fmt.Errorf("%w: %v", biz.ErrBackendUnavailable, err)
	}
	updated, err := k.client.Resource(pvcGVR).Namespace(namespace).Update(ctx, current, metav1.UpdateOptions{})
	if err != nil {
		return nil, backendError(err)
	}
	record := volumeRecord(updated)
	if record.RequestedBytes != capacityBytes {
		// 以服务端已接受值为准展示，不反改用户申请值。
		record.RequestedBytes = capacityBytes
	}
	return record, nil
}

func (k *Kubernetes) DeleteVolume(ctx context.Context, namespace, name, uid string) error {
	if namespace == "" || name == "" || uid == "" {
		return biz.ErrInvalid
	}
	expected := types.UID(uid)
	err := k.client.Resource(pvcGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &expected}})
	if err != nil {
		return backendError(err)
	}
	return nil
}

func (k *Kubernetes) ListStorageClasses(ctx context.Context) ([]biz.Class, error) {
	list, err := k.client.Resource(storageClassGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, backendError(err)
	}
	out := make([]biz.Class, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, storageClass(&list.Items[i]))
	}
	return out, nil
}

func (k *Kubernetes) GetStorageClass(ctx context.Context, name string) (*biz.Class, error) {
	item, err := k.client.Resource(storageClassGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, backendError(err)
	}
	class := storageClass(item)
	return &class, nil
}

func (k *Kubernetes) GetPersistentVolume(ctx context.Context, name string) (*biz.PVRecord, error) {
	pv, err := k.client.Resource(pvGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, backendError(err)
	}
	policy, _, _ := unstructured.NestedString(pv.Object, "spec", "persistentVolumeReclaimPolicy")
	return &biz.PVRecord{Name: pv.GetName(), ReclaimPolicy: policy}, nil
}

func (k *Kubernetes) ListPodConsumers(ctx context.Context, namespace, claimName string) ([]biz.Consumer, error) {
	list, err := k.client.Resource(podGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, backendError(err)
	}
	out := []biz.Consumer{}
	seen := map[string]bool{}
	for i := range list.Items {
		pod := &list.Items[i]
		if !referencesClaim(pod.Object, claimName, "spec", "volumes") || seen[pod.GetName()] {
			continue
		}
		seen[pod.GetName()] = true
		out = append(out, biz.Consumer{Kind: "Pod", Namespace: namespace, Name: pod.GetName(), Claim: claimName})
	}
	return out, nil
}

func (k *Kubernetes) ListNotebookConsumers(ctx context.Context, namespace, claimName string) ([]biz.Consumer, error) {
	list, err := k.client.Resource(notebookGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, biz.ErrNotInstalled
		}
		return nil, backendError(err)
	}
	out := []biz.Consumer{}
	seen := map[string]bool{}
	for i := range list.Items {
		notebook := &list.Items[i]
		if !referencesClaim(notebook.Object, claimName, "spec", "template", "spec", "volumes") || seen[notebook.GetName()] {
			continue
		}
		seen[notebook.GetName()] = true
		out = append(out, biz.Consumer{Kind: "Notebook", Namespace: namespace, Name: notebook.GetName(), Claim: claimName})
	}
	return out, nil
}

func (k *Kubernetes) ListEvents(ctx context.Context, namespace, pvcUID string) ([]biz.Event, error) {
	list, err := k.client.Resource(eventGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, backendError(err)
	}
	out := []biz.Event{}
	for i := range list.Items {
		event := &list.Items[i]
		uid, _, _ := unstructured.NestedString(event.Object, "involvedObject", "uid")
		kind, _, _ := unstructured.NestedString(event.Object, "involvedObject", "kind")
		if uid != pvcUID || kind != "PersistentVolumeClaim" {
			continue
		}
		item := biz.Event{}
		item.Type, _, _ = unstructured.NestedString(event.Object, "type")
		item.Reason, _, _ = unstructured.NestedString(event.Object, "reason")
		item.Message, _, _ = unstructured.NestedString(event.Object, "message")
		if count, found, _ := unstructured.NestedInt64(event.Object, "count"); found {
			item.Count = int32(count)
		}
		if stamp, found, _ := unstructured.NestedString(event.Object, "lastTimestamp"); found {
			item.LastTimestamp, _ = time.Parse(time.RFC3339, stamp)
		}
		if item.LastTimestamp.IsZero() {
			if stamp, found, _ := unstructured.NestedString(event.Object, "eventTime"); found {
				item.LastTimestamp, _ = time.Parse(time.RFC3339, stamp)
			}
		}
		out = append(out, item)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].LastTimestamp.After(out[b].LastTimestamp) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out, nil
}

func volumeRecord(pvc *unstructured.Unstructured) *biz.VolumeRecord {
	record := &biz.VolumeRecord{
		Name:            pvc.GetName(),
		UID:             string(pvc.GetUID()),
		ResourceVersion: pvc.GetResourceVersion(),
		Deleting:        pvc.GetDeletionTimestamp() != nil,
		Labels:          pvc.GetLabels(),
		Annotations:     pvc.GetAnnotations(),
	}
	record.DisplayName = record.Annotations[biz.AnnotationName]
	record.StorageClassName, _, _ = unstructured.NestedString(pvc.Object, "spec", "storageClassName")
	record.VolumeMode, _, _ = unstructured.NestedString(pvc.Object, "spec", "volumeMode")
	if record.VolumeMode == "" {
		record.VolumeMode = biz.ModeFilesystem
	}
	record.AccessModes, _, _ = unstructured.NestedStringSlice(pvc.Object, "spec", "accessModes")
	record.RequestedQuantity, _, _ = unstructured.NestedString(pvc.Object, "spec", "resources", "requests", "storage")
	record.RequestedBytes = quantityBytes(record.RequestedQuantity)
	record.PVName, _, _ = unstructured.NestedString(pvc.Object, "spec", "volumeName")
	record.Phase, _, _ = unstructured.NestedString(pvc.Object, "status", "phase")
	record.ObservedQuantity, _, _ = unstructured.NestedString(pvc.Object, "status", "capacity", "storage")
	if record.ObservedQuantity != "" {
		record.ObservedBytes = quantityBytes(record.ObservedQuantity)
		record.ObservedKnown = true
	}
	if conditions, found, _ := unstructured.NestedSlice(pvc.Object, "status", "conditions"); found {
		for _, raw := range conditions {
			condition, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			item := biz.Condition{}
			item.Type, _ = condition["type"].(string)
			item.Status, _ = condition["status"].(string)
			item.Reason, _ = condition["reason"].(string)
			item.Message, _ = condition["message"].(string)
			if stamp, ok := condition["lastTransitionTime"].(string); ok {
				item.LastTransitionTime, _ = time.Parse(time.RFC3339, stamp)
			}
			record.Conditions = append(record.Conditions, item)
		}
	}
	return record
}

func quantityBytes(value string) int64 {
	if value == "" {
		return 0
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0
	}
	return quantity.Value()
}

func storageClass(item *unstructured.Unstructured) biz.Class {
	class := biz.Class{
		Name:          item.GetName(),
		ReclaimPolicy: "Delete",
	}
	class.Provisioner, _, _ = unstructured.NestedString(item.Object, "provisioner")
	class.AllowVolumeExpansion, _, _ = unstructured.NestedBool(item.Object, "allowVolumeExpansion")
	class.VolumeBindingMode, _, _ = unstructured.NestedString(item.Object, "volumeBindingMode")
	if class.VolumeBindingMode == "" {
		class.VolumeBindingMode = "Immediate"
	}
	if policy, found, _ := unstructured.NestedString(item.Object, "reclaimPolicy"); found && policy != "" {
		class.ReclaimPolicy = policy
	}
	annotations := item.GetAnnotations()
	class.Default = annotations["storageclass.kubernetes.io/is-default-class"] == "true" ||
		annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"
	return class
}

// referencesClaim 判断指定路径下的 volumes 是否直接引用目标 PVC；其它 PVC 不算。
func referencesClaim(object map[string]any, claimName string, path ...string) bool {
	if claimName == "" {
		return false
	}
	volumes, found, err := unstructured.NestedSlice(object, path...)
	if err != nil || !found {
		return false
	}
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok || volume["persistentVolumeClaim"] == nil {
			continue
		}
		if claim, ok := volume["persistentVolumeClaim"].(map[string]any); ok && claim["claimName"] == claimName {
			return true
		}
	}
	return false
}

func stringMap(values map[string]string) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func stringList(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

var _ biz.VolumePort = (*Kubernetes)(nil)
