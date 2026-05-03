# CLIProxyAPI Deploy

Docker Compose deployment for a New API-fronted public API site with CLIProxyAPI as an internal upstream.

## API Site Mode

This repository now targets `ai.x2r.store` as a New API-fronted public API site. New API is the only public user and SDK entry point. CLIProxyAPI is internal-only and is used as a New API upstream channel.

For implementation and operations, see:

- `docs/superpowers/specs/2026-05-03-new-api-cliproxy-api-site-design.md`
- `docs/superpowers/plans/2026-05-03-new-api-cliproxy-api-site.md`
- `docs/api-site-runbook.md`

## Hostnames

Configure DNS A/AAAA records to point at the server:

```text
ai.x2r.store
```

Open ports `80` and `443` on the server firewall. Do not expose CLIProxyAPI port `8317`, Postgres, Redis, or CPA Usage Keeper to the public internet.

Traefik uses the Docker socket to discover labelled containers, so Traefik and anyone able to start or label containers on this Docker daemon are inside the deployment trust boundary.

## Cloudflare

This hostname is a first-level subdomain of `x2r.store`, so it can use Cloudflare's standard proxied Universal SSL coverage:

```text
A  ai        <server-ip>  Proxied
```

Set Cloudflare SSL/TLS mode to `Full (strict)`. Do not use `Flexible`.

Add a cache rule to bypass cache for the hostname:

```text
Hostname equals ai.x2r.store       -> Bypass cache
```

If initial Let's Encrypt issuance fails while Cloudflare proxying is enabled, temporarily switch the record to DNS only, restart Traefik, wait for certificate issuance, then switch it back to proxied.

## Files

Committed templates:

```text
docker-compose.yml
config.yaml.template
.env.example
```

Local-only runtime files:

```text
.env
config.yaml
auths/
logs/
letsencrypt/
```

## Initial Setup

```bash
cp .env.example .env
cp config.yaml.template config.yaml
mkdir -p auths logs letsencrypt
touch letsencrypt/acme.json
chmod 600 letsencrypt/acme.json
```

`docker compose config` will fail until `.env` contains all required values, including `ACME_EMAIL`, `AI_HOST`, image tags, generated secrets, and database passwords.

Generate separate values for New API secrets, database passwords, Redis password, the CLIProxyAPI management secret, and the internal New API-to-CLIProxyAPI channel key:

```bash
openssl rand -hex 32
openssl rand -hex 24
MANAGEMENT_SECRET="$(openssl rand -hex 32)"
CLIPROXY_INTERNAL_API_KEY="$(scripts/generate-api-key.py)"
printf 'management secret: %s\ninternal api key: %s\n' "$MANAGEMENT_SECRET" "$CLIPROXY_INTERNAL_API_KEY"
```

Edit `config.yaml` and replace:

```text
replace-with-management-secret
replace-with-internal-new-api-channel-key
```

The `remote-management.secret-key` value must exactly match `MANAGEMENT_SECRET` in `.env`. The `api-keys` entry must exactly match `CLIPROXY_INTERNAL_API_KEY` in `.env`.

CLIProxyAPI hashes a plaintext `remote-management.secret-key` on startup and writes it back to `config.yaml`, so keep `config.yaml` writable by the container. CLIProxyAPI remains internal-only in API Site Mode.

## Start

```bash
docker compose config
docker compose up -d
docker compose ps
```

## Verify

API site hostname:

```bash
curl -I https://ai.x2r.store
```

Run the API-site verification helper:

```bash
scripts/verify-api-site.sh
```

New API is the public management, user, and SDK entry point. CLIProxyAPI management and API routes are internal operations surfaces only.

## Latency Profiling

Public profiling targets New API through Traefik at `ai.x2r.store`, not CLIProxyAPI internal management routes. From your local machine, compare the normal route, direct Cloudflare route, and direct VPS origin route for an API-site path:

```bash
scripts/profile-latency.py \
  --host ai.x2r.store \
  --path /api/status \
  --origin-ip <vps-ip> \
  --runs 10 \
  --csv tmp/local-latency.csv
```

If your terminal does not use proxy environment variables, pass the local proxy explicitly:

