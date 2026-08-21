package policy_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/inbox"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
)

const walletManifestFingerprint = "sha256:2878401247d5cde57a96e03424e944773b21399dc3a68a9508016c2c5adea48b"
const walletAuxManifestFingerprint = "sha256:71c706744f17369215ed3f1a1929ea098f26667bba48f03189597c260a5dc9a6"
const levelManifestFingerprint = "sha256:fb7a57402d28d90ae5552e53aeef13e4e66a38818f61e2aa4326c984784d5f59"

func TestLoadDirectoryResolvesWalletApprovalByExactDisplayText(t *testing.T) {
	policyDirectory := t.TempDir()
	writeFile(t, filepath.Join(policyDirectory, "employee-v1.policy.json"), `{
  "format_version": 1,
  "policy_version": "employee-v1",
  "state": "active",
  "levels": [
    {"level_code": "basic", "rank": 10, "monthly_quota": 5000000},
    {"level_code": "plus", "rank": 20, "monthly_quota": 12500000},
    {"level_code": "pro", "rank": 30, "monthly_quota": 25000000}
  ],
  "wallet_packages": [
    {"package_code": "topup_5", "quota_delta": 2500000},
    {"package_code": "topup_10", "quota_delta": 5000000}
  ]
}`)
	bindingsPath := filepath.Join(t.TempDir(), "approval-bindings.json")
	writeFile(t, bindingsPath, `{
  "format_version": 1,
  "bindings": [{
    "approval_code": "approval-wallet-v1",
    "locale": "zh-CN",
    "policy_version": "employee-v1",
    "approval_kind": "wallet_topup",
    "schema_fingerprint": "`+walletAuxManifestFingerprint+`",
    "manifest": {
      "approval_kind": "wallet_topup",
      "locale": "zh-CN",
      "fields": [
        {"custom_id": "cost_center", "type": "radioV2", "required": true, "options": [
          {"display_text": "Platform", "code": "cost_platform"},
          {"display_text": "Research", "code": "cost_research"}
        ]},
        {"custom_id": "request_reason", "type": "textarea", "required": true, "options": []},
        {"custom_id": "wallet_package", "type": "radioV2", "required": true, "options": [
          {"display_text": "Small", "code": "topup_5"},
          {"display_text": "Large", "code": "topup_10"}
        ]}
      ]
    }
  }, {
    "approval_code": "approval-level-v1",
    "locale": "zh-CN",
    "policy_version": "employee-v1",
    "approval_kind": "subscription_level",
    "schema_fingerprint": "`+levelManifestFingerprint+`",
    "manifest": {
      "approval_kind": "subscription_level",
      "locale": "zh-CN",
      "fields": [
        {"custom_id": "request_reason", "type": "textarea", "required": true, "options": []},
        {"custom_id": "target_level", "type": "radioV2", "required": true, "options": [
          {"display_text": "Plus", "code": "plus"},
          {"display_text": "Pro", "code": "pro"}
        ]}
      ]
    }
  }]
}`)

	catalog, err := policy.LoadDirectory(policyDirectory, bindingsPath)
	if err != nil {
		t.Fatalf("load policy catalog: %v", err)
	}
	if catalog.ActivePolicyVersion() != "employee-v1" {
		t.Fatalf("active policy = %q, want employee-v1", catalog.ActivePolicyVersion())
	}
	base, err := catalog.ResolveBaseSubscription()
	if err != nil {
		t.Fatalf("resolve base subscription: %v", err)
	}
	if base.PolicyVersion != "employee-v1" || base.LevelCode != "basic" ||
		base.LevelRank != 10 || base.MonthlyQuota != 5_000_000 || len(base.CatalogSHA256) != 64 {
		t.Fatalf("unexpected base subscription: %+v", base)
	}
	store, err := inbox.Open(filepath.Join(t.TempDir(), "controller.sqlite"))
	if err != nil {
		t.Fatalf("open policy snapshot store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SyncPolicySnapshot(context.Background(), catalog.Snapshot()); err != nil {
		t.Fatalf("sync loaded catalog snapshot: %v", err)
	}
	if err := store.SyncPolicySnapshot(context.Background(), catalog.Snapshot()); err != nil {
		t.Fatalf("replay loaded catalog snapshot: %v", err)
	}
	resolved, err := catalog.ResolveApproval(policy.ApprovalRequest{
		ApprovalCode: "approval-wallet-v1",
		Locale:       "zh-CN",
		StartTime:    "1787270300000",
		FormJSON: `[
          {"id":"widget-cost","custom_id":"cost_center","name":"Cost center","type":"radioV2","value":"Platform"},
          {"id":"widget-reason","custom_id":"request_reason","name":"Reason","type":"textarea","value":"capacity test"},
          {"id":"widget-package","custom_id":"wallet_package","name":"Package","type":"radioV2","value":"Small"}
        ]`,
	})
	if err != nil {
		t.Fatalf("resolve approval: %v", err)
	}
	if resolved.PolicyVersion != "employee-v1" || resolved.ApprovalKind != policy.ApprovalKindWalletTopUp ||
		resolved.BusinessCode != "topup_5" || resolved.QuotaDelta != 2500000 {
		t.Fatalf("unexpected resolution: %+v", resolved)
	}
	if resolved.SchemaFingerprint != walletAuxManifestFingerprint {
		t.Fatalf("schema fingerprint = %q, want %q", resolved.SchemaFingerprint, walletAuxManifestFingerprint)
	}
	if len(resolved.CatalogSHA256) != 64 {
		t.Fatalf("catalog hash = %q, want 64 hex characters", resolved.CatalogSHA256)
	}

	driftCases := []struct {
		name    string
		request policy.ApprovalRequest
	}{
		{
			name: "display text whitespace",
			request: policy.ApprovalRequest{
				ApprovalCode: "approval-wallet-v1", Locale: "zh-CN", StartTime: "1787270300000",
				FormJSON: `[{"custom_id":"cost_center","type":"radioV2","value":"Platform"},{"custom_id":"request_reason","type":"textarea","value":"test"},{"custom_id":"wallet_package","type":"radioV2","value":" Small "}]`,
			},
		},
		{
			name: "auxiliary radio display text drift",
			request: policy.ApprovalRequest{
				ApprovalCode: "approval-wallet-v1", Locale: "zh-CN", StartTime: "1787270300000",
				FormJSON: `[{"custom_id":"cost_center","type":"radioV2","value":"Unknown"},{"custom_id":"request_reason","type":"textarea","value":"test"},{"custom_id":"wallet_package","type":"radioV2","value":"Small"}]`,
			},
		},
		{
			name: "locale drift",
			request: policy.ApprovalRequest{
				ApprovalCode: "approval-wallet-v1", Locale: "en-US", StartTime: "1787270300000",
				FormJSON: `[{"custom_id":"cost_center","type":"radioV2","value":"Platform"},{"custom_id":"request_reason","type":"textarea","value":"test"},{"custom_id":"wallet_package","type":"radioV2","value":"Small"}]`,
			},
		},
		{
			name: "field order drift",
			request: policy.ApprovalRequest{
				ApprovalCode: "approval-wallet-v1", Locale: "zh-CN", StartTime: "1787270300000",
				FormJSON: `[{"custom_id":"wallet_package","type":"radioV2","value":"Small"},{"custom_id":"cost_center","type":"radioV2","value":"Platform"},{"custom_id":"request_reason","type":"textarea","value":"test"}]`,
			},
		},
		{
			name: "control type drift",
			request: policy.ApprovalRequest{
				ApprovalCode: "approval-wallet-v1", Locale: "zh-CN", StartTime: "1787270300000",
				FormJSON: `[{"custom_id":"cost_center","type":"radioV2","value":"Platform"},{"custom_id":"request_reason","type":"textarea","value":"test"},{"custom_id":"wallet_package","type":"radio","value":"Small"}]`,
			},
		},
		{
			name: "unknown control property",
			request: policy.ApprovalRequest{
				ApprovalCode: "approval-wallet-v1", Locale: "zh-CN", StartTime: "1787270300000",
				FormJSON: `[{"custom_id":"cost_center","type":"radioV2","value":"Platform"},{"custom_id":"request_reason","type":"textarea","value":"test","unexpected":true},{"custom_id":"wallet_package","type":"radioV2","value":"Small"}]`,
			},
		},
		{
			name: "duplicate form key",
			request: policy.ApprovalRequest{
				ApprovalCode: "approval-wallet-v1", Locale: "zh-CN", StartTime: "1787270300000",
				FormJSON: `[{"custom_id":"cost_center","type":"radioV2","value":"Platform"},{"custom_id":"request_reason","type":"textarea","value":"test"},{"custom_id":"wallet_package","type":"radioV2","value":"Large","value":"Small"}]`,
			},
		},
	}
	for _, test := range driftCases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := catalog.ResolveApproval(test.request); err == nil {
				t.Fatal("drifted approval form was accepted")
			}
		})
	}

	level, err := catalog.ResolveApproval(policy.ApprovalRequest{
		ApprovalCode: "approval-level-v1",
		Locale:       "zh-CN",
		StartTime:    "1787270300000",
		FormJSON: `[
          {"custom_id":"request_reason","type":"textarea","value":"monthly research"},
          {"custom_id":"target_level","type":"radioV2","value":"Pro"}
        ]`,
	})
	if err != nil {
		t.Fatalf("resolve subscription approval: %v", err)
	}
	if level.ApprovalKind != policy.ApprovalKindSubscriptionLevel || level.BusinessCode != "pro" ||
		level.LevelRank != 30 || level.MonthlyQuota != 25000000 || level.QuotaDelta != 0 ||
		level.SchemaFingerprint != levelManifestFingerprint {
		t.Fatalf("unexpected subscription resolution: %+v", level)
	}
}

