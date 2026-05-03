# CLIProxyAPI Deployment Design

Date: 2026-05-02

## Goal

Generate a production-oriented Docker Compose deployment for CLIProxyAPI in this repository. The deployment should use Traefik for automatic HTTPS, expose separate public hostnames for API and administration, enable detailed request logging, and enable CLIProxyAPI's built-in Redis-compatible usage queue without deploying a separate Redis container.

Base domain:

```text
x2r.store
```

Public hostnames:

```text
cliproxy.x2r.store
cliproxy-admin.x2r.store
```

ACME email:

```text
ymviv@qq.com
```

## Architecture

The repository root is the deployment directory. There should not be an extra `deploy/` subdirectory.

Planned committed files:

```text
docker-compose.yml
config.yaml.template
.env.example
.gitignore
README.md
docs/init-discussion.md
docs/superpowers/specs/2026-05-02-cliproxy-deploy-design.md
```

Planned local-only files and directories:

```text
.env
config.yaml
auths/
logs/
letsencrypt/
```

Traffic flow:

```text
Internet
  -> Traefik :80/:443
    -> cliproxy.x2r.store
      -> CLIProxyAPI :8317 on Docker network

    -> cliproxy-admin.x2r.store
      -> Traefik BasicAuth
      -> management UI shell

    -> cliproxy-admin.x2r.store/v0/management
      -> CLIProxyAPI management secret
      -> CLIProxyAPI :8317 on Docker network
```

CLIProxyAPI must not publish host port `8317`. It should only be reachable through the internal Docker network with `expose: 8317`.

Compose service names should be stable because they are used by Traefik and by future internal collectors:

```text
traefik
cliproxyapi
```

## Components

### Traefik

Traefik owns all public HTTP/S ingress:

```text
80  -> redirects to HTTPS
443 -> API and admin routers
```

Traefik should use the Docker provider with `exposedByDefault=false`, so only explicitly labelled containers are routed.

The Docker socket mount should be read-only:

```text
/var/run/docker.sock:/var/run/docker.sock:ro
```

Even read-only Docker socket access is sensitive. The host should be treated as the trust boundary for Traefik.

Traefik should use Let's Encrypt HTTP challenge with:

```text
ACME_EMAIL=ymviv@qq.com
```

The ACME storage file should live under local-only `letsencrypt/`.

### CLIProxyAPI

CLIProxyAPI listens internally on plain HTTP:

```yaml
host: ""
port: 8317
tls:
  enable: false
```

Docker volumes should persist:

```text
./config.yaml:/CLIProxyAPI/config.yaml
./auths:/root/.cli-proxy-api
./logs:/CLIProxyAPI/logs
```

The compose service should use the upstream image:

```text
eceasy/cli-proxy-api:latest
```

It should keep the upstream deployment-mode environment variable available with the local/default empty value:

```yaml
environment:
  DEPLOY: ${DEPLOY:-}
```

## Routing

### API Router

`cliproxy.x2r.store` routes to CLIProxyAPI without Traefik BasicAuth.

This hostname is intended for normal API client traffic. API access is controlled by CLIProxyAPI `api-keys`.

The API hostname must not serve the management UI as a usable entry point. Because CLIProxyAPI management is enabled globally with `allow-remote: true`, the Traefik configuration must explicitly handle these paths on the API hostname:

```text
/management
/management.html
/v0/management
```

The implementation should:

- redirect `/management*` UI paths to `https://${ADMIN_HOST}/management.html`, and
- route `/v0/management*` API paths through a higher-priority BasicAuth-protected router.

Do not rely on Traefik's default rule-length priority for this overlap; set explicit router priorities:

```text
api-management-ui redirect router priority: 110
api-management-api BasicAuth router priority: 100
api router priority: 10
```

The API-host management routers should use these rule shapes:

```text
Host(`${API_HOST}`) && PathPrefix(`/management`)
Host(`${API_HOST}`) && PathPrefix(`/v0/management`)
```

### Admin Router

`cliproxy-admin.x2r.store` routes to the same CLIProxyAPI service. The UI shell attaches a Traefik BasicAuth middleware, but `/v0/management` on this hostname must not use Traefik BasicAuth because the Web UI sends the CLIProxyAPI management key as `Authorization: Bearer <key>`.

This hostname is intended for:

```text
/management
/management.html
/v0/management
```

No IP allowlist is included in the first version.

The admin UI router should protect non-`/v0/management` admin paths with BasicAuth. The admin `/v0/management` router should rely on `remote-management.secret-key`. All management paths must still be protected when reached through `cliproxy.x2r.store`.

## Security

The admin UI and management API have separate protection layers:

```text
management UI shell -> Traefik BasicAuth
management API      -> CLIProxyAPI remote-management.secret-key
```

CLIProxyAPI management config:

