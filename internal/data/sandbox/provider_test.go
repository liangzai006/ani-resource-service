package sandbox

import (
	"context"
	"errors"
	"fmt"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
	if obs, err := provider.Observe(context.Background(), s); err == nil || obs.Observed == biz.ObservedDeleted || obs.ProviderRefs.CleanupReceipt != "" {
		t.Fatalf("failed PV list became reclaim receipt: %#v %v", obs, err)
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
			_, provider, s := providerFixture(t)
			applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
			s.Desired = biz.DesiredPaused
			s.Pending = &biz.Mutation{ID: "55555555-5555-4555-8555-555555555555", Kind: kind, OperationID: s.LastOperationID, Version: s.Version}
			obs, err := provider.Observe(context.Background(), s)
			if err != nil || obs.Observed != biz.ObservedUnknown || obs.Reason != "PROVIDER_MUTATION_PENDING" || obs.PodExists || obs.ProviderRefs.PVCUID != s.ProviderRefs.PVCUID {
				t.Fatalf("unresolved create completed pause: %#v %v", obs, err)
			}
		})
	}
}
