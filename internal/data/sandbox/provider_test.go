package sandbox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	storagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func providerFixture(t *testing.T) (*fake.FakeDynamicClient, *KubernetesProvider, biz.Sandbox) {
	t.Helper()
	sc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": map[string]any{"name": "fast", "uid": "storage-class-uid"}, "provisioner": "test.csi", "reclaimPolicy": "Delete"}}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{pvGVR: "PersistentVolumeList"}, sc)
	var sequence int
	client.PrependReactor("create", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		sequence++
		object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		if object.GetUID() == "" {
			object.SetUID(types.UID(fmt.Sprintf("provider-uid-%d", sequence)))
		}
		object.SetResourceVersion(fmt.Sprint(sequence))
		return false, nil, nil
	})
	const clusterID = "44444444-4444-4444-8444-444444444444"
	provider, err := NewKubernetesProvider(client, clusterID)
	if err != nil {
		t.Fatal(err)
	}
	s := biz.Sandbox{ID: "11111111-1111-4111-8111-111111111111", TenantID: "22222222-2222-4222-8222-222222222222", Version: 1, Desired: biz.DesiredRunning, LastOperationID: "33333333-3333-4333-8333-333333333333",
		Snapshot: biz.Snapshot{ClusterID: clusterID, Namespace: "sandbox-ns", RuntimeClass: "sandbox-kata", ImageReference: "registry.example/sandbox@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CPUMillis: 500, MemoryMiB: 512, Workspace: biz.WorkspaceSpec{MountPath: "/workspace", Size: "10Gi", StorageClass: "fast", ReclaimPolicy: "Delete"}}}
	return client, provider, s
}

func providerMutation(s biz.Sandbox, kind string) biz.Mutation {
	m := biz.Mutation{ID: "mutation-" + kind, Kind: kind, OperationID: s.LastOperationID, Version: s.Version}
	if kind == biz.MutationDeletePod {
		m.TargetUID = s.ProviderRefs.PodUID
	}
	if kind == biz.MutationDeleteWorkspace {
		m.TargetUID = s.ProviderRefs.PVCUID
	}
	return m
}

func applyProvider(t *testing.T, provider *KubernetesProvider, s *biz.Sandbox, kind string) biz.MutationResult {
	t.Helper()
	result, err := provider.Apply(context.Background(), *s, providerMutation(*s, kind))
	if err != nil || result.Outcome != biz.MutationApplied {
		t.Fatalf("apply %s: result=%#v err=%v", kind, result, err)
	}
	s.ProviderRefs = result.Refs
	return result
}

func providerBoundWorkspace(t *testing.T, client *fake.FakeDynamicClient, s biz.Sandbox) {
	t.Helper()
	pvc, err := client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), "workspace-"+s.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	name := "pv-" + s.ID
	pv := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": name, "uid": "original-pv-uid"}, "spec": map[string]any{"persistentVolumeReclaimPolicy": "Delete", "claimRef": map[string]any{"namespace": s.Snapshot.Namespace, "name": pvc.GetName(), "uid": string(pvc.GetUID())}}, "status": map[string]any{"phase": "Bound"}}}
	if _, err := client.Resource(pvGVR).Create(context.Background(), pv, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(pvc.Object, name, "spec", "volumeName")
	_ = unstructured.SetNestedField(pvc.Object, "Bound", "status", "phase")
	if _, err := client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func providerPodStatus(t *testing.T, client *fake.FakeDynamicClient, s biz.Sandbox, phase, condition string, containerReady bool) {
	t.Helper()
	pod, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), "sandbox-"+s.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Object["status"] = map[string]any{"phase": phase, "conditions": []any{map[string]any{"type": "Ready", "status": condition}}, "containerStatuses": []any{map[string]any{"name": "sandbox", "ready": containerReady}}}
	if _, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func observeProvider(t *testing.T, provider *KubernetesProvider, s *biz.Sandbox) biz.Observation {
	t.Helper()
	obs, err := provider.Observe(context.Background(), *s)
	if err != nil {
		t.Fatal(err)
	}
	s.ProviderRefs = obs.ProviderRefs
	return obs
}

func providerWriteCount(client *fake.FakeDynamicClient) int {
	count := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" || action.GetVerb() == "delete" {
			count++
		}
	}
	return count
}

func TestKubernetesProviderKeepsWorkspaceAcrossPause(t *testing.T) {
	client, provider, s := providerFixture(t)
	before := providerWriteCount(client)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	if providerWriteCount(client)-before != 1 {
		t.Fatal("workspace mutation wrote more than one child")
	}
	if _, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), "sandbox-"+s.ID, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("workspace mutation also created a Pod: %v", err)
	}
	originalPVC := s.ProviderRefs.PVCUID
	providerBoundWorkspace(t, client, s)
	applyProvider(t, provider, &s, biz.MutationCreatePod)
	originalPod := s.ProviderRefs.PodUID
	providerPodStatus(t, client, s, "Running", "True", true)
	if obs := observeProvider(t, provider, &s); obs.Observed != biz.ObservedRunning || !obs.WorkspaceBound || obs.ProviderRefs.PVUID != "original-pv-uid" {
		t.Fatalf("not running with bound PV: %#v", obs)
	}
	s.Desired = biz.DesiredPaused
	before = providerWriteCount(client)
	applyProvider(t, provider, &s, biz.MutationDeletePod)
	if providerWriteCount(client)-before != 1 {
		t.Fatal("pause mutation touched more than its Pod")
	}
	if obs := observeProvider(t, provider, &s); obs.Observed != biz.ObservedPaused || obs.ProviderRefs.PVCUID != originalPVC || obs.ProviderRefs.PodUID != "" {
		t.Fatalf("pause did not preserve original workspace: %#v", obs)
	}
	s.Desired = biz.DesiredRunning
	applyProvider(t, provider, &s, biz.MutationCreatePod)
	if s.ProviderRefs.PodUID == originalPod || s.ProviderRefs.PVCUID != originalPVC {
		t.Fatalf("resume replaced workspace or reused deleted Pod: %#v", s.ProviderRefs)
	}
	providerPodStatus(t, client, s, "Running", "True", true)
	if obs := observeProvider(t, provider, &s); obs.Observed != biz.ObservedRunning {
		t.Fatalf("resume did not converge: %#v", obs)
	}
	s.Desired = biz.DesiredDeleted
	applyProvider(t, provider, &s, biz.MutationDeletePod)
	observeProvider(t, provider, &s)
	applyProvider(t, provider, &s, biz.MutationDeleteWorkspace)
	if obs := observeProvider(t, provider, &s); obs.Observed != biz.ObservedDeleting || !obs.VolumeExists || obs.ProviderRefs.CleanupReceipt != "" {
		t.Fatalf("declared reclaimed before PV disappearance: %#v", obs)
	}
	if err := client.Resource(pvGVR).Delete(context.Background(), s.ProviderRefs.PVName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if obs := observeProvider(t, provider, &s); obs.Observed != biz.ObservedDeleted || obs.ProviderRefs.CleanupReceipt == "" {
		t.Fatalf("missing completed reclaim receipt: %#v", obs)
	}
}

