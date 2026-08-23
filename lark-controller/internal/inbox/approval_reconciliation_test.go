package inbox_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
)

func TestApprovalReconciliationCursorAdvancesOnlyWithSuccessfulWindow(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	start := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	firstEnd := start.Add(10 * time.Hour)
	if err := store.SyncPolicySnapshot(ctx, policy.Snapshot{
		Policies: []policy.PolicySnapshot{
			policySnapshot("employee-v1", policy.PolicyStateActive, "a"),
			policySnapshot("employee-v0", policy.PolicyStateDraining, "b"),
		},
		Bindings: []policy.ApprovalBindingSnapshot{
			approvalBindingSnapshot("approval-wallet-v1", "employee-v1", "1", ""),
			approvalBindingSnapshot("approval-wallet-missing", "employee-v1", "2", ""),
			approvalBindingSnapshot(
				"approval-wallet-draining",
				"employee-v0",
				"3",
				firstEnd.Format(time.RFC3339),
			),
		},
	}); err != nil {
		t.Fatalf("sync reconciliation metric policies: %v", err)
	}

	if cursor, found, err := store.ApprovalReconciliationCursor(ctx, "approval-wallet-v1"); err != nil || found || !cursor.IsZero() {
		t.Fatalf("initial cursor: found=%t cursor=%s err=%v", found, cursor, err)
	}
	if err := store.FailApprovalReconciliationWindow(
		ctx,
		"approval-wallet-v1",
		start,
		firstEnd,
		inbox.ApprovalReconciliationResultRateLimited,
		2,
	); err != nil {
		t.Fatalf("record failed reconciliation: %v", err)
	}
	if _, found, err := store.ApprovalReconciliationCursor(ctx, "approval-wallet-v1"); err != nil || found {
		t.Fatalf("failed window advanced cursor: found=%t err=%v", found, err)
	}
	if err := store.CompleteApprovalReconciliationWindow(
		ctx, "approval-wallet-v1", start, firstEnd, 3,
	); err != nil {
		t.Fatalf("complete first reconciliation window: %v", err)
	}
	overlapStart := firstEnd.Add(-time.Hour)
	overlapEnd := firstEnd.Add(-30 * time.Minute)
	if err := store.CompleteApprovalReconciliationWindow(
		ctx, "approval-wallet-v1", overlapStart, overlapEnd, 1,
	); err != nil {
		t.Fatalf("complete overlap reconciliation window: %v", err)
	}
	cursor, found, err := store.ApprovalReconciliationCursor(ctx, "approval-wallet-v1")
	if err != nil || !found || !cursor.Equal(firstEnd) {
		t.Fatalf("monotonic cursor: found=%t cursor=%s want=%s err=%v", found, cursor, firstEnd, err)
	}
	audit, err := store.ListApprovalReconciliationAudit(ctx)
	if err != nil {
		t.Fatalf("list approval reconciliation audit: %v", err)
	}
	if len(audit) != 3 ||
		audit[0].Result != inbox.ApprovalReconciliationResultRateLimited ||
		audit[0].InstanceCount != 2 ||
		audit[1].Result != inbox.ApprovalReconciliationResultSuccess ||
		audit[1].InstanceCount != 3 ||
		audit[2].Result != inbox.ApprovalReconciliationResultSuccess {
		t.Fatalf("approval reconciliation audit = %+v", audit)
	}
	if err := store.CompleteApprovalReconciliationWindow(
		ctx,
		"approval-wallet-draining",
		start,
		firstEnd,
		0,
	); err != nil {
		t.Fatalf("complete draining reconciliation cursor: %v", err)
	}
	if _, err := store.Record(ctx, inbox.Event{
		Key: "lark:reconcile:internal", Origin: inbox.EventOriginApprovalReconciliation,
		SchemaVersion: "2.0", EventID: "internal", EventType: "approval.instance.status_changed_v4",
		AppID: "cli_test", TenantKey: "tenant-test", ApprovalCode: "approval-wallet-v1",
		InstanceCode: "instance-internal", Status: "APPROVED", PayloadJSON: `{"status":"APPROVED"}`,
	}); err != nil {
		t.Fatalf("record reconciliation-origin event: %v", err)
	}
	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read reconciliation operational snapshot: %v", err)
	}
	if len(snapshot.WebhookReceived) != 0 || len(snapshot.WebhookDuplicates) != 0 {
		t.Fatalf("reconciliation event leaked into webhook metrics: received=%v duplicates=%v",
			snapshot.WebhookReceived, snapshot.WebhookDuplicates)
	}
	if snapshot.ApprovalReconciliations["success"] != 3 ||
		snapshot.ApprovalReconciliations["rate_limited"] != 1 {
		t.Fatalf("approval reconciliation metrics = %v", snapshot.ApprovalReconciliations)
	}
	if snapshot.ApprovalCursorInitialized["approval-wallet-v1"] != 1 ||
		snapshot.ApprovalCursorInitialized["approval-wallet-missing"] != 0 ||
		snapshot.ApprovalCursorInitialized["approval-wallet-draining"] != 1 {
		t.Fatalf("approval cursor initialization metrics = %v", snapshot.ApprovalCursorInitialized)
	}
	if _, exists := snapshot.ApprovalCursorLagSeconds["approval-wallet-v1"]; !exists ||
		snapshot.ApprovalCursorLagSeconds["approval-wallet-draining"] != 0 ||
		snapshot.ApprovalCursorLagSeconds["approval-wallet-missing"] != 0 {
		t.Fatalf("approval cursor lag metrics = %v", snapshot.ApprovalCursorLagSeconds)
	}
}

