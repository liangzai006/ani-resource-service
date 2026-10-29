package sandbox

import (
	"context"
	"errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	sandboxdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

const (
	testTenant  = "11111111-1111-4111-8111-111111111111"
	testCluster = "22222222-2222-4222-8222-222222222222"
	testPreset  = "33333333-3333-4333-8333-333333333333"
	testSandbox = "44444444-4444-4444-8444-444444444444"
	testOp      = "55555555-5555-4555-8555-555555555555"
)

type serviceRepo struct{}

func (serviceRepo) AcceptCreate(context.Context, biz.Create) (biz.ACK, error) {
	return biz.ACK{OperationID: testOp, ResourceID: testSandbox, Accepted: true}, nil
}
func (serviceRepo) Get(context.Context, string, string) (biz.Sandbox, error) {
	return biz.Sandbox{}, biz.ErrNotFound
}
func (serviceRepo) List(context.Context, string, biz.ListFilter) ([]biz.Sandbox, int64, error) {
	return nil, 0, nil
}
func (serviceRepo) GetOperation(context.Context, string, string) (biz.Operation, error) {
	return biz.Operation{}, biz.ErrNotFound
}
func (serviceRepo) Change(context.Context, biz.Command) (biz.ACK, error) {
	return biz.ACK{}, biz.ErrNotFound
}

type serviceResolver struct{ selection compute.Selection }

type testNamespaceResolver struct {
	uid                    string
	ensureCalls, readCalls int
	readErr                error
}

func (r *testNamespaceResolver) EnsureSandboxNamespaceUID(context.Context, string) (string, error) {
	r.ensureCalls++
	return r.uid, nil
}

func (r *testNamespaceResolver) ResolveSandboxNamespaceUID(context.Context, string) (string, error) {
	r.readCalls++
	return r.uid, r.readErr
}

func (r serviceResolver) Resolve(context.Context, string, string, compute.Usage) (compute.Selection, error) {
	return r.selection, nil
}

type testImageResolver struct {
	refs  map[string]string
	err   error
	calls int
}

func (r *testImageResolver) ResolveSandboxImage(_ context.Context, tenant, requested string) (string, error) {
	r.calls++
	if tenant != testTenant {
		return "", biz.ErrDenied
	}
	if r.err != nil {
		return "", r.err
	}
	if ref := r.refs[requested]; ref != "" {
		return ref, nil
	}
	return "", biz.ErrDenied
}

type testStorageResolver struct {
	class string
	err   error
	calls int
}

type testRuntimeResolver struct {
	class string
	err   error
	calls int
	valid map[string]bool
}

func (r *testRuntimeResolver) ResolveSandboxRuntimeClass(context.Context) (string, error) {
	r.calls++
	return r.class, r.err
}

func (r *testRuntimeResolver) ValidateSandboxRuntimeClass(_ context.Context, class string) error {
	r.calls++
	if r.err != nil {
		return r.err
	}
	if !r.valid[class] {
		return biz.ErrDependencyUnavailable
	}
	return nil
}

func TestRuntimeClassFrozenRevalidatedAndReplayIndependent(t *testing.T) {
	s := newTestService(t, validSelection())
	runtime := &testRuntimeResolver{class: "kata", valid: map[string]bool{"kata": true}}
	s.config.RuntimeClassResolver = runtime
	s.config.RuntimeClass = "" // Production uses the live cluster resolver only.
	spec, err := s.ResolveCreateSpec(serviceContext(), validResolveRequest())
	if err != nil || spec.RuntimeClass != "kata" {
		t.Fatalf("resolution=%v err=%v", spec, err)
	}
	accept := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: testTenant, ResourceId: testSandbox, OperationId: testOp}, Spec: spec}
	accept.Context.RequestDigest, err = commandDigest(accept.Context, "", spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptCreate(serviceContext(), accept); err != nil {
		t.Fatalf("unchanged runtime rejected: %v", err)
	}
	runtime.class = "sandbox-kata"
	runtime.valid["sandbox-kata"] = true
	if _, err := s.AcceptCreate(serviceContext(), accept); err != nil {
		t.Fatalf("still valid frozen runtime rejected after preferred class appeared: %v", err)
	}
	delete(runtime.valid, "kata")
	if _, err := s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.Unavailable {
		t.Fatalf("removed frozen runtime accepted: %v", err)
	}
	runtime.err = biz.ErrDependencyUnavailable
	if _, err := s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.Unavailable {
		t.Fatalf("unavailable runtime accepted: %v", err)
	}
	repo := &frozenReplayRepo{value: biz.Sandbox{ID: testSandbox, CreateOperationID: testOp, CreateRequestDigest: accept.Context.RequestDigest, Snapshot: frozenSnapshot(specValue(spec))}}
	s.repo = repo
	count := runtime.calls
	namespaces := s.config.NamespaceResolver.(*testNamespaceResolver)
	ensureCount, readCount := namespaces.ensureCalls, namespaces.readCalls
	got, err := s.AcceptCreate(serviceContext(), accept)
	if err != nil || !got.GetAck().GetReplayed() || runtime.calls != count || repo.accepts != 0 || namespaces.ensureCalls != ensureCount || namespaces.readCalls != readCount {
		t.Fatalf("replay=%v err=%v calls=%d", got, err, runtime.calls)
	}
}

