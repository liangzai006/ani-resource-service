package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

const vmNetworkTemplate = "kcn-single-plugin-v1"
const vmManagedNADName = "ani-vm-primary-v1"
const vmManagementLabel = "network.ani.io/vm-network-id"
const vmTemplateLabel = "network.ani.io/vm-template"

var vmServiceAccounts = schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}

// VMAutoNetworkConfig is trusted deployment configuration, never tenant input.
// The NAD is shared by the tenant's Subnets; the actual Subnet reference always
// comes from the tenant-scoped, persisted KCN binding selected for the VM.
type VMAutoNetworkConfig struct {
	ServiceAccount string
	ServerSocket   string
}

func (p *KCProvider) ConfigureVMAutoNetwork(config VMAutoNetworkConfig) error {
	if config.ServiceAccount == "" {
		config.ServiceAccount = "ani-vm-workload"
	}
	if config.ServerSocket == "" {
		config.ServerSocket = "/run/openvswitch/kc-networking-daemon.sock"
	}
	if len(validation.IsDNS1123Subdomain(config.ServiceAccount)) != 0 || !filepath.IsAbs(config.ServerSocket) || filepath.Clean(config.ServerSocket) != config.ServerSocket || strings.ContainsAny(config.ServerSocket, "\r\n\x00") {
		return fmt.Errorf("invalid automatic VM network configuration")
	}
	p.vmAutoNetwork = &config
	return nil
}

func automaticVMNADConfig(config VMAutoNetworkConfig) string {
	raw, _ := json.Marshal(map[string]string{"cniVersion": "0.3.1", "name": "kc-networking", "type": "kc-networking", "server_socket": config.ServerSocket})
	return string(raw)
}

// PrepareVMNetwork is called only by Network's durable worker after a healthy
// Subnet observation. It never creates VM/Pod resources or adopts existing NADs.
func (p *KCProvider) PrepareVMNetwork(ctx context.Context, target biz.ProviderTarget) (result error) {
	if p.vmAutoNetwork == nil || target.Kind != "subnet" {
		return nil
	}
	binding, err := p.binding(ctx, target)
	if err != nil {
		return err
	}
	if binding.ResourceKind != "subnet" || binding.ProviderUid == "" {
		return biz.Fail(biz.NetworkNotReady, "VM Subnet identity unavailable")
	}
	// Namespace creation remains part of the existing VPC/Subnet lifecycle.
	// This ancillary preparation never recreates a deleted Namespace.
	ns, err := p.client.Resource(namespaces).Get(ctx, binding.Namespace, metav1.GetOptions{})
	if err != nil {
		return readFailure(err)
	}
	if ns.GetUID() == "" || ns.GetDeletionTimestamp() != nil || ns.GetLabels()[tenantLabel] != binding.TenantID || ns.GetLabels()[ownerLabel] != "ani-network-service" {
		return biz.Fail(biz.AttachmentConflict, "managed VM namespace ownership changed")
	}
	raw := automaticVMNADConfig(*p.vmAutoNetwork)
	digest, err := VMNADConfigDigest(raw)
	if err != nil {
		return err
	}
	if err = p.repository.queries.ReserveVMNamespace(ctx, sqlcgen.ReserveVMNamespaceParams{TenantID: binding.TenantID, ClusterID: binding.ClusterID, Namespace: binding.Namespace, NamespaceUid: string(ns.GetUID()), ManagementID: uuid.NewString(), NadName: vmManagedNADName, NadConfig: raw, NadConfigDigest: digest, ServiceAccount: p.vmAutoNetwork.ServiceAccount}); err != nil {
		return databaseFailure(err)
	}
	record, err := p.repository.queries.GetVMNamespace(ctx, sqlcgen.GetVMNamespaceParams{TenantID: binding.TenantID, ClusterID: binding.ClusterID})
	if err != nil {
		return databaseFailure(err)
	}
	failureScope := record
	defer func() {
		if result != nil {
			// A generic reason is enough for admission and avoids persisting any
			// transport details. The next due Subnet work retries this same intent.
			_ = p.repository.queries.FailVMNamespacePreparation(ctx, sqlcgen.FailVMNamespacePreparationParams{TenantID: failureScope.TenantID, ClusterID: failureScope.ClusterID, ManagementID: failureScope.ManagementID, Reason: "VM_NETWORK_PREPARATION_FAILED"})
		}
	}()
	if record.Namespace != binding.Namespace || record.NamespaceUid == "" || record.NamespaceUid != string(ns.GetUID()) || record.TemplateID != vmNetworkTemplate || record.NadName != vmManagedNADName || record.NadConfigDigest != digest || record.ServiceAccount != p.vmAutoNetwork.ServiceAccount {
		return biz.Fail(biz.AttachmentConflict, "managed VM namespace or template identity changed")
	}
	nad, err := p.ensureVMNetworkObject(ctx, record, vmNADs, "NetworkAttachmentDefinition", record.NadName, record.NadUid)
	if err != nil {
		return err
	}
	record, err = p.repository.queries.SaveVMNamespaceObjectIdentity(ctx, sqlcgen.SaveVMNamespaceObjectIdentityParams{TenantID: record.TenantID, ClusterID: record.ClusterID, ManagementID: record.ManagementID, ObjectKind: "nad", ObjectUid: string(nad.GetUID())})
	if err != nil {
		return databaseFailure(err)
	}
	sa, err := p.ensureVMNetworkObject(ctx, record, vmServiceAccounts, "ServiceAccount", record.ServiceAccount, record.ServiceAccountUid)
	if err != nil {
		return err
	}
	_, err = p.repository.queries.SaveVMNamespaceObjectIdentity(ctx, sqlcgen.SaveVMNamespaceObjectIdentityParams{TenantID: record.TenantID, ClusterID: record.ClusterID, ManagementID: record.ManagementID, ObjectKind: "service_account", ObjectUid: string(sa.GetUID())})
	if err != nil {
		return databaseFailure(err)
	}
	return nil
}

