package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/digest"
)

const minimumEmploymentMissingInterval = 24 * time.Hour

type EmploymentCheckResult string

const (
	EmploymentCheckResultPresent  EmploymentCheckResult = "present"
	EmploymentCheckResultResigned EmploymentCheckResult = "resigned"
	EmploymentCheckResultExited   EmploymentCheckResult = "exited"
	EmploymentCheckResultNotFound EmploymentCheckResult = "not_found"
	EmploymentCheckResultError    EmploymentCheckResult = "error"
)

type EmploymentCheck struct {
	SubjectSHA256       string
	CheckedAt           time.Time
	Result              EmploymentCheckResult
	LarkResultCode      int
	PermissionHealthy   bool
	EvidenceSHA256      string
	PrincipalDisableJob *PrincipalDisableJobDraft
}

type EmploymentReconciliation struct {
	ReconciliationID  string
	EvidenceDate      string
	StartedAt         time.Time
	CompletedAt       time.Time
	PermissionHealthy bool
	ScanComplete      bool
	Checks            []EmploymentCheck
}

var employmentReconciliationFailureReasons = map[string]struct{}{
	"health_probe_failed":     {},
	"principal_list_failed":   {},
	"employment_check_failed": {},
	"incomplete_scan":         {},
}

func (s *Store) HasCompletedEmploymentReconciliation(
	ctx context.Context,
	evidenceDate string,
) (bool, error) {
	if _, err := time.Parse(time.DateOnly, evidenceDate); err != nil {
		return false, errors.New("invalid employment reconciliation evidence date")
	}
	var count int
	if err := s.database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM employment_reconciliation_runs
WHERE evidence_date = ? AND status = 'complete'`, evidenceDate).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect completed employment reconciliation: %w", err)
	}
	return count == 1, nil
}

func (s *Store) CompleteEmploymentReconciliation(
	ctx context.Context,
	reconciliation EmploymentReconciliation,
) (bool, error) {
	if !validEmploymentReconciliation(reconciliation) {
		return false, errors.New("invalid completed employment reconciliation")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin employment reconciliation completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var completed int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM employment_reconciliation_runs
WHERE evidence_date = ? AND status = 'complete'`, reconciliation.EvidenceDate).Scan(&completed); err != nil {
		return false, fmt.Errorf("inspect employment reconciliation replay: %w", err)
	}
	if completed > 0 {
		return true, nil
	}
	startedAt := reconciliation.StartedAt.UTC().Format(time.RFC3339Nano)
	completedAt := reconciliation.CompletedAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO employment_reconciliation_runs (
    reconciliation_id, evidence_date, status, permission_healthy, scan_complete,
    checked_count, failure_reason, started_at, completed_at, updated_at
) VALUES (?, ?, 'complete', 1, 1, ?, '', ?, ?, ?)
ON CONFLICT(evidence_date) DO UPDATE SET
    status = 'complete', permission_healthy = 1, scan_complete = 1,
    checked_count = excluded.checked_count, failure_reason = '',
    started_at = excluded.started_at, completed_at = excluded.completed_at,
    updated_at = excluded.updated_at`,
		reconciliation.ReconciliationID,
		reconciliation.EvidenceDate,
		len(reconciliation.Checks),
		startedAt,
		completedAt,
		completedAt,
	); err != nil {
		return false, fmt.Errorf("store employment reconciliation run: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM employment_checks WHERE reconciliation_id = ?`, reconciliation.ReconciliationID); err != nil {
		return false, fmt.Errorf("replace employment reconciliation checks: %w", err)
	}
	for _, check := range reconciliation.Checks {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO employment_checks (
    reconciliation_id, subject_sha256, checked_at, result, lark_result_code,
    permission_healthy, evidence_sha256
) VALUES (?, ?, ?, ?, ?, 1, ?)`,
			reconciliation.ReconciliationID,
			check.SubjectSHA256,
			check.CheckedAt.UTC().Format(time.RFC3339Nano),
			check.Result,
			check.LarkResultCode,
			check.EvidenceSHA256,
		); err != nil {
			return false, fmt.Errorf("store employment check: %w", err)
		}
		createDisable, err := applyEmploymentEvidence(ctx, tx, check, completedAt)
		if err != nil {
			return false, err
		}
		if createDisable && check.PrincipalDisableJob != nil {
			var existing int
			if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM principal_disable_jobs
WHERE subject_sha256 = ? AND status != ?`,
				check.SubjectSHA256,
				PrincipalDisableJobStatusDeadLetter,
			).Scan(&existing); err != nil {
				return false, fmt.Errorf("inspect existing principal disable job: %w", err)
			}
			if existing > 0 {
				continue
			}
			if _, err := insertPrincipalDisableJob(
				ctx,
				tx,
				"",
				*check.PrincipalDisableJob,
				completedAt,
			); err != nil {
				return false, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO employment_reconciliation_audit (reconciliation_id, result, created_at)
VALUES (?, 'success', ?)`, reconciliation.ReconciliationID, completedAt); err != nil {
		return false, fmt.Errorf("store employment reconciliation success audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit employment reconciliation: %w", err)
	}
	return false, nil
}

func (s *Store) FailEmploymentReconciliation(
	ctx context.Context,
	evidenceDate string,
	startedAt time.Time,
	completedAt time.Time,
	reason string,
	checks []EmploymentCheck,
) error {
	if _, ok := employmentReconciliationFailureReasons[reason]; !ok ||
		startedAt.IsZero() || completedAt.Before(startedAt) {
		return errors.New("invalid failed employment reconciliation")
	}
	if _, err := time.Parse(time.DateOnly, evidenceDate); err != nil {
		return errors.New("invalid failed employment reconciliation evidence date")
	}
	for _, check := range checks {
		if !validFailedEmploymentCheck(check) {
			return errors.New("invalid failed employment check")
		}
	}
	reconciliationID := "lark:employment-scan:" + evidenceDate
	completedRaw := completedAt.UTC().Format(time.RFC3339Nano)
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin failed employment reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO employment_reconciliation_runs (
    reconciliation_id, evidence_date, status, permission_healthy, scan_complete,
    checked_count, failure_reason, started_at, completed_at, updated_at
) VALUES (?, ?, 'failed', 0, 0, ?, ?, ?, ?, ?)
ON CONFLICT(evidence_date) DO UPDATE SET
    status = CASE WHEN status = 'complete' THEN status ELSE 'failed' END,
    permission_healthy = CASE WHEN status = 'complete' THEN permission_healthy ELSE 0 END,
    scan_complete = CASE WHEN status = 'complete' THEN scan_complete ELSE 0 END,
	    checked_count = CASE WHEN status = 'complete' THEN checked_count ELSE excluded.checked_count END,
    failure_reason = CASE WHEN status = 'complete' THEN failure_reason ELSE excluded.failure_reason END,
    started_at = CASE WHEN status = 'complete' THEN started_at ELSE excluded.started_at END,
    completed_at = CASE WHEN status = 'complete' THEN completed_at ELSE excluded.completed_at END,
    updated_at = CASE WHEN status = 'complete' THEN updated_at ELSE excluded.updated_at END`,
		reconciliationID,
		evidenceDate,
		len(checks),
		reason,
		startedAt.UTC().Format(time.RFC3339Nano),
		completedRaw,
		completedRaw,
	); err != nil {
		return fmt.Errorf("store failed employment reconciliation: %w", err)
	}
	var runStatus string
	if err := tx.QueryRowContext(ctx, `
SELECT status FROM employment_reconciliation_runs WHERE evidence_date = ?`, evidenceDate).
		Scan(&runStatus); err != nil {
		return fmt.Errorf("read failed employment reconciliation status: %w", err)
	}
	for _, check := range checks {
		if check.Result == EmploymentCheckResultPresent {
			if err := clearEmploymentMissingEvidence(ctx, tx, check.SubjectSHA256); err != nil {
				return err
			}
		}
	}
	if runStatus == "complete" {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO employment_reconciliation_audit (reconciliation_id, result, created_at)
VALUES (?, ?, ?)`, reconciliationID, reason, completedRaw); err != nil {
			return fmt.Errorf("store failed employment reconciliation audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit failed employment reconciliation: %w", err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM employment_checks WHERE reconciliation_id = ?`, reconciliationID); err != nil {
		return fmt.Errorf("replace failed employment reconciliation checks: %w", err)
	}
	for _, check := range checks {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO employment_checks (
    reconciliation_id, subject_sha256, checked_at, result, lark_result_code,
    permission_healthy, evidence_sha256
) VALUES (?, ?, ?, ?, ?, 0, ?)`,
			reconciliationID,
			check.SubjectSHA256,
			check.CheckedAt.UTC().Format(time.RFC3339Nano),
			check.Result,
			check.LarkResultCode,
			check.EvidenceSHA256,
		); err != nil {
			return fmt.Errorf("store failed employment check: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO employment_reconciliation_audit (reconciliation_id, result, created_at)
VALUES (?, ?, ?)`, reconciliationID, reason, completedRaw); err != nil {
		return fmt.Errorf("store failed employment reconciliation audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit failed employment reconciliation: %w", err)
	}
	return nil
}

