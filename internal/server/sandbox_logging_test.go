package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-kratos/kratos/v3/transport"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
)

func TestSandboxRequestNotLogged(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	req := &pb.PauseRequest{IdempotencyKey: "sandbox-sensitive-sentinel"}
	ctx := transport.NewServerContext(context.Background(), imageLogTransport{pb.SandboxService_Pause_FullMethodName})
	called := false
	_, err := requestLogging(logger)(func(_ context.Context, actual any) (any, error) {
		called = actual == req
		return &pb.PauseResponse{}, nil
	})(ctx, req)
	if err != nil || !called || strings.Contains(out.String(), req.IdempotencyKey) || !strings.Contains(out.String(), "[REDACTED]") {
		t.Fatal("Sandbox request disclosed in logs or changed before handler")
	}
}
