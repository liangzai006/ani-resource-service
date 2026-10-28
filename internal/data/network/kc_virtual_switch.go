package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const virtualSwitchGroupPrefix = "vcluster.networking.kubercloud.com/"
const allocatedNodeLabel = "networking.kubercloud.com/allocated"

// The hash keeps the annotation name within Kubernetes' 63 character limit,
// including when a legal group name itself is 63 characters long.
func VirtualSwitchOwnerKey(group string) string {
	return "network.ani.io/virtual-switch-" + contentHash(group)[:32]
}

func virtualSwitchDeleteFenceKey(group string) string {
	return "network.ani.io/virtual-switch-fence-" + contentHash(group)[:32]
}

func virtualSwitchDeleteFenceValue(sw biz.VirtualSwitch) string {
	return sw.BindingID + ":" + strconv.FormatInt(sw.Version, 10)
}

func switchConflict(causes ...error) error {
	return &biz.ProviderError{Kind: biz.ProviderConflict, Cause: errors.Join(causes...)}
}
func switchUnavailable(causes ...error) error {
	return &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: errors.Join(causes...)}
}

func (p *KCProvider) VirtualSwitchInventory(ctx context.Context, cluster string, sw biz.VirtualSwitch, device string) (biz.VirtualSwitchInventory, error) {
	if cluster == "" || cluster != p.repository.placement.ClusterID || (sw.ClusterID != "" && sw.ClusterID != cluster) || device == "" {
		return biz.VirtualSwitchInventory{}, switchConflict()
	}
	nodes, maps, vlans, collected, err := p.nodeFactsObjects(ctx)
	if err != nil {
		return biz.VirtualSwitchInventory{}, err
	}
	daemons, err := p.listAll(ctx, pods)
	if err != nil {
		return biz.VirtualSwitchInventory{}, err
	}
	cm, err := p.client.Resource(kcConfigMaps).Namespace(kcSystemNamespace).Get(ctx, "kcn-config", metav1.GetOptions{})
	if err != nil {
		return biz.VirtualSwitchInventory{}, readFailure(err)
	}
	now, err := p.repository.queries.DatabaseTime(ctx)
	if err != nil {
		return biz.VirtualSwitchInventory{}, readFailure(err)
	}
	return virtualSwitchInventory(sw, device, nodes, maps, vlans, daemons, cm, now, collected)
}

// Every allocated node is retained. A missing interface or facts object fails
// the entire admission, rather than silently shrinking the switch membership.
func virtualSwitchInventory(sw biz.VirtualSwitch, device string, nodes, maps, vlans, daemons []unstructured.Unstructured, config *unstructured.Unstructured, now, collected time.Time) (biz.VirtualSwitchInventory, error) {
	return virtualSwitchInventoryMode(sw, device, nodes, maps, vlans, daemons, config, now, collected, true)
}

