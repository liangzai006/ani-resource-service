package instance

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
)

func TestCPUNotebookProjectionAndPodBinding(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	i := biz.Instance{TenantID: id, ID: id, CreateOperationID: id, CreateRequestHash: strings.Repeat("a", 64), BusinessDigest: strings.Repeat("b", 64), Version: 1, Desired: biz.Running, RuntimeDesired: biz.Running, MountsInitialized: true, CurrentMounts: []biz.VolumeMount{}, Spec: biz.Spec{ContractVersion: 2, WorkspaceMountPath: "/workspace", InstanceKind: biz.KindContainer, Scheduling: biz.Scheduling{PodPlacement: "NONE"}, ComputeType: biz.TypeCPU, ImageReference: "registry.invalid/work@sha256:" + strings.Repeat("c", 64), ImageDigest: "sha256:" + strings.Repeat("c", 64), CPU: "1", Memory: "2Gi", UID: 1000, GID: 1000, Shell: "/bin/bash", ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", ClusterID: id, Namespace: "tenant-" + id, NotebookName: "instance-" + id, PVCName: "workspace-" + id}}
	notebook, err := RenderNotebook(i)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"schedulerName", "runtimeClassName", "nodeSelector"} {
		if _, exists, _ := unstructured.NestedFieldNoCopy(notebook.Object, "spec", "template", "spec", field); exists {
			t.Fatal("CPU instance received GPU scheduling configuration", field)
		}
	}
	for key := range notebook.GetAnnotations() {
		if key != versionAnnotation {
			t.Fatal("CPU instance received GPU annotations", key)
		}
	}
	notebook.SetUID("notebook-original")
	i.NotebookUID = string(notebook.GetUID())
	template, _, _ := unstructured.NestedMap(notebook.Object, "spec", "template")
	pod := &unstructured.Unstructured{Object: template}
	pod.SetAPIVersion("v1")
	pod.SetKind("Pod")
	pod.SetName(i.Spec.NotebookName + "-0")
	pod.SetNamespace(i.Spec.Namespace)
	pod.SetUID("pod-original")
	pod.SetLabels(notebook.GetLabels())
	pod.SetAnnotations(notebook.GetAnnotations())
	// The Kubernetes API defaults this field even though the Notebook omits it.
	_ = unstructured.SetNestedField(pod.Object, "default-scheduler", "spec", "schedulerName")
	if err := verifyPod(pod, i); err != nil {
		t.Fatal("defaulted CPU Pod rejected", err)
	}
	mutations := map[string]func(*unstructured.Unstructured){
		"GPU annotation": func(p *unstructured.Unstructured) {
			a := p.GetAnnotations()
			a["accelerator.ani.io/execution-digest"] = "foreign"
			p.SetAnnotations(a)
		},
		"GPU scheduler": func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, "volcano", "spec", "schedulerName")
		},
		"GPU selector": func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedStringMap(p.Object, map[string]string{"gpu": "true"}, "spec", "nodeSelector")
		},
		"runtime class": func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, "nvidia", "spec", "runtimeClassName")
		},
		"different user": func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, int64(0), "spec", "securityContext", "runAsUser")
		},
		"different working directory": func(p *unstructured.Unstructured) {
			cs, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
			cs[0].(map[string]any)["workingDir"] = "/"
			_ = unstructured.SetNestedSlice(p.Object, cs, "spec", "containers")
		},
		"different CPU limit": func(p *unstructured.Unstructured) {
			cs, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
			_ = unstructured.SetNestedField(cs[0].(map[string]any), "2", "resources", "limits", "cpu")
			_ = unstructured.SetNestedSlice(p.Object, cs, "spec", "containers")
		},
		"additional GPU limit": func(p *unstructured.Unstructured) {
			cs, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
			_ = unstructured.SetNestedField(cs[0].(map[string]any), "1", "resources", "limits", "nvidia.com/gpu")
			_ = unstructured.SetNestedSlice(p.Object, cs, "spec", "containers")
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			mutate(changed)
			if err := verifyPod(changed, i); !errors.Is(err, biz.ErrOwnership) {
				t.Fatal("changed CPU target accepted", err)
			}
		})
	}
	controller := true
	set := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": i.Spec.NotebookName, "namespace": i.Spec.Namespace, "uid": "set-original"}}}
	set.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubeflow.org/v1", Kind: "Notebook", Name: i.Spec.NotebookName, UID: notebook.GetUID(), Controller: &controller}})
	pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: i.Spec.NotebookName, UID: set.GetUID(), Controller: &controller}})
	pod.Object["status"] = map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": biz.Container, "ready": true}}, "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{podGVR: "PodList"}, notebook, set, pod)
	provider, _ := NewNotebook(client)
	if err = provider.ConfigureSigningKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))); err != nil {
		t.Fatal(err)
	}
	obs, err := provider.Observe(context.Background(), i)
	if err != nil || !obs.PodReady || obs.PodUID != "pod-original" || obs.StatefulSetUID != "set-original" {
		t.Fatal("managed CPU target not observed", obs, err)
	}
	current, err := client.Resource(podGVR).Namespace(i.Spec.Namespace).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
	if err != nil || current.GetAnnotations()["accelerator.ani.io/owner-signature"] != "" {
		t.Fatal("CPU Pod received an Acc signature", err)
	}
}

