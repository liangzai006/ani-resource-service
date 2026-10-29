package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

type storageClassesFixture struct {
	items  []storage.Class
	err    error
	tenant string
}

type storageNamespacesFixture struct {
	namespace storage.Namespace
	err       error
	requested string
}

func (s *storageNamespacesFixture) GetNamespace(_ context.Context, name string) (storage.Namespace, error) {
	s.requested = name
	return s.namespace, s.err
}

func usableNamespace(tenant string) *storageNamespacesFixture {
	return &storageNamespacesFixture{namespace: storage.Namespace{Name: "tenant-" + tenant, ManagedBy: storage.NamespaceOwner, TenantID: tenant}}
}

func (s *storageClassesFixture) ListStorageClasses(_ context.Context, tenant string) ([]storage.Class, error) {
	s.tenant = tenant
	return s.items, s.err
}

func storageClassObject(policy string) *unstructured.Unstructured {
	class := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass",
		"metadata": map[string]any{"name": "platform-default", "annotations": map[string]any{"storageclass.kubernetes.io/is-default-class": "true"}},
	}}
	if policy != "" {
		class.Object["reclaimPolicy"] = policy
	}
	return class
}

func TestSandboxDefaultStorageClassRequiresUnambiguousSafeLiveObject(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	good := storage.Class{Name: "platform-default", Default: true, ReclaimPolicy: "Delete"}
	tests := []struct {
		name   string
		items  []storage.Class
		object *unstructured.Unstructured
		want   string
	}{
		{"one explicit safe default", []storage.Class{good}, storageClassObject("Delete"), "platform-default"},
		{"no default", []storage.Class{{Name: "other", ReclaimPolicy: "Delete"}}, storageClassObject("Delete"), ""},
		{"two defaults", []storage.Class{good, {Name: "other", Default: true, ReclaimPolicy: "Delete"}}, storageClassObject("Delete"), ""},
		{"catalog retain", []storage.Class{{Name: "platform-default", Default: true, ReclaimPolicy: "Retain"}}, storageClassObject("Delete"), ""},
		{"missing explicit reclaim", []storage.Class{good}, storageClassObject(""), ""},
		{"live retain", []storage.Class{good}, storageClassObject("Retain"), ""},
		{"default annotation changed", []storage.Class{good}, func() *unstructured.Unstructured { o := storageClassObject("Delete"); o.SetAnnotations(nil); return o }(), ""},
		{"deleting", []storage.Class{good}, func() *unstructured.Unstructured {
			o := storageClassObject("Delete")
			stamp := metav1.NewTime(time.Unix(1, 0))
			o.SetDeletionTimestamp(&stamp)
			return o
		}(), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			catalog := &storageClassesFixture{items: tc.items}
			namespaces := usableNamespace(tenant)
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), tc.object)
			got, err := (StorageClass{Classes: catalog, Namespaces: namespaces, NamespacePrefix: "tenant-", Client: client}).ResolveSandboxStorageClass(context.Background(), tenant)
			if tc.want != "" {
				if err != nil || got != tc.want || catalog.tenant != tenant || namespaces.requested != "tenant-"+tenant {
					t.Fatalf("got=%q err=%v tenant=%q namespace=%q", got, err, catalog.tenant, namespaces.requested)
				}
			} else if got != "" || !errors.Is(err, biz.ErrDependencyUnavailable) {
				t.Fatalf("got=%q err=%v, expected unavailable", got, err)
			}
		})
	}
}

func TestSandboxDefaultStorageClassPropagatesDependencyErrors(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	catalog := &storageClassesFixture{err: context.DeadlineExceeded}
	resolver := StorageClass{Classes: catalog, Namespaces: usableNamespace(tenant), NamespacePrefix: "tenant-", Client: client}
	if _, err := resolver.ResolveSandboxStorageClass(context.Background(), tenant); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("catalog deadline: %v", err)
	}
	catalog.err = nil
	catalog.items = []storage.Class{{Name: "vanished", Default: true, ReclaimPolicy: "Delete"}}
	if _, err := resolver.ResolveSandboxStorageClass(context.Background(), tenant); err == nil {
		t.Fatal("missing default object was accepted")
	}
	if _, err := (StorageClass{Classes: catalog}).ResolveSandboxStorageClass(context.Background(), tenant); !errors.Is(err, biz.ErrDependencyUnavailable) {
		t.Fatalf("missing client: %v", err)
	}
	if _, err := resolver.ResolveSandboxStorageClass(context.Background(), "bad-tenant"); !errors.Is(err, biz.ErrInvalid) {
		t.Fatalf("invalid tenant: %v", err)
	}
}

func TestSandboxDefaultStorageClassRejectsUnownedNamespaces(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), storageClassObject("Delete"))
	for _, tc := range []struct {
		name string
		ns   storage.Namespace
	}{
		{"wrong name", storage.Namespace{Name: "elsewhere", ManagedBy: storage.NamespaceOwner, TenantID: tenant}},
		{"wrong owner", storage.Namespace{Name: "tenant-" + tenant, ManagedBy: "other", TenantID: tenant}},
		{"wrong tenant", storage.Namespace{Name: "tenant-" + tenant, ManagedBy: storage.NamespaceOwner, TenantID: "other"}},
		{"deleting", storage.Namespace{Name: "tenant-" + tenant, ManagedBy: storage.NamespaceOwner, TenantID: tenant, Deleting: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := &storageClassesFixture{items: []storage.Class{{Name: "platform-default", Default: true, ReclaimPolicy: "Delete"}}}
			resolver := StorageClass{Classes: catalog, Namespaces: &storageNamespacesFixture{namespace: tc.ns}, NamespacePrefix: "tenant-", Client: client}
			if _, err := resolver.ResolveSandboxStorageClass(context.Background(), tenant); !errors.Is(err, biz.ErrDependencyUnavailable) {
				t.Fatalf("namespace accepted: %v", err)
			}
			if catalog.tenant != "" {
				t.Fatal("catalog queried for an unowned namespace")
			}
		})
	}
}
