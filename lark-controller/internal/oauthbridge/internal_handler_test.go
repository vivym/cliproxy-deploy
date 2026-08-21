package oauthbridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/oauthbridge"
)

const testBridgeClientSecret = "bridge-client-secret-32-bytes-minimum"

func TestInternalTokenExchangesLoginCodeForOpaqueBearerHandle(t *testing.T) {
	fixture := newInternalOAuthFixture(t, 0)
	form := validTokenForm(fixture.loginCode)
	exchange := func() *httptest.ResponseRecorder {
		return requestInternalToken(fixture.handler, http.MethodPost, form)
	}

	response := exchange()
	assertPrivateOAuthResponse(t, response.Header())
	if response.Code != http.StatusOK {
		t.Fatalf("token status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	accessToken, _ := payload["access_token"].(string)
	if len(payload) != 3 || len(accessToken) != 43 || payload["token_type"] != "Bearer" ||
		payload["expires_in"] != float64(60) {
		t.Fatalf("token response=%#v, want minimal opaque 60-second bearer", payload)
	}
	if replay := exchange(); replay.Code != http.StatusBadRequest ||
		replay.Body.String() != "{\"error\":\"invalid_grant\"}\n" {
		t.Fatalf("replayed token status=%d body=%s, want invalid_grant",
			replay.Code, replay.Body.String())
	}
}

func TestInternalTokenRejectsInvalidClientWithoutConsumingLoginCode(t *testing.T) {
	fixture := newInternalOAuthFixture(t, 0)
	form := validTokenForm(fixture.loginCode)
	form.Set("client_secret", "wrong-client-secret-32-bytes-minimum")
	denied := requestInternalToken(fixture.handler, http.MethodPost, form)
	if denied.Code != http.StatusBadRequest || denied.Body.String() != "{\"error\":\"invalid_client\"}\n" {
		t.Fatalf("invalid client status=%d body=%s, want 400 invalid_client",
			denied.Code, denied.Body.String())
	}
	form.Set("client_secret", testBridgeClientSecret)
	if accepted := requestInternalToken(fixture.handler, http.MethodPost, form); accepted.Code != http.StatusOK {
		t.Fatalf("valid client after rejection status=%d body=%s, want unconsumed code",
			accepted.Code, accepted.Body.String())
	}
}

func TestInternalTokenRejectsUnsupportedGrantWithoutConsumingLoginCode(t *testing.T) {
	fixture := newInternalOAuthFixture(t, 0)
	form := validTokenForm(fixture.loginCode)
	form.Set("grant_type", "refresh_token")
	denied := requestInternalToken(fixture.handler, http.MethodPost, form)
	if denied.Code != http.StatusBadRequest ||
		denied.Body.String() != "{\"error\":\"unsupported_grant_type\"}\n" {
		t.Fatalf("unsupported grant status=%d body=%s, want unsupported_grant_type",
			denied.Code, denied.Body.String())
	}
	form.Set("grant_type", "authorization_code")
	if accepted := requestInternalToken(fixture.handler, http.MethodPost, form); accepted.Code != http.StatusOK {
		t.Fatalf("valid grant after rejection status=%d body=%s, want unconsumed code",
			accepted.Code, accepted.Body.String())
	}
}

func TestInternalTokenRejectsRedirectMismatchWithoutConsumingLoginCode(t *testing.T) {
	fixture := newInternalOAuthFixture(t, 0)
	form := validTokenForm(fixture.loginCode)
	form.Set("redirect_uri", "https://attacker.example/callback")
	denied := requestInternalToken(fixture.handler, http.MethodPost, form)
	if denied.Code != http.StatusBadRequest || denied.Body.String() != "{\"error\":\"invalid_grant\"}\n" {
		t.Fatalf("redirect mismatch status=%d body=%s, want invalid_grant",
			denied.Code, denied.Body.String())
	}
	form.Set("redirect_uri", testNewAPICallback)
	if accepted := requestInternalToken(fixture.handler, http.MethodPost, form); accepted.Code != http.StatusOK {
		t.Fatalf("valid redirect after rejection status=%d body=%s, want unconsumed code",
			accepted.Code, accepted.Body.String())
	}
}

func TestInternalTokenRejectsDuplicateFieldsWithoutConsumingLoginCode(t *testing.T) {
	fixture := newInternalOAuthFixture(t, 0)
	form := validTokenForm(fixture.loginCode)
	form["client_secret"] = append(form["client_secret"], "second-secret-value-32-bytes-long")
	denied := requestInternalToken(fixture.handler, http.MethodPost, form)
	if denied.Code != http.StatusBadRequest || denied.Body.String() != "{\"error\":\"invalid_request\"}\n" {
		t.Fatalf("duplicate secret status=%d body=%s, want invalid_request",
			denied.Code, denied.Body.String())
	}
	form.Set("client_secret", testBridgeClientSecret)
	if accepted := requestInternalToken(fixture.handler, http.MethodPost, form); accepted.Code != http.StatusOK {
		t.Fatalf("canonical request after rejection status=%d body=%s, want unconsumed code",
			accepted.Code, accepted.Body.String())
	}
}

func TestInternalTokenRejectsBasicAuthWithoutConsumingLoginCode(t *testing.T) {
	fixture := newInternalOAuthFixture(t, 0)
	form := validTokenForm(fixture.loginCode)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/internal/oauth/token",
		strings.NewReader(form.Encode()),
	)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Basic bridge-credentials")
	request.RemoteAddr = "198.51.100.10:1234"
	fixture.handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || response.Body.String() != "{\"error\":\"invalid_request\"}\n" {
		t.Fatalf("Basic token auth status=%d body=%s, want invalid_request",
			response.Code, response.Body.String())
	}
	if accepted := requestInternalToken(fixture.handler, http.MethodPost, form); accepted.Code != http.StatusOK {
		t.Fatalf("params auth after Basic rejection status=%d body=%s, want unconsumed code",
			accepted.Code, accepted.Body.String())
	}
}

func TestInternalTokenRejectsMalformedRequestsWithoutConsumingLoginCode(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		contentType string
		mutate      func(url.Values) string
	}{
		{
			name: "unknown field", target: "/internal/oauth/token",
			contentType: "application/x-www-form-urlencoded",
			mutate: func(form url.Values) string {
				form.Set("unexpected", "value")
				return form.Encode()
			},
		},
		{
			name: "missing field", target: "/internal/oauth/token",
			contentType: "application/x-www-form-urlencoded",
			mutate: func(form url.Values) string {
				form.Del("redirect_uri")
				return form.Encode()
			},
		},
		{
			name: "query parameter", target: "/internal/oauth/token?unexpected=value",
			contentType: "application/x-www-form-urlencoded",
			mutate:      func(form url.Values) string { return form.Encode() },
		},
		{
			name: "wrong content type", target: "/internal/oauth/token",
			contentType: "application/json",
			mutate:      func(form url.Values) string { return form.Encode() },
		},
		{
			name: "oversized body", target: "/internal/oauth/token",
			contentType: "application/x-www-form-urlencoded",
			mutate: func(url.Values) string {
				return strings.Repeat("x", 17*1024)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInternalOAuthFixture(t, 0)
			form := validTokenForm(fixture.loginCode)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, test.target, strings.NewReader(test.mutate(form)))
			request.Header.Set("Content-Type", test.contentType)
			request.RemoteAddr = "198.51.100.10:1234"
			fixture.handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest ||
				response.Body.String() != "{\"error\":\"invalid_request\"}\n" {
				t.Fatalf("malformed token status=%d body=%s, want invalid_request",
					response.Code, response.Body.String())
			}
			if accepted := requestInternalToken(
				fixture.handler,
				http.MethodPost,
				validTokenForm(fixture.loginCode),
			); accepted.Code != http.StatusOK {
				t.Fatalf("canonical token after rejection status=%d body=%s, want unconsumed code",
					accepted.Code, accepted.Body.String())
			}
		})
	}
}