func TestKubernetesProviderNeverReplacesMissingWorkspace(t *testing.T) {
	client, provider, s := providerFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	original := s.ProviderRefs.PVCUID
	if err := client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Delete(context.Background(), "workspace-"+s.ID, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	obs := observeProvider(t, provider, &s)
	if obs.Observed != biz.ObservedUnknown || obs.Reason != "WORKSPACE_MISSING" || obs.ProviderRefs.PVCUID != original {
		t.Fatalf("missing workspace was forgotten: %#v", obs)
	}
	before := providerWriteCount(client)
	result, err := provider.Apply(context.Background(), s, providerMutation(s, biz.MutationCreateWorkspace))
	if err == nil || result.Outcome != biz.MutationNotSent || result.Reason != "WORKSPACE_MISSING" || providerWriteCount(client) != before {
		t.Fatalf("missing original workspace was recreated: result=%#v err=%v", result, err)
	}
}

func TestKubernetesProviderRequiresReadyPodAndContainer(t *testing.T) {
	for _, tc := range []struct {
		name, phase, condition                string
		containerReady, deleting, wantRunning bool
	}{
		{"ready", "Running", "True", true, false, true}, {"condition_false", "Running", "False", true, false, false}, {"container_unready", "Running", "True", false, false, false}, {"pending", "Pending", "True", true, false, false}, {"deleting", "Running", "True", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, provider, s := providerFixture(t)
			applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
			providerBoundWorkspace(t, client, s)
			applyProvider(t, provider, &s, biz.MutationCreatePod)
			providerPodStatus(t, client, s, tc.phase, tc.condition, tc.containerReady)
			if tc.deleting {
				pod, _ := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), "sandbox-"+s.ID, metav1.GetOptions{})
				now := metav1.Now()
				pod.SetDeletionTimestamp(&now)
				if _, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if obs := observeProvider(t, provider, &s); (obs.Observed == biz.ObservedRunning) != tc.wantRunning {
				t.Fatalf("readiness mismatch: %#v", obs)
			}
		})
	}
}

func TestKubernetesProviderRejectsRetainStorageClassBeforeCreate(t *testing.T) {
	client, provider, s := providerFixture(t)
	sc, _ := client.Resource(storageClassGVR).Get(context.Background(), "fast", metav1.GetOptions{})
	sc.Object["reclaimPolicy"] = "Retain"
	if _, err := client.Resource(storageClassGVR).Update(context.Background(), sc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	before := providerWriteCount(client)
	result, err := provider.Apply(context.Background(), s, providerMutation(s, biz.MutationCreateWorkspace))
	if err == nil || result.Outcome != biz.MutationNotSent || result.Reason != "WORKSPACE_RECLAIM_POLICY_MISMATCH" || providerWriteCount(client) != before {
		t.Fatalf("Retain workspace was provisioned: %#v %v", result, err)
	}
}

func TestKubernetesProviderWaitsForOriginalVolumeReclaim(t *testing.T) {
	for _, policy := range []string{"Delete", "Retain"} {
		t.Run(policy, func(t *testing.T) {
			client, provider, s := providerFixture(t)
			applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
			providerBoundWorkspace(t, client, s)
			obs := observeProvider(t, provider, &s)
			pv, _ := client.Resource(pvGVR).Get(context.Background(), obs.ProviderRefs.PVName, metav1.GetOptions{})
			_ = unstructured.SetNestedField(pv.Object, policy, "spec", "persistentVolumeReclaimPolicy")
			if _, err := client.Resource(pvGVR).Update(context.Background(), pv, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			s.Desired = biz.DesiredDeleted
			applyProvider(t, provider, &s, biz.MutationDeleteWorkspace)
			if obs = observeProvider(t, provider, &s); obs.Observed != biz.ObservedDeleting || !obs.VolumeExists || obs.ProviderRefs.CleanupReceipt != "" || obs.ProviderRefs.PVUID != "original-pv-uid" {
				t.Fatalf("remaining %s PV was reported reclaimed: %#v", policy, obs)
			}
		})
	}
}

func TestKubernetesProviderUnknownCreateBlocksDeleteUntilResolved(t *testing.T) {
	client, provider, s := providerFixture(t)
	var pending *unstructured.Unstructured
	client.PrependReactor("create", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		pending = action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		pending.SetUID("late-pvc-uid")
		return true, nil, apierrors.NewTimeoutError("write outcome unknown", 0)
	})
	mutation := providerMutation(s, biz.MutationCreateWorkspace)
	result, err := provider.Apply(context.Background(), s, mutation)
	if err == nil || result.Outcome != biz.MutationUnknown {
		t.Fatalf("timeout was not unknown: %#v %v", result, err)
	}
	s.Pending, s.Desired = &mutation, biz.DesiredDeleted
	if obs := observeProvider(t, provider, &s); obs.Observed == biz.ObservedDeleted || obs.ProviderRefs.CleanupReceipt != "" {
		t.Fatalf("unknown create was finalized as deleted: %#v", obs)
	}
	if err := client.Tracker().Create(pvcGVR, pending, s.Snapshot.Namespace); err != nil {
		t.Fatal(err)
	}
	if obs := observeProvider(t, provider, &s); !obs.WorkspaceExists || obs.ProviderRefs.PVCUID != "late-pvc-uid" {
		t.Fatalf("late create was not recovered: %#v", obs)
	}
	// The worker resolves the persisted mutation from the attributable object
	// before issuing this separate cleanup mutation.
	s.Pending = nil
	applyProvider(t, provider, &s, biz.MutationDeleteWorkspace)
	if obs := observeProvider(t, provider, &s); obs.Observed != biz.ObservedDeleted {
		t.Fatalf("late claim not cleaned: %#v", obs)
	}
}

func TestKubernetesProviderClassifiesMutationOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		want    string
	}{
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "workspace", errors.New("forbidden")), biz.MutationRejected}, {"invalid", apierrors.NewBadRequest("invalid request"), biz.MutationRejected}, {"timeout", apierrors.NewTimeoutError("unknown", 0), biz.MutationUnknown}, {"server_error", apierrors.NewInternalError(errors.New("backend failed")), biz.MutationUnknown}, {"canceled_after_dispatch", context.Canceled, biz.MutationUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, provider, s := providerFixture(t)
			client.PrependReactor("create", "persistentvolumeclaims", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, tc.failure })
			result, err := provider.Apply(context.Background(), s, providerMutation(s, biz.MutationCreateWorkspace))
			if err == nil || result.Outcome != tc.want {
				t.Fatalf("outcome=%#v err=%v", result, err)
			}
		})
	}
	client, provider, s := providerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := provider.Apply(ctx, s, providerMutation(s, biz.MutationCreateWorkspace))
	if err == nil || result.Outcome != biz.MutationNotSent || providerWriteCount(client) != 0 {
		t.Fatalf("canceled request was sent: %#v %v", result, err)
	}
}