func virtualSwitchInventoryMode(sw biz.VirtualSwitch, device string, nodes, maps, vlans, daemons []unstructured.Unstructured, config *unstructured.Unstructured, now, collected time.Time, admission bool) (biz.VirtualSwitchInventory, error) {
	result := biz.VirtualSwitchInventory{Inventory: biz.InterfaceInventory{CollectedAt: collected}}
	if config == nil || config.GetUID() == "" || config.GetResourceVersion() == "" || config.GetDeletionTimestamp() != nil || collected.IsZero() {
		return result, switchUnavailable()
	}
	if config.GetAnnotations()[adoptionMarker(device)] != "" {
		return result, switchConflict()
	}
	if err := validateSwitchMembers(sw, nodes); err != nil {
		return result, err
	}
	for _, vlan := range vlans {
		if crString(&vlan, "spec", "vClusterName") == sw.GroupName && (sw.BindingID == "" || vlan.GetLabels()[ownerLabel] != "ani-network-service" || vlan.GetLabels()[resourceLabel] == "") {
			return result, switchConflict()
		}
	}
	byUID := map[string][]unstructured.Unstructured{}
	for _, m := range maps {
		if m.GetNamespace() == kcSystemNamespace && m.GetLabels()[ownerLabel] == factsOwner {
			byUID[m.GetLabels()[factsNodeLabel]] = append(byUID[m.GetLabels()[factsNodeLabel]], m)
		}
	}
	for _, n := range nodes {
		if n.GetLabels()[allocatedNodeLabel] != "true" {
			continue
		}
		if !nodeReady(&n) || n.GetAnnotations()["networking.kubercloud.com/chassis"] == "" || !switchDaemonReady(n.GetName(), daemons) {
			return result, switchUnavailable()
		}
		uid := string(n.GetUID())
		if uid == "" || n.GetResourceVersion() == "" {
			return result, switchConflict()
		}
		sources := byUID[uid]
		if len(sources) != 1 {
			return result, switchUnavailable()
		}
		cm := sources[0]
		var doc NodeFactsDocument
		if cm.GetUID() == "" || cm.GetResourceVersion() == "" || cm.GetDeletionTimestamp() != nil || json.Unmarshal([]byte(crString(&cm, "data", "facts.json")), &doc) != nil || doc.Version != 1 || doc.NodeName != n.GetName() || doc.NodeUID != uid || doc.CollectedAt.IsZero() || doc.CollectedAt.After(now) || now.Sub(doc.CollectedAt) > time.Minute {
			return result, switchUnavailable()
		}
		if doc.CollectedAt.Before(result.Inventory.CollectedAt) {
			result.Inventory.CollectedAt = doc.CollectedAt
		}
		var selected *biz.NodeInterface
		seen := map[string]bool{}
		for _, v := range doc.Interfaces {
			if v.Name == "" || seen[v.Name] || v.NodeName != n.GetName() || v.NodeUID != uid {
				return result, switchConflict()
			}
			seen[v.Name] = true
			if v.Name == device {
				copy := v
				selected = &copy
			}
		}
		if selected == nil {
			return result, switchUnavailable()
		}
		v := *selected
		v.ObservedAt = doc.CollectedAt
		v.KCConfigUID = string(config.GetUID())
		if admission && !switchPhysicalSafe(v) {
			return result, switchConflict()
		}
		owned := sw.BindingID != "" && n.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)] == sw.BindingID && n.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName] == "100"
		if (v.OVSManaged || v.Master != "") && (!owned || (admission && !switchWiring(&n, v, sw.GroupName))) {
			return result, switchConflict()
		}
		for _, vlan := range vlans {
			if crString(&vlan, "spec", "devName") != device {
				continue
			}
			if sw.BindingID == "" || crString(&vlan, "spec", "vClusterName") != sw.GroupName || vlan.GetLabels()[ownerLabel] != "ani-network-service" || vlan.GetLabels()[resourceLabel] == "" {
				return result, switchConflict()
			}
			v.VlanNetworkIDs = append(v.VlanNetworkIDs, vlan.GetLabels()[resourceLabel])
		}
		v.Selectable = true
		v.UnavailableReasons = nil
		slices.Sort(v.VlanNetworkIDs)
		result.Inventory.Items = append(result.Inventory.Items, v)
		result.Members = append(result.Members, biz.VirtualSwitchNode{NodeName: n.GetName(), NodeUID: uid, MAC: v.MAC, Priority: 100, Allocated: true, ResourceVersion: n.GetResourceVersion()})
	}
	if len(result.Members) == 0 {
		return result, switchUnavailable()
	}
	slices.SortFunc(result.Members, func(a, b biz.VirtualSwitchNode) int { return strings.Compare(a.NodeUID, b.NodeUID) })
	slices.SortFunc(result.Inventory.Items, func(a, b biz.NodeInterface) int { return strings.Compare(a.NodeUID, b.NodeUID) })
	stable := slices.Clone(result.Members)
	for i := range stable {
		stable[i].ResourceVersion = ""
	}
	result.Fingerprint = contentHash(stable)
	inventory := slices.Clone(result.Inventory.Items)
	for i := range inventory {
		inventory[i].ObservedAt = time.Time{}
	}
	result.Inventory.Fingerprint = contentHash(inventory)
	return result, nil
}