func TestResolveApprovalUsesDrainingHistoricalPolicyAndEnforcesStartWindow(t *testing.T) {
	policyDirectory := t.TempDir()
	writeFile(t, filepath.Join(policyDirectory, "employee-v1.policy.json"), `{
  "format_version": 1,
  "policy_version": "employee-v1",
  "state": "draining",
  "levels": [
    {"level_code": "basic", "rank": 10, "monthly_quota": 5000000},
    {"level_code": "plus", "rank": 20, "monthly_quota": 12500000},
    {"level_code": "pro", "rank": 30, "monthly_quota": 25000000}
  ],
  "wallet_packages": [
    {"package_code": "topup_5", "quota_delta": 2500000},
    {"package_code": "topup_10", "quota_delta": 5000000}
  ]
}`)
	writeFile(t, filepath.Join(policyDirectory, "employee-v2.policy.json"), `{
  "format_version": 1,
  "policy_version": "employee-v2",
  "state": "active",
  "levels": [
    {"level_code": "basic", "rank": 10, "monthly_quota": 6000000},
    {"level_code": "plus", "rank": 20, "monthly_quota": 15000000},
    {"level_code": "pro", "rank": 30, "monthly_quota": 30000000}
  ],
  "wallet_packages": [
    {"package_code": "topup_5", "quota_delta": 3000000},
    {"package_code": "topup_10", "quota_delta": 6000000}
  ]
}`)
	bindingsPath := filepath.Join(t.TempDir(), "approval-bindings.json")
	drainingBindings := `{
  "format_version": 1,
  "bindings": [{
    "approval_code": "approval-wallet-v1",
    "locale": "zh-CN",
    "policy_version": "employee-v1",
    "approval_kind": "wallet_topup",
    "schema_fingerprint": "` + walletManifestFingerprint + `",
    "accept_instance_started_before": "2026-08-22T00:00:00Z",
    "manifest": {
      "approval_kind": "wallet_topup",
      "locale": "zh-CN",
      "fields": [
        {"custom_id": "request_reason", "type": "textarea", "required": true, "options": []},
        {"custom_id": "wallet_package", "type": "radioV2", "required": true, "options": [
          {"display_text": "Small", "code": "topup_5"},
          {"display_text": "Large", "code": "topup_10"}
        ]}
      ]
    }
  }, {
    "approval_code": "approval-level-v1",
    "locale": "zh-CN",
    "policy_version": "employee-v1",
    "approval_kind": "subscription_level",
    "schema_fingerprint": "` + levelManifestFingerprint + `",
    "accept_instance_started_before": "2026-08-22T00:00:00Z",
    "manifest": {
      "approval_kind": "subscription_level",
      "locale": "zh-CN",
      "fields": [
        {"custom_id": "request_reason", "type": "textarea", "required": true, "options": []},
        {"custom_id": "target_level", "type": "radioV2", "required": true, "options": [
          {"display_text": "Plus", "code": "plus"},
          {"display_text": "Pro", "code": "pro"}
        ]}
      ]
    }
  }, {
    "approval_code": "approval-wallet-v2",
    "locale": "zh-CN",
    "policy_version": "employee-v2",
    "approval_kind": "wallet_topup",
    "schema_fingerprint": "` + walletManifestFingerprint + `",
    "manifest": {
      "approval_kind": "wallet_topup",
      "locale": "zh-CN",
      "fields": [
        {"custom_id": "request_reason", "type": "textarea", "required": true, "options": []},
        {"custom_id": "wallet_package", "type": "radioV2", "required": true, "options": [
          {"display_text": "Small", "code": "topup_5"},
          {"display_text": "Large", "code": "topup_10"}
        ]}
      ]
    }
  }, {
    "approval_code": "approval-level-v2",
    "locale": "zh-CN",
    "policy_version": "employee-v2",
    "approval_kind": "subscription_level",
    "schema_fingerprint": "` + levelManifestFingerprint + `",
    "manifest": {
      "approval_kind": "subscription_level",
      "locale": "zh-CN",
      "fields": [
        {"custom_id": "request_reason", "type": "textarea", "required": true, "options": []},
        {"custom_id": "target_level", "type": "radioV2", "required": true, "options": [
          {"display_text": "Plus", "code": "plus"},
          {"display_text": "Pro", "code": "pro"}
        ]}
      ]
    }
  }]
}`
	writeFile(t, bindingsPath, strings.Replace(
		drainingBindings,
		`    "accept_instance_started_before": "2026-08-22T00:00:00Z",
`,
		"",
		1,
	))
	if _, err := policy.LoadDirectory(policyDirectory, bindingsPath); err == nil {
		t.Fatal("draining approval binding without a cutoff was accepted")
	}
	writeFile(t, bindingsPath, drainingBindings)
	catalog, err := policy.LoadDirectory(policyDirectory, bindingsPath)
	if err != nil {
		t.Fatalf("load policy catalog: %v", err)
	}
	request := policy.ApprovalRequest{
		ApprovalCode: "approval-wallet-v1",
		Locale:       "zh-CN",
		StartTime:    "1787270300000",
		FormJSON: `[
          {"custom_id":"request_reason","type":"textarea","value":"capacity test"},
          {"custom_id":"wallet_package","type":"radioV2","value":"Small"}
        ]`,
	}
	resolved, err := catalog.ResolveApproval(request)
	if err != nil {
		t.Fatalf("resolve historical approval: %v", err)
	}
	if resolved.PolicyVersion != "employee-v1" || resolved.QuotaDelta != 2500000 {
		t.Fatalf("historical resolution = %+v, want employee-v1 quota 2500000", resolved)
	}

	request.StartTime = "1999999999999"
	if _, err := catalog.ResolveApproval(request); err == nil {
		t.Fatal("approval started after draining cutoff was accepted")
	}
}

