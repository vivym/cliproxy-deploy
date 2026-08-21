package newapi_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

func TestPlanApprovalGrantProducesStableWalletCommandAndSanitizedReceipt(t *testing.T) {
	request, receipt, err := newapi.PlanApprovalGrant(newapi.ApprovalGrantInput{
		TenantKey: "tenant-1", OpenID: "ou-requester",
		PolicyVersion: "employee-v1", ApprovalKind: "wallet_topup",
		BusinessCode: "topup_10", QuotaDelta: 5_000_000,
		ApprovalCode: "approval-1", InstanceCode: "instance-1",
		StartTimeMilliseconds: "1787303900000",
		SchemaFingerprint:     "sha256:abc", Locale: "zh-CN", CatalogSHA256: "sha256:catalog",
	})
	if err != nil {
		t.Fatalf("plan wallet grant: %v", err)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	want := `{"external_id":"lark:wallet-topup:instance-1","source":"lark_approval","policy_version":"employee-v1","identity":{"provider_slug":"lark","subject":"tenant-1:ou-requester"},"grant":{"type":"wallet_quota","package_code":"topup_10","quota_delta":5000000},"evidence":{"approval_code":"approval-1","instance_code":"instance-1","instance_started_at":"2026-08-21T09:18:20Z","schema_fingerprint":"sha256:abc","locale":"zh-CN"}}`
	if string(payload) != want {
		t.Fatalf("payload = %s\nwant    = %s", payload, want)
	}
	if receipt.ExternalID != "lark:wallet-topup:instance-1" || receipt.GrantType != "wallet_quota" ||
		receipt.BusinessCode != "topup_10" || receipt.QuotaDelta != 5_000_000 ||
		receipt.SubjectSHA256 != "60d47581bb71cb0484339201776d4d08b4b06fed9607a6c7230dad44e71af1e8" ||
		receipt.RequestSHA256 != "49f299ec0af925c72d67a9461c9ec1f860e4298f17941dc36023c2cdcd825834" {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
}

func TestPlanApprovalGrantKeepsSubscriptionQuotaOutOfCommand(t *testing.T) {
	request, receipt, err := newapi.PlanApprovalGrant(newapi.ApprovalGrantInput{
		TenantKey: "tenant-1", OpenID: "ou-requester",
		PolicyVersion: "employee-v1", ApprovalKind: "subscription_level",
		BusinessCode: "pro", MonthlyQuota: 25_000_000,
		ApprovalCode: "approval-2", InstanceCode: "instance-2",
		StartTimeMilliseconds: "1787303900000",
		SchemaFingerprint:     "sha256:def", Locale: "zh-CN", CatalogSHA256: "sha256:catalog",
	})
	if err != nil {
		t.Fatalf("plan subscription grant: %v", err)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	want := `{"external_id":"lark:subscription-level:instance-2","source":"lark_approval","policy_version":"employee-v1","identity":{"provider_slug":"lark","subject":"tenant-1:ou-requester"},"grant":{"type":"subscription_level","level_code":"pro","minimum_rank_only":true},"evidence":{"approval_code":"approval-2","instance_code":"instance-2","instance_started_at":"2026-08-21T09:18:20Z","schema_fingerprint":"sha256:def","locale":"zh-CN"}}`
	if string(payload) != want {
		t.Fatalf("payload = %s\nwant    = %s", payload, want)
	}
	if receipt.MonthlyQuota != 25_000_000 || receipt.QuotaDelta != 0 ||
		receipt.GrantType != "subscription_level" || receipt.BusinessCode != "pro" {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
}

func TestPlanBaseSubscriptionGrantProducesStableMinimumBasicCommand(t *testing.T) {
	request, receipt, err := newapi.PlanBaseSubscriptionGrant(newapi.BaseSubscriptionGrantInput{
		Subject: "tenant-1:ou-employee", PolicyVersion: "employee-v1",
		LevelCode: "basic", MonthlyQuota: 5_000_000,
		CatalogSHA256: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatalf("plan base subscription grant: %v", err)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal base subscription request: %v", err)
	}
	want := `{"external_id":"lark:base:tenant-1:ou-employee:employee-v1","source":"base_login","policy_version":"employee-v1","identity":{"provider_slug":"lark","subject":"tenant-1:ou-employee"},"grant":{"type":"subscription_level","level_code":"basic","minimum_rank_only":true}}`
	if string(payload) != want {
		t.Fatalf("payload = %s\nwant    = %s", payload, want)
	}
	if receipt.ExternalID != "lark:base:tenant-1:ou-employee:employee-v1" ||
		receipt.PolicyVersion != "employee-v1" || receipt.CatalogSHA256 != strings.Repeat("a", 64) ||
		receipt.GrantType != "subscription_level" || receipt.BusinessCode != "basic" ||
		receipt.MonthlyQuota != 5_000_000 || receipt.QuotaDelta != 0 ||
		receipt.SubjectSHA256 != "251804d8ac92f6793c7c5eb1c8aa4b503de4d68151e22683de897e601140fd87" ||
		receipt.RequestSHA256 != "f0c23ad1c33791850eebb4c552b0ebf7e738f6045210c649e29d3b71efbf6e13" {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
}

func TestPlanBaseSubscriptionGrantRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*newapi.BaseSubscriptionGrantInput)
	}{
		{name: "missing subject", mutate: func(input *newapi.BaseSubscriptionGrantInput) { input.Subject = "" }},
		{name: "missing policy", mutate: func(input *newapi.BaseSubscriptionGrantInput) { input.PolicyVersion = "" }},
		{name: "non-basic level", mutate: func(input *newapi.BaseSubscriptionGrantInput) { input.LevelCode = "pro" }},
		{name: "zero monthly quota", mutate: func(input *newapi.BaseSubscriptionGrantInput) { input.MonthlyQuota = 0 }},
		{name: "negative monthly quota", mutate: func(input *newapi.BaseSubscriptionGrantInput) { input.MonthlyQuota = -1 }},
		{name: "missing catalog hash", mutate: func(input *newapi.BaseSubscriptionGrantInput) { input.CatalogSHA256 = "" }},
		{name: "non-hex catalog hash", mutate: func(input *newapi.BaseSubscriptionGrantInput) { input.CatalogSHA256 = strings.Repeat("z", 64) }},
		{name: "uppercase catalog hash", mutate: func(input *newapi.BaseSubscriptionGrantInput) { input.CatalogSHA256 = strings.Repeat("A", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := newapi.BaseSubscriptionGrantInput{
				Subject: "tenant-1:ou-employee", PolicyVersion: "employee-v1",
				LevelCode: "basic", MonthlyQuota: 5_000_000,
				CatalogSHA256: strings.Repeat("a", 64),
			}
			test.mutate(&input)
			if _, _, err := newapi.PlanBaseSubscriptionGrant(input); err == nil {
				t.Fatal("invalid base subscription grant input was accepted")
			}
		})
	}
}
