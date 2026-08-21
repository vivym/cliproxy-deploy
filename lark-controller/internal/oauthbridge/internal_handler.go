package oauthbridge

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
)

const (
	maxTokenRequestBytes         = 16 * 1024
	accessHandleExpiresInSeconds = 60
)

func (h *Handler) token(response http.ResponseWriter, request *http.Request) {
	setPrivateOAuthHeaders(response)
	if !requireExactPOST(response, request) {
		return
	}
	if allowed, retryAfter := h.tokenLimiter.Allow(request); !allowed {
		writeOAuthRateLimitError(response, retryAfter)
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" ||
		request.URL.RawQuery != "" || len(request.Header.Values("Authorization")) != 0 {
		writeOAuthError(response, http.StatusBadRequest, "invalid_request")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, maxTokenRequestBytes)
	if err := request.ParseForm(); err != nil || !validTokenFormShape(request.PostForm) {
		writeOAuthError(response, http.StatusBadRequest, "invalid_request")
		return
	}
	if !singleValue(request.PostForm, "client_id", h.config.BridgeClientID) ||
		!singleValue(request.PostForm, "client_secret", h.config.BridgeClientSecret) {
		writeOAuthError(response, http.StatusBadRequest, "invalid_client")
		return
	}
	if request.PostForm.Get("grant_type") != "authorization_code" {
		writeOAuthError(response, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	if !secretEqual(request.PostForm.Get("redirect_uri"), h.config.NewAPIRedirectURI) {
		writeOAuthError(response, http.StatusBadRequest, "invalid_grant")
		return
	}
	accessHandle, err := h.store.ExchangeOAuthLoginCode(request.Context(), request.PostForm.Get("code"))
	if err != nil {
		if errors.Is(err, inbox.ErrOAuthCredentialInvalid) {
			writeOAuthError(response, http.StatusBadRequest, "invalid_grant")
		} else {
			writeOAuthError(response, http.StatusServiceUnavailable, "temporarily_unavailable")
		}
		return
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}{AccessToken: accessHandle, TokenType: "Bearer", ExpiresIn: accessHandleExpiresInSeconds})
}

func (h *Handler) userInfo(response http.ResponseWriter, request *http.Request) {
	setPrivateOAuthHeaders(response)
	if !requireExactGET(response, request) {
		return
	}
	if allowed, retryAfter := h.userInfoLimiter.Allow(request); !allowed {
		writeOAuthRateLimitError(response, retryAfter)
		return
	}
	if request.URL.RawQuery != "" {
		writeOAuthError(response, http.StatusBadRequest, "invalid_request")
		return
	}
	accessHandle, valid := bearerToken(request)
	if !valid {
		writeOAuthBearerError(response)
		return
	}
	identity, err := h.store.ConsumeOAuthAccessHandle(request.Context(), accessHandle)
	if err != nil {
		if errors.Is(err, inbox.ErrOAuthCredentialInvalid) {
			writeOAuthBearerError(response)
		} else {
			writeOAuthError(response, http.StatusServiceUnavailable, "temporarily_unavailable")
		}
		return
	}
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(response).Encode(struct {
		Subject  string `json:"sub"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}{Subject: identity.Subject, Username: identity.Username, Name: identity.Name})
}

func validTokenFormShape(values url.Values) bool {
	if len(values) != 5 {
		return false
	}
	for _, field := range []string{"grant_type", "code", "redirect_uri", "client_id", "client_secret"} {
		if len(values[field]) != 1 || values.Get(field) == "" {
			return false
		}
	}
	return true
}

func bearerToken(request *http.Request) (string, bool) {
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func requireExactPOST(response http.ResponseWriter, request *http.Request) bool {
	if request.Method == http.MethodPost {
		return true
	}
	response.Header().Set("Allow", http.MethodPost)
	writeOAuthError(response, http.StatusMethodNotAllowed, "invalid_request")
	return false
}

func writeOAuthBearerError(response http.ResponseWriter) {
	response.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	writeOAuthError(response, http.StatusUnauthorized, "invalid_token")
}
