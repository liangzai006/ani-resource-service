package data

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var secretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
var namespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// The tenant label is the one ModelDev's webhook policy writes and checks. The
// managed-by label comes from each deployment binding's owner_label allowlist.
const (
	pullSecretTenantLabel    = "ani.io/resource-tenant-id"
	pullSecretManagedByLabel = "ani.io/managed-by"
	pullSecretCredentialAnn  = "image.ani.io/credential-generation"
	pullSecretWriterIdentity = "ani-resource-service"
	pullSecretDataKey        = ".dockerconfigjson"
	pullSecretRequestBudget  = 15 * time.Second
)

// PullSecrets is Resource's single Kubernetes writer for shared tenant pull
// Secrets. It never returns plaintext and refuses objects it does not own.
type PullSecrets struct {
	client    dynamic.Interface
	clusterID string
}

// NewPullSecrets opens the writer for the one cluster Image may write into.
// clusterID is the configured deployment identity compared against every
// request; an empty or mismatched value rejects the write.
func NewPullSecrets(clusterID, kubeconfig string) (*PullSecrets, error) {
	if clusterID == "" {
		return nil, fmt.Errorf("pull secret writer requires a configured cluster identity")
	}
	var config *rest.Config
	var err error
	if kubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("pull secret writer Kubernetes configuration unavailable")
	}
	copy := rest.CopyConfig(config)
	copy.Timeout = pullSecretRequestBudget
	copy.UserAgent = pullSecretWriterIdentity
	client, err := dynamic.NewForConfig(copy)
	if err != nil {
		return nil, fmt.Errorf("pull secret writer Kubernetes client invalid")
	}
	return &PullSecrets{client: client, clusterID: clusterID}, nil
}

// EnsurePullSecret creates or updates the shared Secret and returns the
// generation now stored. A newer existing generation is never downgraded and an
// object owned by another manager is never overwritten.
func (p *PullSecrets) EnsurePullSecret(ctx context.Context, target imagebiz.PullSecretTarget, secret imagebiz.Secret) (int64, error) {
	if len(secret) == 0 || target.Generation <= 0 || target.SecretName == "" || target.Namespace == "" || target.ClusterID != p.clusterID {
		return 0, imagebiz.Fail(imagebiz.InvalidArgument, "pull secret write target rejected")
	}
	config, err := json.Marshal(map[string]any{"auths": map[string]any{target.RegistryAuthority: map[string]string{"username": target.Username, "password": string(secret)}}})
	if err != nil {
		return 0, imagebiz.Fail(imagebiz.InternalError, "pull secret encoding failed")
	}
	defer clear(config)
	api := p.client.Resource(secretGVR).Namespace(target.Namespace)
	owned := map[string]string{
		"image.ani.io/tenant-id": target.TenantID,
		"image.ani.io/space-id":  target.SpaceID,
		pullSecretManagedByLabel: pullSecretWriterIdentity,
	}
	current, getErr := api.Get(ctx, target.SecretName, metav1.GetOptions{})
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return 0, imagebiz.Fail(imagebiz.DependencyUnavailable, "pull secret read failed")
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name": target.SecretName, "namespace": target.Namespace,
			"labels":      stringMap(owned),
			"annotations": map[string]any{pullSecretCredentialAnn: strconv.FormatInt(target.Generation, 10)},
		},
		"type": "kubernetes.io/dockerconfigjson",
		"data": map[string]any{pullSecretDataKey: base64.StdEncoding.EncodeToString(config)},
	}}
	if getErr == nil {
		if reason := p.ownershipMismatch(current, target); reason != "" {
			return 0, imagebiz.Fail(imagebiz.PermissionDenied, reason)
		}
		object.SetUID(current.GetUID())
		object.SetResourceVersion(current.GetResourceVersion())
		if _, err = api.Update(ctx, object, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				return 0, imagebiz.Fail(imagebiz.DependencyUnavailable, "pull secret concurrent update")
			}
			return 0, imagebiz.Fail(imagebiz.DependencyUnavailable, "pull secret update failed")
		}
		return target.Generation, nil
	}
	if _, err = api.Create(ctx, object, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return 0, imagebiz.Fail(imagebiz.DependencyUnavailable, "pull secret concurrent create")
		}
		return 0, imagebiz.Fail(imagebiz.DependencyUnavailable, "pull secret create failed")
	}
	return target.Generation, nil
}

// ownershipMismatch rejects foreign objects and newer credential generations.
func (p *PullSecrets) ownershipMismatch(current *unstructured.Unstructured, target imagebiz.PullSecretTarget) string {
	labels := current.GetLabels()
	if labels["image.ani.io/tenant-id"] != target.TenantID || labels["image.ani.io/space-id"] != target.SpaceID ||
		labels[pullSecretManagedByLabel] != pullSecretWriterIdentity {
		return "pull secret is owned by another manager"
	}
	if current.GetAnnotations()[pullSecretCredentialAnn] == "" {
		// A pre-unification instance writer used the same scope but a different
		// name, so an object here without our generation annotation is foreign.
		return "pull secret carries no managed generation"
	}
	existing, err := strconv.ParseInt(current.GetAnnotations()[pullSecretCredentialAnn], 10, 64)
	if err != nil || existing > target.Generation {
		return "pull secret generation would regress"
	}
	return ""
}

// VerifyNamespace confirms the real namespace name, UID and labels. Label
// expectations are per caller: training requires the tenant label and one of
// the binding owner labels; inference only proves the deployment allowlist.
func (p *PullSecrets) VerifyNamespace(ctx context.Context, claim imagebiz.NamespaceClaim) error {
	if claim.ClusterID != p.clusterID || claim.Namespace == "" {
		return imagebiz.Fail(imagebiz.InvalidArgument, "namespace target rejected")
	}
	if !claim.Verify {
		// Resource's own instance path already resolved this namespace from the
		// tenant UUID; the cluster comparison above is the remaining guard.
		return nil
	}
	if claim.NamespaceUID == "" {
		return imagebiz.Fail(imagebiz.InvalidArgument, "namespace UID required")
	}
	object, err := p.client.Resource(namespaceGVR).Get(ctx, claim.Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return imagebiz.Fail(imagebiz.PermissionDenied, "target namespace is not registered")
	}
	if err != nil {
		return imagebiz.Fail(imagebiz.DependencyUnavailable, "namespace read failed")
	}
	if object.GetDeletionTimestamp() != nil || string(object.GetUID()) != claim.NamespaceUID {
		return imagebiz.Fail(imagebiz.PermissionDenied, "target namespace identity mismatch")
	}
	labels := object.GetLabels()
	if claim.RequireTenantLabel && labels[pullSecretTenantLabel] != claim.TenantID {
		return imagebiz.Fail(imagebiz.PermissionDenied, "target namespace tenant label mismatch")
	}
	if len(claim.OwnerLabels) > 0 {
		managed := labels[pullSecretManagedByLabel]
		allowed := false
		for _, value := range claim.OwnerLabels {
			if value == managed {
				allowed = true
				break
			}
		}
		if !allowed {
			return imagebiz.Fail(imagebiz.PermissionDenied, "target namespace owner label mismatch")
		}
	}
	return nil
}

func stringMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
