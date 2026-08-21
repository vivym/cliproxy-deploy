package larkapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/larkapi"
)

func TestOAuthExchangerReturnsNormalizedLarkIdentity(t *testing.T) {
	var tokenRequests int
	var userInfoRequests int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth/v3/token":
			tokenRequests++
			if request.Method != http.MethodPost {
				t.Fatalf("token method = %s, want POST", request.Method)
			}
			if request.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("token content type = %q, want application/json", request.Header.Get("Content-Type"))
			}
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatalf("decode token request: %v", err)
			}
			want := map[string]any{
				"grant_type":    "authorization_code",
				"client_id":     "cli_test",
				"client_secret": "app-secret",
				"code":          "authorization-code",
				"redirect_uri":  "https://ai.x2r.store/integrations/lark/oauth/callback",
			}
			if !reflect.DeepEqual(body, want) {
				t.Fatalf("token request = %#v, want %#v", body, want)
			}
			writeOAuthTestJSON(t, response, map[string]any{
				"code": 0, "access_token": "user-access-token", "refresh_token": "must-not-escape",
			})
		case "/open-apis/authen/v1/user_info":
			userInfoRequests++
			if request.Method != http.MethodGet {
				t.Fatalf("userinfo method = %s, want GET", request.Method)
			}
			if request.Header.Get("Authorization") != "Bearer user-access-token" {
				t.Fatalf("userinfo authorization = %q, want user bearer token", request.Header.Get("Authorization"))
			}
			writeOAuthTestJSON(t, response, map[string]any{
				"code": 0,
				"data": map[string]any{
					"name": "Employee", "open_id": "ou_employee", "union_id": "on_employee",
					"tenant_key": "tenant-test", "email": "employee@example.com", "mobile": "secret",
				},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	exchanger, err := larkapi.NewOAuthExchanger(larkapi.OAuthConfig{
		AppID: "cli_test", AppSecret: "app-secret",
		RedirectURI: "https://ai.x2r.store/integrations/lark/oauth/callback",
		TenantKey:   "tenant-test", OAuthBaseURL: server.URL, OpenBaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("new OAuth exchanger: %v", err)
	}
	identity, err := exchanger.Exchange(context.Background(), "authorization-code")
	if err != nil {
		t.Fatalf("exchange authorization code: %v", err)
	}
	if tokenRequests != 1 || userInfoRequests != 1 {
		t.Fatalf("requests token=%d userinfo=%d, want 1 each", tokenRequests, userInfoRequests)
	}
	if identity.Subject != "tenant-test:ou_employee" ||
		identity.Username != "lark_te7ozrid4egv6gj" || identity.Name != "Employee" {
		t.Fatalf("normalized identity = %+v", identity)
	}
}

func TestOAuthExchangerTruncatesDisplayNameByUnicodeCodePoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth/v3/token":
			writeOAuthTestJSON(t, response, map[string]any{"code": 0, "access_token": "user-access-token"})
		case "/open-apis/authen/v1/user_info":
			writeOAuthTestJSON(t, response, map[string]any{
				"code": 0,
				"data": map[string]any{
					"name":    "一二三四五六七八九十一二三四五六七八九十末",
					"open_id": "ou_employee", "tenant_key": "tenant-test",
				},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	exchanger := newOAuthTestExchanger(t, server.URL, 0)
	identity, err := exchanger.Exchange(context.Background(), "authorization-code")
	if err != nil {
		t.Fatalf("exchange authorization code: %v", err)
	}
	if identity.Name != "一二三四五六七八九十一二三四五六七八九十" {
		t.Fatalf("display name = %q, want first 20 Unicode code points", identity.Name)
	}
}

func TestOAuthExchangerRejectsUnexpectedTenant(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth/v3/token":
			writeOAuthTestJSON(t, response, map[string]any{"code": 0, "access_token": "user-access-token"})
		case "/open-apis/authen/v1/user_info":
			writeOAuthTestJSON(t, response, map[string]any{
				"code": 0,
				"data": map[string]any{
					"name": "Employee", "open_id": "ou_employee", "tenant_key": "unexpected-tenant",
				},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	_, err := newOAuthTestExchanger(t, server.URL, 0).Exchange(context.Background(), "authorization-code")
	var failure *larkapi.OAuthExchangeError
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want *larkapi.OAuthExchangeError", err)
	}
	if failure.Reason != larkapi.OAuthTenantMismatch || err.Error() != "Lark OAuth exchange failed: tenant_mismatch" {
		t.Fatalf("failure = %v, want stable tenant_mismatch", err)
	}
}

func TestOAuthExchangerRejectsResponsesMissingRequiredIdentityFields(t *testing.T) {
	tests := []struct {
		name     string
		token    map[string]any
		userInfo map[string]any
	}{
		{
			name: "access token", token: map[string]any{"code": 0},
		},
		{
			name: "display name", token: map[string]any{"code": 0, "access_token": "user-access-token"},
			userInfo: map[string]any{
				"code": 0, "data": map[string]any{"open_id": "ou_employee", "tenant_key": "tenant-test"},
			},
		},
		{
			name: "open id", token: map[string]any{"code": 0, "access_token": "user-access-token"},
			userInfo: map[string]any{
				"code": 0, "data": map[string]any{"name": "Employee", "tenant_key": "tenant-test"},
			},
		},
		{
			name: "tenant key", token: map[string]any{"code": 0, "access_token": "user-access-token"},
			userInfo: map[string]any{
				"code": 0, "data": map[string]any{"name": "Employee", "open_id": "ou_employee"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/oauth/v3/token":
					writeOAuthTestJSON(t, response, test.token)
				case "/open-apis/authen/v1/user_info":
					writeOAuthTestJSON(t, response, test.userInfo)
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()

			_, err := newOAuthTestExchanger(t, server.URL, 0).Exchange(context.Background(), "authorization-code")
			var failure *larkapi.OAuthExchangeError
			if !errors.As(err, &failure) {
				t.Fatalf("error = %v, want *larkapi.OAuthExchangeError", err)
			}
			if failure.Reason != larkapi.OAuthInvalidResponse ||
				err.Error() != "Lark OAuth exchange failed: invalid_response" {
				t.Fatalf("failure = %v, want stable invalid_response", err)
			}
		})
	}
}

func TestOAuthExchangerClassifiesRejectedAuthorizationCodeWithoutLeakingResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/oauth/v3/token" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusBadRequest)
		if _, err := response.Write([]byte(`{"code":20021,"error":"invalid_grant","error_description":"authorization-code-secret"}`)); err != nil {
			t.Fatalf("write token rejection: %v", err)
		}
	}))
	defer server.Close()

	_, err := newOAuthTestExchanger(t, server.URL, 0).Exchange(context.Background(), "authorization-code-secret")
	var failure *larkapi.OAuthExchangeError
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want *larkapi.OAuthExchangeError", err)
	}
	if failure.Reason != larkapi.OAuthAuthorizationCodeInvalid ||
		err.Error() != "Lark OAuth exchange failed: authorization_code_invalid" {
		t.Fatalf("failure = %v, want stable authorization_code_invalid", err)
	}
	if strings.Contains(err.Error(), "authorization-code-secret") || strings.Contains(err.Error(), "20021") {
		t.Fatalf("failure leaked Lark response details: %v", err)
	}
}

