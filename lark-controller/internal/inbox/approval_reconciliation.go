package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ApprovalReconciliationResult string

const (
	ApprovalReconciliationResultSuccess           ApprovalReconciliationResult = "success"
	ApprovalReconciliationResultRateLimited       ApprovalReconciliationResult = "rate_limited"
	ApprovalReconciliationResultServerError       ApprovalReconciliationResult = "server_error"
	ApprovalReconciliationResultClientError       ApprovalReconciliationResult = "client_error"
	ApprovalReconciliationResultTimeout           ApprovalReconciliationResult = "timeout"
	ApprovalReconciliationResultTransportError    ApprovalReconciliationResult = "transport_error"
	ApprovalReconciliationResultInvalidResponse   ApprovalReconciliationResult = "invalid_response"
	ApprovalReconciliationResultUnclassifiedError ApprovalReconciliationResult = "unclassified_error"
	ApprovalReconciliationResultIncompleteScan    ApprovalReconciliationResult = "incomplete_scan"
)

type ApprovalRecheckTarget struct {
	ApprovalCode string
	InstanceCode string
}

type ApprovalReconciliationAudit struct {
	ApprovalCode  string
	WindowStart   time.Time
	WindowEnd     time.Time
	Result        ApprovalReconciliationResult
	InstanceCount int
	CreatedAt     time.Time
}

const maxApprovalRecheckTargets = 100_000

func (s *Store) ApprovalReconciliationCursor(
	ctx context.Context,
	approvalCode string,
) (time.Time, bool, error) {
	if !validApprovalReconciliationIdentifier(approvalCode) {
		return time.Time{}, false, errors.New("invalid approval reconciliation code")
	}
	var raw string
	err := s.database.QueryRowContext(ctx, `
SELECT scanned_through FROM approval_reconciliation_cursors WHERE approval_code = ?`,
		approvalCode,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read approval reconciliation cursor: %w", err)
	}
	cursor, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse approval reconciliation cursor: %w", err)
	}
	return cursor, true, nil
}

