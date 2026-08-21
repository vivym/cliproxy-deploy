package oauthbridge

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/larkapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/oauthcontract"
)

const (
	maxLarkAuthorizationCodeBytes = 4096
	defaultRateLimitPerMinute     = 30
	defaultCallbackTimeout        = 10 * time.Second
	callbackWriteDeadlineGrace    = 2 * time.Second
	maxClientStatesPerTTL         = 20
	maxGlobalStatesPerMinute      = 500
)

type Config struct {
	BridgeClientID     string
	NewAPIRedirectURI  string
	RateLimitPerMinute int
	TrustedProxyCIDRs  []netip.Prefix
	CallbackTimeout    time.Duration
}

type Store interface {
	CreateOAuthAuthorizationState(context.Context, inbox.OAuthAuthorizationState) (string, error)
	ConsumeOAuthAuthorizationState(context.Context, string) (inbox.OAuthAuthorizationState, error)
	CreateOAuthLoginCode(context.Context, inbox.OAuthIdentity) (string, error)
}

type OAuthProvider interface {
	AuthorizationURL(string) (string, error)
	Exchange(context.Context, string) (inbox.OAuthIdentity, error)
}

type Handler struct {
	config             Config
	store              Store
	provider           OAuthProvider
	authorizeLimiter   *clientRateLimiter
	stateLimiter       *clientRateLimiter
	globalStateLimiter *fixedWindowRateLimiter
	callbackLimiter    *clientRateLimiter
}

func NewHandler(config Config, store Store, provider OAuthProvider) (*Handler, error) {
	if config.BridgeClientID == "" || config.NewAPIRedirectURI == "" {
		return nil, errors.New("OAuth bridge client id and New API redirect URI are required")
	}
	if config.NewAPIRedirectURI != oauthcontract.NewAPICallbackURI {
		return nil, errors.New("New API redirect URI must match the registered callback")
	}
	if store == nil || provider == nil {
		return nil, errors.New("OAuth credential store and provider are required")
	}
	if config.RateLimitPerMinute == 0 {
		config.RateLimitPerMinute = defaultRateLimitPerMinute
	}
	if config.RateLimitPerMinute < 0 {
		return nil, errors.New("OAuth rate limit must be positive")
	}
	if config.CallbackTimeout == 0 {
		config.CallbackTimeout = defaultCallbackTimeout
	}
	if config.CallbackTimeout < 0 {
		return nil, errors.New("OAuth callback timeout must be positive")
	}
	trustedProxyCIDRs := make([]netip.Prefix, len(config.TrustedProxyCIDRs))
	for index, prefix := range config.TrustedProxyCIDRs {
		if !prefix.IsValid() {
			return nil, errors.New("OAuth trusted proxy CIDRs must be valid")
		}
		trustedProxyCIDRs[index] = prefix.Masked()
	}
	config.TrustedProxyCIDRs = trustedProxyCIDRs
	return &Handler{
		config: config, store: store, provider: provider,
		authorizeLimiter:   newClientRateLimiter(config.RateLimitPerMinute, time.Minute, trustedProxyCIDRs),
		stateLimiter:       newClientRateLimiter(maxClientStatesPerTTL, 5*time.Minute, trustedProxyCIDRs),
		globalStateLimiter: newFixedWindowRateLimiter(maxGlobalStatesPerMinute, time.Minute),
		callbackLimiter:    newClientRateLimiter(config.RateLimitPerMinute, time.Minute, trustedProxyCIDRs),
	}, nil
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /integrations/lark/oauth/authorize", h.authorize)
	mux.HandleFunc("GET /integrations/lark/oauth/callback", h.callback)
}

