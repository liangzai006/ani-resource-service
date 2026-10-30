package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func TestSandboxTokenActorIsLimitedToDataPlane(t *testing.T) {
	cert := &x509.Certificate{DNSNames: []string{GovernanceSAN}}
	base := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
	tenant := "11111111-1111-4111-8111-111111111111"
	actor := "governance:sandbox-token:33333333-3333-4333-8333-333333333333"
	ctx := metadata.NewIncomingContext(base, metadata.Pairs("x-ani-tenant-id", tenant, "x-ani-actor", actor, "x-ani-request-id", "22222222-2222-4222-8222-222222222222"))
	for _, test := range []struct {
		method string
		allow  bool
	}{
		{pb.SandboxService_ValidateToken_FullMethodName, true}, {pb.SandboxService_QueryFiles_FullMethodName, true}, {pb.SandboxService_RunCode_FullMethodName, true},
		{pb.SandboxService_CreateToken_FullMethodName, false}, {pb.SandboxService_RevokeToken_FullMethodName, false}, {pb.SandboxService_Delete_FullMethodName, false}, {"/network.v1.NetworkService/GetVPC", false},
	} {
		t.Run(test.method, func(t *testing.T) {
			called := false
			_, err := GovernanceUnary()(ctx, &pb.ValidateTokenRequest{TenantId: tenant}, &grpc.UnaryServerInfo{FullMethod: test.method}, func(context.Context, any) (any, error) { called = true; return nil, nil })
			if test.allow {
				if err != nil || !called {
					t.Fatalf("expected allowed: %v", err)
				}
			} else if called || status.Code(err) != codes.PermissionDenied {
				t.Fatalf("unexpected access: called=%v err=%v", called, err)
			}
		})
	}
}
