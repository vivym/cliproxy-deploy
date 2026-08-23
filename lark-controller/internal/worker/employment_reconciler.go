package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

type EmploymentStatus string

type EmploymentCheckFailureReason string

const (
	EmploymentStatusPresent  EmploymentStatus = "present"
	EmploymentStatusResigned EmploymentStatus = "resigned"
	EmploymentStatusExited   EmploymentStatus = "exited"
	EmploymentStatusNotFound EmploymentStatus = "not_found"
)

const (
	EmploymentCheckPermissionDenied EmploymentCheckFailureReason = "permission_denied"
	EmploymentCheckRateLimited      EmploymentCheckFailureReason = "rate_limited"
	EmploymentCheckServerError      EmploymentCheckFailureReason = "server_error"
	EmploymentCheckTimeout          EmploymentCheckFailureReason = "timeout"
	EmploymentCheckTransportError   EmploymentCheckFailureReason = "transport_error"
	EmploymentCheckInvalidResponse  EmploymentCheckFailureReason = "invalid_response"
	EmploymentCheckClientError      EmploymentCheckFailureReason = "client_error"
)

type EmploymentCheckError struct {
	Reason     EmploymentCheckFailureReason
	Retryable  bool
	RetryAfter time.Duration
	StatusCode int
	LarkCode   int
}

func (e *EmploymentCheckError) Error() string {
	return fmt.Sprintf("Lark employment check failed: %s", e.Reason)
}

type EmploymentCheckResult struct {
	Status         EmploymentStatus
	LarkResultCode int
}

type ActivePrincipalLister interface {
	ListActiveLarkPrincipals(context.Context, string, int) (newapi.PrincipalPage, error)
}

type EmploymentChecker interface {
	CheckEmployment(context.Context, string) (EmploymentCheckResult, error)
}

type PrincipalDisableSealer interface {
	SealPrincipalDisable(newapi.PrincipalDisableRequest) (newapi.SealedPrincipalDisableRequest, error)
}

type EmploymentReconcilerConfig struct {
	Store                  *inbox.Store
	PrincipalLister        ActivePrincipalLister
	EmploymentChecker      EmploymentChecker
	PrincipalDisableSealer PrincipalDisableSealer
	TenantKey              string
	HealthOpenID           string
	PageLimit              int
	MinimumCheckInterval   time.Duration
	RequestPacer           RequestPacer
	Now                    func() time.Time
}

type EmploymentReconciler struct {
	config EmploymentReconcilerConfig
}

const maxEmploymentReconciliationPages = 10_000

