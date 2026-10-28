package data

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"strings"
	"testing"
)

func TestPublicEgressPrerequisitesWithoutManualVerification(t *testing.T) {
	r, pool, gw, eip := poolFixtures()
	r.pool.Scope = "public"
	r.target.Kind = "eip"
	set := egressReadSet{objects: map[string]*unstructured.Unstructured{"pool": pool, "gateway": gw}, all: map[schema.GroupVersionResource][]unstructured.Unstructured{}}
	for _, role := range []string{"controller", "cni-ds", "ovs-ds", "ovn-central"} {
		pod := crFixture("Pod", kcSystemNamespace, role)
		pod.SetLabels(map[string]string{"networking.kubercloud.com/app": role})
		pod.Object["spec"] = map[string]any{"containers": []any{map[string]any{"name": role}}}
		pod.Object["status"] = map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": role, "ready": true, "imageID": "docker-pullable://provider@sha256:" + strings.Repeat("b", 64)}}}
		set.all[pods] = append(set.all[pods], *pod)
	}
	if !egressPrerequisites(r, set) {
		t.Fatal("ready public pool still requires manual evidence before EIP creation")
	}
	set.objects["gateway"] = nil
	if egressPrerequisites(r, set) {
		t.Fatal("missing public gateway was ignored")
	}
	set.objects["gateway"] = gw
	r.target.Kind = "snat"
	vpc := crFixture("VPC", eip.GetNamespace(), "vpc")
	_ = unstructured.SetNestedField(vpc.Object, int64(2), "status", "observedGeneration")
	_ = unstructured.SetNestedField(eip.Object, "Available", "status", "phase")
	set.objects["vpc"], set.objects["eip"] = vpc, eip
	if !egressPrerequisites(r, set) {
		t.Fatal("ready public EIP still requires manual evidence before SNAT creation")
	}
	_ = unstructured.SetNestedField(eip.Object, "Bound", "status", "phase")
	if egressPrerequisites(r, set) {
		t.Fatal("already bound EIP was accepted")
	}
	r.target.Kind = "eip"
	set.all[pods] = nil
	if egressPrerequisites(r, set) {
		t.Fatal("missing live provider health was ignored")
	}
	r.pool.Scope = "intranet"
	if verifiedEgressExit(r, set) {
		t.Fatal("intranet verification requirement was removed")
	}
}