func (p *KCProvider) virtualSwitchDeviceState(ctx context.Context, sw biz.VirtualSwitch, device sqlcgen.NetworkDeviceAdoption, view *auditView) (bool, bool, biz.ObservationProof, error) {
	var proof biz.ObservationProof
	var nodes, maps, vlans, daemons []unstructured.Unstructured
	var collected time.Time
	var err error
	if view == nil {
		nodes, maps, vlans, collected, err = p.nodeFactsObjects(ctx)
		if err == nil {
			daemons, err = p.listAll(ctx, pods)
		}
	} else {
		// A dependent check must consume the caller's complete collection.
		// Starting a second audit can invalidate the parent and starve its proof.
		if p.observation == nil || !p.observation.valid(view, []string{"node-facts"}) || view.indices[pods] == nil {
			return false, false, proof, errEgressAuditInvalid
		}
		nodes, maps, vlans, collected, err = nodeFactsFromView(view)
		for _, raw := range view.indices[pods].List() {
			o := raw.(*unstructured.Unstructured)
			if o.GetNamespace() == kcSystemNamespace && o.GetLabels()["networking.kubercloud.com/app"] == "cni-ds" {
				daemons = append(daemons, *o.DeepCopy())
			}
		}
	}
	if err != nil {
		return false, false, proof, err
	}
	cm, err := p.client.Resource(kcConfigMaps).Namespace(kcSystemNamespace).Get(ctx, "kcn-config", metav1.GetOptions{})
	if err != nil {
		return false, false, proof, readFailure(err)
	}
	now, err := p.repository.queries.DatabaseTime(ctx)
	if err != nil {
		return false, false, proof, readFailure(err)
	}
	current, err := virtualSwitchInventoryMode(sw, device.DeviceName, nodes, maps, vlans, daemons, cm, now, collected, false)
	if err != nil {
		return false, false, proof, err
	}
	var frozen []biz.NodeInterface
	if json.Unmarshal(device.NodeInventory, &frozen) != nil || len(frozen) != len(current.Inventory.Items) {
		return false, false, proof, switchConflict()
	}
	identities := map[string]biz.NodeInterface{}
	for _, v := range frozen {
		if identities[v.NodeUID].NodeUID != "" {
			return false, false, proof, switchConflict()
		}
		identities[v.NodeUID] = v
	}
	byUID := map[string]*unstructured.Unstructured{}
	for i := range nodes {
		byUID[string(nodes[i].GetUID())] = &nodes[i]
	}
	configured, wired := sw.BindingState == "configured", true
	for _, v := range current.Inventory.Items {
		expected, ok := identities[v.NodeUID]
		if !ok || expected.NodeName != v.NodeName || expected.Name != v.Name || expected.MAC != v.MAC {
			return false, false, proof, switchConflict()
		}
		n := byUID[v.NodeUID]
		owned := n.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)] == sw.BindingID && n.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName] == "100"
		idle := !v.OVSManaged && v.Master == ""
		configured = configured && owned && switchPhysicalSafe(v) && (idle || switchWiring(n, v, sw.GroupName))
		wired = wired && owned && switchWiring(n, v, sw.GroupName)
	}
	proof = virtualSwitchObservationProof(sw, current.Inventory, collected)
	return configured, wired && configured, proof, nil
}

func virtualSwitchObservationProof(sw biz.VirtualSwitch, inventory biz.InterfaceInventory, collected time.Time) biz.ObservationProof {
	// Keep physical-source freshness separate from the complete API read fence.
	return biz.ObservationProof{CollectedAt: inventory.CollectedAt, CollectionStartedAt: collected, Hash: contentHash(struct {
		SwitchID, BindingID, Fingerprint string
		Version                          int64
		Inventory                        biz.InterfaceInventory
	}{sw.ID, sw.BindingID, sw.MembershipFingerprint, sw.BindingVersion, inventory})}
}

func switchPhysicalSafe(v biz.NodeInterface) bool {
	if v.Kind != "device" || v.MAC == "" || !v.LinkUp || !v.Carrier || v.Management || v.DefaultRoute {
		return false
	}
	mac, err := net.ParseMAC(v.MAC)
	if err != nil || len(mac) != 6 {
		return false
	}
	for _, raw := range v.Addresses {
		ip, err := netip.ParsePrefix(raw)
		if err != nil || !ip.Addr().IsLinkLocalUnicast() {
			return false
		}
	}
	return true
}

func switchWiring(n *unstructured.Unstructured, v biz.NodeInterface, group string) bool {
	return switchPhysicalSafe(v) && v.OVSManaged && v.Master == "ovs-system" && v.KCBridgeReady && slices.Contains(v.KCBridgeNetworks, group+"."+v.Name) && slices.Contains(strings.FieldsFunc(n.GetAnnotations()[virtualSwitchGroupPrefix+group], func(r rune) bool { return r == ',' || r == ' ' }), v.Name)
}

func nodeReady(n *unstructured.Unstructured) bool {
	if n == nil || n.GetDeletionTimestamp() != nil {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(n.Object, "status", "conditions")
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if ok && c["type"] == "Ready" {
			return c["status"] == "True"
		}
	}
	return false
}

