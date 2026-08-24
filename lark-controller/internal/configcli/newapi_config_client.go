package configcli

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

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/strictjson"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

const newAPIConfigurationResponseLimit = 1 << 20

type newAPIConfigurationClient struct {
	baseURL string
	secret  string
	client  *http.Client
}

type newAPIConfigurationState struct {
	Policies []struct {
		PolicyVersion string `json:"policy_version"`
		CatalogHash   string `json:"catalog_hash"`
		State         string `json:"state"`
	} `json:"policies"`
	ActivePolicyVersion string                                `json:"active_policy_version,omitempty"`
	OAuthProvider       *tenantconfig.OAuthProviderProjection `json:"oauth_provider,omitempty"`
}

func newNewAPIConfigurationClient(baseURL, secretFile string) (*newAPIConfigurationClient, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.User != nil || !allowedNewAPIConfigurationOrigin(parsed) {
		return nil, errors.New("New API configuration base URL is not an allowed origin")
	}
	secret, err := readSecretToken(secretFile, 32, 4096)
	if err != nil {
		return nil, fmt.Errorf("load New API configuration credential: %w", err)
	}
	return &newAPIConfigurationClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		secret:  secret,
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("redirects are disabled")
			},
		},
	}, nil
}

func allowedNewAPIConfigurationOrigin(parsed *url.URL) bool {
	if parsed.Scheme != "http" {
		return false
	}
	hostname := parsed.Hostname()
	if hostname == "new-api-config-endpoint" {
		return true
	}
	ip := net.ParseIP(hostname)
	return hostname == "localhost" || ip != nil && ip.IsLoopback()
}

func (client *newAPIConfigurationClient) request(
	ctx context.Context,
	method string,
	path string,
	payload []byte,
	target any,
) error {
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return errors.New("construct New API configuration request")
	}
	request.Header.Set("Authorization", "Bearer "+client.secret)
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.client.Do(request)
	if err != nil {
		return errors.New("New API configuration request failed")
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, newAPIConfigurationResponseLimit+1))
	if err != nil || len(contents) > newAPIConfigurationResponseLimit {
		return errors.New("New API configuration response is unreadable or too large")
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("New API configuration request returned HTTP %d", response.StatusCode)
	}
	if target != nil {
		if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
			return errors.New("New API configuration response is not JSON")
		}
		if err := strictjson.Decode(contents, target); err != nil {
			return errors.New("New API configuration response is invalid")
		}
	}
	return nil
}

func (client *newAPIConfigurationClient) observe(
	ctx context.Context,
	compiled tenantconfig.CompiledBundle,
) (*tenantconfig.ObservedNewAPI, error) {
	var state newAPIConfigurationState
	if err := client.request(ctx, http.MethodGet, "/api/integrations/v1/config/state", nil, &state); err != nil {
		return nil, err
	}
	observed := &tenantconfig.ObservedNewAPI{
		PolicyCatalogs:      make(map[string]string, len(state.Policies)),
		PolicyStates:        make(map[string]string, len(state.Policies)),
		ActivePolicyVersion: state.ActivePolicyVersion,
		OAuthProvider:       state.OAuthProvider,
	}
	for _, policy := range state.Policies {
		if policy.PolicyVersion == "" || policy.CatalogHash == "" || !validNewAPIPolicyState(policy.State) {
			return nil, errors.New("New API configuration state contains invalid policy metadata")
		}
		if _, duplicate := observed.PolicyCatalogs[policy.PolicyVersion]; duplicate {
			return nil, errors.New("New API configuration state contains duplicate policy versions")
		}
		observed.PolicyCatalogs[policy.PolicyVersion] = policy.CatalogHash
		observed.PolicyStates[policy.PolicyVersion] = policy.State
	}
	if observed.ActivePolicyVersion != "" && observed.PolicyStates[observed.ActivePolicyVersion] != "active" {
		return nil, errors.New("New API configuration state contains an inconsistent active policy")
	}
	publication, err := compiled.Artifact("new-api/policy-publication.json")
	if err != nil {
		return nil, err
	}
	var desiredPolicy struct {
		PolicyVersion string `json:"policy_version"`
		CatalogHash   string `json:"catalog_hash"`
	}
	if err := json.Unmarshal(publication.Contents, &desiredPolicy); err != nil {
		return nil, errors.New("compiled New API policy publication is invalid")
	}
	var policyPreflight struct {
		PolicyVersion string `json:"policy_version"`
		CatalogHash   string `json:"catalog_hash"`
		Valid         bool   `json:"valid"`
	}
	if err := client.request(
		ctx,
		http.MethodPost,
		"/api/integrations/v1/config/policies/preflight",
		publication.Contents,
		&policyPreflight,
	); err != nil {
		return nil, err
	}
	if !policyPreflight.Valid || policyPreflight.PolicyVersion != desiredPolicy.PolicyVersion ||
		policyPreflight.CatalogHash != desiredPolicy.CatalogHash {
		return nil, errors.New("New API policy preflight did not validate the requested publication")
	}
	oauthProvider, err := compiled.Artifact("new-api/oauth-provider.json")
	if err != nil {
		return nil, err
	}
	var oauthPreflight struct {
		Slug           string `json:"slug"`
		Valid          bool   `json:"valid"`
		ChangeRequired bool   `json:"change_required"`
		CurrentDigest  string `json:"current_digest,omitempty"`
		DesiredDigest  string `json:"desired_digest"`
	}
	if err := client.request(
		ctx,
		http.MethodPost,
		"/api/integrations/v1/config/oauth-provider/preflight",
		oauthProvider.Contents,
		&oauthPreflight,
	); err != nil {
		return nil, err
	}
	if !oauthPreflight.Valid || oauthPreflight.Slug != "lark" {
		return nil, errors.New("New API OAuth provider preflight did not validate the requested provider")
	}
	observed.OAuthPreflight = &tenantconfig.ObservedOAuthPreflight{
		ChangeRequired: oauthPreflight.ChangeRequired,
		CurrentDigest:  oauthPreflight.CurrentDigest,
		DesiredDigest:  oauthPreflight.DesiredDigest,
	}
	return observed, nil
}

