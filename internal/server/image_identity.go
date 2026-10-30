package server

import (
	"context"
	"log/slog"
	"strings"

	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/go-kratos/kratos/v3/middleware/logging"
	"github.com/go-kratos/kratos/v3/transport"
	imagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/image/v1"
	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var imageGovernanceMethods = map[string]struct{}{
	imagev1.TenantImageService_EnsureImageSpace_FullMethodName:           {},
	imagev1.TenantImageService_GetImageSpace_FullMethodName:              {},
	imagev1.TenantImageService_GetPublisherCredential_FullMethodName:     {},
	imagev1.TenantImageService_IssuePublisherCredential_FullMethodName:   {},
	imagev1.TenantImageService_ResetPublisherCredential_FullMethodName:   {},
	imagev1.TenantImageService_DisablePublisherCredential_FullMethodName: {},
	imagev1.TenantImageService_RegisterImage_FullMethodName:              {},
	imagev1.TenantImageService_GetImage_FullMethodName:                   {},
	imagev1.TenantImageService_ListImages_FullMethodName:                 {},
	imagev1.TenantImageService_UpdateImage_FullMethodName:                {},
	imagev1.TenantImageService_UnregisterImage_FullMethodName:            {},
}

func withImageGovernanceCaller(ctx context.Context, p governancePrincipal, request proto.Message) (context.Context, error) {
	value := request.ProtoReflect()
	if !value.IsValid() {
		return nil, status.Error(codes.PermissionDenied, "image request required")
	}
	fd := value.Descriptor().Fields().ByName("tenant_id")
	if fd == nil || value.Get(fd).String() != p.TenantID {
		return nil, status.Error(codes.PermissionDenied, "image request tenant differs from authenticated scope")
	}
	return imagebiz.WithCaller(ctx, imagebiz.Caller{Kind: imagebiz.GovernanceCaller, Subject: p.Workload, Actor: p.Actor, TenantID: p.TenantID}), nil
}

type redactedImageRequest struct{}

func (redactedImageRequest) Redact() string { return "[REDACTED]" }

// Use the framework's logging and Redacter contract, keeping existing Network
// logs intact. Sensitive requests reach the handler but never the logger.
// Framework logging does not serialize replies.
func requestLogging(logger *slog.Logger) middleware.Middleware {
	normal := logging.Server(logger)
	return func(handler middleware.Handler) middleware.Handler {
		standard := normal(handler)
		return func(ctx context.Context, req any) (any, error) {
			info, ok := transport.FromServerContext(ctx)
			if !ok || (!strings.HasPrefix(info.Operation(), "/image.v1.") &&
				!strings.HasPrefix(info.Operation(), "/vm.v1.") &&
				!strings.HasPrefix(info.Operation(), "/storage.v1.") &&
				!strings.HasPrefix(info.Operation(), "/sandbox.v1.")) {
				return standard(ctx, req)
			}
			return normal(func(ctx context.Context, _ any) (any, error) { return handler(ctx, req) })(ctx, redactedImageRequest{})
		}
	}
}
