package inbox_test

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

func TestCompleteDecisionPersistsHeldGrantJobAcrossRestart(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	event := operationalEvent("evt-held-grant", "APPROVED")
	if _, err := store.Record(ctx, event); err != nil {
		t.Fatalf("record event: %v", err)
	}
	job, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim event job: found=%t err=%v", found, err)
	}
	request, receipt, err := newapi.PlanApprovalGrant(newapi.ApprovalGrantInput{
		TenantKey: event.TenantKey, OpenID: "ou-requester", PolicyVersion: "employee-v1",
		ApprovalKind: "wallet_topup", BusinessCode: "topup_5", QuotaDelta: 2_500_000,
		ApprovalCode: event.ApprovalCode, InstanceCode: event.InstanceCode,
		StartTimeMilliseconds: "1787303900000", SchemaFingerprint: "sha256:abc",
		Locale: "zh-CN", CatalogSHA256: "sha256:catalog",
	})
	if err != nil {
		t.Fatalf("plan grant: %v", err)
	}
	sealer, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant sealer: %v", err)
	}
	sealed, err := sealer.Seal(request)
	if err != nil {
		t.Fatalf("seal grant: %v", err)
	}
	if err := store.CompleteDecision(ctx, job, inbox.Decision{
		EventKey: event.Key, ApprovalCode: event.ApprovalCode,
		InstanceCode: event.InstanceCode, EventStatus: event.Status,
		AuthorityStatus: "APPROVED", Outcome: inbox.DecisionOutcomeShadowAuthorityVerified,
		EntitlementCommand: &inbox.EntitlementCommandShadow{
			ExternalID: receipt.ExternalID, RequestSHA256: receipt.RequestSHA256,
			SubjectSHA256: receipt.SubjectSHA256, Source: "lark_approval",
			PolicyVersion: receipt.PolicyVersion, CatalogSHA256: receipt.CatalogSHA256,
			GrantType: receipt.GrantType, BusinessCode: receipt.BusinessCode,
			QuotaDelta: receipt.QuotaDelta,
		},
		EntitlementGrantJob: &inbox.EntitlementGrantJobDraft{
			ExternalID: sealed.ExternalID, RequestSHA256: sealed.RequestSHA256,
			SubjectSHA256: receipt.SubjectSHA256, KeyID: sealed.KeyID,
			Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		},
	}); err != nil {
		t.Fatalf("complete decision: %v", err)
	}
	if _, found, err := store.ClaimNext(ctx); err != nil || found {
		t.Fatalf("ordinary claimant observed held grant job: found=%t err=%v", found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	store, err = inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stored, err := store.GetEntitlementGrantJob(ctx, sealed.ExternalID)
	if err != nil {
		t.Fatalf("get held grant job: %v", err)
	}
	if stored.Status != inbox.EntitlementGrantJobStatusHeldShadow || stored.Attempts != 0 ||
		stored.RequestSHA256 != sealed.RequestSHA256 || stored.SubjectSHA256 != receipt.SubjectSHA256 ||
		stored.KeyID != sealed.KeyID {
		t.Fatalf("unexpected held grant job: %+v", stored)
	}
	opened, err := sealer.Open(newapi.SealedGrantRequest{
		KeyID: stored.KeyID, ExternalID: stored.ExternalID,
		RequestSHA256: stored.RequestSHA256, Nonce: stored.Nonce,
		Ciphertext: stored.Ciphertext,
	})
	if err != nil {
		t.Fatalf("open restarted grant job: %v", err)
	}
	if opened.ExternalID != request.ExternalID || opened.Identity.Subject != request.Identity.Subject {
		t.Fatalf("opened grant = %+v, want original request", opened)
	}
	if err := store.ValidateEntitlementGrantJobKeyIDs(ctx, []string{sealer.KeyID()}); err != nil {
		t.Fatalf("validate original grant payload key: %v", err)
	}
	replacement, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x24}, 32))
	if err != nil {
		t.Fatalf("new replacement grant sealer: %v", err)
	}
	if err := store.ValidateEntitlementGrantJobKeyIDs(
		ctx,
		[]string{replacement.KeyID(), sealer.KeyID()},
	); err != nil {
		t.Fatalf("validate rotated grant payload keyring: %v", err)
	}
	err = store.ValidateEntitlementGrantJobKeyIDs(ctx, []string{replacement.KeyID()})
	if err == nil || strings.Contains(err.Error(), replacement.KeyID()) ||
		strings.Contains(err.Error(), sealed.KeyID) {
		t.Fatalf("replacement key validation error = %v", err)
	}
}

