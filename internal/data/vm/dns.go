package vm

import (
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func configureDNS(dns *biz.DNS, object *unstructured.Unstructured) error {
	if dns == nil {
		// Older frozen specs retain their original native DNS behavior.
		return nil
	}
	if len(dns.Nameservers) == 0 {
		return biz.ErrInvalid
	}
	if e := unstructured.SetNestedField(object.Object, "None", "spec", "template", "spec", "dnsPolicy"); e != nil {
		return e
	}
	return unstructured.SetNestedStringSlice(object.Object, dns.Nameservers, "spec", "template", "spec", "dnsConfig", "nameservers")
}
