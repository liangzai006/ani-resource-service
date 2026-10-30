package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	CodeRunMaxCodeBytes  = 64 * 1024
	CodeRunMaxStdinBytes = 1024 * 1024
	CodeRunOutputBytes   = 64 * 1024
)

// CodeRunInput is the requested code and its input, not a shell command.
type CodeRunInput struct {
	Language       string
	Code           string
	Stdin          string
	TimeoutSeconds int
}

type CodeRunCommand struct {
	TenantID, SandboxID, RunID, IdempotencyKey string
	ExpectedVersion                            int64
	Spec                                       CodeRunSpec
}

// CodeRunIntent contains no submitted source or stdin; the digest is used to
// reject conflicting retries, and the target freezes the admitted Pod identity.
type CodeRunIntent struct {
	TenantID, SandboxID, RunID, State, Reason string
	Target                                    DataPlaneTarget
	ExitCode                                  *int
	Stdout, Stderr                            []byte
	Truncated                                 bool
	Replayed                                  bool
}

type CodeRunRepository interface {
	DataPlaneReader
	AcceptCodeRun(context.Context, CodeRunCommand) (CodeRunIntent, error)
	BeginCodeRunDispatch(context.Context, CodeRunCommand) (CodeRunIntent, bool, error)
	FailCodeRunBeforeDispatch(context.Context, string, string, string) error
	FinishCodeRun(context.Context, string, string, string, *int, []byte, []byte, bool) error
	GetCodeRun(context.Context, string, string, string) (CodeRunIntent, error)
}

type CodeRunVerifier interface {
	VerifyDataPlaneTarget(context.Context, Sandbox, DataPlaneTarget) error
}

type CodeRunResult struct {
	ExitCode       *int
	Stdout, Stderr []byte
	Truncated      bool
}

type CodeRunExecutor interface {
	ExecuteCodeRun(context.Context, DataPlaneTarget, CodeRunSpec) (CodeRunResult, error)
}

type CodeRunner struct {
	repo     CodeRunRepository
	verifier CodeRunVerifier
	executor CodeRunExecutor
	slots    chan struct{}
}

func NewCodeRunner(repo CodeRunRepository, verifier CodeRunVerifier, executor CodeRunExecutor) (*CodeRunner, error) {
	if repo == nil || verifier == nil || executor == nil {
		return nil, ErrInvalid
	}
	return &CodeRunner{repo: repo, verifier: verifier, executor: executor, slots: make(chan struct{}, 16)}, nil
}

func (r *CodeRunner) Get(ctx context.Context, tenant, sandboxID, runID string) (CodeRunIntent, error) {
	return r.repo.GetCodeRun(ctx, tenant, sandboxID, runID)
}

// Submit durably accepts before starting a bounded asynchronous attempt. The
// source is not stored: if the process dies before dispatch, retrying the same
// request can resume acceptance; a dispatching record is never sent twice.
func (r *CodeRunner) Submit(ctx context.Context, command CodeRunCommand) (CodeRunIntent, error) {
	intent, err := r.repo.AcceptCodeRun(ctx, command)
	if err != nil || intent.State != "accepted" {
		return intent, err
	}
	command.RunID = intent.RunID
	select {
	case r.slots <- struct{}{}:
		go func() {
			defer func() { <-r.slots }()
			defer func() {
				if recover() != nil {
					slog.Error("sandbox code run worker panicked", "sandbox_id", command.SandboxID, "run_id", command.RunID)
				}
			}()
			background, cancel := context.WithTimeout(context.WithoutCancel(ctx), 315*time.Second)
			defer cancel()
			if _, err := r.Run(background, command); err != nil {
				slog.ErrorContext(background, "sandbox code run worker failed", "sandbox_id", command.SandboxID, "run_id", command.RunID)
			}
		}()
		return intent, nil
	default:
		// The durable row remains accepted; a same-key retry can start it.
		return intent, ErrDependencyUnavailable
	}
}

