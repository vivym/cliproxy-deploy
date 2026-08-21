package worker_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

type approvalFetcher struct {
	calls    int
	instance worker.ApprovalInstance
	failures []error
}

type approvalResolver struct {
	calls       int
	request     policy.ApprovalRequest
	resolution  policy.ApprovalResolution
	resolutions []policy.ApprovalResolution
	err         error
}

func (r *approvalResolver) ResolveApproval(request policy.ApprovalRequest) (policy.ApprovalResolution, error) {
	r.calls++
	r.request = request
	if r.calls <= len(r.resolutions) {
		return r.resolutions[r.calls-1], r.err
	}
	return r.resolution, r.err
}

func (f *approvalFetcher) Fetch(_ context.Context, instanceCode, locale string) (worker.ApprovalInstance, error) {
	f.calls++
	if f.calls <= len(f.failures) {
		return worker.ApprovalInstance{}, f.failures[f.calls-1]
	}
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

func TestShadowProcessorDeadLettersTerminalApprovalFetchFailure(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordApprovedEvent(t, ctx, store, "evt-terminal-fetch")

	fetcher := &approvalFetcher{failures: []error{&worker.ApprovalFetchError{
		Reason: worker.ApprovalFetchClientError,
	}}}
	processor, err := worker.NewShadowProcessor(
		store, fetcher, &approvalResolver{resolution: verifiedWalletResolution()}, "zh-CN",
	)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("process terminal fetch failure: processed=%t err=%v", processed, err)
	}
	decision, err := store.GetDecision(ctx, "lark:v2:evt-terminal-fetch")
	if err != nil {
		t.Fatalf("get terminal fetch decision: %v", err)
	}
	if decision.Outcome != "dead_letter_approval_fetch_failed" || decision.FailureReason != "client_error" {
		t.Fatalf("terminal fetch decision = %+v", decision)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || processed {
		t.Fatalf("terminal fetch job was reclaimed: processed=%t err=%v", processed, err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetcher.calls)
	}
}

func TestShadowProcessorNormalizesUnknownFetchFailureReason(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordApprovedEvent(t, ctx, store, "evt-unknown-fetch-reason")

	fetcher := &approvalFetcher{failures: []error{&worker.ApprovalFetchError{
		Reason: "external_text_must_not_be_persisted", Retryable: true,
	}}}
	processor, err := worker.NewShadowProcessor(
		store, fetcher, &approvalResolver{resolution: verifiedWalletResolution()}, "zh-CN",
	)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("process unknown failure: processed=%t err=%v", processed, err)
	}
	decision, err := store.GetDecision(ctx, "lark:v2:evt-unknown-fetch-reason")
	if err != nil {
		t.Fatalf("get unknown failure decision: %v", err)
	}
	if decision.FailureReason != "unclassified_error" {
		t.Fatalf("failure reason = %q, want unclassified_error", decision.FailureReason)
	}
}

func TestShadowProcessorRetriesTransientFetchFailureOnlyWithinSchedule(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordApprovedEvent(t, ctx, store, "evt-retry-exhausted")

	fetcher := &approvalFetcher{failures: []error{
		&worker.ApprovalFetchError{Reason: worker.ApprovalFetchRateLimited, Retryable: true},
		&worker.ApprovalFetchError{Reason: worker.ApprovalFetchRateLimited, Retryable: true},
	}}
	processor, err := worker.NewShadowProcessor(
		store, fetcher, &approvalResolver{resolution: verifiedWalletResolution()}, "zh-CN",
		worker.WithRetryPolicy(worker.RetryPolicy{
			Schedule: []time.Duration{time.Nanosecond}, MaxDelay: time.Hour,
		}),
	)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("schedule retry: processed=%t err=%v", processed, err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("exhaust retry: processed=%t err=%v", processed, err)
	}
	decision, err := store.GetDecision(ctx, "lark:v2:evt-retry-exhausted")
	if err != nil {
		t.Fatalf("get exhausted fetch decision: %v", err)
	}
	if decision.Outcome != "dead_letter_approval_fetch_failed" ||
		decision.FailureReason != "retry_exhausted_rate_limited" {
		t.Fatalf("exhausted fetch decision = %+v", decision)
	}
	if fetcher.calls != 2 {
		t.Fatalf("fetch calls = %d, want 2", fetcher.calls)
	}
}

