package inbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/digest"
)

type BaseSubscriptionGrantDraft struct {
	ExternalID    string
	RequestSHA256 string
	SubjectSHA256 string
	PolicyVersion string
	CatalogSHA256 string
	LevelCode     string
	MonthlyQuota  int64
	GrantJob      EntitlementGrantJobDraft
}

type entitlementGrantPolicyQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) ValidateActiveBaseGrantPolicy(
	ctx context.Context,
	activePolicyVersion string,
) error {
	if s == nil || s.database == nil {
		return errors.New("entitlement grant store is required")
	}
	return validateActiveBaseGrantPolicy(ctx, s.database, activePolicyVersion)
}

func validateActiveBaseGrantPolicy(
	ctx context.Context,
	queryer entitlementGrantPolicyQueryer,
	activePolicyVersion string,
) error {
	if queryer == nil || activePolicyVersion == "" {
		return errors.New("active base policy version is required")
	}
	var unfinishedHistoricalGrant int
	if err := queryer.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM entitlement_grant_jobs grant_job
    JOIN base_subscription_grants base_grant ON base_grant.external_id = grant_job.external_id
    WHERE base_grant.policy_version != ? AND grant_job.status IN (?, ?, ?, ?)
)`,
		activePolicyVersion,
		EntitlementGrantJobStatusHeldShadow,
		EntitlementGrantJobStatusPending,
		EntitlementGrantJobStatusProcessing,
		EntitlementGrantJobStatusRetryWait,
	).Scan(&unfinishedHistoricalGrant); err != nil {
		return fmt.Errorf("validate active base grant policy: %w", err)
	}
	if unfinishedHistoricalGrant != 0 {
		return errors.New("non-active policy has unfinished base subscription grant jobs")
	}
	return nil
}

func (s *Store) ConsumeOAuthAccessHandleAndStoreBaseGrant(
	ctx context.Context,
	raw string,
	plan func(OAuthIdentity) (BaseSubscriptionGrantDraft, error),
) (OAuthIdentity, error) {
	digest, valid := hashOAuthCredential(raw)
	if s == nil || s.database == nil || !valid {
		return OAuthIdentity{}, ErrOAuthCredentialInvalid
	}
	if plan == nil {
		return OAuthIdentity{}, errors.New("base subscription grant planner is required")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return OAuthIdentity{}, fmt.Errorf("begin OAuth access handle consumption: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.currentTime()
	var identity OAuthIdentity
	err = tx.QueryRowContext(ctx, `
DELETE FROM oauth_access_handles
WHERE handle_hash = ? AND consumed_at = '' AND expires_at > ?
RETURNING subject, username, display_name`,
		digest[:],
		now.UnixNano(),
	).Scan(&identity.Subject, &identity.Username, &identity.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthIdentity{}, ErrOAuthCredentialInvalid
	}
	if err != nil {
		return OAuthIdentity{}, fmt.Errorf("consume OAuth access handle: %w", err)
	}
	draft, err := plan(identity)
	if err != nil {
		return OAuthIdentity{}, fmt.Errorf("plan base subscription grant: %w", err)
	}
	if !validBaseSubscriptionGrantDraft(identity, draft) {
		return OAuthIdentity{}, errors.New("invalid base subscription grant")
	}
	createdAt := now.Format(time.RFC3339Nano)
	outcome, err := insertBaseSubscriptionGrant(ctx, tx, draft, createdAt)
	if err != nil {
		return OAuthIdentity{}, err
	}
	if _, err := insertEntitlementGrantJob(ctx, tx, draft.GrantJob, createdAt); err != nil {
		return OAuthIdentity{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO base_subscription_audit (external_id, action, outcome, created_at)
VALUES (?, 'new_api_grant', ?, ?)`, draft.ExternalID, outcome, createdAt); err != nil {
		return OAuthIdentity{}, fmt.Errorf("store base subscription audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OAuthIdentity{}, fmt.Errorf("commit OAuth access handle consumption: %w", err)
	}
	s.releaseOAuthCredential(oauthCredentialAccessHandle)
	return identity, nil
}

func validBaseSubscriptionGrantDraft(identity OAuthIdentity, draft BaseSubscriptionGrantDraft) bool {
	wantExternalID := "lark:base:" + identity.Subject + ":" + draft.PolicyVersion
	subjectDigest := sha256.Sum256([]byte(identity.Subject))
	wantSubjectSHA256 := hex.EncodeToString(subjectDigest[:])
	return validOAuthIdentity(identity) && draft.ExternalID == wantExternalID &&
		digest.IsCanonicalSHA256(draft.RequestSHA256) &&
		digest.IsCanonicalSHA256(draft.SubjectSHA256) &&
		draft.SubjectSHA256 == wantSubjectSHA256 &&
		digest.IsCanonicalSHA256(draft.CatalogSHA256) && draft.PolicyVersion != "" &&
		draft.LevelCode == "basic" && draft.MonthlyQuota > 0 &&
		draft.GrantJob.ExternalID == draft.ExternalID &&
		draft.GrantJob.RequestSHA256 == draft.RequestSHA256 &&
		draft.GrantJob.SubjectSHA256 == draft.SubjectSHA256
}

func insertBaseSubscriptionGrant(
	ctx context.Context,
	tx *sql.Tx,
	draft BaseSubscriptionGrantDraft,
	createdAt string,
) (string, error) {
	var stored BaseSubscriptionGrantDraft
	err := tx.QueryRowContext(ctx, `
SELECT request_sha256, subject_sha256, policy_version, catalog_sha256, level_code, monthly_quota
FROM base_subscription_grants WHERE external_id = ?`,
		draft.ExternalID,
	).Scan(
		&stored.RequestSHA256,
		&stored.SubjectSHA256,
		&stored.PolicyVersion,
		&stored.CatalogSHA256,
		&stored.LevelCode,
		&stored.MonthlyQuota,
	)
	switch {
	case err == nil && sameBaseSubscriptionGrantMetadata(stored, draft):
		return EntitlementCommandOutcomeShadowReplayed, nil
	case err == nil:
		return "", ErrEntitlementCommandPayloadMismatch
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("inspect base subscription grant replay: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO base_subscription_grants (
    external_id, request_sha256, subject_sha256, policy_version,
    catalog_sha256, level_code, monthly_quota, outcome, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		draft.ExternalID, draft.RequestSHA256, draft.SubjectSHA256,
		draft.PolicyVersion, draft.CatalogSHA256, draft.LevelCode,
		draft.MonthlyQuota, EntitlementCommandOutcomeShadowPlanned, createdAt,
	); err != nil {
		return "", fmt.Errorf("store base subscription grant: %w", err)
	}
	return EntitlementCommandOutcomeShadowPlanned, nil
}

func sameBaseSubscriptionGrantMetadata(stored, draft BaseSubscriptionGrantDraft) bool {
	return stored.RequestSHA256 == draft.RequestSHA256 &&
		stored.SubjectSHA256 == draft.SubjectSHA256 &&
		stored.PolicyVersion == draft.PolicyVersion &&
		stored.CatalogSHA256 == draft.CatalogSHA256 &&
		stored.LevelCode == draft.LevelCode &&
		stored.MonthlyQuota == draft.MonthlyQuota
}
