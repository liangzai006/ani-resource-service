package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type earlyCompletionRepository struct {
	WorkRepository
	work     Work
	err      error
	finishes int
}

func (r *earlyCompletionRepository) Claim(context.Context, string, time.Duration) (Work, bool, error) {
	return r.work, true, nil
}
func (r *earlyCompletionRepository) Finish(context.Context, Work, Progress) error {
	r.finishes++
	return r.err
}

// Embedded nil methods panic if an early completion accidentally invokes the
// Provider. Neither branch should issue an external mutation after fencing.
type noProviderCalls struct{ ResourceProvider }

func TestWorkerEarlyCompletionLeaseLossDoesNotTerminateService(t *testing.T) {
	storageFailure := errors.New("storage unavailable")
	for _, work := range []Work{{CancelUnsent: true}, {GateReason: ParentNotReady}} {
		for _, completionErr := range []error{ErrLeaseLost, storageFailure} {
			r := &earlyCompletionRepository{work: work, err: completionErr}
			w, err := NewWorker(r, noProviderCalls{}, uuid.NewString(), DefaultWorkerPolicy())
			if err != nil {
				t.Fatal(err)
			}
			worked, err := w.Step(context.Background())
			if !worked || r.finishes != 1 {
				t.Fatal("completion was skipped or replayed", worked, r.finishes)
			}
			if completionErr == ErrLeaseLost && err != nil {
				t.Fatal("fenced completion terminated worker", err)
			}
			if completionErr == storageFailure && !errors.Is(err, storageFailure) {
				t.Fatal("unrelated persistence failure hidden", err)
			}
		}
	}
}

type failedObservationProvider struct {
	ResourceProvider
	err error
}

func (p failedObservationProvider) Observe(context.Context, ProviderTarget) (ProviderObservation, error) {
	return ProviderObservation{}, p.err
}

func TestWorkerObserverRetainsProviderCauseAfterSuccessfulPersistence(t *testing.T) {
	cause := errors.New("kubernetes request deadline exceeded")
	r := &earlyCompletionRepository{work: Work{Resource: ResourceWork{Kind: "egress_gateway", State: Available}}}
	calls := 0
	w, err := NewWorker(r, failedObservationProvider{err: &ProviderError{Kind: ProviderTemporary, Cause: cause}}, uuid.NewString(), DefaultWorkerPolicy(), func(_ context.Context, _ Work, p Progress, finishErr error) {
		calls++
		if finishErr != nil || !errors.Is(p.ProviderError, cause) || p.Reason != ProviderUnavailable {
			t.Error("provider failure lost or confused with persistence", p, finishErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Step(context.Background()); err != nil || calls != 1 {
		t.Fatal("observer missing", calls, err)
	}
}