func TestShadowProcessorRetriesTransientFetchThenSucceeds(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordApprovedEvent(t, ctx, store, "evt-retry-success")

	fetcher := &approvalFetcher{failures: []error{&worker.ApprovalFetchError{
		Reason: worker.ApprovalFetchServerError, Retryable: true,
	}}}
	processor, err := worker.NewShadowProcessor(
		store, fetcher, &approvalResolver{resolution: verifiedWalletResolution()}, "zh-CN",
		worker.WithRetryPolicy(worker.RetryPolicy{
			Schedule: []time.Duration{time.Nanosecond}, MaxDelay: time.Hour,
		}),
	)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if processed, err := processor.RunOnce(ctx); err != nil || !processed {
			t.Fatalf("process attempt %d: processed=%t err=%v", attempt+1, processed, err)
		}
	}
	decision, err := store.GetDecision(ctx, "lark:v2:evt-retry-success")
	if err != nil {
		t.Fatalf("get successful retry decision: %v", err)
	}
	if decision.Outcome != inbox.DecisionOutcomeShadowAuthorityVerified || decision.FailureReason != "" {
		t.Fatalf("successful retry decision = %+v", decision)
	}
	if fetcher.calls != 2 {
		t.Fatalf("fetch calls = %d, want 2", fetcher.calls)
	}
}

func TestShadowProcessorHonorsRetryAfterBeforeReclaimingJob(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordApprovedEvent(t, ctx, store, "evt-retry-after")

	fetcher := &approvalFetcher{failures: []error{&worker.ApprovalFetchError{
		Reason: worker.ApprovalFetchRateLimited, Retryable: true, RetryAfter: time.Hour,
	}}}
	processor, err := worker.NewShadowProcessor(
		store, fetcher, &approvalResolver{resolution: verifiedWalletResolution()}, "zh-CN",
		worker.WithRetryPolicy(worker.RetryPolicy{
			Schedule: []time.Duration{time.Nanosecond}, MaxDelay: 2 * time.Hour,
		}),
	)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("schedule retry-after: processed=%t err=%v", processed, err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || processed {
		t.Fatalf("retry-after job reclaimed early: processed=%t err=%v", processed, err)
	}
	if fetcher.calls != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetcher.calls)
	}
}

