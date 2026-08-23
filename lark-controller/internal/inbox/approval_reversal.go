package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrApprovalReverted = errors.New("approval instance already authoritatively reverted")

type ApprovalReversalResult string

const (
	ApprovalReversalResultGrantFenced         ApprovalReversalResult = "grant_fenced"
	ApprovalReversalResultGrantAlreadyPending ApprovalReversalResult = "grant_already_pending"
	ApprovalReversalResultGrantTerminal       ApprovalReversalResult = "grant_terminal"
	ApprovalReversalResultGrantStatusUnknown  ApprovalReversalResult = "grant_status_unknown"
	ApprovalReversalResultGrantJobMissing     ApprovalReversalResult = "grant_job_missing"
	ApprovalReversalResultOriginalMissing     ApprovalReversalResult = "original_missing"
	ApprovalReversalResultOriginalAmbiguous   ApprovalReversalResult = "original_ambiguous"
	ApprovalReversalResultAuthorityMismatch   ApprovalReversalResult = "authority_mismatch"
	ApprovalReversalResultFetchTerminalError  ApprovalReversalResult = "fetch_terminal_error"
	ApprovalReversalResultFetchRetryExhausted ApprovalReversalResult = "fetch_retry_exhausted"
)

type ApprovalReversalReason string

const (
	ApprovalReversalReasonManualReviewRequired ApprovalReversalReason = "manual_review_required"
	ApprovalReversalReasonOriginalMissing      ApprovalReversalReason = "original_missing"
	ApprovalReversalReasonOriginalAmbiguous    ApprovalReversalReason = "original_ambiguous"
	ApprovalReversalReasonGrantJobMissing      ApprovalReversalReason = "grant_job_missing"
	ApprovalReversalReasonGrantStatusUnknown   ApprovalReversalReason = "grant_status_unknown"
	ApprovalReversalReasonAuthorityMismatch    ApprovalReversalReason = "authority_mismatch"
	ApprovalReversalReasonTargetMissing        ApprovalReversalReason = "target_missing"
	ApprovalReversalReasonApprovalCodeMissing  ApprovalReversalReason = "approval_code_missing"
)

type ApprovalReversalDraft struct {
	TargetInstanceCode    string
	AuthorityApprovalCode string
	AuthorityInstanceCode string
	AuthorityStatus       string
	AuthorityReverted     bool
	Result                ApprovalReversalResult
	Reason                ApprovalReversalReason
}

type ApprovalReversal struct {
	EventKey              string
	ApprovalCode          string
	TargetInstanceCode    string
	AuthorityApprovalCode string
	AuthorityInstanceCode string
	AuthorityStatus       string
	AuthorityReverted     bool
	OriginalExternalID    string
	OriginalGrantStatus   EntitlementGrantJobStatus
	OriginalGrantType     string
	OriginalQuotaDelta    int64
	OriginalMonthlyQuota  int64
	OriginalPolicyVersion string
	OriginalBusinessCode  string
	Result                ApprovalReversalResult
	Reason                ApprovalReversalReason
	CreatedAt             time.Time
}

