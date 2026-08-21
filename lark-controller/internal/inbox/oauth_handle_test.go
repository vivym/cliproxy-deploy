package inbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

func TestOAuthAuthorizationStateIsBoundSingleUseAndExpires(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	store.now = func() time.Time { return now }

	rawState, err := store.CreateOAuthAuthorizationState(ctx, OAuthAuthorizationState{
		NewAPIState: "new-api-state-1",
		RedirectURI: "https://ai.x2r.store/oauth/lark",
	})
	if err != nil {
		t.Fatalf("create OAuth authorization state: %v", err)
	}
	if len(rawState) != 43 {
		t.Fatalf("OAuth state length = %d, want 43 base64url characters", len(rawState))
	}
	assertOAuthCredentialStoredAsHash(t, store, "oauth_states", "state_hash", rawState)
	if err := store.Close(); err != nil {
		t.Fatalf("close store before restart: %v", err)
	}
	store, err = Open(databasePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	store.now = func() time.Time { return now }
	consumed, err := store.ConsumeOAuthAuthorizationState(ctx, rawState)
	if err != nil {
		t.Fatalf("consume OAuth authorization state: %v", err)
	}
	if consumed.NewAPIState != "new-api-state-1" ||
		consumed.RedirectURI != "https://ai.x2r.store/oauth/lark" {
		t.Fatalf("consumed OAuth state = %+v", consumed)
	}
	if _, err := store.ConsumeOAuthAuthorizationState(ctx, rawState); !errors.Is(err, ErrOAuthCredentialInvalid) {
		t.Fatalf("reused OAuth state error = %v", err)
	}
	if _, err := store.ConsumeOAuthAuthorizationState(ctx, tamperOAuthCredential(rawState)); !errors.Is(err, ErrOAuthCredentialInvalid) {
		t.Fatalf("tampered OAuth state error = %v", err)
	}

	expiredState, err := store.CreateOAuthAuthorizationState(ctx, OAuthAuthorizationState{
		NewAPIState: "new-api-state-expired",
		RedirectURI: "https://ai.x2r.store/oauth/lark",
	})
	if err != nil {
		t.Fatalf("create expiring OAuth state: %v", err)
	}
	now = now.Add(oauthAuthorizationStateTTL + time.Nanosecond)
	if _, err := store.ConsumeOAuthAuthorizationState(ctx, expiredState); !errors.Is(err, ErrOAuthCredentialInvalid) {
		t.Fatalf("expired OAuth state error = %v", err)
	}
}

func TestOAuthLoginCodeAndAccessHandleAreAtomicSingleUse(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, time.August, 21, 9, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	identity := OAuthIdentity{
		Subject:  "tenant-test:ou_employee",
		Username: "lark_te7ozrid4egv6gj",
		Name:     "Employee",
	}

	loginCode, err := store.CreateOAuthLoginCode(ctx, identity)
	if err != nil {
		t.Fatalf("create OAuth login code: %v", err)
	}
	assertOAuthCredentialStoredAsHash(t, store, "oauth_login_codes", "code_hash", loginCode)
	var wait sync.WaitGroup
	wait.Add(2)
	type exchangeResult struct {
		handle string
		err    error
	}
	results := make(chan exchangeResult, 2)
	for range 2 {
		go func() {
			defer wait.Done()
			handle, exchangeErr := store.ExchangeOAuthLoginCode(ctx, loginCode)
			results <- exchangeResult{handle: handle, err: exchangeErr}
		}()
	}
	wait.Wait()
	close(results)
	var accessHandle string
	invalidCount := 0
	for result := range results {
		switch {
		case result.err == nil:
			accessHandle = result.handle
		case errors.Is(result.err, ErrOAuthCredentialInvalid):
			invalidCount++
		default:
			t.Fatalf("exchange OAuth login code: %v", result.err)
		}
	}
	if accessHandle == "" || invalidCount != 1 {
		t.Fatalf("concurrent exchange result: handle=%t invalid=%d", accessHandle != "", invalidCount)
	}
	assertOAuthCredentialStoredAsHash(t, store, "oauth_access_handles", "handle_hash", accessHandle)
	consumed, err := store.ConsumeOAuthAccessHandle(ctx, accessHandle)
	if err != nil {
		t.Fatalf("consume OAuth access handle: %v", err)
	}
	if consumed != identity {
		t.Fatalf("consumed OAuth identity = %+v, want %+v", consumed, identity)
	}
	if _, err := store.ConsumeOAuthAccessHandle(ctx, accessHandle); !errors.Is(err, ErrOAuthCredentialInvalid) {
		t.Fatalf("reused OAuth access handle error = %v", err)
	}

	expiringCode, err := store.CreateOAuthLoginCode(ctx, identity)
	if err != nil {
		t.Fatalf("create expiring OAuth login code: %v", err)
	}
	now = now.Add(oauthLoginCodeTTL + time.Nanosecond)
	if _, err := store.ExchangeOAuthLoginCode(ctx, expiringCode); !errors.Is(err, ErrOAuthCredentialInvalid) {
		t.Fatalf("expired OAuth login code error = %v", err)
	}

	loginCode, err = store.CreateOAuthLoginCode(ctx, identity)
	if err != nil {
		t.Fatalf("create OAuth login code for expiring handle: %v", err)
	}
	accessHandle, err = store.ExchangeOAuthLoginCode(ctx, loginCode)
	if err != nil {
		t.Fatalf("exchange OAuth login code for expiring handle: %v", err)
	}
	now = now.Add(oauthAccessHandleTTL + time.Nanosecond)
	if _, err := store.ConsumeOAuthAccessHandle(ctx, accessHandle); !errors.Is(err, ErrOAuthCredentialInvalid) {
		t.Fatalf("expired OAuth access handle error = %v", err)
	}
}

func TestConsumeOAuthAccessHandleStoresBaseSubscriptionGrantAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("new OAuth identity: %v", err)
	}
	accessHandle := testOAuthAccessHandle(t, ctx, store, identity)
	sealer, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant sealer: %v", err)
	}
	planner := func(got OAuthIdentity) (BaseSubscriptionGrantDraft, error) {
		if got != identity {
			t.Fatalf("planned identity = %+v, want %+v", got, identity)
		}
		return testBaseSubscriptionGrantDraft(t, got, 5_000_000, strings.Repeat("a", 64), sealer), nil
	}

	consumed, err := store.ConsumeOAuthAccessHandleAndStoreBaseGrant(ctx, accessHandle, planner)
	if err != nil {
		t.Fatalf("consume handle and store base grant: %v", err)
	}
	if consumed != identity {
		t.Fatalf("consumed identity = %+v, want %+v", consumed, identity)
	}
	externalID := "lark:base:tenant-test:ou_employee:employee-v1"
	job, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get base subscription grant job: %v", err)
	}
	if job.Status != EntitlementGrantJobStatusHeldShadow || job.Attempts != 0 {
		t.Fatalf("base subscription job = %+v, want held_shadow", job)
	}
	opened, err := sealer.Open(newapi.SealedGrantRequest{
		KeyID: job.KeyID, ExternalID: job.ExternalID, RequestSHA256: job.RequestSHA256,
		Nonce: job.Nonce, Ciphertext: job.Ciphertext,
	})
	if err != nil {
		t.Fatalf("open base subscription grant: %v", err)
	}
	if opened.Source != "base_login" || opened.Identity.Subject != identity.Subject ||
		opened.Grant.Type != "subscription_level" || opened.Grant.LevelCode != "basic" ||
		!opened.Grant.MinimumRankOnly || opened.Evidence != nil {
		t.Fatalf("opened base subscription grant = %+v", opened)
	}
	if _, err := store.ConsumeOAuthAccessHandleAndStoreBaseGrant(ctx, accessHandle, planner); !errors.Is(err, ErrOAuthCredentialInvalid) {
		t.Fatalf("reused access handle error = %v, want invalid credential", err)
	}
}

