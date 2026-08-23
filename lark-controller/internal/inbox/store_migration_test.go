package inbox_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
)

func TestOpenMigratesLegacyInboxForRevertedInstanceCode(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	legacyApproved := inbox.Event{
		Key: "lark:v2:legacy-approved", SchemaVersion: "2.0", EventID: "legacy-approved",
		EventType: "approval.instance.status_changed_v4", AppID: "cli_test",
		TenantKey: "tenant-test", ApprovalCode: "approval-wallet-v1",
		InstanceCode: "instance-approved", Status: "APPROVED", PayloadJSON: `{}`,
	}
	legacyEvent := inbox.Event{
		Key: "lark:v2:legacy", SchemaVersion: "2.0", EventID: "legacy",
		EventType: "approval.instance.status_changed_v4", AppID: "cli_test",
		TenantKey: "tenant-test", ApprovalCode: "approval-wallet-v1",
		InstanceCode:         "instance-reversal",
		RevertedInstanceCode: "instance-original",
		Status:               "REVERTED",
		PayloadJSON:          `{"approval_code":"approval-wallet-v1","instance_code":"instance-reversal","status":"REVERTED","reverted_instance_code":"instance-original"}`,
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = database.Exec(`
CREATE TABLE lark_event_inbox (
    event_key TEXT PRIMARY KEY,
    schema_version TEXT NOT NULL,
    event_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    app_id TEXT NOT NULL,
    tenant_key TEXT NOT NULL,
    approval_code TEXT NOT NULL DEFAULT '',
    instance_code TEXT NOT NULL DEFAULT '',
    event_status TEXT NOT NULL DEFAULT '',
    payload_json TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    processing_state TEXT NOT NULL DEFAULT 'pending',
    duplicate_count INTEGER NOT NULL DEFAULT 0,
    received_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL
);
CREATE TABLE jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_key TEXT NOT NULL UNIQUE REFERENCES lark_event_inbox(event_key) ON DELETE CASCADE,
    job_type TEXT NOT NULL,
    status TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
INSERT INTO lark_event_inbox (
    event_key, schema_version, event_id, event_type, app_id, tenant_key,
    approval_code, instance_code, event_status, payload_json, payload_hash,
    processing_state, duplicate_count, received_at, last_seen_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0, ?, ?);
INSERT INTO jobs (
    event_key, job_type, status, attempts, next_attempt_at, created_at, updated_at
) VALUES (?, 'process_lark_event', 'pending', 0, ?, ?, ?)`,
		legacyEvent.Key,
		legacyEvent.SchemaVersion,
		legacyEvent.EventID,
		legacyEvent.EventType,
		legacyEvent.AppID,
		legacyEvent.TenantKey,
		legacyEvent.ApprovalCode,
		legacyEvent.InstanceCode,
		legacyEvent.Status,
		legacyEvent.PayloadJSON,
		legacyInboxPayloadHash(t, legacyEvent),
		"2026-08-23T00:00:00Z",
		"2026-08-23T00:00:00Z",
		legacyEvent.Key,
		"2026-08-23T00:00:00Z",
		"2026-08-23T00:00:00Z",
		"2026-08-23T00:00:00Z",
	)
	if err != nil {
		_ = database.Close()
		t.Fatalf("create legacy inbox: %v", err)
	}
	_, err = database.Exec(`
INSERT INTO lark_event_inbox (
    event_key, schema_version, event_id, event_type, app_id, tenant_key,
    approval_code, instance_code, event_status, payload_json, payload_hash,
    processing_state, duplicate_count, received_at, last_seen_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0, ?, ?)`,
		legacyApproved.Key,
		legacyApproved.SchemaVersion,
		legacyApproved.EventID,
		legacyApproved.EventType,
		legacyApproved.AppID,
		legacyApproved.TenantKey,
		legacyApproved.ApprovalCode,
		legacyApproved.InstanceCode,
		legacyApproved.Status,
		legacyApproved.PayloadJSON,
		legacyInboxPayloadHash(t, legacyApproved),
		"2026-08-23T00:00:00Z",
		"2026-08-23T00:00:00Z",
	)
	if err != nil {
		_ = database.Close()
		t.Fatalf("create legacy approved inbox row: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open and migrate store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	legacy, err := store.Get(ctx, "lark:v2:legacy")
	if err != nil {
		t.Fatalf("get migrated legacy event: %v", err)
	}
	if legacy.RevertedInstanceCode != "instance-original" {
		t.Fatalf("legacy reverted instance code = %q, want instance-original", legacy.RevertedInstanceCode)
	}
	if legacy.Origin != inbox.EventOriginWebhook {
		t.Fatalf("legacy event origin = %q, want webhook", legacy.Origin)
	}
	if duplicate, err := store.Record(ctx, legacyEvent); err != nil || !duplicate {
		t.Fatalf("redelivered legacy event: duplicate=%t err=%v", duplicate, err)
	}
	if duplicate, err := store.Record(ctx, legacyApproved); err != nil || !duplicate {
		t.Fatalf("redelivered legacy approved event: duplicate=%t err=%v", duplicate, err)
	}
	job, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim migrated reversal: found=%t err=%v", found, err)
	}
	if job.Event.RevertedInstanceCode != "instance-original" {
		t.Fatalf("claimed reversal target = %q, want instance-original", job.Event.RevertedInstanceCode)
	}

	_, err = store.Record(ctx, inbox.Event{
		Key: "lark:v2:reverted", SchemaVersion: "2.0", EventID: "reverted",
		EventType: "approval.instance.status_changed_v4", AppID: "cli_test",
		TenantKey: "tenant-test", ApprovalCode: "approval-wallet-v1",
		InstanceCode: "instance-reversal", RevertedInstanceCode: "instance-original",
		Status: "REVERTED", PayloadJSON: `{"status":"REVERTED"}`,
	})
	if err != nil {
		t.Fatalf("record reverted event after migration: %v", err)
	}
	reverted, err := store.Get(ctx, "lark:v2:reverted")
	if err != nil {
		t.Fatalf("get reverted event after migration: %v", err)
	}
	if reverted.RevertedInstanceCode != "instance-original" {
		t.Fatalf("reverted instance code = %q, want instance-original", reverted.RevertedInstanceCode)
	}
}

func legacyInboxPayloadHash(t *testing.T, event inbox.Event) string {
	t.Helper()
	legacyIdentity := struct {
		SchemaVersion        string `json:"schema_version"`
		EventID              string `json:"event_id"`
		EventType            string `json:"event_type"`
		AppID                string `json:"app_id"`
		TenantKey            string `json:"tenant_key"`
		ApprovalCode         string `json:"approval_code"`
		InstanceCode         string `json:"instance_code"`
		Status               string `json:"status"`
		PayloadJSON          string `json:"payload_json"`
		DisableExternalID    string `json:"disable_external_id"`
		DisableRequestSHA256 string `json:"disable_request_sha256"`
		DisableSubjectSHA256 string `json:"disable_subject_sha256"`
	}{
		SchemaVersion: event.SchemaVersion, EventID: event.EventID,
		EventType: event.EventType, AppID: event.AppID, TenantKey: event.TenantKey,
		ApprovalCode: event.ApprovalCode, InstanceCode: event.InstanceCode,
		Status: event.Status, PayloadJSON: event.PayloadJSON,
	}
	encoded, err := json.Marshal(legacyIdentity)
	if err != nil {
		t.Fatalf("encode legacy event identity: %v", err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:])
}
