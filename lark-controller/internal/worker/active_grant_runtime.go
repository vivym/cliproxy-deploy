package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
)

type ActiveGrantRuntime struct {
	store    *inbox.Store
	executor *GrantExecutor
}

func NewActiveGrantRuntime(
	store *inbox.Store,
	executor *GrantExecutor,
) (*ActiveGrantRuntime, error) {
	if store == nil || executor == nil {
		return nil, errors.New("store and grant executor are required")
	}
	return &ActiveGrantRuntime{store: store, executor: executor}, nil
}

func (r *ActiveGrantRuntime) ReleaseHeldJobs(ctx context.Context) (int64, error) {
	released, err := r.store.ReleaseHeldEntitlementGrantJobs(ctx)
	if err != nil {
		return 0, fmt.Errorf("release held jobs for active grant runtime: %w", err)
	}
	return released, nil
}

func (r *ActiveGrantRuntime) RunOnce(ctx context.Context) (bool, error) {
	released, err := r.ReleaseHeldJobs(ctx)
	if err != nil {
		return false, err
	}
	processed, err := r.executor.RunOnce(ctx)
	return released > 0 || processed, err
}