func TestOAuthExchangerRejectsMalformedAndOversizedResponses(t *testing.T) {
	tests := []struct {
		name   string
		stage  string
		body   string
		reason larkapi.OAuthExchangeFailureReason
	}{
		{name: "malformed token", stage: "token", body: "not-json", reason: larkapi.OAuthInvalidResponse},
		{name: "malformed userinfo", stage: "userinfo", body: "not-json", reason: larkapi.OAuthInvalidResponse},
		{
			name: "oversized token", stage: "token",
			body:   `{"code":0,"access_token":"user-access-token","padding":"` + strings.Repeat("x", 64*1024) + `"}`,
			reason: larkapi.OAuthResponseTooLarge,
		},
		{
			name: "oversized userinfo", stage: "userinfo",
			body:   `{"code":0,"data":{"name":"Employee","open_id":"ou_employee","tenant_key":"tenant-test"},"padding":"` + strings.Repeat("x", 64*1024) + `"}`,
			reason: larkapi.OAuthResponseTooLarge,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/oauth/v3/token":
					if test.stage == "token" {
						_, _ = response.Write([]byte(test.body))
						return
					}
					writeOAuthTestJSON(t, response, map[string]any{"code": 0, "access_token": "user-access-token"})
				case "/open-apis/authen/v1/user_info":
					_, _ = response.Write([]byte(test.body))
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()

			_, err := newOAuthTestExchanger(t, server.URL, 0).Exchange(context.Background(), "authorization-code")
			var failure *larkapi.OAuthExchangeError
			if !errors.As(err, &failure) {
				t.Fatalf("error = %v, want *larkapi.OAuthExchangeError", err)
			}
			if failure.Reason != test.reason {
				t.Fatalf("failure reason = %q, want %q", failure.Reason, test.reason)
			}
		})
	}
}

