package main

import (
	"context"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v3/transport"
	sandboxpb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
)

// sandboxRetry keeps Sandbox registered on the shared listener while its
// database schema or other Sandbox-specific dependencies are unavailable.
// A successfully opened target is published once; its normal durable worker
// owns all subsequent retries and existing unknown mutation barriers.
type sandboxRetry struct {
	sandboxpb.UnimplementedSandboxServiceServer
	open     func(context.Context) (*sandboxRetryTarget, error)
	interval time.Duration

	stateMu sync.Mutex
	started bool
	stopped bool
	cancel  context.CancelFunc
	done    chan struct{}

	targetMu sync.RWMutex
	target   *sandboxRetryTarget
	close    sync.Once
}

type sandboxRetryTarget struct {
	service   sandboxpb.SandboxServiceServer
	execution transport.Server
	close     func()
}

func newSandboxRetry(open func(context.Context) (*sandboxRetryTarget, error), interval time.Duration) *sandboxRetry {
	return &sandboxRetry{open: open, interval: interval, done: make(chan struct{})}
}

// Start is called by Kratos alongside its other transport servers. It must not
// fail the application when Sandbox alone cannot yet be assembled.
func (r *sandboxRetry) Start(parent context.Context) error {
	r.stateMu.Lock()
	if r.started || r.stopped {
		r.stateMu.Unlock()
		return nil
	}
	r.started = true
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	r.stateMu.Unlock()
	defer close(r.done)
	defer cancel()

	timer := time.NewTimer(r.interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		target, err := r.open(ctx)
		if ctx.Err() != nil {
			if target != nil && target.close != nil {
				target.close()
			}
			return nil
		}
		if err != nil || target == nil || target.service == nil || target.execution == nil {
			if target != nil && target.close != nil {
				target.close()
			}
			timer.Reset(r.interval)
			continue
		}
		r.targetMu.Lock()
		r.target = target
		r.targetMu.Unlock()
		// The worker server stops on this same context. No new migration or
		// external mutation is performed by this composition loop.
		return target.execution.Start(ctx)
	}
}

func (r *sandboxRetry) Stop(ctx context.Context) error {
	r.stateMu.Lock()
	r.stopped = true
	started, cancel := r.started, r.cancel
	r.stateMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !started {
		return nil
	}
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close is also used by failed application construction before Start. Calls
// that already reached the recovered handler finish before its pool is closed.
func (r *sandboxRetry) Close() {
	r.close.Do(func() {
		_ = r.Stop(context.Background())
		r.targetMu.Lock()
		defer r.targetMu.Unlock()
		if r.target != nil && r.target.close != nil {
			r.target.close()
		}
		r.target = nil
	})
}

func (r *sandboxRetry) ResolveCreateSpec(ctx context.Context, req *sandboxpb.ResolveCreateSpecRequest) (*sandboxpb.ResolveCreateSpecResponse, error) {
	r.targetMu.RLock()
	defer r.targetMu.RUnlock()
	if r.target == nil {
		return nil, sandboxUnavailable()
	}
	return r.target.service.ResolveCreateSpec(ctx, req)
}
func (r *sandboxRetry) AcceptCreate(ctx context.Context, req *sandboxpb.AcceptCreateRequest) (*sandboxpb.AcceptCreateResponse, error) {
	r.targetMu.RLock()
	defer r.targetMu.RUnlock()
	if r.target == nil {
		return nil, sandboxUnavailable()
	}
	return r.target.service.AcceptCreate(ctx, req)
}
func (r *sandboxRetry) GetSandbox(ctx context.Context, req *sandboxpb.GetSandboxRequest) (*sandboxpb.GetSandboxResponse, error) {
	r.targetMu.RLock()
	defer r.targetMu.RUnlock()
	if r.target == nil {
		return nil, sandboxUnavailable()
	}
	return r.target.service.GetSandbox(ctx, req)
}
func (r *sandboxRetry) ListSandboxes(ctx context.Context, req *sandboxpb.ListSandboxesRequest) (*sandboxpb.ListSandboxesResponse, error) {
	r.targetMu.RLock()
	defer r.targetMu.RUnlock()
	if r.target == nil {
		return nil, sandboxUnavailable()
	}
	return r.target.service.ListSandboxes(ctx, req)
}
func (r *sandboxRetry) GetOperation(ctx context.Context, req *sandboxpb.GetOperationRequest) (*sandboxpb.GetOperationResponse, error) {
	r.targetMu.RLock()
	defer r.targetMu.RUnlock()
	if r.target == nil {
		return nil, sandboxUnavailable()
	}
	return r.target.service.GetOperation(ctx, req)
}
func (r *sandboxRetry) Pause(ctx context.Context, req *sandboxpb.PauseRequest) (*sandboxpb.PauseResponse, error) {
	r.targetMu.RLock()
	defer r.targetMu.RUnlock()
	if r.target == nil {
		return nil, sandboxUnavailable()
	}
	return r.target.service.Pause(ctx, req)
}
func (r *sandboxRetry) Resume(ctx context.Context, req *sandboxpb.ResumeRequest) (*sandboxpb.ResumeResponse, error) {
	r.targetMu.RLock()
	defer r.targetMu.RUnlock()
	if r.target == nil {
		return nil, sandboxUnavailable()
	}
	return r.target.service.Resume(ctx, req)
}
func (r *sandboxRetry) Delete(ctx context.Context, req *sandboxpb.DeleteRequest) (*sandboxpb.DeleteResponse, error) {
	r.targetMu.RLock()
	defer r.targetMu.RUnlock()
	if r.target == nil {
		return nil, sandboxUnavailable()
	}
	return r.target.service.Delete(ctx, req)
}

var _ transport.Server = (*sandboxRetry)(nil)
var _ sandboxpb.SandboxServiceServer = (*sandboxRetry)(nil)