func TestConsumeOAuthAccessHandleRejectsUnboundBaseGrantWithoutConsumingHandle(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("new OAuth identity: %v", err)
	}
	accessHandle := testOAuthAccessHandle(t, ctx, store, identity)
	sealer, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant sealer: %v", err)
	}
	invalidPlanner := func(got OAuthIdentity) (BaseSubscriptionGrantDraft, error) {
		draft := testBaseSubscriptionGrantDraft(t, got, 5_000_000, strings.Repeat("a", 64), sealer)
		draft.SubjectSHA256 = strings.Repeat("b", 64)
		draft.GrantJob.SubjectSHA256 = draft.SubjectSHA256
		return draft, nil
	}
	if _, err := store.ConsumeOAuthAccessHandleAndStoreBaseGrant(ctx, accessHandle, invalidPlanner); err == nil ||
		!strings.Contains(err.Error(), "invalid base subscription grant") {
		t.Fatalf("unbound subject hash error = %v, want invalid base grant", err)
	}
	validPlanner := func(got OAuthIdentity) (BaseSubscriptionGrantDraft, error) {
		return testBaseSubscriptionGrantDraft(t, got, 5_000_000, strings.Repeat("a", 64), sealer), nil
	}
	if _, err := store.ConsumeOAuthAccessHandleAndStoreBaseGrant(ctx, accessHandle, validPlanner); err != nil {
		t.Fatalf("retry valid base grant with preserved handle: %v", err)
	}
}

