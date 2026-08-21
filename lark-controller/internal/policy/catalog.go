package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	supportedFormatVersion          = 1
	supportedLocale                 = "zh-CN"
	walletEntitlementCustomID       = "wallet_package"
	subscriptionEntitlementCustomID = "target_level"
)

type ApprovalKind string

const (
	ApprovalKindWalletTopUp       ApprovalKind = "wallet_topup"
	ApprovalKindSubscriptionLevel ApprovalKind = "subscription_level"
)

type PolicyState string

const (
	PolicyStateActive   PolicyState = "active"
	PolicyStateDraining PolicyState = "draining"
	PolicyStateRetired  PolicyState = "retired"
)

type Level struct {
	LevelCode    string `json:"level_code"`
	Rank         int    `json:"rank"`
	MonthlyQuota int64  `json:"monthly_quota"`
}

type WalletPackage struct {
	PackageCode string `json:"package_code"`
	QuotaDelta  int64  `json:"quota_delta"`
}

type policyBundle struct {
	FormatVersion  int             `json:"format_version"`
	PolicyVersion  string          `json:"policy_version"`
	State          PolicyState     `json:"state"`
	RetireAfter    string          `json:"retire_after,omitempty"`
	Levels         []Level         `json:"levels"`
	WalletPackages []WalletPackage `json:"wallet_packages"`
}

type ManifestOption struct {
	DisplayText string `json:"display_text"`
	Code        string `json:"code"`
}

type ManifestField struct {
	CustomID string           `json:"custom_id"`
	Type     string           `json:"type"`
	Required bool             `json:"required"`
	Options  []ManifestOption `json:"options"`
}

type DefinitionManifest struct {
	ApprovalKind ApprovalKind    `json:"approval_kind"`
	Locale       string          `json:"locale"`
	Fields       []ManifestField `json:"fields"`
}

type approvalBinding struct {
	ApprovalCode                string             `json:"approval_code"`
	Locale                      string             `json:"locale"`
	PolicyVersion               string             `json:"policy_version"`
	ApprovalKind                ApprovalKind       `json:"approval_kind"`
	SchemaFingerprint           string             `json:"schema_fingerprint"`
	AcceptInstanceStartedBefore string             `json:"accept_instance_started_before,omitempty"`
	Manifest                    DefinitionManifest `json:"manifest"`
	acceptBefore                time.Time
	manifestJSON                string
}

type bindingsFile struct {
	FormatVersion int               `json:"format_version"`
	Bindings      []approvalBinding `json:"bindings"`
}

type loadedPolicy struct {
	bundle        policyBundle
	catalogSHA256 string
	sourceSHA256  string
	catalogJSON   string
	levels        map[string]Level
	packages      map[string]WalletPackage
}

type Catalog struct {
	policies map[string]loadedPolicy
	bindings map[string]approvalBinding
}

type ApprovalRequest struct {
	ApprovalCode string
	Locale       string
	StartTime    string
	FormJSON     string
}

type ApprovalResolution struct {
	PolicyVersion     string
	ApprovalKind      ApprovalKind
	BusinessCode      string
	QuotaDelta        int64
	MonthlyQuota      int64
	LevelRank         int
	SchemaFingerprint string
	CatalogSHA256     string
}

type BaseSubscriptionResolution struct {
	PolicyVersion string
	LevelCode     string
	LevelRank     int
	MonthlyQuota  int64
	CatalogSHA256 string
}

type Snapshot struct {
	Policies []PolicySnapshot
	Bindings []ApprovalBindingSnapshot
}

type PolicySnapshot struct {
	PolicyVersion string
	State         PolicyState
	RetireAfter   string
	CatalogSHA256 string
	SourceSHA256  string
	CatalogJSON   string
}

type ApprovalBindingSnapshot struct {
	ApprovalCode                string
	SchemaFingerprint           string
	Locale                      string
	PolicyVersion               string
	ApprovalKind                ApprovalKind
	DefinitionManifestSHA256    string
	DefinitionManifestJSON      string
	AcceptInstanceStartedBefore string
}

