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

type ApprovalResolver interface {
	ResolveApproval(policy.ApprovalRequest) (policy.ApprovalResolution, error)
}

type ShadowProcessor struct {
	store    *inbox.Store
	fetcher  ApprovalFetcher
	resolver ApprovalResolver
	locale   string
}

func NewShadowProcessor(
	store *inbox.Store,
	fetcher ApprovalFetcher,
	resolver ApprovalResolver,
	locale string,
) (*ShadowProcessor, error) {
	if store == nil || fetcher == nil || resolver == nil || locale == "" {
		return nil, errors.New("store, approval fetcher, approval resolver, and locale are required")
	}
	return &ShadowProcessor{store: store, fetcher: fetcher, resolver: resolver, locale: locale}, nil
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
		if retryErr := p.store.Retry(ctx, job, err, 5*time.Second); retryErr != nil {
			return true, fmt.Errorf("fetch approval instance: %v; schedule retry: %w", err, retryErr)
		}
		return true, fmt.Errorf("fetch approval instance: %w", err)
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
