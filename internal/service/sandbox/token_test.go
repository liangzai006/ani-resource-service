package sandbox

import (
	"context"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"testing"
)

func TestSandboxTokenMissingKeyKeepsTokenMethodsUnavailable(t *testing.T) {
	service := &Service{}
	_, err := service.CreateToken(context.Background(), &pb.CreateTokenRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("create: %v", err)
	}
	_, err = service.ValidateToken(context.Background(), &pb.ValidateTokenRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("validate: %v", err)
	}
	_, err = service.RevokeToken(context.Background(), &pb.RevokeTokenRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("revoke: %v", err)
	}
}
