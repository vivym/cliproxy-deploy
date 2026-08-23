package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/digest"
)

type EntitlementGrantJobStatus string

type EntitlementGrantFailureReason string

const (
	EntitlementGrantJobStatusHeldShadow      EntitlementGrantJobStatus = "held_shadow"
	EntitlementGrantJobStatusPending         EntitlementGrantJobStatus = "pending"
	EntitlementGrantJobStatusProcessing      EntitlementGrantJobStatus = "processing"
	EntitlementGrantJobStatusRetryWait       EntitlementGrantJobStatus = "retry_wait"
	EntitlementGrantJobStatusReversalPending EntitlementGrantJobStatus = "reversal_pending"
	EntitlementGrantJobStatusSucceeded       EntitlementGrantJobStatus = "succeeded"
	EntitlementGrantJobStatusDeadLetter      EntitlementGrantJobStatus = "dead_letter"
)

const (
	EntitlementGrantFailureInvalidRequest                EntitlementGrantFailureReason = "invalid_request"
	EntitlementGrantFailureIntegrationUnauthorized       EntitlementGrantFailureReason = "integration_unauthorized"
	EntitlementGrantFailurePrincipalNotReady             EntitlementGrantFailureReason = "principal_not_ready"
	EntitlementGrantFailurePrincipalDisabled             EntitlementGrantFailureReason = "principal_disabled"
	EntitlementGrantFailureUnmanagedSubscriptionConflict EntitlementGrantFailureReason = "unmanaged_subscription_conflict"
	EntitlementGrantFailurePolicyVersionMismatch         EntitlementGrantFailureReason = "policy_version_mismatch"
	EntitlementGrantFailureApprovalBindingMismatch       EntitlementGrantFailureReason = "approval_binding_mismatch"
	EntitlementGrantFailureTemporarilyUnavailable        EntitlementGrantFailureReason = "temporarily_unavailable"
	EntitlementGrantFailureExternalIDPayloadMismatch     EntitlementGrantFailureReason = "external_id_payload_mismatch"
	EntitlementGrantFailureUnknownPackage                EntitlementGrantFailureReason = "unknown_package"
	EntitlementGrantFailureUnknownLevel                  EntitlementGrantFailureReason = "unknown_level"
	EntitlementGrantFailureQuotaOutOfRange               EntitlementGrantFailureReason = "quota_out_of_range"
	EntitlementGrantFailureTimeout                       EntitlementGrantFailureReason = "timeout"
	EntitlementGrantFailureTransport                     EntitlementGrantFailureReason = "transport_error"
	EntitlementGrantFailureInvalidResponse               EntitlementGrantFailureReason = "invalid_response"
	EntitlementGrantFailureInvalidSealedPayload          EntitlementGrantFailureReason = "invalid_sealed_payload"
	EntitlementGrantFailureUnclassified                  EntitlementGrantFailureReason = "unclassified_error"
	EntitlementGrantFailureRetryExhaustedPrincipal       EntitlementGrantFailureReason = "retry_exhausted_principal_not_ready"
	EntitlementGrantFailureRetryExhaustedUnavailable     EntitlementGrantFailureReason = "retry_exhausted_temporarily_unavailable"
	EntitlementGrantFailureRetryExhaustedTimeout         EntitlementGrantFailureReason = "retry_exhausted_timeout"
	EntitlementGrantFailureRetryExhaustedTransport       EntitlementGrantFailureReason = "retry_exhausted_transport_error"
)

type entitlementGrantFailureMetadata struct {
	retryable bool
	exhausted EntitlementGrantFailureReason
}

