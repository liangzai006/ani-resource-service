package server

import (
	"context"

	instancebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	vmbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	InstanceLogsMethod = "/instance.v1.InstanceService/StreamInstanceLogs"
	VMLogsMethod       = "/vm.v1.VMService/StreamVMLogs"
)

// Each permitted stream injects the caller type its service consumes.
var governanceStreamCallers = map[string]func(context.Context, governancePrincipal) context.Context{
	InstanceLogsMethod: func(ctx context.Context, p governancePrincipal) context.Context {
		return instancebiz.WithCaller(ctx, instancebiz.Caller{TenantID: p.TenantID, Actor: p.Actor})
	},
	VMLogsMethod: func(ctx context.Context, p governancePrincipal) context.Context {
		return vmbiz.WithCaller(ctx, vmbiz.Caller{TenantID: p.TenantID, Actor: p.Actor})
	},
}

type governanceLogServerStream struct {
	grpc.ServerStream
	ctx    context.Context
	tenant string
}

func (s *governanceLogServerStream) Context() context.Context { return s.ctx }
func (s *governanceLogServerStream) RecvMsg(message any) error {
	if e := s.ServerStream.RecvMsg(message); e != nil {
		return e
	}
	r, ok := message.(proto.Message)
	if !ok || !r.ProtoReflect().IsValid() {
		return status.Error(codes.PermissionDenied, "log request must be a proto message")
	}
	field := r.ProtoReflect().Descriptor().Fields().ByName("tenant_id")
	if field == nil || r.ProtoReflect().Get(field).String() != s.tenant {
		return status.Error(codes.PermissionDenied, "log tenant differs from authenticated scope")
	}
	return nil
}

// GovernanceStreams grants only the enumerated protected server streams.
// Read-only composition and every unregistered stream remain closed under the
// same Gov mTLS policy.
func GovernanceStreams(readOnly bool) grpc.StreamServerInterceptor {
	return func(server any, stream grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		if readOnly || stream == nil || info == nil || !info.IsServerStream || info.IsClientStream || next == nil {
			return status.Error(codes.PermissionDenied, "internal stream is not permitted")
		}
		caller, ok := governanceStreamCallers[info.FullMethod]
		if !ok {
			return status.Error(codes.PermissionDenied, "internal stream is not permitted")
		}
		p, e := (GovernanceResolver{}).Resolve(stream.Context())
		if e != nil {
			return e
		}
		if p.Platform || validSandboxTokenActor(p.Actor) {
			return status.Error(codes.PermissionDenied, "log streams require tenant delegation")
		}
		ctx := caller(context.WithValue(stream.Context(), governancePrincipalKey{}, p), p)
		return next(server, &governanceLogServerStream{ServerStream: stream, ctx: ctx, tenant: p.TenantID})
	}
}
