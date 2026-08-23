package webhook_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/webhook"
)

func TestEncryptedV2EventIsVerifiedDurableAndDeduplicated(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store := openStore(t, databasePath)
	handler := newEncryptedHandler(t, store)

	body, headers := encryptedV2Request(t, map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id":    "evt-001",
			"event_type":  "approval.instance.status_changed_v4",
			"app_id":      "cli_test",
			"tenant_key":  "tenant-test",
			"create_time": "1787270400000",
			"token":       "verification-token",
		},
		"event": map[string]any{
			"approval_code": "approval-wallet-v1",
			"instance_code": "instance-001",
			"status":        "APPROVED",
		},
	})
	first := postRaw(t, handler, body, headers)
	if first.Code != http.StatusOK {
		t.Fatalf("first event status = %d, want %d; body=%s", first.Code, http.StatusOK, first.Body.String())
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	store = openStore(t, databasePath)
	handler = newEncryptedHandler(t, store)
	second := postRaw(t, handler, body, headers)
	if second.Code != http.StatusOK {
		t.Fatalf("duplicate event status = %d, want %d; body=%s", second.Code, http.StatusOK, second.Body.String())
	}

	recorded, err := store.Get(context.Background(), "lark:v2:evt-001")
	if err != nil {
		t.Fatalf("get recorded event: %v", err)
	}
	if recorded.EventType != "approval.instance.status_changed_v4" ||
		recorded.ApprovalCode != "approval-wallet-v1" ||
		recorded.InstanceCode != "instance-001" || recorded.Status != "APPROVED" {
		t.Fatalf("unexpected normalized event: %+v", recorded)
	}
	if recorded.DuplicateCount != 1 {
		t.Fatalf("duplicate count = %d, want 1", recorded.DuplicateCount)
	}
	if strings.Contains(recorded.PayloadJSON, "verification-token") {
		t.Fatal("normalized inbox payload retained the verification token")
	}
}

func TestDuplicateEventIDRejectsDifferentPayload(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "controller.sqlite"))
	handler := newEncryptedHandler(t, store)
	firstBody, firstHeaders := encryptedV2Request(t, approvalEvent("evt-conflict", "instance-001", "PENDING"))
	first := postRaw(t, handler, firstBody, firstHeaders)
	if first.Code != http.StatusOK {
		t.Fatalf("first event status = %d, want %d", first.Code, http.StatusOK)
	}
	changedBody, changedHeaders := encryptedV2Request(t, approvalEvent("evt-conflict", "instance-002", "APPROVED"))
	changed := postRaw(t, handler, changedBody, changedHeaders)
	if changed.Code != http.StatusConflict {
		t.Fatalf("changed duplicate status = %d, want %d; body=%s", changed.Code, http.StatusConflict, changed.Body.String())
	}
	if !strings.Contains(changed.Body.String(), "event_id_payload_mismatch") {
		t.Fatalf("changed duplicate body = %s, want stable mismatch code", changed.Body.String())
	}
	recorded, err := store.Get(context.Background(), "lark:v2:evt-conflict")
	if err != nil {
		t.Fatalf("get original event: %v", err)
	}
	if recorded.InstanceCode != "instance-001" || recorded.Status != "PENDING" || recorded.DuplicateCount != 0 {
		t.Fatalf("original event was changed: %+v", recorded)
	}
}

