package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	ProcessingRecoveryQueueApproval         = "approval"
	ProcessingRecoveryQueueEntitlementGrant = "entitlement_grant"
	ProcessingRecoveryQueuePrincipalDisable = "principal_disable"
)

type ProcessingRecoveryResult struct {
	ApprovalJobs         int64
	EntitlementGrantJobs int64
	PrincipalDisableJobs int64
}

func (r ProcessingRecoveryResult) Total() int64 {
	return r.ApprovalJobs + r.EntitlementGrantJobs + r.PrincipalDisableJobs
}

func (s *Store) RecoverStaleProcessing(
	ctx context.Context,
	staleBefore time.Time,
) (ProcessingRecoveryResult, error) {
	recoveredAt := s.currentTime()
	if staleBefore.IsZero() || !staleBefore.Before(recoveredAt) {
		return ProcessingRecoveryResult{}, errors.New("stale processing cutoff must be before recovery time")
	}
	cutoff := staleBefore.UTC().Format(time.RFC3339Nano)
	now := recoveredAt.Format(time.RFC3339Nano)
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return ProcessingRecoveryResult{}, fmt.Errorf("begin processing recovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
UPDATE lark_event_inbox SET processing_state = ?
WHERE processing_state = ? AND event_key IN (
    SELECT event_key FROM jobs
    WHERE status = ? AND julianday(updated_at) <= julianday(?)
)`, ProcessingStatePending, ProcessingStateProcessing, jobStatusProcessing, cutoff); err != nil {
		return ProcessingRecoveryResult{}, fmt.Errorf("recover stale approval inbox events: %w", err)
	}

	result := ProcessingRecoveryResult{}
	result.ApprovalJobs, err = recoverProcessingRows(
		ctx,
		tx,
		"jobs",
		string(jobStatusProcessing),
		string(jobStatusPending),
		cutoff,
		now,
	)
	if err != nil {
		return ProcessingRecoveryResult{}, fmt.Errorf("recover stale approval jobs: %w", err)
	}
	result.EntitlementGrantJobs, err = recoverProcessingRows(
		ctx,
		tx,
		"entitlement_grant_jobs",
		string(EntitlementGrantJobStatusProcessing),
		string(EntitlementGrantJobStatusPending),
		cutoff,
		now,
	)
	if err != nil {
		return ProcessingRecoveryResult{}, fmt.Errorf("recover stale entitlement grant jobs: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE lark_event_inbox SET processing_state = ?
WHERE processing_state = ? AND event_key IN (
    SELECT event_key FROM principal_disable_jobs
    WHERE event_key IS NOT NULL AND status = ?
      AND julianday(updated_at) <= julianday(?)
)`, ProcessingStatePending, ProcessingStateProcessing,
		PrincipalDisableJobStatusProcessing, cutoff); err != nil {
		return ProcessingRecoveryResult{}, fmt.Errorf("recover stale principal disable inbox events: %w", err)
	}
	result.PrincipalDisableJobs, err = recoverProcessingRows(
		ctx,
		tx,
		"principal_disable_jobs",
		string(PrincipalDisableJobStatusProcessing),
		string(PrincipalDisableJobStatusPending),
		cutoff,
		now,
	)
	if err != nil {
		return ProcessingRecoveryResult{}, fmt.Errorf("recover stale principal disable jobs: %w", err)
	}

	for _, audit := range []struct {
		queue string
		count int64
	}{
		{ProcessingRecoveryQueueApproval, result.ApprovalJobs},
		{ProcessingRecoveryQueueEntitlementGrant, result.EntitlementGrantJobs},
		{ProcessingRecoveryQueuePrincipalDisable, result.PrincipalDisableJobs},
	} {
		if audit.count == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO processing_recovery_audit (queue, recovered_count, created_at)
VALUES (?, ?, ?)`, audit.queue, audit.count, now); err != nil {
			return ProcessingRecoveryResult{}, fmt.Errorf("audit %s processing recovery: %w", audit.queue, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ProcessingRecoveryResult{}, fmt.Errorf("commit processing recovery: %w", err)
	}
	return result, nil
}

func recoverProcessingRows(
	ctx context.Context,
	tx *sql.Tx,
	table string,
	processingStatus string,
	pendingStatus string,
	cutoff string,
	recoveredAt string,
) (int64, error) {
	var statement string
	switch table {
	case "jobs", "entitlement_grant_jobs", "principal_disable_jobs":
		statement = `UPDATE ` + table + `
SET status = ?, next_attempt_at = ?, last_error = 'stale_claim_recovered', updated_at = ?
WHERE status = ? AND julianday(updated_at) <= julianday(?)`
	default:
		return 0, errors.New("unsupported processing recovery table")
	}
	result, err := tx.ExecContext(
		ctx,
		statement,
		pendingStatus,
		recoveredAt,
		recoveredAt,
		processingStatus,
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect recovered rows: %w", err)
	}
	return count, nil
}
