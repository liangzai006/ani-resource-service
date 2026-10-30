package instance

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func stoppedLifecycleFixture(t *testing.T) (*Notebook, biz.Instance, *fake.FakeDynamicClient, *unstructured.Unstructured, *unstructured.Unstructured) {
	t.Helper()
	n, i, client, pod := diagnosticFixture(t, false)
	i.Desired, i.RuntimeDesired = biz.Stopped, biz.Stopped
	i.StatefulSetUID = "set-uid"
	notebook, err := client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Get(context.Background(), i.Spec.NotebookName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	notebook.SetResourceVersion("10")
	notebook.SetGeneration(4)
	ann := notebook.GetAnnotations()
	ann[stoppedAnnotation] = "true"
	notebook.SetAnnotations(ann)
	if _, err := client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Update(context.Background(), notebook, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	set, err := client.Resource(stsGVR).Namespace(i.Spec.Namespace).Get(context.Background(), i.Spec.NotebookName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	set.SetResourceVersion("20")
	set.SetGeneration(7)
	_ = unstructured.SetNestedField(set.Object, int64(0), "spec", "replicas")
	_ = unstructured.SetNestedField(set.Object, int64(7), "status", "observedGeneration")
	podSpec, _, _ := unstructured.NestedMap(notebook.Object, "spec", "template", "spec")
	_ = unstructured.SetNestedMap(set.Object, podSpec, "spec", "template", "spec")
	if _, err := client.Resource(stsGVR).Namespace(i.Spec.Namespace).Update(context.Background(), set, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := client.Resource(podGVR).Namespace(i.Spec.Namespace).Delete(context.Background(), pod.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	return n, i, client, notebook, set
}

func TestLifecycleIdentityRequiresCompleteStopProof(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, *fake.FakeDynamicClient, biz.Instance, *unstructured.Unstructured, *unstructured.Unstructured){
		"replicas pending": func(t *testing.T, c *fake.FakeDynamicClient, i biz.Instance, nb, sts *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(sts.Object, int64(1), "spec", "replicas")
			_, _ = c.Resource(stsGVR).Namespace(i.Spec.Namespace).Update(context.Background(), sts, metav1.UpdateOptions{})
		},
		"controller generation pending": func(t *testing.T, c *fake.FakeDynamicClient, i biz.Instance, nb, sts *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(sts.Object, int64(6), "status", "observedGeneration")
			_, _ = c.Resource(stsGVR).Namespace(i.Spec.Namespace).Update(context.Background(), sts, metav1.UpdateOptions{})
		},
		"notebook running": func(t *testing.T, c *fake.FakeDynamicClient, i biz.Instance, nb, sts *unstructured.Unstructured) {
			ann := nb.GetAnnotations()
			delete(ann, stoppedAnnotation)
			nb.SetAnnotations(ann)
			_, _ = c.Resource(notebookGVR).Namespace(i.Spec.Namespace).Update(context.Background(), nb, metav1.UpdateOptions{})
		},
		"terminating pod without labels": func(t *testing.T, c *fake.FakeDynamicClient, i biz.Instance, nb, sts *unstructured.Unstructured) {
			pod := projectedPod(t, i)
			pod.SetAPIVersion("v1")
			pod.SetKind("Pod")
			pod.SetName(i.Spec.NotebookName + "-0")
			pod.SetUID("terminating-old")
			pod.SetLabels(nil)
			controller := true
			pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts.GetName(), UID: sts.GetUID(), Controller: &controller}})
			deleting := metav1.NewTime(time.Now())
			pod.SetDeletionTimestamp(&deleting)
			if _, err := c.Resource(podGVR).Namespace(i.Spec.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			n, i, client, notebook, set := stoppedLifecycleFixture(t)
			mutate(t, client, i, notebook, set)
			client.ClearActions()
			out, err := n.ObserveIdentity(context.Background(), i)
			if err != nil || out.SafeStopped {
				t.Fatal("incomplete stop accepted", out, err)
			}
			assertNotebookObservationReadOnly(t, client.Actions())
		})
	}
	n, i, client, _, _ := stoppedLifecycleFixture(t)
	out, err := n.ObserveIdentity(context.Background(), i)
	if err != nil || !out.SafeStopped || out.StatefulSetReplicas != 0 || out.StatefulSetObservedGeneration != 7 {
		t.Fatal("complete stop not proven", out, err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" && action.GetResource() == podGVR && action.(ktesting.ListAction).GetListRestrictions().Labels.String() != "" {
			t.Fatal("stop proof depended on mutable labels")
		}
	}
}

func TestLifecycleIdentityDirectlyReadsPersistedCPUHistory(t *testing.T) {
	n, i, client, _, _ := stoppedLifecycleFixture(t)
	ref := biz.PodRef{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, Name: "lost-labels", UID: "original-pod", ContainerName: biz.Container}
	i.TrackedPods = []biz.PodRef{ref}
	pod := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": ref.Name, "namespace": ref.Namespace, "uid": ref.UID}}}
	_, _ = client.Resource(podGVR).Namespace(ref.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	// Even a successful empty list cannot hide a persisted UID from its GET.
	client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.UnstructuredList{Object: map[string]any{"apiVersion": "v1", "kind": "PodList"}}, nil
	})
	out, err := n.ObserveIdentity(context.Background(), i)
	if err != nil || out.SafeStopped || !out.PodExists || len(out.Pods) != 1 || out.Pods[0].UID != ref.UID {
		t.Fatal("persisted CPU UID was ignored", out, err)
	}
	pod.SetUID("same-name-replacement")
	_, _ = client.Resource(podGVR).Namespace(ref.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
	out, err = n.ObserveIdentity(context.Background(), i)
	if err != nil || !out.SafeStopped {
		t.Fatal("same-name foreign UID mistaken for old Pod", out, err)
	}
}

func TestLifecycleIdentityRechecksControllerAfterPodRead(t *testing.T) {
	for _, resource := range []string{"notebooks", "statefulsets"} {
		t.Run(resource, func(t *testing.T) {
			n, i, client, notebook, set := stoppedLifecycleFixture(t)
			reads := 0
			client.PrependReactor("get", resource, func(action ktesting.Action) (bool, runtime.Object, error) {
				reads++
				obj := notebook.DeepCopy()
				if resource == "statefulsets" {
					obj = set.DeepCopy()
				}
				if reads > 1 {
					obj.SetResourceVersion("changed-during-pod-read")
					obj.SetGeneration(obj.GetGeneration() + 1)
				}
				return true, obj, nil
			})
			out, err := n.ObserveIdentity(context.Background(), i)
			if err != nil && !errors.Is(err, biz.ErrVersion) || out.SafeStopped || reads < 2 {
				t.Fatal("controller drift accepted as stopped", out, reads, err)
			}
		})
	}
	n, i, client, _, _ := stoppedLifecycleFixture(t)
	failure := errors.New("Pod list denied")
	client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, failure })
	if out, err := n.ObserveIdentity(context.Background(), i); !errors.Is(err, failure) || out.SafeStopped {
		t.Fatal("failed Pod list became stop proof", out, err)
	}
}

