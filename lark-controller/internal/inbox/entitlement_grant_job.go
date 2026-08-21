package inbox

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

type EntitlementGrantJobStatus string

const EntitlementGrantJobStatusHeldShadow EntitlementGrantJobStatus = "held_shadow"

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
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func insertEntitlementGrantJob(
	ctx context.Context,
	tx *sql.Tx,
	decisionOutcome DecisionOutcome,
	command EntitlementCommandShadow,
	draft EntitlementGrantJobDraft,
	createdAt string,
) (int64, error) {
	if decisionOutcome != DecisionOutcomeShadowAuthorityVerified ||
		draft.ExternalID != command.ExternalID || draft.RequestSHA256 != command.RequestSHA256 ||
		draft.SubjectSHA256 != command.SubjectSHA256 || !isSHA256Hex(draft.RequestSHA256) ||
		!isSHA256Hex(draft.SubjectSHA256) || !isSHA256Hex(draft.KeyID) ||
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
	var job EntitlementGrantJob
	var nextAttemptAt string
	var createdAt string
	var updatedAt string
	err := s.database.QueryRowContext(ctx, `
SELECT id, external_id, request_sha256, subject_sha256, key_id, nonce, ciphertext,
       status, attempts, next_attempt_at, last_error, created_at, updated_at
FROM entitlement_grant_jobs WHERE external_id = ?`, externalID).Scan(
		&job.ID, &job.ExternalID, &job.RequestSHA256, &job.SubjectSHA256,
		&job.KeyID, &job.Nonce, &job.Ciphertext, &job.Status, &job.Attempts,
		&nextAttemptAt, &job.LastError, &createdAt, &updatedAt,
	)
	if err != nil {
		return EntitlementGrantJob{}, err
	}
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
	return job, nil
}

func (s *Store) ValidateEntitlementGrantJobKeyID(ctx context.Context, keyID string) error {
	if !isSHA256Hex(keyID) {
		return errors.New("invalid grant payload key ID")
	}
	var incompatible int64
	if err := s.database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM entitlement_grant_jobs WHERE key_id != ?`, keyID).Scan(&incompatible); err != nil {
		return fmt.Errorf("validate held grant job key: %w", err)
	}
	if incompatible != 0 {
		return errors.New("configured grant payload key cannot open held grant jobs")
	}
	return nil
}

func isSHA256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return false
	}
	return hex.EncodeToString(decoded) == value
}
