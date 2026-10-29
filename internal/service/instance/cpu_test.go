package instance

import (
	"context"
	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

// Only external persistence/image/environment boundaries are replaced here.
// The same intake is exercised against real PostgreSQL in instanceintegration.
type cpuIntake struct {
	biz.Repository
	compute.PresetRepository
	preset  compute.Preset
	creates []biz.Create
	deletes []biz.Delete
}

func (r *cpuIntake) AcceptCreate(_ context.Context, c biz.Create) (biz.ACK, error) {
	if e := biz.ValidateCreate(c); e != nil {
		return biz.ACK{}, e
	}
	r.creates = append(r.creates, c)
	return biz.ACK{OperationID: c.OperationID, ResourceID: c.ID, Accepted: true}, nil
}
func (r *cpuIntake) AcceptDelete(_ context.Context, d biz.Delete) (biz.ACK, error) {
	if e := biz.ValidateDelete(d); e != nil {
		return biz.ACK{}, e
	}
	r.deletes = append(r.deletes, d)
	return biz.ACK{OperationID: d.OperationID, ResourceID: d.ID, Accepted: true}, nil
}
func (r *cpuIntake) GetPreset(_ context.Context, id string) (compute.Preset, error) {
	if id != r.preset.ID {
		return compute.Preset{}, compute.ErrNotFound
	}
	return r.preset, nil
}
func (r *cpuIntake) ListPresets(context.Context, compute.PresetFilter) ([]compute.Preset, int64, bool, error) {
	return []compute.Preset{r.preset}, 1, false, nil
}

type cpuImageBoundary struct{ calls int }

func (r *cpuImageBoundary) Resolve(context.Context, string, string, string) (biz.ResolvedImage, error) {
	r.calls++
	d := "sha256:" + strings.Repeat("a", 64)
	return biz.ResolvedImage{Reference: "registry.invalid/work@" + d, Digest: d}, nil
}

type cpuEnvironmentBoundary struct{ calls int }

func (r *cpuEnvironmentBoundary) ValidateEnvironment(context.Context, string, string, string, string) error {
	r.calls++
	return nil
}
func testGPU() *acc.GpuExecution {
	g := &acc.GpuExecution{SchedulerName: "volcano", QueueName: "default", RuntimeClassName: "nvidia", NodeSelector: map[string]string{"accelerator.ani.io/supply-group": "group", "accelerator.ani.io/model-key": "t4", "accelerator.ani.io/baseline-id": "config"}, PodAnnotations: map[string]string{"volcano.sh/vgpu-mode": "hami-core"}, Resources: map[string]string{"volcano.sh/vgpu-number": "1", "volcano.sh/vgpu-memory": "8192", "volcano.sh/vgpu-cores": "25"}}
	g.ExecutionDigest, _ = pb.ExecutionDigest(g)
	return g
}
func TestTrustedCPUAndGPUIntake(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const resource = "22222222-2222-4222-8222-222222222222"
	const operation = "33333333-3333-4333-8333-333333333333"
	for _, kind := range []string{biz.TypeCPU, biz.TypeGPU} {
		t.Run(kind, func(t *testing.T) {
			repo := &cpuIntake{preset: compute.Preset{ID: resource, Code: "small", Name: "small", ComputeType: kind, CPUMillis: 4000, MemoryMiB: 8192, Enabled: true}}
			if kind == biz.TypeGPU {
				repo.preset.GPU = &compute.GPUReference{ProfileID: resource, ProfileVersion: 1, SpecDigest: strings.Repeat("b", 64), DevicesPerInstance: 1, ClusterID: tenant, PoolID: tenant, Mode: "SHARED_FIXED", ModelKey: "t4", MemoryMiBPerDevice: 8192, CoreLimitPercent: 25}
			}
			images, environment := &cpuImageBoundary{}, &cpuEnvironmentBoundary{}
			s, e := NewService(repo, newTestComputeResolver(t, repo, tenant), images, environment, Config{NewIntentsEnabled: true, NamespacePrefix: "tenant-", ClusterID: tenant, ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/bash", UID: 1000, GID: 1000})
			if e != nil {
				t.Fatal(e)
			}
			ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "governance:user:42"})
			input := &pb.CreateInstanceData{Name: "work", ImageId: "img_" + strings.Repeat("a", 32), ImageScope: "platform", PresetId: resource}
			for _, version := range []uint32{0, 1} {
				if _, err := s.ResolveCreateSpec(testContainerScope(ctx, tenant), &pb.ResolveCreateSpecRequest{InstanceContractVersion: version, TenantId: tenant, Data: input}); status.Code(err) != codes.InvalidArgument {
					t.Fatal("retired resolver version accepted", version, err)
				}
			}
			resolved, e := s.ResolveCreateSpec(testContainerScope(ctx, tenant), &pb.ResolveCreateSpecRequest{InstanceContractVersion: 2, TenantId: tenant, Data: input})
			if e != nil {
				t.Fatal(e)
			}
			if resolved.InstanceContractVersion != 2 || resolved.WorkspaceMountPath != "/workspace" {
				t.Fatal("resolution did not freeze current defaults")
			}
			if images.calls != 1 || environment.calls != 1 || resolved.Cpu != "4000m" || resolved.Memory != "8192Mi" {
				t.Fatal("managed resolution missing", resolved)
			}
			if kind == biz.TypeGPU {
				resolved.GpuSelection = nil
				resolved.GpuExecution = testGPU()
				resolved.GpuDisplay = &acc.GpuDisplay{ProfileId: resource, ProfileVersion: 1, ProfileName: "T4", ModelKey: "t4", Mode: acc.SupplyMode_SHARED_FIXED}
			}
			request := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: tenant, ResourceId: resource, OperationId: operation}, Spec: resolved}
			request.Context.RequestDigest, e = pb.CommandDigest(request.Context, request.Spec)
			if e != nil {
				t.Fatal(e)
			}
			accepted, e := s.AcceptCreate(ctx, request)
			if e != nil || !accepted.GetDurableOwnerAck().GetAccepted() || len(repo.creates) != 1 {
				t.Fatal("durable intake", e)
			}
			if kind == biz.TypeGPU && repo.creates[0].Spec.GPU.RuntimeClass != "nvidia" {
				t.Fatal("execution runtime was not frozen")
			}
			bad := proto.Clone(request).(*pb.AcceptCreateRequest)
			bad.Spec.Cpu = "1"
			if _, e = s.AcceptCreate(ctx, bad); status.Code(e) != codes.InvalidArgument {
				t.Fatal("changed business accepted", e)
			}
			foreign := biz.WithCaller(context.Background(), biz.Caller{TenantID: resource, Actor: "governance:user:42"})
			if _, e = s.AcceptCreate(foreign, request); status.Code(e) != codes.PermissionDenied {
				t.Fatal("cross tenant create", e)
			}
			del := &pb.AcceptDeleteRequest{Context: &pb.CommandContext{TenantId: tenant, ResourceId: resource, OperationId: resource}, CreateOperationId: operation, InstanceKind: biz.KindContainer, ComputeType: kind, ExpectedVersion: 1}
			del.Context.RequestDigest, _ = pb.DeleteCommandDigest(del)
			if _, e = s.AcceptDelete(ctx, del); e != nil || len(repo.deletes) != 1 {
				t.Fatal("delete without plan or quota", e)
			}
			if _, e = s.AcceptDelete(foreign, del); status.Code(e) != codes.PermissionDenied {
				t.Fatal("cross tenant delete", e)
			}
		})
	}
}

func newTestComputeResolver(t *testing.T, r compute.PresetRepository, cluster string) *compute.Resolver {
	t.Helper()
	v, e := compute.NewResolver(r, cluster)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func testContainerScope(ctx context.Context, tenant string) context.Context {
	return compute.WithScope(ctx, compute.Scope{TenantID: tenant, Actor: "governance:user:42", RequestID: "33333333-3333-4333-8333-333333333333", Workload: "ani-governance", Method: compute.ContainerResolveMethod, Usage: compute.ContainerInstance})
}
