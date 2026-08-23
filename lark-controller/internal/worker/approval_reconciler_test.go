package worker_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

type reconciliationListerFunc func(
	context.Context,
	string,
	time.Time,
	time.Time,
	string,
	int,
) (worker.ApprovalInstancePage, error)

func (function reconciliationListerFunc) ListInstanceCodes(
	ctx context.Context,
	approvalCode string,
	windowStart time.Time,
	windowEnd time.Time,
	pageToken string,
	pageSize int,
) (worker.ApprovalInstancePage, error) {
	return function(ctx, approvalCode, windowStart, windowEnd, pageToken, pageSize)
}

type reconciliationFetcherFunc func(context.Context, string, string) (worker.ApprovalInstance, error)

func (function reconciliationFetcherFunc) Fetch(
	ctx context.Context,
	instanceCode string,
	locale string,
) (worker.ApprovalInstance, error) {
	return function(ctx, instanceCode, locale)
}

type reconciliationListCall struct {
	ApprovalCode string
	WindowStart  time.Time
	WindowEnd    time.Time
	PageToken    string
	PageSize     int
}

func TestApprovalReconcilerChunksWindowsPaginatesAndRecordsMissedApprovals(t *testing.T) {
	ctx := context.Background()
	store := openReconciliationStore(t)
	runEnd := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	scanStart := runEnd.Add(-21 * time.Hour)
	calls := make([]reconciliationListCall, 0)
	lister := reconciliationListerFunc(func(
		_ context.Context,
		approvalCode string,
		windowStart time.Time,
		windowEnd time.Time,
		pageToken string,
		pageSize int,
	) (worker.ApprovalInstancePage, error) {
		calls = append(calls, reconciliationListCall{
			ApprovalCode: approvalCode, WindowStart: windowStart, WindowEnd: windowEnd,
			PageToken: pageToken, PageSize: pageSize,
		})
		if windowEnd.Sub(windowStart) > 10*time.Hour {
			t.Fatalf("list window = %s, exceeds 10h", windowEnd.Sub(windowStart))
		}
		if windowStart.Equal(scanStart) && pageToken == "" {
			return worker.ApprovalInstancePage{
				InstanceCodes: []string{"instance-missed-1"},
				NextPageToken: "page-2",
				HasMore:       true,
			}, nil
		}
		if windowStart.Equal(scanStart) && pageToken == "page-2" {
			return worker.ApprovalInstancePage{InstanceCodes: []string{"instance-missed-2"}}, nil
		}
		return worker.ApprovalInstancePage{}, nil
	})
	fetcher := reconciliationFetcherFunc(func(
		_ context.Context,
		instanceCode string,
		locale string,
	) (worker.ApprovalInstance, error) {
		if locale != "zh-CN" {
			t.Fatalf("fetch locale = %q", locale)
		}
		return reconciledInstance("approval-wallet-v1", instanceCode, false), nil
	})
	reconciler, err := worker.NewApprovalReconciler(worker.ApprovalReconcilerConfig{
		Store: store, InstanceLister: lister, InstanceFetcher: fetcher,
		Bindings: []worker.ApprovalReconciliationBinding{{ApprovalCode: "approval-wallet-v1"}},
		AppID:    "cli_test", TenantKey: "tenant-test", Locale: "zh-CN",
		InitialLookback: 21 * time.Hour, Now: func() time.Time { return runEnd },
	})
	if err != nil {
		t.Fatalf("new approval reconciler: %v", err)
	}
	if processed, err := reconciler.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run approval reconciliation: processed=%t err=%v", processed, err)
	}
	if len(calls) != 4 || calls[0].PageToken != "" || calls[1].PageToken != "page-2" ||
		!calls[0].WindowStart.Equal(scanStart) ||
		!calls[0].WindowEnd.Equal(scanStart.Add(10*time.Hour)) ||
		!calls[2].WindowStart.Equal(scanStart.Add(10*time.Hour)) ||
		!calls[3].WindowStart.Equal(scanStart.Add(20*time.Hour)) {
		t.Fatalf("unexpected list calls: %+v", calls)
	}
	cursor, found, err := store.ApprovalReconciliationCursor(ctx, "approval-wallet-v1")
	if err != nil || !found || !cursor.Equal(runEnd) {
		t.Fatalf("reconciliation cursor: found=%t cursor=%s err=%v", found, cursor, err)
	}
	for _, instanceCode := range []string{"instance-missed-1", "instance-missed-2"} {
		event, err := store.Get(ctx, reconciledEventKey("tenant-test", "approval-wallet-v1", instanceCode, "APPROVED"))
		if err != nil {
			t.Fatalf("get reconciled event %q: %v", instanceCode, err)
		}
		if event.Origin != inbox.EventOriginApprovalReconciliation ||
			event.Status != "APPROVED" || event.InstanceCode != instanceCode ||
			event.ProcessingState != inbox.ProcessingStatePending {
			t.Fatalf("unexpected reconciled event: %+v", event)
		}
	}
}

