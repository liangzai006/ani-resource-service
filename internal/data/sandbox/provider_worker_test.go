package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

func TestProviderWorkerPendingCreateRequiresMatchingMutationAnnotation(t *testing.T) {
	for _, kind := range []string{biz.MutationCreateWorkspace, biz.MutationCreatePod} {
		for _, annotation := range []string{"", "different-attempt"} {
			t.Run(kind+"/"+annotation, func(t *testing.T) {
				client, provider, s := providerFixture(t)
				if kind == biz.MutationCreatePod {
					applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
				}
				pending := biz.Mutation{ID: uuid.NewString(), Kind: kind, OperationID: s.LastOperationID, Version: s.Version}
				s.Pending, s.Desired = &pending, biz.DesiredDeleted
				resource, name := pvcGVR, "workspace-"+s.ID
				object := renderWorkspace(s, name)
				if kind == biz.MutationCreatePod {
					resource, name = podGVR, "sandbox-"+s.ID
					object = renderPod(s, name, labels(s))
				}
				if annotation != "" {
					object.SetAnnotations(map[string]string{providerMutationID: annotation})
				}
				if _, err := client.Resource(resource).Namespace(s.Snapshot.Namespace).Create(context.Background(), object, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				worker, repo := newProviderWorker(t, provider, s)
				providerWorkerStep(t, worker)
				if repo.state.Pending == nil || *repo.state.Pending != pending || repo.state.Observed != biz.ObservedUnknown || len(repo.prepared) != 0 {
					t.Fatal("unattributable object cleared the unknown create barrier")
				}
				if result, err := provider.Apply(context.Background(), s, pending); !errors.Is(err, biz.ErrOwnership) || result.Outcome != biz.MutationNotSent {
					t.Fatalf("unattributable object was accepted during create: result=%#v err=%v", result, err)
				}
				observed, err := client.Resource(resource).Namespace(s.Snapshot.Namespace).Get(context.Background(), name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				observed.SetAnnotations(map[string]string{providerMutationID: pending.ID})
				if _, err = client.Resource(resource).Namespace(s.Snapshot.Namespace).Update(context.Background(), observed, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				providerWorkerStep(t, worker)
				if repo.state.Pending != nil || len(repo.prepared) != 0 {
					t.Fatal("object from the matching mutation did not resolve the barrier")
				}
			})
		}
	}
}

func TestProviderWorkerStopsPodDespiteVolumeReadFailure(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"Forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "", errors.New("no PV read permission"))},
		{"Timeout", apierrors.NewTimeoutError("PV read timed out", 0)},
	} {
		for _, desired := range []string{biz.DesiredPaused, biz.DesiredDeleted} {
			t.Run(failure.name+"/"+desired, func(t *testing.T) {
				client, provider, s := providerFixture(t)
				applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
				providerBoundWorkspace(t, client, s)
				applyProvider(t, provider, &s, biz.MutationCreatePod)
				providerPodStatus(t, client, s, "Running", "True", true)
				observeProvider(t, provider, &s)
				original := s.ProviderRefs
				s.Desired, s.Version, s.LastOperationID = desired, s.Version+1, uuid.NewString()
				worker, repo := newProviderWorker(t, provider, s)
				readFailed := true
				client.PrependReactor("get", "persistentvolumes", func(ktesting.Action) (bool, runtime.Object, error) {
					if readFailed {
						return true, nil, failure.err
					}
					return false, nil, nil
				})
				client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
					options := action.(ktesting.DeleteAction).GetDeleteOptions()
					if repo.state.Pending == nil || repo.state.Pending.Kind != biz.MutationDeletePod || repo.state.Pending.TargetUID != original.PodUID || options.Preconditions == nil || options.Preconditions.UID == nil || string(*options.Preconditions.UID) != original.PodUID {
						t.Fatal("storage failure bypassed the journal or original Pod UID fence")
					}
					return false, nil, nil
				})
				providerWorkerStep(t, worker)
				if _, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), original.PodName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Fatalf("known Pod was not stopped: %v", err)
				}
				if repo.state.Observed == biz.ObservedPaused || repo.state.Observed == biz.ObservedDeleted || repo.state.Reason != "WORKSPACE_VOLUME_OBSERVATION_FAILED" || repo.state.Pending != nil || len(repo.prepared) != 1 || providerActionCount(client, "delete", "persistentvolumeclaims") != 0 {
					t.Fatalf("stop used its pre-write observation or lost storage diagnosis: %#v", repo.state)
				}
				podReads := providerActionCount(client, "get", "pods")
				providerWorkerStep(t, worker)
				if providerActionCount(client, "get", "pods") <= podReads || repo.state.Reason != "WORKSPACE_VOLUME_OBSERVATION_FAILED" || repo.state.ProviderRefs.PVCUID != original.PVCUID || repo.state.ProviderRefs.PVUID != original.PVUID || repo.state.ProviderRefs.PodUID != "" || repo.state.ProviderRefs.CleanupReceipt != "" || providerActionCount(client, "delete", "persistentvolumeclaims") != 0 {
					t.Fatalf("fresh PodGone observation lost the storage barrier or identities: %#v", repo.state)
				}
				if desired == biz.DesiredPaused {
					if repo.state.Observed != biz.ObservedPaused {
						t.Fatalf("PV read failure prevented pause after fresh PodGone: %#v", repo.state)
					}
					// A successful stop does not authorize Resume while storage facts
					// remain unknown, nor permit an empty replacement workspace.
					repo.state.Desired, repo.state.Version, repo.state.LastOperationID = biz.DesiredRunning, repo.state.Version+1, uuid.NewString()
					providerWorkerStep(t, worker)
					if repo.state.Observed != biz.ObservedUnknown || len(repo.prepared) != 1 || providerActionCount(client, "create", "pods") != 1 || providerActionCount(client, "create", "persistentvolumeclaims") != 1 {
						t.Fatalf("Resume bypassed unknown storage: %#v", repo.state)
					}
					return
				}
				if repo.state.Observed != biz.ObservedUnknown {
					t.Fatalf("delete completed before storage could be observed: %#v", repo.state)
				}
				readFailed = false
				providerWorkerStep(t, worker)
				if providerActionCount(client, "delete", "persistentvolumeclaims") != 1 || repo.state.ProviderRefs.PVCUID != original.PVCUID || repo.state.ProviderRefs.PVUID != original.PVUID || repo.state.Pending != nil {
					t.Fatalf("storage recovery did not delete only the original workspace: %#v", repo.state)
				}
				providerWorkerStep(t, worker)
				if repo.state.Observed != biz.ObservedDeleting || repo.state.ProviderRefs.CleanupReceipt != "" {
					t.Fatalf("PVCGone bypassed remaining PV: %#v", repo.state)
				}
				readFailed = true
				providerWorkerStep(t, worker)
				if repo.state.Observed != biz.ObservedUnknown || repo.state.Reason != "WORKSPACE_VOLUME_OBSERVATION_FAILED" || repo.state.ProviderRefs.CleanupReceipt != "" {
					t.Fatalf("PV query failure became a reclaim receipt: %#v", repo.state)
				}
				readFailed = false
				if err := client.Tracker().Delete(pvGVR, "", original.PVName); err != nil {
					t.Fatal(err)
				}
				providerWorkerStep(t, worker)
				if repo.state.Observed != biz.ObservedDeleted || repo.state.ProviderRefs.CleanupReceipt == "" || repo.state.ProviderRefs.PVCUID != original.PVCUID || repo.state.ProviderRefs.PVUID != original.PVUID || providerActionCount(client, "delete", "persistentvolumes") != 0 {
					t.Fatalf("delete failed to wait for provider-observed PVGone: %#v", repo.state)
				}
			})
		}
	}
}