func TestEmptyStoreAcceptsInitialGrantPayloadKey(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.ValidateEntitlementGrantJobKeyIDs(
		context.Background(),
		[]string{strings.Repeat("a", 64)},
	); err != nil {
		t.Fatalf("validate initial grant payload key: %v", err)
	}
	if err := store.ValidateEntitlementGrantJobKeyIDs(context.Background(), nil); err == nil {
		t.Fatal("accepted empty grant payload keyring")
	}
}

func TestTerminalEntitlementGrantJobDoesNotBlockKeyRetirement(t *testing.T) {
	for _, terminalStatus := range []inbox.EntitlementGrantJobStatus{
		inbox.EntitlementGrantJobStatusSucceeded,
		inbox.EntitlementGrantJobStatusDeadLetter,
	} {
		t.Run(string(terminalStatus), func(t *testing.T) {
			ctx := context.Background()
			store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			externalID := recordHeldGrantJob(t, ctx, store, "evt-retire-grant-key-"+string(terminalStatus))
			if released, err := store.ReleaseHeldEntitlementGrantJobs(ctx, "employee-v1"); err != nil || released != 1 {
				t.Fatalf("release held grant: released=%d err=%v", released, err)
			}
			job, found, err := store.ClaimNextEntitlementGrantJob(ctx)
			if err != nil || !found {
				t.Fatalf("claim released grant: found=%t err=%v", found, err)
			}
			switch terminalStatus {
			case inbox.EntitlementGrantJobStatusSucceeded:
				err = store.CompleteEntitlementGrantJob(ctx, job, inbox.EntitlementGrantReceipt{
					ExternalID: externalID,
					Status:     "applied",
					UserID:     42,
					GrantType:  "wallet_quota",
					QuotaDelta: 2_500_000,
				})
			case inbox.EntitlementGrantJobStatusDeadLetter:
				err = store.DeadLetterEntitlementGrantJob(
					ctx,
					job,
					inbox.EntitlementGrantFailureInvalidSealedPayload,
				)
			}
			if err != nil {
				t.Fatalf("transition grant to %s: %v", terminalStatus, err)
			}
			replacement, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x24}, 32))
			if err != nil {
				t.Fatalf("new replacement sealer: %v", err)
			}
			if err := store.ValidateEntitlementGrantJobKeyIDs(ctx, []string{replacement.KeyID()}); err != nil {
				t.Fatalf("terminal grant blocked key retirement: %v", err)
			}
		})
	}
}

func TestUnknownEntitlementGrantJobStatusBlocksKeyRetirement(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	createLegacyEntitlementGrantJobDatabase(
		t,
		databasePath,
		inbox.EntitlementGrantJobStatus("future_nonterminal_status"),
	)
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store with future grant status: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	configuredKeyID := strings.Repeat("d", 64)
	err = store.ValidateEntitlementGrantJobKeyIDs(context.Background(), []string{configuredKeyID})
	if err == nil || strings.Contains(err.Error(), configuredKeyID) ||
		strings.Contains(err.Error(), strings.Repeat("c", 64)) {
		t.Fatalf("future grant status key validation error = %v", err)
	}
}

func TestOpenMigratesLegacyEntitlementGrantJobSchema(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	createLegacyEntitlementGrantJobDatabase(
		t,
		databasePath,
		inbox.EntitlementGrantJobStatusHeldShadow,
	)

	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open and migrate store: %v", err)
	}
	job, err := store.GetEntitlementGrantJob(ctx, "lark:wallet-topup:instance-legacy-grant")
	if err != nil {
		_ = store.Close()
		t.Fatalf("get migrated grant job: %v", err)
	}
	if job.Status != inbox.EntitlementGrantJobStatusHeldShadow || job.Receipt != nil ||
		!job.ActivatedAt.IsZero() || !job.CompletedAt.IsZero() {
		_ = store.Close()
		t.Fatalf("unexpected migrated grant job: %+v", job)
	}
	if released, err := store.ReleaseHeldEntitlementGrantJobs(ctx, "employee-v1"); err != nil || released != 0 {
		_ = store.Close()
		t.Fatalf("orphaned migrated grant job release: released=%d err=%v", released, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}

	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	grantColumns := tableColumnNames(t, database, "entitlement_grant_jobs")
	jobColumns := tableColumnNames(t, database, "jobs")
	for _, column := range []string{"activated_at", "response_status", "completed_at"} {
		if _, ok := grantColumns[column]; !ok {
			t.Fatalf("migrated entitlement_grant_jobs is missing %q", column)
		}
		if _, ok := jobColumns[column]; ok {
			t.Fatalf("generic jobs unexpectedly contains grant column %q", column)
		}
	}
}

