//go:build sandboxintegration

package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
)

type codeRunVerifierFunc func(context.Context, biz.Sandbox, biz.DataPlaneTarget) error

func (f codeRunVerifierFunc) VerifyDataPlaneTarget(ctx context.Context, s biz.Sandbox, target biz.DataPlaneTarget) error {
	return f(ctx, s, target)
}

type codeRunExecutorFunc func(context.Context, biz.DataPlaneTarget, biz.CodeRunSpec) (biz.CodeRunResult, error)

func (f codeRunExecutorFunc) ExecuteCodeRun(ctx context.Context, target biz.DataPlaneTarget, spec biz.CodeRunSpec) (biz.CodeRunResult, error) {
	return f(ctx, target, spec)
}

func codeRunFixture(t *testing.T) (*sandboxFixture, context.Context, biz.CodeRunCommand) {
	t.Helper()
	f := sandboxDatabase(t)
	tenant, id := uuid.NewString(), uuid.NewString()
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "governance:user:42"})
	create := biz.Create{TenantID: tenant, ID: id, OperationID: uuid.NewString(), RequestDigest: strings.Repeat("a", 64), IntentDigest: strings.Repeat("b", 64), Spec: biz.CreateSpec{
		ImageReference: "registry.invalid/image@sha256:" + strings.Repeat("c", 64), ImageDigest: "sha256:" + strings.Repeat("c", 64),
		ClusterID: uuid.NewString(), Namespace: "tenant-test", NamespaceUID: "namespace-uid",
	}}
	if _, err := f.repo.AcceptCreate(ctx, create); err != nil {
		t.Fatal(err)
	}
	refs, err := json.Marshal(biz.ProviderRefs{PodName: "sandbox-" + id, PodUID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.runtime.Exec(ctx, `UPDATE sandbox.sandboxes SET observed='running',provider_refs=$3 WHERE tenant_id=$1 AND sandbox_id=$2`, tenant, id, refs); err != nil {
		t.Fatal(err)
	}
	return f, ctx, biz.CodeRunCommand{TenantID: tenant, SandboxID: id, RunID: uuid.NewString(), IdempotencyKey: "key", ExpectedVersion: 1,
		Spec: biz.CodeRunSpec{Language: "python", Code: "print(1)", Timeout: time.Minute}}
}

func TestCodeRunDurableIdempotencyAndDispatch(t *testing.T) {
	f, ctx, command := codeRunFixture(t)
	const workers = 12
	type result struct {
		intent biz.CodeRunIntent
		err    error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := command
			c.RunID = uuid.NewString()
			intent, err := f.repo.AcceptCodeRun(ctx, c)
			results <- result{intent, err}
		}()
	}
	wg.Wait()
	close(results)
	created := 0
	var runID string
	for r := range results {
		if r.err != nil {
			t.Fatalf("accept: %v", r.err)
		}
		if runID == "" {
			runID = r.intent.RunID
		}
		if r.intent.RunID != runID {
			t.Fatal("one key created multiple runs")
		}
		if !r.intent.Replayed {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created=%d", created)
	}
	command.RunID = runID
	changed := command
	changed.Spec.Code = "print(2)"
	if _, err := f.repo.AcceptCodeRun(ctx, changed); !errors.Is(err, biz.ErrConflict) {
		t.Fatalf("changed input: %v", err)
	}
	dispatches := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, send, err := f.repo.BeginCodeRunDispatch(ctx, command)
			if err != nil {
				t.Errorf("begin dispatch: %v", err)
			}
			dispatches <- send
		}()
	}
	wg.Wait()
	close(dispatches)
	count := 0
	for send := range dispatches {
		if send {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("dispatches=%d", count)
	}
	if err := f.repo.FinishCodeRun(ctx, command.TenantID, command.SandboxID, runID, nil, []byte("partial"), nil, false); err != nil {
		t.Fatal(err)
	}
	intent, send, err := f.repo.BeginCodeRunDispatch(ctx, command)
	if err != nil || send || intent.State != "unknown" || string(intent.Stdout) != "partial" {
		t.Fatalf("unknown was retried: %+v %v %v", intent, send, err)
	}
	other := uuid.NewString()
	otherCtx := biz.WithCaller(context.Background(), biz.Caller{TenantID: other, Actor: "user"})
	if _, err := f.repo.GetCodeRun(otherCtx, other, command.SandboxID, runID); !errors.Is(err, biz.ErrNotFound) {
		t.Fatalf("cross-tenant lookup: %v", err)
	}
}

