package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestReviewPodReportsFactsAndRecordedUIDMismatch(t *testing.T) {
	client, provider, s := providerFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	applyProvider(t, provider, &s, biz.MutationCreatePod)
	pod, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), "sandbox-"+s.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = unstructured.SetNestedField(pod.Object, "node-1", "spec", "nodeName"); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	s.ProviderRefs.PodUID = "different-recorded-uid"
	client.ClearActions()
	review := provider.reviewObject(context.Background(), podGVR, s.Snapshot.Namespace, pod.GetName(), s)
	if review.Status != "found" || review.UID != string(pod.GetUID()) || review.NodeName != "node-1" || review.OwnershipMatches == nil || !*review.OwnershipMatches || review.SpecMatches == nil || !*review.SpecMatches || review.RecordedUIDMatches == nil || *review.RecordedUIDMatches || review.Deleting == nil || *review.Deleting || review.ObservedAt == nil {
		t.Fatalf("review concealed observed mismatch: %+v", review)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "get" {
			t.Fatalf("review mutated provider: %s", action.GetVerb())
		}
	}
	pod.SetLabels(map[string]string{})
	if _, err = client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	mismatch := provider.reviewObject(context.Background(), podGVR, s.Snapshot.Namespace, pod.GetName(), s)
	encoded, err := json.Marshal(mismatch)
	if err != nil || !strings.Contains(string(encoded), `"ownership_matches":false`) || !strings.Contains(string(encoded), `"deleting":false`) || strings.Contains(string(encoded), `"provider_spec_checks_match"`) {
		t.Fatalf("mismatch must be explicit, unchecked spec omitted: %s %v", encoded, err)
	}
	pod.SetLabels(labels(s))
	containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
	containers[0].(map[string]any)["image"] = "unexpected"
	if err := unstructured.SetNestedSlice(pod.Object, containers, "spec", "containers"); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	specMismatch := provider.reviewObject(context.Background(), podGVR, s.Snapshot.Namespace, pod.GetName(), s)
	encoded, err = json.Marshal(specMismatch)
	if err != nil || !strings.Contains(string(encoded), `"provider_spec_checks_match":false`) {
		t.Fatalf("checked mismatch omitted: %s %v", encoded, err)
	}
	missing := provider.reviewObject(context.Background(), podGVR, s.Snapshot.Namespace, "absent", s)
	encoded, err = json.Marshal(missing)
	if err != nil || missing.Status != "not_found" || strings.Contains(string(encoded), `"ownership_matches"`) || strings.Contains(string(encoded), `"deleting"`) {
		t.Fatalf("not found must not invent checks: %s %v", encoded, err)
	}
}
