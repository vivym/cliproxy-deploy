package inbox_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
)

func validCorrectionSchemaPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(path)
	if err != nil {
		t.Fatalf("create controller schema: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close controller schema: %v", err)
	}
	return path
}

func rewriteCorrectionSchema(t *testing.T, path string, ddl string) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open correction schema fixture: %v", err)
	}
	if _, err := database.Exec(ddl); err != nil {
		_ = database.Close()
		t.Fatalf("rewrite correction schema fixture: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close correction schema fixture: %v", err)
	}
}

func expectCorrectionSchemaError(t *testing.T, path string, fragment string) {
	t.Helper()
	store, err := inbox.OpenCorrection(path)
	if err == nil {
		_ = store.Close()
		t.Fatal("OpenCorrection accepted malformed schema")
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("OpenCorrection error = %v, want fragment %q", err, fragment)
	}
}

func TestOpenCorrectionRejectsIntentTableWithoutPrimaryKey(t *testing.T) {
	path := validCorrectionSchemaPath(t)
	rewriteCorrectionSchema(t, path, `
DROP TABLE approval_reversal_correction_intents;
CREATE TABLE approval_reversal_correction_intents (
    correction_external_id TEXT NOT NULL,
    original_external_id TEXT NOT NULL,
    original_subject_sha256 TEXT NOT NULL,
    correction_request_sha256 TEXT NOT NULL,
    correction_type TEXT NOT NULL,
    operator TEXT NOT NULL,
    reason TEXT NOT NULL,
    change_ticket TEXT NOT NULL,
    status TEXT NOT NULL,
    failure_code TEXT NOT NULL DEFAULT '',
    claimed_at TEXT NOT NULL,
    ended_at TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX ux_approval_reversal_open_intent_original
    ON approval_reversal_correction_intents(original_external_id)
    WHERE status IN ('active', 'remote_conflict', 'resolved');`)
	expectCorrectionSchemaError(t, path, "approval_reversal_correction_intents primary key")
}

func TestOpenCorrectionRejectsReceiptTableWithoutUniqueOriginal(t *testing.T) {
	path := validCorrectionSchemaPath(t)
	rewriteCorrectionSchema(t, path, `
DROP TABLE approval_reversal_resolution_receipts;
CREATE TABLE approval_reversal_resolution_receipts (
    correction_external_id TEXT PRIMARY KEY,
    original_external_id TEXT NOT NULL,
    original_subject_sha256 TEXT NOT NULL,
    correction_request_sha256 TEXT NOT NULL,
    operator TEXT NOT NULL,
    reason TEXT NOT NULL,
    change_ticket TEXT NOT NULL,
    response_status TEXT NOT NULL,
    result_json TEXT NOT NULL,
    resolved_at TEXT NOT NULL
);`)
	expectCorrectionSchemaError(t, path, "approval_reversal_resolution_receipts unique original_external_id")
}

func TestOpenCorrectionRejectsReceiptTableWithoutPrimaryKey(t *testing.T) {
	path := validCorrectionSchemaPath(t)
	rewriteCorrectionSchema(t, path, `
DROP TABLE approval_reversal_resolution_receipts;
CREATE TABLE approval_reversal_resolution_receipts (
    correction_external_id TEXT NOT NULL,
    original_external_id TEXT NOT NULL UNIQUE,
    original_subject_sha256 TEXT NOT NULL,
    correction_request_sha256 TEXT NOT NULL,
    operator TEXT NOT NULL,
    reason TEXT NOT NULL,
    change_ticket TEXT NOT NULL,
    response_status TEXT NOT NULL,
    result_json TEXT NOT NULL,
    resolved_at TEXT NOT NULL
);`)
	expectCorrectionSchemaError(t, path, "approval_reversal_resolution_receipts primary key")
}

func TestOpenCorrectionRejectsWrongOpenIntentPartialPredicate(t *testing.T) {
	path := validCorrectionSchemaPath(t)
	rewriteCorrectionSchema(t, path, `
DROP INDEX ux_approval_reversal_open_intent_original;
CREATE UNIQUE INDEX ux_approval_reversal_open_intent_original
    ON approval_reversal_correction_intents(original_external_id)
    WHERE status = 'active';`)
	expectCorrectionSchemaError(t, path, "open correction intent partial index")
}

func TestOpenCorrectionRejectsNonPartialOpenIntentIndex(t *testing.T) {
	path := validCorrectionSchemaPath(t)
	rewriteCorrectionSchema(t, path, `
DROP INDEX ux_approval_reversal_open_intent_original;
CREATE UNIQUE INDEX ux_approval_reversal_open_intent_original
    ON approval_reversal_correction_intents(original_external_id);`)
	expectCorrectionSchemaError(t, path, "open correction intent partial index")
}
