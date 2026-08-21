package larkapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/oauthcontract"
)

const (
	defaultOAuthBaseURL   = "https://accounts.feishu.cn"
	defaultOpenBaseURL    = "https://open.feishu.cn"
	maxOAuthResponseBytes = 64 * 1024
)

type OAuthConfig struct {
	AppID        string
	AppSecret    string
	RedirectURI  string
	TenantKey    string
	OAuthBaseURL string
	OpenBaseURL  string
	Timeout      time.Duration
}

type OAuthExchanger struct {
	config OAuthConfig
	client *http.Client
}

type OAuthExchangeFailureReason string

const (
	OAuthTenantMismatch           OAuthExchangeFailureReason = "tenant_mismatch"
	OAuthInvalidResponse          OAuthExchangeFailureReason = "invalid_response"
	OAuthAuthorizationCodeInvalid OAuthExchangeFailureReason = "authorization_code_invalid"
	OAuthResponseTooLarge         OAuthExchangeFailureReason = "response_too_large"
	OAuthTimeout                  OAuthExchangeFailureReason = "timeout"
	OAuthTransportError           OAuthExchangeFailureReason = "transport_error"
	OAuthTokenRejected            OAuthExchangeFailureReason = "token_rejected"
	OAuthUserInfoRejected         OAuthExchangeFailureReason = "userinfo_rejected"
	OAuthRequestCanceled          OAuthExchangeFailureReason = "request_canceled"
	OAuthInvalidRequest           OAuthExchangeFailureReason = "invalid_request"
	OAuthUpstreamUnavailable      OAuthExchangeFailureReason = "upstream_unavailable"
)

var errOAuthResponseTooLarge = errors.New("Lark OAuth response exceeds size limit")

type OAuthExchangeError struct {
	Reason OAuthExchangeFailureReason
}

func (e *OAuthExchangeError) Error() string {
	return fmt.Sprintf("Lark OAuth exchange failed: %s", e.Reason)
}

