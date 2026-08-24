package tenantconfig_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

func TestCompileRotationPreservesHistoricalPolicyAndApprovalBindings(t *testing.T) {
	source, binding := completeConfiguration()
	oldPolicy := source.Policy
	oldPolicy.PolicyVersion = "employee-v0"
	oldPolicy.State = policy.PolicyStateDraining
	cutoff := "2026-08-24T02:00:00Z"
	source.HistoricalPolicies = []tenantconfig.HistoricalPolicySource{{
		Policy: oldPolicy,
		Approvals: []tenantconfig.HistoricalApprovalSource{
			{
				ApprovalCode: "approval-wallet-v0", Manifest: source.Approvals[1],
				AcceptInstanceStartedBefore: cutoff,
			},
			{
				ApprovalCode: "approval-level-v0", Manifest: source.Approvals[0],
				AcceptInstanceStartedBefore: cutoff,
			},
		},
	}}
	binding.NewAPI.ExpectedActivePolicyVersion = "employee-v0"
	binding.NewAPI.AcceptCurrentInstancesStartedBefore = cutoff

	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile policy rotation: %v", err)
	}
	oldArtifact := requireArtifact(t, compiled, "policies/employee-v0.policy.json")
	if !bytes.Contains(oldArtifact.Contents, []byte(`"state": "draining"`)) {
		t.Fatalf("historical policy is not draining:\n%s", oldArtifact.Contents)
	}
	bindings := requireArtifact(t, compiled, "policies/approval-bindings.json")
	for _, required := range []string{
		`"approval_code": "approval-wallet-v0"`,
		`"approval_code": "approval-level-v0"`,
		`"policy_version": "employee-v0"`,
		`"accept_instance_started_before": "` + cutoff + `"`,
	} {
		if !bytes.Contains(bindings.Contents, []byte(required)) {
			t.Fatalf("historical bindings are missing %q:\n%s", required, bindings.Contents)
		}
	}
	preflight := requireArtifact(t, compiled, "lark/tenant-preflight.json")
	for _, approvalCode := range []string{"approval-wallet-v0", "approval-level-v0"} {
		if !bytes.Contains(preflight.Contents, []byte(`"approval_code": "`+approvalCode+`"`)) {
			t.Fatalf("historical approval %q is not retained in Lark subscription preflight:\n%s", approvalCode, preflight.Contents)
		}
	}
	publication := requireArtifact(t, compiled, "new-api/policy-publication.json")
	for _, historicalCode := range []string{"approval-wallet-v0", "approval-level-v0"} {
		if bytes.Contains(publication.Contents, []byte(historicalCode)) {
			t.Fatalf("new policy publication contains historical approval %q:\n%s", historicalCode, publication.Contents)
		}
	}
	for _, activeCode := range []string{"approval-wallet-v1", "approval-level-v1"} {
		if !bytes.Contains(publication.Contents, []byte(activeCode)) {
			t.Fatalf("new policy publication is missing active approval %q:\n%s", activeCode, publication.Contents)
		}
	}

	directory := t.TempDir()
	for _, path := range []string{
		"policies/employee-v0.policy.json",
		"policies/employee-v1.policy.json",
		"policies/approval-bindings.json",
	} {
		artifact := requireArtifact(t, compiled, path)
		if err := os.WriteFile(filepath.Join(directory, filepath.Base(path)), artifact.Contents, 0o600); err != nil {
			t.Fatalf("write compiled catalog artifact %q: %v", path, err)
		}
	}
	catalog, err := policy.LoadDirectory(directory, filepath.Join(directory, "approval-bindings.json"))
	if err != nil {
		t.Fatalf("load rotation catalog: %v", err)
	}
	snapshot := catalog.Snapshot()
	if catalog.ActivePolicyVersion() != "employee-v1" || len(snapshot.Policies) != 2 || len(snapshot.Bindings) != 4 {
		t.Fatalf("rotation catalog lost history: active=%q snapshot=%+v", catalog.ActivePolicyVersion(), snapshot)
	}
}

func TestCompileRotationRejectsMissingExpectedActiveHistory(t *testing.T) {
	source, binding := completeConfiguration()
	binding.NewAPI.ExpectedActivePolicyVersion = "employee-v0"
	binding.NewAPI.AcceptCurrentInstancesStartedBefore = "2026-08-24T02:00:00Z"
	_, err := tenantconfig.Compile(source, binding)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("expected active policy must be retained")) {
		t.Fatalf("missing historical policy error = %v", err)
	}
}