func TestLoadDirectoryRejectsDuplicateJSONKeys(t *testing.T) {
	policyDirectory := t.TempDir()
	policyPath := filepath.Join(policyDirectory, "employee-v1.policy.json")
	writeFile(t, policyPath, `{
  "format_version": 1,
  "policy_version": "employee-v1",
  "state": "draining",
  "state": "active",
  "levels": [{"level_code": "basic", "rank": 10, "monthly_quota": 5000000}],
  "wallet_packages": [{"package_code": "topup_5", "quota_delta": 2500000}]
}`)
	bindingsPath := filepath.Join(t.TempDir(), "approval-bindings.json")
	if _, err := policy.LoadDirectory(policyDirectory, bindingsPath); err == nil ||
		!strings.Contains(err.Error(), "duplicate JSON object key") {
		t.Fatalf("duplicate policy key error = %v, want duplicate-key rejection", err)
	}

	writeFile(t, policyPath, `{
  "format_version": 1,
  "policy_version": "employee-v1",
  "state": "active",
  "levels": [{"level_code": "basic", "rank": 10, "monthly_quota": 5000000}],
  "wallet_packages": [{"package_code": "topup_5", "quota_delta": 2500000}]
}`)
	writeFile(t, bindingsPath, `{
  "format_version": 1,
  "bindings": [{
    "approval_code": "approval-wallet-v1",
    "locale": "zh-CN",
    "policy_version": "employee-v1",
    "approval_kind": "wallet_topup",
    "schema_fingerprint": "`+walletManifestFingerprint+`",
    "manifest": {
      "approval_kind": "wallet_topup",
      "locale": "zh-CN",
      "locale": "en-US",
      "fields": []
    }
  }]
}`)
	if _, err := policy.LoadDirectory(policyDirectory, bindingsPath); err == nil ||
		!strings.Contains(err.Error(), "duplicate JSON object key") {
		t.Fatalf("duplicate manifest key error = %v, want duplicate-key rejection", err)
	}
}

