package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/config"
)

func TestLoadRequiresCommonSecretsAndDefaultsToShadowMode(t *testing.T) {
	values := map[string]string{
		"LARK_CONTROLLER_MODE":              "shadow",
		"LARK_CONTROLLER_DB_PATH":           "/data/controller.sqlite",
		"LARK_APP_ID":                       "cli_test",
		"LARK_APP_SECRET":                   "app-secret",
		"LARK_VERIFICATION_TOKEN":           "verification-token",
		"LARK_EVENT_ENCRYPT_KEY":            "event-encryption-key",
		"LARK_TENANT_KEY":                   "tenant-test",
		"LARK_ACTIVE_POLICY_VERSION":        "employee-v1",
		"LARK_POLICY_BUNDLE_DIR":            "/policies",
		"LARK_APPROVAL_BINDINGS_FILE":       "/policies/approval-bindings.json",
		"LARK_GRANT_PAYLOAD_KEYRING_FILE":   "/run/secrets/lark_grant_payload_keyring",
		"NEW_API_BRIDGE_CLIENT_ID":          "bridge-client-id",
		"NEW_API_BRIDGE_CLIENT_SECRET_FILE": "/run/secrets/new_api_bridge_client_secret",
		"NEW_API_OAUTH_CALLBACK_ALLOWLIST":  "https://ai.x2r.store/oauth/lark",
	}
	loaded, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("load valid config: %v", err)
	}
	if loaded.Mode != "shadow" || loaded.ListenAddress != "0.0.0.0:8080" || loaded.Locale != "zh-CN" {
		t.Fatalf("unexpected defaults: %+v", loaded)
	}
	if loaded.ReadinessMaxQueueAge != 15*time.Minute {
		t.Fatalf("readiness max queue age = %s, want 15m", loaded.ReadinessMaxQueueAge)
	}
	if loaded.ProcessingLeaseTimeout != 5*time.Minute ||
		loaded.ProcessingRecoveryInterval != time.Minute {
		t.Fatalf("unexpected processing recovery defaults: %+v", loaded)
	}
	if loaded.ApprovalReconcileInterval != 15*time.Minute ||
		loaded.ApprovalReconcileLookback != 72*time.Hour {
		t.Fatalf("unexpected approval reconciliation defaults: %+v", loaded)
	}
	if loaded.OAuthRateLimitPerMinute != 30 || len(loaded.OAuthTrustedProxyCIDRs) != 0 ||
		loaded.BridgeClientID != "bridge-client-id" ||
		loaded.BridgeClientSecretFile != "/run/secrets/new_api_bridge_client_secret" ||
		len(loaded.NewAPIOAuthCallbackAllowlist) != 1 ||
		loaded.NewAPIOAuthCallbackAllowlist[0] != "https://ai.x2r.store/oauth/lark" {
		t.Fatalf("unexpected OAuth config defaults: %+v", loaded)
	}
	if loaded.ActivePolicyVersion != "employee-v1" || loaded.PolicyBundleDirectory != "/policies" ||
		loaded.ApprovalBindingsFile != "/policies/approval-bindings.json" ||
		loaded.GrantPayloadKeyringFile != "/run/secrets/lark_grant_payload_keyring" {
		t.Fatalf("unexpected policy config: %+v", loaded)
	}

	values["LARK_CONTROLLER_MODE"] = "unexpected"
	_, err = config.Load(func(key string) string { return values[key] })
	if err == nil || !strings.Contains(err.Error(), "LARK_CONTROLLER_MODE") {
		t.Fatalf("unknown mode error = %v", err)
	}

	values["LARK_CONTROLLER_MODE"] = "shadow"
	delete(values, "LARK_APP_SECRET")
	_, err = config.Load(func(key string) string { return values[key] })
	if err == nil || strings.Contains(err.Error(), "app-secret") {
		t.Fatalf("missing secret error = %v, want non-secret validation error", err)
	}
}

