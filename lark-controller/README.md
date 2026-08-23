# Lark quota controller

This module is the isolated Lark integration controller described in
`../docs/architecture/lark-entitlement-integration.md`.

The controller supports a locally verified `shadow` mode and an explicit
`active` integration-write mode. It:

- verifies and decrypts Lark v2 webhooks and validates v1 callbacks;
- accepts `contact.user.deleted_v3`, persists only a subject hash in the
  ordinary inbox payload, and atomically creates a sealed principal-disable
  job before acknowledging the event;
- in active mode, runs a scheduled employment reconciliation over every active
  Lark principal exposed by New API, with a known-active Lark health probe
  before and after each complete paginated scan; every Lark lookup is serial and
  its request start is spaced by at least 100 milliseconds;
- creates an eventless sealed principal-disable job for authoritative
  `is_resigned` or `is_exited` results, while `41012` requires two healthy
  complete scans at least 24 hours apart; present results clear prior missing
  evidence, and permission, rate-limit, server, health, or pagination failures
  never advance it;
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
  remaining keys decrypt grant and principal-disable jobs created before
  rotation;
- records same-payload external-ID reuse as `shadow_replayed` and dead-letters
  payload mismatches without replacing the first shadow ledger entry;
- persists 256-bit OAuth state, login-code, and access-handle credentials only
  by SHA-256 digest, with atomic single-use consumption and fixed five-minute
  or 60-second expiry windows; consumption atomically deletes the row, while
  indexed expiry pruning is amortized to at most once per minute and each
  credential stage has an independent hard row limit (10,000 states, 5,000
  login codes, and 5,000 access handles);
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
- exposes the public OAuth authorize/callback bridge with exact New API client
  and callback validation, exact-GET semantics, single-use state consumption,
  redacted authorization-error mapping, a bounded callback context, and a
  callback-only write-deadline extension that preserves the webhook
  acknowledgement budget;
- exposes internal token/userinfo endpoints that authenticate the fixed New API
  bridge client, atomically exchange the 60-second login code for a second
  60-second opaque bearer handle, and return only `sub`, `username`, and `name`;
- resolves `basic` from the unique active policy and, before userinfo succeeds,
  atomically consumes the handle and creates or reuses the deterministic
  `lark:base:<subject>:<policy_version>` ledger, sealed `held_shadow` job, and
  separate base-subscription audit record;
- keeps monthly quota and approval evidence out of the `base_login` wire request,
  binds its subject hash to the consumed identity, preserves the first sealed
  payload on replay, and rejects catalog or quota metadata drift;
- applies separate per-client fixed-window limits to OAuth authorize and
  callback, token, and userinfo, caps state issuance at 20 per client per five
  minutes and 500 globally per minute, and groups IPv6 clients by `/64`;
  forwarded client addresses are used only when the immediate peer belongs to
  an explicitly configured trusted proxy CIDR;
- classifies Approval v4 failures, honors bounded `Retry-After`, and applies a
  six-step jittered retry schedule before durable dead-lettering;
- recovers interrupted jobs with their attempt counters after restart;
- activates held jobs only after the active startup gate has validated the
  keyring, New API credential/client, SQLite state, webhook server, and listen
  socket, then decrypts canonical grant and principal-disable requests,
  persists sanitized results, and applies the documented retry/dead-letter
  matrices;
- exposes liveness, readiness, and bounded-label Prometheus metrics for inbox,
  jobs, approval fetches, New API shadow grants, held grant jobs, policy
  failures, principal disable jobs, dead letters, and queue age;
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
NEW_API_BRIDGE_CLIENT_ID=...
NEW_API_BRIDGE_CLIENT_SECRET_FILE=/run/secrets/new_api_bridge_client_secret
NEW_API_OAUTH_CALLBACK_ALLOWLIST=https://ai.x2r.store/oauth/lark
```

Active mode additionally requires:

```text
LARK_CONTROLLER_MODE=active
NEW_API_INTERNAL_BASE_URL=http://new-api:3001
LARK_INTEGRATION_SECRET_FILE=/run/secrets/lark_integration_secret
LARK_RECONCILIATION_HEALTH_OPEN_ID=ou_known_active_employee
```

The integration secret file contains one printable, non-whitespace ASCII token
of at least 32 bytes, with an optional LF or CRLF ending. It is a dedicated
narrow-scope integration credential, not a New API administrator token.

`LARK_RECONCILIATION_HEALTH_OPEN_ID` must be a stable, known-active member in
the configured Lark tenant and application availability scope. If either the
pre-scan or post-scan probe is not present, the run is audited as failed and no
employment evidence or disable job is committed.

The bridge client secret file contains one printable, non-whitespace ASCII
token between 32 and 4096 bytes, with an optional LF or CRLF ending. Configure
that same token as the Lark Custom OAuth provider client secret in New API; do
not use the Lark App Secret or the New API integration credential.

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
LARK_OAUTH_RATE_LIMIT_PER_MINUTE=30
LARK_OAUTH_TRUSTED_PROXY_CIDRS=172.31.20.0/24
LARK_RECONCILIATION_INTERVAL=24h
```

