package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
)

type ApprovalInstance struct {
	ApprovalCode string
	InstanceCode string
	Status       string
	OpenID       string
	StartTime    string
	FormJSON     string
	Reverted     bool
}

type ApprovalFetcher interface {
	Fetch(context.Context, string, string) (ApprovalInstance, error)
}

type ApprovalFetchFailureReason string

const (
	ApprovalFetchRateLimited     ApprovalFetchFailureReason = "rate_limited"
	ApprovalFetchServerError     ApprovalFetchFailureReason = "server_error"
	ApprovalFetchClientError     ApprovalFetchFailureReason = "client_error"
	ApprovalFetchTimeout         ApprovalFetchFailureReason = "timeout"
	ApprovalFetchTransportError  ApprovalFetchFailureReason = "transport_error"
	ApprovalFetchInvalidResponse ApprovalFetchFailureReason = "invalid_response"
	ApprovalFetchUnclassified    ApprovalFetchFailureReason = "unclassified_error"
)

type ApprovalFetchError struct {
	Reason     ApprovalFetchFailureReason
	Retryable  bool
	RetryAfter time.Duration
	StatusCode int
	LarkCode   int
}

func (e *ApprovalFetchError) Error() string {
	return fmt.Sprintf(
		"Lark approval fetch failed: %s (HTTP %d, code %d)",
		e.Reason,
		e.StatusCode,
		e.LarkCode,
	)
}

type ApprovalResolver interface {
	ResolveApproval(policy.ApprovalRequest) (policy.ApprovalResolution, error)
}

type ShadowProcessor struct {
	store       *inbox.Store
	fetcher     ApprovalFetcher
	resolver    ApprovalResolver
	locale      string
	retryPolicy RetryPolicy
}

type RetryPolicy struct {
	Schedule       []time.Duration
	MaxDelay       time.Duration
	JitterFraction float64
}

type ProcessorOption func(*ShadowProcessor) error

func WithRetryPolicy(policy RetryPolicy) ProcessorOption {
	return func(processor *ShadowProcessor) error {
		if err := validateRetryPolicy(policy); err != nil {
			return err
		}
		policy.Schedule = append([]time.Duration(nil), policy.Schedule...)
		processor.retryPolicy = policy
		return nil
	}
}

func defaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		Schedule: []time.Duration{
			5 * time.Second,
			15 * time.Second,
			time.Minute,
			5 * time.Minute,
			15 * time.Minute,
			time.Hour,
		},
		MaxDelay:       24 * time.Hour,
		JitterFraction: 0.2,
	}
}

func validateRetryPolicy(policy RetryPolicy) error {
	if len(policy.Schedule) == 0 || policy.MaxDelay <= 0 ||
		policy.JitterFraction < 0 || policy.JitterFraction > 0.5 {
		return errors.New("invalid approval fetch retry policy")
	}
	for _, delay := range policy.Schedule {
		if delay <= 0 || delay > policy.MaxDelay {
			return errors.New("invalid approval fetch retry schedule")
		}
	}
	return nil
}

func NewShadowProcessor(
	store *inbox.Store,
	fetcher ApprovalFetcher,
	resolver ApprovalResolver,
	locale string,
	options ...ProcessorOption,
) (*ShadowProcessor, error) {
	if store == nil || fetcher == nil || resolver == nil || locale == "" {
		return nil, errors.New("store, approval fetcher, approval resolver, and locale are required")
	}
	processor := &ShadowProcessor{
		store: store, fetcher: fetcher, resolver: resolver, locale: locale,
		retryPolicy: defaultRetryPolicy(),
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("processor option is required")
		}
		if err := option(processor); err != nil {
			return nil, err
		}
	}
	return processor, nil
}

func (p *ShadowProcessor) RunOnce(ctx context.Context) (bool, error) {
	job, found, err := p.store.ClaimNext(ctx)
	if err != nil || !found {
		return false, err
	}
	if !supportedApprovalInstanceEvent(job.Event) {
		decision := inbox.Decision{
			EventKey: job.Event.Key, ApprovalCode: job.Event.ApprovalCode,
			InstanceCode: job.Event.InstanceCode, EventStatus: job.Event.Status,
			Outcome: inbox.DecisionOutcomeDeadLetterUnsupportedEventType,
		}
		if err := p.store.CompleteDecision(ctx, job, decision); err != nil {
			return true, err
		}
		return true, nil
	}
	if decision, terminal := decisionWithoutFetch(job.Event); terminal {
		if err := p.store.CompleteDecision(ctx, job, decision); err != nil {
			return true, err
		}
		return true, nil
	}
	instance, err := p.fetcher.Fetch(ctx, job.Event.InstanceCode, p.locale)
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		var failure *ApprovalFetchError
		classified := errors.As(err, &failure) && failure != nil && knownFetchFailure(failure.Reason)
		if !classified || !failure.Retryable {
			reason := ApprovalFetchUnclassified
			if classified {
				reason = failure.Reason
			}
			if completeErr := p.completeFetchDeadLetter(ctx, job, string(reason)); completeErr != nil {
				return true, completeErr
			}
			return true, nil
		}
		delay, retry := p.retryDelay(job, failure)
		if !retry {
			if completeErr := p.completeFetchDeadLetter(
				ctx,
				job,
				"retry_exhausted_"+string(failure.Reason),
			); completeErr != nil {
				return true, completeErr
			}
			return true, nil
		}
		if retryErr := p.store.Retry(ctx, job, string(failure.Reason), delay); retryErr != nil {
			return true, fmt.Errorf("schedule approval fetch retry: %w", retryErr)
		}
		return true, nil
	}
	decision, validationErr := evaluateApprovedEvent(job.Event, instance)
	if validationErr != nil {
		decision = inbox.Decision{
			EventKey: job.Event.Key, ApprovalCode: job.Event.ApprovalCode,
			InstanceCode: job.Event.InstanceCode, EventStatus: job.Event.Status,
			AuthorityStatus: instance.Status, Outcome: inbox.DecisionOutcomeShadowAuthorityRejected,
			OpenIDHash: HashEvidence(instance.OpenID), FormSHA256: HashEvidence(instance.FormJSON),
			StartTime: instance.StartTime, Reverted: instance.Reverted,
		}
	} else {
		resolved, resolveErr := p.resolver.ResolveApproval(policy.ApprovalRequest{
			ApprovalCode: instance.ApprovalCode,
			Locale:       p.locale,
			StartTime:    instance.StartTime,
			FormJSON:     instance.FormJSON,
		})
		if resolveErr != nil {
			decision.Outcome = inbox.DecisionOutcomeDeadLetterPolicyValidation
		} else {
			decision.PolicyVersion = resolved.PolicyVersion
			decision.ApprovalKind = resolved.ApprovalKind
			decision.SchemaFingerprint = resolved.SchemaFingerprint
			decision.BusinessCode = resolved.BusinessCode
			decision.Locale = p.locale
			decision.CatalogSHA256 = resolved.CatalogSHA256
			decision.QuotaDelta = resolved.QuotaDelta
			decision.MonthlyQuota = resolved.MonthlyQuota
			decision.LevelRank = resolved.LevelRank
		}
	}
	if err := p.store.CompleteDecision(ctx, job, decision); err != nil {
		return true, err
	}
	return true, nil
}