func TestLifecycleStopIgnoresRuntimeEligibilityButPreservesOwnership(t *testing.T) {
	n, i, client, _, _ := stoppedLifecycleFixture(t)
	i.CurrentMounts = []biz.VolumeMount{{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, PVCName: "missing", PVCUID: "missing-uid", MountPath: "/data", Ownership: "borrowed"}}
	i.Spec.ComputeType = biz.TypeGPU
	n.signingKey = nil
	client.ClearActions()
	if out, err := n.ObserveIdentity(context.Background(), i); err != nil || !out.SafeStopped {
		t.Fatal("PVC/GPU eligibility prevented owner proof", out, err)
	}
	stop := true
	m := biz.Mutation{ID: "stop-attempt", Kind: "set_stopped", OperationID: "operation", Version: 2, Phase: "recovering_stop", PhaseSequence: 4, TargetUID: i.NotebookUID, ResourceVersion: "10", StoppedTarget: &stop}
	if err := n.ApplyMutation(context.Background(), i, m); err != nil {
		t.Fatal("safe stop depended on runtime eligibility", err)
	}
	for _, action := range client.Actions() {
		if action.GetResource() == pvcGVR || action.GetResource().Resource == "namespaces" || action.GetResource() == podGVR && action.GetVerb() == "update" {
			t.Fatal("safe stop inspected PVC or signed GPU Pod", action)
		}
	}
	notebook, _ := client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Get(context.Background(), i.Spec.NotebookName, metav1.GetOptions{})
	wrong := notebook.DeepCopy()
	wrong.SetLabels(nil)
	_, _ = client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Update(context.Background(), wrong, metav1.UpdateOptions{})
	client.ClearActions()
	if err := n.ApplyMutation(context.Background(), i, m); !errors.Is(err, biz.ErrOwnership) {
		t.Fatal("foreign Notebook was eligible for safe stop", err)
	}
	assertNotebookObservationReadOnly(t, client.Actions())
}