func TestConsumeOAuthAccessHandleRejectsBaseGrantMetadataDriftOnReplay(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("new OAuth identity: %v", err)
	}
	sealer, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant sealer: %v", err)
	}
	validPlanner := func(got OAuthIdentity) (BaseSubscriptionGrantDraft, error) {
		return testBaseSubscriptionGrantDraft(t, got, 5_000_000, strings.Repeat("a", 64), sealer), nil
	}
	if _, err := store.ConsumeOAuthAccessHandleAndStoreBaseGrant(
		ctx,
		testOAuthAccessHandle(t, ctx, store, identity),
		validPlanner,
	); err != nil {
		t.Fatalf("store original base grant: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*BaseSubscriptionGrantDraft)
	}{
		{name: "catalog hash", mutate: func(draft *BaseSubscriptionGrantDraft) {
			draft.CatalogSHA256 = strings.Repeat("b", 64)
		}},
		{name: "monthly quota", mutate: func(draft *BaseSubscriptionGrantDraft) {
			draft.MonthlyQuota = 6_000_000
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accessHandle := testOAuthAccessHandle(t, ctx, store, identity)
			driftedPlanner := func(got OAuthIdentity) (BaseSubscriptionGrantDraft, error) {
				draft := testBaseSubscriptionGrantDraft(t, got, 5_000_000, strings.Repeat("a", 64), sealer)
				test.mutate(&draft)
				return draft, nil
			}
			if _, err := store.ConsumeOAuthAccessHandleAndStoreBaseGrant(
				ctx,
				accessHandle,
				driftedPlanner,
			); !errors.Is(err, ErrEntitlementCommandPayloadMismatch) {
				t.Fatalf("metadata drift error = %v, want payload mismatch", err)
			}
			if _, err := store.ConsumeOAuthAccessHandleAndStoreBaseGrant(
				ctx,
				accessHandle,
				validPlanner,
			); err != nil {
				t.Fatalf("retry stable replay with preserved handle: %v", err)
			}
		})
	}
}

func testOAuthAccessHandle(
	t *testing.T,
	ctx context.Context,
	store *Store,
	identity OAuthIdentity,
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
	return accessHandle
}