func TestKubernetesProviderRejectsUIDReplacementAndRuntimeDrift(t *testing.T) {
	for _, field := range []string{"pod_uid", "pvc_uid", "pv_uid", "runtime"} {
		t.Run(field, func(t *testing.T) {
			client, provider, s := providerFixture(t)
			applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
			providerBoundWorkspace(t, client, s)
			applyProvider(t, provider, &s, biz.MutationCreatePod)
			observeProvider(t, provider, &s)
			gvr, name, namespace := podGVR, "sandbox-"+s.ID, s.Snapshot.Namespace
			if field == "pvc_uid" {
				gvr, name = pvcGVR, "workspace-"+s.ID
			}
			if field == "pv_uid" {
				gvr, name, namespace = pvGVR, s.ProviderRefs.PVName, ""
			}
			object, err := client.Resource(gvr).Namespace(namespace).Get(context.Background(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if field == "runtime" {
				_ = unstructured.SetNestedField(object.Object, "foreign-runtime", "spec", "runtimeClassName")
			} else {
				object.SetUID("foreign-uid")
			}
			if _, err := client.Resource(gvr).Namespace(namespace).Update(context.Background(), object, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.Observe(context.Background(), s); !errors.Is(err, biz.ErrOwnership) {
				t.Fatalf("drift accepted: %v", err)
			}
			if field == "pod_uid" || field == "runtime" {
				before := providerWriteCount(client)
				result, err := provider.Apply(context.Background(), s, providerMutation(s, biz.MutationDeletePod))
				if !errors.Is(err, biz.ErrOwnership) || result.Outcome != biz.MutationNotSent || providerWriteCount(client) != before {
					t.Fatalf("foreign Pod deleted: %#v %v", result, err)
				}
			}
		})
	}
}

func TestKubernetesProviderTerminalPodRequiresSeparateDeleteMutation(t *testing.T) {
	client, provider, s := providerFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	applyProvider(t, provider, &s, biz.MutationCreatePod)
	providerPodStatus(t, client, s, "Failed", "False", false)
	if obs := observeProvider(t, provider, &s); !obs.PodTerminated || !obs.PodExists {
		t.Fatalf("terminal Pod not exposed: %#v", obs)
	}
	before := providerWriteCount(client)
	applyProvider(t, provider, &s, biz.MutationDeletePod)
	if providerWriteCount(client)-before != 1 {
		t.Fatal("terminal Pod delete created another child")
	}
	observeProvider(t, provider, &s)
	applyProvider(t, provider, &s, biz.MutationCreatePod)
}

func TestKubernetesProviderDeleteLostAndPendingWorkspaces(t *testing.T) {
	for _, phase := range []string{"Lost", "Pending"} {
		t.Run(phase, func(t *testing.T) {
			client, provider, s := providerFixture(t)
			applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
			pvc, _ := client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), "workspace-"+s.ID, metav1.GetOptions{})
			_ = unstructured.SetNestedField(pvc.Object, phase, "status", "phase")
			if _, err := client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pvc, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			s.Desired = biz.DesiredDeleted
			if obs := observeProvider(t, provider, &s); obs.Observed != biz.ObservedDeleting {
				t.Fatalf("cleanup blocked by %s: %#v", phase, obs)
			}
			applyProvider(t, provider, &s, biz.MutationDeleteWorkspace)
			if obs := observeProvider(t, provider, &s); obs.Observed != biz.ObservedDeleted {
				t.Fatalf("%s cleanup did not finish: %#v", phase, obs)
			}
		})
	}
}

func TestKubernetesProviderCreateRaceAdoptsOnlyMatchingOwnedObject(t *testing.T) {
	for _, matching := range []bool{true, false} {
		t.Run(fmt.Sprint(matching), func(t *testing.T) {
			client, provider, s := providerFixture(t)
			client.PrependReactor("create", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
				object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
				object.SetUID("racing-pvc-uid")
				if !matching {
					object.SetLabels(map[string]string{providerOwnerLabel: "foreign"})
				}
				if err := client.Tracker().Create(pvcGVR, object, s.Snapshot.Namespace); err != nil {
					t.Fatal(err)
				}
				return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, object.GetName())
			})
			result, err := provider.Apply(context.Background(), s, providerMutation(s, biz.MutationCreateWorkspace))
			if matching {
				if err != nil || result.Outcome != biz.MutationApplied || result.Refs.PVCUID != "racing-pvc-uid" {
					t.Fatalf("matching create receipt lost: %#v %v", result, err)
				}
			} else if !errors.Is(err, biz.ErrOwnership) || result.Outcome != biz.MutationRejected {
				t.Fatalf("foreign racing object adopted: %#v %v", result, err)
			}
		})
	}
}