The configured OAuth rate limit is applied independently to authorize,
callback, token, and userinfo for each resolved client address. Authorize state
issuance has additional hard limits of 20 states per resolved client per five
minutes and 500 states globally per minute. IPv4 clients are keyed by address
and IPv6 clients by masked `/64`. A rejected admission or failed state write
rolls back any reserved issuance capacity, and `Retry-After` reflects the
limiter window that rejected the request. `X-Forwarded-For` is ignored unless
the direct peer is covered by `LARK_OAUTH_TRUSTED_PROXY_CIDRS`; configure only
the exact proxy network that reaches the controller. The initial callback
allowlist is intentionally a single fixed URL and rejects additional or
prefix-matching entries.

`LARK_RECONCILIATION_INTERVAL` is active-mode only, defaults to `24h`, and must
be between `24h` and `168h`. A failed run is retried after at least 15 minutes;
a longer Lark `Retry-After` is honored up to the normal reconciliation interval.
Completed runs are idempotent by UTC evidence date, and Prometheus exposes
their append-only outcomes through `employment_reconciliation_total{result}`.

The internal token endpoint accepts only `POST` with an at-most-16-KiB
`application/x-www-form-urlencoded` body containing exactly one non-empty
`grant_type`, `code`, `redirect_uri`, `client_id`, and `client_secret`. Configure
New API Custom OAuth `auth_style` as params (`1`). A successful exchange returns
only `access_token`, `token_type: Bearer`, and `expires_in: 60`. The internal
userinfo endpoint accepts only exact `GET` with one bearer authorization header
and no query, then returns only `sub`, `username`, and `name`. Both endpoints
reject `HEAD` before limiting or credential consumption and always send
`Cache-Control: no-store`.

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
grants, principal disable, and paginated active-Lark-principal enumeration.
Both write adapters use only the dedicated integration bearer credential,
validate bounded responses, and classify response loss as retryable because
the external ID is idempotent. The principal response intentionally contains
no New API user ID, wallet balance, token, or subscription details. In shadow mode,
`cmd/lark-controller` neither loads the integration credential nor constructs
the client/executor, so it performs no New API request. In active mode, startup
preflights the credential and client before opening SQLite, then binds the HTTP
listener and validates every nonterminal grant and principal-disable job key
before releasing the held approval/disable backlog and only the base jobs for
the current active policy. The active runtimes apply the same gates to newly
created held jobs before claiming them. Policy snapshot sync and the grant
runtime startup/release gate reject any nonterminal base job from a non-active
policy, including an already released or restart-recovered job; a policy switch
cannot proceed until the documented drain or explicit migration is complete.
`held_shadow` jobs remain excluded from ready-queue age.

Principal-disable jobs use an independent durable ledger and audit table. A
real-time contact event binds its optional inbox event key, while the job model
also permits an employment-reconciliation command without fabricating a webhook
event. Duplicate delivery preserves the first key ID, nonce, and ciphertext.
Active execution calls the idempotent New API disable endpoint with
`lark:disable:<event_id>` for contact events or
`lark:disable-reconcile:<tenant_key>:<open_id>:<evidence_date>` for completed
employment evidence, retries transport/temporary failures with the same
request, recovers claimed jobs after restart, and persists only the bounded
status, outcome, principal version, and auth version receipt. Reconciliation
stores subject hashes, stable result classes, Lark result codes, permission
health, actual lookup completion times, and evidence hashes; it does not store
raw employee identities.

The active policy must define a positive `basic` level before the OAuth bridge
can start. Userinfo handle deletion, the base-subscription ledger row, the
sealed grant job, and its audit row commit in one SQLite transaction. Planning,
sealing, validation, replay mismatch, or database failure rolls the transaction
back so New API can retry the same handle. Separate employee logins reuse the
first job only when request hash, subject hash, policy version, catalog hash,
level, and monthly quota all match; replay never replaces its key ID, nonce, or
ciphertext. Base-login retry, result, and dead-letter audits feed the same
bounded operational metrics as approval grants without fabricating a webhook
event.

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
Active grant and real-time principal-disable execution, the durable opaque
OAuth credential store, the outbound Lark token/userinfo adapter, and the
public OAuth authorize/callback handlers, internal token/userinfo handlers,
and idempotent base-subscription dispatch are implemented locally. Employment
reconciliation is also implemented locally. Compose wiring, operational
runbooks, and production validation remain follow-up work. Do not enable active
mode in production before those gates are complete.
