package vm

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"strconv"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var vmResource = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
var vmiResource = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
var dvResource = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes"}
var pvcResource = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
var pvResource = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
var podResource = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
var nsResource = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

type Native struct {
	client            dynamic.Interface
	control           rest.Interface
	guestPasswordHash func(context.Context, biz.VM) (string, error)
	logSource         podLogSource
	logLimits         biz.LogLimits
}

func NewNative(c dynamic.Interface, config *rest.Config) (*Native, error) {
	if c == nil || config == nil {
		return nil, biz.ErrInvalid
	}
	cfg := dynamic.ConfigFor(config)
	// Power requests carry JSON, but KubeVirt's power routes declare no
	// response media type. Accept their empty success response as well.
	cfg.ContentType = "application/json"
	cfg.AcceptContentTypes = "*/*"
	gv := schema.GroupVersion{Group: "subresources.kubevirt.io", Version: "v1"}
	cfg.GroupVersion = &gv
	cfg.APIPath = "/apis"
	// The subresource client sends raw JSON only, with the original TLS config.
	client, e := rest.RESTClientFor(cfg)
	if e != nil {
		return nil, e
	}
	return &Native{client: c, control: client}, nil
}
func NewNativeWithClients(c dynamic.Interface, control rest.Interface) *Native {
	return &Native{client: c, control: control}
}
func objectRef(o *unstructured.Unstructured) biz.ObjectRef {
	return biz.ObjectRef{Kind: o.GetKind(), Namespace: o.GetNamespace(), Name: o.GetName(), UID: string(o.GetUID())}
}
func labelVM(v biz.VM) map[string]string {
	// Kubernetes labels allow at most 63 characters. Preserve a full 256-bit
	// fingerprint rather than truncating the canonical hexadecimal digest.
	digest := sha256.Sum256([]byte(v.Spec.Digest))
	labelDigest := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:])
	return map[string]string{"ani.io/tenant-id": v.TenantID, "vm.ani.io/vm-id": v.ID, "vm.ani.io/create-digest": labelDigest}
}
func matches(o *unstructured.Unstructured, v biz.VM) bool {
	for k, x := range labelVM(v) {
		if o.GetLabels()[k] != x {
			return false
		}
	}
	return o.GetNamespace() == v.Spec.Namespace
}
func owned(o *unstructured.Unstructured, kind, uid string) bool {
	api := map[string]string{"VirtualMachine": "kubevirt.io/v1", "VirtualMachineInstance": "kubevirt.io/v1", "DataVolume": "cdi.kubevirt.io/v1beta1"}[kind]
	for _, r := range o.GetOwnerReferences() {
		if uid != "" && r.APIVersion == api && r.Kind == kind && string(r.UID) == uid && r.Controller != nil && *r.Controller {
			return true
		}
	}
	return false
}
func ownReference(v biz.VM) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachine", Name: v.Native.Name, UID: types.UID(v.Native.UID), Controller: &yes, BlockOwnerDeletion: &yes}}
}
func (n *Native) namespace(ctx context.Context, v biz.VM) error {
	o, e := n.client.Resource(nsResource).Get(ctx, v.Spec.Namespace, metav1.GetOptions{})
	if e != nil {
		return e
	}
	if string(o.GetUID()) != v.Spec.NamespaceUID || o.GetLabels()["network.ani.io/tenant-id"] != v.TenantID || o.GetLabels()["network.ani.io/managed-by"] != "ani-network-service" {
		return biz.ErrOwnership
	}
	return nil
}
func (n *Native) ValidateSource(ctx context.Context, s biz.Source) error {
	o, e := n.client.Resource(pvcResource).Namespace(s.Namespace).Get(ctx, s.PVCName, metav1.GetOptions{})
	if e != nil {
		return e
	}
	if string(o.GetUID()) != s.PVCUID {
		return biz.ErrChanged
	}
	mode, _, _ := unstructured.NestedString(o.Object, "spec", "volumeMode")
	if mode == "" {
		mode = "Filesystem"
	}
	if mode != s.VolumeMode {
		return biz.ErrChanged
	}
	if o.GetAnnotations()["vm.ani.io/source-sha256"] != s.Checksum {
		return biz.ErrChanged
	}
	return nil
}
func (n *Native) EnsureVM(ctx context.Context, v biz.VM) (biz.ObjectRef, error) {
	if e := n.namespace(ctx, v); e != nil {
		return biz.ObjectRef{}, e
	}
	old, e := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if e == nil {
		if !matches(old, v) || (v.Native.UID != "" && string(old.GetUID()) != v.Native.UID) {
			return biz.ObjectRef{}, biz.ErrOwnership
		}
		return objectRef(old), nil
	}
	if !apierrors.IsNotFound(e) {
		return biz.ObjectRef{}, e
	}
	if v.Native.UID != "" {
		return biz.ObjectRef{}, biz.ErrOwnership
	}
	if v.Spec.Media.Kind != "DISK_IMAGE" && v.Spec.Media.Kind != "INSTALL_ISO" {
		return biz.ObjectRef{}, biz.ErrDependency
	}
	volumes := []any{}
	disks := []any{}
	for _, d := range v.Disks {
		if !d.Attached || d.Tombstone {
			continue
		}
		source := map[string]any{"name": d.DV.Name}
		if d.Role == "data" {
			source["hotpluggable"] = true
		}
		volumes = append(volumes, map[string]any{"name": d.VolumeName, "dataVolume": source})
		bus := v.Spec.Media.RootBus
		if d.Role == "data" {
			bus = "scsi"
		}
		device := map[string]any{"name": d.VolumeName, "disk": map[string]any{"bus": bus}, "serial": d.Serial}
		if d.Role == "system" {
			device["bootOrder"] = int64(1)
		}
		disks = append(disks, device)
	}
	labels := labelVM(v)
	for k, x := range v.Network.Labels {
		labels[k] = x
	}
	domain := map[string]any{"cpu": map[string]any{"sockets": int64(1), "cores": int64(v.Spec.Cores), "threads": int64(1)}, "memory": map[string]any{"guest": fmt.Sprintf("%dMi", v.Spec.MemoryMiB)}, "resources": map[string]any{"requests": map[string]any{"cpu": fmt.Sprintf("%dm", v.Spec.CPUMillis), "memory": fmt.Sprintf("%dMi", v.Spec.MemoryMiB)}, "limits": map[string]any{"cpu": fmt.Sprintf("%dm", v.Spec.CPUMillis)}}, "devices": map[string]any{"disks": disks, "interfaces": []any{map[string]any{"name": "default", "bridge": map[string]any{}, "model": v.Spec.Media.NICModel}}}}
	domain["devices"].(map[string]any)["inputs"] = []any{consoleTablet()}
	if e = configureWindowsDomain(v, domain); e != nil {
		return biz.ObjectRef{}, e
	}
	if v.Spec.Media.Firmware == "uefi" {
		domain["firmware"] = map[string]any{"bootloader": map[string]any{"efi": map[string]any{"secureBoot": false}}}
	}
	if e = n.ValidateBackendConfiguration(ctx, v.Spec); e != nil {
		return biz.ObjectRef{}, e
	}
	if e = n.ConfigureBackendDomain(v, domain); e != nil {
		return biz.ObjectRef{}, e
	}
	// Native NoCloud uses firmware UUID as instance-id. Keep the logical VM
	// identity across VMI recreation so Stop/Start does not initialize again.
	if e = unstructured.SetNestedField(domain, v.ID, "firmware", "uuid"); e != nil {
		return biz.ObjectRef{}, e
	}
	if v.Spec.GuestInitialization != nil {
		volumes = append(volumes, map[string]any{"name": "guest-initialization", "cloudInitNoCloud": map[string]any{"secretRef": map[string]any{"name": initializationSecretName(v)}, "networkDataSecretRef": map[string]any{"name": initializationSecretName(v)}}})
		disks = append(disks, map[string]any{"name": "guest-initialization", "disk": map[string]any{"bus": "virtio"}})
		if e = unstructured.SetNestedSlice(domain, disks, "devices", "disks"); e != nil {
			return biz.ObjectRef{}, e
		}
	}
	volumes, disks, e = n.MediaTemplate(v, domain, volumes, disks)
	if e != nil {
		return biz.ObjectRef{}, e
	}
	if e = unstructured.SetNestedSlice(domain, disks, "devices", "disks"); e != nil {
		return biz.ObjectRef{}, e
	}
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine", "metadata": map[string]any{"name": v.Native.Name, "namespace": v.Spec.Namespace}, "spec": map[string]any{"runStrategy": "Manual", "template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{"networking.kubercloud.com/subnet": v.Network.KCNSubnetRef}}, "spec": map[string]any{"serviceAccountName": v.Spec.ServiceAccount, "domain": domain, "volumes": volumes, "networks": []any{map[string]any{"name": "default", "multus": map[string]any{"default": true, "networkName": v.Spec.Namespace + "/" + v.Network.NADName}}}}}}}}
	if e = configureDNS(v.Spec.DNS, o); e != nil {
		return biz.ObjectRef{}, e
	}
	o.SetLabels(labels)
	rawLabels := map[string]any{}
	for k, x := range labels {
		rawLabels[k] = x
	}
	if e = unstructured.SetNestedMap(o.Object, rawLabels, "spec", "template", "metadata", "labels"); e != nil {
		return biz.ObjectRef{}, e
	}
	created, e := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Create(ctx, o, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(e) {
		return n.EnsureVM(ctx, v)
	}
	if e != nil {
		return biz.ObjectRef{}, e
	}
	return objectRef(created), nil
}
func (n *Native) EnsureVolume(ctx context.Context, v biz.VM, d biz.Disk) (biz.Disk, error) {
	if v.Native.UID == "" {
		return d, biz.ErrOwnership
	}
	old, e := n.client.Resource(dvResource).Namespace(v.Spec.Namespace).Get(ctx, d.DV.Name, metav1.GetOptions{})
	if e == nil {
		if !matches(old, v) || !owned(old, "VirtualMachine", v.Native.UID) || (d.DV.UID != "" && d.DV.UID != string(old.GetUID())) {
			return d, biz.ErrOwnership
		}
		d.DV = objectRef(old)
		return n.observeDisk(ctx, v, d)
	}
	if !apierrors.IsNotFound(e) {
		return d, e
	}
	if d.DV.UID != "" {
		return d, biz.ErrOwnership
	}
	source := map[string]any{"blank": map[string]any{}}
	if d.Role == "system" && v.Spec.Media.Kind == "DISK_IMAGE" {
		if e = n.ValidateSource(ctx, v.Spec.Media); e != nil {
			return d, e
		}
		source = map[string]any{"pvc": map[string]any{"namespace": v.Spec.Media.Namespace, "name": v.Spec.Media.PVCName}}
	}
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataVolume", "metadata": map[string]any{"name": d.DV.Name, "namespace": v.Spec.Namespace}, "spec": map[string]any{"source": source, "contentType": "kubevirt", "storage": map[string]any{"storageClassName": v.Spec.StorageClass, "volumeMode": "Block", "accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": strconv.FormatInt(d.SizeGB, 10) + "G"}}}}}}
	o.SetLabels(labelVM(v))
	o.SetOwnerReferences(ownReference(v))
	created, e := n.client.Resource(dvResource).Namespace(v.Spec.Namespace).Create(ctx, o, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(e) {
		return n.EnsureVolume(ctx, v, d)
	}
	if e != nil {
		return d, e
	}
	d.DV = objectRef(created)
	return n.observeDisk(ctx, v, d)
}
func (n *Native) observeDisk(ctx context.Context, v biz.VM, d biz.Disk) (biz.Disk, error) {
	if d.DV.Name != "" {
		dv, e := n.client.Resource(dvResource).Namespace(v.Spec.Namespace).Get(ctx, d.DV.Name, metav1.GetOptions{})
		if e != nil && !apierrors.IsNotFound(e) {
			return d, e
		}
		if e == nil {
			if !matches(dv, v) || !owned(dv, "VirtualMachine", v.Native.UID) || (d.DV.UID != "" && d.DV.UID != string(dv.GetUID())) {
				return d, biz.ErrOwnership
			}
			d.DV = objectRef(dv)
		}
	}
	pvcName := d.DV.Name
	if d.PVC.Name != "" {
		pvcName = d.PVC.Name
	}
	if pvcName == "" {
		return d, biz.ErrOwnership
	}
	o, e := n.client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if apierrors.IsNotFound(e) {
		return d, nil
	}
	if e != nil {
		return d, e
	}
	if !owned(o, "DataVolume", d.DV.UID) && (v.Native.UID == "" || !owned(o, "VirtualMachine", v.Native.UID)) {
		return d, biz.ErrOwnership
	}
	if d.PVC.UID != "" && d.PVC.UID != string(o.GetUID()) {
		return d, biz.ErrOwnership
	}
	d.PVC = objectRef(o)
	mode, _, _ := unstructured.NestedString(o.Object, "spec", "volumeMode")
	if mode != "Block" {
		return d, biz.ErrOwnership
	}
	d.Preparation, _, _ = unstructured.NestedString(o.Object, "status", "phase")
	capacity, _, _ := unstructured.NestedString(o.Object, "status", "capacity", "storage")
	if capacity != "" {
		quantity, err := resource.ParseQuantity(capacity)
		if err != nil {
			return d, biz.ErrChanged
		}
		size, ok := quantity.AsInt64()
		if !ok || size < d.RequestedBytes {
			return d, biz.ErrChanged
		}
		d.ObservedCapacityBytes = size
	}
	pvName, _, _ := unstructured.NestedString(o.Object, "spec", "volumeName")
	if pvName != "" {
		pv, e := n.client.Resource(pvResource).Get(ctx, pvName, metav1.GetOptions{})
		if e != nil {
			return d, e
		}
		claim, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "uid")
		policy, _, _ := unstructured.NestedString(pv.Object, "spec", "persistentVolumeReclaimPolicy")
		if claim != d.PVC.UID || policy != "Delete" {
			return d, biz.ErrOwnership
		}
		d.Cleanup = []biz.ObjectRef{objectRef(pv)}
	}
	return d, nil
}
func (n *Native) ObserveConsole(ctx context.Context, v biz.VM) (biz.Observation, error) {
	return n.observe(ctx, v, false)
}
func (n *Native) Observe(ctx context.Context, v biz.VM) (biz.Observation, error) {
	return n.observe(ctx, v, true)
}
func (n *Native) observe(ctx context.Context, v biz.VM, includeStorage bool) (biz.Observation, error) {
	x := biz.Observation{State: "Unknown"}
	o, e := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if e != nil && !apierrors.IsNotFound(e) {
		return x, e
	}
	if e == nil {
		if !matches(o, v) || (v.Native.UID != "" && string(o.GetUID()) != v.Native.UID) {
			return x, biz.ErrOwnership
		}
		x.VM = objectRef(o)
		requests, _, _ := unstructured.NestedSlice(o.Object, "status", "stateChangeRequests")
		for _, r := range requests {
			b, _ := json.Marshal(r)
			x.StateChangeRequests = append(x.StateChangeRequests, string(b))
		}
	}
	vmi, z := n.client.Resource(vmiResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if z != nil && !apierrors.IsNotFound(z) {
		return x, z
	}
	if z == nil {
		if !owned(vmi, "VirtualMachine", x.VM.UID) {
			return x, biz.ErrOwnership
		}
		ref := objectRef(vmi)
		x.VMI = &ref
		phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
		x.State = phase
		x.Running = phase == "Running"
	}
	if x.VMI != nil {
		pods, z := n.client.Resource(podResource).Namespace(v.Spec.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "kubevirt.io/created-by=" + x.VMI.UID})
		if z != nil {
			return x, z
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
			if phase == "Succeeded" || phase == "Failed" {
				continue
			}
			if !owned(pod, "VirtualMachineInstance", x.VMI.UID) {
				return x, biz.ErrOwnership
			}
			if x.Launcher != nil {
				return x, biz.ErrDependency
			}
			ref := objectRef(pod)
			x.Launcher = &ref
		}
	}
	// A vanished VMI is insufficient while an old launcher still runs.
	if x.Launcher == nil {
		pods, pe := n.client.Resource(podResource).Namespace(v.Spec.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "vm.ani.io/vm-id=" + v.ID})
		if pe != nil {
			return x, pe
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
			if phase == "Succeeded" || phase == "Failed" {
				continue
			}
			if !matches(pod, v) {
				return x, biz.ErrOwnership
			}
			if x.Launcher != nil {
				return x, biz.ErrDependency
			}
			ref := objectRef(pod)
			x.Launcher = &ref
		}
	}
	x.Deleted = apierrors.IsNotFound(e) && apierrors.IsNotFound(z) && x.Launcher == nil
	x.Stopped = e == nil && apierrors.IsNotFound(z) && x.Launcher == nil && len(x.StateChangeRequests) == 0
	if x.Stopped {
		x.State = "Stopped"
	}
	if x.Deleted {
		x.State = "Deleted"
	}
	if !includeStorage {
		return x, nil
	}
	for _, d := range v.Disks {
		if d.Tombstone {
			continue
		}
		observed, z := n.observeDisk(ctx, v, d)
		if z != nil {
			return x, z
		}
		x.Volumes = append(x.Volumes, observed)
	}
	x.BackendStates, e = n.ObserveBackendState(ctx, v, v.BackendStates)
	if e != nil {
		return x, e
	}
	return x, nil
}
func (n *Native) controlVM(ctx context.Context, v biz.VM, action string, options map[string]any) error {
	x, e := n.Observe(ctx, v)
	if e != nil {
		return e
	}
	if x.VM.UID != v.Native.UID || v.Native.UID == "" {
		return biz.ErrOwnership
	}
	body, e := json.Marshal(options)
	if e != nil {
		return e
	}
	return n.control.Put().SetHeader("Accept", "*/*").SetHeader("Content-Type", "application/json").Namespace(v.Spec.Namespace).Resource("virtualmachines").Name(v.Native.Name).SubResource(action).Body(body).Do(ctx).Error()
}
func (n *Native) Start(ctx context.Context, v biz.VM) error {
	if e := n.ValidateBackendConfiguration(ctx, v.Spec); e != nil {
		return e
	}
	if e := n.ensureWindowsDomain(ctx, v); e != nil {
		return e
	}
	if e := n.ensureConsoleTablet(ctx, v); e != nil {
		return e
	}
	return n.controlVM(ctx, v, "start", map[string]any{})
}
func (n *Native) Stop(ctx context.Context, v biz.VM, force bool) error {
	options := map[string]any{}
	if force {
		options["gracePeriod"] = int64(0)
	}
	return n.controlVM(ctx, v, "stop", options)
}
func (n *Native) Restart(ctx context.Context, v biz.VM) error {
	if e := n.ValidateBackendConfiguration(ctx, v.Spec); e != nil {
		return e
	}
	return n.controlVM(ctx, v, "restart", map[string]any{})
}
func (n *Native) Delete(ctx context.Context, v biz.VM) error {
	if v.Native.UID == "" {
		return biz.ErrOwnership
	}
	uid := types.UID(v.Native.UID)
	policy := metav1.DeletePropagationForeground
	e := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Delete(ctx, v.Native.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy})
	if apierrors.IsNotFound(e) {
		return nil
	}
	return e
}
func (n *Native) DeleteVolume(ctx context.Context, v biz.VM, d biz.Disk) (bool, error) {
	for _, item := range []struct {
		r   schema.GroupVersionResource
		ref biz.ObjectRef
	}{{dvResource, d.DV}, {pvcResource, d.PVC}} {
		ref := item.ref
		if ref.Name == "" {
			continue
		}
		o, e := n.client.Resource(item.r).Namespace(v.Spec.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(e) {
			continue
		}
		if e != nil {
			return false, e
		}
		if ref.UID == "" || string(o.GetUID()) != ref.UID {
			return false, biz.ErrOwnership
		}
		uid := types.UID(ref.UID)
		policy := metav1.DeletePropagationForeground
		e = n.client.Resource(item.r).Namespace(v.Spec.Namespace).Delete(ctx, ref.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: &policy})
		if e != nil && !apierrors.IsNotFound(e) {
			return false, e
		}
		return false, nil
	}
	for _, ref := range d.Cleanup {
		if ref.Kind != "PersistentVolume" {
			return false, biz.ErrOwnership
		}
		o, e := n.client.Resource(pvResource).Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(e) {
			continue
		}
		if e != nil {
			return false, e
		}
		if string(o.GetUID()) != ref.UID {
			return false, biz.ErrOwnership
		}
		return false, nil
	}
	return true, nil
}
