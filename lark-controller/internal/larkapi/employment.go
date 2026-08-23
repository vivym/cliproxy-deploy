package larkapi

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkcontact "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

const (
	larkNoUserAuthorityCode = 41050
	larkUserIDInvalidCode   = 41012
	larkInternalErrorCode   = 40003
)

type EmploymentChecker struct {
	client *lark.Client
}

type employmentHTTPClient struct {
	delegate larkcore.HttpClient
}

func NewEmploymentChecker(config Config) (*EmploymentChecker, error) {
	if config.AppID == "" || config.AppSecret == "" {
		return nil, errors.New("Lark app id and app secret are required")
	}
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}
	if config.Timeout < 0 {
		return nil, errors.New("Lark request timeout must be positive")
	}
	options := []lark.ClientOptionFunc{
		lark.WithReqTimeout(config.Timeout),
		lark.WithHttpClient(&employmentHTTPClient{delegate: &http.Client{Timeout: config.Timeout}}),
		lark.WithLogLevel(larkcore.LogLevelWarn),
		lark.WithLogReqAtDebug(false),
	}
	if config.BaseURL != "" {
		options = append(options, lark.WithOpenBaseUrl(config.BaseURL))
	}
	return &EmploymentChecker{client: lark.NewClient(config.AppID, config.AppSecret, options...)}, nil
}

func (c *EmploymentChecker) CheckEmployment(
	ctx context.Context,
	openID string,
) (worker.EmploymentCheckResult, error) {
	if openID == "" {
		return worker.EmploymentCheckResult{}, &worker.EmploymentCheckError{
			Reason: worker.EmploymentCheckClientError,
		}
	}
	request := larkcontact.NewGetUserReqBuilder().
		UserId(openID).
		UserIdType("open_id").
		Build()
	response, err := c.client.Contact.User.Get(ctx, request)
	if err != nil {
		if code, ok := employmentLarkCode(err); ok && code == larkUserIDInvalidCode {
			return worker.EmploymentCheckResult{
				Status: worker.EmploymentStatusNotFound, LarkResultCode: code,
			}, nil
		}
		return worker.EmploymentCheckResult{}, classifyEmploymentRequestFailure(ctx, err)
	}
	if !response.Success() {
		switch response.Code {
		case larkUserIDInvalidCode:
			return worker.EmploymentCheckResult{
				Status: worker.EmploymentStatusNotFound, LarkResultCode: response.Code,
			}, nil
		case larkNoUserAuthorityCode:
			return worker.EmploymentCheckResult{}, &worker.EmploymentCheckError{
				Reason: worker.EmploymentCheckPermissionDenied, LarkCode: response.Code,
				StatusCode: employmentResponseStatus(response),
			}
		case larkRateLimitCode:
			return worker.EmploymentCheckResult{}, &worker.EmploymentCheckError{
				Reason: worker.EmploymentCheckRateLimited, Retryable: true,
				RetryAfter: employmentResponseRetryAfter(response), LarkCode: response.Code,
				StatusCode: employmentResponseStatus(response),
			}
		case larkInternalErrorCode:
			return worker.EmploymentCheckResult{}, &worker.EmploymentCheckError{
				Reason: worker.EmploymentCheckServerError, Retryable: true,
				LarkCode: response.Code, StatusCode: employmentResponseStatus(response),
			}
		default:
			return worker.EmploymentCheckResult{}, &worker.EmploymentCheckError{
				Reason: worker.EmploymentCheckClientError, LarkCode: response.Code,
				StatusCode: employmentResponseStatus(response),
			}
		}
	}
	if response.Data == nil || response.Data.User == nil ||
		stringValue(response.Data.User.OpenId) != openID || response.Data.User.Status == nil ||
		response.Data.User.Status.IsResigned == nil || response.Data.User.Status.IsExited == nil {
		return worker.EmploymentCheckResult{}, &worker.EmploymentCheckError{
			Reason: worker.EmploymentCheckInvalidResponse,
		}
	}
	status := worker.EmploymentStatusPresent
	if *response.Data.User.Status.IsResigned {
		status = worker.EmploymentStatusResigned
	} else if *response.Data.User.Status.IsExited {
		status = worker.EmploymentStatusExited
	}
	return worker.EmploymentCheckResult{Status: status, LarkResultCode: 0}, nil
}