// Run durably accepts first and claims dispatch at most once. An unconfirmed
// execution remains unknown; it is never automatically dispatched again.
func (r *CodeRunner) Run(ctx context.Context, command CodeRunCommand) (CodeRunIntent, error) {
	intent, err := r.repo.AcceptCodeRun(ctx, command)
	if err != nil || intent.State != "accepted" {
		return intent, err
	}
	replayed := intent.Replayed
	// A retry can generate a new run ID; dispatch always uses the saved ID.
	command.RunID = intent.RunID
	s, err := r.repo.Get(ctx, command.TenantID, command.SandboxID)
	if err != nil {
		return intent, err
	}
	current, targetErr := DataPlaneTargetForSandbox(s, DataPlaneCodeRun)
	if targetErr == nil && current == intent.Target {
		if err = r.verifier.VerifyDataPlaneTarget(ctx, s, intent.Target); err != nil {
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			failure := r.repo.FailCodeRunBeforeDispatch(finishCtx, intent.TenantID, intent.SandboxID, intent.RunID)
			if failure != nil && !errors.Is(failure, ErrConflict) {
				return intent, failure
			}
			return r.repo.GetCodeRun(finishCtx, intent.TenantID, intent.SandboxID, intent.RunID)
		}
	}
	intent, dispatch, err := r.repo.BeginCodeRunDispatch(ctx, command)
	if err != nil || !dispatch {
		return intent, err
	}
	result, executeErr := r.executor.ExecuteCodeRun(ctx, intent.Target, command.Spec)
	// Client cancellation must not suppress recording a confirmed result or an
	// unknown dispatch. The bounded context retains trusted caller identity.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if executeErr != nil {
		result.ExitCode = nil
	}
	if err = r.repo.FinishCodeRun(finishCtx, intent.TenantID, intent.SandboxID, intent.RunID, result.ExitCode, result.Stdout, result.Stderr, result.Truncated); err != nil {
		return intent, err
	}
	finished, err := r.repo.GetCodeRun(finishCtx, intent.TenantID, intent.SandboxID, intent.RunID)
	finished.Replayed = replayed
	return finished, err
}

func CodeRunRequestDigest(command CodeRunCommand) (string, error) {
	if !UUID(command.TenantID) || !UUID(command.SandboxID) || !UUID(command.RunID) ||
		command.IdempotencyKey == "" || !utf8.ValidString(command.IdempotencyKey) || strings.ContainsRune(command.IdempotencyKey, 0) || len([]rune(command.IdempotencyKey)) > 256 || command.ExpectedVersion < 0 ||
		command.Spec.Timeout <= 0 || command.Spec.Timeout%time.Second != 0 || command.Spec.Timeout > 300*time.Second {
		return "", ErrInvalid
	}
	validated, err := NormalizeCodeRun(CodeRunInput{Language: command.Spec.Language, Code: command.Spec.Code, Stdin: command.Spec.Stdin, TimeoutSeconds: int(command.Spec.Timeout / time.Second)})
	if err != nil || validated != command.Spec {
		return "", ErrInvalid
	}
	data, err := json.Marshal(struct {
		Version         int
		ExpectedVersion int64
		Spec            CodeRunSpec
	}{1, command.ExpectedVersion, command.Spec})
	if err != nil {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type CodeRunSpec struct {
	Language string
	Code     string
	Stdin    string
	Timeout  time.Duration
}

// NormalizeCodeRun keeps the existing Python/JavaScript and timeout behavior
// while bounding request sizes before any execution or operation acceptance.
func NormalizeCodeRun(input CodeRunInput) (CodeRunSpec, error) {
	language := strings.ToLower(strings.TrimSpace(input.Language))
	if language != "python" && language != "javascript" {
		return CodeRunSpec{}, ErrInvalid
	}
	if strings.TrimSpace(input.Code) == "" || len(input.Code) > CodeRunMaxCodeBytes || len(input.Stdin) > CodeRunMaxStdinBytes ||
		!utf8.ValidString(input.Code) || !utf8.ValidString(input.Stdin) || strings.ContainsRune(input.Code, 0) {
		return CodeRunSpec{}, ErrInvalid
	}
	timeout := input.TimeoutSeconds
	if timeout <= 0 {
		timeout = 60
	}
	if timeout > 300 {
		return CodeRunSpec{}, ErrInvalid
	}
	return CodeRunSpec{Language: language, Code: input.Code, Stdin: input.Stdin, Timeout: time.Duration(timeout) * time.Second}, nil
}