func TestOAuthExchangerClassifiesTimeoutAndTransportFailures(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			time.Sleep(30 * time.Millisecond)
			response.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()

		_, err := newOAuthTestExchanger(t, server.URL, 5*time.Millisecond).
			Exchange(context.Background(), "authorization-code-secret")
		assertOAuthExchangeFailure(t, err, larkapi.OAuthTimeout)
	})

	t.Run("transport", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		baseURL := server.URL
		server.Close()

		_, err := newOAuthTestExchanger(t, baseURL, 0).
			Exchange(context.Background(), "authorization-code-secret")
		assertOAuthExchangeFailure(t, err, larkapi.OAuthTransportError)
	})
}

func TestOAuthExchangerClassifiesLarkRejectionsWithoutLeakingResponse(t *testing.T) {
	tests := []struct {
		name   string
		stage  string
		status int
		body   map[string]any
		reason larkapi.OAuthExchangeFailureReason
	}{
		{
			name: "token rejected", stage: "token", status: http.StatusUnauthorized,
			body: map[string]any{
				"code": 20024, "error": "invalid_client", "error_description": "app-secret-must-not-leak",
			},
			reason: larkapi.OAuthTokenRejected,
		},
		{
			name: "userinfo rejected", stage: "userinfo", status: http.StatusUnauthorized,
			body:   map[string]any{"code": 20002, "msg": "user-access-token-must-not-leak"},
			reason: larkapi.OAuthUserInfoRejected,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/oauth/v3/token":
					if test.stage == "token" {
						response.WriteHeader(test.status)
						writeOAuthTestJSON(t, response, test.body)
						return
					}
					writeOAuthTestJSON(t, response, map[string]any{"code": 0, "access_token": "user-access-token"})
				case "/open-apis/authen/v1/user_info":
					response.WriteHeader(test.status)
					writeOAuthTestJSON(t, response, test.body)
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()

			_, err := newOAuthTestExchanger(t, server.URL, 0).Exchange(context.Background(), "authorization-code")
			assertOAuthExchangeFailure(t, err, test.reason)
			if strings.Contains(err.Error(), "must-not-leak") || strings.Contains(err.Error(), "20024") ||
				strings.Contains(err.Error(), "20002") {
				t.Fatalf("failure leaked Lark response details: %v", err)
			}
		})
	}
}

func TestOAuthExchangerClassifiesUpstreamUnavailableResponses(t *testing.T) {
	tests := []struct {
		name      string
		stage     string
		status    int
		oversized bool
	}{
		{name: "token service unavailable", stage: "token", status: http.StatusServiceUnavailable},
		{
			name: "token service unavailable with oversized body", stage: "token",
			status: http.StatusServiceUnavailable, oversized: true,
		},
		{name: "userinfo rate limited", stage: "userinfo", status: http.StatusTooManyRequests},
		{
			name: "userinfo rate limited with oversized body", stage: "userinfo",
			status: http.StatusTooManyRequests, oversized: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/oauth/v3/token":
					if test.stage == "token" {
						response.WriteHeader(test.status)
						if test.oversized {
							_, _ = response.Write([]byte(strings.Repeat("x", 65*1024)))
							return
						}
						writeOAuthTestJSON(t, response, map[string]any{
							"code": 999999, "msg": "sensitive upstream outage",
						})
						return
					}
					writeOAuthTestJSON(t, response, map[string]any{
						"code": 0, "access_token": "user-access-token",
					})
				case "/open-apis/authen/v1/user_info":
					response.WriteHeader(test.status)
					if test.oversized {
						_, _ = response.Write([]byte(strings.Repeat("x", 65*1024)))
						return
					}
					writeOAuthTestJSON(t, response, map[string]any{
						"code": 999999, "msg": "sensitive upstream outage",
					})
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()

			_, err := newOAuthTestExchanger(t, server.URL, 0).
				Exchange(context.Background(), "authorization-code-secret")
			assertOAuthExchangeFailure(t, err, larkapi.OAuthUpstreamUnavailable)
			if strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "999999") {
				t.Fatalf("failure leaked upstream response details: %v", err)
			}
		})
	}
}

