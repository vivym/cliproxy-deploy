# Lark entitlement correction runbook

## Scope and status

This runbook covers one-shot operator correction of a durable Lark approval
reversal. The code path and `lark-ops` Compose profile are implemented and
locally tested, but have not been enabled or validated in production.

The workflow never edits the original grant. It creates a new
`lark:correction:*` command in New API and then records an immutable resolution
against the Controller reversal. One original grant can have only one correction
receipt. Multiple or late reversal events for that original grant share the same
receipt; they never authorize another New API mutation. Do not update either
database with ad hoc SQL.

## Credential boundary

New API registers the correction endpoints only when
`LARK_CORRECTION_SECRET_FILE` or `LARK_CORRECTION_SECRET` is set. The credential
must be at least 32 printable ASCII bytes and must differ from both current and
next `LARK_INTEGRATION_SECRET` values.

Keep this credential out of both long-running services. The base New API and
Controller do not mount it. During an approved maintenance window, `lark-ops`
mounts it only into a temporary New API correction endpoint and the one-shot
CLI container. Remove both containers after the operation, then rotate or
revoke the credential according to the change ticket. It is not a New API admin
PAT and cannot call the ordinary grant, disable, or principal-list endpoints.

## Build

Build the independent CLI target from the repository root:

```bash
docker compose --profile lark-ops build lark-correction
```

The long-running Controller image contains only `lark-controller`; the
`lark-correction` binary is in the separate `correction` Dockerfile target.

Controller startup owns schema migration, including creation and legacy-receipt
backfill of `approval_reversal_correction_intents`. Run the CLI only after the
matching Controller version has opened the database successfully;
`OpenCorrection` deliberately refuses to migrate or recover runtime state.

The examples below assume:

```text
Controller DB: /var/lib/lark-controller/controller.sqlite
New API origin: http://new-api-correction-endpoint:3001
Secret file: /run/secrets/lark-controller/new-api/lark_correction_secret
```

## Find pending reversals

This opens SQLite with `mode=ro` and `query_only`, runs no migration or
processing-state recovery, and does not require the correction credential or a
New API endpoint. The dedicated `lark-correction-readonly` service has no
network, mounts no secret, and mounts Controller SQLite read-only. Stop the
Controller writer first; the host runner rejects discovery while it is still
running and refuses to create an empty state volume:

```bash
docker compose --profile lark stop lark-quota-controller
scripts/run-lark-correction.sh --list-pending
```

The output includes the reversal event key, original external ID, original
grant type and amount/level evidence, policy version, bounded reason, and
creation time. When a non-`abandoned` attempt currently fences the original it
also includes the correction external ID, request hash, type, bounded audit
fields, lifecycle status, and failure code. Historical `abandoned` attempts
remain in the SQLite audit ledger but are not presented as the current intent.
Select exactly one item and record the operator decision in a change ticket. It
deliberately omits the internal original-subject digest.

## Open the maintenance boundary

Obtain separate production authorization before running these commands. The
temporary endpoint is deliberately not on `edge`, has no Traefik labels or host
port, and is started only by the host-side runner. The runner rejects a running
primary New API or Controller by Docker service state, independent of health
status, then holds a host-only `lark-runtime/ops/maintenance.session` mutex and
`maintenance.lock` with `mode=correction`; `--list-pending` uses the same mutex
with `mode=readonly` and fixed container name
`new-api-lark-correction-readonly-ops`. Both regular
services refuse startup for any lock mode, while both temporary write services
accept only `correction` and reject `backup`, `restore`, or `readonly`. Stop both services first so two New API processes do not
run background work against the same Postgres/Redis state and the CLI does not
write Controller SQLite beside its worker:

The lock directory is `0755` and its non-secret mode marker is `0644`, so the
UID `10001` containers can read it through the read-only bind mount. Ownership
of the directory remains with the host runner; containers cannot change it.
The runner force-removes and exactly queries either named correction container
before unlocking; uncertain cleanup retains both the lock and session.

```bash
umask 077
correction_tmp="$(mktemp)"
trap 'rm -f "$correction_tmp"' EXIT
openssl rand -base64 48 | tr -d '\n' > "$correction_tmp"
sudo install -o 10001 -g 10001 -m 0600 "$correction_tmp" \
  lark-runtime/secrets/new-api/lark_correction_secret
sudo scripts/verify-lark-secret-permissions.sh --include-correction
docker compose --profile lark stop lark-quota-controller
docker compose stop new-api
```

Do not start either write-capable `lark-ops` service directly. For each preview/apply,
the runner verifies secrets, rejects any running primary, creates the lock,
rechecks the primary state, starts and waits for the temporary endpoint, runs
one CLI command, removes the named CLI and endpoint containers, confirms both
are absent, and only then releases the lock. If cleanup cannot be confirmed, the runner returns
failure and deliberately retains the lock; follow its recovery command only
after removing both containers and confirming their ID queries are empty.
Define this helper in the same authorized operator session. `sudo` is required
because the verifier must read effective token values from `10001:10001/0600`
files without printing them:

```bash
correction() {
  sudo scripts/run-lark-correction.sh "$@"
}
```

## Wallet correction

Choose the actual signed `wallet_delta`; do not automatically negate the
original grant. `0` is valid when the correct audited decision is no balance
change. The expected wallet quota must be copied from the current preview, not
calculated from the original grant.

Run without `--apply` first:

```bash
correction \
  --reversal-event-key '<reversal-event-key>' \
  --original-external-id 'lark:wallet-topup:<original-instance>' \
  --external-id 'lark:correction:<change-ticket>:wallet' \
  --policy-version '<original-policy-version>' \
  --subject '<tenant-key>:<open-id>' \
  --operator '<operator-identity>' \
  --reason '<bounded correction reason>' \
  --change-ticket '<change-ticket>' \
  --wallet-delta '<signed-int64-delta>' \
  --expected-wallet-quota '<current-wallet-quota>'
```

