package sandbox

import (
	"context"
	"errors"
	"fmt"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// TenantNamespace uses the shared Network namespace ownership convention.
// Only initial specification resolution may initialize a missing namespace;
// acceptance and worker operations check the previously frozen UID.
type TenantNamespace struct {
	Client          dynamic.Interface
	NamespacePrefix string
}

func (n TenantNamespace) ResolveSandboxNamespaceUID(ctx context.Context, tenant string) (string, error) {
	return n.namespaceUID(ctx, tenant, false)
}

func (n TenantNamespace) EnsureSandboxNamespaceUID(ctx context.Context, tenant string) (string, error) {
	return n.namespaceUID(ctx, tenant, true)
}

func (n TenantNamespace) namespaceUID(ctx context.Context, tenant string, ensure bool) (string, error) {
	if n.Client == nil || !biz.UUID(tenant) {
		return "", biz.ErrDependencyUnavailable
	}
	name, err := storage.NamespaceName(n.NamespacePrefix, tenant)
	if err != nil {
		return "", biz.ErrDependencyUnavailable
	}
	resource := n.Client.Resource(namespaceGVR)
	namespace, err := resource.Get(ctx, name, metav1.GetOptions{})
	if ensure && apierrors.IsNotFound(err) {
		namespace, err = resource.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{"name": name, "labels": map[string]any{
				storage.NamespaceManagedBy: storage.NamespaceOwner, storage.NamespaceTenantID: tenant,
			}},
		}}, metav1.CreateOptions{FieldManager: "ani-network-service"})
		if err != nil {
			// A concurrent create or an uncertain response is resolved only by
			// observing the fixed name again; never issue a second create here.
			namespace, err = resource.Get(ctx, name, metav1.GetOptions{})
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, context.Canceled) {
			return "", context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "", context.DeadlineExceeded
		}
		return "", biz.ErrDependencyUnavailable
	}
	if namespace == nil {
		return "", biz.ErrDependencyUnavailable
	}
	labels := namespace.GetLabels()
	uid := string(namespace.GetUID())
	if namespace.GetName() != name || uid == "" || namespace.GetDeletionTimestamp() != nil || labels[storage.NamespaceManagedBy] != storage.NamespaceOwner || labels[storage.NamespaceTenantID] != tenant {
		return "", fmt.Errorf("%w: sandbox tenant namespace is not usable", biz.ErrOwnership)
	}
	return uid, nil
}

var _ biz.NamespaceResolver = TenantNamespace{}
