package configcli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

func runInit(arguments []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("lark-config init", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	var directory string
	flags.StringVar(&directory, "dir", "lark-runtime/config", "configuration source directory")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("init does not accept positional arguments")
	}
	sourcePath := filepath.Join(directory, "policy.json")
	bindingPath := filepath.Join(directory, "production.binding.json")
	for _, path := range []string{sourcePath, bindingPath} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("refusing to overwrite existing configuration %q", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	source, binding := initialTemplates()
	sourceContents, err := marshalDocument(source)
	if err != nil {
		return err
	}
	bindingContents, err := marshalDocument(binding)
	if err != nil {
		return err
	}
	if err := atomicWriteWithin(directory, sourcePath, sourceContents, 0o600); err != nil {
		return err
	}
	if err := atomicWriteWithin(directory, bindingPath, bindingContents, 0o600); err != nil {
		return err
	}
	return writeJSON(output, struct {
		Status  string   `json:"status"`
		Created []string `json:"created"`
	}{Status: "initialized", Created: []string{sourcePath, bindingPath}})
}

func initialTemplates() (tenantconfig.Source, tenantconfig.EnvironmentBinding) {
	levels := []policy.Level{
		{LevelCode: "basic", Rank: 10, MonthlyQuota: 5_000_000},
		{LevelCode: "plus", Rank: 20, MonthlyQuota: 12_500_000},
		{LevelCode: "pro", Rank: 30, MonthlyQuota: 25_000_000},
		{LevelCode: "power", Rank: 40, MonthlyQuota: 50_000_000},
	}
	source := tenantconfig.Source{
		FormatVersion: 1,
		Policy: tenantconfig.PolicySource{
			PolicyVersion: "employee-entitlements-YYYY-MM-v1", State: policy.PolicyStateActive,
			Levels: levels,
			WalletPackages: []policy.WalletPackage{
				{PackageCode: "topup_5", QuotaDelta: 2_500_000},
				{PackageCode: "topup_10", QuotaDelta: 5_000_000},
				{PackageCode: "topup_25", QuotaDelta: 12_500_000},
				{PackageCode: "topup_50", QuotaDelta: 25_000_000},
			},
		},
		Approvals: []policy.DefinitionManifest{
			{
				ApprovalKind: policy.ApprovalKindWalletTopUp, Locale: "zh-CN",
				Fields: []policy.ManifestField{
					{CustomID: "cost_center", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "estimated_usage", Type: "textarea", Required: false, Options: []policy.ManifestOption{}},
					{CustomID: "request_reason", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "wallet_package", Type: "radioV2", Required: true, Options: []policy.ManifestOption{
						{DisplayText: "$5", Code: "topup_5"}, {DisplayText: "$10", Code: "topup_10"},
						{DisplayText: "$25", Code: "topup_25"}, {DisplayText: "$50", Code: "topup_50"},
					}},
				},
			},
			{
				ApprovalKind: policy.ApprovalKindSubscriptionLevel, Locale: "zh-CN",
				Fields: []policy.ManifestField{
					{CustomID: "cost_center", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "estimated_usage", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "request_reason", Type: "textarea", Required: true, Options: []policy.ManifestOption{}},
					{CustomID: "target_level", Type: "radioV2", Required: true, Options: []policy.ManifestOption{
						{DisplayText: "Plus", Code: "plus"}, {DisplayText: "Pro", Code: "pro"}, {DisplayText: "Power", Code: "power"},
					}},
				},
			},
		},
	}
	binding := tenantconfig.EnvironmentBinding{
		FormatVersion: 1, Environment: "production", PublicOrigin: "https://ai.example.com",
		Lark: tenantconfig.LarkBinding{
			AppID: "REPLACE_WITH_LARK_APP_ID", TenantKey: "REPLACE_WITH_TENANT_KEY",
			ApprovalCodes: map[policy.ApprovalKind]string{
				policy.ApprovalKindWalletTopUp:       "REPLACE_WITH_WALLET_APPROVAL_CODE",
				policy.ApprovalKindSubscriptionLevel: "REPLACE_WITH_LEVEL_APPROVAL_CODE",
			},
		},
		NewAPI: tenantconfig.NewAPIBinding{
			BridgeClientID:        "lark-controller",
			ManagedPlanIDs:        map[string]int64{"basic": 0, "plus": 0, "pro": 0, "power": 0},
			PlanResetContractHash: strings.Repeat("0", 64),
		},
		SecretRefs: tenantconfig.SecretRefs{
			LarkAppSecret: "lark_app_secret", BridgeClientSecret: "new_api_bridge_client_secret",
			IntegrationSecret: "lark_integration_secret", GrantPayloadKeyring: "lark_grant_payload_keyring",
		},
	}
	return source, binding
}
