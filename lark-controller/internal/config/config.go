package config

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/oauthcontract"
)

type Mode string

const (
	ModeShadow                        Mode = "shadow"
	ModeActive                        Mode = "active"
	defaultOAuthRateLimitPerMinute         = 30
	maxOAuthRateLimitPerMinute             = 10_000
	defaultReconciliationInterval          = 24 * time.Hour
	maxReconciliationInterval              = 7 * 24 * time.Hour
	defaultProcessingLeaseTimeout          = 5 * time.Minute
	defaultProcessingRecoveryInterval      = time.Minute
	minProcessingLeaseTimeout              = time.Minute
	maxProcessingLeaseTimeout              = time.Hour
	minProcessingRecoveryInterval          = 10 * time.Second
)

type Config struct {
	Mode                         Mode
	ListenAddress                string
	DatabasePath                 string
	AppID                        string
	AppSecret                    string
	VerificationToken            string
	EventEncryptKey              string
	TenantKey                    string
	Locale                       string
	ActivePolicyVersion          string
	PolicyBundleDirectory        string
	ApprovalBindingsFile         string
	GrantPayloadKeyringFile      string
	NewAPIBaseURL                string
	IntegrationSecretFile        string
	ReconciliationHealthOpenID   string
	ReconciliationInterval       time.Duration
	BridgeClientID               string
	BridgeClientSecretFile       string
	NewAPIOAuthCallbackAllowlist []string
	OAuthRateLimitPerMinute      int
	OAuthTrustedProxyCIDRs       []netip.Prefix
	WorkerPoll                   time.Duration
	ReadinessMaxQueueAge         time.Duration
	ProcessingLeaseTimeout       time.Duration
	ProcessingRecoveryInterval   time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("environment lookup is required")
	}
	loaded := Config{
		Mode:                       Mode(getenv("LARK_CONTROLLER_MODE")),
		ListenAddress:              getenv("LARK_CONTROLLER_LISTEN_ADDR"),
		DatabasePath:               getenv("LARK_CONTROLLER_DB_PATH"),
		AppID:                      getenv("LARK_APP_ID"),
		AppSecret:                  getenv("LARK_APP_SECRET"),
		VerificationToken:          getenv("LARK_VERIFICATION_TOKEN"),
		EventEncryptKey:            getenv("LARK_EVENT_ENCRYPT_KEY"),
		TenantKey:                  getenv("LARK_TENANT_KEY"),
		Locale:                     getenv("LARK_APPROVAL_LOCALE"),
		ActivePolicyVersion:        getenv("LARK_ACTIVE_POLICY_VERSION"),
		PolicyBundleDirectory:      getenv("LARK_POLICY_BUNDLE_DIR"),
		ApprovalBindingsFile:       getenv("LARK_APPROVAL_BINDINGS_FILE"),
		GrantPayloadKeyringFile:    getenv("LARK_GRANT_PAYLOAD_KEYRING_FILE"),
		BridgeClientID:             getenv("NEW_API_BRIDGE_CLIENT_ID"),
		BridgeClientSecretFile:     getenv("NEW_API_BRIDGE_CLIENT_SECRET_FILE"),
		WorkerPoll:                 time.Second,
		ReadinessMaxQueueAge:       15 * time.Minute,
		OAuthRateLimitPerMinute:    defaultOAuthRateLimitPerMinute,
		ReconciliationInterval:     defaultReconciliationInterval,
		ProcessingLeaseTimeout:     defaultProcessingLeaseTimeout,
		ProcessingRecoveryInterval: defaultProcessingRecoveryInterval,
	}
	callbackAllowlist, err := parseOAuthCallbackAllowlist(getenv("NEW_API_OAUTH_CALLBACK_ALLOWLIST"))
	if err != nil {
		return Config{}, err
	}
	loaded.NewAPIOAuthCallbackAllowlist = callbackAllowlist
	if raw := getenv("LARK_OAUTH_RATE_LIMIT_PER_MINUTE"); raw != "" {
		limit, parseErr := strconv.Atoi(raw)
		if parseErr != nil || limit < 1 || limit > maxOAuthRateLimitPerMinute {
			return Config{}, errors.New("LARK_OAUTH_RATE_LIMIT_PER_MINUTE must be an integer between 1 and 10000")
		}
		loaded.OAuthRateLimitPerMinute = limit
	}
	trustedProxyCIDRs, err := parseTrustedProxyCIDRs(getenv("LARK_OAUTH_TRUSTED_PROXY_CIDRS"))
	if err != nil {
		return Config{}, err
	}
	loaded.OAuthTrustedProxyCIDRs = trustedProxyCIDRs
	if loaded.Mode == "" {
		loaded.Mode = ModeShadow
	}
	if loaded.Mode != ModeShadow && loaded.Mode != ModeActive {
		return Config{}, errors.New("LARK_CONTROLLER_MODE must be shadow or active")
	}
	if loaded.ListenAddress == "" {
		loaded.ListenAddress = "0.0.0.0:8080"
	}
	if loaded.Locale == "" {
		loaded.Locale = "zh-CN"
	}
	if loaded.Locale != "zh-CN" {
		return Config{}, errors.New("LARK_APPROVAL_LOCALE must be zh-CN for the initial policy")
	}
	if raw := getenv("LARK_READINESS_MAX_QUEUE_AGE"); raw != "" {
		threshold, err := time.ParseDuration(raw)
		if err != nil || threshold <= 0 {
			return Config{}, errors.New("LARK_READINESS_MAX_QUEUE_AGE must be a positive duration")
		}
		loaded.ReadinessMaxQueueAge = threshold
	}
	if raw := getenv("LARK_PROCESSING_LEASE_TIMEOUT"); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout < minProcessingLeaseTimeout || timeout > maxProcessingLeaseTimeout {
			return Config{}, errors.New("LARK_PROCESSING_LEASE_TIMEOUT must be between 1m and 1h")
		}
		loaded.ProcessingLeaseTimeout = timeout
	}
	if raw := getenv("LARK_PROCESSING_RECOVERY_INTERVAL"); raw != "" {
		interval, err := time.ParseDuration(raw)
		if err != nil || interval < minProcessingRecoveryInterval {
			return Config{}, errors.New("LARK_PROCESSING_RECOVERY_INTERVAL must be at least 10s")
		}
		loaded.ProcessingRecoveryInterval = interval
	}
	if loaded.ProcessingRecoveryInterval > loaded.ProcessingLeaseTimeout {
		return Config{}, errors.New("LARK_PROCESSING_RECOVERY_INTERVAL must not exceed LARK_PROCESSING_LEASE_TIMEOUT")
	}
	if loaded.ProcessingLeaseTimeout+loaded.ProcessingRecoveryInterval >= loaded.ReadinessMaxQueueAge {
		return Config{}, errors.New("LARK_READINESS_MAX_QUEUE_AGE must exceed the processing lease plus recovery interval")
	}
	required := map[string]string{
		"LARK_CONTROLLER_DB_PATH":           loaded.DatabasePath,
		"LARK_APP_ID":                       loaded.AppID,
		"LARK_APP_SECRET":                   loaded.AppSecret,
		"LARK_VERIFICATION_TOKEN":           loaded.VerificationToken,
		"LARK_EVENT_ENCRYPT_KEY":            loaded.EventEncryptKey,
		"LARK_TENANT_KEY":                   loaded.TenantKey,
		"LARK_ACTIVE_POLICY_VERSION":        loaded.ActivePolicyVersion,
		"LARK_POLICY_BUNDLE_DIR":            loaded.PolicyBundleDirectory,
		"LARK_APPROVAL_BINDINGS_FILE":       loaded.ApprovalBindingsFile,
		"LARK_GRANT_PAYLOAD_KEYRING_FILE":   loaded.GrantPayloadKeyringFile,
		"NEW_API_BRIDGE_CLIENT_ID":          loaded.BridgeClientID,
		"NEW_API_BRIDGE_CLIENT_SECRET_FILE": loaded.BridgeClientSecretFile,
		"NEW_API_OAUTH_CALLBACK_ALLOWLIST":  strings.Join(loaded.NewAPIOAuthCallbackAllowlist, ","),
	}
	for name, value := range required {
		if value == "" {
			return Config{}, fmt.Errorf("%s is required", name)
		}
	}
	if loaded.Mode == ModeActive {
		loaded.IntegrationSecretFile = getenv("LARK_INTEGRATION_SECRET_FILE")
		loaded.ReconciliationHealthOpenID = getenv("LARK_RECONCILIATION_HEALTH_OPEN_ID")
		loaded.NewAPIBaseURL = getenv("NEW_API_INTERNAL_BASE_URL")
		if raw := getenv("LARK_RECONCILIATION_INTERVAL"); raw != "" {
			interval, err := time.ParseDuration(raw)
			if err != nil || interval < defaultReconciliationInterval ||
				interval > maxReconciliationInterval {
				return Config{}, errors.New("LARK_RECONCILIATION_INTERVAL must be between 24h and 168h")
			}
			loaded.ReconciliationInterval = interval
		}
		if !validOpenID(loaded.ReconciliationHealthOpenID) {
			return Config{}, errors.New("LARK_RECONCILIATION_HEALTH_OPEN_ID must be one valid open_id")
		}
		activeRequired := map[string]string{
			"LARK_INTEGRATION_SECRET_FILE":       loaded.IntegrationSecretFile,
			"LARK_RECONCILIATION_HEALTH_OPEN_ID": loaded.ReconciliationHealthOpenID,
			"NEW_API_INTERNAL_BASE_URL":          loaded.NewAPIBaseURL,
		}
		for name, value := range activeRequired {
			if value == "" {
				return Config{}, fmt.Errorf("%s is required in active mode", name)
			}
		}
	}
	return loaded, nil
}

func validOpenID(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 128 ||
		strings.Contains(value, ":") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func parseOAuthCallbackAllowlist(raw string) ([]string, error) {
	callbacks := strings.Split(raw, ",")
	if len(callbacks) != 1 || callbacks[0] != oauthcontract.NewAPICallbackURI {
		return nil, errors.New("NEW_API_OAUTH_CALLBACK_ALLOWLIST must contain only the registered New API callback")
	}
	return callbacks, nil
}

func parseTrustedProxyCIDRs(raw string) ([]netip.Prefix, error) {
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	seen := make(map[netip.Prefix]struct{}, len(parts))
	for _, part := range parts {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(part))
		if err != nil || prefix != prefix.Masked() {
			return nil, errors.New("LARK_OAUTH_TRUSTED_PROXY_CIDRS must contain canonical comma-separated CIDRs")
		}
		prefix = prefix.Masked()
		if _, exists := seen[prefix]; exists {
			return nil, errors.New("LARK_OAUTH_TRUSTED_PROXY_CIDRS must not contain duplicate CIDRs")
		}
		seen[prefix] = struct{}{}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, nil
}