func (p *KCProvider) ensureVMNetworkObject(ctx context.Context, record sqlcgen.NetworkVmNamespace, gvr schema.GroupVersionResource, kind, name, uid string) (*unstructured.Unstructured, error) {
	resource := p.client.Resource(gvr).Namespace(record.Namespace)
	value, err := resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && uid == "" {
		object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": gvr.GroupVersion().String(), "kind": kind,
			"metadata": map[string]any{"namespace": record.Namespace, "name": name, "labels": map[string]any{ownerLabel: "ani-network-service", tenantLabel: record.TenantID, vmManagementLabel: record.ManagementID, vmTemplateLabel: record.TemplateID}}}}
		if kind == "NetworkAttachmentDefinition" {
			object.Object["spec"] = map[string]any{"config": record.NadConfig}
		} else {
			object.Object["automountServiceAccountToken"] = false
		}
		value, err = resource.Create(ctx, object, metav1.CreateOptions{FieldManager: "ani-network-service"})
		if apierrors.IsAlreadyExists(err) {
			value, err = resource.Get(ctx, name, metav1.GetOptions{})
		}
	}
	if err != nil {
		return nil, readFailure(err)
	}
	if err = verifyVMNetworkObject(value, record, kind, name, uid); err != nil {
		return nil, err
	}
	return value, nil
}

func verifyVMNetworkObject(value *unstructured.Unstructured, record sqlcgen.NetworkVmNamespace, kind, name, uid string) error {
	expectedAPI := "v1"
	if kind == "NetworkAttachmentDefinition" {
		expectedAPI = "k8s.cni.cncf.io/v1"
	}
	labels := value.GetLabels()
	if value.GetAPIVersion() != expectedAPI || value.GetKind() != kind || value.GetNamespace() != record.Namespace || value.GetName() != name || value.GetUID() == "" || value.GetDeletionTimestamp() != nil || (uid != "" && string(value.GetUID()) != uid) || labels[ownerLabel] != "ani-network-service" || labels[tenantLabel] != record.TenantID || labels[vmManagementLabel] != record.ManagementID || labels[vmTemplateLabel] != record.TemplateID {
		return biz.Fail(biz.AttachmentConflict, "managed VM network object ownership or identity changed")
	}
	if kind == "NetworkAttachmentDefinition" {
		raw, _, _ := unstructured.NestedString(value.Object, "spec", "config")
		digest, err := VMNADConfigDigest(raw)
		if err != nil || validateVMSinglePluginNAD(raw) != nil || digest != record.NadConfigDigest {
			return biz.Fail(biz.AttachmentConflict, "managed VM NAD template changed")
		}
	} else {
		automount, found, err := unstructured.NestedBool(value.Object, "automountServiceAccountToken")
		if err != nil || !found || automount {
			return biz.Fail(biz.AttachmentConflict, "managed VM ServiceAccount configuration changed")
		}
	}
	return nil
}

