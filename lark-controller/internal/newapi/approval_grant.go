package newapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

type ApprovalGrantInput struct {
	TenantKey             string
	OpenID                string
	PolicyVersion         string
	ApprovalKind          string
	BusinessCode          string
	QuotaDelta            int64
	MonthlyQuota          int64
	ApprovalCode          string
	InstanceCode          string
	StartTimeMilliseconds string
	SchemaFingerprint     string
	Locale                string
	CatalogSHA256         string
}

type ShadowReceipt struct {
	ExternalID    string
	RequestSHA256 string
	SubjectSHA256 string
	PolicyVersion string
	CatalogSHA256 string
	GrantType     string
	BusinessCode  string
	QuotaDelta    int64
	MonthlyQuota  int64
}

func PlanApprovalGrant(input ApprovalGrantInput) (EntitlementGrantRequest, ShadowReceipt, error) {
	if input.TenantKey == "" || input.OpenID == "" || input.PolicyVersion == "" ||
		input.BusinessCode == "" || input.ApprovalCode == "" || input.InstanceCode == "" ||
		input.StartTimeMilliseconds == "" || input.SchemaFingerprint == "" ||
		input.Locale == "" || input.CatalogSHA256 == "" {
		return EntitlementGrantRequest{}, ShadowReceipt{}, errors.New("incomplete approval grant input")
	}
	startedAtMilliseconds, err := strconv.ParseInt(input.StartTimeMilliseconds, 10, 64)
	if err != nil || startedAtMilliseconds <= 0 {
		return EntitlementGrantRequest{}, ShadowReceipt{}, errors.New("invalid approval start time")
	}
	subject := input.TenantKey + ":" + input.OpenID
	request := EntitlementGrantRequest{
		Source:        "lark_approval",
		PolicyVersion: input.PolicyVersion,
		Identity:      Identity{ProviderSlug: "lark", Subject: subject},
		Evidence: &Evidence{
			ApprovalCode: input.ApprovalCode, InstanceCode: input.InstanceCode,
			InstanceStartedAt: time.UnixMilli(startedAtMilliseconds).UTC().Format(time.RFC3339Nano),
			SchemaFingerprint: input.SchemaFingerprint, Locale: input.Locale,
		},
	}
	receipt := ShadowReceipt{
		PolicyVersion: input.PolicyVersion, CatalogSHA256: input.CatalogSHA256,
		BusinessCode: input.BusinessCode, QuotaDelta: input.QuotaDelta,
		MonthlyQuota: input.MonthlyQuota, SubjectSHA256: sha256Hex([]byte(subject)),
	}
	switch input.ApprovalKind {
	case "wallet_topup":
		if input.QuotaDelta <= 0 || input.MonthlyQuota != 0 {
			return EntitlementGrantRequest{}, ShadowReceipt{}, errors.New("invalid wallet approval grant input")
		}
		request.ExternalID = "lark:wallet-topup:" + input.InstanceCode
		request.Grant = Grant{
			Type: "wallet_quota", PackageCode: input.BusinessCode, QuotaDelta: input.QuotaDelta,
		}
	case "subscription_level":
		if input.MonthlyQuota <= 0 || input.QuotaDelta != 0 {
			return EntitlementGrantRequest{}, ShadowReceipt{}, errors.New("invalid subscription approval grant input")
		}
		request.ExternalID = "lark:subscription-level:" + input.InstanceCode
		request.Grant = Grant{
			Type: "subscription_level", LevelCode: input.BusinessCode, MinimumRankOnly: true,
		}
	default:
		return EntitlementGrantRequest{}, ShadowReceipt{}, errors.New("unsupported approval grant kind")
	}
	if err := validateGrantRequest(request); err != nil {
		return EntitlementGrantRequest{}, ShadowReceipt{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return EntitlementGrantRequest{}, ShadowReceipt{}, err
	}
	receipt.ExternalID = request.ExternalID
	receipt.RequestSHA256 = sha256Hex(payload)
	receipt.GrantType = request.Grant.Type
	return request, receipt, nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
