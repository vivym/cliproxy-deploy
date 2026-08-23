package inbox

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type OperationalSnapshot struct {
	WebhookReceived             map[string]int64
	WebhookDuplicates           map[string]int64
	InboxStates                 map[ProcessingState]int64
	JobStates                   map[string]int64
	EntitlementGrantJobStates   map[string]int64
	EntitlementGrantResults     map[EntitlementGrantResultKey]int64
	EntitlementGrantRetries     map[string]int64
	EntitlementGrantDeadLetters map[string]int64
	PrincipalDisableJobStates   map[string]int64
	PrincipalDisableResults     map[string]int64
	PrincipalDisableRetries     map[string]int64
	PrincipalDisableDeadLetters map[string]int64
	EmploymentReconciliations   map[string]int64
	ApprovalFetches             map[string]int64
	NewAPIGrants                map[string]int64
	DeadLetters                 map[string]int64
	PolicyValidationFailures    int64
	OldestActiveJobAge          time.Duration
	OldestReadyJobAge           time.Duration
}

type EntitlementGrantResultKey struct {
	GrantType string
	Status    string
}

func (s *Store) OperationalSnapshot(ctx context.Context) (OperationalSnapshot, error) {
	tx, err := s.database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("begin operational snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	snapshot := OperationalSnapshot{
		WebhookReceived:             make(map[string]int64),
		WebhookDuplicates:           make(map[string]int64),
		InboxStates:                 make(map[ProcessingState]int64),
		JobStates:                   make(map[string]int64),
		EntitlementGrantJobStates:   make(map[string]int64),
		EntitlementGrantResults:     make(map[EntitlementGrantResultKey]int64),
		EntitlementGrantRetries:     make(map[string]int64),
		EntitlementGrantDeadLetters: make(map[string]int64),
		PrincipalDisableJobStates:   make(map[string]int64),
		PrincipalDisableResults:     make(map[string]int64),
		PrincipalDisableRetries:     make(map[string]int64),
		PrincipalDisableDeadLetters: make(map[string]int64),
		EmploymentReconciliations:   make(map[string]int64),
		ApprovalFetches:             make(map[string]int64),
		NewAPIGrants:                make(map[string]int64),
		DeadLetters:                 make(map[string]int64),
	}
	rows, err := tx.QueryContext(ctx, `
SELECT event_type, COUNT(*) + COALESCE(SUM(duplicate_count), 0), COALESCE(SUM(duplicate_count), 0)
FROM lark_event_inbox GROUP BY event_type`)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query webhook metrics: %w", err)
	}
	for rows.Next() {
		var eventType string
		var received int64
		var duplicates int64
		if err := rows.Scan(&eventType, &received, &duplicates); err != nil {
			_ = rows.Close()
			return OperationalSnapshot{}, fmt.Errorf("scan webhook metrics: %w", err)
		}
		snapshot.WebhookReceived[eventType] = received
		snapshot.WebhookDuplicates[eventType] = duplicates
	}
	if err := closeRows(rows, "webhook metrics"); err != nil {
		return OperationalSnapshot{}, err
	}

	rows, err = tx.QueryContext(ctx, `
SELECT processing_state, COUNT(*) FROM lark_event_inbox GROUP BY processing_state`)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query inbox state metrics: %w", err)
	}
	for rows.Next() {
		var state ProcessingState
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			_ = rows.Close()
			return OperationalSnapshot{}, fmt.Errorf("scan inbox state metrics: %w", err)
		}
		snapshot.InboxStates[state] = count
	}
	if err := closeRows(rows, "inbox state metrics"); err != nil {
		return OperationalSnapshot{}, err
	}

	rows, err = tx.QueryContext(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query job state metrics: %w", err)
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			_ = rows.Close()
			return OperationalSnapshot{}, fmt.Errorf("scan job state metrics: %w", err)
		}
		snapshot.JobStates[state] = count
	}
	if err := closeRows(rows, "job state metrics"); err != nil {
		return OperationalSnapshot{}, err
	}

	rows, err = tx.QueryContext(ctx, `
SELECT status, COUNT(*) FROM entitlement_grant_jobs GROUP BY status`)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query entitlement grant job state metrics: %w", err)
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			_ = rows.Close()
			return OperationalSnapshot{}, fmt.Errorf("scan entitlement grant job state metrics: %w", err)
		}
		snapshot.EntitlementGrantJobStates[state] = count
	}
	if err := closeRows(rows, "entitlement grant job state metrics"); err != nil {
		return OperationalSnapshot{}, err
	}

	rows, err = tx.QueryContext(ctx, `
SELECT result_grant_type, response_status, COUNT(*)
FROM entitlement_grant_jobs
WHERE status = 'succeeded'
GROUP BY result_grant_type, response_status`)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query entitlement grant result metrics: %w", err)
	}
	for rows.Next() {
		var key EntitlementGrantResultKey
		var count int64
		if err := rows.Scan(&key.GrantType, &key.Status, &count); err != nil {
			_ = rows.Close()
			return OperationalSnapshot{}, fmt.Errorf("scan entitlement grant result metrics: %w", err)
		}
		snapshot.EntitlementGrantResults[key] = count
	}
	if err := closeRows(rows, "entitlement grant result metrics"); err != nil {
		return OperationalSnapshot{}, err
	}

	snapshot.EntitlementGrantRetries, err = entitlementGrantAuditOutcomeCounts(
		ctx,
		tx,
		"entitlement_grant_retry",
	)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query entitlement grant retry metrics: %w", err)
	}
	snapshot.EntitlementGrantDeadLetters, err = entitlementGrantAuditOutcomeCounts(
		ctx,
		tx,
		"entitlement_grant_dead_letter",
	)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query entitlement grant dead-letter metrics: %w", err)
	}

	rows, err = tx.QueryContext(ctx, `
SELECT status, COUNT(*) FROM principal_disable_jobs GROUP BY status`)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query principal disable job state metrics: %w", err)
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			_ = rows.Close()
			return OperationalSnapshot{}, fmt.Errorf("scan principal disable job state metrics: %w", err)
		}
		snapshot.PrincipalDisableJobStates[state] = count
	}
	if err := closeRows(rows, "principal disable job state metrics"); err != nil {
		return OperationalSnapshot{}, err
	}

	rows, err = tx.QueryContext(ctx, `
SELECT response_status, COUNT(*) FROM principal_disable_jobs
WHERE status = 'succeeded' GROUP BY response_status`)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query principal disable result metrics: %w", err)
	}
	for rows.Next() {
		var result string
		var count int64
		if err := rows.Scan(&result, &count); err != nil {
			_ = rows.Close()
			return OperationalSnapshot{}, fmt.Errorf("scan principal disable result metrics: %w", err)
		}
		snapshot.PrincipalDisableResults[result] = count
	}
	if err := closeRows(rows, "principal disable result metrics"); err != nil {
		return OperationalSnapshot{}, err
	}

	snapshot.PrincipalDisableRetries, err = principalDisableAuditOutcomeCounts(
		ctx,
		tx,
		"principal_disable_retry",
	)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query principal disable retry metrics: %w", err)
	}
	snapshot.PrincipalDisableDeadLetters, err = principalDisableAuditOutcomeCounts(
		ctx,
		tx,
		"principal_disable_dead_letter",
	)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query principal disable dead-letter metrics: %w", err)
	}

	rows, err = tx.QueryContext(ctx, `
SELECT result, COUNT(*) FROM employment_reconciliation_audit GROUP BY result`)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query employment reconciliation metrics: %w", err)
	}
	for rows.Next() {
		var result string
		var count int64
		if err := rows.Scan(&result, &count); err != nil {
			_ = rows.Close()
			return OperationalSnapshot{}, fmt.Errorf("scan employment reconciliation metrics: %w", err)
		}
		snapshot.EmploymentReconciliations[result] = count
	}
	if err := closeRows(rows, "employment reconciliation metrics"); err != nil {
		return OperationalSnapshot{}, err
	}

	rows, err = tx.QueryContext(ctx, `
SELECT outcome, COUNT(*) FROM controller_audit
WHERE action = 'approval_fetch' GROUP BY outcome`)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query approval fetch metrics: %w", err)
	}
	for rows.Next() {
		var result string
		var count int64
		if err := rows.Scan(&result, &count); err != nil {
			_ = rows.Close()
			return OperationalSnapshot{}, fmt.Errorf("scan approval fetch metrics: %w", err)
		}
		snapshot.ApprovalFetches[result] = count
	}
	if err := closeRows(rows, "approval fetch metrics"); err != nil {
		return OperationalSnapshot{}, err
	}

	snapshot.NewAPIGrants, err = entitlementGrantAuditOutcomeCounts(ctx, tx, "new_api_grant")
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query New API grant metrics: %w", err)
	}

	rows, err = tx.QueryContext(ctx, `
SELECT CASE WHEN failure_reason = '' THEN outcome ELSE failure_reason END AS reason, COUNT(*)
FROM approval_instances WHERE outcome LIKE 'dead_letter_%' GROUP BY reason`)
	if err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query dead letter metrics: %w", err)
	}
	for rows.Next() {
		var reason string
		var count int64
		if err := rows.Scan(&reason, &count); err != nil {
			_ = rows.Close()
			return OperationalSnapshot{}, fmt.Errorf("scan dead letter metrics: %w", err)
		}
		snapshot.DeadLetters[reason] = count
	}
	if err := closeRows(rows, "dead letter metrics"); err != nil {
		return OperationalSnapshot{}, err
	}

	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM approval_instances WHERE outcome = ?`,
		DecisionOutcomeDeadLetterPolicyValidation,
	).Scan(&snapshot.PolicyValidationFailures); err != nil {
		return OperationalSnapshot{}, fmt.Errorf("query policy validation metrics: %w", err)
	}
	now := time.Now().UTC()
	snapshot.OldestActiveJobAge, err = oldestJobAge(ctx, tx, now, false)
	if err != nil {
		return OperationalSnapshot{}, err
	}
	snapshot.OldestReadyJobAge, err = oldestJobAge(ctx, tx, now, true)
	if err != nil {
		return OperationalSnapshot{}, err
	}
	oldestActiveGrantAge, err := oldestEntitlementGrantJobAge(ctx, tx, now, false)
	if err != nil {
		return OperationalSnapshot{}, err
	}
	snapshot.OldestActiveJobAge = max(snapshot.OldestActiveJobAge, oldestActiveGrantAge)
	oldestReadyGrantAge, err := oldestEntitlementGrantJobAge(ctx, tx, now, true)
	if err != nil {
		return OperationalSnapshot{}, err
	}
	snapshot.OldestReadyJobAge = max(snapshot.OldestReadyJobAge, oldestReadyGrantAge)
	oldestActiveDisableAge, err := oldestPrincipalDisableJobAge(ctx, tx, now, false)
	if err != nil {
		return OperationalSnapshot{}, err
	}
	snapshot.OldestActiveJobAge = max(snapshot.OldestActiveJobAge, oldestActiveDisableAge)
	oldestReadyDisableAge, err := oldestPrincipalDisableJobAge(ctx, tx, now, true)
	if err != nil {
		return OperationalSnapshot{}, err
	}
	snapshot.OldestReadyJobAge = max(snapshot.OldestReadyJobAge, oldestReadyDisableAge)
	if err := tx.Commit(); err != nil {
		return OperationalSnapshot{}, fmt.Errorf("commit operational snapshot: %w", err)
	}
	return snapshot, nil
}

func principalDisableAuditOutcomeCounts(
	ctx context.Context,
	tx *sql.Tx,
	action string,
) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT outcome, COUNT(*) FROM principal_disable_audit
WHERE action = ? GROUP BY outcome`, action)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int64)
	for rows.Next() {
		var outcome string
		var count int64
		if err := rows.Scan(&outcome, &count); err != nil {
			_ = rows.Close()
			return nil, err
		}
		counts[outcome] = count
	}
	if err := closeRows(rows, "controller audit metrics"); err != nil {
		return nil, err
	}
	return counts, nil
}

