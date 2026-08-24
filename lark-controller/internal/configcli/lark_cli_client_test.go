package configcli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

type recordedLarkCommand struct {
	executable string
	arguments  []string
	stdin      []byte
}

type fakeLarkCommandRunner struct {
	commands       []recordedLarkCommand
	responses      map[string][]byte
	standardErrors map[string][]byte
	errors         map[string]error
}

func (runner *fakeLarkCommandRunner) Run(
	_ context.Context,
	executable string,
	arguments []string,
	stdin []byte,
) ([]byte, []byte, error) {
	runner.commands = append(runner.commands, recordedLarkCommand{
		executable: executable, arguments: append([]string(nil), arguments...), stdin: append([]byte(nil), stdin...),
	})
	key := ""
	if len(arguments) >= 2 && arguments[0] == "auth" {
		key = "auth-status"
	} else if len(arguments) >= 3 && arguments[0] == "api" && arguments[1] == "GET" {
		key = arguments[2]
	} else if len(arguments) >= 5 && arguments[0] == "approval" {
		key = arguments[4]
	} else if len(arguments) >= 3 && arguments[0] == "api" {
		key = arguments[2]
	}
	response, exists := runner.responses[key]
	if runErr := runner.errors[key]; runErr != nil {
		return append([]byte(nil), response...), append([]byte(nil), runner.standardErrors[key]...), runErr
	}
	if !exists {
		return nil, []byte(`{"ok":false,"error":{"type":"test","message":"missing fixture"}}`), errors.New("missing fixture")
	}
	return append([]byte(nil), response...), nil, nil
}

func TestLarkCLIClientObservesApprovalDefinitionsWithoutWriting(t *testing.T) {
	source, binding := testConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}
	attestationPath := writeLarkAttestation(t, binding, []string{
		"approval.instance.status_changed_v4",
		"contact.user.deleted_v3",
	}, []string{"approval-wallet-v1"})
	runner := &fakeLarkCommandRunner{responses: map[string][]byte{
		"auth-status":                       authenticatedIdentityStatus(t, binding.Lark.AppID),
		"/open-apis/tenant/v2/tenant/query": tenantIdentityEnvelope(t, binding.Lark.TenantKey),
		"approval-wallet-v1": approvalDefinitionEnvelope(t, `[
			{"custom_id":"cost_center","type":"textarea","required":true},
			{"custom_id":"estimated_usage","type":"textarea","required":false},
			{"custom_id":"request_reason","type":"textarea","required":true},
			{"custom_id":"wallet_package","type":"radioV2","required":true,"option":[{"value":"opaque-1","text":"Small"}]}
		]`),
		"approval-level-v1": approvalDefinitionEnvelope(t, `[
			{"custom_id":"cost_center","type":"textarea","required":true},
			{"custom_id":"estimated_usage","type":"textarea","required":true},
			{"custom_id":"request_reason","type":"textarea","required":true},
			{"custom_id":"target_level","type":"radioV2","required":true,"option":[{"value":"opaque-2","text":"Plus"}]}
		]`),
	}}
	client, err := newLarkCLIClient("lark-cli", attestationPath, runner)
	if err != nil {
		t.Fatalf("construct Lark CLI client: %v", err)
	}
	observed, err := client.observe(context.Background(), compiled)
	if err != nil {
		t.Fatalf("observe Lark tenant: %v", err)
	}
	if observed.AppID != binding.Lark.AppID || observed.TenantKey != binding.Lark.TenantKey {
		t.Fatalf("observed identity = %q/%q", observed.AppID, observed.TenantKey)
	}
	if observed.ApprovalFingerprints["approval-wallet-v1"] != "sha256:2e40e401ef32a26a267724b6068dd56a4a4099cb96cca6406d38417e76d899ec" ||
		observed.ApprovalFingerprints["approval-level-v1"] != "sha256:ad5cf5c9d5cf3eb5d789046a28b98d6cd07122099e6f25f404bd64606b560356" {
		t.Fatalf("unexpected approval fingerprints: %+v", observed.ApprovalFingerprints)
	}
	if !observed.ApprovalSubscriptions["approval-wallet-v1"] || observed.ApprovalSubscriptions["approval-level-v1"] {
		t.Fatalf("unexpected approval subscriptions: %+v", observed.ApprovalSubscriptions)
	}
	if len(runner.commands) != 4 {
		t.Fatalf("Lark observation commands = %d, want 4", len(runner.commands))
	}
	if !slices.Equal(runner.commands[0].arguments, []string{"auth", "status", "--json", "--verify"}) ||
		!slices.Equal(runner.commands[1].arguments, []string{
			"api", "GET", "/open-apis/tenant/v2/tenant/query", "--as", "bot", "--json",
		}) {
		t.Fatalf("unexpected Lark identity commands: %+v", runner.commands[:2])
	}
	for _, command := range runner.commands {
		if command.executable != "lark-cli" || len(command.stdin) != 0 {
			t.Fatalf("unexpected Lark command boundary: %+v", command)
		}
	}
}