```yaml
remote-management:
  allow-remote: true
  secret-key: "replace-with-management-secret"
  disable-control-panel: false
```

Secrets must not be committed to git.

Committed `.env.example` should contain placeholders:

```env
ACME_EMAIL=ymviv@qq.com
API_HOST=cliproxy.x2r.store
ADMIN_HOST=cliproxy-admin.x2r.store
TRAEFIK_BASIC_AUTH_USERS=admin:replace-with-escaped-htpasswd-hash
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

The local `config.yaml` should be copied from `config.yaml.template` and edited on the server. This avoids relying on environment-variable expansion inside CLIProxyAPI config files.

CLIProxyAPI hashes a plaintext `remote-management.secret-key` on startup and writes the hashed value back to `config.yaml`. The bind-mounted `config.yaml` must remain writable by the container.

Password and secret generation should be documented:

```bash
openssl rand -hex 32
htpasswd -nbB admin 'your-admin-password'
```

For Docker Compose labels, all `$` characters in the htpasswd hash must be escaped as `$$`.

## CLIProxyAPI Runtime Config

The deployment should enable:

```yaml
logging-to-file: true
request-log: true
usage-statistics-enabled: true
redis-usage-queue-retention-seconds: 300
logs-max-total-size-mb: 2048
```

`request-log: true` intentionally records detailed request and response data. Documentation should call out that this can include sensitive prompt, response, header, and upstream API data. The operator should treat `logs/` as sensitive.

Detailed request logs can be large. The template should set an explicit log size cap and the README should tell operators to raise or lower it based on available disk space.

The Redis usage queue is CLIProxyAPI's built-in Redis-compatible RESP queue on the same internal port. It does not require an external Redis container. It is intended for a future collector on the same Docker network, for example:

```bash
redis-cli -h cliproxyapi -p 8317 -a "$MANAGEMENT_SECRET" --raw LPOP queue
```

That command is intended to run from a container on the same Docker network, not from the public internet.

The queue is not long-term storage. Any billing, analytics, or alerting system should consume it into a real datastore later.

## Operations

Initial setup should be:

```bash
cp .env.example .env
cp config.yaml.template config.yaml
mkdir -p auths logs letsencrypt
touch letsencrypt/acme.json
chmod 600 letsencrypt/acme.json
```

Then edit `.env` and `config.yaml` with real credentials.

Startup:

```bash
docker compose config
docker compose up -d
docker compose ps
```

Verification:

```bash
curl -I https://cliproxy.x2r.store
curl -I https://cliproxy-admin.x2r.store/management.html
```

Expected admin behavior:

```text
No BasicAuth credentials -> Traefik returns 401
Valid BasicAuth credentials -> management page loads
Management actions -> require CLIProxyAPI management secret
```

OAuth login callback ports should not be permanently exposed. Account login should be handled with container exec commands plus SSH tunneling or temporary exposure when needed.

## Error Handling

If HTTPS fails:

- Check DNS records for `cliproxy.x2r.store` and `cliproxy-admin.x2r.store`.
- Check that ports `80` and `443` are open.
- Check Traefik logs.
- Check `letsencrypt/acme.json` permissions.

If the admin page is reachable through `cliproxy.x2r.store` without BasicAuth, the Traefik router priorities or API router path exclusions are wrong and must be fixed before use.

If admin returns `401`, BasicAuth is working. Retry with credentials.

If admin loads but management APIs fail, verify that the Web UI API address is `https://cliproxy-admin.x2r.store`, not `https://cliproxy.x2r.store`, then verify `remote-management.secret-key` in `config.yaml`.

If API requests fail, verify `api-keys`, upstream auth files under `auths/`, and CLIProxyAPI logs.

## Testing

Before starting services:

```bash
docker compose config
```

After starting services:

```bash
docker compose ps
docker compose logs --tail=100 traefik
docker compose logs --tail=100 cliproxyapi
curl -I https://cliproxy.x2r.store
curl -I https://cliproxy-admin.x2r.store/management.html
```

Admin BasicAuth should be verified with both unauthenticated and authenticated requests.

Management paths should be tested on both hostnames. On the API hostname, UI paths should redirect to the admin hostname and API paths should still require Traefik BasicAuth:

```bash
curl -I https://cliproxy.x2r.store/management
curl -I https://cliproxy.x2r.store/management.html
curl -I https://cliproxy.x2r.store/v0/management/api-key-usage
```

On the admin hostname, management API calls should use the CLIProxyAPI management secret:

```bash
curl -H "Authorization: Bearer $MANAGEMENT_SECRET" https://cliproxy-admin.x2r.store/v0/management/config
```

## Deferred Work

The first version does not include:

- IP allowlist for admin access.
- External Redis.
- Usage collector.
- Long-term analytics storage.
- Billing or quota enforcement.
- Prometheus/Grafana monitoring.

These can be added later without changing the basic public routing model.
