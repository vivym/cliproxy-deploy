# Lark quota controller

This module is the isolated Lark integration controller described in
`../docs/architecture/lark-entitlement-integration.md`.

The current tracer slice is deliberately shadow-only. It:

- verifies and decrypts Lark v2 webhooks and validates v1 callbacks;
- validates the configured Verification Token, application ID, and tenant key;
- durably deduplicates v1 `uuid` and v2 `event_id` values in single-replica
  SQLite WAL storage;
- fetches authoritative Approval v4 instances for `APPROVED` and
  `OVERTIME_RECOVER` events;
- loads immutable historical policy catalogs and approval definition manifests
  from operator-mounted strict JSON files;
- resolves `radioV2` values by exact display text and persists only sanitized
  shadow evidence;
- plans the exact New API entitlement-grant contract and atomically stores only
  its external ID, policy fields, business values, and request/subject hashes;
- records same-payload external-ID reuse as `shadow_replayed` and dead-letters
  payload mismatches without replacing the first shadow ledger entry;
- classifies Approval v4 failures, honors bounded `Retry-After`, and applies a
  six-step jittered retry schedule before durable dead-lettering;
- recovers interrupted jobs with their attempt counters after restart;
- exposes liveness, readiness, and bounded-label Prometheus metrics for inbox,
  jobs, approval fetches, New API shadow grants, policy failures, dead letters,
  and queue age;
- stores only normalized event data, requester/form hashes, and shadow
  decisions; and
- does not configure, construct, or call the included New API HTTP client.

Required environment variables:

```text
LARK_CONTROLLER_MODE=shadow
LARK_CONTROLLER_DB_PATH=/data/controller.sqlite
LARK_APP_ID=...
LARK_APP_SECRET=...
LARK_VERIFICATION_TOKEN=...
LARK_EVENT_ENCRYPT_KEY=...
LARK_TENANT_KEY=...
LARK_ACTIVE_POLICY_VERSION=...
LARK_POLICY_BUNDLE_DIR=/policies
LARK_APPROVAL_BINDINGS_FILE=/policies/approval-bindings.json
```

Optional variables:

```text
LARK_CONTROLLER_LISTEN_ADDR=0.0.0.0:8080
LARK_APPROVAL_LOCALE=zh-CN
LARK_READINESS_MAX_QUEUE_AGE=15m
```

Approval fetches retry after `5s, 15s, 1m, 5m, 15m, 1h`, with deterministic
20 percent jitter. A valid Lark `Retry-After` takes precedence and is capped at
24 hours. A seventh failed attempt is terminal. HTTP `408`, `429`, `5xx`, Lark
business code `99991400`, request timeouts, and transport failures are
retryable; other `4xx`, token rejection, malformed success responses, and
unclassified failures are terminal. Only stable failure reasons are persisted.

Operational endpoints:

```text
GET /healthz  process liveness; does not depend on SQLite
GET /readyz   SQLite availability and age of jobs ready to run
GET /metrics  Prometheus text format with bounded label values
```

`LARK_READINESS_MAX_QUEUE_AGE` controls when an already-due job is considered
stalled. A future `retry_wait` job does not fail readiness merely because Lark
asked the controller to wait. Dead letters remain visible in metrics but do not
disable webhook ingestion.

`internal/newapi` implements the versioned HTTP contracts for entitlement
grants and paginated active-Lark-principal enumeration. The grant adapter uses
only the dedicated integration bearer credential, validates bounded responses,
and classifies response loss as retryable because the external ID is
idempotent. The principal response intentionally contains no New API user ID,
wallet balance, token, or subscription details. This module is contract-tested
with local HTTP servers but is not wired into `cmd/lark-controller`; shadow
mode has no New API URL or credential setting and performs no New API network
request.

The policy directory has no built-in defaults. Operators must mount one or
more files named `*.policy.json`; every file uses `format_version: 1` and
contains one immutable `policy_version`, its lifecycle `state`, versioned
`levels`, and versioned `wallet_packages`. Exactly one loaded version must be
`active`, and it must equal `LARK_ACTIVE_POLICY_VERSION`. A `level_code` keeps
the same rank across every loaded version. All policy, binding, and approval
form JSON rejects unknown fields, duplicate object keys, and trailing data.

The bindings file uses `format_version: 1` and retains active, draining, and
retired approval definitions. Each binding contains:

```text
approval_code + locale + policy_version + approval_kind
schema_fingerprint + optional accept_instance_started_before
manifest.approval_kind + manifest.locale + manifest.fields
```

Every loaded policy version has exactly one `wallet_topup` and one
`subscription_level` definition in `zh-CN`. New definitions can only be added
with a new policy version; an existing definition may only close its acceptance
window.

Manifest fields must be sorted by `custom_id`. Every field includes `type`,
`required`, and an explicit `options` array. A `radioV2` option contains its
exact `display_text` and stable business `code`; display text is never trimmed,
parsed as an amount, or matched approximately. The canonical manifest is
compact JSON with manifest keys `approval_kind, locale, fields`, field keys
`custom_id, type, required, options`, and option keys `display_text, code`.
Fields are sorted by `custom_id`; options remain in declared order.
`schema_fingerprint` is `sha256:<hex>` over those canonical bytes. The fixed
test vectors in `internal/policy/catalog_test.go` are the publishing contract.
Auxiliary `radioV2` controls are validated against their own exact option maps,
but only `wallet_package` or `target_level` supplies the entitlement code.

Policy state may only move `active -> draining -> retired`. Closing a binding
adds one RFC3339 `accept_instance_started_before` value; an existing cutoff
cannot be changed or removed. SQLite snapshots reject removed history or
mutated catalog/manifest content at startup. A retired bundle also requires an
RFC3339 `retire_after` that is later than every binding cutoff and already in
the past. The transition is rejected while any matching local job is pending,
processing, waiting for retry, or awaiting reversal. Operators set this value
only after Lark confirms all instances are terminal and the trace window has
expired. Decisions created before policy validation existed are relabeled
`shadow_authority_verified_legacy_unresolved`; they are never treated as
policy-validated evidence.

Run local checks from this directory:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/lark-controller
```

This slice does not add the service to Docker Compose and must not be deployed.
OAuth, employment reconciliation, New API runtime wiring/retry, and all
entitlement mutation remain follow-up WP3 slices. There is still no active
entitlement path.
