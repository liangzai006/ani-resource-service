package instance

import (
	"context"
	"errors"
	"fmt"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func tenantNamespace(tenant, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": namespace, "labels": map[string]any{"network.ani.io/tenant-id": tenant, "network.ani.io/managed-by": "ani-network-service"}}}}
}

func testBorrowedPVC(namespace, name, uid, phase, mode string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"namespace": namespace, "name": name, "uid": uid}, "spec": map[string]any{"volumeMode": mode, "accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "20Gi"}}}, "status": map[string]any{"phase": phase}}}
}

func TestResolveBorrowedVolumesFreezesPendingUIDAndRejectsScope(t *testing.T) {
	i := configuredInstance(t)
	pending := testBorrowedPVC(i.Spec.Namespace, "dataset", "dataset-original", "Pending", "Filesystem")
	models := testBorrowedPVC(i.Spec.Namespace, "models", "models-original", "Bound", "Filesystem")
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), tenantNamespace(i.TenantID, i.Spec.Namespace), pending, models)
	n, _ := NewNotebook(client)
	input := append([]biz.VolumeMount(nil), i.Spec.VolumeMounts...)
	for k := range input {
		input[k].PVCUID, input[k].ClusterID, input[k].Namespace, input[k].Ownership = "", "", "", ""
	}
	resolved, err := n.ResolveVolumes(context.Background(), i.TenantID, i.Spec.ClusterID, i.Spec.Namespace, input)
	if err != nil || len(resolved) != 2 || resolved[0].PVCUID != "dataset-original" || resolved[0].Ownership != "borrowed" || resolved[0].ClusterID != i.Spec.ClusterID || resolved[0].Namespace != i.Spec.Namespace || !resolved[1].ReadOnly {
		t.Fatal("Pending filesystem claims must resolve without requiring Bound", err)
	}
	if input[0].PVCUID != "" || input[0].Ownership != "" {
		t.Fatal("resolver mutated user input")
	}
	if _, err = n.ResolveVolumes(context.Background(), "22222222-2222-4222-8222-222222222222", i.Spec.ClusterID, i.Spec.Namespace, input); !errors.Is(err, biz.ErrPVCNotFound) {
		t.Fatal("foreign Namespace must have non-enumerating not-found result", err)
	}
	for name, mutate := range map[string]func(*unstructured.Unstructured){
		"dedicated workspace": func(p *unstructured.Unstructured) { p.SetLabels(labels(i)) },
		"block volume": func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, "Block", "spec", "volumeMode")
		},
		"lost claim": func(p *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(p.Object, "Lost", "status", "phase")
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := pending.DeepCopy()
			mutate(changed)
			_, _ = client.Resource(pvcGVR).Namespace(i.Spec.Namespace).Update(context.Background(), changed, metav1.UpdateOptions{})
			if _, err := n.ResolveVolumes(context.Background(), i.TenantID, i.Spec.ClusterID, i.Spec.Namespace, input); err == nil {
				t.Fatal("unmountable claim accepted")
			}
			_, _ = client.Resource(pvcGVR).Namespace(i.Spec.Namespace).Update(context.Background(), pending, metav1.UpdateOptions{})
		})
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" || action.GetVerb() == "patch" {
			t.Fatal("borrowed volume resolver changed PVC lifecycle")
		}
	}
}

func TestBorrowedPVCCatalogDeterministicBoundedSnapshot(t *testing.T) {
	i := configuredInstance(t)
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{pvcGVR: "PersistentVolumeClaimList"}, tenantNamespace(i.TenantID, i.Spec.Namespace))
	calls := 0
	client.PrependReactor("list", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		calls++
		options := action.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
		if options.Limit != 200 || action.GetNamespace() != i.Spec.Namespace {
			t.Fatal("catalog list is not tenant scoped and bounded")
		}
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion("v1")
		list.SetKind("PersistentVolumeClaimList")
		list.SetResourceVersion("snapshot-1")
		if options.Continue == "" {
			list.Items = []unstructured.Unstructured{*testBorrowedPVC(i.Spec.Namespace, "zeta", "z", "Pending", "Filesystem"), *testBorrowedPVC(i.Spec.Namespace, "alpha", "a", "Bound", "Filesystem")}
			list.SetContinue("second")
		} else if options.Continue == "second" {
			workspace := testBorrowedPVC(i.Spec.Namespace, "workspace", "work", "Bound", "Filesystem")
			workspace.SetLabels(labels(i))
			list.Items = []unstructured.Unstructured{*workspace, *testBorrowedPVC(i.Spec.Namespace, "beta", "b", "Lost", "Filesystem")}
		} else {
			t.Fatal("unexpected continuation", options.Continue)
		}
		return true, list, nil
	})
	n, _ := NewNotebook(client)
	items, total, err := n.ListMountablePVCs(context.Background(), i.TenantID, i.Spec.ClusterID, i.Spec.Namespace, 2, 1, "")
	if err != nil || calls != 2 || total != 3 || len(items) != 1 || items[0].PVCName != "beta" || items[0].Selectable || items[0].UnavailableReason == "" || items[0].RequestedCapacity != "20Gi" {
		t.Fatal("catalog snapshot/filter/sort/page mismatch", items, total, calls, err)
	}
}

