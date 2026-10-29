package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type testPullMaterial struct {
	tenant, space, authority, username string
	secret                             []byte
	last                               []byte
	generation                         int64
}

type unavailablePullMaterial struct{}

func (unavailablePullMaterial) GetTenantPullMaterial(context.Context, string) (imagebiz.PullMaterial, error) {
	return imagebiz.PullMaterial{}, errors.New("image runtime unavailable")
}

func (m *testPullMaterial) GetTenantPullMaterial(ctx context.Context, tenant string) (imagebiz.PullMaterial, error) {
	if _, err := imagebiz.RequireRuntime(ctx, tenant); err != nil {
		return imagebiz.PullMaterial{}, err
	}
	m.last = append([]byte(nil), m.secret...)
	return imagebiz.PullMaterial{TenantID: m.tenant, SpaceID: m.space, RegistryAuthority: m.authority, Username: m.username, Secret: m.last, Generation: m.generation}, nil
}

func testPullSource(s biz.Sandbox) *testPullMaterial {
	return &testPullMaterial{tenant: s.TenantID, space: "space-id", authority: "registry.example", username: "robot$tenant", secret: []byte("a-unique-test-password"), generation: 1}
}

func TestSandboxPullSecretCreationRotationAndPodBinding(t *testing.T) {
	client, provider, s := providerFixture(t)
	material := testPullSource(s)
	if err := provider.ConfigureImages(material); err != nil {
		t.Fatal(err)
	}
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	applyProvider(t, provider, &s, biz.MutationCreatePod)
	for _, b := range material.last {
		if b != 0 {
			t.Fatal("plaintext returned by Image was not cleared")
		}
	}
	secret, err := client.Resource(secretGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), pullSecretName(s), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, found, err := unstructured.NestedString(secret.Object, "data", ".dockerconfigjson")
	if err != nil || !found {
		t.Fatal("missing docker credentials")
	}
	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(body)
	var cfg struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err = json.Unmarshal(body, &cfg); err != nil || cfg.Auths[material.authority].Username != material.username || cfg.Auths[material.authority].Password != string(material.secret) {
		t.Fatal("pull Secret did not contain the tenant's registry credentials")
	}
	pod, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), "sandbox-"+s.ID, metav1.GetOptions{})
	if err != nil || !provider.matchesPod(pod, s) {
		t.Fatal("created Pod did not bind the owned pull Secret", err)
	}
	if err = unstructured.SetNestedSlice(pod.Object, []any{map[string]any{"name": "another-secret"}}, "spec", "imagePullSecrets"); err != nil {
		t.Fatal(err)
	}
	if provider.matchesPod(pod, s) {
		t.Fatal("Pod with a different pull Secret was accepted")
	}
	material.generation = 2
	material.secret = []byte("rotated-test-password")
	if err = provider.ensurePullSecret(context.Background(), s); err != nil {
		t.Fatalf("rotate pull credential: %v", err)
	}
	secret, err = client.Resource(secretGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), pullSecretName(s), metav1.GetOptions{})
	if err != nil || secret.GetAnnotations()["image.ani.io/credential-generation"] != "2" {
		t.Fatal("rotated generation was not stored", err)
	}
	material.secret = []byte("different-password-same-generation")
	if err = provider.ensurePullSecret(context.Background(), s); !errors.Is(err, biz.ErrOwnership) {
		t.Fatalf("changed credentials at the same generation were accepted: %v", err)
	}
	material.generation = 1
	if err = provider.ensurePullSecret(context.Background(), s); !errors.Is(err, biz.ErrOwnership) {
		t.Fatalf("stale credential replaced a newer generation: %v", err)
	}
}

func TestSandboxPullSecretRejectsForeignIdentityAndLegacyUnknownPod(t *testing.T) {
	client, provider, s := providerFixture(t)
	material := testPullSource(s)
	if err := provider.ConfigureImages(material); err != nil {
		t.Fatal(err)
	}
	foreign := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": pullSecretName(s), "namespace": s.Snapshot.Namespace, "labels": map[string]any{"image.ani.io/tenant-id": "foreign"}}}}
	if _, err := client.Resource(secretGVR).Namespace(s.Snapshot.Namespace).Create(context.Background(), foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := provider.ensurePullSecret(context.Background(), s); !errors.Is(err, biz.ErrOwnership) {
		t.Fatalf("foreign Secret was accepted: %v", err)
	}
	legacy := renderPod(s, "sandbox-"+s.ID, labels(s))
	legacy.SetUID(types.UID("old-pod-uid"))
	if provider.matchesPod(legacy, s) {
		t.Fatal("unknown legacy Pod with no recorded UID was adopted")
	}
	s.ProviderRefs.PodUID = "old-pod-uid"
	if !provider.matchesPod(legacy, s) {
		t.Fatal("recorded historical Pod identity was rejected")
	}
}

func TestSandboxUnavailableImageNeverCreatesUncredentialedPod(t *testing.T) {
	client, provider, s := providerFixture(t)
	if err := provider.ConfigureImages(unavailablePullMaterial{}); err != nil {
		t.Fatal(err)
	}
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	result, err := provider.Apply(context.Background(), s, providerMutation(s, biz.MutationCreatePod))
	if err == nil || result.Outcome != biz.MutationNotSent || result.Reason != "IMAGE_PULL_SECRET_FAILED" {
		t.Fatalf("unavailable Image did not fence Pod creation: result=%#v err=%v", result, err)
	}
	if _, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Get(context.Background(), "sandbox-"+s.ID, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("uncredentialed Pod was created: %v", err)
	}
}

func TestSandboxUnknownLegacyPodRemainsFencedWithUnavailableImage(t *testing.T) {
	client, provider, s := providerFixture(t)
	if err := provider.ConfigureImages(unavailablePullMaterial{}); err != nil {
		t.Fatal(err)
	}
	applyProvider(t, provider, &s, biz.MutationCreateWorkspace)
	legacy := renderPod(s, "sandbox-"+s.ID, labels(s))
	legacy.SetUID(types.UID("unknown-pod-uid"))
	if _, err := client.Resource(podGVR).Namespace(s.Snapshot.Namespace).Create(context.Background(), legacy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Observe(context.Background(), s); !errors.Is(err, biz.ErrOwnership) {
		t.Fatalf("unknown uncredentialed Pod was observed as owned: %v", err)
	}
	result, err := provider.Apply(context.Background(), s, providerMutation(s, biz.MutationCreatePod))
	if !errors.Is(err, biz.ErrOwnership) || result.Outcome != biz.MutationNotSent {
		t.Fatalf("unknown uncredentialed Pod was adopted: result=%#v err=%v", result, err)
	}
	// Once the original UID is persisted, the old Pod can be observed and
	// deleted without requiring Image service availability.
	s.ProviderRefs.PodUID = "unknown-pod-uid"
	if _, err = provider.Observe(context.Background(), s); err != nil {
		t.Fatalf("recorded historical Pod could not be observed: %v", err)
	}
}
