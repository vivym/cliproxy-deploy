package worker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

type processingRecoveryStore struct {
	cutoff time.Time
	result inbox.ProcessingRecoveryResult
	err    error
}

func (s *processingRecoveryStore) RecoverStaleProcessing(
	_ context.Context,
	cutoff time.Time,
) (inbox.ProcessingRecoveryResult, error) {
	s.cutoff = cutoff
	return s.result, s.err
}

func TestProcessingReconcilerUsesLeaseCutoffAndReportsRecoveredWork(t *testing.T) {
	now := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	store := &processingRecoveryStore{result: inbox.ProcessingRecoveryResult{ApprovalJobs: 1}}
	reconciler, err := worker.NewProcessingReconciler(
		store,
		5*time.Minute,
		worker.WithProcessingReconciliationClock(func() time.Time { return now }),
	)
	if err != nil {
		t.Fatalf("new processing reconciler: %v", err)
	}
	processed, err := reconciler.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("run processing reconciliation: processed=%t err=%v", processed, err)
	}
	if want := now.Add(-5 * time.Minute); !store.cutoff.Equal(want) {
		t.Fatalf("recovery cutoff = %s, want %s", store.cutoff, want)
	}

	store.result = inbox.ProcessingRecoveryResult{}
	processed, err = reconciler.RunOnce(context.Background())
	if err != nil || processed {
		t.Fatalf("run no-op reconciliation: processed=%t err=%v", processed, err)
	}
	store.err = errors.New("database unavailable")
	processed, err = reconciler.RunOnce(context.Background())
	if err == nil || processed {
		t.Fatalf("run failed reconciliation: processed=%t err=%v", processed, err)
	}
}

func TestProcessingReconcilerRejectsInvalidDependencies(t *testing.T) {
	store := &processingRecoveryStore{}
	if _, err := worker.NewProcessingReconciler(nil, time.Minute); err == nil {
		t.Fatal("nil processing recovery store accepted")
	}
	if _, err := worker.NewProcessingReconciler(store, 0); err == nil {
		t.Fatal("zero processing lease accepted")
	}
	if _, err := worker.NewProcessingReconciler(
		store,
		time.Minute,
		worker.WithProcessingReconciliationClock(nil),
	); err == nil {
		t.Fatal("nil processing reconciliation clock accepted")
	}
}
