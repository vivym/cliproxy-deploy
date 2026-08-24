package tenantconfig_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/tenantconfig"
)

type recordingExecutor struct {
	calls          []string
	fail           string
	resultMismatch string
}

func completeApplyObservedState(binding tenantconfig.EnvironmentBinding) tenantconfig.ObservedState {
	return tenantconfig.ObservedState{
		LocalArtifacts: map[string]string{},
		NewAPI: &tenantconfig.ObservedNewAPI{
			PolicyCatalogs: map[string]string{},
			OAuthPreflight: &tenantconfig.ObservedOAuthPreflight{
				ChangeRequired: true, DesiredDigest: strings.Repeat("a", 64),
			},
		},
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
		},
	}
}

func (executor *recordingExecutor) Execute(_ context.Context, change tenantconfig.Change) (tenantconfig.ExecutionResult, error) {
	executor.calls = append(executor.calls, change.ID)
	if change.ID == executor.fail {
		return tenantconfig.ExecutionResult{}, context.DeadlineExceeded
	}
	if change.ID == executor.resultMismatch {
		return tenantconfig.ExecutionResult{ResultDigest: strings.Repeat("f", 64)}, nil
	}
	return tenantconfig.ExecutionResult{ResultDigest: change.DesiredDigest}, nil
}

func TestApplyVerifiesPlanBeforeExecutingAndReturnsRedactedReceipt(t *testing.T) {
	source, binding := completeConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}
	plan, err := tenantconfig.Diff(compiled, completeApplyObservedState(binding))
	if err != nil {
		t.Fatalf("plan tenant configuration: %v", err)
	}
	executor := &recordingExecutor{}
	receipt, err := tenantconfig.Apply(context.Background(), plan, plan.Digest, tenantconfig.ApplyOptions{
		ChangeTicket: "CHG-2026-0042",
		Executor:     executor,
	})
	if err != nil {
		t.Fatalf("apply tenant configuration: %v", err)
	}
	if receipt.Status != tenantconfig.ApplyStatusSucceeded || receipt.PlanDigest != plan.Digest ||
		receipt.ChangeTicket != "CHG-2026-0042" || receipt.Digest == "" {
		t.Fatalf("unexpected apply receipt: %+v", receipt)
	}
	if len(executor.calls) != len(plan.Changes) || len(receipt.Operations) != len(plan.Changes) {
		t.Fatalf("executed %d changes and recorded %d operations, want %d", len(executor.calls), len(receipt.Operations), len(plan.Changes))
	}
	for index, change := range plan.Changes {
		if executor.calls[index] != change.ID || !strings.HasPrefix(receipt.Operations[index].ID, "sha256:") ||
			receipt.Operations[index].ID == change.ID {
			t.Fatalf("operation %d = call %q receipt %q, want %q", index, executor.calls[index], receipt.Operations[index].ID, change.ID)
		}
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("encode apply receipt: %v", err)
	}
	for _, forbidden := range []string{"cli_public_app_id", "tenant-public-key", "approval-wallet-v1", "payload"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("apply receipt contains configuration payload fragment %q: %s", forbidden, encoded)
		}
	}
}

func TestApplyRejectsDigestMismatchBlockersAndPayloadTamperingBeforeExecution(t *testing.T) {
	source, binding := completeConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}
	validPlan, err := tenantconfig.Diff(compiled, completeApplyObservedState(binding))
	if err != nil {
		t.Fatalf("plan tenant configuration: %v", err)
	}

	tests := []struct {
		name           string
		plan           tenantconfig.ChangePlan
		expectedDigest string
	}{
		{name: "caller expected digest mismatch", plan: validPlan, expectedDigest: "sha256:" + strings.Repeat("0", 64)},
		{
			name: "plan contains blocker",
			plan: func() tenantconfig.ChangePlan {
				blocked := validPlan
				blocked.Blockers = []tenantconfig.Blocker{{Code: "test_blocker", Resource: "test", Message: "blocked"}}
				return blocked
			}(),
			expectedDigest: validPlan.Digest,
		},
		{
			name: "payload tampering",
			plan: func() tenantconfig.ChangePlan {
				tampered := validPlan
				tampered.Changes = append([]tenantconfig.Change(nil), validPlan.Changes...)
				tampered.Changes[0].Payload = []byte("tampered")
				return tampered
			}(),
			expectedDigest: validPlan.Digest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &recordingExecutor{}
			if _, err := tenantconfig.Apply(context.Background(), test.plan, test.expectedDigest, tenantconfig.ApplyOptions{
				ChangeTicket: "CHG-2026-0042",
				Executor:     executor,
			}); err == nil {
				t.Fatal("unsafe plan was applied")
			}
			if len(executor.calls) != 0 {
				t.Fatalf("executor was called before plan verification: %v", executor.calls)
			}
		})
	}
}

func TestApplyStopsOnFirstFailedOperationAndReturnsBoundedFailureReceipt(t *testing.T) {
	source, binding := completeConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}
	plan, err := tenantconfig.Diff(compiled, completeApplyObservedState(binding))
	if err != nil {
		t.Fatalf("plan tenant configuration: %v", err)
	}
	executor := &recordingExecutor{fail: plan.Changes[1].ID}
	receipt, err := tenantconfig.Apply(context.Background(), plan, plan.Digest, tenantconfig.ApplyOptions{
		ChangeTicket: "CHG-2026-0042",
		Executor:     executor,
	})
	if err == nil {
		t.Fatal("failed operation returned success")
	}
	if receipt.Status != tenantconfig.ApplyStatusFailed || len(receipt.Operations) != 2 ||
		receipt.Operations[1].Result != tenantconfig.OperationResultFailed || receipt.Digest == "" {
		t.Fatalf("unexpected failure receipt: %+v", receipt)
	}
	if strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("apply error leaked executor detail: %v", err)
	}
}

func TestApplyRejectsExecutorResultDigestMismatch(t *testing.T) {
	source, binding := completeConfiguration()
	compiled, err := tenantconfig.Compile(source, binding)
	if err != nil {
		t.Fatalf("compile tenant configuration: %v", err)
	}
	plan, err := tenantconfig.Diff(compiled, completeApplyObservedState(binding))
	if err != nil {
		t.Fatalf("plan tenant configuration: %v", err)
	}
	executor := &recordingExecutor{resultMismatch: plan.Changes[0].ID}
	receipt, err := tenantconfig.Apply(context.Background(), plan, plan.Digest, tenantconfig.ApplyOptions{
		ChangeTicket: "CHG-2026-0042",
		Executor:     executor,
	})
	if err == nil || !strings.Contains(err.Error(), "desired digest") {
		t.Fatalf("result digest mismatch error = %v", err)
	}
	if receipt.Status != tenantconfig.ApplyStatusFailed || len(receipt.Operations) != 1 ||
		receipt.Operations[0].Result != tenantconfig.OperationResultFailed ||
		receipt.Operations[0].ResultDigest != "" {
		t.Fatalf("unexpected mismatch receipt: %+v", receipt)
	}
}