func TestLifecycleUnknownWriteKeepsOriginalUIDRVAndMutation(t *testing.T) {
	n, i, client, _, _ := stoppedLifecycleFixture(t)
	stop := false
	m := biz.Mutation{ID: "original-attempt", Kind: "set_stopped", OperationID: "operation", Version: 2, Phase: "starting", PhaseSequence: 4, TargetUID: i.NotebookUID, ResourceVersion: "10", StoppedTarget: &stop}
	var sent []*unstructured.Unstructured
	client.PrependReactor("update", "notebooks", func(action ktesting.Action) (bool, runtime.Object, error) {
		sent = append(sent, action.(ktesting.UpdateAction).GetObject().(*unstructured.Unstructured).DeepCopy())
		return true, nil, context.DeadlineExceeded
	})
	for count := 0; count < 2; count++ {
		if err := n.ApplyMutation(context.Background(), i, m); !errors.Is(err, biz.ErrUnknown) {
			t.Fatal("timeout was treated as definitive", err)
		}
	}
	if len(sent) != 2 || !reflect.DeepEqual(sent[0].Object, sent[1].Object) || sent[0].GetResourceVersion() != m.ResourceVersion || string(sent[0].GetUID()) != m.TargetUID || sent[0].GetAnnotations()[mutationAnnotation] != m.ID || sent[0].GetAnnotations()[versionAnnotation] != "2" {
		t.Fatal("unknown retry changed its conditional payload", sent)
	}
	newer := sent[0].DeepCopy()
	newer.SetResourceVersion("11")
	if err := client.Tracker().Update(notebookGVR, newer, i.Spec.Namespace); err != nil {
		t.Fatal(err)
	}
	if err := n.ApplyMutation(context.Background(), i, m); !errors.Is(err, biz.ErrVersion) || len(sent) != 2 {
		t.Fatal("retry refreshed its saved resourceVersion", len(sent), err)
	}
}

func TestLifecycleCurrentEmptyNeverRestoresCreateMounts(t *testing.T) {
	i := configuredInstance(t)
	i.CurrentMounts = []biz.VolumeMount{}
	notebook, err := RenderNotebook(i)
	if err != nil {
		t.Fatal(err)
	}
	podSpec, _, _ := unstructured.NestedMap(notebook.Object, "spec", "template", "spec")
	if mountContentClass(podSpec, i.Spec, i.Spec.VolumeMounts, nil) != "target" {
		t.Fatal("empty current mounts fell back to CREATE")
	}
	if err := verifyPod(projectedPod(t, i), i); err != nil {
		t.Fatal("runtime verifier fell back to CREATE", err)
	}
	i.MountsInitialized = false
	if _, err := RenderNotebook(i); err == nil {
		t.Fatal("uninitialized configuration was implicitly initialized")
	}
}

