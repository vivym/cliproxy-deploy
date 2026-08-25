# Lark tenant configuration runbook

## Scope and authority

This runbook operates the Lark/New API configuration control plane:

```text
Compile(source, environment binding) -> CompiledBundle
Diff(CompiledBundle, observed state)  -> ChangePlan
Apply(ChangePlan, expected digest)    -> Receipt
```

It does not authorize access to a deployment server or a real Lark tenant.
Server access, Lark Console changes, user authorization, and `apply` are separate
operator actions and require explicit approval for the exact environment. Local
development must use placeholders, local databases, and fake Lark command runners.

The source of truth consists of:

- `policy.json`: subscription levels, one-time wallet packages, approval manifests,
  and retained historical policy versions;
- `<environment>.binding.json`: public origin, Lark/New API identifiers, plan IDs,
  rotation expectations, and secret references; and
- `lark-console-attestation.json`: a reviewed record of Console-only events and
  app-level approval subscriptions that have no reliable read API.

Examples are in `examples/lark-config/`. They contain no credentials and are not
production values.

The four `secret_refs` are logical file names, not secret values or host paths.
Compilation projects them into the fixed, read-only Controller mounts under
`/run/secrets/lark-controller/{controller,shared}/`; the resulting paths are
written to `runtime/controller.env`. A reference must be 1 to 128 lowercase ASCII
letters, digits, underscores, or hyphens; all four references must be distinct, and
`bridge_client_secret` uses the fixed name `new_api_bridge_client_secret`.

## Safety properties

- JSON decoding rejects unknown fields, duplicate keys, and trailing documents.
- Plans and receipts contain secret references, never secret values.
- Plans are written with mode `0600`; plan reads, receipt paths, and local artifacts
  stay under `output-root`, reject symlink traversal, and use atomic writes. Apply
  preflights receipt writability and refuses a receipt that aliases its reviewed plan.
- `apply` requires the exact reviewed plan digest and a change ticket. A changed or
  blocker-bearing plan fails closed. Every executor result must exactly match its
  operation's desired digest.
- `apply` owns `maintenance.session` plus `maintenance.lock/mode=config` for its
  entire mutation and receipt window. Each local/remote operation and every New API
  configuration write rechecks that mode; backup, restore, correction, and config
  writes are therefore mutually exclusive.
- New API configuration uses a credential independent from grants and corrections.
- The New API configuration listener has no host port or Traefik route and only joins
  `new-api-data` plus the isolated internal `new-api-lark-config` network.
- The OAuth provider is always staged disabled. Enabling it remains a separate pilot
  change under `lark-controller-compose-rollout.md`.
- `lark-config` only subscribes an app to exact approval codes. It does not create or
  edit approval definitions and does not edit Lark Console event settings.

## Prepare source files

On a Linux deployment host, prepare every bind source before Compose can start the
non-root config container. This also prevents Compose from silently creating
root-owned directories. The paths contain configuration artifacts, not secrets:

```bash
sudo install -d -o 10001 -g 10001 -m 0750 \
  lark-runtime/config lark-runtime/policies lark-runtime/new-api \
  lark-runtime/lark lark-runtime/runtime lark-runtime/receipts \
  lark-runtime/ops
```

If an operator edits the generated source files outside the container, preserve
UID/GID `10001:10001` and mode `0600`. Compose uses `create_host_path: false` for
these mounts; a missing preparation step fails instead of changing ownership.

Initialize new files without overwriting existing configuration:

```bash
docker compose --profile lark-config run --rm --no-deps lark-config init
```

Alternatively, copy the three reviewed examples into `lark-runtime/config/` and
replace every `REPLACE_WITH_...` value. Keep environment-specific bindings and
attestations out of a shared template when they contain production identifiers.

