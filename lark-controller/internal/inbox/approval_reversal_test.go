package inbox_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
)

func TestCompleteDecisionRecordsReversalAndFencesHeldGrantAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	externalID := recordHeldGrantJob(t, ctx, store, "original")

	reversalEvent := operationalEvent("reversal", "REVERTED")
	reversalEvent.RevertedInstanceCode = "instance-original"
	if _, err := store.Record(ctx, reversalEvent); err != nil {
		t.Fatalf("record reversal event: %v", err)
	}
	job, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim reversal event: found=%t err=%v", found, err)
	}
	if err := store.CompleteDecision(ctx, job, inbox.Decision{
		EventKey: job.Event.Key, ApprovalCode: job.Event.ApprovalCode,
		InstanceCode: "instance-original", EventStatus: job.Event.Status,
		AuthorityStatus: "APPROVED", Reverted: true,
		Outcome: inbox.DecisionOutcomeReversalPending,
		ApprovalReversal: &inbox.ApprovalReversalDraft{
			TargetInstanceCode:    "instance-original",
			AuthorityApprovalCode: "approval-wallet-v1",
			AuthorityInstanceCode: "instance-original",
			AuthorityStatus:       "APPROVED",
			AuthorityReverted:     true,
		},
	}); err != nil {
		t.Fatalf("complete reversal decision: %v", err)
	}

	reversal, err := store.GetApprovalReversal(ctx, reversalEvent.Key)
	if err != nil {
		t.Fatalf("get approval reversal: %v", err)
	}
	if reversal.Result != inbox.ApprovalReversalResultGrantFenced ||
		reversal.Reason != inbox.ApprovalReversalReasonManualReviewRequired ||
		reversal.TargetInstanceCode != "instance-original" ||
		reversal.OriginalExternalID != externalID ||
		reversal.OriginalSubjectSHA256 != testSHA256("tenant-test:ou-requester") ||
		reversal.OriginalGrantStatus != inbox.EntitlementGrantJobStatusHeldShadow ||
		reversal.OriginalGrantType != "wallet_quota" || reversal.OriginalQuotaDelta != 2_500_000 ||
		reversal.OriginalMonthlyQuota != 0 || reversal.OriginalPolicyVersion != "employee-v1" ||
		reversal.OriginalBusinessCode != "topup_5" ||
		!reversal.AuthorityReverted {
		t.Fatalf("unexpected approval reversal: %+v", reversal)
	}
	grant, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get fenced grant job: %v", err)
	}
	if grant.Status != inbox.EntitlementGrantJobStatusReversalPending || grant.Attempts != 0 {
		t.Fatalf("fenced grant job = %+v, want reversal_pending with attempts preserved", grant)
	}
}

func TestResolveApprovalReversalClosesPendingStateIdempotently(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	externalID := recordHeldGrantJob(t, ctx, store, "resolution-original")

	reversalEvent := operationalEvent("resolution-reversal", "REVERTED")
	reversalEvent.RevertedInstanceCode = "instance-resolution-original"
	if _, err := store.Record(ctx, reversalEvent); err != nil {
		t.Fatalf("record reversal event: %v", err)
	}
	job, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim reversal event: found=%t err=%v", found, err)
	}
	if err := store.CompleteDecision(ctx, job, inbox.Decision{
		EventKey: job.Event.Key, ApprovalCode: job.Event.ApprovalCode,
		InstanceCode: "instance-resolution-original", EventStatus: job.Event.Status,
		AuthorityStatus: "APPROVED", Reverted: true,
		Outcome: inbox.DecisionOutcomeReversalPending,
		ApprovalReversal: &inbox.ApprovalReversalDraft{
			TargetInstanceCode:    "instance-resolution-original",
			AuthorityApprovalCode: "approval-wallet-v1",
			AuthorityInstanceCode: "instance-resolution-original",
			AuthorityStatus:       "APPROVED",
			AuthorityReverted:     true,
		},
	}); err != nil {
		t.Fatalf("complete reversal decision: %v", err)
	}

	pending, err := store.GetPendingApprovalReversal(ctx, reversalEvent.Key, externalID)
	if err != nil || pending.OriginalExternalID != externalID {
		t.Fatalf("get pending reversal: reversal=%+v err=%v", pending, err)
	}
	pendingList, err := store.ListPendingApprovalReversals(ctx, 100)
	if err != nil || len(pendingList) != 1 || pendingList[0].EventKey != reversalEvent.Key {
		t.Fatalf("pending reversal list = %+v err=%v", pendingList, err)
	}
	resolution := inbox.ApprovalReversalResolution{
		EventKey:                reversalEvent.Key,
		OriginalExternalID:      externalID,
		OriginalSubjectSHA256:   testSHA256("tenant-test:ou-requester"),
		CorrectionExternalID:    "lark:correction:CHG-2026-0055:wallet",
		CorrectionRequestSHA256: strings.Repeat("a", 64),
		Operator:                "ops@example.com",
		Reason:                  "reverted approval after partial usage",
		ChangeTicket:            "CHG-2026-0055",
		ResponseStatus:          "applied",
		Result: inbox.ApprovalCorrectionResult{
			CorrectionType: "wallet_quota", QuotaDelta: -1_000_000,
			WalletQuota: int64Pointer(2_000_000),
		},
	}
	typeMismatch := resolution
	typeMismatch.Result = inbox.ApprovalCorrectionResult{
		CorrectionType: "subscription_level", LevelCode: "basic",
		SubscriptionID: 7, AssignmentVersion: 2, Transition: "updated",
	}
	if _, err := store.ResolveApprovalReversal(ctx, typeMismatch); !errors.Is(err, inbox.ErrApprovalReversalResolutionMismatch) {
		t.Fatalf("mismatched correction type error = %v", err)
	}
	subjectMismatch := resolution
	subjectMismatch.OriginalSubjectSHA256 = strings.Repeat("d", 64)
	if _, err := store.ResolveApprovalReversal(ctx, subjectMismatch); !errors.Is(err, inbox.ErrApprovalReversalResolutionMismatch) {
		t.Fatalf("mismatched correction subject error = %v", err)
	}
	stored, err := store.ResolveApprovalReversal(ctx, resolution)
	if err != nil {
		t.Fatalf("resolve approval reversal: %v", err)
	}
	if stored.Replayed || stored.CorrectionExternalID != resolution.CorrectionExternalID ||
		stored.ResolvedAt.IsZero() {
		t.Fatalf("stored resolution = %+v", stored)
	}
	intent, found, err := store.GetApprovalReversalCorrectionIntentForOriginal(ctx, externalID)
	if err != nil || !found || intent.Status != inbox.ApprovalReversalCorrectionIntentResolved ||
		!intent.EndedAt.Equal(stored.ResolvedAt) {
		t.Fatalf("resolved correction intent: intent=%+v found=%t err=%v", intent, found, err)
	}

	replayed, err := store.ResolveApprovalReversal(ctx, resolution)
	if err != nil || !replayed.Replayed || !replayed.ResolvedAt.Equal(stored.ResolvedAt) {
		t.Fatalf("replay resolution: resolution=%+v err=%v", replayed, err)
	}

	conflict := resolution
	conflict.Reason = "different reason"
	if _, err := store.ResolveApprovalReversal(ctx, conflict); !errors.Is(err, inbox.ErrApprovalReversalResolutionMismatch) {
		t.Fatalf("conflicting resolution error = %v", err)
	}

	event, err := store.Get(ctx, reversalEvent.Key)
	if err != nil || event.ProcessingState != inbox.ProcessingStateReversalResolved {
		t.Fatalf("resolved event = %+v err=%v", event, err)
	}
	grant, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil || grant.Status != inbox.EntitlementGrantJobStatusReversalResolved {
		t.Fatalf("resolved grant = %+v err=%v", grant, err)
	}
	if _, err := store.GetPendingApprovalReversal(ctx, reversalEvent.Key, externalID); !errors.Is(err, inbox.ErrApprovalReversalNotPending) {
		t.Fatalf("resolved reversal pending lookup error = %v", err)
	}
	pendingList, err = store.ListPendingApprovalReversals(ctx, 100)
	if err != nil || len(pendingList) != 0 {
		t.Fatalf("resolved pending reversal list = %+v err=%v", pendingList, err)
	}

	reversal, err := store.GetApprovalReversal(ctx, reversalEvent.Key)
	if err != nil || reversal.Resolution == nil ||
		reversal.Resolution.CorrectionRequestSHA256 != resolution.CorrectionRequestSHA256 ||
		reversal.Resolution.OriginalSubjectSHA256 != resolution.OriginalSubjectSHA256 {
		t.Fatalf("resolved reversal = %+v err=%v", reversal, err)
	}
	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil || snapshot.JobStates["reversal_resolved"] != 1 {
		t.Fatalf("resolved job snapshot = %+v err=%v", snapshot.JobStates, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close resolved store: %v", err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open resolved database: %v", err)
	}
	defer func() { _ = database.Close() }()
	var jobStatus string
	if err := database.QueryRow(
		"SELECT status FROM jobs WHERE event_key = ?", reversalEvent.Key,
	).Scan(&jobStatus); err != nil {
		t.Fatalf("read resolved inbox job: %v", err)
	}
	if jobStatus != "reversal_resolved" {
		t.Fatalf("resolved inbox job status = %q, want reversal_resolved", jobStatus)
	}
}

