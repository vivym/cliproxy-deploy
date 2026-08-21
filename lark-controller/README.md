# Lark quota controller

This module is the isolated Lark integration controller described in
`../docs/architecture/lark-entitlement-integration.md`.

The controller supports a locally verified `shadow` mode and an explicit
`active` grant mode. It:

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
- plans the exact New API entitlement-grant contract and atomically stores its
  sanitized receipt plus an AES-256-GCM sealed canonical request in a durable
  `held_shadow` grant job;
- loads a rotation-safe payload keyring whose first key seals new jobs and whose
  remaining keys decrypt jobs created before rotation;
- records same-payload external-ID reuse as `shadow_replayed` and dead-letters
  payload mismatches without replacing the first shadow ledger entry;
- persists 256-bit OAuth state, login-code, and access-handle credentials only
  by SHA-256 digest, with atomic single-use consumption and fixed five-minute
  or 60-second expiry windows;
- exchanges Lark OAuth v3 authorization codes with the required JSON contract,
  immediately reads userinfo with the user bearer token, and returns only the
  normalized `tenant_key:open_id` identity, deterministic username, and a
  20-code-point display name;
- bounds both OAuth responses at 64 KiB and exposes only stable failure reasons;
  it never returns, persists, or logs Lark access/refresh tokens or response
  descriptions;
- requires the registered Controller callback, permits only HTTPS upstream
  origins (plus loopback HTTP for tests), and refuses redirects so credentials
  cannot be replayed to another endpoint;
- classifies Approval v4 failures, honors bounded `Retry-After`, and applies a
  six-step jittered retry schedule before durable dead-lettering;
- recovers interrupted jobs with their attempt counters after restart;
- activates held jobs only after the active startup gate has validated the
  keyring, New API credential/client, SQLite state, webhook server, and listen
  socket, then decrypts canonical requests, persists sanitized results, and
  applies the documented retry/dead-letter matrix;
- exposes liveness, readiness, and bounded-label Prometheus metrics for inbox,
  jobs, approval fetches, New API shadow grants, held grant jobs, policy
  failures, dead letters, and queue age;
- keeps queryable columns limited to normalized event data, hashes, decisions,
  stable failure reasons, and sanitized New API receipts; the complete grant
  request exists only as authenticated ciphertext; and
- in shadow mode, does not read New API configuration, construct its
  client/executor, release held jobs, or send New API requests.

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
LARK_GRANT_PAYLOAD_KEYRING_FILE=/run/secrets/lark_grant_payload_keyring
```

Active mode additionally requires:

```text
LARK_CONTROLLER_MODE=active
NEW_API_INTERNAL_BASE_URL=http://new-api:3001
LARK_INTEGRATION_SECRET_FILE=/run/secrets/lark_integration_secret
```

The integration secret file contains one printable, non-whitespace ASCII token
of at least 32 bytes, with an optional LF or CRLF ending. It is a dedicated
narrow-scope integration credential, not a New API administrator token.

The grant payload keyring file contains one or more 64-character lowercase hex
lines, with an optional final line ending. Use LF or CRLF consistently; mixed
line endings and bare CR are rejected. The first line is the primary key for
new jobs; later lines are decrypt-only keys retained from earlier rotations.
Generate the initial line with
`openssl rand -hex 32 > /secure/path/lark_grant_payload_keyring` and keep the
file out of Git. To rotate, atomically install a file with a newly generated key
as the first line followed by every existing line, then restart the controller
and require its startup gate to pass before resuming service. Do not remove an
older line until no nonterminal grant job uses it. Retire an old key by
atomically installing the reduced file and restarting through the same gate.
Startup checks every nonterminal job and fails closed before starting the
worker if its key is unavailable. Succeeded and dead-letter jobs do not block
key retirement.

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

The active grant executor uses `5s, 15s, 1m, 5m, 15m, 1h` for retryable New API
transport, timeout, and `temporarily_unavailable` failures. Once that
schedule is exhausted the job is dead-lettered. `principal_not_ready` repeats
the final one-hour delay until 24 hours after explicit release, then becomes a
terminal `retry_exhausted_principal_not_ready`. Response loss is retried with
the same external ID, so New API can return its immutable `replayed` result.

Operational endpoints:

```text
GET /healthz  process liveness; does not depend on SQLite
GET /readyz   SQLite availability and age of jobs ready to run
GET /metrics  Prometheus text format with bounded label values
```

`LARK_READINESS_MAX_QUEUE_AGE` controls when an already-due event or released
grant job is considered stalled. A future `retry_wait` job does not fail
readiness merely because the upstream asked the controller to wait.
Released grant age starts at explicit activation; time spent in `held_shadow`
is excluded. Dead letters remain visible in metrics but do not disable webhook
ingestion.

`internal/newapi` implements the versioned HTTP contracts for entitlement
grants and paginated active-Lark-principal enumeration. The grant adapter uses
only the dedicated integration bearer credential, validates bounded responses,
and classifies response loss as retryable because the external ID is
idempotent. The principal response intentionally contains no New API user ID,
wallet balance, token, or subscription details. In shadow mode,
`cmd/lark-controller` neither loads the integration credential nor constructs
the client/executor, so it performs no New API request. In active mode, startup
preflights the credential and client before opening SQLite, then binds the HTTP
listener and validates every nonterminal job key before releasing the held
backlog. The active grant runtime also releases newly created held jobs before
claiming them. `held_shadow` jobs remain excluded from the event-worker claim
path and from ready-queue age.

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
Active grant execution and the durable opaque OAuth credential store are
implemented locally, as is the outbound Lark token/userinfo adapter. The OAuth
authorize/callback/token/userinfo HTTP handlers, base-subscription dispatch,
employment reconciliation, Compose wiring, operational runbooks, and
production validation remain follow-up work. Do not enable active mode in
production before those gates are complete.
