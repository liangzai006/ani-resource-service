package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"

	vmpb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	vmbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type vmLogStreamProbe struct {
	grpc.ServerStream
	ctx     context.Context
	request *vmpb.StreamVMLogsRequest
}

func (s *vmLogStreamProbe) Context() context.Context { return s.ctx }
func (s *vmLogStreamProbe) RecvMsg(m any) error {
	proto.Merge(m.(proto.Message), s.request)
	return nil
}

func TestVMLogStreamExclusiveAuthority(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const request = "22222222-2222-4222-8222-222222222222"
	cert := &x509.Certificate{DNSNames: []string{GovernanceSAN}}
	base := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}}})
	metadataFor := func() metadata.MD {
		return metadata.Pairs("x-ani-tenant-id", tenant, "x-ani-actor", "governance:user:42", "x-ani-request-id", request)
	}
	for _, test := range []struct {
		name       string
		md         metadata.MD
		ctx        context.Context
		method     string
		readonly   bool
		bodyTenant string
		want       codes.Code
	}{
		{"authorized", metadataFor(), base, VMLogsMethod, false, tenant, codes.OK},
		{"different body tenant", metadataFor(), base, VMLogsMethod, false, request, codes.PermissionDenied},
		{"read-only composition", metadataFor(), base, VMLogsMethod, true, tenant, codes.PermissionDenied},
		{"instance stream stays scoped", metadataFor(), base, InstanceLogsMethod, false, tenant, codes.OK},
		{"missing TLS", metadataFor(), context.Background(), VMLogsMethod, false, tenant, codes.Unauthenticated},
		{"platform identity", metadata.Pairs("x-ani-scope", "platform", "x-ani-actor", "governance:user:42", "x-ani-request-id", request), base, VMLogsMethod, false, tenant, codes.PermissionDenied},
		{"duplicate tenant", func() metadata.MD { m := metadataFor(); m.Append("x-ani-tenant-id", tenant); return m }(), base, VMLogsMethod, false, tenant, codes.Unauthenticated},
		{"missing actor", metadata.Pairs("x-ani-tenant-id", tenant, "x-ani-request-id", request), base, VMLogsMethod, false, tenant, codes.Unauthenticated},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := &vmLogStreamProbe{ctx: metadata.NewIncomingContext(test.ctx, test.md), request: &vmpb.StreamVMLogsRequest{TenantId: test.bodyTenant}}
			e := GovernanceStreams(test.readonly)(nil, stream, &grpc.StreamServerInfo{FullMethod: test.method, IsServerStream: true}, func(_ any, s grpc.ServerStream) error {
				if test.method != VMLogsMethod {
					return s.RecvMsg(new(vmpb.StreamVMLogsRequest))
				}
				if e := vmbiz.RequireTenant(s.Context(), tenant); e != nil {
					return e
				}
				return s.RecvMsg(new(vmpb.StreamVMLogsRequest))
			})
			if code := status.Code(e); code != test.want {
				t.Fatalf("want %v got %v (%v)", test.want, code, e)
			}
		})
	}
}
