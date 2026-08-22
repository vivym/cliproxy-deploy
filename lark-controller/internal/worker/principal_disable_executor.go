package worker

import (
	"context"
	"errors"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

type PrincipalDisableClient interface {
	DisablePrincipal(
		context.Context,
		newapi.PrincipalDisableRequest,
	) (newapi.PrincipalDisableResponse, error)
}

type PrincipalDisableRequestOpener interface {
	OpenPrincipalDisable(
		newapi.SealedPrincipalDisableRequest,
	) (newapi.PrincipalDisableRequest, error)
}

type PrincipalDisableExecutor struct {
	store       *inbox.Store
	client      PrincipalDisableClient
	opener      PrincipalDisableRequestOpener
	retryPolicy []time.Duration
}

func NewPrincipalDisableExecutor(
	store *inbox.Store,
	client PrincipalDisableClient,
	opener PrincipalDisableRequestOpener,
) (*PrincipalDisableExecutor, error) {
	if store == nil || isNilDependency(client) || isNilDependency(opener) {
		return nil, errors.New("store, principal disable client, and payload opener are required")
	}
	return &PrincipalDisableExecutor{
		store:  store,
		client: client,
		opener: opener,
		retryPolicy: []time.Duration{
			5 * time.Second,
			15 * time.Second,
			time.Minute,
			5 * time.Minute,
			15 * time.Minute,
			time.Hour,
		},
	}, nil
}

func (e *PrincipalDisableExecutor) RunOnce(ctx context.Context) (bool, error) {
	job, found, err := e.store.ClaimNextPrincipalDisableJob(ctx)
	if err != nil || !found {
		return false, err
	}
	request, err := e.opener.OpenPrincipalDisable(newapi.SealedPrincipalDisableRequest{
		KeyID: job.KeyID, ExternalID: job.ExternalID,
		RequestSHA256: job.RequestSHA256, Nonce: job.Nonce,
		Ciphertext: job.Ciphertext,
	})
	if err != nil {
		if deadLetterErr := e.store.DeadLetterPrincipalDisableJob(
			ctx,
			job,
			inbox.PrincipalDisableFailureInvalidSealedPayload,
		); deadLetterErr != nil {
			return true, deadLetterErr
		}
		return true, nil
	}
	response, err := e.client.DisablePrincipal(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		reason, retryable := classifyPrincipalDisableFailure(err)
		if retryable {
			index := job.Attempts - 1
			if index >= 0 && index < len(e.retryPolicy) {
				if retryErr := e.store.RetryPrincipalDisableJob(
					ctx,
					job,
					reason,
					e.retryPolicy[index],
				); retryErr != nil {
					return true, retryErr
				}
				return true, nil
			}
			reason = inbox.ExhaustedPrincipalDisableFailure(reason)
		}
		if deadLetterErr := e.store.DeadLetterPrincipalDisableJob(
			ctx,
			job,
			reason,
		); deadLetterErr != nil {
			return true, deadLetterErr
		}
		return true, nil
	}
	if err := e.store.CompletePrincipalDisableJob(ctx, job, inbox.PrincipalDisableReceipt{
		ExternalID: response.ExternalID, Status: response.Status, Outcome: response.Outcome,
		PrincipalVersion: response.PrincipalVersion, AuthVersion: response.AuthVersion,
	}); err != nil {
		return true, err
	}
	return true, nil
}

func classifyPrincipalDisableFailure(errorValue error) (
	inbox.PrincipalDisableFailureReason,
	bool,
) {
	var apiError *newapi.APIError
	if errors.As(errorValue, &apiError) && apiError != nil {
		reason := inbox.ParsePrincipalDisableFailureReason(apiError.Code)
		return reason, apiError.Retryable && inbox.IsRetryablePrincipalDisableFailure(reason)
	}
	var requestError *newapi.RequestError
	if errors.As(errorValue, &requestError) && requestError != nil {
		reason := inbox.ParsePrincipalDisableFailureReason(requestError.Reason)
		return reason, requestError.Retryable && inbox.IsRetryablePrincipalDisableFailure(reason)
	}
	return inbox.PrincipalDisableFailureUnclassified, false
}
