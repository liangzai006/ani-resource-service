package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type FileRepository interface {
	DataPlaneReader
	AcceptFileOperation(context.Context, FileCommand) (FileIntent, error)
	BeginFileOperation(context.Context, FileCommand) (FileIntent, bool, error)
	FinishFileOperation(context.Context, string, string, string, string, FileResult) error
	GetFileOperation(context.Context, string, string, string) (FileIntent, error)
}
type FileExecutor interface {
	ExecuteFiles(context.Context, DataPlaneTarget, FileRequest) (FileResult, error)
}
type FileService struct {
	repo     FileRepository
	verifier CodeRunVerifier
	executor FileExecutor
	slots    chan struct{}
}

func NewFileService(repo FileRepository, verifier CodeRunVerifier, executor FileExecutor) (*FileService, error) {
	if repo == nil || verifier == nil || executor == nil {
		return nil, ErrInvalid
	}
	return &FileService{repo: repo, verifier: verifier, executor: executor, slots: make(chan struct{}, 16)}, nil
}

func FileRequestDigest(c FileCommand) (string, error) {
	if !UUID(c.TenantID) || !UUID(c.SandboxID) || !UUID(c.OperationID) || c.ExpectedVersion < 0 || c.IdempotencyKey == "" || !utf8.ValidString(c.IdempotencyKey) || strings.ContainsRune(c.IdempotencyKey, 0) || len([]rune(c.IdempotencyKey)) > 256 || (c.Request.Action != "write" && c.Request.Action != "delete") {
		return "", ErrInvalid
	}
	if err := ValidateFileRequest(c.Request); err != nil {
		return "", err
	}
	b, err := json.Marshal(struct {
		Version         int
		ExpectedVersion int64
		Request         FileRequest
	}{1, c.ExpectedVersion, c.Request})
	if err != nil {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Read is for list and read only. The live target is checked twice before
// exec; the pod UID guard prevents dispatch to a replacement Pod.
func (s *FileService) Read(ctx context.Context, tenant, id string, request FileRequest) (FileResult, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return FileResult{}, ErrDependencyUnavailable
	}
	if request.Action != "read" && request.Action != "list" {
		return FileResult{}, ErrInvalid
	}
	if err := ValidateFileRequest(request); err != nil {
		return FileResult{}, err
	}
	target, err := ResolveDataPlaneTarget(ctx, s.repo, tenant, id, DataPlaneFiles)
	if err != nil {
		return FileResult{}, err
	}
	sandbox, err := s.repo.Get(ctx, tenant, id)
	if err != nil {
		return FileResult{}, err
	}
	if err = s.verifier.VerifyDataPlaneTarget(ctx, sandbox, target); err != nil {
		return FileResult{}, err
	}
	if err = RevalidateDataPlaneTarget(ctx, s.repo, target); err != nil {
		return FileResult{}, err
	}
	result, err := s.executor.ExecuteFiles(ctx, target, request)
	if err != nil || request.Action != "list" {
		return result, err
	}
	start := 0
	if request.Cursor != "" {
		start, _ = strconv.Atoi(request.Cursor)
	}
	if start > len(result.Items) {
		start = len(result.Items)
	}
	limit := int(request.Limit)
	if limit == 0 {
		limit = 100
	}
	end := start + limit
	if end > len(result.Items) {
		end = len(result.Items)
	}
	result.Total = int64(len(result.Items))
	if end < len(result.Items) {
		result.NextCursor = strconv.Itoa(end)
	}
	result.Items = result.Items[start:end]
	return result, nil
}

// Mutate commits dispatch before exec and never sends the same operation a
// second time. The body is held only during the request, not stored in SQL.
func (s *FileService) Mutate(ctx context.Context, c FileCommand) (FileIntent, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return FileIntent{}, ErrDependencyUnavailable
	}
	intent, err := s.repo.AcceptFileOperation(ctx, c)
	if err != nil || intent.State != "accepted" {
		return intent, err
	}
	replayed := intent.Replayed
	c.OperationID = intent.OperationID
	sandbox, err := s.repo.Get(ctx, c.TenantID, c.SandboxID)
	if err != nil {
		return intent, err
	}
	current, targetErr := DataPlaneTargetForSandbox(sandbox, DataPlaneFiles)
	if targetErr == nil && current == intent.Target {
		if err = s.verifier.VerifyDataPlaneTarget(ctx, sandbox, intent.Target); err != nil {
			finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if e := s.repo.FinishFileOperation(finish, c.TenantID, c.SandboxID, c.OperationID, "target_unavailable", FileResult{}); e != nil && !errors.Is(e, ErrConflict) {
				return intent, e
			}
			return s.repo.GetFileOperation(finish, c.TenantID, c.SandboxID, c.OperationID)
		}
	}
	intent, dispatch, err := s.repo.BeginFileOperation(ctx, c)
	if err != nil || !dispatch {
		return intent, err
	}
	result, execErr := s.executor.ExecuteFiles(ctx, intent.Target, c.Request)
	reason := ""
	if execErr != nil {
		switch {
		case errors.Is(execErr, ErrFileTooLarge):
			reason = "too_large"
		case errors.Is(execErr, ErrInvalid):
			reason = "invalid"
		case errors.Is(execErr, ErrConflict):
			reason = "conflict"
		case errors.Is(execErr, ErrNotFound):
			reason = "not_found"
		default:
			reason = "unknown"
		}
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err = s.repo.FinishFileOperation(finish, c.TenantID, c.SandboxID, c.OperationID, reason, result); err != nil {
		return intent, err
	}
	intent, err = s.repo.GetFileOperation(finish, c.TenantID, c.SandboxID, c.OperationID)
	intent.Replayed = replayed
	return intent, err
}
func (s *FileService) Get(ctx context.Context, tenant, id, operation string) (FileIntent, error) {
	return s.repo.GetFileOperation(ctx, tenant, id, operation)
}
