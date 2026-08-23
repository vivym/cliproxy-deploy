package inbox

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/digest"
)

var ErrApprovalReverted = errors.New("approval instance already authoritatively reverted")

var (
	ErrApprovalReversalNotPending         = errors.New("approval reversal is not pending correction")
	ErrApprovalReversalResolutionMismatch = errors.New("approval reversal resolution payload mismatch")
)

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
	OriginalSubjectSHA256 string
	OriginalGrantStatus   EntitlementGrantJobStatus
	OriginalGrantType     string
	OriginalQuotaDelta    int64
	OriginalMonthlyQuota  int64
	OriginalPolicyVersion string
	OriginalBusinessCode  string
	Result                ApprovalReversalResult
	Reason                ApprovalReversalReason
	Resolution            *ApprovalReversalResolution
	CreatedAt             time.Time
}

type ApprovalCorrectionResult struct {
	CorrectionType    string `json:"correction_type"`
	QuotaDelta        int64  `json:"quota_delta,omitempty"`
	WalletQuota       *int64 `json:"wallet_quota,omitempty"`
	LevelCode         string `json:"level_code,omitempty"`
	SubscriptionID    int64  `json:"subscription_id,omitempty"`
	AssignmentVersion int64  `json:"assignment_version,omitempty"`
	Transition        string `json:"transition,omitempty"`
}

type ApprovalReversalResolution struct {
	EventKey                string
	OriginalExternalID      string
	OriginalSubjectSHA256   string
	CorrectionExternalID    string
	CorrectionRequestSHA256 string
	Operator                string
	Reason                  string
	ChangeTicket            string
	ResponseStatus          string
	Result                  ApprovalCorrectionResult
	ResolvedAt              time.Time
	Replayed                bool
}

type ApprovalReversalCorrectionIntent struct {
	EventKey                string
	OriginalExternalID      string
	OriginalSubjectSHA256   string
	CorrectionExternalID    string
	CorrectionRequestSHA256 string
	CorrectionType          string
	Operator                string
	Reason                  string
	ChangeTicket            string
	Status                  ApprovalReversalCorrectionIntentStatus
	FailureCode             string
	ClaimedAt               time.Time
	EndedAt                 time.Time
	Replayed                bool
}

type ApprovalReversalCorrectionIntentStatus string

const (
	ApprovalReversalCorrectionIntentActive         ApprovalReversalCorrectionIntentStatus = "active"
	ApprovalReversalCorrectionIntentAbandoned      ApprovalReversalCorrectionIntentStatus = "abandoned"
	ApprovalReversalCorrectionIntentRemoteConflict ApprovalReversalCorrectionIntentStatus = "remote_conflict"
	ApprovalReversalCorrectionIntentResolved       ApprovalReversalCorrectionIntentStatus = "resolved"
)

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
SELECT subject_sha256, grant_type, quota_delta, monthly_quota, policy_version, business_code
FROM entitlement_command_shadows
WHERE external_id = ?
ORDER BY created_at, event_key
LIMIT 1`, reversal.OriginalExternalID).Scan(
		&reversal.OriginalSubjectSHA256,
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
	case EntitlementGrantJobStatusSucceeded,
		EntitlementGrantJobStatusDeadLetter,
		EntitlementGrantJobStatusReversalResolved:
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
	var resolution ApprovalReversalResolution
	var resolutionResultJSON string
	var resolvedAt string
	err := s.database.QueryRowContext(ctx, `
	SELECT event_key, approval_code, target_instance_code, authority_approval_code,
	       authority_instance_code, authority_status, authority_reverted,
	       original_external_id,
	       COALESCE((
	           SELECT command.subject_sha256
	           FROM entitlement_command_shadows command
	           WHERE command.external_id = approval_reversals.original_external_id
	           ORDER BY command.created_at, command.event_key
	           LIMIT 1
	       ), ''),
	       original_grant_status,
       original_grant_type, original_quota_delta, original_monthly_quota,
       original_policy_version, original_business_code, result, reason,
       resolution_external_id, resolution_request_sha256, resolution_operator,
       resolution_reason, resolution_change_ticket, resolution_response_status,
       resolution_result_json, resolved_at, created_at
FROM approval_reversals WHERE event_key = ?`, eventKey).Scan(
		&reversal.EventKey,
		&reversal.ApprovalCode,
		&reversal.TargetInstanceCode,
		&reversal.AuthorityApprovalCode,
		&reversal.AuthorityInstanceCode,
		&reversal.AuthorityStatus,
		&reversal.AuthorityReverted,
		&reversal.OriginalExternalID,
		&reversal.OriginalSubjectSHA256,
		&reversal.OriginalGrantStatus,
		&reversal.OriginalGrantType,
		&reversal.OriginalQuotaDelta,
		&reversal.OriginalMonthlyQuota,
		&reversal.OriginalPolicyVersion,
		&reversal.OriginalBusinessCode,
		&reversal.Result,
		&reversal.Reason,
		&resolution.CorrectionExternalID,
		&resolution.CorrectionRequestSHA256,
		&resolution.Operator,
		&resolution.Reason,
		&resolution.ChangeTicket,
		&resolution.ResponseStatus,
		&resolutionResultJSON,
		&resolvedAt,
		&createdAt,
	)
	if err != nil {
		return ApprovalReversal{}, err
	}
	reversal.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return ApprovalReversal{}, fmt.Errorf("parse approval reversal created_at: %w", err)
	}
	if resolution.CorrectionExternalID != "" {
		resolution.EventKey = reversal.EventKey
		resolution.OriginalExternalID = reversal.OriginalExternalID
		resolution.OriginalSubjectSHA256 = reversal.OriginalSubjectSHA256
		if err := json.Unmarshal([]byte(resolutionResultJSON), &resolution.Result); err != nil {
			return ApprovalReversal{}, fmt.Errorf("decode approval reversal resolution result: %w", err)
		}
		resolution.ResolvedAt, err = time.Parse(time.RFC3339Nano, resolvedAt)
		if err != nil {
			return ApprovalReversal{}, fmt.Errorf("parse approval reversal resolved_at: %w", err)
		}
		reversal.Resolution = &resolution
	}
	return reversal, nil
}

type approvalReversalRowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type approvalReversalExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type approvalReversalIntentStore interface {
	approvalReversalRowQuerier
	approvalReversalExecer
}

func approvalReversalCorrectionIntentForOriginal(
	ctx context.Context,
	querier approvalReversalRowQuerier,
	originalExternalID string,
) (ApprovalReversalCorrectionIntent, bool, error) {
	return scanApprovalReversalCorrectionIntent(querier.QueryRowContext(ctx, `
