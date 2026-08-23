package inbox

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoverStaleProcessingRecoversOnlyExpiredClaimsAndAuditsCounts(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	recoveredAt := time.Date(2026, time.August, 23, 8, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return recoveredAt }
	stale := recoveredAt.Add(-6 * time.Minute).Format(time.RFC3339Nano)
	fresh := recoveredAt.Add(-4 * time.Minute).Format(time.RFC3339Nano)

	insertRecoveryInbox(t, ctx, store, "evt-approval-stale", ProcessingStateProcessing, stale)
	insertRecoveryInbox(t, ctx, store, "evt-approval-fresh", ProcessingStateProcessing, fresh)
	insertRecoveryInbox(t, ctx, store, "evt-disable-stale", ProcessingStateProcessing, stale)
	if _, err := store.database.ExecContext(ctx, `
INSERT INTO jobs (event_key, job_type, status, attempts, next_attempt_at, last_error, created_at, updated_at)
VALUES
    ('evt-approval-stale', 'process_lark_event', 'processing', 2, ?, '', ?, ?),
    ('evt-approval-fresh', 'process_lark_event', 'processing', 1, ?, '', ?, ?)`,
		stale, stale, stale, fresh, fresh, fresh); err != nil {
		t.Fatalf("insert approval recovery fixtures: %v", err)
	}
	if _, err := store.database.ExecContext(ctx, `
	INSERT INTO entitlement_grant_jobs (
	    external_id, request_sha256, subject_sha256, key_id, nonce, ciphertext,
	    status, attempts, next_attempt_at, last_error, activated_at, created_at, updated_at
	) VALUES ('grant-stale', ?, ?, ?, X'01', X'02', 'processing', 3, ?, '', ?, ?, ?)`,
		strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64),
		stale, stale, stale, stale); err != nil {
		t.Fatalf("insert grant recovery fixture: %v", err)
	}
	if _, err := store.database.ExecContext(ctx, `
	INSERT INTO principal_disable_jobs (
	    event_key, external_id, request_sha256, subject_sha256, key_id, nonce, ciphertext,
	    status, attempts, next_attempt_at, last_error, activated_at, created_at, updated_at
	) VALUES ('evt-disable-stale', 'disable-stale', ?, ?, ?, X'03', X'04',
	    'processing', 4, ?, '', ?, ?, ?)`,
		strings.Repeat("d", 64), strings.Repeat("e", 64), strings.Repeat("f", 64),
		stale, stale, stale, stale); err != nil {
		t.Fatalf("insert disable recovery fixture: %v", err)
	}

	result, err := store.RecoverStaleProcessing(ctx, recoveredAt.Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("recover stale processing: %v", err)
	}
	if result.ApprovalJobs != 1 || result.EntitlementGrantJobs != 1 ||
		result.PrincipalDisableJobs != 1 || result.Total() != 3 {
		t.Fatalf("recovery result = %+v, want one recovery per queue", result)
	}
	assertRecoveryJob(t, ctx, store, "jobs", "event_key", "evt-approval-stale", "pending", 2)
	assertRecoveryJob(t, ctx, store, "jobs", "event_key", "evt-approval-fresh", "processing", 1)
	assertRecoveryJob(t, ctx, store, "entitlement_grant_jobs", "external_id", "grant-stale", "pending", 3)
	assertRecoveryJob(t, ctx, store, "principal_disable_jobs", "external_id", "disable-stale", "pending", 4)
	for eventKey, want := range map[string]ProcessingState{
		"evt-approval-stale": ProcessingStatePending,
		"evt-approval-fresh": ProcessingStateProcessing,
		"evt-disable-stale":  ProcessingStatePending,
	} {
		var got ProcessingState
		if err := store.database.QueryRowContext(ctx,
			"SELECT processing_state FROM lark_event_inbox WHERE event_key = ?", eventKey,
		).Scan(&got); err != nil || got != want {
			t.Fatalf("inbox %s state = %q err=%v, want %q", eventKey, got, err, want)
		}
	}
	rows, err := store.database.QueryContext(ctx, `
SELECT queue, recovered_count FROM processing_recovery_audit ORDER BY queue`)
	if err != nil {
		t.Fatalf("query recovery audit: %v", err)
	}
	gotAudit := make(map[string]int64)
	for rows.Next() {
		var queue string
		var count int64
		if err := rows.Scan(&queue, &count); err != nil {
			t.Fatalf("scan recovery audit: %v", err)
		}
		gotAudit[queue] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate recovery audit: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close recovery audit: %v", err)
	}
	for _, queue := range []string{
		ProcessingRecoveryQueueApproval,
		ProcessingRecoveryQueueEntitlementGrant,
		ProcessingRecoveryQueuePrincipalDisable,
	} {
		if gotAudit[queue] != 1 {
			t.Fatalf("recovery audit[%s] = %d, want 1", queue, gotAudit[queue])
		}
	}

	second, err := store.RecoverStaleProcessing(ctx, recoveredAt.Add(-5*time.Minute))
	if err != nil || second.Total() != 0 {
		t.Fatalf("repeat recovery = %+v err=%v, want no-op", second, err)
	}
	var auditCount int
	if err := store.database.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM processing_recovery_audit",
	).Scan(&auditCount); err != nil || auditCount != 3 {
		t.Fatalf("audit count after no-op = %d err=%v, want 3", auditCount, err)
	}
	if _, err := store.RecoverStaleProcessing(ctx, recoveredAt); err == nil {
		t.Fatal("non-past stale cutoff accepted")
	}
	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read processing recovery metrics: %v", err)
	}
	for _, queue := range []string{
		ProcessingRecoveryQueueApproval,
		ProcessingRecoveryQueueEntitlementGrant,
		ProcessingRecoveryQueuePrincipalDisable,
	} {
		if snapshot.ProcessingRecoveries[queue] != 1 {
			t.Fatalf("processing recovery metric[%s] = %d, want 1",
				queue, snapshot.ProcessingRecoveries[queue])
		}
	}
}