func (s *Store) ListEmploymentChecks(
	ctx context.Context,
	evidenceDate string,
) ([]EmploymentCheck, error) {
	if _, err := time.Parse(time.DateOnly, evidenceDate); err != nil {
		return nil, errors.New("invalid employment reconciliation evidence date")
	}
	rows, err := s.database.QueryContext(ctx, `
SELECT subject_sha256, checked_at, result, lark_result_code,
       permission_healthy, evidence_sha256
FROM employment_checks
WHERE reconciliation_id = ? ORDER BY subject_sha256`,
		"lark:employment-scan:"+evidenceDate,
	)
	if err != nil {
		return nil, fmt.Errorf("list employment checks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	checks := make([]EmploymentCheck, 0)
	for rows.Next() {
		var check EmploymentCheck
		var checkedAt string
		if err := rows.Scan(
			&check.SubjectSHA256,
			&checkedAt,
			&check.Result,
			&check.LarkResultCode,
			&check.PermissionHealthy,
			&check.EvidenceSHA256,
		); err != nil {
			return nil, fmt.Errorf("scan employment check: %w", err)
		}
		check.CheckedAt, err = time.Parse(time.RFC3339Nano, checkedAt)
		if err != nil {
			return nil, errors.New("stored employment check has invalid time")
		}
		checks = append(checks, check)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate employment checks: %w", err)
	}
	return checks, nil
}

func validEmploymentReconciliation(reconciliation EmploymentReconciliation) bool {
	if reconciliation.ReconciliationID != "lark:employment-scan:"+reconciliation.EvidenceDate ||
		!reconciliation.PermissionHealthy || !reconciliation.ScanComplete ||
		reconciliation.StartedAt.IsZero() || reconciliation.CompletedAt.Before(reconciliation.StartedAt) {
		return false
	}
	if _, err := time.Parse(time.DateOnly, reconciliation.EvidenceDate); err != nil {
		return false
	}
	seen := make(map[string]struct{}, len(reconciliation.Checks))
	for _, check := range reconciliation.Checks {
		if !digest.IsCanonicalSHA256(check.SubjectSHA256) ||
			!digest.IsCanonicalSHA256(check.EvidenceSHA256) || check.CheckedAt.IsZero() ||
			!check.PermissionHealthy {
			return false
		}
		switch check.Result {
		case EmploymentCheckResultPresent:
			if check.LarkResultCode != 0 || check.PrincipalDisableJob != nil {
				return false
			}
		case EmploymentCheckResultResigned, EmploymentCheckResultExited:
			if check.LarkResultCode != 0 || check.PrincipalDisableJob == nil ||
				check.PrincipalDisableJob.SubjectSHA256 != check.SubjectSHA256 {
				return false
			}
		case EmploymentCheckResultNotFound:
			if check.LarkResultCode != 41012 || check.PrincipalDisableJob == nil ||
				check.PrincipalDisableJob.SubjectSHA256 != check.SubjectSHA256 {
				return false
			}
		default:
			return false
		}
		if _, duplicate := seen[check.SubjectSHA256]; duplicate {
			return false
		}
		seen[check.SubjectSHA256] = struct{}{}
	}
	return true
}

func validFailedEmploymentCheck(check EmploymentCheck) bool {
	if !digest.IsCanonicalSHA256(check.SubjectSHA256) ||
		!digest.IsCanonicalSHA256(check.EvidenceSHA256) || check.CheckedAt.IsZero() ||
		check.PermissionHealthy || check.PrincipalDisableJob != nil || check.LarkResultCode < 0 {
		return false
	}
	switch check.Result {
	case EmploymentCheckResultPresent, EmploymentCheckResultResigned, EmploymentCheckResultExited:
		return check.LarkResultCode == 0
	case EmploymentCheckResultNotFound:
		return check.LarkResultCode == 41012
	case EmploymentCheckResultError:
		return true
	default:
		return false
	}
}

func applyEmploymentEvidence(
	ctx context.Context,
	tx *sql.Tx,
	check EmploymentCheck,
	updatedAt string,
) (bool, error) {
	switch check.Result {
	case EmploymentCheckResultPresent:
		if err := clearEmploymentMissingEvidence(ctx, tx, check.SubjectSHA256); err != nil {
			return false, err
		}
		return false, nil
	case EmploymentCheckResultResigned, EmploymentCheckResultExited:
		if _, err := tx.ExecContext(ctx, `
DELETE FROM employment_missing_evidence WHERE subject_sha256 = ?`, check.SubjectSHA256); err != nil {
			return false, fmt.Errorf("clear terminal employment missing evidence: %w", err)
		}
		return true, nil
	case EmploymentCheckResultNotFound:
		var count int
		var lastRaw string
		err := tx.QueryRowContext(ctx, `
SELECT consecutive_count, last_not_found_at
FROM employment_missing_evidence WHERE subject_sha256 = ?`, check.SubjectSHA256).
			Scan(&count, &lastRaw)
		checkedAt := check.CheckedAt.UTC()
		if errors.Is(err, sql.ErrNoRows) {
			when := checkedAt.Format(time.RFC3339Nano)
			if _, err := tx.ExecContext(ctx, `
INSERT INTO employment_missing_evidence (
    subject_sha256, consecutive_count, first_not_found_at, last_not_found_at, updated_at
) VALUES (?, 1, ?, ?, ?)`, check.SubjectSHA256, when, when, updatedAt); err != nil {
				return false, fmt.Errorf("store first employment missing evidence: %w", err)
			}
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("read employment missing evidence: %w", err)
		}
		last, err := time.Parse(time.RFC3339Nano, lastRaw)
		if err != nil {
			return false, errors.New("stored employment missing evidence has invalid time")
		}
		if checkedAt.Sub(last) >= minimumEmploymentMissingInterval {
			count++
			if _, err := tx.ExecContext(ctx, `
UPDATE employment_missing_evidence
SET consecutive_count = ?, last_not_found_at = ?, updated_at = ?
WHERE subject_sha256 = ?`,
				count,
				checkedAt.Format(time.RFC3339Nano),
				updatedAt,
				check.SubjectSHA256,
			); err != nil {
				return false, fmt.Errorf("advance employment missing evidence: %w", err)
			}
		}
		return count >= 2, nil
	default:
		return false, errors.New("invalid employment evidence result")
	}
}

func clearEmploymentMissingEvidence(ctx context.Context, tx *sql.Tx, subjectSHA256 string) error {
	if _, err := tx.ExecContext(ctx, `
DELETE FROM employment_missing_evidence WHERE subject_sha256 = ?`, subjectSHA256); err != nil {
		return fmt.Errorf("clear employment missing evidence: %w", err)
	}
	return nil
}