func TestBorrowedPVCCatalogRestartsExpiredSnapshotAndFailsCapacity(t *testing.T) {
	i := configuredInstance(t)
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{pvcGVR: "PersistentVolumeClaimList"}, tenantNamespace(i.TenantID, i.Spec.Namespace))
	calls := 0
	client.PrependReactor("list", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		calls++
		options := action.(interface{ GetListOptions() metav1.ListOptions }).GetListOptions()
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion("v1")
		list.SetKind("PersistentVolumeClaimList")
		if calls == 1 {
			list.SetContinue("expired")
			list.Items = []unstructured.Unstructured{*testBorrowedPVC(i.Spec.Namespace, "old-snapshot", "old", "Pending", "Filesystem")}
		} else if calls == 2 {
			return true, nil, apierrors.NewResourceExpired("expired")
		} else if calls == 3 {
			if options.Continue != "" {
				t.Fatal("expired snapshot did not restart")
			}
			list.Items = []unstructured.Unstructured{*testBorrowedPVC(i.Spec.Namespace, "fresh-snapshot", "fresh", "Pending", "Filesystem")}
		}
		return true, list, nil
	})
	n, _ := NewNotebook(client)
	items, total, err := n.ListMountablePVCs(context.Background(), i.TenantID, i.Spec.ClusterID, i.Spec.Namespace, 1, 20, "")
	if err != nil || calls != 3 || total != 1 || len(items) != 1 || items[0].PVCName != "fresh-snapshot" {
		t.Fatal("expired result mixed snapshot generations", items, total, calls, err)
	}
	client.ReactionChain = client.ReactionChain[1:]
	client.PrependReactor("list", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion("v1")
		list.SetKind("PersistentVolumeClaimList")
		for k := 0; k < 200; k++ {
			list.Items = append(list.Items, *testBorrowedPVC(i.Spec.Namespace, fmt.Sprintf("claim-%d", k), fmt.Sprintf("uid-%d", k), "Pending", "Filesystem"))
		}
		list.SetContinue("more")
		return true, list, nil
	})
	if items, _, err = n.ListMountablePVCs(context.Background(), i.TenantID, i.Spec.ClusterID, i.Spec.Namespace, 1, 20, ""); !errors.Is(err, biz.ErrPVCCatalogLimit) || len(items) != 0 {
		t.Fatal("catalog silently truncated capacity", err)
	}
}

func TestCreateNotebookChecksBorrowedPVCUIDWithoutMutatingBorrowedClaims(t *testing.T) {
	i := configuredInstance(t)
	objects := []runtime.Object{tenantNamespace(i.TenantID, i.Spec.Namespace)}
	for _, mount := range i.Spec.VolumeMounts {
		objects = append(objects, testBorrowedPVC(i.Spec.Namespace, mount.PVCName, mount.PVCUID, "Pending", "Filesystem"))
	}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
	client.PrependReactor("create", "notebooks", func(action ktesting.Action) (bool, runtime.Object, error) {
		notebook := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		notebook.SetUID(types.UID("created"))
		return true, notebook, nil
	})
	n, _ := NewNotebook(client)
	if e := n.ConfigureImages(pullSecretStub{}); e != nil {
		t.Fatal(e)
	}
	if _, err := n.CreateNotebook(context.Background(), i); err != nil {
		t.Fatal("original Pending claims rejected", err)
	}
	for _, action := range client.Actions() {
		if action.GetResource() == pvcGVR && action.GetVerb() != "get" {
			t.Fatal("borrowed claim acquired ownerReference or lifecycle mutation")
		}
	}
	client.ClearActions()
	replaced := testBorrowedPVC(i.Spec.Namespace, "dataset", "replacement", "Pending", "Filesystem")
	_, _ = client.Resource(pvcGVR).Namespace(i.Spec.Namespace).Update(context.Background(), replaced, metav1.UpdateOptions{})
	client.ClearActions()
	if _, err := n.CreateNotebook(context.Background(), i); !errors.Is(err, biz.ErrPVCReferenceChanged) {
		t.Fatal("Create accepted same-name replacement PVC", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "get" {
			t.Fatal("reference failure created external workload")
		}
	}
}
