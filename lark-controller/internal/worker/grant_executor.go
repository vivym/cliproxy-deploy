package worker

import (
	"context"
	"errors"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

type GrantClient interface {
	Grant(context.Context, newapi.EntitlementGrantRequest) (newapi.EntitlementGrantResponse, error)
}

type GrantRequestOpener interface {
	Open(newapi.SealedGrantRequest) (newapi.EntitlementGrantRequest, error)
}

type GrantExecutor struct {
	store       *inbox.Store
	client      GrantClient
	opener      GrantRequestOpener
	retryPolicy GrantRetryPolicy
	now         func() time.Time
}

type GrantRetryPolicy struct {
	Schedule                []time.Duration
	PrincipalNotReadyMaxAge time.Duration
}

type GrantExecutorOption func(*GrantExecutor) error

func WithGrantRetryPolicy(policy GrantRetryPolicy) GrantExecutorOption {
	return func(executor *GrantExecutor) error {
		if err := validateGrantRetryPolicy(policy); err != nil {
			return err
		}
		policy.Schedule = append([]time.Duration(nil), policy.Schedule...)
		executor.retryPolicy = policy
		return nil
	}
}

func WithGrantClock(now func() time.Time) GrantExecutorOption {
	return func(executor *GrantExecutor) error {
		if now == nil {
			return errors.New("grant executor clock is required")
		}
		executor.now = now
		return nil
	}
}

func NewGrantExecutor(
	store *inbox.Store,
	client GrantClient,
	opener GrantRequestOpener,
	options ...GrantExecutorOption,
) (*GrantExecutor, error) {
	if store == nil || isNilDependency(client) || isNilDependency(opener) {
		return nil, errors.New("store, grant client, and grant payload opener are required")
	}
	executor := &GrantExecutor{
		store: store, client: client, opener: opener, retryPolicy: defaultGrantRetryPolicy(),
		now: func() time.Time { return time.Now().UTC() },
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("grant executor option is required")
		}
		if err := option(executor); err != nil {
			return nil, err
		}
	}
	return executor, nil
}

func (e *GrantExecutor) RunOnce(ctx context.Context) (bool, error) {
	job, found, err := e.store.ClaimNextEntitlementGrantJob(ctx)
	if err != nil || !found {
		return false, err
	}
	request, err := e.opener.Open(newapi.SealedGrantRequest{
		KeyID: job.KeyID, ExternalID: job.ExternalID,
		RequestSHA256: job.RequestSHA256, Nonce: job.Nonce,
		Ciphertext: job.Ciphertext,
	})
	if err != nil {
		if deadLetterErr := e.store.DeadLetterEntitlementGrantJob(
			ctx,
			job,
			inbox.EntitlementGrantFailureInvalidSealedPayload,
		); deadLetterErr != nil {
			return true, deadLetterErr
		}
		return true, nil
	}
	response, err := e.client.Grant(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		reason, retryable := classifyEntitlementGrantFailure(err)
		if retryable {
			delay, retry := e.retryDelay(job, reason, e.now())
			if retry {
				if retryErr := e.store.RetryEntitlementGrantJob(ctx, job, reason, delay); retryErr != nil {
					return true, retryErr
				}
				return true, nil
			}
			reason = inbox.ExhaustedEntitlementGrantFailure(reason)
		}
		if deadLetterErr := e.store.DeadLetterEntitlementGrantJob(
			ctx,
			job,
			reason,
		); deadLetterErr != nil {
			return true, deadLetterErr
		}
		return true, nil
	}
	receipt := inbox.EntitlementGrantReceipt{
		ExternalID: response.ExternalID, Status: response.Status, UserID: response.UserID,
		GrantType: response.Result.GrantType, QuotaDelta: response.Result.QuotaDelta,
		LevelCode: response.Result.LevelCode, SubscriptionID: response.Result.SubscriptionID,
		AssignmentVersion: response.Result.AssignmentVersion,
		Transition:        response.Result.Transition,
	}
	if err := e.store.CompleteEntitlementGrantJob(ctx, job, receipt); err != nil {
		return true, err
	}
	return true, nil
}

func defaultGrantRetryPolicy() GrantRetryPolicy {
	return GrantRetryPolicy{
		Schedule: []time.Duration{
			5 * time.Second,
			15 * time.Second,
			time.Minute,
			5 * time.Minute,
			15 * time.Minute,
			time.Hour,
		},
		PrincipalNotReadyMaxAge: 24 * time.Hour,
	}
}

func validateGrantRetryPolicy(policy GrantRetryPolicy) error {
	if len(policy.Schedule) == 0 || policy.PrincipalNotReadyMaxAge <= 0 {
		return errors.New("invalid entitlement grant retry policy")
	}
	for _, delay := range policy.Schedule {
		if delay <= 0 || delay > policy.PrincipalNotReadyMaxAge {
			return errors.New("invalid entitlement grant retry schedule")
		}
	}
	return nil
}

func (e *GrantExecutor) retryDelay(
	job inbox.EntitlementGrantJob,
	reason inbox.EntitlementGrantFailureReason,
	now time.Time,
) (time.Duration, bool) {
	index := job.Attempts - 1
	if index < 0 {
		return 0, false
	}
	if reason != inbox.EntitlementGrantFailurePrincipalNotReady {
		if index >= len(e.retryPolicy.Schedule) {
			return 0, false
		}
		return e.retryPolicy.Schedule[index], true
	}
	if job.ActivatedAt.IsZero() {
		return 0, false
	}
	elapsed := now.Sub(job.ActivatedAt)
	remaining := e.retryPolicy.PrincipalNotReadyMaxAge - elapsed
	if remaining <= 0 {
		return 0, false
	}
	index = min(index, len(e.retryPolicy.Schedule)-1)
	delay := e.retryPolicy.Schedule[index]
	if delay > remaining {
		delay = remaining
	}
	return delay, delay > 0
}

func classifyEntitlementGrantFailure(errorValue error) (
	inbox.EntitlementGrantFailureReason,
	bool,
) {
	var apiError *newapi.APIError
	if errors.As(errorValue, &apiError) && apiError != nil {
		reason := inbox.ParseEntitlementGrantFailureReason(apiError.Code)
		return reason, apiError.Retryable && inbox.IsRetryableEntitlementGrantFailure(reason)
	}
	var requestError *newapi.RequestError
	if errors.As(errorValue, &requestError) && requestError != nil {
		reason := inbox.ParseEntitlementGrantFailureReason(requestError.Reason)
		return reason, requestError.Retryable && inbox.IsRetryableEntitlementGrantFailure(reason)
	}
	return inbox.EntitlementGrantFailureUnclassified, false
}