Inspect `current.wallet_quota`, `current.used_quota`, `current.last_login_at`,
the original reversal evidence, the proposed delta, and
`expected_state_matches`. Obtain the required human approval, then repeat the
identical command with `--apply` appended. Do not change the external ID or any
evidence fields between preview and apply.

`--subject` must be the exact subject used by the original grant. Before it
reads the correction credential or calls New API, the CLI hashes this value and
compares it with the original command shadow. The Store repeats the comparison
inside the resolution transaction; the digest is never printed in CLI JSON.
The CLI also checks for an existing receipt for the original grant before it
reads the credential or calls New API. An exact command is shown as
`existing_resolution` in preview mode and attached to a late reversal by
`--apply`; any different external ID or payload fails closed.

Before an unresolved `--apply` reads the correction credential or calls New
API, it durably records an attempt in
`approval_reversal_correction_intents`. Each attempt immutably binds its
correction external ID, canonical request hash, subject hash, correction type,
and audit fields. Its lifecycle is:

- `active`: the remote commit outcome may still be unknown. The original grant
  remains fenced; only the exact command may replay.
- `abandoned`: the failure proves that no New API correction committed. The
  original grant is released for a newly approved external ID; the old attempt
  remains auditable and cannot be rebound to another payload.
- `remote_conflict`: New API reported an existing correction or external-ID
  payload conflict. The original remains fenced and requires investigation.
- `resolved`: the immutable Controller receipt was committed and all matching
  pending reversals were closed.

An exact preview after response loss returns `existing_intent`; an alternate
command fails locally and reports the active external ID. `--list-pending`
shows the same bounded lifecycle summary for the current non-`abandoned`
attempt, without the subject hash.

## Subscription correction

A subscription correction is an absolute target. It requires the current
assignment version and never means "add another subscription":

```bash
correction \
  --reversal-event-key '<reversal-event-key>' \
  --original-external-id 'lark:subscription-level:<original-instance>' \
  --external-id 'lark:correction:<change-ticket>:subscription' \
  --policy-version '<target-published-policy-version>' \
  --subject '<tenant-key>:<open-id>' \
  --operator '<operator-identity>' \
  --reason '<bounded correction reason>' \
  --change-ticket '<change-ticket>' \
  --level-code '<absolute-target-level>' \
  --expected-assignment-version '<current-version>'
```

Preview first, inspect the current level, source external ID, used/total quota,
and reset timestamps, then repeat the exact command with `--apply`. New API
keeps the same subscription ID and cycle timestamps. A downgrade caps
`amount_used` at the target plan total, so remaining subscription quota cannot
become negative. If the target level code already equals the assignment level,
the correction records a `noop` ledger receipt only. It does not migrate the
assignment to another policy version or write user settings, subscription rows,
assignment state, or cache outbox events.

## Failure recovery

- `correction_state_mismatch`: the Controller marks the attempt `abandoned`.
  Refresh the preview and reopen the human decision. Do not reuse the external
  ID with a changed payload; create a new correction external ID under the same
  or a new change ticket.
- `correction_original_grant_mismatch`: stop and investigate the selected
  employee and original grant. Do not retry against another subject with the
  same correction external ID.
- `correction_already_applied`: the Controller marks the local attempt
  `remote_conflict`. Another correction receipt already owns the original grant.
  Do not create a second correction and do not assume the local external ID is
  the remote winner. Escalate to reconcile the New API ledger with the change
  record before attaching any Controller receipt.
- `managed_plan_mismatch`: stop and repair the immutable catalog/plan binding
  drift. The attempt is `abandoned`; preview and approve a new command. This is
  not a transient `503`.
- HTTP response loss or timeout: run the exact command without `--apply` to
  confirm `mode=existing_intent`, then rerun it with `--apply`. New API checks
  the correction external ID and payload hash before expected-state validation,
  so an already committed command returns `replayed` even though current state
  now differs. Never replace the command bound by the intent.
- New API succeeded but Controller resolution failed: rerun the exact same
  command. The New API call replays and the Controller resolution transaction
  completes.
- Controller resolution succeeded but CLI output was lost: rerun the exact
  same command. The CLI returns the stored resolution without another New API
  write.
- A duplicate reversal arrived after resolution: preview and apply the exact
  stored correction command. The CLI reads the original-level receipt locally
  and closes the late reversal without a New API request.
- `external_id_payload_mismatch`: the attempt is `remote_conflict`. Stop and
  escalate. Never change an existing correction payload or create a replacement
  while the conflict remains unresolved.

## Verify and close

Run `scripts/run-lark-correction.sh --list-pending` again. The handled event must be absent; its inbox,
ordinary job, and any fenced grant job are `reversal_resolved`; and
`lark_approval_reversal_pending{reason}` must decrease. Preserve the CLI
receipt with the change ticket. Then close the maintenance boundary in this
order:

```bash
test ! -e lark-runtime/ops/maintenance.lock
test ! -e lark-runtime/ops/maintenance.session
docker compose --profile lark-ops ps
docker compose up -d new-api
docker compose --profile lark up -d lark-quota-controller
scripts/verify-deployment.sh
```

Confirm no `lark-ops` container, maintenance lock, or maintenance session remains, then complete credential rotation or
revocation and remove the short-lived host file according to the change ticket.
If the remote commit outcome is uncertain, preserve the original command and
attempt record and follow the exact-replay recovery rules before closing the
ticket; never improvise a second correction.
