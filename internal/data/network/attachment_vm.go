package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ManagedVMNAD comes from a trusted durable preparation record or compatible
// deployment input, never browser input or an arbitrary observed NAD.
type ManagedVMNAD struct {
	TenantID, VPCID, SubnetID, ClusterID, Namespace, NamespaceUID string
	NADName, NADUID, NADConfigDigest, KCNSubnetRef                string
}

func vmNetworkPolicyKey(tenant, subnet, namespace string) string {
	return tenant + "/" + subnet + "/" + namespace
}

func (p *KCProvider) ConfigureVMNetworks(values []ManagedVMNAD) error {
	configured := make(map[string]ManagedVMNAD, len(values))
	for _, v := range values {
		if v.TenantID == "" || v.SubnetID == "" || v.ClusterID != p.repository.placement.ClusterID || v.Namespace == "" || v.NamespaceUID == "" || v.KCNSubnetRef == "" || !biz.ValidVMNetworkExpectation(&biz.VMNetworkExpectation{NADName: v.NADName, NADUID: v.NADUID, NADConfigDigest: v.NADConfigDigest}) {
			return fmt.Errorf("invalid managed VM NAD policy")
		}
		key := vmNetworkPolicyKey(v.TenantID, v.SubnetID, v.Namespace)
		if _, ok := configured[key]; ok {
			return fmt.Errorf("duplicate managed VM NAD policy")
		}
		configured[key] = v
	}
	p.vmNetworks = configured
	return nil
}

func (p *Postgres) SetVMNetworkResolver(resolver biz.VMNetworkResolver, freshness time.Duration) error {
	if resolver == nil || freshness <= 0 {
		return fmt.Errorf("VM network resolver and freshness required")
	}
	p.vmNetworks = resolver
	p.vmAddressFreshness = freshness
	return nil
}

var vmNADs = schema.GroupVersionResource{Group: "k8s.cni.cncf.io", Version: "v1", Resource: "network-attachment-definitions"}

func (p *KCProvider) ResolveVMNetwork(ctx context.Context, s biz.VMNetworkSelection, e biz.VMNetworkExpectation) (biz.VMPrimaryPlan, error) {
	return p.resolveVMNetwork(ctx, s, e, true)
}