func TestLifecycleAddedMountsRejectsReplacedUIDBeforeStop(t *testing.T) {
	n, i, client, _ := diagnosticFixture(t, false)
	i.CurrentMounts = []biz.VolumeMount{{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, PVCName: "existing-missing", PVCUID: "existing-original", MountPath: "/existing", Ownership: "borrowed", ReferenceGeneration: 1}}
	added := biz.VolumeMount{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, PVCName: "new-data", PVCUID: "new-data-frozen-original", MountPath: "/data", Ownership: "borrowed", ReferenceGeneration: 1}
	target := append(append([]biz.VolumeMount{}, i.CurrentMounts...), added)
	_, _ = client.Resource(namespaceGVR).Create(context.Background(), tenantNamespace(i.TenantID, i.Spec.Namespace), metav1.CreateOptions{})
	replacement := testBorrowedPVC(i.Spec.Namespace, added.PVCName, "same-name-replacement", "Bound", "Filesystem")
	_, _ = client.Resource(pvcGVR).Namespace(i.Spec.Namespace).Create(context.Background(), replacement, metav1.CreateOptions{})
	client.ClearActions()
	if err := n.VerifyAddedMounts(context.Background(), i, target); !errors.Is(err, biz.ErrPVCReferenceChanged) {
		t.Fatal("running Attach accepted same-name replacement before stop", err)
	}
	assertNotebookObservationReadOnly(t, client.Actions())
	for _, action := range client.Actions() {
		if action.GetResource() == notebookGVR || action.GetResource() == stsGVR || action.GetResource() == podGVR {
			t.Fatal("added-volume preflight touched running workload", action)
		}
		if get, ok := action.(ktesting.GetAction); ok && action.GetResource() == pvcGVR && get.GetName() != added.PVCName {
			t.Fatal("added-volume preflight inspected existing missing PVC", get.GetName())
		}
	}
	// The same frozen target is accepted once its original identity is present;
	// the provider does not substitute the UID from the earlier replacement.
	replacement.SetUID("new-data-frozen-original")
	_, _ = client.Resource(pvcGVR).Namespace(i.Spec.Namespace).Update(context.Background(), replacement, metav1.UpdateOptions{})
	if err := n.VerifyAddedMounts(context.Background(), i, target); err != nil {
		t.Fatal("original added PVC was rejected because another current PVC was missing", err)
	}
	if target[1].PVCUID != "new-data-frozen-original" {
		t.Fatal("preflight changed frozen target identity")
	}
}

func TestLifecycleAddedMountsDetachDoesNotReadRemainingInvalidPVC(t *testing.T) {
	n, i, client, _ := diagnosticFixture(t, false)
	i.CurrentMounts = []biz.VolumeMount{
		{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, PVCName: "remove-missing", PVCUID: "remove-original", MountPath: "/remove", Ownership: "borrowed", ReferenceGeneration: 1},
		{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, PVCName: "remaining-missing", PVCUID: "remaining-original", MountPath: "/remaining", Ownership: "borrowed", ReferenceGeneration: 1},
	}
	client.ClearActions()
	if err := n.VerifyAddedMounts(context.Background(), i, i.CurrentMounts[1:]); err != nil || len(client.Actions()) != 0 {
		t.Fatal("Detach preflight queried an unchanged remaining PVC", client.Actions(), err)
	}
}