func (r *testStorageResolver) ResolveSandboxStorageClass(_ context.Context, tenant string) (string, error) {
	r.calls++
	if tenant != testTenant {
		return "", biz.ErrDenied
	}
	return r.class, r.err
}

func serviceContext() context.Context {
	return biz.WithCaller(context.Background(), biz.Caller{TenantID: testTenant, Actor: "governance:user:1"})
}

func newTestService(t *testing.T, selection compute.Selection) *Service {
	t.Helper()
	s, err := NewService(serviceRepo{}, serviceResolver{selection: selection}, Config{
		ClusterID: testCluster, Namespace: "sandbox", RuntimeClass: "sandbox-kata", StorageClass: "sandbox-rwo", WorkspaceSize: "10Gi", ImagePrefixes: []string{"registry/"}, NamespaceResolver: &testNamespaceResolver{uid: "namespace-uid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validSelection() compute.Selection {
	return compute.Selection{PresetID: testPreset, Code: "cpu-small", Name: "CPU Small", ComputeType: "cpu", CPUMillis: 500, MemoryMiB: 512}
}

func validResolveRequest() *pb.ResolveCreateSpecRequest {
	return &pb.ResolveCreateSpecRequest{TenantId: testTenant, ContractVersion: 1, Data: &pb.CreateSandboxData{Name: "demo", ImageReference: "registry/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PresetId: testPreset}}
}

func TestResolveAndAcceptFreezeCanonicalSpec(t *testing.T) {
	s := newTestService(t, validSelection())
	resolved, err := s.ResolveCreateSpec(serviceContext(), validResolveRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SelectionDigest == "" || resolved.SpecDigest == "" {
		t.Fatalf("missing frozen digests: %#v", resolved)
	}
	accept := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: testTenant, ResourceId: testSandbox, OperationId: testOp, RequestDigest: "request-1"}, Spec: resolved}
	accept.Context.RequestDigest, err = commandDigest(accept.Context, accept.InstanceId, accept.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcceptCreate(serviceContext(), accept); err != nil {
		t.Fatal(err)
	}
	accept.Spec.Namespace = "foreign"
	if _, err = s.AcceptCreate(serviceContext(), accept); err == nil {
		t.Fatal("modified namespace was accepted")
	}
}

func TestCreateBindsNamespaceUIDAndRejectsReplacement(t *testing.T) {
	s := newTestService(t, validSelection())
	namespaces := &testNamespaceResolver{uid: "original-uid"}
	s.config.NamespaceResolver = namespaces
	spec, err := s.ResolveCreateSpec(serviceContext(), validResolveRequest())
	if err != nil || spec.NamespaceUid != "original-uid" {
		t.Fatalf("resolution=%+v err=%v", spec, err)
	}
	if namespaces.ensureCalls != 1 || namespaces.readCalls != 0 {
		t.Fatalf("resolution must initialize once: %+v", namespaces)
	}
	accept := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: testTenant, ResourceId: testSandbox, OperationId: testOp}, Spec: spec}
	accept.Context.RequestDigest, err = commandDigest(accept.Context, "", spec)
	if err != nil {
		t.Fatal(err)
	}
	// Altering the UID without recomputing the digests is invalid.
	spec.NamespaceUid = "other-uid"
	if _, err = s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("tampered UID accepted: %v", err)
	}
	spec.NamespaceUid = "original-uid"
	namespaces.uid = "replacement-uid"
	if _, err = s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("replaced namespace accepted: %v", err)
	}
	namespaces.readErr = biz.ErrDependencyUnavailable
	if _, err = s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.Unavailable {
		t.Fatalf("missing namespace accepted: %v", err)
	}
	if namespaces.ensureCalls != 1 {
		t.Fatalf("accept must not recreate the namespace: %+v", namespaces)
	}
	namespaces.readErr = nil
	namespaces.uid = "original-uid"
	if _, err = s.AcceptCreate(serviceContext(), accept); err != nil {
		t.Fatalf("original namespace rejected: %v", err)
	}
}

func TestCreateInitializesNamespaceButAcceptDoesNotRecreateIt(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("create", "namespaces", func(action ktesting.Action) (bool, runtime.Object, error) {
		action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).SetUID(types.UID("created-uid"))
		return false, nil, nil
	})
	s := newTestService(t, validSelection())
	s.config.NamespacePrefix = "tenant-"
	s.config.NamespaceResolver = sandboxdata.TenantNamespace{Client: client, NamespacePrefix: "tenant-"}
	spec, err := s.ResolveCreateSpec(serviceContext(), validResolveRequest())
	if err != nil || spec.NamespaceUid != "created-uid" {
		t.Fatalf("resolve failed: %v", err)
	}
	accept := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: testTenant, ResourceId: testSandbox, OperationId: testOp}, Spec: spec}
	accept.Context.RequestDigest, err = commandDigest(accept.Context, "", spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, "", "tenant-"+testTenant); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.Unavailable {
		t.Fatalf("accept after namespace disappeared: %v", err)
	}
	creates := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("accept recreated the namespace: %d creates", creates)
	}
}

