package larkapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/larkapi"
	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

func TestEmploymentCheckerClassifiesAuthoritativeLarkUserStates(t *testing.T) {
	tests := []struct {
		name   string
		status map[string]bool
		want   worker.EmploymentStatus
	}{
		{name: "present", status: map[string]bool{"is_resigned": false, "is_exited": false}, want: worker.EmploymentStatusPresent},
		{name: "resigned", status: map[string]bool{"is_resigned": true, "is_exited": false}, want: worker.EmploymentStatusResigned},
		{name: "exited", status: map[string]bool{"is_resigned": false, "is_exited": true}, want: worker.EmploymentStatusExited},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newEmploymentLarkServer(t, func(response http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/open-apis/contact/v3/users/ou_employee" ||
					request.URL.Query().Get("user_id_type") != "open_id" {
					t.Fatalf("employment request = %s %s", request.Method, request.URL.String())
				}
				writeEmploymentJSON(t, response, map[string]any{
					"code": 0,
					"data": map[string]any{"user": map[string]any{
						"open_id": "ou_employee", "status": test.status,
					}},
				})
			})
			defer server.Close()
			checker, err := larkapi.NewEmploymentChecker(larkapi.Config{
				AppID: "cli_test", AppSecret: "app-secret", BaseURL: server.URL,
			})
			if err != nil {
				t.Fatalf("new employment checker: %v", err)
			}
			result, err := checker.CheckEmployment(context.Background(), "ou_employee")
			if err != nil || result.Status != test.want || result.LarkResultCode != 0 {
				t.Fatalf("employment result=%+v err=%v, want %s", result, err, test.want)
			}
		})
	}
}

func TestEmploymentCheckerSeparatesNotFoundFromPermissionAndTemporaryFailures(t *testing.T) {
	tests := []struct {
		name       string
		httpStatus int
		larkCode   int
		wantStatus worker.EmploymentStatus
		wantReason worker.EmploymentCheckFailureReason
		retryable  bool
		retryAfter time.Duration
	}{
		{name: "not found", httpStatus: http.StatusBadRequest, larkCode: 41012, wantStatus: worker.EmploymentStatusNotFound},
		{name: "permission", httpStatus: http.StatusBadRequest, larkCode: 41050, wantReason: worker.EmploymentCheckPermissionDenied},
		{name: "rate limited", httpStatus: http.StatusTooManyRequests, wantReason: worker.EmploymentCheckRateLimited, retryable: true},
		{
			name: "business rate limited", httpStatus: http.StatusOK, larkCode: 99991400,
			wantReason: worker.EmploymentCheckRateLimited, retryable: true, retryAfter: 30 * time.Minute,
		},
		{name: "server error", httpStatus: http.StatusBadGateway, wantReason: worker.EmploymentCheckServerError, retryable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newEmploymentLarkServer(t, func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				if test.retryAfter > 0 {
					response.Header().Set("Retry-After", "1800")
				}
				response.WriteHeader(test.httpStatus)
				if err := json.NewEncoder(response).Encode(map[string]any{
					"code": test.larkCode, "msg": "sensitive detail",
				}); err != nil {
					t.Fatalf("write employment failure: %v", err)
				}
			})
			defer server.Close()
			checker, err := larkapi.NewEmploymentChecker(larkapi.Config{
				AppID: "cli_test", AppSecret: "app-secret", BaseURL: server.URL,
			})
			if err != nil {
				t.Fatalf("new employment checker: %v", err)
			}
			result, err := checker.CheckEmployment(context.Background(), "ou_employee")
			if test.wantStatus != "" {
				if err != nil || result.Status != test.wantStatus || result.LarkResultCode != test.larkCode {
					t.Fatalf("employment result=%+v err=%v, want not found", result, err)
				}
				return
			}
			var failure *worker.EmploymentCheckError
			if !errors.As(err, &failure) || failure.Reason != test.wantReason ||
				failure.Retryable != test.retryable || failure.LarkCode != test.larkCode ||
				failure.RetryAfter != test.retryAfter {
				t.Fatalf("employment failure=%+v err=%v", failure, err)
			}
		})
	}
}

func newEmploymentLarkServer(
	t *testing.T,
	employmentHandler http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			writeEmploymentJSON(t, response, map[string]any{
				"code": 0, "msg": "ok", "tenant_access_token": "tenant-token", "expire": 7200,
			})
		default:
			employmentHandler(response, request)
		}
	}))
}

func writeEmploymentJSON(t *testing.T, response http.ResponseWriter, payload any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(payload); err != nil {
		t.Fatalf("write employment JSON: %v", err)
	}
}
