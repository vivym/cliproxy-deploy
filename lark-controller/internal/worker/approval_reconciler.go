package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
)

type ApprovalInstancePage struct {
	InstanceCodes []string
	NextPageToken string
	HasMore       bool
}

type ApprovalInstanceLister interface {
	ListInstanceCodes(
		context.Context,
		string,
		time.Time,
		time.Time,
		string,
		int,
	) (ApprovalInstancePage, error)
}

type ApprovalReconciliationBinding struct {
	ApprovalCode string
	ScanUntil    time.Time
}

type ApprovalReconcilerConfig struct {
	Store                  *inbox.Store
	InstanceLister         ApprovalInstanceLister
	InstanceFetcher        ApprovalFetcher
	Bindings               []ApprovalReconciliationBinding
	AppID                  string
	TenantKey              string
	Locale                 string
	InitialLookback        time.Duration
	Overlap                time.Duration
	PageSize               int
	MinimumRequestInterval time.Duration
	RequestPacer           RequestPacer
	Now                    func() time.Time
}

type ApprovalReconciler struct {
	config ApprovalReconcilerConfig
}

const (
	defaultApprovalReconciliationLookback = 72 * time.Hour
	defaultApprovalReconciliationOverlap  = 10 * time.Minute
	maxApprovalReconciliationLookback     = 30 * 24 * time.Hour
	maxApprovalReconciliationWindow       = 10 * time.Hour
	maxApprovalReconciliationPages        = 10_000
	maxApprovalReconciliationBindings     = 1_000
	maxApprovalReconciliationInstances    = 100_000
)

var errIncompleteApprovalReconciliation = errors.New("approval reconciliation scan is incomplete")

type approvalReconciliationFailures struct {
	count               int
	preferred           error
	preferredRetryAfter time.Duration
}

func (f *approvalReconciliationFailures) Add(err error) {
	if err == nil {
		return
	}
	f.count++
	var failure *ApprovalFetchError
	if errors.As(err, &failure) && failure != nil && failure.Retryable &&
		failure.RetryAfter > f.preferredRetryAfter {
		f.preferred = err
		f.preferredRetryAfter = failure.RetryAfter
		return
	}
	if f.preferred == nil {
		f.preferred = err
	}
}

func (f *approvalReconciliationFailures) Err() error {
	if f.count == 0 {
		return nil
	}
	return fmt.Errorf(
		"approval reconciliation completed with %d failure(s): %w",
		f.count,
		f.preferred,
	)
}

