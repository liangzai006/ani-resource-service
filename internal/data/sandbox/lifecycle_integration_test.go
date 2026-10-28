//go:build sandboxintegration

package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

var lifecyclePVC = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
var lifecyclePod = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
var lifecyclePV = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}

type lifecycleFixture struct {
	db     *sandboxFixture
	ctx    context.Context
	create biz.Create
	worker *biz.Worker
	client *fake.FakeDynamicClient
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	f := &lifecycleFixture{db: sandboxDatabase(t)}
	f.ctx, f.create = fixtureCreate()
	c := f.create
	sc := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "storage.k8s.io/v1", "kind": "StorageClass", "metadata": map[string]any{"name": c.Spec.Workspace.StorageClass}, "reclaimPolicy": "Delete", "volumeBindingMode": "WaitForFirstConsumer", "provisioner": "test.csi"}}
	f.client = fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{lifecyclePV: "PersistentVolumeList"}, sc)
	sequence := 0
	f.client.PrependReactor("create", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetResource() != lifecyclePVC && a.GetResource() != lifecyclePod {
			return false, nil, nil
		}
		s := f.get(t)
		want := biz.MutationCreateWorkspace
		if a.GetResource() == lifecyclePod {
			want = biz.MutationCreatePod
		}
		if s.Pending == nil || s.Pending.Kind != want {
			t.Fatal("provider write before durable journal")
		}
		object := a.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		sequence++
		object.SetUID(types.UID(fmt.Sprintf("lifecycle-uid-%d", sequence)))
		object.SetResourceVersion(fmt.Sprint(sequence))
		return false, nil, nil
	})
	f.client.PrependReactor("delete", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetResource() != lifecyclePVC && a.GetResource() != lifecyclePod {
			return false, nil, nil
		}
		s := f.get(t)
		options := a.(ktesting.DeleteAction).GetDeleteOptions()
		want := biz.MutationDeleteWorkspace
		if a.GetResource() == lifecyclePod {
			want = biz.MutationDeletePod
		}
		if s.Pending == nil || s.Pending.Kind != want || options.Preconditions == nil || options.Preconditions.UID == nil || string(*options.Preconditions.UID) != s.Pending.TargetUID {
			t.Fatal("unfenced provider delete")
		}
		return false, nil, nil
	})
	provider, err := data.NewKubernetesProvider(f.client, c.Spec.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	f.worker, err = biz.NewWorker(f.db.repo, provider, "lifecycle-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.repo.AcceptCreate(f.ctx, c); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *lifecycleFixture) get(t *testing.T) biz.Sandbox {
	t.Helper()
	s, err := f.db.repo.Get(f.ctx, f.create.TenantID, f.create.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func (f *lifecycleFixture) step(t *testing.T) {
	t.Helper()
	forceSandboxWorkDue(t, f.db, f.ctx, f.create, false)
	worked, err := f.worker.Step(f.ctx)
	if err != nil || !worked {
		t.Fatal("worker", worked, err)
	}
}
func (f *lifecycleFixture) action(t *testing.T, action string) string {
	t.Helper()
	s := f.get(t)
	operation := uuid.NewString()
	if _, err := f.db.repo.Change(f.ctx, biz.Command{TenantID: s.TenantID, ID: s.ID, InstanceID: s.InstanceID, OperationID: operation, Action: action, IdempotencyKey: uuid.NewString(), RequestDigest: strings.Repeat("e", 64), ExpectedVersion: s.Version}); err != nil {
		t.Fatal(err)
	}
	return operation
}
func (f *lifecycleFixture) operation(t *testing.T, id string) biz.Operation {
	t.Helper()
	op, err := f.db.repo.GetOperation(f.ctx, f.create.TenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}
func (f *lifecycleFixture) claimPhase(t *testing.T, phase string) {
	t.Helper()
	pvc, err := f.client.Resource(lifecyclePVC).Namespace(f.create.Spec.Namespace).Get(f.ctx, "workspace-"+f.create.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = unstructured.SetNestedField(pvc.Object, phase, "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.client.Resource(lifecyclePVC).Namespace(f.create.Spec.Namespace).Update(f.ctx, pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}
func (f *lifecycleFixture) boundAndReady(t *testing.T) {
	t.Helper()
	s := f.get(t)
	pvc, err := f.client.Resource(lifecyclePVC).Namespace(s.Snapshot.Namespace).Get(f.ctx, "workspace-"+s.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pvName := "original-pv"
	if _, err = f.client.Resource(lifecyclePV).Get(f.ctx, pvName, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		pv := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "PersistentVolume", "metadata": map[string]any{"name": pvName, "uid": "original-pv-uid"}, "spec": map[string]any{"persistentVolumeReclaimPolicy": "Delete", "claimRef": map[string]any{"namespace": s.Snapshot.Namespace, "name": pvc.GetName(), "uid": string(pvc.GetUID())}}}}
		if _, err = f.client.Resource(lifecyclePV).Create(f.ctx, pv, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(pvc.Object, pvName, "spec", "volumeName")
	_ = unstructured.SetNestedField(pvc.Object, "Bound", "status", "phase")
	if _, err = f.client.Resource(lifecyclePVC).Namespace(s.Snapshot.Namespace).Update(f.ctx, pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod, err := f.client.Resource(lifecyclePod).Namespace(s.Snapshot.Namespace).Get(f.ctx, "sandbox-"+s.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Object["status"] = map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "containerStatuses": []any{map[string]any{"name": "sandbox", "ready": true}}}
	if _, err = f.client.Resource(lifecyclePod).Namespace(s.Snapshot.Namespace).Update(f.ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// The repository, worker, intent changes and journal are real. Only Kubernetes
// and its scheduler/CSI controller responses are controlled protocol boundaries.
func TestSandboxWorkerRepositoryPendingPauseLostRecoveryAndReclaim(t *testing.T) {
	f := newLifecycleFixture(t)
	f.step(t) // write-ahead journal -> original Pending PVC
	originalPVC := f.get(t).ProviderRefs.PVCUID
	f.claimPhase(t, "Pending")
	pause := f.action(t, "pause")
	f.step(t)
	if s := f.get(t); s.Observed != biz.ObservedPaused || s.ProviderRefs.PVCUID != originalPVC || f.operation(t, pause).State != "succeeded" {
		t.Fatalf("WFFC pause did not complete: %+v", s)
	}
	resume := f.action(t, "resume")
	f.step(t) // consumer permits WFFC binding
	f.boundAndReady(t)
	f.step(t)
	running := f.get(t)
	if running.Observed != biz.ObservedRunning || running.ProviderRefs.PVCUID != originalPVC || f.operation(t, resume).State != "succeeded" {
		t.Fatal("resume lost original workspace")
	}
	f.claimPhase(t, "Lost")
	pause = f.action(t, "pause")
	f.step(t)
	f.step(t)
	if s := f.get(t); s.Observed != biz.ObservedPaused || s.ProviderRefs.PodUID != "" || s.ProviderRefs.PVCUID != originalPVC || s.Reason != "WORKSPACE_LOST" || f.operation(t, pause).State != "succeeded" {
		t.Fatalf("Lost workspace prevented stop: %+v", s)
	}
	resume = f.action(t, "resume")
	f.step(t)
	if s := f.get(t); s.Observed != biz.ObservedUnknown || s.ProviderRefs.PVCUID != originalPVC || f.operation(t, resume).State == "succeeded" {
		t.Fatal("unhealthy workspace resumed or was replaced")
	}
	f.claimPhase(t, "Bound")
	f.step(t)
	f.boundAndReady(t)
	f.step(t)
	if s := f.get(t); s.Observed != biz.ObservedRunning || s.ProviderRefs.PVCUID != originalPVC || s.ProviderRefs.PodUID == running.ProviderRefs.PodUID || f.operation(t, resume).State != "succeeded" {
		t.Fatal("recovery did not remount original workspace")
	}
	deletion := f.action(t, "delete")
	f.step(t)
	f.step(t)
	f.step(t)
	if s := f.get(t); s.Observed == biz.ObservedDeleted || s.ProviderRefs.CleanupReceipt != "" || f.operation(t, deletion).State == "succeeded" {
		t.Fatal("PV absence was not required")
	}
	if _, err := f.client.Resource(lifecyclePV).Get(f.ctx, "original-pv", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	// Only the external CSI boundary completes PV reclaim.
	if err := f.client.Resource(lifecyclePV).Delete(f.ctx, "original-pv", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.step(t)
	s := f.get(t)
	var terminal bool
	if err := f.db.runtime.QueryRow(f.ctx, "SELECT due_at='infinity'::timestamptz FROM sandbox.work WHERE tenant_id=$1 AND sandbox_id=$2", s.TenantID, s.ID).Scan(&terminal); err != nil {
		t.Fatal(err)
	}
	if s.Observed != biz.ObservedDeleted || s.ProviderRefs.CleanupReceipt == "" || !terminal || f.operation(t, deletion).State != "succeeded" {
		t.Fatalf("reclaim did not conclude: %+v", s)
	}
	claims := 0
	for _, a := range f.client.Actions() {
		if a.GetVerb() == "create" && a.GetResource() == lifecyclePVC {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("workspace replaced %d times", claims)
	}
}

func TestSandboxWorkerRepositoryOldReceiptCannotCompleteResume(t *testing.T) {
	f := newLifecycleFixture(t)
	f.step(t)
	f.step(t)
	f.boundAndReady(t)
	f.step(t)
	f.action(t, "pause")
	old := claimWork(t, f.db, f.ctx, "stalled-worker")
	mutation := biz.Mutation{ID: uuid.NewString(), Kind: biz.MutationDeletePod, OperationID: old.Sandbox.LastOperationID, Version: old.Sandbox.Version, TargetUID: old.Sandbox.ProviderRefs.PodUID}
	if err := f.db.repo.PrepareMutation(f.ctx, *old, mutation); err != nil {
		t.Fatal(err)
	}
	forceSandboxWorkDue(t, f.db, f.ctx, f.create, true)
	delayed := true
	f.client.PrependReactor("delete", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		if delayed {
			return true, nil, apierrors.NewTimeoutError("request may still be executing", 0)
		}
		return false, nil, nil
	})
	resume := f.action(t, "resume")
	f.step(t) // real worker increments/persists retry before timeout response
	current := f.get(t)
	if current.Pending == nil || current.Pending.Attempt != 1 || !current.Pending.PreviousUncertain {
		t.Fatal("retry has no distinct durable attempt")
	}
	if err := f.db.repo.Finish(f.ctx, *old, biz.Progress{Sandbox: old.Sandbox, Receipt: &biz.MutationResult{Mutation: mutation, Outcome: biz.MutationNotSent}}); !errors.Is(err, biz.ErrConflict) {
		t.Fatalf("old receipt accepted: %v", err)
	}
	f.step(t)
	if current = f.get(t); current.Pending == nil || current.Pending.Attempt != 2 || f.operation(t, resume).State == "succeeded" {
		t.Fatal("old receipt allowed premature Resume completion")
	}
	delayed = false
	f.step(t) // conclusive UID-fenced deletion resolves pending
	if f.operation(t, resume).State == "succeeded" {
		t.Fatal("Resume completed from the pre-delete Ready observation before replacement Pod existed")
	}
	f.step(t)
	if f.operation(t, resume).State == "succeeded" {
		t.Fatal("Pod create acknowledgement completed Resume before Ready")
	}
	f.boundAndReady(t)
	f.step(t)
	if current = f.get(t); current.Pending != nil || current.Observed != biz.ObservedRunning || current.ProviderRefs.PodUID == mutation.TargetUID || f.operation(t, resume).State != "succeeded" {
		t.Fatalf("resume failed ordered recovery: %+v", current)
	}
}
