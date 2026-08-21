package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/config"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/oauthbridge"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/oauthcontract"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/webhook"
)

func TestActiveGrantRuntimeRequiresCredentialPreflight(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new grant keyring: %v", err)
	}

	grantClient, err := prepareGrantClient(config.Config{Mode: "shadow"})
	if err != nil || grantClient != nil {
		t.Fatalf("shadow grant client = %v, err=%v", grantClient, err)
	}
	grantRuntime, err := activateGrantRuntime(ctx, "shadow", store, grantClient, keyring)
	if err != nil || grantRuntime != nil {
		t.Fatalf("shadow grant runtime = %v, err=%v", grantRuntime, err)
	}

	secretPath := filepath.Join(t.TempDir(), "lark-integration.secret")
	if err := os.WriteFile(secretPath, []byte("too-short\n"), 0o600); err != nil {
		t.Fatalf("write invalid integration secret: %v", err)
	}
	activeConfig := config.Config{
		Mode: "active", NewAPIBaseURL: "http://new-api:3001", IntegrationSecretFile: secretPath,
	}
	if _, err := prepareGrantClient(activeConfig); err == nil {
		t.Fatal("active grant client accepted invalid integration secret")
	}

	if err := os.WriteFile(secretPath, []byte(strings.Repeat("a", 32)+"\n"), 0o600); err != nil {
		t.Fatalf("write valid integration secret: %v", err)
	}
	grantClient, err = prepareGrantClient(activeConfig)
	if err != nil || grantClient == nil {
		t.Fatalf("prepare active grant client: client=%v err=%v", grantClient, err)
	}
	grantRuntime, err = activateGrantRuntime(ctx, "active", store, grantClient, keyring)
	if err != nil || grantRuntime == nil {
		t.Fatalf("activate grant runtime: runtime=%v err=%v", grantRuntime, err)
	}
}