func TestLifecycleTemplateRequiresNotebookAndSTSContent(t *testing.T) {
	n, i, client, notebook, set := stoppedLifecycleFixture(t)
	before := []biz.VolumeMount{}
	target := []biz.VolumeMount{{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, PVCName: "missing-original-pvc", PVCUID: "frozen-uid", MountPath: "/data", Ownership: "borrowed", ReferenceGeneration: 1}}
	proof, err := n.VerifyMountTemplate(context.Background(), i, before, target)
	if err != nil || proof.Class != "old" || !proof.SafeStopped {
		t.Fatal("old content unproven", proof, err)
	}
	if err := replaceManagedMounts(notebook, i.Spec, target); err != nil {
		t.Fatal(err)
	}
	ann := notebook.GetAnnotations()
	ann[mutationAnnotation] = "target-annotation-is-insufficient"
	notebook.SetAnnotations(ann)
	_, _ = client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Update(context.Background(), notebook, metav1.UpdateOptions{})
	proof, err = n.VerifyMountTemplate(context.Background(), i, before, target)
	if err != nil || proof.Class != "mixed" {
		t.Fatal("Notebook or annotation alone proved target", proof, err)
	}
	spec, _, _ := unstructured.NestedMap(notebook.Object, "spec", "template", "spec")
	_ = unstructured.SetNestedMap(set.Object, spec, "spec", "template", "spec")
	_, _ = client.Resource(stsGVR).Namespace(i.Spec.Namespace).Update(context.Background(), set, metav1.UpdateOptions{})
	client.ClearActions()
	proof, err = n.VerifyMountTemplate(context.Background(), i, before, target)
	if err != nil || proof.Class != "target" || !proof.SafeStopped {
		t.Fatal("matching content did not prove target", proof, err)
	}
	for _, action := range client.Actions() {
		if action.GetResource() == pvcGVR {
			t.Fatal("template proof depended on a missing PVC")
		}
	}
}

func TestLifecycleTemplateIgnoresStatusResourceVersionChurn(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprintf("stopped=%t", stopped), func(t *testing.T) {
			n, i, client, notebook, set := stoppedLifecycleFixture(t)
			// Controller probes change status/RV while the complete managed
			// templates and ownership remain stable in either runtime state.
			if !stopped {
				ann := notebook.GetAnnotations()
				delete(ann, stoppedAnnotation)
				notebook.SetAnnotations(ann)
				_ = unstructured.SetNestedField(set.Object, int64(1), "spec", "replicas")
			}
			lastNotebookRV := ""
			for resource, original := range map[string]*unstructured.Unstructured{"notebooks": notebook, "statefulsets": set} {
				resource, original := resource, original
				reads := 0
				client.PrependReactor("get", resource, func(ktesting.Action) (bool, runtime.Object, error) {
					reads++
					obj := original.DeepCopy()
					obj.SetResourceVersion(fmt.Sprintf("%s-status-%d", resource, reads))
					if resource == "notebooks" {
						lastNotebookRV = obj.GetResourceVersion()
					}
					_ = unstructured.SetNestedSlice(obj.Object, []any{map[string]any{"lastProbeTime": fmt.Sprintf("probe-%d", reads)}}, "status", "conditions")
					obj.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: fmt.Sprintf("status-manager-%d", reads)}})
					return true, obj, nil
				})
			}
			client.ClearActions()
			proof, err := n.VerifyMountTemplate(context.Background(), i, i.CurrentMounts, i.CurrentMounts)
			if err != nil || proof.Class != "target" || proof.SafeStopped != stopped || proof.NotebookRV != lastNotebookRV {
				t.Fatal("status-only controller updates prevented target proof", proof, err)
			}
			observation, err := n.ObserveIdentity(context.Background(), i)
			if err != nil || observation.SafeStopped != stopped || observation.NotebookRV != lastNotebookRV {
				t.Fatal("identity proof lost its latest trusted resourceVersion", observation, lastNotebookRV, err)
			}
			assertNotebookObservationReadOnly(t, client.Actions())
		})
	}
}