func TestOpenMigratesLegacyApprovalReversalResolutionSchema(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	externalID := recordHeldGrantJob(t, ctx, store, "legacy-resolution-original")
	reversal := completeVerifiedReversal(
		t, ctx, store, "legacy-resolution-reversal", "instance-legacy-resolution-original",
	)
	if err := store.Close(); err != nil {
		t.Fatalf("close store before legacy schema rewrite: %v", err)
	}

	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open database for legacy schema rewrite: %v", err)
	}
	_, err = database.Exec(`
ALTER TABLE approval_reversals RENAME TO approval_reversals_with_resolution;
CREATE TABLE approval_reversals (
    event_key TEXT PRIMARY KEY REFERENCES lark_event_inbox(event_key) ON DELETE CASCADE,
    approval_code TEXT NOT NULL,
    target_instance_code TEXT NOT NULL,
    authority_approval_code TEXT NOT NULL DEFAULT '',
    authority_instance_code TEXT NOT NULL DEFAULT '',
    authority_status TEXT NOT NULL DEFAULT '',
    authority_reverted INTEGER NOT NULL DEFAULT 0,
    original_external_id TEXT NOT NULL DEFAULT '',
    original_grant_status TEXT NOT NULL DEFAULT '',
    original_grant_type TEXT NOT NULL DEFAULT '',
    original_quota_delta INTEGER NOT NULL DEFAULT 0,
    original_monthly_quota INTEGER NOT NULL DEFAULT 0,
    original_policy_version TEXT NOT NULL DEFAULT '',
    original_business_code TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL,
    reason TEXT NOT NULL,
    created_at TEXT NOT NULL
);
INSERT INTO approval_reversals (
    event_key, approval_code, target_instance_code, authority_approval_code,
    authority_instance_code, authority_status, authority_reverted,
    original_external_id, original_grant_status, original_grant_type,
    original_quota_delta, original_monthly_quota, original_policy_version,
    original_business_code, result, reason, created_at
)
SELECT event_key, approval_code, target_instance_code, authority_approval_code,
       authority_instance_code, authority_status, authority_reverted,
       original_external_id, original_grant_status, original_grant_type,
       original_quota_delta, original_monthly_quota, original_policy_version,
       original_business_code, result, reason, created_at
FROM approval_reversals_with_resolution;
DROP TABLE approval_reversals_with_resolution;`)
	if err != nil {
		_ = database.Close()
		t.Fatalf("rewrite legacy approval reversal schema: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err = inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open and migrate legacy approval reversal schema: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	resolution := inbox.ApprovalReversalResolution{
		EventKey:                reversal.EventKey,
		OriginalExternalID:      externalID,
		OriginalSubjectSHA256:   testSHA256("tenant-test:ou-requester"),
		CorrectionExternalID:    "lark:correction:CHG-LEGACY:wallet",
		CorrectionRequestSHA256: strings.Repeat("a", 64),
		Operator:                "ops@example.com",
		Reason:                  "migrated reversal correction",
		ChangeTicket:            "CHG-LEGACY",
		ResponseStatus:          "noop",
		Result: inbox.ApprovalCorrectionResult{
			CorrectionType: "wallet_quota", WalletQuota: int64Pointer(2_500_000),
		},
	}
	stored, err := store.ResolveApprovalReversal(ctx, resolution)
	if err != nil || stored.CorrectionExternalID != resolution.CorrectionExternalID {
		t.Fatalf("resolve migrated approval reversal: resolution=%+v err=%v", stored, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}
	database, err = sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open migrated database: %v", err)
	}
	defer func() { _ = database.Close() }()
	var receiptTableCount int
	if err := database.QueryRow(`
SELECT COUNT(*) FROM sqlite_master
WHERE type = 'table' AND name = 'approval_reversal_resolution_receipts'`).Scan(&receiptTableCount); err != nil {
		t.Fatalf("inspect migrated approval reversal receipt table: %v", err)
	}
	if receiptTableCount != 1 {
		t.Fatalf("migrated approval reversal receipt table count = %d, want 1", receiptTableCount)
	}
	var receiptCount int
	if err := database.QueryRow(`
SELECT COUNT(*) FROM approval_reversal_resolution_receipts
WHERE correction_external_id = ? AND original_external_id = ?`,
		resolution.CorrectionExternalID, resolution.OriginalExternalID,
	).Scan(&receiptCount); err != nil || receiptCount != 1 {
		t.Fatalf("migrated approval reversal receipt count = %d err=%v, want 1", receiptCount, err)
	}
}