type formControl struct {
	ID       string `json:"id"`
	CustomID string `json:"custom_id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Value    string `json:"value"`
}

func LoadDirectory(policyDirectory, bindingsPath string) (*Catalog, error) {
	if policyDirectory == "" || bindingsPath == "" {
		return nil, errors.New("policy directory and approval bindings file are required")
	}
	entries, err := os.ReadDir(policyDirectory)
	if err != nil {
		return nil, fmt.Errorf("read policy directory: %w", err)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	catalog := &Catalog{
		policies: make(map[string]loadedPolicy),
		bindings: make(map[string]approvalBinding),
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".policy.json") {
			continue
		}
		path := filepath.Join(policyDirectory, entry.Name())
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read policy bundle %q: %w", entry.Name(), err)
		}
		var bundle policyBundle
		if err := decodeStrict(contents, &bundle); err != nil {
			return nil, fmt.Errorf("decode policy bundle %q: %w", entry.Name(), err)
		}
		loaded, err := validatePolicyBundle(bundle)
		if err != nil {
			return nil, fmt.Errorf("validate policy bundle %q: %w", entry.Name(), err)
		}
		if _, exists := catalog.policies[bundle.PolicyVersion]; exists {
			return nil, fmt.Errorf("duplicate policy version %q", bundle.PolicyVersion)
		}
		loaded.sourceSHA256 = sha256Hex(contents)
		catalog.policies[bundle.PolicyVersion] = loaded
	}
	if len(catalog.policies) == 0 {
		return nil, errors.New("policy directory contains no *.policy.json bundles")
	}
	activePolicies := 0
	for _, loaded := range catalog.policies {
		if loaded.bundle.State == PolicyStateActive {
			activePolicies++
		}
	}
	if activePolicies != 1 {
		return nil, fmt.Errorf("policy catalog must contain exactly one active version, got %d", activePolicies)
	}
	if err := catalog.validateCrossVersionRanks(); err != nil {
		return nil, err
	}

	contents, err := os.ReadFile(bindingsPath)
	if err != nil {
		return nil, fmt.Errorf("read approval bindings: %w", err)
	}
	var definitions bindingsFile
	if err := decodeStrict(contents, &definitions); err != nil {
		return nil, fmt.Errorf("decode approval bindings: %w", err)
	}
	if definitions.FormatVersion != supportedFormatVersion || len(definitions.Bindings) == 0 {
		return nil, errors.New("approval bindings require format_version 1 and at least one binding")
	}
	for _, binding := range definitions.Bindings {
		if err := catalog.addBinding(binding); err != nil {
			return nil, err
		}
	}
	if err := catalog.validateApprovalDefinitions(); err != nil {
		return nil, err
	}
	return catalog, nil
}

func (c *Catalog) validateCrossVersionRanks() error {
	versions := make([]string, 0, len(c.policies))
	for version := range c.policies {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	ranks := make(map[string]int)
	for _, version := range versions {
		for levelCode, level := range c.policies[version].levels {
			if previous, exists := ranks[levelCode]; exists && previous != level.Rank {
				return fmt.Errorf(
					"level %q changed rank across policy versions from %d to %d",
					levelCode,
					previous,
					level.Rank,
				)
			}
			ranks[levelCode] = level.Rank
		}
	}
	return nil
}

func validatePolicyBundle(bundle policyBundle) (loadedPolicy, error) {
	if bundle.FormatVersion != supportedFormatVersion || bundle.PolicyVersion == "" {
		return loadedPolicy{}, errors.New("format_version 1 and policy_version are required")
	}
	if bundle.State != PolicyStateActive && bundle.State != PolicyStateDraining && bundle.State != PolicyStateRetired {
		return loadedPolicy{}, fmt.Errorf("unsupported policy state %q", bundle.State)
	}
	if bundle.State == PolicyStateRetired {
		retireAfter, err := time.Parse(time.RFC3339, bundle.RetireAfter)
		if err != nil {
			return loadedPolicy{}, errors.New("retired policy requires an RFC3339 retire_after")
		}
		if retireAfter.After(time.Now()) {
			return loadedPolicy{}, errors.New("retired policy retire_after is still in the future")
		}
	} else if bundle.RetireAfter != "" {
		return loadedPolicy{}, errors.New("only retired policy may define retire_after")
	}
	loaded := loadedPolicy{
		bundle: bundle,
		levels: make(map[string]Level), packages: make(map[string]WalletPackage),
	}
	if len(bundle.Levels) == 0 || len(bundle.WalletPackages) == 0 {
		return loadedPolicy{}, errors.New("policy requires levels and wallet packages")
	}
	seenRanks := make(map[int]struct{}, len(bundle.Levels))
	for _, level := range bundle.Levels {
		if level.LevelCode == "" || level.Rank <= 0 || level.MonthlyQuota <= 0 {
			return loadedPolicy{}, errors.New("level code, positive rank, and positive monthly quota are required")
		}
		if _, exists := loaded.levels[level.LevelCode]; exists {
			return loadedPolicy{}, fmt.Errorf("duplicate level code %q", level.LevelCode)
		}
		if _, exists := seenRanks[level.Rank]; exists {
			return loadedPolicy{}, fmt.Errorf("duplicate level rank %d", level.Rank)
		}
		loaded.levels[level.LevelCode] = level
		seenRanks[level.Rank] = struct{}{}
	}
	for _, walletPackage := range bundle.WalletPackages {
		if walletPackage.PackageCode == "" || walletPackage.QuotaDelta <= 0 {
			return loadedPolicy{}, errors.New("package code and positive quota delta are required")
		}
		if _, exists := loaded.packages[walletPackage.PackageCode]; exists {
			return loadedPolicy{}, fmt.Errorf("duplicate package code %q", walletPackage.PackageCode)
		}
		loaded.packages[walletPackage.PackageCode] = walletPackage
	}
	levels := append([]Level(nil), bundle.Levels...)
	packages := append([]WalletPackage(nil), bundle.WalletPackages...)
	sort.Slice(levels, func(left, right int) bool { return levels[left].LevelCode < levels[right].LevelCode })
	sort.Slice(packages, func(left, right int) bool {
		return packages[left].PackageCode < packages[right].PackageCode
	})
	canonical, err := json.Marshal(struct {
		FormatVersion  int             `json:"format_version"`
		PolicyVersion  string          `json:"policy_version"`
		Levels         []Level         `json:"levels"`
		WalletPackages []WalletPackage `json:"wallet_packages"`
	}{
		FormatVersion: supportedFormatVersion, PolicyVersion: bundle.PolicyVersion,
		Levels: levels, WalletPackages: packages,
	})
	if err != nil {
		return loadedPolicy{}, fmt.Errorf("encode canonical policy catalog: %w", err)
	}
	loaded.catalogSHA256 = sha256Hex(canonical)
	loaded.catalogJSON = string(canonical)
	return loaded, nil
}

func (c *Catalog) addBinding(binding approvalBinding) error {
	if binding.ApprovalCode == "" || binding.Locale == "" || binding.PolicyVersion == "" ||
		binding.ApprovalKind == "" || binding.SchemaFingerprint == "" {
		return errors.New("approval binding identifiers are required")
	}
	policy, exists := c.policies[binding.PolicyVersion]
	if !exists {
		return fmt.Errorf("approval %q references unknown policy %q", binding.ApprovalCode, binding.PolicyVersion)
	}
	if binding.Manifest.ApprovalKind != binding.ApprovalKind || binding.Manifest.Locale != binding.Locale {
		return fmt.Errorf("approval %q manifest identity does not match binding", binding.ApprovalCode)
	}
	if binding.Locale != supportedLocale {
		return fmt.Errorf("approval %q locale must be %q", binding.ApprovalCode, supportedLocale)
	}
	canonical, err := canonicalManifest(binding.Manifest)
	if err != nil {
		return fmt.Errorf("approval %q manifest: %w", binding.ApprovalCode, err)
	}
	if fingerprint := "sha256:" + sha256Hex(canonical); fingerprint != binding.SchemaFingerprint {
		return fmt.Errorf("approval %q schema fingerprint mismatch", binding.ApprovalCode)
	}
	binding.manifestJSON = string(canonical)
	if err := validateManifestCatalog(binding, policy); err != nil {
		return fmt.Errorf("approval %q manifest: %w", binding.ApprovalCode, err)
	}
	if binding.AcceptInstanceStartedBefore != "" {
		cutoff, err := time.Parse(time.RFC3339, binding.AcceptInstanceStartedBefore)
		if err != nil {
			return fmt.Errorf("approval %q has invalid accept_instance_started_before: %w", binding.ApprovalCode, err)
		}
		binding.acceptBefore = cutoff
	}
	if policy.bundle.State == PolicyStateActive && !binding.acceptBefore.IsZero() {
		return fmt.Errorf("active approval %q cannot have a closed acceptance window", binding.ApprovalCode)
	}
	if policy.bundle.State != PolicyStateActive && binding.acceptBefore.IsZero() {
		return fmt.Errorf("historical approval %q requires an acceptance cutoff", binding.ApprovalCode)
	}
	if policy.bundle.State == PolicyStateRetired {
		retireAfter, _ := time.Parse(time.RFC3339, policy.bundle.RetireAfter)
		if !retireAfter.After(binding.acceptBefore) {
			return fmt.Errorf("retired approval %q cutoff must be before retire_after", binding.ApprovalCode)
		}
	}
	key := bindingKey(binding.ApprovalCode, binding.Locale)
	if _, exists := c.bindings[key]; exists {
		return fmt.Errorf("duplicate approval binding for %q and locale %q", binding.ApprovalCode, binding.Locale)
	}
	c.bindings[key] = binding
	return nil
}

func (c *Catalog) validateApprovalDefinitions() error {
	counts := make(map[string]map[ApprovalKind]int, len(c.policies))
	versions := make([]string, 0, len(c.policies))
	for version := range c.policies {
		versions = append(versions, version)
		counts[version] = map[ApprovalKind]int{
			ApprovalKindWalletTopUp:       0,
			ApprovalKindSubscriptionLevel: 0,
		}
	}
	for _, binding := range c.bindings {
		counts[binding.PolicyVersion][binding.ApprovalKind]++
	}
	sort.Strings(versions)
	for _, version := range versions {
		kinds := counts[version]
		for _, kind := range []ApprovalKind{ApprovalKindWalletTopUp, ApprovalKindSubscriptionLevel} {
			if kinds[kind] != 1 {
				return fmt.Errorf(
					"policy %q requires exactly one %s approval definition in locale %q, got %d",
					version,
					kind,
					supportedLocale,
					kinds[kind],
				)
			}
		}
	}
	return nil
}

func (c *Catalog) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	versions := make([]string, 0, len(c.policies))
	for version := range c.policies {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	snapshot := Snapshot{Policies: make([]PolicySnapshot, 0, len(versions))}
	for _, version := range versions {
		loaded := c.policies[version]
		snapshot.Policies = append(snapshot.Policies, PolicySnapshot{
			PolicyVersion: version,
			State:         loaded.bundle.State,
			RetireAfter:   loaded.bundle.RetireAfter,
			CatalogSHA256: loaded.catalogSHA256,
			SourceSHA256:  loaded.sourceSHA256,
			CatalogJSON:   loaded.catalogJSON,
		})
	}
	keys := make([]string, 0, len(c.bindings))
	for key := range c.bindings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	snapshot.Bindings = make([]ApprovalBindingSnapshot, 0, len(keys))
	for _, key := range keys {
		binding := c.bindings[key]
		snapshot.Bindings = append(snapshot.Bindings, ApprovalBindingSnapshot{
			ApprovalCode: binding.ApprovalCode, SchemaFingerprint: binding.SchemaFingerprint,
			Locale: binding.Locale, PolicyVersion: binding.PolicyVersion,
			ApprovalKind:                binding.ApprovalKind,
			DefinitionManifestSHA256:    strings.TrimPrefix(binding.SchemaFingerprint, "sha256:"),
			DefinitionManifestJSON:      binding.manifestJSON,
			AcceptInstanceStartedBefore: binding.AcceptInstanceStartedBefore,
		})
	}
	return snapshot
}

func (c *Catalog) ActivePolicyVersion() string {
	if c == nil {
		return ""
	}
	for version, loaded := range c.policies {
		if loaded.bundle.State == PolicyStateActive {
			return version
		}
	}
	return ""
}

func (c *Catalog) ResolveBaseSubscription() (BaseSubscriptionResolution, error) {
	if c == nil {
		return BaseSubscriptionResolution{}, errors.New("policy catalog is required")
	}
	policyVersion := c.ActivePolicyVersion()
	loaded, exists := c.policies[policyVersion]
	if !exists {
		return BaseSubscriptionResolution{}, errors.New("active policy is required")
	}
	level, exists := loaded.levels["basic"]
	if !exists {
		return BaseSubscriptionResolution{}, fmt.Errorf(
			"active policy %q does not define the basic subscription level",
			policyVersion,
		)
	}
	return BaseSubscriptionResolution{
		PolicyVersion: policyVersion, LevelCode: level.LevelCode,
		LevelRank: level.Rank, MonthlyQuota: level.MonthlyQuota,
		CatalogSHA256: loaded.catalogSHA256,
	}, nil
}

func canonicalManifest(manifest DefinitionManifest) ([]byte, error) {
	if manifest.ApprovalKind != ApprovalKindWalletTopUp && manifest.ApprovalKind != ApprovalKindSubscriptionLevel {
		return nil, fmt.Errorf("unsupported approval kind %q", manifest.ApprovalKind)
	}
	if manifest.Locale == "" || len(manifest.Fields) == 0 {
		return nil, errors.New("locale and fields are required")
	}
	previousCustomID := ""
	for _, field := range manifest.Fields {
		if field.CustomID == "" || field.Type == "" || field.CustomID <= previousCustomID {
			return nil, errors.New("manifest fields must have unique custom_id values sorted ascending")
		}
		previousCustomID = field.CustomID
		if field.Options == nil {
			return nil, fmt.Errorf("field %q options must be an explicit array", field.CustomID)
		}
	}
	return json.Marshal(manifest)
}

func validateManifestCatalog(binding approvalBinding, policy loadedPolicy) error {
	entitlementCustomID := entitlementFieldCustomID(binding.ApprovalKind)
	entitlementFields := 0
	for _, field := range binding.Manifest.Fields {
		if field.Type != "radioV2" {
			if len(field.Options) != 0 {
				return fmt.Errorf("non-radio field %q cannot define options", field.CustomID)
			}
			continue
		}
		if len(field.Options) == 0 {
			return fmt.Errorf("radio field %q must define options", field.CustomID)
		}
		isEntitlementField := field.CustomID == entitlementCustomID
		if isEntitlementField {
			entitlementFields++
			if !field.Required {
				return fmt.Errorf("entitlement field %q must be required", field.CustomID)
			}
		}
		seenDisplayText := make(map[string]struct{})
		seenCodes := make(map[string]struct{})
		for _, option := range field.Options {
			trimmed := strings.TrimSpace(option.DisplayText)
			if trimmed == "" || option.Code == "" {
				return fmt.Errorf("radio field %q has an empty display text or code", field.CustomID)
			}
			if _, duplicate := seenDisplayText[trimmed]; duplicate {
				return fmt.Errorf("radio field %q has display text duplicated after trim", field.CustomID)
			}
			if _, duplicate := seenCodes[option.Code]; duplicate {
				return fmt.Errorf("radio field %q has duplicate business code %q", field.CustomID, option.Code)
			}
			seenDisplayText[trimmed] = struct{}{}
			seenCodes[option.Code] = struct{}{}
			if !isEntitlementField {
				continue
			}
			switch binding.ApprovalKind {
			case ApprovalKindWalletTopUp:
				if _, exists := policy.packages[option.Code]; !exists {
					return fmt.Errorf("unknown wallet package %q", option.Code)
				}
			case ApprovalKindSubscriptionLevel:
				if _, exists := policy.levels[option.Code]; !exists {
					return fmt.Errorf("unknown subscription level %q", option.Code)
				}
			}
		}
	}
	if entitlementFields != 1 {
		return fmt.Errorf("manifest must define exactly one entitlement field %q", entitlementCustomID)
	}
	return nil
}

func (c *Catalog) ResolveApproval(request ApprovalRequest) (ApprovalResolution, error) {
	if c == nil || request.ApprovalCode == "" || request.Locale == "" || request.StartTime == "" || request.FormJSON == "" {
		return ApprovalResolution{}, errors.New("approval code, locale, start time, and form are required")
	}
	binding, exists := c.bindings[bindingKey(request.ApprovalCode, request.Locale)]
	if !exists {
		return ApprovalResolution{}, errors.New("approval binding not found")
	}
	policy := c.policies[binding.PolicyVersion]
	if policy.bundle.State == PolicyStateRetired {
		return ApprovalResolution{}, errors.New("retired policy does not accept approval instances")
	}
	startMilliseconds, err := strconv.ParseInt(request.StartTime, 10, 64)
	if err != nil || startMilliseconds <= 0 {
		return ApprovalResolution{}, errors.New("approval instance start time must be a positive millisecond timestamp")
	}
	if !binding.acceptBefore.IsZero() && !time.UnixMilli(startMilliseconds).Before(binding.acceptBefore) {
		return ApprovalResolution{}, errors.New("approval instance started outside the binding acceptance window")
	}
	var controls []formControl
	if err := decodeStrict([]byte(request.FormJSON), &controls); err != nil {
		return ApprovalResolution{}, fmt.Errorf("decode approval form: %w", err)
	}
	if len(controls) == 0 {
		return ApprovalResolution{}, errors.New("approval form is empty")
	}
	controlByID := make(map[string]formControl, len(controls))
	for _, control := range controls {
		if control.CustomID == "" {
			return ApprovalResolution{}, errors.New("approval form control is missing custom_id")
		}
		if _, duplicate := controlByID[control.CustomID]; duplicate {
			return ApprovalResolution{}, fmt.Errorf("duplicate approval form custom_id %q", control.CustomID)
		}
		controlByID[control.CustomID] = control
	}
	if len(controlByID) != len(binding.Manifest.Fields) {
		return ApprovalResolution{}, errors.New("approval form field count does not match manifest")
	}
	resolution := ApprovalResolution{
		PolicyVersion: binding.PolicyVersion, ApprovalKind: binding.ApprovalKind,
		SchemaFingerprint: binding.SchemaFingerprint, CatalogSHA256: policy.catalogSHA256,
	}
	entitlementCustomID := entitlementFieldCustomID(binding.ApprovalKind)
	for index, field := range binding.Manifest.Fields {
		control, exists := controlByID[field.CustomID]
		if !exists || controls[index].CustomID != field.CustomID || control.Type != field.Type {
			return ApprovalResolution{}, fmt.Errorf("approval form field %q does not match manifest", field.CustomID)
		}
		if field.Required && control.Value == "" {
			return ApprovalResolution{}, fmt.Errorf("required approval form field %q is empty", field.CustomID)
		}
		if field.Type != "radioV2" {
			continue
		}
		if !field.Required && control.Value == "" {
			continue
		}
		matched := false
		for _, option := range field.Options {
			if control.Value == option.DisplayText {
				matched = true
				if field.CustomID == entitlementCustomID {
					resolution.BusinessCode = option.Code
				}
				break
			}
		}
		if !matched {
			return ApprovalResolution{}, fmt.Errorf("approval form field %q has unknown display text", field.CustomID)
		}
	}
	if resolution.BusinessCode == "" {
		return ApprovalResolution{}, errors.New("approval form did not resolve an entitlement code")
	}
	switch binding.ApprovalKind {
	case ApprovalKindWalletTopUp:
		resolution.QuotaDelta = policy.packages[resolution.BusinessCode].QuotaDelta
	case ApprovalKindSubscriptionLevel:
		level := policy.levels[resolution.BusinessCode]
		resolution.MonthlyQuota = level.MonthlyQuota
		resolution.LevelRank = level.Rank
	}
	return resolution, nil
}

func decodeStrict(contents []byte, target any) error {
	if err := rejectDuplicateJSONKeys(contents); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after JSON document")
	}
	return nil
}

func rejectDuplicateJSONKeys(contents []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("unexpected data after JSON document")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, duplicate := keys[key]; duplicate {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			keys[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("JSON object has an invalid closing delimiter")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("JSON array has an invalid closing delimiter")
		}
	default:
		return errors.New("JSON value starts with an invalid closing delimiter")
	}
	return nil
}

func entitlementFieldCustomID(kind ApprovalKind) string {
	switch kind {
	case ApprovalKindWalletTopUp:
		return walletEntitlementCustomID
	case ApprovalKindSubscriptionLevel:
		return subscriptionEntitlementCustomID
	default:
		return ""
	}
}

func bindingKey(approvalCode, locale string) string {
	return approvalCode + "\x00" + locale
}

func sha256Hex(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}