func TestOpenRejectsLegacyActiveEntitlementGrantJobWithoutActivation(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	createLegacyEntitlementGrantJobDatabase(
		t,
		databasePath,
		inbox.EntitlementGrantJobStatusPending,
	)

	store, err := inbox.Open(databasePath)
	if err == nil {
		_ = store.Close()
		t.Fatal("opened active legacy grant job without activated_at")
	}
	if strings.Contains(err.Error(), "lark:wallet-topup:instance-legacy-grant") {
		t.Fatalf("migration error leaked external ID: %v", err)
	}
}

func createLegacyEntitlementGrantJobDatabase(
	t *testing.T,
	databasePath string,
	status inbox.EntitlementGrantJobStatus,
) {
	t.Helper()
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = database.Exec(`
CREATE TABLE entitlement_grant_jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    external_id TEXT NOT NULL UNIQUE,
    request_sha256 TEXT NOT NULL,
    subject_sha256 TEXT NOT NULL,
    key_id TEXT NOT NULL,
    nonce BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    status TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
INSERT INTO entitlement_grant_jobs (
    external_id, request_sha256, subject_sha256, key_id, nonce, ciphertext,
    status, attempts, next_attempt_at, last_error, created_at, updated_at
) VALUES (?, ?, ?, ?, zeroblob(12), zeroblob(32), ?, 0, ?, '', ?, ?)`,
		"lark:wallet-topup:instance-legacy-grant",
		strings.Repeat("a", 64),
		strings.Repeat("b", 64),
		strings.Repeat("c", 64),
		status,
		"2026-08-20T00:00:00Z",
		"2026-08-20T00:00:00Z",
		"2026-08-20T00:00:00Z",
	)
	if err != nil {
		_ = database.Close()
		t.Fatalf("create legacy grant job table: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}
}

func TestHeldGrantJobsRequireExplicitReleaseBeforeClaim(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	externalID := recordHeldGrantJob(t, ctx, store, "evt-release-grant")
	if _, found, err := store.ClaimNextEntitlementGrantJob(ctx); err != nil || found {
		t.Fatalf("claim held job: found=%t err=%v", found, err)
	}
	released, err := store.ReleaseHeldEntitlementGrantJobs(ctx, "employee-v1")
	if err != nil || released != 1 {
		t.Fatalf("release held jobs: released=%d err=%v", released, err)
	}
	if released, err := store.ReleaseHeldEntitlementGrantJobs(ctx, "employee-v1"); err != nil || released != 0 {
		t.Fatalf("repeat release: released=%d err=%v", released, err)
	}
	job, found, err := store.ClaimNextEntitlementGrantJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim released job: found=%t err=%v", found, err)
	}
	if job.ExternalID != externalID || job.Status != inbox.EntitlementGrantJobStatusProcessing ||
		job.Attempts != 1 || job.ActivatedAt.IsZero() {
		t.Fatalf("unexpected claimed grant job: %+v", job)
	}
	if _, found, err := store.ClaimNextEntitlementGrantJob(ctx); err != nil || found {
		t.Fatalf("claim processing job twice: found=%t err=%v", found, err)
	}
}

func TestOpenRecoversProcessingEntitlementGrantJob(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	externalID := recordHeldGrantJob(t, ctx, store, "evt-recover-grant")
	if released, err := store.ReleaseHeldEntitlementGrantJobs(ctx, "employee-v1"); err != nil || released != 1 {
		t.Fatalf("release held job: released=%d err=%v", released, err)
	}
	first, found, err := store.ClaimNextEntitlementGrantJob(ctx)
	if err != nil || !found || first.Attempts != 1 {
		t.Fatalf("first claim: found=%t job=%+v err=%v", found, first, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	store, err = inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	recovered, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get recovered grant job: %v", err)
	}
	if recovered.Status != inbox.EntitlementGrantJobStatusPending || recovered.Attempts != 1 {
		t.Fatalf("unexpected recovered grant job: %+v", recovered)
	}
	second, found, err := store.ClaimNextEntitlementGrantJob(ctx)
	if err != nil || !found || second.Attempts != 2 {
		t.Fatalf("recovered claim: found=%t job=%+v err=%v", found, second, err)
	}
}

func TestCompleteEntitlementGrantJobPersistsSanitizedReceipt(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	externalID := recordHeldGrantJob(t, ctx, store, "evt-complete-grant")
	if released, err := store.ReleaseHeldEntitlementGrantJobs(ctx, "employee-v1"); err != nil || released != 1 {
		t.Fatalf("release held job: released=%d err=%v", released, err)
	}
	job, found, err := store.ClaimNextEntitlementGrantJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim grant job: found=%t err=%v", found, err)
	}
	receipt := inbox.EntitlementGrantReceipt{
		ExternalID: externalID, Status: "applied", UserID: 42,
		GrantType: "wallet_quota", QuotaDelta: 2_500_000,
	}
	if err := store.CompleteEntitlementGrantJob(ctx, job, receipt); err != nil {
		t.Fatalf("complete grant job: %v", err)
	}
	stored, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get completed grant job: %v", err)
	}
	if stored.Status != inbox.EntitlementGrantJobStatusSucceeded || stored.Receipt == nil ||
		*stored.Receipt != receipt || stored.LastError != "" || stored.CompletedAt.IsZero() {
		t.Fatalf("unexpected completed grant job: %+v", stored)
	}
	if _, found, err := store.ClaimNextEntitlementGrantJob(ctx); err != nil || found {
		t.Fatalf("claim completed grant job: found=%t err=%v", found, err)
	}
	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read completed grant metrics: %v", err)
	}
	if snapshot.NewAPIGrants["applied"] != 1 {
		t.Fatalf("unexpected New API grant metrics: %v", snapshot.NewAPIGrants)
	}
	resultKey := inbox.EntitlementGrantResultKey{GrantType: "wallet_quota", Status: "applied"}
	if snapshot.EntitlementGrantResults[resultKey] != 1 {
		t.Fatalf("unexpected entitlement grant results: %v", snapshot.EntitlementGrantResults)
	}
}

