package data

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var kcVPCs = schema.GroupVersionResource{Group: "networking.kubercloud.com", Version: "v1", Resource: "vpcs"}
var kcSubnets = schema.GroupVersionResource{Group: "networking.kubercloud.com", Version: "v1", Resource: "subnets"}
var namespaces = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

const ownerLabel = "network.ani.io/managed-by"
const tenantLabel = "network.ani.io/tenant-id"
const resourceLabel = "network.ani.io/resource-id"
const bindingLabel = "network.ani.io/binding-id"

// KCProvider implements only the fixed networking.kubercloud.com/v1 VPC
// contract. It does not import the kc repository or operate on OVN.
type KCProvider struct {
	vmAutoNetwork            *VMAutoNetworkConfig
	vmNetworks               map[string]ManagedVMNAD
	loadBalancerInstallation *LoadBalancerInstallation
	repository               *Postgres
	client                   dynamic.Interface
	watchClient              dynamic.Interface
	observation              *KCObservation
	io                       *providerIO
}

type KCClientPolicy struct {
	QPS   float32
	Burst int
}

func OpenKCProvider(repository *Postgres, kubeconfig string, policy ...KCClientPolicy) (*KCProvider, error) {
	var config *rest.Config
	var err error
	if kubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("Kubernetes credentials/configuration could not be loaded")
	}
	if len(policy) > 1 {
		return nil, fmt.Errorf("invalid Kubernetes request budget")
	}
	if len(policy) == 1 {
		config.QPS = policy[0].QPS
		config.Burst = policy[0].Burst
	}
	return NewKCProvider(repository, config)
}

func NewKCProvider(repository *Postgres, config *rest.Config) (*KCProvider, error) {
	if repository == nil || config == nil {
		return nil, fmt.Errorf("PostgreSQL mapping and Kubernetes configuration required")
	}
	copy := rest.CopyConfig(config)
	copy.Timeout = 10 * time.Second
	copy.UserAgent = "ani-network-service/net-05a"
	stats := &providerIO{values: map[string]float64{}}
	previousWrap := copy.WrapTransport
	copy.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		if previousWrap != nil {
			rt = previousWrap(rt)
		}
		return measuredTransport{rt, stats}
	}
	client, err := dynamic.NewForConfig(copy)
	if err != nil {
		return nil, fmt.Errorf("Kubernetes client configuration invalid")
	}
	watchConfig := rest.CopyConfig(copy)
	watchConfig.Timeout = 0
	watchClient, err := dynamic.NewForConfig(watchConfig)
	if err != nil {
		return nil, fmt.Errorf("Kubernetes watch configuration invalid")
	}
	return &KCProvider{repository: repository, client: client, watchClient: watchClient, io: stats}, nil
}

func (p *KCProvider) binding(ctx context.Context, target biz.ProviderTarget) (sqlcgen.NetworkProviderBinding, error) {
	value, err := p.repository.queries.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: target.TenantID, ResourceID: target.ResourceID})
	if err != nil {
		return value, &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: err}
	}
	if (target.Kind != "" && target.Kind != value.ResourceKind) || value.BindingID != target.BindingID || value.ClusterID != p.repository.placement.ClusterID ||
		(value.ProviderUid != "" && target.KnownIdentity != "" && value.ProviderUid != target.KnownIdentity) {
		return value, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return value, nil
}

func (p *KCProvider) observeDirect(ctx context.Context, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	binding, err := p.binding(ctx, target)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	object, err := p.client.Resource(kcResource(binding)).Namespace(binding.Namespace).Get(ctx, binding.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		dependencies, err := p.hasDependencies(ctx, binding)
		return biz.ProviderObservation{HasDependencies: dependencies}, err
	}
	if err != nil {
		return biz.ProviderObservation{}, readFailure(err)
	}
	return p.inspect(ctx, object, binding, target)
}