func TestInternalTokenReturnsTemporarilyUnavailableWhenSQLiteFails(t *testing.T) {
	fixture := newInternalOAuthFixture(t, 0)
	if err := fixture.store.Close(); err != nil {
		t.Fatalf("close OAuth store: %v", err)
	}

	response := requestInternalToken(fixture.handler, http.MethodPost, validTokenForm(fixture.loginCode))
	assertPrivateOAuthResponse(t, response.Header())
	if response.Code != http.StatusServiceUnavailable ||
		response.Body.String() != "{\"error\":\"temporarily_unavailable\"}\n" {
		t.Fatalf("token SQLite failure status=%d body=%s, want temporarily_unavailable",
			response.Code, response.Body.String())
	}
}

func TestInternalUserInfoConsumesBearerHandleAndReturnsMinimalIdentity(t *testing.T) {
	fixture := newInternalOAuthFixture(t, 0)
	accessHandle, err := fixture.store.ExchangeOAuthLoginCode(context.Background(), fixture.loginCode)
	if err != nil {
		t.Fatalf("exchange login code: %v", err)
	}
	request := func() *httptest.ResponseRecorder {
		return requestInternalUserInfo(fixture.handler, http.MethodGet, accessHandle)
	}

	response := request()
	assertPrivateOAuthResponse(t, response.Header())
	if response.Code != http.StatusOK {
		t.Fatalf("userinfo status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode userinfo response: %v", err)
	}
	if len(payload) != 3 || payload["sub"] != fixture.identity.Subject ||
		payload["username"] != fixture.identity.Username || payload["name"] != fixture.identity.Name {
		t.Fatalf("userinfo response=%#v, want minimal normalized identity", payload)
	}
	baseExternalID := "lark:base:" + fixture.identity.Subject + ":employee-v1"
	baseJob, err := fixture.store.GetEntitlementGrantJob(context.Background(), baseExternalID)
	if err != nil {
		t.Fatalf("get userinfo base subscription job: %v", err)
	}
	if baseJob.Status != inbox.EntitlementGrantJobStatusHeldShadow || baseJob.Attempts != 0 {
		t.Fatalf("userinfo base subscription job=%+v, want held_shadow", baseJob)
	}
	replay := request()
	if replay.Code != http.StatusUnauthorized ||
		replay.Header().Get("WWW-Authenticate") != `Bearer error="invalid_token"` ||
		replay.Body.String() != "{\"error\":\"invalid_token\"}\n" {
		t.Fatalf("replayed userinfo status=%d authenticate=%q body=%s, want invalid_token",
			replay.Code, replay.Header().Get("WWW-Authenticate"), replay.Body.String())
	}
}