func TestResolveBaseSubscriptionRejectsActivePolicyWithoutBasicLevel(t *testing.T) {
	policyDirectory := t.TempDir()
	writeFile(t, filepath.Join(policyDirectory, "employee-v1.policy.json"), `{
  "format_version": 1,
  "policy_version": "employee-v1",
  "state": "active",
  "levels": [
    {"level_code": "plus", "rank": 20, "monthly_quota": 12500000},
    {"level_code": "pro", "rank": 30, "monthly_quota": 25000000}
  ],
  "wallet_packages": [
    {"package_code": "topup_5", "quota_delta": 2500000},
    {"package_code": "topup_10", "quota_delta": 5000000}
  ]
}`)
	bindingsPath := filepath.Join(t.TempDir(), "approval-bindings.json")
	writeFile(t, bindingsPath, `{
  "format_version": 1,
  "bindings": [{
    "approval_code": "approval-wallet-v1",
    "locale": "zh-CN",
    "policy_version": "employee-v1",
    "approval_kind": "wallet_topup",
    "schema_fingerprint": "`+walletManifestFingerprint+`",
    "manifest": {
      "approval_kind": "wallet_topup",
      "locale": "zh-CN",
      "fields": [
        {"custom_id": "request_reason", "type": "textarea", "required": true, "options": []},
        {"custom_id": "wallet_package", "type": "radioV2", "required": true, "options": [
          {"display_text": "Small", "code": "topup_5"},
          {"display_text": "Large", "code": "topup_10"}
        ]}
      ]
    }
  }, {
    "approval_code": "approval-level-v1",
    "locale": "zh-CN",
    "policy_version": "employee-v1",
    "approval_kind": "subscription_level",
    "schema_fingerprint": "`+levelManifestFingerprint+`",
    "manifest": {
      "approval_kind": "subscription_level",
      "locale": "zh-CN",
      "fields": [
        {"custom_id": "request_reason", "type": "textarea", "required": true, "options": []},
        {"custom_id": "target_level", "type": "radioV2", "required": true, "options": [
          {"display_text": "Plus", "code": "plus"},
          {"display_text": "Pro", "code": "pro"}
        ]}
      ]
    }
  }]
}`)

	catalog, err := policy.LoadDirectory(policyDirectory, bindingsPath)
	if err != nil {
		t.Fatalf("load catalog without basic level: %v", err)
	}
	if _, err := catalog.ResolveBaseSubscription(); err == nil || !strings.Contains(err.Error(), "basic") {
		t.Fatalf("resolve base subscription error = %v, want missing basic rejection", err)
	}
}