func TestLifecycleIdentityRefreshesRVAfterPodObservation(t *testing.T) {
	for _, test := range []struct {
		name    string
		stopped bool
		pod     bool
	}{
		{"running with Pod", false, true},
		{"running without Pod", false, false},
		{"stopping with Pod", true, true},
		{"stopped controller not observed", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			n, i, client, pod := diagnosticFixture(t, false)
			notebook, err := client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Get(context.Background(), i.Spec.NotebookName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			notebook.SetResourceVersion("10")
			if test.stopped {
				annotations := notebook.GetAnnotations()
				annotations[stoppedAnnotation] = "true"
				notebook.SetAnnotations(annotations)
			}
			if err := client.Tracker().Update(notebookGVR, notebook, i.Spec.Namespace); err != nil {
				t.Fatal(err)
			}
			if !test.pod {
				if err := client.Tracker().Delete(podGVR, i.Spec.Namespace, pod.GetName()); err != nil {
					t.Fatal(err)
				}
			}
			// Model the real controller probe advancing only status/RV while
			// ObserveIdentity is gathering its Pod/StatefulSet observations.
			client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
				latest := notebook.DeepCopy()
				latest.SetResourceVersion("11")
				latest.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastProbeTime": "2026-10-29T15:03:42Z"}}}
				latest.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "notebook-status-controller", Subresource: "status"}})
				if err := client.Tracker().Update(notebookGVR, latest, i.Spec.Namespace); err != nil {
					t.Fatal(err)
				}
				return false, nil, nil
			})
			client.ClearActions()
			out, err := n.ObserveIdentity(context.Background(), i)
			if err != nil || !out.NotebookExists || out.NotebookUID != i.NotebookUID || out.NotebookRV != "11" || out.Stopped != test.stopped || out.PodExists != test.pod || out.SafeStopped {
				t.Fatal("stable control observation did not return the final Notebook RV", out, err)
			}
			assertNotebookObservationReadOnly(t, client.Actions())
		})
	}
}

func TestLifecycleIdentityRejectsControlDriftAfterPodObservation(t *testing.T) {
	for name, mutate := range map[string]func(*unstructured.Unstructured){
		"UID": func(o *unstructured.Unstructured) { o.SetUID("same-name-replacement") },
		"ownership": func(o *unstructured.Unstructured) {
			labels := o.GetLabels()
			labels[resourceLabel] = "another-instance"
			o.SetLabels(labels)
		},
		"spec": func(o *unstructured.Unstructured) {
			containers, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
			containers[0].(map[string]any)["image"] = "unexpected-image:changed"
			_ = unstructured.SetNestedSlice(o.Object, containers, "spec", "template", "spec", "containers")
		},
		"metadata": func(o *unstructured.Unstructured) {
			annotations := o.GetAnnotations()
			annotations["test.ani.io/controller-policy"] = "changed"
			o.SetAnnotations(annotations)
		},
	} {
		t.Run(name, func(t *testing.T) {
			n, i, client, _ := diagnosticFixture(t, false)
			notebook, err := client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Get(context.Background(), i.Spec.NotebookName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			notebook.SetResourceVersion("10")
			if err := client.Tracker().Update(notebookGVR, notebook, i.Spec.Namespace); err != nil {
				t.Fatal(err)
			}
			client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
				latest := notebook.DeepCopy()
				mutate(latest)
				latest.SetResourceVersion("11")
				if err := client.Tracker().Update(notebookGVR, latest, i.Spec.Namespace); err != nil {
					t.Fatal(err)
				}
				return false, nil, nil
			})
			client.ClearActions()
			out, err := n.ObserveIdentity(context.Background(), i)
			if !errors.Is(err, biz.ErrVersion) && !errors.Is(err, biz.ErrOwnership) {
				t.Fatal("control drift returned an authorized Notebook write observation", out, err)
			}
			assertNotebookObservationReadOnly(t, client.Actions())
		})
	}
}

