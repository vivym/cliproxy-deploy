package oauthbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/larkapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/oauthbridge"
)

const (
	testControllerCallback = "https://ai.x2r.store/integrations/lark/oauth/callback"
	testNewAPICallback     = "https://ai.x2r.store/oauth/lark"
)

func TestBrowserCanCompleteLarkAuthorizationAndReceiveOpaqueLoginCode(t *testing.T) {
	var tokenRequests int
	var userInfoRequests int
	larkServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth/v3/token":
			tokenRequests++
			writeBridgeTestJSON(t, response, map[string]any{
				"code": 0, "access_token": "user-access-token", "refresh_token": "must-not-escape",
			})
		case "/open-apis/authen/v1/user_info":
			userInfoRequests++
			writeBridgeTestJSON(t, response, map[string]any{
				"code": 0,
				"data": map[string]any{
					"name": "Employee", "open_id": "ou_employee", "tenant_key": "tenant-test",
				},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer larkServer.Close()

	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exchanger, err := larkapi.NewOAuthExchanger(larkapi.OAuthConfig{
		AppID: "cli_test", AppSecret: "app-secret", RedirectURI: testControllerCallback,
		TenantKey: "tenant-test", OAuthBaseURL: larkServer.URL, OpenBaseURL: larkServer.URL,
	})
	if err != nil {
		t.Fatalf("new OAuth exchanger: %v", err)
	}
	handler, err := oauthbridge.NewHandler(oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
	}, store, exchanger)
	if err != nil {
		t.Fatalf("new OAuth bridge handler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)

	authorizeQuery := make(url.Values)
	authorizeQuery.Set("response_type", "code")
	authorizeQuery.Set("client_id", "bridge-client-id")
	authorizeQuery.Set("redirect_uri", testNewAPICallback)
	authorizeQuery.Set("state", "new-api-state")
	authorizeQuery.Set("scope", "openid profile email")
	authorizeQuery.Set("affiliate_code", "must-not-propagate")
	authorize := httptest.NewRecorder()
	mux.ServeHTTP(authorize, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/authorize?"+authorizeQuery.Encode(),
		nil,
	))
	if authorize.Code != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302; body=%s", authorize.Code, authorize.Body.String())
	}
	assertPrivateOAuthResponse(t, authorize.Header())
	larkLocation, err := url.Parse(authorize.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Lark redirect: %v", err)
	}
	if larkLocation.Scheme+"://"+larkLocation.Host != larkServer.URL ||
		larkLocation.Path != "/open-apis/authen/v1/authorize" {
		t.Fatalf("Lark redirect = %s", larkLocation)
	}
	wantLarkQuery := url.Values{
		"app_id":       {"cli_test"},
		"redirect_uri": {testControllerCallback},
		"state":        {larkLocation.Query().Get("state")},
	}
	if larkLocation.Query().Encode() != wantLarkQuery.Encode() || len(larkLocation.Query().Get("state")) != 43 {
		t.Fatalf("Lark authorize query = %q, want fixed minimal query", larkLocation.RawQuery)
	}

	callbackQuery := make(url.Values)
	callbackQuery.Set("code", "lark-authorization-code")
	callbackQuery.Set("state", larkLocation.Query().Get("state"))
	callback := httptest.NewRecorder()
	mux.ServeHTTP(callback, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/callback?"+callbackQuery.Encode(),
		nil,
	))
	if callback.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want 302; body=%s", callback.Code, callback.Body.String())
	}
	assertPrivateOAuthResponse(t, callback.Header())
	newAPILocation, err := url.Parse(callback.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse New API redirect: %v", err)
	}
	if newAPILocation.Scheme+"://"+newAPILocation.Host+newAPILocation.Path != testNewAPICallback ||
		newAPILocation.Query().Get("state") != "new-api-state" ||
		len(newAPILocation.Query().Get("code")) != 43 || newAPILocation.Query().Get("error") != "" {
		t.Fatalf("New API callback redirect = %s", newAPILocation)
	}
	if tokenRequests != 1 || userInfoRequests != 1 {
		t.Fatalf("Lark requests token=%d userinfo=%d, want 1 each", tokenRequests, userInfoRequests)
	}
}