func (c *employmentHTTPClient) Do(request *http.Request) (*http.Response, error) {
	response, err := c.delegate.Do(request)
	if err != nil {
		if request.Context().Err() != nil {
			return nil, request.Context().Err()
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return nil, &worker.EmploymentCheckError{
				Reason: worker.EmploymentCheckTimeout, Retryable: true,
			}
		}
		return nil, &worker.EmploymentCheckError{
			Reason: worker.EmploymentCheckTransportError, Retryable: true,
		}
	}
	reason := worker.EmploymentCheckFailureReason("")
	switch {
	case response.StatusCode == http.StatusTooManyRequests:
		reason = worker.EmploymentCheckRateLimited
	case response.StatusCode == http.StatusRequestTimeout:
		reason = worker.EmploymentCheckTimeout
	case response.StatusCode >= http.StatusInternalServerError && response.StatusCode < 600:
		reason = worker.EmploymentCheckServerError
	}
	if reason == "" {
		return response, nil
	}
	retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
	if response.Body != nil {
		_ = response.Body.Close()
	}
	return nil, &worker.EmploymentCheckError{
		Reason: reason, Retryable: true, RetryAfter: retryAfter, StatusCode: response.StatusCode,
	}
}

func classifyEmploymentRequestFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var classified *worker.EmploymentCheckError
	if errors.As(err, &classified) && classified != nil {
		return classified
	}
	var clientTimeout *larkcore.ClientTimeoutError
	var serverTimeout *larkcore.ServerTimeoutError
	if errors.As(err, &clientTimeout) || errors.As(err, &serverTimeout) {
		return &worker.EmploymentCheckError{
			Reason: worker.EmploymentCheckTimeout, Retryable: true,
		}
	}
	if code, ok := employmentLarkCode(err); ok {
		return classifyEmploymentCodeFailure(code)
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		reason := worker.EmploymentCheckTransportError
		if networkError.Timeout() {
			reason = worker.EmploymentCheckTimeout
		}
		return &worker.EmploymentCheckError{Reason: reason, Retryable: true}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) {
		return &worker.EmploymentCheckError{
			Reason: worker.EmploymentCheckTransportError, Retryable: true,
		}
	}
	return &worker.EmploymentCheckError{Reason: worker.EmploymentCheckInvalidResponse}
}

func employmentLarkCode(err error) (int, bool) {
	var pointer *larkcore.CodeError
	if errors.As(err, &pointer) && pointer != nil {
		return pointer.Code, true
	}
	var value larkcore.CodeError
	if errors.As(err, &value) {
		return value.Code, true
	}
	return 0, false
}

func classifyEmploymentCodeFailure(code int) error {
	switch code {
	case larkNoUserAuthorityCode:
		return &worker.EmploymentCheckError{
			Reason: worker.EmploymentCheckPermissionDenied, LarkCode: code,
		}
	case larkRateLimitCode:
		return &worker.EmploymentCheckError{
			Reason: worker.EmploymentCheckRateLimited, Retryable: true, LarkCode: code,
		}
	case larkInternalErrorCode:
		return &worker.EmploymentCheckError{
			Reason: worker.EmploymentCheckServerError, Retryable: true, LarkCode: code,
		}
	default:
		return &worker.EmploymentCheckError{
			Reason: worker.EmploymentCheckClientError, LarkCode: code,
		}
	}
}

func employmentResponseStatus(response *larkcontact.GetUserResp) int {
	if response == nil || response.ApiResp == nil {
		return 0
	}
	return response.StatusCode
}

func employmentResponseRetryAfter(response *larkcontact.GetUserResp) time.Duration {
	if response == nil || response.ApiResp == nil {
		return 0
	}
	return parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
}
