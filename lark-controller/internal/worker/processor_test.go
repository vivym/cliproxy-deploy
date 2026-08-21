package worker_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

type approvalFetcher struct {
	calls    int
	instance worker.ApprovalInstance
}

func (f *approvalFetcher) Fetch(_ context.Context, instanceCode, locale string) (worker.ApprovalInstance, error) {
	f.calls++
	if f.instance.InstanceCode != "" {
		return f.instance, nil
	}
	return worker.ApprovalInstance{
		ApprovalCode: "approval-wallet-v1",
		InstanceCode: instanceCode,
		Status:       "APPROVED",
		OpenID:       "ou_requester",
		StartTime:    "1787270300000",
		FormJSON:     `[{"custom_id":"wallet_package","value":"Small"}]`,
	}, nil
}

func TestShadowProcessorFailsClosedAcrossEventStatusMatrix(t *testing.T) {
	tests := []struct {
		status     string
		fetchCalls int
		outcome    string
	}{
		{status: "PENDING", fetchCalls: 0, outcome: "shadow_ignored_non_approved"},
		{status: "REJECTED", fetchCalls: 0, outcome: "shadow_ignored_non_approved"},
		{status: "CANCELED", fetchCalls: 0, outcome: "shadow_ignored_non_approved"},
		{status: "DELETED", fetchCalls: 0, outcome: "shadow_ignored_non_approved"},
		{status: "OVERTIME_CLOSE", fetchCalls: 0, outcome: "shadow_ignored_non_approved"},
		{status: "OVERTIME_RECOVER", fetchCalls: 1, outcome: "shadow_authority_verified"},
		{status: "REVERTED", fetchCalls: 0, outcome: "reversal_pending"},
		{status: "UNKNOWN_FUTURE_STATUS", fetchCalls: 0, outcome: "dead_letter_unknown_status"},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			ctx := context.Background()
			store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			eventKey := "lark:v2:evt-" + test.status
			_, err = store.Record(ctx, inbox.Event{
				Key: eventKey, SchemaVersion: "2.0", EventID: "evt-" + test.status,
				EventType: "approval.instance.status_changed_v4", AppID: "cli_test", TenantKey: "tenant-test",
				ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-" + test.status, Status: test.status,
				PayloadJSON: `{"status":"` + test.status + `"}`,
			})
			if err != nil {
				t.Fatalf("record event: %v", err)
			}
			fetcher := &approvalFetcher{instance: worker.ApprovalInstance{
				ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-" + test.status,
				Status: "APPROVED", OpenID: "ou_requester", FormJSON: `[]`,
			}}
			processor, err := worker.NewShadowProcessor(store, fetcher, "zh-CN")
			if err != nil {
				t.Fatalf("new processor: %v", err)
			}
			processed, err := processor.RunOnce(ctx)
			if err != nil {
				t.Fatalf("process event: %v", err)
			}
			if !processed {
				t.Fatal("processor reported no work")
			}
			if fetcher.calls != test.fetchCalls {
				t.Fatalf("fetch calls = %d, want %d", fetcher.calls, test.fetchCalls)
			}
			decision, err := store.GetDecision(ctx, eventKey)
			if err != nil {
				t.Fatalf("get decision: %v", err)
			}
			if decision.Outcome != test.outcome {
				t.Fatalf("outcome = %q, want %q", decision.Outcome, test.outcome)
			}
		})
	}
}

func TestShadowProcessorRejectsUnsupportedEventTypeBeforeFetch(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Record(ctx, inbox.Event{
		Key: "lark:v2:task-001", SchemaVersion: "2.0", EventID: "task-001",
		EventType: "approval.task.status_changed_v4", AppID: "cli_test", TenantKey: "tenant-test",
		ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-001", Status: "APPROVED",
		PayloadJSON: `{"status":"APPROVED"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	fetcher := &approvalFetcher{}
	processor, err := worker.NewShadowProcessor(store, fetcher, "zh-CN")
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if _, err := processor.RunOnce(ctx); err != nil {
		t.Fatalf("process event: %v", err)
	}
	if fetcher.calls != 0 {
		t.Fatalf("approval task triggered %d instance fetches, want 0", fetcher.calls)
	}
	decision, err := store.GetDecision(ctx, "lark:v2:task-001")
	if err != nil {
		t.Fatalf("get decision: %v", err)
	}
	if decision.Outcome != "dead_letter_unsupported_event_type" {
		t.Fatalf("outcome = %q, want dead_letter_unsupported_event_type", decision.Outcome)
	}
}

func TestShadowProcessorFetchesApprovedInstanceOnceAndPersistsSanitizedDecision(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	_, err = store.Record(ctx, inbox.Event{
		Key: "lark:v2:evt-001", SchemaVersion: "2.0", EventID: "evt-001",
		EventType: "approval.instance.status_changed_v4", AppID: "cli_test", TenantKey: "tenant-test",
		ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-001", Status: "APPROVED",
		PayloadJSON: `{"approval_code":"approval-wallet-v1","instance_code":"instance-001","status":"APPROVED"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}

	fetcher := &approvalFetcher{}
	processor, err := worker.NewShadowProcessor(store, fetcher, "zh-CN")
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	processed, err := processor.RunOnce(ctx)
	if err != nil {
		t.Fatalf("process event: %v", err)
	}
	if !processed {
		t.Fatal("processor reported no work")
	}
	if fetcher.calls != 1 {
		t.Fatalf("approval fetch calls = %d, want 1", fetcher.calls)
	}
	decision, err := store.GetDecision(ctx, "lark:v2:evt-001")
	if err != nil {
		t.Fatalf("get decision: %v", err)
	}
	if decision.Outcome != "shadow_authority_verified" || decision.AuthorityStatus != "APPROVED" {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	if decision.OpenIDHash == "" || decision.FormSHA256 == "" {
		t.Fatalf("decision is missing sanitized evidence: %+v", decision)
	}
	if decision.OpenIDHash == "ou_requester" || decision.FormSHA256 == worker.HashEvidence("different") {
		t.Fatalf("decision retained raw identity or wrong evidence hash: %+v", decision)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	store, err = inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	processor, err = worker.NewShadowProcessor(store, fetcher, "zh-CN")
	if err != nil {
		t.Fatalf("new restarted processor: %v", err)
	}
	processed, err = processor.RunOnce(ctx)
	if err != nil {
		t.Fatalf("process after restart: %v", err)
	}
	if processed {
		t.Fatal("completed event was processed again after restart")
	}
	if fetcher.calls != 1 {
		t.Fatalf("approval fetch calls after restart = %d, want 1", fetcher.calls)
	}
}