func inspectResource(object *unstructured.Unstructured, binding sqlcgen.NetworkProviderBinding, target biz.ProviderTarget, parentRef string) (biz.ProviderObservation, error) {
	labels := object.GetLabels()
	expectedKind := "VPC"
	if binding.ResourceKind == "subnet" {
		expectedKind = "Subnet"
	}
	if object.GetAPIVersion() != "networking.kubercloud.com/v1" || object.GetKind() != expectedKind {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	if object.GetName() != binding.ProviderName || object.GetNamespace() != binding.Namespace || object.GetUID() == "" || object.GetResourceVersion() == "" ||
		labels[ownerLabel] != "ani-network-service" || labels[tenantLabel] != target.TenantID || labels[resourceLabel] != target.ResourceID || labels[bindingLabel] != binding.BindingID ||
		(target.KnownIdentity != "" && string(object.GetUID()) != target.KnownIdentity) ||
		(binding.ProviderUid != "" && string(object.GetUID()) != binding.ProviderUid) {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	cidr, _, _ := unstructured.NestedString(object.Object, "spec", "cidrBlock")
	ip, _, _ := unstructured.NestedString(object.Object, "spec", "ipVersion")
	allowed, _, _ := unstructured.NestedString(object.Object, "spec", "allowedNamespaces", "from")
	route, _, _ := unstructured.NestedString(object.Object, "spec", "routeTable")
	routes, _, _ := unstructured.NestedSlice(object.Object, "spec", "policyRoutes")
	selector, _, _ := unstructured.NestedMap(object.Object, "spec", "allowedNamespaces", "selector")
	if cidr != target.CIDR || ip != "IPv4" || allowed != "Same" || route != "" || len(routes) > 0 || len(selector) > 0 {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	if binding.ResourceKind == "subnet" {
		kind, _, _ := unstructured.NestedString(object.Object, "spec", "type")
		parent, _, _ := unstructured.NestedString(object.Object, "spec", "gateway")
		gateway, _, _ := unstructured.NestedString(object.Object, "spec", "gatewayIP")
		underlay, _, _ := unstructured.NestedMap(object.Object, "spec", "underlayConfig")
		exclusions, _, _ := unstructured.NestedSlice(object.Object, "spec", "excludeIPs")
		nat, _, _ := unstructured.NestedBool(object.Object, "spec", "natOutgoing")
		if kind != "VPC" || parentRef == "" || parent != parentRef || gateway != target.Gateway || len(underlay) > 0 || len(exclusions) > 0 || nat {
			return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
	}
	observed, _, _ := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	router, _, _ := unstructured.NestedString(object.Object, "status", "boundResources", "router")
	if binding.ResourceKind == "subnet" {
		switchName, _, _ := unstructured.NestedString(object.Object, "status", "boundResources", "switch")
		port, _, _ := unstructured.NestedString(object.Object, "status", "boundResources", "gatewayPort")
		router = ""
		if switchName != "" && port != "" {
			router = switchName
		}
	}
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	good := map[string]bool{}
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := condition["type"].(string)
		generation, _, _ := unstructured.NestedInt64(condition, "observedGeneration")
		// The fixed kc helper does not populate condition.observedGeneration;
		// use status.observedGeneration, and reject a contradictory nonzero value.
		good[kind] = condition["status"] == "True" && (generation == 0 || generation == object.GetGeneration())
	}
	subnets, _, _ := unstructured.NestedMap(object.Object, "status", "subnets")
	eips, _, _ := unstructured.NestedMap(object.Object, "status", "eips")
	used, _, _ := unstructured.NestedInt64(object.Object, "status", "v4usingIPs")
	ranges := biz.SubnetIPRanges{}
	if binding.ResourceKind == "subnet" && object.GetGeneration() > 0 && observed == object.GetGeneration() {
		ranges.Available = subnetIPRange(object, "v4availableIPrange")
		ranges.Used = subnetIPRange(object, "v4usingIPrange")
	}
	return biz.ProviderObservation{SubnetIPRanges: ranges, Exists: true, Identity: string(object.GetUID()), HasDependencies: len(subnets) > 0 || len(eips) > 0 || used > 0,
		Ready: object.GetDeletionTimestamp() == nil && object.GetGeneration() > 0 && observed == object.GetGeneration() && router != "" && good["Valid"] && good["Initialized"] && good["Ready"]}, nil
}

// subnetIPRange preserves the provider's comma-separated IPv4 ranges verbatim.
// An empty string is a known empty set; a missing/non-string field is unknown.
func subnetIPRange(object *unstructured.Unstructured, field string) *string {
	value, found, err := unstructured.NestedString(object.Object, "status", field)
	if err != nil || !found {
		return nil
	}
	return &value
}

func (p *KCProvider) EnsureVPC(ctx context.Context, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	target.Kind = "vpc"
	return p.ensure(ctx, target)
}
func (p *KCProvider) EnsureSubnet(ctx context.Context, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	target.Kind = "subnet"
	return p.ensure(ctx, target)
}
func (p *KCProvider) ensure(ctx context.Context, target biz.ProviderTarget) (biz.ProviderObservation, error) {
	binding, err := p.binding(ctx, target)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	if err := p.ensureNamespace(ctx, binding); err != nil {
		return biz.ProviderObservation{}, err
	}
	resource := p.client.Resource(kcResource(binding)).Namespace(binding.Namespace)
	existing, err := resource.Get(ctx, binding.ProviderName, metav1.GetOptions{})
	if err == nil {
		return p.inspect(ctx, existing, binding, target)
	}
	if !apierrors.IsNotFound(err) {
		return biz.ProviderObservation{}, readFailure(err)
	}
	if target.KnownIdentity != "" || binding.ProviderUid != "" {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	if binding.ResourceKind == "vpc" {
		tx, err := p.repository.pool.Begin(ctx)
		if err != nil {
			return biz.ProviderObservation{}, readFailure(err)
		}
		defer tx.Rollback(ctx)
		q := p.repository.queries.WithTx(tx)
		if err = q.LockPlatformCluster(ctx, sqlcgen.LockPlatformClusterParams{ClusterID: binding.ClusterID}); err != nil {
			return biz.ProviderObservation{}, readFailure(err)
		}
		// Accepted requests survive later whitelist changes; conflict facts are rechecked immediately before first POST.
		if err = p.repository.validateVPCPlatformCIDR(ctx, q, target.CIDR); err != nil {
			return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: err}
		}
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.kubercloud.com/v1", "kind": "VPC",
		"metadata": map[string]any{"name": binding.ProviderName, "namespace": binding.Namespace, "labels": map[string]any{
			ownerLabel: "ani-network-service", tenantLabel: target.TenantID, resourceLabel: target.ResourceID, bindingLabel: binding.BindingID}},
		"spec": map[string]any{"cidrBlock": target.CIDR, "ipVersion": "IPv4", "allowedNamespaces": map[string]any{"from": "Same"}},
	}}
	if binding.ResourceKind == "subnet" {
		parent, err := p.parentReference(ctx, binding, target)
		if err != nil {
			return biz.ProviderObservation{}, err
		}
		object.SetKind("Subnet")
		object.Object["spec"] = map[string]any{"type": "VPC", "ipVersion": "IPv4", "cidrBlock": target.CIDR, "gateway": parent, "gatewayIP": target.Gateway, "allowedNamespaces": map[string]any{"from": "Same"}}
	}
	created, err := resource.Create(ctx, object, metav1.CreateOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
	if apierrors.IsAlreadyExists(err) {
		created, err = resource.Get(ctx, binding.ProviderName, metav1.GetOptions{})
		if err != nil {
			return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
		}
	}
	if err != nil {
		return biz.ProviderObservation{}, mutationFailure(err)
	}
	// A malformed success cannot disprove the POST. Preserve pending_create
	// until a later GET supplies an owned object and stable UID.
	value, err := p.inspect(ctx, created, binding, target)
	if err != nil {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
	}
	return value, nil
}

func (p *KCProvider) ensureNamespace(ctx context.Context, binding sqlcgen.NetworkProviderBinding) error {
	resource := p.client.Resource(namespaces)
	value, err := resource.Get(ctx, binding.Namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		value, err = resource.Create(ctx, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{"name": binding.Namespace, "labels": map[string]any{ownerLabel: "ani-network-service", tenantLabel: binding.TenantID}}}}, metav1.CreateOptions{FieldManager: "ani-network-service"})
		if apierrors.IsAlreadyExists(err) {
			value, err = resource.Get(ctx, binding.Namespace, metav1.GetOptions{})
		}
	}
	// No VPC POST was sent yet; namespace creation can be re-observed safely.
	if err != nil {
		return readFailure(err)
	}
	if value.GetName() != binding.Namespace || value.GetDeletionTimestamp() != nil || value.GetLabels()[tenantLabel] != binding.TenantID || value.GetLabels()[ownerLabel] != "ani-network-service" {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return nil
}

func (p *KCProvider) Delete(ctx context.Context, target biz.ProviderTarget) error {
	if egressKind(target.Kind) {
		return p.deleteEgress(ctx, target)
	}
	binding, err := p.binding(ctx, target)
	if err != nil {
		return err
	}
	if target.KnownIdentity == "" {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	resource := p.client.Resource(kcResource(binding)).Namespace(binding.Namespace)
	object, err := resource.Get(ctx, binding.ProviderName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return readFailure(err)
	}
	observed, err := p.inspect(ctx, object, binding, target)
	if err != nil {
		return err
	}
	if observed.HasDependencies {
		return &biz.ProviderError{Kind: biz.ProviderInUse}
	}
	dependencies, err := p.hasDependencies(ctx, binding)
	if err != nil {
		return err
	}
	if dependencies {
		return &biz.ProviderError{Kind: biz.ProviderInUse}
	}
	uid := types.UID(target.KnownIdentity)
	revision := object.GetResourceVersion()
	orphan := metav1.DeletePropagationOrphan
	err = resource.Delete(ctx, binding.ProviderName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &revision}, PropagationPolicy: &orphan})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return mutationFailure(err)
	}
	return nil
}