var entitlementGrantFailureCatalog = map[EntitlementGrantFailureReason]entitlementGrantFailureMetadata{
	EntitlementGrantFailureInvalidRequest:                {},
	EntitlementGrantFailureIntegrationUnauthorized:       {},
	EntitlementGrantFailurePrincipalNotReady:             {retryable: true, exhausted: EntitlementGrantFailureRetryExhaustedPrincipal},
	EntitlementGrantFailurePrincipalDisabled:             {},
	EntitlementGrantFailureUnmanagedSubscriptionConflict: {},
	EntitlementGrantFailurePolicyVersionMismatch:         {},
	EntitlementGrantFailureApprovalBindingMismatch:       {},
	EntitlementGrantFailureTemporarilyUnavailable:        {retryable: true, exhausted: EntitlementGrantFailureRetryExhaustedUnavailable},
	EntitlementGrantFailureExternalIDPayloadMismatch:     {},
	EntitlementGrantFailureUnknownPackage:                {},
	EntitlementGrantFailureUnknownLevel:                  {},
	EntitlementGrantFailureQuotaOutOfRange:               {},
	EntitlementGrantFailureTimeout:                       {retryable: true, exhausted: EntitlementGrantFailureRetryExhaustedTimeout},
	EntitlementGrantFailureTransport:                     {retryable: true, exhausted: EntitlementGrantFailureRetryExhaustedTransport},
	EntitlementGrantFailureInvalidResponse:               {},
	EntitlementGrantFailureInvalidSealedPayload:          {},
	EntitlementGrantFailureUnclassified:                  {},
	EntitlementGrantFailureRetryExhaustedPrincipal:       {},
	EntitlementGrantFailureRetryExhaustedUnavailable:     {},
	EntitlementGrantFailureRetryExhaustedTimeout:         {},
	EntitlementGrantFailureRetryExhaustedTransport:       {},
}

func EntitlementGrantFailureReasons() []EntitlementGrantFailureReason {
	reasons := make([]EntitlementGrantFailureReason, 0, len(entitlementGrantFailureCatalog))
	for reason := range entitlementGrantFailureCatalog {
		reasons = append(reasons, reason)
	}
	sort.Slice(reasons, func(left, right int) bool { return reasons[left] < reasons[right] })
	return reasons
}

func ParseEntitlementGrantFailureReason(value string) EntitlementGrantFailureReason {
	reason := EntitlementGrantFailureReason(value)
	if knownEntitlementGrantFailure(reason) {
		return reason
	}
	return EntitlementGrantFailureUnclassified
}

func IsRetryableEntitlementGrantFailure(reason EntitlementGrantFailureReason) bool {
	metadata, ok := entitlementGrantFailureCatalog[reason]
	return ok && metadata.retryable
}

func ExhaustedEntitlementGrantFailure(
	reason EntitlementGrantFailureReason,
) EntitlementGrantFailureReason {
	metadata, ok := entitlementGrantFailureCatalog[reason]
	if !ok || !metadata.retryable || metadata.exhausted == "" {
		return EntitlementGrantFailureUnclassified
	}
	return metadata.exhausted
}

type EntitlementGrantJobDraft struct {
	ExternalID    string
	RequestSHA256 string
	SubjectSHA256 string
	KeyID         string
	Nonce         []byte
	Ciphertext    []byte
}