The active policy requires exactly one `wallet_topup` and one
`subscription_level` approval manifest in `zh-CN`. Each manifest has exactly four
approved fields and no extras: required `cost_center` (`textarea` or `radioV2`),
required `request_reason` (`textarea`), `estimated_usage` (`textarea`, optional only
for wallet and required for subscription), plus the required kind-specific
`wallet_package` or `target_level` radio. Field order, `custom_id`, type, required
state, and radio display text are contract data. The Controller maps the display
text received from Lark to the stable business `code`; changing display text without
a new policy/approval binding is schema drift.

Each subscription level binds to a distinct existing New API plan. Every bound plan
must be enabled, `managed_only`, allow wallet overflow, disallow balance payment,
have no Stripe/Creem product, and have `total_amount == period_quota`. Every bound
plan must use `quota_reset_period=weekly` and
`quota_reset_timezone=Asia/Shanghai`; all levels in one binding share one reset
contract hash. For a one-month plan that resets each Monday at 00:00 in
`Asia/Shanghai`, the hash authority is exactly:

```json
{"duration_unit":"month","duration_value":1,"custom_seconds":0,"quota_reset_period":"weekly","quota_reset_timezone":"Asia/Shanghai","quota_reset_custom_seconds":0}
```

Its SHA-256 is
`8e4c1bf09a361b2a87438a5dc537b4cf78159336be75d776dcf5f04fca614be2`.
New API preflight locks and validates every referenced plan before any policy write;
do not bypass a mismatch by guessing another digest.

Schema v2 deliberately rejects startup migration when a non-empty legacy
`managed_subscription_levels.monthly_quota` authority table exists. Those rows bind
an immutable v1 catalog hash and cannot be renamed safely in place. The expected
first rollout has no such rows; if preflight finds any, stop and prepare an explicit
offline policy migration instead of deleting or rewriting them during deployment.

Validate compilation without network access:

```bash
docker compose --profile lark-config run --rm --no-deps lark-config check
```

Record the returned `compiled_digest`. Compilation produces no runtime files and
does not contact Lark or New API.

## Prepare secrets

No secret belongs in `.env`, Git, a JSON source, a plan, a receipt, a shell argument,
or command output. The configuration window adds one independent credential:

```text
lark-runtime/secrets/config/lark_config_secret
```

It is a 32 to 4096 byte printable ASCII bearer token and must differ from current,
next, correction, Lark App, and New API bridge credentials. The New API bridge
client secret remains at:

```text
lark-runtime/secrets/controller/new_api_bridge_client_secret
```

The reviewed environment binding must set `secret_refs.bridge_client_secret` to
the fixed logical name `new_api_bridge_client_secret`. Compose mounts only that
fixed file into the short-lived endpoint, avoiding host-path interpolation, and
New API rejects a projection whose reference does not match the mounted credential.

On the target host, under separately approved operator authority, create the
directory and install secret files as UID/GID `10001:10001`, directory mode `0700`,
and file mode `0600`. Verify the configuration window in addition to normal runtime
secrets:

```bash
sudo scripts/verify-lark-secret-permissions.sh --include-config
```

The config endpoint receives only `lark_config_secret` and the bridge secret. The
CLI receives only `lark_config_secret`; Lark CLI app credentials are stored in the
dedicated `new-api-lark-config-cli-data` volume.

## Prepare Lark identities and Console state

The two CLI identities are deliberately separate:

- user identity reads approval definitions with scope `approval:approval:read`;
- bot identity calls
  `POST /open-apis/approval/v4/approvals/:approval_code/subscribe` using the app's
  tenant access token. The app needs either `approval:approval` or
  `approval:definition`, plus `tenant:tenant:readonly` so planning can bind the live
  tenant key returned by `GET /open-apis/tenant/v2/tenant/query`. Do not run
  `auth login` for bot identity.

Initialize the pinned Lark CLI profile without exposing the app secret in the
process list. This is a write to the local CLI credential volume, not a tenant
mutation:

```bash
docker compose --profile lark-config run --rm --no-deps -T \
  --entrypoint lark-cli lark-config \
  config init --app-id REPLACE_WITH_LARK_APP_ID --app-secret-stdin \
  < lark-runtime/secrets/controller/REPLACE_WITH_LARK_APP_SECRET_REF
```