func TestAuthorizeFailsClosedOnInvalidBridgeRequest(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exchanger, err := larkapi.NewOAuthExchanger(larkapi.OAuthConfig{
		AppID: "cli_test", AppSecret: "app-secret", RedirectURI: testControllerCallback,
		TenantKey: "tenant-test", OAuthBaseURL: "http://127.0.0.1:1", OpenBaseURL: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("new OAuth exchanger: %v", err)
	}
	handler, err := oauthbridge.NewHandler(oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
	}, store, exchanger)
	if err != nil {
		t.Fatalf("new OAuth bridge handler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)

	valid := func() url.Values {
		query := make(url.Values)
		query.Set("response_type", "code")
		query.Set("client_id", "bridge-client-id")
		query.Set("redirect_uri", testNewAPICallback)
		query.Set("state", "new-api-state")
		return query
	}
	tests := []struct {
		name   string
		mutate func(url.Values)
	}{
		{name: "wrong response type", mutate: func(query url.Values) { query.Set("response_type", "token") }},
		{name: "wrong client id", mutate: func(query url.Values) { query.Set("client_id", "other-client") }},
		{name: "callback prefix attack", mutate: func(query url.Values) {
			query.Set("redirect_uri", testNewAPICallback+"/attacker")
		}},
		{name: "missing state", mutate: func(query url.Values) { query.Del("state") }},
		{name: "duplicate redirect", mutate: func(query url.Values) {
			query["redirect_uri"] = append(query["redirect_uri"], "https://attacker.example/callback")
		}},
		{name: "state newline", mutate: func(query url.Values) { query.Set("state", "state\ninjected") }},
		{name: "oversized state", mutate: func(query url.Values) { query.Set("state", strings.Repeat("s", 1025)) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			query := valid()
			test.mutate(query)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(
				http.MethodGet,
				"/integrations/lark/oauth/authorize?"+query.Encode(),
				nil,
			))
			if response.Code != http.StatusBadRequest || response.Body.String() != "{\"error\":\"invalid_request\"}\n" {
				t.Fatalf("response status=%d body=%s, want stable invalid_request", response.Code, response.Body.String())
			}
			assertPrivateOAuthResponse(t, response.Header())
		})
	}
}

func TestCallbackConsumesStateAndRedactsLarkAuthorizationDenial(t *testing.T) {
	var larkAPIRequests int
	larkServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		larkAPIRequests++
		http.Error(response, "must not be called", http.StatusInternalServerError)
	}))
	defer larkServer.Close()

	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exchanger, err := larkapi.NewOAuthExchanger(larkapi.OAuthConfig{
		AppID: "cli_test", AppSecret: "app-secret", RedirectURI: testControllerCallback,
		TenantKey: "tenant-test", OAuthBaseURL: larkServer.URL, OpenBaseURL: larkServer.URL,
	})
	if err != nil {
		t.Fatalf("new OAuth exchanger: %v", err)
	}
	handler, err := oauthbridge.NewHandler(oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
	}, store, exchanger)
	if err != nil {
		t.Fatalf("new OAuth bridge handler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)

	authorizeQuery := url.Values{
		"response_type": {"code"}, "client_id": {"bridge-client-id"},
		"redirect_uri": {testNewAPICallback}, "state": {"new-api-state"},
	}
	authorize := httptest.NewRecorder()
	mux.ServeHTTP(authorize, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/authorize?"+authorizeQuery.Encode(),
		nil,
	))
	larkLocation, err := url.Parse(authorize.Header().Get("Location"))
	if err != nil || larkLocation.Query().Get("state") == "" {
		t.Fatalf("authorize location = %q, error = %v", authorize.Header().Get("Location"), err)
	}

	denialQuery := url.Values{
		"error": {"access_denied"}, "error_description": {"sensitive Lark description"},
		"state": {larkLocation.Query().Get("state")},
	}
	denial := httptest.NewRecorder()
	mux.ServeHTTP(denial, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/callback?"+denialQuery.Encode(),
		nil,
	))
	if denial.Code != http.StatusFound {
		t.Fatalf("denial status = %d, want 302; body=%s", denial.Code, denial.Body.String())
	}
	denialLocation, err := url.Parse(denial.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse denial redirect: %v", err)
	}
	wantDenialQuery := url.Values{"error": {"access_denied"}, "state": {"new-api-state"}}
	if denialLocation.Scheme+"://"+denialLocation.Host+denialLocation.Path != testNewAPICallback ||
		denialLocation.Query().Encode() != wantDenialQuery.Encode() ||
		strings.Contains(denial.Header().Get("Location"), "sensitive") {
		t.Fatalf("denial redirect = %s, want redacted stable access_denied", denialLocation)
	}

	replay := httptest.NewRecorder()
	mux.ServeHTTP(replay, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/callback?code=lark-code&state="+
			url.QueryEscape(larkLocation.Query().Get("state")),
		nil,
	))
	if replay.Code != http.StatusBadRequest || replay.Body.String() != "{\"error\":\"invalid_state\"}\n" {
		t.Fatalf("replay response status=%d body=%s, want invalid_state", replay.Code, replay.Body.String())
	}
	if larkAPIRequests != 0 {
		t.Fatalf("denial/replay made %d Lark API request(s), want 0", larkAPIRequests)
	}
}