func TestInternalUserInfoReturnsTemporarilyUnavailableWhenSQLiteFails(t *testing.T) {
	fixture := newInternalOAuthFixture(t, 0)
	accessHandle, err := fixture.store.ExchangeOAuthLoginCode(context.Background(), fixture.loginCode)
	if err != nil {
		t.Fatalf("exchange login code: %v", err)
	}
	if err := fixture.store.Close(); err != nil {
		t.Fatalf("close OAuth store: %v", err)
	}

	response := requestInternalUserInfo(fixture.handler, http.MethodGet, accessHandle)
	assertPrivateOAuthResponse(t, response.Header())
	if response.Code != http.StatusServiceUnavailable ||
		response.Body.String() != "{\"error\":\"temporarily_unavailable\"}\n" {
		t.Fatalf("userinfo SQLite failure status=%d body=%s, want temporarily_unavailable",
			response.Code, response.Body.String())
	}
}

func TestInternalUserInfoRollsBackHandleWhenBaseGrantPlanningFails(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := inbox.NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("new identity: %v", err)
	}
	loginCode, err := store.CreateOAuthLoginCode(context.Background(), identity)
	if err != nil {
		t.Fatalf("create login code: %v", err)
	}
	accessHandle, err := store.ExchangeOAuthLoginCode(context.Background(), loginCode)
	if err != nil {
		t.Fatalf("exchange login code: %v", err)
	}
	delegate, err := newapi.NewGrantSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant sealer: %v", err)
	}
	sealer := &failOnceGrantSealer{delegate: delegate}
	handler := newBridgeTestHandlerWithGrantSealer(t, oauthbridge.Config{
		BridgeClientID: "bridge-client-id", BridgeClientSecret: testBridgeClientSecret,
		NewAPIRedirectURI: testNewAPICallback, BaseSubscription: testBaseSubscriptionConfig,
	}, store, &bridgeTestProvider{}, sealer)

	failed := requestInternalUserInfo(handler, http.MethodGet, accessHandle)
	if failed.Code != http.StatusServiceUnavailable ||
		failed.Body.String() != "{\"error\":\"temporarily_unavailable\"}\n" {
		t.Fatalf("failed base plan status=%d body=%s, want temporarily_unavailable",
			failed.Code, failed.Body.String())
	}
	retried := requestInternalUserInfo(handler, http.MethodGet, accessHandle)
	if retried.Code != http.StatusOK {
		t.Fatalf("userinfo after planning recovery status=%d body=%s, want unconsumed handle",
			retried.Code, retried.Body.String())
	}
}

