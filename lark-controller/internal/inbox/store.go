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
	"time"

	_ "modernc.org/sqlite"
)

type Event struct {
	Key             string
	SchemaVersion   string
	EventID         string
	EventType       string
	AppID           string
	TenantKey       string
	ApprovalCode    string
	InstanceCode    string
	Status          string
	PayloadJSON     string
	ProcessingState string
	DuplicateCount  int64
	ReceivedAt      time.Time
	LastSeenAt      time.Time
}

type Store struct {
	database *sql.DB
}

var ErrEventPayloadMismatch = errors.New("event id payload mismatch")

type Job struct {
	ID    int64
	Event Event
}

type Decision struct {
	EventKey        string
	ApprovalCode    string
	InstanceCode    string
	EventStatus     string
	AuthorityStatus string
	Outcome         string
	OpenIDHash      string
	FormSHA256      string
	StartTime       string
	Reverted        bool
	CreatedAt       time.Time
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("sqlite path is required")
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)",
	}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	database.SetMaxOpenConns(1)
	store := &Store{database: database}
	if err := store.migrate(context.Background()); err != nil {
		_ = database.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS lark_event_inbox (
    event_key TEXT PRIMARY KEY,
    schema_version TEXT NOT NULL,
    event_id TEXT NOT NULL,
    event_type TEXT NOT NULL,
    app_id TEXT NOT NULL,
    tenant_key TEXT NOT NULL,
    approval_code TEXT NOT NULL DEFAULT '',
    instance_code TEXT NOT NULL DEFAULT '',
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
    open_id_hash TEXT NOT NULL DEFAULT '',
    form_sha256 TEXT NOT NULL DEFAULT '',
    start_time TEXT NOT NULL DEFAULT '',
    reverted INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS controller_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_key TEXT NOT NULL REFERENCES lark_event_inbox(event_key) ON DELETE CASCADE,
    action TEXT NOT NULL,
    outcome TEXT NOT NULL,
    created_at TEXT NOT NULL
);
UPDATE jobs SET status = 'pending', updated_at = next_attempt_at
WHERE status = 'processing';
`
	if _, err := s.database.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate inbox: %w", err)
	}
	return nil
}

func (s *Store) Record(ctx context.Context, event Event) (bool, error) {
	if event.Key == "" || event.EventID == "" || event.EventType == "" ||
		event.AppID == "" || event.TenantKey == "" || event.PayloadJSON == "" {
		return false, errors.New("incomplete inbox event")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	payloadHash, err := hashEvent(event)
	if err != nil {
		return false, err
	}
	const insertEvent = `
INSERT INTO lark_event_inbox (
    event_key, schema_version, event_id, event_type, app_id, tenant_key,
    approval_code, instance_code, event_status, payload_json, payload_hash,
    processing_state, duplicate_count, received_at, last_seen_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0, ?, ?)
ON CONFLICT(event_key) DO NOTHING`
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin inbox transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, insertEvent,
		event.Key, event.SchemaVersion, event.EventID, event.EventType,
		event.AppID, event.TenantKey, event.ApprovalCode, event.InstanceCode,
		event.Status, event.PayloadJSON, payloadHash, now, now,
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
		const insertJob = `
INSERT INTO jobs (event_key, job_type, status, next_attempt_at, created_at, updated_at)
VALUES (?, 'process_lark_event', 'pending', ?, ?, ?)`
		if _, err := tx.ExecContext(ctx, insertJob, event.Key, now, now, now); err != nil {
			return false, fmt.Errorf("create inbox job: %w", err)
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
	canonical, err := json.Marshal(struct {
		SchemaVersion string `json:"schema_version"`
		EventID       string `json:"event_id"`
		EventType     string `json:"event_type"`
		AppID         string `json:"app_id"`
		TenantKey     string `json:"tenant_key"`
		ApprovalCode  string `json:"approval_code"`
		InstanceCode  string `json:"instance_code"`
		Status        string `json:"status"`
		PayloadJSON   string `json:"payload_json"`
	}{
		SchemaVersion: event.SchemaVersion, EventID: event.EventID,
		EventType: event.EventType, AppID: event.AppID, TenantKey: event.TenantKey,
		ApprovalCode: event.ApprovalCode, InstanceCode: event.InstanceCode,
		Status: event.Status, PayloadJSON: event.PayloadJSON,
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
       approval_code, instance_code, event_status, payload_json,
       processing_state, duplicate_count, received_at, last_seen_at
FROM lark_event_inbox WHERE event_key = ?`
	var event Event
	var receivedAt string
	var lastSeenAt string
	err := s.database.QueryRowContext(ctx, query, key).Scan(
		&event.Key, &event.SchemaVersion, &event.EventID, &event.EventType,
		&event.AppID, &event.TenantKey, &event.ApprovalCode, &event.InstanceCode,
		&event.Status, &event.PayloadJSON, &event.ProcessingState,
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
SELECT j.id, i.event_key, i.schema_version, i.event_id, i.event_type,
       i.app_id, i.tenant_key, i.approval_code, i.instance_code,
       i.event_status, i.payload_json, i.processing_state,
       i.duplicate_count, i.received_at, i.last_seen_at
FROM jobs j
JOIN lark_event_inbox i ON i.event_key = j.event_key
WHERE j.status IN ('pending', 'retry_wait') AND j.next_attempt_at <= ?
ORDER BY j.id
LIMIT 1`
	var job Job
	var receivedAt string
	var lastSeenAt string
	err = tx.QueryRowContext(ctx, query, now).Scan(
		&job.ID, &job.Event.Key, &job.Event.SchemaVersion, &job.Event.EventID,
		&job.Event.EventType, &job.Event.AppID, &job.Event.TenantKey,
		&job.Event.ApprovalCode, &job.Event.InstanceCode, &job.Event.Status,
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
UPDATE jobs SET status = 'processing', attempts = attempts + 1, updated_at = ?
WHERE id = ? AND status IN ('pending', 'retry_wait')`
	result, err := tx.ExecContext(ctx, claim, now, job.ID)
	if err != nil {
		return Job{}, false, fmt.Errorf("claim job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return Job{}, false, fmt.Errorf("claim job affected %d rows: %w", affected, err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE lark_event_inbox SET processing_state = 'processing' WHERE event_key = ?",
		job.Event.Key,
	); err != nil {
		return Job{}, false, fmt.Errorf("mark inbox processing: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, fmt.Errorf("commit job claim: %w", err)
	}
	job.Event.ReceivedAt, _ = time.Parse(time.RFC3339Nano, receivedAt)
	job.Event.LastSeenAt, _ = time.Parse(time.RFC3339Nano, lastSeenAt)
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
	jobStatus := "succeeded"
	inboxState := "shadow_recorded"
	switch {
	case decision.Outcome == "reversal_pending":
		jobStatus = "reversal_pending"
		inboxState = "reversal_pending"
	case strings.HasPrefix(decision.Outcome, "dead_letter_"):
		jobStatus = "dead_letter"
		inboxState = "dead_letter"
	}
	const insertDecision = `
INSERT INTO approval_instances (
    event_key, approval_code, instance_code, event_status, authority_status,
    outcome, open_id_hash, form_sha256, start_time, reverted, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, insertDecision,
		decision.EventKey, decision.ApprovalCode, decision.InstanceCode,
		decision.EventStatus, decision.AuthorityStatus, decision.Outcome,
		decision.OpenIDHash, decision.FormSHA256, decision.StartTime,
		decision.Reverted, createdAt,
	); err != nil {
		return fmt.Errorf("store approval decision: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO controller_audit (event_key, action, outcome, created_at) VALUES (?, 'shadow_evaluate', ?, ?)",
		decision.EventKey, decision.Outcome, createdAt,
	); err != nil {
		return fmt.Errorf("store controller audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE jobs SET status = ?, last_error = '', updated_at = ? WHERE id = ? AND status = 'processing'",
		jobStatus, createdAt, job.ID,
	); err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE lark_event_inbox SET processing_state = ? WHERE event_key = ?",
		inboxState, decision.EventKey,
	); err != nil {
		return fmt.Errorf("complete inbox event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit decision: %w", err)
	}
	return nil
}

func (s *Store) Retry(ctx context.Context, job Job, processErr error, delay time.Duration) error {
	if job.ID <= 0 || processErr == nil || delay <= 0 {
		return errors.New("invalid job retry")
	}
	now := time.Now().UTC()
	_, err := s.database.ExecContext(ctx, `
UPDATE jobs SET status = 'retry_wait', next_attempt_at = ?, last_error = ?, updated_at = ?
WHERE id = ? AND status = 'processing'`,
		now.Add(delay).Format(time.RFC3339Nano), processErr.Error(), now.Format(time.RFC3339Nano), job.ID,
	)
	if err != nil {
		return fmt.Errorf("schedule job retry: %w", err)
	}
	return nil
}

func (s *Store) GetDecision(ctx context.Context, eventKey string) (Decision, error) {
	const query = `
SELECT event_key, approval_code, instance_code, event_status, authority_status,
       outcome, open_id_hash, form_sha256, start_time, reverted, created_at
FROM approval_instances WHERE event_key = ?`
	var decision Decision
	var createdAt string
	err := s.database.QueryRowContext(ctx, query, eventKey).Scan(
		&decision.EventKey, &decision.ApprovalCode, &decision.InstanceCode,
		&decision.EventStatus, &decision.AuthorityStatus, &decision.Outcome,
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
