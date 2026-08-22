package newapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

const integrationSecret = "test-only-" + "not-a-real-integration-secret"

func TestLoadIntegrationSecretFileAcceptsOnePrintableToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lark-integration.secret")
	for name, contents := range map[string]string{
		"no line ending": integrationSecret,
		"LF":             integrationSecret + "\n",
		"CRLF":           integrationSecret + "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("write integration secret: %v", err)
			}
			loaded, err := newapi.LoadIntegrationSecretFile(path)
			if err != nil {
				t.Fatalf("load integration secret: %v", err)
			}
			if loaded != integrationSecret {
				t.Fatal("loaded integration secret does not match")
			}
		})
	}

	for name, contents := range map[string]string{
		"empty":          "",
		"too short":      strings.Repeat("a", 31),
		"leading space":  " " + integrationSecret,
		"trailing space": integrationSecret + " ",
		"tab":            strings.Repeat("a", 32) + "\t",
		"bare CR":        integrationSecret + "\r",
		"multiple lines": integrationSecret + "\n" + strings.Repeat("b", 32),
		"non ASCII":      strings.Repeat("a", 32) + "中",
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("write invalid integration secret: %v", err)
			}
			if _, err := newapi.LoadIntegrationSecretFile(path); err == nil ||
				(contents != "" && strings.Contains(err.Error(), contents)) {
				t.Fatalf("invalid integration secret error = %v", err)
			}
		})
	}
}

func TestClientRejectsUnsafeIntegrationSecret(t *testing.T) {
	for name, secret := range map[string]string{
		"too short": strings.Repeat("a", 31),
		"space":     strings.Repeat("a", 32) + " ",
		"newline":   strings.Repeat("a", 32) + "\n",
		"non ASCII": strings.Repeat("a", 32) + "中",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newapi.NewClient(newapi.Config{
				BaseURL: "http://new-api:3001", IntegrationSecret: secret,
			}); err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("unsafe integration secret error = %v", err)
			}
		})
	}
}

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

