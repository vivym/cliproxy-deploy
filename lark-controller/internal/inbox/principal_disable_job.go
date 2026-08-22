package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/digest"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

type PrincipalDisableJobStatus string

const (
	PrincipalDisableJobStatusHeldShadow PrincipalDisableJobStatus = "held_shadow"
	PrincipalDisableJobStatusPending    PrincipalDisableJobStatus = "pending"
	PrincipalDisableJobStatusProcessing PrincipalDisableJobStatus = "processing"
	PrincipalDisableJobStatusRetryWait  PrincipalDisableJobStatus = "retry_wait"
	PrincipalDisableJobStatusSucceeded  PrincipalDisableJobStatus = "succeeded"
	PrincipalDisableJobStatusDeadLetter PrincipalDisableJobStatus = "dead_letter"
)

type PrincipalDisableJobDraft struct {
	ExternalID    string
	RequestSHA256 string
	SubjectSHA256 string
	KeyID         string
	Nonce         []byte
	Ciphertext    []byte
}

type PrincipalDisableJob struct {
	ID            int64
	EventKey      string
	ExternalID    string
	RequestSHA256 string
	SubjectSHA256 string
	KeyID         string
	Nonce         []byte
	Ciphertext    []byte
	Status        PrincipalDisableJobStatus
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	ActivatedAt   time.Time
	Receipt       *PrincipalDisableReceipt
	CompletedAt   time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type PrincipalDisableReceipt struct {
	ExternalID       string
	Status           string
	Outcome          string
	PrincipalVersion int64
	AuthVersion      int64
}

type PrincipalDisableFailureReason string

const (
	PrincipalDisableFailureInvalidRequest            PrincipalDisableFailureReason = "invalid_request"
	PrincipalDisableFailureIntegrationUnauthorized   PrincipalDisableFailureReason = "integration_unauthorized"
	PrincipalDisableFailureTemporarilyUnavailable    PrincipalDisableFailureReason = "temporarily_unavailable"
	PrincipalDisableFailureExternalIDPayloadMismatch PrincipalDisableFailureReason = "external_id_payload_mismatch"
	PrincipalDisableFailureTimeout                   PrincipalDisableFailureReason = "timeout"
	PrincipalDisableFailureTransport                 PrincipalDisableFailureReason = "transport_error"
	PrincipalDisableFailureInvalidResponse           PrincipalDisableFailureReason = "invalid_response"
	PrincipalDisableFailureInvalidSealedPayload      PrincipalDisableFailureReason = "invalid_sealed_payload"
	PrincipalDisableFailureUnclassified              PrincipalDisableFailureReason = "unclassified_error"
	PrincipalDisableFailureRetryExhaustedUnavailable PrincipalDisableFailureReason = "retry_exhausted_temporarily_unavailable"
	PrincipalDisableFailureRetryExhaustedTimeout     PrincipalDisableFailureReason = "retry_exhausted_timeout"
	PrincipalDisableFailureRetryExhaustedTransport   PrincipalDisableFailureReason = "retry_exhausted_transport_error"
)

type principalDisableFailureMetadata struct {
	retryable bool
	exhausted PrincipalDisableFailureReason
}

var principalDisableFailureCatalog = map[PrincipalDisableFailureReason]principalDisableFailureMetadata{
	PrincipalDisableFailureInvalidRequest:            {},
	PrincipalDisableFailureIntegrationUnauthorized:   {},
	PrincipalDisableFailureTemporarilyUnavailable:    {retryable: true, exhausted: PrincipalDisableFailureRetryExhaustedUnavailable},
	PrincipalDisableFailureExternalIDPayloadMismatch: {},
	PrincipalDisableFailureTimeout:                   {retryable: true, exhausted: PrincipalDisableFailureRetryExhaustedTimeout},
	PrincipalDisableFailureTransport:                 {retryable: true, exhausted: PrincipalDisableFailureRetryExhaustedTransport},
	PrincipalDisableFailureInvalidResponse:           {},
	PrincipalDisableFailureInvalidSealedPayload:      {},
	PrincipalDisableFailureUnclassified:              {},
	PrincipalDisableFailureRetryExhaustedUnavailable: {},
	PrincipalDisableFailureRetryExhaustedTimeout:     {},
	PrincipalDisableFailureRetryExhaustedTransport:   {},
}

func PrincipalDisableFailureReasons() []PrincipalDisableFailureReason {
	reasons := make([]PrincipalDisableFailureReason, 0, len(principalDisableFailureCatalog))
	for reason := range principalDisableFailureCatalog {
		reasons = append(reasons, reason)
	}
	sort.Slice(reasons, func(left, right int) bool { return reasons[left] < reasons[right] })
	return reasons
}

func ParsePrincipalDisableFailureReason(value string) PrincipalDisableFailureReason {
	reason := PrincipalDisableFailureReason(value)
	if _, ok := principalDisableFailureCatalog[reason]; ok {
		return reason
	}
	return PrincipalDisableFailureUnclassified
}

func IsRetryablePrincipalDisableFailure(reason PrincipalDisableFailureReason) bool {
	metadata, ok := principalDisableFailureCatalog[reason]
	return ok && metadata.retryable
}

func ExhaustedPrincipalDisableFailure(reason PrincipalDisableFailureReason) PrincipalDisableFailureReason {
	metadata, ok := principalDisableFailureCatalog[reason]
	if !ok || !metadata.retryable || metadata.exhausted == "" {
		return PrincipalDisableFailureUnclassified
	}
	return metadata.exhausted
}

func insertPrincipalDisableJob(
	ctx context.Context,
	tx *sql.Tx,
	eventKey string,
	draft PrincipalDisableJobDraft,
	createdAt string,
) (int64, error) {
	if draft.ExternalID == "" ||
		!digest.IsCanonicalSHA256(draft.RequestSHA256) ||
		!digest.IsCanonicalSHA256(draft.SubjectSHA256) ||
		!digest.IsCanonicalSHA256(draft.KeyID) || len(draft.Nonce) != 12 ||
		len(draft.Ciphertext) <= 16 {
		return 0, errors.New("invalid principal disable job")
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO principal_disable_jobs (
    event_key, external_id, request_sha256, subject_sha256, key_id, nonce, ciphertext,
    status, attempts, next_attempt_at, last_error, created_at, updated_at
) VALUES (NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, 0, ?, '', ?, ?)`,
		eventKey,
		draft.ExternalID,
		draft.RequestSHA256,
		draft.SubjectSHA256,
		draft.KeyID,
		draft.Nonce,
		draft.Ciphertext,
		PrincipalDisableJobStatusHeldShadow,
		createdAt,
		createdAt,
		createdAt,
	)
	if err != nil {
		return 0, fmt.Errorf("store principal disable job: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("inspect principal disable job insert: %w", err)
	}
	if err := insertPrincipalDisableAudit(
		ctx,
		tx,
		draft.ExternalID,
		eventKey,
		"principal_disable_shadow",
		"held_shadow",
		createdAt,
	); err != nil {
		return 0, fmt.Errorf("store principal disable shadow audit: %w", err)
	}
	return id, nil
}

func (s *Store) GetPrincipalDisableJob(
	ctx context.Context,
	externalID string,
) (PrincipalDisableJob, error) {
	if externalID == "" {
		return PrincipalDisableJob{}, errors.New("external ID is required")
	}
	return scanPrincipalDisableJob(s.database.QueryRowContext(ctx, `
SELECT id, event_key, external_id, request_sha256, subject_sha256, key_id, nonce,
       ciphertext, status, attempts, next_attempt_at, last_error, activated_at,
       response_status, response_outcome, response_principal_version,
       response_auth_version, completed_at, created_at, updated_at
FROM principal_disable_jobs WHERE external_id = ?`, externalID))
}

func (s *Store) ReleaseHeldPrincipalDisableJobs(ctx context.Context) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin held principal disable release: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
UPDATE principal_disable_jobs
SET status = ?, next_attempt_at = ?, activated_at = ?, updated_at = ?
WHERE status = ?`,
		PrincipalDisableJobStatusPending,
		now,
		now,
		now,
		PrincipalDisableJobStatusHeldShadow,
	)
	if err != nil {
		return 0, fmt.Errorf("release held principal disable jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE lark_event_inbox SET processing_state = ?
WHERE event_key IN (
    SELECT event_key FROM principal_disable_jobs
    WHERE status = ? AND activated_at = ? AND event_key IS NOT NULL
)`, ProcessingStatePending, PrincipalDisableJobStatusPending, now); err != nil {
		return 0, fmt.Errorf("release held principal disable events: %w", err)
	}
	released, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect released principal disable jobs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit held principal disable release: %w", err)
	}
	return released, nil
}

func (s *Store) ClaimNextPrincipalDisableJob(
	ctx context.Context,
) (PrincipalDisableJob, bool, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return PrincipalDisableJob{}, false, fmt.Errorf("begin principal disable job claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	job, err := scanPrincipalDisableJob(tx.QueryRowContext(ctx, `
SELECT id, event_key, external_id, request_sha256, subject_sha256, key_id, nonce,
       ciphertext, status, attempts, next_attempt_at, last_error, activated_at,
       response_status, response_outcome, response_principal_version,
       response_auth_version, completed_at, created_at, updated_at
FROM principal_disable_jobs
WHERE status IN (?, ?) AND julianday(next_attempt_at) <= julianday(?)
ORDER BY id
LIMIT 1`,
		PrincipalDisableJobStatusPending,
		PrincipalDisableJobStatusRetryWait,
		now.Format(time.RFC3339Nano),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return PrincipalDisableJob{}, false, nil
	}
	if err != nil {
		return PrincipalDisableJob{}, false, fmt.Errorf("select principal disable job: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE principal_disable_jobs
SET status = ?, attempts = attempts + 1, updated_at = ?
WHERE id = ? AND status IN (?, ?)`,
		PrincipalDisableJobStatusProcessing,
		now.Format(time.RFC3339Nano),
		job.ID,
		PrincipalDisableJobStatusPending,
		PrincipalDisableJobStatusRetryWait,
	)
	if err != nil {
		return PrincipalDisableJob{}, false, fmt.Errorf("claim principal disable job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return PrincipalDisableJob{}, false, fmt.Errorf(
			"claim principal disable job affected %d rows: %w",
			affected,
			err,
		)
	}
	if job.EventKey != "" {
		if _, err := tx.ExecContext(ctx, `
UPDATE lark_event_inbox SET processing_state = ? WHERE event_key = ?`,
			ProcessingStateProcessing,
			job.EventKey,
		); err != nil {
			return PrincipalDisableJob{}, false, fmt.Errorf("mark principal disable event processing: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return PrincipalDisableJob{}, false, fmt.Errorf("commit principal disable job claim: %w", err)
	}
	job.Status = PrincipalDisableJobStatusProcessing
	job.Attempts++
	job.UpdatedAt = now
	return job, true, nil
}

func (s *Store) CompletePrincipalDisableJob(
	ctx context.Context,
	job PrincipalDisableJob,
	receipt PrincipalDisableReceipt,
) error {
	if job.ID <= 0 || job.Status != PrincipalDisableJobStatusProcessing ||
		receipt.ExternalID != job.ExternalID || !validPrincipalDisableReceipt(receipt) {
		return errors.New("invalid principal disable completion")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin principal disable completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `
UPDATE principal_disable_jobs
SET status = ?, last_error = '', response_status = ?, response_outcome = ?,
    response_principal_version = ?, response_auth_version = ?, completed_at = ?, updated_at = ?
WHERE id = ? AND status = ? AND attempts = ?`,
		PrincipalDisableJobStatusSucceeded,
		receipt.Status,
		receipt.Outcome,
		receipt.PrincipalVersion,
		receipt.AuthVersion,
		now,
		now,
		job.ID,
		PrincipalDisableJobStatusProcessing,
		job.Attempts,
	)
	if err != nil {
		return fmt.Errorf("complete principal disable job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return fmt.Errorf("complete principal disable job affected %d rows: %w", affected, err)
	}
	if err := insertPrincipalDisableAudit(
		ctx,
		tx,
		job.ExternalID,
		job.EventKey,
		"principal_disable",
		receipt.Status,
		now,
	); err != nil {
		return fmt.Errorf("store principal disable completion audit: %w", err)
	}
	if job.EventKey != "" {
		if _, err := tx.ExecContext(ctx, `
UPDATE lark_event_inbox SET processing_state = ? WHERE event_key = ?`,
			ProcessingStatePrincipalDisabled,
			job.EventKey,
		); err != nil {
			return fmt.Errorf("complete principal disable event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit principal disable completion: %w", err)
	}
	return nil
}

func (s *Store) RetryPrincipalDisableJob(
	ctx context.Context,
	job PrincipalDisableJob,
	reason PrincipalDisableFailureReason,
	delay time.Duration,
) error {
	if delay <= 0 || !IsRetryablePrincipalDisableFailure(reason) {
		return errors.New("invalid principal disable retry")
	}
	return s.transitionPrincipalDisableFailure(
		ctx,
		job,
		reason,
		PrincipalDisableJobStatusRetryWait,
		delay,
	)
}

func (s *Store) DeadLetterPrincipalDisableJob(
	ctx context.Context,
	job PrincipalDisableJob,
	reason PrincipalDisableFailureReason,
) error {
	if _, ok := principalDisableFailureCatalog[reason]; !ok {
		return errors.New("invalid principal disable terminal failure")
	}
	return s.transitionPrincipalDisableFailure(
		ctx,
		job,
		reason,
		PrincipalDisableJobStatusDeadLetter,
		0,
	)
}

func (s *Store) transitionPrincipalDisableFailure(
	ctx context.Context,
	job PrincipalDisableJob,
	reason PrincipalDisableFailureReason,
	status PrincipalDisableJobStatus,
	delay time.Duration,
) error {
	if job.ID <= 0 || job.Status != PrincipalDisableJobStatusProcessing || job.Attempts <= 0 {
		return errors.New("invalid principal disable failure transition")
	}
	now := time.Now().UTC()
	nextAttemptAt := now
	completedAt := ""
	action := "principal_disable_retry"
	inboxState := ProcessingStatePending
	if status == PrincipalDisableJobStatusRetryWait {
		nextAttemptAt = now.Add(delay)
	} else if status == PrincipalDisableJobStatusDeadLetter {
		completedAt = now.Format(time.RFC3339Nano)
		action = "principal_disable_dead_letter"
		inboxState = ProcessingStateDeadLetter
	} else {
		return errors.New("invalid principal disable failure status")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin principal disable failure transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
UPDATE principal_disable_jobs
SET status = ?, next_attempt_at = ?, last_error = ?, completed_at = ?, updated_at = ?
WHERE id = ? AND status = ? AND attempts = ?`,
		status,
		nextAttemptAt.Format(time.RFC3339Nano),
		reason,
		completedAt,
		now.Format(time.RFC3339Nano),
		job.ID,
		PrincipalDisableJobStatusProcessing,
		job.Attempts,
	)
	if err != nil {
		return fmt.Errorf("transition principal disable failure: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return fmt.Errorf("transition principal disable failure affected %d rows: %w", affected, err)
	}
	if err := insertPrincipalDisableAudit(
		ctx,
		tx,
		job.ExternalID,
		job.EventKey,
		action,
		string(reason),
		now.Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("store principal disable failure audit: %w", err)
	}
	if job.EventKey != "" {
		if _, err := tx.ExecContext(ctx, `
UPDATE lark_event_inbox SET processing_state = ? WHERE event_key = ?`,
			inboxState,
			job.EventKey,
		); err != nil {
			return fmt.Errorf("transition principal disable event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit principal disable failure transition: %w", err)
	}
	return nil
}

func (s *Store) ValidatePrincipalDisableJobKeyIDs(ctx context.Context, keyIDs []string) error {
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
SELECT DISTINCT key_id FROM principal_disable_jobs
WHERE status NOT IN (?, ?)`,
		PrincipalDisableJobStatusSucceeded,
		PrincipalDisableJobStatusDeadLetter,
	)
	if err != nil {
		return fmt.Errorf("validate principal disable job keyring: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var keyID string
		if err := rows.Scan(&keyID); err != nil {
			return fmt.Errorf("scan principal disable job key ID: %w", err)
		}
		if _, ok := available[keyID]; !ok {
			return errors.New("configured grant payload keyring cannot open nonterminal principal disable jobs")
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate principal disable job key IDs: %w", err)
	}
	return nil
}

func validPrincipalDisableReceipt(receipt PrincipalDisableReceipt) bool {
	return receipt.ExternalID != "" && newapi.ValidatePrincipalDisableResult(
		receipt.Status,
		receipt.Outcome,
		receipt.PrincipalVersion,
		receipt.AuthVersion,
	) == nil
}

func insertPrincipalDisableAudit(
	ctx context.Context,
	tx *sql.Tx,
	externalID string,
	eventKey string,
	action string,
	outcome string,
	createdAt string,
) error {
	if externalID == "" || action == "" || outcome == "" || createdAt == "" {
		return errors.New("invalid principal disable audit")
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO principal_disable_audit (external_id, event_key, action, outcome, created_at)
VALUES (?, NULLIF(?, ''), ?, ?, ?)`, externalID, eventKey, action, outcome, createdAt)
	return err
}

type principalDisableJobScanner interface {
	Scan(...any) error
}

func scanPrincipalDisableJob(scanner principalDisableJobScanner) (PrincipalDisableJob, error) {
	var job PrincipalDisableJob
	var eventKey sql.NullString
	var nextAttemptAt string
	var activatedAt string
	var receipt PrincipalDisableReceipt
	var completedAt string
	var createdAt string
	var updatedAt string
	if err := scanner.Scan(
		&job.ID,
		&eventKey,
		&job.ExternalID,
		&job.RequestSHA256,
		&job.SubjectSHA256,
		&job.KeyID,
		&job.Nonce,
		&job.Ciphertext,
		&job.Status,
		&job.Attempts,
		&nextAttemptAt,
		&job.LastError,
		&activatedAt,
		&receipt.Status,
		&receipt.Outcome,
		&receipt.PrincipalVersion,
		&receipt.AuthVersion,
		&completedAt,
		&createdAt,
		&updatedAt,
	); err != nil {
		return PrincipalDisableJob{}, err
	}
	job.EventKey = eventKey.String
	var err error
	job.NextAttemptAt, err = time.Parse(time.RFC3339Nano, nextAttemptAt)
	if err != nil {
		return PrincipalDisableJob{}, fmt.Errorf("parse principal disable next_attempt_at: %w", err)
	}
	job.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return PrincipalDisableJob{}, fmt.Errorf("parse principal disable created_at: %w", err)
	}
	job.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return PrincipalDisableJob{}, fmt.Errorf("parse principal disable updated_at: %w", err)
	}
	if activatedAt != "" {
		job.ActivatedAt, err = time.Parse(time.RFC3339Nano, activatedAt)
		if err != nil {
			return PrincipalDisableJob{}, fmt.Errorf("parse principal disable activated_at: %w", err)
		}
	}
	if receipt.Status != "" {
		receipt.ExternalID = job.ExternalID
		job.Receipt = &receipt
	}
	if completedAt != "" {
		job.CompletedAt, err = time.Parse(time.RFC3339Nano, completedAt)
		if err != nil {
			return PrincipalDisableJob{}, fmt.Errorf("parse principal disable completed_at: %w", err)
		}
	}
	return job, nil
}