func TestLarkCLIClientRejectsAuthenticatedIdentityMismatch(t *testing.T) {
	source, binding := testConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}
	attestationPath := writeLarkAttestation(t, binding, []string{
		"approval.instance.status_changed_v4",
		"contact.user.deleted_v3",
	}, nil)
	tests := []struct {
		name      string
		appID     string
		tenantKey string
	}{
		{name: "different app", appID: "cli_different", tenantKey: binding.Lark.TenantKey},
		{name: "different tenant", appID: binding.Lark.AppID, tenantKey: "different-tenant"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeLarkCommandRunner{responses: map[string][]byte{
				"auth-status":                       authenticatedIdentityStatus(t, test.appID),
				"/open-apis/tenant/v2/tenant/query": tenantIdentityEnvelope(t, test.tenantKey),
			}}
			client, err := newLarkCLIClient("lark-cli", attestationPath, runner)
			if err != nil {
				t.Fatalf("construct Lark CLI client: %v", err)
			}
			if _, err := client.observe(context.Background(), compiled); err == nil {
				t.Fatal("authenticated identity mismatch was accepted")
			}
		})
	}
}

func TestLarkCLIClientTreatsExistingApprovalSubscriptionAsReplay(t *testing.T) {
	endpoint := "/open-apis/approval/v4/approvals/approval-wallet-v1/subscribe"
	_, binding := testConfiguration()
	runner := &fakeLarkCommandRunner{
		responses: map[string][]byte{
			"auth-status":                       authenticatedIdentityStatus(t, binding.Lark.AppID),
			"/open-apis/tenant/v2/tenant/query": tenantIdentityEnvelope(t, binding.Lark.TenantKey),
		},
		standardErrors: map[string][]byte{
			endpoint: []byte(`{"ok":false,"identity":"bot","error":{"type":"upstream","code":1390007,"message":"subscription existed"}}`),
		},
		errors: map[string]error{endpoint: errors.New("exit status 1")},
	}
	client, err := newLarkCLIClient("lark-cli", "", runner)
	if err != nil {
		t.Fatalf("construct Lark CLI client: %v", err)
	}
	result, err := client.Execute(context.Background(), tenantconfig.Change{
		Target: "lark", Action: "subscribe-approval", Resource: "approval-wallet-v1",
		DesiredDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Payload: []byte(`{"approval_code":"approval-wallet-v1","app_id":"` + binding.Lark.AppID +
			`","tenant_key":"` + binding.Lark.TenantKey + `"}`),
	})
	if err != nil {
		t.Fatalf("replay existing approval subscription: %v", err)
	}
	if !result.Replayed || result.ResultDigest == "" {
		t.Fatalf("existing approval subscription result = %+v, want replay", result)
	}
}

func TestLarkCLIClientAppliesOnlyExactApprovalCodeSubscriptions(t *testing.T) {
	_, binding := testConfiguration()
	runner := &fakeLarkCommandRunner{responses: map[string][]byte{
		"auth-status":                       authenticatedIdentityStatus(t, binding.Lark.AppID),
		"/open-apis/tenant/v2/tenant/query": tenantIdentityEnvelope(t, binding.Lark.TenantKey),
		"/open-apis/approval/v4/approvals/approval-wallet-v1/subscribe": []byte(`{"ok":true,"identity":"bot","data":{}}`),
	}}
	client, err := newLarkCLIClient("lark-cli", "", runner)
	if err != nil {
		t.Fatalf("construct Lark CLI client: %v", err)
	}
	payload := []byte(`{"approval_code":"approval-wallet-v1","app_id":"` + binding.Lark.AppID +
		`","tenant_key":"` + binding.Lark.TenantKey + `"}`)
	result, err := client.Execute(context.Background(), tenantconfig.Change{
		Target: "lark", Action: "subscribe-approval", Resource: "approval-wallet-v1",
		DesiredDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Payload: payload,
	})
	if err != nil {
		t.Fatalf("apply Lark approval relation: %v", err)
	}
	if result.ResultDigest == "" || len(runner.commands) != 3 {
		t.Fatalf("unexpected Lark apply result=%+v commands=%+v", result, runner.commands)
	}
	wantArguments := []string{
		"api", "POST", "/open-apis/approval/v4/approvals/approval-wallet-v1/subscribe",
		"--as", "bot", "--json",
	}
	if !slices.Equal(runner.commands[2].arguments, wantArguments) || len(runner.commands[2].stdin) != 0 {
		t.Fatalf("Lark apply command = %+v", runner.commands[2])
	}
}

