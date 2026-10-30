package sandbox

import (
	"context"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func codeRunResponse(intent biz.CodeRunIntent) *pb.CodeRun {
	response := &pb.CodeRun{RunId: intent.RunID, SandboxId: intent.SandboxID, State: intent.State, Reason: intent.Reason, Stdout: intent.Stdout, Stderr: intent.Stderr, Truncated: intent.Truncated, Replayed: intent.Replayed}
	if intent.ExitCode != nil {
		code := int32(*intent.ExitCode)
		response.ExitCode = &code
	}
	return response
}

func (s *Service) RunCode(ctx context.Context, r *pb.RunCodeRequest) (*pb.RunCodeResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if s.config.CodeRunner == nil {
		return nil, status.Error(codes.Unavailable, "SANDBOX_CODE_RUN_UNAVAILABLE")
	}
	spec, err := biz.NormalizeCodeRun(biz.CodeRunInput{Language: r.Language, Code: r.Code, Stdin: r.Stdin, TimeoutSeconds: int(r.TimeoutSeconds)})
	if err != nil {
		return nil, rpcError(err)
	}
	intent, err := s.config.CodeRunner.Submit(ctx, biz.CodeRunCommand{TenantID: r.TenantId, SandboxID: r.SandboxId, RunID: r.RunId, IdempotencyKey: r.IdempotencyKey, ExpectedVersion: r.ExpectedVersion, Spec: spec})
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.RunCodeResponse{Run: codeRunResponse(intent)}, nil
}

func (s *Service) GetCodeRun(ctx context.Context, r *pb.GetCodeRunRequest) (*pb.GetCodeRunResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if s.config.CodeRunner == nil {
		return nil, status.Error(codes.Unavailable, "SANDBOX_CODE_RUN_UNAVAILABLE")
	}
	intent, err := s.config.CodeRunner.Get(ctx, r.TenantId, r.SandboxId, r.RunId)
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.GetCodeRunResponse{Run: codeRunResponse(intent)}, nil
}
