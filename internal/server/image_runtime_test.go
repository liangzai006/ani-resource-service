package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	imagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/image/v1"
	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	imageservice "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/image"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

const runtimeTestTenant = "11111111-1111-4111-8111-111111111111"

func TestImageRuntimeIdentityAndTenantBoundary(t *testing.T) {
	valid := metadata.Pairs("x-ani-tenant-id", runtimeTestTenant, "x-ani-request-id", "22222222-2222-4222-8222-222222222222")
	ensureRequest := func() *imagev1.EnsureTenantPullSecretRequest {
		return &imagev1.EnsureTenantPullSecretRequest{TenantId: runtimeTestTenant, ClusterId: "cluster-a", Namespace: "tenant-ns", NamespaceUid: "uid-a", RegistryAuthority: "registry.test"}
	}
	for _, tc := range []struct {
		name, identity, method string
		request                any
		mutate                 func(metadata.MD)
		want                   codes.Code
	}{
		{"resolve-inference", InferenceSAN, imagev1.ImageRuntimeService_ResolveImageForWorkload_FullMethodName, &imagev1.ResolveImageForWorkloadRequest{TenantId: runtimeTestTenant}, nil, codes.OK},
		{"resolve-modeldev", ModelDevSAN, imagev1.ImageRuntimeService_ResolveImageForWorkload_FullMethodName, &imagev1.ResolveImageForWorkloadRequest{TenantId: runtimeTestTenant}, nil, codes.OK},
		{"ensure-inference", InferenceSAN, imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, ensureRequest(), nil, codes.OK},
		{"ensure-modeldev", ModelDevSAN, imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, ensureRequest(), nil, codes.OK},
		// Both runtime identities lost plaintext access to pull material.
		{"pull", InferenceSAN, imagev1.ImageRuntimeService_GetTenantPullMaterial_FullMethodName, &imagev1.GetTenantPullMaterialRequest{TenantId: runtimeTestTenant}, nil, codes.PermissionDenied},
		{"pull-modeldev", ModelDevSAN, imagev1.ImageRuntimeService_GetTenantPullMaterial_FullMethodName, &imagev1.GetTenantPullMaterialRequest{TenantId: runtimeTestTenant}, nil, codes.PermissionDenied},
		{"governance", GovernanceSAN, imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, ensureRequest(), nil, codes.Unauthenticated},
		{"stranger", "ani-unknown-service", imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, ensureRequest(), nil, codes.Unauthenticated},
		{"catalog", InferenceSAN, imagev1.TenantImageService_GetImageSpace_FullMethodName, &imagev1.GetImageSpaceRequest{TenantId: runtimeTestTenant}, nil, codes.PermissionDenied},
		{"wrong-request-type", InferenceSAN, imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, &imagev1.GetImageSpaceRequest{TenantId: runtimeTestTenant}, nil, codes.PermissionDenied},
		{"cross-tenant", InferenceSAN, imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, &imagev1.EnsureTenantPullSecretRequest{TenantId: "33333333-3333-4333-8333-333333333333"}, nil, codes.PermissionDenied},
		{"empty-tenant", InferenceSAN, imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, &imagev1.EnsureTenantPullSecretRequest{}, nil, codes.PermissionDenied},
		{"actor", InferenceSAN, imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, ensureRequest(), func(m metadata.MD) { m.Set("x-ani-actor", "governance:user:1") }, codes.Unauthenticated},
		{"scope", InferenceSAN, imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, ensureRequest(), func(m metadata.MD) { m.Set(GovernanceScopeHeader, "platform") }, codes.Unauthenticated},
		{"operator", InferenceSAN, imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, ensureRequest(), func(m metadata.MD) { m.Set(OperatorHeader, "gov-user-1") }, codes.Unauthenticated},
		{"duplicate-tenant", InferenceSAN, imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, ensureRequest(), func(m metadata.MD) { m.Append("x-ani-tenant-id", runtimeTestTenant) }, codes.Unauthenticated},
		{"missing-request-id", InferenceSAN, imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, ensureRequest(), func(m metadata.MD) { m.Delete("x-ani-request-id") }, codes.Unauthenticated},
		{"nil-request-id", InferenceSAN, imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName, ensureRequest(), func(m metadata.MD) { m.Set("x-ani-request-id", "00000000-0000-0000-0000-000000000000") }, codes.Unauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert := &x509.Certificate{DNSNames: []string{tc.identity}}
			ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
			md := valid.Copy()
			if tc.mutate != nil {
				tc.mutate(md)
			}
			ctx = metadata.NewIncomingContext(ctx, md)
			called := false
			_, err := ImageRuntimeUnary()(ctx, tc.request, &grpc.UnaryServerInfo{FullMethod: tc.method}, func(ctx context.Context, _ any) (any, error) {
				called = true
				c, err := imagebiz.RequireRuntime(ctx, runtimeTestTenant)
				if err != nil || c.Actor != "" || c.Subject != tc.identity {
					t.Fatalf("runtime identity not bound: %+v %v", c, err)
				}
				if _, err := imagebiz.RequireTenant(ctx, runtimeTestTenant); err == nil {
					t.Fatal("runtime became catalog actor")
				}
				return nil, nil
			})
			if status.Code(err) != tc.want || called != (tc.want == codes.OK) {
				t.Fatalf("code=%v called=%v want=%v", status.Code(err), called, tc.want)
			}
		})
	}
}