func NewEmploymentReconciler(config EmploymentReconcilerConfig) (*EmploymentReconciler, error) {
	if config.Store == nil || isNilDependency(config.PrincipalLister) ||
		isNilDependency(config.EmploymentChecker) || isNilDependency(config.PrincipalDisableSealer) ||
		config.TenantKey == "" || config.HealthOpenID == "" {
		return nil, errors.New("employment reconciliation dependencies and identity are required")
	}
	if config.PageLimit == 0 {
		config.PageLimit = 100
	}
	if config.PageLimit < 1 || config.PageLimit > 100 {
		return nil, errors.New("employment reconciliation page limit must be between 1 and 100")
	}
	if config.MinimumCheckInterval < 0 {
		return nil, errors.New("employment reconciliation check interval must not be negative")
	}
	if isNilDependency(config.RequestPacer) {
		var err error
		config.RequestPacer, err = NewRequestPacer(config.MinimumCheckInterval)
		if err != nil {
			return nil, err
		}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &EmploymentReconciler{config: config}, nil
}

func (r *EmploymentReconciler) RunOnce(ctx context.Context) (bool, error) {
	startedAt := r.config.Now().UTC()
	evidenceDate := startedAt.Format(time.DateOnly)
	completed, err := r.config.Store.HasCompletedEmploymentReconciliation(ctx, evidenceDate)
	if err != nil || completed {
		return false, err
	}
	checks := make([]inbox.EmploymentCheck, 0)
	checkEmployment := pacedEmploymentChecks(
		r.config.EmploymentChecker.CheckEmployment,
		r.config.RequestPacer,
	)
	if err := r.requireHealthyProbe(ctx, checkEmployment); err != nil {
		return r.fail(ctx, evidenceDate, startedAt, "health_probe_failed", checks, err)
	}
	cursor := ""
	seenCursors := make(map[string]struct{})
	seenSubjects := make(map[string]struct{})
	pageCount := 0
	for {
		pageCount++
		if pageCount > maxEmploymentReconciliationPages {
			return r.fail(
				ctx, evidenceDate, startedAt, "incomplete_scan", checks,
				errors.New("active principal scan exceeded its page limit"),
			)
		}
		page, err := r.config.PrincipalLister.ListActiveLarkPrincipals(ctx, cursor, r.config.PageLimit)
		if err != nil {
			return r.fail(
				ctx,
				evidenceDate,
				startedAt,
				"principal_list_failed",
				checks,
				fmt.Errorf("list active Lark principals: %w", err),
			)
		}
		for _, principal := range page.Principals {
			if _, duplicate := seenSubjects[principal.Subject]; duplicate {
				return r.fail(
					ctx, evidenceDate, startedAt, "incomplete_scan", checks,
					errors.New("active principal scan repeated a subject"),
				)
			}
			seenSubjects[principal.Subject] = struct{}{}
			check, err := r.checkPrincipal(ctx, evidenceDate, principal, checkEmployment)
			if check.SubjectSHA256 != "" {
				checks = append(checks, check)
			}
			if err != nil {
				return r.fail(
					ctx, evidenceDate, startedAt, "employment_check_failed", checks, err,
				)
			}
		}
		if page.ScanComplete {
			break
		}
		if page.NextCursor == "" {
			return r.fail(
				ctx,
				evidenceDate,
				startedAt,
				"incomplete_scan",
				checks,
				errors.New("active principal scan ended without completion"),
			)
		}
		if _, duplicate := seenCursors[page.NextCursor]; duplicate {
			return r.fail(
				ctx,
				evidenceDate,
				startedAt,
				"incomplete_scan",
				checks,
				errors.New("active principal scan repeated a cursor"),
			)
		}
		seenCursors[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	if err := r.requireHealthyProbe(ctx, checkEmployment); err != nil {
		return r.fail(ctx, evidenceDate, startedAt, "health_probe_failed", checks, err)
	}
	completedAt := r.config.Now().UTC()
	duplicate, err := r.config.Store.CompleteEmploymentReconciliation(ctx, inbox.EmploymentReconciliation{
		ReconciliationID:  "lark:employment-scan:" + evidenceDate,
		EvidenceDate:      evidenceDate,
		StartedAt:         startedAt,
		CompletedAt:       completedAt,
		PermissionHealthy: true,
		ScanComplete:      true,
		Checks:            checks,
	})
	return !duplicate, err
}

func (r *EmploymentReconciler) fail(
	ctx context.Context,
	evidenceDate string,
	startedAt time.Time,
	reason string,
	checks []inbox.EmploymentCheck,
	cause error,
) (bool, error) {
	failedChecks := make([]inbox.EmploymentCheck, 0, len(checks))
	for _, check := range checks {
		check.PermissionHealthy = false
		check.PrincipalDisableJob = nil
		var err error
		check.EvidenceSHA256, err = employmentEvidenceSHA256(check)
		if err != nil {
			return true, err
		}
		failedChecks = append(failedChecks, check)
	}
	if err := r.config.Store.FailEmploymentReconciliation(
		ctx,
		evidenceDate,
		startedAt,
		r.config.Now().UTC(),
		reason,
		failedChecks,
	); err != nil {
		return true, err
	}
	return true, cause
}

type employmentCheckFunc func(context.Context, string) (EmploymentCheckResult, error)

func pacedEmploymentChecks(
	check employmentCheckFunc,
	pacer RequestPacer,
) employmentCheckFunc {
	return func(ctx context.Context, openID string) (EmploymentCheckResult, error) {
		if err := pacer.Wait(ctx); err != nil {
			return EmploymentCheckResult{}, err
		}
		return check(ctx, openID)
	}
}

func (r *EmploymentReconciler) requireHealthyProbe(
	ctx context.Context,
	checkEmployment employmentCheckFunc,
) error {
	result, err := checkEmployment(ctx, r.config.HealthOpenID)
	if err != nil {
		return fmt.Errorf("employment reconciliation health probe: %w", err)
	}
	if result.Status != EmploymentStatusPresent || result.LarkResultCode != 0 {
		return errors.New("employment reconciliation health probe is not active")
	}
	return nil
}

func (r *EmploymentReconciler) checkPrincipal(
	ctx context.Context,
	evidenceDate string,
	principal newapi.Principal,
	checkEmployment employmentCheckFunc,
) (inbox.EmploymentCheck, error) {
	tenantKey, openID, ok := strings.Cut(principal.Subject, ":")
	if !ok || tenantKey != r.config.TenantKey || openID == "" || strings.Contains(openID, ":") {
		return inbox.EmploymentCheck{}, errors.New("active Lark principal has an invalid tenant subject")
	}
	subjectHash := sha256.Sum256([]byte(principal.Subject))
	result, err := checkEmployment(ctx, openID)
	check := inbox.EmploymentCheck{
		SubjectSHA256: hex.EncodeToString(subjectHash[:]),
		CheckedAt:     r.config.Now().UTC(),
	}
	if err != nil {
		check.Result = inbox.EmploymentCheckResultError
		var failure *EmploymentCheckError
		if errors.As(err, &failure) && failure != nil {
			check.LarkResultCode = failure.LarkCode
		}
		check.EvidenceSHA256, _ = employmentEvidenceSHA256(check)
		return check, fmt.Errorf("check Lark employment: %w", err)
	}
	check.Result = inbox.EmploymentCheckResult(result.Status)
	check.PermissionHealthy = true
	check.LarkResultCode = result.LarkResultCode
	if result.Status == EmploymentStatusPresent {
	} else {
		request, receipt, err := newapi.PlanEmploymentReconciliationPrincipalDisable(
			tenantKey,
			openID,
			evidenceDate,
			string(result.Status),
		)
		if err != nil {
			return inbox.EmploymentCheck{}, errors.New("plan employment reconciliation disable")
		}
		sealed, err := r.config.PrincipalDisableSealer.SealPrincipalDisable(request)
		if err != nil {
			return inbox.EmploymentCheck{}, errors.New("seal employment reconciliation disable")
		}
		check.SubjectSHA256 = receipt.SubjectSHA256
		check.PrincipalDisableJob = &inbox.PrincipalDisableJobDraft{
			ExternalID: sealed.ExternalID, RequestSHA256: sealed.RequestSHA256,
			SubjectSHA256: receipt.SubjectSHA256, KeyID: sealed.KeyID,
			Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		}
	}
	check.EvidenceSHA256, err = employmentEvidenceSHA256(check)
	if err != nil {
		return inbox.EmploymentCheck{}, err
	}
	return check, nil
}

func employmentEvidenceSHA256(check inbox.EmploymentCheck) (string, error) {
	payload, err := json.Marshal(struct {
		SubjectSHA256     string                      `json:"subject_sha256"`
		CheckedAt         string                      `json:"checked_at"`
		Result            inbox.EmploymentCheckResult `json:"result"`
		LarkResultCode    int                         `json:"lark_result_code"`
		PermissionHealthy bool                        `json:"permission_healthy"`
	}{
		SubjectSHA256:     check.SubjectSHA256,
		CheckedAt:         check.CheckedAt.UTC().Format(time.RFC3339Nano),
		Result:            check.Result,
		LarkResultCode:    check.LarkResultCode,
		PermissionHealthy: check.PermissionHealthy,
	})
	if err != nil {
		return "", errors.New("encode employment evidence")
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