func entitlementGrantAuditOutcomeCounts(
	ctx context.Context,
	tx *sql.Tx,
	action string,
) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT outcome, SUM(total) FROM (
    SELECT outcome, COUNT(*) AS total FROM controller_audit
    WHERE action = ? GROUP BY outcome
    UNION ALL
    SELECT outcome, COUNT(*) AS total FROM base_subscription_audit
    WHERE action = ? GROUP BY outcome
) GROUP BY outcome`, action, action)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int64)
	for rows.Next() {
		var outcome string
		var count int64
		if err := rows.Scan(&outcome, &count); err != nil {
			_ = rows.Close()
			return nil, err
		}
		counts[outcome] = count
	}
	if err := closeRows(rows, "entitlement grant audit metrics"); err != nil {
		return nil, err
	}
	return counts, nil
}

func closeRows(rows *sql.Rows, description string) error {
	iterationErr := rows.Err()
	closeErr := rows.Close()
	if iterationErr != nil {
		return fmt.Errorf("iterate %s: %w", description, iterationErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", description, closeErr)
	}
	return nil
}

func oldestJobAge(ctx context.Context, tx *sql.Tx, now time.Time, readyOnly bool) (time.Duration, error) {
	timestampExpression := "created_at"
	if readyOnly {
		timestampExpression = "CASE WHEN status = 'retry_wait' THEN next_attempt_at ELSE created_at END"
	}
	query := `
