package configcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/strictjson"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

type larkCommandRunner interface {
	Run(context.Context, string, []string, []byte) ([]byte, []byte, error)
}

type execLarkCommandRunner struct{}

func (execLarkCommandRunner) Run(
	ctx context.Context,
	executable string,
	arguments []string,
	stdin []byte,
) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Env = append(os.Environ(),
		"LARKSUITE_CLI_NO_UPDATE_NOTIFIER=1",
		"LARKSUITE_CLI_NO_SKILLS_NOTIFIER=1",
	)
	command.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type larkConsoleAttestation struct {
	FormatVersion         int      `json:"format_version"`
	AppID                 string   `json:"app_id"`
	TenantKey             string   `json:"tenant_key"`
	RedirectURLs          []string `json:"redirect_urls"`
	ConsoleEvents         []string `json:"console_events"`
	ApprovalSubscriptions []string `json:"approval_subscriptions"`
	ReviewedBy            string   `json:"reviewed_by"`
	ReviewedAt            string   `json:"reviewed_at"`
	Evidence              string   `json:"evidence"`
}

type larkCLIClient struct {
	executable  string
	attestation *larkConsoleAttestation
	runner      larkCommandRunner
}

func newLarkCLIClient(
	executable string,
	attestationPath string,
	runner larkCommandRunner,
) (*larkCLIClient, error) {
	if executable == "" || strings.TrimSpace(executable) != executable || strings.ContainsRune(executable, 0) {
		return nil, errors.New("Lark CLI executable is invalid")
	}
	if runner == nil {
		return nil, errors.New("Lark command runner is required")
	}
	client := &larkCLIClient{executable: executable, runner: runner}
	if attestationPath == "" {
		return client, nil
	}
	contents, exists, err := readRegularFile(attestationPath)
	if err != nil || !exists {
		return nil, errors.New("Lark console attestation must be a readable regular non-symlink file")
	}
	var attestation larkConsoleAttestation
	if err := strictjson.Decode(contents, &attestation); err != nil {
		return nil, fmt.Errorf("decode Lark console attestation: %w", err)
	}
	if err := validateLarkConsoleAttestation(attestation); err != nil {
		return nil, err
	}
	client.attestation = &attestation
	return client, nil
}

func validateLarkConsoleAttestation(attestation larkConsoleAttestation) error {
	if attestation.FormatVersion != 1 || attestation.AppID == "" || attestation.TenantKey == "" ||
		attestation.ReviewedBy == "" || attestation.Evidence == "" {
		return errors.New("Lark console attestation requires format_version 1, identity, reviewer, and evidence")
	}
	if _, err := time.Parse(time.RFC3339, attestation.ReviewedAt); err != nil {
		return errors.New("Lark console attestation reviewed_at must be RFC3339")
	}
	if err := validateAttestedSet("redirect URL", attestation.RedirectURLs); err != nil {
		return err
	}
	for _, redirectURL := range attestation.RedirectURLs {
		parsed, err := url.Parse(redirectURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
			parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("Lark console attestation contains invalid redirect URL %q", redirectURL)
		}
	}
	if err := validateAttestedSet("event", attestation.ConsoleEvents); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(attestation.ApprovalSubscriptions))
	for _, approvalCode := range attestation.ApprovalSubscriptions {
		if approvalCode == "" || strings.TrimSpace(approvalCode) != approvalCode {
			return errors.New("Lark console attestation contains an invalid approval subscription")
		}
		if _, duplicate := seen[approvalCode]; duplicate {
			return fmt.Errorf("Lark console attestation contains duplicate approval subscription %q", approvalCode)
		}
		seen[approvalCode] = struct{}{}
	}
	return nil
}

