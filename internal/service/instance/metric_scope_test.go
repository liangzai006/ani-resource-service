package instance

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Only Get is implemented: an unexpected persistence write fails immediately.
type metricScopeRepository struct {
	biz.Repository
	compute.PresetRepository
	item  biz.Instance
	err   error
	reads int
}

func (r *metricScopeRepository) Get(_ context.Context, tenant, id string) (biz.Instance, error) {
	r.reads++
	if r.err != nil {
		return biz.Instance{}, r.err
	}
	if tenant != r.item.TenantID || id != r.item.ID {
		return biz.Instance{}, biz.ErrNotFound
	}
	return r.item, nil
}

func TestMetricScopePersistedReadOnlyHistory(t *testing.T) {
	for _, state := range []string{biz.Running, biz.Stopped, biz.Deleted, "pending"} {
		t.Run(state, func(t *testing.T) {
			item := biz.Instance{TenantID: uuid.NewString(), ID: uuid.NewString(), Desired: state, Observed: state, Spec: biz.Spec{ClusterID: uuid.NewString(), Namespace: "saved-namespace"}}
			repo := &metricScopeRepository{item: item}
			images, environment := &cpuImageBoundary{}, &cpuEnvironmentBoundary{}
			s, err := NewService(repo, newTestComputeResolver(t, repo, uuid.NewString()), images, environment, Config{NamespacePrefix: "different-", ClusterID: uuid.NewString(), ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/sh"})
			if err != nil {
				t.Fatal(err)
			}
			ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: item.TenantID, Actor: "governance:user:42"})
			got, err := s.ResolveInstanceMetricScope(ctx, &pb.GetInstanceRequest{TenantId: item.TenantID, InstanceId: item.ID})
			if err != nil || got.GetTenantId() != item.TenantID || got.GetInstanceId() != item.ID || got.GetClusterId() != item.Spec.ClusterID || got.GetNamespace() != item.Spec.Namespace || got.GetContainerName() != biz.Container {
				t.Fatal("persisted metric scope changed", got, err)
			}
			if repo.reads != 1 || images.calls != 0 || environment.calls != 0 || !reflect.DeepEqual(repo.item, item) {
				t.Fatal("historical read invoked another boundary or changed record")
			}
		})
	}
}