func testBaseSubscriptionGrantDraft(
	t *testing.T,
	identity OAuthIdentity,
	monthlyQuota int64,
	catalogSHA256 string,
	sealer *newapi.GrantSealer,
) BaseSubscriptionGrantDraft {
	t.Helper()
	request, receipt, err := newapi.PlanBaseSubscriptionGrant(newapi.BaseSubscriptionGrantInput{
		Subject: identity.Subject, PolicyVersion: "employee-v1", LevelCode: "basic",
		MonthlyQuota: monthlyQuota, CatalogSHA256: catalogSHA256,
	})
	if err != nil {
		t.Fatalf("plan base subscription grant: %v", err)
	}
	sealed, err := sealer.Seal(request)
	if err != nil {
		t.Fatalf("seal base subscription grant: %v", err)
	}
	return BaseSubscriptionGrantDraft{
		ExternalID: receipt.ExternalID, RequestSHA256: receipt.RequestSHA256,
		SubjectSHA256: receipt.SubjectSHA256, PolicyVersion: receipt.PolicyVersion,
		CatalogSHA256: receipt.CatalogSHA256, LevelCode: receipt.BusinessCode,
		MonthlyQuota: receipt.MonthlyQuota,
		GrantJob: EntitlementGrantJobDraft{
			ExternalID: sealed.ExternalID, RequestSHA256: sealed.RequestSHA256,
			SubjectSHA256: receipt.SubjectSHA256, KeyID: sealed.KeyID,
			Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		},
	}
}

func TestOAuthCredentialCreationPrunesConsumedAndExpiredRows(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, time.August, 21, 9, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	identity, err := NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("new OAuth identity: %v", err)
	}

	consumedState, err := store.CreateOAuthAuthorizationState(ctx, OAuthAuthorizationState{
		NewAPIState: "consumed-state", RedirectURI: "https://ai.x2r.store/oauth/lark",
	})
	if err != nil {
		t.Fatalf("create consumed state: %v", err)
	}
	if _, err := store.ConsumeOAuthAuthorizationState(ctx, consumedState); err != nil {
		t.Fatalf("consume state: %v", err)
	}
	if _, err := store.CreateOAuthAuthorizationState(ctx, OAuthAuthorizationState{
		NewAPIState: "expiring-state", RedirectURI: "https://ai.x2r.store/oauth/lark",
	}); err != nil {
		t.Fatalf("create expiring state: %v", err)
	}
	loginCode, err := store.CreateOAuthLoginCode(ctx, identity)
	if err != nil {
		t.Fatalf("create login code: %v", err)
	}
	accessHandle, err := store.ExchangeOAuthLoginCode(ctx, loginCode)
	if err != nil {
		t.Fatalf("exchange login code: %v", err)
	}
	if _, err := store.ConsumeOAuthAccessHandle(ctx, accessHandle); err != nil {
		t.Fatalf("consume access handle: %v", err)
	}

	now = now.Add(oauthAuthorizationStateTTL + time.Nanosecond)
	if _, err := store.CreateOAuthAuthorizationState(ctx, OAuthAuthorizationState{
		NewAPIState: "retained-state", RedirectURI: "https://ai.x2r.store/oauth/lark",
	}); err != nil {
		t.Fatalf("create state after retention window: %v", err)
	}
	for table, want := range map[string]int{
		"oauth_states": 1, "oauth_login_codes": 0, "oauth_access_handles": 0,
	} {
		var count int
		if err := store.database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != want {
			t.Fatalf("%s rows=%d, want %d after credential pruning", table, count, want)
		}
	}
}

func TestOAuthCredentialCapacityKeepsCredentialStagesIndependent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("new OAuth identity: %v", err)
	}

	store.oauthMu.Lock()
	store.oauthCounts[oauthCredentialState] = maxOutstandingOAuthStates
	store.oauthMu.Unlock()
	if _, err := store.CreateOAuthAuthorizationState(ctx, OAuthAuthorizationState{
		NewAPIState: "new-api-state", RedirectURI: "https://ai.x2r.store/oauth/lark",
	}); !errors.Is(err, ErrOAuthCredentialCapacity) {
		t.Fatalf("state capacity error=%v, want ErrOAuthCredentialCapacity", err)
	}
	loginCode, err := store.CreateOAuthLoginCode(ctx, identity)
	if err != nil {
		t.Fatalf("state capacity blocked downstream login code: %v", err)
	}

	store.oauthMu.Lock()
	store.oauthCounts[oauthCredentialAccessHandle] = maxOutstandingOAuthAccessHandles
	store.oauthMu.Unlock()
	if _, err := store.ExchangeOAuthLoginCode(ctx, loginCode); !errors.Is(err, ErrOAuthCredentialCapacity) {
		t.Fatalf("access-handle capacity error=%v, want ErrOAuthCredentialCapacity", err)
	}
	store.oauthMu.Lock()
	store.oauthCounts[oauthCredentialAccessHandle] = 0
	store.oauthMu.Unlock()
	if _, err := store.ExchangeOAuthLoginCode(ctx, loginCode); err != nil {
		t.Fatalf("capacity rejection consumed login code: %v", err)
	}
}