func (p *KCProvider) resolveVMNetwork(ctx context.Context, s biz.VMNetworkSelection, e biz.VMNetworkExpectation, requireRunnable bool) (biz.VMPrimaryPlan, error) {
	// Existing VM specifications keep their explicitly frozen static identity
	// when automatic preparation becomes ready for later creates in that tenant.
	v, static := p.vmNetworks[vmNetworkPolicyKey(s.TenantID, s.SubnetID, s.Namespace)]
	static = static && e.NADName == v.NADName && e.NADUID == v.NADUID && e.NADConfigDigest == v.NADConfigDigest
	if !static {
		var err error
		v, err = p.lookupManagedVMNetwork(ctx, s.TenantID, s.VPCID, s.SubnetID, s.ClusterID, s.Namespace, requireRunnable)
		if err != nil {
			return biz.VMPrimaryPlan{}, err
		}
	}
	if !biz.ValidVMNetworkExpectation(&e) || s.ClusterID != v.ClusterID || s.VPCID != v.VPCID || s.KCNSubnetRef != v.KCNSubnetRef || e.NADName != v.NADName || e.NADUID != v.NADUID || e.NADConfigDigest != v.NADConfigDigest {
		return biz.VMPrimaryPlan{}, biz.Fail(biz.AttachmentConflict, "managed VM NAD differs from frozen network selection")
	}
	ns, err := p.client.Resource(namespaces).Get(ctx, s.Namespace, metav1.GetOptions{})
	if err != nil {
		return biz.VMPrimaryPlan{}, readFailure(err)
	}
	if string(ns.GetUID()) != v.NamespaceUID || ns.GetDeletionTimestamp() != nil {
		return biz.VMPrimaryPlan{}, biz.Fail(biz.AttachmentConflict, "VM namespace identity changed")
	}
	var managedRecord *sqlcgen.NetworkVmNamespace
	if !static {
		record, err := p.repository.queries.GetVMNamespace(ctx, sqlcgen.GetVMNamespaceParams{TenantID: s.TenantID, ClusterID: s.ClusterID})
		if err != nil {
			return biz.VMPrimaryPlan{}, databaseFailure(err)
		}
		managedRecord = &record
		if ns.GetLabels()[ownerLabel] != "ani-network-service" || ns.GetLabels()[tenantLabel] != s.TenantID {
			return biz.VMPrimaryPlan{}, biz.Fail(biz.AttachmentConflict, "managed VM namespace ownership changed")
		}
		if requireRunnable {
			sa, err := p.client.Resource(vmServiceAccounts).Namespace(s.Namespace).Get(ctx, record.ServiceAccount, metav1.GetOptions{})
			if err != nil {
				return biz.VMPrimaryPlan{}, readFailure(err)
			}
			if err = verifyVMNetworkObject(sa, record, "ServiceAccount", record.ServiceAccount, record.ServiceAccountUid); err != nil {
				return biz.VMPrimaryPlan{}, err
			}
		}
	}
	nad, err := p.client.Resource(vmNADs).Namespace(s.Namespace).Get(ctx, e.NADName, metav1.GetOptions{})
	if err != nil {
		return biz.VMPrimaryPlan{}, readFailure(err)
	}
	if nad.GetAPIVersion() != "k8s.cni.cncf.io/v1" || nad.GetKind() != "NetworkAttachmentDefinition" || string(nad.GetUID()) != e.NADUID || nad.GetDeletionTimestamp() != nil {
		return biz.VMPrimaryPlan{}, biz.Fail(biz.AttachmentConflict, "managed VM NAD identity changed")
	}
	if managedRecord != nil {
		if err := verifyVMNetworkObject(nad, *managedRecord, "NetworkAttachmentDefinition", managedRecord.NadName, managedRecord.NadUid); err != nil {
			return biz.VMPrimaryPlan{}, err
		}
	}
	config, ok := nad.Object["spec"].(map[string]any)
	if !ok {
		return biz.VMPrimaryPlan{}, biz.Fail(biz.AttachmentConflict, "managed VM NAD config missing")
	}
	raw, ok := config["config"].(string)
	if !ok {
		return biz.VMPrimaryPlan{}, biz.Fail(biz.AttachmentConflict, "managed VM NAD config missing")
	}
	if err := validateVMSinglePluginNAD(raw); requireRunnable && err != nil {
		return biz.VMPrimaryPlan{}, biz.Fail(biz.AttachmentConflict, "managed VM NAD must contain a KCN single-plugin config with a top-level server_socket")
	}
	digest, err := VMNADConfigDigest(raw)
	if err != nil || digest != e.NADConfigDigest {
		return biz.VMPrimaryPlan{}, biz.Fail(biz.AttachmentConflict, "managed VM NAD config changed")
	}
	return biz.VMPrimaryPlan{FormatVersion: 1, Namespace: s.Namespace, NADName: e.NADName, NADUID: e.NADUID, NADConfigDigest: e.NADConfigDigest, KCNSubnetRef: s.KCNSubnetRef}, nil
}

func validateVMSinglePluginNAD(raw string) error {
	var config struct {
		Type         string          `json:"type"`
		ServerSocket string          `json:"server_socket"`
		Plugins      json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return err
	}
	// KCN's PodReconciler passes the NAD directly to LoadNetConf. Its
	// server_socket/type lookup does not unwrap a CNI plugins conflist, even
	// though Multus can execute that list and reach the daemon successfully.
	if config.Type != "kc-networking" || strings.TrimSpace(config.ServerSocket) == "" || len(config.Plugins) != 0 {
		return fmt.Errorf("KCN single-plugin NAD requires top-level type and server_socket")
	}
	return nil
}