func switchDaemonReady(node string, daemons []unstructured.Unstructured) bool {
	count := 0
	for _, p := range daemons {
		if p.GetNamespace() != kcSystemNamespace || p.GetLabels()["networking.kubercloud.com/app"] != "cni-ds" || crString(&p, "spec", "nodeName") != node {
			continue
		}
		count++
		if !nodeReady(&p) || crString(&p, "status", "phase") != "Running" {
			return false
		}
		statuses, _, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
		containers, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
		if len(statuses) == 0 || len(statuses) != len(containers) {
			return false
		}
		for _, raw := range statuses {
			s, ok := raw.(map[string]any)
			if !ok || s["ready"] != true {
				return false
			}
		}
	}
	return count == 1
}

func validateSwitchMembers(sw biz.VirtualSwitch, nodes []unstructured.Unstructured) error {
	frozen := map[string]biz.VirtualSwitchNode{}
	for _, m := range sw.Nodes {
		if m.NodeUID == "" || frozen[m.NodeUID].NodeUID != "" {
			return switchConflict()
		}
		frozen[m.NodeUID] = m
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		uid := string(n.GetUID())
		allocated := n.GetLabels()[allocatedNodeLabel] == "true"
		_, hasLabel := n.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName]
		_, hasOwner := n.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)]
		_, hasFeedback := n.GetAnnotations()[virtualSwitchGroupPrefix+sw.GroupName]
		if hasLabel || hasOwner || hasFeedback {
			m, ok := frozen[uid]
			if !ok || !allocated || m.NodeName != n.GetName() || sw.BindingID == "" || n.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)] != sw.BindingID || n.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName] != "100" {
				return switchConflict()
			}
		}
		if !allocated {
			continue
		}
		if len(frozen) > 0 {
			m, ok := frozen[uid]
			if !ok || m.NodeName != n.GetName() || !m.Allocated || m.Priority != 100 {
				return switchConflict()
			}
		}
		seen[uid] = true
	}
	if len(frozen) > 0 && len(seen) != len(frozen) {
		return switchConflict()
	}
	return nil
}

func (p *KCProvider) InspectVirtualSwitchNode(ctx context.Context, sw biz.VirtualSwitch, member biz.VirtualSwitchNode) (biz.VirtualSwitchNode, error) {
	if sw.ClusterID != p.repository.placement.ClusterID || sw.BindingID == "" {
		return member, switchConflict(fmt.Errorf("virtual switch %s cluster or binding identity invalid", sw.ID))
	}
	nodes, maps, _, collected, err := p.nodeFactsObjects(ctx)
	if err != nil {
		return member, err
	}
	if sw.State != biz.Deleting {
		if err = validateSwitchMembers(sw, nodes); err != nil {
			return member, switchConflict(fmt.Errorf("virtual switch %s frozen membership changed: %w", sw.ID, err))
		}
		daemons, err := p.listAll(ctx, pods)
		if err != nil {
			return member, err
		}
		now, err := p.repository.queries.DatabaseTime(ctx)
		if err != nil {
			return member, readFailure(err)
		}
		physicalAt, inspectErr := inspectFrozenSwitchPhysical(sw, nodes, maps, daemons, now)
		if inspectErr != nil {
			return member, inspectErr
		}
		if physicalAt.Before(collected) {
			collected = physicalAt
		}
		if collected.IsZero() || collected.After(now) {
			return member, switchUnavailable(fmt.Errorf("virtual switch %s has invalid observation time %s", sw.ID, collected))
		}
		if !member.LastObservedAt.IsZero() && collected.Before(member.LastObservedAt) {
			return member, switchUnavailable(fmt.Errorf("virtual switch %s node %s observation moved backwards from %s to %s", sw.ID, member.NodeName, member.LastObservedAt, collected))
		}
	}
	for _, n := range nodes {
		if n.GetName() == member.NodeName {
			current, err := inspectSwitchNodeObject(sw, member, &n)
			if err != nil {
				return current, switchConflict(fmt.Errorf("virtual switch %s node %s identity, owner or group configuration mismatch: %w", sw.ID, member.NodeName, err))
			}
			if err == nil && sw.State != biz.Deleting && current.Applied {
				current.LastObservedAt = collected
			}
			if err != nil || sw.State != biz.Deleting || !current.DeleteFenced || current.Applied {
				return current, err
			}
			devices, err := p.repository.queries.ListVirtualSwitchDevices(ctx, sqlcgen.ListVirtualSwitchDevicesParams{ClusterID: sw.ClusterID, SwitchID: sw.ID})
			if err != nil {
				return current, readFailure(err)
			}
			_, freshMaps, freshVlans, err := p.readFreshRetirementFacts(ctx)
			if err != nil {
				return current, err
			}
			freshNode, err := p.client.Resource(kcNodes).Get(ctx, member.NodeName, metav1.GetOptions{})
			if err != nil {
				return current, readFailure(err)
			}
			now, err := p.repository.queries.DatabaseTime(ctx)
			if err != nil {
				return current, readFailure(err)
			}
			return inspectSwitchNodeCleanup(sw, current, freshNode, freshMaps, freshVlans, devices, now)
		}
	}
	return member, switchConflict(fmt.Errorf("virtual switch %s member node %s missing", sw.ID, member.NodeName))
}