SELECT correction_external_id, original_external_id, original_subject_sha256,
       correction_request_sha256, correction_type, operator, reason,
       change_ticket, status, failure_code, claimed_at, ended_at
FROM approval_reversal_correction_intents
WHERE original_external_id = ?
  AND status IN ('active', 'remote_conflict', 'resolved')`, originalExternalID))
}

func approvalReversalCorrectionIntentForCorrection(
	ctx context.Context,
	querier approvalReversalRowQuerier,
	correctionExternalID string,
) (ApprovalReversalCorrectionIntent, bool, error) {
	return scanApprovalReversalCorrectionIntent(querier.QueryRowContext(ctx, `
SELECT correction_external_id, original_external_id, original_subject_sha256,
       correction_request_sha256, correction_type, operator, reason,
       change_ticket, status, failure_code, claimed_at, ended_at
FROM approval_reversal_correction_intents
WHERE correction_external_id = ?`, correctionExternalID))
}

func scanApprovalReversalCorrectionIntent(
	row *sql.Row,
) (ApprovalReversalCorrectionIntent, bool, error) {
	var intent ApprovalReversalCorrectionIntent
	var claimedAt string
	var endedAt string
	err := row.Scan(
		&intent.CorrectionExternalID, &intent.OriginalExternalID,
		&intent.OriginalSubjectSHA256, &intent.CorrectionRequestSHA256,
		&intent.CorrectionType, &intent.Operator, &intent.Reason,
		&intent.ChangeTicket, &intent.Status, &intent.FailureCode,
		&claimedAt, &endedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ApprovalReversalCorrectionIntent{}, false, nil
	}
	if err != nil {
		return ApprovalReversalCorrectionIntent{}, false, fmt.Errorf(
			"read approval reversal correction intent by original grant: %w", err,
		)
	}
	intent.ClaimedAt, err = time.Parse(time.RFC3339Nano, claimedAt)
	if err != nil {
		return ApprovalReversalCorrectionIntent{}, false, fmt.Errorf(
			"parse approval reversal correction intent claimed_at: %w", err,
		)
	}
	if endedAt != "" {
		intent.EndedAt, err = time.Parse(time.RFC3339Nano, endedAt)
		if err != nil {
			return ApprovalReversalCorrectionIntent{}, false, fmt.Errorf(
				"parse approval reversal correction intent ended_at: %w", err,
			)
		}
	}
	return intent, true, nil
}

func sameApprovalReversalCorrectionIntent(
	stored ApprovalReversalCorrectionIntent,
	requested ApprovalReversalCorrectionIntent,
) bool {
	return stored.OriginalExternalID == requested.OriginalExternalID &&
		stored.OriginalSubjectSHA256 == requested.OriginalSubjectSHA256 &&
		stored.CorrectionExternalID == requested.CorrectionExternalID &&
		stored.CorrectionRequestSHA256 == requested.CorrectionRequestSHA256 &&
		stored.CorrectionType == requested.CorrectionType &&
		stored.Operator == requested.Operator && stored.Reason == requested.Reason &&
		stored.ChangeTicket == requested.ChangeTicket
}

func ensureApprovalReversalCorrectionIntent(
	ctx context.Context,
	store approvalReversalIntentStore,
	requested ApprovalReversalCorrectionIntent,
) (ApprovalReversalCorrectionIntent, error) {
	result, err := store.ExecContext(ctx, `
	INSERT OR IGNORE INTO approval_reversal_correction_intents (
	    correction_external_id, original_external_id, original_subject_sha256,
	    correction_request_sha256, correction_type, operator, reason, change_ticket,
	    status, failure_code, claimed_at, ended_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', '', ?, '')`,
		requested.CorrectionExternalID, requested.OriginalExternalID,
		requested.OriginalSubjectSHA256, requested.CorrectionRequestSHA256,
		requested.CorrectionType, requested.Operator, requested.Reason, requested.ChangeTicket,
		requested.ClaimedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return ApprovalReversalCorrectionIntent{}, fmt.Errorf(
			"store approval reversal correction intent: %w", err,
		)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return ApprovalReversalCorrectionIntent{}, fmt.Errorf(
			"inspect approval reversal correction intent insert: %w", err,
		)
	}
	stored, found, err := approvalReversalCorrectionIntentForCorrection(
		ctx, store, requested.CorrectionExternalID,
	)
	if err != nil {
		return ApprovalReversalCorrectionIntent{}, err
	}
	if !found || !sameApprovalReversalCorrectionIntent(stored, requested) {
		return ApprovalReversalCorrectionIntent{}, ErrApprovalReversalResolutionMismatch
	}
	if inserted == 1 {
		stored.EventKey = requested.EventKey
		return stored, nil
	}
	if stored.Status == ApprovalReversalCorrectionIntentAbandoned {
		result, err := store.ExecContext(ctx, `
UPDATE OR IGNORE approval_reversal_correction_intents
SET status = 'active', failure_code = '', claimed_at = ?, ended_at = ''
WHERE correction_external_id = ? AND status = 'abandoned'`,
			requested.ClaimedAt.Format(time.RFC3339Nano), requested.CorrectionExternalID,
		)
		if err != nil {
			return ApprovalReversalCorrectionIntent{}, fmt.Errorf(
				"reactivate approval reversal correction intent: %w", err,
			)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return ApprovalReversalCorrectionIntent{}, fmt.Errorf(
				"inspect approval reversal correction intent reactivation: %w", err,
			)
		}
		if affected != 1 {
			return ApprovalReversalCorrectionIntent{}, ErrApprovalReversalResolutionMismatch
		}
		stored.Status = ApprovalReversalCorrectionIntentActive
		stored.FailureCode = ""
		stored.ClaimedAt = requested.ClaimedAt
		stored.EndedAt = time.Time{}
		stored.EventKey = requested.EventKey
		return stored, nil
	}
	stored.EventKey = requested.EventKey
	stored.Replayed = true
	return stored, nil
}