func TestLifecycleTemplateRejectsCriticalDrift(t *testing.T) {
	mutations := map[string]func(*unstructured.Unstructured){
		"UID":        func(o *unstructured.Unstructured) { o.SetUID("same-name-replacement") },
		"generation": func(o *unstructured.Unstructured) { o.SetGeneration(o.GetGeneration() + 1) },
		"non-mount template spec": func(o *unstructured.Unstructured) {
			containers, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "containers")
			containers[0].(map[string]any)["image"] = "unexpected-image:changed"
			_ = unstructured.SetNestedSlice(o.Object, containers, "spec", "template", "spec", "containers")
		},
		"owned label": func(o *unstructured.Unstructured) {
			m := o.GetLabels()
			if m == nil {
				m = map[string]string{}
			}
			m[resourceLabel] = "another-instance"
			o.SetLabels(m)
		},
		"other label": func(o *unstructured.Unstructured) {
			m := o.GetLabels()
			if m == nil {
				m = map[string]string{}
			}
			m["test.ani.io/controller"] = "changed"
			o.SetLabels(m)
		},
		"owner reference": func(o *unstructured.Unstructured) {
			controller := true
			o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubeflow.org/v1", Kind: "Notebook", Name: "foreign", UID: "foreign", Controller: &controller}})
		},
		"stopped marker": func(o *unstructured.Unstructured) {
			m := o.GetAnnotations()
			if m == nil {
				m = map[string]string{}
			}
			m[stoppedAnnotation] = "true"
			o.SetAnnotations(m)
		},
		"intent marker": func(o *unstructured.Unstructured) {
			m := o.GetAnnotations()
			if m == nil {
				m = map[string]string{}
			}
			m[versionAnnotation] = "99"
			o.SetAnnotations(m)
		},
		"mutation marker": func(o *unstructured.Unstructured) {
			m := o.GetAnnotations()
			if m == nil {
				m = map[string]string{}
			}
			m[mutationAnnotation] = "another-mutation"
			o.SetAnnotations(m)
		},
		"finalizer": func(o *unstructured.Unstructured) { o.SetFinalizers([]string{"test.ani.io/controller"}) },
		"deletion timestamp": func(o *unstructured.Unstructured) {
			stamp := metav1.NewTime(time.Unix(1000, 0))
			o.SetDeletionTimestamp(&stamp)
		},
	}
	for _, resource := range []string{"notebooks", "statefulsets"} {
		checks := map[string]func(*unstructured.Unstructured){}
		for name, mutate := range mutations {
			checks[name] = mutate
		}
		if resource == "statefulsets" {
			checks["controller generation regressed"] = func(o *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(o.Object, int64(6), "status", "observedGeneration")
			}
			checks["controller generation changed"] = func(o *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(o.Object, int64(8), "status", "observedGeneration")
			}
		}
		for _, boundary := range []string{"identity read", "pod observation"} {
			for name, mutate := range checks {
				t.Run(resource+"/"+boundary+"/"+name, func(t *testing.T) {
					n, i, client, notebook, set := stoppedLifecycleFixture(t)
					ann := notebook.GetAnnotations()
					delete(ann, stoppedAnnotation)
					notebook.SetAnnotations(ann)
					_ = unstructured.SetNestedField(set.Object, int64(1), "spec", "replicas")
					for key, obj := range map[string]*unstructured.Unstructured{"notebooks": notebook, "statefulsets": set} {
						key, obj := key, obj
						changed, reads := false, 0
						if key == resource && boundary == "pod observation" {
							client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) { changed = true; return false, nil, nil })
						}
						client.PrependReactor("get", key, func(ktesting.Action) (bool, runtime.Object, error) {
							reads++
							out := obj.DeepCopy()
							if key == resource && (changed || boundary == "identity read" && reads > 1) {
								mutate(out)
								out.SetResourceVersion("critical-change")
							}
							return true, out, nil
						})
					}
					client.ClearActions()
					proof, err := n.VerifyMountTemplate(context.Background(), i, i.CurrentMounts, i.CurrentMounts)
					if proof.Class != "mixed" || err != nil && !errors.Is(err, biz.ErrVersion) && !errors.Is(err, biz.ErrOwnership) {
						t.Fatal("critical drift produced a template proof", proof, err)
					}
					assertNotebookObservationReadOnly(t, client.Actions())
				})
			}
		}
	}
}

