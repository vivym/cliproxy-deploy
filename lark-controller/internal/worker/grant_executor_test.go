package worker_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

type grantClient struct {
	calls    int
	request  newapi.EntitlementGrantRequest
	response newapi.EntitlementGrantResponse
	err      error
}

type grantClientFunc func(
	context.Context,
	newapi.EntitlementGrantRequest,
) (newapi.EntitlementGrantResponse, error)

func (function grantClientFunc) Grant(
	ctx context.Context,
	request newapi.EntitlementGrantRequest,
) (newapi.EntitlementGrantResponse, error) {
	return function(ctx, request)
}

func (c *grantClient) Grant(
	_ context.Context,
	request newapi.EntitlementGrantRequest,
) (newapi.EntitlementGrantResponse, error) {
	c.calls++
	c.request = request
	return c.response, c.err
}

func TestGrantExecutorRejectsTypedNilDependencies(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant keyring: %v", err)
	}
	var nilClient *grantClient
	var nilKeyring *newapi.GrantKeyring
	for name, dependencies := range map[string]struct {
		client worker.GrantClient
		opener worker.GrantRequestOpener
	}{
		"grant client":   {client: nilClient, opener: keyring},
		"payload opener": {client: &grantClient{}, opener: nilKeyring},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := worker.NewGrantExecutor(
				store,
				dependencies.client,
				dependencies.opener,
			); err == nil {
				t.Fatalf("accepted typed-nil %s", name)
			}
		})
	}
}

func TestGrantExecutorAppliesReleasedJobAndStoresReceipt(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sealer := prepareReleasedGrantJob(t, ctx, store, "evt-execute-grant")
	externalID := "lark:wallet-topup:instance-evt-execute-grant"
	client := &grantClient{response: newapi.EntitlementGrantResponse{
		Status: "applied", ExternalID: externalID, UserID: 42,
		Result: newapi.GrantResult{GrantType: "wallet_quota", QuotaDelta: 2_500_000},
	}}
	executor, err := worker.NewGrantExecutor(store, client, sealer)
	if err != nil {
		t.Fatalf("new grant executor: %v", err)
	}
	processed, err := executor.RunOnce(ctx)
	if err != nil || !processed {
		t.Fatalf("execute grant: processed=%t err=%v", processed, err)
	}
	if client.calls != 1 || client.request.ExternalID != externalID ||
		client.request.Identity.Subject != "tenant-test:ou_requester" ||
		client.request.Grant.Type != "wallet_quota" || client.request.Grant.QuotaDelta != 2_500_000 {
		t.Fatalf("unexpected grant client call: calls=%d request=%+v", client.calls, client.request)
	}
	stored, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get executed grant job: %v", err)
	}
	if stored.Status != inbox.EntitlementGrantJobStatusSucceeded || stored.Receipt == nil ||
		stored.Receipt.Status != "applied" || stored.Receipt.UserID != 42 {
		t.Fatalf("unexpected executed grant job: %+v", stored)
	}
	if processed, err := executor.RunOnce(ctx); err != nil || processed {
		t.Fatalf("execute completed grant again: processed=%t err=%v", processed, err)
	}
}

func TestGrantExecutorOpensPreviousKeyAfterRotation(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_ = prepareReleasedGrantJob(t, ctx, store, "evt-execute-rotated-grant")
	externalID := "lark:wallet-topup:instance-evt-execute-rotated-grant"
	keyring, err := newapi.NewGrantKeyring(
		bytes.Repeat([]byte{0x24}, 32),
		bytes.Repeat([]byte{0x42}, 32),
	)
	if err != nil {
		t.Fatalf("new rotated grant keyring: %v", err)
	}
	executor, err := worker.NewGrantExecutor(store, &grantClient{
		response: newapi.EntitlementGrantResponse{
			Status: "applied", ExternalID: externalID, UserID: 42,
			Result: newapi.GrantResult{GrantType: "wallet_quota", QuotaDelta: 2_500_000},
		},
	}, keyring)
	if err != nil {
		t.Fatalf("new grant executor with rotated keyring: %v", err)
	}
	if processed, err := executor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("execute grant sealed by previous key: processed=%t err=%v", processed, err)
	}
	stored, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get rotated grant execution: %v", err)
	}
	if stored.Status != inbox.EntitlementGrantJobStatusSucceeded {
		t.Fatalf("rotated grant execution = %+v", stored)
	}
}