func readFailure(err error) error { return &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: err} }
func mutationFailure(err error) error {
	kind := biz.ProviderUncertain
	switch {
	case apierrors.IsConflict(err):
		kind = biz.ProviderConflict
	case apierrors.IsInvalid(err) || apierrors.IsBadRequest(err):
		kind = biz.ProviderReject
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsTooManyRequests(err):
		kind = biz.ProviderTemporary
	default:
		var status apierrors.APIStatus
		if errors.As(err, &status) && status.Status().Code == http.StatusRequestEntityTooLarge {
			kind = biz.ProviderReject
		}
	}
	return &biz.ProviderError{Kind: kind, Cause: err}
}

func kcResource(b sqlcgen.NetworkProviderBinding) schema.GroupVersionResource {
	if b.ResourceKind == "subnet" {
		return kcSubnets
	}
	return kcVPCs
}
func (p *KCProvider) parentReference(ctx context.Context, b sqlcgen.NetworkProviderBinding, t biz.ProviderTarget) (string, error) {
	parent, err := p.repository.queries.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: t.TenantID, ResourceID: t.VPCID})
	if err != nil {
		return "", readFailure(err)
	}
	if parent.ResourceKind != "vpc" || parent.ClusterID != b.ClusterID || parent.Namespace != b.Namespace || parent.ProviderUid == "" {
		return "", &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return parent.Namespace + "/" + parent.ProviderName, nil
}
func (p *KCProvider) inspect(ctx context.Context, o *unstructured.Unstructured, b sqlcgen.NetworkProviderBinding, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	parent := ""
	if b.ResourceKind == "subnet" {
		var err error
		parent, err = p.parentReference(ctx, b, t)
		if err != nil {
			return biz.ProviderObservation{}, err
		}
	}
	return inspectResource(o, b, t, parent)
}

