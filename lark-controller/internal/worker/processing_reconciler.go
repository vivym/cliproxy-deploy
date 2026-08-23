package worker

import (
	"context"
	"errors"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
)

type ProcessingRecoveryStore interface {
	RecoverStaleProcessing(
		context.Context,
		time.Time,
	) (inbox.ProcessingRecoveryResult, error)
}

type ProcessingReconciler struct {
	store        ProcessingRecoveryStore
	leaseTimeout time.Duration
	now          func() time.Time
}

type ProcessingReconcilerOption func(*ProcessingReconciler) error

func WithProcessingReconciliationClock(now func() time.Time) ProcessingReconcilerOption {
	return func(reconciler *ProcessingReconciler) error {
		if now == nil {
			return errors.New("processing reconciliation clock is required")
		}
		reconciler.now = now
		return nil
	}
}

func NewProcessingReconciler(
	store ProcessingRecoveryStore,
	leaseTimeout time.Duration,
	options ...ProcessingReconcilerOption,
) (*ProcessingReconciler, error) {
	if isNilDependency(store) || leaseTimeout <= 0 {
		return nil, errors.New("processing recovery store and lease timeout are required")
	}
	reconciler := &ProcessingReconciler{
		store: store, leaseTimeout: leaseTimeout,
		now: func() time.Time { return time.Now().UTC() },
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("processing reconciler option is required")
		}
		if err := option(reconciler); err != nil {
			return nil, err
		}
	}
	return reconciler, nil
}

func (r *ProcessingReconciler) RunOnce(ctx context.Context) (bool, error) {
	cutoff := r.now().UTC().Add(-r.leaseTimeout)
	result, err := r.store.RecoverStaleProcessing(ctx, cutoff)
	if err != nil {
		return false, err
	}
	return result.Total() > 0, nil
}
