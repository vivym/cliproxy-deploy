package config

import (
	"errors"
	"fmt"
	"time"
)

type Mode string

const (
	ModeShadow Mode = "shadow"
	ModeActive Mode = "active"
)

type Config struct {
	Mode                    Mode
	ListenAddress           string
	DatabasePath            string
	AppID                   string
	AppSecret               string
	VerificationToken       string
	EventEncryptKey         string
	TenantKey               string
	Locale                  string
	ActivePolicyVersion     string
	PolicyBundleDirectory   string
	ApprovalBindingsFile    string
	GrantPayloadKeyringFile string
	NewAPIBaseURL           string
	IntegrationSecretFile   string
	WorkerPoll              time.Duration
	ReadinessMaxQueueAge    time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("environment lookup is required")
	}
	loaded := Config{
		Mode:                    Mode(getenv("LARK_CONTROLLER_MODE")),
		ListenAddress:           getenv("LARK_CONTROLLER_LISTEN_ADDR"),
		DatabasePath:            getenv("LARK_CONTROLLER_DB_PATH"),
		AppID:                   getenv("LARK_APP_ID"),
		AppSecret:               getenv("LARK_APP_SECRET"),
		VerificationToken:       getenv("LARK_VERIFICATION_TOKEN"),
		EventEncryptKey:         getenv("LARK_EVENT_ENCRYPT_KEY"),
		TenantKey:               getenv("LARK_TENANT_KEY"),
		Locale:                  getenv("LARK_APPROVAL_LOCALE"),
		ActivePolicyVersion:     getenv("LARK_ACTIVE_POLICY_VERSION"),
		PolicyBundleDirectory:   getenv("LARK_POLICY_BUNDLE_DIR"),
		ApprovalBindingsFile:    getenv("LARK_APPROVAL_BINDINGS_FILE"),
		GrantPayloadKeyringFile: getenv("LARK_GRANT_PAYLOAD_KEYRING_FILE"),
		WorkerPoll:              time.Second,
		ReadinessMaxQueueAge:    15 * time.Minute,
	}
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
	required := map[string]string{
		"LARK_CONTROLLER_DB_PATH":         loaded.DatabasePath,
		"LARK_APP_ID":                     loaded.AppID,
		"LARK_APP_SECRET":                 loaded.AppSecret,
		"LARK_VERIFICATION_TOKEN":         loaded.VerificationToken,
		"LARK_EVENT_ENCRYPT_KEY":          loaded.EventEncryptKey,
		"LARK_TENANT_KEY":                 loaded.TenantKey,
		"LARK_ACTIVE_POLICY_VERSION":      loaded.ActivePolicyVersion,
		"LARK_POLICY_BUNDLE_DIR":          loaded.PolicyBundleDirectory,
		"LARK_APPROVAL_BINDINGS_FILE":     loaded.ApprovalBindingsFile,
		"LARK_GRANT_PAYLOAD_KEYRING_FILE": loaded.GrantPayloadKeyringFile,
	}
	for name, value := range required {
		if value == "" {
			return Config{}, fmt.Errorf("%s is required", name)
		}
	}
	if loaded.Mode == ModeActive {
		loaded.IntegrationSecretFile = getenv("LARK_INTEGRATION_SECRET_FILE")
		loaded.NewAPIBaseURL = getenv("NEW_API_INTERNAL_BASE_URL")
		activeRequired := map[string]string{
			"LARK_INTEGRATION_SECRET_FILE": loaded.IntegrationSecretFile,
			"NEW_API_INTERNAL_BASE_URL":    loaded.NewAPIBaseURL,
		}
		for name, value := range activeRequired {
			if value == "" {
				return Config{}, fmt.Errorf("%s is required in active mode", name)
			}
		}
	}
	return loaded, nil
}