func TestShadowProcessorClampsJitterBeforeDurationOverflow(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordApprovedEvent(t, ctx, store, "evt-jitter-overflow")

	fetcher := &approvalFetcher{failures: []error{&worker.ApprovalFetchError{
		Reason: worker.ApprovalFetchServerError, Retryable: true,
	}}}
	const maxDuration = time.Duration(1<<63 - 1)
	processor, err := worker.NewShadowProcessor(
		store, fetcher, &approvalResolver{resolution: verifiedWalletResolution()}, "zh-CN",
		worker.WithRetryPolicy(worker.RetryPolicy{
			Schedule: []time.Duration{maxDuration}, MaxDelay: maxDuration, JitterFraction: 0.5,
		}),
	)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("schedule clamped jitter retry: processed=%t err=%v", processed, err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || processed {
		t.Fatalf("overflowed retry became immediately ready: processed=%t err=%v", processed, err)
	}
}

func TestShadowProcessorClampsJitterToPositiveDuration(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordApprovedEvent(t, ctx, store, "evt-jitter-underflow")

	fetcher := &approvalFetcher{failures: []error{&worker.ApprovalFetchError{
		Reason: worker.ApprovalFetchServerError, Retryable: true,
	}}}
	processor, err := worker.NewShadowProcessor(
		store, fetcher, &approvalResolver{resolution: verifiedWalletResolution()}, "zh-CN",
		worker.WithRetryPolicy(worker.RetryPolicy{
			Schedule: []time.Duration{time.Nanosecond}, MaxDelay: time.Hour, JitterFraction: 0.5,
		}),
	)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("schedule positive jitter retry: processed=%t err=%v", processed, err)
	}
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
				Status: "APPROVED", OpenID: "ou_requester", StartTime: "1787270300000", FormJSON: `[]`,
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
	command, err := store.GetEntitlementCommandShadow(ctx, "lark:v2:evt-policy")
	if err != nil {
		t.Fatalf("get entitlement command shadow: %v", err)
	}
	if command.ExternalID != "lark:wallet-topup:instance-policy" ||
		command.Source != "lark_approval" || command.PolicyVersion != "employee-v1" ||
		command.GrantType != "wallet_quota" || command.BusinessCode != "topup_5" ||
		command.QuotaDelta != 2_500_000 || command.MonthlyQuota != 0 ||
		command.SubjectSHA256 != "51b4284131693f52e5701a9aa003814e2290e41df7a1825b17c9f3a553434afa" ||
		command.RequestSHA256 == "" || command.Outcome != "shadow_planned" {
		t.Fatalf("unexpected entitlement command shadow: %+v", command)
	}
	if command.SubjectSHA256 == "tenant-test:ou_requester" || command.SubjectSHA256 == "ou_requester" {
		t.Fatalf("entitlement command retained raw subject: %+v", command)
	}
}

func TestShadowProcessorWithGrantSealerPersistsHeldCanonicalRequest(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordApprovedEvent(t, ctx, store, "evt-sealed-grant")
	sealer, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant sealer: %v", err)
	}
	processor, err := worker.NewShadowProcessorWithGrantSealer(
		store,
		&approvalFetcher{},
		&approvalResolver{resolution: verifiedWalletResolution()},
		"zh-CN",
		sealer,
	)
	if err != nil {
		t.Fatalf("new processor with grant sealer: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("process sealed grant: processed=%t err=%v", processed, err)
	}
	stored, err := store.GetEntitlementGrantJob(ctx, "lark:wallet-topup:instance-evt-sealed-grant")
	if err != nil {
		t.Fatalf("get entitlement grant job: %v", err)
	}
	opened, err := sealer.Open(newapi.SealedGrantRequest{
		KeyID: stored.KeyID, ExternalID: stored.ExternalID,
		RequestSHA256: stored.RequestSHA256, Nonce: stored.Nonce,
		Ciphertext: stored.Ciphertext,
	})
	if err != nil {
		t.Fatalf("open entitlement grant job: %v", err)
	}
	if opened.ExternalID != stored.ExternalID || opened.Identity.Subject != "tenant-test:ou_requester" ||
		opened.Grant.Type != "wallet_quota" || opened.Grant.QuotaDelta != 2_500_000 {
		t.Fatalf("unexpected held request: %+v", opened)
	}
}

