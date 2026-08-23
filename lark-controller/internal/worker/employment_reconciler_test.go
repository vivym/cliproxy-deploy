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
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

func TestEmploymentReconcilerCreatesHeldDisableForExplicitResignation(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	checkedAt := time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)
	lister := &principalListerStub{pages: []newapi.PrincipalPage{{
		Principals: []newapi.Principal{{
			ProviderSlug: "lark", Subject: "tenant-test:ou_resigned",
			PrincipalVersion: 4, UpdatedAt: "2026-08-20T00:00:00Z",
		}},
		ScanComplete: true,
	}}}
	checker := &employmentCheckerStub{results: map[string]worker.EmploymentCheckResult{
		"ou_health":   {Status: worker.EmploymentStatusPresent, LarkResultCode: 0},
		"ou_resigned": {Status: worker.EmploymentStatusResigned, LarkResultCode: 0},
	}}
	reconciler, err := worker.NewEmploymentReconciler(worker.EmploymentReconcilerConfig{
		Store: store, PrincipalLister: lister, EmploymentChecker: checker,
		PrincipalDisableSealer: keyring, TenantKey: "tenant-test", HealthOpenID: "ou_health",
		Now: func() time.Time { return checkedAt },
	})
	if err != nil {
		t.Fatalf("new employment reconciler: %v", err)
	}
	processed, err := reconciler.RunOnce(ctx)
	if err != nil || !processed {
		t.Fatalf("run employment reconciliation: processed=%t err=%v", processed, err)
	}
	job, err := store.GetPrincipalDisableJob(
		ctx,
		"lark:disable-reconcile:tenant-test:ou_resigned:2026-08-23",
	)
	if err != nil {
		t.Fatalf("get reconciliation disable job: %v", err)
	}
	if job.Status != inbox.PrincipalDisableJobStatusHeldShadow || job.EventKey != "" {
		t.Fatalf("reconciliation job = %+v, want eventless held_shadow", job)
	}
	request, err := keyring.OpenPrincipalDisable(newapi.SealedPrincipalDisableRequest{
		KeyID: job.KeyID, ExternalID: job.ExternalID, RequestSHA256: job.RequestSHA256,
		Nonce: job.Nonce, Ciphertext: job.Ciphertext,
	})
	if err != nil {
		t.Fatalf("open reconciliation disable request: %v", err)
	}
	if request.Source != "employment_reconciliation" ||
		request.Identity.Subject != "tenant-test:ou_resigned" ||
		request.Reason != "lark_employment_resigned" {
		t.Fatalf("reconciliation disable request = %+v", request)
	}
}

func TestEmploymentReconcilerRequiresTwoHealthyNotFoundScansTwentyFourHoursApart(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	firstCheck := time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)
	newReconciler := func(checkedAt time.Time) *worker.EmploymentReconciler {
		t.Helper()
		reconciler, err := worker.NewEmploymentReconciler(worker.EmploymentReconcilerConfig{
			Store: store,
			PrincipalLister: &principalListerStub{pages: []newapi.PrincipalPage{{
				Principals: []newapi.Principal{{
					ProviderSlug: "lark", Subject: "tenant-test:ou_missing",
					PrincipalVersion: 4, UpdatedAt: "2026-08-20T00:00:00Z",
				}},
				ScanComplete: true,
			}}},
			EmploymentChecker: &employmentCheckerStub{results: map[string]worker.EmploymentCheckResult{
				"ou_health":  {Status: worker.EmploymentStatusPresent, LarkResultCode: 0},
				"ou_missing": {Status: worker.EmploymentStatusNotFound, LarkResultCode: 41012},
			}},
			PrincipalDisableSealer: keyring,
			TenantKey:              "tenant-test", HealthOpenID: "ou_health",
			Now: func() time.Time { return checkedAt },
		})
		if err != nil {
			t.Fatalf("new employment reconciler: %v", err)
		}
		return reconciler
	}
	if processed, err := newReconciler(firstCheck).RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run first missing scan: processed=%t err=%v", processed, err)
	}
	externalID := "lark:disable-reconcile:tenant-test:ou_missing:2026-08-24"
	if _, err := store.GetPrincipalDisableJob(ctx, externalID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("first missing scan created disable job: %v", err)
	}
	if processed, err := newReconciler(firstCheck.Add(24 * time.Hour)).RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run second missing scan: processed=%t err=%v", processed, err)
	}
	job, err := store.GetPrincipalDisableJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get confirmed missing disable job: %v", err)
	}
	request, err := keyring.OpenPrincipalDisable(newapi.SealedPrincipalDisableRequest{
		KeyID: job.KeyID, ExternalID: job.ExternalID, RequestSHA256: job.RequestSHA256,
		Nonce: job.Nonce, Ciphertext: job.Ciphertext,
	})
	if err != nil {
		t.Fatalf("open confirmed missing disable request: %v", err)
	}
	if request.Reason != "lark_employment_not_found_confirmed" {
		t.Fatalf("confirmed missing reason = %q", request.Reason)
	}
}