func TestKubernetesProviderDeletePinsUIDAndPreservesRunningWorkspace(t *testing.T) {
	client, provider, s := providerFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	applyProvider(t, provider, &s, biz.MutationCreatePod)
	before := providerWriteCount(client)
	result, err := provider.Apply(context.Background(), s, providerMutation(s, biz.MutationDeleteWorkspace))
	if err == nil || result.Outcome != biz.MutationNotSent || providerWriteCount(client) != before {
		t.Fatalf("workspace deleted before its Pod: %#v %v", result, err)
	}
	pinned := false
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		options := action.(ktesting.DeleteAction).GetDeleteOptions()
		pinned = options.Preconditions != nil && options.Preconditions.UID != nil && string(*options.Preconditions.UID) == s.ProviderRefs.PodUID
		return false, nil, nil
	})
	applyProvider(t, provider, &s, biz.MutationDeletePod)
	if !pinned {
		t.Fatal("Pod deletion omitted original UID precondition")
	}
}

func TestKubernetesProviderRendersRestrictedPod(t *testing.T) {
	_, _, s := providerFixture(t)
	pod := renderPod(s, "sandbox-"+s.ID, labels(s))
	if token, found, err := unstructured.NestedBool(pod.Object, "spec", "automountServiceAccountToken"); err != nil || !found || token {
		t.Fatalf("service account token enabled: %v %v %v", token, found, err)
	}
	for _, field := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if enabled, _, _ := unstructured.NestedBool(pod.Object, "spec", field); enabled {
			t.Fatalf("%s enabled", field)
		}
	}
	security, _, _ := unstructured.NestedMap(pod.Object, "spec", "securityContext")
	if security["runAsNonRoot"] != true || security["runAsUser"] != int64(10001) || security["fsGroup"] != int64(10001) || security["fsGroupChangePolicy"] != "OnRootMismatch" {
		t.Fatalf("unsafe workspace ownership: %#v", security)
	}
	containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
	container := containers[0].(map[string]any)
	containerSecurity := container["securityContext"].(map[string]any)
	if containerSecurity["allowPrivilegeEscalation"] != false || containerSecurity["privileged"] != false || containerSecurity["readOnlyRootFilesystem"] != true {
		t.Fatalf("unsafe container: %#v", containerSecurity)
	}
}