func validateAttestedSet(name string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("Lark console attestation contains an invalid %s", name)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("Lark console attestation contains duplicate %s %q", name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

type larkCLIApprovalDefinition struct {
	ApprovalName string          `json:"approval_name"`
	Form         string          `json:"form"`
	NodeList     json.RawMessage `json:"node_list"`
}

type larkCLIEnvelope[T any] struct {
	OK       bool            `json:"ok"`
	Identity string          `json:"identity"`
	Data     T               `json:"data"`
	Meta     json.RawMessage `json:"meta,omitempty"`
	Notice   json.RawMessage `json:"_notice,omitempty"`
}

func (client *larkCLIClient) observe(
	ctx context.Context,
	compiled tenantconfig.CompiledBundle,
) (*tenantconfig.ObservedLark, error) {
	if client.attestation == nil {
		return nil, errors.New("Lark remote planning requires a console attestation")
	}
	artifact, err := compiled.Artifact("lark/tenant-preflight.json")
	if err != nil {
		return nil, err
	}
	var desired tenantconfig.LarkTenantPreflight
	if err := strictjson.Decode(artifact.Contents, &desired); err != nil {
		return nil, fmt.Errorf("decode compiled Lark preflight: %w", err)
	}
	appID, tenantKey, err := client.observeAuthenticatedIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if appID != client.attestation.AppID || tenantKey != client.attestation.TenantKey {
		return nil, errors.New("authenticated lark-cli app or tenant does not match the console attestation")
	}
	observed := &tenantconfig.ObservedLark{
		AppID: appID, TenantKey: tenantKey,
		ApprovalFingerprints:  make(map[string]string, len(desired.ApprovalDefinitions)),
		RedirectURLs:          make(map[string]bool, len(client.attestation.RedirectURLs)),
		ConsoleEvents:         make(map[string]bool, len(client.attestation.ConsoleEvents)),
		ApprovalSubscriptions: make(map[string]bool, len(client.attestation.ApprovalSubscriptions)),
	}
	for _, redirectURL := range client.attestation.RedirectURLs {
		observed.RedirectURLs[redirectURL] = true
	}
	for _, event := range client.attestation.ConsoleEvents {
		observed.ConsoleEvents[event] = true
	}
	for _, approvalCode := range client.attestation.ApprovalSubscriptions {
		observed.ApprovalSubscriptions[approvalCode] = true
	}
	for _, definition := range desired.ApprovalDefinitions {
		fingerprint, err := client.observeApprovalDefinition(ctx, definition)
		if err != nil {
			return nil, fmt.Errorf("observe Lark approval definition %q: %w", definition.ApprovalCode, err)
		}
		observed.ApprovalFingerprints[definition.ApprovalCode] = fingerprint
	}
	return observed, nil
}

func (client *larkCLIClient) observeAuthenticatedIdentity(ctx context.Context) (string, string, error) {
	stdout, _, err := client.runner.Run(ctx, client.executable, []string{
		"auth", "status", "--json", "--verify",
	}, nil)
	if err != nil {
		return "", "", errors.New("lark-cli authenticated identity verification failed")
	}
	var status map[string]json.RawMessage
	if err := strictjson.Decode(stdout, &status); err != nil {
		return "", "", errors.New("lark-cli authenticated identity response is invalid")
	}
	var appID string
	var identities map[string]json.RawMessage
	if !decodeLarkField(status, "appId", &appID) || appID == "" ||
		!decodeLarkField(status, "identities", &identities) {
		return "", "", errors.New("lark-cli authenticated identity response is incomplete")
	}
	for _, identity := range []string{"bot", "user"} {
		var identityState map[string]json.RawMessage
		var available bool
		var state string
		if !decodeLarkField(identities, identity, &identityState) ||
			!decodeLarkField(identityState, "available", &available) || !available ||
			!decodeLarkField(identityState, "status", &state) || state != "ready" {
			return "", "", fmt.Errorf("lark-cli %s identity is not verified and ready", identity)
		}
		if identity == "user" {
			var scopes string
			if !decodeLarkField(identityState, "scope", &scopes) ||
				!containsSpaceDelimitedValue(scopes, "approval:approval:read") {
				return "", "", errors.New("lark-cli user identity is missing approval:approval:read")
			}
		}
	}

	stdout, _, err = client.runner.Run(ctx, client.executable, []string{
		"api", "GET", "/open-apis/tenant/v2/tenant/query", "--as", "bot", "--json",
	}, nil)
	if err != nil {
		return "", "", errors.New("lark-cli tenant identity query failed")
	}
	data, err := decodeLarkCLIEnvelopeData(stdout, "bot")
	if err != nil {
		return "", "", errors.New("lark-cli tenant identity response is invalid or unsuccessful")
	}
	var dataFields map[string]json.RawMessage
	var tenantFields map[string]json.RawMessage
	var tenantKey string
	if strictjson.Decode(data, &dataFields) != nil ||
		!decodeLarkField(dataFields, "tenant", &tenantFields) ||
		!decodeLarkField(tenantFields, "tenant_key", &tenantKey) || tenantKey == "" {
		return "", "", errors.New("lark-cli tenant identity response is incomplete")
	}
	return appID, tenantKey, nil
}

func containsSpaceDelimitedValue(values, wanted string) bool {
	for _, value := range strings.Fields(values) {
		if value == wanted {
			return true
		}
	}
	return false
}

func decodeLarkCLIEnvelopeData(contents []byte, identity string) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := strictjson.Decode(contents, &envelope); err != nil {
		return nil, err
	}
	var ok bool
	var observedIdentity string
	var data json.RawMessage
	if !decodeLarkField(envelope, "ok", &ok) || !ok ||
		!decodeLarkField(envelope, "identity", &observedIdentity) || observedIdentity != identity ||
		!decodeLarkField(envelope, "data", &data) {
		return nil, errors.New("invalid Lark CLI response envelope")
	}
	return data, nil
}

func (client *larkCLIClient) observeApprovalDefinition(
	ctx context.Context,
	desired tenantconfig.LarkApprovalTarget,
) (string, error) {
	stdout, _, err := client.runner.Run(ctx, client.executable, []string{
		"approval", "approvals", "get",
		"--approval-code", desired.ApprovalCode,
		"--locale", desired.Locale,
		"--as", "user", "--json",
	}, nil)
	if err != nil {
		return "", errors.New("lark-cli approval definition query failed")
	}
	var envelope larkCLIEnvelope[larkCLIApprovalDefinition]
	if err := strictjson.Decode(stdout, &envelope); err != nil || !envelope.OK || envelope.Identity != "user" {
		return "", errors.New("lark-cli approval definition response is invalid or unsuccessful")
	}
	matches, err := larkDefinitionMatchesManifest(envelope.Data.Form, desired.Manifest)
	if err != nil {
		return "", err
	}
	if matches {
		return desired.SchemaFingerprint, nil
	}
	return "sha256:" + sha256Hex([]byte(envelope.Data.Form)), nil
}

func larkDefinitionMatchesManifest(form string, manifest policy.DefinitionManifest) (bool, error) {
	if form == "" {
		return false, errors.New("Lark approval definition form is empty")
	}
	var controls []map[string]json.RawMessage
	if err := strictjson.Decode([]byte(form), &controls); err != nil {
		return false, errors.New("Lark approval definition form is invalid")
	}
	if len(controls) != len(manifest.Fields) {
		return false, nil
	}
	for index, desired := range manifest.Fields {
		control := controls[index]
		var customID string
		var controlType string
		var required bool
		if !decodeLarkField(control, "custom_id", &customID) ||
			!decodeLarkField(control, "type", &controlType) ||
			!decodeLarkField(control, "required", &required) {
			return false, errors.New("Lark approval definition control is incomplete")
		}
		if customID != desired.CustomID || controlType != desired.Type || required != desired.Required {
			return false, nil
		}
		if controlType != "radioV2" {
			if len(desired.Options) != 0 {
				return false, nil
			}
			continue
		}
		optionContents, exists := control["option"]
		if !exists {
			optionContents, exists = control["value"]
		}
		if !exists {
			return false, nil
		}
		var liveOptions []map[string]json.RawMessage
		if err := strictjson.Decode(optionContents, &liveOptions); err != nil {
			return false, errors.New("Lark approval radio options are invalid")
		}
		if len(liveOptions) != len(desired.Options) {
			return false, nil
		}
		for optionIndex, liveOption := range liveOptions {
			var displayText string
			if !decodeLarkField(liveOption, "text", &displayText) {
				return false, errors.New("Lark approval radio option is missing display text")
			}
			if displayText != desired.Options[optionIndex].DisplayText {
				return false, nil
			}
		}
	}
	return true, nil
}

func decodeLarkField(fields map[string]json.RawMessage, name string, target any) bool {
	contents, exists := fields[name]
	return exists && strictjson.Decode(contents, target) == nil
}

func (client *larkCLIClient) Execute(
	ctx context.Context,
	change tenantconfig.Change,
) (tenantconfig.ExecutionResult, error) {
	if change.Target != tenantconfig.TargetLark || change.Action != tenantconfig.ActionSubscribeApproval {
		return tenantconfig.ExecutionResult{}, errors.New("unsupported Lark configuration operation")
	}
	var subscription tenantconfig.LarkApprovalSubscriptionMutation
	if err := strictjson.Decode(change.Payload, &subscription); err != nil {
		return tenantconfig.ExecutionResult{}, errors.New("invalid Lark approval subscription payload")
	}
	escapedCode := url.PathEscape(subscription.ApprovalCode)
	if subscription.ApprovalCode == "" || escapedCode != subscription.ApprovalCode ||
		change.Resource != subscription.ApprovalCode || subscription.AppID == "" || subscription.TenantKey == "" {
		return tenantconfig.ExecutionResult{}, errors.New("Lark approval subscription is not allowed")
	}
	appID, tenantKey, err := client.observeAuthenticatedIdentity(ctx)
	if err != nil {
		return tenantconfig.ExecutionResult{}, err
	}
	if appID != subscription.AppID || tenantKey != subscription.TenantKey {
		return tenantconfig.ExecutionResult{}, errors.New("authenticated lark-cli app or tenant does not match the reviewed plan")
	}
	endpoint := "/open-apis/approval/v4/approvals/" + escapedCode + "/subscribe"
	stdout, stderr, err := client.runner.Run(ctx, client.executable, []string{
		"api", "POST", endpoint, "--as", "bot", "--json",
	}, nil)
	if err != nil {
		if isExistingApprovalSubscription(stderr) {
			return tenantconfig.ExecutionResult{ResultDigest: change.DesiredDigest, Replayed: true}, nil
		}
		return tenantconfig.ExecutionResult{}, errors.New("lark-cli approval subscription failed")
	}
	var envelope larkCLIEnvelope[json.RawMessage]
	if err := strictjson.Decode(stdout, &envelope); err != nil || !envelope.OK || envelope.Identity != "bot" {
		return tenantconfig.ExecutionResult{}, errors.New("lark-cli approval subscription response is invalid or unsuccessful")
	}
	return tenantconfig.ExecutionResult{ResultDigest: change.DesiredDigest}, nil
}

func isExistingApprovalSubscription(contents []byte) bool {
	var envelope map[string]json.RawMessage
	if strictjson.Decode(contents, &envelope) != nil {
		return false
	}
	var ok bool
	var identity string
	var errorFields map[string]json.RawMessage
	if strictjson.Decode(envelope["ok"], &ok) != nil || ok ||
		strictjson.Decode(envelope["identity"], &identity) != nil || identity != "bot" ||
		strictjson.Decode(envelope["error"], &errorFields) != nil {
		return false
	}
	var code int
	return strictjson.Decode(errorFields["code"], &code) == nil && code == 1390007
}
