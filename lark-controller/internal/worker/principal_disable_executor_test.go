package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

func TestActivePrincipalDisableRuntimeReleasesAndExecutesHeldJob(t *testing.T) {
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
	wantRequest := prepareHeldPrincipalDisableJob(t, ctx, store, keyring, "evt-resigned-1")
	client := principalDisableClientFunc(func(
		_ context.Context,
		request newapi.PrincipalDisableRequest,
	) (newapi.PrincipalDisableResponse, error) {
		if request != wantRequest {
			t.Fatalf("request = %+v, want %+v", request, wantRequest)
		}
		return newapi.PrincipalDisableResponse{
			Status: "applied", ExternalID: request.ExternalID, Outcome: "disabled",
			PrincipalVersion: 4, AuthVersion: 7,
		}, nil
	})
	executor, err := worker.NewPrincipalDisableExecutor(store, client, keyring)
	if err != nil {
		t.Fatalf("new principal disable executor: %v", err)
	}
	runtime, err := worker.NewActivePrincipalDisableRuntime(store, executor)
	if err != nil {
		t.Fatalf("new active principal disable runtime: %v", err)
	}
	held, err := store.GetPrincipalDisableJob(ctx, wantRequest.ExternalID)
	if err != nil {
		t.Fatalf("get held job: %v", err)
	}
	if held.Status != inbox.PrincipalDisableJobStatusHeldShadow || !held.ActivatedAt.IsZero() {
		t.Fatalf("job bypassed shadow hold: %+v", held)
	}
	if processed, err := runtime.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run active principal disable: processed=%t err=%v", processed, err)
	}
	completed, err := store.GetPrincipalDisableJob(ctx, wantRequest.ExternalID)
	if err != nil {
		t.Fatalf("get completed job: %v", err)
	}
	if completed.Status != inbox.PrincipalDisableJobStatusSucceeded ||
		completed.ActivatedAt.IsZero() || completed.Receipt == nil ||
		completed.Receipt.Status != "applied" || completed.Receipt.Outcome != "disabled" ||
		completed.Receipt.PrincipalVersion != 4 || completed.Receipt.AuthVersion != 7 {
		t.Fatalf("principal disable did not complete: %+v", completed)
	}
}

func TestPrincipalDisableExecutorRetriesResponseLossWithoutReplacingRequest(t *testing.T) {
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
	request := prepareHeldPrincipalDisableJob(t, ctx, store, keyring, "evt-response-loss")
	if released, err := store.ReleaseHeldPrincipalDisableJobs(ctx); err != nil || released != 1 {
		t.Fatalf("release held principal disable: released=%d err=%v", released, err)
	}
	executor, err := worker.NewPrincipalDisableExecutor(
		store,
		principalDisableClientFunc(func(
			context.Context,
			newapi.PrincipalDisableRequest,
		) (newapi.PrincipalDisableResponse, error) {
			return newapi.PrincipalDisableResponse{}, &newapi.RequestError{
				Reason: "transport_error", Retryable: true,
			}
		}),
		keyring,
	)
	if err != nil {
		t.Fatalf("new principal disable executor: %v", err)
	}
	if processed, err := executor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run principal disable response loss: processed=%t err=%v", processed, err)
	}
	job, err := store.GetPrincipalDisableJob(ctx, request.ExternalID)
	if err != nil {
		t.Fatalf("get retrying principal disable: %v", err)
	}
	if job.Status != inbox.PrincipalDisableJobStatusRetryWait ||
		job.LastError != "transport_error" || job.Attempts != 1 ||
		job.NextAttemptAt.Before(job.UpdatedAt.Add(4*time.Second)) {
		t.Fatalf("principal disable was not scheduled for stable replay: %+v", job)
	}
}

func TestPrincipalDisableExecutorRetriesTransientHTTPStatuses(t *testing.T) {
	for _, statusCode := range []int{
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
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
			request := prepareHeldPrincipalDisableJob(t, ctx, store, keyring, "evt-transient-http")
			if released, err := store.ReleaseHeldPrincipalDisableJobs(ctx); err != nil || released != 1 {
				t.Fatalf("release held principal disable: released=%d err=%v", released, err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(statusCode)
				_, _ = response.Write([]byte("unstructured upstream failure"))
			}))
			defer server.Close()
			client, err := newapi.NewClient(newapi.Config{
				BaseURL:           server.URL,
				IntegrationSecret: "worker-test-not-a-real-integration-secret",
				HTTPClient:        server.Client(),
			})
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			executor, err := worker.NewPrincipalDisableExecutor(store, client, keyring)
			if err != nil {
				t.Fatalf("new principal disable executor: %v", err)
			}
			if processed, err := executor.RunOnce(ctx); err != nil || !processed {
				t.Fatalf("run principal disable: processed=%t err=%v", processed, err)
			}
			job, err := store.GetPrincipalDisableJob(ctx, request.ExternalID)
			if err != nil {
				t.Fatalf("get retrying principal disable: %v", err)
			}
			if job.Status != inbox.PrincipalDisableJobStatusRetryWait ||
				job.LastError != "temporarily_unavailable" || job.Attempts != 1 {
				t.Fatalf("HTTP %d principal disable was not retried: %+v", statusCode, job)
			}
		})
	}
}

