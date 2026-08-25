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
- fetches authoritative Approval v4 instances for `APPROVED`,
  `OVERTIME_RECOVER`, and `REVERTED` events;
- in both modes, scans every active or draining approval binding in at-most-
  10-hour creation windows, persists a monotonic cursor per `approval_code`,
  overlaps resumed scans by ten minutes, and creates deterministic internal
  inbox events only from authoritative `APPROVED` or `reverted=true` evidence;
- rechecks grant-backed approvals under non-retired policies for late
  reversals, routes them through the same deny fence and manual-correction
  semantics as webhook reversals, and keeps internal reconciliation events out
  of `lark_webhook_*` counters;
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
- resolves a `REVERTED` event only by its explicit `reverted_instance_code`, or
  by the event's exact `instance_code` when the explicit relation is absent;
  exact approval/instance matching and authoritative `reverted=true` are
  required before a validated original command can be resolved;
- records sanitized reversal evidence and stable results in
  `approval_reversals`, atomically moves an original `held_shadow`, `pending`,
  `processing`, or `retry_wait` grant job to `reversal_pending`, and uses the
  existing status/attempt fence to reject a late worker completion; it never
  subtracts wallet quota or downgrades a subscription automatically;
- provides the separate one-shot `cmd/lark-correction` workflow: `--list-pending`
  discovers durable reversals through a SQLite read-only connection without
  migration, processing recovery, or raw SQLite; the default mode previews
  sanitized current state, and only `--apply` submits a correction with the
  correction-only New API credential;
- durably records an original-level correction attempt before any New API write;
  uncertain outcomes remain `active` for exact replay, proven no-commit failures
  become auditable `abandoned` attempts that release the original, and remote
  ledger conflicts remain fenced as `remote_conflict`; a concurrent different
  request fails before the correction credential is read;
- records a successful correction in a group-level receipt table whose
  correction external ID is primary and whose original external ID is unique,
  requires its type and SHA-256 subject evidence to match the original grant,
  atomically moves every pending reversal in the original-grant group plus the
  ordinary and any fenced grant job to `reversal_resolved`, and verifies every
  expected row transition; a late reversal can attach the exact receipt without
  a New API call, while changed identity, request, audit evidence, status, or
  result fails closed;
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
- keeps period quota, reset metadata, and approval evidence out of the `base_login` wire request,
  binds its subject hash to the consumed identity, preserves the first sealed
  payload on replay, and rejects catalog or quota metadata drift;
- applies separate per-client fixed-window limits to OAuth authorize and
  callback, token, and userinfo, caps state issuance at 20 per client per five
  minutes and 500 globally per minute, and groups IPv6 clients by `/64`;
  forwarded client addresses are used only when the immediate peer belongs to
  an explicitly configured trusted proxy CIDR;
- classifies Approval v4 failures, honors bounded `Retry-After`, and applies a
  six-step jittered retry schedule before durable dead-lettering for approval
  grants or manual `reversal_pending` resolution for reversals;
- recovers interrupted jobs with their attempt counters after restart, and
  periodically requeues claims that remain `processing` beyond a bounded lease;
  the attempt counter fences late approval completions and all New API writes
  already use stable external IDs for response-loss replay;
- activates held jobs only after the active startup gate has validated the
  keyring, New API credential/client, SQLite state, webhook server, and listen
  socket, then decrypts canonical grant and principal-disable requests,
  persists sanitized results, and applies the documented retry/dead-letter
  matrices;
