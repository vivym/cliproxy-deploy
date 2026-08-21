package inbox_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

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
	if err := store.ValidateEntitlementGrantJobKeyID(ctx, sealer.KeyID()); err != nil {
		t.Fatalf("validate original grant payload key: %v", err)
	}
	replacement, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x24}, 32))
	if err != nil {
		t.Fatalf("new replacement grant sealer: %v", err)
	}
	err = store.ValidateEntitlementGrantJobKeyID(ctx, replacement.KeyID())
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
	if err := store.ValidateEntitlementGrantJobKeyID(
		context.Background(),
		strings.Repeat("a", 64),
	); err != nil {
		t.Fatalf("validate initial grant payload key: %v", err)
	}
}