func TestEmploymentReconcilerUsesActualLookupTimeForMissingInterval(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	principal := newapi.Principal{
		ProviderSlug: "lark", Subject: "tenant-test:ou_missing",
		PrincipalVersion: 4, UpdatedAt: "2026-08-20T00:00:00Z",
	}
	newReconciler := func(times ...time.Time) *worker.EmploymentReconciler {
		t.Helper()
		nextTime := 0
		now := func() time.Time {
			if nextTime >= len(times) {
				t.Fatal("employment reconciler requested an unexpected clock value")
			}
			value := times[nextTime]
			nextTime++
			return value
		}
		reconciler, err := worker.NewEmploymentReconciler(worker.EmploymentReconcilerConfig{
			Store: store,
			PrincipalLister: &principalListerStub{pages: []newapi.PrincipalPage{{
				Principals: []newapi.Principal{principal}, ScanComplete: true,
			}}},
			EmploymentChecker: &employmentCheckerStub{results: map[string]worker.EmploymentCheckResult{
				"ou_health":  {Status: worker.EmploymentStatusPresent},
				"ou_missing": {Status: worker.EmploymentStatusNotFound, LarkResultCode: 41012},
			}},
			PrincipalDisableSealer: keyring, TenantKey: "tenant-test", HealthOpenID: "ou_health",
			Now: now,
		})
		if err != nil {
			t.Fatalf("new employment reconciler: %v", err)
		}
		return reconciler
	}
	firstStarted := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	firstLookup := firstStarted.Add(25 * time.Hour)
	if processed, err := newReconciler(
		firstStarted,
		firstLookup,
		firstLookup.Add(time.Minute),
	).RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run long first scan: processed=%t err=%v", processed, err)
	}
	secondStarted := firstLookup.Add(2 * time.Minute)
	secondLookup := secondStarted.Add(time.Minute)
	if processed, err := newReconciler(
		secondStarted,
		secondLookup,
		secondLookup.Add(time.Minute),
	).RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run restarted second scan: processed=%t err=%v", processed, err)
	}
	if _, err := store.GetPrincipalDisableJob(
		ctx,
		"lark:disable-reconcile:tenant-test:ou_missing:2026-08-21",
	); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("lookups less than 24 hours apart created disable job: %v", err)
	}
}

type principalListerStub struct {
	pages  []newapi.PrincipalPage
	errors map[int]error
	calls  int
}

func (s *principalListerStub) ListActiveLarkPrincipals(
	context.Context,
	string,
	int,
) (newapi.PrincipalPage, error) {
	if err := s.errors[s.calls]; err != nil {
		s.calls++
		return newapi.PrincipalPage{}, err
	}
	if s.calls >= len(s.pages) {
		return newapi.PrincipalPage{}, nil
	}
	page := s.pages[s.calls]
	s.calls++
	return page, nil
}

