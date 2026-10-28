package data

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func switchFixture(t *testing.T) (biz.VirtualSwitch, []unstructured.Unstructured, []unstructured.Unstructured, []unstructured.Unstructured, *unstructured.Unstructured, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	node := unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "n1", "uid": "uid1", "resourceVersion": "1", "labels": map[string]any{allocatedNodeLabel: "true"}, "annotations": map[string]any{"networking.kubercloud.com/chassis": "chassis1"}}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}}
	iface := biz.NodeInterface{NodeName: "n1", NodeUID: "uid1", Name: "ens35", Kind: "device", MAC: "00:11:22:33:44:55", LinkUp: true, Carrier: true}
	body, _ := json.Marshal(NodeFactsDocument{Version: 1, NodeName: "n1", NodeUID: "uid1", CollectedAt: now, Interfaces: []biz.NodeInterface{iface}})
	facts := unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "facts", "namespace": kcSystemNamespace, "uid": "facts1", "resourceVersion": "2", "labels": map[string]any{ownerLabel: factsOwner, factsNodeLabel: "uid1"}}, "data": map[string]any{"facts.json": string(body)}}}
	daemon := unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "cni1", "namespace": kcSystemNamespace, "labels": map[string]any{"networking.kubercloud.com/app": "cni-ds"}}, "spec": map[string]any{"nodeName": "n1", "containers": []any{map[string]any{"name": "cni"}}}, "status": map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": "cni", "ready": true}}}}}
	config := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "kcn-config", "namespace": kcSystemNamespace, "uid": "config1", "resourceVersion": "3"}}}
	return biz.VirtualSwitch{GroupName: "test-group"}, []unstructured.Unstructured{node}, []unstructured.Unstructured{facts}, []unstructured.Unstructured{daemon}, config, now
}

