package sandbox

import (
	"context"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ktesting "k8s.io/client-go/testing"
)

func TestVerifyDataPlaneTargetRejectsProviderDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *KubernetesProvider, *biz.Sandbox, *biz.DataPlaneTarget)
	}{
		{"ready", nil},
		{"wrong version", func(_ *testing.T, _ *KubernetesProvider, _ *biz.Sandbox, target *biz.DataPlaneTarget) {
			target.Version++
		}},
		{"wrong cluster", func(_ *testing.T, _ *KubernetesProvider, s *biz.Sandbox, target *biz.DataPlaneTarget) {
			s.Snapshot.ClusterID = "55555555-5555-4555-8555-555555555555"
			target.ClusterID = s.Snapshot.ClusterID
		}},
		{"wrong namespace UID", func(_ *testing.T, _ *KubernetesProvider, s *biz.Sandbox, target *biz.DataPlaneTarget) {
			s.Snapshot.NamespaceUID = "other"
			target.NamespaceUID = "other"
		}},
		{"wrong pod UID", func(_ *testing.T, _ *KubernetesProvider, s *biz.Sandbox, target *biz.DataPlaneTarget) {
			s.ProviderRefs.PodUID = "other"
			target.PodUID = "other"
		}},
		{"not ready", func(t *testing.T, p *KubernetesProvider, s *biz.Sandbox, _ *biz.DataPlaneTarget) {
			c := p.client
			pod, err := c.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), s.ProviderRefs.PodName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			pod.Object["status"] = map[string]any{"phase": "Pending"}
			if _, err = c.Resource(podGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
		{"pod replaced", func(t *testing.T, p *KubernetesProvider, s *biz.Sandbox, _ *biz.DataPlaneTarget) {
			c := p.client
			pod, err := c.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), s.ProviderRefs.PodName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			pod.SetUID(types.UID("replacement"))
			if _, err = c.Resource(podGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
		{"legacy pod lacks UID binding", func(t *testing.T, p *KubernetesProvider, s *biz.Sandbox, _ *biz.DataPlaneTarget) {
			c := p.client
			pod, err := c.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), s.ProviderRefs.PodName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			containers := pod.Object["spec"].(map[string]any)["containers"].([]any)
			delete(containers[0].(map[string]any), "env")
			if !p.matchesPod(pod, *s) {
				t.Fatal("legacy pod must remain valid for lifecycle observation")
			}
			if _, err = c.Resource(podGVR).Namespace(s.Snapshot.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
		{"pause", func(_ *testing.T, _ *KubernetesProvider, s *biz.Sandbox, _ *biz.DataPlaneTarget) {
			s.Desired = biz.DesiredPaused
		}},
		{"unconfigured namespace guard", func(_ *testing.T, p *KubernetesProvider, _ *biz.Sandbox, _ *biz.DataPlaneTarget) {
			p.tenantNamespacePrefix = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, provider, s := configuredNamespaceFixture(t)
			applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
			applyProvider(t, provider, &s, biz.MutationCreatePod)
			providerPodStatus(t, client, s, "Running", "True", true)
			s.Observed = biz.ObservedRunning
			target := biz.DataPlaneTarget{TenantID: s.TenantID, SandboxID: s.ID, Action: biz.DataPlaneCodeRun, Version: s.Version, ClusterID: s.Snapshot.ClusterID, Namespace: s.Snapshot.Namespace, NamespaceUID: s.Snapshot.NamespaceUID, PodName: s.ProviderRefs.PodName, PodUID: s.ProviderRefs.PodUID, Container: "sandbox"}
			ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: s.TenantID, Actor: "user"})
			if tc.change != nil {
				tc.change(t, provider, &s, &target)
			}
			err := provider.VerifyDataPlaneTarget(ctx, s, target)
			if tc.change == nil && err != nil {
				t.Fatalf("ready target rejected: %v", err)
			}
			if tc.change != nil && err == nil {
				t.Fatal("drift was accepted")
			}
			wrong := biz.WithCaller(context.Background(), biz.Caller{TenantID: "33333333-3333-4333-8333-333333333333", Actor: "user"})
			if err := provider.VerifyDataPlaneTarget(wrong, s, target); !errors.Is(err, biz.ErrDenied) {
				t.Fatalf("cross-tenant: %v", err)
			}
		})
	}
}

func TestVerifyDataPlaneTargetRejectsNamespaceReplacementDuringPodRead(t *testing.T) {
	client, provider, s := configuredNamespaceFixture(t)
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	applyProvider(t, provider, &s, biz.MutationCreatePod)
	providerPodStatus(t, client, s, "Running", "True", true)
	s.Observed = biz.ObservedRunning
	target := biz.DataPlaneTarget{TenantID: s.TenantID, SandboxID: s.ID, Action: biz.DataPlaneFiles, Version: s.Version, ClusterID: s.Snapshot.ClusterID, Namespace: s.Snapshot.Namespace, NamespaceUID: s.Snapshot.NamespaceUID, PodName: s.ProviderRefs.PodName, PodUID: s.ProviderRefs.PodUID, Container: "sandbox"}
	client.PrependReactor("get", "pods", func(_ ktesting.Action) (bool, runtime.Object, error) {
		obj, err := client.Tracker().Get(namespaceGVR, "", s.Snapshot.Namespace)
		if err != nil {
			return true, nil, err
		}
		ns := obj.(*unstructured.Unstructured).DeepCopy()
		ns.SetUID(types.UID("replacement-namespace"))
		if err := client.Tracker().Update(namespaceGVR, ns, ""); err != nil {
			return true, nil, err
		}
		return false, nil, nil
	})
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: s.TenantID, Actor: "user"})
	if err := provider.VerifyDataPlaneTarget(ctx, s, target); !errors.Is(err, biz.ErrOwnership) {
		t.Fatalf("replaced namespace accepted: %v", err)
	}
}