type employmentCheckerStub struct {
	results  map[string]worker.EmploymentCheckResult
	errors   map[string]error
	calledAt []time.Time
}

func (s *employmentCheckerStub) CheckEmployment(
	_ context.Context,
	openID string,
) (worker.EmploymentCheckResult, error) {
	s.calledAt = append(s.calledAt, time.Now())
	if err := s.errors[openID]; err != nil {
		return worker.EmploymentCheckResult{}, err
	}
	return s.results[openID], nil
}

func TestEmploymentReconcilerSpacesLarkChecks(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	checker := &employmentCheckerStub{results: map[string]worker.EmploymentCheckResult{
		"ou_health":   {Status: worker.EmploymentStatusPresent},
		"ou_employee": {Status: worker.EmploymentStatusPresent},
	}}
	const minimumInterval = 20 * time.Millisecond
	reconciler, err := worker.NewEmploymentReconciler(worker.EmploymentReconcilerConfig{
		Store: store,
		PrincipalLister: &principalListerStub{pages: []newapi.PrincipalPage{{
			Principals: []newapi.Principal{{
				ProviderSlug: "lark", Subject: "tenant-test:ou_employee",
				PrincipalVersion: 4, UpdatedAt: "2026-08-20T00:00:00Z",
			}},
			ScanComplete: true,
		}}},
		EmploymentChecker: checker, PrincipalDisableSealer: keyring,
		TenantKey: "tenant-test", HealthOpenID: "ou_health",
		MinimumCheckInterval: minimumInterval,
		Now: func() time.Time {
			return time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new employment reconciler: %v", err)
	}
	if processed, err := reconciler.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run paced employment reconciliation: processed=%t err=%v", processed, err)
	}
	if len(checker.calledAt) != 3 {
		t.Fatalf("employment checker calls = %d, want pre-probe, user, post-probe", len(checker.calledAt))
	}
	for index := 1; index < len(checker.calledAt); index++ {
		if gap := checker.calledAt[index].Sub(checker.calledAt[index-1]); gap < minimumInterval {
			t.Fatalf("employment checker call gap = %s, want at least %s", gap, minimumInterval)
		}
	}
}

func TestEmploymentReconcilerRecordsHealthFailureWithoutCreatingEvidence(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	reconciler, err := worker.NewEmploymentReconciler(worker.EmploymentReconcilerConfig{
		Store: store,
		PrincipalLister: &principalListerStub{pages: []newapi.PrincipalPage{{
			Principals: []newapi.Principal{{
				ProviderSlug: "lark", Subject: "tenant-test:ou_resigned",
				PrincipalVersion: 4, UpdatedAt: "2026-08-20T00:00:00Z",
			}},
			ScanComplete: true,
		}}},
		EmploymentChecker: &employmentCheckerStub{errors: map[string]error{
			"ou_health": &worker.EmploymentCheckError{
				Reason: worker.EmploymentCheckPermissionDenied,
			},
		}},
		PrincipalDisableSealer: keyring,
		TenantKey:              "tenant-test", HealthOpenID: "ou_health",
		Now: func() time.Time {
			return time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new employment reconciler: %v", err)
	}
	if processed, err := reconciler.RunOnce(ctx); err == nil || !processed {
		t.Fatalf("health failure: processed=%t err=%v", processed, err)
	}
	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read operational snapshot: %v", err)
	}
	if snapshot.EmploymentReconciliations["health_probe_failed"] != 1 {
		t.Fatalf("employment reconciliation results = %+v", snapshot.EmploymentReconciliations)
	}
	if _, err := store.GetPrincipalDisableJob(
		ctx,
		"lark:disable-reconcile:tenant-test:ou_resigned:2026-08-23",
	); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failed health probe created a disable job: %v", err)
	}
}

func TestEmploymentReconcilerIncompleteScanDoesNotAdvanceMissingEvidence(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	firstCheck := time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)
	newReconciler := func(checkedAt time.Time, lister *principalListerStub) *worker.EmploymentReconciler {
		t.Helper()
		reconciler, err := worker.NewEmploymentReconciler(worker.EmploymentReconcilerConfig{
			Store: store, PrincipalLister: lister,
			EmploymentChecker: &employmentCheckerStub{results: map[string]worker.EmploymentCheckResult{
				"ou_health":  {Status: worker.EmploymentStatusPresent},
				"ou_missing": {Status: worker.EmploymentStatusNotFound, LarkResultCode: 41012},
			}},
			PrincipalDisableSealer: keyring, TenantKey: "tenant-test", HealthOpenID: "ou_health",
			Now: func() time.Time { return checkedAt },
		})
		if err != nil {
			t.Fatalf("new employment reconciler: %v", err)
		}
		return reconciler
	}
	principal := newapi.Principal{
		ProviderSlug: "lark", Subject: "tenant-test:ou_missing",
		PrincipalVersion: 4, UpdatedAt: "2026-08-20T00:00:00Z",
	}
	if processed, err := newReconciler(firstCheck, &principalListerStub{pages: []newapi.PrincipalPage{{
		Principals: []newapi.Principal{principal}, ScanComplete: true,
	}}}).RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run first missing scan: processed=%t err=%v", processed, err)
	}
	failedLister := &principalListerStub{
		pages: []newapi.PrincipalPage{{
			Principals: []newapi.Principal{principal}, NextCursor: "page-2", ScanComplete: false,
		}},
		errors: map[int]error{1: errors.New("temporary page failure")},
	}
	if processed, err := newReconciler(firstCheck.Add(24*time.Hour), failedLister).RunOnce(ctx); err == nil || !processed {
		t.Fatalf("run incomplete missing scan: processed=%t err=%v", processed, err)
	}
	externalID := "lark:disable-reconcile:tenant-test:ou_missing:2026-08-24"
	if _, err := store.GetPrincipalDisableJob(ctx, externalID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("incomplete scan created disable job: %v", err)
	}
	checks, err := store.ListEmploymentChecks(ctx, "2026-08-24")
	if err != nil {
		t.Fatalf("list partial employment checks: %v", err)
	}
	if len(checks) != 1 || checks[0].Result != "not_found" || checks[0].PermissionHealthy {
		t.Fatalf("partial employment checks = %+v", checks)
	}
}

