package configcli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

func TestRunCheckAndRejectLocalOnlyConfigurationPlan(t *testing.T) {
	source, binding := testConfiguration()
	root := t.TempDir()
	sourcePath := filepath.Join(root, "policy.json")
	bindingPath := filepath.Join(root, "production.json")
	writeFixtureJSON(t, sourcePath, source)
	writeFixtureJSON(t, bindingPath, binding)

	var output bytes.Buffer
	var errorOutput bytes.Buffer
	if err := Run(context.Background(), []string{
		"check", "--source", sourcePath, "--binding", bindingPath,
	}, &output, &errorOutput); err != nil {
		t.Fatalf("check configuration: %v stderr=%s", err, errorOutput.String())
	}
	if !strings.Contains(output.String(), `"status": "valid"`) || strings.Contains(output.String(), binding.Lark.TenantKey) {
		t.Fatalf("unexpected check output: %s", output.String())
	}

	output.Reset()
	errorOutput.Reset()
	runtimeRoot := filepath.Join(root, "runtime")
	planPath := filepath.Join(runtimeRoot, "ops", "plan.json")
	if err := Run(context.Background(), []string{
		"plan", "--source", sourcePath, "--binding", bindingPath,
		"--output-root", runtimeRoot, "--plan", planPath, "--remote", "none",
	}, &output, &errorOutput); err != nil {
		t.Fatalf("plan configuration: %v stderr=%s", err, errorOutput.String())
	}
	planContents, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	var plan tenantconfig.ChangePlan
	if err := json.Unmarshal(planContents, &plan); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if plan.Digest == "" || len(plan.Changes) == 0 || len(plan.Blockers) != 2 {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	for _, change := range plan.Changes {
		if change.Target != "local" {
			t.Fatalf("local-only plan contains target %q", change.Target)
		}
	}

	applyArgs := []string{
		"apply", "--plan", planPath, "--output-root", runtimeRoot,
		"--receipt", filepath.Join(runtimeRoot, "ops", "apply-receipt.json"), "--expected-digest", plan.Digest,
		"--change-ticket", "CHG-2026-0042",
	}
	if err := Run(context.Background(), applyArgs, &bytes.Buffer{}, &bytes.Buffer{}); err == nil ||
		!strings.Contains(err.Error(), "blockers") {
		t.Fatalf("local-only plan apply error = %v, want blockers", err)
	}
}

func TestRunCheckRejectsDuplicateJSONKeys(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "policy.json")
	bindingPath := filepath.Join(root, "binding.json")
	if err := os.WriteFile(sourcePath, []byte(`{"format_version":1,"format_version":1}`), 0o600); err != nil {
		t.Fatalf("write duplicate source: %v", err)
	}
	_, binding := testConfiguration()
	writeFixtureJSON(t, bindingPath, binding)

	err := Run(context.Background(), []string{
		"check", "--source", sourcePath, "--binding", bindingPath,
	}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "duplicate JSON object key") {
		t.Fatalf("duplicate JSON error = %v", err)
	}
}

func TestReadSecretTokenAcceptsOneCRLFTerminator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config-secret")
	want := strings.Repeat("s", 32)
	if err := os.WriteFile(path, []byte(want+"\r\n"), 0o600); err != nil {
		t.Fatalf("write CRLF secret: %v", err)
	}
	got, err := readSecretToken(path, 32, 4096)
	if err != nil {
		t.Fatalf("read CRLF secret: %v", err)
	}
	if got != want {
		t.Fatalf("secret = %q, want %q", got, want)
	}
}

func TestAtomicWriteRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o755); err != nil {
		t.Fatalf("create real parent: %v", err)
	}
	symlinkParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, symlinkParent); err != nil {
		t.Fatalf("create parent symlink: %v", err)
	}

	err := atomicWriteWithin(root, filepath.Join(symlinkParent, "plan-or-receipt.json"), []byte("{}\n"), 0o600)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("atomic write through symlink parent error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(realParent, "plan-or-receipt.json")); !os.IsNotExist(err) {
		t.Fatalf("symlink target was written, stat error = %v", err)
	}
}