type oauthTokenRequest struct {
	GrantType    string `json:"grant_type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
}

type oauthTokenResponse struct {
	Code        int    `json:"code"`
	AccessToken string `json:"access_token"`
}

type oauthUserInfoResponse struct {
	Code int `json:"code"`
	Data struct {
		Name      string `json:"name"`
		OpenID    string `json:"open_id"`
		TenantKey string `json:"tenant_key"`
	} `json:"data"`
}

func NewOAuthExchanger(config OAuthConfig) (*OAuthExchanger, error) {
	if config.AppID == "" || config.AppSecret == "" || config.RedirectURI == "" || config.TenantKey == "" {
		return nil, errors.New("Lark app id, app secret, OAuth redirect URI, and tenant key are required")
	}
	if config.RedirectURI != oauthcontract.ControllerCallbackURI {
		return nil, errors.New("Lark OAuth redirect URI must match the registered controller callback")
	}
	if config.OAuthBaseURL == "" {
		config.OAuthBaseURL = defaultOAuthBaseURL
	}
	if config.OpenBaseURL == "" {
		config.OpenBaseURL = defaultOpenBaseURL
	}
	if !validOAuthBaseURL(config.OAuthBaseURL) || !validOAuthBaseURL(config.OpenBaseURL) {
		return nil, errors.New("Lark OAuth base URLs must be HTTPS origins or loopback HTTP origins")
	}
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}
	if config.Timeout < 0 {
		return nil, errors.New("Lark OAuth request timeout must be positive")
	}
	return &OAuthExchanger{
		config: config,
		client: &http.Client{
			Timeout: config.Timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (e *OAuthExchanger) AuthorizationURL(state string) (string, error) {
	if state == "" {
		return "", &OAuthExchangeError{Reason: OAuthInvalidRequest}
	}
	query := make(url.Values)
	query.Set("app_id", e.config.AppID)
	query.Set("redirect_uri", e.config.RedirectURI)
	query.Set("state", state)
	return strings.TrimRight(e.config.OAuthBaseURL, "/") +
		"/open-apis/authen/v1/authorize?" + query.Encode(), nil
}

func (e *OAuthExchanger) Exchange(ctx context.Context, code string) (inbox.OAuthIdentity, error) {
	if code == "" {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthInvalidRequest}
	}
	body, err := json.Marshal(oauthTokenRequest{
		GrantType: "authorization_code", ClientID: e.config.AppID,
		ClientSecret: e.config.AppSecret, Code: code, RedirectURI: e.config.RedirectURI,
	})
	if err != nil {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthInvalidRequest}
	}
	tokenRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(e.config.OAuthBaseURL, "/")+"/oauth/v3/token",
		bytes.NewReader(body),
	)
	if err != nil {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthInvalidRequest}
	}
	tokenRequest.Header.Set("Content-Type", "application/json")
	tokenRequest.Header.Set("Accept", "application/json")
	tokenHTTPResponse, err := e.client.Do(tokenRequest)
	if err != nil {
		return inbox.OAuthIdentity{}, classifyOAuthRequestFailure(ctx, err)
	}
	defer func() { _ = tokenHTTPResponse.Body.Close() }()
	var tokenResponse oauthTokenResponse
	if err := decodeOAuthJSON(tokenHTTPResponse.Body, &tokenResponse); err != nil {
		return inbox.OAuthIdentity{}, classifyOAuthDecodeFailure(
			tokenHTTPResponse.StatusCode,
			err,
			OAuthTokenRejected,
		)
	}
	if oauthUpstreamUnavailable(tokenHTTPResponse.StatusCode) {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthUpstreamUnavailable}
	}
	if tokenResponse.Code == 20021 {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthAuthorizationCodeInvalid}
	}
	if tokenHTTPResponse.StatusCode != http.StatusOK || tokenResponse.Code != 0 {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthTokenRejected}
	}
	if tokenResponse.AccessToken == "" {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthInvalidResponse}
	}

	userInfoRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		strings.TrimRight(e.config.OpenBaseURL, "/")+"/open-apis/authen/v1/user_info",
		nil,
	)
	if err != nil {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthInvalidRequest}
	}
	userInfoRequest.Header.Set("Authorization", "Bearer "+tokenResponse.AccessToken)
	userInfoRequest.Header.Set("Accept", "application/json")
	userInfoHTTPResponse, err := e.client.Do(userInfoRequest)
	if err != nil {
		return inbox.OAuthIdentity{}, classifyOAuthRequestFailure(ctx, err)
	}
	defer func() { _ = userInfoHTTPResponse.Body.Close() }()
	var userInfoResponse oauthUserInfoResponse
	if err := decodeOAuthJSON(userInfoHTTPResponse.Body, &userInfoResponse); err != nil {
		return inbox.OAuthIdentity{}, classifyOAuthDecodeFailure(
			userInfoHTTPResponse.StatusCode,
			err,
			OAuthUserInfoRejected,
		)
	}
	if oauthUpstreamUnavailable(userInfoHTTPResponse.StatusCode) {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthUpstreamUnavailable}
	}
	if userInfoHTTPResponse.StatusCode != http.StatusOK || userInfoResponse.Code != 0 {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthUserInfoRejected}
	}
	if userInfoResponse.Data.Name == "" || userInfoResponse.Data.OpenID == "" || userInfoResponse.Data.TenantKey == "" {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthInvalidResponse}
	}
	if userInfoResponse.Data.TenantKey != e.config.TenantKey {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthTenantMismatch}
	}
	subject := userInfoResponse.Data.TenantKey + ":" + userInfoResponse.Data.OpenID
	identity, err := inbox.NewOAuthIdentity(subject, userInfoResponse.Data.Name)
	if err != nil {
		return inbox.OAuthIdentity{}, &OAuthExchangeError{Reason: OAuthInvalidResponse}
	}
	return identity, nil
}

func decodeOAuthJSON(reader io.Reader, target any) error {
	limited := &io.LimitedReader{R: reader, N: maxOAuthResponseBytes + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(target); err != nil {
		if limited.N <= 0 {
			return errOAuthResponseTooLarge
		}
		return err
	}
	var trailing any
	err := decoder.Decode(&trailing)
	if limited.N <= 0 {
		return errOAuthResponseTooLarge
	}
	if !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("Lark OAuth response contains trailing JSON")
		}
		return err
	}
	return nil
}

func classifyOAuthRequestFailure(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &OAuthExchangeError{Reason: OAuthTimeout}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return &OAuthExchangeError{Reason: OAuthRequestCanceled}
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return &OAuthExchangeError{Reason: OAuthTimeout}
	}
	return &OAuthExchangeError{Reason: OAuthTransportError}
}

func validOAuthBaseURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return true
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return false
	}
	hostname := parsed.Hostname()
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

func oauthUpstreamUnavailable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError
}

func classifyOAuthDecodeFailure(
	status int,
	decodeErr error,
	rejectedReason OAuthExchangeFailureReason,
) *OAuthExchangeError {
	switch {
	case oauthUpstreamUnavailable(status):
		return &OAuthExchangeError{Reason: OAuthUpstreamUnavailable}
	case errors.Is(decodeErr, errOAuthResponseTooLarge):
		return &OAuthExchangeError{Reason: OAuthResponseTooLarge}
	case status != http.StatusOK:
		return &OAuthExchangeError{Reason: rejectedReason}
	default:
		return &OAuthExchangeError{Reason: OAuthInvalidResponse}
	}
}