func TestRetryEntitlementGrantJobWaitsUntilEligible(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	externalID := recordHeldGrantJob(t, ctx, store, "evt-retry-grant")
	if _, err := store.ReleaseHeldEntitlementGrantJobs(ctx, "employee-v1"); err != nil {
		t.Fatalf("release held job: %v", err)
	}
	job, found, err := store.ClaimNextEntitlementGrantJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim grant job: found=%t err=%v", found, err)
	}
	if err := store.RetryEntitlementGrantJob(
		ctx,
		job,
		inbox.EntitlementGrantFailureTemporarilyUnavailable,
		50*time.Millisecond,
	); err != nil {
		t.Fatalf("retry grant job: %v", err)
	}
	stored, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get retrying grant job: %v", err)
	}
	if stored.Status != inbox.EntitlementGrantJobStatusRetryWait ||
		stored.LastError != string(inbox.EntitlementGrantFailureTemporarilyUnavailable) {
		t.Fatalf("unexpected retrying grant job: %+v", stored)
	}
	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read retrying grant metrics: %v", err)
	}
	if snapshot.EntitlementGrantRetries[string(inbox.EntitlementGrantFailureTemporarilyUnavailable)] != 1 ||
		snapshot.OldestReadyJobAge != 0 {
		t.Fatalf("unexpected retrying grant metrics: %+v", snapshot)
	}
	if _, found, err := store.ClaimNextEntitlementGrantJob(ctx); err != nil || found {
		t.Fatalf("claim grant before retry eligibility: found=%t err=%v", found, err)
	}
	time.Sleep(60 * time.Millisecond)
	retry, found, err := store.ClaimNextEntitlementGrantJob(ctx)
	if err != nil || !found || retry.Attempts != 2 {
		t.Fatalf("claim eligible grant retry: found=%t job=%+v err=%v", found, retry, err)
	}
}

