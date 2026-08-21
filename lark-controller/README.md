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
- stores only normalized event data, requester/form hashes, and shadow
  decisions; and
- has no New API entitlement client or grant execution path.

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
```

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
Retry classification, metrics, OAuth, employment reconciliation, and the New
API adapter remain follow-up WP3 slices.
