package larkapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/larkapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

func TestApprovalFetcherParsesSanitizedRevertedInstanceFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "lark", "approval_instance_reverted.json"))
	if err != nil {
		t.Fatalf("read approval instance fixture: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeTestJSON(t, response, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case "/open-apis/approval/v4/instances/instance-original-v2":
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write(fixture)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
		AppID: "cli_fixture", AppSecret: "app-secret", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("new approval fetcher: %v", err)
	}
	instance, err := fetcher.Fetch(context.Background(), "instance-original-v2", "zh-CN")
	if err != nil {
		t.Fatalf("fetch reverted approval instance: %v", err)
	}
	if instance.ApprovalCode != "approval-wallet-v1" ||
		instance.InstanceCode != "instance-original-v2" ||
		instance.Status != "APPROVED" || !instance.Reverted ||
		instance.OpenID != "ou_sanitized_requester" || instance.FormJSON == "" {
		t.Fatalf("unexpected reverted approval instance: %+v", instance)
	}
}

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

func TestApprovalFetcherClassifiesHTTPFailures(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		code       int
		retryAfter string
		reason     worker.ApprovalFetchFailureReason
		retryable  bool
		delay      time.Duration
	}{
		{
			name: "rate limit", status: http.StatusTooManyRequests, code: 99991400,
			retryAfter: "17", reason: worker.ApprovalFetchRateLimited,
			retryable: true, delay: 17 * time.Second,
		},
		{
			name: "rate limit oversized retry after", status: http.StatusTooManyRequests, code: 99991400,
			retryAfter: "9223372036854775807", reason: worker.ApprovalFetchRateLimited,
			retryable: true, delay: time.Duration(1<<63 - 1),
		},
		{
			name: "rate limit business code", status: http.StatusOK, code: 99991400,
			reason: worker.ApprovalFetchRateLimited, retryable: true,
		},
		{
			name: "server error", status: http.StatusServiceUnavailable, code: 999999,
			reason: worker.ApprovalFetchServerError, retryable: true,
		},
		{
			name: "request timeout", status: http.StatusRequestTimeout, code: 999999,
			reason: worker.ApprovalFetchTimeout, retryable: true,
		},
		{
			name: "client error", status: http.StatusBadRequest, code: 1390001,
			reason: worker.ApprovalFetchClientError, retryable: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/open-apis/auth/v3/tenant_access_token/internal":
					writeTestJSON(t, response, map[string]any{
						"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
					})
				case "/open-apis/approval/v4/instances/instance-failure":
					if test.retryAfter != "" {
						response.Header().Set("Retry-After", test.retryAfter)
					}
					response.Header().Set("Content-Type", "application/json")
					response.WriteHeader(test.status)
					writeTestJSON(t, response, map[string]any{"code": test.code, "msg": "sanitized test failure"})
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
			_, err = fetcher.Fetch(context.Background(), "instance-failure", "zh-CN")
			if err == nil {
				t.Fatal("fetch succeeded, want classified failure")
			}
			var failure *worker.ApprovalFetchError
			if !errors.As(err, &failure) {
				t.Fatalf("error type = %T, want *worker.ApprovalFetchError", err)
			}
			if failure.Reason != test.reason || failure.Retryable != test.retryable || failure.RetryAfter != test.delay {
				t.Fatalf("failure = %+v, want reason=%q retryable=%t retry_after=%s",
					failure, test.reason, test.retryable, test.delay)
			}
		})
	}
}

func TestApprovalFetcherClassifiesRequestTimeoutAsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		time.Sleep(50 * time.Millisecond)
		writeTestJSON(t, response, map[string]any{
			"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
		})
	}))
	defer server.Close()

	fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
		AppID: "cli_test", AppSecret: "app-secret", BaseURL: server.URL, Timeout: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new approval fetcher: %v", err)
	}
	_, err = fetcher.Fetch(context.Background(), "instance-timeout", "zh-CN")
	var failure *worker.ApprovalFetchError
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want classified timeout", err)
	}
	if failure.Reason != worker.ApprovalFetchTimeout || !failure.Retryable {
		t.Fatalf("failure = %+v, want retryable timeout", failure)
	}
}

func TestApprovalFetcherClassifiesMissingDataAsTerminalInvalidResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeTestJSON(t, response, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case "/open-apis/approval/v4/instances/instance-missing-data":
			writeTestJSON(t, response, map[string]any{"code": 0, "msg": "ok"})
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
	_, err = fetcher.Fetch(context.Background(), "instance-missing-data", "zh-CN")
	var failure *worker.ApprovalFetchError
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want classified invalid response", err)
	}
	if failure.Reason != worker.ApprovalFetchInvalidResponse || failure.Retryable {
		t.Fatalf("failure = %+v, want terminal invalid response", failure)
	}
}