func TestInitialTemplatesIncludeApprovalCostAndUsageContext(t *testing.T) {
	source, _ := initialTemplates()
	wantPackages := []policy.WalletPackage{
		{PackageCode: "topup_5", QuotaDelta: 2_500_000},
		{PackageCode: "topup_10", QuotaDelta: 5_000_000},
		{PackageCode: "topup_25", QuotaDelta: 12_500_000},
		{PackageCode: "topup_50", QuotaDelta: 25_000_000},
	}
	if !reflect.DeepEqual(source.Policy.WalletPackages, wantPackages) {
		t.Fatalf("wallet packages = %+v, want %+v", source.Policy.WalletPackages, wantPackages)
	}
	if len(source.Approvals) != 2 {
		t.Fatalf("approval template count = %d, want 2", len(source.Approvals))
	}
	for _, approval := range source.Approvals {
		fields := make(map[string]policy.ManifestField, len(approval.Fields))
		for _, field := range approval.Fields {
			fields[field.CustomID] = field
		}
		if !fields["cost_center"].Required || fields["cost_center"].Type != "textarea" {
			t.Fatalf("%s cost center field = %+v", approval.ApprovalKind, fields["cost_center"])
		}
		estimated := fields["estimated_usage"]
		wantRequired := approval.ApprovalKind == policy.ApprovalKindSubscriptionLevel
		if estimated.Type != "textarea" || estimated.Required != wantRequired {
			t.Fatalf("%s estimated usage field = %+v, required want %t", approval.ApprovalKind, estimated, wantRequired)
		}
		if approval.ApprovalKind == policy.ApprovalKindWalletTopUp {
			wantOptions := []policy.ManifestOption{
				{DisplayText: "$5", Code: "topup_5"},
				{DisplayText: "$10", Code: "topup_10"},
				{DisplayText: "$25", Code: "topup_25"},
				{DisplayText: "$50", Code: "topup_50"},
			}
			if !reflect.DeepEqual(fields["wallet_package"].Options, wantOptions) {
				t.Fatalf("wallet package options = %+v, want %+v", fields["wallet_package"].Options, wantOptions)
			}
		}
	}
}

func testConfiguration() (tenantconfig.Source, tenantconfig.EnvironmentBinding) {
	source := tenantconfig.Source{
		FormatVersion: 1,
		Policy: tenantconfig.PolicySource{
			PolicyVersion: "employee-v1", State: policy.PolicyStateActive,
			Levels: []policy.Level{
				{LevelCode: "basic", Rank: 10, MonthlyQuota: 5_000_000},
				{LevelCode: "plus", Rank: 20, MonthlyQuota: 12_500_000},
			},
			WalletPackages: []policy.WalletPackage{
				{PackageCode: "topup_5", QuotaDelta: 2_500_000},
			},
		},
		Approvals: []policy.DefinitionManifest{
			{
				ApprovalKind: policy.ApprovalKindWalletTopUp, Locale: "zh-CN",
				Fields: []policy.ManifestField{
					{CustomID: "cost_center", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "estimated_usage", Type: "textarea", Required: false, Options: []policy.ManifestOption{}},
					{CustomID: "request_reason", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "wallet_package", Type: "radioV2", Required: true, Options: []policy.ManifestOption{{DisplayText: "Small", Code: "topup_5"}}},
				},
			},
			{
				ApprovalKind: policy.ApprovalKindSubscriptionLevel, Locale: "zh-CN",
				Fields: []policy.ManifestField{
					{CustomID: "cost_center", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "estimated_usage", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "request_reason", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "target_level", Type: "radioV2", Required: true, Options: []policy.ManifestOption{{DisplayText: "Plus", Code: "plus"}}},
				},
			},
		},
	}
	binding := tenantconfig.EnvironmentBinding{
		FormatVersion: 1, Environment: "production", PublicOrigin: "https://ai.example.com",
		Lark: tenantconfig.LarkBinding{
			AppID: "cli_public_app_id", TenantKey: "tenant-public-key",
			ApprovalCodes: map[policy.ApprovalKind]string{
				policy.ApprovalKindWalletTopUp:       "approval-wallet-v1",
				policy.ApprovalKindSubscriptionLevel: "approval-level-v1",
			},
		},
		NewAPI: tenantconfig.NewAPIBinding{
			BridgeClientID: "lark-controller", ManagedPlanIDs: map[string]int64{"basic": 101, "plus": 102},
			PlanResetContractHash: strings.Repeat("a", 64),
		},
		SecretRefs: tenantconfig.SecretRefs{
			LarkAppSecret: "lark_app_secret", BridgeClientSecret: "new_api_bridge_client_secret",
			IntegrationSecret: "lark_integration_secret", GrantPayloadKeyring: "lark_grant_payload_keyring",
		},
	}
	return source, binding
}

func writeFixtureJSON(t *testing.T, path string, value any) {
	t.Helper()
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	contents = append(contents, '\n')
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
