package vm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestNativeIndependentBlockVolumesAndUIDOwnership(t *testing.T) {
	ctx := context.Background()
	tenant, id := uuid.NewString(), uuid.NewString()
	nsUID := uuid.NewString()
	sourceUID := uuid.NewString()
	digest := strings.Repeat("a", 64)
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "tenant-test", "uid": nsUID, "labels": map[string]any{"network.ani.io/tenant-id": tenant, "network.ani.io/managed-by": "ani-network-service"}}}}
	source := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"name": "source", "namespace": "media", "uid": sourceUID, "annotations": map[string]any{"vm.ani.io/source-sha256": digest}}, "spec": map[string]any{"volumeMode": "Block"}}}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{podResource: "PodList"}, ns, source)
	client.PrependReactor("create", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		object := a.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		for key, value := range object.GetLabels() {
			if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
				t.Fatalf("native %s label %s rejected by Kubernetes: %v", object.GetKind(), key, errs)
			}
		}
		object.SetUID(types.UID(uuid.NewString()))
		return false, nil, nil
	})
	n := NewNativeWithClients(client, nil)
	v := biz.VM{TenantID: tenant, ID: id, Native: biz.ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-test", Name: "vm-" + id}, Spec: biz.Spec{Digest: strings.Repeat("b", 64), Namespace: "tenant-test", NamespaceUID: nsUID, CPUMillis: 1500, MemoryMiB: 512, Cores: 2, ServiceAccount: "default", StorageClass: "ani-block", Media: biz.Source{Kind: "DISK_IMAGE", Namespace: "media", PVCName: "source", PVCUID: sourceUID, Checksum: digest, VolumeMode: "Block", RootBus: "virtio", NICModel: "virtio"}}, Network: biz.NetworkPlan{Namespace: "tenant-test", NADName: "main", KCNSubnetRef: "tenant-test/kcn-subnet"}}
	v.Disks = []biz.Disk{biz.NewInitialDisk(id, "system", "system", "system", 8), biz.NewInitialDisk(id, "first", "data-a", "data", 2), biz.NewInitialDisk(id, "second", "data-b", "data", 3)}
	v.Spec.DNS = &biz.DNS{Mode: "PLATFORM_DEFAULT", Nameservers: []string{"223.5.5.5"}}
	ref, e := n.EnsureVM(ctx, v)
	if e != nil || ref.UID == "" {
		t.Fatalf("root %v %v", ref, e)
	}
	v.Native = ref
	root, e := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if _, found, _ := unstructured.NestedSlice(root.Object, "spec", "dataVolumeTemplates"); found {
		t.Fatal("embedded DV lifecycle used")
	}
	strategy, _, _ := unstructured.NestedString(root.Object, "spec", "runStrategy")
	cpu, _, _ := unstructured.NestedString(root.Object, "spec", "template", "spec", "domain", "resources", "requests", "cpu")
	cores, _, _ := unstructured.NestedInt64(root.Object, "spec", "template", "spec", "domain", "cpu", "cores")
	if strategy != "Manual" || cpu != "1500m" || cores != 2 {
		t.Fatalf("projection %s %s %d", strategy, cpu, cores)
	}
	policy, _, _ := unstructured.NestedString(root.Object, "spec", "template", "spec", "dnsPolicy")
	servers, _, _ := unstructured.NestedStringSlice(root.Object, "spec", "template", "spec", "dnsConfig", "nameservers")
	if policy != "None" || len(servers) != 1 || servers[0] != "223.5.5.5" {
		t.Fatal("native VM omitted its frozen DNS", root.Object)
	}
	firmware, _, _ := unstructured.NestedString(root.Object, "spec", "template", "spec", "domain", "firmware", "uuid")
	if firmware != id {
		t.Fatal("unstable NoCloud instance identity")
	}
	for i, d := range v.Disks {
		d, e = n.EnsureVolume(ctx, v, d)
		if e != nil {
			t.Fatal(e)
		}
		v.Disks[i] = d
		dv, e := client.Resource(dvResource).Namespace(v.Spec.Namespace).Get(ctx, d.DV.Name, metav1.GetOptions{})
		if e != nil {
			t.Fatal(e)
		}
		mode, _, _ := unstructured.NestedString(dv.Object, "spec", "storage", "volumeMode")
		size, _, _ := unstructured.NestedString(dv.Object, "spec", "storage", "resources", "requests", "storage")
		if mode != "Block" || strings.HasSuffix(size, "Gi") || !strings.HasSuffix(size, "G") || len(dv.GetOwnerReferences()) != 1 || !owned(dv, "VirtualMachine", ref.UID) {
			t.Fatal("DV mode, decimal GB or ownership invalid", dv.Object)
		}
		if again, e := n.EnsureVolume(ctx, v, d); e != nil || again.DV.UID != d.DV.UID {
			t.Fatal("DV retry recreated disk", e)
		}
	}
	unchanged, e := client.Resource(pvcResource).Namespace("media").Get(ctx, "source", metav1.GetOptions{})
	if e != nil || len(unchanged.GetOwnerReferences()) != 0 {
		t.Fatal("shared source ownership changed", e)
	}
	root.SetUID(types.UID(uuid.NewString()))
	if _, e = client.Resource(vmResource).Namespace(v.Spec.Namespace).Update(ctx, root, metav1.UpdateOptions{}); e != nil {
		t.Fatal(e)
	}
	if _, e = n.EnsureVM(ctx, v); e != biz.ErrOwnership {
		t.Fatalf("same-name replacement adopted: %v", e)
	}
}