func (p *ShadowProcessor) completeFetchDeadLetter(ctx context.Context, job inbox.Job, reason string) error {
	return p.store.CompleteDecision(ctx, job, inbox.Decision{
		EventKey: job.Event.Key, ApprovalCode: job.Event.ApprovalCode,
		InstanceCode: job.Event.InstanceCode, EventStatus: job.Event.Status,
		Outcome:       inbox.DecisionOutcomeDeadLetterApprovalFetch,
		FailureReason: reason,
	})
}

func knownFetchFailure(reason ApprovalFetchFailureReason) bool {
	switch reason {
	case ApprovalFetchRateLimited,
		ApprovalFetchServerError,
		ApprovalFetchClientError,
		ApprovalFetchTimeout,
		ApprovalFetchTransportError,
		ApprovalFetchInvalidResponse:
		return true
	default:
		return false
	}
}

func (p *ShadowProcessor) retryDelay(job inbox.Job, failure *ApprovalFetchError) (time.Duration, bool) {
	index := job.Attempts - 1
	if index < 0 || index >= len(p.retryPolicy.Schedule) {
		return 0, false
	}
	if failure.RetryAfter > 0 {
		return min(failure.RetryAfter, p.retryPolicy.MaxDelay), true
	}
	delay := p.retryPolicy.Schedule[index]
	if p.retryPolicy.JitterFraction == 0 {
		return delay, true
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", job.Event.Key, job.Attempts)))
	unit := float64(uint16(digest[0])<<8|uint16(digest[1])) / 65535
	factor := 1 - p.retryPolicy.JitterFraction + 2*p.retryPolicy.JitterFraction*unit
	jittered := float64(delay) * factor
	if jittered >= float64(p.retryPolicy.MaxDelay) {
		return p.retryPolicy.MaxDelay, true
	}
	if jittered < 1 {
		return time.Nanosecond, true
	}
	return time.Duration(jittered), true
}

func supportedApprovalInstanceEvent(event inbox.Event) bool {
	return (event.SchemaVersion == "2.0" && event.EventType == "approval.instance.status_changed_v4") ||
		(event.SchemaVersion == "1.0" && event.EventType == "approval_instance")
}

func decisionWithoutFetch(event inbox.Event) (inbox.Decision, bool) {
	decision := inbox.Decision{
		EventKey: event.Key, ApprovalCode: event.ApprovalCode,
		InstanceCode: event.InstanceCode, EventStatus: event.Status,
	}
	switch event.Status {
	case "APPROVED", "OVERTIME_RECOVER":
		return inbox.Decision{}, false
	case "PENDING", "REJECTED", "CANCELED", "DELETED", "OVERTIME_CLOSE":
		decision.Outcome = inbox.DecisionOutcomeShadowIgnoredNonApproved
	case "REVERTED":
		decision.Outcome = inbox.DecisionOutcomeReversalPending
	default:
		decision.Outcome = inbox.DecisionOutcomeDeadLetterUnknownStatus
	}
	return decision, true
}

func evaluateApprovedEvent(event inbox.Event, instance ApprovalInstance) (inbox.Decision, error) {
	if event.Status != "APPROVED" && event.Status != "OVERTIME_RECOVER" {
		return inbox.Decision{}, errors.New("event does not require authoritative approval fetch")
	}
	if event.ApprovalCode == "" || event.InstanceCode == "" ||
		instance.ApprovalCode != event.ApprovalCode || instance.InstanceCode != event.InstanceCode ||
		instance.Status != "APPROVED" || instance.Reverted || instance.OpenID == "" {
		return inbox.Decision{}, errors.New("authoritative approval instance does not match event")
	}
	return inbox.Decision{
		EventKey: event.Key, ApprovalCode: instance.ApprovalCode,
		InstanceCode: instance.InstanceCode, EventStatus: event.Status,
		AuthorityStatus: instance.Status, Outcome: inbox.DecisionOutcomeShadowAuthorityVerified,
		OpenIDHash: HashEvidence(instance.OpenID), FormSHA256: HashEvidence(instance.FormJSON),
		StartTime: instance.StartTime, Reverted: instance.Reverted,
	}, nil
}

func HashEvidence(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:])
}