Then obtain the user grant interactively and verify it:

```bash
docker compose --profile lark-config run --rm --no-deps \
  --entrypoint lark-cli lark-config \
  auth login --scope "approval:approval:read"

docker compose --profile lark-config run --rm --no-deps \
  --entrypoint lark-cli lark-config \
  auth status --json --verify
```

In Lark Developer Console, configure the compiled Controller callback and exactly
these event subscriptions:

```text
https://<public-origin>/integrations/lark/oauth/callback
approval.instance.status_changed_v4
contact.user.deleted_v3
```

Do not add `approval.task.status_changed_v4`; the Controller deliberately does not
process it. Create the two native approval definitions manually and make their form
contracts exactly match `policy.json`.

After an independent reviewer verifies app ID, tenant key, redirect URL, events,
approval definitions, and evidence, update `lark-console-attestation.json`:

- `redirect_urls` lists exactly the compiled callback and no other URL;
- `console_events` lists only events actually enabled in Console;
- `approval_subscriptions` initially lists only approval codes already confirmed as
  subscribed for this app;
- `reviewed_by`, `reviewed_at`, and `evidence` identify the review receipt.

An attestation is evidence, not desired state. Never list a subscription merely to
make a plan clean.

## Plan

Before opening the configuration window, select a New API image built from the
current committed config-control-plane revision. The historical `dbfcf0c7` image
does not contain the `/api/integrations/v1/config/*` routes and must not be used.
The `new-api-config-endpoint` healthcheck's authenticated-boundary probe expects
`/api/integrations/v1/config/state` to return `401`; failure is an image gate, not
a reason to bypass the healthcheck.

Start the temporary config-only New API endpoint only during an approved
configuration window:

```bash
docker compose --profile lark-config up -d new-api-config-endpoint
```

Create a plan from local artifacts plus both remote observations:

```bash
docker compose --profile lark-config run --rm lark-config plan \
  --remote=lark,new-api \
  --new-api-base-url=http://new-api-config-endpoint:3001 \
  --new-api-config-secret-file=/run/secrets/lark-config/lark_config_secret \
  --lark-console-attestation=lark-runtime/config/lark-console-attestation.json
```

Planning verifies both lark-cli identities with `auth status --json --verify`, binds
the live CLI profile app ID to the attestation, and queries the bot-visible tenant key.
It fails before producing a plan if the bot or user identity is not ready, the user
grant lacks `approval:approval:read`, or the authenticated app/tenant differs from the
reviewed attestation. Redirect URLs, Console events, and existing app-level approval
subscriptions are treated as exact reviewed sets; unexpected entries are blockers.

The plan order is fixed:

```text
10  Lark app-level approval subscriptions
20  staged New API policy and disabled OAuth provider
30  local compiled artifacts
40  New API policy activation
```

Review `lark-runtime/ops/lark-config-plan.json` out of band. Confirm:

- `blockers` is empty;
- `compiled_digest` matches the reviewed check result;
- every target, action, approval code, policy version, plan ID, quota, callback, and
  before/desired digest is expected;
- `observed_targets` is exactly `local`, `new-api`, and `lark`; OAuth provider
  `before_digest` and `desired_digest` come from the authenticated New API preflight
  and bind the mounted bridge secret without exposing it;
- local artifacts retain all required active, draining, and retired policy files;
- no secret value appears anywhere in the file.

Capture the exact top-level `digest`. Editing the plan invalidates it; regenerate
instead.

## Apply and verify

`apply` is a remote write to Lark/New API. Run it only after explicit authorization
for the reviewed plan digest and change ticket:

```bash
docker compose --profile lark stop lark-quota-controller

docker compose --profile lark-config run --rm lark-config apply \
  --plan=lark-runtime/ops/lark-config-plan.json \
  --expected-digest='sha256:REPLACE_WITH_REVIEWED_PLAN_DIGEST' \
  --change-ticket='CHG-REPLACE_ME' \
  --new-api-base-url=http://new-api-config-endpoint:3001 \
  --new-api-config-secret-file=/run/secrets/lark-config/lark_config_secret
```