func TestInternalUserInfoReusesBaseGrantAcrossDistinctLogins(t *testing.T) {
	fixture := newInternalOAuthFixture(t, 0)
	firstHandle, err := fixture.store.ExchangeOAuthLoginCode(context.Background(), fixture.loginCode)
	if err != nil {
		t.Fatalf("exchange first login code: %v", err)
	}
	if response := requestInternalUserInfo(fixture.handler, http.MethodGet, firstHandle); response.Code != http.StatusOK {
		t.Fatalf("first userinfo status=%d body=%s", response.Code, response.Body.String())
	}
	externalID := "lark:base:" + fixture.identity.Subject + ":employee-v1"
	firstJob, err := fixture.store.GetEntitlementGrantJob(context.Background(), externalID)
	if err != nil {
		t.Fatalf("get first base grant job: %v", err)
	}
	secondCode, err := fixture.store.CreateOAuthLoginCode(context.Background(), fixture.identity)
	if err != nil {
		t.Fatalf("create second login code: %v", err)
	}
	secondHandle, err := fixture.store.ExchangeOAuthLoginCode(context.Background(), secondCode)
	if err != nil {
		t.Fatalf("exchange second login code: %v", err)
	}
	if response := requestInternalUserInfo(fixture.handler, http.MethodGet, secondHandle); response.Code != http.StatusOK {
		t.Fatalf("second userinfo status=%d body=%s", response.Code, response.Body.String())
	}
	secondJob, err := fixture.store.GetEntitlementGrantJob(context.Background(), externalID)
	if err != nil {
		t.Fatalf("get replayed base grant job: %v", err)
	}
	if secondJob.ID != firstJob.ID || !bytes.Equal(secondJob.Nonce, firstJob.Nonce) ||
		!bytes.Equal(secondJob.Ciphertext, firstJob.Ciphertext) {
		t.Fatalf("repeat login replaced base grant job: first=%+v second=%+v", firstJob, secondJob)
	}
}