// LookupManagedVMNetwork is read-only. The caller must still run the existing
// ResolveVMNetworkSelection checks before admitting a create specification.
func (p *KCProvider) LookupManagedVMNetwork(ctx context.Context, tenant, vpc, subnet, cluster, namespace string) (ManagedVMNAD, error) {
	return p.lookupManagedVMNetwork(ctx, tenant, vpc, subnet, cluster, namespace, true)
}

// Stopped/closing consumers still verify the pinned namespace/NAD identity and
// real Subnet binding, but do not need today's auxiliary preparation to be ready.
// This never creates a registration or promotes an observed object to trust.
func (p *KCProvider) lookupManagedVMNetwork(ctx context.Context, tenant, vpc, subnet, cluster, namespace string, requireRunnable bool) (ManagedVMNAD, error) {
	legacy, hasLegacy := p.vmNetworks[vmNetworkPolicyKey(tenant, subnet, namespace)]
	if hasLegacy && (legacy.ClusterID != cluster || legacy.VPCID != vpc) {
		return ManagedVMNAD{}, biz.Fail(biz.PlacementMismatch, "VM network placement differs")
	}
	if (requireRunnable && p.vmAutoNetwork == nil) || cluster != p.repository.placement.ClusterID {
		if hasLegacy {
			return legacy, nil
		}
		return ManagedVMNAD{}, biz.Fail(biz.NetworkNotReady, "managed VM network is unavailable")
	}
	s, err := p.repository.queries.GetSubnet(ctx, sqlcgen.GetSubnetParams{TenantID: tenant, SubnetID: subnet})
	if err != nil {
		return ManagedVMNAD{}, databaseFailure(err)
	}
	b, err := p.repository.queries.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: tenant, ResourceID: subnet})
	if err != nil {
		return ManagedVMNAD{}, databaseFailure(err)
	}
	if s.VpcID != vpc || b.ClusterID != cluster || b.Namespace != namespace || b.ProviderUid == "" || b.ResourceKind != "subnet" {
		return ManagedVMNAD{}, biz.Fail(biz.PlacementMismatch, "VM network placement differs")
	}
	record, err := p.repository.queries.GetVMNamespace(ctx, sqlcgen.GetVMNamespaceParams{TenantID: tenant, ClusterID: cluster})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ((requireRunnable && !record.Ready) || record.Namespace != namespace)) {
		if hasLegacy {
			return legacy, nil
		}
		return ManagedVMNAD{}, biz.Fail(biz.NetworkNotReady, "managed VM network preparation is pending or failed")
	}
	if err != nil {
		return ManagedVMNAD{}, databaseFailure(err)
	}
	if record.TemplateID != vmNetworkTemplate {
		return ManagedVMNAD{}, biz.Fail(biz.NetworkNotReady, "managed VM network template differs from deployment policy")
	}
	if requireRunnable {
		digest, _ := VMNADConfigDigest(automaticVMNADConfig(*p.vmAutoNetwork))
		if record.NadConfigDigest != digest || record.ServiceAccount != p.vmAutoNetwork.ServiceAccount {
			return ManagedVMNAD{}, biz.Fail(biz.NetworkNotReady, "managed VM network template differs from deployment policy")
		}
	}
	return ManagedVMNAD{TenantID: tenant, VPCID: vpc, SubnetID: subnet, ClusterID: cluster, Namespace: namespace, NamespaceUID: record.NamespaceUid, NADName: record.NadName, NADUID: record.NadUid, NADConfigDigest: record.NadConfigDigest, KCNSubnetRef: namespace + "/" + b.ProviderName}, nil
}
