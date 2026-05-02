# CLIProxyAPI Deploy

Docker Compose deployment for CLIProxyAPI with Traefik-managed HTTPS.

## Hostnames

Configure DNS A/AAAA records to point at the server:

```text
api.cliproxy.x2r.store
admin.cliproxy.x2r.store
```

Open ports `80` and `443` on the server firewall. Do not expose CLIProxyAPI port `8317` to the public internet.

Traefik uses the Docker socket to discover labelled containers, so Traefik and anyone able to start or label containers on this Docker daemon are inside the deployment trust boundary.

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

`docker compose config` will fail until `.env` contains required values for `ACME_EMAIL`, `API_HOST`, `ADMIN_HOST`, and `TRAEFIK_BASIC_AUTH_USERS`.

Generate separate values for the management secret and client API key:

```bash
MANAGEMENT_SECRET="$(openssl rand -hex 32)"
CLIENT_API_KEY="$(openssl rand -hex 32)"
printf 'management secret: %s\nclient api key: %s\n' "$MANAGEMENT_SECRET" "$CLIENT_API_KEY"
```

Generate Traefik BasicAuth:

```bash
htpasswd -nbB admin 'your-admin-password'
```

Paste the `htpasswd` output into `.env` as `TRAEFIK_BASIC_AUTH_USERS`.
Every `$` in the hash must be escaped as `$$` for Docker Compose interpolation.

Example:

```env
TRAEFIK_BASIC_AUTH_USERS=admin:$$2y$$05$$...
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
curl -I https://api.cliproxy.x2r.store
```

Admin hostname without BasicAuth should return `401`:

```bash
curl -I https://admin.cliproxy.x2r.store/management.html
```

Management paths on the API hostname must also require BasicAuth or otherwise not pass through the unauthenticated API router:

```bash
curl -I https://api.cliproxy.x2r.store/management
curl -I https://api.cliproxy.x2r.store/management.html
curl -I https://api.cliproxy.x2r.store/v0/management/api-key-usage
```

With valid BasicAuth credentials, the management page should load:

```bash
curl -I -u 'admin:your-admin-password' https://admin.cliproxy.x2r.store/management.html
```

Management API actions still require the CLIProxyAPI management secret.

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

If admin access returns `401`, BasicAuth is active. Retry with credentials.

If management API calls fail after BasicAuth succeeds, verify `remote-management.secret-key` in `config.yaml`.

If API requests fail, verify `api-keys`, auth files under `auths/`, and CLIProxyAPI logs.