func prepareApprovalReversal(
	ctx context.Context,
	tx *sql.Tx,
	event Event,
	decision Decision,
	draft ApprovalReversalDraft,
	createdAt string,
) (ApprovalReversal, error) {
	reversal := ApprovalReversal{
		EventKey: decision.EventKey, ApprovalCode: decision.ApprovalCode,
		TargetInstanceCode:    draft.TargetInstanceCode,
		AuthorityApprovalCode: draft.AuthorityApprovalCode,
		AuthorityInstanceCode: draft.AuthorityInstanceCode,
		AuthorityStatus:       draft.AuthorityStatus,
		AuthorityReverted:     draft.AuthorityReverted,
		Result:                draft.Result, Reason: draft.Reason,
	}
	if event.TenantKey == "" || event.Status != "REVERTED" ||
		decision.EventStatus != event.Status || decision.ApprovalCode != event.ApprovalCode ||
		decision.InstanceCode != draft.TargetInstanceCode ||
		draft.AuthorityStatus != decision.AuthorityStatus ||
		draft.AuthorityReverted != decision.Reverted {
		return ApprovalReversal{}, errors.New("invalid approval reversal evidence")
	}
	if draft.Result != "" {
		if !validPreResolvedApprovalReversal(draft.Result, draft.Reason) {
			return ApprovalReversal{}, errors.New("invalid pre-resolved approval reversal")
		}
		return reversal, nil
	}
	if decision.ApprovalCode == "" || draft.TargetInstanceCode == "" ||
		draft.AuthorityApprovalCode != decision.ApprovalCode ||
		draft.AuthorityInstanceCode != draft.TargetInstanceCode ||
		draft.AuthorityStatus == "" || !draft.AuthorityReverted {
		return ApprovalReversal{}, errors.New("verified approval reversal must be reverted")
	}

	rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT command.external_id
FROM approval_instances decision
JOIN entitlement_command_shadows command ON command.event_key = decision.event_key
JOIN lark_event_inbox event ON event.event_key = decision.event_key
WHERE event.tenant_key = ? AND decision.approval_code = ?
  AND decision.instance_code = ? AND decision.outcome = ?
ORDER BY command.external_id
LIMIT 2`,
		event.TenantKey,
		decision.ApprovalCode,
		draft.TargetInstanceCode,
		DecisionOutcomeShadowAuthorityVerified,
	)
	if err != nil {
		return ApprovalReversal{}, fmt.Errorf("resolve original approval grant: %w", err)
	}
	externalIDs := make([]string, 0, 2)
	for rows.Next() {
		var externalID string
		if err := rows.Scan(&externalID); err != nil {
			_ = rows.Close()
			return ApprovalReversal{}, fmt.Errorf("scan original approval grant: %w", err)
		}
		externalIDs = append(externalIDs, externalID)
	}
	if err := rows.Close(); err != nil {
		return ApprovalReversal{}, fmt.Errorf("close original approval grant rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return ApprovalReversal{}, fmt.Errorf("iterate original approval grants: %w", err)
	}
	switch len(externalIDs) {
	case 0:
		reversal.Result = ApprovalReversalResultOriginalMissing
		reversal.Reason = ApprovalReversalReasonOriginalMissing
		return reversal, nil
	case 2:
		reversal.Result = ApprovalReversalResultOriginalAmbiguous
		reversal.Reason = ApprovalReversalReasonOriginalAmbiguous
		return reversal, nil
	}
	reversal.OriginalExternalID = externalIDs[0]
	if err := tx.QueryRowContext(ctx, `
SELECT grant_type, quota_delta, monthly_quota, policy_version, business_code
FROM entitlement_command_shadows
WHERE external_id = ?
ORDER BY created_at, event_key
LIMIT 1`, reversal.OriginalExternalID).Scan(
		&reversal.OriginalGrantType,
		&reversal.OriginalQuotaDelta,
		&reversal.OriginalMonthlyQuota,
		&reversal.OriginalPolicyVersion,
		&reversal.OriginalBusinessCode,
	); err != nil {
		return ApprovalReversal{}, fmt.Errorf("read original entitlement grant evidence: %w", err)
	}
	var status EntitlementGrantJobStatus
	if err := tx.QueryRowContext(ctx,
		"SELECT status FROM entitlement_grant_jobs WHERE external_id = ?",
		reversal.OriginalExternalID,
	).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			reversal.Result = ApprovalReversalResultGrantJobMissing
			reversal.Reason = ApprovalReversalReasonGrantJobMissing
			return reversal, nil
		}
		return ApprovalReversal{}, fmt.Errorf("inspect original entitlement grant job: %w", err)
	}
	reversal.OriginalGrantStatus = status
	switch status {
	case EntitlementGrantJobStatusHeldShadow,
		EntitlementGrantJobStatusPending,
		EntitlementGrantJobStatusProcessing,
		EntitlementGrantJobStatusRetryWait:
		result, err := tx.ExecContext(ctx, `
UPDATE entitlement_grant_jobs
SET status = ?, last_error = 'approval_reverted', updated_at = ?
WHERE external_id = ? AND status = ?`,
			EntitlementGrantJobStatusReversalPending,
			createdAt,
			reversal.OriginalExternalID,
			status,
		)
		if err != nil {
			return ApprovalReversal{}, fmt.Errorf("fence reverted entitlement grant job: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 1 {
			return ApprovalReversal{}, fmt.Errorf("fence reverted entitlement grant job affected %d rows: %w", affected, err)
		}
		reversal.Result = ApprovalReversalResultGrantFenced
		reversal.Reason = ApprovalReversalReasonManualReviewRequired
	case EntitlementGrantJobStatusReversalPending:
		reversal.Result = ApprovalReversalResultGrantAlreadyPending
		reversal.Reason = ApprovalReversalReasonManualReviewRequired
	case EntitlementGrantJobStatusSucceeded, EntitlementGrantJobStatusDeadLetter:
		reversal.Result = ApprovalReversalResultGrantTerminal
		reversal.Reason = ApprovalReversalReasonManualReviewRequired
	default:
		reversal.Result = ApprovalReversalResultGrantStatusUnknown
		reversal.Reason = ApprovalReversalReasonGrantStatusUnknown
	}
	return reversal, nil
}

func validPreResolvedApprovalReversal(
	result ApprovalReversalResult,
	reason ApprovalReversalReason,
) bool {
	switch result {
	case ApprovalReversalResultAuthorityMismatch:
		switch reason {
		case ApprovalReversalReasonAuthorityMismatch,
			ApprovalReversalReasonTargetMissing,
			ApprovalReversalReasonApprovalCodeMissing:
			return true
		}
	case ApprovalReversalResultFetchTerminalError:
		switch reason {
		case "rate_limited",
			"server_error",
			"client_error",
			"timeout",
			"transport_error",
			"invalid_response",
			"unclassified_error":
			return true
		}
	case ApprovalReversalResultFetchRetryExhausted:
		switch reason {
		case "retry_exhausted_rate_limited",
			"retry_exhausted_server_error",
			"retry_exhausted_client_error",
			"retry_exhausted_timeout",
			"retry_exhausted_transport_error",
			"retry_exhausted_invalid_response":
			return true
		}
	default:
	}
	return false
}

func rejectEntitlementCommandAfterReversal(
	ctx context.Context,
	tx *sql.Tx,
	event Event,
	decision Decision,
) error {
	var reverted int
	if err := tx.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM approval_reversals reversal
    JOIN lark_event_inbox reversal_event ON reversal_event.event_key = reversal.event_key
    WHERE reversal_event.tenant_key = ?
      AND reversal.approval_code = ?
      AND reversal.target_instance_code = ?
      AND reversal.authority_approval_code = ?
      AND reversal.authority_instance_code = ?
      AND reversal.authority_reverted = 1
)`,
		event.TenantKey,
		decision.ApprovalCode,
		decision.InstanceCode,
		decision.ApprovalCode,
		decision.InstanceCode,
	).Scan(&reverted); err != nil {
		return fmt.Errorf("check authoritative reversal fence: %w", err)
	}
	if reverted != 0 {
		return ErrApprovalReverted
	}
	return nil
}