func TestResolveRejectsInvalidRequestsBeforeInitializingNamespace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Service, *pb.ResolveCreateSpecRequest)
	}{
		{"invalid name", func(_ *Service, r *pb.ResolveCreateSpecRequest) { r.Data.Name = " " }},
		{"invalid image", func(_ *Service, r *pb.ResolveCreateSpecRequest) { r.Data.ImageReference = "invalid" }},
		{"unregistered image", func(s *Service, _ *pb.ResolveCreateSpecRequest) {
			s.config.ImageResolver = &testImageResolver{err: biz.ErrDenied}
		}},
		{"unavailable runtime", func(s *Service, _ *pb.ResolveCreateSpecRequest) {
			s.config.RuntimeClassResolver = &testRuntimeResolver{err: biz.ErrDependencyUnavailable}
		}},
		{"invalid compute", func(s *Service, _ *pb.ResolveCreateSpecRequest) {
			s.compute = serviceResolver{selection: compute.Selection{}}
		}},
		{"invalid workspace", func(_ *Service, r *pb.ResolveCreateSpecRequest) { r.Data.WorkspaceSize = "-1Gi" }},
		{"foreign tenant", func(_ *Service, r *pb.ResolveCreateSpecRequest) { r.TenantId = "66666666-6666-4666-8666-666666666666" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestService(t, validSelection())
			namespaces := &testNamespaceResolver{uid: "namespace-uid"}
			s.config.NamespaceResolver = namespaces
			req := validResolveRequest()
			tc.mutate(s, req)
			if _, err := s.ResolveCreateSpec(serviceContext(), req); err == nil {
				t.Fatal("invalid request succeeded")
			}
			if namespaces.ensureCalls != 0 || namespaces.readCalls != 0 {
				t.Fatalf("invalid request touched namespace: %+v", namespaces)
			}
		})
	}
}