func TestApprovalReconciliationProjectionAndRecheckTargetsAreTenantScoped(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncPolicySnapshot(ctx, policy.Snapshot{
		Policies: []policy.PolicySnapshot{
			policySnapshot("employee-v1", policy.PolicyStateActive, "a"),
		},
		Bindings: []policy.ApprovalBindingSnapshot{
			approvalBindingSnapshot("approval-wallet-v1", "employee-v1", "1", ""),
		},
	}); err != nil {
		t.Fatalf("sync policy snapshot: %v", err)
	}
	recordVerifiedCommand(
		t,
		ctx,
		store,
		"recheck-original",
		"instance-original",
		"lark:wallet-topup:instance-original",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"employee-v1",
	)

	projected, err := store.HasApprovalAuthorityProjection(
		ctx, "tenant-test", "approval-wallet-v1", "instance-original", false,
	)
	if err != nil || !projected {
		t.Fatalf("approved authority projection: projected=%t err=%v", projected, err)
	}
	projected, err = store.HasApprovalAuthorityProjection(
		ctx, "tenant-other", "approval-wallet-v1", "instance-original", false,
	)
	if err != nil || projected {
		t.Fatalf("cross-tenant authority projection: projected=%t err=%v", projected, err)
	}
	targets, err := store.ListApprovalRecheckTargets(ctx, "tenant-test")
	if err != nil || len(targets) != 1 ||
		targets[0].ApprovalCode != "approval-wallet-v1" ||
		targets[0].InstanceCode != "instance-original" {
		t.Fatalf("approval recheck targets = %+v err=%v", targets, err)
	}
	if targets, err := store.ListApprovalRecheckTargets(ctx, "tenant-other"); err != nil || len(targets) != 0 {
		t.Fatalf("cross-tenant recheck targets = %+v err=%v", targets, err)
	}

	completeVerifiedReversal(t, ctx, store, "recheck-reversal", "instance-original")
	projected, err = store.HasApprovalAuthorityProjection(
		ctx, "tenant-test", "approval-wallet-v1", "instance-original", true,
	)
	if err != nil || !projected {
		t.Fatalf("reversal authority projection: projected=%t err=%v", projected, err)
	}
	if targets, err := store.ListApprovalRecheckTargets(ctx, "tenant-test"); err != nil || len(targets) != 0 {
		t.Fatalf("reverted approval remained a recheck target: targets=%+v err=%v", targets, err)
	}
}

func TestApprovalReconciliationRejectsUnboundedAuditValues(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.FailApprovalReconciliationWindow(
		ctx,
		"approval-wallet-v1",
		now,
		now,
		"raw upstream error text",
		0,
	); err == nil {
		t.Fatal("unbounded approval reconciliation result was accepted")
	}
	audit, err := store.ListApprovalReconciliationAudit(ctx)
	if err != nil || len(audit) != 0 {
		t.Fatalf("rejected reconciliation left audit=%+v err=%v", audit, err)
	}
	invalidOriginEvents := []inbox.Event{
		{
			Key: "lark:v2:internal", Origin: inbox.EventOriginApprovalReconciliation,
			SchemaVersion: "2.0", EventID: "internal", EventType: "approval.instance.status_changed_v4",
			AppID: "cli_test", TenantKey: "tenant-test", PayloadJSON: `{}`,
		},
		{
			Key: "lark:reconcile:webhook", Origin: inbox.EventOriginWebhook,
			SchemaVersion: "2.0", EventID: "webhook", EventType: "approval.instance.status_changed_v4",
			AppID: "cli_test", TenantKey: "tenant-test", PayloadJSON: `{}`,
		},
	}
	for _, event := range invalidOriginEvents {
		if _, err := store.Record(ctx, event); err == nil {
			t.Fatalf("accepted mismatched event origin: %+v", event)
		}
	}
}