func TestEmploymentReconcilerPresentClearsPriorMissingEvidence(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	firstCheck := time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)
	run := func(checkedAt time.Time, status worker.EmploymentStatus, code int) {
		t.Helper()
		reconciler, err := worker.NewEmploymentReconciler(worker.EmploymentReconcilerConfig{
			Store: store,
			PrincipalLister: &principalListerStub{pages: []newapi.PrincipalPage{{
				Principals: []newapi.Principal{{
					ProviderSlug: "lark", Subject: "tenant-test:ou_employee",
					PrincipalVersion: 4, UpdatedAt: "2026-08-20T00:00:00Z",
				}},
				ScanComplete: true,
			}}},
			EmploymentChecker: &employmentCheckerStub{results: map[string]worker.EmploymentCheckResult{
				"ou_health":   {Status: worker.EmploymentStatusPresent},
				"ou_employee": {Status: status, LarkResultCode: code},
			}},
			PrincipalDisableSealer: keyring, TenantKey: "tenant-test", HealthOpenID: "ou_health",
			Now: func() time.Time { return checkedAt },
		})
		if err != nil {
			t.Fatalf("new employment reconciler: %v", err)
		}
		if processed, err := reconciler.RunOnce(ctx); err != nil || !processed {
			t.Fatalf("run employment reconciliation: processed=%t err=%v", processed, err)
		}
	}
	run(firstCheck, worker.EmploymentStatusNotFound, 41012)
	run(firstCheck.Add(24*time.Hour), worker.EmploymentStatusPresent, 0)
	run(firstCheck.Add(48*time.Hour), worker.EmploymentStatusNotFound, 41012)
	externalID := "lark:disable-reconcile:tenant-test:ou_employee:2026-08-22"
	if _, err := store.GetPrincipalDisableJob(ctx, externalID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("first missing result after a present scan created disable job: %v", err)
	}
	run(firstCheck.Add(72*time.Hour), worker.EmploymentStatusNotFound, 41012)
	if _, err := store.GetPrincipalDisableJob(
		ctx,
		"lark:disable-reconcile:tenant-test:ou_employee:2026-08-23",
	); err != nil {
		t.Fatalf("second consecutive missing result did not create disable job: %v", err)
	}
}

