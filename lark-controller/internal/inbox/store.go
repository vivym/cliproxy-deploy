package inbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vivym/x2r-ai-gateway/lark-controller/internal/policy"
)

const sqliteBusyTimeout = 500 * time.Millisecond

type Event struct {
	Key                  string
	Origin               EventOrigin
	SchemaVersion        string
	EventID              string
	EventType            string
	AppID                string
	TenantKey            string
	ApprovalCode         string
	InstanceCode         string
	RevertedInstanceCode string
	Status               string
	PayloadJSON          string
	ProcessingState      ProcessingState
	DuplicateCount       int64
	ReceivedAt           time.Time
	LastSeenAt           time.Time
	PrincipalDisableJob  *PrincipalDisableJobDraft
}

type EventOrigin string

const (
	EventOriginWebhook                EventOrigin = "webhook"
	EventOriginApprovalReconciliation EventOrigin = "approval_reconciliation"
)

type ProcessingState string

const (
	ProcessingStatePending           ProcessingState = "pending"
	ProcessingStateProcessing        ProcessingState = "processing"
	ProcessingStateShadowRecorded    ProcessingState = "shadow_recorded"
	ProcessingStateReversalPending   ProcessingState = "reversal_pending"
	ProcessingStateDeadLetter        ProcessingState = "dead_letter"
	ProcessingStatePrincipalDisabled ProcessingState = "principal_disabled"
)

type DecisionOutcome string

const (
	DecisionOutcomeShadowAuthorityVerified        DecisionOutcome = "shadow_authority_verified"
	DecisionOutcomeShadowLegacyUnresolved         DecisionOutcome = "shadow_authority_verified_legacy_unresolved"
	DecisionOutcomeShadowAuthorityRejected        DecisionOutcome = "shadow_authority_rejected"
	DecisionOutcomeShadowIgnoredNonApproved       DecisionOutcome = "shadow_ignored_non_approved"
	DecisionOutcomeReversalPending                DecisionOutcome = "reversal_pending"
	DecisionOutcomeDeadLetterUnknownStatus        DecisionOutcome = "dead_letter_unknown_status"
	DecisionOutcomeDeadLetterUnsupportedEventType DecisionOutcome = "dead_letter_unsupported_event_type"
	DecisionOutcomeDeadLetterPolicyValidation     DecisionOutcome = "dead_letter_policy_validation_failed"
	DecisionOutcomeDeadLetterApprovalFetch        DecisionOutcome = "dead_letter_approval_fetch_failed"
	DecisionOutcomeDeadLetterCommandPlanning      DecisionOutcome = "dead_letter_command_planning_failed"
)

type jobStatus string

const (
	jobStatusPending         jobStatus = "pending"
	jobStatusProcessing      jobStatus = "processing"
	jobStatusRetryWait       jobStatus = "retry_wait"
	jobStatusSucceeded       jobStatus = "succeeded"
	jobStatusReversalPending jobStatus = "reversal_pending"
	jobStatusDeadLetter      jobStatus = "dead_letter"
)

type Store struct {
	database       *sql.DB
	now            func() time.Time
	oauthMu        sync.Mutex
	oauthCounts    [oauthCredentialKindCount]int64
	oauthLastPrune time.Time
}

var ErrEventPayloadMismatch = errors.New("event id payload mismatch")

type Job struct {
	ID       int64
	Attempts int
	Event    Event
}