func TestTenantNamespaceAndVariableWorkspaceAreValidatedOnAccept(t *testing.T) {
	s := newTestService(t, validSelection())
	// The shared Resource prefix wins over the old fixed-namespace setting.
	s.config.NamespacePrefix = "tenant-"
	req := validResolveRequest()
	req.Data.WorkspaceSize = "25Gi"
	spec, err := s.ResolveCreateSpec(serviceContext(), req)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Namespace != "tenant-"+testTenant || spec.Workspace.Size != "25Gi" {
		t.Fatalf("unexpected namespace or size: %+v", spec)
	}
	accept := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: testTenant, ResourceId: testSandbox, OperationId: testOp}, Spec: spec}
	redigest := func() {
		accept.Spec.SpecDigest, err = frozenSpecDigest(accept.Spec)
		if err != nil {
			t.Fatal(err)
		}
		accept.Context.RequestDigest, err = commandDigest(accept.Context, "", accept.Spec)
		if err != nil {
			t.Fatal(err)
		}
	}
	redigest()
	if _, err = s.AcceptCreate(serviceContext(), accept); err != nil {
		t.Fatalf("accept valid non-default workspace: %v", err)
	}
	// Even a self-consistent digest cannot authorize another tenant's namespace.
	accept.Spec.Namespace = "tenant-66666666-6666-4666-8666-666666666666"
	redigest()
	if _, err = s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("foreign namespace accepted: %v", err)
	}
	accept.Spec.Namespace = "tenant-" + testTenant
	for _, size := range []string{"", "0", "-1Gi", "invalid"} {
		accept.Spec.Workspace.Size = size
		redigest()
		if _, err = s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.InvalidArgument {
			t.Errorf("accepted workspace size %q: %v", size, err)
		}
	}
}

func TestResolveWorkspaceSizesAndNamespacePrefix(t *testing.T) {
	s := newTestService(t, validSelection())
	s.config.NamespacePrefix = "tenant-"
	for _, tc := range []struct {
		size string
		want string
		ok   bool
	}{{"", "10Gi", true}, {"1Gi", "1Gi", true}, {"30Gi", "30Gi", true}, {"0", "", false}, {"-1Gi", "", false}, {"wrong", "", false}} {
		req := validResolveRequest()
		req.Data.WorkspaceSize = tc.size
		spec, err := s.ResolveCreateSpec(serviceContext(), req)
		if tc.ok && (err != nil || spec.Workspace.Size != tc.want || spec.Namespace != "tenant-"+testTenant) {
			t.Errorf("size %q: spec=%v err=%v", tc.size, spec, err)
		} else if !tc.ok && status.Code(err) != codes.InvalidArgument {
			t.Errorf("size %q: expected invalid argument, got %v", tc.size, err)
		}
	}
	cfg := s.config
	cfg.NamespacePrefix = "bad_prefix-"
	if _, err := NewService(serviceRepo{}, serviceResolver{selection: validSelection()}, cfg); !errors.Is(err, biz.ErrInvalid) {
		t.Fatalf("invalid prefix accepted: %v", err)
	}
}