type runtimeTransportProbe struct{}

func (runtimeTransportProbe) ResolveImageForWorkload(ctx context.Context, in imagebiz.ResolveImage) (imagebiz.ResolvedImage, error) {
	if _, err := imagebiz.RequireRuntime(ctx, in.TenantID); err != nil {
		return imagebiz.ResolvedImage{}, err
	}
	return imagebiz.ResolvedImage{TenantID: in.TenantID, ImageID: in.ImageID, Scope: in.Scope, Reference: "registry.test/platform/engine@sha256:" + strings.Repeat("a", 64), Platform: in.TargetPlatform, RegistryAuthority: "registry.test"}, nil
}
func (runtimeTransportProbe) GetTenantPullMaterial(ctx context.Context, tenant string) (imagebiz.PullMaterial, error) {
	if _, err := imagebiz.RequireRuntime(ctx, tenant); err != nil {
		return imagebiz.PullMaterial{}, err
	}
	return imagebiz.PullMaterial{TenantID: tenant, SpaceID: "space-a", RegistryAuthority: "registry.test", Username: "pull-a", Secret: imagebiz.Secret("runtime-secret-sentinel"), Generation: 1}, nil
}
func (runtimeTransportProbe) EnsureTenantPullSecret(ctx context.Context, in imagebiz.EnsurePullSecret) (imagebiz.EnsuredPullSecret, error) {
	if _, err := imagebiz.RequireRuntime(ctx, in.TenantID); err != nil {
		return imagebiz.EnsuredPullSecret{}, err
	}
	name, err := imagebiz.PullSecretName(in.TenantID, in.ClusterID, in.Namespace, in.RegistryAuthority)
	if err != nil {
		return imagebiz.EnsuredPullSecret{}, err
	}
	return imagebiz.EnsuredPullSecret{SecretRef: name, Generation: 3, RegistryAuthority: in.RegistryAuthority, SpaceID: "space-a"}, nil
}

