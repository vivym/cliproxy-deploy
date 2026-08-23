package newapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/newapi"
)

func TestCorrectionClientSendsIndependentContractAndValidatesReceipt(t *testing.T) {
	expectedQuota := int64(5_000_000)
	want := newapi.EntitlementCorrectionRequest{
		ExternalID:    "lark:correction:CHG-2026-0042:wallet",
		Source:        "correction",
		PolicyVersion: "employee-entitlements-2026-09-v1",
		Identity: newapi.Identity{
			ProviderSlug: "lark", Subject: "tenant-key:ou_1",
		},
		Correction: newapi.Correction{
			Type: "wallet_quota", QuotaDelta: -2_000_000,
			ExpectedWalletQuota: &expectedQuota,
		},
		Evidence: newapi.CorrectionEvidence{
			Operator: "ops@example.com", Reason: "reverted approval",
			ChangeTicket:       "CHG-2026-0042",
			OriginalExternalID: "lark:wallet-topup:instance-original",
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost ||
			request.URL.Path != "/api/integrations/v1/entitlement-corrections" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+integrationSecret {
			t.Fatal("correction authorization header is missing")
		}
		var got newapi.EntitlementCorrectionRequest
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("request = %+v, want %+v", got, want)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
			"status":"applied",
			"external_id":"lark:correction:CHG-2026-0042:wallet",
			"user_id":42,
			"result":{
				"grant_type":"wallet_quota",
				"quota_delta":-2000000,
				"wallet_quota":3000000
			}
		}`))
	}))
	defer server.Close()

	client, err := newapi.NewCorrectionClient(newapi.CorrectionConfig{
		BaseURL: server.URL, CorrectionSecret: integrationSecret, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("new correction client: %v", err)
	}
	result, err := client.Correct(context.Background(), want)
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if result.Status != "applied" || result.UserID != 42 ||
		result.Result.WalletQuota == nil || *result.Result.WalletQuota != 3_000_000 {
		t.Fatalf("unexpected correction result: %+v", result)
	}
}

func TestCorrectionClientRejectsStaleStateAsTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusConflict)
		_, _ = response.Write([]byte(`{
			"error":{"code":"correction_state_mismatch","message":"correction_state_mismatch"}
		}`))
	}))
	defer server.Close()
	client, err := newapi.NewCorrectionClient(newapi.CorrectionConfig{
		BaseURL: server.URL, CorrectionSecret: integrationSecret, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("new correction client: %v", err)
	}
	expectedQuota := int64(5_000_000)
	_, err = client.Correct(context.Background(), newapi.EntitlementCorrectionRequest{
		ExternalID: "lark:correction:CHG-1:wallet", Source: "correction", PolicyVersion: "employee-v1",
		Identity: newapi.Identity{ProviderSlug: "lark", Subject: "tenant:open-id"},
		Correction: newapi.Correction{
			Type: "wallet_quota", QuotaDelta: -1, ExpectedWalletQuota: &expectedQuota,
		},
		Evidence: newapi.CorrectionEvidence{
			Operator: "ops@example.com", Reason: "reverted approval", ChangeTicket: "CHG-1",
			OriginalExternalID: "lark:wallet-topup:original",
		},
	})
	apiError, ok := err.(*newapi.APIError)
	if !ok || apiError.Code != "correction_state_mismatch" || apiError.Retryable {
		t.Fatalf("error = %v, want terminal correction_state_mismatch", err)
	}
}

func TestCorrectionClientPreviewsCurrentDecisionState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost ||
			request.URL.Path != "/api/integrations/v1/entitlement-corrections/preview" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer "+integrationSecret {
			t.Fatal("correction authorization header is missing")
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{
			"wallet_quota":3000000,
			"used_quota":7000000,
			"last_login_at":1788192100,
			"managed_subscription":{
				"policy_version":"employee-v1",
				"level_code":"pro",
				"assignment_version":3,
				"source_external_id":"lark:subscription-level:instance-1",
				"subscription_id":701,
				"amount_total":3000000,
				"amount_used":1500000,
				"start_time":1788192000,
				"end_time":1791388800,
				"last_reset_time":1788192000,
				"next_reset_time":1790870400
			}
		}`))
	}))
	defer server.Close()
	client, err := newapi.NewCorrectionClient(newapi.CorrectionConfig{
		BaseURL: server.URL, CorrectionSecret: integrationSecret, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("new correction client: %v", err)
	}
	preview, err := client.Preview(context.Background(), newapi.Identity{
		ProviderSlug: "lark", Subject: "tenant-key:ou_1",
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.WalletQuota != 3_000_000 || preview.UsedQuota != 7_000_000 ||
		preview.ManagedSubscription == nil ||
		preview.ManagedSubscription.AssignmentVersion != 3 ||
		preview.ManagedSubscription.AmountUsed != 1_500_000 {
		t.Fatalf("unexpected correction preview: %+v", preview)
	}
}
