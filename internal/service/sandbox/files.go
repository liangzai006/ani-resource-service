package sandbox

import (
	"context"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func fileResult(r biz.FileResult) *pb.FileResult {
	out := &pb.FileResult{Path: r.Path, Kind: r.Kind, SizeBytes: r.SizeBytes, ContentBase64: r.ContentBase64, Total: r.Total, NextCursor: r.NextCursor}
	for _, v := range r.Items {
		out.Items = append(out.Items, &pb.FileEntry{Path: v.Path, Kind: v.Kind, SizeBytes: v.SizeBytes, UpdatedAtNs: v.UpdatedAtNS})
	}
	return out
}
func fileOperation(i biz.FileIntent) *pb.FileOperation {
	return &pb.FileOperation{OperationId: i.OperationID, SandboxId: i.SandboxID, State: i.State, Reason: i.Reason, Replayed: i.Replayed, Result: fileResult(i.Result)}
}
func (s *Service) QueryFiles(ctx context.Context, r *pb.QueryFilesRequest) (*pb.QueryFilesResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if s.config.FileService == nil {
		return nil, status.Error(codes.Unavailable, "SANDBOX_FILES_UNAVAILABLE")
	}
	result, err := s.config.FileService.Read(ctx, r.TenantId, r.SandboxId, biz.FileRequest{Action: r.Action, Path: r.Path, Limit: r.Limit, Cursor: r.Cursor})
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.QueryFilesResponse{Result: fileResult(result)}, nil
}
func (s *Service) MutateFiles(ctx context.Context, r *pb.MutateFilesRequest) (*pb.MutateFilesResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if s.config.FileService == nil {
		return nil, status.Error(codes.Unavailable, "SANDBOX_FILES_UNAVAILABLE")
	}
	intent, err := s.config.FileService.Mutate(ctx, biz.FileCommand{TenantID: r.TenantId, SandboxID: r.SandboxId, OperationID: r.OperationId, IdempotencyKey: r.IdempotencyKey, ExpectedVersion: r.ExpectedVersion, Request: biz.FileRequest{Action: r.Action, Path: r.Path, ContentBase64: r.ContentBase64, Overwrite: r.Overwrite}})
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.MutateFilesResponse{Operation: fileOperation(intent)}, nil
}
func (s *Service) GetFileOperation(ctx context.Context, r *pb.GetFileOperationRequest) (*pb.GetFileOperationResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if s.config.FileService == nil {
		return nil, status.Error(codes.Unavailable, "SANDBOX_FILES_UNAVAILABLE")
	}
	intent, err := s.config.FileService.Get(ctx, r.TenantId, r.SandboxId, r.OperationId)
	if err != nil {
		return nil, rpcError(err)
	}
	return &pb.GetFileOperationResponse{Operation: fileOperation(intent)}, nil
}