func TestShadowProcessorReplaysSameEntitlementCommandAcrossDistinctEvents(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, eventID := range []string{"evt-first", "evt-recovery"} {
		_, err := store.Record(ctx, inbox.Event{
			Key: "lark:v2:" + eventID, SchemaVersion: "2.0", EventID: eventID,
			EventType: "approval.instance.status_changed_v4", AppID: "cli_test", TenantKey: "tenant-test",
			ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-shared", Status: "APPROVED",
			PayloadJSON: `{"status":"APPROVED"}`,
		})
		if err != nil {
			t.Fatalf("record %s: %v", eventID, err)
		}
	}
	sealer, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant sealer: %v", err)
	}
	processor, err := worker.NewShadowProcessorWithGrantSealer(
		store,
		&approvalFetcher{},
		&approvalResolver{resolution: verifiedWalletResolution()},
		"zh-CN",
		sealer,
	)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("process first command: processed=%t err=%v", processed, err)
	}
	heldFirst, err := store.GetEntitlementGrantJob(ctx, "lark:wallet-topup:instance-shared")
	if err != nil {
		t.Fatalf("get first held grant job: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("process command replay: processed=%t err=%v", processed, err)
	}
	heldReplay, err := store.GetEntitlementGrantJob(ctx, "lark:wallet-topup:instance-shared")
	if err != nil {
		t.Fatalf("get replayed held grant job: %v", err)
	}
	if heldReplay.ID != heldFirst.ID || !bytes.Equal(heldReplay.Nonce, heldFirst.Nonce) ||
		!bytes.Equal(heldReplay.Ciphertext, heldFirst.Ciphertext) {
		t.Fatalf("replay replaced first held job: first=%+v replay=%+v", heldFirst, heldReplay)
	}
	first, err := store.GetEntitlementCommandShadow(ctx, "lark:v2:evt-first")
	if err != nil {
		t.Fatalf("get first command: %v", err)
	}
	replay, err := store.GetEntitlementCommandShadow(ctx, "lark:v2:evt-recovery")
	if err != nil {
		t.Fatalf("get replayed command: %v", err)
	}
	if first.Outcome != "shadow_planned" || replay.Outcome != "shadow_replayed" ||
		first.ExternalID != replay.ExternalID || first.RequestSHA256 != replay.RequestSHA256 {
		t.Fatalf("unexpected command replay: first=%+v replay=%+v", first, replay)
	}
}

func TestShadowProcessorDeadLettersExternalIDPayloadMismatch(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, eventID := range []string{"evt-original", "evt-conflict"} {
		_, err := store.Record(ctx, inbox.Event{
			Key: "lark:v2:" + eventID, SchemaVersion: "2.0", EventID: eventID,
			EventType: "approval.instance.status_changed_v4", AppID: "cli_test", TenantKey: "tenant-test",
			ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-conflict", Status: "APPROVED",
			PayloadJSON: `{"status":"APPROVED"}`,
		})
		if err != nil {
			t.Fatalf("record %s: %v", eventID, err)
		}
	}
	conflictingResolution := verifiedWalletResolution()
	conflictingResolution.BusinessCode = "topup_10"
	conflictingResolution.QuotaDelta = 5_000_000
	processor, err := worker.NewShadowProcessor(
		store,
		&approvalFetcher{},
		&approvalResolver{resolutions: []policy.ApprovalResolution{
			verifiedWalletResolution(), conflictingResolution,
		}},
		"zh-CN",
	)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	for range 2 {
		if processed, err := processor.RunOnce(ctx); err != nil || !processed {
			t.Fatalf("process command conflict: processed=%t err=%v", processed, err)
		}
	}
	decision, err := store.GetDecision(ctx, "lark:v2:evt-conflict")
	if err != nil {
		t.Fatalf("get conflict decision: %v", err)
	}
	if decision.Outcome != inbox.DecisionOutcomeDeadLetterCommandPlanning ||
		decision.FailureReason != "external_id_payload_mismatch" {
		t.Fatalf("unexpected conflict decision: %+v", decision)
	}
	if _, err := store.GetEntitlementCommandShadow(ctx, "lark:v2:evt-conflict"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("conflicting command shadow error = %v, want no row", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || processed {
		t.Fatalf("conflicting command was reclaimed: processed=%t err=%v", processed, err)
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

func recordApprovedEvent(t *testing.T, ctx context.Context, store *inbox.Store, eventID string) {
	t.Helper()
	_, err := store.Record(ctx, inbox.Event{
		Key: "lark:v2:" + eventID, SchemaVersion: "2.0", EventID: eventID,
		EventType: "approval.instance.status_changed_v4", AppID: "cli_test", TenantKey: "tenant-test",
		ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-" + eventID, Status: "APPROVED",
		PayloadJSON: `{"status":"APPROVED"}`,
	})
	if err != nil {
		t.Fatalf("record approved event: %v", err)
	}
}