func TestLarkCLIClientRejectsApplyAfterProfileChanges(t *testing.T) {
	_, binding := testConfiguration()
	runner := &fakeLarkCommandRunner{responses: map[string][]byte{
		"auth-status":                       authenticatedIdentityStatus(t, "cli_different"),
		"/open-apis/tenant/v2/tenant/query": tenantIdentityEnvelope(t, binding.Lark.TenantKey),
	}}
	client, err := newLarkCLIClient("lark-cli", "", runner)
	if err != nil {
		t.Fatalf("construct Lark CLI client: %v", err)
	}
	payload := []byte(`{"approval_code":"approval-wallet-v1","app_id":"` + binding.Lark.AppID +
		`","tenant_key":"` + binding.Lark.TenantKey + `"}`)
	_, err = client.Execute(context.Background(), tenantconfig.Change{
		Target: "lark", Action: "subscribe-approval", Resource: "approval-wallet-v1",
		DesiredDigest: strings.Repeat("a", 64), Payload: payload,
	})
	if err == nil {
		t.Fatal("changed lark-cli profile was accepted during apply")
	}
	for _, command := range runner.commands {
		if len(command.arguments) > 0 && command.arguments[0] == "api" && slices.Contains(command.arguments, "POST") {
			t.Fatalf("apply mutated Lark after identity mismatch: %+v", runner.commands)
		}
	}
}

func writeLarkAttestation(
	t *testing.T,
	binding tenantconfig.EnvironmentBinding,
	events []string,
	subscriptions []string,
) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lark-console-attestation.json")
	contents, err := json.Marshal(map[string]any{
		"format_version":         1,
		"app_id":                 binding.Lark.AppID,
		"tenant_key":             binding.Lark.TenantKey,
		"redirect_urls":          []string{binding.PublicOrigin + "/integrations/lark/oauth/callback"},
		"console_events":         events,
		"approval_subscriptions": subscriptions,
		"reviewed_by":            "operator@example.com",
		"reviewed_at":            "2026-08-24T10:00:00+08:00",
		"evidence":               "CHG-2026-0044",
	})
	if err != nil {
		t.Fatalf("encode Lark attestation: %v", err)
	}
	if err := os.WriteFile(path, append(contents, '\n'), 0o600); err != nil {
		t.Fatalf("write Lark attestation: %v", err)
	}
	return path
}

func authenticatedIdentityStatus(t *testing.T, appID string) []byte {
	t.Helper()
	contents, err := json.Marshal(map[string]any{
		"appId": appID,
		"identities": map[string]any{
			"bot":  map[string]any{"status": "ready", "available": true},
			"user": map[string]any{"status": "ready", "available": true, "scope": "approval:approval:read offline_access"},
		},
	})
	if err != nil {
		t.Fatalf("encode authenticated identity status: %v", err)
	}
	return contents
}

func tenantIdentityEnvelope(t *testing.T, tenantKey string) []byte {
	t.Helper()
	contents, err := json.Marshal(map[string]any{
		"ok": true, "identity": "bot",
		"data": map[string]any{"tenant": map[string]any{"tenant_key": tenantKey}},
	})
	if err != nil {
		t.Fatalf("encode tenant identity envelope: %v", err)
	}
	return contents
}

func approvalDefinitionEnvelope(t *testing.T, form string) []byte {
	t.Helper()
	contents, err := json.Marshal(map[string]any{
		"ok":       true,
		"identity": "user",
		"data":     map[string]any{"approval_name": "Employee entitlement", "form": form, "node_list": []any{}},
	})
	if err != nil {
		t.Fatalf("encode approval definition envelope: %v", err)
	}
	return contents
}