func TestLoadValidatesApprovalReconciliationDurations(t *testing.T) {
	values := map[string]string{
		"LARK_CONTROLLER_DB_PATH":               "/data/controller.sqlite",
		"LARK_APP_ID":                           "cli_test",
		"LARK_APP_SECRET":                       "app-secret",
		"LARK_VERIFICATION_TOKEN":               "verification-token",
		"LARK_EVENT_ENCRYPT_KEY":                "event-encryption-key",
		"LARK_TENANT_KEY":                       "tenant-test",
		"LARK_ACTIVE_POLICY_VERSION":            "employee-v1",
		"LARK_POLICY_BUNDLE_DIR":                "/policies",
		"LARK_APPROVAL_BINDINGS_FILE":           "/policies/approval-bindings.json",
		"LARK_GRANT_PAYLOAD_KEYRING_FILE":       "/run/secrets/lark_grant_payload_keyring",
		"NEW_API_BRIDGE_CLIENT_ID":              "bridge-client-id",
		"NEW_API_BRIDGE_CLIENT_SECRET_FILE":     "/run/secrets/new_api_bridge_client_secret",
		"NEW_API_OAUTH_CALLBACK_ALLOWLIST":      "https://ai.x2r.store/oauth/lark",
		"LARK_APPROVAL_RECONCILIATION_INTERVAL": "30m",
		"LARK_APPROVAL_RECONCILIATION_LOOKBACK": "168h",
	}
	loaded, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("load approval reconciliation config: %v", err)
	}
	if loaded.ApprovalReconcileInterval != 30*time.Minute ||
		loaded.ApprovalReconcileLookback != 168*time.Hour {
		t.Fatalf("unexpected approval reconciliation config: %+v", loaded)
	}
	tests := []struct {
		name  string
		value string
	}{
		{name: "LARK_APPROVAL_RECONCILIATION_INTERVAL", value: "59s"},
		{name: "LARK_APPROVAL_RECONCILIATION_INTERVAL", value: "25h"},
		{name: "LARK_APPROVAL_RECONCILIATION_INTERVAL", value: "later"},
		{name: "LARK_APPROVAL_RECONCILIATION_LOOKBACK", value: "59m"},
		{name: "LARK_APPROVAL_RECONCILIATION_LOOKBACK", value: "721h"},
		{name: "LARK_APPROVAL_RECONCILIATION_LOOKBACK", value: "later"},
	}
	for _, test := range tests {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			original := values[test.name]
			values[test.name] = test.value
			_, err := config.Load(func(key string) string { return values[key] })
			values[test.name] = original
			if err == nil || !strings.Contains(err.Error(), test.name) {
				t.Fatalf("invalid %s=%q error = %v", test.name, test.value, err)
			}
		})
	}
}

