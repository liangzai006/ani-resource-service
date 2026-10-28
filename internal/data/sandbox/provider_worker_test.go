package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

// These tests compose the real Worker and KubernetesProvider. This repository
// port double records the pre-write journal and applies its attributable receipt;
// PostgreSQL lease/transaction behavior has its own integration suite.
type providerWorkerRepository struct {
	state    biz.Sandbox
	prepared []biz.Mutation
}

func (r *providerWorkerRepository) Claim(context.Context, string, time.Duration) (*biz.Work, error) {
	return &biz.Work{Sandbox: r.state, LeaseOwner: "provider-worker-test", LeaseEpoch: 1}, nil
}
func (r *providerWorkerRepository) PrepareMutation(_ context.Context, work biz.Work, mutation biz.Mutation) error {
	if r.state.Pending != nil || work.Sandbox.Version != r.state.Version || mutation.OperationID != r.state.LastOperationID {
		return biz.ErrConflict
	}
	r.state.Pending = &mutation
	r.prepared = append(r.prepared, mutation)
	return nil
}
func (r *providerWorkerRepository) Finish(_ context.Context, _ biz.Work, progress biz.Progress) error {
	pending := r.state.Pending
	if progress.Receipt != nil && (pending == nil || *pending != progress.Receipt.Mutation) {
		return biz.ErrConflict
	}
	r.state.Observed, r.state.Reason, r.state.ObservedAt = progress.Sandbox.Observed, progress.Sandbox.Reason, progress.Sandbox.ObservedAt
	r.state.ProviderRefs = progress.Sandbox.ProviderRefs
	if progress.Receipt != nil {
		r.state.ProviderRefs = progress.Receipt.Refs
		if progress.Receipt.Outcome != biz.MutationUnknown {
			r.state.Pending = nil
		}
	}
	return nil
}

func newProviderWorker(t *testing.T, provider *KubernetesProvider, s biz.Sandbox) (*biz.Worker, *providerWorkerRepository) {
	t.Helper()
	repo := &providerWorkerRepository{state: s}
	worker, err := biz.NewWorker(repo, provider, "provider-worker-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return worker, repo
}
func providerWorkerStep(t *testing.T, worker *biz.Worker) {
	t.Helper()
	worked, err := worker.Step(context.Background())
	if err != nil || !worked {
		t.Fatalf("worker step=%v: %v", worked, err)
	}
}
func providerActionCount(client *fake.FakeDynamicClient, verb, resource string) int {
	count := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == verb && action.GetResource().Resource == resource {
			count++
		}
	}
	return count
}