func TestV2RevertedInstanceCodeIsDurableAndPartOfDuplicateIdentity(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "controller.sqlite"))
	handler := newEncryptedHandler(t, store)
	event := loadLarkFixture(t, "approval_reverted_v2.json")
	body, headers := encryptedV2Request(t, event)
	response := postRaw(t, handler, body, headers)
	if response.Code != http.StatusOK {
		t.Fatalf("reverted event status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	recorded, err := store.Get(context.Background(), "lark:v2:evt-reverted-001")
	if err != nil {
		t.Fatalf("get reverted event: %v", err)
	}
	if recorded.RevertedInstanceCode != "instance-original-v2" {
		t.Fatalf("reverted instance code = %q, want instance-original-v2", recorded.RevertedInstanceCode)
	}

	changed := loadLarkFixture(t, "approval_reverted_v2.json")
	changed["event"].(map[string]any)["reverted_instance_code"] = "instance-other"
	changedBody, changedHeaders := encryptedV2Request(t, changed)
	conflict := postRaw(t, handler, changedBody, changedHeaders)
	if conflict.Code != http.StatusConflict ||
		!strings.Contains(conflict.Body.String(), "event_id_payload_mismatch") {
		t.Fatalf("changed reversal identity: status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	recorded, err = store.Get(context.Background(), "lark:v2:evt-reverted-001")
	if err != nil {
		t.Fatalf("get original reverted event after conflict: %v", err)
	}
	if recorded.RevertedInstanceCode != "instance-original-v2" || recorded.DuplicateCount != 0 {
		t.Fatalf("conflicting reversal changed original event: %+v", recorded)
	}
}

func TestContactUserDeletedEventCreatesHeldSealedDisableJob(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "controller.sqlite"))
	handler := newEncryptedHandler(t, store)
	body, headers := encryptedV2Request(t, map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": "evt-resigned-1", "event_type": "contact.user.deleted_v3",
			"app_id": "cli_test", "tenant_key": "tenant-test", "token": "verification-token",
		},
		"event": map[string]any{
			"object": map[string]any{"open_id": "ou-resigned", "name": "must-not-persist"},
		},
	})
	response := postRaw(t, handler, body, headers)
	if response.Code != http.StatusOK {
		t.Fatalf("contact event status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	firstJob, err := store.GetPrincipalDisableJob(context.Background(), "lark:disable:evt-resigned-1")
	if err != nil {
		t.Fatalf("get first principal disable job: %v", err)
	}
	duplicate := postRaw(t, handler, body, headers)
	if duplicate.Code != http.StatusOK {
		t.Fatalf("duplicate contact event status = %d, want %d; body=%s", duplicate.Code, http.StatusOK, duplicate.Body.String())
	}
	recorded, err := store.Get(context.Background(), "lark:v2:evt-resigned-1")
	if err != nil {
		t.Fatalf("get recorded contact event: %v", err)
	}
	if recorded.EventType != "contact.user.deleted_v3" ||
		strings.Contains(recorded.PayloadJSON, "ou-resigned") ||
		strings.Contains(recorded.PayloadJSON, "must-not-persist") || recorded.DuplicateCount != 1 {
		t.Fatalf("contact event retained plaintext identity: %+v", recorded)
	}
	job, err := store.GetPrincipalDisableJob(context.Background(), "lark:disable:evt-resigned-1")
	if err != nil {
		t.Fatalf("get principal disable job: %v", err)
	}
	if job.Status != inbox.PrincipalDisableJobStatusHeldShadow || job.SubjectSHA256 == "" ||
		len(job.Nonce) != 12 || len(job.Ciphertext) == 0 {
		t.Fatalf("incomplete held principal disable job: %+v", job)
	}
	if !bytes.Equal(job.Nonce, firstJob.Nonce) || !bytes.Equal(job.Ciphertext, firstJob.Ciphertext) ||
		job.KeyID != firstJob.KeyID || job.RequestSHA256 != firstJob.RequestSHA256 {
		t.Fatal("duplicate contact event replaced the first sealed disable request")
	}
}

func TestContactUserDeletedEventRequiresOpenID(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "controller.sqlite"))
	handler := newEncryptedHandler(t, store)
	body, headers := encryptedV2Request(t, map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": "evt-resigned-missing-id", "event_type": "contact.user.deleted_v3",
			"app_id": "cli_test", "tenant_key": "tenant-test", "token": "verification-token",
		},
		"event": map[string]any{"object": map[string]any{"name": "ignored"}},
	})
	response := postRaw(t, handler, body, headers)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_event") {
		t.Fatalf("missing open_id response: status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := store.Get(context.Background(), "lark:v2:evt-resigned-missing-id"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("invalid contact event was persisted: %v", err)
	}
}

