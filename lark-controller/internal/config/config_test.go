package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/config"
)

func TestLoadRequiresSecretsAndOnlyAllowsShadowMode(t *testing.T) {
	values := map[string]string{
		"LARK_CONTROLLER_MODE":        "shadow",
		"LARK_CONTROLLER_DB_PATH":     "/data/controller.sqlite",
		"LARK_APP_ID":                 "cli_test",
		"LARK_APP_SECRET":             "app-secret",
		"LARK_VERIFICATION_TOKEN":     "verification-token",
		"LARK_EVENT_ENCRYPT_KEY":      "event-encryption-key",
		"LARK_TENANT_KEY":             "tenant-test",
		"LARK_ACTIVE_POLICY_VERSION":  "employee-v1",
		"LARK_POLICY_BUNDLE_DIR":      "/policies",
		"LARK_APPROVAL_BINDINGS_FILE": "/policies/approval-bindings.json",
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
		loaded.ApprovalBindingsFile != "/policies/approval-bindings.json" {
		t.Fatalf("unexpected policy config: %+v", loaded)
	}

	values["LARK_CONTROLLER_MODE"] = "active"
	_, err = config.Load(func(key string) string { return values[key] })
	if err == nil || !strings.Contains(err.Error(), "shadow") {
		t.Fatalf("active mode error = %v, want shadow-only rejection", err)
	}

	values["LARK_CONTROLLER_MODE"] = "shadow"
	delete(values, "LARK_APP_SECRET")
	_, err = config.Load(func(key string) string { return values[key] })
	if err == nil || strings.Contains(err.Error(), "app-secret") {
		t.Fatalf("missing secret error = %v, want non-secret validation error", err)
	}
}

func TestLoadValidatesReadinessQueueAge(t *testing.T) {
	values := map[string]string{
		"LARK_CONTROLLER_DB_PATH":      "/data/controller.sqlite",
		"LARK_APP_ID":                  "cli_test",
		"LARK_APP_SECRET":              "app-secret",
		"LARK_VERIFICATION_TOKEN":      "verification-token",
		"LARK_EVENT_ENCRYPT_KEY":       "event-encryption-key",
		"LARK_TENANT_KEY":              "tenant-test",
		"LARK_ACTIVE_POLICY_VERSION":   "employee-v1",
		"LARK_POLICY_BUNDLE_DIR":       "/policies",
		"LARK_APPROVAL_BINDINGS_FILE":  "/policies/approval-bindings.json",
		"LARK_READINESS_MAX_QUEUE_AGE": "20m",
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