func TestOpenBackfillsCorrectionIntentFromExistingReceipt(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	externalID := recordHeldGrantJob(t, ctx, store, "intent-backfill-original")
	reversal := completeVerifiedReversal(
		t, ctx, store, "intent-backfill-reversal", "instance-intent-backfill-original",
	)
	resolution := inbox.ApprovalReversalResolution{
		EventKey: reversal.EventKey, OriginalExternalID: externalID,
		OriginalSubjectSHA256:   testSHA256("tenant-test:ou-requester"),
		CorrectionExternalID:    "lark:correction:CHG-INTENT-BACKFILL:wallet",
		CorrectionRequestSHA256: strings.Repeat("a", 64),
		Operator:                "ops@example.com",
		Reason:                  "backfill intent from legacy receipt",
		ChangeTicket:            "CHG-INTENT-BACKFILL",
		ResponseStatus:          "noop",
		Result: inbox.ApprovalCorrectionResult{
			CorrectionType: "wallet_quota", WalletQuota: int64Pointer(2_500_000),
		},
	}
	resolved, err := store.ResolveApprovalReversal(ctx, resolution)
	if err != nil {
		t.Fatalf("resolve reversal before intent backfill: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close resolved store: %v", err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open database for intent table removal: %v", err)
	}
	if _, err := database.Exec("DROP TABLE approval_reversal_correction_intents"); err != nil {
		_ = database.Close()
		t.Fatalf("remove intent table from legacy fixture: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close legacy intent database: %v", err)
	}

	store, err = inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open and backfill correction intent: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	intent, found, err := store.GetApprovalReversalCorrectionIntentForOriginal(ctx, externalID)
	if err != nil || !found || intent.CorrectionExternalID != resolution.CorrectionExternalID ||
		intent.CorrectionRequestSHA256 != resolution.CorrectionRequestSHA256 ||
		!intent.ClaimedAt.Equal(resolved.ResolvedAt) {
		t.Fatalf("backfilled correction intent: intent=%+v found=%t err=%v", intent, found, err)
	}
}

func TestCorrectionIntentSurvivesRestartAndFencesDifferentRequest(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	externalID := recordHeldGrantJob(t, ctx, store, "intent-original")
	reversal := completeVerifiedReversal(t, ctx, store, "intent-reversal", "instance-intent-original")
	intent := inbox.ApprovalReversalCorrectionIntent{
		EventKey: reversal.EventKey, OriginalExternalID: externalID,
		OriginalSubjectSHA256:   testSHA256("tenant-test:ou-requester"),
		CorrectionExternalID:    "lark:correction:CHG-INTENT-A:wallet",
		CorrectionRequestSHA256: strings.Repeat("a", 64),
		CorrectionType:          "wallet_quota",
		Operator:                "ops@example.com",
		Reason:                  "claim correction before remote mutation",
		ChangeTicket:            "CHG-INTENT-A",
	}
	claimed, err := store.ClaimApprovalReversalCorrectionIntent(ctx, intent)
	if err != nil || claimed.Replayed || claimed.ClaimedAt.IsZero() {
		t.Fatalf("claim correction intent: intent=%+v err=%v", claimed, err)
	}
	replayed, err := store.ClaimApprovalReversalCorrectionIntent(ctx, intent)
	if err != nil || !replayed.Replayed || !replayed.ClaimedAt.Equal(claimed.ClaimedAt) {
		t.Fatalf("replay correction intent: intent=%+v err=%v", replayed, err)
	}
	conflict := intent
	conflict.CorrectionExternalID = "lark:correction:CHG-INTENT-B:wallet"
	conflict.CorrectionRequestSHA256 = strings.Repeat("b", 64)
	conflict.ChangeTicket = "CHG-INTENT-B"
	if _, err := store.ClaimApprovalReversalCorrectionIntent(ctx, conflict); !errors.Is(err, inbox.ErrApprovalReversalResolutionMismatch) {
		t.Fatalf("conflicting correction intent error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store after intent claim: %v", err)
	}

	store, err = inbox.OpenCorrection(databasePath)
	if err != nil {
		t.Fatalf("reopen correction store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	persisted, found, err := store.GetApprovalReversalCorrectionIntentForOriginal(ctx, externalID)
	if err != nil || !found || !sameCorrectionIntentForTest(persisted, claimed) {
		t.Fatalf("persisted correction intent: intent=%+v found=%t err=%v", persisted, found, err)
	}
}

func TestConcurrentCorrectionIntentsRetainWinnerAfterResponseLoss(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	setupStore, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open setup store: %v", err)
	}
	externalID := recordHeldGrantJob(t, ctx, setupStore, "concurrent-intent-original")
	reversal := completeVerifiedReversal(
		t, ctx, setupStore, "concurrent-intent-reversal", "instance-concurrent-intent-original",
	)
	if err := setupStore.Close(); err != nil {
		t.Fatalf("close setup store: %v", err)
	}

	stores := make([]*inbox.Store, 2)
	for index := range stores {
		stores[index], err = inbox.OpenCorrection(databasePath)
		if err != nil {
			t.Fatalf("open correction store %d: %v", index, err)
		}
		defer func(store *inbox.Store) { _ = store.Close() }(stores[index])
	}
	intents := []inbox.ApprovalReversalCorrectionIntent{
		{
			EventKey: reversal.EventKey, OriginalExternalID: externalID,
			OriginalSubjectSHA256:   testSHA256("tenant-test:ou-requester"),
			CorrectionExternalID:    "lark:correction:CHG-CONCURRENT-INTENT-A:wallet",
			CorrectionRequestSHA256: strings.Repeat("a", 64), CorrectionType: "wallet_quota",
			Operator: "ops-a@example.com",
			Reason:   "first concurrent correction", ChangeTicket: "CHG-CONCURRENT-INTENT-A",
		},
		{
			EventKey: reversal.EventKey, OriginalExternalID: externalID,
			OriginalSubjectSHA256:   testSHA256("tenant-test:ou-requester"),
			CorrectionExternalID:    "lark:correction:CHG-CONCURRENT-INTENT-B:wallet",
			CorrectionRequestSHA256: strings.Repeat("b", 64), CorrectionType: "wallet_quota",
			Operator: "ops-b@example.com",
			Reason:   "second concurrent correction", ChangeTicket: "CHG-CONCURRENT-INTENT-B",
		},
	}
	results := make([]inbox.ApprovalReversalCorrectionIntent, len(intents))
	errorsByIntent := make([]error, len(intents))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range intents {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index], errorsByIntent[index] = stores[index].ClaimApprovalReversalCorrectionIntent(
				ctx, intents[index],
			)
		}(index)
	}
	close(start)
	wait.Wait()
	winner := -1
	for index, claimErr := range errorsByIntent {
		switch {
		case claimErr == nil:
			if winner != -1 {
				t.Fatalf("multiple correction intent winners: %d and %d", winner, index)
			}
			winner = index
		case errors.Is(claimErr, inbox.ErrApprovalReversalResolutionMismatch):
		default:
			t.Fatalf("concurrent correction intent %d error = %v", index, claimErr)
		}
	}
	if winner == -1 {
		t.Fatalf("no correction intent winner: errors=%v", errorsByIntent)
	}
	persisted, found, err := stores[winner].GetApprovalReversalCorrectionIntentForOriginal(ctx, externalID)
	if err != nil || !found || !sameCorrectionIntentForTest(persisted, results[winner]) {
		t.Fatalf("winner intent after response loss: intent=%+v found=%t err=%v", persisted, found, err)
	}
}

