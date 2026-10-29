package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	sandboxpb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type retryTestService struct {
	sandboxpb.UnimplementedSandboxServiceServer
	call func()
}

func (s retryTestService) failure() error {
	if s.call != nil {
		s.call()
	}
	return status.Error(codes.Aborted, "forwarded")
}
func (s retryTestService) ResolveCreateSpec(context.Context, *sandboxpb.ResolveCreateSpecRequest) (*sandboxpb.ResolveCreateSpecResponse, error) {
	return nil, s.failure()
}
func (s retryTestService) AcceptCreate(context.Context, *sandboxpb.AcceptCreateRequest) (*sandboxpb.AcceptCreateResponse, error) {
	return nil, s.failure()
}
func (s retryTestService) GetSandbox(context.Context, *sandboxpb.GetSandboxRequest) (*sandboxpb.GetSandboxResponse, error) {
	return nil, s.failure()
}
func (s retryTestService) ListSandboxes(context.Context, *sandboxpb.ListSandboxesRequest) (*sandboxpb.ListSandboxesResponse, error) {
	return nil, s.failure()
}
func (s retryTestService) GetOperation(context.Context, *sandboxpb.GetOperationRequest) (*sandboxpb.GetOperationResponse, error) {
	return nil, s.failure()
}
func (s retryTestService) Pause(context.Context, *sandboxpb.PauseRequest) (*sandboxpb.PauseResponse, error) {
	return nil, s.failure()
}
func (s retryTestService) Resume(context.Context, *sandboxpb.ResumeRequest) (*sandboxpb.ResumeResponse, error) {
	return nil, s.failure()
}
func (s retryTestService) Delete(context.Context, *sandboxpb.DeleteRequest) (*sandboxpb.DeleteResponse, error) {
	return nil, s.failure()
}

type retryTestWorker struct{ started chan struct{} }

func (w retryTestWorker) Start(ctx context.Context) error { close(w.started); <-ctx.Done(); return nil }
func (retryTestWorker) Stop(context.Context) error        { return nil }

func sandboxRetryStatusChecks(r *sandboxRetry) []error {
	ctx := context.Background()
	_, a := r.ResolveCreateSpec(ctx, &sandboxpb.ResolveCreateSpecRequest{})
	_, b := r.AcceptCreate(ctx, &sandboxpb.AcceptCreateRequest{})
	_, c := r.GetSandbox(ctx, &sandboxpb.GetSandboxRequest{})
	_, d := r.ListSandboxes(ctx, &sandboxpb.ListSandboxesRequest{})
	_, e := r.GetOperation(ctx, &sandboxpb.GetOperationRequest{})
	_, f := r.Pause(ctx, &sandboxpb.PauseRequest{})
	_, g := r.Resume(ctx, &sandboxpb.ResumeRequest{})
	_, h := r.Delete(ctx, &sandboxpb.DeleteRequest{})
	return []error{a, b, c, d, e, f, g, h}
}

func TestSandboxRetryRecoversAllMethodsAndStopsWorker(t *testing.T) {
	var attempts atomic.Int32
	var calls atomic.Int32
	var closed atomic.Int32
	workerStarted := make(chan struct{})
	retry := newSandboxRetry(func(context.Context) (*sandboxRetryTarget, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("database unavailable")
		}
		return &sandboxRetryTarget{service: retryTestService{call: func() { calls.Add(1) }}, execution: retryTestWorker{started: workerStarted}, close: func() { closed.Add(1) }}, nil
	}, time.Millisecond)
	for _, err := range sandboxRetryStatusChecks(retry) {
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("before recovery: %v", err)
		}
	}
	done := make(chan error, 1)
	go func() { done <- retry.Start(context.Background()) }()
	select {
	case <-workerStarted:
	case <-time.After(time.Second):
		t.Fatal("worker not started after dependency recovered")
	}
	for _, err := range sandboxRetryStatusChecks(retry) {
		if status.Code(err) != codes.Aborted {
			t.Fatalf("recovered handler not called: %v", err)
		}
	}
	if calls.Load() != 8 || attempts.Load() != 2 {
		t.Fatalf("calls=%d attempts=%d", calls.Load(), attempts.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := retry.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	retry.Close()
	retry.Close()
	if closed.Load() != 1 {
		t.Fatalf("target closed %d times", closed.Load())
	}
	for _, err := range sandboxRetryStatusChecks(retry) {
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("after close: %v", err)
		}
	}
}

func TestSandboxRetryStopCancelsOpenAndClosesLateTarget(t *testing.T) {
	opened := make(chan struct{})
	var closed atomic.Int32
	retry := newSandboxRetry(func(ctx context.Context) (*sandboxRetryTarget, error) {
		close(opened)
		<-ctx.Done()
		return &sandboxRetryTarget{close: func() { closed.Add(1) }}, nil
	}, time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- retry.Start(context.Background()) }()
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("retry not attempted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := retry.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	retry.Close()
	if closed.Load() != 1 {
		t.Fatalf("late target closed %d times", closed.Load())
	}
}

func TestSandboxRetryCloseWaitsForInflightCall(t *testing.T) {
	entered, release, workerStarted, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	retry := newSandboxRetry(func(context.Context) (*sandboxRetryTarget, error) {
		return &sandboxRetryTarget{service: retryTestService{call: func() { close(entered); <-release }}, execution: retryTestWorker{started: workerStarted}, close: func() { close(closed) }}, nil
	}, time.Millisecond)
	go func() { _ = retry.Start(context.Background()) }()
	select {
	case <-workerStarted:
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	callDone := make(chan struct{})
	go func() {
		_, _ = retry.ListSandboxes(context.Background(), &sandboxpb.ListSandboxesRequest{})
		close(callDone)
	}()
	<-entered
	go retry.Close()
	select {
	case <-closed:
		t.Fatal("closed while handler still using target")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("target not closed")
	}
	<-callDone
}

func TestSandboxRetryStopBeforeStartDoesNotOpen(t *testing.T) {
	var attempts atomic.Int32
	retry := newSandboxRetry(func(context.Context) (*sandboxRetryTarget, error) { attempts.Add(1); return nil, nil }, time.Millisecond)
	if err := retry.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := retry.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	retry.Close()
	if attempts.Load() != 0 {
		t.Fatalf("opened %d times after stopping", attempts.Load())
	}
}