func TestNativeGuestInitializationSecretRetryAndOwnership(t *testing.T) {
	ctx := context.Background()
	v := biz.VM{TenantID: uuid.NewString(), ID: uuid.NewString(), Spec: biz.Spec{Namespace: "tenant-init", NamespaceUID: uuid.NewString(), Digest: strings.Repeat("b", 64), GuestInitialization: &biz.GuestInitialization{Method: "CLOUD_INIT_NOCLOUD", Username: "vmuser", PasswordSet: true, CredentialIntentDigest: strings.Repeat("c", 64)}}}
	v.Native = biz.ObjectRef{Kind: "VirtualMachine", Name: "vm-" + v.ID, Namespace: v.Spec.Namespace, UID: uuid.NewString()}
	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": v.Spec.Namespace, "uid": v.Spec.NamespaceUID, "labels": map[string]any{"network.ani.io/tenant-id": v.TenantID, "network.ani.io/managed-by": "ani-network-service"}}}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), ns)
	client.PrependReactor("create", "secrets", func(a ktesting.Action) (bool, runtime.Object, error) {
		a.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).SetUID(types.UID(uuid.NewString()))
		return false, nil, nil
	})
	n := NewNativeWithClients(client, nil)
	calls := 0
	n.SetGuestCredentialReader(func(context.Context, biz.VM) (string, error) { calls++; return "$6$fixture$hashed-password", nil })
	ref, e := n.EnsureInitialization(ctx, v)
	if e != nil {
		t.Fatal(e)
	}
	v.InitializationSecret = ref
	secret, e := client.Resource(secretResource).Namespace(v.Spec.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	data, _, _ := unstructured.NestedString(secret.Object, "data", "userdata")
	decoded, e := base64.StdEncoding.DecodeString(data)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(decoded), "\"hashed_passwd\":\"$6$") || !strings.Contains(string(decoded), "\"ssh_pwauth\":false") || !owned(secret, "VirtualMachine", v.Native.UID) {
		t.Fatal("invalid cloud-init secret")
	}
	network, _, _ := unstructured.NestedString(secret.Object, "data", "networkdata")
	networkRaw, e := base64.StdEncoding.DecodeString(network)
	var networkConfig map[string]any
	if e != nil || json.Unmarshal(networkRaw, &networkConfig) != nil {
		t.Fatal("missing bounded network initialization", e)
	}
	match, found, _ := unstructured.NestedString(networkConfig, "ethernets", "default", "match", "name")
	dhcp, _, _ := unstructured.NestedBool(networkConfig, "ethernets", "default", "dhcp4")
	if !found || match != "e*" || !dhcp || strings.Contains(string(networkRaw), "macaddress") || strings.Contains(string(networkRaw), "addresses") || strings.Contains(string(networkRaw), "gateway") {
		t.Fatal("network initialization pins a MAC/address instead of dynamic DHCP")
	}
	immutable, _, _ := unstructured.NestedBool(secret.Object, "immutable")
	if !immutable {
		t.Fatal("initialization secret mutable")
	}
	restarted := NewNativeWithClients(client, nil)
	restarted.SetGuestCredentialReader(func(context.Context, biz.VM) (string, error) { t.Fatal("replay reread credentials"); return "", nil })
	again, e := restarted.EnsureInitialization(ctx, v)
	if e != nil || again.UID != ref.UID || calls != 1 {
		t.Fatal("retry reinitialized credentials", e)
	}
	secret.SetUID(types.UID(uuid.NewString()))
	if _, e = client.Resource(secretResource).Namespace(v.Spec.Namespace).Update(ctx, secret, metav1.UpdateOptions{}); e != nil {
		t.Fatal(e)
	}
	if _, e = n.EnsureInitialization(ctx, v); e != biz.ErrOwnership {
		t.Fatalf("adopted replacement secret %v", e)
	}
	if _, e = n.DeleteInitialization(ctx, v); e != biz.ErrOwnership {
		t.Fatalf("deleted replacement secret %v", e)
	}
}