func TestCallbackClassifiesLarkAuthorizationErrorsWithoutForwardingDetails(t *testing.T) {
	tests := []struct {
		name      string
		larkError string
		want      string
	}{
		{name: "access denied", larkError: "access_denied", want: "access_denied"},
		{name: "server error", larkError: "server_error", want: "temporarily_unavailable"},
		{name: "temporarily unavailable", larkError: "temporarily_unavailable", want: "temporarily_unavailable"},
		{name: "unknown", larkError: "unexpected_sensitive_error", want: "server_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &bridgeTestStore{state: inbox.OAuthAuthorizationState{
				NewAPIState: "new-api-state", RedirectURI: testNewAPICallback,
			}}
			provider := &bridgeTestProvider{}
			handler := newBridgeTestHandler(t, oauthbridge.Config{
				BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
			}, store, provider)
			query := url.Values{
				"error": {test.larkError}, "error_description": {"sensitive upstream detail"},
				"state": {"internal-state"},
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(
				http.MethodGet,
				"/integrations/lark/oauth/callback?"+query.Encode(),
				nil,
			))
			location, err := url.Parse(response.Header().Get("Location"))
			if response.Code != http.StatusFound || err != nil ||
				location.Query().Get("error") != test.want ||
				location.Query().Get("state") != "new-api-state" ||
				strings.Contains(response.Header().Get("Location"), "sensitive") ||
				strings.Contains(response.Header().Get("Location"), "unexpected_sensitive_error") {
				t.Fatalf("callback status=%d location=%q error=%v, want %s",
					response.Code, response.Header().Get("Location"), err, test.want)
			}
			if provider.exchangeCalls != 0 {
				t.Fatalf("authorization error made %d exchange call(s), want 0", provider.exchangeCalls)
			}
		})
	}
}

func TestOAuthEndpointsRejectHEADWithoutSideEffects(t *testing.T) {
	store := &bridgeTestStore{state: inbox.OAuthAuthorizationState{
		NewAPIState: "new-api-state", RedirectURI: testNewAPICallback,
	}}
	provider := &bridgeTestProvider{identity: bridgeTestIdentity(t)}
	handler := newBridgeTestHandler(t, oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
	}, store, provider)
	paths := []string{
		"/integrations/lark/oauth/authorize?response_type=code&client_id=bridge-client-id" +
			"&redirect_uri=" + url.QueryEscape(testNewAPICallback) + "&state=new-api-state",
		"/integrations/lark/oauth/callback?code=lark-code&state=internal-state",
	}
	for _, path := range paths {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodHead, path, nil))
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
			t.Fatalf("HEAD %s status=%d allow=%q, want 405 Allow GET",
				path, response.Code, response.Header().Get("Allow"))
		}
		assertPrivateOAuthResponse(t, response.Header())
	}
	if store.createStateCalls != 0 || store.consumeStateCalls != 0 || store.createLoginCodeCalls != 0 ||
		provider.authorizationURLCalls != 0 || provider.exchangeCalls != 0 {
		t.Fatalf("HEAD caused side effects: store=%+v provider=%+v", store, provider)
	}
}

