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
	calls         int
	instance      worker.ApprovalInstance
	failures      []error
	instanceCodes []string
	locales       []string
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
	f.instanceCodes = append(f.instanceCodes, instanceCode)
	f.locales = append(f.locales, locale)
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

func TestShadowProcessorFetchesExplicitReversalTargetAndRecordsMissingOriginal(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.Record(ctx, inbox.Event{
		Key: "lark:v2:evt-reversal-explicit", SchemaVersion: "2.0",
		EventID: "evt-reversal-explicit", EventType: "approval.instance.status_changed_v4",
		AppID: "cli_test", TenantKey: "tenant-test", ApprovalCode: "approval-wallet-v1",
		InstanceCode: "instance-reversal", RevertedInstanceCode: "instance-original",
		Status: "REVERTED", PayloadJSON: `{"status":"REVERTED"}`,
	})
	if err != nil {
		t.Fatalf("record reversal event: %v", err)
	}
	fetcher := &approvalFetcher{instance: worker.ApprovalInstance{
		ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-original",
		Status: "APPROVED", OpenID: "ou-requester", StartTime: "1787270300000",
		FormJSON: `[]`, Reverted: true,
	}}
	resolver := &approvalResolver{resolution: verifiedWalletResolution()}
	processor, err := worker.NewShadowProcessor(store, fetcher, resolver, "zh-CN")
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("process reversal: processed=%t err=%v", processed, err)
	}
	if fetcher.calls != 1 || len(fetcher.instanceCodes) != 1 ||
		fetcher.instanceCodes[0] != "instance-original" || fetcher.locales[0] != "zh-CN" {
		t.Fatalf("reversal fetch calls=%d instances=%v locales=%v", fetcher.calls, fetcher.instanceCodes, fetcher.locales)
	}
	if resolver.calls != 0 {
		t.Fatalf("reversal invoked approval policy resolver %d times, want 0", resolver.calls)
	}
	reversal, err := store.GetApprovalReversal(ctx, "lark:v2:evt-reversal-explicit")
	if err != nil {
		t.Fatalf("get reversal ledger: %v", err)
	}
	if reversal.Result != inbox.ApprovalReversalResultOriginalMissing ||
		reversal.Reason != inbox.ApprovalReversalReasonOriginalMissing ||
		reversal.TargetInstanceCode != "instance-original" ||
		reversal.AuthorityStatus != "APPROVED" || !reversal.AuthorityReverted {
		t.Fatalf("unexpected reversal ledger: %+v", reversal)
	}
	decision, err := store.GetDecision(ctx, "lark:v2:evt-reversal-explicit")
	if err != nil {
		t.Fatalf("get reversal decision: %v", err)
	}
	if decision.Outcome != inbox.DecisionOutcomeReversalPending ||
		decision.InstanceCode != "instance-original" || !decision.Reverted {
		t.Fatalf("unexpected reversal decision: %+v", decision)
	}
}

func TestShadowProcessorUsesEventInstanceAsReversalFallbackTarget(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordReversalEvent(t, ctx, store, "fallback", "instance-original", "")
	fetcher := &approvalFetcher{instance: worker.ApprovalInstance{
		ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-original",
		Status: "APPROVED", OpenID: "ou-requester", StartTime: "1787270300000",
		FormJSON: `[]`, Reverted: true,
	}}
	processor, err := worker.NewShadowProcessor(
		store, fetcher, &approvalResolver{resolution: verifiedWalletResolution()}, "zh-CN",
	)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("process fallback reversal: processed=%t err=%v", processed, err)
	}
	if len(fetcher.instanceCodes) != 1 || fetcher.instanceCodes[0] != "instance-original" {
		t.Fatalf("fallback fetch targets = %v, want exact event instance", fetcher.instanceCodes)
	}
	reversal, err := store.GetApprovalReversal(ctx, "lark:v2:fallback")
	if err != nil {
		t.Fatalf("get fallback reversal: %v", err)
	}
	if reversal.TargetInstanceCode != "instance-original" {
		t.Fatalf("fallback target = %q, want instance-original", reversal.TargetInstanceCode)
	}
}

