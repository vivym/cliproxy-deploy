package webhook_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/webhook"
)

func TestV1ApprovalEventUsesTopLevelUUIDForDurableDeduplication(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "controller.sqlite"))
	handler, err := webhook.NewHandler(webhook.Config{
		VerificationToken: "verification-token",
		AppID:             "cli_test",
		TenantKey:         "tenant-test",
	}, store)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	payload := map[string]any{
		"ts":    "1787270400",
		"uuid":  "uuid-001",
		"token": "verification-token",
		"type":  "event_callback",
		"event": map[string]any{
			"type":          "approval_instance",
			"app_id":        "cli_test",
			"tenant_key":    "tenant-test",
			"approval_code": "approval-level-v1",
			"instance_code": "instance-v1-001",
			"status":        "PENDING",
		},
	}
	for attempt := 0; attempt < 2; attempt++ {
		response := postJSON(t, handler, payload)
		if response.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want %d; body=%s", attempt+1, response.Code, http.StatusOK, response.Body.String())
		}
	}

	recorded, err := store.Get(context.Background(), "lark:v1:uuid-001")
	if err != nil {
		t.Fatalf("get recorded event: %v", err)
	}
	want := inbox.Event{
		EventType: "approval_instance", ApprovalCode: "approval-level-v1",
		InstanceCode: "instance-v1-001", Status: "PENDING", DuplicateCount: 1,
	}
	if recorded.EventType != want.EventType || recorded.ApprovalCode != want.ApprovalCode ||
		recorded.InstanceCode != want.InstanceCode || recorded.Status != want.Status ||
		recorded.DuplicateCount != want.DuplicateCount {
		t.Fatalf("recorded event = %+v, want fields %+v", recorded, want)
	}
}

func TestV1EventRejectsNonCallbackEnvelopeWithoutRecording(t *testing.T) {
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
		"uuid":  "uuid-wrong-type",
		"token": "verification-token",
		"type":  "approval_callback",
		"event": map[string]any{
			"type":          "approval_instance",
			"app_id":        "cli_test",
			"tenant_key":    "tenant-test",
			"approval_code": "approval-level-v1",
			"instance_code": "instance-v1-wrong-type",
			"status":        "APPROVED",
		},
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("non-callback status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if recorder.records != 0 {
		t.Fatalf("non-callback envelope wrote %d inbox records, want 0", recorder.records)
	}
}

func TestEventRejectsUnknownSchemaWithoutRecording(t *testing.T) {
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
		"schema": "3.0",
		"uuid":   "uuid-unknown-schema",
		"token":  "verification-token",
		"type":   "event_callback",
		"event": map[string]any{
			"type":          "approval_instance",
			"app_id":        "cli_test",
			"tenant_key":    "tenant-test",
			"approval_code": "approval-level-v1",
			"instance_code": "instance-v1-unknown-schema",
			"status":        "APPROVED",
		},
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown schema status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if recorder.records != 0 {
		t.Fatalf("unknown schema wrote %d inbox records, want 0", recorder.records)
	}
}