func TestWebhookAcknowledgementBudgetIncludesHeaderReadAndInboxContention(t *testing.T) {
	if controllerReadHeaderTimeout+controllerWriteTimeout >= 3*time.Second {
		t.Fatalf("server acknowledgement budget = %s, want less than 3s",
			controllerReadHeaderTimeout+controllerWriteTimeout)
	}
	databasePath := filepath.Join(t.TempDir(), "controller.sqlite")
	store, err := inbox.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	locker, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: databasePath}).String())
	if err != nil {
		t.Fatalf("open lock connection: %v", err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	lockConnection, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatalf("acquire lock connection: %v", err)
	}
	t.Cleanup(func() { _ = lockConnection.Close() })
	if _, err := lockConnection.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("lock database for writing: %v", err)
	}
	t.Cleanup(func() { _, _ = lockConnection.ExecContext(context.Background(), "ROLLBACK") })

	eventHandler, err := webhook.NewHandler(webhook.Config{
		VerificationToken: "verification-token",
		AppID:             "cli_test",
		TenantKey:         "tenant-test",
	}, store)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("POST /integrations/lark/events", eventHandler)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := newControllerHTTPServer(listener.Addr().String(), mux)
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		<-serveResult
	})

	body, err := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id": "evt-slow-header", "event_type": "approval.instance.status_changed_v4",
			"app_id": "cli_test", "tenant_key": "tenant-test", "token": "verification-token",
		},
		"event": map[string]any{
			"approval_code": "approval-wallet-v1", "instance_code": "instance-slow-header", "status": "APPROVED",
		},
	})
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if err := connection.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		t.Fatalf("set connection deadline: %v", err)
	}
	started := time.Now()
	if _, err := fmt.Fprintf(connection,
		"POST /integrations/lark/events HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\n",
		len(body),
	); err != nil {
		t.Fatalf("write partial headers: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := connection.Write(append([]byte("\r\n"), body...)); err != nil {
		t.Fatalf("complete request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("contended response status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("end-to-end acknowledgement took %s, want less than 3s", elapsed)
	}
}

func TestPrepareOAuthBridgeRegistersFixedNewAPIEntryPoint(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bridgeSecretPath := filepath.Join(t.TempDir(), "bridge-client-secret")
	bridgeSecret := strings.Repeat("b", 32)
	if err := os.WriteFile(bridgeSecretPath, []byte(bridgeSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write bridge client secret: %v", err)
	}
	handler, err := prepareOAuthBridge(config.Config{
		AppID: "cli_test", AppSecret: "app-secret", TenantKey: "tenant-test",
		BridgeClientID:               "bridge-client-id",
		BridgeClientSecretFile:       bridgeSecretPath,
		NewAPIOAuthCallbackAllowlist: []string{oauthcontract.NewAPICallbackURI},
		OAuthRateLimitPerMinute:      30,
	}, store)
	if err != nil {
		t.Fatalf("prepare OAuth bridge: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	query := url.Values{
		"response_type": {"code"}, "client_id": {"bridge-client-id"},
		"redirect_uri": {oauthcontract.NewAPICallbackURI}, "state": {"new-api-state"},
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet,
		"/integrations/lark/oauth/authorize?"+query.Encode(),
		nil,
	))
	location, err := url.Parse(response.Header().Get("Location"))
	if response.Code != http.StatusFound || err != nil ||
		location.Scheme+"://"+location.Host != "https://accounts.feishu.cn" ||
		location.Path != "/open-apis/authen/v1/authorize" ||
		location.Query().Get("app_id") != "cli_test" ||
		location.Query().Get("redirect_uri") != oauthcontract.ControllerCallbackURI {
		t.Fatalf("authorize status=%d location=%q error=%v, want fixed Lark entry point",
			response.Code, response.Header().Get("Location"), err)
	}
	identity, err := inbox.NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("new identity: %v", err)
	}
	loginCode, err := store.CreateOAuthLoginCode(context.Background(), identity)
	if err != nil {
		t.Fatalf("create login code: %v", err)
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {loginCode},
		"redirect_uri": {oauthcontract.NewAPICallbackURI}, "client_id": {"bridge-client-id"},
		"client_secret": {bridgeSecret},
	}
	tokenResponse := httptest.NewRecorder()
	tokenRequest := httptest.NewRequest(
		http.MethodPost,
		"/internal/oauth/token",
		strings.NewReader(form.Encode()),
	)
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(tokenResponse, tokenRequest)
	if tokenResponse.Code != http.StatusOK {
		t.Fatalf("internal token status=%d body=%s, want loaded bridge secret",
			tokenResponse.Code, tokenResponse.Body.String())
	}
}

func TestOAuthCallbackCanCompleteBeyondWebhookWriteTimeout(t *testing.T) {
	identity, err := inbox.NewOAuthIdentity("tenant-test:ou_employee", "Employee")
	if err != nil {
		t.Fatalf("new OAuth identity: %v", err)
	}
	handler, err := oauthbridge.NewHandler(oauthbridge.Config{
		BridgeClientID: "bridge-client-id", BridgeClientSecret: strings.Repeat("b", 32),
		NewAPIRedirectURI: oauthcontract.NewAPICallbackURI,
		CallbackTimeout:   4 * time.Second, RateLimitPerMinute: 30,
	}, mainOAuthStore{identity: identity}, mainOAuthProvider{
		identity: identity, delay: controllerWriteTimeout + 200*time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new OAuth handler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := newControllerHTTPServer(listener.Addr().String(), mux)
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		<-serveResult
	})

	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	started := time.Now()
	response, err := client.Get("http://" + listener.Addr().String() +
		"/integrations/lark/oauth/callback?code=lark-code&state=internal-state")
	if err != nil {
		t.Fatalf("request slow OAuth callback: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if elapsed := time.Since(started); elapsed <= controllerWriteTimeout || elapsed >= 4*time.Second {
		t.Fatalf("callback elapsed=%s, want beyond webhook timeout and within OAuth timeout", elapsed)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if response.StatusCode != http.StatusFound || err != nil ||
		location.Query().Get("code") != "opaque-login-code" ||
		location.Query().Get("state") != "new-api-state" {
		t.Fatalf("callback status=%d location=%q error=%v, want successful delayed redirect",
			response.StatusCode, response.Header.Get("Location"), err)
	}
}

type mainOAuthStore struct {
	identity inbox.OAuthIdentity
}

func (mainOAuthStore) CreateOAuthAuthorizationState(
	context.Context,
	inbox.OAuthAuthorizationState,
) (string, error) {
	return "internal-state", nil
}

func (mainOAuthStore) ConsumeOAuthAuthorizationState(
	context.Context,
	string,
) (inbox.OAuthAuthorizationState, error) {
	return inbox.OAuthAuthorizationState{
		NewAPIState: "new-api-state", RedirectURI: oauthcontract.NewAPICallbackURI,
	}, nil
}

func (mainOAuthStore) CreateOAuthLoginCode(context.Context, inbox.OAuthIdentity) (string, error) {
	return "opaque-login-code", nil
}

func (mainOAuthStore) ExchangeOAuthLoginCode(context.Context, string) (string, error) {
	return "opaque-access-handle", nil
}

func (s mainOAuthStore) ConsumeOAuthAccessHandle(
	context.Context,
	string,
) (inbox.OAuthIdentity, error) {
	return s.identity, nil
}

type mainOAuthProvider struct {
	identity inbox.OAuthIdentity
	delay    time.Duration
}

func (mainOAuthProvider) AuthorizationURL(string) (string, error) {
	return "https://accounts.feishu.cn/open-apis/authen/v1/authorize", nil
}

func (p mainOAuthProvider) Exchange(ctx context.Context, _ string) (inbox.OAuthIdentity, error) {
	timer := time.NewTimer(p.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return inbox.OAuthIdentity{}, ctx.Err()
	case <-timer.C:
		return p.identity, nil
	}
}
