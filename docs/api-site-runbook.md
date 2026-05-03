# API Site Runbook

## Release Version Selection

Use:

```bash
scripts/select-new-api-version.py
```

Do not use GitHub's Latest marker blindly. Select the highest non-prerelease semver tag.

## Initial Setup

1. Copy `.env.example` to `.env`.
2. Generate secrets with `openssl rand -hex 32`.
3. Generate `CLIPROXY_INTERNAL_API_KEY` with `scripts/generate-api-key.py`.
4. Copy `config.yaml.template` to `config.yaml`.
5. Replace `config.yaml` `remote-management.secret-key` with the exact `MANAGEMENT_SECRET` value from `.env`.
6. Replace `config.yaml` `api-keys` with the exact `CLIPROXY_INTERNAL_API_KEY` value from `.env`.
7. Create `auths`, `logs`, and `letsencrypt`.

`MANAGEMENT_SECRET` must match in `.env` and `config.yaml`; CPA Usage Keeper reads it from `.env`, while CLIProxyAPI authenticates against `config.yaml`. `CLIPROXY_INTERNAL_API_KEY` must also match; New API uses the `.env` value for the internal channel and CLIProxyAPI accepts the `config.yaml` value.

## New API Admin Hardening

- Rotate default admin credentials before public launch.
- Enable login rate limiting and captcha/2FA where supported.
- Disable unused authentication methods.
- Disable online payment providers.
- Disable registration bonus/free-credit settings.
- Confirm the only public top-up route is redeem-code redemption.

## New API Business Configuration

- Enable invitation-code registration.
- Set new user initial balance to `$0`.
- Configure redeem-code denominations: `$10`, `$50`, `$100`, `$500`.
- Preserve `$1 = 500,000 quota units`.
- Create groups: `unactivated`, `standard`, `trusted`, `admin-test`.
- Keep `standard` users on CLIProxyAPI-backed channels only.

## Channel Configuration

- Add CLIProxyAPI as an internal New API channel using `http://cliproxyapi:8317`.
- Use `CLIPROXY_INTERNAL_API_KEY` as the channel key.
- Configure `codex-cli` only after `/v1/responses` validation.
- Configure official OpenAI fallback only for `admin-test`.

## Codex Validation

Record:

- Codex CLI version.
- New API version.
- CLIProxyAPI version.
- Model alias.
- Request id correlation.
- Balance before and after.

Run a real Codex CLI agent session in a disposable test repository.

## Billing Acceptance

Before any model is visible to `standard` users, record:

- New API model ratio.
- Completion/output ratio if supported.
- Group ratio.
- Cache-token handling if supported.
- Reasoning-token handling if reported.
- Failed-attempt charging behavior.
- Retry charging behavior.
- Effective total-token billing rule if separate input/output/cache/reasoning rates are unavailable.

For each case, record user balance before, user balance after, observed quota delta, expected quota delta, request id or correlation id, New API channel, CLIProxyAPI usage event if applicable, and official provider bill if applicable:

| Case | Endpoint | Required validation |
| --- | --- | --- |
| Responses non-stream | `/v1/responses` | Input/output usage and quota delta match configured model/group formula within documented tolerance. |
| Responses stream | `/v1/responses` streaming | Final usage appears and deduction matches equivalent non-stream usage within tolerance. |
| Tool/function call | `/v1/responses` | Tool-call tokens are included in deduction or explicitly documented if provider usage excludes them. |
| Long context | `/v1/responses` | Request succeeds with correct deduction or is rejected before upstream call. |
| Upstream retry | CLIProxyAPI-backed model | User is charged only for final billable provider usage, or retry charging is documented and priced. |
| Upstream failure | CLIProxyAPI-backed model | No balance deduction unless New API/provider reports billable usage. |
| Official fallback test | `admin-test` only | New API deduction and official provider bill reconcile before any trusted-user exposure. |

## Usage Keeper

- Keep CPA Usage Keeper private on the backend network.
- Persist its data.
- Use the CLIProxyAPI management key from `.env`.
- Do not expose the dashboard unless protected.

## Backup And Restore

- Run `scripts/backup-api-site.sh`.
- Store encrypted backups off-host.
- Before meaningful paid usage, run a disposable restore drill:
  - Restore `config.yaml`.
  - Restore `auths`.
  - Restore `newapi-postgres.dump` with `pg_restore` into disposable New API Postgres.
  - Restore CPA Usage Keeper data if `cpa-usage-keeper-data` is present.
  - Run compose validation and `scripts/verify-api-site.sh`.

## Rollback

- Keep previous pinned image tags.
- Back up Postgres before upgrades.
- Roll back Compose image tags and run `docker compose up -d`.

## Launch Gates

Set `NEW_API_TEST_API_KEY` and `CODEX_TEST_API_KEY` before launch validation. Launch is blocked if `/v1/responses` is skipped.

Run:

```bash
test -f .env
test -f config.yaml
! rg -n 'replace-with-|CHANGEME|change-me' .env config.yaml
docker compose config --format json > /tmp/api-site-compose.json
scripts/validate-api-site-compose.py /tmp/api-site-compose.json --host ai.x2r.store
: "${NEW_API_TEST_API_KEY:?set NEW_API_TEST_API_KEY for launch validation}"
: "${CODEX_TEST_API_KEY:?set CODEX_TEST_API_KEY for /v1/responses launch validation}"
scripts/verify-api-site.sh > /tmp/api-site-verify.log
cat /tmp/api-site-verify.log
! rg -n 'Skipping /v1/responses' /tmp/api-site-verify.log
```
