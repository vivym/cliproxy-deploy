package tenantconfig_test

import (
	"bytes"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

const walletManifestFingerprint = "sha256:05a573f10bcfffbc406a503394cccbc2a8f168812ce0ddfba5b689de3e84608a"

func TestCompileProducesDeterministicControllerArtifactsAndRedactedReceipt(t *testing.T) {
	source := tenantconfig.Source{
		FormatVersion: 1,
		Policy: tenantconfig.PolicySource{
			PolicyVersion: "employee-v1",
			State:         policy.PolicyStateActive,
			Levels: []policy.Level{
				{LevelCode: "plus", Rank: 20, MonthlyQuota: 12_500_000},
				{LevelCode: "basic", Rank: 10, MonthlyQuota: 5_000_000},
			},
			WalletPackages: []policy.WalletPackage{
				{PackageCode: "topup_10", QuotaDelta: 5_000_000},
				{PackageCode: "topup_5", QuotaDelta: 2_500_000},
			},
		},
		Approvals: []policy.DefinitionManifest{{
			ApprovalKind: policy.ApprovalKindWalletTopUp,
			Locale:       "zh-CN",
			Fields: []policy.ManifestField{
				{CustomID: "cost_center", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
				{CustomID: "estimated_usage", Type: "textarea", Required: false, Options: []policy.ManifestOption{}},
				{CustomID: "request_reason", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
				{
					CustomID: "wallet_package",
					Type:     "radioV2",
					Required: true,
					Options: []policy.ManifestOption{
						{DisplayText: "Small", Code: "topup_5"},
						{DisplayText: "Large", Code: "topup_10"},
					},
				},
			},
		}, {
			ApprovalKind: policy.ApprovalKindSubscriptionLevel,
			Locale:       "zh-CN",
			Fields: []policy.ManifestField{
				{CustomID: "cost_center", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
				{CustomID: "estimated_usage", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
				{CustomID: "request_reason", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
				{
					CustomID: "target_level", Type: "radioV2", Required: true,
					Options: []policy.ManifestOption{{DisplayText: "Plus", Code: "plus"}},
				},
			},
		}},
	}
	binding := tenantconfig.EnvironmentBinding{
		FormatVersion: 1,
		Environment:   "production",
		PublicOrigin:  "https://ai.example.com",
		Lark: tenantconfig.LarkBinding{
			AppID:     "cli_public_app_id",
			TenantKey: "tenant-public-key",
			ApprovalCodes: map[policy.ApprovalKind]string{
				policy.ApprovalKindWalletTopUp:       "approval-wallet-v1",
				policy.ApprovalKindSubscriptionLevel: "approval-level-v1",
			},
		},
		NewAPI: tenantconfig.NewAPIBinding{
			BridgeClientID:        "lark-controller",
			ManagedPlanIDs:        map[string]int64{"plus": 102, "basic": 101},
			PlanResetContractHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		SecretRefs: tenantconfig.SecretRefs{
			LarkAppSecret:       "lark_app_secret",
			BridgeClientSecret:  "new_api_bridge_client_secret",
			IntegrationSecret:   "lark_integration_secret",
			GrantPayloadKeyring: "lark_grant_payload_keyring",
		},
	}

	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}

	policyArtifact := requireArtifact(t, compiled, "policies/employee-v1.policy.json")
	wantPolicy := "{\n" +
		"  \"format_version\": 1,\n" +
		"  \"policy_version\": \"employee-v1\",\n" +
		"  \"state\": \"active\",\n" +
		"  \"levels\": [\n" +
		"    {\n" +
		"      \"level_code\": \"basic\",\n" +
		"      \"rank\": 10,\n" +
		"      \"monthly_quota\": 5000000\n" +
		"    },\n" +
		"    {\n" +
		"      \"level_code\": \"plus\",\n" +
		"      \"rank\": 20,\n" +
		"      \"monthly_quota\": 12500000\n" +
		"    }\n" +
		"  ],\n" +
		"  \"wallet_packages\": [\n" +
		"    {\n" +
		"      \"package_code\": \"topup_10\",\n" +
		"      \"quota_delta\": 5000000\n" +
		"    },\n" +
		"    {\n" +
		"      \"package_code\": \"topup_5\",\n" +
		"      \"quota_delta\": 2500000\n" +
		"    }\n" +
		"  ]\n" +
		"}\n"
	if string(policyArtifact.Contents) != wantPolicy {
		t.Fatalf("policy artifact:\n%s\nwant:\n%s", policyArtifact.Contents, wantPolicy)
	}

	bindingsArtifact := requireArtifact(t, compiled, "policies/approval-bindings.json")
	if !bytes.Contains(bindingsArtifact.Contents, []byte(walletManifestFingerprint)) {
		t.Fatalf("approval bindings do not contain independently fixed fingerprint %q:\n%s", walletManifestFingerprint, bindingsArtifact.Contents)
	}
	if !bytes.Contains(bindingsArtifact.Contents, []byte(`"approval_code": "approval-wallet-v1"`)) {
		t.Fatalf("approval bindings do not contain environment approval code:\n%s", bindingsArtifact.Contents)
	}

	receipt := requireArtifact(t, compiled, "receipts/compile.json")
	for _, forbidden := range []string{"client_secret", "app_secret", "integration_secret", "grant_payload"} {
		if bytes.Contains(bytes.ToLower(receipt.Contents), []byte(forbidden)) {
			t.Fatalf("compile receipt contains secret-related field %q:\n%s", forbidden, receipt.Contents)
		}
	}
	if compiled.Digest == "" || !bytes.Contains(receipt.Contents, []byte(compiled.Digest)) {
		t.Fatalf("compile receipt does not bind bundle digest %q:\n%s", compiled.Digest, receipt.Contents)
	}

	reordered := source
	reordered.Policy.Levels = []policy.Level{source.Policy.Levels[1], source.Policy.Levels[0]}
	reordered.Policy.WalletPackages = []policy.WalletPackage{
		source.Policy.WalletPackages[1], source.Policy.WalletPackages[0],
	}
	again, err := tenantconfig.Compile(reordered, binding)
	if err != nil {
		t.Fatalf("compile reordered tenant configuration: %v", err)
	}
	if again.Digest != compiled.Digest {
		t.Fatalf("reordered source digest = %q, want %q", again.Digest, compiled.Digest)
	}
}

func requireArtifact(t *testing.T, compiled tenantconfig.CompiledBundle, path string) tenantconfig.Artifact {
	t.Helper()
	for _, artifact := range compiled.Artifacts {
		if artifact.Path == path {
			return artifact
		}
	}
	t.Fatalf("compiled bundle does not contain artifact %q", path)
	return tenantconfig.Artifact{}
}
