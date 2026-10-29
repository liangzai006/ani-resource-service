package sandbox

import (
	"context"
	"fmt"
	"strings"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// StorageClasses is the existing Storage use case's read-only catalog.
type StorageClasses interface {
	ListStorageClasses(context.Context, string) ([]storage.Class, error)
}

type StorageNamespaces interface {
	GetNamespace(context.Context, string) (storage.Namespace, error)
}

// StorageClass selects the unambiguous platform default without inventing a
// separate Sandbox policy. Client verifies the underlying object because the
// Storage catalog normalizes an absent reclaim policy to Delete, whereas the
// Sandbox provider requires the policy to be explicitly Delete.
type StorageClass struct {
	Classes         StorageClasses
	Namespaces      StorageNamespaces
	NamespacePrefix string
	Client          dynamic.Interface
}

func (s StorageClass) ResolveSandboxStorageClass(ctx context.Context, tenant string) (string, error) {
	if !biz.UUID(tenant) {
		return "", biz.ErrInvalid
	}
	if s.Classes == nil || s.Namespaces == nil || s.NamespacePrefix == "" || s.Client == nil {
		return "", biz.ErrDependencyUnavailable
	}
	name, err := storage.NamespaceName(s.NamespacePrefix, tenant)
	if err != nil {
		return "", biz.ErrDependencyUnavailable
	}
	namespace, err := s.Namespaces.GetNamespace(ctx, name)
	if err != nil {
		return "", fmt.Errorf("read sandbox tenant namespace: %w", err)
	}
	if namespace.Name != name || namespace.Deleting || namespace.ManagedBy != storage.NamespaceOwner || namespace.TenantID != tenant {
		return "", fmt.Errorf("%w: sandbox tenant namespace is not usable", biz.ErrDependencyUnavailable)
	}
	classes, err := s.Classes.ListStorageClasses(ctx, tenant)
	if err != nil {
		return "", fmt.Errorf("list platform storage classes: %w", err)
	}
	var selected storage.Class
	defaults := 0
	for _, class := range classes {
		if class.Default {
			selected = class
			defaults++
		}
	}
	if defaults != 1 || selected.Name == "" || selected.Name != strings.TrimSpace(selected.Name) || selected.ReclaimPolicy != "Delete" {
		return "", fmt.Errorf("%w: platform default storage class is ambiguous or lacks Delete reclaim policy", biz.ErrDependencyUnavailable)
	}
	class, err := s.Client.Resource(storageClassGVR).Get(ctx, selected.Name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read platform default storage class: %w", err)
	}
	policy, found, fieldErr := unstructured.NestedString(class.Object, "reclaimPolicy")
	annotations := class.GetAnnotations()
	stillDefault := annotations["storageclass.kubernetes.io/is-default-class"] == "true" || annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"
	if fieldErr != nil || !found || policy != "Delete" || class.GetDeletionTimestamp() != nil || !stillDefault {
		return "", fmt.Errorf("%w: platform default storage class is no longer usable", biz.ErrDependencyUnavailable)
	}
	return selected.Name, nil
}

var _ biz.StorageClassResolver = StorageClass{}