func TestLoadDirectoryRejectsFutureRetirementBoundary(t *testing.T) {
	policyDirectory := t.TempDir()
	writeFile(t, filepath.Join(policyDirectory, "employee-v0.policy.json"), fmt.Sprintf(`{
  "format_version": 1,
  "policy_version": "employee-v0",
  "state": "retired",
  "retire_after": %q,
  "levels": [{"level_code": "basic", "rank": 10, "monthly_quota": 4000000}],
  "wallet_packages": [{"package_code": "topup_5", "quota_delta": 2000000}]
}`, time.Now().UTC().Add(time.Hour).Format(time.RFC3339)))
	if _, err := policy.LoadDirectory(policyDirectory, filepath.Join(t.TempDir(), "missing-bindings.json")); err == nil || !strings.Contains(err.Error(), "still in the future") {
		t.Fatalf("future retirement error = %v, want fail-closed rejection", err)
	}
}

func TestLoadDirectoryRejectsCrossVersionRankDrift(t *testing.T) {
	policyDirectory := t.TempDir()
	for _, item := range []struct {
		version string
		state   string
		rank    int
	}{
		{version: "employee-v1", state: "draining", rank: 10},
		{version: "employee-v2", state: "active", rank: 20},
	} {
		writeFile(t, filepath.Join(policyDirectory, item.version+".policy.json"), fmt.Sprintf(`{
  "format_version": 1,
  "policy_version": %q,
  "state": %q,
  "levels": [{"level_code": "basic", "rank": %d, "monthly_quota": 5000000}],
  "wallet_packages": [{"package_code": "topup_5", "quota_delta": 2500000}]
}`, item.version, item.state, item.rank))
	}
	if _, err := policy.LoadDirectory(policyDirectory, filepath.Join(t.TempDir(), "missing-bindings.json")); err == nil || !strings.Contains(err.Error(), "changed rank") {
		t.Fatalf("cross-version rank error = %v, want stable-rank rejection", err)
	}
}

