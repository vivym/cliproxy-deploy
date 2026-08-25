package tenantconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
)

const supportedFormatVersion = 2

type Source struct {
	FormatVersion      int                         `json:"format_version"`
	Policy             PolicySource                `json:"policy"`
	Approvals          []policy.DefinitionManifest `json:"approvals"`
	HistoricalPolicies []HistoricalPolicySource    `json:"historical_policies,omitempty"`
}

type HistoricalPolicySource struct {
	Policy    PolicySource               `json:"policy"`
	Approvals []HistoricalApprovalSource `json:"approvals"`
}

type HistoricalApprovalSource struct {
	ApprovalCode                string                    `json:"approval_code"`
	Manifest                    policy.DefinitionManifest `json:"manifest"`
	AcceptInstanceStartedBefore string                    `json:"accept_instance_started_before"`
}

type PolicySource struct {
	PolicyVersion  string                 `json:"policy_version"`
	State          policy.PolicyState     `json:"state"`
	RetireAfter    string                 `json:"retire_after,omitempty"`
	Levels         []policy.Level         `json:"levels"`
	WalletPackages []policy.WalletPackage `json:"wallet_packages"`
}

type EnvironmentBinding struct {
	FormatVersion int           `json:"format_version"`
	Environment   string        `json:"environment"`
	PublicOrigin  string        `json:"public_origin"`
	Lark          LarkBinding   `json:"lark"`
	NewAPI        NewAPIBinding `json:"new_api"`
	SecretRefs    SecretRefs    `json:"secret_refs"`
}

type LarkBinding struct {
	AppID         string                         `json:"app_id"`
	TenantKey     string                         `json:"tenant_key"`
	ApprovalCodes map[policy.ApprovalKind]string `json:"approval_codes"`
}

type NewAPIBinding struct {
	BridgeClientID                      string           `json:"bridge_client_id"`
	ManagedPlanIDs                      map[string]int64 `json:"managed_plan_ids"`
	PlanResetContractHash               string           `json:"plan_reset_contract_hash"`
	ExpectedActivePolicyVersion         string           `json:"expected_active_policy_version,omitempty"`
	AcceptCurrentInstancesStartedBefore string           `json:"accept_current_instances_started_before,omitempty"`
}

type SecretRefs struct {
	LarkAppSecret       string `json:"lark_app_secret"`
	BridgeClientSecret  string `json:"bridge_client_secret"`
	IntegrationSecret   string `json:"integration_secret"`
	GrantPayloadKeyring string `json:"grant_payload_keyring"`
}

type Artifact struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Contents []byte `json:"-"`
}

type CompiledBundle struct {
	Digest    string     `json:"digest"`
	Artifacts []Artifact `json:"artifacts"`
}

func (bundle CompiledBundle) Artifact(path string) (Artifact, error) {
	for _, artifact := range bundle.Artifacts {
		if artifact.Path == path {
			return artifact, nil
		}
	}
	return Artifact{}, fmt.Errorf("compiled bundle is missing artifact %q", path)
}

type compileReceipt struct {
	FormatVersion int               `json:"format_version"`
	Environment   string            `json:"environment"`
	PolicyVersion string            `json:"policy_version"`
	BundleDigest  string            `json:"bundle_digest"`
	Artifacts     []artifactReceipt `json:"artifacts"`
}

type managedLevelDefinition struct {
	LevelCode         string `json:"level_code"`
	Rank              int    `json:"rank"`
	PeriodQuota       int64  `json:"period_quota"`
	ResetPeriod       string `json:"reset_period"`
	ResetTimezone     string `json:"reset_timezone"`
	PlanID            int64  `json:"plan_id"`
	ResetContractHash string `json:"reset_contract_hash"`
}

type managedWalletPackageDefinition struct {
	PackageCode string `json:"package_code"`
	QuotaDelta  int64  `json:"quota_delta"`
}

type approvalPolicyBindingDefinition struct {
	ApprovalCode           string `json:"approval_code"`
	SchemaFingerprint      string `json:"schema_fingerprint"`
	Locale                 string `json:"locale"`
	ApprovalKind           string `json:"approval_kind"`
	DefinitionManifestHash string `json:"definition_manifest_hash"`
}