func TestCallbackDistinguishesUnavailableStateStoreFromInvalidState(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	exchanger, err := larkapi.NewOAuthExchanger(larkapi.OAuthConfig{
		AppID: "cli_test", AppSecret: "app-secret", RedirectURI: testControllerCallback,
		TenantKey: "tenant-test", OAuthBaseURL: "http://127.0.0.1:1", OpenBaseURL: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("new OAuth exchanger: %v", err)
	}
	handler, err := oauthbridge.NewHandler(oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
	}, store, exchanger)
	if err != nil {
		t.Fatalf("new OAuth bridge handler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	malformed := httptest.NewRecorder()
	mux.ServeHTTP(malformed, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/callback?code=lark-code&state=not-a-generated-state",
		nil,
	))
	if malformed.Code != http.StatusBadRequest ||
		malformed.Body.String() != "{\"error\":\"invalid_state\"}\n" {
		t.Fatalf("malformed state response status=%d body=%s, want invalid_state",
			malformed.Code, malformed.Body.String())
	}
	authorizeQuery := url.Values{
		"response_type": {"code"}, "client_id": {"bridge-client-id"},
		"redirect_uri": {testNewAPICallback}, "state": {"new-api-state"},
	}
	authorize := httptest.NewRecorder()
	mux.ServeHTTP(authorize, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/authorize?"+authorizeQuery.Encode(),
		nil,
	))
	larkLocation, err := url.Parse(authorize.Header().Get("Location"))
	if err != nil || larkLocation.Query().Get("state") == "" {
		t.Fatalf("authorize location = %q, error = %v", authorize.Header().Get("Location"), err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	callback := httptest.NewRecorder()
	mux.ServeHTTP(callback, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/callback?code=lark-code&state="+
			url.QueryEscape(larkLocation.Query().Get("state")),
		nil,
	))
	if callback.Code != http.StatusServiceUnavailable ||
		callback.Body.String() != "{\"error\":\"temporarily_unavailable\"}\n" ||
		callback.Header().Get("Location") != "" {
		t.Fatalf("callback status=%d location=%q body=%s, want retryable store error",
			callback.Code, callback.Header().Get("Location"), callback.Body.String())
	}
}

func TestCallbackRejectsAmbiguousOrOversizedCodeBeforeLarkExchange(t *testing.T) {
	var larkAPIRequests int
	larkServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		larkAPIRequests++
		http.Error(response, "must not be called", http.StatusInternalServerError)
	}))
	defer larkServer.Close()

	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exchanger, err := larkapi.NewOAuthExchanger(larkapi.OAuthConfig{
		AppID: "cli_test", AppSecret: "app-secret", RedirectURI: testControllerCallback,
		TenantKey: "tenant-test", OAuthBaseURL: larkServer.URL, OpenBaseURL: larkServer.URL,
	})
	if err != nil {
		t.Fatalf("new OAuth exchanger: %v", err)
	}
	handler, err := oauthbridge.NewHandler(oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
	}, store, exchanger)
	if err != nil {
		t.Fatalf("new OAuth bridge handler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)

	tests := []struct {
		name  string
		query url.Values
	}{
		{name: "oversized code", query: url.Values{"code": {strings.Repeat("c", 4097)}}},
		{name: "duplicate code", query: url.Values{"code": {"first", "second"}}},
		{name: "code and error", query: url.Values{"code": {"lark-code"}, "error": {"access_denied"}}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalState := "new-api-state-" + string(rune('a'+index))
			authorizeQuery := url.Values{
				"response_type": {"code"}, "client_id": {"bridge-client-id"},
				"redirect_uri": {testNewAPICallback}, "state": {originalState},
			}
			authorize := httptest.NewRecorder()
			mux.ServeHTTP(authorize, httptest.NewRequest(
				http.MethodGet,
				"/integrations/lark/oauth/authorize?"+authorizeQuery.Encode(),
				nil,
			))
			larkLocation, err := url.Parse(authorize.Header().Get("Location"))
			if err != nil || larkLocation.Query().Get("state") == "" {
				t.Fatalf("authorize location = %q, error = %v", authorize.Header().Get("Location"), err)
			}
			callbackQuery := test.query
			callbackQuery.Set("state", larkLocation.Query().Get("state"))
			callback := httptest.NewRecorder()
			mux.ServeHTTP(callback, httptest.NewRequest(
				http.MethodGet,
				"/integrations/lark/oauth/callback?"+callbackQuery.Encode(),
				nil,
			))
			location, err := url.Parse(callback.Header().Get("Location"))
			if callback.Code != http.StatusFound || err != nil ||
				location.Query().Get("error") != "invalid_request" ||
				location.Query().Get("state") != originalState || location.Query().Get("code") != "" {
				t.Fatalf("callback status=%d location=%q error=%v, want invalid_request redirect",
					callback.Code, callback.Header().Get("Location"), err)
			}
		})
	}
	if larkAPIRequests != 0 {
		t.Fatalf("invalid callbacks made %d Lark API request(s), want 0", larkAPIRequests)
	}
}

