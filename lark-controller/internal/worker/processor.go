package worker

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
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

type GrantRequestSealer interface {
	Seal(newapi.EntitlementGrantRequest) (newapi.SealedGrantRequest, error)
}

type ShadowProcessor struct {
	store       *inbox.Store
	fetcher     ApprovalFetcher
	resolver    ApprovalResolver
	grantSealer GrantRequestSealer
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
	if store == nil || isNilDependency(fetcher) || isNilDependency(resolver) || locale == "" {
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

func NewShadowProcessorWithGrantSealer(
	store *inbox.Store,
	fetcher ApprovalFetcher,
	resolver ApprovalResolver,
	locale string,
	grantSealer GrantRequestSealer,
	options ...ProcessorOption,
) (*ShadowProcessor, error) {
	if isNilDependency(grantSealer) {
		return nil, errors.New("grant payload sealer is required")
	}
	processor, err := NewShadowProcessor(store, fetcher, resolver, locale, options...)
	if err != nil {
		return nil, err
	}
	processor.grantSealer = grantSealer
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
	targetInstanceCode := job.Event.InstanceCode
	if job.Event.Status == "REVERTED" && job.Event.RevertedInstanceCode != "" {
		targetInstanceCode = job.Event.RevertedInstanceCode
	}
	if job.Event.Status == "REVERTED" &&
		(targetInstanceCode == "" || job.Event.ApprovalCode == "") {
		reason := inbox.ApprovalReversalReasonTargetMissing
		if job.Event.ApprovalCode == "" {
			reason = inbox.ApprovalReversalReasonApprovalCodeMissing
		}
		decision := inbox.Decision{
			EventKey: job.Event.Key, ApprovalCode: job.Event.ApprovalCode,
			InstanceCode: targetInstanceCode, EventStatus: job.Event.Status,
			Outcome: inbox.DecisionOutcomeReversalPending,
			ApprovalReversal: &inbox.ApprovalReversalDraft{
				TargetInstanceCode: targetInstanceCode,
				Result:             inbox.ApprovalReversalResultAuthorityMismatch,
				Reason:             reason,
			},
		}
		if err := p.store.CompleteDecision(ctx, job, decision); err != nil {
			return true, err
		}
		return true, nil
	}
	instance, err := p.fetcher.Fetch(ctx, targetInstanceCode, p.locale)
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
			if completeErr := p.completeFetchFailure(ctx, job, targetInstanceCode, string(reason)); completeErr != nil {
				return true, completeErr
			}
			return true, nil
		}
		delay, retry := p.retryDelay(job, failure)
		if !retry {
			if completeErr := p.completeFetchFailure(
				ctx,
				job,
				targetInstanceCode,
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
	if job.Event.Status == "REVERTED" {
		decision := evaluateRevertedEvent(job.Event, targetInstanceCode, instance)
		if err := p.store.CompleteDecision(ctx, job, decision); err != nil {
			return true, err
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
			request, receipt, planErr := newapi.PlanApprovalGrant(newapi.ApprovalGrantInput{
				TenantKey: job.Event.TenantKey, OpenID: instance.OpenID,
				PolicyVersion: resolved.PolicyVersion, ApprovalKind: string(resolved.ApprovalKind),
				BusinessCode: resolved.BusinessCode, QuotaDelta: resolved.QuotaDelta,
				MonthlyQuota: resolved.MonthlyQuota, ApprovalCode: instance.ApprovalCode,
				InstanceCode: instance.InstanceCode, StartTimeMilliseconds: instance.StartTime,
				SchemaFingerprint: resolved.SchemaFingerprint, Locale: p.locale,
				CatalogSHA256: resolved.CatalogSHA256,
			})
			if planErr != nil {
				decision.Outcome = inbox.DecisionOutcomeDeadLetterCommandPlanning
				decision.FailureReason = "invalid_command_plan"
			} else {
				command := &inbox.EntitlementCommandShadow{
					ExternalID: receipt.ExternalID, RequestSHA256: receipt.RequestSHA256,
					SubjectSHA256: receipt.SubjectSHA256, Source: "lark_approval",
					PolicyVersion: receipt.PolicyVersion, CatalogSHA256: receipt.CatalogSHA256,
					GrantType: receipt.GrantType, BusinessCode: receipt.BusinessCode,
					QuotaDelta: receipt.QuotaDelta, MonthlyQuota: receipt.MonthlyQuota,
				}
				if p.grantSealer == nil {
					decision.EntitlementCommand = command
				} else if sealed, sealErr := p.grantSealer.Seal(request); sealErr != nil {
					decision.Outcome = inbox.DecisionOutcomeDeadLetterCommandPlanning
					decision.FailureReason = "invalid_command_plan"
				} else {
					decision.EntitlementCommand = command
					decision.EntitlementGrantJob = &inbox.EntitlementGrantJobDraft{
						ExternalID: sealed.ExternalID, RequestSHA256: sealed.RequestSHA256,
						SubjectSHA256: receipt.SubjectSHA256, KeyID: sealed.KeyID,
						Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
					}
				}
			}
		}
	}
	if err := p.store.CompleteDecision(ctx, job, decision); err != nil {
		if errors.Is(err, inbox.ErrApprovalReverted) {
			decision.Outcome = inbox.DecisionOutcomeShadowAuthorityRejected
			decision.FailureReason = "approval_reverted"
			decision.EntitlementCommand = nil
			decision.EntitlementGrantJob = nil
			if completeErr := p.store.CompleteDecision(ctx, job, decision); completeErr != nil {
				return true, fmt.Errorf("reject grant fenced by approval reversal: %w", completeErr)
			}
			return true, nil
		}
		if errors.Is(err, inbox.ErrEntitlementCommandPayloadMismatch) {
			decision.Outcome = inbox.DecisionOutcomeDeadLetterCommandPlanning
			decision.FailureReason = "external_id_payload_mismatch"
			decision.EntitlementCommand = nil
			decision.EntitlementGrantJob = nil
			if completeErr := p.store.CompleteDecision(ctx, job, decision); completeErr != nil {
				return true, fmt.Errorf("dead-letter conflicting entitlement command: %w", completeErr)
			}
			return true, nil
		}
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

func (p *ShadowProcessor) completeFetchFailure(
	ctx context.Context,
	job inbox.Job,
	targetInstanceCode string,
	reason string,
) error {
	if job.Event.Status != "REVERTED" {
		return p.completeFetchDeadLetter(ctx, job, reason)
	}
	result := inbox.ApprovalReversalResultFetchTerminalError
	if strings.HasPrefix(reason, "retry_exhausted_") {
		result = inbox.ApprovalReversalResultFetchRetryExhausted
	}
	return p.store.CompleteDecision(ctx, job, inbox.Decision{
		EventKey: job.Event.Key, ApprovalCode: job.Event.ApprovalCode,
		InstanceCode: targetInstanceCode, EventStatus: job.Event.Status,
		Outcome: inbox.DecisionOutcomeReversalPending,
		ApprovalReversal: &inbox.ApprovalReversalDraft{
			TargetInstanceCode: targetInstanceCode,
			Result:             result,
			Reason:             inbox.ApprovalReversalReason(reason),
		},
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
	case "APPROVED", "OVERTIME_RECOVER", "REVERTED":
		return inbox.Decision{}, false
	case "PENDING", "REJECTED", "CANCELED", "DELETED", "OVERTIME_CLOSE":
		decision.Outcome = inbox.DecisionOutcomeShadowIgnoredNonApproved
	default:
		decision.Outcome = inbox.DecisionOutcomeDeadLetterUnknownStatus
	}
	return decision, true
}

func evaluateRevertedEvent(
	event inbox.Event,
	targetInstanceCode string,
	instance ApprovalInstance,
) inbox.Decision {
	decision := inbox.Decision{
		EventKey: event.Key, ApprovalCode: event.ApprovalCode,
		InstanceCode: targetInstanceCode, EventStatus: event.Status,
		AuthorityStatus: instance.Status, Outcome: inbox.DecisionOutcomeReversalPending,
		OpenIDHash: HashEvidence(instance.OpenID), FormSHA256: HashEvidence(instance.FormJSON),
		StartTime: instance.StartTime, Reverted: instance.Reverted,
		ApprovalReversal: &inbox.ApprovalReversalDraft{
			TargetInstanceCode:    targetInstanceCode,
			AuthorityApprovalCode: instance.ApprovalCode,
			AuthorityInstanceCode: instance.InstanceCode,
			AuthorityStatus:       instance.Status,
			AuthorityReverted:     instance.Reverted,
		},
	}
	if event.ApprovalCode == "" || targetInstanceCode == "" ||
		instance.ApprovalCode != event.ApprovalCode ||
		instance.InstanceCode != targetInstanceCode || !instance.Reverted {
		decision.ApprovalReversal.Result = inbox.ApprovalReversalResultAuthorityMismatch
		decision.ApprovalReversal.Reason = inbox.ApprovalReversalReasonAuthorityMismatch
	}
	return decision
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
