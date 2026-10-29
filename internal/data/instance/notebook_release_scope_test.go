package instance

import (
	"context"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func TestNotebookDeleteChecksHistoricalPodUIDs(t *testing.T) {
	for _, useFallback := range []bool{false, true} {
		name := "history"
		if useFallback {
			name = "current identity fallback"
		}
		t.Run(name, func(t *testing.T) {
			provider, i, client, pod := diagnosticFixture(t, !useFallback)
			i.Desired, i.RuntimeDesired = biz.Deleted, biz.Deleted
			if useFallback {
				i.PodName, i.PodUID = pod.GetName(), string(pod.GetUID())
			} else {
				i.GPUPods = []biz.PodRef{{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, Name: pod.GetName(), UID: string(pod.GetUID()), ContainerName: biz.Container}}
			}
			for _, resource := range []struct {
				gvr  string
				name string
			}{{"notebooks", i.Spec.NotebookName}, {"statefulsets", i.Spec.NotebookName}} {
				gvr := notebookGVR
				if resource.gvr == "statefulsets" {
					gvr = stsGVR
				}
				if err := client.Resource(gvr).Namespace(i.Spec.Namespace).Delete(context.Background(), resource.name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			pod.SetLabels(nil)
			if _, err := client.Resource(podGVR).Namespace(i.Spec.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			client.ClearActions()
			out, err := provider.Observe(context.Background(), i)
			if err != nil || !out.PodExists || out.PodUID != string(pod.GetUID()) || out.PodName != pod.GetName() {
				t.Fatal("label removal hid the original Pod from DELETE", out, err)
			}
			assertNotebookObservationReadOnly(t, client.Actions())
			// The identity must survive the worker's normal observation writeback.
			i.PodName, i.PodUID = out.PodName, out.PodUID
			out, err = provider.Observe(context.Background(), i)
			if err != nil || !out.PodExists {
				t.Fatal("residual Pod identity was lost on the next DELETE turn", out, err)
			}
			pod.SetUID("same-name-replacement")
			if _, err := client.Resource(podGVR).Namespace(i.Spec.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			client.ClearActions()
			out, err = provider.Observe(context.Background(), i)
			if err != nil || out.PodExists {
				t.Fatal("same-name replacement was mistaken for the original Pod", out, err)
			}
			assertNotebookObservationReadOnly(t, client.Actions())
			if err := client.Resource(podGVR).Namespace(i.Spec.Namespace).Delete(context.Background(), pod.GetName(), metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			out, err = provider.Observe(context.Background(), i)
			if err != nil || out.PodExists {
				t.Fatal("confirmed historical UID absence did not permit cleanup", out, err)
			}
		})
	}
}

func TestNotebookDeleteHistoricalPodReadFailsClosed(t *testing.T) {
	provider, i, client, pod := diagnosticFixture(t, true)
	i.Desired, i.RuntimeDesired = biz.Deleted, biz.Deleted
	i.GPUPods = []biz.PodRef{{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, Name: pod.GetName(), UID: string(pod.GetUID()), ContainerName: biz.Container}}
	pod.SetLabels(nil)
	if _, err := client.Resource(podGVR).Namespace(i.Spec.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("historical Pod read unavailable")
	client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, failure
	})
	client.ClearActions()
	if _, err := provider.Observe(context.Background(), i); !errors.Is(err, failure) {
		t.Fatal("historical Pod read failure allowed cleanup", err)
	}
	assertNotebookObservationReadOnly(t, client.Actions())
}

func TestNotebookDeleteHistoricalPodScopeIsFrozen(t *testing.T) {
	for name, mutate := range map[string]func(*biz.PodRef){
		"cluster":           func(ref *biz.PodRef) { ref.ClusterID = "foreign-cluster" },
		"namespace":         func(ref *biz.PodRef) { ref.Namespace = "foreign-namespace" },
		"missing cluster":   func(ref *biz.PodRef) { ref.ClusterID = "" },
		"missing namespace": func(ref *biz.PodRef) { ref.Namespace = "" },
		"missing name":      func(ref *biz.PodRef) { ref.Name = "" },
		"missing UID":       func(ref *biz.PodRef) { ref.UID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			provider, i, client, pod := diagnosticFixture(t, true)
			i.Desired, i.RuntimeDesired = biz.Deleted, biz.Deleted
			ref := biz.PodRef{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, Name: pod.GetName(), UID: string(pod.GetUID()), ContainerName: biz.Container}
			mutate(&ref)
			i.GPUPods = []biz.PodRef{ref}
			client.ClearActions()
			if _, err := provider.Observe(context.Background(), i); !errors.Is(err, biz.ErrOwnership) {
				t.Fatal("invalid historical scope accepted", err)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "get" && action.GetResource().Resource == "pods" {
					t.Fatal("historical scope validation issued a Pod GET")
				}
			}
		})
	}
}

func TestNotebookDeleteDirectlyReadsListedHistoricalPodUID(t *testing.T) {
	provider, i, client, pod := diagnosticFixture(t, true)
	i.Desired, i.RuntimeDesired = biz.Deleted, biz.Deleted
	i.GPUPods = []biz.PodRef{{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, Name: pod.GetName(), UID: string(pod.GetUID()), ContainerName: biz.Container}}
	reads := 0
	client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		reads++
		return false, nil, nil
	})
	out, err := provider.Observe(context.Background(), i)
	if err != nil || !out.PodExists || reads == 0 {
		t.Fatal("persisted Pod UID was not directly read", out, reads, err)
	}
}

func assertNotebookObservationReadOnly(t *testing.T, actions []ktesting.Action) {
	t.Helper()
	for _, action := range actions {
		if action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Fatal("historical Pod observation mutated Kubernetes", action.GetVerb())
		}
	}
}
