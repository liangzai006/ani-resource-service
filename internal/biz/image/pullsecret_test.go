package biz

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

const pullSecretTestTenant = "11111111-1111-4111-8111-111111111111"

// pullSecretFakes wires the Runtime to in-memory fakes that exercise the real
// Ensure use case: space lookup, credential decryption, namespace authority and
// the single writer.
type pullSecretFakes struct {
	space     Space
	stored    StoredCredential
	names     []NamespaceClaim
	writes    []PullSecretTarget
	now       time.Time
	regressed bool
}

func newPullSecretRuntime(t *testing.T, f *pullSecretFakes) *Runtime {
	t.Helper()
	if f.now.IsZero() {
		f.now = time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	}
	if f.space.ID == "" {
		f.space = Space{ID: "space-a", TenantID: pullSecretTestTenant, InstallationID: "install", ProjectName: "tenant-a", RegistryAuthority: "Registry.Example.Test:443", State: "available", Scope: TenantImages, ProjectID: 7}
	}
	if f.stored.Info.Generation == 0 {
		expires := f.now.Add(24 * time.Hour)
		f.stored = StoredCredential{
			Info:   CredentialInfo{TenantID: pullSecretTestTenant, SpaceID: "space-a", Purpose: "pull", State: "active", Username: "robot$pull-a", Scope: TenantImages, RobotID: 3, Generation: 4, ExpiresAt: &expires},
			Secret: EncryptedSecret{Ciphertext: []byte("ciphertext")},
			AAD:    SecretAAD{InstallationID: "install", TenantID: pullSecretTestTenant, SpaceID: "space-a", Purpose: "pull", Scope: TenantImages, Generation: 4},
		}
	}
	runtime, err := NewRuntime(pullSecretCatalog{f: f}, pullSecretSpaces{f: f}, pullSecretCredentials{f: f}, pullSecretRegistry{}, pullSecretCipher{}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.UsePullSecretWriter(pullSecretWriter{f: f}, pullSecretNamespaces{f: f}, []PullSecretCaller{
		{Subject: "ani-modeldev-service", ClusterIDs: []string{"cluster-a"}, Namespaces: []string{"tenant-training"}, OwnerLabels: []string{"ani-modeldev-service"}, RequireTenantLabel: true},
		{Subject: "ani-inference-service", ClusterIDs: []string{"cluster-a"}, Namespaces: []string{"ani-inference"}},
	}); err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestPullSecretNameRule(t *testing.T) {
	base, err := PullSecretName(pullSecretTestTenant, "cluster-a", "tenant-ns", "Registry.Example.Test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(base, "ani-pull-") || len(base) != len("ani-pull-")+32 {
		t.Fatalf("unexpected name: %s", base)
	}
	if strings.ToLower(base) != base {
		t.Fatalf("name must be lowercase: %s", base)
	}
	// Normalization: scheme and path must not change the name.
	for _, variant := range []string{"registry.example.test", "https://registry.example.test/"} {
		got, err := PullSecretName(pullSecretTestTenant, "CLUSTER-A", "tenant-ns", variant)
		if err != nil || got != base {
			t.Fatalf("normalization changed the name: %s %v != %s", got, err, base)
		}
	}
	// An explicit default port is normalized away; a non-default port is not.
	if got, err := PullSecretName(pullSecretTestTenant, "cluster-a", "tenant-ns", "registry.example.test:443"); err != nil || got != base {
		t.Fatalf("default port authority should normalize: %s %v", got, err)
	}
	if got, err := PullSecretName(pullSecretTestTenant, "cluster-a", "tenant-ns", "registry.example.test:8443"); err != nil || got == base {
		t.Fatalf("non-default port must change the name: %s %v", got, err)
	}
	// Every dimension must change the name, including the cluster.
	for _, mutate := range []func() (string, error){
		func() (string, error) {
			return PullSecretName("22222222-2222-4222-8222-222222222222", "cluster-a", "tenant-ns", "registry.example.test")
		},
		func() (string, error) {
			return PullSecretName(pullSecretTestTenant, "cluster-b", "tenant-ns", "registry.example.test")
		},
		func() (string, error) {
			return PullSecretName(pullSecretTestTenant, "cluster-a", "other-ns", "registry.example.test")
		},
		func() (string, error) {
			return PullSecretName(pullSecretTestTenant, "cluster-a", "tenant-ns", "registry.other.test")
		},
	} {
		got, err := mutate()
		if err != nil || got == base {
			t.Fatalf("scope dimension did not change the name: %s %v", got, err)
		}
	}
	if _, err := PullSecretName(pullSecretTestTenant, "", "tenant-ns", "registry.example.test"); err == nil {
		t.Fatal("missing cluster accepted")
	}
}

func TestEnsureTenantPullSecretWritesOnceAndIsIdempotent(t *testing.T) {
	f := &pullSecretFakes{}
	runtime := newPullSecretRuntime(t, f)
	ctx := WithCaller(context.Background(), Caller{Kind: RuntimeCaller, Subject: "ani-modeldev-service", TenantID: pullSecretTestTenant})
	in := EnsurePullSecret{PullSecretScope: PullSecretScope{TenantID: pullSecretTestTenant, ClusterID: "cluster-a", Namespace: "tenant-training", RegistryAuthority: "registry.example.test:443"}, NamespaceUID: "ns-uid"}

	first, err := runtime.EnsureTenantPullSecret(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runtime.EnsureTenantPullSecret(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.SecretRef != second.SecretRef || first.Generation != 4 || second.Generation != 4 {
		t.Fatalf("ensure is not idempotent: %+v %+v", first, second)
	}
	if len(f.writes) != 2 || f.writes[0].SecretName != f.writes[1].SecretName {
		t.Fatalf("retry changed the derived name: %+v", f.writes)
	}
	if f.writes[0].RegistryAuthority != "Registry.Example.Test:443" {
		t.Fatalf("writer must receive the authoritative registry: %+v", f.writes[0])
	}
	if first.RegistryAuthority != "Registry.Example.Test:443" || first.SpaceID != "space-a" {
		t.Fatalf("receipt lost authoritative facts: %+v", first)
	}
	if len(f.names) != 2 || !f.names[0].Verify || !f.names[0].RequireTenantLabel || f.names[0].NamespaceUID != "ns-uid" {
		t.Fatalf("namespace authority not consulted correctly: %+v", f.names)
	}
}

func TestEnsureTenantPullSecretRejections(t *testing.T) {
	cases := []struct {
		name   string
		caller Caller
		mutate func(*EnsurePullSecret)
		want   Reason
	}{
		{"unknown identity", Caller{Kind: RuntimeCaller, Subject: "ani-stranger", TenantID: pullSecretTestTenant}, func(*EnsurePullSecret) {}, PermissionDenied},
		{"namespace outside allowlist", Caller{Kind: RuntimeCaller, Subject: "ani-modeldev-service", TenantID: pullSecretTestTenant}, func(in *EnsurePullSecret) { in.Namespace = "other-ns" }, PermissionDenied},
		{"cluster outside allowlist", Caller{Kind: RuntimeCaller, Subject: "ani-modeldev-service", TenantID: pullSecretTestTenant}, func(in *EnsurePullSecret) { in.ClusterID = "cluster-b" }, PermissionDenied},
		{"registry mismatch", Caller{Kind: RuntimeCaller, Subject: "ani-modeldev-service", TenantID: pullSecretTestTenant}, func(in *EnsurePullSecret) { in.RegistryAuthority = "registry.other.test" }, SpaceNotReady},
		{"space mismatch", Caller{Kind: RuntimeCaller, Subject: "ani-modeldev-service", TenantID: pullSecretTestTenant}, func(in *EnsurePullSecret) { in.SpaceID = "space-b" }, PermissionDenied},
		{"cross tenant", Caller{Kind: RuntimeCaller, Subject: "ani-modeldev-service", TenantID: "22222222-2222-4222-8222-222222222222"}, func(*EnsurePullSecret) {}, PermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &pullSecretFakes{}
			runtime := newPullSecretRuntime(t, f)
			in := EnsurePullSecret{PullSecretScope: PullSecretScope{TenantID: pullSecretTestTenant, ClusterID: "cluster-a", Namespace: "tenant-training", RegistryAuthority: "registry.example.test:443"}, NamespaceUID: "ns-uid"}
			tc.mutate(&in)
			_, err := runtime.EnsureTenantPullSecret(WithCaller(context.Background(), tc.caller), in)
			if ReasonOf(err) != tc.want {
				t.Fatalf("reason=%v want=%v", ReasonOf(err), tc.want)
			}
			if len(f.writes) != 0 {
				t.Fatal("rejected request reached the writer")
			}
		})
	}
}

func TestEnsureTenantPullSecretRejectsInactiveCredential(t *testing.T) {
	f := &pullSecretFakes{}
	runtime := newPullSecretRuntime(t, f)
	f.stored.Info.State = "disabled"
	ctx := WithCaller(context.Background(), Caller{Kind: RuntimeCaller, Subject: "ani-inference-service", TenantID: pullSecretTestTenant})
	if _, err := runtime.EnsureTenantPullSecret(ctx, EnsurePullSecret{PullSecretScope: PullSecretScope{TenantID: pullSecretTestTenant, ClusterID: "cluster-a", Namespace: "ani-inference", RegistryAuthority: "registry.example.test:443"}, NamespaceUID: "ns-uid"}); ReasonOf(err) != SpaceNotReady {
		t.Fatalf("inactive credential accepted: %v", err)
	}
	if len(f.writes) != 0 {
		t.Fatal("inactive credential reached the writer")
	}
}

func TestEnsureTenantPullSecretGenerationRegressionIsRejected(t *testing.T) {
	f := &pullSecretFakes{}
	runtime := newPullSecretRuntime(t, f)
	f.regressed = true
	ctx := WithCaller(context.Background(), Caller{Kind: RuntimeCaller, Subject: "ani-inference-service", TenantID: pullSecretTestTenant})
	if _, err := runtime.EnsureTenantPullSecret(ctx, EnsurePullSecret{PullSecretScope: PullSecretScope{TenantID: pullSecretTestTenant, ClusterID: "cluster-a", Namespace: "ani-inference", RegistryAuthority: "registry.example.test:443"}, NamespaceUID: "ns-uid"}); ReasonOf(err) != InternalError {
		t.Fatalf("generation regression accepted: %v", err)
	}
}

func TestEnsureTenantPullSecretConcurrentScopes(t *testing.T) {
	f := &pullSecretFakes{}
	runtime := newPullSecretRuntime(t, f)
	ctx := WithCaller(context.Background(), Caller{Kind: RuntimeCaller, Subject: "ani-inference-service", TenantID: pullSecretTestTenant})
	var wg sync.WaitGroup
	names := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := runtime.EnsureTenantPullSecret(ctx, EnsurePullSecret{PullSecretScope: PullSecretScope{TenantID: pullSecretTestTenant, ClusterID: "cluster-a", Namespace: "ani-inference", RegistryAuthority: "registry.example.test:443"}, NamespaceUID: "ns-uid"})
			if err != nil {
				t.Error(err)
				return
			}
			names <- out.SecretRef
		}()
	}
	wg.Wait()
	close(names)
	seen := map[string]bool{}
	for name := range names {
		seen[name] = true
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent ensure derived multiple names: %v", seen)
	}
}

// --- fakes ---

type pullSecretCatalog struct{ f *pullSecretFakes }

func (pullSecretCatalog) FindTenantRegistration(context.Context, string, string) (Registration, error) {
	return Registration{}, Fail(ImageNotFound, "not used")
}
func (pullSecretCatalog) FindPlatformRegistration(context.Context, string) (Registration, error) {
	return Registration{}, Fail(ImageNotFound, "not used")
}
func (pullSecretCatalog) PageTenantRegistrations(context.Context, string, Filter, *PageKey) ([]Registration, int64, error) {
	return nil, 0, nil
}
func (pullSecretCatalog) PagePlatformRegistrations(context.Context, Filter, *PageKey) ([]Registration, int64, error) {
	return nil, 0, nil
}
func (pullSecretCatalog) FindTenantRegistrationsByReference(context.Context, string, ReferenceLookup) ([]Registration, error) {
	return nil, Fail(ImageNotFound, "not used")
}
func (pullSecretCatalog) FindPlatformRegistrationsByReference(context.Context, ReferenceLookup) ([]Registration, error) {
	return nil, Fail(ImageNotFound, "not used")
}
func (pullSecretCatalog) ApplyTenantRegistration(context.Context, Command, Registration) (Registration, error) {
	return Registration{}, Fail(InternalError, "not used")
}
func (pullSecretCatalog) ApplyTenantMetadata(context.Context, Command, UpdateImage) (Registration, error) {
	return Registration{}, Fail(InternalError, "not used")
}
func (pullSecretCatalog) ApplyTenantUnregister(context.Context, Command, UnregisterImage) (Registration, error) {
	return Registration{}, Fail(InternalError, "not used")
}

type pullSecretSpaces struct{ f *pullSecretFakes }

func (s pullSecretSpaces) FindTenantSpace(context.Context, string) (Space, error) {
	return s.f.space, nil
}
func (pullSecretSpaces) FindPlatformSpace(context.Context) (Space, error) {
	return Space{}, Fail(SpaceNotFound, "not used")
}
func (pullSecretSpaces) ReserveTenantSpace(context.Context, Space, Command) (Space, Command, error) {
	return Space{}, Command{}, Fail(InternalError, "not used")
}

type pullSecretCredentials struct{ f *pullSecretFakes }

func (pullSecretCredentials) GetTenantPublisher(context.Context, string, string) (CredentialInfo, error) {
	return CredentialInfo{}, Fail(CredentialNotIssued, "not used")
}
func (c pullSecretCredentials) GetTenantPull(context.Context, string, string) (StoredCredential, error) {
	return c.f.stored, nil
}

type pullSecretCipher struct{}

// pullSecretRegistry is unused by Ensure; it exists only because the Runtime
// constructor requires the registry port.
type pullSecretRegistry struct{}

func (pullSecretRegistry) GetProjectByID(context.Context, int64) (Project, error) {
	return Project{}, Fail(InternalError, "not used")
}
func (pullSecretRegistry) FindProjectByName(context.Context, string) (Project, error) {
	return Project{}, Fail(InternalError, "not used")
}
func (pullSecretRegistry) CreatePrivateProject(context.Context, string) (Project, error) {
	return Project{}, Fail(InternalError, "not used")
}
func (pullSecretRegistry) CreateRobot(context.Context, RobotRequest) (Robot, error) {
	return Robot{}, Fail(InternalError, "not used")
}
func (pullSecretRegistry) FindOwnedRobot(context.Context, RobotRequest) (Robot, error) {
	return Robot{}, Fail(InternalError, "not used")
}
func (pullSecretRegistry) GetRobot(context.Context, int64) (Robot, error) {
	return Robot{}, Fail(InternalError, "not used")
}
func (pullSecretRegistry) SetRobotSecret(context.Context, Robot, Secret) error {
	return Fail(InternalError, "not used")
}
func (pullSecretRegistry) SetRobotDisabled(context.Context, Robot, bool) error {
	return Fail(InternalError, "not used")
}
func (pullSecretRegistry) ResolveArtifact(context.Context, string, string, string) (Artifact, error) {
	return Artifact{}, Fail(InternalError, "not used")
}
func (pullSecretRegistry) GetArtifactByDigest(context.Context, string, string, string) (Artifact, error) {
	return Artifact{}, Fail(InternalError, "not used")
}

func (pullSecretCipher) Seal(context.Context, SecretAAD, Secret) (EncryptedSecret, error) {
	return EncryptedSecret{}, nil
}
func (pullSecretCipher) Open(context.Context, SecretAAD, EncryptedSecret) (Secret, error) {
	return Secret("decrypted-pull-secret"), nil
}

type pullSecretNamespaces struct{ f *pullSecretFakes }

func (n pullSecretNamespaces) VerifyNamespace(_ context.Context, claim NamespaceClaim) error {
	n.f.names = append(n.f.names, claim)
	return nil
}

type pullSecretWriter struct{ f *pullSecretFakes }

func (w pullSecretWriter) EnsurePullSecret(_ context.Context, target PullSecretTarget, secret Secret) (int64, error) {
	w.f.writes = append(w.f.writes, target)
	if string(secret) != "decrypted-pull-secret" {
		return 0, Fail(InternalError, "writer received unexpected material")
	}
	if w.f.regressed {
		return target.Generation - 1, nil
	}
	return target.Generation, nil
}