func TestLoadRequiresNewAPIConfigOnlyInActiveMode(t *testing.T) {
	values := map[string]string{
		"LARK_CONTROLLER_MODE":               "active",
		"LARK_CONTROLLER_DB_PATH":            "/data/controller.sqlite",
		"LARK_APP_ID":                        "cli_test",
		"LARK_APP_SECRET":                    "app-secret",
		"LARK_VERIFICATION_TOKEN":            "verification-token",
		"LARK_EVENT_ENCRYPT_KEY":             "event-encryption-key",
		"LARK_TENANT_KEY":                    "tenant-test",
		"LARK_ACTIVE_POLICY_VERSION":         "employee-v1",
		"LARK_POLICY_BUNDLE_DIR":             "/policies",
		"LARK_APPROVAL_BINDINGS_FILE":        "/policies/approval-bindings.json",
		"LARK_GRANT_PAYLOAD_KEYRING_FILE":    "/run/secrets/lark_grant_payload_keyring",
		"LARK_INTEGRATION_SECRET_FILE":       "/run/secrets/lark_integration_secret",
		"LARK_RECONCILIATION_HEALTH_OPEN_ID": "ou_health_probe",
		"NEW_API_INTERNAL_BASE_URL":          "http://new-api:3001",
		"NEW_API_BRIDGE_CLIENT_ID":           "bridge-client-id",
		"NEW_API_BRIDGE_CLIENT_SECRET_FILE":  "/run/secrets/new_api_bridge_client_secret",
		"NEW_API_OAUTH_CALLBACK_ALLOWLIST":   "https://ai.x2r.store/oauth/lark",
	}
	loaded, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("load active config: %v", err)
	}
	if loaded.Mode != "active" || loaded.NewAPIBaseURL != "http://new-api:3001" ||
		loaded.IntegrationSecretFile != "/run/secrets/lark_integration_secret" ||
		loaded.ReconciliationHealthOpenID != "ou_health_probe" ||
		loaded.ReconciliationInterval != 24*time.Hour {
		t.Fatalf("unexpected active config: %+v", loaded)
	}
	values["LARK_RECONCILIATION_INTERVAL"] = "48h"
	loaded, err = config.Load(func(key string) string { return values[key] })
	if err != nil || loaded.ReconciliationInterval != 48*time.Hour {
		t.Fatalf("load custom reconciliation interval: interval=%s err=%v",
			loaded.ReconciliationInterval, err)
	}
	for _, interval := range []string{"23h", "169h", "tomorrow"} {
		values["LARK_RECONCILIATION_INTERVAL"] = interval
		if _, err := config.Load(func(key string) string { return values[key] }); err == nil ||
			!strings.Contains(err.Error(), "LARK_RECONCILIATION_INTERVAL") {
			t.Fatalf("invalid reconciliation interval %q error = %v", interval, err)
		}
	}
	delete(values, "LARK_RECONCILIATION_INTERVAL")
	originalHealthOpenID := values["LARK_RECONCILIATION_HEALTH_OPEN_ID"]
	values["LARK_RECONCILIATION_HEALTH_OPEN_ID"] = "ou_health:other"
	if _, err := config.Load(func(key string) string { return values[key] }); err == nil ||
		!strings.Contains(err.Error(), "LARK_RECONCILIATION_HEALTH_OPEN_ID") {
		t.Fatalf("invalid reconciliation health open_id error = %v", err)
	}
	values["LARK_RECONCILIATION_HEALTH_OPEN_ID"] = originalHealthOpenID

	for _, name := range []string{
		"LARK_INTEGRATION_SECRET_FILE",
		"LARK_RECONCILIATION_HEALTH_OPEN_ID",
		"NEW_API_INTERNAL_BASE_URL",
	} {
		value := values[name]
		delete(values, name)
		if _, err := config.Load(func(key string) string { return values[key] }); err == nil ||
			!strings.Contains(err.Error(), name) || strings.Contains(err.Error(), value) {
			t.Fatalf("missing %s error = %v", name, err)
		}
		values[name] = value
	}

	values["LARK_CONTROLLER_MODE"] = "shadow"
	delete(values, "LARK_INTEGRATION_SECRET_FILE")
	delete(values, "LARK_RECONCILIATION_HEALTH_OPEN_ID")
	delete(values, "NEW_API_INTERNAL_BASE_URL")
	activeLookups := 0
	if _, err := config.Load(func(key string) string {
		if key == "LARK_INTEGRATION_SECRET_FILE" ||
			key == "LARK_RECONCILIATION_HEALTH_OPEN_ID" || key == "NEW_API_INTERNAL_BASE_URL" {
			activeLookups++
		}
		return values[key]
	}); err != nil {
		t.Fatalf("shadow config required active-only settings: %v", err)
	}
	if activeLookups != 0 {
		t.Fatalf("shadow config performed %d active-only lookups", activeLookups)
	}
}

