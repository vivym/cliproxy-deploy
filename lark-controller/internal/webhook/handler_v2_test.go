package webhook_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
		VerificationToken: "verification-token",
		EncryptKey:        "event-encryption-key",
		AppID:             "cli_test",
		TenantKey:         "tenant-test",
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