func TestApprovalFetcherClassifiesTenantTokenRejectionAsTerminalClientError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, response, map[string]any{"code": 10003, "msg": "invalid credentials"})
	}))
	defer server.Close()

	fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
		AppID: "cli_test", AppSecret: "app-secret", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("new approval fetcher: %v", err)
	}
	_, err = fetcher.Fetch(context.Background(), "instance-token-failure", "zh-CN")
	var failure *worker.ApprovalFetchError
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want classified token rejection", err)
	}
	if failure.Reason != worker.ApprovalFetchClientError || failure.Retryable || failure.LarkCode != 10003 {
		t.Fatalf("failure = %+v, want terminal client error with Lark code", failure)
	}
}

func TestApprovalFetcherClassifiesTenantTokenRateLimitAsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, response, map[string]any{"code": 99991400, "msg": "rate limited"})
	}))
	defer server.Close()

	fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
		AppID: "cli_test", AppSecret: "app-secret", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("new approval fetcher: %v", err)
	}
	_, err = fetcher.Fetch(context.Background(), "instance-token-rate-limit", "zh-CN")
	var failure *worker.ApprovalFetchError
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want classified token rate limit", err)
	}
	if failure.Reason != worker.ApprovalFetchRateLimited || !failure.Retryable || failure.LarkCode != 99991400 {
		t.Fatalf("failure = %+v, want retryable rate limit with Lark code", failure)
	}
}

func TestApprovalFetcherClassifiesTokenAndNonJSONServerFailuresAsRetryable(t *testing.T) {
	tests := []struct {
		name             string
		failTokenRequest bool
	}{
		{name: "token endpoint", failTokenRequest: true},
		{name: "approval endpoint"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" && !test.failTokenRequest {
					writeTestJSON(t, response, map[string]any{
						"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
					})
					return
				}
				response.Header().Set("Content-Type", "text/html")
				response.Header().Set("Retry-After", "23")
				response.WriteHeader(http.StatusServiceUnavailable)
				_, _ = response.Write([]byte("<html>proxy unavailable</html>"))
			}))
			defer server.Close()

			fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
				AppID: "cli_test", AppSecret: "app-secret", BaseURL: server.URL,
			})
			if err != nil {
				t.Fatalf("new approval fetcher: %v", err)
			}
			_, err = fetcher.Fetch(context.Background(), "instance-server-failure", "zh-CN")
			var failure *worker.ApprovalFetchError
			if !errors.As(err, &failure) {
				t.Fatalf("error = %v, want classified server failure", err)
			}
			if failure.Reason != worker.ApprovalFetchServerError || !failure.Retryable ||
				failure.StatusCode != http.StatusServiceUnavailable || failure.RetryAfter != 23*time.Second {
				t.Fatalf("failure = %+v, want retryable server error with Retry-After", failure)
			}
		})
	}
}

func TestApprovalFetcherClassifiesConnectionResetAsRetryableTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			writeTestJSON(t, response, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
			return
		}
		connection, _, err := response.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack connection: %v", err)
			return
		}
		_ = connection.Close()
	}))
	defer server.Close()

	fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
		AppID: "cli_test", AppSecret: "app-secret", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("new approval fetcher: %v", err)
	}
	_, err = fetcher.Fetch(context.Background(), "instance-reset", "zh-CN")
	var failure *worker.ApprovalFetchError
	if !errors.As(err, &failure) || failure.Reason != worker.ApprovalFetchTransportError || !failure.Retryable {
		t.Fatalf("failure = %+v err=%v, want retryable transport error", failure, err)
	}
}

func TestApprovalFetcherClassifiesMidBodyResetAsRetryableTransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			writeTestJSON(t, response, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
			return
		}
		connection, buffer, err := response.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack connection: %v", err)
			return
		}
		_, _ = fmt.Fprint(buffer, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 200\r\n\r\n{\"code\":0")
		_ = buffer.Flush()
		_ = connection.Close()
	}))
	defer server.Close()

	fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
		AppID: "cli_test", AppSecret: "app-secret", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("new approval fetcher: %v", err)
	}
	_, err = fetcher.Fetch(context.Background(), "instance-mid-body-reset", "zh-CN")
	var failure *worker.ApprovalFetchError
	if !errors.As(err, &failure) || failure.Reason != worker.ApprovalFetchTransportError || !failure.Retryable {
		t.Fatalf("failure = %+v err=%v, want retryable transport error", failure, err)
	}
}