func TestVirtualSwitchInventoryRequiresEveryAllocatedNode(t *testing.T) {
	for _, failure := range []string{"", "missing_interface", "stale", "node_unready", "missing_daemon", "external_same_value", "extra_allocated", "management"} {
		t.Run(failure, func(t *testing.T) {
			sw, nodes, maps, daemons, config, now := switchFixture(t)
			var doc NodeFactsDocument
			_ = json.Unmarshal([]byte(crString(&maps[0], "data", "facts.json")), &doc)
			switch failure {
			case "missing_interface":
				doc.Interfaces[0].Name = "ens36"
			case "stale":
				doc.CollectedAt = now.Add(-2 * time.Minute)
			case "node_unready":
				_ = unstructured.SetNestedSlice(nodes[0].Object, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
			case "missing_daemon":
				daemons = nil
			case "external_same_value":
				labels := nodes[0].GetLabels()
				labels[virtualSwitchGroupPrefix+sw.GroupName] = "100"
				nodes[0].SetLabels(labels)
			case "extra_allocated":
				extra := nodes[0].DeepCopy()
				extra.SetName("n2")
				extra.SetUID("uid2")
				nodes = append(nodes, *extra)
			case "management":
				doc.Interfaces[0].Management = true
			}
			body, _ := json.Marshal(doc)
			_ = unstructured.SetNestedField(maps[0].Object, string(body), "data", "facts.json")
			v, err := virtualSwitchInventory(sw, "ens35", nodes, maps, nil, daemons, config, now, now)
			if failure != "" {
				if err == nil {
					t.Fatal("incomplete or foreign inventory admitted")
				}
				return
			}
			if err != nil || len(v.Members) != 1 || len(v.Inventory.Items) != 1 || len(v.Fingerprint) != 64 || !v.Members[0].Allocated || v.Members[0].Priority != 100 {
				t.Fatal(v, err)
			}
		})
	}
}

func TestVirtualSwitchWiringRequiresOwnGroupMapping(t *testing.T) {
	sw, nodes, maps, _, _, _ := switchFixture(t)
	var doc NodeFactsDocument
	_ = json.Unmarshal([]byte(crString(&maps[0], "data", "facts.json")), &doc)
	v := doc.Interfaces[0]
	v.Master = "ovs-system"
	v.OVSManaged = true
	v.KCBridgeReady = true
	annotations := nodes[0].GetAnnotations()
	annotations[virtualSwitchGroupPrefix+sw.GroupName] = "ens35"
	nodes[0].SetAnnotations(annotations)
	v.KCBridgeNetworks = []string{"other-group.ens35"}
	if switchWiring(&nodes[0], v, sw.GroupName) {
		t.Fatal("foreign mapping proved readiness")
	}
	v.KCBridgeNetworks = []string{sw.GroupName + ".ens35"}
	if !switchWiring(&nodes[0], v, sw.GroupName) {
		t.Fatal("real group mapping missing")
	}
	delete(annotations, virtualSwitchGroupPrefix+sw.GroupName)
	nodes[0].SetAnnotations(annotations)
	if switchWiring(&nodes[0], v, sw.GroupName) {
		t.Fatal("missing daemon annotation proved readiness")
	}
}

func TestInspectSwitchUnknownRequiresBindingOwner(t *testing.T) {
	sw, nodes, _, _, _, _ := switchFixture(t)
	sw.BindingID = "binding1"
	member := biz.VirtualSwitchNode{NodeName: "n1", NodeUID: "uid1", MAC: "00:11:22:33:44:55", Priority: 100, Allocated: true, Prepared: true, Unknown: true}
	labels := nodes[0].GetLabels()
	labels[virtualSwitchGroupPrefix+sw.GroupName] = "100"
	nodes[0].SetLabels(labels)
	if _, err := inspectSwitchNodeObject(sw, member, &nodes[0]); err == nil {
		t.Fatal("same label value adopted without owner")
	}
	annotations := nodes[0].GetAnnotations()
	annotations[VirtualSwitchOwnerKey(sw.GroupName)] = sw.BindingID
	nodes[0].SetAnnotations(annotations)
	unprepared := member
	unprepared.Prepared = false
	if _, err := inspectSwitchNodeObject(sw, unprepared, &nodes[0]); err == nil {
		t.Fatal("unrecorded dispatch adopted")
	}
	got, err := inspectSwitchNodeObject(sw, member, &nodes[0])
	if err != nil || !got.Applied || got.Unknown || got.ResourceVersion != "1" {
		t.Fatal(got, err)
	}
	annotations[VirtualSwitchOwnerKey(sw.GroupName)] = "foreign"
	nodes[0].SetAnnotations(annotations)
	if _, err := inspectSwitchNodeObject(sw, member, &nodes[0]); err == nil {
		t.Fatal("foreign binding owner admitted")
	}
	if len(strings.SplitN(VirtualSwitchOwnerKey(strings.Repeat("a", 63)), "/", 2)[1]) > 63 {
		t.Fatal("illegal annotation key")
	}
}

func TestV09VlanCreationPrecedesWiringButReadinessDoesNot(t *testing.T) {
	r := egressResolved{target: biz.ProviderTarget{Kind: "vlan", ResourceID: "vlan-id"}, binding: sqlcgen.NetworkProviderBinding{ResourceKind: "vlan", ProviderName: "vlan-provider", BindingID: "binding"}, spec: map[string]any{"devName": "ens35", "vClusterName": "test-group", "vlanID": int64(0)}, virtualSwitch: &biz.VirtualSwitch{GroupName: "test-group"}}
	set := egressReadSet{deviceRequired: true, deviceReady: true, switchConfigured: true, objects: map[string]*unstructured.Unstructured{}}
	if !egressPrerequisites(r, set) {
		t.Fatal("creation waited for the physical wiring it must initiate")
	}
	o := egressObject(r)
	o.SetUID("vlan-uid")
	o.SetResourceVersion("1")
	o.SetGeneration(1)
	_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}}, "status", "conditions")
	set.objects["self"] = o
	value, err := inspectEgressSet(r, set)
	if err != nil || value.Ready {
		t.Fatal("unwired VLAN reported ready", value, err)
	}
	set.switchWired = true
	value, err = inspectEgressSet(r, set)
	if err != nil || !value.Ready {
		t.Fatal("v0.9 VLAN incorrectly required unpublished Ready condition", value, err)
	}
	set.switchConfigured = false
	if egressPrerequisites(r, set) {
		t.Fatal("missing owned group admitted")
	}
}