func TestShadowProcessorDoesNotFetchWithoutExactReversalTarget(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordReversalEvent(t, ctx, store, "missing-target", "", "")
	fetcher := &approvalFetcher{}
	processor, err := worker.NewShadowProcessor(
		store, fetcher, &approvalResolver{resolution: verifiedWalletResolution()}, "zh-CN",
	)
	if err != nil {
		t.Fatalf("new processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("process targetless reversal: processed=%t err=%v", processed, err)
	}
	if fetcher.calls != 0 {
		t.Fatalf("targetless reversal made %d fetches, want 0", fetcher.calls)
	}
	reversal, err := store.GetApprovalReversal(ctx, "lark:v2:missing-target")
	if err != nil {
		t.Fatalf("get targetless reversal: %v", err)
	}
	if reversal.Result != inbox.ApprovalReversalResultAuthorityMismatch ||
		reversal.Reason != inbox.ApprovalReversalReasonTargetMissing ||
		reversal.TargetInstanceCode != "" {
		t.Fatalf("targetless reversal = %+v", reversal)
	}
}

func TestShadowProcessorAuthorityMismatchCannotFenceOriginalGrant(t *testing.T) {
	for _, test := range []struct {
		name     string
		instance worker.ApprovalInstance
	}{
		{
			name: "approval code mismatch",
			instance: worker.ApprovalInstance{
				ApprovalCode: "approval-other", InstanceCode: "instance-original",
				Status: "APPROVED", OpenID: "ou-requester", StartTime: "1787270300000",
				FormJSON: `[]`, Reverted: true,
			},
		},
		{
			name: "instance code mismatch",
			instance: worker.ApprovalInstance{
				ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-other",
				Status: "APPROVED", OpenID: "ou-requester", StartTime: "1787270300000",
				FormJSON: `[]`, Reverted: true,
			},
		},
		{
			name: "not authoritatively reverted",
			instance: worker.ApprovalInstance{
				ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-original",
				Status: "APPROVED", OpenID: "ou-requester", StartTime: "1787270300000",
				FormJSON: `[]`, Reverted: false,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			externalID := processVerifiedOriginalGrant(t, ctx, store, "original")
			recordReversalEvent(t, ctx, store, "mismatch", "instance-reversal", "instance-original")
			processor, err := worker.NewShadowProcessor(
				store,
				&approvalFetcher{instance: test.instance},
				&approvalResolver{resolution: verifiedWalletResolution()},
				"zh-CN",
			)
			if err != nil {
				t.Fatalf("new reversal processor: %v", err)
			}
			if processed, err := processor.RunOnce(ctx); err != nil || !processed {
				t.Fatalf("process mismatched reversal: processed=%t err=%v", processed, err)
			}
			reversal, err := store.GetApprovalReversal(ctx, "lark:v2:mismatch")
			if err != nil {
				t.Fatalf("get mismatched reversal: %v", err)
			}
			if reversal.Result != inbox.ApprovalReversalResultAuthorityMismatch ||
				reversal.OriginalExternalID != "" {
				t.Fatalf("mismatched reversal = %+v", reversal)
			}
			grant, err := store.GetEntitlementGrantJob(ctx, externalID)
			if err != nil {
				t.Fatalf("get original grant: %v", err)
			}
			if grant.Status != inbox.EntitlementGrantJobStatusHeldShadow {
				t.Fatalf("authority mismatch fenced grant as %q", grant.Status)
			}
		})
	}
}

func TestShadowProcessorRetriesReversalFetchBeforeResolvingOriginal(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordReversalEvent(t, ctx, store, "retry-reversal", "instance-reversal", "instance-original")
	fetcher := &approvalFetcher{
		failures: []error{&worker.ApprovalFetchError{
			Reason: worker.ApprovalFetchServerError, Retryable: true,
		}},
		instance: worker.ApprovalInstance{
			ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-original",
			Status: "APPROVED", OpenID: "ou-requester", StartTime: "1787270300000",
			FormJSON: `[]`, Reverted: true,
		},
	}
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
		t.Fatalf("schedule reversal retry: processed=%t err=%v", processed, err)
	}
	if _, err := store.GetApprovalReversal(ctx, "lark:v2:retry-reversal"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("retrying reversal was finalized early: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("finish reversal retry: processed=%t err=%v", processed, err)
	}
	reversal, err := store.GetApprovalReversal(ctx, "lark:v2:retry-reversal")
	if err != nil {
		t.Fatalf("get retried reversal: %v", err)
	}
	if reversal.Result != inbox.ApprovalReversalResultOriginalMissing || fetcher.calls != 2 {
		t.Fatalf("retried reversal=%+v fetch_calls=%d", reversal, fetcher.calls)
	}
}

func TestAuthoritativeReversalDenyFenceBlocksLaterApprovedGrant(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordReversalEvent(t, ctx, store, "reversal-first", "instance-reversal", "instance-original")
	reversalProcessor, err := worker.NewShadowProcessor(
		store,
		&approvalFetcher{instance: worker.ApprovalInstance{
			ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-original",
			Status: "APPROVED", OpenID: "ou-requester", StartTime: "1787270300000",
			FormJSON: `[]`, Reverted: true,
		}},
		&approvalResolver{resolution: verifiedWalletResolution()},
		"zh-CN",
	)
	if err != nil {
		t.Fatalf("new reversal processor: %v", err)
	}
	if processed, err := reversalProcessor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("process leading reversal: processed=%t err=%v", processed, err)
	}

	recordApprovedEvent(t, ctx, store, "original")
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant keyring: %v", err)
	}
	approvedProcessor, err := worker.NewShadowProcessorWithGrantSealer(
		store,
		&approvalFetcher{instance: worker.ApprovalInstance{
			ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-original",
			Status: "APPROVED", OpenID: "ou-requester", StartTime: "1787270300000",
			FormJSON: `[{"custom_id":"wallet_package","value":"Small"}]`, Reverted: false,
		}},
		&approvalResolver{resolution: verifiedWalletResolution()},
		"zh-CN",
		keyring,
	)
	if err != nil {
		t.Fatalf("new approved processor: %v", err)
	}
	if processed, err := approvedProcessor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("process approval after reversal: processed=%t err=%v", processed, err)
	}
	decision, err := store.GetDecision(ctx, "lark:v2:original")
	if err != nil {
		t.Fatalf("get denied approval decision: %v", err)
	}
	if decision.Outcome != inbox.DecisionOutcomeShadowAuthorityRejected ||
		decision.FailureReason != "approval_reverted" {
		t.Fatalf("approval after reversal decision = %+v", decision)
	}
	if _, err := store.GetEntitlementCommandShadow(ctx, "lark:v2:original"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("approval after reversal left a command shadow: %v", err)
	}
	if _, err := store.GetEntitlementGrantJob(ctx, "lark:wallet-topup:instance-original"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("approval after reversal left a grant job: %v", err)
	}
}

