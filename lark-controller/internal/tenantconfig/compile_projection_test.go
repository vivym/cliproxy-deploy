package tenantconfig_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

const (
	levelManifestFingerprint = "sha256:fbab61010c268cf2c92dc4bc60354d741f3883c1428fc6d759e1f6e73c5f5d60"
	wantNewAPICatalogHash    = "ce26bf8e1d8734d805f9bf03c4dadb0e218b7fc0542cb4e0802640a457fe2dee"
)

func TestCompileProducesRuntimeValidCatalogAndOperableProjections(t *testing.T) {
	source, binding := completeConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile complete tenant configuration: %v", err)
	}

	policyDirectory := t.TempDir()
	for _, path := range []string{"policies/employee-v1.policy.json", "policies/approval-bindings.json"} {
		artifact := requireArtifact(t, compiled, path)
		if err := os.WriteFile(filepath.Join(policyDirectory, filepath.Base(path)), artifact.Contents, 0o600); err != nil {
			t.Fatalf("write compiled controller artifact %q: %v", path, err)
		}
	}
	catalog, err := policy.LoadDirectory(policyDirectory, filepath.Join(policyDirectory, "approval-bindings.json"))
	if err != nil {
		t.Fatalf("load compiled controller catalog: %v", err)
	}
	if catalog.ActivePolicyVersion() != "employee-v1" {
		t.Fatalf("compiled active policy = %q, want employee-v1", catalog.ActivePolicyVersion())
	}

	publication := requireArtifact(t, compiled, "new-api/policy-publication.json")
	var publicationDocument struct {
		PolicyVersion string `json:"policy_version"`
		CatalogHash   string `json:"catalog_hash"`
		State         string `json:"state"`
		Levels        []struct {
			LevelCode         string `json:"level_code"`
			PlanID            int64  `json:"plan_id"`
			ResetContractHash string `json:"reset_contract_hash"`
		} `json:"levels"`
	}
	if err := json.Unmarshal(publication.Contents, &publicationDocument); err != nil {
		t.Fatalf("decode New API publication: %v", err)
	}
	if publicationDocument.PolicyVersion != "employee-v1" || publicationDocument.State != "staged" ||
		publicationDocument.CatalogHash != wantNewAPICatalogHash {
		t.Fatalf("unexpected New API publication metadata: %+v", publicationDocument)
	}
	if len(publicationDocument.Levels) != 3 || publicationDocument.Levels[0].LevelCode != "basic" ||
		publicationDocument.Levels[0].PlanID != 101 ||
		publicationDocument.Levels[0].ResetContractHash != binding.NewAPI.PlanResetContractHash {
		t.Fatalf("unexpected New API level bindings: %+v", publicationDocument.Levels)
	}
	activation := requireArtifact(t, compiled, "new-api/policy-activation.json")
	if !bytes.Contains(activation.Contents, []byte(`"policy_version": "employee-v1"`)) ||
		!bytes.Contains(activation.Contents, []byte(`"catalog_hash": "`+wantNewAPICatalogHash+`"`)) ||
		bytes.Contains(activation.Contents, []byte("expected_active_policy_version")) {
		t.Fatalf("unexpected initial policy activation command:\n%s", activation.Contents)
	}

	oauth := requireArtifact(t, compiled, "new-api/oauth-provider.json")
	if !bytes.Contains(oauth.Contents, []byte(`"enabled": false`)) ||
		!bytes.Contains(oauth.Contents, []byte(`"client_secret_ref": "new_api_bridge_client_secret"`)) ||
		!bytes.Contains(oauth.Contents, []byte(`"email_field": ""`)) ||
		!bytes.Contains(oauth.Contents, []byte(`"authorization_endpoint": "https://ai.example.com/integrations/lark/oauth/authorize"`)) {
		t.Fatalf("unexpected redacted OAuth provider projection:\n%s", oauth.Contents)
	}
	if bytes.Contains(oauth.Contents, []byte(`"client_secret"`)) {
		t.Fatalf("OAuth provider projection contains a client_secret value field:\n%s", oauth.Contents)
	}

	checklist := requireArtifact(t, compiled, "lark/tenant-preflight.json")
	for _, required := range []string{
		`"console_events"`,
		`"approval_subscriptions"`,
		"approval.instance.status_changed_v4",
		"contact.user.deleted_v3",
		walletManifestFingerprint,
		levelManifestFingerprint,
		"https://ai.example.com/integrations/lark/oauth/callback",
	} {
		if !bytes.Contains(checklist.Contents, []byte(required)) {
			t.Fatalf("Lark preflight does not contain %q:\n%s", required, checklist.Contents)
		}
	}

	controllerEnvironment := requireArtifact(t, compiled, "runtime/controller.env")
	for _, required := range []string{
		"LARK_APP_ID='cli_public_app_id'\n",
		"LARK_APP_SECRET_FILE='/run/secrets/lark-controller/controller/lark_app_secret'\n",
		"LARK_TENANT_KEY='tenant-public-key'\n",
		"LARK_ACTIVE_POLICY_VERSION='employee-v1'\n",
		"LARK_GRANT_PAYLOAD_KEYRING_FILE='/run/secrets/lark-controller/controller/lark_grant_payload_keyring'\n",
		"LARK_INTEGRATION_SECRET_FILE='/run/secrets/lark-controller/shared/lark_integration_secret'\n",
		"NEW_API_BRIDGE_CLIENT_ID='lark-controller'\n",
		"NEW_API_BRIDGE_CLIENT_SECRET_FILE='/run/secrets/lark-controller/controller/new_api_bridge_client_secret'\n",
		"LARK_CONTROLLER_CALLBACK_URI='https://ai.example.com/integrations/lark/oauth/callback'\n",
		"NEW_API_OAUTH_CALLBACK_ALLOWLIST='https://ai.example.com/oauth/lark'\n",
	} {
		if !bytes.Contains(controllerEnvironment.Contents, []byte(required)) {
			t.Fatalf("controller environment does not contain %q:\n%s", required, controllerEnvironment.Contents)
		}
	}
}

