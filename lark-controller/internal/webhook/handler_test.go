package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/webhook"
)

type eventRecorder struct {
	records int
	err     error
}

func (r *eventRecorder) Record(context.Context, inbox.Event) (bool, error) {
	r.records++
	return false, r.err
}

func TestURLVerificationRequiresConfiguredToken(t *testing.T) {
	recorder := &eventRecorder{}
	handler, err := webhook.NewHandler(webhook.Config{
		VerificationToken: "verification-token",
		AppID:             "cli_test",
		TenantKey:         "tenant-test",
	}, recorder)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	valid := postJSON(t, handler, map[string]any{
		"challenge": "challenge-value",
		"token":     "verification-token",
		"type":      "url_verification",
	})
	if valid.Code != http.StatusOK {
		t.Fatalf("valid challenge status = %d, want %d", valid.Code, http.StatusOK)
	}
	var response map[string]string
	if err := json.Unmarshal(valid.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode challenge response: %v", err)
	}
	if response["challenge"] != "challenge-value" {
		t.Fatalf("challenge = %q, want challenge-value", response["challenge"])
	}

	invalid := postJSON(t, handler, map[string]any{
		"challenge": "challenge-value",
		"token":     "wrong-secret-token",
		"type":      "url_verification",
	})
	if invalid.Code != http.StatusUnauthorized {
		t.Fatalf("invalid challenge status = %d, want %d", invalid.Code, http.StatusUnauthorized)
	}
	if strings.Contains(invalid.Body.String(), "wrong-secret-token") {
		t.Fatal("invalid challenge response exposed the supplied token")
	}
	if recorder.records != 0 {
		t.Fatalf("challenge wrote %d inbox records, want 0", recorder.records)
	}
}

func TestURLVerificationRejectsUnknownSchema(t *testing.T) {
	recorder := &eventRecorder{}
	handler, err := webhook.NewHandler(webhook.Config{
		VerificationToken: "verification-token",
		AppID:             "cli_test",
		TenantKey:         "tenant-test",
	}, recorder)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	response := postJSON(t, handler, map[string]any{
		"schema":    "3.0",
		"challenge": "challenge-value",
		"token":     "verification-token",
		"type":      "url_verification",
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown challenge schema status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if recorder.records != 0 {
		t.Fatalf("unknown challenge schema wrote %d inbox records, want 0", recorder.records)
	}
}

func TestEventReturnsRetryableFailureUntilInboxCommitSucceeds(t *testing.T) {
	handler, err := webhook.NewHandler(webhook.Config{
		VerificationToken: "verification-token",
		AppID:             "cli_test",
		TenantKey:         "tenant-test",
	}, &eventRecorder{err: errors.New("database unavailable")})
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	response := postJSON(t, handler, map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": "evt-failed", "event_type": "approval.instance.status_changed_v4",
			"app_id": "cli_test", "tenant_key": "tenant-test", "token": "verification-token",
		},
		"event": map[string]any{
			"approval_code": "approval-wallet-v1", "instance_code": "instance-failed", "status": "APPROVED",
		},
	})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("inbox failure status = %d, want %d; body=%s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "inbox_unavailable") ||
		strings.Contains(response.Body.String(), "database unavailable") {
		t.Fatalf("unexpected inbox failure body: %s", response.Body.String())
	}
}

func postJSON(t *testing.T, handler http.Handler, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/integrations/lark/events", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