SELECT MIN(` + timestampExpression + `) FROM jobs
WHERE status IN ('pending', 'processing', 'retry_wait')`
	args := []any{}
	if readyOnly {
		query += ` AND (status != 'retry_wait' OR julianday(next_attempt_at) <= julianday(?))`
		args = append(args, now.Format(time.RFC3339Nano))
	}
	var oldest sql.NullString
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&oldest); err != nil {
		return 0, fmt.Errorf("query oldest job age: %w", err)
	}
	if !oldest.Valid {
		return 0, nil
	}
	createdAt, err := time.Parse(time.RFC3339Nano, oldest.String)
	if err != nil {
		return 0, fmt.Errorf("parse oldest job age: %w", err)
	}
	if createdAt.After(now) {
		return 0, nil
	}
	return now.Sub(createdAt), nil
}

func oldestEntitlementGrantJobAge(
	ctx context.Context,
	tx *sql.Tx,
	now time.Time,
	readyOnly bool,
) (time.Duration, error) {
	timestampExpression := "activated_at"
	if readyOnly {
		timestampExpression = "CASE WHEN status = 'retry_wait' THEN next_attempt_at ELSE activated_at END"
	}
	query := `
SELECT MIN(` + timestampExpression + `) FROM entitlement_grant_jobs
WHERE status IN ('pending', 'processing', 'retry_wait')`
	args := []any{}
	if readyOnly {
		query += ` AND (status != 'retry_wait' OR julianday(next_attempt_at) <= julianday(?))`
		args = append(args, now.Format(time.RFC3339Nano))
	}
	var oldest sql.NullString
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&oldest); err != nil {
		return 0, fmt.Errorf("query oldest entitlement grant job age: %w", err)
	}
	if !oldest.Valid {
		return 0, nil
	}
	createdAt, err := time.Parse(time.RFC3339Nano, oldest.String)
	if err != nil {
		return 0, fmt.Errorf("parse oldest entitlement grant job age: %w", err)
	}
	if createdAt.After(now) {
		return 0, nil
	}
	return now.Sub(createdAt), nil
}

func oldestPrincipalDisableJobAge(
	ctx context.Context,
	tx *sql.Tx,
	now time.Time,
	readyOnly bool,
) (time.Duration, error) {
	timestampExpression := "activated_at"
	if readyOnly {
		timestampExpression = "CASE WHEN status = 'retry_wait' THEN next_attempt_at ELSE activated_at END"
	}
	query := `
SELECT MIN(` + timestampExpression + `) FROM principal_disable_jobs
WHERE status IN ('pending', 'processing', 'retry_wait')`
	args := []any{}
	if readyOnly {
		query += ` AND (status != 'retry_wait' OR julianday(next_attempt_at) <= julianday(?))`
		args = append(args, now.Format(time.RFC3339Nano))
	}
	var oldest sql.NullString
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&oldest); err != nil {
		return 0, fmt.Errorf("query oldest principal disable job age: %w", err)
	}
	if !oldest.Valid {
		return 0, nil
	}
	activatedAt, err := time.Parse(time.RFC3339Nano, oldest.String)
	if err != nil {
		return 0, fmt.Errorf("parse oldest principal disable job age: %w", err)
	}
	if activatedAt.After(now) {
		return 0, nil
	}
	return now.Sub(activatedAt), nil
}