func TestCallbackMapsLarkUpstreamFailureToRedactedRetryableError(t *testing.T) {
	larkServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/oauth/v3/token" {
			http.NotFound(response, request)
			return
		}
		response.WriteHeader(http.StatusServiceUnavailable)
		writeBridgeTestJSON(t, response, map[string]any{
			"code": 999999, "msg": "sensitive Lark outage detail",
		})
	}))
	defer larkServer.Close()

	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exchanger, err := larkapi.NewOAuthExchanger(larkapi.OAuthConfig{
		AppID: "cli_test", AppSecret: "app-secret", RedirectURI: testControllerCallback,
		TenantKey: "tenant-test", OAuthBaseURL: larkServer.URL, OpenBaseURL: larkServer.URL,
	})
	if err != nil {
		t.Fatalf("new OAuth exchanger: %v", err)
	}
	handler, err := oauthbridge.NewHandler(oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
	}, store, exchanger)
	if err != nil {
		t.Fatalf("new OAuth bridge handler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	authorizeQuery := url.Values{
		"response_type": {"code"}, "client_id": {"bridge-client-id"},
		"redirect_uri": {testNewAPICallback}, "state": {"new-api-state"},
	}
	authorize := httptest.NewRecorder()
	mux.ServeHTTP(authorize, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/authorize?"+authorizeQuery.Encode(),
		nil,
	))
	larkLocation, err := url.Parse(authorize.Header().Get("Location"))
	if err != nil || larkLocation.Query().Get("state") == "" {
		t.Fatalf("authorize location = %q, error = %v", authorize.Header().Get("Location"), err)
	}
	callbackQuery := url.Values{
		"code": {"lark-code"}, "state": {larkLocation.Query().Get("state")},
	}
	callback := httptest.NewRecorder()
	mux.ServeHTTP(callback, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/callback?"+callbackQuery.Encode(),
		nil,
	))
	location, err := url.Parse(callback.Header().Get("Location"))
	if callback.Code != http.StatusFound || err != nil ||
		location.Query().Get("error") != "temporarily_unavailable" ||
		location.Query().Get("state") != "new-api-state" ||
		strings.Contains(callback.Header().Get("Location"), "sensitive") ||
		strings.Contains(callback.Header().Get("Location"), "999999") {
		t.Fatalf("callback status=%d location=%q error=%v, want redacted retryable error",
			callback.Code, callback.Header().Get("Location"), err)
	}
}

func TestCallbackMapsTerminalExchangeFailureToRedactedServerError(t *testing.T) {
	store := &bridgeTestStore{
		state: inbox.OAuthAuthorizationState{
			NewAPIState: "new-api-state", RedirectURI: testNewAPICallback,
		},
	}
	provider := &bridgeTestProvider{
		exchangeErr: &larkapi.OAuthExchangeError{Reason: larkapi.OAuthTokenRejected},
	}
	handler := newBridgeTestHandler(t, oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
	}, store, provider)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/callback?code=sensitive-lark-code&state=internal-state",
		nil,
	))
	location, err := url.Parse(response.Header().Get("Location"))
	if response.Code != http.StatusFound || err != nil ||
		location.Query().Get("error") != "server_error" ||
		location.Query().Get("state") != "new-api-state" ||
		strings.Contains(response.Header().Get("Location"), "sensitive") ||
		strings.Contains(response.Header().Get("Location"), "token_rejected") {
		t.Fatalf("callback status=%d location=%q error=%v, want redacted server_error",
			response.Code, response.Header().Get("Location"), err)
	}
}

func TestCallbackMapsLoginCodePersistenceFailureToRetryableError(t *testing.T) {
	store := &bridgeTestStore{
		state: inbox.OAuthAuthorizationState{
			NewAPIState: "new-api-state", RedirectURI: testNewAPICallback,
		},
		loginCodeErr: errors.New("database unavailable with sensitive detail"),
	}
	provider := &bridgeTestProvider{identity: bridgeTestIdentity(t)}
	handler := newBridgeTestHandler(t, oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
	}, store, provider)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/callback?code=lark-code&state=internal-state",
		nil,
	))
	location, err := url.Parse(response.Header().Get("Location"))
	if response.Code != http.StatusFound || err != nil ||
		location.Query().Get("error") != "temporarily_unavailable" ||
		location.Query().Get("state") != "new-api-state" ||
		strings.Contains(response.Header().Get("Location"), "sensitive") {
		t.Fatalf("callback status=%d location=%q error=%v, want retryable redacted error",
			response.Code, response.Header().Get("Location"), err)
	}
}