func inspectSwitchNodeCleanup(sw biz.VirtualSwitch, member biz.VirtualSwitchNode, node *unstructured.Unstructured, maps, vlans []unstructured.Unstructured, devices []sqlcgen.NetworkDeviceAdoption, now time.Time) (biz.VirtualSwitchNode, error) {
	current, err := inspectSwitchNodeObject(sw, member, node)
	if err != nil || current.Applied || !current.DeleteFenced {
		return current, err
	}
	clean, err := switchNodeCleanupConfirmed(sw, current, node, maps, vlans, devices, now)
	if err != nil || !clean {
		return current, err
	}
	current.State = "cleaned"
	if _, present := node.GetAnnotations()[virtualSwitchDeleteFenceKey(sw.GroupName)]; present {
		current.State = "unfencing"
	}
	return current, nil
}

func inspectFrozenSwitchPhysical(sw biz.VirtualSwitch, nodes, maps, daemons []unstructured.Unstructured, now time.Time) (time.Time, error) {
	oldest := now
	for _, member := range sw.Nodes {
		var node *unstructured.Unstructured
		for i := range nodes {
			if string(nodes[i].GetUID()) == member.NodeUID && nodes[i].GetName() == member.NodeName {
				node = &nodes[i]
				break
			}
		}
		if !nodeReady(node) {
			return time.Time{}, switchUnavailable(fmt.Errorf("node not ready: %s", member.NodeName))
		}
		if node.GetAnnotations()["networking.kubercloud.com/chassis"] == "" || !switchDaemonReady(member.NodeName, daemons) {
			return time.Time{}, switchUnavailable(fmt.Errorf("node %s chassis or CNI daemon not ready", member.NodeName))
		}
		found := 0
		for _, cm := range maps {
			if cm.GetNamespace() != kcSystemNamespace || cm.GetLabels()[ownerLabel] != factsOwner || cm.GetLabels()[factsNodeLabel] != member.NodeUID {
				continue
			}
			found++
			var doc NodeFactsDocument
			if cm.GetUID() == "" || cm.GetResourceVersion() == "" || cm.GetDeletionTimestamp() != nil || json.Unmarshal([]byte(crString(&cm, "data", "facts.json")), &doc) != nil || doc.Version != 1 || doc.NodeName != member.NodeName || doc.NodeUID != member.NodeUID {
				return time.Time{}, switchUnavailable(fmt.Errorf("node %s facts document or identity invalid", member.NodeName))
			}
			if doc.CollectedAt.IsZero() || doc.CollectedAt.After(now) {
				return time.Time{}, switchUnavailable(fmt.Errorf("node %s facts time invalid: collected_at=%s now=%s", member.NodeName, doc.CollectedAt, now))
			}
			if now.Sub(doc.CollectedAt) > time.Minute {
				return time.Time{}, switchUnavailable(fmt.Errorf("node facts expired: %s age=%s", member.NodeName, now.Sub(doc.CollectedAt)))
			}
			if doc.CollectedAt.Before(oldest) {
				oldest = doc.CollectedAt
			}
			interfaces := 0
			for _, v := range doc.Interfaces {
				if v.MAC != member.MAC || v.Kind != "device" {
					continue
				}
				interfaces++
				if !switchPhysicalSafe(v) {
					return time.Time{}, switchConflict(fmt.Errorf("physical interface unsafe: node %s interface %s link_up=%t carrier=%t management=%t default_route=%t", member.NodeName, v.Name, v.LinkUp, v.Carrier, v.Management, v.DefaultRoute))
				}
				if v.OVSManaged || v.Master != "" {
					if node.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)] != sw.BindingID || node.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName] != "100" || !switchWiring(node, v, sw.GroupName) {
						return time.Time{}, switchConflict(fmt.Errorf("node %s interface %s virtual switch owner or wiring mismatch", member.NodeName, v.Name))
					}
				}
			}
			if interfaces != 1 {
				return time.Time{}, switchConflict(fmt.Errorf("node %s expected one physical interface for MAC %s, found %d", member.NodeName, member.MAC, interfaces))
			}
		}
		if found != 1 {
			return time.Time{}, switchUnavailable(fmt.Errorf("node facts missing or duplicated: %s count=%d", member.NodeName, found))
		}
	}
	return oldest, nil
}