func TestOAuthExpiryPruningUsesEachExpiryIndex(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	indexes := map[string]string{
		"oauth_states":         "idx_oauth_states_expiry",
		"oauth_login_codes":    "idx_oauth_login_codes_expiry",
		"oauth_access_handles": "idx_oauth_access_handles_expiry",
	}
	for table, index := range indexes {
		rows, err := store.database.Query(
			"EXPLAIN QUERY PLAN DELETE FROM "+table+" WHERE expires_at <= ?",
			time.Now().UnixNano(),
		)
		if err != nil {
			t.Fatalf("explain %s expiry pruning: %v", table, err)
		}
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				_ = rows.Close()
				t.Fatalf("scan %s query plan: %v", table, err)
			}
			details = append(details, detail)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close %s query plan: %v", table, err)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s query plan: %v", table, err)
		}
		if !strings.Contains(strings.Join(details, " "), index) {
			t.Fatalf("%s expiry plan=%q, want index %s", table, details, index)
		}
	}
}

func TestOAuthIdentityUsesTenantOpenIDAndDeterministicUsername(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	username, err := OAuthUsername("tenant-test:ou_employee")
	if err != nil {
		t.Fatalf("derive OAuth username: %v", err)
	}
	if username != "lark_te7ozrid4egv6gj" {
		t.Fatalf("OAuth username = %q", username)
	}
	validIdentity, err := NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("normalize valid OAuth identity: %v", err)
	}
	if _, err := store.CreateOAuthLoginCode(ctx, validIdentity); err != nil {
		t.Fatalf("create login code for valid identity: %v", err)
	}
	truncated, err := NewOAuthIdentity(
		"tenant-test:ou_employee",
		"一二三四五六七八九十一二三四五六七八九十末",
	)
	if err != nil || truncated.Name != "一二三四五六七八九十一二三四五六七八九十" {
		t.Fatalf("normalized Unicode display name = %+v, error = %v", truncated, err)
	}
	if _, err := NewOAuthIdentity("tenant-test:ou_employee", "Employee\nInjected"); err == nil {
		t.Fatal("OAuth identity constructor accepted a display name with a newline")
	}

	for name, identity := range map[string]OAuthIdentity{
		"email subject": {
			Subject: "employee@example.com", Username: username, Name: "Employee",
		},
		"missing tenant": {
			Subject: ":ou_employee", Username: username, Name: "Employee",
		},
		"multiple separators": {
			Subject: "tenant:test:ou_employee", Username: username, Name: "Employee",
		},
		"mismatched username": {
			Subject: "tenant-test:ou_employee", Username: "lark_abcdefghijklmn2", Name: "Employee",
		},
		"oversized display name": {
			Subject: "tenant-test:ou_employee", Username: username, Name: "123456789012345678901",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.CreateOAuthLoginCode(ctx, identity); err == nil {
				t.Fatal("invalid OAuth identity was accepted")
			}
		})
	}
}

func tamperOAuthCredential(raw string) string {
	last := byte('A')
	if raw[len(raw)-1] == last {
		last = 'B'
	}
	return raw[:len(raw)-1] + string(last)
}

func assertOAuthCredentialStoredAsHash(
	t *testing.T,
	store *Store,
	table string,
	column string,
	raw string,
) {
	t.Helper()
	var stored []byte
	query := "SELECT " + column + " FROM " + table + " ORDER BY created_at DESC LIMIT 1"
	if err := store.database.QueryRow(query).Scan(&stored); err != nil {
		t.Fatalf("read stored OAuth credential digest: %v", err)
	}
	want := sha256.Sum256([]byte(raw))
	if !bytes.Equal(stored, want[:]) || bytes.Equal(stored, []byte(raw)) {
		t.Fatal("OAuth credential was not stored as its SHA-256 digest")
	}
}
