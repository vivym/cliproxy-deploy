package worker_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

func TestActiveGrantRuntimeReleasesBacklogAndNewHeldJobs(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant keyring: %v", err)
	}
	firstExternalID := prepareHeldGrantJob(t, ctx, store, "evt-active-backlog", keyring)
	client := grantClientFunc(func(
		_ context.Context,
		request newapi.EntitlementGrantRequest,
	) (newapi.EntitlementGrantResponse, error) {
		return newapi.EntitlementGrantResponse{
			Status: "applied", ExternalID: request.ExternalID, UserID: 42,
			Result: newapi.GrantResult{
				GrantType: request.Grant.Type, QuotaDelta: request.Grant.QuotaDelta,
			},
		}, nil
	})
	executor, err := worker.NewGrantExecutor(store, client, keyring)
	if err != nil {
		t.Fatalf("new grant executor: %v", err)
	}
	runtime, err := worker.NewActiveGrantRuntime(store, executor, "employee-v1")
	if err != nil {
		t.Fatalf("new active grant runtime: %v", err)
	}
	released, err := runtime.ReleaseHeldJobs(ctx)
	if err != nil || released != 1 {
		t.Fatalf("activate grant backlog: released=%d err=%v", released, err)
	}
	first, err := store.GetEntitlementGrantJob(ctx, firstExternalID)
	if err != nil {
		t.Fatalf("get activated backlog grant: %v", err)
	}
	if first.Status != inbox.EntitlementGrantJobStatusPending || first.ActivatedAt.IsZero() {
		t.Fatalf("backlog grant was not activated: %+v", first)
	}
	if processed, err := runtime.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("execute backlog grant: processed=%t err=%v", processed, err)
	}
	assertGrantJobSucceeded(t, ctx, store, firstExternalID)

	secondExternalID := prepareHeldGrantJob(t, ctx, store, "evt-active-new", keyring)
	second, err := store.GetEntitlementGrantJob(ctx, secondExternalID)
	if err != nil {
		t.Fatalf("get new held grant: %v", err)
	}
	if second.Status != inbox.EntitlementGrantJobStatusHeldShadow || !second.ActivatedAt.IsZero() {
		t.Fatalf("new grant bypassed active release: %+v", second)
	}
	if processed, err := runtime.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("activate and execute new grant: processed=%t err=%v", processed, err)
	}
	assertGrantJobSucceeded(t, ctx, store, secondExternalID)
}

func TestActiveGrantRuntimeRejectsPolicySwitchWithUnfinishedHistoricalBaseJob(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant keyring: %v", err)
	}
	identity, err := inbox.NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("new OAuth identity: %v", err)
	}
	historicalExternalID := prepareHeldBaseGrantJob(
		t,
		ctx,
		store,
		identity,
		"employee-v1",
		keyring,
	)
	client := grantClientFunc(func(
		context.Context,
		newapi.EntitlementGrantRequest,
	) (newapi.EntitlementGrantResponse, error) {
		return newapi.EntitlementGrantResponse{}, nil
	})
	executor, err := worker.NewGrantExecutor(store, client, keyring)
	if err != nil {
		t.Fatalf("new grant executor: %v", err)
	}
	historicalRuntime, err := worker.NewActiveGrantRuntime(store, executor, "employee-v1")
	if err != nil {
		t.Fatalf("new historical policy runtime: %v", err)
	}
	if released, err := historicalRuntime.ReleaseHeldJobs(ctx); err != nil || released != 1 {
		t.Fatalf("release historical base job before policy switch: released=%d err=%v", released, err)
	}
	activeExternalID := prepareHeldBaseGrantJob(
		t,
		ctx,
		store,
		identity,
		"employee-v2",
		keyring,
	)
	runtime, err := worker.NewActiveGrantRuntime(store, executor, "employee-v2")
	if err != nil {
		t.Fatalf("new active grant runtime: %v", err)
	}
	if released, err := runtime.ReleaseHeldJobs(ctx); err == nil || released != 0 ||
		!strings.Contains(err.Error(), "non-active policy") {
		t.Fatalf("policy switch release gate: released=%d err=%v", released, err)
	}
	historical, err := store.GetEntitlementGrantJob(ctx, historicalExternalID)
	if err != nil {
		t.Fatalf("get historical base job: %v", err)
	}
	if historical.Status != inbox.EntitlementGrantJobStatusPending || historical.ActivatedAt.IsZero() {
		t.Fatalf("historical base job changed during rejected policy switch: %+v", historical)
	}
	active, err := store.GetEntitlementGrantJob(ctx, activeExternalID)
	if err != nil {
		t.Fatalf("get active base job: %v", err)
	}
	if active.Status != inbox.EntitlementGrantJobStatusHeldShadow || !active.ActivatedAt.IsZero() {
		t.Fatalf("new-policy base job released despite failed switch gate: %+v", active)
	}
}

func prepareHeldGrantJob(
	t *testing.T,
	ctx context.Context,
	store *inbox.Store,
	eventID string,
	sealer worker.GrantRequestSealer,
) string {
	t.Helper()
	recordApprovedEvent(t, ctx, store, eventID)
	processor, err := worker.NewShadowProcessorWithGrantSealer(
		store,
		&approvalFetcher{},
		&approvalResolver{resolution: verifiedWalletResolution()},
		"zh-CN",
		sealer,
	)
	if err != nil {
		t.Fatalf("new shadow processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("prepare held grant: processed=%t err=%v", processed, err)
	}
	return "lark:wallet-topup:instance-" + eventID
}

func prepareHeldBaseGrantJob(
	t *testing.T,
	ctx context.Context,
	store *inbox.Store,
	identity inbox.OAuthIdentity,
	policyVersion string,
	sealer worker.GrantRequestSealer,
) string {
	t.Helper()
	loginCode, err := store.CreateOAuthLoginCode(ctx, identity)
	if err != nil {
		t.Fatalf("create OAuth login code: %v", err)
	}
	accessHandle, err := store.ExchangeOAuthLoginCode(ctx, loginCode)
	if err != nil {
		t.Fatalf("exchange OAuth login code: %v", err)
	}
	var externalID string
	_, err = store.ConsumeOAuthAccessHandleAndStoreBaseGrant(
		ctx,
		accessHandle,
		func(got inbox.OAuthIdentity) (inbox.BaseSubscriptionGrantDraft, error) {
			request, receipt, err := newapi.PlanBaseSubscriptionGrant(newapi.BaseSubscriptionGrantInput{
				Subject: got.Subject, PolicyVersion: policyVersion, LevelCode: "basic",
				PeriodQuota: 5_000_000, ResetPeriod: "weekly", ResetTimezone: "Asia/Shanghai",
				CatalogSHA256: strings.Repeat("a", 64),
			})
			if err != nil {
				return inbox.BaseSubscriptionGrantDraft{}, err
			}
			sealed, err := sealer.Seal(request)
			if err != nil {
				return inbox.BaseSubscriptionGrantDraft{}, err
			}
			externalID = receipt.ExternalID
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
		t.Fatalf("store held base grant job: %v", err)
	}
	return externalID
}

func assertGrantJobSucceeded(
	t *testing.T,
	ctx context.Context,
	store *inbox.Store,
	externalID string,
) {
	t.Helper()
	job, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get completed grant: %v", err)
	}
	if job.Status != inbox.EntitlementGrantJobStatusSucceeded || job.Receipt == nil {
		t.Fatalf("grant did not succeed: %+v", job)
	}
}
