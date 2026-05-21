# x2r AI Gateway

Production Docker Compose deployment for an OpenAI-compatible AI gateway at `ai.x2r.store`, `cliproxy.x2r.store`, and `keeper.x2r.store`.

The stack exposes [New API](https://github.com/Calcium-Ion/new-api) as the public user, admin, and SDK entry point. CLIProxyAPI and CPA Usage Keeper also have public HTTPS hostnames and rely on their own application-level keys or login protection.

## What This Deploys

- Traefik TLS reverse proxy with automatic Let's Encrypt certificates.
- New API public gateway on one hostname.
- Postgres and Redis for New API.
- CLIProxyAPI on a public HTTPS hostname, the private backend network, and host loopback for local maintenance.
- CPA Usage Keeper on a public HTTPS hostname with password login enabled by default.
- Helper scripts for version selection, API-site validation, backups, latency profiling, and account conversion.

## Architecture

```text
Users / SDKs / Codex CLI
        |                 Operators
        |                    |
        v                    v
https://ai.x2r.store
https://cliproxy.x2r.store
https://keeper.x2r.store
        |
        v
Traefik :443
        |
        +--> New API :3000  ---> Postgres
        |       |
        |       v
        |   CLIProxyAPI :8317 ---> Upstream model accounts
        |
        +--> CLIProxyAPI :8317
        |
        +--> CPA Usage Keeper :8080

CPA Usage Keeper reads CLIProxyAPI usage internally through the backend network.
```

Postgres and Redis stay private. New API, CLIProxyAPI, and CPA Usage Keeper are public through Traefik.

## Repository Contents

```text
docker-compose.yml                              Production Compose stack
config.yaml.template                            CLIProxyAPI production config template
.env.example                                    Required environment variables
docker-compose.cliproxy-public.override.yml.template
                                                Temporary maintenance-only exposure template
scripts/generate-api-key.py                     Generate internal CLIProxyAPI API keys
scripts/verify-api-site.sh                      Production verification checks
scripts/validate-api-site-compose.py            Rendered Compose policy checks
scripts/backup-api-site.sh                      Runtime backup helper
scripts/restore-api-site.sh                     Runtime restore helper
scripts/archive-cliproxy-logs.sh                Compress and upload CLIProxyAPI logs to R2
scripts/profile-latency.py                      Public route latency comparison
scripts/profile-origin.py                       VPS-side origin latency comparison
scripts/convert-codex-switcher-accounts.py      Convert codex-switcher auth files
scripts/manage-cliproxy-auth-priority.py        Manage CLIProxyAPI auth priority
docs/api-site-runbook.md                        Operational runbook
tests/                                          Template and script tests
```

Runtime files are intentionally local-only and should not be committed:

```text
.env
config.yaml
auths/
logs/
letsencrypt/
tmp/
```

## Requirements

- A Linux server with Docker Engine and Docker Compose v2.
- DNS control for the public hostnames.
- Open inbound ports `80` and `443`.
- No direct public exposure for port `8317`, Postgres, Redis, or the CPA Usage Keeper container port.
- AWS CLI v2 if using Cloudflare R2 log archiving.

Traefik reads the Docker socket to discover labelled containers. Anyone who can control containers or labels on this Docker daemon is inside the deployment trust boundary.

## DNS And Cloudflare

Create A or AAAA records for the public hostnames:

```text
ai.x2r.store -> <server-ip>
cliproxy.x2r.store -> <server-ip>
keeper.x2r.store -> <server-ip>
```

For Cloudflare:

```text
A  ai        <server-ip>  Proxied
A  cliproxy  <server-ip>  Proxied
A  keeper    <server-ip>  Proxied
```

Use SSL/TLS mode `Full (strict)`. Do not use `Flexible`.

Add a cache rule:

```text
Hostname in ai.x2r.store, cliproxy.x2r.store, keeper.x2r.store -> Bypass cache
```

If Let's Encrypt issuance fails while Cloudflare proxying is enabled, temporarily switch the record to DNS only, restart Traefik, wait for certificate issuance, then switch proxying back on.

## Initial Setup

Create local runtime files:

```bash
cp .env.example .env
cp config.yaml.template config.yaml
mkdir -p auths logs letsencrypt
touch letsencrypt/acme.json
chmod 600 letsencrypt/acme.json
```

Generate secrets:

```bash
openssl rand -hex 32
openssl rand -hex 24
MANAGEMENT_SECRET="$(openssl rand -hex 32)"
CLIPROXY_INTERNAL_API_KEY="$(scripts/generate-api-key.py)"
printf 'management secret: %s\ninternal api key: %s\n' "$MANAGEMENT_SECRET" "$CLIPROXY_INTERNAL_API_KEY"
```

Edit `.env` and set:

```text
ACME_EMAIL
AI_HOST
CLIPROXY_HOST
CPA_USAGE_KEEPER_HOST
NEW_API_IMAGE_TAG
CLIPROXYAPI_IMAGE_TAG
CPA_USAGE_KEEPER_IMAGE
CPA_USAGE_KEEPER_IMAGE_TAG
NEW_API_SESSION_SECRET
NEW_API_CRYPTO_SECRET
POSTGRES_PASSWORD
REDIS_PASSWORD
CLIPROXY_INTERNAL_API_KEY
MANAGEMENT_SECRET
CPA_USAGE_KEEPER_AUTH_PASSWORD
BACKUP_DIR
```

Optional Cloudflare R2 log archiving variables:

```text
R2_ACCOUNT_ID
R2_BUCKET
R2_ACCESS_KEY_ID
R2_SECRET_ACCESS_KEY
CLIPROXY_LOG_ARCHIVE_R2_PREFIX
CLIPROXY_LOG_ARCHIVE_MIN_AGE_MINUTES
CLIPROXY_LOG_ARCHIVE_DELETE_AFTER_DAYS=1
CLIPROXY_LOG_ARCHIVE_GZIP_LEVEL=1
CLIPROXY_LOG_ARCHIVE_NICE=19
CLIPROXY_LOG_ARCHIVE_IONICE_IDLE=true
CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT
```

Edit `config.yaml` and replace:

```text
replace-with-management-secret
replace-with-internal-new-api-channel-key
```

`remote-management.secret-key` in `config.yaml` must exactly match `MANAGEMENT_SECRET` in `.env`.

`api-keys` in `config.yaml` must exactly match `CLIPROXY_INTERNAL_API_KEY` in `.env`.

CLIProxyAPI hashes a plaintext `remote-management.secret-key` on startup and writes it back to `config.yaml`, so keep `config.yaml` writable by the container.

## Validate Before Production

Validate the rendered Compose file in a temporary directory before touching the server deployment:

```bash
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
cp docker-compose.yml "$tmpdir"/
cp .env.example "$tmpdir/.env"
cp config.yaml.template "$tmpdir/config.yaml"
mkdir -p "$tmpdir/auths" "$tmpdir/logs" "$tmpdir/letsencrypt"
touch "$tmpdir/letsencrypt/acme.json"
chmod 600 "$tmpdir/letsencrypt/acme.json"
(cd "$tmpdir" && docker compose config --format json) > /tmp/api-site-compose.json
scripts/validate-api-site-compose.py \
  /tmp/api-site-compose.json \
  --host ai.x2r.store \
  --cliproxy-host cliproxy.x2r.store \
  --keeper-host keeper.x2r.store
```

The backend Docker network must allow outbound internet access. New API and CLIProxyAPI need outbound connections for upstream API calls, provider access, and validation.

## Start The Stack

```bash
docker compose config
docker compose pull
docker compose up -d
docker compose ps
```

## Configure New API

After first boot, configure New API through the public hostname:

```text
https://ai.x2r.store
```

Required production hardening:

- Rotate default admin credentials.
- Enable invitation-code registration.
- Set new user initial balance to `0`.
- Disable unused authentication methods.
- Disable online payments unless intentionally configured.
- Configure redeem codes as the only public top-up path if using prepaid access.
- Add CLIProxyAPI as an internal New API channel with base URL `http://cliproxyapi:8317`.
- Use `CLIPROXY_INTERNAL_API_KEY` as the channel key.
- Keep official-provider fallback channels limited to admin test groups until billing is reconciled.

See [docs/api-site-runbook.md](docs/api-site-runbook.md) for the full launch checklist.

## Verify Production

Basic public check:

```bash
curl -I https://ai.x2r.store
```

Run the verification helper:

```bash
scripts/verify-api-site.sh
```

For complete launch validation, set test API keys first:

```bash
NEW_API_TEST_API_KEY=sk-... CODEX_TEST_API_KEY=sk-... scripts/verify-api-site.sh
```

The verification helper checks:

- New API public endpoint.
- CLIProxyAPI public endpoint with `CLIPROXY_INTERNAL_API_KEY`.
- CPA Usage Keeper public health endpoint.
- CPA Usage Keeper unauthenticated API access returns `401`.
- `/v1/models` when `NEW_API_TEST_API_KEY` is set.
- `/v1/responses` when `CODEX_TEST_API_KEY` is set.
- New API container access to internal CLIProxyAPI.

## API Usage

Point OpenAI-compatible clients at:

```text
https://ai.x2r.store/v1
```

Example:

```bash
curl https://ai.x2r.store/v1/models \
  -H "Authorization: Bearer sk-..."
```

Responses API smoke test:

```bash
curl https://ai.x2r.store/v1/responses \
  -H "Authorization: Bearer sk-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"codex-cli","input":"Reply with ok.","store":false}'
```

## CLIProxyAPI Management

CLIProxyAPI is public at:

```text
https://cliproxy.x2r.store
```

It also keeps the server loopback binding for local maintenance:

```yaml
127.0.0.1:8317:8317
```

You can manage it through the public hostname with `remote-management.secret-key`, or create an SSH tunnel:

```bash
ssh -L 8317:127.0.0.1:8317 <user>@<server>
```

Then open:

```text
http://127.0.0.1:8317
```

Keep CLIProxyAPI API keys and the management secret enabled. Public access relies on application authentication, not network isolation.

## Auth Priority Management

CLIProxyAPI supports per-auth `priority`. Higher priority auths are selected before lower priority auths; auths with the same priority are distributed by the configured routing strategy.

Use the helper through the public hostname or the SSH tunnel:

```bash
MANAGEMENT_SECRET=... scripts/manage-cliproxy-auth-priority.py list
```

Set one auth file:

```bash
MANAGEMENT_SECRET=... scripts/manage-cliproxy-auth-priority.py set \
  --name codex-a@example.com-plus.json \
  --priority 20 \
  --note "expires 2026-05-06 batch-a"
```

Apply a JSON plan:

```json
[
  {"name": "codex-a@example.com-plus.json", "priority": 20, "note": "batch-a"},
  {"name": "codex-b@example.com-plus.json", "priority": 10, "note": "batch-b"}
]
```

```bash
MANAGEMENT_SECRET=... scripts/manage-cliproxy-auth-priority.py apply priorities.json --dry-run
MANAGEMENT_SECRET=... scripts/manage-cliproxy-auth-priority.py apply priorities.json
```

For short-lived accounts, maintain expiration times and let the helper compute priority and write the expiration into `note`:

```json
[
  {
    "name": "codex-a@example.com-plus.json",
    "expires_at": "2026-05-05T20:00:00+08:00",
    "batch": "batch-a"
  },
  {
    "name": "codex-b@example.com-plus.json",
    "expires_at": "2026-05-06T20:00:00+08:00",
    "batch": "batch-b"
  }
]
```

```bash
MANAGEMENT_SECRET=... scripts/manage-cliproxy-auth-priority.py apply-expiry expiry-plan.json --dry-run
MANAGEMENT_SECRET=... scripts/manage-cliproxy-auth-priority.py apply-expiry expiry-plan.json
```

The computed priority bands are `30` for accounts expiring within 24 hours, `20` for accounts expiring within 48 hours, and `10` for later accounts. Expired accounts fail by default; pass `--allow-expired` to set them to priority `0`.

## Backups

Set `BACKUP_DIR` in `.env` to an absolute path outside this repository, then run:

```bash
scripts/backup-api-site.sh
```

The backup helper captures:

- New API Postgres dump.
- Redis data.
- `.env`.
- `config.yaml`.
- `auths/`.
- `letsencrypt/`.
- CPA Usage Keeper data when the service is running.
- SHA-256 checksums.

The script writes a timestamped backup package under `BACKUP_DIR`. `logs/` is not included because request logs may contain sensitive request and response data. Store backup packages encrypted and off-host. Restore on a new server with:

```bash
scripts/restore-api-site.sh /path/to/cliproxy-api-site-backup.tgz
```

Run a restore drill before meaningful paid usage.

## Latency Profiling

Compare Cloudflare, proxy, and origin latency from a workstation:

```bash
scripts/profile-latency.py \
  --host ai.x2r.store \
  --path /api/status \
  --origin-ip <vps-ip> \
  --runs 10 \
  --csv tmp/local-latency.csv
```

If your terminal does not use proxy environment variables, pass a local proxy explicitly:

```bash
scripts/profile-latency.py \
  --host ai.x2r.store \
  --path /api/status \
  --origin-ip <vps-ip> \
  --proxy http://127.0.0.1:7890
```

On the VPS, compare Traefik loopback to direct New API container access:

```bash
new_api_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{println .IPAddress}}{{end}}' new-api | awk 'NF{print; exit}')"
scripts/profile-origin.py \
  --host ai.x2r.store \
  --path /api/status \
  --cliproxy-url "http://${new_api_ip}:3000/api/status" \
  --runs 10 \
  --csv tmp/origin-latency.csv
```

The `--cliproxy-url` option name is historical in API-site mode; point it at the direct New API container URL.

## Account Import

Convert codex-switcher's multi-account `accounts.json` into CLIProxyAPI Codex auth files:

```bash
scripts/convert-codex-switcher-accounts.py tmp/accounts.json auths
```

The script writes one `codex-<email>-<plan>.json` file per account, sets generated file permissions to `0600`, and prints only generated file paths.

## OAuth Login

Do not permanently expose OAuth callback ports. Run login commands inside the container and use SSH tunneling or temporary port exposure only when needed:

```bash
docker compose exec cliproxyapi /CLIProxyAPI/CLIProxyAPI -no-browser --codex-login
```

## Logs

CLIProxyAPI request logging is disabled by default. If enabled for a short troubleshooting window, it can record request bodies, response bodies, headers, streaming chunks, and upstream API data. Treat `logs/` as sensitive.

The template sets:

```yaml
logging-to-file: true
logs-max-total-size-mb: 2048
request-log: false
```

Adjust `logs-max-total-size-mb` based on available disk space.

When full request logging is temporarily enabled, each request log can be archived without touching files that are still being written:

```bash
scripts/archive-cliproxy-logs.sh
```

The archiver compresses request-log files older than `CLIPROXY_LOG_ARCHIVE_MIN_AGE_MINUTES`, uploads `.gz` files to Cloudflare R2 with `aws s3 cp`, marks successful uploads, and deletes uploaded local copies after `CLIPROXY_LOG_ARCHIVE_DELETE_AFTER_DAYS=1`. Compression defaults to `gzip -1`, `nice -n 19`, and idle `ionice` when available; set `CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT` only if `cpulimit` is installed and a hard CPU cap is required. Install it as a cron job or systemd timer on the host; do not treat R2 upload as declassification because full request logs remain sensitive after compression and upload.

Setup details are in [docs/cliproxy-log-archive-r2-runbook.md](docs/cliproxy-log-archive-r2-runbook.md).

## Troubleshooting

Check service status:

```bash
docker compose ps
docker compose logs --tail=100 traefik
docker compose logs --tail=100 new-api
docker compose logs --tail=100 cliproxyapi
docker compose logs --tail=100 cpa-usage-keeper
```

Common checks:

- HTTPS failure: verify DNS, firewall ports `80` and `443`, Traefik logs, and `letsencrypt/acme.json` permissions.
- Management failure: verify `remote-management.secret-key` in `config.yaml`.
- API failure: verify New API channel configuration, `api-keys`, `auths/`, and CLIProxyAPI logs.
- Repeated bad management keys may trigger a temporary remote IP block; wait for expiry or restart CLIProxyAPI before retesting.

## Development

Run tests:

```bash
python3 -m unittest discover -s tests
```

Run Compose policy validation against a rendered config:

```bash
docker compose config --format json > /tmp/api-site-compose.json
scripts/validate-api-site-compose.py \
  /tmp/api-site-compose.json \
  --host ai.x2r.store \
  --cliproxy-host cliproxy.x2r.store \
  --keeper-host keeper.x2r.store
```

## More Documentation

- [API site runbook](docs/api-site-runbook.md)
- [Historical New API site design notes](docs/superpowers/specs/2026-05-03-new-api-cliproxy-api-site-design.md)
- [Historical deployment implementation plan](docs/superpowers/plans/2026-05-03-new-api-cliproxy-api-site.md)
