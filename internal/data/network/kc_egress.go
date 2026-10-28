package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
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
)

var kcEIPs = schema.GroupVersionResource{Group: kcVPCs.Group, Version: "v1", Resource: "eips"}
var kcSnats = schema.GroupVersionResource{Group: kcVPCs.Group, Version: "v1", Resource: "snats"}
var kcNats = schema.GroupVersionResource{Group: kcVPCs.Group, Version: "v1", Resource: "nats"}
var kcEgressGateways = schema.GroupVersionResource{Group: kcVPCs.Group, Version: "v1", Resource: "eipgateways"}
var kcVlans = schema.GroupVersionResource{Group: kcVPCs.Group, Version: "v1", Resource: "vlannetworks"}
var kcNodes = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
var kcConfigMaps = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
var kcServices = schema.GroupVersionResource{Version: "v1", Resource: "services"}

const kcSystemNamespace = "kcn-system"

func egressKind(kind string) bool {
	switch kind {
	case "eip", "snat", "device", "vlan", "egress_gateway", "public_pool":
		return true
	}
	return false
}
func egressGVR(kind string) schema.GroupVersionResource {
	switch kind {
	case "eip":
		return kcEIPs
	case "snat":
		return kcSnats
	case "vlan":
		return kcVlans
	case "egress_gateway":
		return kcEgressGateways
	case "public_pool":
		return kcSubnets
	}
	return schema.GroupVersionResource{}
}
func egressCRKind(kind string) string {
	switch kind {
	case "eip":
		return "EIP"
	case "snat":
		return "Snat"
	case "vlan":
		return "VlanNetwork"
	case "egress_gateway":
		return "EIPGateway"
	case "public_pool":
		return "Subnet"
	case "vpc":
		return "VPC"
	}
	return ""
}
func resourceEndpoint(c dynamic.Interface, b sqlcgen.NetworkProviderBinding) dynamic.ResourceInterface {
	gvr := egressGVR(b.ResourceKind)
	if b.ResourceKind == "vpc" {
		gvr = kcVPCs
	}
	if b.Namespace == "" {
		return c.Resource(gvr)
	}
	return c.Resource(gvr).Namespace(b.Namespace)
}
func (p *KCProvider) platformBinding(ctx context.Context, kind, id string) (sqlcgen.NetworkProviderBinding, error) {
	r, err := p.repository.queries.GetPlatform(ctx, sqlcgen.GetPlatformParams{ClusterID: p.repository.placement.ClusterID, Kind: kind, ResourceID: id})
	if err != nil {
		return sqlcgen.NetworkProviderBinding{}, readFailure(err)
	}
	namespace := ""
	if kind == "public_pool" {
		namespace = kcSystemNamespace
	}
	return sqlcgen.NetworkProviderBinding{ResourceKind: kind, ClusterID: r.ClusterID, Namespace: namespace, ProviderName: r.ProviderName, ProviderUid: r.ProviderUid, BindingID: r.BindingID}, nil
}
func (p *KCProvider) egressBinding(ctx context.Context, t biz.ProviderTarget) (sqlcgen.NetworkProviderBinding, error) {
	if t.TenantID != "" {
		return p.binding(ctx, t)
	}
	b, err := p.platformBinding(ctx, t.Kind, t.ResourceID)
	if err != nil {
		return b, err
	}
	if b.BindingID != t.BindingID || (t.KnownIdentity != "" && b.ProviderUid != "" && t.KnownIdentity != b.ProviderUid) {
		return b, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return b, nil
}

type egressReference struct {
	binding sqlcgen.NetworkProviderBinding
	id      string
}
type egressResolved struct {
	lbID                       string
	binding                    sqlcgen.NetworkProviderBinding
	target                     biz.ProviderTarget
	spec                       map[string]any
	refs                       map[string]egressReference
	pool                       sqlcgen.NetworkPublicPool
	snat                       sqlcgen.NetworkSnatBinding
	address, vpcCIDR, deviceID string
	virtualSwitch              *biz.VirtualSwitch
	switchDevice               sqlcgen.NetworkDeviceAdoption
}

func (p *KCProvider) resolveEgress(ctx context.Context, t biz.ProviderTarget) (egressResolved, error) {
	r := egressResolved{target: t, refs: map[string]egressReference{}}
	var err error
	r.binding, err = p.egressBinding(ctx, t)
	if err != nil {
		return r, err
	}
	addPlatform := func(key, kind, id string) error {
		b, err := p.platformBinding(ctx, kind, id)
		if err != nil {
			return err
		}
		r.refs[key] = egressReference{b, id}
		return nil
	}
	addTenant := func(key, kind, id string) error {
		b, err := p.repository.queries.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: t.TenantID, ResourceID: id})
		if err != nil {
			return readFailure(err)
		}
		if b.ResourceKind != kind || b.ClusterID != r.binding.ClusterID || b.Namespace != r.binding.Namespace || b.ProviderUid == "" {
			return &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		r.refs[key] = egressReference{b, id}
		if kind == "vpc" {
			v, err := p.repository.queries.GetVPC(ctx, sqlcgen.GetVPCParams{TenantID: t.TenantID, VpcID: id})
			if err != nil {
				return readFailure(err)
			}
			r.vpcCIDR = v.NetworkVpc.Cidr
		}
		return nil
	}
	poolID := ""
	switch t.Kind {
	case "eip":
		e, err := p.repository.queries.GetEIPInternal(ctx, sqlcgen.GetEIPInternalParams{TenantID: t.TenantID, EipID: t.ResourceID})
		if err != nil {
			return r, readFailure(err)
		}
		poolID = e.PoolID
		if e.Scope == "intranet" && (e.ManagedBy != "system" || e.SystemOwnerVpc == nil) {
			return r, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		r.address = e.Address
		view, err := p.repository.queries.GetEIPClaim(ctx, sqlcgen.GetEIPClaimParams{TenantID: t.TenantID, EipID: t.ResourceID})
		if err != nil {
			return r, readFailure(err)
		}
		if view.BindingID != "" {
			s, err := p.repository.queries.GetSnatInternal(ctx, sqlcgen.GetSnatInternalParams{TenantID: t.TenantID, SnatID: view.BindingID})
			if err != nil {
				return r, readFailure(err)
			}
			r.snat = s
			// An accepted binding may still lack a CR/UID. That is pending, not an
			// unrelated object to adopt or a reason to release EIP occupancy.
			b, err := p.repository.queries.GetBinding(ctx, sqlcgen.GetBindingParams{TenantID: t.TenantID, ResourceID: s.SnatID})
			if err != nil {
				return r, readFailure(err)
			}
			if b.Namespace != r.binding.Namespace || b.ClusterID != r.binding.ClusterID {
				return r, &biz.ProviderError{Kind: biz.ProviderConflict}
			}
			r.refs["snat"] = egressReference{b, s.SnatID}
			if err = addTenant("vpc", "vpc", s.VpcID); err != nil {
				return r, err
			}
		}
		if view.BindingTargetKind == "load_balancer" {
			r.lbID = view.BindingTargetID
		}
	case "snat":
		s, err := p.repository.queries.GetSnatInternal(ctx, sqlcgen.GetSnatInternalParams{TenantID: t.TenantID, SnatID: t.ResourceID})
		if err != nil {
			return r, readFailure(err)
		}
		r.snat = s
		if t.Egress == nil || t.Egress.EIPID != s.EipID || t.VPCID != s.VpcID || t.Egress.DesiredEnabled != s.DesiredEnabled {
			return r, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		if err = addTenant("eip", "eip", s.EipID); err != nil {
			return r, err
		}
		if err = addTenant("vpc", "vpc", s.VpcID); err != nil {
			return r, err
		}
		e, err := p.repository.queries.GetEIPInternal(ctx, sqlcgen.GetEIPInternalParams{TenantID: t.TenantID, EipID: s.EipID})
		if err != nil {
			return r, readFailure(err)
		}
		poolID = e.PoolID
		r.address = e.Address
		r.spec = map[string]any{"eip": r.refs["eip"].binding.ProviderName, "vpc": r.refs["vpc"].binding.ProviderName, "disable": !s.DesiredEnabled}
	case "egress_gateway":
		r.spec = map[string]any{"scope": "Public", "egressType": "Host"}
	case "vlan":
		vlan, err := p.repository.queries.GetVlanNetwork(ctx, sqlcgen.GetVlanNetworkParams{ClusterID: r.binding.ClusterID, ResourceID: t.ResourceID})
		if err != nil {
			return r, readFailure(err)
		}
		device, err := p.repository.queries.GetDeviceAdoption(ctx, sqlcgen.GetDeviceAdoptionParams{ClusterID: r.binding.ClusterID, ResourceID: vlan.DeviceID})
		if err != nil {
			return r, readFailure(err)
		}
		r.spec = map[string]any{"devName": device.DeviceName, "vlanID": int64(vlan.VlanID)}
		if device.VirtualSwitchID != nil {
			sw, err := p.repository.GetVirtualSwitch(ctx, *device.VirtualSwitchID)
			if err != nil {
				return r, err
			}
			if device.SwitchBindingID == nil || sw.BindingID != *device.SwitchBindingID || sw.ClusterID != r.binding.ClusterID {
				return r, switchConflict(fmt.Errorf("virtual switch %s binding identity does not match device %s", sw.ID, device.ResourceID))
			}
			if sw.State != biz.Available {
				return r, switchUnavailable(fmt.Errorf("virtual switch %s dependency is %s: %s", sw.ID, sw.State, sw.Reason))
			}
			r.spec["vClusterName"] = sw.GroupName
		}
		r.deviceID = vlan.DeviceID
	case "public_pool":
		poolID = t.ResourceID
	default:
		return r, &biz.ProviderError{Kind: biz.ProviderReject}
	}
	if poolID != "" {
		r.pool, err = p.repository.queries.GetPublicPool(ctx, sqlcgen.GetPublicPoolParams{ClusterID: r.binding.ClusterID, ResourceID: poolID})
		if err != nil {
			return r, readFailure(err)
		}
		if t.Kind == "eip" || t.Kind == "snat" {
			eipID := t.ResourceID
			if t.Kind == "snat" {
				eipID = r.snat.EipID
			}
			e, err := p.repository.queries.GetEIPInternal(ctx, sqlcgen.GetEIPInternalParams{TenantID: t.TenantID, EipID: eipID})
			if err != nil {
				return r, readFailure(err)
			}
			if e.Scope != r.pool.Scope || (r.snat.SnatID != "" && (r.snat.Purpose != e.Scope || (e.Scope == "intranet" && (e.ManagedBy != "system" || e.SystemOwnerVpc == nil || *e.SystemOwnerVpc != r.snat.VpcID)))) {
				return r, &biz.ProviderError{Kind: biz.ProviderConflict}
			}
		}
		if err = addPlatform("pool", "public_pool", poolID); err != nil {
			return r, err
		}
		if r.pool.Scope == "intranet" {
			if r.pool.GatewayID != nil || r.pool.DefaultVpcName == "" || r.pool.DefaultVpcUid == "" {
				return r, &biz.ProviderError{Kind: biz.ProviderConflict}
			}
			r.refs["default_vpc"] = egressReference{binding: sqlcgen.NetworkProviderBinding{ResourceKind: "vpc", ClusterID: r.binding.ClusterID, Namespace: kcSystemNamespace, ProviderName: r.pool.DefaultVpcName, ProviderUid: r.pool.DefaultVpcUid}}
		} else {
			if err = addPlatform("gateway", "egress_gateway", textValue(r.pool.GatewayID)); err != nil {
				return r, err
			}
		}
		if r.pool.VlanNetworkID != nil {
			vlan, err := p.repository.queries.GetVlanNetwork(ctx, sqlcgen.GetVlanNetworkParams{ClusterID: r.binding.ClusterID, ResourceID: *r.pool.VlanNetworkID})
			if err != nil {
				return r, readFailure(err)
			}
			r.deviceID = vlan.DeviceID
			if err = addPlatform("vlan", "vlan", *r.pool.VlanNetworkID); err != nil {
				return r, err
			}
		}
		if t.Kind == "eip" {
			r.spec = map[string]any{"subnet": kcSystemNamespace + "/" + r.refs["pool"].binding.ProviderName, "ipVersion": "IPv4"}
		}
		if t.Kind == "public_pool" {
			if r.pool.Scope == "intranet" {
				r.spec = renderIntranetPool(r.pool)
			} else {
				r.spec = renderPublicPool(r.pool, r.refs["gateway"].binding.ProviderName, r.refs["vlan"].binding.ProviderName)
			}
		}
	}
	if r.deviceID != "" {
		device, err := p.repository.queries.GetDeviceAdoption(ctx, sqlcgen.GetDeviceAdoptionParams{ClusterID: r.binding.ClusterID, ResourceID: r.deviceID})
		if err != nil {
			return r, readFailure(err)
		}
		if device.VirtualSwitchID != nil {
			sw, err := p.repository.GetVirtualSwitch(ctx, *device.VirtualSwitchID)
			if err != nil {
				return r, err
			}
			if device.Retired || device.SwitchBindingID == nil || sw.BindingID != *device.SwitchBindingID || sw.ClusterID != r.binding.ClusterID {
				return r, switchConflict(fmt.Errorf("virtual switch %s binding identity does not match active device %s", sw.ID, device.ResourceID))
			}
			if sw.State != biz.Available {
				return r, switchUnavailable(fmt.Errorf("virtual switch %s dependency is %s: %s", sw.ID, sw.State, sw.Reason))
			}
			r.virtualSwitch = &sw
			r.switchDevice = device
		}
	}
	return r, nil
}
func renderPublicPool(c sqlcgen.NetworkPublicPool, gateway, vlan string) map[string]any {
	excluded := make([]any, len(c.ExcludedIps))
	for i, x := range c.ExcludedIps {
		excluded[i] = x
	}
	spec := map[string]any{"type": "Public", "ipVersion": "IPv4", "cidrBlock": c.Cidr, "gatewayIP": c.OvnGatewayIp, "gateway": gateway, "excludeIPs": excluded, "allowedNamespaces": map[string]any{"from": "All"}, "enableDHCP": false, "natOutgoing": false}
	if c.Mode == "underlay" {
		spec["underlayConfig"] = map[string]any{"vlanNetwork": vlan, "gatewayIP": textValue(c.UpstreamGatewayIp)}
	}
	return spec
}
func egressObject(r egressResolved) *unstructured.Unstructured {
	labels := map[string]any{ownerLabel: "ani-network-service", resourceLabel: r.target.ResourceID, bindingLabel: r.binding.BindingID}
	if r.target.TenantID != "" {
		labels[tenantLabel] = r.target.TenantID
	}
	metadata := map[string]any{"name": r.binding.ProviderName, "labels": labels}
	if r.binding.Namespace != "" {
		metadata["namespace"] = r.binding.Namespace
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": egressCRKind(r.target.Kind), "metadata": metadata, "spec": r.spec}}
}
func inspectEgressIdentity(o *unstructured.Unstructured, b sqlcgen.NetworkProviderBinding, id, uid string) error {
	if o == nil {
		return &biz.ProviderError{Kind: biz.ProviderTemporary}
	}
	labels := o.GetLabels()
	if o.GetAPIVersion() != "networking.kubercloud.com/v1" || o.GetKind() != egressCRKind(b.ResourceKind) || o.GetName() != b.ProviderName || o.GetNamespace() != b.Namespace || o.GetUID() == "" || o.GetResourceVersion() == "" || labels[ownerLabel] != "ani-network-service" || labels[resourceLabel] != id || labels[bindingLabel] != b.BindingID || labels[tenantLabel] != b.TenantID || (uid != "" && string(o.GetUID()) != uid) || (b.ProviderUid != "" && string(o.GetUID()) != b.ProviderUid) {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return nil
}
func crString(o *unstructured.Unstructured, fields ...string) string {
	if o == nil {
		return ""
	}
	v, _, _ := unstructured.NestedString(o.Object, fields...)
	return v
}
func crBool(o *unstructured.Unstructured, fields ...string) bool {
	if o == nil {
		return false
	}
	v, _, _ := unstructured.NestedBool(o.Object, fields...)
	return v
}
func crInt(o *unstructured.Unstructured, fields ...string) int64 {
	if o == nil {
		return 0
	}
	v, _, _ := unstructured.NestedInt64(o.Object, fields...)
	return v
}
func hasField(o *unstructured.Unstructured, fields ...string) bool {
	if o == nil {
		return false
	}
	_, ok, _ := unstructured.NestedFieldNoCopy(o.Object, fields...)
	return ok
}
func goodConditions(o *unstructured.Unstructured, names ...string) bool {
	if o == nil || o.GetDeletionTimestamp() != nil || o.GetGeneration() < 1 {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	good := map[string]bool{}
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		generation, _, _ := unstructured.NestedInt64(c, "observedGeneration")
		kind, _ := c["type"].(string)
		good[kind] = c["status"] == "True" && (generation == 0 || generation == o.GetGeneration())
	}
	for _, name := range names {
		if !good[name] {
			return false
		}
	}
	return true
}
func matchesEgressSpec(o *unstructured.Unstructured, expected map[string]any, kind string) bool {
	actual, _, _ := unstructured.NestedMap(o.Object, "spec")
	for k, want := range expected {
		got, ok := actual[k]
		if kind == "snat" && k == "disable" {
			continue
		}
		if !ok {
			if b, ok := want.(bool); ok && !b {
				continue
			}
			return false
		}
		if k == "excludeIPs" {
			a, ok := got.([]any)
			b, _ := want.([]any)
			if !ok {
				return false
			}
			aa := []string{}
			bb := []string{}
			for _, v := range a {
				s, ok := v.(string)
				if !ok {
					return false
				}
				aa = append(aa, s)
			}
			for _, v := range b {
				bb = append(bb, v.(string))
			}
			slices.Sort(aa)
			slices.Sort(bb)
			if !slices.Equal(aa, bb) {
				return false
			}
			continue
		}
		if !reflect.DeepEqual(got, want) {
			return false
		}
	}
	for _, field := range []string{"type", "gatewayConfig", "hostConfig", "cidrs", "securityGroups", "underlayConfig", "routeTable", "policyRoutes", "vClusterName"} {
		if _, specified := expected[field]; specified {
			continue
		}
		if v, exists := actual[field]; exists && v != nil {
			// Empty optional arrays can be injected by CR defaulting; a nonempty
			// forbidden route/security scope is never accepted.
			if a, ok := v.([]any); ok && len(a) == 0 {
				continue
			}
			if s, ok := v.(string); ok && s == "" {
				continue
			}
			return false
		}
	}
	return true
}

// readEgressSet uses the same complete audit view/index as existing resources.
// Critical calls force a collection after Claim and GET each identity directly.
// Ordinary observations reuse proven facts and never renew them on cache access.
type egressReadSet struct {
	lbBound                       bool
	lbServiceUID                  string
	deviceRequired, deviceReady   bool
	switchConfigured, switchWired bool
	objects                       map[string]*unstructured.Unstructured
	view                          *auditView
	proof                         biz.ObservationProof
	all                           map[schema.GroupVersionResource][]unstructured.Unstructured
	keys                          []string
}

func objectKey(b sqlcgen.NetworkProviderBinding) string {
	return "object:" + egressCRKind(b.ResourceKind) + "/" + b.Namespace + "/" + b.ProviderName
}

var errEgressAuditInvalid = errors.New("egress audit needs a fresh collection")

func (p *KCProvider) readEgressSet(ctx context.Context, r egressResolved, critical bool) (egressReadSet, error) {
	for {
		if err := ctx.Err(); err != nil {
			return egressReadSet{}, readFailure(err)
		}
		set, err := p.readEgressSetOnce(ctx, r, critical)
		if err != errEgressAuditInvalid {
			return set, err
		}
		// Consecutive Watch renewals can invalidate more than one collection.
		// Keep the original caller deadline and collect after a new database
		// boundary on each attempt. Read failures and identity conflicts are
		// not retry signals. Unbounded callers retain one critical fallback.
		if critical {
			if _, bounded := ctx.Deadline(); !bounded {
				return egressReadSet{}, &biz.ProviderError{Kind: biz.ProviderTemporary}
			}
		}
		critical = true
	}
}

func (p *KCProvider) readEgressSetOnce(ctx context.Context, r egressResolved, critical bool) (egressReadSet, error) {
	result := egressReadSet{objects: map[string]*unstructured.Unstructured{}, all: map[schema.GroupVersionResource][]unstructured.Unstructured{}}
	now, err := p.repository.queries.DatabaseTime(ctx)
	if err != nil {
		return result, readFailure(err)
	}
	refs := map[string]egressReference{"self": {r.binding, r.target.ResourceID}}
	for k, v := range r.refs {
		refs[k] = v
	}
	result.keys = append(result.keys, objectKey(r.binding), "provider-images")
	if r.pool.Scope == "intranet" {
		result.keys = append(result.keys, "object:ConfigMap/kcn-system/kcn-config", "intranet-service-ranges")
	}
	if r.deviceID != "" {
		result.keys = append(result.keys, "node-facts")
	}
	relation := r.target.Kind
	if relation == "public_pool" {
		relation = "pool"
	}
	if relation == "egress_gateway" {
		relation = "gateway"
	}
	result.keys = append(result.keys, relation+":"+r.binding.Namespace+"/"+r.binding.ProviderName)
	for _, ref := range refs {
		result.keys = append(result.keys, objectKey(ref.binding), "uid:"+ref.binding.ProviderUid)
	}
	for _, name := range []string{"eip", "snat", "vpc", "pool", "gateway", "vlan"} {
		if ref, ok := refs[name]; ok {
			result.keys = append(result.keys, name+":"+ref.binding.Namespace+"/"+ref.binding.ProviderName)
		}
	}
	if p.observation != nil {
		after := time.Time{}
		if critical {
			after = now
		}
		result.view, err = p.observation.snapshot(ctx, after)
		if err != nil {
			return result, readFailure(err)
		}
		for _, gvr := range []schema.GroupVersionResource{kcSubnets, kcEIPs, kcSnats, kcNats, kcServices, kcVlans, kcEgressGateways, observationGVRs[3], observationGVRs[4], pods} {
			index := result.view.indices[gvr]
			if index == nil {
				return result, &biz.ProviderError{Kind: biz.ProviderTemporary}
			}
			// Only the material relationship candidates are copied out of the index.
			result.all[gvr] = indexedObjects(index, result.keys)
		}
		result.proof = biz.ObservationProof{CollectedAt: result.view.collected, CoveredGeneration: result.view.generations[observationTarget{r.target.TenantID, r.target.ResourceID, r.target.Kind}.key()]}
	}
	for key, ref := range refs {
		var o *unstructured.Unstructured
		if critical || p.observation == nil {
			o, err = resourceEndpoint(p.client, ref.binding).Get(ctx, ref.binding.ProviderName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				err = nil
				o = nil
			}
			if err != nil {
				return result, readFailure(err)
			}
		} else {
			gvr := egressGVR(ref.binding.ResourceKind)
			if ref.binding.ResourceKind == "vpc" {
				gvr = kcVPCs
			}
			index := result.view.indices[gvr]
			if index == nil {
				return result, &biz.ProviderError{Kind: biz.ProviderTemporary}
			}
			lookup := ref.binding.ProviderName
			if ref.binding.Namespace != "" {
				lookup = ref.binding.Namespace + "/" + lookup
			}
			raw, exists, err := index.GetByKey(lookup)
			if err != nil {
				return result, readFailure(err)
			}
			if exists {
				o = raw.(*unstructured.Unstructured)
			}
			// Cache absence is never a lifecycle deletion/ownership conclusion.
			if o == nil {
				return result, errEgressAuditInvalid
			}
		}
		result.objects[key] = o
	}
	if p.observation == nil {
		for _, gvr := range []schema.GroupVersionResource{kcSubnets, kcEIPs, kcSnats, kcNats, kcServices, kcVlans, kcEgressGateways, observationGVRs[3], observationGVRs[4], pods} {
			objects, err := p.listAll(ctx, gvr)
			if err != nil {
				return result, err
			}
			result.all[gvr] = objects
		}
	}
	if critical || p.observation == nil {
		result.proof.CollectedAt = now
		result.proof.CoveredGeneration = r.target.Requirement.RequestedGeneration
	}
	if r.pool.Scope == "intranet" {
		config, err := p.client.Resource(kcConfigMaps).Namespace(kcSystemNamespace).Get(ctx, "kcn-config", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			config, err = nil, nil
		}
		if err != nil {
			return result, readFailure(err)
		}
		result.objects["intranet_config"] = config
		serviceNetworks, err := p.listAll(ctx, kcServiceCIDRs)
		if err != nil {
			return result, err
		}
		result.all[kcServiceCIDRs] = serviceNetworks
	}
	if r.lbID != "" {
		if err = p.readEIPLoadBalancer(ctx, r, &result); err != nil {
			return result, err
		}
	}
	result.proof.Hash = contentHash(struct {
		Objects map[string]*unstructured.Unstructured
		All     map[string][]unstructured.Unstructured
	}{result.objects, candidatesForHash(result.all)})
	if r.deviceID != "" && !(r.target.Kind == "snat" && !r.snat.DesiredEnabled) {
		result.deviceRequired = true
		if r.virtualSwitch != nil {
			configured, wired, proof, err := p.virtualSwitchDeviceState(ctx, *r.virtualSwitch, r.switchDevice, result.view)
			if err != nil {
				return result, err
			}
			result.proof.CollectionStartedAt = result.proof.CollectedAt
			if proof.CollectionStartedAt.Before(result.proof.CollectionStartedAt) {
				result.proof.CollectionStartedAt = proof.CollectionStartedAt
			}
			result.switchConfigured, result.switchWired = configured, wired
			result.deviceReady = wired
			// A VLAN is what causes physical takeover. Its POST gate requires
			// the owned group, while its Ready gate still requires real wiring.
			if r.target.Kind == "vlan" {
				result.deviceReady = configured
			}
			if proof.CollectedAt.Before(result.proof.CollectedAt) {
				result.proof.CollectedAt = proof.CollectedAt
			}
			result.proof.Hash = contentHash([]string{result.proof.Hash, proof.Hash})
		} else {
			b, err := p.platformBinding(ctx, "device", r.deviceID)
			if err != nil {
				return result, err
			}
			device, err := p.observeDeviceFromView(ctx, biz.ProviderTarget{Kind: "device", ResourceID: r.deviceID, BindingID: b.BindingID, KnownIdentity: b.ProviderUid, Direct: critical, Requirement: biz.ObservationRequirement{RequestedGeneration: r.target.Requirement.RequestedGeneration}}, result.view)
			if err != nil {
				return result, err
			}
			result.deviceReady = device.Ready
			if device.Proof.CollectedAt.Before(result.proof.CollectedAt) {
				result.proof.CollectedAt = device.Proof.CollectedAt
			}
			result.proof.Hash = contentHash([]string{result.proof.Hash, device.Proof.Hash})
		}
	}
	if result.view != nil && !p.observation.valid(result.view, result.keys) {
		return result, errEgressAuditInvalid
	}
	if !result.proof.Covers(r.target.Requirement) {
		return result, errEgressAuditInvalid
	}
	return result, nil
}
func (p *KCProvider) observeEgress(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	if t.Kind == "device" {
		return p.observeDevice(ctx, t)
	}
	r, err := p.resolveEgress(ctx, t)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	set, err := p.readEgressSet(ctx, r, t.Direct || t.KnownIdentity == "")
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	value, err := inspectEgressSet(r, set)
	value.Proof = set.proof
	return value, err
}
func inspectEgressSet(r egressResolved, set egressReadSet) (biz.ProviderObservation, error) {
	o := set.objects["self"]
	value := biz.ProviderObservation{Egress: &biz.EgressAppliedFacts{}}
	if o != nil {
		if err := inspectEgressIdentity(o, r.binding, r.target.ResourceID, r.target.KnownIdentity); err != nil {
			return value, err
		}
		value.Exists = true
		value.Identity = string(o.GetUID())
		if !matchesEgressSpec(o, r.spec, r.target.Kind) {
			return value, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
	}
	for key, ref := range r.refs {
		other := set.objects[key]
		if other != nil {
			if key == "default_vpc" {
				if !intranetGatewayIdentity(r.pool, other) {
					return value, &biz.ProviderError{Kind: biz.ProviderConflict}
				}
			} else if err := inspectEgressIdentity(other, ref.binding, ref.id, ref.binding.ProviderUid); err != nil {
				return value, err
			}
		}
	}
	if vpc := set.objects["vpc"]; vpc != nil {
		ref := r.refs["vpc"]
		_, err := inspectResource(vpc, ref.binding, biz.ProviderTarget{TenantID: ref.binding.TenantID, ResourceID: ref.id, KnownIdentity: ref.binding.ProviderUid, CIDR: r.vpcCIDR}, "")
		if err != nil {
			return value, err
		}
		if hasField(vpc, "spec", "type") {
			return value, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
	}
	switch r.target.Kind {
	case "egress_gateway":
		value.Ready = goodConditions(o, "Valid", "Initialized", "Ready") && crString(o, "status", "localIP") != "" && crString(o, "status", "boundResources", "router") != ""
		for _, child := range set.all[kcSubnets] {
			if crString(&child, "spec", "type") == "Public" && crString(&child, "spec", "gateway") == r.binding.ProviderName {
				value.HasDependencies = true
			}
		}
	case "vlan":
		missing, _, _ := unstructured.NestedSlice(objectMap(o), "status", "notReadyNodes")
		value.Ready = vlanControlReady(o) && len(missing) == 0
		if r.virtualSwitch != nil {
			value.Ready = value.Ready && set.switchWired
		}
		value.HasDependencies = crString(o, "status", "subnet") != ""
		for _, child := range set.all[kcSubnets] {
			if crString(&child, "spec", "underlayConfig", "vlanNetwork") == r.binding.ProviderName {
				value.HasDependencies = true
			}
		}
	case "public_pool":
		value.Ready = addressPoolReady(r, o, set)
		value.Egress.ProviderImages = providerImages(set.all[pods])
		ref := r.binding.Namespace + "/" + r.binding.ProviderName
		for _, gvr := range []schema.GroupVersionResource{kcEIPs, observationGVRs[3], observationGVRs[4], pods} {
			for _, child := range set.all[gvr] {
				if kcRef(crString(&child, "spec", "subnet"), child.GetNamespace()) == ref {
					value.HasDependencies = true
				}
			}
		}
	case "eip", "snat":
		eip := o
		if r.target.Kind == "snat" {
			eip = set.objects["eip"]
		}
		ready, address := allocatedEIP(r, eip, set)
		if r.address != "" && address != "" && address != r.address {
			return value, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		value.Egress.Address = address
		bound, _, _ := unstructured.NestedMap(objectMap(eip), "status", "boundResource")
		expectedSnat := ""
		snat := set.objects["snat"]
		if r.target.Kind == "snat" {
			expectedSnat = r.binding.ProviderName
			snat = o
		} else if ref, ok := r.refs["snat"]; ok {
			expectedSnat = ref.binding.ProviderName
		}
		conflict := eipBindingConflictAllowed(set, r.binding.Namespace, crName(eip), expectedSnat, set.lbServiceUID)
		if r.target.Kind == "eip" {
			value.HasDependencies = len(bound) > 0 || conflict || expectedSnat != "" || r.lbID != ""
			value.Ready = ready && verifiedEgressExit(r, set) && !conflict && ((len(bound) == 0 && crString(eip, "status", "phase") == "Available") || bindingApplied(eip, snat, set.objects["vpc"], r.snat.DesiredEnabled) || set.lbBound)
		} else {
			value.HasDependencies = conflict || (o == nil && (len(bound) > 0 || !goodConditions(eip, "Valid", "Initialized") || crString(eip, "status", "phase") != "Available"))
			if o != nil {
				value.NeedsUpdate = crBool(o, "spec", "disable") == r.snat.DesiredEnabled
				value.Egress.TargetGeneration = o.GetGeneration()
				applied := !conflict && !value.NeedsUpdate && bindingApplied(eip, o, set.objects["vpc"], r.snat.DesiredEnabled)
				value.Ready = applied && (!r.snat.DesiredEnabled || (ready && verifiedEgressExit(r, set)))
				if applied {
					enabled := r.snat.DesiredEnabled
					value.Egress.AppliedEnabled = &enabled
				}
			}
		}
	}
	if set.deviceRequired && !set.deviceReady {
		value.Ready = false
	}
	return value, nil
}
func objectMap(o *unstructured.Unstructured) map[string]any {
	if o == nil {
		return nil
	}
	return o.Object
}
func crName(o *unstructured.Unstructured) string {
	if o == nil {
		return ""
	}
	return o.GetName()
}
func publicPoolReady(r egressResolved, pool, gw, vlan *unstructured.Unstructured) bool {
	if !goodConditions(pool, "Valid", "Initialized", "Ready") || !goodConditions(gw, "Valid", "Initialized", "Ready") || crString(gw, "status", "localIP") == "" || crString(gw, "status", "boundResources", "router") == "" {
		return false
	}
	if !matchesEgressSpec(pool, renderPublicPool(r.pool, crName(gw), crName(vlan)), "public_pool") || crString(gw, "spec", "scope") != "Public" || crString(gw, "spec", "egressType") != "Host" {
		return false
	}
	if r.pool.Mode == "overlay" {
		return !hasField(pool, "spec", "underlayConfig") && !hasField(pool, "status", "underlayState")
	}
	missing, _, _ := unstructured.NestedSlice(objectMap(pool), "status", "underlayState", "notReadyNodes")
	vlanMissing, _, _ := unstructured.NestedSlice(objectMap(vlan), "status", "notReadyNodes")
	v09Observed := crString(vlan, "spec", "vClusterName") == "" || (crInt(pool, "status", "observedGeneration") == pool.GetGeneration() && crString(pool, "status", "underlayState", "vlanNetwork") == vlan.GetName())
	return vlanControlReady(vlan) && len(vlanMissing) == 0 && crString(vlan, "status", "subnet") == pool.GetNamespace()+"/"+pool.GetName() && v09Observed && crBool(pool, "status", "underlayState", "ready") && len(missing) == 0 && crString(pool, "status", "underlayState", "chassisNode") != ""
}
func vlanControlReady(vlan *unstructured.Unstructured) bool {
	if crString(vlan, "spec", "vClusterName") != "" {
		return goodConditions(vlan, "Valid", "Initialized")
	}
	return goodConditions(vlan, "Valid", "Ready")
}
func eipAllocated(r egressResolved, eip, pool, gw, vlan *unstructured.Unstructured) (bool, string) {
	address := crString(eip, "spec", "ipAddress")
	ip, err := netip.ParseAddr(address)
	cidr, e2 := netip.ParsePrefix(r.pool.Cidr)
	if err != nil || e2 != nil || !ip.Is4() || !cidr.Contains(ip) || ip == cidr.Masked().Addr() || !cidr.Contains(ip.Next()) {
		return false, ""
	}
	for _, excluded := range r.pool.ExcludedIps {
		lo, hi, rangeIP := strings.Cut(excluded, "..")
		if !rangeIP {
			hi = lo
		}
		a, ea := netip.ParseAddr(lo)
		b, eb := netip.ParseAddr(hi)
		if ea != nil || eb != nil || (ip.Compare(a) >= 0 && ip.Compare(b) <= 0) {
			return false, ""
		}
	}
	if !goodConditions(eip, "Valid", "Initialized") || crString(eip, "spec", "subnet") != kcSystemNamespace+"/"+crName(pool) || crString(eip, "status", "gateway") != crName(gw) || !publicPoolReady(r, pool, gw, vlan) {
		return false, address
	}
	return true, address
}
func bindingApplied(eip, snat, vpc *unstructured.Unstructured, enabled bool) bool {
	if !goodConditions(snat, "Valid") || !goodConditions(eip, "Valid", "Initialized") || (vpc == nil || (enabled && (!goodConditions(vpc, "Valid", "Initialized", "Ready") || crInt(vpc, "status", "observedGeneration") != vpc.GetGeneration()))) || crString(eip, "status", "phase") != "Bound" || snat.GetNamespace() != eip.GetNamespace() || snat.GetNamespace() != vpc.GetNamespace() {
		return false
	}
	phase := "Disabled"
	if enabled {
		phase = "Bound"
	}
	if crString(snat, "status", "phase") != phase || crBool(snat, "spec", "disable") == enabled || crString(snat, "spec", "eip") != eip.GetName() || crString(snat, "spec", "vpc") != vpc.GetName() || crString(snat, "status", "eipAddress") != crString(eip, "spec", "ipAddress") {
		return false
	}
	if crString(eip, "status", "boundResource", "resourceType") != "Snat" || crString(eip, "status", "boundResource", "resource") != snat.GetName() || crString(eip, "status", "boundResource", "vpc") != vpc.GetNamespace()+"/"+vpc.GetName() || crInt(eip, "status", "boundResource", "observedGeneration") != snat.GetGeneration() || crBool(eip, "status", "boundResource", "disabled") == enabled {
		return false
	}
	return !enabled || crString(eip, "status", "boundResource", "nodeName") != ""
}
func eipBindingConflict(set egressReadSet, namespace, eip, expectedSnat string) bool {
	return eipBindingConflictAllowed(set, namespace, eip, expectedSnat, "")
}
func eipBindingConflictAllowed(set egressReadSet, namespace, eip, expectedSnat, serviceUID string) bool {
	if eip == "" {
		return false
	}
	for _, gvr := range []schema.GroupVersionResource{kcSnats, kcNats} {
		for _, o := range set.all[gvr] {
			// Namespace participates in every reference; a cross-namespace same name
			// never counts as this binding or as proof of its applied state.
			if kcRef(crString(&o, "spec", "eip"), o.GetNamespace()) != namespace+"/"+eip {
				continue
			}
			if gvr == kcSnats && o.GetNamespace() == namespace && o.GetName() == expectedSnat {
				continue
			}
			return true
		}
	}
	for _, svc := range set.all[kcServices] {
		if crString(&svc, "spec", "type") != "LoadBalancer" {
			continue
		}
		for _, name := range strings.Split(svc.GetAnnotations()["networking.kubercloud.com/lb_eips"], ",") {
			if kcRef(strings.TrimSpace(name), svc.GetNamespace()) == namespace+"/"+eip {
				if svc.GetNamespace() == namespace && serviceUID != "" && string(svc.GetUID()) == serviceUID {
					continue
				}
				return true
			}
		}
	}

	return false
}
func (p *KCProvider) EnsureEgress(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	if t.Kind == "device" {
		return p.ensureDevice(ctx, t)
	}
	r, err := p.resolveEgress(ctx, t)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	if t.KnownIdentity != "" || r.binding.ProviderUid != "" {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	if t.TenantID != "" {
		if err = p.ensureNamespace(ctx, r.binding); err != nil {
			return biz.ProviderObservation{}, err
		}
	}
	endpoint := resourceEndpoint(p.client, r.binding)
	o, err := endpoint.Get(ctx, r.binding.ProviderName, metav1.GetOptions{})
	if err == nil {
		if err = inspectEgressIdentity(o, r.binding, t.ResourceID, ""); err != nil {
			return biz.ProviderObservation{}, err
		}
		t.Direct = true
		return p.observeEgress(ctx, t)
	}
	if !apierrors.IsNotFound(err) {
		return biz.ProviderObservation{}, readFailure(err)
	}
	set, err := p.readEgressSet(ctx, r, true)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	if _, err = inspectEgressSet(r, set); err != nil {
		return biz.ProviderObservation{}, err
	}
	if !egressPrerequisites(r, set) {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderTemporary}
	}
	o, err = endpoint.Create(ctx, egressObject(r), metav1.CreateOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
	if apierrors.IsAlreadyExists(err) {
		o, err = endpoint.Get(ctx, r.binding.ProviderName, metav1.GetOptions{})
	}
	if err != nil {
		return biz.ProviderObservation{}, mutationFailure(err)
	}
	if err = inspectEgressIdentity(o, r.binding, t.ResourceID, ""); err != nil {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
	}
	t.KnownIdentity = string(o.GetUID())
	t.Direct = true
	observed, err := p.observeEgress(ctx, t)
	if err != nil {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
	}
	return observed, nil
}
func (p *KCProvider) UpdateEgress(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	if t.Kind != "snat" || t.Egress == nil || t.KnownIdentity == "" {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	r, err := p.resolveEgress(ctx, t)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	endpoint := resourceEndpoint(p.client, r.binding)
	o, err := endpoint.Get(ctx, r.binding.ProviderName, metav1.GetOptions{})
	if err != nil {
		return biz.ProviderObservation{}, readFailure(err)
	}
	if err = inspectEgressIdentity(o, r.binding, t.ResourceID, t.KnownIdentity); err != nil {
		return biz.ProviderObservation{}, err
	}
	if !matchesEgressSpec(o, r.spec, "snat") {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	set, err := p.readEgressSet(ctx, r, true)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	checked, err := inspectEgressSet(r, set)
	if err != nil {
		return checked, err
	}
	current := set.objects["self"]
	if current == nil {
		return checked, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	o = current
	if t.Egress.DesiredEnabled && crBool(o, "spec", "disable") {
		ready, _ := allocatedEIP(r, set.objects["eip"], set)
		if !ready || !verifiedEgressExit(r, set) || !bindingApplied(set.objects["eip"], o, set.objects["vpc"], false) || eipBindingConflict(set, r.binding.Namespace, crName(set.objects["eip"]), r.binding.ProviderName) {
			return checked, &biz.ProviderError{Kind: biz.ProviderTemporary}
		}
	}
	if crBool(o, "spec", "disable") == t.Egress.DesiredEnabled {
		patch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": t.KnownIdentity}, {"op": "test", "path": "/metadata/resourceVersion", "value": o.GetResourceVersion()}, {"op": "add", "path": "/spec/disable", "value": !t.Egress.DesiredEnabled}})
		_, err = endpoint.Patch(ctx, r.binding.ProviderName, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: "ani-network-service", FieldValidation: "Strict"})
		if err != nil {
			return biz.ProviderObservation{}, mutationFailure(err)
		}
	}
	t.Direct = true
	observed, err := p.observeEgress(ctx, t)
	if err != nil {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
	}
	return observed, nil
}
func (p *KCProvider) deleteEgress(ctx context.Context, t biz.ProviderTarget) error {
	if t.Kind == "device" || t.KnownIdentity == "" {
		return &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	t.Direct = true
	r, err := p.resolveEgress(ctx, t)
	if err != nil {
		return err
	}
	// Removing our exact provider object must remain possible after hardware or
	// group degradation. UID, owner, references and finalizers still fence the
	// delete; a positive physical readiness proof only gates creation/activation.
	if r.virtualSwitch != nil {
		r.deviceID = ""
		r.virtualSwitch = nil
	}
	set, err := p.readEgressSet(ctx, r, true)
	if err != nil {
		return err
	}
	observed, err := inspectEgressSet(r, set)
	if err != nil {
		return err
	}
	if observed.HasDependencies {
		return &biz.ProviderError{Kind: biz.ProviderInUse}
	}
	o := set.objects["self"]
	if o == nil {
		return nil
	}
	uid, rv := types.UID(t.KnownIdentity), o.GetResourceVersion()
	orphan := metav1.DeletePropagationOrphan
	err = resourceEndpoint(p.client, r.binding).Delete(ctx, r.binding.ProviderName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}, PropagationPolicy: &orphan})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return mutationFailure(err)
	}
	return nil
}

// This gate is repeated immediately before POST, outside all SQL transactions.
func egressPrerequisites(r egressResolved, set egressReadSet) bool {
	if set.deviceRequired && !set.deviceReady {
		return false
	}
	gw := set.objects["gateway"]
	vlan := set.objects["vlan"]
	switch r.target.Kind {
	case "vlan":
		if r.virtualSwitch != nil {
			return set.switchConfigured
		}
	case "public_pool":
		if r.pool.Scope == "intranet" {
			return intranetInfrastructureReady(r.pool, set.objects["default_vpc"], set.objects["intranet_config"], set.all[kcServiceCIDRs], set.all[pods])
		}
		if !goodConditions(gw, "Valid", "Initialized", "Ready") || crString(gw, "status", "localIP") == "" || crString(gw, "status", "boundResources", "router") == "" {
			return false
		}
		if r.pool.Mode == "underlay" {
			missing, _, _ := unstructured.NestedSlice(objectMap(vlan), "status", "notReadyNodes")
			return vlanControlReady(vlan) && len(missing) == 0 && crString(vlan, "status", "subnet") == ""
		}
		return true
	case "eip":
		return addressPoolReady(r, set.objects["pool"], set) && verifiedEgressExit(r, set)
	case "snat":
		eip, vpc := set.objects["eip"], set.objects["vpc"]
		ok, _ := allocatedEIP(r, eip, set)
		return ok && verifiedEgressExit(r, set) && goodConditions(vpc, "Valid", "Initialized", "Ready") && crInt(vpc, "status", "observedGeneration") == vpc.GetGeneration() && crString(eip, "status", "phase") == "Available" && emptyBoundResource(eip) && !eipBindingConflict(set, r.binding.Namespace, crName(eip), r.binding.ProviderName)
	}
	return true
}

// Record actual running provider digests. Missing, mixed rollout, or unready
// provider pods remove admission evidence; tags and declared images are not proof.
func providerImages(objects []unstructured.Unstructured) []string {
	digests := []string{}
	roles := map[string]bool{}
	for _, p := range objects {
		if p.GetNamespace() != kcSystemNamespace {
			continue
		}
		role := p.GetLabels()["networking.kubercloud.com/app"]
		switch role {
		case "controller", "cni-ds", "ovs-ds", "ovn-central":
		default:
			continue
		}
		if p.GetDeletionTimestamp() != nil || crString(&p, "status", "phase") != "Running" {
			return nil
		}
		statuses, _, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
		containers, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
		if len(statuses) == 0 || len(statuses) != len(containers) {
			return nil
		}
		for _, raw := range statuses {
			c, ok := raw.(map[string]any)
			if !ok || c["ready"] != true {
				return nil
			}
			id, _ := c["imageID"].(string)
			idx := strings.LastIndex(id, "sha256:")
			if idx < 0 {
				return nil
			}
			digest := id[idx:]
			if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest) {
				return nil
			}
			digests = append(digests, digest)
		}
		roles[role] = true
	}
	for _, role := range []string{"controller", "cni-ds", "ovs-ds", "ovn-central"} {
		if !roles[role] {
			return nil
		}
	}
	slices.Sort(digests)
	return slices.Compact(digests)
}

func emptyBoundResource(eip *unstructured.Unstructured) bool {
	raw, found, err := unstructured.NestedFieldNoCopy(objectMap(eip), "status", "boundResource")
	if err != nil {
		return false
	}
	if !found || raw == nil {
		return true
	}
	bound, ok := raw.(map[string]any)
	return ok && len(bound) == 0
}

func verifiedEgressExit(r egressResolved, set egressReadSet) bool {
	// Public admission uses current component readiness, without requiring an
	// administrator's manual forwarding evidence. Intranet remains independent.
	if r.pool.Scope == "public" {
		return len(providerImages(set.all[pods])) > 0
	}
	var evidence biz.PublicPoolVerification
	if json.Unmarshal(r.pool.Verification, &evidence) != nil {
		return false
	}
	return sameProviderImages(evidence.ProviderImageDigests, providerImages(set.all[pods])) && biz.ValidatePoolVerificationForScope(evidence, r.pool.Scope, time.Now()) == nil
}