func TestInternalUserInfoRejectsMalformedAuthorizationWithoutConsumingHandle(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		setHeader func(*http.Request, string)
	}{
		{name: "missing", target: "/internal/oauth/userinfo", setHeader: func(*http.Request, string) {}},
		{
			name: "wrong scheme", target: "/internal/oauth/userinfo",
			setHeader: func(request *http.Request, handle string) {
				request.Header.Set("Authorization", "Basic "+handle)
			},
		},
		{
			name: "duplicate", target: "/internal/oauth/userinfo",
			setHeader: func(request *http.Request, handle string) {
				request.Header.Add("Authorization", "Bearer "+handle)
				request.Header.Add("Authorization", "Bearer second-handle")
			},
		},
		{
			name: "query parameter", target: "/internal/oauth/userinfo?access_token=forbidden",
			setHeader: func(request *http.Request, handle string) {
				request.Header.Set("Authorization", "Bearer "+handle)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInternalOAuthFixture(t, 0)
			handle, err := fixture.store.ExchangeOAuthLoginCode(context.Background(), fixture.loginCode)
			if err != nil {
				t.Fatalf("exchange login code: %v", err)
			}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.target, nil)
			test.setHeader(request, handle)
			fixture.handler.ServeHTTP(response, request)
			wantStatus := http.StatusUnauthorized
			wantBody := "{\"error\":\"invalid_token\"}\n"
			if test.name == "query parameter" {
				wantStatus = http.StatusBadRequest
				wantBody = "{\"error\":\"invalid_request\"}\n"
			}
			if response.Code != wantStatus || response.Body.String() != wantBody {
				t.Fatalf("malformed userinfo status=%d body=%s, want %d/%s",
					response.Code, response.Body.String(), wantStatus, wantBody)
			}
			if accepted := requestInternalUserInfo(fixture.handler, http.MethodGet, handle); accepted.Code != http.StatusOK {
				t.Fatalf("canonical userinfo after rejection status=%d body=%s, want unconsumed handle",
					accepted.Code, accepted.Body.String())
			}
		})
	}
}

func TestInternalEndpointMethodsRejectBeforeCredentialConsumption(t *testing.T) {
	fixture := newInternalOAuthFixture(t, 0)
	form := validTokenForm(fixture.loginCode)
	headToken := requestInternalToken(fixture.handler, http.MethodHead, form)
	assertPrivateOAuthResponse(t, headToken.Header())
	if headToken.Code != http.StatusMethodNotAllowed || headToken.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("HEAD token status=%d allow=%q, want 405/POST",
			headToken.Code, headToken.Header().Get("Allow"))
	}
	acceptedToken := requestInternalToken(fixture.handler, http.MethodPost, form)
	if acceptedToken.Code != http.StatusOK {
		t.Fatalf("POST token after HEAD status=%d body=%s, want unconsumed code",
			acceptedToken.Code, acceptedToken.Body.String())
	}
	var tokenPayload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(acceptedToken.Body.Bytes(), &tokenPayload); err != nil {
		t.Fatalf("decode token after HEAD: %v", err)
	}
	headUserInfo := requestInternalUserInfo(fixture.handler, http.MethodHead, tokenPayload.AccessToken)
	assertPrivateOAuthResponse(t, headUserInfo.Header())
	if headUserInfo.Code != http.StatusMethodNotAllowed || headUserInfo.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("HEAD userinfo status=%d allow=%q, want 405/GET",
			headUserInfo.Code, headUserInfo.Header().Get("Allow"))
	}
	postUserInfo := requestInternalUserInfo(fixture.handler, http.MethodPost, tokenPayload.AccessToken)
	assertPrivateOAuthResponse(t, postUserInfo.Header())
	if postUserInfo.Code != http.StatusMethodNotAllowed ||
		postUserInfo.Header().Get("Allow") != http.MethodGet ||
		postUserInfo.Body.String() != "{\"error\":\"invalid_request\"}\n" {
		t.Fatalf("POST userinfo status=%d allow=%q body=%s, want stable 405/GET",
			postUserInfo.Code, postUserInfo.Header().Get("Allow"), postUserInfo.Body.String())
	}
	if accepted := requestInternalUserInfo(fixture.handler, http.MethodGet, tokenPayload.AccessToken); accepted.Code != http.StatusOK {
		t.Fatalf("GET userinfo after HEAD status=%d body=%s, want unconsumed handle",
			accepted.Code, accepted.Body.String())
	}
}

