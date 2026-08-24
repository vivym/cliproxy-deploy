package configcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

func writeFakeLarkCLI(t *testing.T, path string) {
	t.Helper()
	envelope := func(form any) string {
		formContents, err := json.Marshal(form)
		if err != nil {
			t.Fatalf("encode fake Lark form: %v", err)
		}
		contents, err := json.Marshal(map[string]any{
			"ok": true, "identity": "user",
			"data": map[string]any{"approval_name": "test", "form": string(formContents), "node_list": []any{}},
		})
		if err != nil {
			t.Fatalf("encode fake Lark envelope: %v", err)
		}
		return "'" + strings.ReplaceAll(string(contents), "'", "'\\''") + "'"
	}
	wallet := envelope([]map[string]any{
		{"custom_id": "cost_center", "type": "textarea", "required": true},
		{"custom_id": "estimated_usage", "type": "textarea", "required": false},
		{"custom_id": "request_reason", "type": "textarea", "required": true},
		{"custom_id": "wallet_package", "type": "radioV2", "required": true,
			"option": []map[string]any{{"text": "Small"}}},
	})
	level := envelope([]map[string]any{
		{"custom_id": "cost_center", "type": "textarea", "required": true},
		{"custom_id": "estimated_usage", "type": "textarea", "required": true},
		{"custom_id": "request_reason", "type": "textarea", "required": true},
		{"custom_id": "target_level", "type": "radioV2", "required": true,
			"option": []map[string]any{{"text": "Plus"}}},
	})
	_, binding := testConfiguration()
	authStatus := fmt.Sprintf("'{\"appId\":%q,\"identities\":{\"bot\":{\"status\":\"ready\",\"available\":true},\"user\":{\"status\":\"ready\",\"available\":true,\"scope\":\"approval:approval:read offline_access\"}}}'", binding.Lark.AppID)
	tenant := fmt.Sprintf("'{\"ok\":true,\"identity\":\"bot\",\"data\":{\"tenant\":{\"tenant_key\":%q}}}'", binding.Lark.TenantKey)
	bot := "'{\"ok\":true,\"identity\":\"bot\",\"data\":{}}'"
	script := fmt.Sprintf(`#!/bin/sh
	case "$1" in
	  auth) printf '%%s\n' %s ;;
	  approval)
    case "$*" in
      *approval-wallet-v1*) printf '%%s\n' %s ;;
      *approval-level-v1*) printf '%%s\n' %s ;;
      *) exit 2 ;;
    esac
    ;;
	  api)
	    case "$*" in
	      *tenant/v2/tenant/query*) printf '%%s\n' %s ;;
	      *) printf '%%s\n' %s ;;
	    esac
	    ;;
	  *) exit 2 ;;
	esac
	`, authStatus, wallet, level, tenant, bot)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake lark-cli: %v", err)
	}
}

