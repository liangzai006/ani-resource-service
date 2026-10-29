package instance

import (
	"context"
	"errors"
	"strconv"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const mutationAnnotation = "instance.ani.io/mutation-id"

// Each request is bounded independently; the worker also bounds its whole Step.
func (n *Notebook) lifecycleGet(ctx context.Context, resource schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return n.client.Resource(resource).Namespace(namespace).Get(call, name, metav1.GetOptions{})
}

func (n *Notebook) lifecycleList(ctx context.Context, resource schema.GroupVersionResource, namespace string) (*unstructured.UnstructuredList, error) {
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Labels are mutable. Only the immutable controller chain selects objects.
	return n.client.Resource(resource).Namespace(namespace).List(call, metav1.ListOptions{})
}

func runtimeSpec(i biz.Instance) (biz.Spec, error) {
	if !i.MountsInitialized {
		return biz.Spec{}, biz.ErrState
	}
	s := i.Spec
	s.VolumeMounts = append([]biz.VolumeMount{}, i.CurrentMounts...)
	return s, nil
}

func executionTarget(i biz.Instance) string {
	return biz.RuntimeTarget(i)
}

// ObserveIdentity proves control and absence without inspecting runtime
// eligibility. A lost borrowed PVC, invalid Pod configuration, or unavailable
// GPU signer must never prevent stopping an object whose owner is proven.
func (n *Notebook) ObserveIdentity(ctx context.Context, i biz.Instance) (biz.Observation, error) {
	var out biz.Observation
	if n.clusterID != "" && n.clusterID != i.Spec.ClusterID {
		return out, biz.ErrNetworkUnavailable
	}
	obj, err := n.lifecycleGet(ctx, notebookGVR, i.Spec.Namespace, i.Spec.NotebookName)
	if err == nil {
		if obj.GetUID() == "" || !owned(obj, i, i.NotebookUID) {
			return out, biz.ErrOwnership
		}
		out.NotebookExists, out.NotebookUID, out.NotebookRV = true, string(obj.GetUID()), obj.GetResourceVersion()
		out.NotebookGeneration = obj.GetGeneration()
		_, out.Stopped = obj.GetAnnotations()[stoppedAnnotation]
		out.IntentVersion, _ = strconv.ParseInt(obj.GetAnnotations()[versionAnnotation], 10, 64)
		out.MutationID = obj.GetAnnotations()[mutationAnnotation]
	} else if !apierrors.IsNotFound(err) {
		return out, err
	}
	notebookUID := out.NotebookUID
	if notebookUID == "" {
		notebookUID = i.NotebookUID
	}
	set, err := n.lifecycleGet(ctx, stsGVR, i.Spec.Namespace, i.Spec.NotebookName)
	setGenerationObserved := false
	if err == nil {
		if set.GetUID() == "" || !controlledBy(set, "kubeflow.org", "Notebook", i.Spec.NotebookName, notebookUID) {
			return out, biz.ErrOwnership
		}
		out.StatefulSetUID = string(set.GetUID())
		out.StatefulSetExists = true
		out.StatefulSetGeneration = set.GetGeneration()
		out.StatefulSetObservedGeneration, setGenerationObserved, err = unstructured.NestedInt64(set.Object, "status", "observedGeneration")
		if err != nil {
			return out, err
		}
		replicas, present, e := unstructured.NestedInt64(set.Object, "spec", "replicas")
		if e != nil || replicas < 0 || replicas > 2147483647 {
			return out, biz.ErrOwnership
		}
		if !present {
			replicas = 1 // Kubernetes defaults this field; never infer stopped.
		}
		out.StatefulSetReplicas = int32(replicas)
	} else if !apierrors.IsNotFound(err) {
		return out, err
	}
	setUIDs := map[string]bool{}
	if out.StatefulSetUID != "" {
		setUIDs[out.StatefulSetUID] = true
	}
	if i.StatefulSetUID != "" {
		setUIDs[i.StatefulSetUID] = true
	}
	tracked := biz.MergePodRefs(i.TrackedPods, i.GPUPods)
	if i.PodUID != "" || i.PodName != "" {
		tracked = biz.MergePodRefs(tracked, []biz.PodRef{{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, Name: i.PodName, UID: i.PodUID, ContainerName: biz.Container}})
	}
	// Persisted identities are directly read even when labels have disappeared.
	for _, ref := range tracked {
		if ref.ClusterID != i.Spec.ClusterID || ref.ClusterID == "" || ref.Namespace != i.Spec.Namespace || ref.Namespace == "" || ref.Name == "" || ref.UID == "" {
			return out, biz.ErrOwnership
		}
		pod, e := n.lifecycleGet(ctx, podGVR, ref.Namespace, ref.Name)
		if apierrors.IsNotFound(e) {
			continue
		}
		if e != nil {
			return out, e
		}
		if string(pod.GetUID()) == ref.UID {
			identityPod(&out, i, pod)
		}
	}
	pods, err := n.lifecycleList(ctx, podGVR, i.Spec.Namespace)
	if err != nil {
		return out, err
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		controlled := false
		for setUID := range setUIDs {
			controlled = controlled || controlledBy(pod, "apps", "StatefulSet", i.Spec.NotebookName, setUID)
		}
		if !controlled {
			if pod.GetLabels()[resourceLabel] == i.ID {
				return out, biz.ErrOwnership
			}
			continue
		}
		if pod.GetNamespace() != i.Spec.Namespace || pod.GetUID() == "" {
			return out, biz.ErrOwnership
		}
		identityPod(&out, i, pod)
	}
	if !out.NotebookExists || !out.Stopped || out.PodExists || (out.StatefulSetUID != "" && (!setGenerationObserved || out.StatefulSetGeneration < 1 || out.StatefulSetReplicas != 0 || out.StatefulSetObservedGeneration < out.StatefulSetGeneration)) {
		return out, nil
	}
	// A controller update during Pod inspection invalidates this turn's proof.
	last, err := n.lifecycleGet(ctx, notebookGVR, i.Spec.Namespace, i.Spec.NotebookName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return out, nil
		}
		return out, err
	}
	if !owned(last, i, out.NotebookUID) {
		return out, biz.ErrOwnership
	}
	if last.GetResourceVersion() != out.NotebookRV || last.GetGeneration() != out.NotebookGeneration {
		return out, nil
	}
	if _, stopped := last.GetAnnotations()[stoppedAnnotation]; !stopped {
		return out, nil
	}
	lastSet, err := n.lifecycleGet(ctx, stsGVR, i.Spec.Namespace, i.Spec.NotebookName)
	if out.StatefulSetUID == "" {
		if !apierrors.IsNotFound(err) {
			return out, err
		}
	} else {
		if err != nil {
			if apierrors.IsNotFound(err) {
				return out, nil
			}
			return out, err
		}
		if !controlledBy(lastSet, "kubeflow.org", "Notebook", i.Spec.NotebookName, out.NotebookUID) {
			return out, biz.ErrOwnership
		}
		if lastSet.GetUID() != set.GetUID() || lastSet.GetResourceVersion() != set.GetResourceVersion() || lastSet.GetGeneration() != set.GetGeneration() {
			return out, nil
		}
	}
	out.SafeStopped = true
	return out, nil
}

