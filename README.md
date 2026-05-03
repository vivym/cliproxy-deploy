# CLIProxyAPI Deploy

Docker Compose deployment for CLIProxyAPI with Traefik-managed HTTPS.

## Hostnames

Configure DNS A/AAAA records to point at the server:

```text
cliproxy.x2r.store
```

Open ports `80` and `443` on the server firewall. Do not expose CLIProxyAPI port `8317` to the public internet.

Traefik uses the Docker socket to discover labelled containers, so Traefik and anyone able to start or label containers on this Docker daemon are inside the deployment trust boundary.

## Cloudflare

This hostname is a first-level subdomain of `x2r.store`, so it can use Cloudflare's standard proxied Universal SSL coverage:

```text
A  cliproxy        <server-ip>  Proxied
```

Set Cloudflare SSL/TLS mode to `Full (strict)`. Do not use `Flexible`.

Add a cache rule to bypass cache for the hostname:

```text
Hostname equals cliproxy.x2r.store       -> Bypass cache
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

`docker compose config` will fail until `.env` contains required values for `ACME_EMAIL` and `API_HOST`.

Generate separate values for the management secret and client API key:

```bash
MANAGEMENT_SECRET="$(openssl rand -hex 32)"
CLIENT_API_KEY="$(scripts/generate-api-key.py)"
printf 'management secret: %s\nclient api key: %s\n' "$MANAGEMENT_SECRET" "$CLIENT_API_KEY"
```

To generate multiple client keys as a `config.yaml` snippet:

```bash
scripts/generate-api-key.py -n 3 --yaml
```

Edit `config.yaml` and replace:

```text
replace-with-management-secret
replace-with-client-api-key
```

CLIProxyAPI hashes a plaintext `remote-management.secret-key` on startup and writes it back to `config.yaml`, so keep `config.yaml` writable by the container.

## Start

```bash
docker compose config
docker compose up -d
docker compose ps
```

## Verify

API hostname:

```bash
curl -I https://cliproxy.x2r.store
```

Open the management UI through the same hostname:

```text
https://cliproxy.x2r.store/management.html
```

When the UI asks for the API address, use:

```text
https://cliproxy.x2r.store
```

Management API actions require the CLIProxyAPI management secret:

```bash
curl -H "Authorization: Bearer $MANAGEMENT_SECRET" https://cliproxy.x2r.store/v0/management/config
```

Public API requests still require a client API key:

```bash
curl -H "Authorization: Bearer $CLIENT_API_KEY" https://cliproxy.x2r.store/v1/models
```

The management page itself is public static UI. Sensitive management actions are protected by `remote-management.secret-key`; public API calls are protected by `api-keys`.

## Latency Profiling

From your local machine, compare the normal route, direct Cloudflare route, and direct VPS origin route:

```bash
scripts/profile-latency.py --origin-ip <vps-ip> --runs 10 --csv tmp/local-latency.csv
```

If your terminal does not use proxy environment variables, pass the local proxy explicitly:

```bash
scripts/profile-latency.py --origin-ip <vps-ip> --proxy http://127.0.0.1:7890
```

For protected API paths, pass the key through an environment variable so it is not printed:

```bash
CLIENT_API_KEY=sk-... scripts/profile-latency.py \
  --origin-ip <vps-ip> \
  --path /v1/models \
  --api-key-env CLIENT_API_KEY
```

On the VPS, compare Traefik loopback against direct CLIProxyAPI container access:

```bash
scripts/profile-origin.py --runs 10 --csv tmp/origin-latency.csv
```

The derived values are approximate:

```text
proxy_delta_ms      ~= cf-default - cf-direct
cloudflare_delta_ms ~= cf-direct - origin-direct
traefik_overhead_ms ~= traefik-loopback - cliproxy-direct
```

## Logs

`request-log: true` is enabled. This can record request bodies, response bodies, headers, streaming chunks, and upstream API data. Treat `logs/` as sensitive.

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
redis-usage-queue-retention-seconds: 300
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

This protects `/v1/ws` on the public hostname. HTTP API requests and management API requests continue to use their existing `api-keys` and `remote-management.secret-key` authentication.

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