func TestPrincipalDisableExecutorDeadLettersInvalidSuccessResponse(t *testing.T) {
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
	request := prepareHeldPrincipalDisableJob(t, ctx, store, keyring, "evt-invalid-success")
	if released, err := store.ReleaseHeldPrincipalDisableJobs(ctx); err != nil || released != 1 {
		t.Fatalf("release held principal disable: released=%d err=%v", released, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"status":"noop","external_id":"` +
			request.ExternalID + `","outcome":"disabled","principal_version":4,"auth_version":7}`))
	}))
	defer server.Close()
	client, err := newapi.NewClient(newapi.Config{
		BaseURL:           server.URL,
		IntegrationSecret: "worker-test-not-a-real-integration-secret",
		HTTPClient:        server.Client(),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	executor, err := worker.NewPrincipalDisableExecutor(store, client, keyring)
	if err != nil {
		t.Fatalf("new principal disable executor: %v", err)
	}
	if processed, err := executor.RunOnce(ctx); err != nil || !processed {
		t.Fatalf("run principal disable: processed=%t err=%v", processed, err)
	}
	job, err := store.GetPrincipalDisableJob(ctx, request.ExternalID)
	if err != nil {
		t.Fatalf("get dead-lettered principal disable: %v", err)
	}
	if job.Status != inbox.PrincipalDisableJobStatusDeadLetter ||
		job.LastError != "invalid_response" || job.Attempts != 1 {
		t.Fatalf("invalid success response was not classified stably: %+v", job)
	}
}

func TestPrincipalDisableJobRecoversClaimAfterRestart(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	request := prepareHeldPrincipalDisableJob(t, ctx, store, keyring, "evt-restart")
	if released, err := store.ReleaseHeldPrincipalDisableJobs(ctx); err != nil || released != 1 {
		t.Fatalf("release principal disable: released=%d err=%v", released, err)
	}
	claimed, found, err := store.ClaimNextPrincipalDisableJob(ctx)
	if err != nil || !found || claimed.Status != inbox.PrincipalDisableJobStatusProcessing {
		t.Fatalf("claim principal disable: found=%t job=%+v err=%v", found, claimed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close claimed store: %v", err)
	}
	restarted, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	recovered, err := restarted.GetPrincipalDisableJob(ctx, request.ExternalID)
	if err != nil {
		t.Fatalf("get recovered principal disable: %v", err)
	}
	if recovered.Status != inbox.PrincipalDisableJobStatusPending || recovered.Attempts != 1 {
		t.Fatalf("claimed principal disable did not recover after restart: %+v", recovered)
	}
}

func prepareHeldPrincipalDisableJob(
	t *testing.T,
	ctx context.Context,
	store *inbox.Store,
	keyring *newapi.GrantKeyring,
	eventID string,
) newapi.PrincipalDisableRequest {
	t.Helper()
	request, receipt, err := newapi.PlanContactEventPrincipalDisable(
		"tenant-test",
		"ou-resigned",
		eventID,
	)
	if err != nil {
		t.Fatalf("plan principal disable: %v", err)
	}
	sealed, err := keyring.SealPrincipalDisable(request)
	if err != nil {
		t.Fatalf("seal principal disable: %v", err)
	}
	payload, err := json.Marshal(map[string]string{"subject_sha256": receipt.SubjectSHA256})
	if err != nil {
		t.Fatalf("encode sanitized contact event: %v", err)
	}
	if _, err := store.Record(ctx, inbox.Event{
		Key: "lark:v2:" + eventID, SchemaVersion: "2.0", EventID: eventID,
		EventType: "contact.user.deleted_v3", AppID: "cli_test", TenantKey: "tenant-test",
		PayloadJSON: string(payload),
		PrincipalDisableJob: &inbox.PrincipalDisableJobDraft{
			ExternalID: sealed.ExternalID, RequestSHA256: sealed.RequestSHA256,
			SubjectSHA256: receipt.SubjectSHA256, KeyID: sealed.KeyID,
			Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
		},
	}); err != nil {
		t.Fatalf("record principal disable event: %v", err)
	}
	return request
}

type principalDisableClientFunc func(
	context.Context,
	newapi.PrincipalDisableRequest,
) (newapi.PrincipalDisableResponse, error)

func (function principalDisableClientFunc) DisablePrincipal(
	ctx context.Context,
	request newapi.PrincipalDisableRequest,
) (newapi.PrincipalDisableResponse, error) {
	return function(ctx, request)
}