func TestGrantExecutorRetriesPrincipalNotReady(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sealer := prepareReleasedGrantJob(t, ctx, store, "evt-principal-not-ready")
	client := &grantClient{err: &newapi.APIError{
		StatusCode: 404, Code: "principal_not_ready", Retryable: true,
	}}
	executor, err := worker.NewGrantExecutor(
		store,
		client,
		sealer,
		worker.WithGrantRetryPolicy(worker.GrantRetryPolicy{
			Schedule:                []time.Duration{10 * time.Millisecond},
			PrincipalNotReadyMaxAge: 24 * time.Hour,
		}),
	)
	if err != nil {
		t.Fatalf("new grant executor: %v", err)
	}
	if processed, err := executor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("execute unavailable grant: processed=%t err=%v", processed, err)
	}
	stored, err := store.GetEntitlementGrantJob(
		ctx,
		"lark:wallet-topup:instance-evt-principal-not-ready",
	)
	if err != nil {
		t.Fatalf("get retrying grant: %v", err)
	}
	if stored.Status != inbox.EntitlementGrantJobStatusRetryWait ||
		stored.LastError != string(inbox.EntitlementGrantFailurePrincipalNotReady) ||
		stored.Attempts != 1 {
		t.Fatalf("unexpected retrying grant: %+v", stored)
	}
	time.Sleep(15 * time.Millisecond)
	if processed, err := executor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("execute principal retry beyond initial schedule: processed=%t err=%v", processed, err)
	}
	stored, err = store.GetEntitlementGrantJob(
		ctx,
		"lark:wallet-topup:instance-evt-principal-not-ready",
	)
	if err != nil {
		t.Fatalf("get continued principal retry: %v", err)
	}
	if stored.Status != inbox.EntitlementGrantJobStatusRetryWait ||
		stored.LastError != string(inbox.EntitlementGrantFailurePrincipalNotReady) ||
		stored.Attempts != 2 {
		t.Fatalf("principal retry stopped after initial schedule: %+v", stored)
	}
}

func TestGrantExecutorDeadLettersOnlyStableTerminalReasons(t *testing.T) {
	tests := []struct {
		name       string
		errorValue error
		wantReason inbox.EntitlementGrantFailureReason
	}{
		{
			name: "API error",
			errorValue: &newapi.APIError{
				StatusCode: 401, Code: "integration_unauthorized", Retryable: false,
			},
			wantReason: inbox.EntitlementGrantFailureIntegrationUnauthorized,
		},
		{
			name: "invalid response",
			errorValue: &newapi.RequestError{
				Reason: "invalid_response", Retryable: false,
			},
			wantReason: inbox.EntitlementGrantFailureInvalidResponse,
		},
		{
			name:       "unknown error",
			errorValue: errors.New("sensitive upstream failure detail"),
			wantReason: inbox.EntitlementGrantFailureUnclassified,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			eventID := "evt-terminal-" + strings.ReplaceAll(strings.ToLower(test.name), " ", "-")
			sealer := prepareReleasedGrantJob(t, ctx, store, eventID)
			executor, err := worker.NewGrantExecutor(
				store,
				&grantClient{err: test.errorValue},
				sealer,
			)
			if err != nil {
				t.Fatalf("new grant executor: %v", err)
			}
			if processed, err := executor.RunOnce(ctx); err != nil || !processed {
				t.Fatalf("execute terminal grant: processed=%t err=%v", processed, err)
			}
			stored, err := store.GetEntitlementGrantJob(
				ctx,
				"lark:wallet-topup:instance-"+eventID,
			)
			if err != nil {
				t.Fatalf("get terminal grant: %v", err)
			}
			if stored.Status != inbox.EntitlementGrantJobStatusDeadLetter ||
				stored.LastError != string(test.wantReason) ||
				strings.Contains(stored.LastError, "sensitive") {
				t.Fatalf("unexpected terminal grant: %+v", stored)
			}
		})
	}
}

func TestGrantExecutorRetriesResponseLossWithSameExternalID(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sealer := prepareReleasedGrantJob(t, ctx, store, "evt-response-loss")
	externalID := "lark:wallet-topup:instance-evt-response-loss"
	calls := 0
	client := grantClientFunc(func(
		_ context.Context,
		request newapi.EntitlementGrantRequest,
	) (newapi.EntitlementGrantResponse, error) {
		calls++
		if request.ExternalID != externalID {
			t.Fatalf("external ID = %q, want %q", request.ExternalID, externalID)
		}
		if calls == 1 {
			return newapi.EntitlementGrantResponse{}, &newapi.RequestError{
				Reason: "transport_error", Retryable: true,
			}
		}
		return newapi.EntitlementGrantResponse{
			Status: "replayed", ExternalID: externalID, UserID: 42,
			Result: newapi.GrantResult{
				GrantType: "wallet_quota", QuotaDelta: 2_500_000,
			},
		}, nil
	})
	executor, err := worker.NewGrantExecutor(
		store,
		client,
		sealer,
		worker.WithGrantRetryPolicy(worker.GrantRetryPolicy{
			Schedule:                []time.Duration{10 * time.Millisecond},
			PrincipalNotReadyMaxAge: time.Hour,
		}),
	)
	if err != nil {
		t.Fatalf("new grant executor: %v", err)
	}
	if processed, err := executor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("execute response-loss attempt: processed=%t err=%v", processed, err)
	}
	time.Sleep(15 * time.Millisecond)
	if processed, err := executor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("execute replay attempt: processed=%t err=%v", processed, err)
	}
	stored, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get replayed grant job: %v", err)
	}
	if calls != 2 || stored.Attempts != 2 ||
		stored.Status != inbox.EntitlementGrantJobStatusSucceeded || stored.Receipt == nil ||
		stored.Receipt.Status != "replayed" {
		t.Fatalf("unexpected replayed grant: calls=%d job=%+v", calls, stored)
	}
}