func TestProviderWorkerPausePendingWFFCThenResumeOriginalWorkspace(t *testing.T) {
	client, provider, s := providerFixture(t)
	sc, err := client.Resource(storageClassGVR).Get(context.Background(), s.Snapshot.Workspace.StorageClass, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sc.Object["volumeBindingMode"] = "WaitForFirstConsumer"
	if _, err = client.Resource(storageClassGVR).Update(context.Background(), sc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	pvc, err := client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), s.ProviderRefs.PVCName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = unstructured.SetNestedField(pvc.Object, "Pending", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	originalPVC := s.ProviderRefs.PVCUID
	s.Desired, s.Version, s.LastOperationID = biz.DesiredPaused, s.Version+1, uuid.NewString()
	worker, repo := newProviderWorker(t, provider, s)
	providerWorkerStep(t, worker)
	if repo.state.Observed != biz.ObservedPaused || repo.state.Reason != "WORKSPACE_PENDING" || repo.state.ProviderRefs.PVCUID != originalPVC || len(repo.prepared) != 0 || providerActionCount(client, "create", "pods") != 0 {
		t.Fatalf("WFFC pause needed a consumer: %#v", repo.state)
	}
	// Resume must journal a consumer against the retained Pending claim. The
	// scheduler's binding is simulated only after the real adapter creates it.
	repo.state.Desired, repo.state.Version, repo.state.LastOperationID = biz.DesiredRunning, repo.state.Version+1, uuid.NewString()
	client.PrependReactor("create", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		if repo.state.Pending == nil || repo.state.Pending.Kind != biz.MutationCreatePod {
			t.Fatal("consumer create was sent before its mutation journal")
		}
		return false, nil, nil
	})
	providerWorkerStep(t, worker)
	if len(repo.prepared) != 1 || repo.prepared[0].Kind != biz.MutationCreatePod || repo.state.Pending != nil || repo.state.ProviderRefs.PVCUID != originalPVC {
		t.Fatalf("resume did not reuse original WFFC claim: %#v", repo.state)
	}
	providerBoundWorkspace(t, client, repo.state)
	providerPodStatus(t, client, repo.state, "Running", "True", true)
	providerWorkerStep(t, worker)
	if repo.state.Observed != biz.ObservedRunning || repo.state.ProviderRefs.PVCUID != originalPVC || providerActionCount(client, "create", "persistentvolumeclaims") != 1 || providerActionCount(client, "create", "pods") != 1 {
		t.Fatalf("resume replaced the workspace or failed readiness: %#v", repo.state)
	}
}

func TestProviderWorkerPauseStopsKnownPodWithWorkspaceFailure(t *testing.T) {
	for _, phase := range []string{"Lost", "Missing", "Deleting"} {
		t.Run(phase, func(t *testing.T) {
			client, provider, s := providerFixture(t)
			applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
			providerBoundWorkspace(t, client, s)
			applyProvider(t, provider, &s, biz.MutationCreatePod)
			providerPodStatus(t, client, s, "Running", "True", true)
			observeProvider(t, provider, &s)
			originalPVC, originalPV, originalPod := s.ProviderRefs.PVCUID, s.ProviderRefs.PVUID, s.ProviderRefs.PodUID
			pvc, err := client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), s.ProviderRefs.PVCName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if phase == "Missing" {
				err = client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Delete(context.Background(), pvc.GetName(), metav1.DeleteOptions{})
			} else {
				if phase == "Deleting" {
					at := metav1.Now()
					pvc.SetDeletionTimestamp(&at)
				} else {
					err = unstructured.SetNestedField(pvc.Object, phase, "status", "phase")
				}
				if err == nil {
					_, err = client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pvc, metav1.UpdateOptions{})
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			s.Desired, s.Version, s.LastOperationID = biz.DesiredPaused, s.Version+1, uuid.NewString()
			worker, repo := newProviderWorker(t, provider, s)
			deletesBefore := providerActionCount(client, "delete", "persistentvolumeclaims")
			client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
				options := action.(ktesting.DeleteAction).GetDeleteOptions()
				if repo.state.Pending == nil || repo.state.Pending.Kind != biz.MutationDeletePod || options.Preconditions == nil || options.Preconditions.UID == nil || string(*options.Preconditions.UID) != originalPod {
					t.Fatal("pause sent an unjournaled or unfenced Pod deletion")
				}
				return false, nil, nil
			})
			providerWorkerStep(t, worker)
			if _, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), s.ProviderRefs.PodName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatalf("original Pod kept running during pause: %v", err)
			}
			providerWorkerStep(t, worker)
			if repo.state.Observed != biz.ObservedPaused || repo.state.Reason == "" || repo.state.ProviderRefs.PVCUID != originalPVC || repo.state.ProviderRefs.PVUID != originalPV || repo.state.ProviderRefs.PodUID != "" || len(repo.prepared) != 1 || repo.state.Pending != nil || providerActionCount(client, "delete", "persistentvolumeclaims") != deletesBefore || providerActionCount(client, "create", "persistentvolumeclaims") != 1 {
				t.Fatalf("pause lost storage facts or changed the workspace: %#v", repo.state)
			}
			if phase != "Missing" {
				retained, err := client.Resource(pvcGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), s.ProviderRefs.PVCName, metav1.GetOptions{})
				if err != nil || string(retained.GetUID()) != originalPVC {
					t.Fatalf("pause replaced original workspace: %v", err)
				}
			}
		})
	}
}

func TestProviderWorkerPauseRejectsReplacedPodOwnership(t *testing.T) {
	client, provider, s := providerFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	providerBoundWorkspace(t, client, s)
	applyProvider(t, provider, &s, biz.MutationCreatePod)
	pod, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), s.ProviderRefs.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.SetUID("replacement-pod-uid")
	if _, err = client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	s.Desired = biz.DesiredPaused
	worker, repo := newProviderWorker(t, provider, s)
	before := providerWriteCount(client)
	providerWorkerStep(t, worker)
	if len(repo.prepared) != 0 || repo.state.Observed != biz.ObservedUnknown || providerWriteCount(client) != before {
		t.Fatal("pause treated ownership failure as permission to delete")
	}
}

func TestProviderWorkerUnknownCreateStillBlocksPause(t *testing.T) {
	client, provider, s := providerFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	s.Desired = biz.DesiredPaused
	pending := biz.Mutation{ID: uuid.NewString(), Kind: biz.MutationCreatePod, OperationID: s.LastOperationID, Version: s.Version}
	s.Pending = &pending
	worker, repo := newProviderWorker(t, provider, s)
	providerWorkerStep(t, worker)
	if repo.state.Observed != biz.ObservedUnknown || repo.state.Pending == nil || *repo.state.Pending != pending || len(repo.prepared) != 0 {
		t.Fatal("unknown create's absence completed pause")
	}
	// A late, attributable Pod resolves the existing journal. Only a subsequent
	// worker step may prepare its UID-fenced pause deletion.
	late, err := provider.Apply(context.Background(), repo.state, pending)
	if err != nil || late.Outcome != biz.MutationApplied {
		t.Fatalf("late create fixture: %#v %v", late, err)
	}
	providerWorkerStep(t, worker)
	if repo.state.Pending != nil || len(repo.prepared) != 0 || repo.state.ProviderRefs.PodUID != late.Refs.PodUID {
		t.Fatal("late Pod was not attributed before cleanup")
	}
	providerWorkerStep(t, worker)
	providerWorkerStep(t, worker)
	if repo.state.Observed != biz.ObservedPaused || repo.state.Pending != nil || len(repo.prepared) != 1 || repo.prepared[0].Kind != biz.MutationDeletePod || repo.prepared[0].TargetUID != late.Refs.PodUID || providerActionCount(client, "create", "persistentvolumeclaims") != 1 {
		t.Fatalf("late create bypassed pause cleanup: %#v", repo.state)
	}
}
