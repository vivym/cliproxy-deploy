package inbox_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
)

func TestOperationalSnapshotReportsDurableInboxJobAndFailureState(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	retryEvent := operationalEvent("evt-retry", "APPROVED")
	if duplicate, err := store.Record(ctx, retryEvent); err != nil || duplicate {
		t.Fatalf("record retry event: duplicate=%t err=%v", duplicate, err)
	}
	if duplicate, err := store.Record(ctx, retryEvent); err != nil || !duplicate {
		t.Fatalf("record retry duplicate: duplicate=%t err=%v", duplicate, err)
	}
	retryJob, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim retry job: found=%t err=%v", found, err)
	}
	if err := store.Retry(ctx, retryJob, "server_error", time.Hour); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}

	policyEvent := operationalEvent("evt-policy", "APPROVED")
	if _, err := store.Record(ctx, policyEvent); err != nil {
		t.Fatalf("record policy event: %v", err)
	}
	policyJob, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim policy job: found=%t err=%v", found, err)
	}
	if err := store.CompleteDecision(ctx, policyJob, inbox.Decision{
		EventKey: policyEvent.Key, ApprovalCode: policyEvent.ApprovalCode,
		InstanceCode: policyEvent.InstanceCode, EventStatus: policyEvent.Status,
		AuthorityStatus: "APPROVED", Outcome: inbox.DecisionOutcomeDeadLetterPolicyValidation,
	}); err != nil {
		t.Fatalf("complete policy failure: %v", err)
	}

	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read operational snapshot: %v", err)
	}
	if snapshot.WebhookReceived["approval.instance.status_changed_v4"] != 3 ||
		snapshot.WebhookDuplicates["approval.instance.status_changed_v4"] != 1 {
		t.Fatalf("unexpected webhook counters: received=%v duplicates=%v",
			snapshot.WebhookReceived, snapshot.WebhookDuplicates)
	}
	if snapshot.InboxStates[inbox.ProcessingStatePending] != 1 ||
		snapshot.InboxStates[inbox.ProcessingStateDeadLetter] != 1 ||
		snapshot.JobStates["retry_wait"] != 1 || snapshot.JobStates["dead_letter"] != 1 {
		t.Fatalf("unexpected state gauges: inbox=%v jobs=%v", snapshot.InboxStates, snapshot.JobStates)
	}
	if snapshot.ApprovalFetches["retryable_error"] != 1 || snapshot.ApprovalFetches["success"] != 1 {
		t.Fatalf("unexpected fetch counters: %v", snapshot.ApprovalFetches)
	}
	if snapshot.PolicyValidationFailures != 1 ||
		snapshot.DeadLetters[string(inbox.DecisionOutcomeDeadLetterPolicyValidation)] != 1 {
		t.Fatalf("unexpected failure counters: policy=%d dead_letters=%v",
			snapshot.PolicyValidationFailures, snapshot.DeadLetters)
	}
	if snapshot.OldestActiveJobAge < 0 || snapshot.OldestReadyJobAge != 0 {
		t.Fatalf("unexpected queue ages: active=%s ready=%s",
			snapshot.OldestActiveJobAge, snapshot.OldestReadyJobAge)
	}
}

func TestOperationalSnapshotMeasuresRetryReadyAgeFromEligibility(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	event := operationalEvent("evt-ready-age", "APPROVED")
	if _, err := store.Record(ctx, event); err != nil {
		t.Fatalf("record event: %v", err)
	}
	job, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim job: found=%t err=%v", found, err)
	}
	if err := store.Retry(ctx, job, "server_error", 100*time.Millisecond); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}
	time.Sleep(120 * time.Millisecond)

	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read operational snapshot: %v", err)
	}
	if snapshot.OldestActiveJobAge < 100*time.Millisecond {
		t.Fatalf("active job age = %s, want at least original backoff", snapshot.OldestActiveJobAge)
	}
	if snapshot.OldestReadyJobAge <= 0 || snapshot.OldestReadyJobAge >= 75*time.Millisecond {
		t.Fatalf("ready job age = %s, want age since retry eligibility", snapshot.OldestReadyJobAge)
	}
}