func TestKubernetesProviderRecoversUnrecordedOriginalVolume(t *testing.T) {
	client, provider, s := providerFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	providerBoundWorkspace(t, client, s)
	// Simulate deletion/crash before the first binding observation persists
	// spec.volumeName. Only the immutable claim UID was recorded.
	if s.ProviderRefs.PVName != "" {
		t.Fatal("fixture already recorded PV")
	}
	if err := client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Delete(context.Background(), "workspace-"+s.ID, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s.Desired = biz.DesiredDeleted
	obs := observeProvider(t, provider, &s)
	if !obs.VolumeExists || obs.ProviderRefs.PVUID != "original-pv-uid" || obs.Observed != biz.ObservedDeleting || obs.ProviderRefs.CleanupReceipt != "" {
		t.Fatalf("unrecorded original PV was lost: %#v", obs)
	}
}

func TestKubernetesProviderCannotProveReclaimWithoutVolumeListAccess(t *testing.T) {
	client, provider, s := providerFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	if err := client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Delete(context.Background(), "workspace-"+s.ID, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("list", "persistentvolumes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "", errors.New("no list permission"))
	})
	s.Desired = biz.DesiredDeleted
	if obs, err := provider.Observe(context.Background(), s); err != nil || !obs.VolumeObservationFailed || obs.Observed != biz.ObservedUnknown || obs.Reason != "WORKSPACE_VOLUME_OBSERVATION_FAILED" || obs.ProviderRefs.PVCUID != s.ProviderRefs.PVCUID || obs.ProviderRefs.CleanupReceipt != "" {
		t.Fatalf("failed PV list became reclaim receipt: %#v %v", obs, err)
	}
}

func TestKubernetesProviderIncompleteVolumeListDoesNotCommitIdentity(t *testing.T) {
	client, provider, s := providerFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	providerBoundWorkspace(t, client, s)
	applyProvider(t, provider, &s, biz.MutationCreatePod)
	pv, err := client.Resource(pvGVR).Get(context.Background(), "pv-"+s.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	duplicate := pv.DeepCopy()
	duplicate.SetName("duplicate-pv")
	duplicate.SetUID("duplicate-pv-uid")
	if err = client.Tracker().Delete(pvcGVR, s.Snapshot.Namespace, s.ProviderRefs.PVCName); err != nil {
		t.Fatal(err)
	}
	page, failSecondPage := 0, true
	client.PrependReactor("list", "persistentvolumes", func(ktesting.Action) (bool, runtime.Object, error) {
		page++
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion("v1")
		list.SetKind("PersistentVolumeList")
		if page == 1 {
			list.Items = []unstructured.Unstructured{*pv.DeepCopy()}
			list.SetContinue("next-page")
			return true, list, nil
		}
		if failSecondPage {
			return true, nil, apierrors.NewTimeoutError("second PV page timed out", 0)
		}
		list.Items = []unstructured.Unstructured{*duplicate.DeepCopy()}
		return true, list, nil
	})
	s.Desired = biz.DesiredPaused
	obs, err := provider.Observe(context.Background(), s)
	if err != nil || !obs.VolumeObservationFailed || obs.Observed != biz.ObservedUnknown || obs.ProviderRefs.PVName != "" || obs.ProviderRefs.PVUID != "" || obs.ProviderRefs.PVCUID != s.ProviderRefs.PVCUID || obs.ProviderRefs.PodUID != s.ProviderRefs.PodUID || obs.ProviderRefs.CleanupReceipt != "" || page != 2 {
		t.Fatalf("incomplete PV scan committed unverified identity: %#v pages=%d err=%v", obs, page, err)
	}
	s.ProviderRefs = obs.ProviderRefs
	page, failSecondPage = 0, false
	if obs, err = provider.Observe(context.Background(), s); !errors.Is(err, biz.ErrOwnership) || obs != (biz.Observation{}) || page != 2 {
		t.Fatalf("retry bypassed the duplicate binding guard: %#v pages=%d err=%v", obs, page, err)
	}
}

func TestKubernetesProviderMatchesEquivalentResourceQuantities(t *testing.T) {
	s := biz.Sandbox{ID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", TenantID: "ffffffff-ffff-4fff-8fff-ffffffffffff", Snapshot: biz.Snapshot{Namespace: "sandbox-ns", RuntimeClass: "kata", ImageReference: "registry.example/sandbox@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CPUMillis: 1000, MemoryMiB: 1024, Workspace: biz.WorkspaceSpec{MountPath: "/workspace", Size: "10Gi", StorageClass: "fast"}}}
	for _, tc := range []struct {
		name, cpu, memory string
		want              bool
	}{
		{"canonical_api_response", "1", "1Gi", true},
		{"different_cpu", "2", "1Gi", false},
		{"different_memory", "1", "2Gi", false},
		{"invalid_cpu", "invalid", "1Gi", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := renderPod(s, "sandbox-"+s.ID, labels(s))
			container := pod.Object["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
			for _, scope := range []string{"requests", "limits"} {
				if err := unstructured.SetNestedField(container, tc.cpu, "resources", scope, "cpu"); err != nil {
					t.Fatal(err)
				}
				if err := unstructured.SetNestedField(container, tc.memory, "resources", scope, "memory"); err != nil {
					t.Fatal(err)
				}
			}
			if got := podMatches(pod, s); got != tc.want {
				t.Fatalf("resource match = %v, want %v; cpu=%q memory=%q", got, tc.want, tc.cpu, tc.memory)
			}
		})
	}
}

func TestKubernetesProviderMatchesImagePlatformAndRuntimeClassSelectors(t *testing.T) {
	_, _, s := providerFixture(t)
	for _, tc := range []struct {
		name     string
		selector map[string]any
		want     bool
	}{
		{"image_platform", map[string]any{"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64"}, true},
		{"runtime_class_selector_merged", map[string]any{"kubernetes.io/os": "linux", "kubernetes.io/arch": "amd64", "sandbox.example/runtime": "kata"}, true},
		{"wrong_arch", map[string]any{"kubernetes.io/os": "linux", "kubernetes.io/arch": "arm64"}, false},
		{"missing_arch", map[string]any{"kubernetes.io/os": "linux"}, false},
		{"wrong_os", map[string]any{"kubernetes.io/os": "windows", "kubernetes.io/arch": "amd64"}, false},
		{"missing_os", map[string]any{"kubernetes.io/arch": "amd64"}, false},
		{"missing_selector", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := renderPod(s, "sandbox-"+s.ID, labels(s))
			if tc.selector == nil {
				unstructured.RemoveNestedField(pod.Object, "spec", "nodeSelector")
			} else if err := unstructured.SetNestedMap(pod.Object, tc.selector, "spec", "nodeSelector"); err != nil {
				t.Fatal(err)
			}
			if got := podMatches(pod, s); got != tc.want {
				t.Fatalf("match = %v, want %v", got, tc.want)
			}
		})
	}
	// Assert the rendered Pod carries the selected image platform; the test
	// cases above deliberately replace the selector to cover API admission.
	pod := renderPod(s, "sandbox-"+s.ID, labels(s))
	selector, found, err := unstructured.NestedStringMap(pod.Object, "spec", "nodeSelector")
	if err != nil || !found || selector["kubernetes.io/os"] != "linux" || selector["kubernetes.io/arch"] != "amd64" {
		t.Fatalf("rendered image platform selector = %v, found=%v, err=%v", selector, found, err)
	}
}