func TestApprovalReconcilerResumesFromOverlapAfterFailedWindow(t *testing.T) {
	ctx := context.Background()
	store := openReconciliationStore(t)
	runEnd := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	initialStart := runEnd.Add(-21 * time.Hour)
	failed := true
	calls := make([]reconciliationListCall, 0)
	lister := reconciliationListerFunc(func(
		_ context.Context,
		approvalCode string,
		windowStart time.Time,
		windowEnd time.Time,
		pageToken string,
		pageSize int,
	) (worker.ApprovalInstancePage, error) {
		calls = append(calls, reconciliationListCall{
			ApprovalCode: approvalCode, WindowStart: windowStart, WindowEnd: windowEnd,
			PageToken: pageToken, PageSize: pageSize,
		})
		if failed && len(calls) == 2 {
			return worker.ApprovalInstancePage{}, &worker.ApprovalFetchError{
				Reason: worker.ApprovalFetchRateLimited, Retryable: true, RetryAfter: time.Hour,
			}
		}
		return worker.ApprovalInstancePage{}, nil
	})
	reconciler, err := worker.NewApprovalReconciler(worker.ApprovalReconcilerConfig{
		Store: store, InstanceLister: lister,
		InstanceFetcher: reconciliationFetcherFunc(func(
			context.Context, string, string,
		) (worker.ApprovalInstance, error) {
			return worker.ApprovalInstance{}, errors.New("unexpected fetch")
		}),
		Bindings: []worker.ApprovalReconciliationBinding{{ApprovalCode: "approval-wallet-v1"}},
		AppID:    "cli_test", TenantKey: "tenant-test", Locale: "zh-CN",
		InitialLookback: 21 * time.Hour, Overlap: time.Hour,
		Now: func() time.Time { return runEnd },
	})
	if err != nil {
		t.Fatalf("new approval reconciler: %v", err)
	}
	processed, runErr := reconciler.RunOnce(ctx)
	var fetchFailure *worker.ApprovalFetchError
	if !processed || !errors.As(runErr, &fetchFailure) || fetchFailure.RetryAfter != time.Hour {
		t.Fatalf("failed run: processed=%t err=%v failure=%+v", processed, runErr, fetchFailure)
	}
	firstCursor := initialStart.Add(10 * time.Hour)
	cursor, found, err := store.ApprovalReconciliationCursor(ctx, "approval-wallet-v1")
	if err != nil || !found || !cursor.Equal(firstCursor) {
		t.Fatalf("cursor after failed window: found=%t cursor=%s want=%s err=%v", found, cursor, firstCursor, err)
	}
	failed = false
	resumeCall := len(calls)
	if processed, err := reconciler.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("resume approval reconciliation: processed=%t err=%v", processed, err)
	}
	if !calls[resumeCall].WindowStart.Equal(firstCursor.Add(-time.Hour)) {
		t.Fatalf("resume start = %s, want %s", calls[resumeCall].WindowStart, firstCursor.Add(-time.Hour))
	}
	cursor, found, err = store.ApprovalReconciliationCursor(ctx, "approval-wallet-v1")
	if err != nil || !found || !cursor.Equal(runEnd) {
		t.Fatalf("cursor after resume: found=%t cursor=%s err=%v", found, cursor, err)
	}
	audit, err := store.ListApprovalReconciliationAudit(ctx)
	if err != nil || len(audit) != 4 ||
		audit[0].Result != inbox.ApprovalReconciliationResultSuccess ||
		audit[1].Result != inbox.ApprovalReconciliationResultRateLimited {
		t.Fatalf("approval reconciliation audit = %+v err=%v", audit, err)
	}
}

