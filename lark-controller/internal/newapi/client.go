package newapi

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
	"strconv"
	"strings"
	"time"
)

const (
	grantPath          = "/api/integrations/v1/entitlement-grants"
	principalsPath     = "/api/integrations/v1/principals"
	maxResponseBytes   = 1 << 20
	minimumSecretBytes = 32
	defaultTimeout     = 5 * time.Second
)

type Config struct {
	BaseURL           string
	IntegrationSecret string
	HTTPClient        *http.Client
}

type Client struct {
	baseURL           string
	integrationSecret string
	httpClient        *http.Client
}

type Identity struct {
	ProviderSlug string `json:"provider_slug"`
	Subject      string `json:"subject"`
}

type Grant struct {
	Type            string `json:"type"`
	PackageCode     string `json:"package_code,omitempty"`
	QuotaDelta      int64  `json:"quota_delta,omitempty"`
	LevelCode       string `json:"level_code,omitempty"`
	MinimumRankOnly bool   `json:"minimum_rank_only,omitempty"`
}

type Evidence struct {
	ApprovalCode      string `json:"approval_code"`
	InstanceCode      string `json:"instance_code"`
	InstanceStartedAt string `json:"instance_started_at"`
	SchemaFingerprint string `json:"schema_fingerprint"`
	Locale            string `json:"locale"`
}

type EntitlementGrantRequest struct {
	ExternalID    string    `json:"external_id"`
	Source        string    `json:"source"`
	PolicyVersion string    `json:"policy_version"`
	Identity      Identity  `json:"identity"`
	Grant         Grant     `json:"grant"`
	Evidence      *Evidence `json:"evidence,omitempty"`
}

type GrantResult struct {
	GrantType         string `json:"grant_type"`
	PackageCode       string `json:"package_code,omitempty"`
	QuotaDelta        int64  `json:"quota_delta,omitempty"`
	LevelCode         string `json:"level_code,omitempty"`
	SubscriptionID    int64  `json:"subscription_id,omitempty"`
	AssignmentVersion int64  `json:"assignment_version,omitempty"`
	Transition        string `json:"transition,omitempty"`
}

type EntitlementGrantResponse struct {
	Status     string      `json:"status"`
	ExternalID string      `json:"external_id"`
	UserID     int64       `json:"user_id"`
	Result     GrantResult `json:"result"`
}

type Principal struct {
	ProviderSlug     string `json:"provider_slug"`
	Subject          string `json:"subject"`
	PrincipalVersion int64  `json:"principal_version"`
	UpdatedAt        string `json:"updated_at"`
}

type PrincipalPage struct {
	Principals   []Principal `json:"principals"`
	NextCursor   string      `json:"next_cursor"`
	ScanComplete bool        `json:"scan_complete"`
}

type APIError struct {
	StatusCode int
	Code       string
	Retryable  bool
}

type RequestError struct {
	Reason    string
	Retryable bool
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("New API integration transport failed: %s", e.Reason)
}

func (e *APIError) Error() string {
	return fmt.Sprintf("New API integration request failed: %s (HTTP %d)", e.Code, e.StatusCode)
}

func NewClient(config Config) (*Client, error) {
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("New API integration base URL must be an HTTP origin")
	}
	if len(config.IntegrationSecret) < minimumSecretBytes {
		return nil, errors.New("New API integration secret must contain at least 32 bytes")
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		baseURL:           strings.TrimSuffix(config.BaseURL, "/"),
		integrationSecret: config.IntegrationSecret,
		httpClient:        httpClient,
	}, nil
}