func TestCodeRunPausePreventsDispatchAndNewAcceptance(t *testing.T) {
	f, ctx, command := codeRunFixture(t)
	intent, err := f.repo.AcceptCodeRun(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	pause := biz.Command{TenantID: command.TenantID, ID: command.SandboxID, OperationID: uuid.NewString(), Action: "pause", IdempotencyKey: "pause", RequestDigest: biz.ActionDigest("", 1, "pause"), ExpectedVersion: 1}
	if _, err = f.repo.Change(ctx, pause); err != nil {
		t.Fatal(err)
	}
	intent, send, err := f.repo.BeginCodeRunDispatch(ctx, command)
	if err != nil || send || intent.State != "cancelled" {
		t.Fatalf("paused dispatch: %+v %v %v", intent, send, err)
	}
	replay, err := f.repo.AcceptCodeRun(ctx, command)
	if err != nil || !replay.Replayed || replay.RunID != intent.RunID || replay.State != "cancelled" {
		t.Fatalf("paused replay: %+v %v", replay, err)
	}
	command.RunID, command.IdempotencyKey, command.ExpectedVersion = uuid.NewString(), "new", 0
	if _, err = f.repo.AcceptCodeRun(ctx, command); !errors.Is(err, biz.ErrDataPlaneUnavailable) {
		t.Fatalf("paused new run: %v", err)
	}
}

func TestCodeRunConcurrentAcceptanceAndPause(t *testing.T) {
	f, ctx, command := codeRunFixture(t)
	pause := biz.Command{TenantID: command.TenantID, ID: command.SandboxID, OperationID: uuid.NewString(), Action: "pause", IdempotencyKey: "pause", RequestDigest: biz.ActionDigest("", 1, "pause"), ExpectedVersion: 1}
	start := make(chan struct{})
	acceptResult := make(chan error, 1)
	pauseResult := make(chan error, 1)
	go func() {
		<-start
		_, err := f.repo.AcceptCodeRun(ctx, command)
		acceptResult <- err
	}()
	go func() {
		<-start
		_, err := f.repo.Change(ctx, pause)
		pauseResult <- err
	}()
	close(start)
	acceptErr, pauseErr := <-acceptResult, <-pauseResult
	if pauseErr != nil || (acceptErr != nil && !errors.Is(acceptErr, biz.ErrDataPlaneUnavailable) && !errors.Is(acceptErr, biz.ErrVersion)) {
		t.Fatalf("accept=%v pause=%v", acceptErr, pauseErr)
	}
	if acceptErr == nil {
		intent, send, err := f.repo.BeginCodeRunDispatch(ctx, command)
		if err != nil || send || intent.State != "cancelled" {
			t.Fatalf("post-pause dispatch: %+v %v %v", intent, send, err)
		}
	}
	command.IdempotencyKey = "after-pause"
	command.RunID = uuid.NewString()
	command.ExpectedVersion = 0
	if _, err := f.repo.AcceptCodeRun(ctx, command); !errors.Is(err, biz.ErrDataPlaneUnavailable) {
		t.Fatalf("post-pause acceptance: %v", err)
	}
}

func TestCodeRunnerUsesDurableResultOnReplay(t *testing.T) {
	f, ctx, command := codeRunFixture(t)
	var calls atomic.Int64
	verifier := codeRunVerifierFunc(func(_ context.Context, s biz.Sandbox, target biz.DataPlaneTarget) error {
		if s.ProviderRefs.PodUID != target.PodUID {
			t.Fatal("target did not match accepted Pod")
		}
		return nil
	})
	executor := codeRunExecutorFunc(func(_ context.Context, _ biz.DataPlaneTarget, _ biz.CodeRunSpec) (biz.CodeRunResult, error) {
		calls.Add(1)
		code := 0
		return biz.CodeRunResult{ExitCode: &code, Stdout: []byte("ok")}, nil
	})
	runner, err := biz.NewCodeRunner(f.repo, verifier, executor)
	if err != nil {
		t.Fatal(err)
	}
	first, err := runner.Run(ctx, command)
	if err != nil || first.State != "succeeded" || string(first.Stdout) != "ok" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	command.RunID = uuid.NewString()
	replay, err := runner.Run(ctx, command)
	if err != nil || !replay.Replayed || replay.RunID != first.RunID || replay.State != "succeeded" || string(replay.Stdout) != "ok" || calls.Load() != 1 {
		t.Fatalf("replay=%+v calls=%d err=%v", replay, calls.Load(), err)
	}
}

func TestCodeRunnerRecordsFailedTargetBeforeDispatch(t *testing.T) {
	f, ctx, command := codeRunFixture(t)
	verifier := codeRunVerifierFunc(func(context.Context, biz.Sandbox, biz.DataPlaneTarget) error {
		return errors.New("provider unavailable")
	})
	executor := codeRunExecutorFunc(func(context.Context, biz.DataPlaneTarget, biz.CodeRunSpec) (biz.CodeRunResult, error) {
		t.Error("unverified target reached execution")
		return biz.CodeRunResult{}, nil
	})
	runner, err := biz.NewCodeRunner(f.repo, verifier, executor)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := runner.Submit(ctx, command)
	if err != nil || accepted.State != "accepted" {
		t.Fatalf("accept=%+v err=%v", accepted, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		intent, err := f.repo.GetCodeRun(ctx, command.TenantID, command.SandboxID, accepted.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if intent.State == "failed" {
			if intent.Reason != "SANDBOX_TARGET_UNAVAILABLE" || intent.ExitCode != nil {
				t.Fatalf("invalid failed target state: %+v", intent)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("target failure was not recorded")
}

func TestCodeRunStaleDispatchBecomesUnknownWithoutReplay(t *testing.T) {
	f, ctx, command := codeRunFixture(t)
	if _, err := f.repo.AcceptCodeRun(ctx, command); err != nil {
		t.Fatal(err)
	}
	if _, send, err := f.repo.BeginCodeRunDispatch(ctx, command); err != nil || !send {
		t.Fatalf("dispatch: %v %v", send, err)
	}
	if _, err := f.runtime.Exec(ctx, `UPDATE sandbox.code_runs SET updated_at=clock_timestamp()-interval '321 seconds' WHERE tenant_id=$1 AND sandbox_id=$2 AND run_id=$3`, command.TenantID, command.SandboxID, command.RunID); err != nil {
		t.Fatal(err)
	}
	intent, err := f.repo.GetCodeRun(ctx, command.TenantID, command.SandboxID, command.RunID)
	if err != nil || intent.State != "unknown" || intent.Reason != "SANDBOX_DISPATCH_OUTCOME_UNKNOWN" {
		t.Fatalf("stale dispatch: %+v %v", intent, err)
	}
	if _, send, err := f.repo.BeginCodeRunDispatch(ctx, command); err != nil || send {
		t.Fatalf("unknown dispatch replay: %v %v", send, err)
	}
}
