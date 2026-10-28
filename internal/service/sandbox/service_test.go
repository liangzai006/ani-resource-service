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

func (r serviceResolver) Resolve(context.Context, string, string, compute.Usage) (compute.Selection, error) {
	return r.selection, nil
}

func serviceContext() context.Context {
	return biz.WithCaller(context.Background(), biz.Caller{TenantID: testTenant, Actor: "governance:user:1"})
}

func newTestService(t *testing.T, selection compute.Selection) *Service {
	t.Helper()
	s, err := NewService(serviceRepo{}, serviceResolver{selection: selection}, Config{
		ClusterID: testCluster, Namespace: "sandbox", RuntimeClass: "sandbox-kata", StorageClass: "sandbox-rwo", WorkspaceSize: "10Gi", ImagePrefixes: []string{"registry/"},
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
	return &pb.ResolveCreateSpecRequest{TenantId: testTenant, InstanceId: "instance-owner", ContractVersion: 1, Data: &pb.CreateSandboxData{Name: "demo", ImageReference: "registry/demo@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PresetId: testPreset}}
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
	accept := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: testTenant, ResourceId: testSandbox, OperationId: testOp, RequestDigest: "request-1"}, Spec: resolved, InstanceId: "instance-owner"}
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
	s.config = Config{ClusterID: "66666666-6666-4666-8666-666666666666", Namespace: "new", RuntimeClass: "new", StorageClass: "new", WorkspaceSize: "20Gi", ImagePrefixes: []string{"new-registry/"}}
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
