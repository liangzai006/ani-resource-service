package server

import (
	"context"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// StorageDeleteWorker joins the normal process lifecycle. Cancellation waits
// for the current bounded step before the owner repository is closed.
type StorageDeleteWorker struct {
	usecase *storage.UseCase
	check   func(context.Context) error
	logger  *slog.Logger
	healthy atomic.Bool
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
}

func NewStorageDeleteWorker(u *storage.UseCase, check func(context.Context) error, logger *slog.Logger) *StorageDeleteWorker {
	return &StorageDeleteWorker{usecase: u, check: check, logger: logger, done: make(chan struct{})}
}
func (s *StorageDeleteWorker) Ready() bool { return s.healthy.Load() }
func (s *StorageDeleteWorker) Start(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	defer close(s.done)
	defer cancel()
	defer s.healthy.Store(false)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		check, cancelCheck := context.WithTimeout(ctx, 15*time.Second)
		err := s.check(check)
		cancelCheck()
		if err == nil {
			_, err = s.usecase.StepDeleteRecovery(ctx)
		}
		s.healthy.Store(err == nil)
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("storage PVC delete recovery unavailable")
		}
		timer.Reset(5 * time.Second)
	}
}
func (s *StorageDeleteWorker) Stop(ctx context.Context) error {
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