func TestInternalTokenAndUserInfoUseIndependentRateLimits(t *testing.T) {
	fixture := newInternalOAuthFixture(t, 2)
	form := validTokenForm("invalid-login-code")
	for attempt := 1; attempt <= 3; attempt++ {
		response := requestInternalToken(fixture.handler, http.MethodPost, form)
		if attempt <= 2 && response.Code != http.StatusBadRequest {
			t.Fatalf("token attempt %d status=%d, want invalid_grant", attempt, response.Code)
		}
		if attempt == 3 && (response.Code != http.StatusTooManyRequests ||
			response.Header().Get("Retry-After") != "60") {
			t.Fatalf("token attempt 3 status=%d retry-after=%q, want 429/60",
				response.Code, response.Header().Get("Retry-After"))
		}
	}
	for attempt := 1; attempt <= 3; attempt++ {
		response := requestInternalUserInfo(fixture.handler, http.MethodGet, "invalid-access-handle")
		if attempt <= 2 && response.Code != http.StatusUnauthorized {
			t.Fatalf("userinfo attempt %d status=%d, want invalid_token", attempt, response.Code)
		}
		if attempt == 3 && (response.Code != http.StatusTooManyRequests ||
			response.Header().Get("Retry-After") != "60") {
			t.Fatalf("userinfo attempt 3 status=%d retry-after=%q, want 429/60",
				response.Code, response.Header().Get("Retry-After"))
		}
	}
}

type internalOAuthFixture struct {
	handler   http.Handler
	store     *inbox.Store
	identity  inbox.OAuthIdentity
	loginCode string
}

func newInternalOAuthFixture(t *testing.T, rateLimit int) internalOAuthFixture {
	t.Helper()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity, err := inbox.NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("new identity: %v", err)
	}
	loginCode, err := store.CreateOAuthLoginCode(context.Background(), identity)
	if err != nil {
		t.Fatalf("create login code: %v", err)
	}
	handler := newBridgeTestHandler(t, oauthbridge.Config{
		BridgeClientID: "bridge-client-id", BridgeClientSecret: testBridgeClientSecret,
		NewAPIRedirectURI: testNewAPICallback, RateLimitPerMinute: rateLimit,
	}, store, &bridgeTestProvider{})
	return internalOAuthFixture{handler: handler, store: store, identity: identity, loginCode: loginCode}
}

func validTokenForm(loginCode string) url.Values {
	return url.Values{
		"grant_type": {"authorization_code"}, "code": {loginCode},
		"redirect_uri": {testNewAPICallback}, "client_id": {"bridge-client-id"},
		"client_secret": {testBridgeClientSecret},
	}
}

func requestInternalToken(handler http.Handler, method string, form url.Values) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/internal/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.RemoteAddr = "198.51.100.10:1234"
	handler.ServeHTTP(response, request)
	return response
}

func requestInternalUserInfo(handler http.Handler, method, accessHandle string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/internal/oauth/userinfo", nil)
	request.Header.Set("Authorization", "Bearer "+accessHandle)
	request.RemoteAddr = "198.51.100.10:1234"
	handler.ServeHTTP(response, request)
	return response
}

type failOnceGrantSealer struct {
	delegate *newapi.GrantSealer
	calls    int
}

func (s *failOnceGrantSealer) Seal(
	request newapi.EntitlementGrantRequest,
) (newapi.SealedGrantRequest, error) {
	s.calls++
	if s.calls == 1 {
		return newapi.SealedGrantRequest{}, errors.New("grant sealer temporarily unavailable")
	}
	return s.delegate.Seal(request)
}