func TestOAuthEndpointsUseIndependentPerClientRateLimits(t *testing.T) {
	store := &bridgeTestStore{
		state: inbox.OAuthAuthorizationState{
			NewAPIState: "new-api-state", RedirectURI: testNewAPICallback,
		},
		loginCode: "opaque-login-code",
	}
	provider := &bridgeTestProvider{identity: bridgeTestIdentity(t)}
	handler := newBridgeTestHandler(t, oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
		RateLimitPerMinute: 2,
	}, store, provider)

	authorizeURL := "/integrations/lark/oauth/authorize?response_type=code" +
		"&client_id=bridge-client-id&redirect_uri=" + url.QueryEscape(testNewAPICallback) +
		"&state=new-api-state"
	for attempt := 1; attempt <= 3; attempt++ {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
		request.RemoteAddr = "198.51.100.10:1234"
		handler.ServeHTTP(response, request)
		if attempt <= 2 && response.Code != http.StatusFound {
			t.Fatalf("authorize attempt %d status=%d, want 302", attempt, response.Code)
		}
		if attempt == 3 && (response.Code != http.StatusTooManyRequests ||
			response.Header().Get("Retry-After") != "60" ||
			response.Body.String() != "{\"error\":\"rate_limited\"}\n") {
			t.Fatalf("authorize attempt 3 status=%d body=%s, want rate_limited",
				response.Code, response.Body.String())
		}
	}

	callback := httptest.NewRecorder()
	callbackRequest := httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/callback?code=lark-code&state=internal-state",
		nil,
	)
	callbackRequest.RemoteAddr = "198.51.100.10:1234"
	handler.ServeHTTP(callback, callbackRequest)
	if callback.Code != http.StatusFound {
		t.Fatalf("callback shared authorize limiter: status=%d body=%s", callback.Code, callback.Body.String())
	}

	otherClient := httptest.NewRecorder()
	otherRequest := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
	otherRequest.RemoteAddr = "198.51.100.11:1234"
	handler.ServeHTTP(otherClient, otherRequest)
	if otherClient.Code != http.StatusFound {
		t.Fatalf("other client shared limiter: status=%d body=%s", otherClient.Code, otherClient.Body.String())
	}
}

func TestOAuthRateLimiterTrustsForwardedClientOnlyFromConfiguredProxy(t *testing.T) {
	store := &bridgeTestStore{}
	provider := &bridgeTestProvider{}
	handler := newBridgeTestHandler(t, oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
		RateLimitPerMinute: 1,
		TrustedProxyCIDRs:  []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
	}, store, provider)
	authorizeURL := "/integrations/lark/oauth/authorize?response_type=code" +
		"&client_id=bridge-client-id&redirect_uri=" + url.QueryEscape(testNewAPICallback) +
		"&state=new-api-state"

	for _, client := range []string{"198.51.100.1", "198.51.100.2"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
		request.RemoteAddr = "192.0.2.10:1234"
		request.Header.Set("X-Forwarded-For", client)
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusFound {
			t.Fatalf("trusted proxy client %s status=%d, want 302", client, response.Code)
		}
	}

	for attempt, spoofed := range []string{"198.51.100.3", "198.51.100.4"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
		request.RemoteAddr = "203.0.113.10:1234"
		request.Header.Set("X-Forwarded-For", spoofed)
		handler.ServeHTTP(response, request)
		if attempt == 0 && response.Code != http.StatusFound {
			t.Fatalf("first untrusted direct request status=%d, want 302", response.Code)
		}
		if attempt == 1 && response.Code != http.StatusTooManyRequests {
			t.Fatalf("spoofed forwarded address bypassed limit: status=%d", response.Code)
		}
	}
}