func TestOperationalSnapshotReportsNewAPIGrantShadows(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	event := operationalEvent("evt-command", "APPROVED")
	if _, err := store.Record(ctx, event); err != nil {
		t.Fatalf("record event: %v", err)
	}
	job, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim job: found=%t err=%v", found, err)
	}
	if err := store.CompleteDecision(ctx, job, inbox.Decision{
		EventKey: event.Key, ApprovalCode: event.ApprovalCode,
		InstanceCode: event.InstanceCode, EventStatus: event.Status,
		AuthorityStatus: "APPROVED", Outcome: inbox.DecisionOutcomeShadowAuthorityVerified,
		EntitlementCommand: &inbox.EntitlementCommandShadow{
			ExternalID:    "lark:wallet-topup:instance-evt-command",
			RequestSHA256: strings.Repeat("a", 64), SubjectSHA256: strings.Repeat("b", 64),
			Source: "lark_approval", PolicyVersion: "employee-v1", CatalogSHA256: "catalog-sha",
			GrantType: "wallet_quota", BusinessCode: "topup_5", QuotaDelta: 2_500_000,
		},
		EntitlementGrantJob: &inbox.EntitlementGrantJobDraft{
			ExternalID:    "lark:wallet-topup:instance-evt-command",
			RequestSHA256: strings.Repeat("a", 64), SubjectSHA256: strings.Repeat("b", 64),
			KeyID: strings.Repeat("c", 64), Nonce: make([]byte, 12), Ciphertext: make([]byte, 17),
		},
	}); err != nil {
		t.Fatalf("complete command shadow: %v", err)
	}
	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read operational snapshot: %v", err)
	}
	if snapshot.NewAPIGrants["shadow_planned"] != 1 {
		t.Fatalf("unexpected New API grant counters: %v", snapshot.NewAPIGrants)
	}
	if snapshot.EntitlementGrantJobStates["held_shadow"] != 1 ||
		snapshot.OldestActiveJobAge != 0 || snapshot.OldestReadyJobAge != 0 {
		t.Fatalf("unexpected held grant job state: states=%v active=%s ready=%s",
			snapshot.EntitlementGrantJobStates,
			snapshot.OldestActiveJobAge,
			snapshot.OldestReadyJobAge,
		)
	}
}

func TestOperationalSnapshotStartsReleasedGrantAgeAtActivation(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordHeldGrantJob(t, ctx, store, "evt-released-age")
	time.Sleep(300 * time.Millisecond)
	if released, err := store.ReleaseHeldEntitlementGrantJobs(ctx); err != nil || released != 1 {
		t.Fatalf("release held grant: released=%d err=%v", released, err)
	}

	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read released grant snapshot: %v", err)
	}
	if snapshot.OldestActiveJobAge <= 0 || snapshot.OldestActiveJobAge >= 200*time.Millisecond ||
		snapshot.OldestReadyJobAge <= 0 || snapshot.OldestReadyJobAge >= 200*time.Millisecond {
		t.Fatalf(
			"released grant ages include held time: active=%s ready=%s",
			snapshot.OldestActiveJobAge,
			snapshot.OldestReadyJobAge,
		)
	}
}

func TestOpenRecoversProcessingJobAndInboxStateTogether(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	event := operationalEvent("evt-restart", "APPROVED")
	if _, err := store.Record(ctx, event); err != nil {
		t.Fatalf("record event: %v", err)
	}
	firstClaim, found, err := store.ClaimNext(ctx)
	if err != nil || !found || firstClaim.Attempts != 1 {
		t.Fatalf("first claim: found=%t attempts=%d err=%v", found, firstClaim.Attempts, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	store, err = inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recoveredEvent, err := store.Get(ctx, event.Key)
	if err != nil {
		t.Fatalf("get recovered event: %v", err)
	}
	if recoveredEvent.ProcessingState != inbox.ProcessingStatePending {
		t.Fatalf("recovered inbox state = %q, want pending", recoveredEvent.ProcessingState)
	}
	recoveredJob, found, err := store.ClaimNext(ctx)
	if err != nil || !found || recoveredJob.Attempts != 2 {
		t.Fatalf("recovered claim: found=%t attempts=%d err=%v", found, recoveredJob.Attempts, err)
	}
}

func operationalEvent(eventID, status string) inbox.Event {
	return inbox.Event{
		Key: "lark:v2:" + eventID, SchemaVersion: "2.0", EventID: eventID,
		EventType: "approval.instance.status_changed_v4", AppID: "cli_test", TenantKey: "tenant-test",
		ApprovalCode: "approval-wallet-v1", InstanceCode: "instance-" + eventID, Status: status,
		PayloadJSON: `{"status":"` + status + `"}`,
	}
}