func TestLifecycleTemplateRequiresSafeStopForMissingStatefulSet(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprintf("stopped=%t", stopped), func(t *testing.T) {
			n, i, client, notebook, _ := stoppedLifecycleFixture(t)
			if !stopped {
				ann := notebook.GetAnnotations()
				delete(ann, stoppedAnnotation)
				notebook.SetAnnotations(ann)
				_, _ = client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Update(context.Background(), notebook, metav1.UpdateOptions{})
			}
			if err := client.Resource(stsGVR).Namespace(i.Spec.Namespace).Delete(context.Background(), i.Spec.NotebookName, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			client.ClearActions()
			proof, err := n.VerifyMountTemplate(context.Background(), i, i.CurrentMounts, i.CurrentMounts)
			if err != nil || (proof.Class == "target") != stopped || proof.SafeStopped != stopped {
				t.Fatal("missing StatefulSet bypassed safe-stop requirement", proof, err)
			}
			assertNotebookObservationReadOnly(t, client.Actions())
		})
	}
}

func TestLifecycleDetachMissingPVCReplacesOnlyManagedFields(t *testing.T) {
	n, i, client, notebook, set := stoppedLifecycleFixture(t)
	i.CurrentMounts = []biz.VolumeMount{
		{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, PVCName: "same", PVCUID: "same-uid", MountPath: "/one", Ownership: "borrowed", ReferenceGeneration: 1},
		{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, PVCName: "missing", PVCUID: "missing-uid", MountPath: "/two", Ownership: "borrowed", ReferenceGeneration: 1},
		{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, PVCName: "same", PVCUID: "same-uid", MountPath: "/three", ReadOnly: true, Ownership: "borrowed", ReferenceGeneration: 1},
	}
	if err := replaceManagedMounts(notebook, i.Spec, i.CurrentMounts); err != nil {
		t.Fatal(err)
	}
	_, _ = client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Update(context.Background(), notebook, metav1.UpdateOptions{})
	spec, _, _ := unstructured.NestedMap(notebook.Object, "spec", "template", "spec")
	_ = unstructured.SetNestedMap(set.Object, spec, "spec", "template", "spec")
	_, _ = client.Resource(stsGVR).Namespace(i.Spec.Namespace).Update(context.Background(), set, metav1.UpdateOptions{})
	target := []biz.VolumeMount{i.CurrentMounts[0], i.CurrentMounts[2]}
	m := biz.Mutation{ID: "detach-attempt", Kind: "update_mounts", OperationID: "operation", Version: 2, Phase: "applying_mounts", PhaseSequence: 3, TargetUID: i.NotebookUID, ResourceVersion: "10", TargetMountDigest: biz.MountDigest(target)}
	client.ClearActions()
	if err := n.UpdateMounts(context.Background(), i, m, target); err != nil {
		t.Fatal("detach required remaining PVCs to exist", err)
	}
	written, _ := client.Resource(notebookGVR).Namespace(i.Spec.Namespace).Get(context.Background(), i.Spec.NotebookName, metav1.GetOptions{})
	writtenSpec, _, _ := unstructured.NestedMap(written.Object, "spec", "template", "spec")
	if mountContentClass(writtenSpec, i.Spec, i.CurrentMounts, target) != "target" {
		t.Fatal("deleted middle mount left stale managed fields")
	}
	oldContainers, _, _ := unstructured.NestedSlice(spec, "containers")
	newContainers, _, _ := unstructured.NestedSlice(writtenSpec, "containers")
	delete(oldContainers[0].(map[string]any), "volumeMounts")
	delete(newContainers[0].(map[string]any), "volumeMounts")
	delete(spec, "volumes")
	delete(writtenSpec, "volumes")
	spec["containers"], writtenSpec["containers"] = oldContainers, newContainers
	if !reflect.DeepEqual(spec, writtenSpec) {
		t.Fatal("mount change modified image/env/compute/workspace settings")
	}
	for _, action := range client.Actions() {
		if action.GetResource() == pvcGVR || action.GetResource() == stsGVR && action.GetVerb() != "get" {
			t.Fatal("detach touched PVC lifecycle or STS", action)
		}
	}
}
