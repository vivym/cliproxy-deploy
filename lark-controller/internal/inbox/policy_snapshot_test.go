package inbox_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
)

func TestOpenMigratesLegacyApprovalDecisionSchema(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = database.Exec(`
CREATE TABLE approval_instances (
    event_key TEXT PRIMARY KEY,
    approval_code TEXT NOT NULL,
    instance_code TEXT NOT NULL,
    event_status TEXT NOT NULL,
    authority_status TEXT NOT NULL,
    outcome TEXT NOT NULL,
    open_id_hash TEXT NOT NULL DEFAULT '',
    form_sha256 TEXT NOT NULL DEFAULT '',
    start_time TEXT NOT NULL DEFAULT '',
    reverted INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE TABLE policy_versions (
    policy_version TEXT PRIMARY KEY,
    catalog_sha256 TEXT NOT NULL UNIQUE,
    source_sha256 TEXT NOT NULL,
    state TEXT NOT NULL,
    catalog_json TEXT NOT NULL,
    loaded_at TEXT NOT NULL
);
CREATE TABLE controller_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_key TEXT NOT NULL,
    action TEXT NOT NULL,
    outcome TEXT NOT NULL,
    created_at TEXT NOT NULL
)`)
	if err != nil {
		_ = database.Close()
		t.Fatalf("create legacy approval decision table: %v", err)
	}
	_, err = database.Exec(`
INSERT INTO approval_instances (
    event_key, approval_code, instance_code, event_status, authority_status,
    outcome, open_id_hash, form_sha256, start_time, reverted, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"lark:v2:legacy-unresolved", "approval-wallet-v0", "instance-legacy-unresolved",
		"APPROVED", "APPROVED", "shadow_authority_verified", strings.Repeat("a", 64),
		strings.Repeat("b", 64), "1787270300000", false, "2026-08-20T00:00:00Z",
	)
	if err != nil {
		_ = database.Close()
		t.Fatalf("insert legacy approval decision: %v", err)
	}
	_, err = database.Exec(`
INSERT INTO controller_audit (event_key, action, outcome, created_at)
VALUES (?, ?, ?, ?)`,
		"lark:v2:legacy-unresolved", "shadow_evaluate", "shadow_authority_verified",
		"2026-08-20T00:00:00Z",
	)
	if err != nil {
		_ = database.Close()
		t.Fatalf("insert legacy controller audit: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open and migrate store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	legacy, err := store.GetDecision(ctx, "lark:v2:legacy-unresolved")
	if err != nil {
		t.Fatalf("get migrated legacy decision: %v", err)
	}
	if legacy.Outcome != inbox.DecisionOutcomeShadowLegacyUnresolved || legacy.PolicyVersion != "" ||
		legacy.SchemaFingerprint != "" {
		t.Fatalf("legacy decision = %+v, want explicit unresolved policy evidence", legacy)
	}
	auditDatabase, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open migrated audit database: %v", err)
	}
	t.Cleanup(func() { _ = auditDatabase.Close() })
	var auditOutcome inbox.DecisionOutcome
	if err := auditDatabase.QueryRow(
		"SELECT outcome FROM controller_audit WHERE event_key = ?",
		"lark:v2:legacy-unresolved",
	).Scan(&auditOutcome); err != nil {
		t.Fatalf("get migrated legacy audit: %v", err)
	}
	if auditOutcome != inbox.DecisionOutcomeShadowLegacyUnresolved {
		t.Fatalf("legacy audit outcome = %q, want %q", auditOutcome, inbox.DecisionOutcomeShadowLegacyUnresolved)
	}
	if err := store.SyncPolicySnapshot(ctx, policy.Snapshot{
		Policies: []policy.PolicySnapshot{policySnapshot("employee-v1", policy.PolicyStateActive, "a")},
		Bindings: []policy.ApprovalBindingSnapshot{
			approvalBindingSnapshot("approval-wallet-v1", "employee-v1", "1", ""),
		},
	}); err != nil {
		t.Fatalf("sync policy through migrated policy_versions schema: %v", err)
	}
	_, err = store.Record(ctx, inbox.Event{
		Key: "lark:v2:legacy-migration", SchemaVersion: "2.0",
		EventID: "legacy-migration", EventType: "approval.instance.status_changed_v4",
		AppID: "cli_test", TenantKey: "tenant-test", ApprovalCode: "approval-wallet-v1",
		InstanceCode: "instance-legacy-migration", Status: "APPROVED",
		PayloadJSON: `{"status":"APPROVED"}`,
	})
	if err != nil {
		t.Fatalf("record event after migration: %v", err)
	}
	job, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim event after migration: found=%v err=%v", found, err)
	}
	decision := inbox.Decision{
		EventKey: job.Event.Key, ApprovalCode: job.Event.ApprovalCode,
		InstanceCode: job.Event.InstanceCode, EventStatus: job.Event.Status,
		AuthorityStatus: "APPROVED", Outcome: inbox.DecisionOutcomeShadowAuthorityVerified,
		PolicyVersion: "employee-v1", ApprovalKind: policy.ApprovalKindWalletTopUp,
		SchemaFingerprint: "sha256:" + strings.Repeat("a", 64), BusinessCode: "topup_5",
		Locale: "zh-CN", CatalogSHA256: strings.Repeat("b", 64), QuotaDelta: 2500000,
	}
	if err := store.CompleteDecision(ctx, job, decision); err != nil {
		t.Fatalf("complete decision after migration: %v", err)
	}
	stored, err := store.GetDecision(ctx, job.Event.Key)
	if err != nil {
		t.Fatalf("get decision after migration: %v", err)
	}
	if stored.PolicyVersion != decision.PolicyVersion || stored.ApprovalKind != decision.ApprovalKind ||
		stored.SchemaFingerprint != decision.SchemaFingerprint || stored.BusinessCode != decision.BusinessCode ||
		stored.Locale != decision.Locale || stored.CatalogSHA256 != decision.CatalogSHA256 ||
		stored.QuotaDelta != decision.QuotaDelta {
		t.Fatalf("migrated decision = %+v, want policy evidence %+v", stored, decision)
	}
}

func TestSyncPolicySnapshotPreservesImmutableHistoryAcrossRotation(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	initial := policy.Snapshot{
		Policies: []policy.PolicySnapshot{
			policySnapshot("employee-v1", policy.PolicyStateActive, "a"),
		},
		Bindings: []policy.ApprovalBindingSnapshot{
			approvalBindingSnapshot("approval-wallet-v1", "employee-v1", "1", ""),
		},
	}
	if err := store.SyncPolicySnapshot(ctx, initial); err != nil {
		t.Fatalf("sync initial policy snapshot: %v", err)
	}

	rotated := policy.Snapshot{
		Policies: []policy.PolicySnapshot{
			policySnapshot("employee-v1", policy.PolicyStateDraining, "a"),
			policySnapshot("employee-v2", policy.PolicyStateActive, "b"),
		},
		Bindings: []policy.ApprovalBindingSnapshot{
			approvalBindingSnapshot("approval-wallet-v1", "employee-v1", "1", cutoff),
			approvalBindingSnapshot("approval-wallet-v2", "employee-v2", "2", ""),
		},
	}
	rotated.Policies[0].SourceSHA256 = strings.Repeat("c", 64)
	if err := store.SyncPolicySnapshot(ctx, rotated); err != nil {
		t.Fatalf("sync rotated policy snapshot: %v", err)
	}
	if err := store.SyncPolicySnapshot(ctx, rotated); err != nil {
		t.Fatalf("replay identical rotated snapshot: %v", err)
	}

	removedHistory := policy.Snapshot{
		Policies: rotated.Policies[1:],
		Bindings: rotated.Bindings[1:],
	}
	if err := store.SyncPolicySnapshot(ctx, removedHistory); err == nil {
		t.Fatal("snapshot that removed historical policy and binding was accepted")
	}

	mutatedHistory := rotated
	mutatedHistory.Policies = append([]policy.PolicySnapshot(nil), rotated.Policies...)
	mutatedHistory.Policies[0].CatalogSHA256 = strings.Repeat("f", 64)
	if err := store.SyncPolicySnapshot(ctx, mutatedHistory); err == nil {
		t.Fatal("snapshot that mutated a historical catalog was accepted")
	}

	changedSourceWithoutTransition := rotated
	changedSourceWithoutTransition.Policies = append([]policy.PolicySnapshot(nil), rotated.Policies...)
	changedSourceWithoutTransition.Policies[0].SourceSHA256 = strings.Repeat("d", 64)
	if err := store.SyncPolicySnapshot(ctx, changedSourceWithoutTransition); err == nil {
		t.Fatal("snapshot that changed source without a state transition was accepted")
	}

	reopenedWindow := rotated
	reopenedWindow.Bindings = append([]policy.ApprovalBindingSnapshot(nil), rotated.Bindings...)
	reopenedWindow.Bindings[0].AcceptInstanceStartedBefore = ""
	if err := store.SyncPolicySnapshot(ctx, reopenedWindow); err == nil {
		t.Fatal("snapshot that reopened a draining approval window was accepted")
	}

	lateBinding := rotated
	lateBinding.Bindings = append([]policy.ApprovalBindingSnapshot(nil), rotated.Bindings...)
	lateBinding.Bindings = append(
		lateBinding.Bindings,
		approvalBindingSnapshot("approval-wallet-v2-extra", "employee-v2", "3", ""),
	)
	if err := store.SyncPolicySnapshot(ctx, lateBinding); err == nil {
		t.Fatal("snapshot that attached a new binding to an existing policy was accepted")
	}

	retired := rotated
	retired.Policies = append([]policy.PolicySnapshot(nil), rotated.Policies...)
	retired.Policies[0].State = policy.PolicyStateRetired
	retired.Policies[0].SourceSHA256 = strings.Repeat("d", 64)
	retired.Policies[0].RetireAfter = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if err := store.SyncPolicySnapshot(ctx, retired); err == nil {
		t.Fatal("snapshot retired a policy before its trace window ended")
	}

	_, err = store.Record(ctx, inbox.Event{
		Key: "lark:v2:pending-retirement", SchemaVersion: "2.0",
		EventID: "pending-retirement", EventType: "approval.instance.status_changed_v4",
		AppID: "cli_test", TenantKey: "tenant-test", ApprovalCode: "approval-wallet-v1",
		InstanceCode: "instance-pending-retirement", Status: "PENDING",
		PayloadJSON: `{"status":"PENDING"}`,
	})
	if err != nil {
		t.Fatalf("record pending historical approval: %v", err)
	}
	retired.Policies[0].RetireAfter = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if err := store.SyncPolicySnapshot(ctx, retired); err == nil {
		t.Fatal("snapshot retired a policy with an unfinished local approval")
	}
	job, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim pending historical approval: found=%v err=%v", found, err)
	}
	if err := store.CompleteDecision(ctx, job, inbox.Decision{
		EventKey: job.Event.Key, ApprovalCode: job.Event.ApprovalCode,
		InstanceCode: job.Event.InstanceCode, EventStatus: job.Event.Status,
		Outcome: inbox.DecisionOutcomeShadowIgnoredNonApproved,
	}); err != nil {
		t.Fatalf("complete pending historical approval: %v", err)
	}
	if err := store.SyncPolicySnapshot(ctx, retired); err != nil {
		t.Fatalf("retire drained historical policy after trace window: %v", err)
	}
}

func TestSyncPolicySnapshotRejectsRotationWithUnfinishedBaseGrant(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	initial := policy.Snapshot{
		Policies: []policy.PolicySnapshot{
			policySnapshot("employee-v1", policy.PolicyStateActive, "a"),
		},
		Bindings: []policy.ApprovalBindingSnapshot{
			approvalBindingSnapshot("approval-wallet-v1", "employee-v1", "1", ""),
		},
	}
	if err := store.SyncPolicySnapshot(ctx, initial); err != nil {
		t.Fatalf("sync initial policy snapshot: %v", err)
	}
	identity, err := inbox.NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("new OAuth identity: %v", err)
	}
	loginCode, err := store.CreateOAuthLoginCode(ctx, identity)
	if err != nil {
		t.Fatalf("create OAuth login code: %v", err)
	}
	accessHandle, err := store.ExchangeOAuthLoginCode(ctx, loginCode)
	if err != nil {
		t.Fatalf("exchange OAuth login code: %v", err)
	}
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant keyring: %v", err)
	}
	_, err = store.ConsumeOAuthAccessHandleAndStoreBaseGrant(
		ctx,
		accessHandle,
		func(got inbox.OAuthIdentity) (inbox.BaseSubscriptionGrantDraft, error) {
			request, receipt, err := newapi.PlanBaseSubscriptionGrant(newapi.BaseSubscriptionGrantInput{
				Subject: got.Subject, PolicyVersion: "employee-v1", LevelCode: "basic",
				PeriodQuota: 5_000_000, ResetPeriod: "weekly", ResetTimezone: "Asia/Shanghai",
				CatalogSHA256: initial.Policies[0].CatalogSHA256,
			})
			if err != nil {
				return inbox.BaseSubscriptionGrantDraft{}, err
			}
			sealed, err := keyring.Seal(request)
			if err != nil {
				return inbox.BaseSubscriptionGrantDraft{}, err
			}
			return inbox.BaseSubscriptionGrantDraft{
				ExternalID: receipt.ExternalID, RequestSHA256: receipt.RequestSHA256,
				SubjectSHA256: receipt.SubjectSHA256, PolicyVersion: receipt.PolicyVersion,
				CatalogSHA256: receipt.CatalogSHA256, LevelCode: receipt.BusinessCode,
				PeriodQuota: receipt.PeriodQuota, ResetPeriod: receipt.ResetPeriod,
				ResetTimezone: receipt.ResetTimezone,
				GrantJob: inbox.EntitlementGrantJobDraft{
					ExternalID: sealed.ExternalID, RequestSHA256: sealed.RequestSHA256,
					SubjectSHA256: receipt.SubjectSHA256, KeyID: sealed.KeyID,
					Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
				},
			}, nil
		},
	)
	if err != nil {
		t.Fatalf("store unfinished base grant: %v", err)
	}

	cutoff := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	rotated := policy.Snapshot{
		Policies: []policy.PolicySnapshot{
			policySnapshot("employee-v1", policy.PolicyStateDraining, "c"),
			policySnapshot("employee-v2", policy.PolicyStateActive, "b"),
		},
		Bindings: []policy.ApprovalBindingSnapshot{
			approvalBindingSnapshot("approval-wallet-v1", "employee-v1", "1", cutoff),
			approvalBindingSnapshot("approval-wallet-v2", "employee-v2", "2", ""),
		},
	}
	rotated.Policies[0].CatalogSHA256 = initial.Policies[0].CatalogSHA256
	rotated.Policies[0].CatalogJSON = initial.Policies[0].CatalogJSON
	if err := store.SyncPolicySnapshot(ctx, rotated); err == nil ||
		!strings.Contains(err.Error(), "unfinished base subscription grant") {
		t.Fatalf("rotate policy with unfinished base grant error = %v", err)
	}
}

func TestSyncPolicySnapshotRejectsRetirementWithUnfinishedApprovalGrant(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	initial := policy.Snapshot{
		Policies: []policy.PolicySnapshot{
			policySnapshot("employee-v1", policy.PolicyStateActive, "a"),
		},
		Bindings: []policy.ApprovalBindingSnapshot{
			approvalBindingSnapshot("approval-wallet-v1", "employee-v1", "1", ""),
		},
	}
	if err := store.SyncPolicySnapshot(ctx, initial); err != nil {
		t.Fatalf("sync initial policy snapshot: %v", err)
	}
	cutoff := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	rotated := policy.Snapshot{
		Policies: []policy.PolicySnapshot{
			policySnapshot("employee-v1", policy.PolicyStateDraining, "c"),
			policySnapshot("employee-v2", policy.PolicyStateActive, "b"),
		},
		Bindings: []policy.ApprovalBindingSnapshot{
			approvalBindingSnapshot("approval-wallet-v1", "employee-v1", "1", cutoff),
			approvalBindingSnapshot("approval-wallet-v2", "employee-v2", "2", ""),
		},
	}
	rotated.Policies[0].CatalogSHA256 = initial.Policies[0].CatalogSHA256
	rotated.Policies[0].CatalogJSON = initial.Policies[0].CatalogJSON
	if err := store.SyncPolicySnapshot(ctx, rotated); err != nil {
		t.Fatalf("sync rotated policy snapshot: %v", err)
	}
	recordHeldGrantJob(t, ctx, store, "pending-grant-retirement")
	retired := rotated
	retired.Policies = append([]policy.PolicySnapshot(nil), rotated.Policies...)
	retired.Policies[0].State = policy.PolicyStateRetired
	retired.Policies[0].SourceSHA256 = strings.Repeat("d", 64)
	retired.Policies[0].RetireAfter = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if err := store.SyncPolicySnapshot(ctx, retired); err == nil ||
		!strings.Contains(err.Error(), "unfinished approval entitlement grant") {
		t.Fatalf("retire policy with unfinished approval grant error = %v", err)
	}
}

func policySnapshot(version string, state policy.PolicyState, hashSeed string) policy.PolicySnapshot {
	catalogJSON := `{"policy_version":"` + version + `"}`
	return policy.PolicySnapshot{
		PolicyVersion: version,
		State:         state,
		CatalogSHA256: testSHA256(catalogJSON),
		SourceSHA256:  strings.Repeat(hashSeed, 64),
		CatalogJSON:   catalogJSON,
	}
}

func approvalBindingSnapshot(code, version, hashSeed, cutoff string) policy.ApprovalBindingSnapshot {
	manifestJSON := `{"approval_kind":"wallet_topup","version":"` + hashSeed + `"}`
	manifestSHA256 := testSHA256(manifestJSON)
	return policy.ApprovalBindingSnapshot{
		ApprovalCode:                code,
		SchemaFingerprint:           "sha256:" + manifestSHA256,
		Locale:                      "zh-CN",
		PolicyVersion:               version,
		ApprovalKind:                policy.ApprovalKindWalletTopUp,
		DefinitionManifestSHA256:    manifestSHA256,
		DefinitionManifestJSON:      manifestJSON,
		AcceptInstanceStartedBefore: cutoff,
	}
}

func testSHA256(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:])
}