func TestCompileShellQuotesRuntimeEnvironmentValues(t *testing.T) {
	source, binding := completeConfiguration()
	binding.NewAPI.BridgeClientID = "bridge'client"

	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile configuration with shell metacharacter: %v", err)
	}

	environment := requireArtifact(t, compiled, "runtime/controller.env")
	if !bytes.Contains(environment.Contents, []byte("NEW_API_BRIDGE_CLIENT_ID='bridge'\\''client'\n")) {
		t.Fatalf("controller environment is not shell quoted:\n%s", environment.Contents)
	}
}

func TestCompileRejectsOversizedSecretReferenceBeforePlanning(t *testing.T) {
	source, binding := completeConfiguration()
	binding.SecretRefs.BridgeClientSecret = strings.Repeat("a", 129)

	if _, err := tenantconfig.Compile(source, binding); err == nil {
		t.Fatal("oversized secret reference was accepted")
	}
}

func TestCompileRejectsReusedSecretReferenceBeforePlanning(t *testing.T) {
	source, binding := completeConfiguration()
	binding.SecretRefs.BridgeClientSecret = binding.SecretRefs.LarkAppSecret

	if _, err := tenantconfig.Compile(source, binding); err == nil ||
		!strings.Contains(err.Error(), "must be distinct") {
		t.Fatalf("reused secret reference error = %v", err)
	}
}