func TestEmploymentReconcilerPresentInFailedScanClearsPriorMissingEvidence(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	firstCheck := time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC)
	principal := newapi.Principal{
		ProviderSlug: "lark", Subject: "tenant-test:ou_employee",
		PrincipalVersion: 4, UpdatedAt: "2026-08-20T00:00:00Z",
	}
	newReconciler := func(
		checkedAt time.Time,
		status worker.EmploymentStatus,
		code int,
		lister *principalListerStub,
	) *worker.EmploymentReconciler {
		t.Helper()
		reconciler, err := worker.NewEmploymentReconciler(worker.EmploymentReconcilerConfig{
			Store: store, PrincipalLister: lister,
			EmploymentChecker: &employmentCheckerStub{results: map[string]worker.EmploymentCheckResult{
				"ou_health":   {Status: worker.EmploymentStatusPresent},
				"ou_employee": {Status: status, LarkResultCode: code},
			}},
			PrincipalDisableSealer: keyring, TenantKey: "tenant-test", HealthOpenID: "ou_health",
			Now: func() time.Time { return checkedAt },
		})
		if err != nil {
			t.Fatalf("new employment reconciler: %v", err)
		}
		return reconciler
	}
	completeLister := func() *principalListerStub {
		return &principalListerStub{pages: []newapi.PrincipalPage{{
			Principals: []newapi.Principal{principal}, ScanComplete: true,
		}}}
	}
	if processed, err := newReconciler(
		firstCheck, worker.EmploymentStatusNotFound, 41012, completeLister(),
	).RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run first missing scan: processed=%t err=%v", processed, err)
	}
	failedLister := &principalListerStub{
		pages: []newapi.PrincipalPage{{
			Principals: []newapi.Principal{principal}, NextCursor: "page-2",
		}},
		errors: map[int]error{1: errors.New("temporary page failure")},
	}
	if processed, err := newReconciler(
		firstCheck.Add(24*time.Hour), worker.EmploymentStatusPresent, 0, failedLister,
	).RunOnce(ctx); err == nil || !processed {
		t.Fatalf("run failed present scan: processed=%t err=%v", processed, err)
	}
	if processed, err := newReconciler(
		firstCheck.Add(48*time.Hour), worker.EmploymentStatusNotFound, 41012, completeLister(),
	).RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run missing scan after present: processed=%t err=%v", processed, err)
	}
	if _, err := store.GetPrincipalDisableJob(
		ctx,
		"lark:disable-reconcile:tenant-test:ou_employee:2026-08-22",
	); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("present result in failed scan did not clear missing evidence: %v", err)
	}
	if processed, err := newReconciler(
		firstCheck.Add(72*time.Hour), worker.EmploymentStatusNotFound, 41012, completeLister(),
	).RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run second consecutive missing scan: processed=%t err=%v", processed, err)
	}
	if _, err := store.GetPrincipalDisableJob(
		ctx,
		"lark:disable-reconcile:tenant-test:ou_employee:2026-08-23",
	); err != nil {
		t.Fatalf("second consecutive missing scan did not create disable job: %v", err)
	}
}

