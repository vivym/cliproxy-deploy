# CLIProxyAPI Deployment Design

Date: 2026-05-02
Updated: 2026-05-03

## Goal

Generate a production-oriented Docker Compose deployment for CLIProxyAPI in this repository. The deployment uses Traefik for automatic HTTPS, exposes a single Cloudflare-proxied hostname, enables detailed request logging, enables session affinity for upstream account selection, and enables CLIProxyAPI's built-in Redis-compatible usage queue without deploying a separate Redis container.

Public hostname:

```text
cliproxy.x2r.store
```

ACME email:

```text
ymviv@qq.com
```

## Architecture

The repository root is the deployment directory. There is no `deploy/` subdirectory.

Traffic flow:

```text
Internet
  -> Cloudflare
    -> Traefik :80/:443
      -> cliproxy.x2r.store
        -> CLIProxyAPI :8317 on Docker network
```

CLIProxyAPI must not publish host port `8317`. It should only be reachable through the internal Docker network with `expose: 8317`.

Local-only runtime files:

```text
.env
config.yaml
auths/
logs/
letsencrypt/
```

## Components

### Traefik

Traefik owns public HTTP/S ingress:

```text
80  -> redirects to HTTPS
443 -> cliproxy.x2r.store
```

Traefik uses Docker provider discovery with `exposedByDefault=false`, so only explicitly labelled containers are routed.

The Docker socket mount is read-only:

```text
/var/run/docker.sock:/var/run/docker.sock:ro
```

Even read-only Docker socket access is sensitive. The host should be treated as the trust boundary for Traefik.

Traefik uses Let's Encrypt HTTP challenge with local-only ACME storage under `letsencrypt/`.

### CLIProxyAPI

CLIProxyAPI listens internally on plain HTTP:

```yaml
host: ""
port: 8317
tls:
  enable: false
```

Docker volumes persist:

```text
./config.yaml:/CLIProxyAPI/config.yaml
./auths:/root/.cli-proxy-api
./logs:/CLIProxyAPI/logs
```

The compose service uses:

```text
eceasy/cli-proxy-api:latest
```

## Routing

There is one public Traefik router:

```text
Host(`${API_HOST}`)
```

All paths route to CLIProxyAPI:

```text
/                  -> CLIProxyAPI
/v1/*              -> CLIProxyAPI public API, protected by api-keys
/management.html   -> CLIProxyAPI bundled management UI
/v0/management/*   -> CLIProxyAPI management API, protected by remote-management.secret-key
```

Traefik BasicAuth is intentionally not used. The Web UI and provider test calls use `Authorization: Bearer ...`; Traefik BasicAuth also uses `Authorization`, which causes authentication conflicts.

## Security

The management UI shell is publicly loadable. Sensitive actions are protected by CLIProxyAPI's management key:

```yaml
remote-management:
  allow-remote: true
  secret-key: "replace-with-management-secret"
  disable-control-panel: false
```

Public API calls are protected by CLIProxyAPI `api-keys`.

Secrets must not be committed to git. Committed `.env.example` should contain only:

```env
ACME_EMAIL=ymviv@qq.com
API_HOST=cliproxy.x2r.store
DEPLOY=
```

Committed `config.yaml.template` should contain placeholders:

```yaml
api-keys:
  - "replace-with-client-api-key"

remote-management:
  allow-remote: true
  secret-key: "replace-with-management-secret"
  disable-control-panel: false
```

CLIProxyAPI hashes a plaintext `remote-management.secret-key` on startup and writes the hashed value back to `config.yaml`, so the bind-mounted config must remain writable by the container.

## Runtime Config

The deployment enables:

```yaml
logging-to-file: true
request-log: true
usage-statistics-enabled: true
redis-usage-queue-retention-seconds: 300
logs-max-total-size-mb: 2048
routing:
  strategy: "round-robin"
  session-affinity: true
  session-affinity-ttl: "2h"
```

`request-log: true` can record prompts, responses, headers, streaming chunks, and upstream API data. Treat `logs/` as sensitive.

The Redis usage queue is CLIProxyAPI's built-in Redis-compatible RESP queue on the same internal port. It does not require an external Redis container and should not be exposed publicly.

Session affinity makes a client session prefer the same upstream account for two hours when possible. It is still allowed to move when retry or credential availability requires it.

## Operations

Initial setup:

```bash
cp .env.example .env
cp config.yaml.template config.yaml
mkdir -p auths logs letsencrypt
touch letsencrypt/acme.json
chmod 600 letsencrypt/acme.json
```

Generate separate values for the management secret and client API key:

```bash
openssl rand -hex 32
openssl rand -hex 32
```

Start:

```bash
docker compose config
docker compose up -d
docker compose ps
```

Verify:

```bash
curl -I https://cliproxy.x2r.store
curl -I https://cliproxy.x2r.store/management.html
curl -H "Authorization: Bearer $MANAGEMENT_SECRET" https://cliproxy.x2r.store/v0/management/config
curl -H "Authorization: Bearer $CLIENT_API_KEY" https://cliproxy.x2r.store/v1/models
```

## Cloudflare

Use one proxied DNS record:

```text
A  cliproxy  <server-ip>  Proxied
```

Set SSL/TLS mode to `Full (strict)`. Do not use `Flexible`.

Add a cache rule to bypass cache:

```text
Hostname equals cliproxy.x2r.store -> Bypass cache
```

## Deferred Work

Optional future hardening:

- Cloudflare Access for `cliproxy.x2r.store/management*` and `/v0/management*`.
- External collector for the built-in usage queue.
- Long-term analytics storage.
- Monitoring and alerting.
