package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/config"
)

func TestLoadRequiresCommonSecretsAndDefaultsToShadowMode(t *testing.T) {
	values := map[string]string{
		"LARK_CONTROLLER_MODE":            "shadow",
		"LARK_CONTROLLER_DB_PATH":         "/data/controller.sqlite",
		"LARK_APP_ID":                     "cli_test",
		"LARK_APP_SECRET":                 "app-secret",
		"LARK_VERIFICATION_TOKEN":         "verification-token",
		"LARK_EVENT_ENCRYPT_KEY":          "event-encryption-key",
		"LARK_TENANT_KEY":                 "tenant-test",
		"LARK_ACTIVE_POLICY_VERSION":      "employee-v1",
		"LARK_POLICY_BUNDLE_DIR":          "/policies",
		"LARK_APPROVAL_BINDINGS_FILE":     "/policies/approval-bindings.json",
		"LARK_GRANT_PAYLOAD_KEYRING_FILE": "/run/secrets/lark_grant_payload_keyring",
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

func TestLoadRequiresNewAPIConfigOnlyInActiveMode(t *testing.T) {
	values := map[string]string{
		"LARK_CONTROLLER_MODE":            "active",
		"LARK_CONTROLLER_DB_PATH":         "/data/controller.sqlite",
		"LARK_APP_ID":                     "cli_test",
		"LARK_APP_SECRET":                 "app-secret",
		"LARK_VERIFICATION_TOKEN":         "verification-token",
		"LARK_EVENT_ENCRYPT_KEY":          "event-encryption-key",
		"LARK_TENANT_KEY":                 "tenant-test",
		"LARK_ACTIVE_POLICY_VERSION":      "employee-v1",
		"LARK_POLICY_BUNDLE_DIR":          "/policies",
		"LARK_APPROVAL_BINDINGS_FILE":     "/policies/approval-bindings.json",
		"LARK_GRANT_PAYLOAD_KEYRING_FILE": "/run/secrets/lark_grant_payload_keyring",
		"LARK_INTEGRATION_SECRET_FILE":    "/run/secrets/lark_integration_secret",
		"NEW_API_INTERNAL_BASE_URL":       "http://new-api:3001",
	}
	loaded, err := config.Load(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("load active config: %v", err)
	}
	if loaded.Mode != "active" || loaded.NewAPIBaseURL != "http://new-api:3001" ||
		loaded.IntegrationSecretFile != "/run/secrets/lark_integration_secret" {
		t.Fatalf("unexpected active config: %+v", loaded)
	}

	for _, name := range []string{"LARK_INTEGRATION_SECRET_FILE", "NEW_API_INTERNAL_BASE_URL"} {
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
	delete(values, "NEW_API_INTERNAL_BASE_URL")
	activeLookups := 0
	if _, err := config.Load(func(key string) string {
		if key == "LARK_INTEGRATION_SECRET_FILE" || key == "NEW_API_INTERNAL_BASE_URL" {
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
		"LARK_CONTROLLER_DB_PATH":         "/data/controller.sqlite",
		"LARK_APP_ID":                     "cli_test",
		"LARK_APP_SECRET":                 "app-secret",
		"LARK_VERIFICATION_TOKEN":         "verification-token",
		"LARK_EVENT_ENCRYPT_KEY":          "event-encryption-key",
		"LARK_TENANT_KEY":                 "tenant-test",
		"LARK_ACTIVE_POLICY_VERSION":      "employee-v1",
		"LARK_POLICY_BUNDLE_DIR":          "/policies",
		"LARK_APPROVAL_BINDINGS_FILE":     "/policies/approval-bindings.json",
		"LARK_GRANT_PAYLOAD_KEYRING_FILE": "/run/secrets/lark_grant_payload_keyring",
		"LARK_READINESS_MAX_QUEUE_AGE":    "20m",
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
