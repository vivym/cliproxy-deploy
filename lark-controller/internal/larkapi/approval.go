package larkapi

import (
	"context"
	"errors"
	"fmt"
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
		return worker.ApprovalInstance{}, fmt.Errorf("get Lark approval instance: %w", err)
	}
	if !response.Success() {
		return worker.ApprovalInstance{}, fmt.Errorf("get Lark approval instance failed with code %d", response.Code)
	}
	if response.Data == nil {
		return worker.ApprovalInstance{}, errors.New("get Lark approval instance returned no data")
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

func stringValue(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return *pointer
}
