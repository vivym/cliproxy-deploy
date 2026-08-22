package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
)

type ActivePrincipalDisableRuntime struct {
	store    *inbox.Store
	executor *PrincipalDisableExecutor
}

func NewActivePrincipalDisableRuntime(
	store *inbox.Store,
	executor *PrincipalDisableExecutor,
) (*ActivePrincipalDisableRuntime, error) {
	if store == nil || executor == nil {
		return nil, errors.New("store and principal disable executor are required")
	}
	return &ActivePrincipalDisableRuntime{store: store, executor: executor}, nil
}

func (r *ActivePrincipalDisableRuntime) ReleaseHeldJobs(ctx context.Context) (int64, error) {
	released, err := r.store.ReleaseHeldPrincipalDisableJobs(ctx)
	if err != nil {
		return 0, fmt.Errorf("release held jobs for active principal disable runtime: %w", err)
	}
	return released, nil
}

func (r *ActivePrincipalDisableRuntime) RunOnce(ctx context.Context) (bool, error) {
	released, err := r.ReleaseHeldJobs(ctx)
	if err != nil {
		return false, err
	}
	processed, err := r.executor.RunOnce(ctx)
	return released > 0 || processed, err
}