func TestImageRuntimeMTLSListener(t *testing.T) {
	fixture := testenv.NewImageRuntimeServiceTLS(t)
	serverTLS, err := ImageRuntimeTLS(fixture.CAFile, fixture.CertFile, fixture.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, cert := range []*x509.Certificate{
		{DNSNames: []string{GovernanceSAN}}, {DNSNames: []string{"ani-unknown-service"}},
		{DNSNames: []string{InferenceSAN, GovernanceSAN}}, {DNSNames: []string{"*.ani-inference-service"}},
		{DNSNames: []string{InferenceSAN}, URIs: []*url.URL{{Scheme: "spiffe", Host: "ani"}}},
	} {
		if serverTLS.VerifyConnection(tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}) == nil {
			t.Fatal("ambiguous or wrong workload identity accepted")
		}
	}
	for _, san := range []string{InferenceSAN, ModelDevSAN} {
		cert := &x509.Certificate{DNSNames: []string{san}}
		if serverTLS.VerifyConnection(tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}) != nil {
			t.Fatalf("trusted runtime identity rejected: %s", san)
		}
	}
	listen, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listen.Addr().String()
	listen.Close()
	var logs bytes.Buffer
	s, err := NewImageRuntimeGRPCServer(&conf.Server_GRPC{Network: "tcp", Addr: addr, Timeout: durationpb.New(time.Second)}, serverTLS, imageservice.NewRuntimeService(runtimeTransportProbe{}), requestLogging(slog.New(slog.NewJSONHandler(&logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Start(context.Background()) }()
	defer func() {
		if err := s.Stop(context.Background()); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("runtime listener did not stop")
		}
	}()
	md := metadata.Pairs("x-ani-tenant-id", runtimeTestTenant, "x-ani-request-id", "22222222-2222-4222-8222-222222222222")
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), md), 3*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(fixture.Client.Clone())))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := imagev1.NewImageRuntimeServiceClient(conn)
	resolved, err := client.ResolveImageForWorkload(ctx, &imagev1.ResolveImageForWorkloadRequest{TenantId: runtimeTestTenant, Scope: imagev1.ImageScope_IMAGE_SCOPE_PLATFORM, ImageId: "img_test", TargetPlatform: &imagev1.ImagePlatform{Os: "linux", Architecture: "amd64"}})
	if err != nil || resolved.GetTenantId() != runtimeTestTenant || !strings.Contains(resolved.GetResolvedReference(), "@sha256:") || resolved.GetRegistryAuthority() != "registry.test" {
		t.Fatalf("resolve: %v %v", resolved, err)
	}
	ensured, err := client.EnsureTenantPullSecret(ctx, &imagev1.EnsureTenantPullSecretRequest{TenantId: runtimeTestTenant, ClusterId: "cluster-a", Namespace: "tenant-ns", NamespaceUid: "uid-a", RegistryAuthority: "registry.test"})
	if err != nil || ensured.GetGeneration() != 3 || !strings.HasPrefix(ensured.GetSecretRef(), "ani-pull-") || ensured.GetRegistryAuthority() != "registry.test" {
		t.Fatalf("ensure transport failed: %v %v", ensured, err)
	}
	if _, err = client.GetTenantPullMaterial(ctx, &imagev1.GetTenantPullMaterialRequest{TenantId: runtimeTestTenant}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("runtime identity retained plaintext access: %v", err)
	}
	if strings.Contains(logs.String(), "runtime-secret-sentinel") {
		t.Fatal("pull plaintext logged")
	}
	_, err = imagev1.NewTenantImageServiceClient(conn).GetImageSpace(ctx, &imagev1.GetImageSpaceRequest{TenantId: runtimeTestTenant})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("catalog accessible on runtime listener: %v", err)
	}
	_, err = client.EnsureTenantPullSecret(ctx, &imagev1.EnsureTenantPullSecretRequest{TenantId: "33333333-3333-4333-8333-333333333333"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("cross-tenant ensure accessible: %v", err)
	}
	wrong := testenv.NewServiceTLS(t)
	badConn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(wrong.Client.Clone())))
	if err != nil {
		t.Fatal(err)
	}
	defer badConn.Close()
	_, err = imagev1.NewImageRuntimeServiceClient(badConn).EnsureTenantPullSecret(ctx, &imagev1.EnsureTenantPullSecretRequest{TenantId: runtimeTestTenant, ClusterId: "cluster-a", Namespace: "tenant-ns", NamespaceUid: "uid-a", RegistryAuthority: "registry.test"})
	if err == nil {
		t.Fatal("foreign Governance credential ensured a pull secret")
	}
}