// Inspect status and actual references independently. A missing Subnet CR does
// not prove its VNic/VNicIP resources have been released.
func (p *KCProvider) hasDependencies(ctx context.Context, b sqlcgen.NetworkProviderBinding) (bool, error) {
	if b.ResourceKind == "vpc" {
		count, err := p.repository.queries.CountBlockingSubnets(ctx, sqlcgen.CountBlockingSubnetsParams{TenantID: b.TenantID, VpcID: textValue(b.VpcID)})
		if err != nil {
			return false, readFailure(err)
		}
		if count > 0 {
			return true, nil
		}
	}
	if b.ResourceKind == "subnet" {
		count, err := p.repository.queries.CountAttachments(ctx, sqlcgen.CountAttachmentsParams{TenantID: b.TenantID, SubnetID: textValue(b.SubnetID)})
		if err != nil {
			return false, readFailure(err)
		}
		if count != 0 {
			return true, nil
		}
	}
	if p.observation != nil {
		return p.completeDependencies(ctx, b)
	}
	resources := []string{"subnets"}
	field := "gateway"
	if b.ResourceKind == "subnet" {
		resources = []string{"vnics", "vnicips", "eips"}
		field = "subnet"
	}
	for _, kind := range resources {
		continuation := ""
		for {
			children, err := p.client.Resource(schema.GroupVersionResource{Group: kcVPCs.Group, Version: kcVPCs.Version, Resource: kind}).List(ctx, metav1.ListOptions{Limit: 500, Continue: continuation})
			if err != nil {
				return false, readFailure(err)
			}
			for _, child := range children.Items {
				ref, _, _ := unstructured.NestedString(child.Object, "spec", field)
				if ref != "" && !strings.Contains(ref, "/") {
					ref = child.GetNamespace() + "/" + ref
				}
				if ref == b.Namespace+"/"+b.ProviderName {
					return true, nil
				}
			}
			continuation = children.GetContinue()
			if continuation == "" {
				break
			}
		}
	}
	return false, nil
}
