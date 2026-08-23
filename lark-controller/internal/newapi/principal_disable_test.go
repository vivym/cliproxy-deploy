package newapi_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

func TestPlanEmploymentReconciliationPrincipalDisableProducesStableCommand(t *testing.T) {
	tests := []struct {
		status string
		reason string
	}{
		{status: "resigned", reason: "lark_employment_resigned"},
		{status: "exited", reason: "lark_employment_exited"},
		{status: "not_found", reason: "lark_employment_not_found_confirmed"},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			request, receipt, err := newapi.PlanEmploymentReconciliationPrincipalDisable(
				"tenant-1", "ou-employee", "2026-08-23", test.status,
			)
			if err != nil {
				t.Fatalf("plan employment reconciliation principal disable: %v", err)
			}
			if request.ExternalID != "lark:disable-reconcile:tenant-1:ou-employee:2026-08-23" ||
				request.Source != "employment_reconciliation" || request.Identity.ProviderSlug != "lark" ||
				request.Identity.Subject != "tenant-1:ou-employee" || request.Reason != test.reason {
				t.Fatalf("unexpected request: %+v", request)
			}
			if receipt.ExternalID != request.ExternalID || receipt.RequestSHA256 == "" ||
				len(receipt.SubjectSHA256) != 64 {
				t.Fatalf("unexpected receipt: %+v", receipt)
			}
		})
	}
	for _, invalid := range []struct {
		name   string
		date   string
		status string
	}{
		{name: "invalid date", date: "2026-8-23", status: "resigned"},
		{name: "present status", date: "2026-08-23", status: "present"},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			_, _, err := newapi.PlanEmploymentReconciliationPrincipalDisable(
				"tenant-1", "ou-employee", invalid.date, invalid.status,
			)
			if err == nil || strings.Contains(err.Error(), "ou-employee") {
				t.Fatalf("invalid planner input error = %v", err)
			}
		})
	}
}

func TestPlanContactEventPrincipalDisableProducesStableCommand(t *testing.T) {
	request, receipt, err := newapi.PlanContactEventPrincipalDisable(
		"tenant-1",
		"ou-resigned",
		"evt-resigned-1",
	)
	if err != nil {
		t.Fatalf("plan contact event principal disable: %v", err)
	}
	if request.ExternalID != "lark:disable:evt-resigned-1" ||
		request.Source != "contact_event" || request.Identity.ProviderSlug != "lark" ||
		request.Identity.Subject != "tenant-1:ou-resigned" ||
		request.Reason != "contact.user.deleted_v3" {
		t.Fatalf("unexpected request: %+v", request)
	}
	if receipt.ExternalID != request.ExternalID ||
		receipt.RequestSHA256 != "658b00ef1375f797c3052626e651b1cbce27c8e225aaccbe9af30c5d25a10400" ||
		receipt.SubjectSHA256 == "" {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
}

func TestGrantKeyringSealsPrincipalDisableWithDomainSeparatedCiphertext(t *testing.T) {
	keyring, err := newapi.NewGrantKeyring(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	request, _, err := newapi.PlanContactEventPrincipalDisable(
		"tenant-1",
		"ou-resigned",
		"evt-resigned-1",
	)
	if err != nil {
		t.Fatalf("plan contact event principal disable: %v", err)
	}
	sealed, err := keyring.SealPrincipalDisable(request)
	if err != nil {
		t.Fatalf("seal principal disable: %v", err)
	}
	opened, err := keyring.OpenPrincipalDisable(sealed)
	if err != nil {
		t.Fatalf("open principal disable: %v", err)
	}
	if opened != request {
		t.Fatalf("opened request = %+v, want %+v", opened, request)
	}
	grantShaped := newapi.SealedGrantRequest{
		KeyID: sealed.KeyID, ExternalID: sealed.ExternalID,
		RequestSHA256: sealed.RequestSHA256, Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext,
	}
	if _, err := keyring.Open(grantShaped); err == nil {
		t.Fatal("principal disable ciphertext opened as an entitlement grant")
	}
}