func TestRunPlansAndAppliesThroughIsolatedNewAPIConfigurationWindow(t *testing.T) {
	const secret = "gggggggggggggggggggggggggggggggg"
	desiredOAuthDigest := strings.Repeat("d", 64)
	var mutex sync.Mutex
	var mutatingCalls []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+secret {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/integrations/v1/config/state":
			_, _ = response.Write([]byte(`{"policies":[]}`))
		case "POST /api/integrations/v1/config/policies/preflight":
			var requested struct {
				PolicyVersion string `json:"policy_version"`
				CatalogHash   string `json:"catalog_hash"`
			}
			if err := json.NewDecoder(request.Body).Decode(&requested); err != nil {
				http.Error(response, "invalid request", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(response, `{"policy_version":%q,"catalog_hash":%q,"valid":true}`, requested.PolicyVersion, requested.CatalogHash)
		case "POST /api/integrations/v1/config/oauth-provider/preflight":
			_, _ = response.Write([]byte(`{"slug":"lark","valid":true,"change_required":true,"desired_digest":"` + desiredOAuthDigest + `"}`))
		case "POST /api/integrations/v1/config/policies":
			mutex.Lock()
			mutatingCalls = append(mutatingCalls, "publish")
			mutex.Unlock()
			_, _ = response.Write([]byte(`{"policy_version":"employee-v1","created":true}`))
		case "PUT /api/integrations/v1/config/oauth-provider":
			mutex.Lock()
			mutatingCalls = append(mutatingCalls, "oauth")
			mutex.Unlock()
			_, _ = response.Write([]byte(`{"slug":"lark","created":true,"replayed":false,"result_digest":"` + desiredOAuthDigest + `"}`))
		case "POST /api/integrations/v1/config/policies/activate":
			mutex.Lock()
			mutatingCalls = append(mutatingCalls, "activate")
			mutex.Unlock()
			_, _ = response.Write([]byte(`{"policy_version":"employee-v1","activated":true}`))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	source, binding := testConfiguration()
	sourcePath := filepath.Join(root, "policy.json")
	bindingPath := filepath.Join(root, "binding.json")
	secretPath := filepath.Join(root, "config-secret")
	larkCLIPath := filepath.Join(root, "lark-cli")
	attestationPath := filepath.Join(root, "lark-console-attestation.json")
	writeFixtureJSON(t, sourcePath, source)
	writeFixtureJSON(t, bindingPath, binding)
	if err := os.WriteFile(secretPath, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatalf("write configuration secret: %v", err)
	}
	writeFakeLarkCLI(t, larkCLIPath)
	writeFixtureJSON(t, attestationPath, map[string]any{
		"format_version": 1, "app_id": binding.Lark.AppID, "tenant_key": binding.Lark.TenantKey,
		"redirect_urls":          []string{binding.PublicOrigin + "/integrations/lark/oauth/callback"},
		"console_events":         []string{"approval.instance.status_changed_v4", "contact.user.deleted_v3"},
		"approval_subscriptions": []string{}, "reviewed_by": "test-reviewer",
		"reviewed_at": "2026-08-25T00:00:00Z", "evidence": "test-only-receipt",
	})

	runtimeRoot := filepath.Join(root, "runtime")
	planPath := filepath.Join(runtimeRoot, "ops", "plan.json")
	var output bytes.Buffer
	if err := Run(context.Background(), []string{
		"plan", "--source", sourcePath, "--binding", bindingPath,
		"--output-root", runtimeRoot, "--plan", planPath, "--remote", "lark,new-api",
		"--new-api-base-url", server.URL, "--new-api-config-secret-file", secretPath,
		"--lark-cli", larkCLIPath, "--lark-console-attestation", attestationPath,
	}, &output, &bytes.Buffer{}); err != nil {
		t.Fatalf("plan with New API preflight: %v", err)
	}
	mutex.Lock()
	requireCalls := append([]string(nil), mutatingCalls...)
	mutex.Unlock()
	if len(requireCalls) != 0 {
		t.Fatalf("plan performed mutating calls: %v", requireCalls)
	}

	planContents, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read remote plan: %v", err)
	}
	var plan tenantconfig.ChangePlan
	if err := json.Unmarshal(planContents, &plan); err != nil {
		t.Fatalf("decode remote plan: %v", err)
	}
	foundActivation := false
	for _, change := range plan.Changes {
		if change.ID == "new-api:activate:employee-v1" {
			foundActivation = true
		}
	}
	if !foundActivation {
		t.Fatalf("remote plan does not include activation: %+v", plan.Changes)
	}

	assertRejectedBeforeMutation := func(name, candidatePlan, candidateReceipt, wantError string) {
		t.Helper()
		err := Run(context.Background(), []string{
			"apply", "--plan", candidatePlan, "--output-root", runtimeRoot,
			"--receipt", candidateReceipt, "--expected-digest", plan.Digest,
			"--change-ticket", "CHG-2026-0043", "--new-api-base-url", server.URL,
			"--new-api-config-secret-file", secretPath, "--lark-cli", larkCLIPath,
		}, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), wantError) {
			t.Fatalf("%s error = %v, want %q", name, err, wantError)
		}
		mutex.Lock()
		calls := append([]string(nil), mutatingCalls...)
		mutex.Unlock()
		if len(calls) != 0 {
			t.Fatalf("%s performed remote mutations: %v", name, calls)
		}
	}
	assertRejectedBeforeMutation(
		"outside receipt",
		planPath,
		filepath.Join(root, "outside-receipt.json"),
		"escapes its managed root",
	)
	assertRejectedBeforeMutation(
		"receipt overwrites plan",
		planPath,
		planPath,
		"must be different",
	)
	hardlinkReceiptPath := filepath.Join(runtimeRoot, "ops", "hardlink-receipt.json")
	if err := os.Link(planPath, hardlinkReceiptPath); err != nil {
		t.Fatalf("create receipt hardlink fixture: %v", err)
	}
	assertRejectedBeforeMutation(
		"receipt hardlinks plan",
		planPath,
		hardlinkReceiptPath,
		"must be different",
	)
	outsidePlanPath := filepath.Join(root, "outside-plan.json")
	if err := os.WriteFile(outsidePlanPath, planContents, 0o600); err != nil {
		t.Fatalf("write outside plan fixture: %v", err)
	}
	assertRejectedBeforeMutation(
		"outside plan",
		outsidePlanPath,
		filepath.Join(runtimeRoot, "ops", "outside-plan-receipt.json"),
		"escapes its managed root",
	)
	symlinkPlanPath := filepath.Join(runtimeRoot, "ops", "symlink-plan.json")
	if err := os.Symlink(planPath, symlinkPlanPath); err != nil {
		t.Fatalf("create plan symlink fixture: %v", err)
	}
	assertRejectedBeforeMutation(
		"symlink plan",
		symlinkPlanPath,
		filepath.Join(runtimeRoot, "ops", "symlink-plan-receipt.json"),
		"regular non-symlink",
	)
	maintenanceSession := filepath.Join(runtimeRoot, "ops", "maintenance.session")
	maintenanceLock := filepath.Join(runtimeRoot, "ops", "maintenance.lock")
	if err := os.Mkdir(maintenanceSession, 0o700); err != nil {
		t.Fatalf("create active maintenance session: %v", err)
	}
	if err := os.Mkdir(maintenanceLock, 0o700); err != nil {
		t.Fatalf("create active maintenance lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(maintenanceLock, "mode"), []byte("backup\n"), 0o600); err != nil {
		t.Fatalf("write active maintenance mode: %v", err)
	}
	if err := Run(context.Background(), []string{
		"apply", "--plan", planPath, "--output-root", runtimeRoot,
		"--receipt", filepath.Join(runtimeRoot, "ops", "blocked-receipt.json"), "--expected-digest", plan.Digest,
		"--change-ticket", "CHG-2026-0043", "--new-api-base-url", server.URL,
		"--new-api-config-secret-file", secretPath, "--lark-cli", larkCLIPath,
	}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "maintenance session") {
		t.Fatalf("apply during backup error = %v, want maintenance session rejection", err)
	}
	mutex.Lock()
	callsDuringMaintenance := append([]string(nil), mutatingCalls...)
	mutex.Unlock()
	if len(callsDuringMaintenance) != 0 {
		t.Fatalf("apply during backup performed remote mutations: %v", callsDuringMaintenance)
	}
	if err := os.Remove(filepath.Join(maintenanceLock, "mode")); err != nil {
		t.Fatalf("remove active maintenance mode: %v", err)
	}
	if err := os.Remove(maintenanceLock); err != nil {
		t.Fatalf("remove active maintenance lock: %v", err)
	}
	if err := os.Remove(maintenanceSession); err != nil {
		t.Fatalf("remove active maintenance session: %v", err)
	}

	if err := Run(context.Background(), []string{
		"apply", "--plan", planPath, "--output-root", runtimeRoot,
		"--receipt", filepath.Join(runtimeRoot, "ops", "receipt.json"), "--expected-digest", plan.Digest,
		"--change-ticket", "CHG-2026-0043", "--new-api-base-url", server.URL,
		"--new-api-config-secret-file", secretPath, "--lark-cli", larkCLIPath,
	}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("apply through New API configuration window: %v", err)
	}
	mutex.Lock()
	gotCalls := append([]string(nil), mutatingCalls...)
	mutex.Unlock()
	wantCalls := []string{"publish", "oauth", "activate"}
	if !slices.Equal(gotCalls, wantCalls) {
		t.Fatalf("New API mutating calls = %v, want %v", gotCalls, wantCalls)
	}
}

func TestNewAPIConfigurationClientRejectsMismatchedPreflightIdentity(t *testing.T) {
	const secret = "gggggggggggggggggggggggggggggggg"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/integrations/v1/config/state":
			_, _ = response.Write([]byte(`{"policies":[]}`))
		case "POST /api/integrations/v1/config/policies/preflight":
			_, _ = response.Write([]byte(`{"policy_version":"wrong-policy","catalog_hash":"wrong-hash","valid":true}`))
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)

	secretPath := filepath.Join(t.TempDir(), "config-secret")
	if err := os.WriteFile(secretPath, []byte(secret), 0o600); err != nil {
		t.Fatalf("write configuration secret: %v", err)
	}
	client, err := newNewAPIConfigurationClient(server.URL, secretPath)
	if err != nil {
		t.Fatalf("create New API configuration client: %v", err)
	}
	source, binding := testConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}
	if _, err := client.observe(context.Background(), compiled); err == nil {
		t.Fatal("mismatched policy preflight identity was accepted")
	}
}