func TestOAuthExchangerRejectsIdentityThatCannotBeStored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth/v3/token":
			writeOAuthTestJSON(t, response, map[string]any{"code": 0, "access_token": "user-access-token"})
		case "/open-apis/authen/v1/user_info":
			writeOAuthTestJSON(t, response, map[string]any{
				"code": 0,
				"data": map[string]any{
					"name": "Employee\r\nInjected", "open_id": "ou_employee", "tenant_key": "tenant-test",
				},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	_, err := newOAuthTestExchanger(t, server.URL, 0).Exchange(context.Background(), "authorization-code")
	assertOAuthExchangeFailure(t, err, larkapi.OAuthInvalidResponse)
}

func TestOAuthExchangerRequiresRegisteredControllerCallback(t *testing.T) {
	_, err := larkapi.NewOAuthExchanger(larkapi.OAuthConfig{
		AppID: "cli_test", AppSecret: "app-secret",
		RedirectURI: "https://attacker.example/integrations/lark/oauth/callback",
		TenantKey:   "tenant-test",
	})
	if err == nil || err.Error() != "Lark OAuth redirect URI must match the registered controller callback" {
		t.Fatalf("constructor error = %v, want fixed callback rejection", err)
	}
}

func TestOAuthExchangerRejectsMissingAuthorizationCodeWithStableReason(t *testing.T) {
	_, err := newOAuthTestExchanger(t, "http://127.0.0.1:1", time.Second).
		Exchange(context.Background(), "")
	assertOAuthExchangeFailure(t, err, larkapi.OAuthInvalidRequest)
}

func TestOAuthExchangerRejectsInsecureRemoteBaseURL(t *testing.T) {
	_, err := larkapi.NewOAuthExchanger(larkapi.OAuthConfig{
		AppID: "cli_test", AppSecret: "app-secret",
		RedirectURI: "https://ai.x2r.store/integrations/lark/oauth/callback",
		TenantKey:   "tenant-test", OAuthBaseURL: "http://accounts.example.com",
	})
	if err == nil || err.Error() != "Lark OAuth base URLs must be HTTPS origins or loopback HTTP origins" {
		t.Fatalf("constructor error = %v, want insecure base URL rejection", err)
	}
}

func TestOAuthExchangerClassifiesCallerDeadlineAsStableTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond)
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := newOAuthTestExchanger(t, server.URL, time.Second).
		Exchange(ctx, "authorization-code-secret")
	assertOAuthExchangeFailure(t, err, larkapi.OAuthTimeout)
}

func TestOAuthExchangerClassifiesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newOAuthTestExchanger(t, "http://127.0.0.1:1", time.Second).
		Exchange(ctx, "authorization-code-secret")
	assertOAuthExchangeFailure(t, err, larkapi.OAuthRequestCanceled)
}

func TestOAuthExchangerDoesNotFollowRedirectsWithCredentials(t *testing.T) {
	var redirectedRequests int
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		redirectedRequests++
		switch request.URL.Path {
		case "/oauth/v3/token":
			writeOAuthTestJSON(t, response, map[string]any{"code": 0, "access_token": "user-access-token"})
		case "/open-apis/authen/v1/user_info":
			writeOAuthTestJSON(t, response, map[string]any{
				"code": 0,
				"data": map[string]any{
					"name": "Employee", "open_id": "ou_employee", "tenant_key": "tenant-test",
				},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL+request.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	_, err := newOAuthTestExchanger(t, source.URL, 0).
		Exchange(context.Background(), "authorization-code-secret")
	assertOAuthExchangeFailure(t, err, larkapi.OAuthTokenRejected)
	if redirectedRequests != 0 {
		t.Fatalf("followed %d redirect(s), want credentials kept at configured origin", redirectedRequests)
	}
}

func assertOAuthExchangeFailure(
	t *testing.T,
	err error,
	want larkapi.OAuthExchangeFailureReason,
) {
	t.Helper()
	var failure *larkapi.OAuthExchangeError
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want *larkapi.OAuthExchangeError", err)
	}
	if failure.Reason != want || err.Error() != "Lark OAuth exchange failed: "+string(want) {
		t.Fatalf("failure = %v, want stable %s", err, want)
	}
	if strings.Contains(err.Error(), "authorization-code-secret") {
		t.Fatalf("failure leaked authorization code: %v", err)
	}
}

func newOAuthTestExchanger(t *testing.T, baseURL string, timeout time.Duration) *larkapi.OAuthExchanger {
	t.Helper()
	exchanger, err := larkapi.NewOAuthExchanger(larkapi.OAuthConfig{
		AppID: "cli_test", AppSecret: "app-secret",
		RedirectURI: "https://ai.x2r.store/integrations/lark/oauth/callback",
		TenantKey:   "tenant-test", OAuthBaseURL: baseURL, OpenBaseURL: baseURL,
		Timeout: timeout,
	})
	if err != nil {
		t.Fatalf("new OAuth exchanger: %v", err)
	}
	return exchanger
}

func writeOAuthTestJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Fatalf("encode test response: %v", err)
	}
}