func (s *Store) CompleteApprovalReconciliationWindow(
	ctx context.Context,
	approvalCode string,
	windowStart time.Time,
	windowEnd time.Time,
	instanceCount int,
) error {
	if !validApprovalReconciliationWindow(approvalCode, windowStart, windowEnd, instanceCount) ||
		!windowStart.Before(windowEnd) {
		return errors.New("invalid completed approval reconciliation window")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin approval reconciliation completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existingRaw string
	err = tx.QueryRowContext(ctx, `
SELECT scanned_through FROM approval_reconciliation_cursors WHERE approval_code = ?`,
		approvalCode,
	).Scan(&existingRaw)
	scannedThrough := windowEnd.UTC()
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("inspect approval reconciliation cursor: %w", err)
	default:
		existing, parseErr := time.Parse(time.RFC3339Nano, existingRaw)
		if parseErr != nil {
			return fmt.Errorf("parse existing approval reconciliation cursor: %w", parseErr)
		}
		if existing.After(scannedThrough) {
			scannedThrough = existing
		}
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO approval_reconciliation_cursors (approval_code, scanned_through, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(approval_code) DO UPDATE SET
    scanned_through = excluded.scanned_through,
    updated_at = excluded.updated_at`,
		approvalCode,
		scannedThrough.Format(time.RFC3339Nano),
		now,
	); err != nil {
		return fmt.Errorf("advance approval reconciliation cursor: %w", err)
	}
	if err := insertApprovalReconciliationAudit(
		ctx,
		tx,
		approvalCode,
		windowStart,
		windowEnd,
		ApprovalReconciliationResultSuccess,
		instanceCount,
		now,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit approval reconciliation completion: %w", err)
	}
	return nil
}

func (s *Store) FailApprovalReconciliationWindow(
	ctx context.Context,
	approvalCode string,
	windowStart time.Time,
	windowEnd time.Time,
	result ApprovalReconciliationResult,
	instanceCount int,
) error {
	if !validApprovalReconciliationWindow(approvalCode, windowStart, windowEnd, instanceCount) ||
		!validApprovalReconciliationFailure(result) {
		return errors.New("invalid failed approval reconciliation window")
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if err := insertApprovalReconciliationAudit(
		ctx,
		s.database,
		approvalCode,
		windowStart,
		windowEnd,
		result,
		instanceCount,
		now,
	); err != nil {
		return err
	}
	return nil
}

type approvalReconciliationExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertApprovalReconciliationAudit(
	ctx context.Context,
	execer approvalReconciliationExecer,
	approvalCode string,
	windowStart time.Time,
	windowEnd time.Time,
	result ApprovalReconciliationResult,
	instanceCount int,
	createdAt string,
) error {
	_, err := execer.ExecContext(ctx, `
INSERT INTO approval_reconciliation_audit (
    approval_code, window_start, window_end, result, instance_count, created_at
) VALUES (?, ?, ?, ?, ?, ?)`,
		approvalCode,
		windowStart.UTC().Format(time.RFC3339Nano),
		windowEnd.UTC().Format(time.RFC3339Nano),
		result,
		instanceCount,
		createdAt,
	)
	if err != nil {
		return fmt.Errorf("store approval reconciliation audit: %w", err)
	}
	return nil
}

func (s *Store) HasApprovalAuthorityProjection(
	ctx context.Context,
	tenantKey string,
	approvalCode string,
	instanceCode string,
	reverted bool,
) (bool, error) {
	if !validApprovalReconciliationIdentifier(tenantKey) ||
		!validApprovalReconciliationIdentifier(approvalCode) ||
		!validApprovalReconciliationIdentifier(instanceCode) {
		return false, errors.New("invalid approval authority projection identity")
	}
	var exists int
	if reverted {
		err := s.database.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM approval_reversals reversal
    JOIN lark_event_inbox event ON event.event_key = reversal.event_key
    WHERE event.tenant_key = ?
      AND reversal.approval_code = ?
      AND reversal.target_instance_code = ?
      AND reversal.authority_approval_code = ?
      AND reversal.authority_instance_code = ?
      AND reversal.authority_reverted = 1
)`, tenantKey, approvalCode, instanceCode, approvalCode, instanceCode).Scan(&exists)
		if err != nil {
			return false, fmt.Errorf("inspect approval reversal projection: %w", err)
		}
		return exists != 0, nil
	}
	err := s.database.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM approval_instances decision
    JOIN lark_event_inbox event ON event.event_key = decision.event_key
    WHERE event.tenant_key = ?
      AND decision.approval_code = ?
      AND decision.instance_code = ?
      AND decision.authority_status = 'APPROVED'
      AND decision.reverted = 0
      AND decision.outcome IN (?, ?, ?, ?)
)`,
		tenantKey,
		approvalCode,
		instanceCode,
		DecisionOutcomeShadowAuthorityVerified,
		DecisionOutcomeShadowLegacyUnresolved,
		DecisionOutcomeDeadLetterPolicyValidation,
		DecisionOutcomeDeadLetterCommandPlanning,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("inspect approval grant projection: %w", err)
	}
	return exists != 0, nil
}

func (s *Store) ListApprovalRecheckTargets(
	ctx context.Context,
	tenantKey string,
) ([]ApprovalRecheckTarget, error) {
	if !validApprovalReconciliationIdentifier(tenantKey) {
		return nil, errors.New("invalid approval recheck tenant")
	}
	rows, err := s.database.QueryContext(ctx, `