func TestAbandonedCorrectionIntentAllowsReplacement(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	externalID := recordHeldGrantJob(t, ctx, store, "abandoned-intent-original")
	reversal := completeVerifiedReversal(
		t, ctx, store, "abandoned-intent-reversal", "instance-abandoned-intent-original",
	)
	first := inbox.ApprovalReversalCorrectionIntent{
		EventKey: reversal.EventKey, OriginalExternalID: externalID,
		OriginalSubjectSHA256:   testSHA256("tenant-test:ou-requester"),
		CorrectionExternalID:    "lark:correction:CHG-ABANDONED-A:wallet",
		CorrectionRequestSHA256: strings.Repeat("a", 64), CorrectionType: "wallet_quota",
		Operator: "ops@example.com", Reason: "stale expected state", ChangeTicket: "CHG-ABANDONED-A",
	}
	claimed, err := store.ClaimApprovalReversalCorrectionIntent(ctx, first)
	if err != nil {
		t.Fatalf("claim first intent: %v", err)
	}
	if err := store.AbandonApprovalReversalCorrectionIntent(
		ctx, claimed, "correction_state_mismatch",
	); err != nil {
		t.Fatalf("abandon first intent: %v", err)
	}
	if intent, found, err := store.GetApprovalReversalCorrectionIntentForOriginal(ctx, externalID); err != nil || found {
		t.Fatalf("abandoned intent still fences original: intent=%+v found=%t err=%v", intent, found, err)
	}

	replacement := first
	replacement.CorrectionExternalID = "lark:correction:CHG-ABANDONED-B:wallet"
	replacement.CorrectionRequestSHA256 = strings.Repeat("b", 64)
	replacement.ChangeTicket = "CHG-ABANDONED-B"
	claimed, err = store.ClaimApprovalReversalCorrectionIntent(ctx, replacement)
	if err != nil || claimed.Status != inbox.ApprovalReversalCorrectionIntentActive || claimed.Replayed {
		t.Fatalf("claim replacement intent: intent=%+v err=%v", claimed, err)
	}
	if _, err := store.ClaimApprovalReversalCorrectionIntent(ctx, first); !errors.Is(err, inbox.ErrApprovalReversalResolutionMismatch) {
		t.Fatalf("reactivate abandoned intent around replacement error = %v", err)
	}
}

func TestRemoteConflictCorrectionIntentFencesReplacement(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	externalID := recordHeldGrantJob(t, ctx, store, "remote-conflict-intent-original")
	reversal := completeVerifiedReversal(
		t, ctx, store, "remote-conflict-intent-reversal", "instance-remote-conflict-intent-original",
	)
	intent := inbox.ApprovalReversalCorrectionIntent{
		EventKey: reversal.EventKey, OriginalExternalID: externalID,
		OriginalSubjectSHA256:   testSHA256("tenant-test:ou-requester"),
		CorrectionExternalID:    "lark:correction:CHG-REMOTE-CONFLICT-A:wallet",
		CorrectionRequestSHA256: strings.Repeat("a", 64), CorrectionType: "wallet_quota",
		Operator: "ops@example.com", Reason: "ambiguous remote receipt", ChangeTicket: "CHG-REMOTE-CONFLICT-A",
	}
	claimed, err := store.ClaimApprovalReversalCorrectionIntent(ctx, intent)
	if err != nil {
		t.Fatalf("claim intent: %v", err)
	}
	if err := store.BlockApprovalReversalCorrectionIntent(
		ctx, claimed, "correction_already_applied",
	); err != nil {
		t.Fatalf("block intent: %v", err)
	}
	blocked, found, err := store.GetApprovalReversalCorrectionIntentForOriginal(ctx, externalID)
	if err != nil || !found ||
		blocked.Status != inbox.ApprovalReversalCorrectionIntentRemoteConflict ||
		blocked.FailureCode != "correction_already_applied" || blocked.EndedAt.IsZero() {
		t.Fatalf("blocked intent: intent=%+v found=%t err=%v", blocked, found, err)
	}
	replacement := intent
	replacement.CorrectionExternalID = "lark:correction:CHG-REMOTE-CONFLICT-B:wallet"
	replacement.CorrectionRequestSHA256 = strings.Repeat("b", 64)
	replacement.ChangeTicket = "CHG-REMOTE-CONFLICT-B"
	if _, err := store.ClaimApprovalReversalCorrectionIntent(ctx, replacement); !errors.Is(err, inbox.ErrApprovalReversalResolutionMismatch) {
		t.Fatalf("replacement around remote conflict error = %v", err)
	}
}

