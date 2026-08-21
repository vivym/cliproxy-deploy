package larkapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/larkapi"
)

func TestApprovalFetcherUsesTenantTokenAndFixedLocale(t *testing.T) {
	var tokenRequests int
	var instanceRequests int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			tokenRequests++
			writeTestJSON(t, response, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case "/open-apis/approval/v4/instances/instance-001":
			instanceRequests++
			if request.Header.Get("Authorization") != "Bearer tenant-token" {
				t.Fatalf("authorization = %q, want tenant token", request.Header.Get("Authorization"))
			}
			if request.URL.Query().Get("locale") != "zh-CN" {
				t.Fatalf("locale = %q, want zh-CN", request.URL.Query().Get("locale"))
			}
			writeTestJSON(t, response, map[string]any{
				"code": 0,
				"msg":  "ok",
				"data": map[string]any{
					"approval_code": "approval-wallet-v1",
					"instance_code": "instance-001",
					"status":        "APPROVED",
					"open_id":       "ou_requester",
					"start_time":    "1787270300000",
					"form":          `[{\"custom_id\":\"wallet_package\",\"value\":\"Small\"}]`,
					"reverted":      false,
				},
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
		AppID: "cli_test", AppSecret: "app-secret", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("new approval fetcher: %v", err)
	}
	instance, err := fetcher.Fetch(context.Background(), "instance-001", "zh-CN")
	if err != nil {
		t.Fatalf("fetch approval instance: %v", err)
	}
	if tokenRequests != 1 || instanceRequests != 1 {
		t.Fatalf("requests token=%d instance=%d, want 1 each", tokenRequests, instanceRequests)
	}
	if instance.ApprovalCode != "approval-wallet-v1" || instance.InstanceCode != "instance-001" ||
		instance.Status != "APPROVED" || instance.OpenID != "ou_requester" || instance.FormJSON == "" {
		t.Fatalf("unexpected normalized instance: %+v", instance)
	}
}

func writeTestJSON(t *testing.T, response http.ResponseWriter, payload any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(payload); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