func VMNADConfigDigest(raw string) (string, error) {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return "", err
	}
	if _, ok := value.(map[string]any); !ok {
		return "", fmt.Errorf("NAD config must be an object")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// ResolveVMNetworkSelection is a read-only preflight used by the VM resolver.
// Prepare repeats these checks under its existing parent locks before saving.
func (p *Postgres) ResolveVMNetworkSelection(ctx context.Context, r biz.PrepareAttachment, freshness time.Duration) (biz.VMPrimaryPlan, error) {
	if r.WorkloadMode != "vm_managed" || !biz.ValidVMNetworkExpectation(r.VMExpectation) || p.vmNetworks == nil {
		return biz.VMPrimaryPlan{}, biz.Fail(biz.NetworkNotReady, "managed VM network is unavailable")
	}
	s, err := p.queries.GetSubnet(ctx, sqlcgen.GetSubnetParams{TenantID: r.TenantID, SubnetID: r.SubnetID})
	if err != nil {
		return biz.VMPrimaryPlan{}, databaseFailure(err)
	}
	v, err := p.queries.GetVPC(ctx, sqlcgen.GetVPCParams{TenantID: r.TenantID, VpcID: s.VpcID})
	if err != nil {
		return biz.VMPrimaryPlan{}, databaseFailure(err)
	}
	now, err := p.queries.DatabaseTime(ctx)
	if err != nil {
		return biz.VMPrimaryPlan{}, databaseFailure(err)
	}
	if freshness <= 0 || v.NetworkVpc.State != "available" || s.State != "available" || v.NetworkVpc.ObservedAt == nil || s.ObservedAt == nil || now.Sub(*v.NetworkVpc.ObservedAt) > freshness || now.Sub(*s.ObservedAt) > freshness || (r.VPCID != "" && r.VPCID != s.VpcID) {
		return biz.VMPrimaryPlan{}, biz.Fail(biz.NetworkNotReady, "fresh available VM network required")
	}
	b, err := p.queries.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: r.TenantID, ResourceID: r.SubnetID})
	if err != nil {
		return biz.VMPrimaryPlan{}, databaseFailure(err)
	}
	if b.ClusterID != r.ClusterID || b.Namespace != r.Namespace || b.ProviderUid == "" {
		return biz.VMPrimaryPlan{}, biz.Fail(biz.PlacementMismatch, "VM network placement or identity differs")
	}
	return p.vmNetworks.ResolveVMNetwork(ctx, biz.VMNetworkSelection{TenantID: r.TenantID, VPCID: s.VpcID, SubnetID: s.SubnetID, ClusterID: b.ClusterID, Namespace: b.Namespace, BindingRevision: b.BindingID + ":" + b.ProviderUid, KCNSubnetRef: b.Namespace + "/" + b.ProviderName}, *r.VMExpectation)
}

func (p *Postgres) vmCurrentAddresses(ctx context.Context, a biz.Attachment, raw []byte) []biz.VMCurrentAddress {
	if a.State != biz.Attached || a.Reason != "" || a.ObservedAt == nil || p.vmAddressFreshness <= 0 || a.ConsumerSnapshot == nil || a.ConsumerSnapshot.State != "open" || a.ConsumerSnapshot.DesiredRunState != "running" || len(a.ConsumerSnapshot.CurrentPods) != 1 {
		return nil
	}
	now, err := p.queries.DatabaseTime(ctx)
	if err != nil || now.Sub(*a.ObservedAt) > p.vmAddressFreshness {
		return nil
	}
	pod := a.ConsumerSnapshot.CurrentPods[0]
	if pod.Reference.UID != a.PodUID || pod.Owner.Kind != "VirtualMachineInstance" {
		return nil
	}
	var relations []attachmentRelation
	if json.Unmarshal(raw, &relations) != nil {
		return nil
	}
	var out []biz.VMCurrentAddress
	for _, relation := range relations {
		if relation.Kind == "VNicIP" && relation.Current && relation.AddressEligible {
			if _, err := netip.ParseAddr(relation.Address); err != nil {
				continue
			}
			out = append(out, biz.VMCurrentAddress{NICName: "default", Address: relation.Address, LauncherPodUID: pod.Reference.UID, VMIUID: pod.Owner.UID})
		}
	}
	return out
}
