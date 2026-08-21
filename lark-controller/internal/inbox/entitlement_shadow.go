package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	EntitlementCommandOutcomeShadowPlanned  = "shadow_planned"
	EntitlementCommandOutcomeShadowReplayed = "shadow_replayed"
)

var ErrEntitlementCommandPayloadMismatch = errors.New("entitlement command external ID payload mismatch")

type EntitlementCommandShadow struct {
	EventKey      string
	ExternalID    string
	RequestSHA256 string
	SubjectSHA256 string
	Source        string
	PolicyVersion string
	CatalogSHA256 string
	GrantType     string
	BusinessCode  string
	QuotaDelta    int64
	MonthlyQuota  int64
	Outcome       string
	CreatedAt     time.Time
}

func insertEntitlementCommandShadow(
	ctx context.Context,
	tx *sql.Tx,
	eventKey string,
	decisionOutcome DecisionOutcome,
	command EntitlementCommandShadow,
	createdAt string,
) (string, error) {
	if decisionOutcome != DecisionOutcomeShadowAuthorityVerified || eventKey == "" ||
		command.ExternalID == "" || command.RequestSHA256 == "" || command.SubjectSHA256 == "" ||
		command.Source != "lark_approval" || command.PolicyVersion == "" ||
		command.CatalogSHA256 == "" || command.GrantType == "" || command.BusinessCode == "" {
		return "", errors.New("invalid entitlement command shadow")
	}
	commandOutcome := EntitlementCommandOutcomeShadowPlanned
	var existingRequestSHA256 string
	err := tx.QueryRowContext(ctx, `
SELECT request_sha256 FROM entitlement_command_shadows
WHERE external_id = ? ORDER BY created_at, event_key LIMIT 1`, command.ExternalID).Scan(&existingRequestSHA256)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return "", fmt.Errorf("inspect entitlement command shadow replay: %w", err)
	case existingRequestSHA256 != command.RequestSHA256:
		return "", ErrEntitlementCommandPayloadMismatch
	default:
		commandOutcome = EntitlementCommandOutcomeShadowReplayed
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO entitlement_command_shadows (
    event_key, external_id, request_sha256, subject_sha256, source,
    policy_version, catalog_sha256, grant_type, business_code,
    quota_delta, monthly_quota, outcome, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		eventKey, command.ExternalID, command.RequestSHA256, command.SubjectSHA256,
		command.Source, command.PolicyVersion, command.CatalogSHA256,
		command.GrantType, command.BusinessCode, command.QuotaDelta,
		command.MonthlyQuota, commandOutcome, createdAt,
	); err != nil {
		return "", fmt.Errorf("store entitlement command shadow: %w", err)
	}
	return commandOutcome, nil
}

func (s *Store) GetEntitlementCommandShadow(
	ctx context.Context,
	eventKey string,
) (EntitlementCommandShadow, error) {
	if eventKey == "" {
		return EntitlementCommandShadow{}, errors.New("event key is required")
	}
	var command EntitlementCommandShadow
	var createdAt string
	err := s.database.QueryRowContext(ctx, `
SELECT event_key, external_id, request_sha256, subject_sha256, source,
       policy_version, catalog_sha256, grant_type, business_code,
       quota_delta, monthly_quota, outcome, created_at
FROM entitlement_command_shadows WHERE event_key = ?`, eventKey).Scan(
		&command.EventKey, &command.ExternalID, &command.RequestSHA256,
		&command.SubjectSHA256, &command.Source, &command.PolicyVersion,
		&command.CatalogSHA256, &command.GrantType, &command.BusinessCode,
		&command.QuotaDelta, &command.MonthlyQuota, &command.Outcome, &createdAt,
	)
	if err != nil {
		return EntitlementCommandShadow{}, err
	}
	command.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return EntitlementCommandShadow{}, fmt.Errorf("parse entitlement command shadow created_at: %w", err)
	}
	return command, nil
}
