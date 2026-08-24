package tenantconfig_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

func TestDiffPlansDeterministicLocalNewAPIAndLarkChanges(t *testing.T) {
	source, binding := completeConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}
	observed := tenantconfig.ObservedState{
		LocalArtifacts: map[string]string{},
		NewAPI: &tenantconfig.ObservedNewAPI{
			PolicyCatalogs: map[string]string{},
			OAuthPreflight: &tenantconfig.ObservedOAuthPreflight{
				ChangeRequired: true, DesiredDigest: strings.Repeat("a", 64),
			},
		},
		Lark: &tenantconfig.ObservedLark{
			AppID:     binding.Lark.AppID,
			TenantKey: binding.Lark.TenantKey,
			ApprovalFingerprints: map[string]string{
				"approval-wallet-v1": walletManifestFingerprint,
				"approval-level-v1":  levelManifestFingerprint,
			},
			RedirectURLs: map[string]bool{
				binding.PublicOrigin + "/integrations/lark/oauth/callback": true,
			},
			ConsoleEvents: map[string]bool{
				"approval.instance.status_changed_v4": true,
				"contact.user.deleted_v3":             true,
			},
		},
	}

	plan, err := tenantconfig.Diff(compiled, observed)
	if err != nil {
		t.Fatalf("diff tenant configuration: %v", err)
	}
	if len(plan.Blockers) != 0 {
		t.Fatalf("unexpected plan blockers: %+v", plan.Blockers)
	}
	if plan.CompiledDigest != compiled.Digest || plan.Digest == "" {
		t.Fatalf("plan digests = compiled %q plan %q, want compiled %q and non-empty plan", plan.CompiledDigest, plan.Digest, compiled.Digest)
	}

	wantRemoteOperations := map[string]bool{
		"new-api:activate:employee-v1":               false,
		"new-api:publish:employee-v1":                false,
		"new-api:upsert-disabled:lark":               false,
		"lark:subscribe-approval:approval-level-v1":  false,
		"lark:subscribe-approval:approval-wallet-v1": false,
	}
	previousID := ""
	previousSequence := tenantconfig.ChangeSequence(-1)
	for _, change := range plan.Changes {
		if change.Sequence < previousSequence || (change.Sequence == previousSequence && change.ID <= previousID) {
			t.Fatalf("changes are not sorted by sequence and stable ID: %d/%q follows %d/%q", change.Sequence, change.ID, previousSequence, previousID)
		}
		previousID = change.ID
		previousSequence = change.Sequence
		if _, expected := wantRemoteOperations[change.ID]; expected {
			wantRemoteOperations[change.ID] = true
		}
	}
	for operation, found := range wantRemoteOperations {
		if !found {
			t.Fatalf("plan is missing operation %q: %+v", operation, plan.Changes)
		}
	}
	for _, artifact := range compiled.Artifacts {
		operation := "local:write:" + artifact.Path
		found := false
		for _, change := range plan.Changes {
			if change.ID == operation && change.DesiredDigest == artifact.SHA256 {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("plan is missing local artifact operation %q", operation)
		}
	}

	replayed, err := tenantconfig.Diff(compiled, observed)
	if err != nil {
		t.Fatalf("repeat diff: %v", err)
	}
	if replayed.Digest != plan.Digest {
		t.Fatalf("repeat plan digest = %q, want %q", replayed.Digest, plan.Digest)
	}
}

func TestDiffRequiresExplicitConsoleEventAttestation(t *testing.T) {
	source, binding := completeConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}
	plan, err := tenantconfig.Diff(compiled, tenantconfig.ObservedState{
		Lark: &tenantconfig.ObservedLark{
			AppID: binding.Lark.AppID, TenantKey: binding.Lark.TenantKey,
			ApprovalFingerprints: map[string]string{
				"approval-wallet-v1": walletManifestFingerprint,
				"approval-level-v1":  levelManifestFingerprint,
			},
			RedirectURLs: map[string]bool{
				binding.PublicOrigin + "/integrations/lark/oauth/callback": true,
			},
			ConsoleEvents: map[string]bool{},
		},
	})
	if err != nil {
		t.Fatalf("diff tenant configuration: %v", err)
	}
	want := map[string]bool{
		"approval.instance.status_changed_v4": false,
		"contact.user.deleted_v3":             false,
	}
	for _, blocker := range plan.Blockers {
		if blocker.Code == "lark_console_event_not_attested" {
			want[blocker.Resource] = true
		}
	}
	for event, found := range want {
		if !found {
			t.Fatalf("missing console event blocker for %q: %+v", event, plan.Blockers)
		}
	}
	for _, change := range plan.Changes {
		if change.Resource == "contact.user.deleted_v3" {
			t.Fatalf("contact console event was planned as an automatic write: %+v", change)
		}
	}
}