func TestShadowProcessorFinalizesReversalFetchFailuresAsManualPending(t *testing.T) {
	for _, test := range []struct {
		name       string
		failures   []error
		schedule   []time.Duration
		wantResult inbox.ApprovalReversalResult
		wantReason inbox.ApprovalReversalReason
	}{
		{
			name: "terminal",
			failures: []error{&worker.ApprovalFetchError{
				Reason: worker.ApprovalFetchClientError,
			}},
			schedule:   []time.Duration{time.Nanosecond},
			wantResult: inbox.ApprovalReversalResultFetchTerminalError,
			wantReason: inbox.ApprovalReversalReason(worker.ApprovalFetchClientError),
		},
		{
			name: "retry exhausted",
			failures: []error{
				&worker.ApprovalFetchError{Reason: worker.ApprovalFetchRateLimited, Retryable: true},
				&worker.ApprovalFetchError{Reason: worker.ApprovalFetchRateLimited, Retryable: true},
			},
			schedule:   []time.Duration{time.Nanosecond},
			wantResult: inbox.ApprovalReversalResultFetchRetryExhausted,
			wantReason: "retry_exhausted_rate_limited",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			recordReversalEvent(t, ctx, store, "failed-reversal", "instance-reversal", "instance-original")
			fetcher := &approvalFetcher{failures: test.failures}
			processor, err := worker.NewShadowProcessor(
				store, fetcher, &approvalResolver{resolution: verifiedWalletResolution()}, "zh-CN",
				worker.WithRetryPolicy(worker.RetryPolicy{
					Schedule: test.schedule, MaxDelay: time.Hour,
				}),
			)
			if err != nil {
				t.Fatalf("new processor: %v", err)
			}
			for range test.failures {
				if processed, err := processor.RunOnce(ctx); err != nil || !processed {
					t.Fatalf("process failed reversal: processed=%t err=%v", processed, err)
				}
			}
			reversal, err := store.GetApprovalReversal(ctx, "lark:v2:failed-reversal")
			if err != nil {
				t.Fatalf("get failed reversal: %v", err)
			}
			if reversal.Result != test.wantResult || reversal.Reason != test.wantReason {
				t.Fatalf("failed reversal = %+v", reversal)
			}
			decision, err := store.GetDecision(ctx, "lark:v2:failed-reversal")
			if err != nil {
				t.Fatalf("get failed reversal decision: %v", err)
			}
			if decision.Outcome != inbox.DecisionOutcomeReversalPending {
				t.Fatalf("failed reversal outcome = %q", decision.Outcome)
			}
		})
	}
}

