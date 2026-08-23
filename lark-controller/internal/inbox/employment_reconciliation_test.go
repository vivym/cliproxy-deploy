package inbox

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFailEmploymentReconciliationRecordsPartialCheckedCount(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	startedAt := time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)
	checks := []EmploymentCheck{
		failedEmploymentCheck("a", startedAt, EmploymentCheckResultNotFound, 41012),
		failedEmploymentCheck("b", startedAt, EmploymentCheckResultError, 41050),
	}
	if err := store.FailEmploymentReconciliation(
		ctx, "2026-08-23", startedAt, startedAt.Add(time.Minute),
		"employment_check_failed", checks[:1],
	); err != nil {
		t.Fatalf("fail employment reconciliation: %v", err)
	}
	if err := store.FailEmploymentReconciliation(
		ctx, "2026-08-23", startedAt.Add(2*time.Minute), startedAt.Add(3*time.Minute),
		"employment_check_failed", checks,
	); err != nil {
		t.Fatalf("replace failed employment reconciliation: %v", err)
	}
	var checkedCount int
	if err := store.database.QueryRowContext(ctx, `
SELECT checked_count FROM employment_reconciliation_runs WHERE evidence_date = ?`,
		"2026-08-23",
	).Scan(&checkedCount); err != nil {
		t.Fatalf("read failed reconciliation count: %v", err)
	}
	if checkedCount != len(checks) {
		t.Fatalf("failed reconciliation checked_count = %d, want %d", checkedCount, len(checks))
	}
}

func TestFailEmploymentReconciliationCannotReplaceCompletedEvidence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	startedAt := time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)
	completedCheck := EmploymentCheck{
		SubjectSHA256: strings.Repeat("a", 64),
		CheckedAt:     startedAt, Result: EmploymentCheckResultPresent, LarkResultCode: 0,
		PermissionHealthy: true, EvidenceSHA256: strings.Repeat("b", 64),
	}
	if duplicate, err := store.CompleteEmploymentReconciliation(ctx, EmploymentReconciliation{
		ReconciliationID: "lark:employment-scan:2026-08-23",
		EvidenceDate:     "2026-08-23", StartedAt: startedAt,
		CompletedAt: startedAt.Add(time.Minute), PermissionHealthy: true,
		ScanComplete: true, Checks: []EmploymentCheck{completedCheck},
	}); err != nil || duplicate {
		t.Fatalf("complete employment reconciliation: duplicate=%t err=%v", duplicate, err)
	}
	failedCheck := failedEmploymentCheck(
		"c", startedAt.Add(2*time.Minute), EmploymentCheckResultPresent, 0,
	)
	if _, err := store.database.ExecContext(ctx, `
INSERT INTO employment_missing_evidence (
    subject_sha256, consecutive_count, first_not_found_at, last_not_found_at, updated_at
) VALUES (?, 1, ?, ?, ?)`,
		failedCheck.SubjectSHA256,
		startedAt.Format(time.RFC3339Nano),
		startedAt.Format(time.RFC3339Nano),
		startedAt.Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("seed missing evidence: %v", err)
	}
	if err := store.FailEmploymentReconciliation(
		ctx, "2026-08-23", startedAt.Add(2*time.Minute), startedAt.Add(3*time.Minute),
		"employment_check_failed", []EmploymentCheck{failedCheck},
	); err != nil {
		t.Fatalf("record late failed reconciliation: %v", err)
	}
	checks, err := store.ListEmploymentChecks(ctx, "2026-08-23")
	if err != nil {
		t.Fatalf("list completed evidence: %v", err)
	}
	if len(checks) != 1 || checks[0].SubjectSHA256 != completedCheck.SubjectSHA256 ||
		!checks[0].PermissionHealthy || checks[0].Result != "present" {
		t.Fatalf("completed evidence was replaced by late failure: %+v", checks)
	}
	var missingCount int
	if err := store.database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM employment_missing_evidence WHERE subject_sha256 = ?`,
		failedCheck.SubjectSHA256,
	).Scan(&missingCount); err != nil {
		t.Fatalf("read missing evidence after late failure: %v", err)
	}
	if missingCount != 0 {
		t.Fatal("present result in late failed run did not clear missing evidence")
	}
	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read operational snapshot: %v", err)
	}
	if snapshot.EmploymentReconciliations["success"] != 1 ||
		snapshot.EmploymentReconciliations["employment_check_failed"] != 1 {
		t.Fatalf("employment reconciliation audit = %+v", snapshot.EmploymentReconciliations)
	}
}

func failedEmploymentCheck(
	hashCharacter string,
	checkedAt time.Time,
	result EmploymentCheckResult,
	larkCode int,
) EmploymentCheck {
	return EmploymentCheck{
		SubjectSHA256: strings.Repeat(hashCharacter, 64),
		CheckedAt:     checkedAt, Result: result, LarkResultCode: larkCode,
		PermissionHealthy: false, EvidenceSHA256: strings.Repeat("d", 64),
	}
}
