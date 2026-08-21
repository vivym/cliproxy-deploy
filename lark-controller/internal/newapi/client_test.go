package newapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

const integrationSecret = "test-only-" + "not-a-real-integration-secret"

func TestClientSendsEntitlementGrantContract(t *testing.T) {
	wantRequest := newapi.EntitlementGrantRequest{
		ExternalID:    "lark:wallet-topup:instance-1",
		Source:        "lark_approval",
		PolicyVersion: "employee-v1",
		Identity: newapi.Identity{
			ProviderSlug: "lark",
			Subject:      "tenant-1:ou-requester",
		},
		Grant: newapi.Grant{
			Type:        "wallet_quota",
			PackageCode: "topup_10",
			QuotaDelta:  5_000_000,
		},
		Evidence: &newapi.Evidence{
			ApprovalCode:      "approval-1",
			InstanceCode:      "instance-1",
			InstanceStartedAt: "2026-08-21T09:18:20Z",
			SchemaFingerprint: "sha256:abc",
			Locale:            "zh-CN",
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/integrations/v1/entitlement-grants" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+integrationSecret {
			t.Fatalf("authorization header is missing")
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content type = %q", request.Header.Get("Content-Type"))
		}
		var got newapi.EntitlementGrantRequest
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if !reflect.DeepEqual(got, wantRequest) {
			t.Fatalf("request = %+v, want %+v", got, wantRequest)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
            "status":"applied",
            "external_id":"lark:wallet-topup:instance-1",
            "user_id":42,
            "result":{"grant_type":"wallet_quota","quota_delta":5000000}
        }`))
	}))
	defer server.Close()

	client, err := newapi.NewClient(newapi.Config{
		BaseURL: server.URL, IntegrationSecret: integrationSecret, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	result, err := client.Grant(context.Background(), wantRequest)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if result.Status != "applied" || result.ExternalID != wantRequest.ExternalID ||
		result.UserID != 42 || result.Result.GrantType != "wallet_quota" ||
		result.Result.QuotaDelta != 5_000_000 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestClientClassifiesOnlyDocumentedRetryableErrors(t *testing.T) {
	tests := []struct {
		name         string
		statusCode   int
		upstreamCode string
		wantCode     string
		retryable    bool
	}{
		{name: "principal not ready", statusCode: http.StatusNotFound, upstreamCode: "principal_not_ready", wantCode: "principal_not_ready", retryable: true},
		{name: "temporary unavailable", statusCode: http.StatusServiceUnavailable, upstreamCode: "temporarily_unavailable", wantCode: "temporarily_unavailable", retryable: true},
		{name: "payload mismatch", statusCode: http.StatusConflict, upstreamCode: "external_id_payload_mismatch", wantCode: "external_id_payload_mismatch", retryable: false},
		{name: "unknown code", statusCode: http.StatusInternalServerError, upstreamCode: "sensitive_internal_detail", wantCode: "unclassified_error", retryable: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(test.statusCode)
				_, _ = response.Write([]byte(`{"code":"` + test.upstreamCode + `","message":"sensitive upstream detail"}`))
			}))
			defer server.Close()
			client, err := newapi.NewClient(newapi.Config{
				BaseURL: server.URL, IntegrationSecret: integrationSecret, HTTPClient: server.Client(),
			})
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			_, err = client.Grant(context.Background(), validSubscriptionGrant())
			var apiError *newapi.APIError
			if !errors.As(err, &apiError) {
				t.Fatalf("error = %v, want APIError", err)
			}
			if apiError.StatusCode != test.statusCode || apiError.Code != test.wantCode ||
				apiError.Retryable != test.retryable {
				t.Fatalf("API error = %+v", apiError)
			}
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("error leaked upstream message: %v", err)
			}
		})
	}
}

func TestClientRetriesUnstructuredServiceUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, err := newapi.NewClient(newapi.Config{
		BaseURL: server.URL, IntegrationSecret: integrationSecret, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.Grant(context.Background(), validSubscriptionGrant())
	var apiError *newapi.APIError
	if !errors.As(err, &apiError) || apiError.Code != "temporarily_unavailable" || !apiError.Retryable {
		t.Fatalf("error = %v, want retryable temporarily_unavailable", err)
	}
}

func TestClientClassifiesMidResponseLossAsRetryable(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(io.MultiReader(
				strings.NewReader(`{"status":"applied"`),
				failingReader{err: io.ErrUnexpectedEOF},
			)),
		}, nil
	})}
	client, err := newapi.NewClient(newapi.Config{
		BaseURL: "http://new-api:3001", IntegrationSecret: integrationSecret, HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.Grant(context.Background(), validSubscriptionGrant())
	var requestError *newapi.RequestError
	if !errors.As(err, &requestError) || requestError.Reason != "transport_error" || !requestError.Retryable {
		t.Fatalf("error = %v, want retryable transport error", err)
	}
}

func TestClientClassifiesEmptySuccessBodyAsRetryable(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})}
	client, err := newapi.NewClient(newapi.Config{
		BaseURL: "http://new-api:3001", IntegrationSecret: integrationSecret, HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.Grant(context.Background(), validSubscriptionGrant())
	var requestError *newapi.RequestError
	if !errors.As(err, &requestError) || requestError.Reason != "transport_error" || !requestError.Retryable {
		t.Fatalf("error = %v, want retryable transport error", err)
	}
}

func TestClientPreservesContextCancellationAfterResponseHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(cancelingReader{cancel: cancel}),
		}, nil
	})}
	client, err := newapi.NewClient(newapi.Config{
		BaseURL: "http://new-api:3001", IntegrationSecret: integrationSecret, HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.Grant(ctx, validSubscriptionGrant())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestClientRejectsResponseWhoseTrailingWhitespaceExceedsLimit(t *testing.T) {
	validResponse := `{
        "status":"applied",
        "external_id":"lark:subscription-level:instance-1",
        "user_id":42,
        "result":{"grant_type":"subscription_level","level_code":"pro"}
    }`
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				validResponse + strings.Repeat(" ", (1<<20)+1),
			)),
		}, nil
	})}
	client, err := newapi.NewClient(newapi.Config{
		BaseURL: "http://new-api:3001", IntegrationSecret: integrationSecret, HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.Grant(context.Background(), validSubscriptionGrant())
	var requestError *newapi.RequestError
	if !errors.As(err, &requestError) || requestError.Reason != "invalid_response" || requestError.Retryable {
		t.Fatalf("error = %v, want terminal invalid_response", err)
	}
}

func TestClientRejectsSubscriptionResponseWithoutLevelCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
            "status":"applied",
            "external_id":"lark:subscription-level:instance-1",
            "user_id":42,
            "result":{"grant_type":"subscription_level"}
        }`))
	}))
	defer server.Close()

	client, err := newapi.NewClient(newapi.Config{
		BaseURL: server.URL, IntegrationSecret: integrationSecret, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.Grant(context.Background(), validSubscriptionGrant())
	if err == nil || !strings.Contains(err.Error(), "subscription result does not match request") {
		t.Fatalf("error = %v, want subscription result mismatch", err)
	}
}

func TestClientListsOnlyActiveLarkPrincipals(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		wantQuery := url.Values{
			"provider_slug": {"lark"}, "status": {"active"}, "limit": {"50"}, "cursor": {"opaque-cursor"},
		}
		if request.Method != http.MethodGet || request.URL.Path != "/api/integrations/v1/principals" ||
			!reflect.DeepEqual(request.URL.Query(), wantQuery) {
			t.Fatalf("request = %s %s?%s", request.Method, request.URL.Path, request.URL.RawQuery)
		}
		if request.Header.Get("Authorization") != "Bearer "+integrationSecret {
			t.Fatalf("authorization header is missing")
		}
		_, _ = response.Write([]byte(`{
            "principals":[{
                "provider_slug":"lark",
                "subject":"tenant-1:ou-requester",
                "principal_version":3,
                "updated_at":"2026-08-21T09:18:20Z"
            }],
            "next_cursor":"next-page",
            "scan_complete":false
        }`))
	}))
	defer server.Close()
	client, err := newapi.NewClient(newapi.Config{
		BaseURL: server.URL, IntegrationSecret: integrationSecret, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	page, err := client.ListActiveLarkPrincipals(context.Background(), "opaque-cursor", 50)
	if err != nil {
		t.Fatalf("list principals: %v", err)
	}
	if len(page.Principals) != 1 || page.Principals[0].ProviderSlug != "lark" ||
		page.Principals[0].Subject != "tenant-1:ou-requester" ||
		page.Principals[0].PrincipalVersion != 3 || page.NextCursor != "next-page" || page.ScanComplete {
		t.Fatalf("unexpected page: %+v", page)
	}
}

func validSubscriptionGrant() newapi.EntitlementGrantRequest {
	return newapi.EntitlementGrantRequest{
		ExternalID:    "lark:subscription-level:instance-1",
		Source:        "lark_approval",
		PolicyVersion: "employee-v1",
		Identity:      newapi.Identity{ProviderSlug: "lark", Subject: "tenant-1:ou-requester"},
		Grant: newapi.Grant{
			Type: "subscription_level", LevelCode: "pro", MinimumRankOnly: true,
		},
		Evidence: &newapi.Evidence{
			ApprovalCode: "approval-1", InstanceCode: "instance-1",
			InstanceStartedAt: "2026-08-21T09:18:20Z",
			SchemaFingerprint: "sha256:abc", Locale: "zh-CN",
		},
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type failingReader struct {
	err error
}

func (r failingReader) Read([]byte) (int, error) {
	return 0, r.err
}

type cancelingReader struct {
	cancel context.CancelFunc
}

func (r cancelingReader) Read([]byte) (int, error) {
	r.cancel()
	return 0, context.Canceled
}
