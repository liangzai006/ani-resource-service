package sandbox

import (
	"context"
	"time"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) CreateToken(ctx context.Context, r *pb.CreateTokenRequest) (*pb.CreateTokenResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if s.config.TokenService == nil {
		return nil, status.Error(codes.Unavailable, "SANDBOX_TOKEN_UNAVAILABLE")
	}
	scopes, err := biz.NormalizeTokenScopes(r.Scopes)
	if err != nil {
		return nil, rpcError(err)
	}
	seconds := r.ExpiresInSeconds
	if seconds == 0 {
		seconds = 900
	}
	if seconds < 1 || seconds > 3600 {
		return nil, rpcError(biz.ErrInvalid)
	}
	record, value, err := s.config.TokenService.Issue(ctx, biz.TokenCommand{TenantID: r.TenantId, SandboxID: r.SandboxId, TokenID: r.TokenId, IdempotencyKey: r.IdempotencyKey, GovTenantID: r.GovTenantId, IssuerID: r.IssuerId, Scopes: scopes, ExpiresIn: time.Duration(seconds) * time.Second})
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.CreateTokenResponse{Token: &pb.SandboxAccessToken{TokenId: record.Claims.TokenID, Value: value, ExpiresAt: record.Claims.ExpiresAt, Scopes: record.Claims.Scopes, Replayed: record.Replayed}}, nil
}
func (s *Service) ValidateToken(ctx context.Context, r *pb.ValidateTokenRequest) (*pb.ValidateTokenResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if s.config.TokenService == nil {
		return nil, status.Error(codes.Unavailable, "SANDBOX_TOKEN_UNAVAILABLE")
	}
	c, err := s.config.TokenService.Verify(ctx, r.TenantId, r.SandboxId, r.Value, r.RequiredScope)
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.ValidateTokenResponse{TenantId: c.TenantID, SandboxId: c.SandboxID, TokenId: c.TokenID, GovTenantId: c.GovTenantID, IssuerId: c.IssuerID, Scopes: c.Scopes, ExpiresAt: c.ExpiresAt}, nil
}
func (s *Service) RevokeToken(ctx context.Context, r *pb.RevokeTokenRequest) (*pb.RevokeTokenResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if s.config.TokenService == nil {
		return nil, status.Error(codes.Unavailable, "SANDBOX_TOKEN_UNAVAILABLE")
	}
	if err := s.config.TokenService.Revoke(ctx, r.TenantId, r.SandboxId, r.TokenId); err != nil {
		return nil, rpcError(err)
	}
	return &pb.RevokeTokenResponse{Revoked: true}, nil
}