func TestProviderWorkerVolumeReadFailureKeepsIdentityAndUnknownCreateGuards(t *testing.T) {
	for _, guard := range []string{"PodReadForbidden", "PodReadTimeout", "PodUID", "PodSpec", "PodOwner", "PVOwner", "Cluster", "UnknownCreate"} {
		for _, desired := range []string{biz.DesiredPaused, biz.DesiredDeleted} {
			t.Run(guard+"/"+desired, func(t *testing.T) {
				client, provider, s := providerFixture(t)
				applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
				providerBoundWorkspace(t, client, s)
				applyProvider(t, provider, &s, biz.MutationCreatePod)
				observeProvider(t, provider, &s)
				s.Desired = desired
				if guard == "PVOwner" {
					pv, err := client.Resource(pvGVR).Get(context.Background(), s.ProviderRefs.PVName, metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if err = unstructured.SetNestedField(pv.Object, "foreign-claim", "spec", "claimRef", "uid"); err != nil {
						t.Fatal(err)
					}
					if _, err = client.Resource(pvGVR).Update(context.Background(), pv, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				} else {
					client.PrependReactor("get", "persistentvolumes", func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewTimeoutError("PV unavailable", 0)
					})
				}
				switch guard {
				case "PodReadForbidden":
					client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, s.ProviderRefs.PodName, errors.New("no Pod read permission"))
					})
				case "PodReadTimeout":
					client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewTimeoutError("Pod read timed out", 0)
					})
				case "PodUID", "PodSpec", "PodOwner":
					pod, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), s.ProviderRefs.PodName, metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					switch guard {
					case "PodUID":
						pod.SetUID("foreign-pod")
					case "PodSpec":
						containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
						containers[0].(map[string]any)["image"] = "foreign-image"
						if err = unstructured.SetNestedSlice(pod.Object, containers, "spec", "containers"); err != nil {
							t.Fatal(err)
						}
					case "PodOwner":
						pod.SetLabels(nil)
					}
					if _, err = client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				case "Cluster":
					s.Snapshot.ClusterID = uuid.NewString()
				case "UnknownCreate":
					if err := client.Tracker().Delete(podGVR, s.Snapshot.Namespace, s.ProviderRefs.PodName); err != nil {
						t.Fatal(err)
					}
					s.Pending = &biz.Mutation{ID: uuid.NewString(), Kind: biz.MutationCreatePod, OperationID: s.LastOperationID, Version: s.Version}
				}
				worker, repo := newProviderWorker(t, provider, s)
				writes := providerWriteCount(client)
				actions := len(client.Actions())
				providerWorkerStep(t, worker)
				if repo.state.Observed != biz.ObservedUnknown || len(repo.prepared) != 0 || providerWriteCount(client) != writes || repo.state.ProviderRefs.CleanupReceipt != "" || repo.state.ProviderRefs.PVCUID != s.ProviderRefs.PVCUID {
					t.Fatalf("storage read handling bypassed %s: %#v", guard, repo.state)
				}
				if guard == "UnknownCreate" && (repo.state.Pending == nil || *repo.state.Pending != *s.Pending) {
					t.Fatal("PV failure erased the unknown creation barrier")
				}
				if guard == "Cluster" && len(client.Actions()) != actions {
					t.Fatal("cluster mismatch sent Kubernetes requests")
				}
			})
		}
	}
}

