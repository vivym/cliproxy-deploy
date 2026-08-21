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
```

Optional variables:

```text
LARK_CONTROLLER_LISTEN_ADDR=0.0.0.0:8080
LARK_APPROVAL_LOCALE=zh-CN
```

Run local checks from this directory:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/lark-controller
```

This slice does not add the service to Docker Compose and must not be deployed.
Policy bundle validation, exact form parsing, retry classification, metrics,
OAuth, employment reconciliation, and New API adapter work remain follow-up
WP3 slices.