func identityPod(out *biz.Observation, i biz.Instance, pod *unstructured.Unstructured) {
	out.PodExists = true // terminating still exists
	ref := biz.PodRef{ClusterID: i.Spec.ClusterID, Namespace: pod.GetNamespace(), Name: pod.GetName(), UID: string(pod.GetUID()), ContainerName: biz.Container}
	out.Pods = biz.MergePodRefs(out.Pods, []biz.PodRef{ref})
	if i.Spec.ComputeType == biz.TypeGPU {
		out.GPUPods = biz.MergePodRefs(out.GPUPods, []biz.PodRef{ref})
	}
	if pod.GetDeletionTimestamp() == nil && out.PodUID == "" {
		out.PodName, out.PodUID = pod.GetName(), string(pod.GetUID())
	}
}

// VerifyRuntime always reads a fresh identity chain and then the exact Pod UID.
// GPU signing remains bound to this UID; previous readiness is never reused.
func (n *Notebook) VerifyRuntime(ctx context.Context, i biz.Instance, previous biz.Observation) (biz.Observation, error) {
	out, err := n.ObserveIdentity(ctx, i)
	if err != nil || !out.NotebookExists || out.Stopped || out.PodUID == "" {
		return out, err
	}
	if previous.PodUID != "" && previous.PodUID != out.PodUID {
		return out, biz.ErrVersion
	}
	if len(out.Pods) != 1 {
		return out, nil
	}
	if err := n.verifyBorrowedVolumes(ctx, i); err != nil {
		return out, err
	}
	pod, err := n.lifecycleGet(ctx, podGVR, i.Spec.Namespace, out.PodName)
	if err != nil {
		return out, err
	}
	if string(pod.GetUID()) != out.PodUID || !owned(pod, i, out.PodUID) || !controlledBy(pod, "apps", "StatefulSet", i.Spec.NotebookName, out.StatefulSetUID) {
		return out, biz.ErrOwnership
	}
	if err := verifyPod(pod, i); err != nil {
		if errors.Is(err, biz.ErrOwnership) {
			// Identity is already proven. Configuration drift is a runtime
			// rejection and must not disable the safe stopping path.
			return out, biz.ErrState
		}
		return out, err
	}
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = n.verifyPodGroupPriority(call, pod, i)
	cancel()
	if err != nil {
		return out, err
	}
	if i.Spec.ComputeType == biz.TypeGPU {
		if len(n.signingKey) == 0 {
			return out, biz.ErrState
		}
		call, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = n.signPod(call, pod, i)
		cancel()
		if err != nil {
			return out, err
		}
	}
	out.PodReady = runtimePodReady(pod)
	return out, nil
}

