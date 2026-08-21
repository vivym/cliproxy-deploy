package worker_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

type approvalFetcher struct {
	calls    int
	instance worker.ApprovalInstance
}

type approvalResolver struct {
	calls      int
	request    policy.ApprovalRequest
	resolution policy.ApprovalResolution
	err        error
}

func (r *approvalResolver) ResolveApproval(request policy.ApprovalRequest) (policy.ApprovalResolution, error) {
	r.calls++
	r.request = request
	return r.resolution, r.err
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
		outcome    inbox.DecisionOutcome
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
			resolver := &approvalResolver{resolution: verifiedWalletResolution()}
			processor, err := worker.NewShadowProcessor(store, fetcher, resolver, "zh-CN")
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
	resolver := &approvalResolver{resolution: verifiedWalletResolution()}
	processor, err := worker.NewShadowProcessor(store, fetcher, resolver, "zh-CN")
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
	resolver := &approvalResolver{resolution: verifiedWalletResolution()}
	processor, err := worker.NewShadowProcessor(store, fetcher, resolver, "zh-CN")
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
	processor, err = worker.NewShadowProcessor(store, fetcher, resolver, "zh-CN")
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

func TestShadowProcessorPersistsResolvedPolicyEvidence(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Record(ctx, inbox.Event{
		Key: "lark:v2:evt-policy", SchemaVersion: "2.0", EventID: "evt-policy",
		EventType: "approval.instance.status_changed_v4", AppID: "cli_test", TenantKey: "tenant-test",
		ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-policy", Status: "APPROVED",
		PayloadJSON: `{"approval_code":"approval-wallet-v1","instance_code":"instance-policy","status":"APPROVED"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	fetcher := &approvalFetcher{instance: worker.ApprovalInstance{
		ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-policy",
		Status: "APPROVED", OpenID: "ou_requester", StartTime: "1787270300000",
		FormJSON: `[{"custom_id":"wallet_package","type":"radioV2","value":"Small"}]`,
	}}
	resolver := &approvalResolver{resolution: policy.ApprovalResolution{
		PolicyVersion: "employee-v1", ApprovalKind: policy.ApprovalKindWalletTopUp,
		BusinessCode: "topup_5", QuotaDelta: 2500000,
		SchemaFingerprint: walletFingerprintForWorkerTest,
		CatalogSHA256:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}
	processor, err := worker.NewShadowProcessor(store, fetcher, resolver, "zh-CN")
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if _, err := processor.RunOnce(ctx); err != nil {
		t.Fatalf("process event: %v", err)
	}
	if resolver.calls != 1 || resolver.request.ApprovalCode != "approval-wallet-v1" ||
		resolver.request.Locale != "zh-CN" || resolver.request.StartTime != "1787270300000" {
		t.Fatalf("unexpected resolver call: calls=%d request=%+v", resolver.calls, resolver.request)
	}
	decision, err := store.GetDecision(ctx, "lark:v2:evt-policy")
	if err != nil {
		t.Fatalf("get decision: %v", err)
	}
	if decision.Outcome != inbox.DecisionOutcomeShadowAuthorityVerified ||
		decision.PolicyVersion != "employee-v1" || decision.ApprovalKind != "wallet_topup" ||
		decision.SchemaFingerprint != walletFingerprintForWorkerTest ||
		decision.BusinessCode != "topup_5" || decision.Locale != "zh-CN" ||
		decision.CatalogSHA256 != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("unexpected policy evidence: %+v", decision)
	}
}

func TestShadowProcessorDeadLettersPolicyValidationFailure(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Record(ctx, inbox.Event{
		Key: "lark:v2:evt-policy-drift", SchemaVersion: "2.0", EventID: "evt-policy-drift",
		EventType: "approval.instance.status_changed_v4", AppID: "cli_test", TenantKey: "tenant-test",
		ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-policy-drift", Status: "APPROVED",
		PayloadJSON: `{"status":"APPROVED"}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	fetcher := &approvalFetcher{instance: worker.ApprovalInstance{
		ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-policy-drift",
		Status: "APPROVED", OpenID: "ou_requester", StartTime: "1787270300000",
		FormJSON: `[{"custom_id":"wallet_package","type":"radioV2","value":"Unknown"}]`,
	}}
	resolver := &approvalResolver{err: errors.New("unknown display text contains sensitive form details")}
	processor, err := worker.NewShadowProcessor(store, fetcher, resolver, "zh-CN")
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if _, err := processor.RunOnce(ctx); err != nil {
		t.Fatalf("process event: %v", err)
	}
	decision, err := store.GetDecision(ctx, "lark:v2:evt-policy-drift")
	if err != nil {
		t.Fatalf("get decision: %v", err)
	}
	if decision.Outcome != inbox.DecisionOutcomeDeadLetterPolicyValidation ||
		decision.PolicyVersion != "" || decision.BusinessCode != "" {
		t.Fatalf("unexpected policy rejection decision: %+v", decision)
	}
}

const walletFingerprintForWorkerTest = "sha256:2878401247d5cde57a96e03424e944773b21399dc3a68a9508016c2c5adea48b"

func verifiedWalletResolution() policy.ApprovalResolution {
	return policy.ApprovalResolution{
		PolicyVersion: "employee-v1", ApprovalKind: policy.ApprovalKindWalletTopUp,
		BusinessCode: "topup_5", QuotaDelta: 2500000,
		SchemaFingerprint: walletFingerprintForWorkerTest,
		CatalogSHA256:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
}