func TestInboxContentionReturnsBeforeLarkAcknowledgementDeadline(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store := openStore(t, databasePath)
	locker, err := sql.Open("sqlite", (&url.URL{
		Scheme: "file",
		Path:   databasePath,
	}).String())
	if err != nil {
		t.Fatalf("open lock connection: %v", err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	lockConnection, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatalf("acquire lock connection: %v", err)
	}
	t.Cleanup(func() { _ = lockConnection.Close() })
	if _, err := lockConnection.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("lock database for writing: %v", err)
	}
	t.Cleanup(func() { _, _ = lockConnection.ExecContext(context.Background(), "ROLLBACK") })

	handler, err := webhook.NewHandler(webhook.Config{
		VerificationToken:      "verification-token",
		AppID:                  "cli_test",
		TenantKey:              "tenant-test",
		InboxTimeout:           200 * time.Millisecond,
		PrincipalDisableSealer: testPrincipalDisableSealer(t),
	}, store)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	started := time.Now()
	response := postJSON(t, handler, approvalEvent("evt-contended", "instance-contended", "APPROVED"))
	elapsed := time.Since(started)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("contended inbox status = %d, want %d; body=%s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if elapsed >= time.Second {
		t.Fatalf("contended inbox response took %s, want less than 1s", elapsed)
	}
	if !strings.Contains(response.Body.String(), "inbox_unavailable") {
		t.Fatalf("contended inbox body = %s, want stable retryable error", response.Body.String())
	}
}

func approvalEvent(eventID, instanceCode, status string) map[string]any {
	return map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": eventID, "event_type": "approval.instance.status_changed_v4",
			"app_id": "cli_test", "tenant_key": "tenant-test", "token": "verification-token",
		},
		"event": map[string]any{
			"approval_code": "approval-wallet-v1", "instance_code": instanceCode, "status": status,
		},
	}
}

func openStore(t *testing.T, path string) *inbox.Store {
	t.Helper()
	store, err := inbox.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newEncryptedHandler(t *testing.T, recorder webhook.Recorder) http.Handler {
	t.Helper()
	handler, err := webhook.NewHandler(webhook.Config{
		VerificationToken:      "verification-token",
		EncryptKey:             "event-encryption-key",
		AppID:                  "cli_test",
		TenantKey:              "tenant-test",
		PrincipalDisableSealer: testPrincipalDisableSealer(t),
	}, recorder)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	return handler
}

func encryptedV2Request(t *testing.T, plaintext any) ([]byte, http.Header) {
	t.Helper()
	plainBody, err := json.Marshal(plaintext)
	if err != nil {
		t.Fatalf("encode plaintext event: %v", err)
	}
	encrypted, err := larkcore.EncryptedEventMsg(context.Background(), string(plainBody), "event-encryption-key")
	if err != nil {
		t.Fatalf("encrypt event: %v", err)
	}
	body, err := json.Marshal(map[string]string{"encrypt": encrypted})
	if err != nil {
		t.Fatalf("encode encrypted event: %v", err)
	}
	timestamp := "1787270400"
	nonce := "nonce-001"
	digest := sha256.Sum256([]byte(timestamp + nonce + "event-encryption-key" + string(body)))
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("X-Lark-Request-Timestamp", timestamp)
	headers.Set("X-Lark-Request-Nonce", nonce)
	headers.Set("X-Lark-Signature", hex.EncodeToString(digest[:]))
	return body, headers
}

func postRaw(t *testing.T, handler http.Handler, body []byte, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/integrations/lark/events", bytes.NewReader(body))
	request.Header = headers.Clone()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
