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

## CLIProxyAPI Management Access

- Keep CLIProxyAPI off Traefik and off public hostnames.
- Use the host loopback binding `127.0.0.1:8317:8317` only for SSH tunnel management.
- From your workstation, run `ssh -L 8317:127.0.0.1:8317 <user>@<server>` and manage CLIProxyAPI through `http://127.0.0.1:8317`.
- Do not replace this with `8317:8317` or `0.0.0.0:8317:8317`.
- Keep CLIProxyAPI API key authentication and `remote-management.secret-key` enabled.

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

## CLIProxyAPI Log Archiving

Use this only for short troubleshooting windows where `request-log: true` is enabled. Full request logs can include request bodies, response bodies, headers, streaming chunks, and upstream API data.

Set Cloudflare R2 credentials in `.env`:

```text
R2_ACCOUNT_ID=...
R2_BUCKET=...
R2_ACCESS_KEY_ID=...
R2_SECRET_ACCESS_KEY=...
CLIPROXY_LOG_ARCHIVE_R2_PREFIX=cliproxy-logs
CLIPROXY_LOG_ARCHIVE_MIN_AGE_MINUTES=30
CLIPROXY_LOG_ARCHIVE_DELETE_AFTER_DAYS=1
CLIPROXY_LOG_ARCHIVE_GZIP_LEVEL=1
CLIPROXY_LOG_ARCHIVE_NICE=19
CLIPROXY_LOG_ARCHIVE_IONICE_IDLE=true
CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT=
```

Install AWS CLI v2 on the host, then run:

```bash
scripts/archive-cliproxy-logs.sh
```

The script compresses request-log files older than the safety window, uploads `.gz` files to Cloudflare R2, writes a `.uploaded` marker only after successful upload, and deletes uploaded local copies after `CLIPROXY_LOG_ARCHIVE_DELETE_AFTER_DAYS=1`.

Compression is intentionally low-impact by default: `gzip -1`, `nice -n 19`, and idle `ionice` when available. If the host is still CPU-bound, install `cpulimit` and set `CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT=25` or another 1-100 percentage.

Example cron entry:

```cron
*/10 * * * * cd /opt/cliproxy-deploy && scripts/archive-cliproxy-logs.sh >> /var/log/cliproxy-log-archive.log 2>&1
```

Do not back up active uncompressed request logs. For backups, prefer already uploaded R2 objects or local `.gz` files with `.uploaded` markers.

Detailed setup steps are in [docs/cliproxy-log-archive-r2-runbook.md](cliproxy-log-archive-r2-runbook.md).

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

## Production Launch

After the launch gates pass, start production with:

```bash
docker compose config
scripts/backup-api-site.sh
docker compose pull
docker compose up -d
docker compose ps
scripts/verify-api-site.sh
```
