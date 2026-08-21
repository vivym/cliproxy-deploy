package worker

import (
	"context"
	"errors"
	"fmt"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
)

type ActiveGrantRuntime struct {
	store                   *inbox.Store
	executor                *GrantExecutor
	activeBasePolicyVersion string
}

func NewActiveGrantRuntime(
	store *inbox.Store,
	executor *GrantExecutor,
	activeBasePolicyVersion string,
) (*ActiveGrantRuntime, error) {
	if store == nil || executor == nil || activeBasePolicyVersion == "" {
		return nil, errors.New("store, grant executor, and active base policy version are required")
	}
	return &ActiveGrantRuntime{
		store: store, executor: executor, activeBasePolicyVersion: activeBasePolicyVersion,
	}, nil
}

func (r *ActiveGrantRuntime) ReleaseHeldJobs(ctx context.Context) (int64, error) {
	if err := r.store.ValidateActiveBaseGrantPolicy(ctx, r.activeBasePolicyVersion); err != nil {
		return 0, fmt.Errorf("validate active base grant policy: %w", err)
	}
	released, err := r.store.ReleaseHeldEntitlementGrantJobs(ctx, r.activeBasePolicyVersion)
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
