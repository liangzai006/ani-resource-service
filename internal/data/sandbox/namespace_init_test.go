package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

const namespaceTenant = "11111111-1111-4111-8111-111111111111"

func ownedNamespace(uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": "tenant-" + namespaceTenant, "uid": uid, "labels": map[string]any{
			storage.NamespaceManagedBy: storage.NamespaceOwner, storage.NamespaceTenantID: namespaceTenant,
		}},
	}}
}

func TestEnsureSandboxNamespaceCreatesSharedOwnedNamespace(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("create", "namespaces", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		if object.GetName() != "tenant-"+namespaceTenant || object.GetLabels()[storage.NamespaceManagedBy] != storage.NamespaceOwner || object.GetLabels()[storage.NamespaceTenantID] != namespaceTenant {
			t.Fatalf("incorrect shared namespace ownership: %v", object.GetLabels())
		}
		object.SetUID(types.UID("created-uid"))
		return false, nil, nil
	})
	resolver := TenantNamespace{Client: client, NamespacePrefix: "tenant-"}
	uid, err := resolver.EnsureSandboxNamespaceUID(context.Background(), namespaceTenant)
	if err != nil || uid != "created-uid" {
		t.Fatalf("uid=%q err=%v", uid, err)
	}
	uid, err = resolver.ResolveSandboxNamespaceUID(context.Background(), namespaceTenant)
	if err != nil || uid != "created-uid" {
		t.Fatalf("read existing uid=%q err=%v", uid, err)
	}
	creates := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("expected exactly one create, got %d", creates)
	}
}

func TestEnsureSandboxNamespaceResolvesConcurrentOrUncertainCreate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		createErr error
		visible   bool
		foreign   bool
		wantUID   string
		wantErr   error
	}{
		{"concurrent same owner", apierrors.NewAlreadyExists(schema.GroupResource{Resource: "namespaces"}, "tenant"), true, false, "owned-uid", nil},
		{"create timeout but visible", apierrors.NewTimeoutError("private timeout detail", 1), true, false, "owned-uid", nil},
		{"timeout still unknown", apierrors.NewTimeoutError("private timeout detail", 1), false, false, "", biz.ErrDependencyUnavailable},
		{"concurrent foreign owner", apierrors.NewAlreadyExists(schema.GroupResource{Resource: "namespaces"}, "tenant"), true, true, "", biz.ErrOwnership},
		{"forbidden without object", apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "tenant", errors.New("private provider detail")), false, false, "", biz.ErrDependencyUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleDynamicClient(runtime.NewScheme())
			client.PrependReactor("create", "namespaces", func(ktesting.Action) (bool, runtime.Object, error) {
				if tc.visible {
					ns := ownedNamespace("owned-uid")
					if tc.foreign {
						ns.SetLabels(map[string]string{storage.NamespaceManagedBy: "other", storage.NamespaceTenantID: namespaceTenant})
					}
					if err := client.Tracker().Add(ns); err != nil {
						t.Fatal(err)
					}
				}
				return true, nil, tc.createErr
			})
			uid, err := (TenantNamespace{Client: client, NamespacePrefix: "tenant-"}).EnsureSandboxNamespaceUID(context.Background(), namespaceTenant)
			if uid != tc.wantUID || !errors.Is(err, tc.wantErr) {
				t.Fatalf("uid=%q err=%v", uid, err)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("provider detail leaked")
			}
			creates := 0
			for _, a := range client.Actions() {
				if a.GetVerb() == "create" {
					creates++
				}
			}
			if creates != 1 {
				t.Fatalf("want a single create, got %d", creates)
			}
		})
	}
}

func TestEnsureSandboxNamespaceRejectsUnsafeExistingWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*unstructured.Unstructured)
	}{
		{"foreign tenant", func(ns *unstructured.Unstructured) {
			ns.SetLabels(map[string]string{storage.NamespaceManagedBy: storage.NamespaceOwner, storage.NamespaceTenantID: "other"})
		}},
		{"deleting", func(ns *unstructured.Unstructured) { now := metav1.Now(); ns.SetDeletionTimestamp(&now) }},
		{"missing uid", func(ns *unstructured.Unstructured) { ns.SetUID("") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := ownedNamespace("owned-uid")
			tc.alter(ns)
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), ns)
			_, err := (TenantNamespace{Client: client, NamespacePrefix: "tenant-"}).EnsureSandboxNamespaceUID(context.Background(), namespaceTenant)
			if !errors.Is(err, biz.ErrOwnership) {
				t.Fatalf("unsafe namespace accepted: %v", err)
			}
			for _, a := range client.Actions() {
				if a.GetVerb() != "get" {
					t.Fatalf("unexpected mutation: %s", a.GetVerb())
				}
			}
		})
	}
}

func TestResolveSandboxNamespaceNeverCreates(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	_, err := (TenantNamespace{Client: client, NamespacePrefix: "tenant-"}).ResolveSandboxNamespaceUID(context.Background(), namespaceTenant)
	if !errors.Is(err, biz.ErrDependencyUnavailable) {
		t.Fatalf("missing namespace: %v", err)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" {
			t.Fatal(fmt.Sprintf("unexpected mutation: %s", a.GetVerb()))
		}
	}
}

func TestNamespaceReadPreservesCancellationWithoutProviderDetails(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			client := fake.NewSimpleDynamicClient(runtime.NewScheme())
			client.PrependReactor("get", "namespaces", func(ktesting.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("private provider detail: %w", want)
			})
			_, err := (TenantNamespace{Client: client, NamespacePrefix: "tenant-"}).ResolveSandboxNamespaceUID(context.Background(), namespaceTenant)
			if err != want {
				t.Fatalf("want cancellation without provider detail, got %v", err)
			}
		})
	}
}