func TestResolvedDependenciesAreFrozenAndReauthorizedOnAccept(t *testing.T) {
	s := newTestService(t, validSelection())
	ref := "registry/tenant/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	images := &testImageResolver{refs: map[string]string{"image-id": ref, ref: ref}}
	storage := &testStorageResolver{class: "platform-storage"}
	s.config.ImagePrefixes = nil
	s.config.ImageResolver = images
	s.config.StorageClassResolver = storage
	req := validResolveRequest()
	req.Data.ImageReference = "image-id"
	spec, err := s.ResolveCreateSpec(serviceContext(), req)
	if err != nil || spec.ImageReference != ref || spec.Workspace.StorageClass != storage.class {
		t.Fatalf("resolve: spec=%v err=%v", spec, err)
	}
	accept := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: testTenant, ResourceId: testSandbox, OperationId: testOp}, Spec: spec}
	accept.Context.RequestDigest, err = commandDigest(accept.Context, "", spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcceptCreate(serviceContext(), accept); err != nil {
		t.Fatalf("accept authorized frozen image and storage: %v", err)
	}
	if images.calls != 2 || storage.calls != 2 {
		t.Fatalf("dependencies were not reauthorized: image=%d storage=%d", images.calls, storage.calls)
	}
	images.err = biz.ErrDenied
	if _, err = s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("revoked image accepted: %v", err)
	}
	images.err = nil
	images.refs[ref] = "registry/tenant/other@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err = s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("changed immutable image accepted: %v", err)
	}
	images.refs[ref] = ref
	storage.class = "different-storage"
	if _, err = s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("changed storage class accepted: %v", err)
	}
	storage.err = errors.New("storage resolver unavailable")
	if _, err = s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.Unavailable {
		t.Fatalf("unavailable storage resolver accepted: %v", err)
	}
}

func TestUnavailableSandboxDependenciesFailClosed(t *testing.T) {
	s := newTestService(t, validSelection())
	s.config.ImagePrefixes = nil
	s.config.ImageResolver = nil
	s.config.StorageClass = ""
	if _, err := NewService(serviceRepo{}, serviceResolver{selection: validSelection()}, s.config); err != nil {
		t.Fatalf("registration must not require image or storage resolvers: %v", err)
	}
	if _, err := s.ResolveCreateSpec(serviceContext(), validResolveRequest()); status.Code(err) != codes.Unavailable {
		t.Fatalf("missing image resolver: %v", err)
	}
	ref := validResolveRequest().Data.ImageReference
	s.config.ImageResolver = &testImageResolver{refs: map[string]string{ref: ref}}
	s.config.StorageClassResolver = nil
	if _, err := s.ResolveCreateSpec(serviceContext(), validResolveRequest()); status.Code(err) != codes.Unavailable {
		t.Fatalf("missing storage resolver: %v", err)
	}
	// An unavailable configured resolver never falls back to the old prefixes.
	s.config.ImagePrefixes = []string{"registry/"}
	s.config.ImageResolver = &testImageResolver{err: errors.New("image unavailable")}
	if _, err := s.ResolveCreateSpec(serviceContext(), validResolveRequest()); status.Code(err) != codes.Unavailable {
		t.Fatalf("configured image resolver failure: %v", err)
	}
}

func TestResolveRejectsResolverPresetMismatch(t *testing.T) {
	s := newTestService(t, compute.Selection{PresetID: "66666666-6666-4666-8666-666666666666", Code: "cpu-small", Name: "CPU Small", ComputeType: "cpu", CPUMillis: 500, MemoryMiB: 512})
	if _, err := s.ResolveCreateSpec(serviceContext(), validResolveRequest()); err == nil {
		t.Fatal("resolver result for a different preset was accepted")
	}
}

func TestResolveRejectsGPUSelectionForPhaseOne(t *testing.T) {
	s := newTestService(t, compute.Selection{PresetID: testPreset, Code: "gpu", Name: "GPU", ComputeType: "gpu", CPUMillis: 500, MemoryMiB: 512, GPU: &compute.GPUReference{ProfileID: testPreset}})
	if _, err := s.ResolveCreateSpec(serviceContext(), validResolveRequest()); err == nil {
		t.Fatal("GPU selection was accepted by the CPU-only一期 contract")
	}
}

