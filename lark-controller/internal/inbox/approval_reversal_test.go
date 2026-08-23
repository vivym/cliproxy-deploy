package inbox_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
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