func TestGrantExecutorDeadLettersExhaustedTransportRetries(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sealer := prepareReleasedGrantJob(t, ctx, store, "evt-exhausted-transport")
	executor, err := worker.NewGrantExecutor(
		store,
		&grantClient{err: &newapi.RequestError{Reason: "transport_error", Retryable: true}},
		sealer,
		worker.WithGrantRetryPolicy(worker.GrantRetryPolicy{
			Schedule:                []time.Duration{10 * time.Millisecond},
			PrincipalNotReadyMaxAge: time.Hour,
		}),
	)
	if err != nil {
		t.Fatalf("new grant executor: %v", err)
	}
	if processed, err := executor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("execute first transport attempt: processed=%t err=%v", processed, err)
	}
	time.Sleep(15 * time.Millisecond)
	if processed, err := executor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("execute exhausted transport attempt: processed=%t err=%v", processed, err)
	}
	stored, err := store.GetEntitlementGrantJob(
		ctx,
		"lark:wallet-topup:instance-evt-exhausted-transport",
	)
	if err != nil {
		t.Fatalf("get exhausted transport grant: %v", err)
	}
	if stored.Status != inbox.EntitlementGrantJobStatusDeadLetter ||
		stored.LastError != string(inbox.EntitlementGrantFailureRetryExhaustedTransport) {
		t.Fatalf("unexpected exhausted transport grant: %+v", stored)
	}
}

func TestGrantExecutorDeadLettersUnreadablePayloadBeforeClientCall(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_ = prepareReleasedGrantJob(t, ctx, store, "evt-unreadable-payload")
	wrongSealer, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x24}, 32))
	if err != nil {
		t.Fatalf("new wrong grant sealer: %v", err)
	}
	client := &grantClient{}
	executor, err := worker.NewGrantExecutor(store, client, wrongSealer)
	if err != nil {
		t.Fatalf("new grant executor: %v", err)
	}
	if processed, err := executor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("execute unreadable grant: processed=%t err=%v", processed, err)
	}
	stored, err := store.GetEntitlementGrantJob(
		ctx,
		"lark:wallet-topup:instance-evt-unreadable-payload",
	)
	if err != nil {
		t.Fatalf("get unreadable grant: %v", err)
	}
	if client.calls != 0 || stored.Status != inbox.EntitlementGrantJobStatusDeadLetter ||
		stored.LastError != string(inbox.EntitlementGrantFailureInvalidSealedPayload) {
		t.Fatalf("unexpected unreadable grant: calls=%d job=%+v", client.calls, stored)
	}
}

func TestGrantExecutorCapsPrincipalNotReadyAtPolicyAge(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sealer := prepareReleasedGrantJob(t, ctx, store, "evt-principal-age")
	externalID := "lark:wallet-topup:instance-evt-principal-age"
	created, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get created grant: %v", err)
	}
	executor, err := worker.NewGrantExecutor(
		store,
		&grantClient{err: &newapi.APIError{
			StatusCode: 404, Code: "principal_not_ready", Retryable: true,
		}},
		sealer,
		worker.WithGrantRetryPolicy(worker.GrantRetryPolicy{
			Schedule:                []time.Duration{time.Hour},
			PrincipalNotReadyMaxAge: 24 * time.Hour,
		}),
		worker.WithGrantClock(func() time.Time {
			return created.ActivatedAt.Add(24*time.Hour + time.Nanosecond)
		}),
	)
	if err != nil {
		t.Fatalf("new grant executor: %v", err)
	}
	if processed, err := executor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("execute aged principal grant: processed=%t err=%v", processed, err)
	}
	stored, err := store.GetEntitlementGrantJob(ctx, externalID)
	if err != nil {
		t.Fatalf("get aged principal grant: %v", err)
	}
	if stored.Status != inbox.EntitlementGrantJobStatusDeadLetter ||
		stored.LastError != string(inbox.EntitlementGrantFailureRetryExhaustedPrincipal) {
		t.Fatalf("unexpected aged principal grant: %+v", stored)
	}
}

func prepareReleasedGrantJob(
	t *testing.T,
	ctx context.Context,
	store *inbox.Store,
	eventID string,
) *newapi.GrantSealer {
	t.Helper()
	recordApprovedEvent(t, ctx, store, eventID)
	sealer, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant sealer: %v", err)
	}
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
	if released, err := store.ReleaseHeldEntitlementGrantJobs(ctx); err != nil || released != 1 {
		t.Fatalf("release held grant: released=%d err=%v", released, err)
	}
	return sealer
}