func TestOAuthRateLimiterAggregatesIPv6ClientsBy64BitPrefix(t *testing.T) {
	handler := newBridgeTestHandler(t, oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
		RateLimitPerMinute: 1,
	}, &bridgeTestStore{}, &bridgeTestProvider{})
	authorizeURL := "/integrations/lark/oauth/authorize?response_type=code" +
		"&client_id=bridge-client-id&redirect_uri=" + url.QueryEscape(testNewAPICallback) +
		"&state=new-api-state"
	for attempt, remoteAddress := range []string{
		"[2001:db8:1234:5678::1]:1234",
		"[2001:db8:1234:5678:ffff::2]:1234",
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
		request.RemoteAddr = remoteAddress
		handler.ServeHTTP(response, request)
		if attempt == 0 && response.Code != http.StatusFound {
			t.Fatalf("first IPv6 address status=%d, want 302", response.Code)
		}
		if attempt == 1 && response.Code != http.StatusTooManyRequests {
			t.Fatalf("rotated IPv6 address bypassed /64 rate limit: status=%d", response.Code)
		}
	}
}

func TestAuthorizeBoundsStateIssuancePerClientAndGlobally(t *testing.T) {
	authorizeURL := "/integrations/lark/oauth/authorize?response_type=code" +
		"&client_id=bridge-client-id&redirect_uri=" + url.QueryEscape(testNewAPICallback) +
		"&state=new-api-state"

	t.Run("per client", func(t *testing.T) {
		store := &bridgeTestStore{}
		handler := newBridgeTestHandler(t, oauthbridge.Config{
			BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
			RateLimitPerMinute: 1000,
		}, store, &bridgeTestProvider{})
		for attempt := 1; attempt <= 21; attempt++ {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
			request.RemoteAddr = "198.51.100.10:1234"
			handler.ServeHTTP(response, request)
			if attempt <= 20 && response.Code != http.StatusFound {
				t.Fatalf("state attempt %d status=%d, want 302", attempt, response.Code)
			}
			if attempt == 21 && (response.Code != http.StatusTooManyRequests ||
				response.Header().Get("Retry-After") != "300") {
				t.Fatalf("state attempt 21 status=%d retry-after=%q, want 429/300",
					response.Code, response.Header().Get("Retry-After"))
			}
		}
		if store.createStateCalls != 20 {
			t.Fatalf("created states=%d, want per-client cap 20", store.createStateCalls)
		}
	})

	t.Run("global", func(t *testing.T) {
		store := &bridgeTestStore{}
		handler := newBridgeTestHandler(t, oauthbridge.Config{
			BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
			RateLimitPerMinute: 1000,
		}, store, &bridgeTestProvider{})
		for attempt := 0; attempt <= 500; attempt++ {
			client := attempt / 20
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
			request.RemoteAddr = "198.51.100." + strconv.Itoa(client+1) + ":1234"
			handler.ServeHTTP(response, request)
			if attempt < 500 && response.Code != http.StatusFound {
				t.Fatalf("global state attempt %d status=%d, want 302", attempt+1, response.Code)
			}
			if attempt == 500 && (response.Code != http.StatusTooManyRequests ||
				response.Header().Get("Retry-After") != "60") {
				t.Fatalf("global state attempt 501 status=%d retry-after=%q, want 429/60",
					response.Code, response.Header().Get("Retry-After"))
			}
		}
		if store.createStateCalls != 500 {
			t.Fatalf("created states=%d, want global cap 500/minute", store.createStateCalls)
		}
	})
}

func TestAuthorizeRollsBackRejectedStateReservations(t *testing.T) {
	authorizeURL := "/integrations/lark/oauth/authorize?response_type=code" +
		"&client_id=bridge-client-id&redirect_uri=" + url.QueryEscape(testNewAPICallback) +
		"&state=new-api-state"

	t.Run("global limit rejection", func(t *testing.T) {
		handler := newBridgeTestHandler(t, oauthbridge.Config{
			BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
			RateLimitPerMinute: 10000,
		}, &bridgeTestStore{}, &bridgeTestProvider{})
		for attempt := 0; attempt < 500; attempt++ {
			request := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
			request.RemoteAddr = "198.51.100." + strconv.Itoa(attempt/20+1) + ":1234"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusFound {
				t.Fatalf("global setup attempt %d status=%d, want 302", attempt+1, response.Code)
			}
		}
		for attempt := 1; attempt <= 21; attempt++ {
			request := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
			request.RemoteAddr = "203.0.113.250:1234"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "60" {
				t.Fatalf("global rejection %d status=%d retry-after=%q, want 429/60",
					attempt, response.Code, response.Header().Get("Retry-After"))
			}
		}
	})

	t.Run("store failure", func(t *testing.T) {
		store := &bridgeTestStore{createStateErr: errors.New("database unavailable")}
		handler := newBridgeTestHandler(t, oauthbridge.Config{
			BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
			RateLimitPerMinute: 1000,
		}, store, &bridgeTestProvider{})
		for attempt := 1; attempt <= 20; attempt++ {
			request := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
			request.RemoteAddr = "198.51.100.10:1234"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("store failure %d status=%d, want 503", attempt, response.Code)
			}
		}
		store.createStateErr = nil
		request := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
		request.RemoteAddr = "198.51.100.10:1234"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusFound {
			t.Fatalf("request after store recovery status=%d, want 302", response.Code)
		}
	})
}