func TestKCBridgeFactsRetainV09GroupNetwork(t *testing.T) {
	c := &NodeFactsCollector{Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		cmd := name + " " + strings.Join(args, " ")
		switch cmd {
		case "ovs-vsctl --timeout=3 iface-to-br ens35":
			return []byte("br-ens35"), nil
		case "ovs-vsctl --timeout=3 --format=json --columns=external_ids list Bridge br-ens35":
			return []byte(`{"headings":["external_ids"],"data":[[["map",[["vendor","kc-networking"],["purpose","vlan-network"]]]]]}`), nil
		case "ovs-vsctl --timeout=3 --format=json --columns=external_ids list Open_vSwitch .":
			return []byte(`{"headings":["external_ids"],"data":[[["map",[["ovn-bridge-mappings","test-group.ens35:br-ens35,foreign.ens35:br-other,dupe.ens35:br-ens35,dupe.ens35:br-ens35"]]]]]}`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", cmd)
	}}
	bridge, networks, err := c.collectKCBridgeFacts(context.Background(), "ens35")
	if err != nil || bridge != "br-ens35" || len(networks) != 1 || networks[0] != "test-group.ens35" {
		t.Fatal(bridge, networks, err)
	}
}

// Node source timestamps gate physical freshness independently of the time of a
// complete Kubernetes collection, which must cover the prior committed fact.
func TestVirtualSwitchObservationProofCoversConsecutiveCollections(t *testing.T) {
	sw, nodes, maps, daemons, config, now := switchFixture(t)
	collected := now.Add(20 * time.Second)
	current, err := virtualSwitchInventoryMode(sw, "ens35", nodes, maps, nil, daemons, config, collected, collected, false)
	if err != nil {
		t.Fatal(err)
	}
	proof := virtualSwitchObservationProof(sw, current.Inventory, collected)
	proof.CoveredGeneration = 2
	requirement := biz.ObservationRequirement{RequestedGeneration: 2, AppliedAt: now.Add(10 * time.Second), Hash: "prior-applied-fact"}
	if !proof.Covers(requirement) {
		t.Fatalf("new complete collection was fenced by old node timestamp: proof=%+v requirement=%+v", proof, requirement)
	}
	if !proof.CollectedAt.Equal(now) {
		t.Fatal("reading source facts renewed physical freshness", proof)
	}
	requirement.PreviousCollectedAt = now.Add(time.Second)
	if proof.Covers(requirement) {
		t.Fatal("physical evidence regressed behind committed source", proof)
	}
	requirement.PreviousCollectedAt = time.Time{}
	proof.CollectionStartedAt = now.Add(5 * time.Second)
	if proof.Covers(requirement) {
		t.Fatal("older API collection bypassed commit fence", proof)
	}
	if _, err = virtualSwitchInventoryMode(sw, "ens35", nodes, maps, nil, daemons, config, now.Add(61*time.Second), now.Add(61*time.Second), false); err == nil {
		t.Fatal("expired physical facts accepted")
	}
}

func TestVirtualSwitchDependentReadRejectsInvalidParentViewForBoundedRetry(t *testing.T) {
	p := &KCProvider{observation: &KCObservation{epoch: 2}}
	_, _, proof, err := p.virtualSwitchDeviceState(context.Background(), biz.VirtualSwitch{}, sqlcgen.NetworkDeviceAdoption{}, &auditView{epoch: 1})
	if err != errEgressAuditInvalid || !proof.CollectedAt.IsZero() {
		t.Fatal("dependent read escaped parent audit fence", proof, err)
	}
}