func TestRecoveredApprovalAttemptFencesLateRetryAndCompletion(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	event := Event{
		Key: "lark:v2:evt-fenced", SchemaVersion: "2.0", EventID: "evt-fenced",
		EventType: "approval.instance.status_changed_v4", AppID: "cli_test",
		TenantKey: "tenant-test", ApprovalCode: "approval-wallet",
		InstanceCode: "instance-fenced", Status: "APPROVED", PayloadJSON: `{}`,
	}
	if duplicate, err := store.Record(ctx, event); err != nil || duplicate {
		t.Fatalf("record event: duplicate=%t err=%v", duplicate, err)
	}
	first, found, err := store.ClaimNext(ctx)
	if err != nil || !found || first.Attempts != 1 {
		t.Fatalf("claim first attempt: found=%t job=%+v err=%v", found, first, err)
	}
	recoveryTime := time.Now().UTC()
	if _, err := store.database.ExecContext(ctx,
		"UPDATE jobs SET updated_at = ? WHERE id = ?",
		recoveryTime.Add(-10*time.Minute).Format(time.RFC3339Nano), first.ID,
	); err != nil {
		t.Fatalf("age first claim: %v", err)
	}
	if recovered, err := store.RecoverStaleProcessing(ctx, recoveryTime.Add(-5*time.Minute)); err != nil || recovered.ApprovalJobs != 1 {
		t.Fatalf("recover first attempt: recovered=%+v err=%v", recovered, err)
	}
	second, found, err := store.ClaimNext(ctx)
	if err != nil || !found || second.Attempts != 2 {
		t.Fatalf("claim second attempt: found=%t job=%+v err=%v", found, second, err)
	}
	if err := store.Retry(ctx, first, "transport_error", time.Minute); err == nil ||
		!strings.Contains(err.Error(), "affected 0 rows") {
		t.Fatalf("late retry error = %v, want fenced update", err)
	}
	lateDecision := Decision{
		EventKey: event.Key,
		Outcome:  DecisionOutcomeShadowIgnoredNonApproved,
	}
	if err := store.CompleteDecision(ctx, first, lateDecision); err == nil ||
		!strings.Contains(err.Error(), "affected 0 rows") {
		t.Fatalf("late completion error = %v, want fenced update", err)
	}
	if _, err := store.GetDecision(ctx, event.Key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("late completion persisted decision: %v", err)
	}
	if err := store.CompleteDecision(ctx, second, lateDecision); err != nil {
		t.Fatalf("complete current attempt: %v", err)
	}
}

func insertRecoveryInbox(
	t *testing.T,
	ctx context.Context,
	store *Store,
	eventKey string,
	state ProcessingState,
	timestamp string,
) {
	t.Helper()
	if _, err := store.database.ExecContext(ctx, `
INSERT INTO lark_event_inbox (
    event_key, schema_version, event_id, event_type, app_id, tenant_key,
    payload_json, payload_hash, processing_state, received_at, last_seen_at
) VALUES (?, '2.0', ?, 'approval.instance.status_changed_v4', 'cli_test',
    'tenant-test', '{}', ?, ?, ?, ?)`,
		eventKey, eventKey, strings.Repeat("0", 64), state, timestamp, timestamp); err != nil {
		t.Fatalf("insert recovery inbox %s: %v", eventKey, err)
	}
}

func assertRecoveryJob(
	t *testing.T,
	ctx context.Context,
	store *Store,
	table string,
	keyColumn string,
	key string,
	wantStatus string,
	wantAttempts int,
) {
	t.Helper()
	var query string
	switch table + ":" + keyColumn {
	case "jobs:event_key", "entitlement_grant_jobs:external_id", "principal_disable_jobs:external_id":
		query = "SELECT status, attempts FROM " + table + " WHERE " + keyColumn + " = ?"
	default:
		t.Fatalf("unsupported recovery assertion target %s:%s", table, keyColumn)
	}
	var status string
	var attempts int
	if err := store.database.QueryRowContext(ctx, query, key).Scan(&status, &attempts); err != nil ||
		status != wantStatus || attempts != wantAttempts {
		t.Fatalf("%s %s status=%q attempts=%d err=%v, want %q/%d",
			table, key, status, attempts, err, wantStatus, wantAttempts)
	}
}