func TestEmploymentReconcilerPreservesFailureAndSuccessAuditOnSameDay(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	checkedAt := time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC)
	newReconciler := func(checker *employmentCheckerStub) *worker.EmploymentReconciler {
		t.Helper()
		reconciler, err := worker.NewEmploymentReconciler(worker.EmploymentReconcilerConfig{
			Store:             store,
			PrincipalLister:   &principalListerStub{pages: []newapi.PrincipalPage{{ScanComplete: true}}},
			EmploymentChecker: checker, PrincipalDisableSealer: keyring,
			TenantKey: "tenant-test", HealthOpenID: "ou_health", Now: func() time.Time { return checkedAt },
		})
		if err != nil {
			t.Fatalf("new employment reconciler: %v", err)
		}
		return reconciler
	}
	failingChecker := &employmentCheckerStub{errors: map[string]error{
		"ou_health": &worker.EmploymentCheckError{Reason: worker.EmploymentCheckPermissionDenied},
	}}
	if processed, err := newReconciler(failingChecker).RunOnce(ctx); err == nil || !processed {
		t.Fatalf("run failed reconciliation: processed=%t err=%v", processed, err)
	}
	successfulChecker := &employmentCheckerStub{results: map[string]worker.EmploymentCheckResult{
		"ou_health": {Status: worker.EmploymentStatusPresent},
	}}
	if processed, err := newReconciler(successfulChecker).RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run successful retry: processed=%t err=%v", processed, err)
	}
	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read operational snapshot: %v", err)
	}
	if snapshot.EmploymentReconciliations["health_probe_failed"] != 1 ||
		snapshot.EmploymentReconciliations["success"] != 1 {
		t.Fatalf("employment reconciliation audit = %+v", snapshot.EmploymentReconciliations)
	}
}

func TestEmploymentReconcilerFailsClosedOnRepeatedSubjectAcrossPages(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	principal := newapi.Principal{
		ProviderSlug: "lark", Subject: "tenant-test:ou_resigned",
		PrincipalVersion: 4, UpdatedAt: "2026-08-20T00:00:00Z",
	}
	reconciler, err := worker.NewEmploymentReconciler(worker.EmploymentReconcilerConfig{
		Store: store,
		PrincipalLister: &principalListerStub{pages: []newapi.PrincipalPage{
			{Principals: []newapi.Principal{principal}, NextCursor: "page-2"},
			{Principals: []newapi.Principal{principal}, ScanComplete: true},
		}},
		EmploymentChecker: &employmentCheckerStub{results: map[string]worker.EmploymentCheckResult{
			"ou_health":   {Status: worker.EmploymentStatusPresent},
			"ou_resigned": {Status: worker.EmploymentStatusResigned},
		}},
		PrincipalDisableSealer: keyring, TenantKey: "tenant-test", HealthOpenID: "ou_health",
		Now: func() time.Time { return time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("new employment reconciler: %v", err)
	}
	if processed, err := reconciler.RunOnce(ctx); err == nil || !processed {
		t.Fatalf("run duplicate-subject reconciliation: processed=%t err=%v", processed, err)
	}
	if _, err := store.GetPrincipalDisableJob(
		ctx,
		"lark:disable-reconcile:tenant-test:ou_resigned:2026-08-23",
	); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("duplicate-subject scan created disable job: %v", err)
	}
	checks, err := store.ListEmploymentChecks(ctx, "2026-08-23")
	if err != nil {
		t.Fatalf("list duplicate-subject checks: %v", err)
	}
	if len(checks) != 1 || checks[0].PermissionHealthy || checks[0].Result != "resigned" {
		t.Fatalf("duplicate-subject partial checks = %+v", checks)
	}
	snapshot, err := store.OperationalSnapshot(ctx)
	if err != nil {
		t.Fatalf("read operational snapshot: %v", err)
	}
	if snapshot.EmploymentReconciliations["incomplete_scan"] != 1 {
		t.Fatalf("employment reconciliation audit = %+v", snapshot.EmploymentReconciliations)
	}
}