func TestCallbackUsesBoundedRequestContext(t *testing.T) {
	store := &bridgeTestStore{
		state: inbox.OAuthAuthorizationState{
			NewAPIState: "new-api-state", RedirectURI: testNewAPICallback,
		},
	}
	provider := &bridgeTestProvider{waitForContext: true}
	handler := newBridgeTestHandler(t, oauthbridge.Config{
		BridgeClientID: "bridge-client-id", NewAPIRedirectURI: testNewAPICallback,
		CallbackTimeout: 20 * time.Millisecond,
	}, store, provider)
	response := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/callback?code=lark-code&state=internal-state",
		nil,
	))
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond || elapsed > time.Second {
		t.Fatalf("callback elapsed=%s, want bounded provider wait", elapsed)
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if response.Code != http.StatusFound || err != nil ||
		location.Query().Get("error") != "temporarily_unavailable" {
		t.Fatalf("callback status=%d location=%q error=%v, want retryable timeout",
			response.Code, response.Header().Get("Location"), err)
	}
}

func assertPrivateOAuthResponse(t *testing.T, header http.Header) {
	t.Helper()
	if header.Get("Cache-Control") != "no-store" || header.Get("Pragma") != "no-cache" ||
		header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("OAuth response cache/referrer headers = %#v", header)
	}
}

func writeBridgeTestJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Fatalf("encode test response: %v", err)
	}
}

type bridgeTestStore struct {
	state                inbox.OAuthAuthorizationState
	consumeErr           error
	createStateErr       error
	loginCode            string
	loginCodeErr         error
	createStateCalls     int
	consumeStateCalls    int
	createLoginCodeCalls int
}

func (s *bridgeTestStore) CreateOAuthAuthorizationState(
	context.Context,
	inbox.OAuthAuthorizationState,
) (string, error) {
	s.createStateCalls++
	return "internal-state", s.createStateErr
}

func (s *bridgeTestStore) ConsumeOAuthAuthorizationState(
	context.Context,
	string,
) (inbox.OAuthAuthorizationState, error) {
	s.consumeStateCalls++
	return s.state, s.consumeErr
}

func (s *bridgeTestStore) CreateOAuthLoginCode(context.Context, inbox.OAuthIdentity) (string, error) {
	s.createLoginCodeCalls++
	if s.loginCodeErr != nil {
		return "", s.loginCodeErr
	}
	if s.loginCode == "" {
		return "opaque-login-code", nil
	}
	return s.loginCode, nil
}

type bridgeTestProvider struct {
	identity              inbox.OAuthIdentity
	exchangeErr           error
	waitForContext        bool
	authorizationURLCalls int
	exchangeCalls         int
}

func (p *bridgeTestProvider) AuthorizationURL(string) (string, error) {
	p.authorizationURLCalls++
	return "https://accounts.feishu.cn/open-apis/authen/v1/authorize", nil
}

func (p *bridgeTestProvider) Exchange(ctx context.Context, _ string) (inbox.OAuthIdentity, error) {
	p.exchangeCalls++
	if p.waitForContext {
		<-ctx.Done()
		return inbox.OAuthIdentity{}, &larkapi.OAuthExchangeError{Reason: larkapi.OAuthTimeout}
	}
	return p.identity, p.exchangeErr
}

func bridgeTestIdentity(t *testing.T) inbox.OAuthIdentity {
	t.Helper()
	identity, err := inbox.NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("new OAuth identity: %v", err)
	}
	return identity
}

func newBridgeTestHandler(
	t *testing.T,
	config oauthbridge.Config,
	store oauthbridge.Store,
	provider oauthbridge.OAuthProvider,
) http.Handler {
	t.Helper()
	handler, err := oauthbridge.NewHandler(config, store, provider)
	if err != nil {
		t.Fatalf("new OAuth bridge handler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	return mux
}