func inspectSwitchNodeObject(sw biz.VirtualSwitch, member biz.VirtualSwitchNode, n *unstructured.Unstructured) (biz.VirtualSwitchNode, error) {
	if n == nil || n.GetName() != member.NodeName || string(n.GetUID()) != member.NodeUID || n.GetResourceVersion() == "" || n.GetDeletionTimestamp() != nil {
		return member, switchConflict()
	}
	label, present := n.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName]
	owner, ownerPresent := n.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)]
	if sw.State == biz.Deleting {
		fence, fencePresent := n.GetAnnotations()[virtualSwitchDeleteFenceKey(sw.GroupName)]
		if fencePresent && (!member.Prepared || fence != virtualSwitchDeleteFenceValue(sw)) {
			return member, switchConflict()
		}
		if ownerPresent || present {
			if !member.Prepared || owner != sw.BindingID || !present || label != strconv.Itoa(int(member.Priority)) {
				return member, switchConflict()
			}
			member.Applied = true
		} else {
			member.Applied = false
		}
		member.State = "cleaning"
		member.ResourceVersion = n.GetResourceVersion()
		if fencePresent {
			// The exact marker proves a successful RV barrier even when its
			// response was lost. The worker saves this before removing it.
			member.DeleteFenced = true
			member.Unknown = false
		} else if member.DeleteFenced {
			// A previously persisted barrier also covers a lost unfence reply.
			member.Unknown = false
		}
		return member, nil
	}
	if owner == sw.BindingID && present && label == strconv.Itoa(int(member.Priority)) {
		if !member.Prepared {
			return member, switchConflict()
		}
		member.Applied = true
		member.State = "applied"
		member.Unknown = false
		member.ResourceVersion = n.GetResourceVersion()
		return member, nil
	}
	if ownerPresent || present {
		return member, switchConflict()
	}
	if member.Applied && !member.Unknown && sw.State != biz.Deleting {
		return member, switchConflict()
	}
	member.Applied = false
	member.State = "pending"
	member.Unknown = false
	member.ResourceVersion = n.GetResourceVersion()
	if !member.Prepared {
		member.OriginalPresent = false
		member.OriginalValue = ""
		member.OriginalOwner = ""
	}
	return member, nil
}