SELECT DISTINCT decision.approval_code, decision.instance_code
FROM approval_instances decision
JOIN lark_event_inbox event ON event.event_key = decision.event_key
JOIN entitlement_command_shadows command ON command.event_key = decision.event_key
JOIN policy_versions policy ON policy.policy_version = decision.policy_version
WHERE event.tenant_key = ?
  AND decision.outcome = ?
  AND policy.state != 'retired'
  AND NOT EXISTS (
      SELECT 1 FROM approval_reversals reversal
      JOIN lark_event_inbox reversal_event ON reversal_event.event_key = reversal.event_key
      WHERE reversal_event.tenant_key = event.tenant_key
        AND reversal.approval_code = decision.approval_code
        AND reversal.target_instance_code = decision.instance_code
        AND reversal.authority_approval_code = decision.approval_code
        AND reversal.authority_instance_code = decision.instance_code
        AND reversal.authority_reverted = 1
  )
ORDER BY decision.approval_code, decision.instance_code
LIMIT ?`, tenantKey, DecisionOutcomeShadowAuthorityVerified, maxApprovalRecheckTargets+1)
	if err != nil {
		return nil, fmt.Errorf("query approval recheck targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	targets := make([]ApprovalRecheckTarget, 0)
	for rows.Next() {
		var target ApprovalRecheckTarget
		if err := rows.Scan(&target.ApprovalCode, &target.InstanceCode); err != nil {
			return nil, fmt.Errorf("scan approval recheck target: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate approval recheck targets: %w", err)
	}
	if len(targets) > maxApprovalRecheckTargets {
		return nil, errors.New("approval recheck target limit exceeded")
	}
	return targets, nil
}

func (s *Store) ListApprovalReconciliationAudit(
	ctx context.Context,
) ([]ApprovalReconciliationAudit, error) {
	rows, err := s.database.QueryContext(ctx, `
SELECT approval_code, window_start, window_end, result, instance_count, created_at
FROM approval_reconciliation_audit ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query approval reconciliation audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	audit := make([]ApprovalReconciliationAudit, 0)
	for rows.Next() {
		var entry ApprovalReconciliationAudit
		var windowStart string
		var windowEnd string
		var createdAt string
		if err := rows.Scan(
			&entry.ApprovalCode,
			&windowStart,
			&windowEnd,
			&entry.Result,
			&entry.InstanceCount,
			&createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan approval reconciliation audit: %w", err)
		}
		var parseErr error
		entry.WindowStart, parseErr = time.Parse(time.RFC3339Nano, windowStart)
		if parseErr == nil {
			entry.WindowEnd, parseErr = time.Parse(time.RFC3339Nano, windowEnd)
		}
		if parseErr == nil {
			entry.CreatedAt, parseErr = time.Parse(time.RFC3339Nano, createdAt)
		}
		if parseErr != nil {
			return nil, fmt.Errorf("parse approval reconciliation audit: %w", parseErr)
		}
		audit = append(audit, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate approval reconciliation audit: %w", err)
	}
	return audit, nil
}

func validApprovalReconciliationWindow(
	approvalCode string,
	windowStart time.Time,
	windowEnd time.Time,
	instanceCount int,
) bool {
	return validApprovalReconciliationIdentifier(approvalCode) &&
		!windowStart.IsZero() && !windowEnd.IsZero() &&
		!windowStart.After(windowEnd) &&
		instanceCount >= 0 && instanceCount <= maxApprovalRecheckTargets
}

func validApprovalReconciliationIdentifier(value string) bool {
	return value != "" && len(value) <= 512 && value == strings.TrimSpace(value)
}

func validApprovalReconciliationFailure(result ApprovalReconciliationResult) bool {
	switch result {
	case ApprovalReconciliationResultRateLimited,
		ApprovalReconciliationResultServerError,
		ApprovalReconciliationResultClientError,
		ApprovalReconciliationResultTimeout,
		ApprovalReconciliationResultTransportError,
		ApprovalReconciliationResultInvalidResponse,
		ApprovalReconciliationResultUnclassifiedError,
		ApprovalReconciliationResultIncompleteScan:
		return true
	default:
		return false
	}
}