func TestClientSendsPrincipalDisableContract(t *testing.T) {
	wantRequest := newapi.PrincipalDisableRequest{
		ExternalID: "lark:disable:evt-resigned-1",
		Source:     "contact_event",
		Identity: newapi.Identity{
			ProviderSlug: "lark",
			Subject:      "tenant-1:ou-resigned",
		},
		Reason: "contact.user.deleted_v3",
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/integrations/v1/principals/disable" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+integrationSecret {
			t.Fatal("authorization header is missing")
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content type = %q", request.Header.Get("Content-Type"))
		}
		var got newapi.PrincipalDisableRequest
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if !reflect.DeepEqual(got, wantRequest) {
			t.Fatalf("request = %+v, want %+v", got, wantRequest)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
            "status":"applied",
            "external_id":"lark:disable:evt-resigned-1",
            "outcome":"disabled",
            "principal_version":4,
            "auth_version":7
        }`))
	}))
	defer server.Close()

	client, err := newapi.NewClient(newapi.Config{
		BaseURL: server.URL, IntegrationSecret: integrationSecret, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	result, err := client.DisablePrincipal(context.Background(), wantRequest)
	if err != nil {
		t.Fatalf("disable principal: %v", err)
	}
	if result.Status != "applied" || result.ExternalID != wantRequest.ExternalID ||
		result.Outcome != "disabled" || result.PrincipalVersion != 4 || result.AuthVersion != 7 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestClientClassifiesPrincipalDisableResponseLossAsRetryable(t *testing.T) {
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
	request, _, err := newapi.PlanContactEventPrincipalDisable(
		"tenant-1",
		"ou-resigned",
		"evt-response-loss",
	)
	if err != nil {
		t.Fatalf("plan principal disable: %v", err)
	}
	_, err = client.DisablePrincipal(context.Background(), request)
	var requestError *newapi.RequestError
	if !errors.As(err, &requestError) || requestError.Reason != "transport_error" ||
		!requestError.Retryable {
		t.Fatalf("error = %v, want retryable transport error", err)
	}
}

func TestClientRejectsInvalidPrincipalDisableResult(t *testing.T) {
	request, _, err := newapi.PlanContactEventPrincipalDisable(
		"tenant-1",
		"ou-resigned",
		"evt-invalid-result",
	)
	if err != nil {
		t.Fatalf("plan principal disable: %v", err)
	}
	tests := []struct {
		name string
		body string
	}{
		{
			name: "noop cannot claim a new disable",
			body: `{"status":"noop","external_id":"lark:disable:evt-invalid-result","outcome":"disabled","principal_version":4,"auth_version":7}`,
		},
		{
			name: "applied cannot claim an absent principal",
			body: `{"status":"applied","external_id":"lark:disable:evt-invalid-result","outcome":"principal_absent"}`,
		},
		{
			name: "disabled replay requires versions",
			body: `{"status":"replayed","external_id":"lark:disable:evt-invalid-result","outcome":"disabled"}`,
		},
		{
			name: "external id must match",
			body: `{"status":"noop","external_id":"lark:disable:other","outcome":"principal_absent"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := newapi.NewClient(newapi.Config{
				BaseURL: server.URL, IntegrationSecret: integrationSecret, HTTPClient: server.Client(),
			})
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			_, err = client.DisablePrincipal(context.Background(), request)
			var requestError *newapi.RequestError
			if !errors.As(err, &requestError) || requestError.Reason != "invalid_response" ||
				requestError.Retryable {
				t.Fatalf("error = %v, want terminal invalid_response", err)
			}
		})
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
		{name: "invalid request", statusCode: http.StatusBadRequest, upstreamCode: "invalid_request", wantCode: "invalid_request", retryable: false},
		{name: "unauthorized", statusCode: http.StatusUnauthorized, upstreamCode: "integration_unauthorized", wantCode: "integration_unauthorized", retryable: false},
		{name: "principal not ready", statusCode: http.StatusNotFound, upstreamCode: "principal_not_ready", wantCode: "principal_not_ready", retryable: true},
		{name: "principal disabled", statusCode: http.StatusConflict, upstreamCode: "principal_disabled", wantCode: "principal_disabled", retryable: false},
		{name: "unmanaged subscription conflict", statusCode: http.StatusConflict, upstreamCode: "unmanaged_subscription_conflict", wantCode: "unmanaged_subscription_conflict", retryable: false},
		{name: "policy version mismatch", statusCode: http.StatusConflict, upstreamCode: "policy_version_mismatch", wantCode: "policy_version_mismatch", retryable: false},
		{name: "approval binding mismatch", statusCode: http.StatusConflict, upstreamCode: "approval_binding_mismatch", wantCode: "approval_binding_mismatch", retryable: false},
		{name: "temporary unavailable", statusCode: http.StatusServiceUnavailable, upstreamCode: "temporarily_unavailable", wantCode: "temporarily_unavailable", retryable: true},
		{name: "payload mismatch", statusCode: http.StatusConflict, upstreamCode: "external_id_payload_mismatch", wantCode: "external_id_payload_mismatch", retryable: false},
		{name: "unknown package", statusCode: http.StatusUnprocessableEntity, upstreamCode: "unknown_package", wantCode: "unknown_package", retryable: false},
		{name: "unknown level", statusCode: http.StatusUnprocessableEntity, upstreamCode: "unknown_level", wantCode: "unknown_level", retryable: false},
		{name: "quota out of range", statusCode: http.StatusUnprocessableEntity, upstreamCode: "quota_out_of_range", wantCode: "quota_out_of_range", retryable: false},
		{name: "unknown code", statusCode: http.StatusInternalServerError, upstreamCode: "sensitive_internal_detail", wantCode: "unclassified_error", retryable: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(test.statusCode)
				_, _ = response.Write([]byte(`{"error":{"code":"` + test.upstreamCode + `","message":"sensitive upstream detail"}}`))
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

func TestClientAcceptsNewAPISubscriptionGrantResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
            "status":"applied",
            "external_id":"lark:subscription-level:instance-1",
            "user_id":42,
            "result":{
                "grant_type":"subscription_level",
                "level_code":"pro",
                "subscription_id":701,
                "assignment_version":3,
                "transition":"updated"
            }
        }`))
	}))
	defer server.Close()

	client, err := newapi.NewClient(newapi.Config{
		BaseURL: server.URL, IntegrationSecret: integrationSecret, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	result, err := client.Grant(context.Background(), validSubscriptionGrant())
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if result.Result.LevelCode != "pro" || result.Result.SubscriptionID != 701 ||
		result.Result.AssignmentVersion != 3 || result.Result.Transition != "updated" {
		t.Fatalf("unexpected subscription result: %+v", result.Result)
	}
}

func TestClientRejectsIncompleteNewAPISubscriptionGrantResult(t *testing.T) {
	tests := []struct {
		name   string
		result string
	}{
		{name: "missing subscription id", result: `"assignment_version":3,"transition":"updated"`},
		{name: "missing assignment version", result: `"subscription_id":701,"transition":"updated"`},
		{name: "missing transition", result: `"subscription_id":701,"assignment_version":3`},
		{name: "unknown transition", result: `"subscription_id":701,"assignment_version":3,"transition":"replaced"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(`{
                    "status":"applied",
                    "external_id":"lark:subscription-level:instance-1",
                    "user_id":42,
                    "result":{"grant_type":"subscription_level","level_code":"pro",` + test.result + `}
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
			if err == nil || !strings.Contains(err.Error(), "subscription result is incomplete") {
				t.Fatalf("error = %v, want incomplete subscription result", err)
			}
		})
	}
}

func TestClientValidatesSubscriptionStatusTransitionMatrix(t *testing.T) {
	validPairs := map[string]bool{
		"applied/created":             true,
		"applied/updated":             true,
		"noop/noop":                   true,
		"ignored_stale/ignored_stale": true,
		"replayed/created":            true,
		"replayed/updated":            true,
		"replayed/noop":               true,
		"replayed/ignored_stale":      true,
	}
	statuses := []string{"applied", "noop", "ignored_stale", "replayed"}
	transitions := []string{"created", "updated", "noop", "ignored_stale"}
	for _, status := range statuses {
		for _, transition := range transitions {
			pair := status + "/" + transition
			t.Run(pair, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
					response.Header().Set("Content-Type", "application/json")
					_, _ = response.Write([]byte(`{
					"status":"` + status + `",
					"external_id":"lark:subscription-level:instance-1",
					"user_id":42,
					"result":{
                        "grant_type":"subscription_level",
						"level_code":"pro",
						"subscription_id":701,
						"assignment_version":3,
						"transition":"` + transition + `"
					}
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
				if validPairs[pair] && err != nil {
					t.Fatalf("grant: %v", err)
				}
				if !validPairs[pair] && (err == nil || !strings.Contains(err.Error(), "subscription status and transition do not match")) {
					t.Fatalf("error = %v, want subscription status/transition mismatch", err)
				}
			})
		}
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