func TestDeadLetterEntitlementGrantJobStoresOnlyKnownReason(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	externalID := recordHeldGrantJob(t, ctx, store, "evt-dead-grant")
	if _, err := store.ReleaseHeldEntitlementGrantJobs(ctx, "employee-v1"); err != nil {
		t.Fatalf("release held job: %v", err)
	}
	job, found, err := store.ClaimNextEntitlementGrantJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim grant job: found=%t err=%v", found, err)
	}
	if err := store.DeadLetterEntitlementGrantJob(
		ctx,
		job,
		inbox.EntitlementGrantFailureIntegrationUnauthorized,
	); err != nil {
		t.Fatalf("dead-letter grant job: %v", err)
	}
	stored, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get dead grant job: %v", err)
	}
	if stored.Status != inbox.EntitlementGrantJobStatusDeadLetter ||
		stored.LastError != string(inbox.EntitlementGrantFailureIntegrationUnauthorized) ||
		stored.Receipt != nil || stored.CompletedAt.IsZero() {
		t.Fatalf("unexpected dead grant job: %+v", stored)
	}
	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read dead grant metrics: %v", err)
	}
	if snapshot.EntitlementGrantDeadLetters[string(inbox.EntitlementGrantFailureIntegrationUnauthorized)] != 1 {
		t.Fatalf("unexpected dead grant metrics: %+v", snapshot)
	}
	if err := store.DeadLetterEntitlementGrantJob(
		ctx,
		job,
		"sensitive upstream message",
	); err == nil {
		t.Fatal("accepted unclassified persistent failure reason")
	}
}

func TestEntitlementGrantFailureCatalogIsClosedAndCarriesRetryPolicy(t *testing.T) {
	type metadata struct {
		retryable bool
		exhausted inbox.EntitlementGrantFailureReason
	}
	expected := map[inbox.EntitlementGrantFailureReason]metadata{
		inbox.EntitlementGrantFailureInvalidRequest:                {},
		inbox.EntitlementGrantFailureIntegrationUnauthorized:       {},
		inbox.EntitlementGrantFailurePrincipalNotReady:             {true, inbox.EntitlementGrantFailureRetryExhaustedPrincipal},
		inbox.EntitlementGrantFailurePrincipalDisabled:             {},
		inbox.EntitlementGrantFailureUnmanagedSubscriptionConflict: {},
		inbox.EntitlementGrantFailurePolicyVersionMismatch:         {},
		inbox.EntitlementGrantFailureApprovalBindingMismatch:       {},
		inbox.EntitlementGrantFailureTemporarilyUnavailable:        {true, inbox.EntitlementGrantFailureRetryExhaustedUnavailable},
		inbox.EntitlementGrantFailureExternalIDPayloadMismatch:     {},
		inbox.EntitlementGrantFailureUnknownPackage:                {},
		inbox.EntitlementGrantFailureUnknownLevel:                  {},
		inbox.EntitlementGrantFailureQuotaOutOfRange:               {},
		inbox.EntitlementGrantFailureTimeout:                       {true, inbox.EntitlementGrantFailureRetryExhaustedTimeout},
		inbox.EntitlementGrantFailureTransport:                     {true, inbox.EntitlementGrantFailureRetryExhaustedTransport},
		inbox.EntitlementGrantFailureInvalidResponse:               {},
		inbox.EntitlementGrantFailureInvalidSealedPayload:          {},
		inbox.EntitlementGrantFailureUnclassified:                  {},
		inbox.EntitlementGrantFailureRetryExhaustedPrincipal:       {},
		inbox.EntitlementGrantFailureRetryExhaustedUnavailable:     {},
		inbox.EntitlementGrantFailureRetryExhaustedTimeout:         {},
		inbox.EntitlementGrantFailureRetryExhaustedTransport:       {},
	}
	reasons := inbox.EntitlementGrantFailureReasons()
	if len(reasons) != len(expected) {
		t.Fatalf("failure catalog has %d reasons, want %d: %v", len(reasons), len(expected), reasons)
	}
	seen := make(map[inbox.EntitlementGrantFailureReason]struct{}, len(reasons))
	for _, reason := range reasons {
		want, ok := expected[reason]
		if !ok {
			t.Fatalf("failure catalog contains unexpected reason %q", reason)
		}
		if _, duplicate := seen[reason]; duplicate {
			t.Fatalf("failure catalog repeats reason %q", reason)
		}
		seen[reason] = struct{}{}
		if parsed := inbox.ParseEntitlementGrantFailureReason(string(reason)); parsed != reason {
			t.Fatalf("parse %q = %q", reason, parsed)
		}
		if retryable := inbox.IsRetryableEntitlementGrantFailure(reason); retryable != want.retryable {
			t.Fatalf("retryable %q = %t, want %t", reason, retryable, want.retryable)
		}
		if want.retryable {
			if exhausted := inbox.ExhaustedEntitlementGrantFailure(reason); exhausted != want.exhausted {
				t.Fatalf("exhausted %q = %q, want %q", reason, exhausted, want.exhausted)
			}
		}
	}
	if parsed := inbox.ParseEntitlementGrantFailureReason("sensitive upstream detail"); parsed != inbox.EntitlementGrantFailureUnclassified {
		t.Fatalf("unknown failure parsed as %q", parsed)
	}
}