func TestLoadDirectoryRequiresBothApprovalKindsForActivePolicy(t *testing.T) {
	policyDirectory := t.TempDir()
	writeFile(t, filepath.Join(policyDirectory, "employee-v1.policy.json"), `{
  "format_version": 1,
  "policy_version": "employee-v1",
  "state": "active",
  "levels": [{"level_code": "basic", "rank": 10, "monthly_quota": 5000000}],
  "wallet_packages": [
    {"package_code": "topup_5", "quota_delta": 2500000},
    {"package_code": "topup_10", "quota_delta": 5000000}
  ]
}`)
	bindingsPath := filepath.Join(t.TempDir(), "approval-bindings.json")
	writeFile(t, bindingsPath, `{
  "format_version": 1,
  "bindings": [{
    "approval_code": "approval-wallet-v1",
    "locale": "zh-CN",
    "policy_version": "employee-v1",
    "approval_kind": "wallet_topup",
    "schema_fingerprint": "`+walletManifestFingerprint+`",
    "manifest": {
      "approval_kind": "wallet_topup",
      "locale": "zh-CN",
      "fields": [
        {"custom_id": "request_reason", "type": "textarea", "required": true, "options": []},
        {"custom_id": "wallet_package", "type": "radioV2", "required": true, "options": [
          {"display_text": "Small", "code": "topup_5"},
          {"display_text": "Large", "code": "topup_10"}
        ]}
      ]
    }
  }]
}`)
	if _, err := policy.LoadDirectory(policyDirectory, bindingsPath); err == nil ||
		!strings.Contains(err.Error(), "subscription_level") {
		t.Fatalf("incomplete active definitions error = %v, want missing subscription definition", err)
	}
}

func TestLoadDirectoryRequiresExactlyOneActivePolicy(t *testing.T) {
	policyDirectory := t.TempDir()
	for _, version := range []string{"employee-v1", "employee-v2"} {
		writeFile(t, filepath.Join(policyDirectory, version+".policy.json"), `{
  "format_version": 1,
  "policy_version": "`+version+`",
  "state": "active",
  "levels": [{"level_code": "basic", "rank": 10, "monthly_quota": 5000000}],
  "wallet_packages": [
    {"package_code": "topup_5", "quota_delta": 2500000},
    {"package_code": "topup_10", "quota_delta": 5000000}
  ]
}`)
	}
	bindingsPath := filepath.Join(t.TempDir(), "approval-bindings.json")
	writeFile(t, bindingsPath, `{
  "format_version": 1,
  "bindings": [{
    "approval_code": "approval-wallet-v1",
    "locale": "zh-CN",
    "policy_version": "employee-v1",
    "approval_kind": "wallet_topup",
    "schema_fingerprint": "`+walletManifestFingerprint+`",
    "manifest": {
      "approval_kind": "wallet_topup",
      "locale": "zh-CN",
      "fields": [
        {"custom_id": "request_reason", "type": "textarea", "required": true, "options": []},
        {"custom_id": "wallet_package", "type": "radioV2", "required": true, "options": [
          {"display_text": "Small", "code": "topup_5"},
          {"display_text": "Large", "code": "topup_10"}
        ]}
      ]
    }
  }]
}`)

	if _, err := policy.LoadDirectory(policyDirectory, bindingsPath); err == nil {
		t.Fatal("catalog with two active policies was accepted")
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