type EntitlementGrantJob struct {
	ID            int64
	ExternalID    string
	RequestSHA256 string
	SubjectSHA256 string
	KeyID         string
	Nonce         []byte
	Ciphertext    []byte
	Status        EntitlementGrantJobStatus
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	ActivatedAt   time.Time
	Receipt       *EntitlementGrantReceipt
	CompletedAt   time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type EntitlementGrantReceipt struct {
	ExternalID        string
	Status            string
	UserID            int64
	GrantType         string
	QuotaDelta        int64
	LevelCode         string
	SubscriptionID    int64
	AssignmentVersion int64
	Transition        string
}

func insertEntitlementGrantJob(
	ctx context.Context,
	tx *sql.Tx,
	draft EntitlementGrantJobDraft,
	createdAt string,
) (int64, error) {
	if draft.ExternalID == "" || !digest.IsCanonicalSHA256(draft.RequestSHA256) ||
		!digest.IsCanonicalSHA256(draft.SubjectSHA256) || !digest.IsCanonicalSHA256(draft.KeyID) ||
		len(draft.Nonce) != 12 || len(draft.Ciphertext) <= 16 {
		return 0, errors.New("invalid entitlement grant job")
	}
	var existingID int64
	var existingRequestSHA256 string
	err := tx.QueryRowContext(ctx, `
SELECT id, request_sha256 FROM entitlement_grant_jobs WHERE external_id = ?`,
		draft.ExternalID,
	).Scan(&existingID, &existingRequestSHA256)
	switch {
	case err == nil && existingRequestSHA256 == draft.RequestSHA256:
		return existingID, nil
	case err == nil:
		return 0, ErrEntitlementCommandPayloadMismatch
	case !errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("inspect entitlement grant job replay: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO entitlement_grant_jobs (
    external_id, request_sha256, subject_sha256, key_id, nonce, ciphertext,
    status, attempts, next_attempt_at, last_error, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, '', ?, ?)`,
		draft.ExternalID, draft.RequestSHA256, draft.SubjectSHA256, draft.KeyID,
		draft.Nonce, draft.Ciphertext, EntitlementGrantJobStatusHeldShadow,
		createdAt, createdAt, createdAt,
	)
	if err != nil {
		return 0, fmt.Errorf("store entitlement grant job: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("inspect entitlement grant job insert: %w", err)
	}
	return id, nil
}

func (s *Store) GetEntitlementGrantJob(
	ctx context.Context,
	externalID string,
) (EntitlementGrantJob, error) {
	if externalID == "" {
		return EntitlementGrantJob{}, errors.New("external ID is required")
	}
	return scanEntitlementGrantJob(s.database.QueryRowContext(ctx, `
SELECT id, external_id, request_sha256, subject_sha256, key_id, nonce, ciphertext,
       status, attempts, next_attempt_at, last_error, activated_at,
       response_status, response_user_id, result_grant_type, result_quota_delta,
       result_level_code, result_subscription_id, result_assignment_version,
       result_transition, completed_at, created_at, updated_at
FROM entitlement_grant_jobs WHERE external_id = ?`, externalID))
}

func (s *Store) ReleaseHeldEntitlementGrantJobs(
	ctx context.Context,
	activeBasePolicyVersion string,
) (int64, error) {
	if activeBasePolicyVersion == "" {
		return 0, errors.New("active base policy version is required")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.database.ExecContext(ctx, `
UPDATE entitlement_grant_jobs
SET status = ?, next_attempt_at = ?, activated_at = ?, updated_at = ?
WHERE status = ? AND (
    EXISTS (
        SELECT 1 FROM entitlement_command_shadows
        WHERE entitlement_command_shadows.external_id = entitlement_grant_jobs.external_id
    )
    OR EXISTS (
        SELECT 1 FROM base_subscription_grants
        WHERE base_subscription_grants.external_id = entitlement_grant_jobs.external_id
          AND base_subscription_grants.policy_version = ?
    )
)`,
		EntitlementGrantJobStatusPending,
		now,
		now,
		now,
		EntitlementGrantJobStatusHeldShadow,
		activeBasePolicyVersion,
	)
	if err != nil {
		return 0, fmt.Errorf("release held entitlement grant jobs: %w", err)
	}
	released, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect released entitlement grant jobs: %w", err)
	}
	return released, nil
}

func (s *Store) ClaimNextEntitlementGrantJob(
	ctx context.Context,
) (EntitlementGrantJob, bool, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return EntitlementGrantJob{}, false, fmt.Errorf("begin entitlement grant job claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	job, err := scanEntitlementGrantJob(tx.QueryRowContext(ctx, `
SELECT id, external_id, request_sha256, subject_sha256, key_id, nonce, ciphertext,
       status, attempts, next_attempt_at, last_error, activated_at,
       response_status, response_user_id, result_grant_type, result_quota_delta,
       result_level_code, result_subscription_id, result_assignment_version,
       result_transition, completed_at, created_at, updated_at
FROM entitlement_grant_jobs
WHERE status IN (?, ?) AND julianday(next_attempt_at) <= julianday(?)
ORDER BY id
LIMIT 1`,
		EntitlementGrantJobStatusPending,
		EntitlementGrantJobStatusRetryWait,
		now.Format(time.RFC3339Nano),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return EntitlementGrantJob{}, false, nil
	}
	if err != nil {
		return EntitlementGrantJob{}, false, fmt.Errorf("select entitlement grant job: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE entitlement_grant_jobs
SET status = ?, attempts = attempts + 1, updated_at = ?
WHERE id = ? AND status IN (?, ?)`,
		EntitlementGrantJobStatusProcessing,
		now.Format(time.RFC3339Nano),
		job.ID,
		EntitlementGrantJobStatusPending,
		EntitlementGrantJobStatusRetryWait,
	)
	if err != nil {
		return EntitlementGrantJob{}, false, fmt.Errorf("claim entitlement grant job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return EntitlementGrantJob{}, false, fmt.Errorf(
			"claim entitlement grant job affected %d rows: %w",
			affected,
			err,
		)
	}
	if err := tx.Commit(); err != nil {
		return EntitlementGrantJob{}, false, fmt.Errorf("commit entitlement grant job claim: %w", err)
	}
	job.Status = EntitlementGrantJobStatusProcessing
	job.Attempts++
	job.UpdatedAt = now
	return job, true, nil
}

func (s *Store) CompleteEntitlementGrantJob(
	ctx context.Context,
	job EntitlementGrantJob,
	receipt EntitlementGrantReceipt,
) error {
	if job.ID <= 0 || job.Status != EntitlementGrantJobStatusProcessing ||
		receipt.ExternalID != job.ExternalID || !validEntitlementGrantReceipt(receipt) {
		return errors.New("invalid entitlement grant completion")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin entitlement grant completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `
UPDATE entitlement_grant_jobs
SET status = ?, last_error = '', response_status = ?, response_user_id = ?,
    result_grant_type = ?, result_quota_delta = ?, result_level_code = ?,
    result_subscription_id = ?, result_assignment_version = ?, result_transition = ?,
    completed_at = ?, updated_at = ?
WHERE id = ? AND status = ? AND attempts = ?`,
		EntitlementGrantJobStatusSucceeded,
		receipt.Status,
		receipt.UserID,
		receipt.GrantType,
		receipt.QuotaDelta,
		receipt.LevelCode,
		receipt.SubscriptionID,
		receipt.AssignmentVersion,
		receipt.Transition,
		now,
		now,
		job.ID,
		EntitlementGrantJobStatusProcessing,
		job.Attempts,
	)
	if err != nil {
		return fmt.Errorf("complete entitlement grant job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect entitlement grant completion: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("complete entitlement grant job affected %d rows", affected)
	}
	if err := insertEntitlementGrantAudit(
		ctx,
		tx,
		job.ExternalID,
		"new_api_grant",
		receipt.Status,
		now,
	); err != nil {
		return fmt.Errorf("store entitlement grant completion audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit entitlement grant completion: %w", err)
	}
	return nil
}

func (s *Store) RetryEntitlementGrantJob(
	ctx context.Context,
	job EntitlementGrantJob,
	reason EntitlementGrantFailureReason,
	delay time.Duration,
) error {
	if delay <= 0 || !IsRetryableEntitlementGrantFailure(reason) {
		return errors.New("invalid entitlement grant retry")
	}
	return s.transitionEntitlementGrantJobFailure(
		ctx,
		job,
		reason,
		EntitlementGrantJobStatusRetryWait,
		delay,
	)
}

func (s *Store) DeadLetterEntitlementGrantJob(
	ctx context.Context,
	job EntitlementGrantJob,
	reason EntitlementGrantFailureReason,
) error {
	if !knownEntitlementGrantFailure(reason) {
		return errors.New("invalid entitlement grant terminal failure")
	}
	return s.transitionEntitlementGrantJobFailure(
		ctx,
		job,
		reason,
		EntitlementGrantJobStatusDeadLetter,
		0,
	)
}

func (s *Store) transitionEntitlementGrantJobFailure(
	ctx context.Context,
	job EntitlementGrantJob,
	reason EntitlementGrantFailureReason,
	status EntitlementGrantJobStatus,
	delay time.Duration,
) error {
	if job.ID <= 0 || job.Status != EntitlementGrantJobStatusProcessing || job.Attempts <= 0 {
		return errors.New("invalid entitlement grant failure transition")
	}
	now := time.Now().UTC()
	nextAttemptAt := now
	completedAt := ""
	auditAction := ""
	if status == EntitlementGrantJobStatusRetryWait {
		nextAttemptAt = now.Add(delay)
		auditAction = "entitlement_grant_retry"
	} else if status == EntitlementGrantJobStatusDeadLetter {
		completedAt = now.Format(time.RFC3339Nano)
		auditAction = "entitlement_grant_dead_letter"
	} else {
		return errors.New("invalid entitlement grant failure status")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin entitlement grant failure transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
UPDATE entitlement_grant_jobs
SET status = ?, next_attempt_at = ?, last_error = ?, completed_at = ?, updated_at = ?
WHERE id = ? AND status = ? AND attempts = ?`,
		status,
		nextAttemptAt.Format(time.RFC3339Nano),
		reason,
		completedAt,
		now.Format(time.RFC3339Nano),
		job.ID,
		EntitlementGrantJobStatusProcessing,
		job.Attempts,
	)
	if err != nil {
		return fmt.Errorf("transition entitlement grant job failure: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect entitlement grant failure transition: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("transition entitlement grant job failure affected %d rows", affected)
	}
	if err := insertEntitlementGrantAudit(
		ctx,
		tx,
		job.ExternalID,
		auditAction,
		string(reason),
		now.Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("store entitlement grant failure audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit entitlement grant failure transition: %w", err)
	}
	return nil
}

func (s *Store) ValidateEntitlementGrantJobKeyIDs(ctx context.Context, keyIDs []string) error {
	if len(keyIDs) == 0 {
		return errors.New("grant payload keyring must contain at least one key ID")
	}
	available := make(map[string]struct{}, len(keyIDs))
	for _, keyID := range keyIDs {
		if !digest.IsCanonicalSHA256(keyID) {
			return errors.New("grant payload keyring contains an invalid key ID")
		}
		if _, duplicate := available[keyID]; duplicate {
			return errors.New("grant payload keyring contains a duplicate key ID")
		}
		available[keyID] = struct{}{}
	}
	rows, err := s.database.QueryContext(ctx, `
SELECT DISTINCT key_id FROM entitlement_grant_jobs
WHERE status NOT IN (?, ?)`,
		EntitlementGrantJobStatusSucceeded,
		EntitlementGrantJobStatusDeadLetter,
	)
	if err != nil {
		return fmt.Errorf("validate grant job keyring: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var keyID string
		if err := rows.Scan(&keyID); err != nil {
			return fmt.Errorf("scan grant job key ID: %w", err)
		}
		if _, ok := available[keyID]; !ok {
			return errors.New("configured grant payload keyring cannot open nonterminal grant jobs")
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate grant job key IDs: %w", err)
	}
	return nil
}

type entitlementGrantJobScanner interface {
	Scan(...any) error
}

func scanEntitlementGrantJob(scanner entitlementGrantJobScanner) (EntitlementGrantJob, error) {
	var job EntitlementGrantJob
	var nextAttemptAt string
	var activatedAt string
	var receipt EntitlementGrantReceipt
	var completedAt string
	var createdAt string
	var updatedAt string
	if err := scanner.Scan(
		&job.ID, &job.ExternalID, &job.RequestSHA256, &job.SubjectSHA256,
		&job.KeyID, &job.Nonce, &job.Ciphertext, &job.Status, &job.Attempts,
		&nextAttemptAt, &job.LastError, &activatedAt, &receipt.Status, &receipt.UserID,
		&receipt.GrantType, &receipt.QuotaDelta, &receipt.LevelCode,
		&receipt.SubscriptionID, &receipt.AssignmentVersion, &receipt.Transition,
		&completedAt, &createdAt, &updatedAt,
	); err != nil {
		return EntitlementGrantJob{}, err
	}
	var err error
	job.NextAttemptAt, err = time.Parse(time.RFC3339Nano, nextAttemptAt)
	if err != nil {
		return EntitlementGrantJob{}, fmt.Errorf("parse entitlement grant job next_attempt_at: %w", err)
	}
	job.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return EntitlementGrantJob{}, fmt.Errorf("parse entitlement grant job created_at: %w", err)
	}
	job.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return EntitlementGrantJob{}, fmt.Errorf("parse entitlement grant job updated_at: %w", err)
	}
	if activatedAt != "" {
		job.ActivatedAt, err = time.Parse(time.RFC3339Nano, activatedAt)
		if err != nil {
			return EntitlementGrantJob{}, fmt.Errorf("parse entitlement grant job activated_at: %w", err)
		}
	}
	if receipt.Status != "" {
		receipt.ExternalID = job.ExternalID
		job.Receipt = &receipt
	}
	if completedAt != "" {
		job.CompletedAt, err = time.Parse(time.RFC3339Nano, completedAt)
		if err != nil {
			return EntitlementGrantJob{}, fmt.Errorf("parse entitlement grant job completed_at: %w", err)
		}
	}
	return job, nil
}

func validEntitlementGrantReceipt(receipt EntitlementGrantReceipt) bool {
	if receipt.ExternalID == "" || receipt.UserID <= 0 {
		return false
	}
	switch receipt.Status {
	case "applied", "replayed", "noop", "ignored_stale":
	default:
		return false
	}
	switch receipt.GrantType {
	case "wallet_quota":
		return receipt.QuotaDelta > 0 && receipt.LevelCode == "" &&
			receipt.SubscriptionID == 0 && receipt.AssignmentVersion == 0 &&
			receipt.Transition == ""
	case "subscription_level":
		return receipt.QuotaDelta == 0 && receipt.LevelCode != "" &&
			receipt.SubscriptionID > 0 && receipt.AssignmentVersion > 0 &&
			validEntitlementSubscriptionTransition(receipt.Status, receipt.Transition)
	default:
		return false
	}
}

func validEntitlementSubscriptionTransition(status, transition string) bool {
	switch status {
	case "applied":
		return transition == "created" || transition == "updated"
	case "noop":
		return transition == "noop"
	case "ignored_stale":
		return transition == "ignored_stale"
	case "replayed":
		return transition == "created" || transition == "updated" ||
			transition == "noop" || transition == "ignored_stale"
	default:
		return false
	}
}

func knownEntitlementGrantFailure(reason EntitlementGrantFailureReason) bool {
	_, ok := entitlementGrantFailureCatalog[reason]
	return ok
}

func insertEntitlementGrantAudit(
	ctx context.Context,
	tx *sql.Tx,
	externalID string,
	action string,
	outcome string,
	createdAt string,
) error {
	var eventKey string
	err := tx.QueryRowContext(ctx, `
SELECT event_key FROM entitlement_command_shadows
WHERE external_id = ? ORDER BY created_at, event_key LIMIT 1`, externalID).Scan(&eventKey)
	if err == nil {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO controller_audit (event_key, action, outcome, created_at)
VALUES (?, ?, ?, ?)`, eventKey, action, outcome, createdAt); err != nil {
			return err
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("resolve entitlement grant audit event: %w", err)
	}
	var baseExternalID string
	if err := tx.QueryRowContext(ctx, `
SELECT external_id FROM base_subscription_grants WHERE external_id = ?`,
		externalID,
	).Scan(&baseExternalID); err != nil {
		return fmt.Errorf("resolve base subscription grant audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO base_subscription_audit (external_id, action, outcome, created_at)
VALUES (?, ?, ?, ?)`, baseExternalID, action, outcome, createdAt); err != nil {
		return err
	}
	return nil
}