func TestKubernetesProviderRejectsPodExecutionAndSecurityDrift(t *testing.T) {
	_, _, s := providerFixture(t)
	cases := []struct {
		name   string
		change func(map[string]any, map[string]any)
	}{
		{"command", func(_ map[string]any, c map[string]any) { c["command"] = []any{"/bin/sh"} }},
		{"args", func(_ map[string]any, c map[string]any) { c["args"] = []any{"-c", "id"} }},
		{"environment", func(_ map[string]any, c map[string]any) {
			c["env"] = []any{map[string]any{"name": "LD_PRELOAD", "value": "/workspace/lib.so"}}
		}},
		{"environment_from", func(_ map[string]any, c map[string]any) {
			c["envFrom"] = []any{map[string]any{"secretRef": map[string]any{"name": "other"}}}
		}},
		{"lifecycle_hook", func(_ map[string]any, c map[string]any) {
			c["lifecycle"] = map[string]any{"postStart": map[string]any{}}
		}},
		{"init_container", func(spec map[string]any, _ map[string]any) {
			spec["initContainers"] = []any{map[string]any{"name": "extra"}}
		}},
		{"ephemeral_container", func(spec map[string]any, _ map[string]any) {
			spec["ephemeralContainers"] = []any{map[string]any{"name": "debug"}}
		}},
		{"container_user_root", func(_ map[string]any, c map[string]any) {
			c["securityContext"].(map[string]any)["runAsUser"] = int64(0)
		}},
		{"container_nonroot_override", func(_ map[string]any, c map[string]any) {
			c["securityContext"].(map[string]any)["runAsNonRoot"] = false
		}},
		{"container_seccomp_override", func(_ map[string]any, c map[string]any) {
			c["securityContext"].(map[string]any)["seccompProfile"] = map[string]any{"type": "Unconfined"}
		}},
		{"added_capability", func(_ map[string]any, c map[string]any) {
			c["securityContext"].(map[string]any)["capabilities"].(map[string]any)["add"] = []any{"SYS_ADMIN"}
		}},
		{"supplemental_groups", func(spec map[string]any, _ map[string]any) {
			spec["securityContext"].(map[string]any)["supplementalGroups"] = []any{int64(0)}
		}},
		{"mount_subpath", func(_ map[string]any, c map[string]any) {
			c["volumeMounts"].([]any)[0].(map[string]any)["subPath"] = "other"
		}},
		{"mount_read_only", func(_ map[string]any, c map[string]any) {
			c["volumeMounts"].([]any)[0].(map[string]any)["readOnly"] = true
		}},
		{"volume_additional_source", func(spec map[string]any, _ map[string]any) {
			spec["volumes"].([]any)[0].(map[string]any)["hostPath"] = map[string]any{"path": "/"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := renderPod(s, "sandbox-"+s.ID, labels(s))
			spec := pod.Object["spec"].(map[string]any)
			container := spec["containers"].([]any)[0].(map[string]any)
			tc.change(spec, container)
			if podMatches(pod, s) {
				t.Fatal("modified Pod was accepted")
			}
		})
	}
	t.Run("kubernetes_defaults", func(t *testing.T) {
		pod := renderPod(s, "sandbox-"+s.ID, labels(s))
		spec := pod.Object["spec"].(map[string]any)
		spec["initContainers"] = []any{}
		spec["ephemeralContainers"] = []any{}
		container := spec["containers"].([]any)[0].(map[string]any)
		container["args"] = []any{}
		container["imagePullPolicy"] = "IfNotPresent"
		container["terminationMessagePath"] = "/dev/termination-log"
		container["terminationMessagePolicy"] = "File"
		mount := container["volumeMounts"].([]any)[0].(map[string]any)
		mount["readOnly"], mount["mountPropagation"] = false, "None"
		claim := spec["volumes"].([]any)[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)
		claim["readOnly"] = false
		if !podMatches(pod, s) {
			t.Fatal("ordinary Kubernetes default fields were rejected")
		}
	})
}

func TestKubernetesProviderPodReasonsExcludeMessages(t *testing.T) {
	tests := []struct {
		name   string
		status map[string]any
		want   string
	}{
		{"image_pull", map[string]any{"phase": "Pending", "containerStatuses": []any{map[string]any{"name": "sandbox", "state": map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff", "message": "secret registry password"}}}}}, "POD_IMAGE_PULL_BACK_OFF"},
		{"unschedulable", map[string]any{"phase": "Pending", "conditions": []any{map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "secret node details"}}}, "POD_UNSCHEDULABLE"},
		{"exit", map[string]any{"phase": "Failed", "containerStatuses": []any{map[string]any{"name": "sandbox", "state": map[string]any{"terminated": map[string]any{"exitCode": int64(137), "message": "secret output"}}}}}, "POD_TERMINATED_EXIT_CODE_137"},
		{"unrecognized_reason", map[string]any{"phase": "Failed", "reason": "secret", "message": "secret"}, "POD_TERMINATED"},
		{"arbitrary_exit", map[string]any{"phase": "Failed", "containerStatuses": []any{map[string]any{"name": "sandbox", "state": map[string]any{"terminated": map[string]any{"exitCode": int64(1000000)}}}}}, "POD_TERMINATED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, provider, s := providerFixture(t)
			applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
			applyProvider(t, provider, &s, biz.MutationCreatePod)
			pod, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), "sandbox-"+s.ID, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			pod.Object["status"] = tc.status
			if _, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			obs := observeProvider(t, provider, &s)
			if obs.Reason != tc.want {
				t.Fatalf("reason = %q, want %q", obs.Reason, tc.want)
			}
		})
	}
}

func TestKubernetesProviderDriftCannotResolvePendingCreate(t *testing.T) {
	client, provider, s := providerFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	pending := providerMutation(s, biz.MutationCreatePod)
	s.Pending = &pending
	pod := renderPod(s, "sandbox-"+s.ID, labels(s))
	pod.SetAnnotations(map[string]string{providerMutationID: pending.ID})
	spec := pod.Object["spec"].(map[string]any)
	spec["containers"].([]any)[0].(map[string]any)["command"] = []any{"/bin/sh", "-c", "unexpected"}
	if _, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	worker, repo := newProviderWorker(t, provider, s)
	providerWorkerStep(t, worker)
	if repo.state.Pending == nil || *repo.state.Pending != pending || repo.state.Observed != biz.ObservedUnknown || len(repo.prepared) != 0 {
		t.Fatalf("drifted Pod cleared the unknown creation barrier: %#v", repo.state)
	}
}

func configuredNamespaceFixture(t *testing.T) (*fake.FakeDynamicClient, *KubernetesProvider, biz.Sandbox) {
	t.Helper()
	client, provider, s := providerFixture(t)
	s.Snapshot.Namespace = "tenant-" + s.TenantID
	s.Snapshot.NamespaceUID = "namespace-uid"
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": s.Snapshot.Namespace, "uid": "namespace-uid", "labels": map[string]any{storagebiz.NamespaceManagedBy: storagebiz.NamespaceOwner, storagebiz.NamespaceTenantID: s.TenantID}}}}
	if _, err := client.Resource(namespaceGVR).Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := provider.ConfigureTenantNamespacePrefix("tenant-"); err != nil {
		t.Fatal(err)
	}
	return client, provider, s
}

func TestKubernetesProviderRejectsChangedTenantNamespaceBeforeWrites(t *testing.T) {
	for _, change := range []string{"tenant", "owner", "deleting", "name"} {
		t.Run(change, func(t *testing.T) {
			client, provider, s := configuredNamespaceFixture(t)
			ns, err := client.Resource(namespaceGVR).Get(context.Background(), s.Snapshot.Namespace, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "tenant":
				ns.SetLabels(map[string]string{storagebiz.NamespaceManagedBy: storagebiz.NamespaceOwner, storagebiz.NamespaceTenantID: "other"})
			case "owner":
				ns.SetLabels(map[string]string{storagebiz.NamespaceManagedBy: "other", storagebiz.NamespaceTenantID: s.TenantID})
			case "deleting":
				now := metav1.Now()
				ns.SetDeletionTimestamp(&now)
			case "name":
				s.Snapshot.Namespace = "foreign-namespace"
			}
			if change != "name" {
				if _, err = client.Resource(namespaceGVR).Update(context.Background(), ns, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			before := providerWriteCount(client)
			result, err := provider.Apply(context.Background(), s, providerMutation(s, biz.MutationCreateWorkspace))
			if err == nil || result.Outcome != biz.MutationNotSent || providerWriteCount(client) != before {
				t.Fatalf("changed namespace permitted a write: %#v %v", result, err)
			}
			if _, err = provider.Observe(context.Background(), s); err == nil {
				t.Fatal("changed namespace was observed as authoritative")
			}
		})
	}
}

func TestKubernetesProviderNamespaceReplacementDuringCreateKeepsUnknown(t *testing.T) {
	client, provider, s := configuredNamespaceFixture(t)
	client.PrependReactor("create", "persistentvolumeclaims", func(ktesting.Action) (bool, runtime.Object, error) {
		object, err := client.Tracker().Get(namespaceGVR, "", s.Snapshot.Namespace)
		if err != nil {
			return true, nil, err
		}
		ns := object.(*unstructured.Unstructured)
		ns.SetUID("replacement-uid")
		if err = client.Tracker().Update(namespaceGVR, ns, ""); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	result, err := provider.Apply(context.Background(), s, providerMutation(s, biz.MutationCreateWorkspace))
	if err == nil || result.Outcome != biz.MutationUnknown || result.Reason != "TENANT_NAMESPACE_CHANGED" || result.Refs.PVCUID != "" {
		t.Fatalf("replacement yielded a conclusive receipt: %#v %v", result, err)
	}
}

func TestKubernetesProviderRejectsReplacementBetweenWorkerSteps(t *testing.T) {
	client, provider, s := configuredNamespaceFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	ns, err := client.Resource(namespaceGVR).Get(context.Background(), s.Snapshot.Namespace, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns.SetUID("replacement-uid")
	if _, err = client.Resource(namespaceGVR).Update(context.Background(), ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	before := providerWriteCount(client)
	result, err := provider.Apply(context.Background(), s, providerMutation(s, biz.MutationCreatePod))
	if err == nil || result.Outcome != biz.MutationNotSent || providerWriteCount(client) != before {
		t.Fatalf("replacement allowed Pod write: %#v %v", result, err)
	}
	if _, err = provider.Observe(context.Background(), s); err == nil {
		t.Fatal("replacement accepted during observation")
	}
}

// This wrapper waits for the actual PV child context to expire, unlike a
// reactor that immediately returns a synthetic timeout error.
type contextBlockingPVClient struct {
	dynamic.Interface
	pvTimedOut             bool
	rejectNamespaceAfterPV bool
}

type contextBlockingPVResource struct {
	dynamic.NamespaceableResourceInterface
	parent   *contextBlockingPVClient
	resource schema.GroupVersionResource
}

func (c *contextBlockingPVClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &contextBlockingPVResource{NamespaceableResourceInterface: c.Interface.Resource(gvr), parent: c, resource: gvr}
}

func (r *contextBlockingPVResource) Get(ctx context.Context, name string, opts metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if r.resource == pvGVR {
		<-ctx.Done()
		r.parent.pvTimedOut = true
		return nil, ctx.Err()
	}
	if r.resource == namespaceGVR && r.parent.pvTimedOut && r.parent.rejectNamespaceAfterPV {
		return nil, errors.New("namespace recheck unavailable")
	}
	return r.NamespaceableResourceInterface.Get(ctx, name, opts, subresources...)
}

func TestProviderWorkerNamespaceRecheckAfterRealPVTimeout(t *testing.T) {
	for _, namespaceUnavailable := range []bool{false, true} {
		t.Run(fmt.Sprintf("namespace_unavailable_%t", namespaceUnavailable), func(t *testing.T) {
			client, provider, s := configuredNamespaceFixture(t)
			applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
			providerBoundWorkspace(t, client, s)
			applyProvider(t, provider, &s, biz.MutationCreatePod)
			original := s.ProviderRefs
			s.Desired = biz.DesiredPaused
			blocking := &contextBlockingPVClient{Interface: client, rejectNamespaceAfterPV: namespaceUnavailable}
			provider.client = blocking
			worker, repo := newProviderWorker(t, provider, s)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			worked, err := worker.Step(ctx)
			if !worked || err != nil || !blocking.pvTimedOut {
				t.Fatalf("worker did not reach PV context timeout: worked=%v err=%v", worked, err)
			}
			if namespaceUnavailable {
				if providerActionCount(client, "delete", "pods") != 0 || repo.state.Observed != biz.ObservedUnknown || repo.state.Reason != "PROVIDER_OBSERVATION_FAILED" {
					t.Fatalf("Pod stopped without namespace recheck: %#v", repo.state)
				}
				return
			}
			if repo.state.Reason != "WORKSPACE_VOLUME_OBSERVATION_FAILED" || repo.state.Pending != nil || len(repo.prepared) != 1 || repo.prepared[0].Kind != biz.MutationDeletePod || repo.prepared[0].TargetUID != original.PodUID || providerActionCount(client, "delete", "pods") != 1 {
				t.Fatalf("verified Pod was not stopped after PV timeout: %#v", repo.state)
			}
		})
	}
}

func TestKubernetesProviderRequiresConfiguredCluster(t *testing.T) {
	client, _, s := providerFixture(t)
	for _, clusterID := range []string{"", "not-a-cluster", "00000000-0000-0000-0000-000000000000", "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"} {
		if _, err := NewKubernetesProvider(client, clusterID); !errors.Is(err, biz.ErrInvalid) {
			t.Fatalf("invalid configured cluster %q accepted: %v", clusterID, err)
		}
	}
	if _, err := NewKubernetesProvider(nil, s.Snapshot.ClusterID); !errors.Is(err, biz.ErrInvalid) {
		t.Fatalf("nil client accepted: %v", err)
	}
}

func TestKubernetesProviderRejectsWrongClusterWithoutRequests(t *testing.T) {
	original, providerA, s := providerFixture(t)
	applyProvider(t, providerA, &s, biz.MutationCreateWorkspace)
	providerBoundWorkspace(t, original, s)
	applyProvider(t, providerA, &s, biz.MutationCreatePod)
	providerPodStatus(t, original, s, "Running", "True", true)
	observeProvider(t, providerA, &s)
	clusterB := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{pvGVR: "PersistentVolumeList"})
	providerB, err := NewKubernetesProvider(clusterB, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	for _, desired := range []string{biz.DesiredRunning, biz.DesiredPaused, biz.DesiredDeleted} {
		s.Desired = desired
		obs, err := providerB.Observe(context.Background(), s)
		if !errors.Is(err, biz.ErrClusterMismatch) || obs != (biz.Observation{}) {
			t.Fatalf("wrong cluster observation produced facts: %#v %v", obs, err)
		}
	}
	for _, kind := range []string{biz.MutationCreateWorkspace, biz.MutationCreatePod, biz.MutationDeletePod, biz.MutationDeleteWorkspace} {
		result, err := providerB.Apply(context.Background(), s, providerMutation(s, kind))
		if !errors.Is(err, biz.ErrClusterMismatch) || result.Outcome != biz.MutationNotSent || result.Reason != "SANDBOX_PROVIDER_CLUSTER_MISMATCH" || result.Refs != (biz.ProviderRefs{}) {
			t.Fatalf("wrong cluster mutation produced a receipt: %#v %v", result, err)
		}
	}
	if actions := clusterB.Actions(); len(actions) != 0 {
		t.Fatalf("wrong cluster received API requests: %#v", actions)
	}
	for _, target := range []struct {
		gvr                  schema.GroupVersionResource
		name, namespace, uid string
	}{
		{pvcGVR, s.ProviderRefs.PVCName, s.Snapshot.Namespace, s.ProviderRefs.PVCUID},
		{podGVR, s.ProviderRefs.PodName, s.Snapshot.Namespace, s.ProviderRefs.PodUID},
		{pvGVR, s.ProviderRefs.PVName, "", s.ProviderRefs.PVUID},
	} {
		object, err := original.Resource(target.gvr).Namespace(target.namespace).Get(context.Background(), target.name, metav1.GetOptions{})
		if err != nil || string(object.GetUID()) != target.uid {
			t.Fatalf("original %s changed: %v", target.gvr.Resource, err)
		}
	}
}

func TestKubernetesProviderPausedPreservesWorkspaceFacts(t *testing.T) {
	for _, test := range []struct{ phase, reason string }{
		{"Pending", "WORKSPACE_PENDING"}, {"Lost", "WORKSPACE_LOST"}, {"Missing", "WORKSPACE_MISSING"}, {"Deleting", "WORKSPACE_DELETING"},
	} {
		t.Run(test.phase, func(t *testing.T) {
			client, provider, s := providerFixture(t)
			applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
			if test.phase != "Pending" {
				providerBoundWorkspace(t, client, s)
				observeProvider(t, provider, &s)
			}
			pvc, err := client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), s.ProviderRefs.PVCName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch test.phase {
			case "Missing":
				err = client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Delete(context.Background(), pvc.GetName(), metav1.DeleteOptions{})
			case "Deleting":
				at := metav1.Now()
				pvc.SetDeletionTimestamp(&at)
			default:
				err = unstructured.SetNestedField(pvc.Object, test.phase, "status", "phase")
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.phase != "Missing" {
				if _, err = client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pvc, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			s.Desired = biz.DesiredPaused
			before, uid, pvUID := providerWriteCount(client), s.ProviderRefs.PVCUID, s.ProviderRefs.PVUID
			obs, err := provider.Observe(context.Background(), s)
			if err != nil || obs.Observed != biz.ObservedPaused || obs.Reason != test.reason || obs.WorkspaceBound || obs.ProviderRefs.PVCUID != uid || obs.ProviderRefs.PVUID != pvUID || obs.ProviderRefs.CleanupReceipt != "" || providerWriteCount(client) != before {
				t.Fatalf("paused observation lost workspace facts: %#v %v", obs, err)
			}
			// The same missing/lost workspace remains unavailable for a running
			// intent; stopping the workload never permits an empty replacement.
			if test.phase == "Missing" || test.phase == "Lost" {
				s.Desired = biz.DesiredRunning
				obs, err = provider.Observe(context.Background(), s)
				if err != nil || obs.Observed != biz.ObservedUnknown || obs.Reason != test.reason || obs.ProviderRefs.PVCUID != uid {
					t.Fatalf("running bypassed original workspace failure: %#v %v", obs, err)
				}
			}
		})
	}
}

func TestKubernetesProviderPendingMutationCannotCompletePause(t *testing.T) {
	for _, kind := range []string{biz.MutationCreateWorkspace, biz.MutationCreatePod} {
		t.Run(kind, func(t *testing.T) {
			client, provider, s := providerFixture(t)
			applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
			s.Desired = biz.DesiredPaused
			s.Pending = &biz.Mutation{ID: "55555555-5555-4555-8555-555555555555", Kind: kind, OperationID: s.LastOperationID, Version: s.Version}
			if kind == biz.MutationCreateWorkspace {
				// The present workspace must be attributable to the pending create.
				// Unannotated and different-attempt objects are covered separately.
				pvc, err := client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), "workspace-"+s.ID, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				pvc.SetAnnotations(map[string]string{providerMutationID: s.Pending.ID})
				if _, err = client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pvc, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			obs, err := provider.Observe(context.Background(), s)
			if err != nil || obs.Observed != biz.ObservedUnknown || obs.Reason != "PROVIDER_MUTATION_PENDING" || obs.PodExists || obs.ProviderRefs.PVCUID != s.ProviderRefs.PVCUID {
				t.Fatalf("unresolved create completed pause: %#v %v", obs, err)
			}
		})
	}
}
