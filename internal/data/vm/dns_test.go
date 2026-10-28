package vm

import (
	"slices"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestNativeDNSUsesFrozenAddressesAndKeepsLegacyTemplates(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{}}}}}
	if e := configureDNS(nil, object); e != nil {
		t.Fatal(e)
	}
	if _, found, _ := unstructured.NestedString(object.Object, "spec", "template", "spec", "dnsPolicy"); found {
		t.Fatal("legacy spec acquired new DNS behavior")
	}
	dns := &biz.DNS{Mode: "CUSTOM", Nameservers: []string{"223.5.5.5", "2001:db8::1"}}
	if e := configureDNS(dns, object); e != nil {
		t.Fatal(e)
	}
	policy, _, _ := unstructured.NestedString(object.Object, "spec", "template", "spec", "dnsPolicy")
	servers, _, _ := unstructured.NestedStringSlice(object.Object, "spec", "template", "spec", "dnsConfig", "nameservers")
	if policy != "None" || !slices.Equal(servers, dns.Nameservers) {
		t.Fatal("KubeVirt did not receive the frozen DNS policy", object.Object)
	}
}
