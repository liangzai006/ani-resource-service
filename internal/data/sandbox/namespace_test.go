package sandbox

import (
	"context"
	"testing"

	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestTenantNamespaceResolveRequiresOwnedActiveUID(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		name     string
		uid      string
		owner    string
		tenant   string
		deleting bool
		ok       bool
	}{
		{"valid", "uid-1", storage.NamespaceOwner, tenant, false, true},
		{"missing UID", "", storage.NamespaceOwner, tenant, false, false},
		{"foreign owner", "uid-1", "other", tenant, false, false},
		{"foreign tenant", "uid-1", storage.NamespaceOwner, "other", false, false},
		{"deleting", "uid-1", storage.NamespaceOwner, tenant, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant-" + tenant, "uid": tc.uid, "labels": map[string]any{storage.NamespaceManagedBy: tc.owner, storage.NamespaceTenantID: tc.tenant}}}}
			if tc.deleting {
				now := metav1.Now()
				ns.SetDeletionTimestamp(&now)
			}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), ns)
			uid, err := (TenantNamespace{Client: client, NamespacePrefix: "tenant-"}).ResolveSandboxNamespaceUID(context.Background(), tenant)
			if (err == nil) != tc.ok || (tc.ok && uid != tc.uid) {
				t.Fatalf("uid=%q err=%v", uid, err)
			}
		})
	}
}