func TestNotebookMappingAndConditionalStop(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	i := biz.Instance{TenantID: id, ID: id, CreateOperationID: id, CreateRequestHash: strings.Repeat("a", 64), BusinessDigest: strings.Repeat("b", 64), Version: 1, Desired: biz.Running, RuntimeDesired: biz.Running, MountsInitialized: true, CurrentMounts: []biz.VolumeMount{}, Spec: biz.Spec{ContractVersion: 2, WorkspaceMountPath: "/workspace", InstanceKind: biz.KindContainer, ComputeType: biz.TypeGPU, Scheduling: biz.Scheduling{PodPlacement: "NONE"}, Name: "work", ImageReference: "registry.invalid/work@sha256:" + strings.Repeat("c", 64), ImageDigest: "sha256:" + strings.Repeat("c", 64), CPU: "1", Memory: "2Gi", UID: 1000, GID: 1000, Shell: "/bin/sh", ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", ClusterID: id, Namespace: "tenant-" + id, NotebookName: "instance-" + id, PVCName: "workspace-" + id, GPU: biz.GPU{Scheduler: "volcano", RuntimeClass: "nvidia", Queue: "test-queue", Digest: strings.Repeat("d", 64), NodeLabels: map[string]string{"accelerator.ani.io/supply-group": id}, Annotations: map[string]string{"volcano.sh/vgpu-mode": "hami-core"}, Limits: map[string]string{"volcano.sh/vgpu-number": "1", "volcano.sh/vgpu-memory": "6144", "volcano.sh/vgpu-cores": "25"}}}}
	obj, e := RenderNotebook(i)
	if e != nil {
		t.Fatal(e)
	}
	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if len(containers) != 1 {
		t.Fatal("extra containers")
	}
	c := containers[0].(map[string]any)
	gpu, _, _ := unstructured.NestedStringMap(c, "resources", "limits")
	if gpu["volcano.sh/vgpu-memory"] != "6144" || gpu["volcano.sh/vgpu-number"] != "1" {
		t.Fatal("frozen units were recomputed")
	}
	queue, _, _ := unstructured.NestedString(obj.Object, "metadata", "annotations", "scheduling.volcano.sh/queue-name")
	if queue != "test-queue" {
		t.Fatal("queue not in supported Notebook metadata")
	}
	if _, exists, _ := unstructured.NestedMap(obj.Object, "spec", "template", "metadata"); exists {
		t.Fatal("unsupported template metadata would be pruned")
	}
	obj.SetUID(types.UID("notebook-original"))
	obj.SetResourceVersion("1")
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), obj)
	provider, _ := NewNotebook(client)
	i.NotebookUID = "notebook-original"
	i.Version = 2
	if e = provider.SetStopped(context.Background(), i, biz.Observation{NotebookUID: i.NotebookUID, NotebookRV: "1"}, true); e != nil {
		t.Fatal(e)
	}
	current, e := client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Get(context.Background(), i.Spec.NotebookName, metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if current.GetAnnotations()[stoppedAnnotation] == "" || current.GetAnnotations()[versionAnnotation] != "2" {
		t.Fatal("stop/version not written together")
	}
	// Same-value Stop still records the new business version.
	i.Version = 3
	if e = provider.SetStopped(context.Background(), i, biz.Observation{NotebookUID: i.NotebookUID, NotebookRV: "1"}, true); e != nil {
		t.Fatal(e)
	}
	current, _ = client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Get(context.Background(), i.Spec.NotebookName, metav1.GetOptions{})
	if current.GetAnnotations()[versionAnnotation] != "3" {
		t.Fatal("same-value intent lost version")
	}
	old := i
	old.Version = 2
	if e = provider.SetStopped(context.Background(), old, biz.Observation{NotebookUID: i.NotebookUID, NotebookRV: "1"}, false); !errors.Is(e, biz.ErrVersion) {
		t.Fatal("late Start overwrote current Stop", e)
	}
	i.Version = 4
	if e = provider.SetStopped(context.Background(), i, biz.Observation{NotebookUID: i.NotebookUID, NotebookRV: "1"}, false); e != nil {
		t.Fatal(e)
	}
	current, _ = client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Get(context.Background(), i.Spec.NotebookName, metav1.GetOptions{})
	if _, exists := current.GetAnnotations()[stoppedAnnotation]; exists {
		t.Fatal("Start must remove native annotation")
	}
	template, _, _ := unstructured.NestedMap(current.Object, "spec", "template")
	pod := &unstructured.Unstructured{Object: template}
	pod.SetAPIVersion("v1")
	pod.SetKind("Pod")
	pod.SetNamespace(i.Spec.Namespace)
	pod.SetName("notebook-0")
	pod.SetUID(types.UID("pod-original"))
	pod.SetLabels(current.GetLabels())
	pod.SetAnnotations(current.GetAnnotations())
	controller := true
	set := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": i.Spec.NotebookName, "namespace": i.Spec.Namespace, "uid": "set-original"}}}
	set.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubeflow.org/v1beta1", Kind: "Notebook", Name: i.Spec.NotebookName, UID: types.UID(i.NotebookUID), Controller: &controller}})
	pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: i.Spec.NotebookName, UID: set.GetUID(), Controller: &controller}})
	pod.Object["status"] = map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": biz.Container, "ready": true}}, "conditions": []any{map[string]any{"type": "Ready", "status": "False"}}}
	observations := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{podGVR: "PodList", stsGVR: "StatefulSetList"}, current, pod, set)
	observer, _ := NewNotebook(observations)
	signing := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	if e = observer.ConfigureSigningKey(signing); e != nil {
		t.Fatal(e)
	}
	obs, e := observer.Observe(context.Background(), i)
	if e != nil || obs.PodReady {
		t.Fatal("container-only readiness exposed terminal", e)
	}
	pod.Object["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Ready", "status": "True"}}
	if _, e = observations.Resource(podGVR).Namespace(i.Spec.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); e != nil {
		t.Fatal(e)
	}
	obs, e = observer.Observe(context.Background(), i)
	if e != nil || !obs.PodReady {
		t.Fatal("ready managed Pod rejected", e)
	}
	signed, e := observations.Resource(podGVR).Namespace(i.Spec.Namespace).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	signature, e := base64.StdEncoding.DecodeString(signed.GetAnnotations()["accelerator.ani.io/owner-signature"])
	if e != nil {
		t.Fatal(e)
	}
	assertion := map[string]any{"pod": map[string]string{"cluster_id": i.Spec.ClusterID, "namespace": signed.GetNamespace(), "name": signed.GetName(), "uid": string(signed.GetUID()), "container_name": biz.Container}, "ref": map[string]string{"tenant_id": i.TenantID, "owner_service": biz.Owner, "resource_id": i.ID, "create_operation_id": i.CreateOperationID}, "execution_digest": i.Spec.GPU.Digest}
	payload, _ := json.Marshal(assertion)
	if !ed25519.Verify(signing.Public().(ed25519.PublicKey), append([]byte("acc-owner-pod-v2\n"), payload...), signature) {
		t.Fatal("owner signature does not bind new execution assertion")
	}
	if ed25519.Verify(signing.Public().(ed25519.PublicKey), append([]byte("acc-owner-pod-v1\n"), payload...), signature) {
		t.Fatal("old signature domain accepted")
	}
	assertion["pod"].(map[string]string)["uid"] = "foreign-pod"
	payload, _ = json.Marshal(assertion)
	if ed25519.Verify(signing.Public().(ed25519.PublicKey), append([]byte("acc-owner-pod-v2\n"), payload...), signature) {
		t.Fatal("signature did not bind actual Pod UID")
	}
	badDigest := pod.DeepCopy()
	annotations := badDigest.GetAnnotations()
	annotations["accelerator.ani.io/execution-digest"] = "forged"
	badDigest.SetAnnotations(annotations)
	if e = verifyPod(badDigest, i); !errors.Is(e, biz.ErrOwnership) {
		t.Fatal("forged execution digest accepted", e)
	}
	broken := pod.DeepCopy()
	containers, _, _ = unstructured.NestedSlice(broken.Object, "spec", "containers")
	containers[0].(map[string]any)["env"] = []any{map[string]any{"name": "ANI_INSTANCE_POD_UID", "value": "pod-original"}}
	_ = unstructured.SetNestedSlice(broken.Object, containers, "spec", "containers")
	if e = verifyPod(broken, i); !errors.Is(e, biz.ErrOwnership) {
		t.Fatal("literal UID bypassed exec guard", e)
	}
	broken = pod.DeepCopy()
	volumes, _, _ := unstructured.NestedSlice(broken.Object, "spec", "volumes")
	volumes[0].(map[string]any)["persistentVolumeClaim"] = map[string]any{"claimName": "shared-unowned-disk"}
	_ = unstructured.SetNestedSlice(broken.Object, volumes, "spec", "volumes")
	if e = verifyPod(broken, i); !errors.Is(e, biz.ErrOwnership) {
		t.Fatal("foreign workspace exposed as instance", e)
	}
	i.StatefulSetUID = obs.StatefulSetUID
	_ = observations.Resource(notebookGVR).Namespace(i.Spec.Namespace).Delete(context.Background(), i.Spec.NotebookName, metav1.DeleteOptions{})
	_ = observations.Resource(podGVR).Namespace(i.Spec.Namespace).Delete(context.Background(), pod.GetName(), metav1.DeleteOptions{})
	obs, e = observer.Observe(context.Background(), i)
	if e != nil || !obs.StatefulSetExists || obs.StatefulSetUID != "set-original" {
		t.Fatal("unlabelled controller StatefulSet allowed premature compute cleanup", obs, e)
	}
	refs := set.GetOwnerReferences()
	refs[0].UID = "foreign-notebook"
	set.SetOwnerReferences(refs)
	_, _ = observations.Resource(stsGVR).Namespace(i.Spec.Namespace).Update(context.Background(), set, metav1.UpdateOptions{})
	if _, e = observer.Observe(context.Background(), i); !errors.Is(e, biz.ErrOwnership) {
		t.Fatal("foreign StatefulSet owner accepted", e)
	}
}