func NewApprovalReconciler(config ApprovalReconcilerConfig) (*ApprovalReconciler, error) {
	if config.Store == nil || isNilDependency(config.InstanceLister) ||
		isNilDependency(config.InstanceFetcher) || config.AppID == "" ||
		config.TenantKey == "" || config.Locale == "" {
		return nil, errors.New("approval reconciliation dependencies and identity are required")
	}
	if len(config.Bindings) > maxApprovalReconciliationBindings {
		return nil, errors.New("approval reconciliation binding limit exceeded")
	}
	if config.InitialLookback == 0 {
		config.InitialLookback = defaultApprovalReconciliationLookback
	}
	if config.InitialLookback <= 0 || config.InitialLookback > maxApprovalReconciliationLookback {
		return nil, errors.New("approval reconciliation lookback must be between 1ns and 720h")
	}
	if config.Overlap == 0 {
		config.Overlap = defaultApprovalReconciliationOverlap
	}
	if config.Overlap < 0 || config.Overlap >= config.InitialLookback ||
		config.Overlap >= maxApprovalReconciliationWindow {
		return nil, errors.New("approval reconciliation overlap must be non-negative and bounded")
	}
	if config.PageSize == 0 {
		config.PageSize = 100
	}
	if config.PageSize < 1 || config.PageSize > 100 {
		return nil, errors.New("approval reconciliation page size must be between 1 and 100")
	}
	if config.MinimumRequestInterval < 0 {
		return nil, errors.New("approval reconciliation request interval must not be negative")
	}
	if isNilDependency(config.RequestPacer) {
		var err error
		config.RequestPacer, err = NewRequestPacer(config.MinimumRequestInterval)
		if err != nil {
			return nil, err
		}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	bindings := append([]ApprovalReconciliationBinding(nil), config.Bindings...)
	sort.Slice(bindings, func(left, right int) bool {
		return bindings[left].ApprovalCode < bindings[right].ApprovalCode
	})
	for index, binding := range bindings {
		if !validApprovalReconciliationID(binding.ApprovalCode) ||
			(index > 0 && bindings[index-1].ApprovalCode == binding.ApprovalCode) {
			return nil, errors.New("approval reconciliation bindings must have unique valid codes")
		}
		bindings[index].ScanUntil = binding.ScanUntil.UTC()
	}
	config.Bindings = bindings
	return &ApprovalReconciler{config: config}, nil
}

func (r *ApprovalReconciler) RunOnce(ctx context.Context) (bool, error) {
	runEnd := r.config.Now().UTC()
	if runEnd.IsZero() {
		return false, errors.New("approval reconciliation clock returned zero time")
	}
	seenTargets := make(map[ApprovalRecheckTarget]struct{})
	processed := false
	var failures approvalReconciliationFailures
	for _, binding := range r.config.Bindings {
		scanEnd := runEnd
		if !binding.ScanUntil.IsZero() && binding.ScanUntil.Before(scanEnd) {
			scanEnd = binding.ScanUntil
		}
		cursor, found, err := r.config.Store.ApprovalReconciliationCursor(ctx, binding.ApprovalCode)
		if err != nil {
			return processed, err
		}
		scanStart := scanEnd.Add(-r.config.InitialLookback)
		if found {
			if !cursor.Before(scanEnd) {
				continue
			}
			scanStart = cursor.Add(-r.config.Overlap)
		}
		for scanStart.Before(scanEnd) {
			windowEnd := minTime(scanStart.Add(maxApprovalReconciliationWindow), scanEnd)
			count, err := r.scanWindow(
				ctx,
				r.config.RequestPacer,
				binding.ApprovalCode,
				scanStart,
				windowEnd,
				seenTargets,
			)
			if err != nil {
				if ctx.Err() != nil {
					return true, ctx.Err()
				}
				result := approvalReconciliationFailureResult(err)
				if auditErr := r.config.Store.FailApprovalReconciliationWindow(
					ctx,
					binding.ApprovalCode,
					scanStart,
					windowEnd,
					result,
					count,
				); auditErr != nil {
					return true, auditErr
				}
				processed = true
				failures.Add(err)
				if approvalReconciliationRateLimited(err) {
					return processed, failures.Err()
				}
				break
			}
			if err := r.config.Store.CompleteApprovalReconciliationWindow(
				ctx,
				binding.ApprovalCode,
				scanStart,
				windowEnd,
				count,
			); err != nil {
				return true, err
			}
			processed = true
			scanStart = windowEnd
		}
	}

	targets, err := r.config.Store.ListApprovalRecheckTargets(ctx, r.config.TenantKey)
	if err != nil {
		failures.Add(err)
		return processed, failures.Err()
	}
	for _, target := range targets {
		targetKey := ApprovalRecheckTarget{
			ApprovalCode: target.ApprovalCode,
			InstanceCode: target.InstanceCode,
		}
		if _, alreadyFetched := seenTargets[targetKey]; alreadyFetched {
			continue
		}
		if err := r.config.RequestPacer.Wait(ctx); err != nil {
			return processed, err
		}
		instance, err := r.config.InstanceFetcher.Fetch(ctx, target.InstanceCode, r.config.Locale)
		if err != nil {
			if ctx.Err() != nil {
				return true, ctx.Err()
			}
			if auditErr := r.config.Store.FailApprovalReconciliationWindow(
				ctx,
				target.ApprovalCode,
				runEnd,
				runEnd,
				approvalReconciliationFailureResult(err),
				0,
			); auditErr != nil {
				return true, auditErr
			}
			processed = true
			failures.Add(err)
			if approvalReconciliationRateLimited(err) {
				return processed, failures.Err()
			}
			continue
		}
		if _, err := r.recordAuthorityInstance(
			ctx,
			target.ApprovalCode,
			target.InstanceCode,
			instance,
		); err != nil {
			if auditErr := r.config.Store.FailApprovalReconciliationWindow(
				ctx,
				target.ApprovalCode,
				runEnd,
				runEnd,
				approvalReconciliationFailureResult(err),
				0,
			); auditErr != nil {
				return true, auditErr
			}
			processed = true
			failures.Add(err)
			if approvalReconciliationRateLimited(err) {
				return processed, failures.Err()
			}
			continue
		}
		processed = true
	}
	return processed, failures.Err()
}

type ApprovalRecheckTarget struct {
	ApprovalCode string
	InstanceCode string
}

func (r *ApprovalReconciler) scanWindow(
	ctx context.Context,
	pacer RequestPacer,
	approvalCode string,
	windowStart time.Time,
	windowEnd time.Time,
	seenTargets map[ApprovalRecheckTarget]struct{},
) (int, error) {
	pageToken := ""
	seenPageTokens := make(map[string]struct{})
	seenInstanceCodes := make(map[string]struct{})
	instanceCount := 0
	for pageCount := 1; pageCount <= maxApprovalReconciliationPages; pageCount++ {
		if err := pacer.Wait(ctx); err != nil {
			return instanceCount, err
		}
		page, err := r.config.InstanceLister.ListInstanceCodes(
			ctx,
			approvalCode,
			windowStart,
			windowEnd,
			pageToken,
			r.config.PageSize,
		)
		if err != nil {
			return instanceCount, err
		}
		for _, instanceCode := range page.InstanceCodes {
			if instanceCount >= maxApprovalReconciliationInstances {
				return instanceCount, fmt.Errorf(
					"%w: instance limit exceeded",
					errIncompleteApprovalReconciliation,
				)
			}
			if !validApprovalReconciliationID(instanceCode) {
				return instanceCount, fmt.Errorf(
					"%w: invalid instance code",
					errIncompleteApprovalReconciliation,
				)
			}
			if _, duplicate := seenInstanceCodes[instanceCode]; duplicate {
				return instanceCount, fmt.Errorf(
					"%w: duplicate instance code",
					errIncompleteApprovalReconciliation,
				)
			}
			seenInstanceCodes[instanceCode] = struct{}{}
			instanceCount++
			target := ApprovalRecheckTarget{ApprovalCode: approvalCode, InstanceCode: instanceCode}
			if _, alreadyFetched := seenTargets[target]; alreadyFetched {
				continue
			}
			if err := pacer.Wait(ctx); err != nil {
				return instanceCount, err
			}
			instance, err := r.config.InstanceFetcher.Fetch(ctx, instanceCode, r.config.Locale)
			if err != nil {
				return instanceCount, err
			}
			if _, err := r.recordAuthorityInstance(ctx, approvalCode, instanceCode, instance); err != nil {
				return instanceCount, err
			}
			seenTargets[target] = struct{}{}
		}
		if !page.HasMore {
			return instanceCount, nil
		}
		if page.NextPageToken == "" {
			return instanceCount, fmt.Errorf(
				"%w: missing next page token",
				errIncompleteApprovalReconciliation,
			)
		}
		if _, duplicate := seenPageTokens[page.NextPageToken]; duplicate {
			return instanceCount, fmt.Errorf(
				"%w: repeated page token",
				errIncompleteApprovalReconciliation,
			)
		}
		seenPageTokens[page.NextPageToken] = struct{}{}
		pageToken = page.NextPageToken
	}
	return instanceCount, fmt.Errorf(
		"%w: page limit exceeded",
		errIncompleteApprovalReconciliation,
	)
}

func (r *ApprovalReconciler) recordAuthorityInstance(
	ctx context.Context,
	approvalCode string,
	instanceCode string,
	instance ApprovalInstance,
) (bool, error) {
	if instance.ApprovalCode != approvalCode || instance.InstanceCode != instanceCode ||
		instance.Status == "" {
		return false, &ApprovalFetchError{Reason: ApprovalFetchInvalidResponse}
	}
	reverted := instance.Reverted
	status := "APPROVED"
	if reverted {
		status = "REVERTED"
	} else if instance.Status != "APPROVED" {
		return false, nil
	}
	projected, err := r.config.Store.HasApprovalAuthorityProjection(
		ctx,
		r.config.TenantKey,
		approvalCode,
		instanceCode,
		reverted,
	)
	if err != nil || projected {
		return false, err
	}
	payload := struct {
		ApprovalCode         string `json:"approval_code"`
		InstanceCode         string `json:"instance_code"`
		Status               string `json:"status"`
		RevertedInstanceCode string `json:"reverted_instance_code,omitempty"`
	}{
		ApprovalCode: approvalCode,
		InstanceCode: instanceCode,
		Status:       status,
	}
	if reverted {
		payload.RevertedInstanceCode = instanceCode
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return false, errors.New("encode reconciled approval event")
	}
	eventID := reconciledApprovalEventID(
		r.config.TenantKey,
		approvalCode,
		instanceCode,
		status,
	)
	duplicate, err := r.config.Store.Record(ctx, inbox.Event{
		Key:                  "lark:reconcile:" + eventID,
		Origin:               inbox.EventOriginApprovalReconciliation,
		SchemaVersion:        "2.0",
		EventID:              eventID,
		EventType:            "approval.instance.status_changed_v4",
		AppID:                r.config.AppID,
		TenantKey:            r.config.TenantKey,
		ApprovalCode:         approvalCode,
		InstanceCode:         instanceCode,
		Status:               status,
		PayloadJSON:          string(normalized),
		RevertedInstanceCode: payload.RevertedInstanceCode,
	})
	if err != nil {
		return false, err
	}
	return !duplicate, nil
}

func reconciledApprovalEventID(
	tenantKey string,
	approvalCode string,
	instanceCode string,
	status string,
) string {
	digest := sha256.Sum256([]byte(strings.Join(
		[]string{tenantKey, approvalCode, instanceCode, status},
		"\x00",
	)))
	return hex.EncodeToString(digest[:])
}

func approvalReconciliationFailureResult(err error) inbox.ApprovalReconciliationResult {
	if errors.Is(err, errIncompleteApprovalReconciliation) {
		return inbox.ApprovalReconciliationResultIncompleteScan
	}
	var failure *ApprovalFetchError
	if errors.As(err, &failure) && failure != nil {
		switch failure.Reason {
		case ApprovalFetchRateLimited:
			return inbox.ApprovalReconciliationResultRateLimited
		case ApprovalFetchServerError:
			return inbox.ApprovalReconciliationResultServerError
		case ApprovalFetchClientError:
			return inbox.ApprovalReconciliationResultClientError
		case ApprovalFetchTimeout:
			return inbox.ApprovalReconciliationResultTimeout
		case ApprovalFetchTransportError:
			return inbox.ApprovalReconciliationResultTransportError
		case ApprovalFetchInvalidResponse:
			return inbox.ApprovalReconciliationResultInvalidResponse
		default:
		}
	}
	return inbox.ApprovalReconciliationResultUnclassifiedError
}

func approvalReconciliationRateLimited(err error) bool {
	var failure *ApprovalFetchError
	return errors.As(err, &failure) && failure != nil &&
		failure.Reason == ApprovalFetchRateLimited
}

func validApprovalReconciliationID(value string) bool {
	return value != "" && len(value) <= 512 && value == strings.TrimSpace(value)
}

func minTime(left time.Time, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}