type Decision struct {
	EventKey            string
	ApprovalCode        string
	InstanceCode        string
	EventStatus         string
	AuthorityStatus     string
	Outcome             DecisionOutcome
	PolicyVersion       string
	ApprovalKind        policy.ApprovalKind
	SchemaFingerprint   string
	BusinessCode        string
	Locale              string
	CatalogSHA256       string
	QuotaDelta          int64
	MonthlyQuota        int64
	LevelRank           int
	FailureReason       string
	OpenIDHash          string
	FormSHA256          string
	StartTime           string
	Reverted            bool
	CreatedAt           time.Time
	EntitlementCommand  *EntitlementCommandShadow
	EntitlementGrantJob *EntitlementGrantJobDraft
	ApprovalReversal    *ApprovalReversalDraft
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("sqlite path is required")
	}
	dsn := (&url.URL{
		Scheme: "file",
		Path:   path,
		RawQuery: fmt.Sprintf(
			"_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)",
			sqliteBusyTimeout.Milliseconds(),
		),
	}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	database.SetMaxOpenConns(1)
	store := &Store{database: database, now: time.Now}
	if err := store.migrate(context.Background()); err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := store.initializeOAuthCredentialRetention(context.Background()); err != nil {
		_ = database.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS lark_event_inbox (
    event_key TEXT PRIMARY KEY,
    event_origin TEXT NOT NULL DEFAULT 'webhook',
    schema_version TEXT NOT NULL,
    event_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    app_id TEXT NOT NULL,
    tenant_key TEXT NOT NULL,
    approval_code TEXT NOT NULL DEFAULT '',
    instance_code TEXT NOT NULL DEFAULT '',
    reverted_instance_code TEXT NOT NULL DEFAULT '',
    event_status TEXT NOT NULL DEFAULT '',
    payload_json TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    processing_state TEXT NOT NULL DEFAULT 'pending',
    duplicate_count INTEGER NOT NULL DEFAULT 0,
    received_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_lark_event_inbox_processing
    ON lark_event_inbox(processing_state, received_at, event_key);
CREATE TABLE IF NOT EXISTS jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_key TEXT NOT NULL UNIQUE REFERENCES lark_event_inbox(event_key) ON DELETE CASCADE,
    job_type TEXT NOT NULL,
    status TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_ready
    ON jobs(status, next_attempt_at, id);
CREATE TABLE IF NOT EXISTS approval_instances (
    event_key TEXT PRIMARY KEY REFERENCES lark_event_inbox(event_key) ON DELETE CASCADE,
    approval_code TEXT NOT NULL,
    instance_code TEXT NOT NULL,
    event_status TEXT NOT NULL,
    authority_status TEXT NOT NULL,
    outcome TEXT NOT NULL,
    policy_version TEXT NOT NULL DEFAULT '',
    approval_kind TEXT NOT NULL DEFAULT '',
    schema_fingerprint TEXT NOT NULL DEFAULT '',
    business_code TEXT NOT NULL DEFAULT '',
    locale TEXT NOT NULL DEFAULT '',
    catalog_sha256 TEXT NOT NULL DEFAULT '',
    quota_delta INTEGER NOT NULL DEFAULT 0,
    monthly_quota INTEGER NOT NULL DEFAULT 0,
    level_rank INTEGER NOT NULL DEFAULT 0,
	 failure_reason TEXT NOT NULL DEFAULT '',
    open_id_hash TEXT NOT NULL DEFAULT '',
    form_sha256 TEXT NOT NULL DEFAULT '',
    start_time TEXT NOT NULL DEFAULT '',
    reverted INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS approval_reversals (
    event_key TEXT PRIMARY KEY REFERENCES lark_event_inbox(event_key) ON DELETE CASCADE,
    approval_code TEXT NOT NULL,
    target_instance_code TEXT NOT NULL,
    authority_approval_code TEXT NOT NULL DEFAULT '',
    authority_instance_code TEXT NOT NULL DEFAULT '',
    authority_status TEXT NOT NULL DEFAULT '',
    authority_reverted INTEGER NOT NULL DEFAULT 0,
    original_external_id TEXT NOT NULL DEFAULT '',
    original_grant_status TEXT NOT NULL DEFAULT '',
    original_grant_type TEXT NOT NULL DEFAULT '',
    original_quota_delta INTEGER NOT NULL DEFAULT 0,
    original_monthly_quota INTEGER NOT NULL DEFAULT 0,
    original_policy_version TEXT NOT NULL DEFAULT '',
    original_business_code TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL,
    reason TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_approval_reversals_result
    ON approval_reversals(result, created_at, event_key);
CREATE TABLE IF NOT EXISTS approval_reconciliation_cursors (
    approval_code TEXT PRIMARY KEY,
    scanned_through TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS approval_reconciliation_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    approval_code TEXT NOT NULL,
    window_start TEXT NOT NULL,
    window_end TEXT NOT NULL,
    result TEXT NOT NULL,
    instance_count INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_approval_reconciliation_audit_result
    ON approval_reconciliation_audit(result, created_at, id);
CREATE TABLE IF NOT EXISTS controller_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_key TEXT NOT NULL REFERENCES lark_event_inbox(event_key) ON DELETE CASCADE,
    action TEXT NOT NULL,
    outcome TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS entitlement_command_shadows (
    event_key TEXT PRIMARY KEY REFERENCES lark_event_inbox(event_key) ON DELETE CASCADE,
    external_id TEXT NOT NULL,
    request_sha256 TEXT NOT NULL,
    subject_sha256 TEXT NOT NULL,
    source TEXT NOT NULL,
    policy_version TEXT NOT NULL,
    catalog_sha256 TEXT NOT NULL,
    grant_type TEXT NOT NULL,
    business_code TEXT NOT NULL,
    quota_delta INTEGER NOT NULL DEFAULT 0,
    monthly_quota INTEGER NOT NULL DEFAULT 0,
    outcome TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_entitlement_command_shadows_external_id
    ON entitlement_command_shadows(external_id);
CREATE TABLE IF NOT EXISTS base_subscription_grants (
    external_id TEXT PRIMARY KEY,
    request_sha256 TEXT NOT NULL,
    subject_sha256 TEXT NOT NULL,
    policy_version TEXT NOT NULL,
    catalog_sha256 TEXT NOT NULL,
    level_code TEXT NOT NULL,
    monthly_quota INTEGER NOT NULL,
    outcome TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS base_subscription_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    external_id TEXT NOT NULL REFERENCES base_subscription_grants(external_id) ON DELETE RESTRICT,
    action TEXT NOT NULL,
    outcome TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_base_subscription_audit_action
    ON base_subscription_audit(action, outcome);
CREATE TABLE IF NOT EXISTS entitlement_grant_jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    external_id TEXT NOT NULL UNIQUE,
    request_sha256 TEXT NOT NULL,
    subject_sha256 TEXT NOT NULL,
    key_id TEXT NOT NULL,
    nonce BLOB NOT NULL,
    ciphertext BLOB NOT NULL,
    status TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TEXT NOT NULL,
    last_error TEXT NOT NULL DEFAULT '',
    activated_at TEXT NOT NULL DEFAULT '',
    response_status TEXT NOT NULL DEFAULT '',
    response_user_id INTEGER NOT NULL DEFAULT 0,
    result_grant_type TEXT NOT NULL DEFAULT '',
    result_quota_delta INTEGER NOT NULL DEFAULT 0,
    result_level_code TEXT NOT NULL DEFAULT '',
    result_subscription_id INTEGER NOT NULL DEFAULT 0,
    result_assignment_version INTEGER NOT NULL DEFAULT 0,
    result_transition TEXT NOT NULL DEFAULT '',
    completed_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
	CREATE INDEX IF NOT EXISTS idx_entitlement_grant_jobs_ready
	    ON entitlement_grant_jobs(status, next_attempt_at, id);
	CREATE TABLE IF NOT EXISTS principal_disable_jobs (
	    id INTEGER PRIMARY KEY AUTOINCREMENT,
	    event_key TEXT UNIQUE REFERENCES lark_event_inbox(event_key) ON DELETE RESTRICT,
	    external_id TEXT NOT NULL UNIQUE,
	    request_sha256 TEXT NOT NULL,
	    subject_sha256 TEXT NOT NULL,
	    key_id TEXT NOT NULL,
	    nonce BLOB NOT NULL,
	    ciphertext BLOB NOT NULL,
	    status TEXT NOT NULL,
	    attempts INTEGER NOT NULL DEFAULT 0,
	    next_attempt_at TEXT NOT NULL,
	    last_error TEXT NOT NULL DEFAULT '',
	    activated_at TEXT NOT NULL DEFAULT '',
	    response_status TEXT NOT NULL DEFAULT '',
	    response_outcome TEXT NOT NULL DEFAULT '',
	    response_principal_version INTEGER NOT NULL DEFAULT 0,
	    response_auth_version INTEGER NOT NULL DEFAULT 0,
	    completed_at TEXT NOT NULL DEFAULT '',
	    created_at TEXT NOT NULL,
	    updated_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_principal_disable_jobs_ready
	    ON principal_disable_jobs(status, next_attempt_at, id);
	CREATE TABLE IF NOT EXISTS principal_disable_audit (
	    id INTEGER PRIMARY KEY AUTOINCREMENT,
	    external_id TEXT NOT NULL REFERENCES principal_disable_jobs(external_id) ON DELETE RESTRICT,
	    event_key TEXT REFERENCES lark_event_inbox(event_key) ON DELETE RESTRICT,
	    action TEXT NOT NULL,
	    outcome TEXT NOT NULL,
	    created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_principal_disable_audit_action
	    ON principal_disable_audit(action, outcome);
	CREATE TABLE IF NOT EXISTS employment_reconciliation_runs (
	    reconciliation_id TEXT PRIMARY KEY,
	    evidence_date TEXT NOT NULL UNIQUE,
	    status TEXT NOT NULL,
	    permission_healthy INTEGER NOT NULL,
	    scan_complete INTEGER NOT NULL,
	    checked_count INTEGER NOT NULL,
	    failure_reason TEXT NOT NULL DEFAULT '',
	    started_at TEXT NOT NULL,
	    completed_at TEXT NOT NULL,
	    updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS employment_checks (
	    reconciliation_id TEXT NOT NULL REFERENCES employment_reconciliation_runs(reconciliation_id) ON DELETE RESTRICT,
	    subject_sha256 TEXT NOT NULL,
	    checked_at TEXT NOT NULL,
	    result TEXT NOT NULL,
	    lark_result_code INTEGER NOT NULL,
	    permission_healthy INTEGER NOT NULL,
	    evidence_sha256 TEXT NOT NULL,
	    PRIMARY KEY (reconciliation_id, subject_sha256)
	);
	CREATE INDEX IF NOT EXISTS idx_employment_checks_subject
	    ON employment_checks(subject_sha256, checked_at);
	CREATE TABLE IF NOT EXISTS employment_reconciliation_audit (
	    id INTEGER PRIMARY KEY AUTOINCREMENT,
	    reconciliation_id TEXT NOT NULL REFERENCES employment_reconciliation_runs(reconciliation_id) ON DELETE RESTRICT,
	    result TEXT NOT NULL,
	    created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_employment_reconciliation_audit_result
	    ON employment_reconciliation_audit(result);
	CREATE TABLE IF NOT EXISTS processing_recovery_audit (
	    id INTEGER PRIMARY KEY AUTOINCREMENT,
	    queue TEXT NOT NULL,
	    recovered_count INTEGER NOT NULL CHECK(recovered_count > 0),
	    created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_processing_recovery_audit_queue
	    ON processing_recovery_audit(queue);
	CREATE TABLE IF NOT EXISTS employment_missing_evidence (
	    subject_sha256 TEXT PRIMARY KEY,
	    consecutive_count INTEGER NOT NULL,
	    first_not_found_at TEXT NOT NULL,
	    last_not_found_at TEXT NOT NULL,
	    updated_at TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS oauth_states (
	    state_hash BLOB PRIMARY KEY CHECK(length(state_hash) = 32),
	    new_api_state TEXT NOT NULL,
	    redirect_uri TEXT NOT NULL,
	    expires_at INTEGER NOT NULL,
	    consumed_at TEXT NOT NULL DEFAULT '',
	    created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_oauth_states_expiry
	    ON oauth_states(expires_at);
	CREATE TABLE IF NOT EXISTS oauth_login_codes (
	    code_hash BLOB PRIMARY KEY CHECK(length(code_hash) = 32),
	    subject TEXT NOT NULL,
	    username TEXT NOT NULL,
	    display_name TEXT NOT NULL,
	    expires_at INTEGER NOT NULL,
	    consumed_at TEXT NOT NULL DEFAULT '',
	    created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_oauth_login_codes_expiry
	    ON oauth_login_codes(expires_at);
	CREATE TABLE IF NOT EXISTS oauth_access_handles (
	    handle_hash BLOB PRIMARY KEY CHECK(length(handle_hash) = 32),
	    subject TEXT NOT NULL,
	    username TEXT NOT NULL,
	    display_name TEXT NOT NULL,
	    expires_at INTEGER NOT NULL,
	    consumed_at TEXT NOT NULL DEFAULT '',
	    created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_oauth_access_handles_expiry
	    ON oauth_access_handles(expires_at);
	CREATE TABLE IF NOT EXISTS policy_versions (
    policy_version TEXT PRIMARY KEY,
    catalog_sha256 TEXT NOT NULL UNIQUE,
    source_sha256 TEXT NOT NULL,
    state TEXT NOT NULL,
    retire_after TEXT NOT NULL DEFAULT '',
    catalog_json TEXT NOT NULL,
    loaded_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS approval_policy_bindings (
    approval_code TEXT NOT NULL,
    schema_fingerprint TEXT NOT NULL,
    locale TEXT NOT NULL,
    policy_version TEXT NOT NULL REFERENCES policy_versions(policy_version),
    approval_kind TEXT NOT NULL,
    definition_manifest_sha256 TEXT NOT NULL,
    definition_manifest_json TEXT NOT NULL,
    accept_instance_started_before TEXT NOT NULL DEFAULT '',
    loaded_at TEXT NOT NULL,
    PRIMARY KEY (approval_code, schema_fingerprint, locale)
);
UPDATE jobs SET status = 'pending', updated_at = next_attempt_at
WHERE status = 'processing';
UPDATE lark_event_inbox SET processing_state = 'pending'
WHERE processing_state = 'processing'
  AND event_key IN (SELECT event_key FROM jobs WHERE status = 'pending');
UPDATE entitlement_grant_jobs SET status = 'pending', updated_at = next_attempt_at
WHERE status = 'processing';
UPDATE principal_disable_jobs SET status = 'pending', updated_at = next_attempt_at
WHERE status = 'processing';
UPDATE lark_event_inbox SET processing_state = 'pending'
WHERE processing_state = 'processing'
  AND event_key IN (
      SELECT event_key FROM principal_disable_jobs
      WHERE status = 'pending' AND event_key IS NOT NULL
  );
`
	if _, err := s.database.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate inbox: %w", err)
	}
	if err := s.ensureApprovalDecisionColumns(ctx); err != nil {
		return err
	}
	if err := s.ensureInboxColumns(ctx); err != nil {
		return err
	}
	if err := s.ensurePolicyVersionColumns(ctx); err != nil {
		return err
	}
	if err := s.ensureEntitlementGrantJobColumns(ctx); err != nil {
		return err
	}
	if err := s.reclassifyLegacyApprovalDecisions(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureApprovalDecisionColumns(ctx context.Context) error {
	required := []struct {
		name string
		ddl  string
	}{
		{"policy_version", "ALTER TABLE approval_instances ADD COLUMN policy_version TEXT NOT NULL DEFAULT ''"},
		{"approval_kind", "ALTER TABLE approval_instances ADD COLUMN approval_kind TEXT NOT NULL DEFAULT ''"},
		{"schema_fingerprint", "ALTER TABLE approval_instances ADD COLUMN schema_fingerprint TEXT NOT NULL DEFAULT ''"},
		{"business_code", "ALTER TABLE approval_instances ADD COLUMN business_code TEXT NOT NULL DEFAULT ''"},
		{"locale", "ALTER TABLE approval_instances ADD COLUMN locale TEXT NOT NULL DEFAULT ''"},
		{"catalog_sha256", "ALTER TABLE approval_instances ADD COLUMN catalog_sha256 TEXT NOT NULL DEFAULT ''"},
		{"quota_delta", "ALTER TABLE approval_instances ADD COLUMN quota_delta INTEGER NOT NULL DEFAULT 0"},
		{"monthly_quota", "ALTER TABLE approval_instances ADD COLUMN monthly_quota INTEGER NOT NULL DEFAULT 0"},
		{"level_rank", "ALTER TABLE approval_instances ADD COLUMN level_rank INTEGER NOT NULL DEFAULT 0"},
		{"failure_reason", "ALTER TABLE approval_instances ADD COLUMN failure_reason TEXT NOT NULL DEFAULT ''"},
	}
	columns, err := s.tableColumns(ctx, "approval_instances")
	if err != nil {
		return err
	}
	for _, column := range required {
		if _, exists := columns[column.name]; exists {
			continue
		}
		if _, err := s.database.ExecContext(ctx, column.ddl); err != nil {
			return fmt.Errorf("add approval decision column %q: %w", column.name, err)
		}
	}
	return nil
}

func (s *Store) ensureInboxColumns(ctx context.Context) error {
	columns, err := s.tableColumns(ctx, "lark_event_inbox")
	if err != nil {
		return err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin inbox column migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, exists := columns["reverted_instance_code"]; !exists {
		if _, err := tx.ExecContext(
			ctx,
			"ALTER TABLE lark_event_inbox ADD COLUMN reverted_instance_code TEXT NOT NULL DEFAULT ''",
		); err != nil {
			return fmt.Errorf("add inbox column %q: %w", "reverted_instance_code", err)
		}
	}
	if _, exists := columns["event_origin"]; !exists {
		if _, err := tx.ExecContext(
			ctx,
			"ALTER TABLE lark_event_inbox ADD COLUMN event_origin TEXT NOT NULL DEFAULT 'webhook'",
		); err != nil {
			return fmt.Errorf("add inbox column %q: %w", "event_origin", err)
		}
	}
	if err := backfillLegacyRevertedInstanceCodes(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit inbox column migration: %w", err)
	}
	return nil
}

type legacyReversalIdentity struct {
	event              Event
	revertedInstanceID string
}

func backfillLegacyRevertedInstanceCodes(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
SELECT event_key, schema_version, event_id, event_type, app_id, tenant_key,
       approval_code, instance_code, event_status, payload_json
FROM lark_event_inbox
WHERE event_status = 'REVERTED' AND reverted_instance_code = ''`)
	if err != nil {
		return fmt.Errorf("query legacy approval reversals: %w", err)
	}
	backfills := make([]legacyReversalIdentity, 0)
	for rows.Next() {
		var identity legacyReversalIdentity
		if err := rows.Scan(
			&identity.event.Key,
			&identity.event.SchemaVersion,
			&identity.event.EventID,
			&identity.event.EventType,
			&identity.event.AppID,
			&identity.event.TenantKey,
			&identity.event.ApprovalCode,
			&identity.event.InstanceCode,
			&identity.event.Status,
			&identity.event.PayloadJSON,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan legacy approval reversal: %w", err)
		}
		var payload struct {
			RevertedInstanceCode string `json:"reverted_instance_code"`
		}
		if err := json.Unmarshal([]byte(identity.event.PayloadJSON), &payload); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode legacy approval reversal %q: %w", identity.event.Key, err)
		}
		if payload.RevertedInstanceCode == "" {
			continue
		}
		identity.revertedInstanceID = payload.RevertedInstanceCode
		identity.event.RevertedInstanceCode = payload.RevertedInstanceCode
		backfills = append(backfills, identity)
	}
	if err := closeRows(rows, "legacy approval reversals"); err != nil {
		return err
	}
	for _, backfill := range backfills {
		payloadHash, err := hashEvent(backfill.event)
		if err != nil {
			return fmt.Errorf("hash legacy approval reversal %q: %w", backfill.event.Key, err)
		}
		result, err := tx.ExecContext(ctx, `
UPDATE lark_event_inbox
SET reverted_instance_code = ?, payload_hash = ?
WHERE event_key = ? AND reverted_instance_code = ''`,
			backfill.revertedInstanceID,
			payloadHash,
			backfill.event.Key,
		)
		if err != nil {
			return fmt.Errorf("backfill legacy approval reversal %q: %w", backfill.event.Key, err)
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 1 {
			return fmt.Errorf(
				"backfill legacy approval reversal %q affected %d rows: %w",
				backfill.event.Key,
				affected,
				err,
			)
		}
	}
	return nil
}

func (s *Store) ensurePolicyVersionColumns(ctx context.Context) error {
	columns, err := s.tableColumns(ctx, "policy_versions")
	if err != nil {
		return err
	}
	if _, exists := columns["retire_after"]; exists {
		return nil
	}
	if _, err := s.database.ExecContext(
		ctx,
		"ALTER TABLE policy_versions ADD COLUMN retire_after TEXT NOT NULL DEFAULT ''",
	); err != nil {
		return fmt.Errorf("add policy version column %q: %w", "retire_after", err)
	}
	return nil
}

func (s *Store) ensureEntitlementGrantJobColumns(ctx context.Context) error {
	required := []struct {
		name string
		ddl  string
	}{
		{"response_status", "ALTER TABLE entitlement_grant_jobs ADD COLUMN response_status TEXT NOT NULL DEFAULT ''"},
		{"activated_at", "ALTER TABLE entitlement_grant_jobs ADD COLUMN activated_at TEXT NOT NULL DEFAULT ''"},
		{"response_user_id", "ALTER TABLE entitlement_grant_jobs ADD COLUMN response_user_id INTEGER NOT NULL DEFAULT 0"},
		{"result_grant_type", "ALTER TABLE entitlement_grant_jobs ADD COLUMN result_grant_type TEXT NOT NULL DEFAULT ''"},
		{"result_quota_delta", "ALTER TABLE entitlement_grant_jobs ADD COLUMN result_quota_delta INTEGER NOT NULL DEFAULT 0"},
		{"result_level_code", "ALTER TABLE entitlement_grant_jobs ADD COLUMN result_level_code TEXT NOT NULL DEFAULT ''"},
		{"result_subscription_id", "ALTER TABLE entitlement_grant_jobs ADD COLUMN result_subscription_id INTEGER NOT NULL DEFAULT 0"},
		{"result_assignment_version", "ALTER TABLE entitlement_grant_jobs ADD COLUMN result_assignment_version INTEGER NOT NULL DEFAULT 0"},
		{"result_transition", "ALTER TABLE entitlement_grant_jobs ADD COLUMN result_transition TEXT NOT NULL DEFAULT ''"},
		{"completed_at", "ALTER TABLE entitlement_grant_jobs ADD COLUMN completed_at TEXT NOT NULL DEFAULT ''"},
	}
	columns, err := s.tableColumns(ctx, "entitlement_grant_jobs")
	if err != nil {
		return err
	}
	for _, column := range required {
		if _, exists := columns[column.name]; exists {
			continue
		}
		if _, err := s.database.ExecContext(ctx, column.ddl); err != nil {
			return fmt.Errorf("add entitlement grant job column %q: %w", column.name, err)
		}
	}
	var missingActivation int64
	if err := s.database.QueryRowContext(ctx, `
SELECT COUNT(*) FROM entitlement_grant_jobs
WHERE status IN (?, ?, ?) AND activated_at = ''`,
		EntitlementGrantJobStatusPending,
		EntitlementGrantJobStatusProcessing,
		EntitlementGrantJobStatusRetryWait,
	).Scan(&missingActivation); err != nil {
		return fmt.Errorf("validate entitlement grant job activation: %w", err)
	}
	if missingActivation != 0 {
		return errors.New("active entitlement grant jobs require activated_at")
	}
	return nil
}

func (s *Store) reclassifyLegacyApprovalDecisions(ctx context.Context) error {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin legacy approval reclassification: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
UPDATE approval_instances
SET outcome = ?
WHERE outcome = ? AND policy_version = ''`,
		DecisionOutcomeShadowLegacyUnresolved,
		DecisionOutcomeShadowAuthorityVerified,
	)
	if err != nil {
		return fmt.Errorf("reclassify legacy approval decisions: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
UPDATE controller_audit
SET outcome = ?
WHERE outcome = ? AND event_key IN (
    SELECT event_key
    FROM approval_instances
    WHERE outcome = ? AND policy_version = ''
)`,
		DecisionOutcomeShadowLegacyUnresolved,
		DecisionOutcomeShadowAuthorityVerified,
		DecisionOutcomeShadowLegacyUnresolved,
	)
	if err != nil {
		return fmt.Errorf("reclassify legacy controller audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy approval reclassification: %w", err)
	}
	return nil
}

func (s *Store) tableColumns(ctx context.Context, table string) (map[string]struct{}, error) {
	var query string
	switch table {
	case "lark_event_inbox":
		query = "PRAGMA table_info(lark_event_inbox)"
	case "approval_instances":
		query = "PRAGMA table_info(approval_instances)"
	case "policy_versions":
		query = "PRAGMA table_info(policy_versions)"
	case "entitlement_grant_jobs":
		query = "PRAGMA table_info(entitlement_grant_jobs)"
	default:
		return nil, errors.New("unsupported schema inspection table")
	}
	rows, err := s.database.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	columns := make(map[string]struct{})
	for rows.Next() {
		var columnID int
		var name string
		var columnType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int
		if err := rows.Scan(&columnID, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("scan %s column: %w", table, err)
		}
		columns[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s columns: %w", table, err)
	}
	return columns, nil
}

func (s *Store) Record(ctx context.Context, event Event) (bool, error) {
	if event.Origin == "" {
		event.Origin = EventOriginWebhook
	}
	if event.Key == "" || event.EventID == "" || event.EventType == "" ||
		event.AppID == "" || event.TenantKey == "" || event.PayloadJSON == "" {
		return false, errors.New("incomplete inbox event")
	}
	if event.Origin != EventOriginWebhook && event.Origin != EventOriginApprovalReconciliation {
		return false, errors.New("invalid inbox event origin")
	}
	isReconciliationKey := strings.HasPrefix(event.Key, "lark:reconcile:")
	if (event.Origin == EventOriginApprovalReconciliation) != isReconciliationKey {
		return false, errors.New("inbox event origin does not match its key")
	}
	isPrincipalDisable := event.PrincipalDisableJob != nil
	if (event.EventType == "contact.user.deleted_v3") != isPrincipalDisable ||
		(isPrincipalDisable && event.SchemaVersion != "2.0") {
		return false, errors.New("principal disable event does not match its durable job")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	payloadHash, err := hashEvent(event)
	if err != nil {
		return false, err
	}
	const insertEvent = `
INSERT INTO lark_event_inbox (
    event_key, event_origin, schema_version, event_id, event_type, app_id, tenant_key,
    approval_code, instance_code, reverted_instance_code, event_status,
    payload_json, payload_hash, processing_state, duplicate_count,
    received_at, last_seen_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
ON CONFLICT(event_key) DO NOTHING`
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin inbox transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	initialState := ProcessingStatePending
	if isPrincipalDisable {
		initialState = ProcessingStateShadowRecorded
	}
	result, err := tx.ExecContext(ctx, insertEvent,
		event.Key, event.Origin, event.SchemaVersion, event.EventID, event.EventType,
		event.AppID, event.TenantKey, event.ApprovalCode, event.InstanceCode,
		event.RevertedInstanceCode, event.Status, event.PayloadJSON, payloadHash,
		initialState, now, now,
	)
	if err != nil {
		return false, fmt.Errorf("record inbox event: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect inbox insert: %w", err)
	}
	duplicate := affected == 0
	if !duplicate {
		if isPrincipalDisable {
			if _, err := insertPrincipalDisableJob(
				ctx,
				tx,
				event.Key,
				*event.PrincipalDisableJob,
				now,
			); err != nil {
				return false, err
			}
		} else {
			const insertJob = `
INSERT INTO jobs (event_key, job_type, status, next_attempt_at, created_at, updated_at)
VALUES (?, 'process_lark_event', ?, ?, ?, ?)`
			if _, err := tx.ExecContext(ctx, insertJob, event.Key, jobStatusPending, now, now, now); err != nil {
				return false, fmt.Errorf("create inbox job: %w", err)
			}
		}
	} else {
		var storedHash string
		if err := tx.QueryRowContext(ctx,
			"SELECT payload_hash FROM lark_event_inbox WHERE event_key = ?",
			event.Key,
		).Scan(&storedHash); err != nil {
			return false, fmt.Errorf("read duplicate inbox event: %w", err)
		}
		if storedHash != payloadHash {
			return false, ErrEventPayloadMismatch
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE lark_event_inbox
SET duplicate_count = duplicate_count + 1, last_seen_at = ?
WHERE event_key = ?`, now, event.Key); err != nil {
			return false, fmt.Errorf("record duplicate inbox event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit inbox transaction: %w", err)
	}
	return duplicate, nil
}

func hashEvent(event Event) (string, error) {
	disableExternalID := ""
	disableRequestSHA256 := ""
	disableSubjectSHA256 := ""
	if event.PrincipalDisableJob != nil {
		disableExternalID = event.PrincipalDisableJob.ExternalID
		disableRequestSHA256 = event.PrincipalDisableJob.RequestSHA256
		disableSubjectSHA256 = event.PrincipalDisableJob.SubjectSHA256
	}
	canonical, err := json.Marshal(struct {
		SchemaVersion        string `json:"schema_version"`
		EventID              string `json:"event_id"`
		EventType            string `json:"event_type"`
		AppID                string `json:"app_id"`
		TenantKey            string `json:"tenant_key"`
		ApprovalCode         string `json:"approval_code"`
		InstanceCode         string `json:"instance_code"`
		RevertedInstanceCode string `json:"reverted_instance_code,omitempty"`
		Status               string `json:"status"`
		PayloadJSON          string `json:"payload_json"`
		DisableExternalID    string `json:"disable_external_id"`
		DisableRequestSHA256 string `json:"disable_request_sha256"`
		DisableSubjectSHA256 string `json:"disable_subject_sha256"`
	}{
		SchemaVersion: event.SchemaVersion, EventID: event.EventID,
		EventType: event.EventType, AppID: event.AppID, TenantKey: event.TenantKey,
		ApprovalCode: event.ApprovalCode, InstanceCode: event.InstanceCode,
		RevertedInstanceCode: event.RevertedInstanceCode,
		Status:               event.Status, PayloadJSON: event.PayloadJSON,
		DisableExternalID: disableExternalID, DisableRequestSHA256: disableRequestSHA256,
		DisableSubjectSHA256: disableSubjectSHA256,
	})
	if err != nil {
		return "", fmt.Errorf("encode inbox event hash: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", digest[:]), nil
}

func (s *Store) Get(ctx context.Context, key string) (Event, error) {
	if key == "" {
		return Event{}, errors.New("event key is required")
	}
	const query = `
SELECT event_key, schema_version, event_id, event_type, app_id, tenant_key,
       event_origin, approval_code, instance_code, reverted_instance_code, event_status, payload_json,
       processing_state, duplicate_count, received_at, last_seen_at
FROM lark_event_inbox WHERE event_key = ?`
	var event Event
	var receivedAt string
	var lastSeenAt string
	err := s.database.QueryRowContext(ctx, query, key).Scan(
		&event.Key, &event.SchemaVersion, &event.EventID, &event.EventType,
		&event.AppID, &event.TenantKey, &event.Origin, &event.ApprovalCode, &event.InstanceCode,
		&event.RevertedInstanceCode, &event.Status, &event.PayloadJSON, &event.ProcessingState,
		&event.DuplicateCount, &receivedAt, &lastSeenAt,
	)
	if err != nil {
		return Event{}, err
	}
	event.ReceivedAt, err = time.Parse(time.RFC3339Nano, receivedAt)
	if err != nil {
		return Event{}, fmt.Errorf("parse received_at: %w", err)
	}
	event.LastSeenAt, err = time.Parse(time.RFC3339Nano, lastSeenAt)
	if err != nil {
		return Event{}, fmt.Errorf("parse last_seen_at: %w", err)
	}
	return event, nil
}

func (s *Store) Close() error {
	return s.database.Close()
}

func (s *Store) ClaimNext(ctx context.Context) (Job, bool, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, fmt.Errorf("begin job claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	const query = `
SELECT j.id, j.attempts, i.event_key, i.schema_version, i.event_id, i.event_type,
       i.app_id, i.tenant_key, i.event_origin, i.approval_code, i.instance_code,
       i.reverted_instance_code, i.event_status, i.payload_json, i.processing_state,
       i.duplicate_count, i.received_at, i.last_seen_at
FROM jobs j
JOIN lark_event_inbox i ON i.event_key = j.event_key
WHERE j.status IN (?, ?) AND julianday(j.next_attempt_at) <= julianday(?)
ORDER BY j.id
LIMIT 1`
	var job Job
	var receivedAt string
	var lastSeenAt string
	err = tx.QueryRowContext(ctx, query, jobStatusPending, jobStatusRetryWait, now).Scan(
		&job.ID, &job.Attempts, &job.Event.Key, &job.Event.SchemaVersion, &job.Event.EventID,
		&job.Event.EventType, &job.Event.AppID, &job.Event.TenantKey,
		&job.Event.Origin, &job.Event.ApprovalCode, &job.Event.InstanceCode,
		&job.Event.RevertedInstanceCode, &job.Event.Status,
		&job.Event.PayloadJSON, &job.Event.ProcessingState, &job.Event.DuplicateCount,
		&receivedAt, &lastSeenAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("select job: %w", err)
	}
	const claim = `
UPDATE jobs SET status = ?, attempts = attempts + 1, updated_at = ?
WHERE id = ? AND status IN (?, ?)`
	result, err := tx.ExecContext(
		ctx, claim, jobStatusProcessing, now, job.ID, jobStatusPending, jobStatusRetryWait,
	)
	if err != nil {
		return Job{}, false, fmt.Errorf("claim job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return Job{}, false, fmt.Errorf("claim job affected %d rows: %w", affected, err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE lark_event_inbox SET processing_state = ? WHERE event_key = ?",
		ProcessingStateProcessing, job.Event.Key,
	); err != nil {
		return Job{}, false, fmt.Errorf("mark inbox processing: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, fmt.Errorf("commit job claim: %w", err)
	}
	job.Event.ReceivedAt, _ = time.Parse(time.RFC3339Nano, receivedAt)
	job.Event.LastSeenAt, _ = time.Parse(time.RFC3339Nano, lastSeenAt)
	job.Attempts++
	return job, true, nil
}

func (s *Store) CompleteDecision(ctx context.Context, job Job, decision Decision) error {
	if job.ID <= 0 || decision.EventKey != job.Event.Key || decision.Outcome == "" {
		return errors.New("invalid shadow decision")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin decision transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	terminalJobStatus, terminalInboxState, err := terminalStatesForOutcome(decision.Outcome)
	if err != nil {
		return err
	}
	var reversal *ApprovalReversal
	if decision.ApprovalReversal != nil {
		if decision.Outcome != DecisionOutcomeReversalPending ||
			decision.EntitlementCommand != nil || decision.EntitlementGrantJob != nil {
			return errors.New("invalid approval reversal decision")
		}
		resolved, resolveErr := prepareApprovalReversal(
			ctx,
			tx,
			job.Event,
			decision,
			*decision.ApprovalReversal,
			createdAt,
		)
		if resolveErr != nil {
			return resolveErr
		}
		reversal = &resolved
		decision.FailureReason = string(resolved.Reason)
	} else if decision.Outcome == DecisionOutcomeReversalPending {
		return errors.New("reversal pending decision requires approval reversal")
	}
	const insertDecision = `
INSERT INTO approval_instances (
    event_key, approval_code, instance_code, event_status, authority_status,
	 outcome, policy_version, approval_kind, schema_fingerprint, business_code,
		 locale, catalog_sha256, quota_delta, monthly_quota, level_rank, failure_reason,
		 open_id_hash, form_sha256, start_time, reverted, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, insertDecision,
		decision.EventKey, decision.ApprovalCode, decision.InstanceCode,
		decision.EventStatus, decision.AuthorityStatus, decision.Outcome,
		decision.PolicyVersion, decision.ApprovalKind, decision.SchemaFingerprint,
		decision.BusinessCode, decision.Locale, decision.CatalogSHA256,
		decision.QuotaDelta, decision.MonthlyQuota, decision.LevelRank,
		decision.FailureReason,
		decision.OpenIDHash, decision.FormSHA256, decision.StartTime,
		decision.Reverted, createdAt,
	); err != nil {
		return fmt.Errorf("store approval decision: %w", err)
	}
	if reversal != nil {
		if err := insertApprovalReversal(ctx, tx, *reversal, createdAt); err != nil {
			return err
		}
	}
	commandOutcome := ""
	if decision.EntitlementCommand != nil {
		if err := rejectEntitlementCommandAfterReversal(ctx, tx, job.Event, decision); err != nil {
			return err
		}
		commandOutcome, err = insertEntitlementCommandShadow(
			ctx,
			tx,
			decision.EventKey,
			decision.Outcome,
			*decision.EntitlementCommand,
			createdAt,
		)
		if err != nil {
			return err
		}
	}
	if reversal != nil {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO controller_audit (event_key, action, outcome, created_at) VALUES (?, 'approval_reversal', ?, ?)",
			decision.EventKey, reversal.Result, createdAt,
		); err != nil {
			return fmt.Errorf("store approval reversal audit: %w", err)
		}
	}
	if decision.EntitlementGrantJob != nil {
		if decision.EntitlementCommand == nil {
			return errors.New("entitlement grant job requires command shadow")
		}
		if decision.Outcome != DecisionOutcomeShadowAuthorityVerified ||
			decision.EntitlementGrantJob.ExternalID != decision.EntitlementCommand.ExternalID ||
			decision.EntitlementGrantJob.RequestSHA256 != decision.EntitlementCommand.RequestSHA256 ||
			decision.EntitlementGrantJob.SubjectSHA256 != decision.EntitlementCommand.SubjectSHA256 {
			return errors.New("entitlement grant job does not match command shadow")
		}
		if _, err := insertEntitlementGrantJob(
			ctx,
			tx,
			*decision.EntitlementGrantJob,
			createdAt,
		); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO controller_audit (event_key, action, outcome, created_at) VALUES (?, 'shadow_evaluate', ?, ?)",
		decision.EventKey, decision.Outcome, createdAt,
	); err != nil {
		return fmt.Errorf("store controller audit: %w", err)
	}
	fetchResult := ""
	if decision.AuthorityStatus != "" {
		fetchResult = "success"
	} else if decision.Outcome == DecisionOutcomeDeadLetterApprovalFetch {
		fetchResult = "terminal_error"
		if strings.HasPrefix(decision.FailureReason, "retry_exhausted_") {
			fetchResult = "retryable_error"
		}
	} else if reversal != nil {
		switch reversal.Result {
		case ApprovalReversalResultFetchTerminalError:
			fetchResult = "terminal_error"
		case ApprovalReversalResultFetchRetryExhausted:
			fetchResult = "retryable_error"
		}
	}
	if fetchResult != "" {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO controller_audit (event_key, action, outcome, created_at) VALUES (?, 'approval_fetch', ?, ?)",
			decision.EventKey, fetchResult, createdAt,
		); err != nil {
			return fmt.Errorf("store approval fetch audit: %w", err)
		}
	}
	if decision.EntitlementCommand != nil {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO controller_audit (event_key, action, outcome, created_at) VALUES (?, 'new_api_grant', ?, ?)",
			decision.EventKey, commandOutcome, createdAt,
		); err != nil {
			return fmt.Errorf("store New API grant shadow audit: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `
UPDATE jobs SET status = ?, last_error = ?, updated_at = ?
WHERE id = ? AND status = ? AND attempts = ?`,
		terminalJobStatus,
		decision.FailureReason,
		createdAt,
		job.ID,
		jobStatusProcessing,
		job.Attempts,
	)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect completed job: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("complete job affected %d rows", affected)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE lark_event_inbox SET processing_state = ? WHERE event_key = ?",
		terminalInboxState, decision.EventKey,
	); err != nil {
		return fmt.Errorf("complete inbox event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit decision: %w", err)
	}
	return nil
}

func terminalStatesForOutcome(outcome DecisionOutcome) (jobStatus, ProcessingState, error) {
	switch outcome {
	case DecisionOutcomeShadowAuthorityVerified,
		DecisionOutcomeShadowAuthorityRejected,
		DecisionOutcomeShadowIgnoredNonApproved:
		return jobStatusSucceeded, ProcessingStateShadowRecorded, nil
	case DecisionOutcomeReversalPending:
		return jobStatusReversalPending, ProcessingStateReversalPending, nil
	case DecisionOutcomeDeadLetterUnknownStatus,
		DecisionOutcomeDeadLetterUnsupportedEventType,
		DecisionOutcomeDeadLetterPolicyValidation,
		DecisionOutcomeDeadLetterApprovalFetch,
		DecisionOutcomeDeadLetterCommandPlanning:
		return jobStatusDeadLetter, ProcessingStateDeadLetter, nil
	default:
		return "", "", fmt.Errorf("unknown shadow decision outcome %q", outcome)
	}
}

func (s *Store) Retry(ctx context.Context, job Job, reason string, delay time.Duration) error {
	if job.ID <= 0 || reason == "" || delay <= 0 {
		return errors.New("invalid job retry")
	}
	now := time.Now().UTC()
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin job retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
UPDATE jobs SET status = ?, next_attempt_at = ?, last_error = ?, updated_at = ?
WHERE id = ? AND status = ? AND attempts = ?`,
		jobStatusRetryWait, now.Add(delay).Format(time.RFC3339Nano), reason,
		now.Format(time.RFC3339Nano), job.ID, jobStatusProcessing, job.Attempts,
	)
	if err != nil {
		return fmt.Errorf("schedule job retry: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect scheduled job retry: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("schedule job retry affected %d rows", affected)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE lark_event_inbox SET processing_state = ? WHERE event_key = ?",
		ProcessingStatePending, job.Event.Key,
	); err != nil {
		return fmt.Errorf("mark retrying inbox event pending: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO controller_audit (event_key, action, outcome, created_at) VALUES (?, 'approval_fetch', 'retryable_error', ?)",
		job.Event.Key, now.Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("audit approval fetch retry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit job retry: %w", err)
	}
	return nil
}

func (s *Store) GetDecision(ctx context.Context, eventKey string) (Decision, error) {
	const query = `
SELECT event_key, approval_code, instance_code, event_status, authority_status,
       outcome, policy_version, approval_kind, schema_fingerprint, business_code,
		 locale, catalog_sha256, quota_delta, monthly_quota, level_rank, failure_reason,
		 open_id_hash, form_sha256, start_time, reverted, created_at
FROM approval_instances WHERE event_key = ?`
	var decision Decision
	var createdAt string
	err := s.database.QueryRowContext(ctx, query, eventKey).Scan(
		&decision.EventKey, &decision.ApprovalCode, &decision.InstanceCode,
		&decision.EventStatus, &decision.AuthorityStatus, &decision.Outcome,
		&decision.PolicyVersion, &decision.ApprovalKind, &decision.SchemaFingerprint,
		&decision.BusinessCode, &decision.Locale, &decision.CatalogSHA256,
		&decision.QuotaDelta, &decision.MonthlyQuota, &decision.LevelRank,
		&decision.FailureReason,
		&decision.OpenIDHash, &decision.FormSHA256, &decision.StartTime,
		&decision.Reverted, &createdAt,
	)
	if err != nil {
		return Decision{}, err
	}
	decision.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Decision{}, fmt.Errorf("parse decision created_at: %w", err)
	}
	return decision, nil
}