func (c *Client) Grant(ctx context.Context, request EntitlementGrantRequest) (EntitlementGrantResponse, error) {
	if err := validateGrantRequest(request); err != nil {
		return EntitlementGrantResponse{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return EntitlementGrantResponse{}, fmt.Errorf("encode New API grant request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.baseURL+grantPath,
		bytes.NewReader(payload),
	)
	if err != nil {
		return EntitlementGrantResponse{}, fmt.Errorf("create New API grant request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.integrationSecret)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return EntitlementGrantResponse{}, classifyTransportError(ctx, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return EntitlementGrantResponse{}, decodeAPIError(response)
	}
	var result EntitlementGrantResponse
	if err := decodeStrictJSON(response.Body, &result); err != nil {
		return EntitlementGrantResponse{}, classifyResponseDecodeError(ctx, err)
	}
	if err := validateGrantResponse(request, result); err != nil {
		return EntitlementGrantResponse{}, err
	}
	return result, nil
}

func (c *Client) ListActiveLarkPrincipals(
	ctx context.Context,
	cursor string,
	limit int,
) (PrincipalPage, error) {
	if limit < 1 || limit > 100 {
		return PrincipalPage{}, errors.New("principal page limit must be between 1 and 100")
	}
	endpoint, err := url.Parse(c.baseURL + principalsPath)
	if err != nil {
		return PrincipalPage{}, fmt.Errorf("create New API principals URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("provider_slug", "lark")
	query.Set("status", "active")
	query.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	endpoint.RawQuery = query.Encode()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return PrincipalPage{}, fmt.Errorf("create New API principals request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.integrationSecret)
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return PrincipalPage{}, classifyTransportError(ctx, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return PrincipalPage{}, decodeAPIError(response)
	}
	var page PrincipalPage
	if err := decodeStrictJSON(response.Body, &page); err != nil {
		return PrincipalPage{}, classifyResponseDecodeError(ctx, err)
	}
	if err := validatePrincipalPage(page, limit); err != nil {
		return PrincipalPage{}, err
	}
	return page, nil
}

func validateGrantRequest(request EntitlementGrantRequest) error {
	if request.ExternalID == "" || request.PolicyVersion == "" ||
		request.Identity.ProviderSlug != "lark" || request.Identity.Subject == "" {
		return errors.New("invalid New API entitlement grant identity")
	}
	switch request.Source {
	case "lark_approval":
		if request.Evidence == nil || request.Evidence.ApprovalCode == "" ||
			request.Evidence.InstanceCode == "" || request.Evidence.InstanceStartedAt == "" ||
			request.Evidence.SchemaFingerprint == "" || request.Evidence.Locale == "" {
			return errors.New("Lark approval evidence is required")
		}
	case "base_login":
		if request.Evidence != nil {
			return errors.New("base login grant must not include approval evidence")
		}
	default:
		return errors.New("unsupported New API entitlement grant source")
	}
	switch request.Grant.Type {
	case "wallet_quota":
		if request.Source != "lark_approval" || request.Grant.PackageCode == "" ||
			request.Grant.QuotaDelta <= 0 || request.Grant.LevelCode != "" || request.Grant.MinimumRankOnly {
			return errors.New("invalid wallet quota grant")
		}
	case "subscription_level":
		if request.Grant.LevelCode == "" || !request.Grant.MinimumRankOnly ||
			request.Grant.PackageCode != "" || request.Grant.QuotaDelta != 0 {
			return errors.New("invalid subscription level grant")
		}
	default:
		return errors.New("unsupported New API entitlement grant type")
	}
	return nil
}

func validateGrantResponse(
	request EntitlementGrantRequest,
	response EntitlementGrantResponse,
) error {
	switch response.Status {
	case "applied", "replayed", "noop", "ignored_stale":
	default:
		return errors.New("New API grant response has invalid status")
	}
	if response.ExternalID != request.ExternalID || response.UserID <= 0 ||
		response.Result.GrantType != request.Grant.Type {
		return errors.New("New API grant response does not match request")
	}
	if request.Grant.Type == "wallet_quota" && response.Result.QuotaDelta != request.Grant.QuotaDelta {
		return errors.New("New API wallet result does not match request")
	}
	if request.Grant.Type == "subscription_level" &&
		response.Result.LevelCode != request.Grant.LevelCode {
		return errors.New("New API subscription result does not match request")
	}
	if request.Grant.Type == "subscription_level" &&
		(response.Result.SubscriptionID <= 0 || response.Result.AssignmentVersion <= 0 ||
			!validSubscriptionTransition(response.Result.Transition)) {
		return errors.New("New API subscription result is incomplete")
	}
	if request.Grant.Type == "subscription_level" &&
		!validSubscriptionStatusTransition(response.Status, response.Result.Transition) {
		return errors.New("New API subscription status and transition do not match")
	}
	return nil
}

func validSubscriptionTransition(transition string) bool {
	switch transition {
	case "created", "updated", "noop", "ignored_stale":
		return true
	default:
		return false
	}
}

func validSubscriptionStatusTransition(status, transition string) bool {
	switch status {
	case "applied":
		return transition == "created" || transition == "updated"
	case "noop":
		return transition == "noop"
	case "ignored_stale":
		return transition == "ignored_stale"
	case "replayed":
		return validSubscriptionTransition(transition)
	default:
		return false
	}
}

func validatePrincipalPage(page PrincipalPage, limit int) error {
	if len(page.Principals) > limit || (page.ScanComplete && page.NextCursor != "") ||
		(!page.ScanComplete && page.NextCursor == "") {
		return errors.New("New API principals response has invalid pagination")
	}
	for _, principal := range page.Principals {
		if principal.ProviderSlug != "lark" || principal.Subject == "" || principal.PrincipalVersion <= 0 {
			return errors.New("New API principals response has invalid principal")
		}
		if _, err := time.Parse(time.RFC3339Nano, principal.UpdatedAt); err != nil {
			return errors.New("New API principals response has invalid updated_at")
		}
	}
	return nil
}

func decodeAPIError(response *http.Response) error {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := decodeStrictJSON(response.Body, &payload); err != nil || payload.Error.Code == "" {
		if response.StatusCode == http.StatusServiceUnavailable {
			return &APIError{
				StatusCode: response.StatusCode, Code: "temporarily_unavailable", Retryable: true,
			}
		}
		return &APIError{StatusCode: response.StatusCode, Code: "invalid_response"}
	}
	code := normalizeAPIErrorCode(payload.Error.Code)
	if response.StatusCode == http.StatusServiceUnavailable {
		return &APIError{
			StatusCode: response.StatusCode, Code: "temporarily_unavailable", Retryable: true,
		}
	}
	retryable := response.StatusCode == http.StatusNotFound && code == "principal_not_ready"
	return &APIError{StatusCode: response.StatusCode, Code: code, Retryable: retryable}
}

func normalizeAPIErrorCode(code string) string {
	switch code {
	case "invalid_request",
		"integration_unauthorized",
		"principal_not_ready",
		"principal_disabled",
		"external_id_payload_mismatch",
		"unmanaged_subscription_conflict",
		"policy_version_mismatch",
		"approval_binding_mismatch",
		"unknown_package",
		"unknown_level",
		"quota_out_of_range",
		"temporarily_unavailable":
		return code
	default:
		return "unclassified_error"
	}
}

func classifyTransportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return &RequestError{Reason: "timeout", Retryable: true}
	}
	return &RequestError{Reason: "transport_error", Retryable: true}
}

func classifyResponseDecodeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &RequestError{Reason: "transport_error", Retryable: true}
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		reason := "transport_error"
		if networkError.Timeout() {
			reason = "timeout"
		}
		return &RequestError{Reason: reason, Retryable: true}
	}
	return &RequestError{Reason: "invalid_response", Retryable: false}
}

func decodeStrictJSON(reader io.Reader, target any) error {
	limited := &io.LimitedReader{R: reader, N: maxResponseBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if limited.N <= 0 {
		return errors.New("response exceeds size limit")
	}
	var trailing any
	err := decoder.Decode(&trailing)
	if limited.N <= 0 {
		return errors.New("response exceeds size limit")
	}
	if !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("response contains trailing JSON")
		}
		return err
	}
	return nil
}