func TestApprovalReconcilerFailsClosedOnIncompleteOrMismatchedAuthority(t *testing.T) {
	tests := []struct {
		name    string
		lister  reconciliationListerFunc
		fetcher reconciliationFetcherFunc
		result  inbox.ApprovalReconciliationResult
	}{
		{
			name: "duplicate instance",
			lister: func(
				context.Context, string, time.Time, time.Time, string, int,
			) (worker.ApprovalInstancePage, error) {
				return worker.ApprovalInstancePage{InstanceCodes: []string{"instance-1", "instance-1"}}, nil
			},
			fetcher: func(
				context.Context, string, string,
			) (worker.ApprovalInstance, error) {
				return reconciledInstance("approval-wallet-v1", "instance-1", false), nil
			},
			result: inbox.ApprovalReconciliationResultIncompleteScan,
		},
		{
			name: "repeated page token",
			lister: func(
				_ context.Context, _ string, _ time.Time, _ time.Time, _ string, _ int,
			) (worker.ApprovalInstancePage, error) {
				return worker.ApprovalInstancePage{NextPageToken: "same-page", HasMore: true}, nil
			},
			fetcher: func(
				context.Context, string, string,
			) (worker.ApprovalInstance, error) {
				return worker.ApprovalInstance{}, errors.New("unexpected fetch")
			},
			result: inbox.ApprovalReconciliationResultIncompleteScan,
		},
		{
			name: "authority mismatch",
			lister: func(
				context.Context, string, time.Time, time.Time, string, int,
			) (worker.ApprovalInstancePage, error) {
				return worker.ApprovalInstancePage{InstanceCodes: []string{"instance-1"}}, nil
			},
			fetcher: func(
				context.Context, string, string,
			) (worker.ApprovalInstance, error) {
				return reconciledInstance("approval-other", "instance-1", false), nil
			},
			result: inbox.ApprovalReconciliationResultInvalidResponse,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := openReconciliationStore(t)
			now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
			reconciler, err := worker.NewApprovalReconciler(worker.ApprovalReconcilerConfig{
				Store: store, InstanceLister: test.lister, InstanceFetcher: test.fetcher,
				Bindings: []worker.ApprovalReconciliationBinding{{ApprovalCode: "approval-wallet-v1"}},
				AppID:    "cli_test", TenantKey: "tenant-test", Locale: "zh-CN",
				InitialLookback: time.Hour, Now: func() time.Time { return now },
			})
			if err != nil {
				t.Fatalf("new approval reconciler: %v", err)
			}
			if processed, err := reconciler.RunOnce(ctx); err == nil || !processed {
				t.Fatalf("incomplete run: processed=%t err=%v", processed, err)
			}
			if _, found, err := store.ApprovalReconciliationCursor(ctx, "approval-wallet-v1"); err != nil || found {
				t.Fatalf("failed scan advanced cursor: found=%t err=%v", found, err)
			}
			audit, err := store.ListApprovalReconciliationAudit(ctx)
			if err != nil || len(audit) != 1 || audit[0].Result != test.result {
				t.Fatalf("approval reconciliation audit = %+v err=%v", audit, err)
			}
		})
	}
}