func TestProviderWorkerPVListFailureStopsPodWithoutReplacingMissingWorkspace(t *testing.T) {
	client, provider, s := providerFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	providerBoundWorkspace(t, client, s)
	applyProvider(t, provider, &s, biz.MutationCreatePod)
	// The PVC disappeared before its volume name could be recorded. PV list
	// cannot resolve the original binding, but the Pod UID is still confirmed.
	if s.ProviderRefs.PVName != "" || s.ProviderRefs.PVUID != "" {
		t.Fatal("fixture already persisted its PV binding")
	}
	if err := client.Tracker().Delete(pvcGVR, s.Snapshot.Namespace, s.ProviderRefs.PVCName); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("list", "persistentvolumes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "", errors.New("no PV list permission"))
	})
	originalPVC, originalPod := s.ProviderRefs.PVCUID, s.ProviderRefs.PodUID
	s.Desired = biz.DesiredPaused
	worker, repo := newProviderWorker(t, provider, s)
	providerWorkerStep(t, worker)
	providerWorkerStep(t, worker)
	if repo.state.Observed != biz.ObservedPaused || repo.state.Reason != "WORKSPACE_VOLUME_OBSERVATION_FAILED" || repo.state.ProviderRefs.PVCUID != originalPVC || repo.state.ProviderRefs.PodUID != "" || repo.state.Pending != nil || len(repo.prepared) != 1 || repo.prepared[0].Kind != biz.MutationDeletePod || repo.prepared[0].TargetUID != originalPod || repo.state.ProviderRefs.CleanupReceipt != "" {
		t.Fatalf("PV list failure prevented safe stop or lost original claim identity: %#v", repo.state)
	}
	repo.state.Desired, repo.state.Version, repo.state.LastOperationID = biz.DesiredRunning, repo.state.Version+1, uuid.NewString()
	providerWorkerStep(t, worker)
	if repo.state.Observed != biz.ObservedUnknown || repo.state.ProviderRefs.PVCUID != originalPVC || len(repo.prepared) != 1 || providerActionCount(client, "create", "persistentvolumeclaims") != 1 || providerActionCount(client, "create", "pods") != 1 {
		t.Fatalf("Resume replaced the missing original workspace: %#v", repo.state)
	}
}