func TestResolvedReplacementSurvivesStartupWithAbandonedHistory(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	externalID := recordHeldGrantJob(t, ctx, store, "resolved-replacement-original")
	reversal := completeVerifiedReversal(
		t, ctx, store, "resolved-replacement-reversal", "instance-resolved-replacement-original",
	)
	first := inbox.ApprovalReversalCorrectionIntent{
		EventKey: reversal.EventKey, OriginalExternalID: externalID,
		OriginalSubjectSHA256:   testSHA256("tenant-test:ou-requester"),
		CorrectionExternalID:    "lark:correction:CHG-RESOLVED-A:wallet",
		CorrectionRequestSHA256: strings.Repeat("a", 64), CorrectionType: "wallet_quota",
		Operator: "ops@example.com", Reason: "stale expected state", ChangeTicket: "CHG-RESOLVED-A",
	}
	claimed, err := store.ClaimApprovalReversalCorrectionIntent(ctx, first)
	if err != nil {
		t.Fatalf("claim first intent: %v", err)
	}
	if err := store.AbandonApprovalReversalCorrectionIntent(
		ctx, claimed, "correction_state_mismatch",
	); err != nil {
		t.Fatalf("abandon first intent: %v", err)
	}
	replacement := first
	replacement.CorrectionExternalID = "lark:correction:CHG-RESOLVED-B:wallet"
	replacement.CorrectionRequestSHA256 = strings.Repeat("b", 64)
	replacement.Reason = "approved replacement"
	replacement.ChangeTicket = "CHG-RESOLVED-B"
	if _, err := store.ClaimApprovalReversalCorrectionIntent(ctx, replacement); err != nil {
		t.Fatalf("claim replacement intent: %v", err)
	}
	resolution := inbox.ApprovalReversalResolution{
		EventKey: reversal.EventKey, OriginalExternalID: externalID,
		OriginalSubjectSHA256:   replacement.OriginalSubjectSHA256,
		CorrectionExternalID:    replacement.CorrectionExternalID,
		CorrectionRequestSHA256: replacement.CorrectionRequestSHA256,
		Operator:                replacement.Operator, Reason: replacement.Reason,
		ChangeTicket: replacement.ChangeTicket, ResponseStatus: "noop",
		Result: inbox.ApprovalCorrectionResult{
			CorrectionType: "wallet_quota", WalletQuota: int64Pointer(2_500_000),
		},
	}
	if _, err := store.ResolveApprovalReversal(ctx, resolution); err != nil {
		t.Fatalf("resolve replacement intent: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close resolved replacement store: %v", err)
	}

	store, err = inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("reopen store with abandoned correction history: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	resolved, found, err := store.GetApprovalReversalCorrectionIntentForOriginal(ctx, externalID)
	if err != nil || !found ||
		resolved.CorrectionExternalID != replacement.CorrectionExternalID ||
		resolved.Status != inbox.ApprovalReversalCorrectionIntentResolved {
		t.Fatalf("resolved replacement after restart: intent=%+v found=%t err=%v", resolved, found, err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open correction history audit: %v", err)
	}
	defer func() { _ = database.Close() }()
	var abandonedStatus string
	var abandonedFailure string
	if err := database.QueryRow(`
SELECT status, failure_code
FROM approval_reversal_correction_intents
WHERE correction_external_id = ?`, first.CorrectionExternalID).Scan(
		&abandonedStatus, &abandonedFailure,
	); err != nil {
		t.Fatalf("read abandoned correction history: %v", err)
	}
	if abandonedStatus != string(inbox.ApprovalReversalCorrectionIntentAbandoned) ||
		abandonedFailure != "correction_state_mismatch" {
		t.Fatalf(
			"abandoned correction history = status %q failure %q",
			abandonedStatus, abandonedFailure,
		)
	}
}

func sameCorrectionIntentForTest(
	first inbox.ApprovalReversalCorrectionIntent,
	second inbox.ApprovalReversalCorrectionIntent,
) bool {
	return first.OriginalExternalID == second.OriginalExternalID &&
		first.OriginalSubjectSHA256 == second.OriginalSubjectSHA256 &&
		first.CorrectionExternalID == second.CorrectionExternalID &&
		first.CorrectionRequestSHA256 == second.CorrectionRequestSHA256 &&
		first.CorrectionType == second.CorrectionType && first.Status == second.Status &&
		first.Operator == second.Operator && first.Reason == second.Reason &&
		first.ChangeTicket == second.ChangeTicket && first.ClaimedAt.Equal(second.ClaimedAt)
}

func TestResolveApprovalReversalRollsBackWhenInboxJobPostconditionIsMissing(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	externalID := recordHeldGrantJob(t, ctx, store, "postcondition-original")
	reversal := completeVerifiedReversal(
		t, ctx, store, "postcondition-reversal", "instance-postcondition-original",
	)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open database for postcondition fault: %v", err)
	}
	if _, err := database.Exec(
		"UPDATE jobs SET status = 'succeeded' WHERE event_key = ?", reversal.EventKey,
	); err != nil {
		_ = database.Close()
		t.Fatalf("inject inbox job postcondition fault: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close postcondition fault database: %v", err)
	}

	_, err = store.ResolveApprovalReversal(ctx, inbox.ApprovalReversalResolution{
		EventKey:                reversal.EventKey,
		OriginalExternalID:      externalID,
		OriginalSubjectSHA256:   testSHA256("tenant-test:ou-requester"),
		CorrectionExternalID:    "lark:correction:CHG-POSTCONDITION:wallet",
		CorrectionRequestSHA256: strings.Repeat("a", 64),
		Operator:                "ops@example.com",
		Reason:                  "postcondition test",
		ChangeTicket:            "CHG-POSTCONDITION",
		ResponseStatus:          "noop",
		Result: inbox.ApprovalCorrectionResult{
			CorrectionType: "wallet_quota", WalletQuota: int64Pointer(2_500_000),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "inbox job affected 0 rows") {
		t.Fatalf("missing inbox job postcondition error = %v", err)
	}
	stored, getErr := store.GetApprovalReversal(ctx, reversal.EventKey)
	if getErr != nil || stored.Resolution != nil {
		t.Fatalf("failed resolution persisted receipt: reversal=%+v err=%v", stored, getErr)
	}
	event, getErr := store.Get(ctx, reversal.EventKey)
	if getErr != nil || event.ProcessingState != inbox.ProcessingStateReversalPending {
		t.Fatalf("failed resolution changed inbox state: event=%+v err=%v", event, getErr)
	}
	grant, getErr := store.GetEntitlementGrantJob(ctx, externalID)
	if getErr != nil || grant.Status != inbox.EntitlementGrantJobStatusReversalPending {
		t.Fatalf("failed resolution changed grant state: grant=%+v err=%v", grant, getErr)
	}
}

func int64Pointer(value int64) *int64 {
	return &value
}

func TestCompleteDecisionAcceptsDuplicateOriginalEventsForOneExternalID(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const externalID = "lark:wallet-topup:instance-original"
	const requestSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	recordVerifiedCommand(t, ctx, store, "original-a", "instance-original", externalID, requestSHA256, "employee-v1")
	recordVerifiedCommand(t, ctx, store, "original-b", "instance-original", externalID, requestSHA256, "employee-v1")

	reversal := completeVerifiedReversal(t, ctx, store, "duplicate-source-reversal", "instance-original")
	if reversal.Result != inbox.ApprovalReversalResultGrantJobMissing ||
		reversal.OriginalExternalID != externalID {
		t.Fatalf("duplicate originals resolved as %+v, want one external ID", reversal)
	}
}

func TestCompleteDecisionRejectsAmbiguousOriginalExternalIDs(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recordVerifiedCommand(
		t, ctx, store, "original-a", "instance-original", "external-a",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "employee-v1",
	)
	recordVerifiedCommand(
		t, ctx, store, "original-b", "instance-original", "external-b",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "employee-v1",
	)

	reversal := completeVerifiedReversal(t, ctx, store, "ambiguous-reversal", "instance-original")
	if reversal.Result != inbox.ApprovalReversalResultOriginalAmbiguous ||
		reversal.Reason != inbox.ApprovalReversalReasonOriginalAmbiguous ||
		reversal.OriginalExternalID != "" {
		t.Fatalf("ambiguous originals resolved as %+v", reversal)
	}
}

func TestLegacyUnresolvedApprovalEvidenceCannotQualifyForReversal(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	recordVerifiedCommand(
		t, ctx, store, "legacy-original", "instance-original", "external-legacy",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "",
	)
	if err := store.Close(); err != nil {
		t.Fatalf("close store before legacy reclassification: %v", err)
	}
	store, err = inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	decision, err := store.GetDecision(ctx, "lark:v2:legacy-original")
	if err != nil {
		t.Fatalf("get reclassified decision: %v", err)
	}
	if decision.Outcome != inbox.DecisionOutcomeShadowLegacyUnresolved {
		t.Fatalf("legacy outcome = %q, want explicit unresolved", decision.Outcome)
	}

	reversal := completeVerifiedReversal(t, ctx, store, "legacy-reversal", "instance-original")
	if reversal.Result != inbox.ApprovalReversalResultOriginalMissing ||
		reversal.OriginalExternalID != "" {
		t.Fatalf("legacy unresolved evidence qualified for reversal: %+v", reversal)
	}
}

func TestCompleteDecisionFencesEveryNonterminalGrantStateAndRejectsLateCompletion(t *testing.T) {
	for _, initialStatus := range []inbox.EntitlementGrantJobStatus{
		inbox.EntitlementGrantJobStatusHeldShadow,
		inbox.EntitlementGrantJobStatusPending,
		inbox.EntitlementGrantJobStatusProcessing,
		inbox.EntitlementGrantJobStatusRetryWait,
	} {
		t.Run(string(initialStatus), func(t *testing.T) {
			ctx := context.Background()
			store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			externalID := recordHeldGrantJob(t, ctx, store, "original")
			var claimed inbox.EntitlementGrantJob
			switch initialStatus {
			case inbox.EntitlementGrantJobStatusHeldShadow:
			case inbox.EntitlementGrantJobStatusPending:
				if released, err := store.ReleaseHeldEntitlementGrantJobs(ctx, "employee-v1"); err != nil || released != 1 {
					t.Fatalf("release grant job: released=%d err=%v", released, err)
				}
			case inbox.EntitlementGrantJobStatusProcessing, inbox.EntitlementGrantJobStatusRetryWait:
				if released, err := store.ReleaseHeldEntitlementGrantJobs(ctx, "employee-v1"); err != nil || released != 1 {
					t.Fatalf("release grant job: released=%d err=%v", released, err)
				}
				claimed, _, err = store.ClaimNextEntitlementGrantJob(ctx)
				if err != nil {
					t.Fatalf("claim grant job: %v", err)
				}
				if initialStatus == inbox.EntitlementGrantJobStatusRetryWait {
					if err := store.RetryEntitlementGrantJob(
						ctx, claimed, inbox.EntitlementGrantFailurePrincipalNotReady, time.Hour,
					); err != nil {
						t.Fatalf("retry grant job: %v", err)
					}
				}
			}
			before, err := store.GetEntitlementGrantJob(ctx, externalID)
			if err != nil {
				t.Fatalf("get grant before reversal: %v", err)
			}
			if before.Status != initialStatus {
				t.Fatalf("grant status before reversal = %q, want %q", before.Status, initialStatus)
			}

			reversal := completeVerifiedReversal(t, ctx, store, "reversal", "instance-original")
			if reversal.Result != inbox.ApprovalReversalResultGrantFenced ||
				reversal.OriginalGrantStatus != initialStatus {
				t.Fatalf("unexpected reversal: %+v", reversal)
			}
			after, err := store.GetEntitlementGrantJob(ctx, externalID)
			if err != nil {
				t.Fatalf("get grant after reversal: %v", err)
			}
			if after.Status != inbox.EntitlementGrantJobStatusReversalPending ||
				after.Attempts != before.Attempts {
				t.Fatalf("fenced grant = %+v, attempts before=%d", after, before.Attempts)
			}
			if !claimed.CreatedAt.IsZero() {
				err := store.CompleteEntitlementGrantJob(ctx, claimed, inbox.EntitlementGrantReceipt{
					ExternalID: externalID, Status: "applied", UserID: 1,
					GrantType: "wallet_quota", QuotaDelta: 2_500_000,
				})
				if err == nil {
					t.Fatal("late grant completion crossed the reversal fence")
				}
			}
		})
	}
}

func TestRepeatedVerifiedReversalKeepsOriginalGrantPending(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	externalID := recordHeldGrantJob(t, ctx, store, "original")
	first := completeVerifiedReversal(t, ctx, store, "reversal-first", "instance-original")
	second := completeVerifiedReversal(t, ctx, store, "reversal-second", "instance-original")
	if first.Result != inbox.ApprovalReversalResultGrantFenced ||
		second.Result != inbox.ApprovalReversalResultGrantAlreadyPending ||
		second.OriginalExternalID != externalID || second.OriginalGrantStatus != inbox.EntitlementGrantJobStatusReversalPending {
		t.Fatalf("repeated reversals: first=%+v second=%+v", first, second)
	}
	grant, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get repeatedly reversed grant: %v", err)
	}
	if grant.Status != inbox.EntitlementGrantJobStatusReversalPending {
		t.Fatalf("repeated reversal changed grant status to %q", grant.Status)
	}
	resolution := inbox.ApprovalReversalResolution{
		EventKey: first.EventKey, OriginalExternalID: externalID,
		OriginalSubjectSHA256:   testSHA256("tenant-test:ou-requester"),
		CorrectionExternalID:    "lark:correction:CHG-REPEATED:wallet",
		CorrectionRequestSHA256: strings.Repeat("a", 64),
		Operator:                "ops@example.com",
		Reason:                  "resolve repeated verified reversal",
		ChangeTicket:            "CHG-REPEATED",
		ResponseStatus:          "noop",
		Result: inbox.ApprovalCorrectionResult{
			CorrectionType: "wallet_quota", WalletQuota: int64Pointer(2_500_000),
		},
	}
	if _, err := store.ResolveApprovalReversal(ctx, resolution); err != nil {
		t.Fatalf("resolve repeated reversal group: %v", err)
	}
	receipt, found, err := store.GetApprovalReversalResolutionForOriginal(ctx, externalID)
	if err != nil || !found || receipt.CorrectionExternalID != resolution.CorrectionExternalID ||
		receipt.OriginalExternalID != externalID {
		t.Fatalf("get grouped reversal receipt: receipt=%+v found=%t err=%v", receipt, found, err)
	}
	for _, reversal := range []inbox.ApprovalReversal{first, second} {
		stored, err := store.GetApprovalReversal(ctx, reversal.EventKey)
		if err != nil || stored.Resolution == nil ||
			stored.Resolution.CorrectionExternalID != resolution.CorrectionExternalID {
			t.Fatalf("grouped reversal resolution = %+v err=%v", stored, err)
		}
	}
	grant, err = store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil || grant.Status != inbox.EntitlementGrantJobStatusReversalResolved {
		t.Fatalf("resolved repeated reversal grant = %+v err=%v", grant, err)
	}
	late := completeVerifiedReversal(t, ctx, store, "reversal-late", "instance-original")
	if late.Result != inbox.ApprovalReversalResultGrantTerminal ||
		late.Reason != inbox.ApprovalReversalReasonManualReviewRequired ||
		late.OriginalGrantStatus != inbox.EntitlementGrantJobStatusReversalResolved {
		t.Fatalf("late reversal after correction = %+v", late)
	}
	resolution.EventKey = late.EventKey
	attached, err := store.ResolveApprovalReversal(ctx, resolution)
	if err != nil || !attached.Replayed {
		t.Fatalf("attach late reversal to existing resolution: resolution=%+v err=%v", attached, err)
	}
}

func TestCorrectionReceiptCannotBindAcrossOriginalGrants(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	firstExternalID := recordHeldGrantJob(t, ctx, store, "receipt-original-first")
	first := completeVerifiedReversal(
		t, ctx, store, "receipt-reversal-first", "instance-receipt-original-first",
	)
	secondExternalID := recordHeldGrantJob(t, ctx, store, "receipt-original-second")
	second := completeVerifiedReversal(
		t, ctx, store, "receipt-reversal-second", "instance-receipt-original-second",
	)
	firstResolution := inbox.ApprovalReversalResolution{
		EventKey: first.EventKey, OriginalExternalID: firstExternalID,
		OriginalSubjectSHA256:   testSHA256("tenant-test:ou-requester"),
		CorrectionExternalID:    "lark:correction:CHG-RECEIPT-BINDING:wallet",
		CorrectionRequestSHA256: strings.Repeat("a", 64),
		Operator:                "ops@example.com",
		Reason:                  "resolve first original grant",
		ChangeTicket:            "CHG-RECEIPT-BINDING",
		ResponseStatus:          "noop",
		Result: inbox.ApprovalCorrectionResult{
			CorrectionType: "wallet_quota", WalletQuota: int64Pointer(2_500_000),
		},
	}
	if _, err := store.ResolveApprovalReversal(ctx, firstResolution); err != nil {
		t.Fatalf("resolve first original receipt: %v", err)
	}
	secondResolution := firstResolution
	secondResolution.EventKey = second.EventKey
	secondResolution.OriginalExternalID = secondExternalID
	secondResolution.CorrectionRequestSHA256 = strings.Repeat("b", 64)
	secondResolution.Reason = "resolve second original grant"
	if _, err := store.ResolveApprovalReversal(ctx, secondResolution); !errors.Is(err, inbox.ErrApprovalReversalResolutionMismatch) {
		t.Fatalf("cross-original correction receipt error = %v", err)
	}
	pending, err := store.GetPendingApprovalReversal(ctx, second.EventKey, secondExternalID)
	if err != nil || pending.Resolution != nil {
		t.Fatalf("cross-original conflict changed pending reversal: reversal=%+v err=%v", pending, err)
	}
}

func TestVerifiedReversalNeverMutatesSucceededGrantReceipt(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	externalID := recordHeldGrantJob(t, ctx, store, "original")
	if released, err := store.ReleaseHeldEntitlementGrantJobs(ctx, "employee-v1"); err != nil || released != 1 {
		t.Fatalf("release original grant: released=%d err=%v", released, err)
	}
	job, found, err := store.ClaimNextEntitlementGrantJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim original grant: found=%t err=%v", found, err)
	}
	receipt := inbox.EntitlementGrantReceipt{
		ExternalID: externalID, Status: "applied", UserID: 1,
		GrantType: "wallet_quota", QuotaDelta: 2_500_000,
	}
	if err := store.CompleteEntitlementGrantJob(ctx, job, receipt); err != nil {
		t.Fatalf("complete original grant: %v", err)
	}

	reversal := completeVerifiedReversal(t, ctx, store, "reversal-terminal", "instance-original")
	if reversal.Result != inbox.ApprovalReversalResultGrantTerminal ||
		reversal.OriginalGrantStatus != inbox.EntitlementGrantJobStatusSucceeded {
		t.Fatalf("terminal reversal = %+v", reversal)
	}
	grant, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get succeeded grant after reversal: %v", err)
	}
	if grant.Status != inbox.EntitlementGrantJobStatusSucceeded || grant.Receipt == nil ||
		*grant.Receipt != receipt {
		t.Fatalf("reversal mutated succeeded grant: %+v", grant)
	}
}