type policyPublication struct {
	PolicyVersion    string                            `json:"policy_version"`
	CatalogHash      string                            `json:"catalog_hash"`
	State            string                            `json:"state"`
	Levels           []managedLevelDefinition          `json:"levels"`
	WalletPackages   []managedWalletPackageDefinition  `json:"wallet_packages"`
	ApprovalBindings []approvalPolicyBindingDefinition `json:"approval_bindings"`
}

type OAuthProviderProjection struct {
	FormatVersion int                 `json:"format_version"`
	Provider      OAuthProviderConfig `json:"provider"`
}

type OAuthProviderConfig struct {
	Name                  string `json:"name"`
	Slug                  string `json:"slug"`
	Enabled               bool   `json:"enabled"`
	ClientID              string `json:"client_id"`
	ClientSecretRef       string `json:"client_secret_ref"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserInfoEndpoint      string `json:"user_info_endpoint"`
	Scopes                string `json:"scopes"`
	UserIDField           string `json:"user_id_field"`
	UsernameField         string `json:"username_field"`
	DisplayNameField      string `json:"display_name_field"`
	EmailField            string `json:"email_field"`
	AuthStyle             int    `json:"auth_style"`
}

type policyActivation struct {
	PolicyVersion                       string `json:"policy_version"`
	CatalogHash                         string `json:"catalog_hash"`
	ExpectedActivePolicyVersion         string `json:"expected_active_policy_version,omitempty"`
	AcceptCurrentInstancesStartedBefore string `json:"accept_current_instances_started_before,omitempty"`
}

type LarkTenantPreflight struct {
	FormatVersion         int                        `json:"format_version"`
	Environment           string                     `json:"environment"`
	AppID                 string                     `json:"app_id"`
	TenantKey             string                     `json:"tenant_key"`
	RedirectURLs          []string                   `json:"redirect_urls"`
	ConsoleEvents         []string                   `json:"console_events"`
	ApprovalSubscriptions []LarkApprovalSubscription `json:"approval_subscriptions"`
	ApprovalDefinitions   []LarkApprovalTarget       `json:"approval_definitions"`
}

type LarkApprovalSubscription struct {
	ApprovalCode string `json:"approval_code"`
}

type LarkApprovalSubscriptionMutation struct {
	ApprovalCode string `json:"approval_code"`
	AppID        string `json:"app_id"`
	TenantKey    string `json:"tenant_key"`
}

type LarkApprovalTarget struct {
	ApprovalCode      string                    `json:"approval_code"`
	ApprovalKind      policy.ApprovalKind       `json:"approval_kind"`
	Locale            string                    `json:"locale"`
	SchemaFingerprint string                    `json:"schema_fingerprint"`
	Manifest          policy.DefinitionManifest `json:"manifest"`
}

type artifactReceipt struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func Compile(source Source, binding EnvironmentBinding) (CompiledBundle, error) {
	if err := validateInputs(source, binding); err != nil {
		return CompiledBundle{}, err
	}

	activeBundle, policyContents, err := compilePolicyBundle(source.Policy)
	if err != nil {
		return CompiledBundle{}, fmt.Errorf("compile active policy bundle: %w", err)
	}
	levels := activeBundle.Levels
	packages := activeBundle.WalletPackages
	policyBundles := []policy.PolicyBundle{activeBundle}
	policyArtifacts := []Artifact{
		newArtifact("policies/"+source.Policy.PolicyVersion+".policy.json", policyContents),
	}

	bindings := make([]policy.ApprovalBinding, 0, len(source.Approvals))
	for _, manifest := range source.Approvals {
		_, fingerprint, err := policy.CompileManifest(manifest)
		if err != nil {
			return CompiledBundle{}, fmt.Errorf("compile %s approval manifest: %w", manifest.ApprovalKind, err)
		}
		bindings = append(bindings, policy.ApprovalBinding{
			ApprovalCode:      binding.Lark.ApprovalCodes[manifest.ApprovalKind],
			Locale:            manifest.Locale,
			PolicyVersion:     source.Policy.PolicyVersion,
			ApprovalKind:      manifest.ApprovalKind,
			SchemaFingerprint: fingerprint,
			Manifest:          manifest,
		})
	}
	activeBindings := append([]policy.ApprovalBinding(nil), bindings...)
	for _, historical := range source.HistoricalPolicies {
		historicalBundle, historicalContents, err := compilePolicyBundle(historical.Policy)
		if err != nil {
			return CompiledBundle{}, fmt.Errorf("compile historical policy %q: %w", historical.Policy.PolicyVersion, err)
		}
		policyBundles = append(policyBundles, historicalBundle)
		policyArtifacts = append(policyArtifacts, newArtifact(
			"policies/"+historical.Policy.PolicyVersion+".policy.json",
			historicalContents,
		))
		for _, historicalApproval := range historical.Approvals {
			_, fingerprint, err := policy.CompileManifest(historicalApproval.Manifest)
			if err != nil {
				return CompiledBundle{}, fmt.Errorf(
					"compile historical %s approval manifest: %w",
					historicalApproval.Manifest.ApprovalKind,
					err,
				)
			}
			bindings = append(bindings, policy.ApprovalBinding{
				ApprovalCode:                historicalApproval.ApprovalCode,
				Locale:                      historicalApproval.Manifest.Locale,
				PolicyVersion:               historical.Policy.PolicyVersion,
				ApprovalKind:                historicalApproval.Manifest.ApprovalKind,
				SchemaFingerprint:           fingerprint,
				AcceptInstanceStartedBefore: historicalApproval.AcceptInstanceStartedBefore,
				Manifest:                    historicalApproval.Manifest,
			})
		}
	}
	sort.Slice(bindings, func(left, right int) bool {
		if bindings[left].PolicyVersion != bindings[right].PolicyVersion {
			return bindings[left].PolicyVersion < bindings[right].PolicyVersion
		}
		if bindings[left].ApprovalKind != bindings[right].ApprovalKind {
			return bindings[left].ApprovalKind < bindings[right].ApprovalKind
		}
		if bindings[left].ApprovalCode != bindings[right].ApprovalCode {
			return bindings[left].ApprovalCode < bindings[right].ApprovalCode
		}
		return bindings[left].Locale < bindings[right].Locale
	})
	bindingContents, err := marshalDocument(policy.BindingsFile{
		FormatVersion: supportedFormatVersion,
		Bindings:      bindings,
	})
	if err != nil {
		return CompiledBundle{}, fmt.Errorf("encode approval bindings: %w", err)
	}
	if err := policy.ValidateCatalog(
		policyBundles,
		policy.BindingsFile{FormatVersion: supportedFormatVersion, Bindings: bindings},
	); err != nil {
		return CompiledBundle{}, fmt.Errorf("validate compiled controller catalog: %w", err)
	}

	publication, err := compilePolicyPublication(source, binding, levels, packages, activeBindings)
	if err != nil {
		return CompiledBundle{}, err
	}
	publicationContents, err := marshalDocument(publication)
	if err != nil {
		return CompiledBundle{}, fmt.Errorf("encode New API policy publication: %w", err)
	}
	activationContents, err := marshalDocument(policyActivation{
		PolicyVersion: source.Policy.PolicyVersion, CatalogHash: publication.CatalogHash,
		ExpectedActivePolicyVersion:         binding.NewAPI.ExpectedActivePolicyVersion,
		AcceptCurrentInstancesStartedBefore: binding.NewAPI.AcceptCurrentInstancesStartedBefore,
	})
	if err != nil {
		return CompiledBundle{}, fmt.Errorf("encode New API policy activation: %w", err)
	}
	oauthContents, err := marshalDocument(OAuthProviderProjection{
		FormatVersion: supportedFormatVersion,
		Provider: OAuthProviderConfig{
			Name: "Lark", Slug: "lark", Enabled: false,
			ClientID: binding.NewAPI.BridgeClientID, ClientSecretRef: binding.SecretRefs.BridgeClientSecret,
			AuthorizationEndpoint: binding.PublicOrigin + "/integrations/lark/oauth/authorize",
			TokenEndpoint:         "http://lark-quota-controller:8080/internal/oauth/token",
			UserInfoEndpoint:      "http://lark-quota-controller:8080/internal/oauth/userinfo",
			Scopes:                "openid profile", UserIDField: "sub", UsernameField: "username", DisplayNameField: "name",
			EmailField: "", AuthStyle: 1,
		},
	})
	if err != nil {
		return CompiledBundle{}, fmt.Errorf("encode OAuth provider projection: %w", err)
	}
	approvalTargets := make([]LarkApprovalTarget, 0, len(bindings))
	for _, compiledBinding := range bindings {
		approvalTargets = append(approvalTargets, LarkApprovalTarget{
			ApprovalCode: compiledBinding.ApprovalCode, ApprovalKind: compiledBinding.ApprovalKind,
			Locale: compiledBinding.Locale, SchemaFingerprint: compiledBinding.SchemaFingerprint,
			Manifest: compiledBinding.Manifest,
		})
	}
	subscriptions := make([]LarkApprovalSubscription, 0, len(bindings))
	for _, compiledBinding := range bindings {
		subscriptions = append(subscriptions, LarkApprovalSubscription{
			ApprovalCode: compiledBinding.ApprovalCode,
		})
	}
	sort.Slice(subscriptions, func(left, right int) bool {
		return subscriptions[left].ApprovalCode < subscriptions[right].ApprovalCode
	})
	preflightContents, err := marshalDocument(LarkTenantPreflight{
		FormatVersion: supportedFormatVersion, Environment: binding.Environment,
		AppID: binding.Lark.AppID, TenantKey: binding.Lark.TenantKey,
		RedirectURLs: []string{binding.PublicOrigin + "/integrations/lark/oauth/callback"},
		ConsoleEvents: []string{
			"approval.instance.status_changed_v4",
			"contact.user.deleted_v3",
		},
		ApprovalSubscriptions: subscriptions,
		ApprovalDefinitions:   approvalTargets,
	})
	if err != nil {
		return CompiledBundle{}, fmt.Errorf("encode Lark tenant preflight: %w", err)
	}
	controllerEnvironment := compileControllerEnvironment(source, binding)

	artifacts := append(policyArtifacts, []Artifact{
		newArtifact("policies/approval-bindings.json", bindingContents),
		newArtifact("new-api/policy-publication.json", publicationContents),
		newArtifact("new-api/policy-activation.json", activationContents),
		newArtifact("new-api/oauth-provider.json", oauthContents),
		newArtifact("lark/tenant-preflight.json", preflightContents),
		newArtifact("runtime/controller.env", controllerEnvironment),
	}...)
	sort.Slice(artifacts, func(left, right int) bool { return artifacts[left].Path < artifacts[right].Path })
	digest := digestArtifacts(artifacts)
	receiptArtifacts := make([]artifactReceipt, 0, len(artifacts))
	for _, artifact := range artifacts {
		receiptArtifacts = append(receiptArtifacts, artifactReceipt{Path: artifact.Path, SHA256: artifact.SHA256})
	}
	receiptContents, err := marshalDocument(compileReceipt{
		FormatVersion: supportedFormatVersion,
		Environment:   binding.Environment,
		PolicyVersion: source.Policy.PolicyVersion,
		BundleDigest:  digest,
		Artifacts:     receiptArtifacts,
	})
	if err != nil {
		return CompiledBundle{}, fmt.Errorf("encode compile receipt: %w", err)
	}
	artifacts = append(artifacts, newArtifact("receipts/compile.json", receiptContents))
	return CompiledBundle{Digest: digest, Artifacts: artifacts}, nil
}

func compileControllerEnvironment(source Source, binding EnvironmentBinding) []byte {
	values := []struct {
		name  string
		value string
	}{
		{"LARK_APP_ID", binding.Lark.AppID},
		{"LARK_APP_SECRET_FILE", controllerSecretPath(binding.SecretRefs.LarkAppSecret)},
		{"LARK_TENANT_KEY", binding.Lark.TenantKey},
		{"LARK_ACTIVE_POLICY_VERSION", source.Policy.PolicyVersion},
		{"LARK_GRANT_PAYLOAD_KEYRING_FILE", controllerSecretPath(binding.SecretRefs.GrantPayloadKeyring)},
		{"LARK_INTEGRATION_SECRET_FILE", sharedSecretPath(binding.SecretRefs.IntegrationSecret)},
		{"NEW_API_BRIDGE_CLIENT_ID", binding.NewAPI.BridgeClientID},
		{"NEW_API_BRIDGE_CLIENT_SECRET_FILE", controllerSecretPath(binding.SecretRefs.BridgeClientSecret)},
		{"LARK_CONTROLLER_CALLBACK_URI", binding.PublicOrigin + "/integrations/lark/oauth/callback"},
		{"NEW_API_OAUTH_CALLBACK_ALLOWLIST", binding.PublicOrigin + "/oauth/lark"},
	}
	lines := make([]string, 0, len(values))
	for _, value := range values {
		lines = append(lines, value.name+"="+shellQuote(value.value))
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func controllerSecretPath(ref string) string {
	return "/run/secrets/lark-controller/controller/" + ref
}

func sharedSecretPath(ref string) string {
	return "/run/secrets/lark-controller/shared/" + ref
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func validateInputs(source Source, binding EnvironmentBinding) error {
	if source.FormatVersion != supportedFormatVersion || binding.FormatVersion != supportedFormatVersion {
		return errors.New("source and environment binding require format_version 2")
	}
	if source.Policy.PolicyVersion == "" || source.Policy.State == "" {
		return errors.New("policy version and state are required")
	}
	if source.Policy.State != policy.PolicyStateActive {
		return errors.New("the target policy must be active")
	}
	if len(source.Policy.Levels) == 0 || len(source.Policy.WalletPackages) == 0 || len(source.Approvals) == 0 {
		return errors.New("policy levels, wallet packages, and approvals are required")
	}
	if binding.Environment == "" || binding.Lark.AppID == "" || binding.Lark.TenantKey == "" || binding.NewAPI.BridgeClientID == "" {
		return errors.New("environment, Lark identity, and New API bridge client are required")
	}
	origin, err := url.Parse(binding.PublicOrigin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil ||
		origin.Opaque != "" || origin.Path != "" || origin.RawPath != "" ||
		origin.RawQuery != "" || origin.Fragment != "" {
		return errors.New("public_origin must be an HTTPS origin without a path, query, or fragment")
	}
	seenKinds := make(map[policy.ApprovalKind]struct{}, len(source.Approvals))
	for _, manifest := range source.Approvals {
		if _, duplicate := seenKinds[manifest.ApprovalKind]; duplicate {
			return fmt.Errorf("duplicate approval kind %q", manifest.ApprovalKind)
		}
		seenKinds[manifest.ApprovalKind] = struct{}{}
		if err := validateApprovalManifestContract(manifest); err != nil {
			return fmt.Errorf("invalid %s approval manifest: %w", manifest.ApprovalKind, err)
		}
		if strings.TrimSpace(binding.Lark.ApprovalCodes[manifest.ApprovalKind]) == "" {
			return fmt.Errorf("approval code for %q is required", manifest.ApprovalKind)
		}
	}
	if !validHexDigest(binding.NewAPI.PlanResetContractHash) {
		return errors.New("New API plan_reset_contract_hash must be 64 lowercase hex characters")
	}
	if (binding.NewAPI.ExpectedActivePolicyVersion == "") !=
		(binding.NewAPI.AcceptCurrentInstancesStartedBefore == "") {
		return errors.New("New API policy rotation requires both expected active policy version and approval cutoff")
	}
	if binding.NewAPI.AcceptCurrentInstancesStartedBefore != "" {
		if _, err := time.Parse(time.RFC3339, binding.NewAPI.AcceptCurrentInstancesStartedBefore); err != nil {
			return errors.New("New API approval cutoff must be an RFC3339 timestamp")
		}
		if binding.NewAPI.ExpectedActivePolicyVersion == source.Policy.PolicyVersion {
			return errors.New("New API expected active policy must differ from the target policy")
		}
		retained := false
		for _, historical := range source.HistoricalPolicies {
			if historical.Policy.PolicyVersion != binding.NewAPI.ExpectedActivePolicyVersion {
				continue
			}
			retained = true
			if historical.Policy.State != policy.PolicyStateDraining {
				return errors.New("the expected active policy must become draining during rotation")
			}
			for _, approval := range historical.Approvals {
				if approval.AcceptInstanceStartedBefore != binding.NewAPI.AcceptCurrentInstancesStartedBefore {
					return errors.New("rotated approval cutoffs must match the New API activation cutoff")
				}
			}
		}
		if !retained {
			return errors.New("the expected active policy must be retained in historical_policies")
		}
	}
	for _, historical := range source.HistoricalPolicies {
		if historical.Policy.PolicyVersion == "" || historical.Policy.State == policy.PolicyStateActive ||
			len(historical.Approvals) == 0 {
			return errors.New("historical policies require a non-active version and approval bindings")
		}
		for _, approval := range historical.Approvals {
			if approval.ApprovalCode == "" || approval.AcceptInstanceStartedBefore == "" {
				return errors.New("historical approvals require an approval code and acceptance cutoff")
			}
			if err := validateApprovalManifestContract(approval.Manifest); err != nil {
				return fmt.Errorf("invalid historical %s approval manifest: %w", approval.Manifest.ApprovalKind, err)
			}
			if _, err := time.Parse(time.RFC3339, approval.AcceptInstanceStartedBefore); err != nil {
				return errors.New("historical approval cutoff must be an RFC3339 timestamp")
			}
		}
	}
	if len(binding.NewAPI.ManagedPlanIDs) != len(source.Policy.Levels) {
		return errors.New("New API managed_plan_ids must bind every policy level exactly once")
	}
	seenPlanIDs := make(map[int64]struct{}, len(binding.NewAPI.ManagedPlanIDs))
	for _, level := range source.Policy.Levels {
		planID, exists := binding.NewAPI.ManagedPlanIDs[level.LevelCode]
		if !exists || planID <= 0 {
			return fmt.Errorf("positive New API plan ID for level %q is required", level.LevelCode)
		}
		if _, duplicate := seenPlanIDs[planID]; duplicate {
			return fmt.Errorf("New API plan ID %d is bound more than once", planID)
		}
		seenPlanIDs[planID] = struct{}{}
	}
	secretRefs := []struct {
		name  string
		value string
	}{
		{name: "lark_app_secret", value: binding.SecretRefs.LarkAppSecret},
		{name: "bridge_client_secret", value: binding.SecretRefs.BridgeClientSecret},
		{name: "integration_secret", value: binding.SecretRefs.IntegrationSecret},
		{name: "grant_payload_keyring", value: binding.SecretRefs.GrantPayloadKeyring},
	}
	for _, secretRef := range secretRefs {
		if !validSecretRef(secretRef.value) {
			return errors.New("secret references must be 1 to 128 lowercase letters, digits, underscores, or hyphens")
		}
	}
	seenSecretRefs := make(map[string]string, len(secretRefs))
	for _, secretRef := range secretRefs {
		if previous, duplicate := seenSecretRefs[secretRef.value]; duplicate {
			return fmt.Errorf("secret references %q and %q must be distinct", previous, secretRef.name)
		}
		seenSecretRefs[secretRef.value] = secretRef.name
	}
	if binding.SecretRefs.BridgeClientSecret != "new_api_bridge_client_secret" {
		return errors.New("bridge_client_secret must use the fixed logical reference new_api_bridge_client_secret")
	}
	return nil
}

func validateApprovalManifestContract(manifest policy.DefinitionManifest) error {
	if len(manifest.Fields) != 4 {
		return errors.New("manifest must contain exactly the four approved business fields")
	}
	fields := make(map[string]policy.ManifestField, len(manifest.Fields))
	for _, field := range manifest.Fields {
		fields[field.CustomID] = field
	}
	requireField := func(customID string, required bool, allowedTypes ...string) error {
		field, exists := fields[customID]
		if !exists {
			return fmt.Errorf("field %q is required", customID)
		}
		validType := false
		for _, allowedType := range allowedTypes {
			validType = validType || field.Type == allowedType
		}
		if !validType || field.Required != required {
			return fmt.Errorf("field %q has invalid type or required state", customID)
		}
		return nil
	}
	if err := requireField("request_reason", true, "textarea"); err != nil {
		return err
	}
	if err := requireField("cost_center", true, "textarea", "radioV2"); err != nil {
		return err
	}
	switch manifest.ApprovalKind {
	case policy.ApprovalKindWalletTopUp:
		return requireField("estimated_usage", false, "textarea")
	case policy.ApprovalKindSubscriptionLevel:
		return requireField("estimated_usage", true, "textarea")
	default:
		return fmt.Errorf("unsupported approval kind %q", manifest.ApprovalKind)
	}
}

func compilePolicyBundle(source PolicySource) (policy.PolicyBundle, []byte, error) {
	levels := append([]policy.Level(nil), source.Levels...)
	sort.Slice(levels, func(left, right int) bool {
		return levels[left].LevelCode < levels[right].LevelCode
	})
	packages := append([]policy.WalletPackage(nil), source.WalletPackages...)
	sort.Slice(packages, func(left, right int) bool {
		return packages[left].PackageCode < packages[right].PackageCode
	})
	bundle := policy.PolicyBundle{
		FormatVersion:  supportedFormatVersion,
		PolicyVersion:  source.PolicyVersion,
		State:          source.State,
		RetireAfter:    source.RetireAfter,
		Levels:         levels,
		WalletPackages: packages,
	}
	contents, err := marshalDocument(bundle)
	if err != nil {
		return policy.PolicyBundle{}, nil, err
	}
	return bundle, contents, nil
}

func compilePolicyPublication(
	source Source,
	binding EnvironmentBinding,
	levels []policy.Level,
	packages []policy.WalletPackage,
	bindings []policy.ApprovalBinding,
) (policyPublication, error) {
	publication := policyPublication{
		PolicyVersion:    source.Policy.PolicyVersion,
		State:            "staged",
		Levels:           make([]managedLevelDefinition, 0, len(levels)),
		WalletPackages:   make([]managedWalletPackageDefinition, 0, len(packages)),
		ApprovalBindings: make([]approvalPolicyBindingDefinition, 0, len(bindings)),
	}
	for _, level := range levels {
		publication.Levels = append(publication.Levels, managedLevelDefinition{
			LevelCode: level.LevelCode, Rank: level.Rank, PeriodQuota: level.PeriodQuota,
			ResetPeriod: level.ResetPeriod, ResetTimezone: level.ResetTimezone,
			PlanID:            binding.NewAPI.ManagedPlanIDs[level.LevelCode],
			ResetContractHash: binding.NewAPI.PlanResetContractHash,
		})
	}
	for _, walletPackage := range packages {
		publication.WalletPackages = append(publication.WalletPackages, managedWalletPackageDefinition{
			PackageCode: walletPackage.PackageCode, QuotaDelta: walletPackage.QuotaDelta,
		})
	}
	for _, approvalBinding := range bindings {
		publication.ApprovalBindings = append(publication.ApprovalBindings, approvalPolicyBindingDefinition{
			ApprovalCode: approvalBinding.ApprovalCode, SchemaFingerprint: approvalBinding.SchemaFingerprint,
			Locale: approvalBinding.Locale, ApprovalKind: string(approvalBinding.ApprovalKind),
			DefinitionManifestHash: strings.TrimPrefix(approvalBinding.SchemaFingerprint, "sha256:"),
		})
	}
	sort.Slice(publication.ApprovalBindings, func(left, right int) bool {
		leftBinding := publication.ApprovalBindings[left]
		rightBinding := publication.ApprovalBindings[right]
		if leftBinding.ApprovalCode != rightBinding.ApprovalCode {
			return leftBinding.ApprovalCode < rightBinding.ApprovalCode
		}
		if leftBinding.SchemaFingerprint != rightBinding.SchemaFingerprint {
			return leftBinding.SchemaFingerprint < rightBinding.SchemaFingerprint
		}
		return leftBinding.Locale < rightBinding.Locale
	})
	authority := struct {
		PolicyVersion    string                            `json:"policy_version"`
		Levels           []managedLevelDefinition          `json:"levels"`
		WalletPackages   []managedWalletPackageDefinition  `json:"wallet_packages"`
		ApprovalBindings []approvalPolicyBindingDefinition `json:"approval_bindings"`
	}{
		PolicyVersion: publication.PolicyVersion, Levels: publication.Levels,
		WalletPackages: publication.WalletPackages, ApprovalBindings: publication.ApprovalBindings,
	}
	encoded, err := json.Marshal(authority)
	if err != nil {
		return policyPublication{}, fmt.Errorf("encode New API catalog hash authority: %w", err)
	}
	publication.CatalogHash = sha256Hex(encoded)
	return publication, nil
}

func validHexDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validSecretRef(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') &&
			character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func marshalDocument(value any) ([]byte, error) {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(contents, '\n'), nil
}

func newArtifact(path string, contents []byte) Artifact {
	return Artifact{Path: path, SHA256: sha256Hex(contents), Contents: contents}
}

func digestArtifacts(artifacts []Artifact) string {
	hash := sha256.New()
	for _, artifact := range artifacts {
		hash.Write([]byte(artifact.Path))
		hash.Write([]byte{0})
		hash.Write([]byte(artifact.SHA256))
		hash.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func sha256Hex(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}