func TestLoadValidatesReadinessQueueAge(t *testing.T) {
	values := map[string]string{
		"LARK_CONTROLLER_DB_PATH":           "/data/controller.sqlite",
		"LARK_APP_ID":                       "cli_test",
		"LARK_APP_SECRET":                   "app-secret",
		"LARK_VERIFICATION_TOKEN":           "verification-token",
		"LARK_EVENT_ENCRYPT_KEY":            "event-encryption-key",
		"LARK_TENANT_KEY":                   "tenant-test",
		"LARK_ACTIVE_POLICY_VERSION":        "employee-v1",
		"LARK_POLICY_BUNDLE_DIR":            "/policies",
		"LARK_APPROVAL_BINDINGS_FILE":       "/policies/approval-bindings.json",
		"LARK_GRANT_PAYLOAD_KEYRING_FILE":   "/run/secrets/lark_grant_payload_keyring",
		"LARK_READINESS_MAX_QUEUE_AGE":      "20m",
		"NEW_API_BRIDGE_CLIENT_ID":          "bridge-client-id",
		"NEW_API_BRIDGE_CLIENT_SECRET_FILE": "/run/secrets/new_api_bridge_client_secret",
		"NEW_API_OAUTH_CALLBACK_ALLOWLIST":  "https://ai.x2r.store/oauth/lark",
	}
	loaded, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("load valid readiness threshold: %v", err)
	}
	if loaded.ReadinessMaxQueueAge != 20*time.Minute {
		t.Fatalf("readiness max queue age = %s, want 20m", loaded.ReadinessMaxQueueAge)
	}
	values["LARK_READINESS_MAX_QUEUE_AGE"] = "0s"
	if _, err := config.Load(func(key string) string { return values[key] }); err == nil ||
		!strings.Contains(err.Error(), "LARK_READINESS_MAX_QUEUE_AGE") {
		t.Fatalf("invalid readiness threshold error = %v", err)
	}
}

func TestLoadValidatesProcessingRecoveryDurations(t *testing.T) {
	values := map[string]string{
		"LARK_CONTROLLER_DB_PATH":           "/data/controller.sqlite",
		"LARK_APP_ID":                       "cli_test",
		"LARK_APP_SECRET":                   "app-secret",
		"LARK_VERIFICATION_TOKEN":           "verification-token",
		"LARK_EVENT_ENCRYPT_KEY":            "event-encryption-key",
		"LARK_TENANT_KEY":                   "tenant-test",
		"LARK_ACTIVE_POLICY_VERSION":        "employee-v1",
		"LARK_POLICY_BUNDLE_DIR":            "/policies",
		"LARK_APPROVAL_BINDINGS_FILE":       "/policies/approval-bindings.json",
		"LARK_GRANT_PAYLOAD_KEYRING_FILE":   "/run/secrets/lark_grant_payload_keyring",
		"LARK_READINESS_MAX_QUEUE_AGE":      "30m",
		"LARK_PROCESSING_LEASE_TIMEOUT":     "10m",
		"LARK_PROCESSING_RECOVERY_INTERVAL": "2m",
		"NEW_API_BRIDGE_CLIENT_ID":          "bridge-client-id",
		"NEW_API_BRIDGE_CLIENT_SECRET_FILE": "/run/secrets/new_api_bridge_client_secret",
		"NEW_API_OAUTH_CALLBACK_ALLOWLIST":  "https://ai.x2r.store/oauth/lark",
	}
	loaded, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("load processing recovery config: %v", err)
	}
	if loaded.ProcessingLeaseTimeout != 10*time.Minute ||
		loaded.ProcessingRecoveryInterval != 2*time.Minute {
		t.Fatalf("unexpected processing recovery config: %+v", loaded)
	}

	tests := []struct {
		name  string
		value string
	}{
		{"LARK_PROCESSING_LEASE_TIMEOUT", "30s"},
		{"LARK_PROCESSING_LEASE_TIMEOUT", "61m"},
		{"LARK_PROCESSING_RECOVERY_INTERVAL", "5s"},
		{"LARK_PROCESSING_RECOVERY_INTERVAL", "11m"},
		{"LARK_READINESS_MAX_QUEUE_AGE", "12m"},
	}
	for _, test := range tests {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			original := values[test.name]
			values[test.name] = test.value
			_, err := config.Load(func(key string) string { return values[key] })
			values[test.name] = original
			if err == nil || !strings.Contains(err.Error(), test.name) {
				t.Fatalf("invalid %s=%q error = %v", test.name, test.value, err)
			}
		})
	}
}