type approvalReversalReceiptRow struct {
	OriginalExternalID      string
	OriginalSubjectSHA256   string
	CorrectionExternalID    string
	CorrectionRequestSHA256 string
	Operator                string
	Reason                  string
	ChangeTicket            string
	ResponseStatus          string
	ResultJSON              string
	ResolvedAt              string
}

func insertApprovalReversalReceipt(
	ctx context.Context,
	execer approvalReversalExecer,
	row approvalReversalReceiptRow,
) error {
	_, err := execer.ExecContext(ctx, `
INSERT INTO approval_reversal_resolution_receipts (
    correction_external_id, original_external_id, original_subject_sha256,
    correction_request_sha256, operator, reason, change_ticket,
    response_status, result_json, resolved_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.CorrectionExternalID, row.OriginalExternalID, row.OriginalSubjectSHA256,
		row.CorrectionRequestSHA256, row.Operator, row.Reason, row.ChangeTicket,
		row.ResponseStatus, row.ResultJSON, row.ResolvedAt,
	)
	return err
}

func approvalReversalReceiptForCorrection(
	ctx context.Context,
	querier approvalReversalRowQuerier,
	correctionExternalID string,
) (approvalReversalReceiptRow, bool, error) {
	var row approvalReversalReceiptRow
	err := querier.QueryRowContext(ctx, `
SELECT original_external_id, original_subject_sha256, correction_external_id,
       correction_request_sha256, operator, reason, change_ticket,
       response_status, result_json, resolved_at
FROM approval_reversal_resolution_receipts
WHERE correction_external_id = ?`, correctionExternalID).Scan(
		&row.OriginalExternalID, &row.OriginalSubjectSHA256, &row.CorrectionExternalID,
		&row.CorrectionRequestSHA256, &row.Operator, &row.Reason, &row.ChangeTicket,
		&row.ResponseStatus, &row.ResultJSON, &row.ResolvedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return approvalReversalReceiptRow{}, false, nil
	}
	if err != nil {
		return approvalReversalReceiptRow{}, false, fmt.Errorf("read approval reversal receipt by correction: %w", err)
	}
	return row, true, nil
}

func (s *Store) backfillApprovalReversalResolutionReceipts(ctx context.Context) error {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin approval reversal receipt backfill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
SELECT reversal.original_external_id,
       COALESCE((
           SELECT command.subject_sha256
           FROM entitlement_command_shadows command
           WHERE command.external_id = reversal.original_external_id
           ORDER BY command.created_at, command.event_key
           LIMIT 1
       ), ''),
       reversal.resolution_external_id, reversal.resolution_request_sha256,
       reversal.resolution_operator, reversal.resolution_reason,
       reversal.resolution_change_ticket, reversal.resolution_response_status,
       reversal.resolution_result_json, reversal.resolved_at
FROM approval_reversals reversal
WHERE reversal.resolution_external_id <> ''
ORDER BY reversal.resolved_at, reversal.event_key`)
	if err != nil {
		return fmt.Errorf("list legacy approval reversal receipts: %w", err)
	}
	receipts := make([]approvalReversalReceiptRow, 0)
	for rows.Next() {
		var row approvalReversalReceiptRow
		if err := rows.Scan(
			&row.OriginalExternalID, &row.OriginalSubjectSHA256, &row.CorrectionExternalID,
			&row.CorrectionRequestSHA256, &row.Operator, &row.Reason, &row.ChangeTicket,
			&row.ResponseStatus, &row.ResultJSON, &row.ResolvedAt,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan legacy approval reversal receipt: %w", err)
		}
		receipts = append(receipts, row)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy approval reversal receipts: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy approval reversal receipts: %w", err)
	}
	for _, receipt := range receipts {
		stored, found, err := approvalReversalReceiptForCorrection(ctx, tx, receipt.CorrectionExternalID)
		if err != nil {
			return err
		}
		if found {
			if stored != receipt {
				return ErrApprovalReversalResolutionMismatch
			}
			continue
		}
		if err := insertApprovalReversalReceipt(ctx, tx, receipt); err != nil {
			return fmt.Errorf("backfill approval reversal receipt: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit approval reversal receipt backfill: %w", err)
	}
	return nil
}

func approvalReversalResolutionForOriginal(
	ctx context.Context,
	querier approvalReversalRowQuerier,
	originalExternalID string,
) (ApprovalReversalResolution, bool, error) {
	var resolution ApprovalReversalResolution
	var resultJSON string
	var resolvedAt string
	err := querier.QueryRowContext(ctx, `
SELECT reversal.event_key, receipt.original_subject_sha256,
       receipt.correction_external_id, receipt.correction_request_sha256,
       receipt.operator, receipt.reason, receipt.change_ticket,
       receipt.response_status, receipt.result_json, receipt.resolved_at
FROM approval_reversal_resolution_receipts receipt
JOIN approval_reversals reversal
  ON reversal.resolution_external_id = receipt.correction_external_id
WHERE receipt.original_external_id = ?
ORDER BY receipt.resolved_at, reversal.event_key
LIMIT 1`, originalExternalID).Scan(
		&resolution.EventKey, &resolution.OriginalSubjectSHA256,
		&resolution.CorrectionExternalID, &resolution.CorrectionRequestSHA256,
		&resolution.Operator, &resolution.Reason, &resolution.ChangeTicket,
		&resolution.ResponseStatus, &resultJSON, &resolvedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ApprovalReversalResolution{}, false, nil
	}
	if err != nil {
		return ApprovalReversalResolution{}, false, fmt.Errorf("read approval reversal resolution by original grant: %w", err)
	}
	resolution.OriginalExternalID = originalExternalID
	if err := json.Unmarshal([]byte(resultJSON), &resolution.Result); err != nil {
		return ApprovalReversalResolution{}, false, fmt.Errorf("decode approval reversal resolution by original grant: %w", err)
	}
	resolution.ResolvedAt, err = time.Parse(time.RFC3339Nano, resolvedAt)
	if err != nil {
		return ApprovalReversalResolution{}, false, fmt.Errorf("parse approval reversal resolution by original grant: %w", err)
	}
	return resolution, true, nil
}

func (s *Store) GetApprovalReversalResolutionForOriginal(
	ctx context.Context,
	originalExternalID string,
) (ApprovalReversalResolution, bool, error) {
	if !validApprovalResolutionIdentifier(originalExternalID, 255) {
		return ApprovalReversalResolution{}, false, errors.New("invalid original grant external ID")
	}
	return approvalReversalResolutionForOriginal(ctx, s.database, originalExternalID)
}

func validApprovalResolutionIdentifier(value string, maximumLength int) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maximumLength {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validApprovalCorrectionResult(result ApprovalCorrectionResult) bool {
	switch result.CorrectionType {
	case "wallet_quota":
		return result.WalletQuota != nil && *result.WalletQuota >= 0 &&
			result.LevelCode == "" && result.SubscriptionID == 0 &&
			result.AssignmentVersion == 0 && result.Transition == ""
	case "subscription_level":
		return result.WalletQuota == nil && result.QuotaDelta == 0 &&
			validApprovalResolutionIdentifier(result.LevelCode, 64) &&
			result.SubscriptionID > 0 && result.AssignmentVersion > 0 &&
			(result.Transition == "updated" || result.Transition == "noop")
	default:
		return false
	}
}

func validateApprovalReversalResolution(resolution ApprovalReversalResolution) error {
	decodedHash, hashErr := hex.DecodeString(resolution.CorrectionRequestSHA256)
	if !validApprovalResolutionIdentifier(resolution.EventKey, 512) ||
		!validApprovalResolutionIdentifier(resolution.OriginalExternalID, 255) ||
		!digest.IsCanonicalSHA256(resolution.OriginalSubjectSHA256) ||
		!validApprovalResolutionIdentifier(resolution.CorrectionExternalID, 255) ||
		!strings.HasPrefix(resolution.CorrectionExternalID, "lark:correction:") ||
		len(resolution.CorrectionExternalID) == len("lark:correction:") ||
		hashErr != nil || len(decodedHash) != 32 ||
		!validApprovalResolutionIdentifier(resolution.Operator, 128) ||
		!validApprovalResolutionIdentifier(resolution.Reason, 512) ||
		!validApprovalResolutionIdentifier(resolution.ChangeTicket, 128) ||
		!validApprovalCorrectionResult(resolution.Result) {
		return errors.New("invalid approval reversal resolution")
	}
	switch resolution.ResponseStatus {
	case "applied", "replayed", "noop":
		return nil
	default:
		return errors.New("invalid approval reversal response status")
	}
}

func validateApprovalReversalCorrectionIntent(intent ApprovalReversalCorrectionIntent) error {
	if !validApprovalResolutionIdentifier(intent.EventKey, 512) ||
		!validApprovalResolutionIdentifier(intent.OriginalExternalID, 255) ||
		!digest.IsCanonicalSHA256(intent.OriginalSubjectSHA256) ||
		!validApprovalResolutionIdentifier(intent.CorrectionExternalID, 255) ||
		!strings.HasPrefix(intent.CorrectionExternalID, "lark:correction:") ||
		len(intent.CorrectionExternalID) == len("lark:correction:") ||
		!digest.IsCanonicalSHA256(intent.CorrectionRequestSHA256) ||
		(intent.CorrectionType != "wallet_quota" && intent.CorrectionType != "subscription_level") ||
		!validApprovalResolutionIdentifier(intent.Operator, 128) ||
		!validApprovalResolutionIdentifier(intent.Reason, 512) ||
		!validApprovalResolutionIdentifier(intent.ChangeTicket, 128) {
		return errors.New("invalid approval reversal correction intent")
	}
	return nil
}

func (s *Store) GetApprovalReversalCorrectionIntentForOriginal(
	ctx context.Context,
	originalExternalID string,
) (ApprovalReversalCorrectionIntent, bool, error) {
	if !validApprovalResolutionIdentifier(originalExternalID, 255) {
		return ApprovalReversalCorrectionIntent{}, false, errors.New("invalid original grant external ID")
	}
	return approvalReversalCorrectionIntentForOriginal(ctx, s.database, originalExternalID)
}

func (s *Store) ClaimApprovalReversalCorrectionIntent(
	ctx context.Context,
	intent ApprovalReversalCorrectionIntent,
) (ApprovalReversalCorrectionIntent, error) {
	if err := validateApprovalReversalCorrectionIntent(intent); err != nil {
		return ApprovalReversalCorrectionIntent{}, err
	}
	reversal, err := s.GetPendingApprovalReversal(ctx, intent.EventKey, intent.OriginalExternalID)
	if err != nil {
		return ApprovalReversalCorrectionIntent{}, err
	}
	if reversal.OriginalSubjectSHA256 != intent.OriginalSubjectSHA256 ||
		reversal.OriginalGrantType != intent.CorrectionType {
		return ApprovalReversalCorrectionIntent{}, ErrApprovalReversalResolutionMismatch
	}
	intent.ClaimedAt = s.now().UTC()
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return ApprovalReversalCorrectionIntent{}, fmt.Errorf(
			"begin approval reversal correction intent: %w", err,
		)
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := ensureApprovalReversalCorrectionIntent(ctx, tx, intent)
	if err != nil {
		return ApprovalReversalCorrectionIntent{}, err
	}
	if !stored.Replayed {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO controller_audit (event_key, action, outcome, created_at)
VALUES (?, 'approval_reversal_correction_intent', ?, ?)`,
			intent.EventKey, intent.CorrectionExternalID,
			intent.ClaimedAt.Format(time.RFC3339Nano),
		); err != nil {
			return ApprovalReversalCorrectionIntent{}, fmt.Errorf(
				"audit approval reversal correction intent: %w", err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return ApprovalReversalCorrectionIntent{}, fmt.Errorf(
			"commit approval reversal correction intent: %w", err,
		)
	}
	return stored, nil
}

func (s *Store) AbandonApprovalReversalCorrectionIntent(
	ctx context.Context,
	intent ApprovalReversalCorrectionIntent,
	failureCode string,
) error {
	return s.finalizeApprovalReversalCorrectionIntent(
		ctx, intent, ApprovalReversalCorrectionIntentAbandoned, failureCode,
	)
}

func (s *Store) BlockApprovalReversalCorrectionIntent(
	ctx context.Context,
	intent ApprovalReversalCorrectionIntent,
	failureCode string,
) error {
	return s.finalizeApprovalReversalCorrectionIntent(
		ctx, intent, ApprovalReversalCorrectionIntentRemoteConflict, failureCode,
	)
}

func (s *Store) finalizeApprovalReversalCorrectionIntent(
	ctx context.Context,
	intent ApprovalReversalCorrectionIntent,
	status ApprovalReversalCorrectionIntentStatus,
	failureCode string,
) error {
	if err := validateApprovalReversalCorrectionIntent(intent); err != nil {
		return err
	}
	if status != ApprovalReversalCorrectionIntentAbandoned &&
		status != ApprovalReversalCorrectionIntentRemoteConflict {
		return errors.New("invalid approval reversal correction intent final status")
	}
	if !validApprovalResolutionIdentifier(failureCode, 128) {
		return errors.New("invalid approval reversal correction intent failure code")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin approval reversal correction intent finalization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stored, found, err := approvalReversalCorrectionIntentForCorrection(
		ctx, tx, intent.CorrectionExternalID,
	)
	if err != nil {
		return err
	}
	if !found || !sameApprovalReversalCorrectionIntent(stored, intent) {
		return ErrApprovalReversalResolutionMismatch
	}
	if stored.Status == status && stored.FailureCode == failureCode {
		return nil
	}
	if stored.Status != ApprovalReversalCorrectionIntentActive {
		return ErrApprovalReversalResolutionMismatch
	}
	if _, found, err := approvalReversalReceiptForCorrection(
		ctx, tx, intent.CorrectionExternalID,
	); err != nil {
		return err
	} else if found {
		return ErrApprovalReversalResolutionMismatch
	}
	endedAt := s.now().UTC()
	result, err := tx.ExecContext(ctx, `
UPDATE approval_reversal_correction_intents
SET status = ?, failure_code = ?, ended_at = ?
WHERE correction_external_id = ? AND status = 'active'`,
		status, failureCode, endedAt.Format(time.RFC3339Nano), intent.CorrectionExternalID,
	)
	if err != nil {
		return fmt.Errorf("finalize approval reversal correction intent: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return fmt.Errorf(
			"finalize approval reversal correction intent affected %d rows: %w", affected, err,
		)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO controller_audit (event_key, action, outcome, created_at)
VALUES (?, ?, ?, ?)`,
		intent.EventKey, "approval_reversal_correction_intent_"+string(status),
		failureCode, endedAt.Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("audit approval reversal correction intent finalization: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit approval reversal correction intent finalization: %w", err)
	}
	return nil
}

func markApprovalReversalCorrectionIntentResolved(
	ctx context.Context,
	store approvalReversalIntentStore,
	intent ApprovalReversalCorrectionIntent,
	resolvedAt time.Time,
) error {
	stored, found, err := approvalReversalCorrectionIntentForCorrection(
		ctx, store, intent.CorrectionExternalID,
	)
	if err != nil {
		return err
	}
	if !found || !sameApprovalReversalCorrectionIntent(stored, intent) {
		return ErrApprovalReversalResolutionMismatch
	}
	if stored.Status == ApprovalReversalCorrectionIntentResolved {
		return nil
	}
	if stored.Status != ApprovalReversalCorrectionIntentActive {
		return ErrApprovalReversalResolutionMismatch
	}
	result, err := store.ExecContext(ctx, `
UPDATE approval_reversal_correction_intents
SET status = 'resolved', failure_code = '', ended_at = ?
WHERE correction_external_id = ? AND status = 'active'`,
		resolvedAt.Format(time.RFC3339Nano), intent.CorrectionExternalID,
	)
	if err != nil {
		return fmt.Errorf("resolve approval reversal correction intent: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return fmt.Errorf(
			"resolve approval reversal correction intent affected %d rows: %w", affected, err,
		)
	}
	return nil
}

func (s *Store) GetPendingApprovalReversal(
	ctx context.Context,
	eventKey string,
	originalExternalID string,
) (ApprovalReversal, error) {
	reversal, err := s.GetApprovalReversal(ctx, eventKey)
	if err != nil {
		return ApprovalReversal{}, err
	}
	if reversal.OriginalExternalID != originalExternalID {
		return ApprovalReversal{}, ErrApprovalReversalResolutionMismatch
	}
	event, err := s.Get(ctx, eventKey)
	if err != nil {
		return ApprovalReversal{}, err
	}
	if event.ProcessingState != ProcessingStateReversalPending || reversal.Resolution != nil {
		return ApprovalReversal{}, ErrApprovalReversalNotPending
	}
	return reversal, nil
}

func (s *Store) ListPendingApprovalReversals(
	ctx context.Context,
	limit int,
) ([]ApprovalReversal, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("approval reversal list limit must be between 1 and 1000")
	}
	rows, err := s.database.QueryContext(ctx, `
SELECT reversal.event_key, reversal.approval_code, reversal.target_instance_code,
       reversal.authority_approval_code, reversal.authority_instance_code,
	       reversal.authority_status, reversal.authority_reverted,
	       reversal.original_external_id,
	       COALESCE((
	           SELECT command.subject_sha256
	           FROM entitlement_command_shadows command
	           WHERE command.external_id = reversal.original_external_id
	           ORDER BY command.created_at, command.event_key
	           LIMIT 1
	       ), ''),
	       reversal.original_grant_status,
       reversal.original_grant_type, reversal.original_quota_delta,
       reversal.original_monthly_quota, reversal.original_policy_version,
       reversal.original_business_code, reversal.result, reversal.reason,
       reversal.created_at
FROM approval_reversals reversal
JOIN lark_event_inbox event ON event.event_key = reversal.event_key
WHERE event.processing_state = ? AND reversal.resolution_external_id = ''
ORDER BY reversal.created_at, reversal.event_key
LIMIT ?`, ProcessingStateReversalPending, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending approval reversals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	reversals := make([]ApprovalReversal, 0)
	for rows.Next() {
		var reversal ApprovalReversal
		var createdAt string
		if err := rows.Scan(
			&reversal.EventKey, &reversal.ApprovalCode, &reversal.TargetInstanceCode,
			&reversal.AuthorityApprovalCode, &reversal.AuthorityInstanceCode,
			&reversal.AuthorityStatus, &reversal.AuthorityReverted,
			&reversal.OriginalExternalID, &reversal.OriginalSubjectSHA256,
			&reversal.OriginalGrantStatus,
			&reversal.OriginalGrantType, &reversal.OriginalQuotaDelta,
			&reversal.OriginalMonthlyQuota, &reversal.OriginalPolicyVersion,
			&reversal.OriginalBusinessCode, &reversal.Result, &reversal.Reason,
			&createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan pending approval reversal: %w", err)
		}
		reversal.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse pending approval reversal created_at: %w", err)
		}
		reversals = append(reversals, reversal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending approval reversals: %w", err)
	}
	return reversals, nil
}

func sameApprovalReversalResolution(
	stored ApprovalReversalResolution,
	requested ApprovalReversalResolution,
	storedResultJSON string,
	requestedResultJSON string,
) bool {
	return stored.EventKey == requested.EventKey &&
		stored.OriginalExternalID == requested.OriginalExternalID &&
		stored.OriginalSubjectSHA256 == requested.OriginalSubjectSHA256 &&
		stored.CorrectionExternalID == requested.CorrectionExternalID &&
		stored.CorrectionRequestSHA256 == requested.CorrectionRequestSHA256 &&
		stored.Operator == requested.Operator && stored.Reason == requested.Reason &&
		stored.ChangeTicket == requested.ChangeTicket &&
		stored.ResponseStatus == requested.ResponseStatus &&
		storedResultJSON == requestedResultJSON
}

func sameApprovalReversalReceipt(
	stored ApprovalReversalResolution,
	requested ApprovalReversalResolution,
	storedResultJSON string,
	requestedResultJSON string,
) bool {
	return stored.OriginalExternalID == requested.OriginalExternalID &&
		stored.OriginalSubjectSHA256 == requested.OriginalSubjectSHA256 &&
		stored.CorrectionExternalID == requested.CorrectionExternalID &&
		stored.CorrectionRequestSHA256 == requested.CorrectionRequestSHA256 &&
		stored.Operator == requested.Operator && stored.Reason == requested.Reason &&
		stored.ChangeTicket == requested.ChangeTicket &&
		stored.ResponseStatus == requested.ResponseStatus &&
		storedResultJSON == requestedResultJSON
}

func (s *Store) ResolveApprovalReversal(
	ctx context.Context,
	resolution ApprovalReversalResolution,
) (ApprovalReversalResolution, error) {
	if err := validateApprovalReversalResolution(resolution); err != nil {
		return ApprovalReversalResolution{}, err
	}
	resultJSON, err := json.Marshal(resolution.Result)
	if err != nil {
		return ApprovalReversalResolution{}, fmt.Errorf("encode approval reversal resolution result: %w", err)
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return ApprovalReversalResolution{}, fmt.Errorf("begin approval reversal resolution: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var originalExternalID string
	var originalSubjectSHA256 string
	var originalGrantType string
	var originalGrantStatus EntitlementGrantJobStatus
	var processingState ProcessingState
	var stored ApprovalReversalResolution
	var storedResultJSON string
	var storedResolvedAt string
	err = tx.QueryRowContext(ctx, `
	SELECT reversal.original_external_id,
	       COALESCE((
	           SELECT command.subject_sha256
	           FROM entitlement_command_shadows command
	           WHERE command.external_id = reversal.original_external_id
	           ORDER BY command.created_at, command.event_key
	           LIMIT 1
	       ), ''),
	       reversal.original_grant_type, reversal.original_grant_status,
	       event.processing_state,
       reversal.resolution_external_id, reversal.resolution_request_sha256,
       reversal.resolution_operator, reversal.resolution_reason,
       reversal.resolution_change_ticket, reversal.resolution_response_status,
       reversal.resolution_result_json, reversal.resolved_at
FROM approval_reversals reversal
JOIN lark_event_inbox event ON event.event_key = reversal.event_key
WHERE reversal.event_key = ?`, resolution.EventKey).Scan(
		&originalExternalID, &originalSubjectSHA256, &originalGrantType,
		&originalGrantStatus, &processingState,
		&stored.CorrectionExternalID, &stored.CorrectionRequestSHA256,
		&stored.Operator, &stored.Reason, &stored.ChangeTicket,
		&stored.ResponseStatus, &storedResultJSON, &storedResolvedAt,
	)
	if err != nil {
		return ApprovalReversalResolution{}, fmt.Errorf("read approval reversal resolution target: %w", err)
	}
	if originalExternalID != resolution.OriginalExternalID {
		return ApprovalReversalResolution{}, ErrApprovalReversalResolutionMismatch
	}
	if originalSubjectSHA256 != resolution.OriginalSubjectSHA256 {
		return ApprovalReversalResolution{}, ErrApprovalReversalResolutionMismatch
	}
	if originalGrantType != resolution.Result.CorrectionType {
		return ApprovalReversalResolution{}, ErrApprovalReversalResolutionMismatch
	}
	if stored.CorrectionExternalID != "" {
		stored.EventKey = resolution.EventKey
		stored.OriginalExternalID = originalExternalID
		stored.OriginalSubjectSHA256 = originalSubjectSHA256
		if !sameApprovalReversalResolution(stored, resolution, storedResultJSON, string(resultJSON)) {
			return ApprovalReversalResolution{}, ErrApprovalReversalResolutionMismatch
		}
		stored.ResolvedAt, err = time.Parse(time.RFC3339Nano, storedResolvedAt)
		if err != nil {
			return ApprovalReversalResolution{}, fmt.Errorf("parse stored approval reversal resolved_at: %w", err)
		}
		stored.Result = resolution.Result
		stored.Replayed = true
		return stored, nil
	}
	if processingState != ProcessingStateReversalPending {
		return ApprovalReversalResolution{}, ErrApprovalReversalNotPending
	}

	requestedEventKey := resolution.EventKey
	resolvedAt := s.now().UTC()
	correctionIntent := ApprovalReversalCorrectionIntent{
		EventKey:                resolution.EventKey,
		OriginalExternalID:      resolution.OriginalExternalID,
		OriginalSubjectSHA256:   resolution.OriginalSubjectSHA256,
		CorrectionExternalID:    resolution.CorrectionExternalID,
		CorrectionRequestSHA256: resolution.CorrectionRequestSHA256,
		CorrectionType:          resolution.Result.CorrectionType,
		Operator:                resolution.Operator,
		Reason:                  resolution.Reason,
		ChangeTicket:            resolution.ChangeTicket,
		ClaimedAt:               resolvedAt,
	}
	storedIntent, err := ensureApprovalReversalCorrectionIntent(ctx, tx, correctionIntent)
	if err != nil {
		return ApprovalReversalResolution{}, err
	}
	if storedIntent.Status != ApprovalReversalCorrectionIntentActive &&
		storedIntent.Status != ApprovalReversalCorrectionIntentResolved {
		return ApprovalReversalResolution{}, ErrApprovalReversalResolutionMismatch
	}
	existing, existingFound, err := approvalReversalResolutionForOriginal(
		ctx, tx, resolution.OriginalExternalID,
	)
	if err != nil {
		return ApprovalReversalResolution{}, err
	}
	if existingFound {
		existingResultJSON, err := json.Marshal(existing.Result)
		if err != nil {
			return ApprovalReversalResolution{}, fmt.Errorf("encode existing approval reversal resolution result: %w", err)
		}
		if !sameApprovalReversalReceipt(existing, resolution, string(existingResultJSON), string(resultJSON)) {
			return ApprovalReversalResolution{}, ErrApprovalReversalResolutionMismatch
		}
		resolvedAt = existing.ResolvedAt
		resolution = existing
		resolution.EventKey = requestedEventKey
		resolution.Replayed = true
	}
	correctionReceipt, correctionReceiptFound, err := approvalReversalReceiptForCorrection(
		ctx, tx, resolution.CorrectionExternalID,
	)
	if err != nil {
		return ApprovalReversalResolution{}, err
	}
	if correctionReceiptFound && correctionReceipt.OriginalExternalID != resolution.OriginalExternalID {
		return ApprovalReversalResolution{}, ErrApprovalReversalResolutionMismatch
	}

	rows, err := tx.QueryContext(ctx, `
SELECT reversal.event_key
FROM approval_reversals reversal
JOIN lark_event_inbox event ON event.event_key = reversal.event_key
WHERE reversal.original_external_id = ?
  AND reversal.original_grant_type = ?
  AND reversal.resolution_external_id = ''
  AND event.processing_state = ?
ORDER BY reversal.created_at, reversal.event_key`,
		resolution.OriginalExternalID,
		resolution.Result.CorrectionType,
		ProcessingStateReversalPending,
	)
	if err != nil {
		return ApprovalReversalResolution{}, fmt.Errorf("list approval reversal resolution group: %w", err)
	}
	eventKeys := make([]string, 0, 1)
	for rows.Next() {
		var eventKey string
		if err := rows.Scan(&eventKey); err != nil {
			_ = rows.Close()
			return ApprovalReversalResolution{}, fmt.Errorf("scan approval reversal resolution group: %w", err)
		}
		eventKeys = append(eventKeys, eventKey)
	}
	if err := rows.Close(); err != nil {
		return ApprovalReversalResolution{}, fmt.Errorf("close approval reversal resolution group: %w", err)
	}
	if err := rows.Err(); err != nil {
		return ApprovalReversalResolution{}, fmt.Errorf("iterate approval reversal resolution group: %w", err)
	}
	if len(eventKeys) == 0 {
		return ApprovalReversalResolution{}, ErrApprovalReversalNotPending
	}
	targetIncluded := false
	for _, eventKey := range eventKeys {
		if eventKey == resolution.EventKey {
			targetIncluded = true
			break
		}
	}
	if !targetIncluded {
		return ApprovalReversalResolution{}, ErrApprovalReversalNotPending
	}

	resolvedAtText := resolvedAt.Format(time.RFC3339Nano)
	if !existingFound {
		if err := insertApprovalReversalReceipt(ctx, tx, approvalReversalReceiptRow{
			OriginalExternalID:      resolution.OriginalExternalID,
			OriginalSubjectSHA256:   resolution.OriginalSubjectSHA256,
			CorrectionExternalID:    resolution.CorrectionExternalID,
			CorrectionRequestSHA256: resolution.CorrectionRequestSHA256,
			Operator:                resolution.Operator,
			Reason:                  resolution.Reason,
			ChangeTicket:            resolution.ChangeTicket,
			ResponseStatus:          resolution.ResponseStatus,
			ResultJSON:              string(resultJSON),
			ResolvedAt:              resolvedAtText,
		}); err != nil {
			return ApprovalReversalResolution{}, fmt.Errorf("store approval reversal receipt: %w", err)
		}
	}
	if err := markApprovalReversalCorrectionIntentResolved(
		ctx, tx, correctionIntent, resolvedAt,
	); err != nil {
		return ApprovalReversalResolution{}, err
	}
	for _, eventKey := range eventKeys {
		updated, err := tx.ExecContext(ctx, `
UPDATE approval_reversals
SET resolution_external_id = ?, resolution_request_sha256 = ?,
    resolution_operator = ?, resolution_reason = ?, resolution_change_ticket = ?,
    resolution_response_status = ?, resolution_result_json = ?, resolved_at = ?
WHERE event_key = ? AND resolution_external_id = ''`,
			resolution.CorrectionExternalID, resolution.CorrectionRequestSHA256,
			resolution.Operator, resolution.Reason, resolution.ChangeTicket,
			resolution.ResponseStatus, string(resultJSON), resolvedAtText,
			eventKey,
		)
		if err != nil {
			return ApprovalReversalResolution{}, fmt.Errorf("store approval reversal resolution: %w", err)
		}
		affected, err := updated.RowsAffected()
		if err != nil || affected != 1 {
			return ApprovalReversalResolution{}, fmt.Errorf("store approval reversal resolution affected %d rows: %w", affected, err)
		}
		updated, err = tx.ExecContext(ctx, `
UPDATE lark_event_inbox SET processing_state = ?
WHERE event_key = ? AND processing_state = ?`,
			ProcessingStateReversalResolved, eventKey, ProcessingStateReversalPending,
		)
		if err != nil {
			return ApprovalReversalResolution{}, fmt.Errorf("resolve approval reversal event: %w", err)
		}
		affected, err = updated.RowsAffected()
		if err != nil || affected != 1 {
			return ApprovalReversalResolution{}, fmt.Errorf("resolve approval reversal event affected %d rows: %w", affected, err)
		}
		updated, err = tx.ExecContext(ctx, `
UPDATE jobs SET status = ?, last_error = '', updated_at = ?
WHERE event_key = ? AND status = ?`,
			jobStatusReversalResolved, resolvedAtText, eventKey, jobStatusReversalPending,
		)
		if err != nil {
			return ApprovalReversalResolution{}, fmt.Errorf("resolve approval reversal inbox job: %w", err)
		}
		affected, err = updated.RowsAffected()
		if err != nil || affected != 1 {
			return ApprovalReversalResolution{}, fmt.Errorf("resolve approval reversal inbox job affected %d rows: %w", affected, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO controller_audit (event_key, action, outcome, created_at)
VALUES (?, 'approval_reversal_correction', ?, ?)`,
			eventKey, resolution.ResponseStatus, resolvedAtText,
		); err != nil {
			return ApprovalReversalResolution{}, fmt.Errorf("audit approval reversal resolution: %w", err)
		}
	}
	if approvalReversalGrantWasFenced(originalGrantStatus) {
		updated, err := tx.ExecContext(ctx, `
UPDATE entitlement_grant_jobs
SET status = ?, last_error = '', completed_at = ?, updated_at = ?
WHERE external_id = ? AND status = ?`,
			EntitlementGrantJobStatusReversalResolved, resolvedAtText, resolvedAtText,
			resolution.OriginalExternalID, EntitlementGrantJobStatusReversalPending,
		)
		if err != nil {
			return ApprovalReversalResolution{}, fmt.Errorf("resolve approval reversal grant job: %w", err)
		}
		affected, err := updated.RowsAffected()
		if err != nil {
			return ApprovalReversalResolution{}, fmt.Errorf("resolve approval reversal grant job affected %d rows: %w", affected, err)
		}
		if affected == 0 {
			var currentStatus EntitlementGrantJobStatus
			if err := tx.QueryRowContext(ctx,
				"SELECT status FROM entitlement_grant_jobs WHERE external_id = ?",
				resolution.OriginalExternalID,
			).Scan(&currentStatus); err != nil {
				return ApprovalReversalResolution{}, fmt.Errorf("inspect already resolved approval reversal grant job: %w", err)
			}
			if currentStatus != EntitlementGrantJobStatusReversalResolved {
				return ApprovalReversalResolution{}, fmt.Errorf(
					"resolve approval reversal grant job affected 0 rows with status %q",
					currentStatus,
				)
			}
		} else if affected != 1 {
			return ApprovalReversalResolution{}, fmt.Errorf("resolve approval reversal grant job affected %d rows", affected)
		}
	}
	if err := tx.Commit(); err != nil {
		return ApprovalReversalResolution{}, fmt.Errorf("commit approval reversal resolution: %w", err)
	}
	resolution.ResolvedAt = resolvedAt
	return resolution, nil
}

func approvalReversalGrantWasFenced(status EntitlementGrantJobStatus) bool {
	switch status {
	case EntitlementGrantJobStatusHeldShadow,
		EntitlementGrantJobStatusPending,
		EntitlementGrantJobStatusProcessing,
		EntitlementGrantJobStatusRetryWait,
		EntitlementGrantJobStatusReversalPending:
		return true
	default:
		return false
	}
}