func (h *Handler) authorize(response http.ResponseWriter, request *http.Request) {
	setPrivateOAuthHeaders(response)
	if !requireExactGET(response, request) {
		return
	}
	if allowed, retryAfter := h.authorizeLimiter.Allow(request); !allowed {
		writeOAuthRateLimitError(response, retryAfter)
		return
	}
	query := request.URL.Query()
	if !singleValue(query, "response_type", "code") ||
		!singleValue(query, "client_id", h.config.BridgeClientID) ||
		!singleValue(query, "redirect_uri", h.config.NewAPIRedirectURI) ||
		len(query["state"]) != 1 || query.Get("state") == "" {
		writeOAuthError(response, http.StatusBadRequest, "invalid_request")
		return
	}
	state, err := inbox.NewOAuthAuthorizationState(query.Get("state"), h.config.NewAPIRedirectURI)
	if err != nil {
		writeOAuthError(response, http.StatusBadRequest, "invalid_request")
		return
	}
	clientReservation, retryAfter := h.stateLimiter.Reserve(request)
	if clientReservation == nil {
		writeOAuthRateLimitError(response, retryAfter)
		return
	}
	globalReservation, retryAfter := h.globalStateLimiter.Reserve()
	if globalReservation == nil {
		clientReservation.Cancel()
		writeOAuthRateLimitError(response, retryAfter)
		return
	}
	internalState, err := h.store.CreateOAuthAuthorizationState(request.Context(), state)
	if err != nil {
		clientReservation.Cancel()
		globalReservation.Cancel()
		writeOAuthError(response, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	clientReservation.Commit()
	globalReservation.Commit()
	location, err := h.provider.AuthorizationURL(internalState)
	if err != nil {
		writeOAuthError(response, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	writeOAuthRedirect(response, location)
}

func (h *Handler) callback(response http.ResponseWriter, request *http.Request) {
	setPrivateOAuthHeaders(response)
	if !requireExactGET(response, request) {
		return
	}
	if allowed, retryAfter := h.callbackLimiter.Allow(request); !allowed {
		writeOAuthRateLimitError(response, retryAfter)
		return
	}
	controller := http.NewResponseController(response)
	writeDeadline := time.Now().Add(h.config.CallbackTimeout + callbackWriteDeadlineGrace)
	if err := controller.SetWriteDeadline(writeDeadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writeOAuthError(response, http.StatusServiceUnavailable, "temporarily_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), h.config.CallbackTimeout)
	defer cancel()
	request = request.WithContext(ctx)
	query := request.URL.Query()
	if len(query["state"]) != 1 || query.Get("state") == "" {
		writeOAuthError(response, http.StatusBadRequest, "invalid_state")
		return
	}
	state, err := h.store.ConsumeOAuthAuthorizationState(request.Context(), query.Get("state"))
	if err != nil {
		if errors.Is(err, inbox.ErrOAuthCredentialInvalid) {
			writeOAuthError(response, http.StatusBadRequest, "invalid_state")
		} else {
			writeOAuthError(response, http.StatusServiceUnavailable, "temporarily_unavailable")
		}
		return
	}
	if len(query["error"]) != 0 {
		if len(query["error"]) != 1 || query.Get("error") == "" || len(query["code"]) != 0 {
			redirectWithError(response, state, "invalid_request")
		} else {
			redirectWithError(response, state, larkAuthorizationError(query.Get("error")))
		}
		return
	}
	if len(query["code"]) != 1 || query.Get("code") == "" ||
		len(query.Get("code")) > maxLarkAuthorizationCodeBytes {
		redirectWithError(response, state, "invalid_request")
		return
	}
	identity, err := h.provider.Exchange(request.Context(), query.Get("code"))
	if err != nil {
		redirectWithError(response, state, oauthCallbackError(err))
		return
	}
	loginCode, err := h.store.CreateOAuthLoginCode(request.Context(), identity)
	if err != nil {
		redirectWithError(response, state, "temporarily_unavailable")
		return
	}
	location, err := callbackURL(state.RedirectURI, url.Values{
		"code": {loginCode}, "state": {state.NewAPIState},
	})
	if err != nil {
		writeOAuthError(response, http.StatusInternalServerError, "server_error")
		return
	}
	writeOAuthRedirect(response, location)
}

func larkAuthorizationError(code string) string {
	switch code {
	case "access_denied":
		return "access_denied"
	case "server_error", "temporarily_unavailable":
		return "temporarily_unavailable"
	default:
		return "server_error"
	}
}

func oauthCallbackError(err error) string {
	var failure *larkapi.OAuthExchangeError
	if errors.As(err, &failure) && failure != nil {
		switch failure.Reason {
		case larkapi.OAuthTimeout, larkapi.OAuthTransportError, larkapi.OAuthUpstreamUnavailable:
			return "temporarily_unavailable"
		case larkapi.OAuthRequestCanceled:
			return "request_canceled"
		}
	}
	return "server_error"
}

func redirectWithError(response http.ResponseWriter, state inbox.OAuthAuthorizationState, code string) {
	location, err := callbackURL(state.RedirectURI, url.Values{
		"error": {code}, "state": {state.NewAPIState},
	})
	if err != nil {
		writeOAuthError(response, http.StatusInternalServerError, "server_error")
		return
	}
	writeOAuthRedirect(response, location)
}

func callbackURL(base string, query url.Values) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	return parsed.String(), nil
}

func singleValue(values url.Values, key, want string) bool {
	entries := values[key]
	return len(entries) == 1 && secretEqual(entries[0], want)
}

func secretEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func setPrivateOAuthHeaders(response http.ResponseWriter) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Pragma", "no-cache")
	response.Header().Set("Referrer-Policy", "no-referrer")
}

func requireExactGET(response http.ResponseWriter, request *http.Request) bool {
	if request.Method == http.MethodGet {
		return true
	}
	response.Header().Set("Allow", http.MethodGet)
	writeOAuthError(response, http.StatusMethodNotAllowed, "invalid_request")
	return false
}

func writeOAuthRedirect(response http.ResponseWriter, location string) {
	response.Header().Set("Location", location)
	response.WriteHeader(http.StatusFound)
}

func writeOAuthError(response http.ResponseWriter, status int, code string) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]string{"error": code})
}

func writeOAuthRateLimitError(response http.ResponseWriter, retryAfter time.Duration) {
	retryAfterSeconds := max(int64(1), int64((retryAfter+time.Second-1)/time.Second))
	response.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds, 10))
	writeOAuthError(response, http.StatusTooManyRequests, "rate_limited")
}