func recordHeldGrantJob(
	t *testing.T,
	ctx context.Context,
	store *inbox.Store,
	eventID string,
) string {
	t.Helper()
	event := operationalEvent(eventID, "APPROVED")
	if _, err := store.Record(ctx, event); err != nil {
		t.Fatalf("record held grant event: %v", err)
	}
	job, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim held grant event: found=%t err=%v", found, err)
	}
	request, receipt, err := newapi.PlanApprovalGrant(newapi.ApprovalGrantInput{
		TenantKey: event.TenantKey, OpenID: "ou-requester", PolicyVersion: "employee-v1",
		ApprovalKind: "wallet_topup", BusinessCode: "topup_5", QuotaDelta: 2_500_000,
		ApprovalCode: event.ApprovalCode, InstanceCode: event.InstanceCode,
		StartTimeMilliseconds: "1787303900000", SchemaFingerprint: "sha256:abc",
		Locale: "zh-CN", CatalogSHA256: "sha256:catalog",
	})
	if err != nil {
		t.Fatalf("plan held grant: %v", err)
	}
	sealer, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new held grant sealer: %v", err)
	}
	sealed, err := sealer.Seal(request)
	if err != nil {
		t.Fatalf("seal held grant: %v", err)
	}
	if err := store.CompleteDecision(ctx, job, inbox.Decision{
		EventKey: event.Key, ApprovalCode: event.ApprovalCode,
		InstanceCode: event.InstanceCode, EventStatus: event.Status,
		AuthorityStatus: "APPROVED", Outcome: inbox.DecisionOutcomeShadowAuthorityVerified,
		EntitlementCommand: &inbox.EntitlementCommandShadow{
			ExternalID: receipt.ExternalID, RequestSHA256: receipt.RequestSHA256,
			SubjectSHA256: receipt.SubjectSHA256, Source: "lark_approval",
			PolicyVersion: receipt.PolicyVersion, CatalogSHA256: receipt.CatalogSHA256,
			GrantType: receipt.GrantType, BusinessCode: receipt.BusinessCode,
			QuotaDelta: receipt.QuotaDelta,
		},
		EntitlementGrantJob: &inbox.EntitlementGrantJobDraft{
			ExternalID: sealed.ExternalID, RequestSHA256: sealed.RequestSHA256,
			SubjectSHA256: receipt.SubjectSHA256, KeyID: sealed.KeyID,
			Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		},
	}); err != nil {
		t.Fatalf("complete held grant decision: %v", err)
	}
	return sealed.ExternalID
}

func tableColumnNames(t *testing.T, database *sql.DB, table string) map[string]struct{} {
	t.Helper()
	rows, err := database.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("inspect %s columns: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	columns := make(map[string]struct{})
	for rows.Next() {
		var columnID int
		var name string
		var columnType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int
		if err := rows.Scan(
			&columnID,
			&name,
			&columnType,
			&notNull,
			&defaultValue,
			&primaryKey,
		); err != nil {
			t.Fatalf("scan %s column: %v", table, err)
		}
		columns[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s columns: %v", table, err)
	}
	return columns
}
