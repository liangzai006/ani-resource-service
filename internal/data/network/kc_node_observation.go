package data

import (
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Ignore only known bookkeeping unrelated to network evidence. All other Node
// fields, including unknown annotations and status fields, remain conservative
// invalidation inputs. The cached/audited objects and their times are untouched.
func unchangedNodeNetworkObservation(before, after *unstructured.Unstructured) bool {
	old, ok := nodeWithoutUnrelatedRenewal(before)
	if !ok {
		return false
	}
	next, ok := nodeWithoutUnrelatedRenewal(after)
	return ok && reflect.DeepEqual(old.Object, next.Object)
}

func nodeWithoutUnrelatedRenewal(object *unstructured.Unstructured) (*unstructured.Unstructured, bool) {
	if object == nil || object.GetAPIVersion() != "v1" || object.GetKind() != "Node" || object.GetName() == "" || object.GetUID() == "" || object.GetResourceVersion() == "" {
		return nil, false
	}
	copy := object.DeepCopy()
	unstructured.RemoveNestedField(copy.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(copy.Object, "metadata", "managedFields")
	for _, key := range []string{"volcano.sh/node-vgpu-handshake", "volcano.sh/node-vgpu-register"} {
		unstructured.RemoveNestedField(copy.Object, "metadata", "annotations", key)
	}
	for _, field := range []string{"capacity", "allocatable"} {
		for _, key := range []string{"nvidia.com/gpu", "amd.com/gpu", "volcano.sh/vgpu-number", "volcano.sh/vgpu-memory", "volcano.sh/vgpu-cores"} {
			unstructured.RemoveNestedField(copy.Object, "status", field, key)
		}
	}
	conditions, found, err := unstructured.NestedSlice(copy.Object, "status", "conditions")
	if err != nil {
		return nil, false
	}
	if found {
		for _, raw := range conditions {
			condition, ok := raw.(map[string]any)
			if !ok {
				return nil, false
			}
			delete(condition, "lastHeartbeatTime")
		}
		if err = unstructured.SetNestedSlice(copy.Object, conditions, "status", "conditions"); err != nil {
			return nil, false
		}
	}
	return copy, true
}