func TestNewAPIConfigurationClientAcceptsOnlyIsolatedOrLoopbackHTTPOrigins(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "config-secret")
	if err := os.WriteFile(secretPath, []byte(strings.Repeat("g", 32)), 0o600); err != nil {
		t.Fatalf("write configuration secret: %v", err)
	}
	tests := []struct {
		origin  string
		allowed bool
	}{
		{origin: "http://new-api-config-endpoint:3001", allowed: true},
		{origin: "http://localhost:3001", allowed: true},
		{origin: "http://127.0.0.1:3001", allowed: true},
		{origin: "http://[::1]:3001", allowed: true},
		{origin: "https://new-api-config-endpoint:3001", allowed: false},
		{origin: "https://config.example.com", allowed: false},
		{origin: "http://config.example.com", allowed: false},
	}
	for _, test := range tests {
		t.Run(test.origin, func(t *testing.T) {
			_, err := newNewAPIConfigurationClient(test.origin, secretPath)
			if test.allowed && err != nil {
				t.Fatalf("allowed origin was rejected: %v", err)
			}
			if !test.allowed && err == nil {
				t.Fatal("non-isolated origin was accepted")
			}
		})
	}
}

func TestNewAPIConfigurationClientRejectsMismatchedMutationResponses(t *testing.T) {
	const secret = "gggggggggggggggggggggggggggggggg"
	desiredDigest := strings.Repeat("d", 64)
	tests := []struct {
		name     string
		change   tenantconfig.Change
		response string
	}{
		{
			name: "policy publication",
			change: tenantconfig.Change{
				Target: tenantconfig.TargetNewAPI, Action: tenantconfig.ActionPublishPolicy,
				Resource: "employee-v1", DesiredDigest: desiredDigest,
			},
			response: `{"policy_version":"other-policy","created":true}`,
		},
		{
			name: "OAuth provider",
			change: tenantconfig.Change{
				Target: tenantconfig.TargetNewAPI, Action: tenantconfig.ActionUpsertDisabled,
				Resource: "lark", DesiredDigest: desiredDigest,
			},
			response: `{"slug":"other-provider","created":true,"result_digest":"` + desiredDigest + `"}`,
		},
		{
			name: "policy activation",
			change: tenantconfig.Change{
				Target: tenantconfig.TargetNewAPI, Action: tenantconfig.ActionActivatePolicy,
				Resource: "employee-v1", DesiredDigest: desiredDigest,
			},
			response: `{"policy_version":"employee-v1","activated":false}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write([]byte(test.response))
			}))
			t.Cleanup(server.Close)
			secretPath := filepath.Join(t.TempDir(), "config-secret")
			if err := os.WriteFile(secretPath, []byte(secret), 0o600); err != nil {
				t.Fatalf("write configuration secret: %v", err)
			}
			client, err := newNewAPIConfigurationClient(server.URL, secretPath)
			if err != nil {
				t.Fatalf("create New API configuration client: %v", err)
			}
			if _, err := client.Execute(context.Background(), test.change); err == nil {
				t.Fatal("mismatched mutation response was accepted")
			}
		})
	}
}