func insertApprovalReversal(
	ctx context.Context,
	tx *sql.Tx,
	reversal ApprovalReversal,
	createdAt string,
) error {
	if reversal.EventKey == "" || reversal.Result == "" || reversal.Reason == "" {
		return errors.New("invalid approval reversal")
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO approval_reversals (
    event_key, approval_code, target_instance_code, authority_approval_code,
    authority_instance_code, authority_status, authority_reverted,
    original_external_id, original_grant_status,
    original_grant_type, original_quota_delta, original_monthly_quota,
    original_policy_version, original_business_code, result, reason, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		reversal.EventKey,
		reversal.ApprovalCode,
		reversal.TargetInstanceCode,
		reversal.AuthorityApprovalCode,
		reversal.AuthorityInstanceCode,
		reversal.AuthorityStatus,
		reversal.AuthorityReverted,
		reversal.OriginalExternalID,
		reversal.OriginalGrantStatus,
		reversal.OriginalGrantType,
		reversal.OriginalQuotaDelta,
		reversal.OriginalMonthlyQuota,
		reversal.OriginalPolicyVersion,
		reversal.OriginalBusinessCode,
		reversal.Result,
		reversal.Reason,
		createdAt,
	)
	if err != nil {
		return fmt.Errorf("store approval reversal: %w", err)
	}
	return nil
}

func (s *Store) GetApprovalReversal(ctx context.Context, eventKey string) (ApprovalReversal, error) {
	var reversal ApprovalReversal
	var createdAt string
	err := s.database.QueryRowContext(ctx, `
	SELECT event_key, approval_code, target_instance_code, authority_approval_code,
	       authority_instance_code, authority_status, authority_reverted,
       original_external_id, original_grant_status,
       original_grant_type, original_quota_delta, original_monthly_quota,
       original_policy_version, original_business_code, result, reason, created_at
FROM approval_reversals WHERE event_key = ?`, eventKey).Scan(
		&reversal.EventKey,
		&reversal.ApprovalCode,
		&reversal.TargetInstanceCode,
		&reversal.AuthorityApprovalCode,
		&reversal.AuthorityInstanceCode,
		&reversal.AuthorityStatus,
		&reversal.AuthorityReverted,
		&reversal.OriginalExternalID,
		&reversal.OriginalGrantStatus,
		&reversal.OriginalGrantType,
		&reversal.OriginalQuotaDelta,
		&reversal.OriginalMonthlyQuota,
		&reversal.OriginalPolicyVersion,
		&reversal.OriginalBusinessCode,
		&reversal.Result,
		&reversal.Reason,
		&createdAt,
	)
	if err != nil {
		return ApprovalReversal{}, err
	}
	reversal.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return ApprovalReversal{}, fmt.Errorf("parse approval reversal created_at: %w", err)
	}
	return reversal, nil
}
