package newapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	correctionPath        = "/api/integrations/v1/entitlement-corrections"
	correctionPreviewPath = "/api/integrations/v1/entitlement-corrections/preview"
)

type CorrectionConfig struct {
	BaseURL          string
	CorrectionSecret string
	HTTPClient       *http.Client
}

type CorrectionClient struct {
	baseURL          string
	correctionSecret string
	httpClient       *http.Client
}

type Correction struct {
	Type                      string `json:"type"`
	QuotaDelta                int64  `json:"quota_delta,omitempty"`
	ExpectedWalletQuota       *int64 `json:"expected_wallet_quota,omitempty"`
	LevelCode                 string `json:"level_code,omitempty"`
	ExpectedAssignmentVersion *int64 `json:"expected_assignment_version,omitempty"`
}

type CorrectionEvidence struct {
	Operator           string `json:"operator"`
	Reason             string `json:"reason"`
	ChangeTicket       string `json:"change_ticket"`
	OriginalExternalID string `json:"original_external_id"`
}

type EntitlementCorrectionRequest struct {
	ExternalID    string             `json:"external_id"`
	Source        string             `json:"source"`
	PolicyVersion string             `json:"policy_version"`
	Identity      Identity           `json:"identity"`
	Correction    Correction         `json:"correction"`
	Evidence      CorrectionEvidence `json:"evidence"`
}

type CorrectionResult struct {
	GrantType         string `json:"grant_type"`
	QuotaDelta        int64  `json:"quota_delta,omitempty"`
	WalletQuota       *int64 `json:"wallet_quota,omitempty"`
	LevelCode         string `json:"level_code,omitempty"`
	SubscriptionID    int64  `json:"subscription_id,omitempty"`
	AssignmentVersion int64  `json:"assignment_version,omitempty"`
	Transition        string `json:"transition,omitempty"`
}

type EntitlementCorrectionResponse struct {
	Status     string           `json:"status"`
	ExternalID string           `json:"external_id"`
	UserID     int64            `json:"user_id"`
	Result     CorrectionResult `json:"result"`
}

type ManagedSubscriptionCorrectionPreview struct {
	PolicyVersion     string `json:"policy_version"`
	LevelCode         string `json:"level_code"`
	AssignmentVersion int64  `json:"assignment_version"`
	SourceExternalID  string `json:"source_external_id"`
	SubscriptionID    int64  `json:"subscription_id"`
	AmountTotal       int64  `json:"amount_total"`
	AmountUsed        int64  `json:"amount_used"`
	StartTime         int64  `json:"start_time"`
	EndTime           int64  `json:"end_time"`
	LastResetTime     int64  `json:"last_reset_time"`
	NextResetTime     int64  `json:"next_reset_time"`
}

type EntitlementCorrectionPreview struct {
	WalletQuota         int64                                 `json:"wallet_quota"`
	UsedQuota           int64                                 `json:"used_quota"`
	LastLoginAt         int64                                 `json:"last_login_at"`
	ManagedSubscription *ManagedSubscriptionCorrectionPreview `json:"managed_subscription,omitempty"`
}

func NewCorrectionClient(config CorrectionConfig) (*CorrectionClient, error) {
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("New API correction base URL must be an HTTP origin")
	}
	if !validIntegrationSecret([]byte(config.CorrectionSecret)) {
		return nil, errors.New("New API correction secret must be one printable ASCII token of at least 32 bytes")
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &CorrectionClient{
		baseURL:          strings.TrimSuffix(config.BaseURL, "/"),
		correctionSecret: config.CorrectionSecret,
		httpClient:       httpClient,
	}, nil
}

func (client *CorrectionClient) Correct(
	ctx context.Context,
	request EntitlementCorrectionRequest,
) (EntitlementCorrectionResponse, error) {
	if err := validateCorrectionRequest(request); err != nil {
		return EntitlementCorrectionResponse{}, err
	}
	payload, err := encodeCorrectionRequest(request)
	if err != nil {
		return EntitlementCorrectionResponse{}, err
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, client.baseURL+correctionPath, bytes.NewReader(payload),
	)
	if err != nil {
		return EntitlementCorrectionResponse{}, fmt.Errorf("create New API correction request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+client.correctionSecret)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return EntitlementCorrectionResponse{}, classifyTransportError(ctx, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return EntitlementCorrectionResponse{}, decodeAPIError(response)
	}
	var result EntitlementCorrectionResponse
	if err := decodeStrictJSON(response.Body, &result); err != nil {
		return EntitlementCorrectionResponse{}, classifyResponseDecodeError(ctx, err)
	}
	if err := validateCorrectionResponse(request, result); err != nil {
		return EntitlementCorrectionResponse{}, &RequestError{Reason: "invalid_response", Retryable: false}
	}
	return result, nil
}

func encodeCorrectionRequest(request EntitlementCorrectionRequest) ([]byte, error) {
	if err := validateCorrectionRequest(request); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode New API correction request: %w", err)
	}
	return payload, nil
}