type frozenReplayRepo struct {
	serviceRepo
	value   biz.Sandbox
	accepts int
}

func (r *frozenReplayRepo) Get(context.Context, string, string) (biz.Sandbox, error) {
	return r.value, nil
}
func (r *frozenReplayRepo) AcceptCreate(context.Context, biz.Create) (biz.ACK, error) {
	r.accepts++
	return biz.ACK{}, nil
}
func TestAcceptReplaysFrozenCommandBeforeCurrentConfiguration(t *testing.T) {
	s := newTestService(t, validSelection())
	req := validResolveRequest()
	spec, err := s.ResolveCreateSpec(serviceContext(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.InstanceId = "instance-owner" // historical accepted command
	intent, err := pb.CreateIntentDigest(req.InstanceId, req.Data)
	if err != nil {
		t.Fatal(err)
	}
	accept := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: testTenant, ResourceId: testSandbox, OperationId: testOp}, Spec: spec, InstanceId: req.InstanceId, CreateIntentDigest: intent}
	accept.Context.RequestDigest, err = pb.CommandDigest(accept.Context, accept.InstanceId, spec, intent)
	if err != nil {
		t.Fatal(err)
	}
	repo := &frozenReplayRepo{value: biz.Sandbox{ID: testSandbox, InstanceID: req.InstanceId, CreateOperationID: testOp, CreateRequestDigest: accept.Context.RequestDigest, CreateIntentDigest: intent, Snapshot: frozenSnapshot(specValue(spec))}}
	s.repo = repo
	s.config = Config{ClusterID: "66666666-6666-4666-8666-666666666666", NamespacePrefix: "tenant-", RuntimeClass: "new", StorageClass: "new", WorkspaceSize: "20Gi", ImageResolver: &testImageResolver{err: errors.New("unavailable")}, StorageClassResolver: &testStorageResolver{err: errors.New("unavailable")}}
	got, err := s.AcceptCreate(serviceContext(), accept)
	if err != nil || !got.GetAck().GetReplayed() || repo.accepts != 0 {
		t.Fatalf("ack=%v err=%v writes=%d", got, err, repo.accepts)
	}
	// A changed command must still conflict under the same stable resource ID.
	accept.Spec.Name = "different"
	accept.Spec.SpecDigest, _ = frozenSpecDigest(accept.Spec)
	accept.Context.RequestDigest, _ = pb.CommandDigest(accept.Context, accept.InstanceId, accept.Spec, intent)
	if _, err = s.AcceptCreate(serviceContext(), accept); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("changed command=%v", err)
	}
}

type failingResolver struct{ err error }

func (r failingResolver) Resolve(context.Context, string, string, compute.Usage) (compute.Selection, error) {
	return compute.Selection{}, r.err
}
func TestResolvePresetBusinessErrorsHaveStableStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code codes.Code
	}{{compute.ErrNotFound, codes.NotFound}, {compute.ErrPresetDisabled, codes.FailedPrecondition}, {errors.New("connection refused"), codes.Unavailable}} {
		t.Run(tc.code.String(), func(t *testing.T) {
			s := newTestService(t, validSelection())
			s.compute = failingResolver{err: tc.err}
			_, err := s.ResolveCreateSpec(serviceContext(), validResolveRequest())
			if status.Code(err) != tc.code {
				t.Fatalf("got=%v want=%v", err, tc.code)
			}
		})
	}
}

type independentRepo struct {
	serviceRepo
	sandbox biz.Sandbox
	command biz.Command
	filter  biz.ListFilter
}

