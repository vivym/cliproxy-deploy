package observability_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/observability"
)

type snapshotStore struct {
	snapshot inbox.OperationalSnapshot
	err      error
}

func (s snapshotStore) OperationalSnapshot(context.Context) (inbox.OperationalSnapshot, error) {
	return s.snapshot, s.err
}

func TestHealthAndReadinessSeparateProcessLivenessFromReadyQueueAge(t *testing.T) {
	handler, err := observability.NewHandler("shadow", snapshotStore{snapshot: inbox.OperationalSnapshot{
		OldestActiveJobAge: time.Hour,
		OldestReadyJobAge:  0,
	}}, 15*time.Minute)
	if err != nil {
		t.Fatalf("new observability handler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)

	health := httptest.NewRecorder()
	mux.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || health.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		!strings.Contains(health.Body.String(), `"status":"ok"`) ||
		!strings.Contains(health.Body.String(), `"mode":"shadow"`) {
		t.Fatalf("health response: status=%d content_type=%q body=%s",
			health.Code, health.Header().Get("Content-Type"), health.Body.String())
	}

	ready := httptest.NewRecorder()
	mux.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK || !strings.Contains(ready.Body.String(), `"status":"ready"`) {
		t.Fatalf("ready response: status=%d body=%s", ready.Code, ready.Body.String())
	}
}

func TestReadinessFailsForStalledReadyQueueOrUnavailableStore(t *testing.T) {
	tests := []struct {
		name   string
		store  snapshotStore
		reason string
	}{
		{
			name:   "stalled queue",
			store:  snapshotStore{snapshot: inbox.OperationalSnapshot{OldestReadyJobAge: 16 * time.Minute}},
			reason: "ready_queue_stalled",
		},
		{
			name:   "store unavailable",
			store:  snapshotStore{err: errors.New("sensitive database detail")},
			reason: "store_unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, err := observability.NewHandler("shadow", test.store, 15*time.Minute)
			if err != nil {
				t.Fatalf("new observability handler: %v", err)
			}
			mux := http.NewServeMux()
			handler.Register(mux)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if response.Code != http.StatusServiceUnavailable ||
				!strings.Contains(response.Body.String(), `"reason":"`+test.reason+`"`) ||
				strings.Contains(response.Body.String(), "sensitive") {
				t.Fatalf("readiness response: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestMetricsExposeBoundedOperationalLabelsAndQueueAge(t *testing.T) {
	handler, err := observability.NewHandler("shadow", snapshotStore{snapshot: inbox.OperationalSnapshot{
		WebhookReceived:   map[string]int64{"approval.instance.status_changed_v4": 3},
		WebhookDuplicates: map[string]int64{"approval.instance.status_changed_v4": 1},
		InboxStates:       map[inbox.ProcessingState]int64{inbox.ProcessingStatePending: 2},
		JobStates:         map[string]int64{"retry_wait": 1, "succeeded": 4},
		ApprovalFetches:   map[string]int64{"success": 4, "retryable_error": 2},
		NewAPIGrants:      map[string]int64{"shadow_planned": 3, "shadow_replayed": 1, "unexpected": 2},
		DeadLetters: map[string]int64{
			"retry_exhausted_rate_limited": 1,
			"external_id_payload_mismatch": 1,
		},
		PolicyValidationFailures: 1,
		OldestActiveJobAge:       90 * time.Second,
		OldestReadyJobAge:        10 * time.Second,
	}}, 15*time.Minute)
	if err != nil {
		t.Fatalf("new observability handler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK ||
		response.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("metrics response: status=%d content_type=%q", response.Code, response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	wantLines := []string{
		`lark_webhook_received_total{event_type="approval.instance.status_changed_v4"} 3`,
		`lark_webhook_duplicate_total{event_type="approval.instance.status_changed_v4"} 1`,
		`lark_controller_inbox_events{state="pending"} 2`,
		`lark_controller_jobs{state="retry_wait"} 1`,
		`lark_approval_fetch_total{result="retryable_error"} 2`,
		`lark_new_api_grant_total{result="shadow_planned"} 3`,
		`lark_new_api_grant_total{result="shadow_replayed"} 1`,
		`lark_new_api_grant_total{result="other"} 2`,
		`lark_policy_validation_failure_total 1`,
		`lark_controller_dead_letter_total{reason="retry_exhausted_rate_limited"} 1`,
		`lark_controller_dead_letter_total{reason="external_id_payload_mismatch"} 1`,
		`lark_controller_oldest_active_job_age_seconds 90`,
		`lark_controller_oldest_ready_job_age_seconds 10`,
		`lark_controller_ready 1`,
	}
	for _, line := range wantLines {
		if !strings.Contains(body, line+"\n") {
			t.Errorf("metrics body missing %q:\n%s", line, body)
		}
	}
}
