//go:build instanceintegration

package instance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	networkbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	networkdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

var verticalNotebookGVR = schema.GroupVersionResource{Group: "kubeflow.org", Version: "v1", Resource: "notebooks"}
var verticalSTSGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
var verticalPodGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
var verticalPVCGVR = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
var verticalPVGVR = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}

// This fixture replaces only Kubernetes, CSI, Notebook Controller and CNI.
// Instance/Network use cases, both repositories, leases, snapshots and release
// decisions are production implementations with separate real PostgreSQL data.
type notebookNetworkBoundary struct {
	client                *fake.FakeDynamicClient
	api                   *testenv.KC
	notebook              *unstructured.Unstructured
	generation            int
	statefulSetGeneration int64
	statefulSetSpec       map[string]any
}

func newNotebookNetworkBoundary(t *testing.T, api *testenv.KC) *notebookNetworkBoundary {
	t.Helper()
	b := &notebookNetworkBoundary{api: api}
	b.client = fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{verticalPodGVR: "PodList", verticalPVGVR: "PersistentVolumeList"})
	b.client.PrependReactor("create", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		claim := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		claim.SetUID("workspace-uid")
		claim.SetResourceVersion("1")
		_ = unstructured.SetNestedField(claim.Object, "workspace-volume", "spec", "volumeName")
		pv := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": "workspace-volume", "uid": "workspace-volume-uid"}, "spec": map[string]any{"persistentVolumeReclaimPolicy": "Delete", "claimRef": map[string]any{"name": claim.GetName(), "namespace": claim.GetNamespace(), "uid": "workspace-uid"}}}}
		if err := b.client.Tracker().Create(verticalPVCGVR, claim, claim.GetNamespace()); err != nil {
			return true, nil, err
		}
		if err := b.client.Tracker().Create(verticalPVGVR, pv, ""); err != nil {
			return true, nil, err
		}
		return true, claim, nil
	})
	b.client.PrependReactor("create", "notebooks", func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		obj.SetUID("notebook-uid")
		obj.SetResourceVersion("1")
		obj.SetGeneration(1)
		b.notebook = obj
		if err := b.client.Tracker().Create(verticalNotebookGVR, obj, obj.GetNamespace()); err != nil {
			return true, nil, err
		}
		if err := b.project(); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	b.client.PrependReactor("update", "notebooks", func(action ktesting.Action) (bool, runtime.Object, error) {
		obj := action.(ktesting.UpdateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		obj.SetResourceVersion(uuid.NewString())
		b.notebook = obj
		if err := b.client.Tracker().Update(verticalNotebookGVR, obj, obj.GetNamespace()); err != nil {
			return true, nil, err
		}
		if err := b.project(); err != nil {
			return true, nil, err
		}
		return true, obj, nil
	})
	b.client.PrependReactor("delete", "notebooks", func(action ktesting.Action) (bool, runtime.Object, error) {
		b.api.Mu.Lock()
		defer b.api.Mu.Unlock()
		ns, name := b.notebook.GetNamespace(), b.notebook.GetName()
		for _, item := range []struct {
			gvr            schema.GroupVersionResource
			resource, name string
		}{{verticalNotebookGVR, "notebooks", name}, {verticalSTSGVR, "statefulsets", name}, {verticalPodGVR, "pods", name + "-0"}} {
			_ = b.client.Tracker().Delete(item.gvr, ns, item.name)
			delete(b.api.Objects, item.resource+"/"+ns+"/"+item.name)
		}
		delete(b.api.Objects, "vnics/"+ns+"/nic")
		delete(b.api.Objects, "vnicips/"+ns+"/ip")
		return true, nil, nil
	})
	b.client.PrependReactor("delete", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		name := action.(ktesting.DeleteAction).GetName()
		_ = b.client.Tracker().Delete(verticalPVGVR, "", "workspace-volume")
		return true, nil, b.client.Tracker().Delete(verticalPVCGVR, action.GetNamespace(), name)
	})
	return b
}
func (b *notebookNetworkBoundary) project() error {
	b.api.Mu.Lock()
	defer b.api.Mu.Unlock()
	obj := b.notebook
	ns, name := obj.GetNamespace(), obj.GetName()
	controller := true
	template, _, _ := unstructured.NestedMap(obj.Object, "spec", "template")
	replicas := int64(1)
	if _, stopped := obj.GetAnnotations()["kubeflow-resource-stopped"]; stopped {
		replicas = 0
	}
	// The controller boundary must project the actual template/replica intent
	// and an observed generation; absence of Pods alone is not a stop receipt.
	setSpec := map[string]any{"replicas": replicas, "template": runtime.DeepCopyJSONValue(template)}
	if !reflect.DeepEqual(b.statefulSetSpec, setSpec) {
		b.statefulSetGeneration++
		b.statefulSetSpec = runtime.DeepCopyJSONValue(setSpec).(map[string]any)
	}
	set := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"name": name, "namespace": ns, "uid": "set-uid", "resourceVersion": "1"}}}
	set.SetGeneration(b.statefulSetGeneration)
	set.SetResourceVersion(fmt.Sprint(b.statefulSetGeneration))
	set.Object["spec"] = setSpec
	set.Object["status"] = map[string]any{"observedGeneration": b.statefulSetGeneration}
	set.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubeflow.org/v1", Kind: "Notebook", Name: name, UID: obj.GetUID(), Controller: &controller}})
	_ = b.client.Tracker().Delete(verticalSTSGVR, ns, name)
	if err := b.client.Tracker().Create(verticalSTSGVR, set, ns); err != nil {
		return err
	}
	b.api.Objects["notebooks/"+ns+"/"+name] = obj.DeepCopy().Object
	b.api.Objects["statefulsets/"+ns+"/"+name] = set.Object
	_ = b.client.Tracker().Delete(verticalPodGVR, ns, name+"-0")
	delete(b.api.Objects, "pods/"+ns+"/"+name+"-0")
	delete(b.api.Objects, "vnics/"+ns+"/nic")
	delete(b.api.Objects, "vnicips/"+ns+"/ip")
	if _, stopped := obj.GetAnnotations()["kubeflow-resource-stopped"]; stopped {
		return nil
	}
	b.generation++
	uid := fmt.Sprintf("pod-%d", b.generation)
	nicUID := fmt.Sprintf("nic-%d", b.generation)
	address := fmt.Sprintf("10.42.1.%d", b.generation+10)
	pod := &unstructured.Unstructured{Object: template}
	pod.SetAPIVersion("v1")
	pod.SetKind("Pod")
	pod.SetNamespace(ns)
	pod.SetName(name + "-0")
	pod.SetUID(types.UID(uid))
	pod.SetResourceVersion("1")
	pod.SetLabels(obj.GetLabels())
	pod.SetAnnotations(obj.GetAnnotations())
	pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: name, UID: set.GetUID(), Controller: &controller}})
	pod.Object["status"] = map[string]any{"phase": "Running", "podIP": address, "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": biz.Container, "ready": true}}}
	if err := b.client.Tracker().Create(verticalPodGVR, pod, ns); err != nil {
		return err
	}
	b.api.Objects["pods/"+ns+"/"+pod.GetName()] = pod.DeepCopy().Object
	ref := obj.GetAnnotations()["networking.kubercloud.com/subnet"]
	b.api.Objects["vnics/"+ns+"/nic"] = map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "metadata": map[string]any{"namespace": ns, "name": "nic", "uid": nicUID, "resourceVersion": "1", "ownerReferences": []any{map[string]any{"apiVersion": "v1", "kind": "Pod", "name": pod.GetName(), "uid": uid}}}, "spec": map[string]any{"type": "VETH", "subnet": ref}}
	b.api.Objects["vnicips/"+ns+"/ip"] = map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"namespace": ns, "name": "ip", "uid": fmt.Sprintf("ip-%d", b.generation), "resourceVersion": "1", "ownerReferences": []any{map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "name": "nic", "uid": nicUID}}}, "spec": map[string]any{"subnet": ref, "vNic": "nic", "ipAddress": address}, "status": map[string]any{"vNic": "nic"}}
	return nil
}

