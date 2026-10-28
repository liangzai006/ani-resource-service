//go:build instanceintegration

package instance_test

import (
	"context"
	"crypto/tls"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	server "github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/instance"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Real PG persistence, generated client, TLS and Governance identity middleware.
// Kubernetes is controlled only while establishing lifecycle history; this RPC
// is served without a runtime/diagnostics adapter and cannot call Kubernetes.
func TestInstanceMetricScopePersistentGRPC(t *testing.T) {
	f := database(t)
	c := cpuCreate()
	ctx := context.Background()
	if _, err := f.repo.AcceptCreate(ctx, c); err != nil {
		t.Fatal(err)
	}
	handler, err := service.NewService(f.repo, computeResolver(t, f, uuid.NewString()), fixtureImage{}, fixtureEnvironment{}, service.Config{NamespacePrefix: "different-", ClusterID: uuid.NewString(), ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	identity := testenv.NewServiceTLS(t)
	tlsConfig, err := server.GovernanceTLS(identity.CAFile, identity.CertFile, identity.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	transport := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.UnaryInterceptor(server.GovernanceUnary()))
	pb.RegisterInstanceServiceServer(transport, handler)
	done := make(chan error, 1)
	go func() { done <- transport.Serve(listener) }()
	t.Cleanup(func() {
		transport.Stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(identity.Client)), grpc.WithDisableRetry())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := pb.NewInstanceServiceClient(conn)
	md := metadata.Pairs("x-ani-tenant-id", c.TenantID, "x-ani-actor", "governance:user:42", "x-ani-request-id", uuid.NewString())
	request := &pb.GetInstanceRequest{TenantId: c.TenantID, InstanceId: c.ID}
	resolve := func(t *testing.T) {
		t.Helper()
		before, err := f.repo.Get(ctx, c.TenantID, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		callCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, md), 2*time.Second)
		defer cancel()
		got, err := client.ResolveInstanceMetricScope(callCtx, request)
		if err != nil || got.GetTenantId() != c.TenantID || got.GetInstanceId() != c.ID || got.GetClusterId() != c.Spec.ClusterID || got.GetNamespace() != c.Spec.Namespace || got.GetContainerName() != biz.Container {
			t.Fatal("persisted scope", got, err)
		}
		after, err := f.repo.Get(ctx, c.TenantID, c.ID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("metric read modified persistence", err)
		}
	}
	t.Run("pending persisted scope", resolve)
	p := &provider{}
	w, err := biz.NewWorker(f.repo, p, "metric-history")
	if err != nil {
		t.Fatal(err)
	}
	var item biz.Instance
	for n := 0; n < 3; n++ {
		item = step(t, f, w, c)
	}
	if item.Observed != biz.Running {
		t.Fatal("fixture did not run", item.Reason)
	}
	command(t, f, c, item, "stop")
	for n := 0; n < 2; n++ {
		item = step(t, f, w, c)
	}
	if item.Observed != biz.Stopped {
		t.Fatal("fixture did not stop", item.Reason)
	}
	t.Run("stopped persisted scope", resolve)
	if _, err = f.repo.AcceptDelete(ctx, biz.Delete{InstanceKind: biz.KindContainer, ComputeType: biz.TypeCPU, TenantID: c.TenantID, ID: c.ID, OperationID: uuid.NewString(), CreateOperationID: c.OperationID, RequestHash: strings.Repeat("e", 64), ExpectedVersion: item.Version}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 8 && item.Observed != biz.Deleted; n++ {
		item = step(t, f, w, c)
	}
	if item.Observed != biz.Deleted {
		t.Fatal("fixture did not delete", item.Reason)
	}
	t.Run("deleted persisted scope", resolve)

	for _, tc := range []struct {
		name       string
		md         metadata.MD
		tenant, id string
		code       codes.Code
	}{
		{"missing identity", nil, c.TenantID, c.ID, codes.Unauthenticated},
		{"mismatched body tenant", md, uuid.NewString(), c.ID, codes.PermissionDenied},
		{"duplicate tenant", func() metadata.MD { m := md.Copy(); m.Append("x-ani-tenant-id", c.TenantID); return m }(), c.TenantID, c.ID, codes.Unauthenticated},
		{"forged actor", func() metadata.MD { m := md.Copy(); m.Set("x-ani-actor", "governance:user:042"); return m }(), c.TenantID, c.ID, codes.Unauthenticated},
		{"foreign instance", func() metadata.MD { m := md.Copy(); m.Set("x-ani-tenant-id", request.InstanceId); return m }(), request.InstanceId, c.ID, codes.NotFound},
		{"not found", md, c.TenantID, uuid.NewString(), codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, tc.md), 2*time.Second)
			defer cancel()
			got, err := client.ResolveInstanceMetricScope(callCtx, &pb.GetInstanceRequest{TenantId: tc.tenant, InstanceId: tc.id})
			if got != nil || status.Code(err) != tc.code {
				t.Fatal(got, err)
			}
		})
	}
	t.Run("no client certificate", func(t *testing.T) {
		plainIdentity := identity.Client.Clone()
		plainIdentity.Certificates = []tls.Certificate{}
		untrusted, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(plainIdentity)))
		if err != nil {
			t.Fatal(err)
		}
		defer untrusted.Close()
		callCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, md), 2*time.Second)
		defer cancel()
		_, err = pb.NewInstanceServiceClient(untrusted).ResolveInstanceMetricScope(callCtx, request)
		if status.Code(err) != codes.Unavailable {
			t.Fatal("untrusted TLS identity accepted", err)
		}
	})
	tombstone := cpuCreate()
	if _, err = f.repo.AcceptDelete(ctx, biz.Delete{InstanceKind: biz.KindContainer, ComputeType: biz.TypeCPU, TenantID: tombstone.TenantID, ID: tombstone.ID, OperationID: uuid.NewString(), CreateOperationID: tombstone.OperationID, RequestHash: strings.Repeat("e", 64)}); err != nil {
		t.Fatal(err)
	}
	t.Run("delete-first unavailable detail", func(t *testing.T) {
		m := md.Copy()
		m.Set("x-ani-tenant-id", tombstone.TenantID)
		callCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, m), 2*time.Second)
		defer cancel()
		_, err := client.ResolveInstanceMetricScope(callCtx, &pb.GetInstanceRequest{TenantId: tombstone.TenantID, InstanceId: tombstone.ID})
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatal(err)
		}
		details := status.Convert(err).Details()
		if len(details) != 1 {
			t.Fatal("missing error detail", details)
		}
		info, ok := details[0].(*errdetails.ErrorInfo)
		if !ok || info.Reason != "INSTANCE_METRIC_SCOPE_UNAVAILABLE" {
			t.Fatal("missing reason over grpc", details)
		}
	})
}