func TestApprovalFetcherRejectsPartiallyPopulatedSuccessResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeTestJSON(t, response, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case "/open-apis/approval/v4/instances/instance-partial":
			writeTestJSON(t, response, map[string]any{
				"code": 0, "msg": "ok", "data": map[string]any{
					"approval_code": "approval-wallet-v1",
					"instance_code": "instance-partial",
					"status":        "APPROVED",
					"start_time":    "1787270300000",
					"form":          `[]`,
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
	_, err = fetcher.Fetch(context.Background(), "instance-partial", "zh-CN")
	var failure *worker.ApprovalFetchError
	if !errors.As(err, &failure) || failure.Reason != worker.ApprovalFetchInvalidResponse || failure.Retryable {
		t.Fatalf("failure = %+v err=%v, want terminal invalid response", failure, err)
	}
}

func TestApprovalFetcherListsInstanceCodesWithBoundedWindowAndPagination(t *testing.T) {
	windowStart := time.Date(2026, 8, 23, 1, 2, 3, 456_000_000, time.UTC)
	windowEnd := windowStart.Add(10 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeTestJSON(t, response, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		case "/open-apis/approval/v4/instances":
			if request.Header.Get("Authorization") != "Bearer tenant-token" {
				t.Fatalf("authorization = %q, want tenant token", request.Header.Get("Authorization"))
			}
			query := request.URL.Query()
			if query.Get("approval_code") != "approval-wallet-v1" ||
				query.Get("start_time") != fmt.Sprint(windowStart.UnixMilli()) ||
				query.Get("end_time") != fmt.Sprint(windowEnd.UnixMilli()) ||
				query.Get("page_token") != "page-1" || query.Get("page_size") != "2" {
				t.Fatalf("unexpected list query: %s", request.URL.RawQuery)
			}
			writeTestJSON(t, response, map[string]any{
				"code": 0, "msg": "ok", "data": map[string]any{
					"instance_code_list": []string{"instance-001", "instance-002"},
					"page_token":         "page-2",
					"has_more":           true,
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
	page, err := fetcher.ListInstanceCodes(
		context.Background(), "approval-wallet-v1", windowStart, windowEnd, "page-1", 2,
	)
	if err != nil {
		t.Fatalf("list approval instances: %v", err)
	}
	if fmt.Sprint(page.InstanceCodes) != "[instance-001 instance-002]" ||
		page.NextPageToken != "page-2" || !page.HasMore {
		t.Fatalf("unexpected approval instance page: %+v", page)
	}
}

func TestApprovalFetcherRejectsMalformedInstanceListResponses(t *testing.T) {
	tests := []struct {
		name string
		data any
	}{
		{name: "missing data"},
		{name: "missing has more", data: map[string]any{"instance_code_list": []string{}}},
		{name: "missing next token", data: map[string]any{
			"instance_code_list": []string{"instance-001"}, "has_more": true,
		}},
		{name: "invalid terminal token", data: map[string]any{
			"instance_code_list": []string{}, "page_token": " bad ", "has_more": false,
		}},
		{name: "empty instance code", data: map[string]any{
			"instance_code_list": []string{""}, "has_more": false,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
					writeTestJSON(t, response, map[string]any{
						"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
					})
					return
				}
				writeTestJSON(t, response, map[string]any{"code": 0, "msg": "ok", "data": test.data})
			}))
			defer server.Close()
			fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
				AppID: "cli_test", AppSecret: "app-secret", BaseURL: server.URL,
			})
			if err != nil {
				t.Fatalf("new approval fetcher: %v", err)
			}
			_, err = fetcher.ListInstanceCodes(
				context.Background(),
				"approval-wallet-v1",
				time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC),
				time.Date(2026, 8, 23, 1, 0, 0, 0, time.UTC),
				"",
				100,
			)
			var failure *worker.ApprovalFetchError
			if !errors.As(err, &failure) || failure.Reason != worker.ApprovalFetchInvalidResponse || failure.Retryable {
				t.Fatalf("failure = %+v err=%v, want terminal invalid response", failure, err)
			}
		})
	}
}

func TestApprovalFetcherRejectsInvalidInstanceListRequestBeforeNetwork(t *testing.T) {
	fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
		AppID: "cli_test", AppSecret: "app-secret", BaseURL: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("new approval fetcher: %v", err)
	}
	start := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	if _, err := fetcher.ListInstanceCodes(
		context.Background(), "approval-wallet-v1", start, start.Add(10*time.Hour+time.Millisecond), "", 100,
	); err == nil || !strings.Contains(err.Error(), "invalid approval instance list request") {
		t.Fatalf("oversized list window error = %v", err)
	}
}

func TestApprovalFetcherClassifiesInstanceListBusinessRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal" {
			writeTestJSON(t, response, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
			return
		}
		response.Header().Set("Retry-After", "17")
		writeTestJSON(t, response, map[string]any{"code": 99991400, "msg": "rate limited"})
	}))
	defer server.Close()
	fetcher, err := larkapi.NewApprovalFetcher(larkapi.Config{
		AppID: "cli_test", AppSecret: "app-secret", BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("new approval fetcher: %v", err)
	}
	start := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	_, err = fetcher.ListInstanceCodes(
		context.Background(), "approval-wallet-v1", start, start.Add(time.Hour), "", 100,
	)
	var failure *worker.ApprovalFetchError
	if !errors.As(err, &failure) || failure.Reason != worker.ApprovalFetchRateLimited ||
		!failure.Retryable || failure.RetryAfter != 17*time.Second {
		t.Fatalf("list rate-limit failure = %+v err=%v", failure, err)
	}
}

func writeTestJSON(t *testing.T, response http.ResponseWriter, payload any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(payload); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