func TestShadowProcessorRejectsTypedNilDependencies(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant keyring: %v", err)
	}
	var nilFetcher *approvalFetcher
	var nilResolver *approvalResolver
	var nilKeyring *newapi.GrantKeyring
	for name, dependencies := range map[string]struct {
		fetcher  worker.ApprovalFetcher
		resolver worker.ApprovalResolver
		sealer   worker.GrantRequestSealer
	}{
		"approval fetcher":  {fetcher: nilFetcher, resolver: &approvalResolver{}, sealer: keyring},
		"approval resolver": {fetcher: &approvalFetcher{}, resolver: nilResolver, sealer: keyring},
		"grant sealer":      {fetcher: &approvalFetcher{}, resolver: &approvalResolver{}, sealer: nilKeyring},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := worker.NewShadowProcessorWithGrantSealer(
				store,
				dependencies.fetcher,
				dependencies.resolver,
				"zh-CN",
				dependencies.sealer,
			); err == nil {
				t.Fatalf("accepted typed-nil %s", name)
			}
		})
	}
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
		{status: "REVERTED", fetchCalls: 1, outcome: "reversal_pending"},
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
				Reverted: test.status == "REVERTED",
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

func TestShadowProcessorWithGrantKeyringPersistsHeldCanonicalRequest(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordApprovedEvent(t, ctx, store, "evt-sealed-grant")
	keyring, err := newapi.NewGrantKeyring(
		bytes.Repeat([]byte{0x24}, 32),
		bytes.Repeat([]byte{0x42}, 32),
	)
	if err != nil {
		t.Fatalf("new grant keyring: %v", err)
	}
	processor, err := worker.NewShadowProcessorWithGrantSealer(
		store,
		&approvalFetcher{},
		&approvalResolver{resolution: verifiedWalletResolution()},
		"zh-CN",
		keyring,
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
	if stored.KeyID != keyring.PrimaryKeyID() {
		t.Fatalf("held request key ID = %q, want primary %q", stored.KeyID, keyring.PrimaryKeyID())
	}
	opened, err := keyring.Open(newapi.SealedGrantRequest{
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

func recordReversalEvent(
	t *testing.T,
	ctx context.Context,
	store *inbox.Store,
	eventID string,
	instanceCode string,
	revertedInstanceCode string,
) {
	t.Helper()
	_, err := store.Record(ctx, inbox.Event{
		Key: "lark:v2:" + eventID, SchemaVersion: "2.0", EventID: eventID,
		EventType: "approval.instance.status_changed_v4", AppID: "cli_test",
		TenantKey: "tenant-test", ApprovalCode: "approval-wallet-v1",
		InstanceCode: instanceCode, RevertedInstanceCode: revertedInstanceCode,
		Status: "REVERTED", PayloadJSON: `{"status":"REVERTED"}`,
	})
	if err != nil {
		t.Fatalf("record reversal event: %v", err)
	}
}

func processVerifiedOriginalGrant(
	t *testing.T,
	ctx context.Context,
	store *inbox.Store,
	eventID string,
) string {
	t.Helper()
	recordApprovedEvent(t, ctx, store, eventID)
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant keyring: %v", err)
	}
	processor, err := worker.NewShadowProcessorWithGrantSealer(
		store,
		&approvalFetcher{},
		&approvalResolver{resolution: verifiedWalletResolution()},
		"zh-CN",
		keyring,
	)
	if err != nil {
		t.Fatalf("new original grant processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("process original grant: processed=%t err=%v", processed, err)
	}
	return "lark:wallet-topup:instance-" + eventID
}
