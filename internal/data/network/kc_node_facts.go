package data

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const factsOwner = "ani-network-node-facts"
const factsNodeLabel = "network.ani.io/node-uid"
const managedNetdevAnnotation = "networking.kubercloud.com/managed_netdevs"

type NodeFactsDocument struct {
	Version                   int                 `json:"version"`
	NodeName                  string              `json:"node_name"`
	NodeUID                   string              `json:"node_uid"`
	CollectedAt               time.Time           `json:"collected_at"`
	Interfaces                []biz.NodeInterface `json:"interfaces"`
	OVSBridgeMappings         map[string]string   `json:"ovs_bridge_mappings,omitempty"`
	OVSBridgeMappingsObserved bool                `json:"ovs_bridge_mappings_observed,omitempty"`
}

func (p *KCProvider) ListNodeInterfaces(ctx context.Context) (biz.InterfaceInventory, error) {
	return p.listNodeInterfaces(ctx, nil)
}

// A dependent read consumes the caller's complete audit. Starting another audit
// here can outlive that caller's deadline and invalidate the first view while
// waiting. Its existing node-facts fence remains authoritative.
func (p *KCProvider) listNodeInterfaces(ctx context.Context, view *auditView) (biz.InterfaceInventory, error) {
	var nodes, maps, vlans []unstructured.Unstructured
	var collected time.Time
	var err error
	if view == nil {
		nodes, maps, vlans, collected, err = p.nodeFactsObjects(ctx)
	} else {
		if p.observation == nil || !p.observation.valid(view, []string{"node-facts"}) {
			return biz.InterfaceInventory{}, &biz.ProviderError{Kind: biz.ProviderTemporary}
		}
		nodes, maps, vlans, collected, err = nodeFactsFromView(view)
	}
	if err != nil {
		return biz.InterfaceInventory{}, err
	}
	config, err := p.client.Resource(kcConfigMaps).Namespace(kcSystemNamespace).Get(ctx, "kcn-config", metav1.GetOptions{})
	if err != nil {
		return biz.InterfaceInventory{}, readFailure(err)
	}
	if config.GetUID() == "" || config.GetDeletionTimestamp() != nil {
		return biz.InterfaceInventory{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	configured := managedDevices(config)
	byUID := map[string][]unstructured.Unstructured{}
	for _, cm := range maps {
		if cm.GetNamespace() != kcSystemNamespace || cm.GetLabels()[ownerLabel] != factsOwner {
			continue
		}
		byUID[cm.GetLabels()[factsNodeLabel]] = append(byUID[cm.GetLabels()[factsNodeLabel]], cm)
	}
	now, err := p.repository.queries.DatabaseTime(ctx)
	if err != nil {
		return biz.InterfaceInventory{}, readFailure(err)
	}
	items := []biz.NodeInterface{}
	for _, node := range nodes {
		uid := string(node.GetUID())
		sources := byUID[uid]
		var doc NodeFactsDocument
		valid := len(sources) == 1
		if valid {
			valid = sources[0].GetNamespace() == kcSystemNamespace && sources[0].GetLabels()[ownerLabel] == factsOwner && sources[0].GetDeletionTimestamp() == nil && json.Unmarshal([]byte(crString(&sources[0], "data", "facts.json")), &doc) == nil && doc.Version == 1 && doc.NodeName == node.GetName() && doc.NodeUID == uid && len(doc.Interfaces) > 0
		}
		if !valid {
			items = append(items, biz.NodeInterface{NodeName: node.GetName(), NodeUID: uid, UnavailableReasons: []string{"node_facts_missing_or_invalid"}})
			continue
		}
		managed := map[string]bool{}
		if raw := node.GetAnnotations()[managedNetdevAnnotation]; raw != "" {
			if json.Unmarshal([]byte(raw), &managed) != nil {
				return biz.InterfaceInventory{}, &biz.ProviderError{Kind: biz.ProviderTemporary}
			}
		}
		names := map[string]bool{}
		for _, iface := range doc.Interfaces {
			if iface.Name == "" || names[iface.Name] || iface.NodeName != doc.NodeName || iface.NodeUID != doc.NodeUID {
				return biz.InterfaceInventory{}, &biz.ProviderError{Kind: biz.ProviderConflict}
			}
			names[iface.Name] = true
			iface.ObservedAt = doc.CollectedAt
			iface.KCManaged = managed[iface.Name]
			iface.KCConfigured = slices.Contains(configured, iface.Name)
			iface.KCConfigUID = string(config.GetUID())
			iface.KCDeviceOwner = config.GetAnnotations()[adoptionMarker(iface.Name)]
			iface.Selectable = false
			iface.UnavailableReasons = nil
			iface.VlanNetworkIDs = nil
			for _, vlan := range vlans {
				if crString(&vlan, "spec", "devName") == iface.Name {
					if vlan.GetLabels()[ownerLabel] == "ani-network-service" && vlan.GetLabels()[resourceLabel] != "" {
						iface.VlanNetworkIDs = append(iface.VlanNetworkIDs, vlan.GetLabels()[resourceLabel])
					} else {
						iface.UnavailableReasons = append(iface.UnavailableReasons, "provider_vlan_in_use")
					}
				}
			}
			slices.Sort(iface.VlanNetworkIDs)
			items = append(items, iface)
		}
	}
	items = biz.FilterNodeInterfaces(items, now, time.Minute)
	slices.SortFunc(items, func(a, b biz.NodeInterface) int {
		if v := strings.Compare(a.NodeUID, b.NodeUID); v != 0 {
			return v
		}
		return strings.Compare(a.Name, b.Name)
	})
	stable := slices.Clone(items)
	for i := range stable {
		stable[i].ObservedAt = time.Time{}
	}
	return biz.InterfaceInventory{Items: items, Fingerprint: contentHash(stable), CollectedAt: collected}, nil
}
func adoptionMarker(device string) string {
	return "network.ani.io/device-adoption-" + contentHash(device)[:16]
}
func managedDevices(cm *unstructured.Unstructured) []string {
	values := []string{}
	for _, v := range strings.Split(crString(cm, "data", "managedDevices"), ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			values = append(values, v)
		}
	}
	slices.Sort(values)
	return slices.Compact(values)
}
func (p *KCProvider) deviceConfig(ctx context.Context, t biz.ProviderTarget) (sqlcgen.NetworkDeviceAdoption, error) {
	r, err := p.repository.queries.GetDeviceAdoption(ctx, sqlcgen.GetDeviceAdoptionParams{ClusterID: p.repository.placement.ClusterID, ResourceID: t.ResourceID})
	if err != nil {
		return r, readFailure(err)
	}
	b, err := p.egressBinding(ctx, t)
	if err != nil {
		return r, err
	}
	if b.BindingID != t.BindingID {
		return r, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	return r, nil
}
func (p *KCProvider) observeDevice(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	return p.observeDeviceFromView(ctx, t, nil)
}

func (p *KCProvider) observeDeviceFromView(ctx context.Context, t biz.ProviderTarget, view *auditView) (biz.ProviderObservation, error) {
	config, err := p.deviceConfig(ctx, t)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	cm, err := p.client.Resource(kcConfigMaps).Namespace(kcSystemNamespace).Get(ctx, "kcn-config", metav1.GetOptions{})
	if err != nil {
		return biz.ProviderObservation{}, readFailure(err)
	}
	if cm.GetUID() == "" || cm.GetResourceVersion() == "" || (t.KnownIdentity != "" && string(cm.GetUID()) != t.KnownIdentity) {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	var accepted []biz.NodeInterface
	if json.Unmarshal(config.NodeInventory, &accepted) != nil || len(accepted) == 0 {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	preManaged := accepted[0].KCConfigured
	for _, item := range accepted {
		if item.KCConfigured != preManaged || (item.KCConfigUID != "" && item.KCConfigUID != string(cm.GetUID())) {
			return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
	}
	configured := slices.Contains(managedDevices(cm), config.DeviceName)
	marker := cm.GetAnnotations()[adoptionMarker(config.DeviceName)]
	if (marker != "" && marker != t.BindingID) || (marker == "" && configured && !preManaged) || (preManaged && !configured) {
		return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	inventory, err := p.listNodeInterfaces(ctx, view)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	// Compare timestamps after reading facts: a collection completed during the
	// read is not a future observation. Truly future/stale facts still fail.
	now, err := p.repository.queries.DatabaseTime(ctx)
	if err != nil {
		return biz.ProviderObservation{}, readFailure(err)
	}
	nodes := map[string]bool{}
	identities := map[string]biz.NodeInterface{}
	for _, v := range accepted {
		nodes[v.NodeUID] = true
		identities[v.NodeUID] = v
	}
	current := map[string]bool{}
	progress := []biz.NodeInterface{}
	ready := len(nodes) > 0
	collected := inventory.CollectedAt
	for _, v := range inventory.Items {
		current[v.NodeUID] = true
		if v.Name != config.DeviceName {
			continue
		}
		progress = append(progress, v)
		identity := identities[v.NodeUID]
		if (identity.MAC != "" && identity.MAC != v.MAC) || (identity.NodeName != "" && identity.NodeName != v.NodeName) {
			return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		if v.KCConfigUID != string(cm.GetUID()) {
			return biz.ProviderObservation{}, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
		if !nodes[v.NodeUID] || !v.KCManaged || !v.OVSManaged || !v.KCConfigured || !v.KCBridgeReady || !v.LinkUp || !v.Carrier || v.Kind != "device" || v.Management || v.DefaultRoute || v.Master != "ovs-system" || v.ObservedAt.IsZero() || v.ObservedAt.After(now) || now.Sub(v.ObservedAt) > time.Minute {
			ready = false
		}
		for _, raw := range v.Addresses {
			address, err := netip.ParsePrefix(raw)
			if err != nil || !address.Addr().IsLinkLocalUnicast() {
				ready = false
			}
		}
	}
	if len(current) != len(nodes) || len(progress) != len(nodes) {
		ready = false
	}
	for uid := range current {
		if !nodes[uid] {
			ready = false
		}
	}
	value := biz.ProviderObservation{Exists: marker == t.BindingID, Ready: ready && marker == t.BindingID && slices.Contains(managedDevices(cm), config.DeviceName), Egress: &biz.EgressAppliedFacts{DeviceNodes: progress}}
	if value.Exists {
		value.Identity = string(cm.GetUID())
	}
	// Proof dates the complete source collection. Each node retains its raw
	// collector timestamp; stale/missing nodes above make Ready false, while
	// a new collection may persist partial adoption progress from other nodes.
	if collected.IsZero() {
		return value, &biz.ProviderError{Kind: biz.ProviderTemporary}
	}
	value.Proof = biz.ObservationProof{CollectedAt: collected, Hash: contentHash(struct {
		Marker  string
		Devices []string
		Nodes   []biz.NodeInterface
	}{marker, managedDevices(cm), progress}), CoveredGeneration: t.Requirement.RequestedGeneration}
	return value, nil
}
func (p *KCProvider) ensureDevice(ctx context.Context, t biz.ProviderTarget) (biz.ProviderObservation, error) {
	config, err := p.deviceConfig(ctx, t)
	if err != nil {
		return biz.ProviderObservation{}, err
	}
	existing, err := p.observeDevice(ctx, t)
	if err != nil {
		return existing, err
	}
	if existing.Exists {
		return existing, nil
	}
	if t.KnownIdentity != "" {
		return existing, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	inventory, err := p.ListNodeInterfaces(ctx)
	if err != nil {
		return existing, err
	}
	if inventory.Fingerprint != config.InventoryFingerprint {
		return existing, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	seen := map[string]bool{}
	nodes := map[string]bool{}
	for _, v := range inventory.Items {
		nodes[v.NodeUID] = true
		if v.Name == config.DeviceName {
			if !v.Selectable {
				return existing, &biz.ProviderError{Kind: biz.ProviderConflict}
			}
			seen[v.NodeUID] = true
		}
	}
	if len(nodes) == 0 || len(nodes) != len(seen) {
		return existing, &biz.ProviderError{Kind: biz.ProviderTemporary}
	}
	endpoint := p.client.Resource(kcConfigMaps).Namespace(kcSystemNamespace)
	cm, err := endpoint.Get(ctx, "kcn-config", metav1.GetOptions{})
	if err != nil {
		return existing, readFailure(err)
	}
	devices := managedDevices(cm)
	var accepted []biz.NodeInterface
	if json.Unmarshal(config.NodeInventory, &accepted) != nil || len(accepted) == 0 {
		return existing, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	preManaged := accepted[0].KCConfigured
	for _, item := range accepted {
		if item.KCConfigured != preManaged || (item.KCConfigUID != "" && item.KCConfigUID != string(cm.GetUID())) {
			return existing, &biz.ProviderError{Kind: biz.ProviderConflict}
		}
	}
	if slices.Contains(devices, config.DeviceName) != preManaged || cm.GetAnnotations()[adoptionMarker(config.DeviceName)] != "" {
		return existing, &biz.ProviderError{Kind: biz.ProviderConflict}
	}
	annotations := cm.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[adoptionMarker(config.DeviceName)] = t.BindingID
	ops := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": string(cm.GetUID())}, {"op": "test", "path": "/metadata/resourceVersion", "value": cm.GetResourceVersion()}}
	if !preManaged {
		devices = append(devices, config.DeviceName)
		slices.Sort(devices)
		ops = append(ops, map[string]any{"op": "add", "path": "/data/managedDevices", "value": strings.Join(devices, ",")})
	}
	ops = append(ops, map[string]any{"op": "add", "path": "/metadata/annotations", "value": annotations})
	patch, _ := json.Marshal(ops)
	if err = p.EnsureLegacyDeviceMutationAllowed(ctx, t); err != nil {
		return existing, err
	}
	_, err = endpoint.Patch(ctx, "kcn-config", types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: "ani-network-service"})
	if err != nil {
		return existing, mutationFailure(err)
	}
	observed, err := p.observeDevice(ctx, t)
	if err != nil {
		return observed, &biz.ProviderError{Kind: biz.ProviderUncertain, Cause: err}
	}
	return observed, nil
}

// Infrastructure collision checks are read-only and happen outside admission
// transactions. The full ServiceCIDR source is required, not just allocated IPs.
func (p *KCProvider) ValidatePublicPool(ctx context.Context, c biz.PublicPoolConfig) error {
	cidr, err := netip.ParsePrefix(c.CIDR)
	if err != nil {
		return biz.Fail(biz.InvalidArgument, "invalid pool CIDR")
	}
	overlaps := func(raw string) bool {
		p, err := netip.ParsePrefix(raw)
		return err == nil && p.Addr().Is4() && p.Overlaps(cidr)
	}
	subnets, err := p.listAll(ctx, kcSubnets)
	if err != nil {
		return err
	}
	for _, s := range subnets {
		if crString(&s, "spec", "type") != "VPC" && overlaps(crString(&s, "spec", "cidrBlock")) {
			return biz.Fail(biz.ResourceInUse, "pool overlaps provider infrastructure")
		}
	}
	nodes, err := p.listAll(ctx, kcNodes)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		addresses, _, _ := unstructured.NestedSlice(n.Object, "status", "addresses")
		for _, raw := range addresses {
			a, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			s, _ := a["address"].(string)
			ip, err := netip.ParseAddr(s)
			if err == nil && cidr.Contains(ip) {
				return biz.Fail(biz.ResourceInUse, "pool overlaps node infrastructure")
			}
		}
		for _, field := range []string{"podCIDR"} {
			if overlaps(crString(&n, "spec", field)) {
				return biz.Fail(biz.ResourceInUse, "pool overlaps cluster Pod network")
			}
		}
	}
	services, err := p.listAll(ctx, schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "servicecidrs"})
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return biz.Fail(biz.DependencyUnavailable, "Service CIDR facts are unavailable")
	}
	for _, s := range services {
		cidrs, _, _ := unstructured.NestedStringSlice(s.Object, "spec", "cidrs")
		for _, other := range cidrs {
			if overlaps(other) {
				return biz.Fail(biz.ResourceInUse, "pool overlaps cluster Service network")
			}
		}
	}
	return nil
}

func (p *KCProvider) nodeFactsObjects(ctx context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, []unstructured.Unstructured, time.Time, error) {
	if p.observation != nil {
		var after time.Time
		refreshed := false
		for {
			if err := ctx.Err(); err != nil {
				return nil, nil, nil, time.Time{}, readFailure(fmt.Errorf("node facts audit stopped: %w", err))
			}
			view, err := p.observation.snapshot(ctx, after)
			if err != nil {
				return nil, nil, nil, time.Time{}, readFailure(fmt.Errorf("node facts audit collection failed: %w", err))
			}
			nodes, maps, vlans, collected, err := nodeFactsFromView(view)
			if err != nil {
				return nil, nil, nil, time.Time{}, err
			}
			if err = ctx.Err(); err != nil {
				return nil, nil, nil, time.Time{}, readFailure(fmt.Errorf("node facts audit stopped: %w", err))
			}
			if p.observation.valid(view, []string{"node-facts"}) {
				return nodes, maps, vlans, collected, nil
			}
			// A related event or Watch boundary can invalidate more than one
			// collection. Recollect within the original caller budget, always
			// after a new DB-clock boundary. Real read failures are not retries.
			if _, bounded := ctx.Deadline(); !bounded && refreshed {
				return nil, nil, nil, time.Time{}, readFailure(fmt.Errorf("node facts audit invalidated after one fresh collection"))
			}
			after, err = p.repository.queries.DatabaseTime(ctx)
			if err != nil {
				return nil, nil, nil, time.Time{}, readFailure(fmt.Errorf("node facts audit boundary failed: %w", err))
			}
			refreshed = true
		}
	}
	collected, err := p.repository.queries.DatabaseTime(ctx)
	if err != nil {
		return nil, nil, nil, time.Time{}, readFailure(err)
	}
	nodes, err := p.listAll(ctx, kcNodes)
	if err != nil {
		return nil, nil, nil, time.Time{}, err
	}
	maps, err := p.client.Resource(kcConfigMaps).Namespace(kcSystemNamespace).List(ctx, metav1.ListOptions{LabelSelector: ownerLabel + "=" + factsOwner})
	if err != nil {
		return nil, nil, nil, time.Time{}, readFailure(err)
	}
	vlans, err := p.listAll(ctx, kcVlans)
	if err != nil {
		return nil, nil, nil, time.Time{}, err
	}
	return nodes, maps.Items, vlans, collected, nil
}

func nodeFactsFromView(view *auditView) ([]unstructured.Unstructured, []unstructured.Unstructured, []unstructured.Unstructured, time.Time, error) {
	for _, gvr := range []schema.GroupVersionResource{kcNodes, kcConfigMaps, kcVlans} {
		if view.indices[gvr] == nil {
			return nil, nil, nil, time.Time{}, readFailure(fmt.Errorf("node facts audit is missing %s", gvr.Resource))
		}
	}
	items := func(gvr schema.GroupVersionResource) []unstructured.Unstructured {
		out := []unstructured.Unstructured{}
		for _, raw := range view.indices[gvr].List() {
			out = append(out, *raw.(*unstructured.Unstructured).DeepCopy())
		}
		return out
	}
	return items(kcNodes), items(kcConfigMaps), items(kcVlans), view.collected, nil
}