func validNewAPIPolicyState(state string) bool {
	switch state {
	case "staged", "active", "draining", "retired":
		return true
	default:
		return false
	}
}

func (client *newAPIConfigurationClient) Execute(
	ctx context.Context,
	change tenantconfig.Change,
) (tenantconfig.ExecutionResult, error) {
	if change.Target != tenantconfig.TargetNewAPI {
		return tenantconfig.ExecutionResult{}, errors.New("unsupported New API configuration target")
	}
	var method string
	var path string
	var response struct {
		PolicyVersion string `json:"policy_version,omitempty"`
		Slug          string `json:"slug,omitempty"`
		Created       bool   `json:"created,omitempty"`
		Replayed      bool   `json:"replayed,omitempty"`
		Activated     bool   `json:"activated,omitempty"`
		ResultDigest  string `json:"result_digest,omitempty"`
	}
	switch change.Action {
	case tenantconfig.ActionPublishPolicy:
		method = http.MethodPost
		path = "/api/integrations/v1/config/policies"
	case tenantconfig.ActionUpsertDisabled:
		method = http.MethodPut
		path = "/api/integrations/v1/config/oauth-provider"
	case tenantconfig.ActionActivatePolicy:
		method = http.MethodPost
		path = "/api/integrations/v1/config/policies/activate"
	default:
		return tenantconfig.ExecutionResult{}, errors.New("unsupported New API configuration action")
	}
	if err := client.request(ctx, method, path, change.Payload, &response); err != nil {
		return tenantconfig.ExecutionResult{}, err
	}
	switch change.Action {
	case tenantconfig.ActionPublishPolicy:
		if response.PolicyVersion != change.Resource {
			return tenantconfig.ExecutionResult{}, errors.New("New API policy publication response mismatch")
		}
	case tenantconfig.ActionUpsertDisabled:
		if response.Slug != change.Resource || response.ResultDigest != change.DesiredDigest {
			return tenantconfig.ExecutionResult{}, errors.New("New API OAuth provider result digest mismatch")
		}
		return tenantconfig.ExecutionResult{ResultDigest: response.ResultDigest, Replayed: response.Replayed}, nil
	case tenantconfig.ActionActivatePolicy:
		if response.PolicyVersion != change.Resource || !response.Activated {
			return tenantconfig.ExecutionResult{}, errors.New("New API policy activation response mismatch")
		}
	}
	replayed := response.Replayed || change.Action == tenantconfig.ActionPublishPolicy && !response.Created
	return tenantconfig.ExecutionResult{ResultDigest: change.DesiredDigest, Replayed: replayed}, nil
}