func TestMetricScopeVMFallback(t *testing.T) {
	tenant, id, cluster := uuid.NewString(), uuid.NewString(), uuid.NewString()
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "governance:user:42"})
	repo := &metricScopeRepository{item: biz.Instance{TenantID: tenant, ID: uuid.NewString()}}
	s, err := NewService(repo, newTestComputeResolver(t, repo, cluster), &cpuImageBoundary{}, &cpuEnvironmentBoundary{}, Config{ClusterID: cluster, ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.SetVMMetricScope(func(_ context.Context, gotTenant, gotID string) (string, string, bool, error) {
		calls++
		if gotTenant != tenant || gotID != id {
			t.Fatal("vm resolver received a different identity")
		}
		return cluster, "tenant-vm", true, nil
	})
	got, err := s.ResolveInstanceMetricScope(ctx, &pb.GetInstanceRequest{TenantId: tenant, InstanceId: id})
	if err != nil || got.GetTenantId() != tenant || got.GetInstanceId() != id || got.GetClusterId() != cluster || got.GetNamespace() != "tenant-vm" || got.GetContainerName() != "compute" {
		t.Fatal("vm metric scope wrong", got, err)
	}
	if calls != 1 {
		t.Fatalf("vm resolver calls=%d", calls)
	}

	for _, tc := range []struct {
		name  string
		scope func(context.Context, string, string) (string, string, bool, error)
		code  codes.Code
	}{
		{"vm not found", func(context.Context, string, string) (string, string, bool, error) { return "", "", false, nil }, codes.NotFound},
		{"vm reader failure", func(context.Context, string, string) (string, string, bool, error) {
			return "", "", false, errors.New("storage failure")
		}, codes.Unavailable},
		{"vm bad cluster", func(context.Context, string, string) (string, string, bool, error) {
			return "bad", "tenant-vm", true, nil
		}, codes.FailedPrecondition},
		{"vm bad namespace", func(context.Context, string, string) (string, string, bool, error) {
			return cluster, "Bad_Namespace", true, nil
		}, codes.FailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &metricScopeRepository{item: biz.Instance{TenantID: tenant, ID: uuid.NewString()}}
			svc, err := NewService(reader, newTestComputeResolver(t, reader, cluster), &cpuImageBoundary{}, &cpuEnvironmentBoundary{}, Config{ClusterID: cluster, ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/sh"})
			if err != nil {
				t.Fatal(err)
			}
			svc.SetVMMetricScope(tc.scope)
			got, err := svc.ResolveInstanceMetricScope(ctx, &pb.GetInstanceRequest{TenantId: tenant, InstanceId: id})
			if got != nil || status.Code(err) != tc.code {
				t.Fatal(got, err)
			}
			if tc.code == codes.FailedPrecondition {
				details := status.Convert(err).Details()
				if len(details) != 1 {
					t.Fatal("missing machine readable reason", details)
				}
				info, ok := details[0].(*errdetails.ErrorInfo)
				if !ok || info.Reason != "INSTANCE_METRIC_SCOPE_UNAVAILABLE" {
					t.Fatal("incorrect machine readable reason", details)
				}
			}
		})
	}
}

func TestMetricScopeRejectsInvalidAndUnavailableRanges(t *testing.T) {
	tenant, id, cluster := uuid.NewString(), uuid.NewString(), uuid.NewString()
	valid := &pb.GetInstanceRequest{TenantId: tenant, InstanceId: id}
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "governance:user:42"})
	for _, tc := range []struct {
		name               string
		request            *pb.GetInstanceRequest
		context            context.Context
		cluster, namespace string
		repoError          error
		code               codes.Code
		reads              int
	}{
		{"nil", nil, ctx, cluster, "saved", nil, codes.InvalidArgument, 0},
		{"bad instance", &pb.GetInstanceRequest{TenantId: tenant, InstanceId: "not-an-id"}, ctx, cluster, "saved", nil, codes.InvalidArgument, 0},
		{"zero tenant", &pb.GetInstanceRequest{TenantId: uuid.Nil.String(), InstanceId: id}, ctx, cluster, "saved", nil, codes.InvalidArgument, 0},
		{"missing identity", valid, context.Background(), cluster, "saved", nil, codes.PermissionDenied, 0},
		{"foreign identity", valid, biz.WithCaller(context.Background(), biz.Caller{TenantID: uuid.NewString(), Actor: "governance:user:42"}), cluster, "saved", nil, codes.PermissionDenied, 0},
		{"platform identity", valid, biz.WithCaller(context.Background(), biz.Caller{Platform: true, Actor: "governance:user:42"}), cluster, "saved", nil, codes.PermissionDenied, 0},
		{"not found", valid, ctx, cluster, "saved", biz.ErrNotFound, codes.NotFound, 1},
		{"dependency", valid, ctx, cluster, "saved", errors.New("storage failure"), codes.Unavailable, 1},
		{"tombstone", valid, ctx, "", "", nil, codes.FailedPrecondition, 1},
		{"empty namespace", valid, ctx, cluster, "", nil, codes.FailedPrecondition, 1},
		{"invalid namespace", valid, ctx, cluster, "bad_namespace", nil, codes.FailedPrecondition, 1},
		{"long namespace", valid, ctx, cluster, strings.Repeat("a", 64), nil, codes.FailedPrecondition, 1},
		{"zero cluster", valid, ctx, uuid.Nil.String(), "saved", nil, codes.FailedPrecondition, 1},
		{"noncanonical cluster", valid, ctx, strings.ToUpper("abcdefab-cdef-4abc-8def-abcdefabcdef"), "saved", nil, codes.FailedPrecondition, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &metricScopeRepository{item: biz.Instance{TenantID: tenant, ID: id, Spec: biz.Spec{ClusterID: tc.cluster, Namespace: tc.namespace}}, err: tc.repoError}
			s, err := NewService(repo, newTestComputeResolver(t, repo, cluster), &cpuImageBoundary{}, &cpuEnvironmentBoundary{}, Config{ClusterID: cluster, ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/sh"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.ResolveInstanceMetricScope(tc.context, tc.request)
			if got != nil || status.Code(err) != tc.code || repo.reads != tc.reads {
				t.Fatal(got, err, repo.reads)
			}
			if tc.code == codes.FailedPrecondition {
				details := status.Convert(err).Details()
				if len(details) != 1 {
					t.Fatal("missing machine readable reason", details)
				}
				info, ok := details[0].(*errdetails.ErrorInfo)
				if !ok || info.Reason != "INSTANCE_METRIC_SCOPE_UNAVAILABLE" || info.Domain != "instance.ani.io" {
					t.Fatal("incorrect machine readable reason", details)
				}
			}
		})
	}
}