Every Lark subscription mutation carries the reviewed app ID and tenant key inside
the digested plan payload. Immediately before each write, `lark-config` verifies the
current CLI profile and live tenant again; switching profiles after planning fails
closed before that mutation.

At apply start, `lark-config` atomically owns the same host maintenance session used
by backup, restore, and correction, with `mode=config`. The New API endpoint checks
that exact mode before every mutating request, while read-only planning remains
available without it. Do not create or remove these lock paths manually.

The Controller loads `controller.env` and the policy catalog only at process start;
it does not hot-reload either artifact. Keep it stopped across apply so a New API
activation can never run beside an old in-memory Controller policy. If apply fails,
do not restart the Controller against potentially partial local artifacts. Preserve
the failed receipt, correct the cause, regenerate a plan from fresh observations,
and finish the same staged change first.

The receipt defaults to `<plan>.receipt.json`. Check `status == "succeeded"`, every
operation result digest equals its reviewed desired digest, and the maintenance lock
has been released. `replayed` is valid for an already published policy,
unchanged disabled provider, unchanged local artifact, or Lark error `1390007`
(`subscription existed`). Any other failure stops the sequence and writes a failed
receipt without exposing the underlying response or credentials.

After a successful receipt, start the Controller from the newly written artifacts
and require its startup/readiness gates before accepting traffic:

```bash
docker compose --profile lark up -d lark-quota-controller
scripts/verify-deployment.sh
```

Independently verify the Lark app subscriptions. Only then add the confirmed codes
to the attestation's `approval_subscriptions` and run `plan` again. A steady-state
plan must have no blockers and no remote changes.

Stop and remove the temporary config endpoint after receipts and verification are
captured:

```bash
docker compose --profile lark-config stop new-api-config-endpoint
docker compose --profile lark-config rm -f new-api-config-endpoint
```

The regular New API service remains the public entry. The configuration endpoint
must not become a long-running sidecar.

## Policy rotation and historical catalog

A new target policy gets a new immutable `policy_version` and new approval codes.
Never reuse an approval code with changed fields or display text.

During rotation:

1. Move the prior policy into `historical_policies` with state `draining`.
2. Retain both prior approval manifests and their exact prior approval codes.
3. Set each prior binding's `accept_instance_started_before` to the approved RFC3339
   cutoff.
4. Set `new_api.expected_active_policy_version` to the prior version and
   `new_api.accept_current_instances_started_before` to the same cutoff.
5. Bind the new active policy to its reviewed plans and new approval definitions.
6. Plan and apply using the same digest/CAS procedure.

Compilation fails if the expected active version is not retained, is not draining,
or uses a different cutoff. Draining and retired policy files remain in the
Controller catalog for delayed events and replay. Retire only after the documented
acceptance/reconciliation window has closed; a retired policy requires a past
`retire_after` later than its approval cutoff.

## Replay, rollback, and recovery

Reapplying the unchanged reviewed plan is safe only with the same exact digest and a
new or retained approved change ticket. Idempotent operations are reported as
`replayed`. After a partial failure, inspect the receipt, correct the external
precondition, regenerate a fresh plan from current observed state, and review its
new digest. Do not delete operations from the old plan.

Rollback disables exposure; it does not erase granted money or subscriptions:

1. Keep the Lark OAuth provider disabled, or disable it through the controlled New
   API admin path if a later pilot enabled it.
2. Set `LARK_OAUTH_PUBLIC_ENABLED=false` and keep Controller in `shadow` under the
   rollout runbook.
3. Stop the Controller if required, but preserve its database, compiled policies,
   bindings, config receipts, and secret rotation history.
4. Correct already applied wallet or subscription mutations only through the
   separate preview/CAS correction runbook.

For lost CLI user authorization, reauthorize the user identity. For a lost CLI app
profile, reinitialize the dedicated volume from the protected app secret; do not
copy access tokens. If New API and Controller state are restored, run the full
post-restore reconciliation before enabling OAuth or active mode.