func CorrectionRequestSHA256(request EntitlementCorrectionRequest) (string, error) {
	payload, err := encodeCorrectionRequest(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("%x", digest[:]), nil
}

func (client *CorrectionClient) Preview(
	ctx context.Context,
	identity Identity,
) (EntitlementCorrectionPreview, error) {
	if identity.ProviderSlug != "lark" || !validIdentifier(identity.Subject, 255) {
		return EntitlementCorrectionPreview{}, errors.New("invalid New API correction preview identity")
	}
	payload, err := json.Marshal(struct {
		Identity Identity `json:"identity"`
	}{Identity: identity})
	if err != nil {
		return EntitlementCorrectionPreview{}, fmt.Errorf("encode New API correction preview: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, client.baseURL+correctionPreviewPath, bytes.NewReader(payload),
	)
	if err != nil {
		return EntitlementCorrectionPreview{}, fmt.Errorf("create New API correction preview request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+client.correctionSecret)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(httpRequest)
	if err != nil {
		return EntitlementCorrectionPreview{}, classifyTransportError(ctx, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return EntitlementCorrectionPreview{}, decodeAPIError(response)
	}
	var preview EntitlementCorrectionPreview
	if err := decodeStrictJSON(response.Body, &preview); err != nil {
		return EntitlementCorrectionPreview{}, classifyResponseDecodeError(ctx, err)
	}
	if err := validateCorrectionPreview(preview); err != nil {
		return EntitlementCorrectionPreview{}, &RequestError{Reason: "invalid_response", Retryable: false}
	}
	return preview, nil
}

func validateCorrectionPreview(preview EntitlementCorrectionPreview) error {
	if preview.WalletQuota < 0 || preview.UsedQuota < 0 || preview.LastLoginAt < 0 {
		return errors.New("New API correction preview has invalid wallet state")
	}
	if preview.ManagedSubscription == nil {
		return nil
	}
	managed := preview.ManagedSubscription
	if !validIdentifier(managed.PolicyVersion, 128) ||
		!validIdentifier(managed.LevelCode, 64) || managed.AssignmentVersion <= 0 ||
		!validIdentifier(managed.SourceExternalID, 255) || managed.SubscriptionID <= 0 ||
		managed.AmountTotal < 0 || managed.AmountUsed < 0 || managed.AmountUsed > managed.AmountTotal ||
		managed.StartTime < 0 || managed.EndTime < 0 || managed.LastResetTime < 0 || managed.NextResetTime < 0 {
		return errors.New("New API correction preview has invalid managed subscription state")
	}
	return nil
}

func validateCorrectionRequest(request EntitlementCorrectionRequest) error {
	if !validIdentifier(request.ExternalID, 255) ||
		!strings.HasPrefix(request.ExternalID, "lark:correction:") ||
		len(request.ExternalID) == len("lark:correction:") ||
		request.Source != "correction" ||
		!validIdentifier(request.PolicyVersion, 128) ||
		request.Identity.ProviderSlug != "lark" ||
		!validIdentifier(request.Identity.Subject, 255) ||
		!validIdentifier(request.Evidence.Operator, 128) ||
		!validIdentifier(request.Evidence.Reason, 512) ||
		!validIdentifier(request.Evidence.ChangeTicket, 128) ||
		!validIdentifier(request.Evidence.OriginalExternalID, 255) {
		return errors.New("invalid New API entitlement correction request")
	}
	switch request.Correction.Type {
	case "wallet_quota":
		if request.Correction.ExpectedWalletQuota == nil ||
			*request.Correction.ExpectedWalletQuota < 0 ||
			request.Correction.LevelCode != "" ||
			request.Correction.ExpectedAssignmentVersion != nil {
			return errors.New("invalid New API wallet correction")
		}
	case "subscription_level":
		if !validIdentifier(request.Correction.LevelCode, 64) ||
			request.Correction.ExpectedAssignmentVersion == nil ||
			*request.Correction.ExpectedAssignmentVersion <= 0 ||
			request.Correction.QuotaDelta != 0 ||
			request.Correction.ExpectedWalletQuota != nil {
			return errors.New("invalid New API subscription correction")
		}
	default:
		return errors.New("unsupported New API entitlement correction type")
	}
	return nil
}

func validateCorrectionResponse(
	request EntitlementCorrectionRequest,
	response EntitlementCorrectionResponse,
) error {
	switch response.Status {
	case "applied", "replayed", "noop":
	default:
		return errors.New("New API correction response has invalid status")
	}
	if response.ExternalID != request.ExternalID || response.UserID <= 0 ||
		response.Result.GrantType != request.Correction.Type {
		return errors.New("New API correction response does not match request")
	}
	switch request.Correction.Type {
	case "wallet_quota":
		if response.Result.QuotaDelta != request.Correction.QuotaDelta ||
			response.Result.WalletQuota == nil || *response.Result.WalletQuota < 0 ||
			(response.Status == "applied" && request.Correction.QuotaDelta == 0) ||
			(response.Status == "noop" && request.Correction.QuotaDelta != 0) {
			return errors.New("New API wallet correction result does not match request")
		}
	case "subscription_level":
		if response.Result.LevelCode != request.Correction.LevelCode ||
			response.Result.SubscriptionID <= 0 || response.Result.AssignmentVersion <= 0 {
			return errors.New("New API subscription correction result is incomplete")
		}
		if response.Status == "applied" && response.Result.Transition != "updated" ||
			response.Status == "noop" && response.Result.Transition != "noop" ||
			response.Status == "replayed" && response.Result.Transition != "updated" && response.Result.Transition != "noop" {
			return errors.New("New API subscription correction status and transition do not match")
		}
	}
	return nil
}