func jsonPointerKey(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func (p *KCProvider) PatchVirtualSwitchNode(ctx context.Context, sw biz.VirtualSwitch, member biz.VirtualSwitchNode, deleting bool) (biz.VirtualSwitchNode, error) {
	if !member.Prepared || member.ResourceVersion == "" || sw.BindingID == "" || sw.ClusterID != p.repository.placement.ClusterID || member.Priority != 100 || !member.Allocated || member.OriginalPresent || member.OriginalOwner != "" {
		return member, switchConflict()
	}
	current, err := p.repository.GetVirtualSwitch(ctx, sw.ID)
	if err != nil {
		return member, err
	}
	if current.BindingID != sw.BindingID || current.BindingVersion != sw.BindingVersion || current.Version != sw.Version || (current.State == biz.Deleting) != deleting || current.State == biz.Deleted {
		return member, switchConflict()
	}
	durable := false
	for _, saved := range current.Nodes {
		if saved.NodeUID == member.NodeUID && saved.NodeName == member.NodeName && saved.MAC == member.MAC && saved.Prepared && saved.ResourceVersion == member.ResourceVersion && saved.OriginalPresent == member.OriginalPresent && saved.OriginalOwner == member.OriginalOwner && saved.OriginalValue == member.OriginalValue && (!deleting || (saved.DeleteFenced == member.DeleteFenced && saved.State == member.State)) {
			durable = true
			break
		}
	}
	if !durable {
		return member, switchConflict()
	}
	if !deleting {
		nodes, err := p.listAll(ctx, kcNodes)
		if err != nil {
			return member, err
		}
		if err = validateSwitchMembers(sw, nodes); err != nil {
			return member, err
		}
	}
	n, err := p.client.Resource(kcNodes).Get(ctx, member.NodeName, metav1.GetOptions{})
	if err != nil {
		return member, readFailure(err)
	}
	if string(n.GetUID()) != member.NodeUID || n.GetResourceVersion() != member.ResourceVersion {
		return member, switchConflict()
	}
	if deleting {
		return p.patchVirtualSwitchDeletionNode(ctx, sw, member, n)
	}
	labelKey, ownerKey := virtualSwitchGroupPrefix+sw.GroupName, VirtualSwitchOwnerKey(sw.GroupName)
	label, hasLabel := n.GetLabels()[labelKey]
	owner, ownerPresent := n.GetAnnotations()[ownerKey]
	patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": member.NodeUID}, {"op": "test", "path": "/metadata/resourceVersion", "value": member.ResourceVersion}}
	if owner == sw.BindingID && hasLabel && label == "100" {
		member.Applied = true
		member.State = "applied"
		member.Unknown = false
		return member, nil
	}
	if ownerPresent || hasLabel {
		return member, switchConflict()
	}
	if n.GetLabels() == nil {
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/labels", "value": map[string]any{}})
	}
	if n.GetAnnotations() == nil {
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations", "value": map[string]any{}})
	}
	patch = append(patch, map[string]any{"op": "add", "path": "/metadata/labels/" + jsonPointerKey(labelKey), "value": "100"}, map[string]any{"op": "add", "path": "/metadata/annotations/" + jsonPointerKey(ownerKey), "value": sw.BindingID})
	body, _ := json.Marshal(patch)
	updated, err := p.client.Resource(kcNodes).Patch(ctx, member.NodeName, types.JSONPatchType, body, metav1.PatchOptions{FieldManager: "ani-resource-virtual-switch"})
	if err != nil {
		member.Unknown = true
		return member, mutationFailure(err)
	}
	member.Applied = true
	member.State = "applied"
	member.Unknown = false
	member.ResourceVersion = updated.GetResourceVersion()
	return member, nil
}

// Even an absent group needs a real CAS write: a timed-out old create can still
// be waiting inside the API server with the last prepared resourceVersion.
func (p *KCProvider) patchVirtualSwitchDeletionNode(ctx context.Context, sw biz.VirtualSwitch, member biz.VirtualSwitchNode, n *unstructured.Unstructured) (biz.VirtualSwitchNode, error) {
	labelKey, ownerKey := virtualSwitchGroupPrefix+sw.GroupName, VirtualSwitchOwnerKey(sw.GroupName)
	fenceKey, fenceValue := virtualSwitchDeleteFenceKey(sw.GroupName), virtualSwitchDeleteFenceValue(sw)
	label, hasLabel := n.GetLabels()[labelKey]
	owner, hasOwner := n.GetAnnotations()[ownerKey]
	fence, hasFence := n.GetAnnotations()[fenceKey]
	if hasFence && fence != fenceValue {
		return member, switchConflict()
	}
	patch := []map[string]any{{"op": "test", "path": "/metadata/uid", "value": member.NodeUID}, {"op": "test", "path": "/metadata/resourceVersion", "value": member.ResourceVersion}}
	if hasLabel || hasOwner {
		if owner != sw.BindingID || !hasLabel || label != "100" {
			return member, switchConflict()
		}
		patch = append(patch, map[string]any{"op": "test", "path": "/metadata/annotations/" + jsonPointerKey(ownerKey), "value": sw.BindingID}, map[string]any{"op": "test", "path": "/metadata/labels/" + jsonPointerKey(labelKey), "value": "100"}, map[string]any{"op": "remove", "path": "/metadata/labels/" + jsonPointerKey(labelKey)}, map[string]any{"op": "remove", "path": "/metadata/annotations/" + jsonPointerKey(ownerKey)})
	} else if member.DeleteFenced {
		if !hasFence || member.State != "unfencing" {
			member.Unknown = false
			member.State = "cleaning"
			return member, nil
		}
		// Only a persisted barrier plus fresh negative physical evidence may
		// request this step. Unknown removal is recoverable from DeleteFenced.
		patch = append(patch, map[string]any{"op": "test", "path": "/metadata/annotations/" + jsonPointerKey(fenceKey), "value": fenceValue}, map[string]any{"op": "remove", "path": "/metadata/annotations/" + jsonPointerKey(fenceKey)})
	}
	removing := !hasLabel && !hasOwner && member.DeleteFenced && hasFence && member.State == "unfencing"
	if !removing {
		if n.GetAnnotations() == nil {
			patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations", "value": map[string]any{}})
		}
		if hasFence {
			patch = append(patch, map[string]any{"op": "test", "path": "/metadata/annotations/" + jsonPointerKey(fenceKey), "value": fenceValue})
		}
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations/" + jsonPointerKey(fenceKey), "value": fenceValue})
	}
	body, _ := json.Marshal(patch)
	updated, err := p.client.Resource(kcNodes).Patch(ctx, member.NodeName, types.JSONPatchType, body, metav1.PatchOptions{FieldManager: "ani-resource-virtual-switch"})
	if err != nil {
		member.Unknown = true
		return member, mutationFailure(err)
	}
	member.DeleteFenced = true
	member.Applied = false
	member.Unknown = false
	member.State = "cleaning"
	member.ResourceVersion = updated.GetResourceVersion()
	return member, nil
}

func switchNodeCleanupConfirmed(sw biz.VirtualSwitch, member biz.VirtualSwitchNode, node *unstructured.Unstructured, maps, vlans []unstructured.Unstructured, devices []sqlcgen.NetworkDeviceAdoption, now time.Time) (bool, error) {
	if node == nil || string(node.GetUID()) != member.NodeUID || len(devices) == 0 {
		return false, switchUnavailable()
	}
	if _, present := node.GetLabels()[virtualSwitchGroupPrefix+sw.GroupName]; present {
		return false, nil
	}
	if _, present := node.GetAnnotations()[VirtualSwitchOwnerKey(sw.GroupName)]; present {
		return false, nil
	}
	for _, vlan := range vlans {
		if crString(&vlan, "spec", "vClusterName") == sw.GroupName {
			return false, nil
		}
	}
	if strings.TrimSpace(node.GetAnnotations()[virtualSwitchGroupPrefix+sw.GroupName]) != "" {
		return false, nil
	}
	sources := 0
	var doc NodeFactsDocument
	for _, cm := range maps {
		if cm.GetNamespace() != kcSystemNamespace || cm.GetLabels()[ownerLabel] != factsOwner || cm.GetLabels()[factsNodeLabel] != member.NodeUID {
			continue
		}
		sources++
		if cm.GetUID() == "" || cm.GetResourceVersion() == "" || cm.GetDeletionTimestamp() != nil || json.Unmarshal([]byte(crString(&cm, "data", "facts.json")), &doc) != nil || doc.Version != 1 || doc.NodeName != member.NodeName || doc.NodeUID != member.NodeUID || doc.CollectedAt.IsZero() || doc.CollectedAt.After(now) || now.Sub(doc.CollectedAt) > time.Minute || !doc.OVSBridgeMappingsObserved {
			return false, switchUnavailable()
		}
	}
	if sources != 1 {
		return false, switchUnavailable()
	}
	for network := range doc.OVSBridgeMappings {
		if strings.HasPrefix(network, sw.GroupName+".") {
			return false, nil
		}
	}
	for _, device := range devices {
		var accepted []biz.NodeInterface
		if json.Unmarshal(device.NodeInventory, &accepted) != nil {
			return false, switchConflict()
		}
		mac := ""
		for _, v := range accepted {
			if v.NodeUID == member.NodeUID && v.NodeName == member.NodeName && v.Name == device.DeviceName {
				if mac != "" {
					return false, switchConflict()
				}
				mac = v.MAC
			}
		}
		if mac == "" {
			return false, switchConflict()
		}
		found := 0
		for _, v := range doc.Interfaces {
			if v.Name != device.DeviceName {
				continue
			}
			found++
			if v.MAC != mac || v.NodeUID != member.NodeUID || v.NodeName != member.NodeName {
				return false, switchConflict()
			}
			for _, network := range v.KCBridgeNetworks {
				if strings.HasPrefix(network, sw.GroupName+".") {
					return false, nil
				}
			}
			if v.OVSManaged || v.Master != "" {
				if !v.KCBridgeReady || v.Master != "ovs-system" || len(v.KCBridgeNetworks) == 0 {
					return false, nil
				}
				for _, network := range v.KCBridgeNetworks {
					if doc.OVSBridgeMappings[network] != "br-"+v.Name {
						return false, nil
					}
				}
			}
		}
		if found != 1 {
			return false, switchUnavailable()
		}
	}
	return true, nil
}