```bash
scripts/profile-latency.py \
  --host ai.x2r.store \
  --path /api/status \
  --origin-ip <vps-ip> \
  --proxy http://127.0.0.1:7890
```

For protected New API paths, pass the key through an environment variable so it is not printed:

```bash
NEW_API_KEY=sk-... scripts/profile-latency.py \
  --host ai.x2r.store \
  --origin-ip <vps-ip> \
  --path /v1/models \
  --api-key-env NEW_API_KEY
```

On the VPS in API Site Mode, use origin profiling as an internal diagnostic by comparing Traefik loopback to direct New API container access. Override the direct target explicitly; the script's `cliproxy-direct` label is historical in this mode.

```bash
new_api_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{println .IPAddress}}{{end}}' new-api | awk 'NF{print; exit}')"
scripts/profile-origin.py \
  --host ai.x2r.store \
  --path /api/status \
  --cliproxy-url "http://${new_api_ip}:3000/api/status" \
  --runs 10 \
  --csv tmp/origin-latency.csv
```

The derived values are approximate:

```text
proxy_delta_ms      ~= cf-default - cf-direct
cloudflare_delta_ms ~= cf-direct - origin-direct
traefik_overhead_ms ~= traefik-loopback - direct New API container
```

## Logs

CLIProxyAPI request logging is disabled by default. If enabled for a short troubleshooting window, it can record request bodies, response bodies, headers, streaming chunks, and upstream API data. Treat `logs/` as sensitive.

The template sets:

```yaml
logging-to-file: true
logs-max-total-size-mb: 2048
```

Adjust `logs-max-total-size-mb` based on available disk space.

## Built-In Usage Queue

CLIProxyAPI's Redis-compatible usage queue is enabled through:

```yaml
usage-statistics-enabled: true
redis-usage-queue-retention-seconds: 3600
```

This is not an external Redis service. It is a small RESP interface on CLIProxyAPI's internal `8317` port. A future collector on the same Docker network can read it with:

```bash
redis-cli -h cliproxyapi -p 8317 -a "$MANAGEMENT_SECRET" --raw LPOP queue
```

Do not expose this port publicly. The queue is short-term memory storage, not a billing or analytics database.

## Session Affinity

The template enables CLIProxyAPI session affinity so one client session prefers the same upstream account when possible:

```yaml
routing:
  strategy: "round-robin"
  session-affinity: true
  session-affinity-ttl: "2h"
```

This is not a hard pin. CLIProxyAPI may still switch credentials when retrying or when the previously selected credential is unavailable.

## WebSocket Auth

The template enables API-key authentication for CLIProxyAPI WebSocket clients:

```yaml
ws-auth: true
```

This protects internal CLIProxyAPI `/v1/ws` clients that connect on the backend network. HTTP API requests and management API requests continue to use their existing `api-keys` and `remote-management.secret-key` authentication.

## Convert Codex Switcher Accounts

Use the helper script to convert codex-switcher's multi-account `accounts.json` into CLIProxyAPI Codex auth files:

```bash
scripts/convert-codex-switcher-accounts.py tmp/accounts.json auths
```

The script writes one `codex-<email>-<plan>.json` file per account, sets generated file permissions to `0600`, and prints only generated file paths.

## OAuth Login

Do not permanently expose OAuth callback ports. Run login commands inside the container and use SSH tunneling or temporary port exposure only when needed.

Example:

```bash
docker compose exec cliproxyapi /CLIProxyAPI/CLIProxyAPI -no-browser --codex-login
```

## Troubleshooting

Check service status:

```bash
docker compose ps
docker compose logs --tail=100 traefik
docker compose logs --tail=100 cliproxyapi
```

If HTTPS fails, check DNS, firewall ports `80`/`443`, Traefik logs, and `letsencrypt/acme.json` permissions.

If management API calls fail, verify `remote-management.secret-key` in `config.yaml`. Repeated failed management-key attempts may trigger a temporary remote IP block; wait for it to expire or restart CLIProxyAPI before retesting.

If API requests fail, verify `api-keys`, auth files under `auths/`, and CLIProxyAPI logs.