func TestApprovalReconcilerContinuesAfterBindingFailure(t *testing.T) {
	ctx := context.Background()
	store := openReconciliationStore(t)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reconciler, err := worker.NewApprovalReconciler(worker.ApprovalReconcilerConfig{
		Store: store,
		InstanceLister: reconciliationListerFunc(func(
			_ context.Context,
			approvalCode string,
			_ time.Time,
			_ time.Time,
			_ string,
			_ int,
		) (worker.ApprovalInstancePage, error) {
			if approvalCode == "approval-a" {
				return worker.ApprovalInstancePage{}, &worker.ApprovalFetchError{
					Reason: worker.ApprovalFetchClientError,
				}
			}
			return worker.ApprovalInstancePage{InstanceCodes: []string{"instance-b"}}, nil
		}),
		InstanceFetcher: reconciliationFetcherFunc(func(
			_ context.Context,
			instanceCode string,
			_ string,
		) (worker.ApprovalInstance, error) {
			return reconciledInstance("approval-b", instanceCode, false), nil
		}),
		Bindings: []worker.ApprovalReconciliationBinding{
			{ApprovalCode: "approval-a"},
			{ApprovalCode: "approval-b"},
		},
		AppID: "cli_test", TenantKey: "tenant-test", Locale: "zh-CN",
		InitialLookback: time.Hour, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new approval reconciler: %v", err)
	}
	if processed, err := reconciler.RunOnce(ctx); err == nil || !processed {
		t.Fatalf("partially failed run: processed=%t err=%v", processed, err)
	}
	if _, found, err := store.ApprovalReconciliationCursor(ctx, "approval-a"); err != nil || found {
		t.Fatalf("failed binding cursor: found=%t err=%v", found, err)
	}
	cursor, found, err := store.ApprovalReconciliationCursor(ctx, "approval-b")
	if err != nil || !found || !cursor.Equal(now) {
		t.Fatalf("later binding cursor: found=%t cursor=%s err=%v", found, cursor, err)
	}
	if _, err := store.Get(ctx, reconciledEventKey(
		"tenant-test", "approval-b", "instance-b", "APPROVED",
	)); err != nil {
		t.Fatalf("later binding event: %v", err)
	}
}

func TestApprovalReconcilerContinuesAfterGrantRecheckFailure(t *testing.T) {
	ctx := context.Background()
	store := openReconciliationStore(t)
	processVerifiedOriginalGrant(t, ctx, store, "a")
	processVerifiedOriginalGrant(t, ctx, store, "b")
	syncReconciliationPolicy(t, ctx, store)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reconciler, err := worker.NewApprovalReconciler(worker.ApprovalReconcilerConfig{
		Store: store,
		InstanceLister: reconciliationListerFunc(func(
			context.Context, string, time.Time, time.Time, string, int,
		) (worker.ApprovalInstancePage, error) {
			return worker.ApprovalInstancePage{}, nil
		}),
		InstanceFetcher: reconciliationFetcherFunc(func(
			_ context.Context,
			instanceCode string,
			_ string,
		) (worker.ApprovalInstance, error) {
			if instanceCode == "instance-a" {
				return reconciledInstance("approval-other", instanceCode, false), nil
			}
			return reconciledInstance("approval-wallet-v1", instanceCode, true), nil
		}),
		Bindings: []worker.ApprovalReconciliationBinding{{ApprovalCode: "approval-wallet-v1"}},
		AppID:    "cli_test", TenantKey: "tenant-test", Locale: "zh-CN",
		InitialLookback: time.Hour, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new approval reconciler: %v", err)
	}
	if processed, err := reconciler.RunOnce(ctx); err == nil || !processed {
		t.Fatalf("partially failed recheck: processed=%t err=%v", processed, err)
	}
	if _, err := store.Get(ctx, reconciledEventKey(
		"tenant-test", "approval-wallet-v1", "instance-b", "REVERTED",
	)); err != nil {
		t.Fatalf("later reversal event: %v", err)
	}
	if _, err := store.Get(ctx, reconciledEventKey(
		"tenant-test", "approval-wallet-v1", "instance-a", "APPROVED",
	)); err == nil {
		t.Fatal("invalid target produced a reconciliation event")
	}
}

func TestApprovalReconcilerReplayIsDeterministicAndDrainingCursorStopsAtCutoff(t *testing.T) {
	ctx := context.Background()
	store := openReconciliationStore(t)
	runEnd := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	listCalls := 0
	lister := reconciliationListerFunc(func(
		context.Context, string, time.Time, time.Time, string, int,
	) (worker.ApprovalInstancePage, error) {
		listCalls++
		return worker.ApprovalInstancePage{InstanceCodes: []string{"instance-replay"}}, nil
	})
	fetcher := reconciliationFetcherFunc(func(
		_ context.Context, instanceCode string, _ string,
	) (worker.ApprovalInstance, error) {
		return reconciledInstance("approval-wallet-v1", instanceCode, false), nil
	})
	reconcilerFor := func(scanUntil time.Time) *worker.ApprovalReconciler {
		reconciler, err := worker.NewApprovalReconciler(worker.ApprovalReconcilerConfig{
			Store: store, InstanceLister: lister, InstanceFetcher: fetcher,
			Bindings: []worker.ApprovalReconciliationBinding{{
				ApprovalCode: "approval-wallet-v1", ScanUntil: scanUntil,
			}},
			AppID: "cli_test", TenantKey: "tenant-test", Locale: "zh-CN",
			InitialLookback: 2 * time.Hour, Now: func() time.Time { return runEnd },
		})
		if err != nil {
			t.Fatalf("new approval reconciler: %v", err)
		}
		return reconciler
	}
	if processed, err := reconcilerFor(time.Time{}).RunOnce(ctx); err != nil || !processed {
		t.Fatalf("first replay scan: processed=%t err=%v", processed, err)
	}
	runEnd = runEnd.Add(30 * time.Minute)
	if processed, err := reconcilerFor(time.Time{}).RunOnce(ctx); err != nil || !processed {
		t.Fatalf("overlap replay scan: processed=%t err=%v", processed, err)
	}
	event, err := store.Get(ctx, reconciledEventKey(
		"tenant-test", "approval-wallet-v1", "instance-replay", "APPROVED",
	))
	if err != nil || event.DuplicateCount != 1 {
		t.Fatalf("deterministic replay event = %+v err=%v", event, err)
	}
	cutoff := runEnd
	if err := store.CompleteApprovalReconciliationWindow(
		ctx, "approval-draining", cutoff.Add(-time.Hour), cutoff, 0,
	); err != nil {
		t.Fatalf("complete draining cursor: %v", err)
	}
	before := listCalls
	reconciler, err := worker.NewApprovalReconciler(worker.ApprovalReconcilerConfig{
		Store: store, InstanceLister: lister, InstanceFetcher: fetcher,
		Bindings: []worker.ApprovalReconciliationBinding{{
			ApprovalCode: "approval-draining", ScanUntil: cutoff,
		}},
		AppID: "cli_test", TenantKey: "tenant-test", Locale: "zh-CN",
		InitialLookback: 2 * time.Hour, Now: func() time.Time { return cutoff.Add(time.Hour) },
	})
	if err != nil {
		t.Fatalf("new draining reconciler: %v", err)
	}
	if processed, err := reconciler.RunOnce(ctx); err != nil || processed {
		t.Fatalf("completed draining scan: processed=%t err=%v", processed, err)
	}
	if listCalls != before {
		t.Fatalf("completed draining cursor made %d new list calls", listCalls-before)
	}
}

func TestApprovalReconcilerDetectsLateReversalAndFencesExistingGrant(t *testing.T) {
	ctx := context.Background()
	store := openReconciliationStore(t)
	externalID := processVerifiedOriginalGrant(t, ctx, store, "original")
	syncReconciliationPolicy(t, ctx, store)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reconciler, err := worker.NewApprovalReconciler(worker.ApprovalReconcilerConfig{
		Store: store,
		InstanceLister: reconciliationListerFunc(func(
			context.Context, string, time.Time, time.Time, string, int,
		) (worker.ApprovalInstancePage, error) {
			return worker.ApprovalInstancePage{}, nil
		}),
		InstanceFetcher: reconciliationFetcherFunc(func(
			_ context.Context, instanceCode string, _ string,
		) (worker.ApprovalInstance, error) {
			return reconciledInstance("approval-wallet-v1", instanceCode, true), nil
		}),
		Bindings: []worker.ApprovalReconciliationBinding{{ApprovalCode: "approval-wallet-v1"}},
		AppID:    "cli_test", TenantKey: "tenant-test", Locale: "zh-CN",
		InitialLookback: time.Hour, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new approval reconciler: %v", err)
	}
	if processed, err := reconciler.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("recheck late reversal: processed=%t err=%v", processed, err)
	}
	reversalKey := reconciledEventKey(
		"tenant-test", "approval-wallet-v1", "instance-original", "REVERTED",
	)
	event, err := store.Get(ctx, reversalKey)
	if err != nil || event.Origin != inbox.EventOriginApprovalReconciliation ||
		event.RevertedInstanceCode != "instance-original" {
		t.Fatalf("late reversal event = %+v err=%v", event, err)
	}
	processor, err := worker.NewShadowProcessor(
		store,
		&approvalFetcher{instance: reconciledInstance("approval-wallet-v1", "instance-original", true)},
		&approvalResolver{resolution: verifiedWalletResolution()},
		"zh-CN",
	)
	if err != nil {
		t.Fatalf("new reversal processor: %v", err)
	}
	if processed, err := processor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("process reconciled reversal: processed=%t err=%v", processed, err)
	}
	grant, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil || grant.Status != inbox.EntitlementGrantJobStatusReversalPending {
		t.Fatalf("fenced grant = %+v err=%v", grant, err)
	}
	reversal, err := store.GetApprovalReversal(ctx, reversalKey)
	if err != nil || reversal.Result != inbox.ApprovalReversalResultGrantFenced {
		t.Fatalf("reconciled reversal = %+v err=%v", reversal, err)
	}
}

func TestApprovalReconcilerAuditsInvalidExistingGrantRecheck(t *testing.T) {
	ctx := context.Background()
	store := openReconciliationStore(t)
	processVerifiedOriginalGrant(t, ctx, store, "original")
	syncReconciliationPolicy(t, ctx, store)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reconciler, err := worker.NewApprovalReconciler(worker.ApprovalReconcilerConfig{
		Store: store,
		InstanceLister: reconciliationListerFunc(func(
			context.Context, string, time.Time, time.Time, string, int,
		) (worker.ApprovalInstancePage, error) {
			return worker.ApprovalInstancePage{}, nil
		}),
		InstanceFetcher: reconciliationFetcherFunc(func(
			_ context.Context, instanceCode string, _ string,
		) (worker.ApprovalInstance, error) {
			return reconciledInstance("approval-other", instanceCode, false), nil
		}),
		Bindings: []worker.ApprovalReconciliationBinding{{ApprovalCode: "approval-wallet-v1"}},
		AppID:    "cli_test", TenantKey: "tenant-test", Locale: "zh-CN",
		InitialLookback: time.Hour, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("new approval reconciler: %v", err)
	}
	if processed, err := reconciler.RunOnce(ctx); err == nil || !processed {
		t.Fatalf("invalid grant recheck: processed=%t err=%v", processed, err)
	}
	audit, err := store.ListApprovalReconciliationAudit(ctx)
	if err != nil || len(audit) != 2 ||
		audit[1].Result != inbox.ApprovalReconciliationResultInvalidResponse ||
		!audit[1].WindowStart.Equal(now) || !audit[1].WindowEnd.Equal(now) {
		t.Fatalf("invalid recheck audit = %+v err=%v", audit, err)
	}
}

func openReconciliationStore(t *testing.T) *inbox.Store {
	t.Helper()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open reconciliation store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func reconciledInstance(approvalCode string, instanceCode string, reverted bool) worker.ApprovalInstance {
	return worker.ApprovalInstance{
		ApprovalCode: approvalCode,
		InstanceCode: instanceCode,
		Status:       "APPROVED",
		OpenID:       "ou_requester",
		StartTime:    "1787270300000",
		FormJSON:     `[{"custom_id":"wallet_package","value":"Small"}]`,
		Reverted:     reverted,
	}
}

func reconciledEventKey(tenantKey string, approvalCode string, instanceCode string, status string) string {
	digest := sha256.Sum256([]byte(strings.Join(
		[]string{tenantKey, approvalCode, instanceCode, status},
		"\x00",
	)))
	return "lark:reconcile:" + hex.EncodeToString(digest[:])
}

func syncReconciliationPolicy(t *testing.T, ctx context.Context, store *inbox.Store) {
	t.Helper()
	catalogJSON := `{"policy_version":"employee-v1"}`
	catalogDigest := sha256.Sum256([]byte(catalogJSON))
	manifestJSON := `{"approval_kind":"wallet_topup"}`
	manifestDigest := sha256.Sum256([]byte(manifestJSON))
	manifestSHA256 := hex.EncodeToString(manifestDigest[:])
	err := store.SyncPolicySnapshot(ctx, policy.Snapshot{
		Policies: []policy.PolicySnapshot{{
			PolicyVersion: "employee-v1",
			State:         policy.PolicyStateActive,
			CatalogSHA256: hex.EncodeToString(catalogDigest[:]),
			SourceSHA256:  strings.Repeat("a", 64),
			CatalogJSON:   catalogJSON,
		}},
		Bindings: []policy.ApprovalBindingSnapshot{{
			ApprovalCode:             "approval-wallet-v1",
			SchemaFingerprint:        "sha256:" + manifestSHA256,
			Locale:                   "zh-CN",
			PolicyVersion:            "employee-v1",
			ApprovalKind:             policy.ApprovalKindWalletTopUp,
			DefinitionManifestSHA256: manifestSHA256,
			DefinitionManifestJSON:   manifestJSON,
		}},
	})
	if err != nil {
		t.Fatalf("sync reconciliation policy: %v", err)
	}
}

func TestRequestPacerSerializesConcurrentCallers(t *testing.T) {
	pacer, err := worker.NewRequestPacer(15 * time.Millisecond)
	if err != nil {
		t.Fatalf("new request pacer: %v", err)
	}
	times := make(chan time.Time, 3)
	start := make(chan struct{})
	for index := 0; index < 3; index++ {
		go func() {
			<-start
			if err := pacer.Wait(context.Background()); err != nil {
				times <- time.Time{}
				return
			}
			times <- time.Now()
		}()
	}
	close(start)
	observed := []time.Time{<-times, <-times, <-times}
	sort.Slice(observed, func(left int, right int) bool {
		return observed[left].Before(observed[right])
	})
	for _, timestamp := range observed {
		if timestamp.IsZero() {
			t.Fatal("paced caller failed")
		}
	}
	for index := 1; index < len(observed); index++ {
		if gap := observed[index].Sub(observed[index-1]); gap < 15*time.Millisecond {
			t.Fatalf("paced caller gap = %s, want at least 15ms", gap)
		}
	}
	if _, err := worker.NewRequestPacer(-time.Second); err == nil {
		t.Fatal("negative request pacer interval was accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pacer.Wait(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pacer wait error = %v", err)
	}
}

func TestApprovalReconcilerConstructorRejectsDuplicateBindings(t *testing.T) {
	store := openReconciliationStore(t)
	_, err := worker.NewApprovalReconciler(worker.ApprovalReconcilerConfig{
		Store: store,
		InstanceLister: reconciliationListerFunc(func(
			context.Context, string, time.Time, time.Time, string, int,
		) (worker.ApprovalInstancePage, error) {
			return worker.ApprovalInstancePage{}, nil
		}),
		InstanceFetcher: reconciliationFetcherFunc(func(
			context.Context, string, string,
		) (worker.ApprovalInstance, error) {
			return worker.ApprovalInstance{}, nil
		}),
		Bindings: []worker.ApprovalReconciliationBinding{
			{ApprovalCode: "duplicate"},
			{ApprovalCode: "duplicate"},
		},
		AppID: "cli_test", TenantKey: "tenant-test", Locale: "zh-CN",
	})
	if err == nil || !strings.Contains(err.Error(), "unique valid codes") {
		t.Fatalf("duplicate binding error = %v", err)
	}
}