func runtimePodReady(pod *unstructured.Unstructured) bool {
	if pod.GetDeletionTimestamp() != nil {
		return false
	}
	phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
	if phase != "Running" {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(pod.Object, "status", "conditions")
	ready := false
	for _, value := range conditions {
		condition, _ := value.(map[string]any)
		ready = ready || condition["type"] == "Ready" && condition["status"] == "True"
	}
	statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
	for _, value := range statuses {
		status, _ := value.(map[string]any)
		if ready && status["name"] == biz.Container && status["ready"] == true {
			return true
		}
	}
	return false
}

func (n *Notebook) ApplyMutation(ctx context.Context, i biz.Instance, m biz.Mutation) error {
	if m.Kind != "set_stopped" || m.StoppedTarget == nil {
		return biz.ErrInvalid
	}
	if !*m.StoppedTarget {
		if err := n.verifyBorrowedVolumes(ctx, i); err != nil {
			return err
		}
	}
	return n.updateNotebookMutation(ctx, i, m, nil)
}

func (n *Notebook) UpdateMounts(ctx context.Context, i biz.Instance, m biz.Mutation, target []biz.VolumeMount) error {
	if m.Kind != "update_mounts" || m.TargetMountDigest != biz.MountDigest(target) {
		return biz.ErrInvalid
	}
	if err := n.VerifyAddedMounts(ctx, i, target); err != nil {
		return err
	}
	identity, err := n.ObserveIdentity(ctx, i)
	if err != nil {
		return err
	}
	if !identity.SafeStopped {
		return biz.ErrVersion
	}
	return n.updateNotebookMutation(ctx, i, m, &target)
}

// VerifyAddedMounts rechecks only newly introduced frozen identities. The
// worker can invoke it before the first stop of a running Attach, so a replaced
// target PVC is rejected before interrupting the original workload. It never
// resolves a replacement UID or requires remaining Detach targets to exist.
func (n *Notebook) VerifyAddedMounts(ctx context.Context, i biz.Instance, target []biz.VolumeMount) error {
	if !i.MountsInitialized {
		return biz.ErrState
	}
	// Detach must remain possible when any remaining frozen PVC is missing.
	// Only newly introduced identities acquire fresh pre-write eligibility.
	added := []biz.VolumeMount{}
	for _, mount := range target {
		present := false
		for _, current := range i.CurrentMounts {
			if mount == current {
				present = true
				break
			}
		}
		if !present {
			added = append(added, mount)
		}
	}
	return n.verifyBorrowedMounts(ctx, i, added)
}

func (n *Notebook) updateNotebookMutation(ctx context.Context, i biz.Instance, m biz.Mutation, target *[]biz.VolumeMount) error {
	if m.ID == "" || m.OperationID == "" || m.Version <= 0 || m.TargetUID == "" || m.ResourceVersion == "" || m.Phase == "" || m.PhaseSequence <= 0 {
		return biz.ErrInvalid
	}
	obj, err := n.lifecycleGet(ctx, notebookGVR, i.Spec.Namespace, i.Spec.NotebookName)
	if err != nil {
		return err
	}
	if !owned(obj, i, m.TargetUID) {
		return biz.ErrOwnership
	}
	if obj.GetResourceVersion() != m.ResourceVersion {
		return biz.ErrVersion
	}
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[versionAnnotation] = strconv.FormatInt(m.Version, 10)
	ann[mutationAnnotation] = m.ID
	if target != nil {
		if _, stopped := ann[stoppedAnnotation]; !stopped {
			return biz.ErrInvalid
		}
		if err := replaceManagedMounts(obj, i.Spec, *target); err != nil {
			return err
		}
		ann[stoppedAnnotation] = "true"
	} else if *m.StoppedTarget {
		ann[stoppedAnnotation] = "true"
	} else {
		delete(ann, stoppedAnnotation)
	}
	obj.SetAnnotations(ann)
	call, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err = n.client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Update(call, obj, metav1.UpdateOptions{})
	return mutationError(err)
}

// Template proof compares content on both controller surfaces. Initial
// running instances may establish a baseline without being Ready or having
// live PVCs; SafeStopped separately gates stopped template commits.
func (n *Notebook) VerifyMountTemplate(ctx context.Context, i biz.Instance, before, target []biz.VolumeMount) (biz.MountTemplateProof, error) {
	proof := biz.MountTemplateProof{Class: "mixed"}
	identity, err := n.ObserveIdentity(ctx, i)
	if err != nil {
		return proof, err
	}
	proof.NotebookUID, proof.NotebookRV = identity.NotebookUID, identity.NotebookRV
	proof.StatefulSetUID = identity.StatefulSetUID
	proof.NotebookGeneration = identity.NotebookGeneration
	proof.StatefulSetGeneration, proof.StatefulSetObservedGeneration = identity.StatefulSetGeneration, identity.StatefulSetObservedGeneration
	proof.SafeStopped = identity.SafeStopped
	if !identity.NotebookExists {
		return proof, nil
	}
	notebook, err := n.lifecycleGet(ctx, notebookGVR, i.Spec.Namespace, i.Spec.NotebookName)
	if err != nil {
		return proof, err
	}
	if !owned(notebook, i, identity.NotebookUID) || notebook.GetResourceVersion() != identity.NotebookRV {
		return proof, biz.ErrVersion
	}
	notebookSpec, present, err := unstructured.NestedMap(notebook.Object, "spec", "template", "spec")
	if err != nil || !present {
		return proof, biz.ErrOwnership
	}
	class := mountContentClass(notebookSpec, i.Spec, before, target)
	set, err := n.lifecycleGet(ctx, stsGVR, i.Spec.Namespace, i.Spec.NotebookName)
	if apierrors.IsNotFound(err) {
		if identity.StatefulSetUID == "" && identity.SafeStopped {
			proof.Class = class
		}
		return proof, nil
	}
	if err != nil {
		return proof, err
	}
	if string(set.GetUID()) != identity.StatefulSetUID || !controlledBy(set, "kubeflow.org", "Notebook", i.Spec.NotebookName, identity.NotebookUID) {
		return proof, biz.ErrOwnership
	}
	setSpec, present, err := unstructured.NestedMap(set.Object, "spec", "template", "spec")
	if err != nil || !present {
		return proof, biz.ErrOwnership
	}
	observed, present, err := unstructured.NestedInt64(set.Object, "status", "observedGeneration")
	if err != nil || !present || observed < set.GetGeneration() || set.GetGeneration() != identity.StatefulSetGeneration || observed != identity.StatefulSetObservedGeneration {
		return proof, nil
	}
	if class != "mixed" && class == mountContentClass(setSpec, i.Spec, before, target) {
		lastNotebook, err := n.lifecycleGet(ctx, notebookGVR, i.Spec.Namespace, i.Spec.NotebookName)
		if err != nil {
			return proof, err
		}
		lastSet, err := n.lifecycleGet(ctx, stsGVR, i.Spec.Namespace, i.Spec.NotebookName)
		if err != nil {
			return proof, err
		}
		if !owned(lastNotebook, i, identity.NotebookUID) || !controlledBy(lastSet, "kubeflow.org", "Notebook", i.Spec.NotebookName, identity.NotebookUID) {
			return proof, biz.ErrOwnership
		}
		if lastNotebook.GetResourceVersion() == notebook.GetResourceVersion() && lastNotebook.GetGeneration() == notebook.GetGeneration() && lastSet.GetUID() == set.GetUID() && lastSet.GetResourceVersion() == set.GetResourceVersion() && lastSet.GetGeneration() == set.GetGeneration() {
			proof.Class = class
		} else {
			proof.SafeStopped = false
		}
	}
	return proof, nil
}

func mountContentClass(podSpec map[string]any, spec biz.Spec, before, target []biz.VolumeMount) string {
	pod := &unstructured.Unstructured{Object: map[string]any{"spec": podSpec}}
	containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
	if len(containers) != 1 {
		return "mixed"
	}
	container, ok := containers[0].(map[string]any)
	if !ok || container["name"] != biz.Container {
		return "mixed"
	}
	spec.VolumeMounts = target
	if verifyFrozenMounts(pod, container, spec) == nil {
		return "target"
	}
	spec.VolumeMounts = before
	if verifyFrozenMounts(pod, container, spec) == nil {
		return "old"
	}
	return "mixed"
}

var _ biz.LifecycleProvider = (*Notebook)(nil)