func TestCompleteDecisionRejectsUnboundedReversalReasonAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	externalID := recordHeldGrantJob(t, ctx, store, "original")
	event := operationalEvent("unsafe-reason", "REVERTED")
	event.RevertedInstanceCode = "instance-original"
	if _, err := store.Record(ctx, event); err != nil {
		t.Fatalf("record unsafe reversal event: %v", err)
	}
	job, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim unsafe reversal event: found=%t err=%v", found, err)
	}
	err = store.CompleteDecision(ctx, job, inbox.Decision{
		EventKey: event.Key, ApprovalCode: event.ApprovalCode,
		InstanceCode: "instance-original", EventStatus: event.Status,
		Outcome: inbox.DecisionOutcomeReversalPending,
		ApprovalReversal: &inbox.ApprovalReversalDraft{
			TargetInstanceCode: "instance-original",
			Result:             inbox.ApprovalReversalResultFetchTerminalError,
			Reason:             "raw upstream response must not persist",
		},
	})
	if err == nil {
		t.Fatal("unbounded reversal reason was accepted")
	}
	if _, err := store.GetApprovalReversal(ctx, event.Key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rejected reversal left a ledger row: %v", err)
	}
	grant, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get original grant: %v", err)
	}
	if grant.Status != inbox.EntitlementGrantJobStatusHeldShadow {
		t.Fatalf("rejected reversal changed grant status to %q", grant.Status)
	}
}