type instanceNetworkPlatformFacts struct{}

func (instanceNetworkPlatformFacts) PlatformCIDRs(context.Context) ([]string, time.Time, error) {
	return []string{"10.16.0.0/16", "10.96.0.0/16", "172.16.101.0/24"}, time.Now(), nil
}

type lostPrepareReceipt struct {
	biz.NetworkPort
	lost                                        bool
	failConfirm, confirmConflicted, confirmLost bool
	confirmIdentities                           []string
	confirmConflictState, confirmLostState      string
}

func (n *lostPrepareReceipt) Prepare(ctx context.Context, i biz.Instance) (biz.NetworkBinding, error) {
	b, err := n.NetworkPort.Prepare(ctx, i)
	if err == nil && !n.lost {
		n.lost = true
		return biz.NetworkBinding{}, biz.ErrNetworkUnavailable
	}
	return b, err
}

func (n *lostPrepareReceipt) Confirm(ctx context.Context, i biz.Instance) error {
	if n.failConfirm {
		n.confirmIdentities = append(n.confirmIdentities, fmt.Sprintf("%s/%s/%d/%s", i.Network.SubmissionID, i.Network.AttachmentID, i.Network.SnapshotRevision, i.PodUID))
		if !n.confirmConflicted {
			n.confirmConflicted = true
			n.confirmConflictState = i.Network.NetworkState
			return biz.ErrNetworkConflict
		}
	}
	err := n.NetworkPort.Confirm(ctx, i)
	if err == nil && n.failConfirm && !n.confirmLost {
		n.confirmLost = true
		n.confirmLostState = i.Network.NetworkState
		return biz.ErrNetworkUnavailable
	}
	return err
}
func TestInstanceNetworkVerticalPGNotebookTwoRestartsAndRecreation(t *testing.T) {
	runInstanceNetworkVertical(t, false)
}
func TestInstanceNetworkVerticalConfirmRetryRequiresDurableAck(t *testing.T) {
	runInstanceNetworkVertical(t, true)
}
func runInstanceNetworkVertical(t *testing.T, confirmFaults bool) {
	t.Helper()
	f := database(t)
	ctx := context.Background()
	c := networkCreate()
	nf := testenv.NewDatabase(t)
	nrepo, err := networkdata.OpenPostgres(ctx, nf.RuntimeDSN, networkdata.Placement{ClusterID: c.Spec.ClusterID, NamespacePrefix: "tenant-"})
	if err != nil {
		t.Fatal(err)
	}
	defer nrepo.Close()
	if err = nrepo.ConfigureVPCCIDRPresets([]string{"10.42.0.0/16"}, instanceNetworkPlatformFacts{}); err != nil {
		t.Fatal(err)
	}
	api := &testenv.KC{}
	httpServer := httptest.NewServer(api)
	defer httpServer.Close()
	nprovider, err := networkdata.NewKCProvider(nrepo, &rest.Config{Host: httpServer.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	policy := networkbiz.DefaultWorkerPolicy()
	policy.ObserveEvery = time.Millisecond
	policy.RetryMin = time.Millisecond
	networkUC, err := networkbiz.NewNetwork(nrepo, make([]byte, 32), time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	resourceWorker, err := networkbiz.NewWorker(nrepo, nprovider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	vpc, err := networkUC.CreateVPC(ctx, networkbiz.CreateVPC{TenantID: c.TenantID, Name: "vertical", CIDR: "10.42.0.0/16", IdempotencyKey: "vertical"})
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 100; round++ {
		if _, err = resourceWorker.Step(ctx); err != nil {
			t.Fatal(err)
		}
		vpc, err = networkUC.GetVPC(ctx, c.TenantID, vpc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if vpc.State == networkbiz.Available {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if vpc.State != networkbiz.Available {
		t.Fatal("real VPC worker did not converge", vpc)
	}
	subnet, err := networkUC.CreateSubnet(ctx, networkbiz.CreateSubnet{TenantID: c.TenantID, VPCID: vpc.ID, Name: "vertical", CIDR: "10.42.1.0/24", IdempotencyKey: "vertical"})
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 100; round++ {
		if _, err = resourceWorker.Step(ctx); err != nil {
			t.Fatal(err)
		}
		subnet, err = networkUC.GetSubnet(ctx, c.TenantID, subnet.ID)
		if err != nil {
			t.Fatal(err)
		}
		if subnet.State == networkbiz.Available {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if subnet.State != networkbiz.Available {
		t.Fatal("real Subnet worker did not converge", subnet)
	}
	c.Spec.Network = &biz.NetworkSelection{VPCID: vpc.ID, SubnetID: subnet.ID}
	if _, err = f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	attachments := networkbiz.NewAttachments(nrepo, time.Minute)
	adapter, err := data.NewInstanceNetwork(networkUC, attachments, c.Spec.ClusterID, "tenant-", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	boundary := newNotebookNetworkBoundary(t, api)
	notebook, err := data.NewNotebook(boundary.client)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := biz.NewWorker(f.repo, notebook, "vertical-instance")
	if err != nil {
		t.Fatal(err)
	}
	loss := &lostPrepareReceipt{NetworkPort: adapter, failConfirm: confirmFaults}
	if err = worker.UseNetwork(loss); err != nil {
		t.Fatal(err)
	}
	consumer, err := data.NewInstanceNetworkConsumer(f.repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	nworker, err := networkbiz.NewAttachmentWorker(nrepo, nprovider, consumer, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	drive := func(want string) biz.Instance {
		t.Helper()
		var current biz.Instance
		for round := 0; round < 100; round++ {
			current = step(t, f, worker, c)
			if current.Observed == biz.Running && current.Network.ConfirmedSnapshotRevision != current.Network.SnapshotRevision {
				t.Fatal("Instance ran before its matching Confirm acknowledgement was durable", current.Network)
			}
			if _, err := nf.Owner.Exec(ctx, `UPDATE network_attachments SET next_check_at=clock_timestamp()`); err != nil {
				t.Fatal(err)
			}
			if _, err := nworker.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if current.Observed == want && (want != biz.Deleted || current.Network.Cleaned) {
				return current
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatalf("vertical flow did not reach %s: state=%s reason=%s network=%+v", want, current.Observed, current.Reason, current.Network)
		return current
	}
	i := drive(biz.Running)
	binding, submission, workspace := i.Network.AttachmentID, i.Network.SubmissionID, i.PVCUID
	frozen, _ := json.Marshal(i.Spec)
	if !loss.lost || i.Network.NetworkState != "attached" || i.Network.ConfirmedSnapshotRevision != i.Network.SnapshotRevision {
		t.Fatal("lost Prepare receipt did not recover to current attachment", i.Network)
	}
	if confirmFaults {
		if !loss.confirmConflicted || !loss.confirmLost || len(loss.confirmIdentities) < 3 || loss.confirmConflictState != "attached" || loss.confirmLostState != "attached" {
			t.Fatal("Confirm conflict and lost committed acknowledgement were not retried", loss.confirmIdentities)
		}
		if loss.confirmIdentities[0] != loss.confirmIdentities[1] || loss.confirmIdentities[1] != loss.confirmIdentities[2] {
			t.Fatal("Confirm retry changed the persisted submission, revision or Pod identity", loss.confirmIdentities[:3])
		}
	}
	for round := 0; round < 2; round++ {
		old := i.PodUID
		command(t, f, c, i, "stop")
		stopping, err := f.repo.Get(ctx, c.TenantID, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stopping.Observed == biz.Running {
			t.Fatal("new Stop snapshot retained the prior Running qualification", stopping.Network)
		}
		i = drive(biz.Stopped)
		claim, err := boundary.client.Resource(verticalPVCGVR).Namespace(c.Spec.Namespace).Get(ctx, c.Spec.PVCName, metav1.GetOptions{})
		if err != nil || string(claim.GetUID()) != workspace {
			t.Fatal("Stop lost the real workspace PVC identity", claim, err)
		}
		if i.Network.State != "open" || i.Network.Cleaned {
			t.Fatal("Stop finalized attachment")
		}
		command(t, f, c, i, "start")
		i = drive(biz.Running)
		if i.PodUID == old || i.Network.AttachmentID != binding || i.Network.SubmissionID != submission || i.PVCUID != workspace {
			t.Fatal("Start changed immutable binding or workspace")
		}
	}
	old := i.PodUID
	if err = boundary.project(); err != nil {
		t.Fatal(err)
	}
	i = drive(biz.Running)
	if i.PodUID == old {
		t.Fatal("automatic Pod recreation was not observed")
	}
	// Fresh workers consume the saved submission instead of rebuilding identity.
	worker, err = biz.NewWorker(f.repo, notebook, "vertical-after-restart")
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.UseNetwork(adapter); err != nil {
		t.Fatal(err)
	}
	nworker, err = networkbiz.NewAttachmentWorker(nrepo, nprovider, consumer, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	i = drive(biz.Running)
	current, _ := json.Marshal(i.Spec)
	if string(frozen) != string(current) {
		t.Fatal("dynamic lifecycle altered CREATE digest input")
	}
	command(t, f, c, i, "delete")
	i = drive(biz.Deleted)
	if !i.ComputeCleaned || !i.WorkspaceCleaned || !i.QuotaReturned || !i.Network.Cleaned || !i.Network.ReleaseAcknowledged || i.Network.State != "closed" {
		t.Fatal("final release omitted real cleanup or called GPU quota", i.Network)
	}
	if _, err := boundary.client.Resource(verticalPVCGVR).Namespace(c.Spec.Namespace).Get(ctx, c.Spec.PVCName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("final DELETE claimed workspace cleanup without removing the PVC", err)
	}
	if _, err := boundary.client.Resource(verticalPVGVR).Get(ctx, "workspace-volume", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("final DELETE claimed workspace cleanup before CSI reclaimed the PV", err)
	}
	a, err := attachments.Get(ctx, c.TenantID, binding)
	if err != nil || a.State != networkbiz.Released || a.FinalizationID != i.Network.FinalizationID {
		t.Fatal("Network release was fabricated", a, err)
	}
	var count int
	if err = nf.Owner.QueryRow(ctx, `SELECT count(*) FROM network_attachments WHERE tenant_id=$1 AND submission_id=$2`, c.TenantID, submission).Scan(&count); err != nil || count != 1 {
		t.Fatal("lost response created duplicate attachment", count, err)
	}
}
