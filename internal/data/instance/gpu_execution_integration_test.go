//go:build instanceintegration

package instance_test

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/instance"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"strings"
	"testing"
)

func TestGpuExecutionPersistenceAndDeletedCatalogReplay(t *testing.T) {
	for _, testcase := range []struct {
		name   string
		mode   acc.SupplyMode
		factor string
	}{{"shared_f1", acc.SupplyMode_SHARED_FIXED, ""}, {"whole_f1", acc.SupplyMode_WHOLE_EXCLUSIVE, ""}, {"shared_f10", acc.SupplyMode_SHARED_FIXED, "10"}} {
		t.Run(testcase.name, func(t *testing.T) {
			mode := testcase.mode
			f := database(t)
			admin := platformContext()
			tenant, id, operation, cluster := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
			ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "governance:user:42"})
			memory, cores := int64(8192), uint32(25)
			if mode == acc.SupplyMode_WHOLE_EXCLUSIVE {
				memory, cores = 15360, 100
			}
			if testcase.factor != "" {
				memory = 3840
			}
			ref := &compute.GPUReference{ProfileID: uuid.NewString(), ProfileVersion: 1, SpecDigest: strings.Repeat("c", 64), DevicesPerInstance: 1, ClusterID: cluster, PoolID: uuid.NewString(), Mode: mode.String(), ModelKey: "t4", MemoryMiBPerDevice: memory, CoreLimitPercent: cores}
			preset, e := f.presets.CreatePreset(admin, compute.Preset{Code: "gpu", Name: "GPU", ComputeType: biz.TypeGPU, CPUMillis: 4000, MemoryMiB: 8192, Enabled: true, GPU: ref})
			if e != nil {
				t.Fatal(e)
			}
			handler, e := service.NewService(f.repo, computeResolver(t, f, cluster), fixtureImage{}, fixtureEnvironment{}, service.Config{NewIntentsEnabled: true, NamespacePrefix: "tenant-", ClusterID: cluster, ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/sh", UID: 1000, GID: 1000})
			if e != nil {
				t.Fatal(e)
			}
			spec, e := handler.ResolveCreateSpec(containerScope(ctx, tenant), &pb.ResolveCreateSpecRequest{InstanceContractVersion: 2, TenantId: tenant, Data: &pb.CreateInstanceData{Name: "gpu-work", ImageId: "img_" + strings.Repeat("a", 32), ImageScope: "platform", PresetId: preset.ID}})
			if e != nil {
				t.Fatal(e)
			}
			g := &acc.GpuExecution{SchedulerName: "volcano", QueueName: "default", RuntimeClassName: "nvidia", NodeSelector: map[string]string{"accelerator.ani.io/supply-group": uuid.NewString(), "accelerator.ani.io/model-key": "t4", "accelerator.ani.io/baseline-id": uuid.NewString()}, PodAnnotations: map[string]string{"volcano.sh/vgpu-mode": "hami-core"}, Resources: map[string]string{"volcano.sh/vgpu-number": "1", "volcano.sh/vgpu-memory": "8192", "volcano.sh/vgpu-cores": "25"}}
			if mode == acc.SupplyMode_WHOLE_EXCLUSIVE {
				g.Resources["volcano.sh/vgpu-memory"] = "15360"
				g.Resources["volcano.sh/vgpu-cores"] = "100"
			}
			if testcase.factor != "" {
				g.Resources["volcano.sh/vgpu-memory"] = "384"
				g.PodAnnotations["accelerator.ani.io/memory-block-mib"] = testcase.factor
			}
			g.ExecutionDigest, _ = pb.ExecutionDigest(g)
			spec.GpuSelection = nil
			spec.GpuExecution = g
			spec.GpuDisplay = &acc.GpuDisplay{ProfileId: ref.ProfileID, ProfileVersion: 1, ProfileName: "T4", ModelKey: "t4", Mode: mode}
			request := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: tenant, ResourceId: id, OperationId: operation}, Spec: spec}
			request.Context.RequestDigest, e = pb.CommandDigest(request.Context, spec)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = handler.AcceptCreate(ctx, request); e != nil {
				t.Fatal(e)
			}
			if _, e = handler.AcceptCreate(ctx, request); e != nil {
				t.Fatal("replay", e)
			}
			if e = f.presets.DeletePreset(admin, preset.ID); e != nil {
				t.Fatal(e)
			}
			handler, e = service.NewService(f.repo, computeResolver(t, f, cluster), fixtureImage{}, fixtureEnvironment{}, service.Config{NewIntentsEnabled: true, NamespacePrefix: "tenant-", ClusterID: cluster, ServiceAccount: "changed", StorageClass: "changed", WorkspaceSize: "2Gi", Shell: "/bin/bash", UID: 1001, GID: 1001})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = handler.AcceptCreate(ctx, request); e != nil {
				t.Fatal("restart/catalog deletion changed accepted command", e)
			}
			record, e := f.repo.Get(ctx, tenant, id)
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := json.Marshal(record)
			for _, old := range []string{"Charges", "Plan", "Recipe", "GPUSpecDigest", "shared_memory_mib", "memory-percentage"} {
				if strings.Contains(string(raw), old) {
					t.Fatal("old authority persisted", old)
				}
			}
			notebook, e := data.RenderNotebook(record)
			if e != nil {
				t.Fatal(e)
			}
			runtime, _, _ := unstructured.NestedString(notebook.Object, "spec", "template", "spec", "runtimeClassName")
			if runtime != "nvidia" {
				t.Fatal("runtime changed", runtime)
			}
			containers, _, _ := unstructured.NestedSlice(notebook.Object, "spec", "template", "spec", "containers")
			limits, _, _ := unstructured.NestedStringMap(containers[0].(map[string]any), "resources", "limits")
			for key, value := range g.Resources {
				if limits[key] != value {
					t.Fatal("frozen execution render", key, limits)
				}
			}
			if notebook.GetAnnotations()["accelerator.ani.io/memory-block-mib"] != testcase.factor {
				t.Fatal("frozen memory factor changed in rendered metadata")
			}
			view, e := handler.GetInstance(ctx, &pb.GetInstanceRequest{TenantId: tenant, InstanceId: id})
			if e != nil || view.GetGpuSummary().GetMemoryMibPerDevice() != memory || view.GetGpuSummary().GetMemoryMibTotal() != memory {
				t.Fatal("historical display after catalog deletion changed absolute MiB", view, e)
			}
			del := &pb.AcceptDeleteRequest{Context: &pb.CommandContext{TenantId: tenant, ResourceId: id, OperationId: uuid.NewString()}, CreateOperationId: operation, InstanceKind: biz.KindContainer, ComputeType: biz.TypeGPU, ExpectedVersion: record.Version}
			del.Context.RequestDigest, _ = pb.DeleteCommandDigest(del)
			if _, e = handler.AcceptDelete(ctx, del); e != nil {
				t.Fatal(e)
			}
			after, e := f.repo.Get(ctx, tenant, id)
			if e != nil || after.Desired != biz.Deleted || after.Spec.GPU.Digest != g.ExecutionDigest {
				t.Fatal("delete changed execution", e)
			}
		})
	}
}