func recordVerifiedCommand(
	t *testing.T,
	ctx context.Context,
	store *inbox.Store,
	eventID string,
	instanceCode string,
	externalID string,
	requestSHA256 string,
	policyVersion string,
) {
	t.Helper()
	event := operationalEvent(eventID, "APPROVED")
	event.InstanceCode = instanceCode
	if _, err := store.Record(ctx, event); err != nil {
		t.Fatalf("record verified command event: %v", err)
	}
	job, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim verified command event: found=%t err=%v", found, err)
	}
	if err := store.CompleteDecision(ctx, job, inbox.Decision{
		EventKey: event.Key, ApprovalCode: event.ApprovalCode,
		InstanceCode: instanceCode, EventStatus: event.Status,
		AuthorityStatus: "APPROVED", Outcome: inbox.DecisionOutcomeShadowAuthorityVerified,
		PolicyVersion: policyVersion,
		EntitlementCommand: &inbox.EntitlementCommandShadow{
			ExternalID: externalID, RequestSHA256: requestSHA256,
			SubjectSHA256: strings.Repeat("c", 64), Source: "lark_approval",
			PolicyVersion: "employee-v1", CatalogSHA256: strings.Repeat("d", 64),
			GrantType: "wallet_quota", BusinessCode: "topup_5", QuotaDelta: 2_500_000,
		},
	}); err != nil {
		t.Fatalf("complete verified command decision: %v", err)
	}
}

