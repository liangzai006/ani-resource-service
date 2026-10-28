package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
)

// InstanceWorkerServer runs the instance lane in the Resource process, using
// only its own durable repository. It never adds instance kinds to Network.
type InstanceWorkerServer struct {
	worker   *biz.Worker
	logger   *slog.Logger
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	releases InstanceReleaseDispatcher
}

type InstanceReleaseDispatcher interface {
	Dispatch(context.Context) (bool, error)
}

func NewInstanceWorkerServer(w *biz.Worker, l *slog.Logger, releases ...InstanceReleaseDispatcher) *InstanceWorkerServer {
	s := &InstanceWorkerServer{worker: w, logger: l, done: make(chan struct{})}
	if len(releases) != 0 {
		s.releases = releases[0]
	}
	return s
}
func (s *InstanceWorkerServer) Start(parent context.Context) error {
	s.mu.Lock()
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.mu.Unlock()
	defer close(s.done)
	defer cancel()
	var dispatch sync.WaitGroup
	if s.releases != nil {
		dispatch.Add(1)
		go func() {
			defer dispatch.Done()
			s.run(ctx, s.releases.Dispatch, "instance quota notification failed")
		}()
	}
	s.run(ctx, s.worker.Step, "instance worker attempt failed")
	cancel()
	dispatch.Wait()
	return nil
}

func (s *InstanceWorkerServer) run(ctx context.Context, step func(context.Context) (bool, error), failure string) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		worked, e := step(ctx)
		if ctx.Err() != nil {
			return
		}
		delay := time.Second
		if e != nil {
			s.logger.Warn(failure)
			delay = 5 * time.Second
		} else if worked {
			delay = time.Millisecond
		}
		timer.Reset(delay)
	}
}
func (s *InstanceWorkerServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