func TestCompileRejectsIncompleteApprovalFieldContracts(t *testing.T) {
	tests := []struct {
		name   string
		kind   policy.ApprovalKind
		mutate func(*policy.DefinitionManifest)
	}{
		{
			name: "missing cost center", kind: policy.ApprovalKindWalletTopUp,
			mutate: func(manifest *policy.DefinitionManifest) {
				manifest.Fields = manifest.Fields[1:]
			},
		},
		{
			name: "wallet estimated usage required", kind: policy.ApprovalKindWalletTopUp,
			mutate: func(manifest *policy.DefinitionManifest) {
				manifest.Fields[1].Required = true
			},
		},
		{
			name: "level estimated usage optional", kind: policy.ApprovalKindSubscriptionLevel,
			mutate: func(manifest *policy.DefinitionManifest) {
				manifest.Fields[1].Required = false
			},
		},
		{
			name: "reason wrong type", kind: policy.ApprovalKindSubscriptionLevel,
			mutate: func(manifest *policy.DefinitionManifest) {
				manifest.Fields[2].Type = "input"
			},
		},
		{
			name: "unapproved extra field", kind: policy.ApprovalKindWalletTopUp,
			mutate: func(manifest *policy.DefinitionManifest) {
				manifest.Fields = append(manifest.Fields, policy.ManifestField{
					CustomID: "raw_quota", Type: "textarea", Required: false,
					Options: []policy.ManifestOption{},
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, binding := completeConfiguration()
			for index := range source.Approvals {
				if source.Approvals[index].ApprovalKind == test.kind {
					test.mutate(&source.Approvals[index])
				}
			}
			if _, err := tenantconfig.Compile(source, binding); err == nil {
				t.Fatal("invalid approval field contract was accepted")
			}
		})
	}
}

func TestCompileRejectsPublicOriginUserInfo(t *testing.T) {
	source, binding := completeConfiguration()
	binding.PublicOrigin = "https://operator:secret@ai.example.com"

	if _, err := tenantconfig.Compile(source, binding); err == nil {
		t.Fatal("public origin containing userinfo was accepted")
	}
}

func completeConfiguration() (tenantconfig.Source, tenantconfig.EnvironmentBinding) {
	source := tenantconfig.Source{
		FormatVersion: 1,
		Policy: tenantconfig.PolicySource{
			PolicyVersion: "employee-v1",
			State:         policy.PolicyStateActive,
			Levels: []policy.Level{
				{LevelCode: "pro", Rank: 30, MonthlyQuota: 25_000_000},
				{LevelCode: "plus", Rank: 20, MonthlyQuota: 12_500_000},
				{LevelCode: "basic", Rank: 10, MonthlyQuota: 5_000_000},
			},
			WalletPackages: []policy.WalletPackage{
				{PackageCode: "topup_5", QuotaDelta: 2_500_000},
				{PackageCode: "topup_10", QuotaDelta: 5_000_000},
			},
		},
		Approvals: []policy.DefinitionManifest{
			{
				ApprovalKind: policy.ApprovalKindSubscriptionLevel,
				Locale:       "zh-CN",
				Fields: []policy.ManifestField{
					{CustomID: "cost_center", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "estimated_usage", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "request_reason", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{
						CustomID: "target_level", Type: "radioV2", Required: true,
						Options: []policy.ManifestOption{
							{DisplayText: "Plus", Code: "plus"},
							{DisplayText: "Pro", Code: "pro"},
						},
					},
				},
			},
			{
				ApprovalKind: policy.ApprovalKindWalletTopUp,
				Locale:       "zh-CN",
				Fields: []policy.ManifestField{
					{CustomID: "cost_center", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "estimated_usage", Type: "textarea", Required: false, Options: []policy.ManifestOption{}},
					{CustomID: "request_reason", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{
						CustomID: "wallet_package", Type: "radioV2", Required: true,
						Options: []policy.ManifestOption{
							{DisplayText: "Small", Code: "topup_5"},
							{DisplayText: "Large", Code: "topup_10"},
						},
					},
				},
			},
		},
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
			ManagedPlanIDs:        map[string]int64{"plus": 102, "basic": 101, "pro": 103},
			PlanResetContractHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		SecretRefs: tenantconfig.SecretRefs{
			LarkAppSecret:       "lark_app_secret",
			BridgeClientSecret:  "new_api_bridge_client_secret",
			IntegrationSecret:   "lark_integration_secret",
			GrantPayloadKeyring: "lark_grant_payload_keyring",
		},
	}
	return source, binding
}