func TestNotebookWorkspaceReclaimOwnership(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	i := biz.Instance{TenantID: tenant, ID: tenant, CreateOperationID: tenant, PVCUID: "claim-original", Spec: biz.Spec{Namespace: "tenant-" + tenant, PVCName: "workspace-original"}}
	pvc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": i.Spec.PVCName, "namespace": i.Spec.Namespace, "uid": i.PVCUID, "labels": stringMap(labels(i))}, "spec": map[string]any{"volumeName": "volume-original"}}}
	pv := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": "volume-original", "uid": "volume-uid"}, "spec": map[string]any{"persistentVolumeReclaimPolicy": "Delete", "claimRef": map[string]any{"uid": i.PVCUID, "name": i.Spec.PVCName, "namespace": i.Spec.Namespace}}}}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{pvGVR: "PersistentVolumeList"}, pvc, pv)
	provider, _ := NewNotebook(client)
	var out biz.Observation
	provider.observeWorkspace(context.Background(), i, &out)
	if out.WorkspaceReason != "" || !out.VolumeExists || out.VolumeUID != "volume-uid" {
		t.Fatal("binding not observed", out)
	}
	i.VolumeName = out.VolumeName
	i.VolumeUID = out.VolumeUID
	if e := client.Resource(pvcGVR).Namespace(i.Spec.Namespace).Delete(context.Background(), i.Spec.PVCName, metav1.DeleteOptions{}); e != nil {
		t.Fatal(e)
	}
	out = biz.Observation{}
	provider.observeWorkspace(context.Background(), i, &out)
	if out.PVCExists || !out.VolumeExists || out.WorkspaceReason != "" {
		t.Fatal("PVC absence hid remaining volume", out)
	}
	pv.SetUID(types.UID("replacement-volume"))
	if _, e := client.Resource(pvGVR).Update(context.Background(), pv, metav1.UpdateOptions{}); e != nil {
		t.Fatal(e)
	}
	out = biz.Observation{}
	provider.observeWorkspace(context.Background(), i, &out)
	if out.WorkspaceReason != "WORKSPACE_OWNERSHIP_MISMATCH" {
		t.Fatal("replacement volume accepted", out)
	}
	pv.SetUID(types.UID("volume-uid"))
	_ = unstructured.SetNestedField(pv.Object, "Retain", "spec", "persistentVolumeReclaimPolicy")
	_, _ = client.Resource(pvGVR).Update(context.Background(), pv, metav1.UpdateOptions{})
	out = biz.Observation{}
	provider.observeWorkspace(context.Background(), i, &out)
	if out.WorkspaceReason != "WORKSPACE_RECLAIM_POLICY_MISMATCH" {
		t.Fatal("Retain treated as automatic cleanup", out)
	}
	if e := client.Resource(pvGVR).Delete(context.Background(), pv.GetName(), metav1.DeleteOptions{}); e != nil {
		t.Fatal(e)
	}
	out = biz.Observation{}
	provider.observeWorkspace(context.Background(), i, &out)
	if out.VolumeExists || out.WorkspaceReason != "" {
		t.Fatal("confirmed reclaimed volume not cleared", out)
	}
	// Missing saved binding is recovered by immutable PVC UID, never by name.
	i.VolumeName = ""
	i.VolumeUID = ""
	_ = unstructured.SetNestedField(pv.Object, "Delete", "spec", "persistentVolumeReclaimPolicy")
	if _, e := client.Resource(pvGVR).Create(context.Background(), pv, metav1.CreateOptions{}); e != nil {
		t.Fatal(e)
	}
	out = biz.Observation{}
	provider.observeWorkspace(context.Background(), i, &out)
	if !out.VolumeExists || out.VolumeUID != "volume-uid" {
		t.Fatal("claim UID recovery lost volume", out)
	}
}