- exposes liveness, readiness, and bounded-label Prometheus metrics for inbox,
  jobs, approval fetches, approval reversals, New API shadow grants, held grant
  jobs, policy failures, principal disable jobs, processing recovery, dead
  letters, approval/employment reconciliation, and queue age;
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
LARK_APP_SECRET_FILE=/run/secrets/lark_app_secret
LARK_VERIFICATION_TOKEN_FILE=/run/secrets/lark_verification_token
LARK_ENCRYPT_KEY_FILE=/run/secrets/lark_encrypt_key
LARK_TENANT_KEY=...
LARK_ACTIVE_POLICY_VERSION=...
LARK_POLICY_BUNDLE_DIR=/policies
LARK_APPROVAL_BINDINGS_FILE=/policies/approval-bindings.json
LARK_GRANT_PAYLOAD_KEYRING_FILE=/run/secrets/lark_grant_payload_keyring
NEW_API_BRIDGE_CLIENT_ID=...
NEW_API_BRIDGE_CLIENT_SECRET_FILE=/run/secrets/new_api_bridge_client_secret
NEW_API_OAUTH_CALLBACK_ALLOWLIST=https://ai.x2r.store/oauth/lark
```

The three common Lark credentials support inline `LARK_APP_SECRET`,
`LARK_VERIFICATION_TOKEN`, and `LARK_EVENT_ENCRYPT_KEY` values for local test
fixtures. Production must use the corresponding file variables above. Setting
an inline value and its file variable together is rejected.

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
LARK_PROCESSING_LEASE_TIMEOUT=5m
LARK_PROCESSING_RECOVERY_INTERVAL=1m
LARK_OAUTH_PUBLIC_ENABLED=false
LARK_OAUTH_RATE_LIMIT_PER_MINUTE=30
LARK_OAUTH_TRUSTED_PROXY_CIDRS=172.31.20.0/24
LARK_RECONCILIATION_INTERVAL=24h
LARK_APPROVAL_RECONCILIATION_INTERVAL=15m
LARK_APPROVAL_RECONCILIATION_LOOKBACK=72h
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

Processing recovery runs in both modes. `LARK_PROCESSING_LEASE_TIMEOUT` must be
between `1m` and `1h`; `LARK_PROCESSING_RECOVERY_INTERVAL` must be at least
`10s` and no longer than the lease. The lease plus recovery interval must stay
below `LARK_READINESS_MAX_QUEUE_AGE`, so the default worker requeues abandoned
approval, entitlement-grant, and principal-disable claims before readiness
reports a stalled queue. Recovery preserves the attempt counter, records only
bounded queue/count audit data, and never releases `held_shadow`, future
`retry_wait`, or terminal jobs. Prometheus exposes cumulative recovery counts
through `lark_controller_processing_recovered_total{queue}`.

`LARK_RECONCILIATION_INTERVAL` is active-mode only, defaults to `24h`, and must
be between `24h` and `168h`. A failed run is retried after at least 15 minutes;
a longer Lark `Retry-After` is honored up to the normal reconciliation interval.
Completed runs are idempotent by UTC evidence date, and Prometheus exposes
their append-only outcomes through `employment_reconciliation_total{result}`.

Approval reconciliation runs in both modes. Its interval defaults to `15m`
and accepts `1m..24h`; its initial lookback defaults to `72h` and accepts
`1h..720h`. Lark list requests use at-most-10-hour windows and pages of at most
100 instance codes. A cursor advances only after its complete window succeeds,
while a ten-minute overlap makes restarts and boundary deliveries replay-safe.
Active bindings scan through the current run time, draining bindings stop at
`accept_instance_started_before`, and retired bindings are excluded. Existing
validated grants remain recheck targets until a confirmed reversal is recorded
or their policy is retired. Pagination loops, duplicate instance codes,
authority mismatches, malformed responses, `429`, and `5xx` fail closed and
append only bounded audit results. Approval and employment reconciliation share
one process-wide request pacer, so their aggregate Lark request starts remain
at least 100ms apart. Failures retry after at least five minutes and honor a
longer `Retry-After` up to 24 hours. Prometheus exposes the audit through
`approval_reconciliation_total{result}` and exposes per-binding progress through
`approval_reconciliation_cursor_initialized{approval_code}` and
`approval_reconciliation_cursor_lag_seconds{approval_code}`. A failed binding
does not block later bindings or grant rechecks; its next window stays pinned to
the last successful cursor. A tenant-wide rate limit still stops the current
run so the scheduler can honor `Retry-After`.

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
For `REVERTED`, transient failures use the same retry schedule, but terminal or
exhausted fetch failures remain `reversal_pending` with bounded reasons instead
of entering the ordinary approval dead-letter queue.

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
grants, principal disable, paginated active-Lark-principal enumeration, and
operator correction preview/apply. The long-running adapters use only the
dedicated integration bearer credential; the separate `CorrectionClient` uses
only the correction credential and is constructed by the one-shot CLI. The
clients validate bounded responses and classify response loss as retryable because
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

Approval reversal processing is event-driven in this slice. The controller
persists only normalized codes, authoritative status/reverted evidence, the
resolved external ID, original grant metadata, and bounded result/reason
values. Multiple validated source events are accepted when they resolve to one
distinct external ID; zero or multiple IDs stay pending for manual review.
Legacy `shadow_authority_verified_legacy_unresolved` decisions never qualify.
An already-applied wallet grant or subscription upgrade also stays pending for
an operator because New API may have committed before the controller fenced its
local job. On startup, legacy inbox rows are migrated transactionally: a
non-empty `reverted_instance_code` retained in normalized `payload_json` is
backfilled into its indexed column and the durable event hash is recomputed, so
an already-pending reversal and a legitimate redelivery keep the same explicit
target.

Outstanding manual work is exposed as
`lark_approval_reversal_pending{reason}`. The checked-in
`monitoring/lark-controller-alerts.yml` rule turns any durable pending reversal
into `LarkApprovalReversalPending`; the deployment must load that rule and route
the alert through Alertmanager to an operator destination. The same file emits
`LarkApprovalReconciliationFailed` when a bounded reconciliation failure result
increases and `LarkApprovalReconciliationCursorStalled` when a non-retired
binding has no cursor or remains more than 26 hours behind its active/current
or draining/cutoff target. The rules and metrics are implemented locally, but
production Prometheus/Alertmanager wiring has not been performed.

Resolve one pending item with the one-shot CLI documented in
`../docs/runbooks/lark-entitlement-correction.md`. Preview is the default and
does not write New API or Controller state. `--apply` uses one timeout only for
each remote call; after New API succeeds, local resolution is not constrained by
that HTTP timeout. A repeated command returns an already stored Controller
resolution without another New API write. If New API committed but its response
was lost, preview returns the durable `existing_intent`; the same external ID
and canonical payload replay before current-state CAS validation and can then
complete the local resolution. Before loading the
correction credential or calling New API, the CLI hashes `--subject` and requires
an exact match with the original command shadow; the Store repeats this check in
the intent claim and resolution transaction. The subject digest remains internal and is not
included in list, preview, or applied JSON output.

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
level, period quota, reset period, and reset timezone all match; replay never replaces its key ID, nonce, or
ciphertext. Base-login retry, result, and dead-letter audits feed the same
bounded operational metrics as approval grants without fabricating a webhook
event.

The policy directory has no built-in defaults. Operators must mount one or
more files named `*.policy.json`; every file uses `format_version: 2` and
contains one immutable `policy_version`, its lifecycle `state`, versioned
`levels`, and versioned `wallet_packages`. Exactly one loaded version must be
`active`, and it must equal `LARK_ACTIVE_POLICY_VERSION`. A `level_code` keeps
the same rank across every loaded version. All policy, binding, and approval
form JSON rejects unknown fields, duplicate object keys, and trailing data.

The bindings file uses `format_version: 2` and retains active, draining, and
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

The root deployment now has locally verified `lark` and `lark-ops` Compose
profiles, separate Controller/correction image targets, and least-privilege
secret mounts. This is not production authorization: immutable published image
digests, real tenant configuration, cross-network probes, and the two-database
quiesce backup/restore barrier remain open gates. Active grant and real-time principal-disable execution, the durable opaque
OAuth credential store, the outbound Lark token/userinfo adapter, and the
public OAuth authorize/callback handlers, internal token/userinfo handlers,
and idempotent base-subscription dispatch are implemented locally. Employment
reconciliation, runtime stale-claim recovery, and event-driven approval
reversal fencing are also implemented locally. Periodic approval reconciliation
and the operator correction workflow are implemented locally. Production
validation remains follow-up work. Do not
enable active mode in production before those gates are complete.