func (r *independentRepo) Get(_ context.Context, _, _ string) (biz.Sandbox, error) {
	return r.sandbox, nil
}
func (r *independentRepo) GetOperation(_ context.Context, _, _ string) (biz.Operation, error) {
	return biz.Operation{ID: testOp, SandboxID: testSandbox, InstanceID: "historical-parent"}, nil
}
func (r *independentRepo) List(_ context.Context, _ string, f biz.ListFilter) ([]biz.Sandbox, int64, error) {
	r.filter = f
	return []biz.Sandbox{r.sandbox}, 1, nil
}
func (r *independentRepo) Change(_ context.Context, c biz.Command) (biz.ACK, error) {
	r.command = c
	return biz.ACK{ResourceID: c.ID, OperationID: testOp, Accepted: true}, nil
}

func TestIndependentSandboxWithoutParentAndHistoricalRows(t *testing.T) {
	s := newTestService(t, validSelection())
	request := validResolveRequest()
	request.InstanceId = ""
	spec, err := s.ResolveCreateSpec(serviceContext(), request)
	if err != nil {
		t.Fatal("resolve without parent:", err)
	}
	intent, err := pb.CreateIntentDigest("", request.Data)
	if err != nil {
		t.Fatal("intent without parent:", err)
	}
	accept := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: testTenant, ResourceId: testSandbox, OperationId: testOp}, Spec: spec, CreateIntentDigest: intent}
	accept.Context.RequestDigest, err = pb.CommandDigest(accept.Context, "", spec, intent)
	if err != nil {
		t.Fatal("digest without parent:", err)
	}
	if _, err = s.AcceptCreate(serviceContext(), accept); err != nil {
		t.Fatal("accept without parent:", err)
	}
	legacyNew := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: testTenant, ResourceId: testSandbox, OperationId: testOp}, Spec: spec, InstanceId: "not-a-parent", CreateIntentDigest: intent}
	legacyNew.Context.RequestDigest, _ = pb.CommandDigest(legacyNew.Context, legacyNew.InstanceId, spec, intent)
	if _, err = s.AcceptCreate(serviceContext(), legacyNew); status.Code(err) != codes.InvalidArgument {
		t.Fatal("new create with historical parent field:", err)
	}

	// A pre-upgrade row retains its old instance_id; access is now keyed by the
	// tenant and Sandbox ID, and does not require clients to know that value.
	repo := &independentRepo{sandbox: biz.Sandbox{ID: testSandbox, TenantID: testTenant, InstanceID: "historical-parent"}}
	s.repo = repo
	if _, err = s.GetSandbox(serviceContext(), &pb.GetSandboxRequest{TenantId: testTenant, SandboxId: testSandbox}); err != nil {
		t.Fatal("get historical row:", err)
	}
	if _, err = s.GetOperation(serviceContext(), &pb.GetOperationRequest{TenantId: testTenant, OperationId: testOp}); err != nil {
		t.Fatal("get historical operation:", err)
	}
	if _, err = s.ListSandboxes(serviceContext(), &pb.ListSandboxesRequest{TenantId: testTenant}); err != nil || repo.filter.InstanceID != "" {
		t.Fatal("list without parent:", repo.filter, err)
	}
	if _, err = s.ListSandboxes(serviceContext(), &pb.ListSandboxesRequest{TenantId: testTenant, InstanceId: "historical-parent"}); status.Code(err) != codes.InvalidArgument {
		t.Fatal("legacy filtered list must not return all tenant resources:", err)
	}
	if _, err = s.Delete(serviceContext(), &pb.DeleteRequest{TenantId: testTenant, SandboxId: testSandbox, IdempotencyKey: "delete", ExpectedVersion: 1}); err != nil || repo.command.InstanceID != "" {
		t.Fatal("delete without parent:", repo.command, err)
	}
	if _, err = s.GetSandbox(serviceContext(), &pb.GetSandboxRequest{TenantId: "66666666-6666-4666-8666-666666666666", SandboxId: testSandbox}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("cross-tenant access:", err)
	}
}