func completeVerifiedReversal(
	t *testing.T,
	ctx context.Context,
	store *inbox.Store,
	eventID string,
	targetInstanceCode string,
) inbox.ApprovalReversal {
	t.Helper()
	event := operationalEvent(eventID, "REVERTED")
	event.RevertedInstanceCode = targetInstanceCode
	if _, err := store.Record(ctx, event); err != nil {
		t.Fatalf("record verified reversal event: %v", err)
	}
	job, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim verified reversal event: found=%t err=%v", found, err)
	}
	if err := store.CompleteDecision(ctx, job, inbox.Decision{
		EventKey: event.Key, ApprovalCode: event.ApprovalCode,
		InstanceCode: targetInstanceCode, EventStatus: event.Status,
		AuthorityStatus: "APPROVED", Reverted: true,
		Outcome: inbox.DecisionOutcomeReversalPending,
		ApprovalReversal: &inbox.ApprovalReversalDraft{
			TargetInstanceCode:    targetInstanceCode,
			AuthorityApprovalCode: "approval-wallet-v1",
			AuthorityInstanceCode: targetInstanceCode,
			AuthorityStatus:       "APPROVED",
			AuthorityReverted:     true,
		},
	}); err != nil {
		t.Fatalf("complete verified reversal: %v", err)
	}
	reversal, err := store.GetApprovalReversal(ctx, event.Key)
	if err != nil {
		t.Fatalf("get verified reversal: %v", err)
	}
	return reversal
}
