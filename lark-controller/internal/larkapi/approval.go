package larkapi

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkapproval "github.com/larksuite/oapi-sdk-go/v3/service/approval/v4"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/worker"
)

type Config struct {
	AppID     string
	AppSecret string
	BaseURL   string
	Timeout   time.Duration
}

type ApprovalFetcher struct {
	client *lark.Client
}

type classifiedHTTPClient struct {
	delegate larkcore.HttpClient
}

const larkRateLimitCode = 99991400

func NewApprovalFetcher(config Config) (*ApprovalFetcher, error) {
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
		lark.WithHttpClient(&classifiedHTTPClient{delegate: &http.Client{Timeout: config.Timeout}}),
		lark.WithLogLevel(larkcore.LogLevelWarn),
		lark.WithLogReqAtDebug(false),
	}
	if config.BaseURL != "" {
		options = append(options, lark.WithOpenBaseUrl(config.BaseURL))
	}
	return &ApprovalFetcher{client: lark.NewClient(config.AppID, config.AppSecret, options...)}, nil
}

func (f *ApprovalFetcher) Fetch(ctx context.Context, instanceCode, locale string) (worker.ApprovalInstance, error) {
	if instanceCode == "" || locale == "" {
		return worker.ApprovalInstance{}, errors.New("instance code and locale are required")
	}
	request := larkapproval.NewGetInstanceReqBuilder().
		InstanceId(instanceCode).
		Locale(locale).
		Build()
	response, err := f.client.Approval.Instance.Get(ctx, request)
	if err != nil {
		return worker.ApprovalInstance{}, classifyRequestFailure(ctx, err)
	}
	if !response.Success() {
		return worker.ApprovalInstance{}, classifyResponseFailure(response)
	}
	if response.Data == nil ||
		stringValue(response.Data.ApprovalCode) == "" ||
		stringValue(response.Data.InstanceCode) == "" ||
		stringValue(response.Data.Status) == "" ||
		stringValue(response.Data.OpenId) == "" ||
		stringValue(response.Data.StartTime) == "" ||
		stringValue(response.Data.Form) == "" {
		return worker.ApprovalInstance{}, &worker.ApprovalFetchError{
			Reason: worker.ApprovalFetchInvalidResponse,
		}
	}
	return worker.ApprovalInstance{
		ApprovalCode: stringValue(response.Data.ApprovalCode),
		InstanceCode: stringValue(response.Data.InstanceCode),
		Status:       stringValue(response.Data.Status),
		OpenID:       stringValue(response.Data.OpenId),
		StartTime:    stringValue(response.Data.StartTime),
		FormJSON:     stringValue(response.Data.Form),
		Reverted:     response.Data.Reverted != nil && *response.Data.Reverted,
	}, nil
}

func (c *classifiedHTTPClient) Do(request *http.Request) (*http.Response, error) {
	response, err := c.delegate.Do(request)
	if err != nil {
		if request.Context().Err() != nil {
			return nil, request.Context().Err()
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return nil, &worker.ApprovalFetchError{
				Reason: worker.ApprovalFetchTimeout, Retryable: true,
			}
		}
		return nil, &worker.ApprovalFetchError{
			Reason: worker.ApprovalFetchTransportError, Retryable: true,
		}
	}
	reason, retryable := classifyHTTPStatus(response.StatusCode)
	if !retryable {
		return response, nil
	}
	retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
	if response.Body != nil {
		_ = response.Body.Close()
	}
	return nil, &worker.ApprovalFetchError{
		Reason: reason, Retryable: true, RetryAfter: retryAfter, StatusCode: response.StatusCode,
	}
}

func classifyRequestFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var classified *worker.ApprovalFetchError
	if errors.As(err, &classified) && classified != nil {
		return classified
	}
	var clientTimeout *larkcore.ClientTimeoutError
	var serverTimeout *larkcore.ServerTimeoutError
	if errors.As(err, &clientTimeout) || errors.As(err, &serverTimeout) {
		return &worker.ApprovalFetchError{
			Reason: worker.ApprovalFetchTimeout, Retryable: true,
		}
	}
	var dialFailure *larkcore.DialFailedError
	if errors.As(err, &dialFailure) {
		return &worker.ApprovalFetchError{
			Reason: worker.ApprovalFetchTransportError, Retryable: true,
		}
	}
	var codeFailure *larkcore.CodeError
	if errors.As(err, &codeFailure) {
		return classifyCodeFailure(codeFailure.Code)
	}
	var codeFailureValue larkcore.CodeError
	if errors.As(err, &codeFailureValue) {
		return classifyCodeFailure(codeFailureValue.Code)
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		reason := worker.ApprovalFetchTransportError
		if networkError.Timeout() {
			reason = worker.ApprovalFetchTimeout
		}
		return &worker.ApprovalFetchError{Reason: reason, Retryable: true}
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return &worker.ApprovalFetchError{
			Reason: worker.ApprovalFetchTransportError, Retryable: true,
		}
	}
	return &worker.ApprovalFetchError{Reason: worker.ApprovalFetchInvalidResponse}
}

func classifyCodeFailure(code int) error {
	if code == larkRateLimitCode {
		return &worker.ApprovalFetchError{
			Reason: worker.ApprovalFetchRateLimited, Retryable: true, LarkCode: code,
		}
	}
	return &worker.ApprovalFetchError{
		Reason: worker.ApprovalFetchClientError, LarkCode: code,
	}
}

func classifyResponseFailure(response *larkapproval.GetInstanceResp) error {
	statusCode := 0
	retryAfter := time.Duration(0)
	if response.ApiResp != nil {
		statusCode = response.StatusCode
		retryAfter = parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
	}
	reason := worker.ApprovalFetchClientError
	retryable := false
	if response.Code == larkRateLimitCode {
		reason = worker.ApprovalFetchRateLimited
		retryable = true
	} else if httpReason, httpRetryable := classifyHTTPStatus(statusCode); httpRetryable {
		reason = httpReason
		retryable = true
	}
	return &worker.ApprovalFetchError{
		Reason: reason, Retryable: retryable, RetryAfter: retryAfter,
		StatusCode: statusCode, LarkCode: response.Code,
	}
}

func classifyHTTPStatus(statusCode int) (worker.ApprovalFetchFailureReason, bool) {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return worker.ApprovalFetchRateLimited, true
	case statusCode == http.StatusRequestTimeout:
		return worker.ApprovalFetchTimeout, true
	case statusCode >= http.StatusInternalServerError:
		return worker.ApprovalFetchServerError, true
	default:
		return "", false
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		const maxDuration = time.Duration(1<<63 - 1)
		if seconds > int64(maxDuration/time.Second) {
			return maxDuration
		}
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}

func stringValue(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return *pointer
}