func TestDiffSkipsAttestedApprovalSubscriptions(t *testing.T) {
	source, binding := completeConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}
	plan, err := tenantconfig.Diff(compiled, tenantconfig.ObservedState{
		Lark: &tenantconfig.ObservedLark{
			AppID: binding.Lark.AppID, TenantKey: binding.Lark.TenantKey,
			ApprovalFingerprints: map[string]string{
				"approval-wallet-v1": walletManifestFingerprint,
				"approval-level-v1":  levelManifestFingerprint,
			},
			RedirectURLs: map[string]bool{
				binding.PublicOrigin + "/integrations/lark/oauth/callback": true,
			},
			ConsoleEvents: map[string]bool{
				"approval.instance.status_changed_v4": true,
				"contact.user.deleted_v3":             true,
			},
			ApprovalSubscriptions: map[string]bool{
				"approval-wallet-v1": true,
			},
		},
	})
	if err != nil {
		t.Fatalf("diff tenant configuration: %v", err)
	}
	foundPending := false
	for _, change := range plan.Changes {
		if change.ID == "lark:subscribe-approval:approval-wallet-v1" {
			t.Fatalf("attested subscription was planned again: %+v", change)
		}
		if change.ID == "lark:subscribe-approval:approval-level-v1" {
			foundPending = true
		}
	}
	if !foundPending {
		t.Fatalf("unattested approval subscription was not planned: %+v", plan.Changes)
	}
}

func TestDiffRejectsUnexpectedLarkConsoleState(t *testing.T) {
	source, binding := completeConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}
	plan, err := tenantconfig.Diff(compiled, tenantconfig.ObservedState{
		Lark: &tenantconfig.ObservedLark{
			AppID: binding.Lark.AppID, TenantKey: binding.Lark.TenantKey,
			ApprovalFingerprints: map[string]string{
				"approval-wallet-v1": walletManifestFingerprint,
				"approval-level-v1":  levelManifestFingerprint,
			},
			RedirectURLs: map[string]bool{
				binding.PublicOrigin + "/integrations/lark/oauth/callback": true,
				"https://unexpected.example.com/callback":                  true,
			},
			ConsoleEvents: map[string]bool{
				"approval.instance.status_changed_v4": true,
				"contact.user.deleted_v3":             true,
				"approval.task.status_changed_v4":     true,
			},
			ApprovalSubscriptions: map[string]bool{"unrelated-approval": true},
		},
	})
	if err != nil {
		t.Fatalf("diff unexpected Lark state: %v", err)
	}
	wantCodes := map[string]bool{
		"lark_redirect_url_unexpected":          false,
		"lark_console_event_unexpected":         false,
		"lark_approval_subscription_unexpected": false,
	}
	for _, blocker := range plan.Blockers {
		if _, wanted := wantCodes[blocker.Code]; wanted {
			wantCodes[blocker.Code] = true
		}
	}
	for code, found := range wantCodes {
		if !found {
			t.Fatalf("missing blocker %q: %+v", code, plan.Blockers)
		}
	}
}

func TestDiffFailsClosedOnNewAPICatalogOrLarkDefinitionDrift(t *testing.T) {
	source, binding := completeConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}

	tests := []struct {
		name     string
		observed tenantconfig.ObservedState
		wantCode string
	}{
		{
			name: "immutable New API catalog drift",
			observed: tenantconfig.ObservedState{NewAPI: &tenantconfig.ObservedNewAPI{
				PolicyCatalogs: map[string]string{"employee-v1": strings.Repeat("b", 64)},
			}},
			wantCode: "new_api_policy_immutable_drift",
		},
		{
			name: "Lark approval display or schema drift",
			observed: tenantconfig.ObservedState{Lark: &tenantconfig.ObservedLark{
				AppID:     binding.Lark.AppID,
				TenantKey: binding.Lark.TenantKey,
				ApprovalFingerprints: map[string]string{
					"approval-wallet-v1": "sha256:" + strings.Repeat("c", 64),
					"approval-level-v1":  levelManifestFingerprint,
				},
			}},
			wantCode: "lark_approval_definition_drift",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := tenantconfig.Diff(compiled, test.observed)
			if err != nil {
				t.Fatalf("diff drifted state: %v", err)
			}
			found := false
			for _, blocker := range plan.Blockers {
				if blocker.Code == test.wantCode {
					found = true
				}
			}
			if !found {
				t.Fatalf("plan blockers = %+v, want code %q", plan.Blockers, test.wantCode)
			}
		})
	}
}

func TestDiffDoesNotActivatePolicyThatIsNoLongerStaged(t *testing.T) {
	source, binding := completeConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}
	publication := requireArtifact(t, compiled, "new-api/policy-publication.json")
	var desired struct {
		CatalogHash string `json:"catalog_hash"`
	}
	if err := json.Unmarshal(publication.Contents, &desired); err != nil {
		t.Fatalf("decode publication: %v", err)
	}

	plan, err := tenantconfig.Diff(compiled, tenantconfig.ObservedState{
		NewAPI: &tenantconfig.ObservedNewAPI{
			PolicyCatalogs: map[string]string{"employee-v1": desired.CatalogHash},
			PolicyStates:   map[string]string{"employee-v1": "retired"},
			OAuthPreflight: &tenantconfig.ObservedOAuthPreflight{
				ChangeRequired: true, DesiredDigest: strings.Repeat("a", 64),
			},
		},
	})
	if err != nil {
		t.Fatalf("diff retired policy: %v", err)
	}
	foundBlocker := false
	for _, blocker := range plan.Blockers {
		if blocker.Code == "new_api_policy_not_staged" {
			foundBlocker = true
		}
	}
	if !foundBlocker {
		t.Fatalf("retired policy blockers = %+v, want new_api_policy_not_staged", plan.Blockers)
	}
	for _, change := range plan.Changes {
		if change.Action == tenantconfig.ActionActivatePolicy {
			t.Fatalf("retired policy was scheduled for activation: %+v", change)
		}
	}
}
