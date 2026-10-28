package data

import (
	"encoding/json"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func networkObservationNode() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Node",
		"metadata": map[string]any{"name": "node2", "uid": "node-uid", "resourceVersion": "1",
			"labels": map[string]any{allocatedNodeLabel: "true", virtualSwitchGroupPrefix + "public": "100"},
			"annotations": map[string]any{"networking.kubercloud.com/chassis": "chassis", VirtualSwitchOwnerKey("public"): "binding", virtualSwitchGroupPrefix + "public": "ens8f1np1",
				"volcano.sh/node-vgpu-handshake": "Requesting_1", "volcano.sh/node-vgpu-register": "gpu-a,gpu-b"}},
		"spec": map[string]any{"podCIDR": "10.244.0.0/24"},
		"status": map[string]any{"addresses": []any{map[string]any{"type": "InternalIP", "address": "10.10.1.67"}},
			"conditions":  []any{map[string]any{"type": "Ready", "status": "True", "lastHeartbeatTime": "2026-10-27T12:00:00Z"}},
			"capacity":    map[string]any{"cpu": "64", "nvidia.com/gpu": "8"},
			"allocatable": map[string]any{"cpu": "60", "nvidia.com/gpu": "8"}},
	}}
}

func TestNodeNetworkObservationIgnoresOnlyUnrelatedRenewals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  []string
		value any
		keep  bool
	}{
		{"resource_version", []string{"metadata", "resourceVersion"}, "2", true},
		{"managed_fields", []string{"metadata", "managedFields"}, []any{map[string]any{"manager": "kubelet", "time": "2026-10-27T12:01:00Z"}}, true},
		{"volcano_heartbeat", []string{"metadata", "annotations", "volcano.sh/node-vgpu-handshake"}, "Requesting_2", true},
		{"gpu_registration_order", []string{"metadata", "annotations", "volcano.sh/node-vgpu-register"}, "gpu-b,gpu-a", true},
		{"gpu_capacity", []string{"status", "capacity", "nvidia.com/gpu"}, "4", true},
		{"gpu_allocatable", []string{"status", "allocatable", "nvidia.com/gpu"}, "4", true},
		{"node_heartbeat", []string{"status", "conditions"}, []any{map[string]any{"type": "Ready", "status": "True", "lastHeartbeatTime": "2026-10-27T12:01:00Z"}}, true},
		{"ready_lost", []string{"status", "conditions"}, []any{map[string]any{"type": "Ready", "status": "False", "lastHeartbeatTime": "2026-10-27T12:01:00Z"}}, false},
		{"identity_replaced", []string{"metadata", "uid"}, "replacement", false},
		{"deletion_started", []string{"metadata", "deletionTimestamp"}, "2026-10-27T12:01:00Z", false},
		{"network_owner", []string{"metadata", "annotations", VirtualSwitchOwnerKey("public")}, "foreign", false},
		{"group_membership", []string{"metadata", "labels", virtualSwitchGroupPrefix + "public"}, "50", false},
		{"allocation", []string{"metadata", "labels", allocatedNodeLabel}, "false", false},
		{"chassis", []string{"metadata", "annotations", "networking.kubercloud.com/chassis"}, "replacement", false},
		{"device_wiring", []string{"metadata", "annotations", virtualSwitchGroupPrefix + "public"}, "ens35", false},
		{"address", []string{"status", "addresses"}, []any{map[string]any{"type": "InternalIP", "address": "10.10.1.99"}}, false},
		{"pod_cidr", []string{"spec", "podCIDR"}, "10.245.0.0/24", false},
		{"unknown_annotation", []string{"metadata", "annotations", "future.example/network"}, "changed", false},
		{"unknown_volcano_annotation", []string{"metadata", "annotations", "volcano.sh/future-network"}, "changed", false},
		{"unknown_field", []string{"status", "futureNetworkFact"}, "changed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := networkObservationNode()
			after := before.DeepCopy()
			after.SetResourceVersion("2")
			if err := unstructured.SetNestedField(after.Object, tc.value, tc.path...); err != nil {
				t.Fatal(err)
			}
			oldBytes, _ := json.Marshal(before.Object)
			newBytes, _ := json.Marshal(after.Object)
			o := &KCObservation{options: DefaultObservationOptions(), pending: map[string]struct{}{}, changes: map[string]uint64{}}
			at := time.Now().Add(-time.Second)
			view := &auditView{collected: at}
			o.changed(before, after)
			if got := o.valid(view, []string{"node-facts"}); got != tc.keep {
				t.Fatalf("network audit validity=%v want %v", got, tc.keep)
			}
			oldAfter, _ := json.Marshal(before.Object)
			newAfter, _ := json.Marshal(after.Object)
			if string(oldBytes) != string(oldAfter) || string(newBytes) != string(newAfter) || !view.collected.Equal(at) {
				t.Fatal("filter mutated objects or renewed the audit timestamp")
			}
		})
	}
	for _, tc := range []struct {
		name          string
		before, after *unstructured.Unstructured
	}{
		{"add", nil, networkObservationNode()}, {"delete", networkObservationNode(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &KCObservation{options: DefaultObservationOptions(), pending: map[string]struct{}{}, changes: map[string]uint64{}}
			o.changed(tc.before, tc.after)
			if o.valid(&auditView{}, []string{"node-facts"}) {
				t.Fatal("node identity lifecycle change ignored")
			}
		})
	}
}