func TestLoadValidatesOAuthBridgeConfiguration(t *testing.T) {
	values := map[string]string{
		"LARK_CONTROLLER_DB_PATH":           "/data/controller.sqlite",
		"LARK_APP_ID":                       "cli_test",
		"LARK_APP_SECRET":                   "app-secret",
		"LARK_VERIFICATION_TOKEN":           "verification-token",
		"LARK_EVENT_ENCRYPT_KEY":            "event-encryption-key",
		"LARK_TENANT_KEY":                   "tenant-test",
		"LARK_ACTIVE_POLICY_VERSION":        "employee-v1",
		"LARK_POLICY_BUNDLE_DIR":            "/policies",
		"LARK_APPROVAL_BINDINGS_FILE":       "/policies/approval-bindings.json",
		"LARK_GRANT_PAYLOAD_KEYRING_FILE":   "/run/secrets/lark_grant_payload_keyring",
		"NEW_API_BRIDGE_CLIENT_ID":          "bridge-client-id",
		"NEW_API_BRIDGE_CLIENT_SECRET_FILE": "/run/secrets/new_api_bridge_client_secret",
		"NEW_API_OAUTH_CALLBACK_ALLOWLIST":  "https://ai.x2r.store/oauth/lark",
		"LARK_OAUTH_RATE_LIMIT_PER_MINUTE":  "45",
		"LARK_OAUTH_TRUSTED_PROXY_CIDRS":    "172.31.20.0/24,10.0.0.0/8",
	}
	loaded, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("load valid OAuth bridge config: %v", err)
	}
	if loaded.OAuthRateLimitPerMinute != 45 || len(loaded.OAuthTrustedProxyCIDRs) != 2 ||
		loaded.OAuthTrustedProxyCIDRs[0].String() != "172.31.20.0/24" ||
		loaded.OAuthTrustedProxyCIDRs[1].String() != "10.0.0.0/8" {
		t.Fatalf("unexpected OAuth limiter config: %+v", loaded)
	}

	invalid := map[string]string{
		"NEW_API_BRIDGE_CLIENT_ID":          "",
		"NEW_API_BRIDGE_CLIENT_SECRET_FILE": "",
		"NEW_API_OAUTH_CALLBACK_ALLOWLIST":  "https://ai.x2r.store/oauth/lark/attacker",
		"LARK_OAUTH_RATE_LIMIT_PER_MINUTE":  "0",
		"LARK_OAUTH_TRUSTED_PROXY_CIDRS":    "172.31.20.1/24",
	}
	for name, value := range invalid {
		t.Run(name, func(t *testing.T) {
			original := values[name]
			values[name] = value
			_, err := config.Load(func(key string) string { return values[key] })
			values[name] = original
			if err == nil || !strings.Contains(err.Error(), name) ||
				(name != "LARK_OAUTH_RATE_LIMIT_PER_MINUTE" && value != "" && strings.Contains(err.Error(), value)) {
				t.Fatalf("invalid %s error=%v, want redacted field validation", name, err)
			}
		})
	}

	values["NEW_API_OAUTH_CALLBACK_ALLOWLIST"] =
		"https://ai.x2r.store/oauth/lark,https://attacker.example/callback"
	if _, err := config.Load(func(key string) string { return values[key] }); err == nil ||
		!strings.Contains(err.Error(), "NEW_API_OAUTH_CALLBACK_ALLOWLIST") {
		t.Fatalf("multiple callback allowlist error=%v", err)
	}
}
