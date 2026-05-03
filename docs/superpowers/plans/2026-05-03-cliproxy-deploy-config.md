# CLIProxyAPI Deploy Config Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Generate root-level Docker Compose deployment files for CLIProxyAPI with Traefik HTTPS, protected admin routing, request logging, and the built-in Redis-compatible usage queue.

**Architecture:** The repository root is the deployment directory. Traefik publishes ports `80` and `443`, terminates HTTPS, and routes API/admin traffic to a private `cliproxyapi:8317` service on the Docker network. Runtime secrets stay out of git through `.env` and local `config.yaml`, while committed templates document every required value.

**Tech Stack:** Docker Compose v2, Traefik v3, CLIProxyAPI Docker image `eceasy/cli-proxy-api:latest`, YAML, Markdown, shell validation.

---

## Source Spec

Implement from:

```text
docs/superpowers/specs/2026-05-02-cliproxy-deploy-design.md
```

Do not create a `deploy/` subdirectory. All deployment files belong at repository root.

This plan document must be committed by the planner before implementation begins. The final file-list verification assumes the plan is already tracked.

## File Structure

Create or modify these files:

- Create: `.gitignore`
  - Responsibility: keep local credentials, generated config, runtime auth files, logs, and ACME state out of git.
- Create: `.env.example`
  - Responsibility: document environment variables consumed by `docker-compose.yml`.
- Create: `config.yaml.template`
  - Responsibility: provide a safe CLIProxyAPI config template with placeholders and required runtime features enabled.
- Create: `docker-compose.yml`
  - Responsibility: define Traefik and CLIProxyAPI services, HTTPS, routing, BasicAuth middleware, volumes, and network.
- Modify: `README.md`
  - Responsibility: operator quickstart, DNS prerequisites, secret generation, deployment, verification, sensitive logging notes, usage queue notes, and OAuth login guidance.
- Keep: `docs/init-discussion.md`
  - Responsibility: historical discussion; do not edit unless implementation contradicts it.
- Keep: `docs/superpowers/specs/2026-05-02-cliproxy-deploy-design.md`
  - Responsibility: approved design source; do not edit during implementation unless the plan is found to be wrong.

Local-only files that must stay ignored:

```text
.env
config.yaml
auths/
logs/
letsencrypt/
```

---

### Task 1: Ignore Local Runtime State And Add Env Template

**Files:**
- Create: `.gitignore`
- Create: `.env.example`

- [ ] **Step 1: Add `.gitignore`**

Create `.gitignore` with exactly:

```gitignore
# Local git worktrees
.worktrees/

# Local secrets and generated runtime config
.env
config.yaml

# CLIProxyAPI runtime state
auths/
logs/

# Traefik ACME certificates
letsencrypt/

# Local editor and OS files
.DS_Store
```

- [ ] **Step 2: Add `.env.example`**

Create `.env.example` with exactly:

```env
# Let's Encrypt account email.
ACME_EMAIL=ymviv@qq.com

# Public hostnames.
API_HOST=cliproxy.x2r.store
ADMIN_HOST=cliproxy-admin.x2r.store

# Traefik BasicAuth users.
# Generate with:
#   htpasswd -nbB admin 'your-admin-password'
# Then replace every "$" with "$$" before pasting into this value.
TRAEFIK_BASIC_AUTH_USERS=admin:replace-with-escaped-htpasswd-hash

# Passed through for compatibility with the upstream CLIProxyAPI compose file.
# Leave empty for normal local/server filesystem deployment.
DEPLOY=
```

- [ ] **Step 3: Verify ignore behavior**

Run:

```bash
git check-ignore .worktrees/ .env config.yaml auths/ logs/ letsencrypt/
```

Expected: each path is printed once:

```text
.worktrees/
.env
config.yaml
auths/
logs/
letsencrypt/
```

- [ ] **Step 4: Verify template has no real secrets**

Run:

```bash
rg -q "replace-with-escaped-htpasswd-hash" .env.example
rg -q "your-admin-password" .env.example
```

Expected: exit `0`.

- [ ] **Step 5: Commit**

Run:

```bash
git add .gitignore .env.example
git commit -m "chore: add deploy environment template"
```

---

### Task 2: Add CLIProxyAPI Config Template

**Files:**
- Create: `config.yaml.template`

- [ ] **Step 1: Create `config.yaml.template`**

Create `config.yaml.template` with exactly:

```yaml
# Server host/interface to bind to. Empty binds all interfaces inside the container.
host: ""
port: 8317

# TLS is terminated by Traefik, so CLIProxyAPI listens on internal HTTP.
tls:
  enable: false
  cert: ""
  key: ""

# Management API and bundled web control panel.
# CLIProxyAPI will hash a plaintext secret-key on startup and write it back to config.yaml.
remote-management:
  allow-remote: true
  secret-key: "replace-with-management-secret"
  disable-control-panel: false
  panel-github-repository: "https://github.com/router-for-me/Cli-Proxy-API-Management-Center"

# Mounted to ./auths by docker-compose.yml.
auth-dir: "~/.cli-proxy-api"

# Client API keys accepted by CLIProxyAPI.
api-keys:
  - "replace-with-client-api-key"

debug: false

# Keep pprof private and disabled.
pprof:
  enable: false
  addr: "127.0.0.1:8316"

commercial-mode: false

# Request logs are intentionally enabled for this deployment.
# Treat ./logs as sensitive because request-log captures request/response data.
logging-to-file: true
logs-max-total-size-mb: 2048
error-logs-max-files: 10
request-log: true

# Enables in-memory usage stats and the built-in Redis-compatible usage queue.
usage-statistics-enabled: true
redis-usage-queue-retention-seconds: 300

proxy-url: ""
force-model-prefix: false
passthrough-headers: false

request-retry: 3
max-retry-credentials: 0
max-retry-interval: 30

ws-auth: false
```

- [ ] **Step 2: Verify required keys are present**

Run:

```bash
for pattern in \
  "request-log: true" \
  "usage-statistics-enabled: true" \
  "redis-usage-queue-retention-seconds: 300" \
  "remote-management:" \
  "allow-remote: true" \
  "secret-key:"; do
  rg -q "$pattern" config.yaml.template
done
```

Expected: exit `0`.

- [ ] **Step 3: Parse template as YAML**

Run:

```bash
ruby -e 'require "yaml"; YAML.load_file("config.yaml.template"); puts "config.yaml.template: valid YAML"'
```

Expected:

```text
config.yaml.template: valid YAML
```

- [ ] **Step 4: Verify no committed real secret**

Run:

```bash
rg -q 'secret-key: "replace-with-management-secret"' config.yaml.template
rg -q -- '- "replace-with-client-api-key"' config.yaml.template
```

Expected: exit `0`.

- [ ] **Step 5: Commit**

Run:

```bash
git add config.yaml.template
git commit -m "chore: add cliproxy config template"
```

---

### Task 3: Add Docker Compose Deployment

**Files:**
- Create: `docker-compose.yml`

- [ ] **Step 1: Create `docker-compose.yml`**

Create `docker-compose.yml` with exactly:

```yaml
services:
  traefik:
    image: traefik:v3.6
    container_name: traefik
    restart: unless-stopped
    command:
      - "--api.dashboard=false"
      - "--providers.docker=true"
      - "--providers.docker.exposedbydefault=false"
      - "--entrypoints.web.address=:80"
      - "--entrypoints.websecure.address=:443"
      - "--entrypoints.web.http.redirections.entrypoint.to=websecure"
      - "--entrypoints.web.http.redirections.entrypoint.scheme=https"
      - "--certificatesresolvers.le.acme.email=${ACME_EMAIL:?set ACME_EMAIL}"
      - "--certificatesresolvers.le.acme.storage=/letsencrypt/acme.json"
      - "--certificatesresolvers.le.acme.httpchallenge=true"
      - "--certificatesresolvers.le.acme.httpchallenge.entrypoint=web"
      - "--accesslog=true"
      - "--log.level=INFO"
    ports:
      - "80:80"
      - "443:443"
    volumes:
      # Docker socket access lets Traefik inspect containers and should be
      # treated as host-level Docker API access even with a read-only mount.
      - "/var/run/docker.sock:/var/run/docker.sock:ro"
      - "./letsencrypt:/letsencrypt"
    networks:
      - proxy

  cliproxyapi:
    image: eceasy/cli-proxy-api:latest
    container_name: cliproxyapi
    restart: unless-stopped
    pull_policy: always
    environment:
      DEPLOY: ${DEPLOY:-}
    volumes:
      - "./config.yaml:/CLIProxyAPI/config.yaml"
      - "./auths:/root/.cli-proxy-api"
      - "./logs:/CLIProxyAPI/logs"
    expose:
      - "8317"
    networks:
      - proxy
    labels:
      - "traefik.enable=true"
      - "traefik.docker.network=proxy"

      # Shared service definition.
      - "traefik.http.services.cliproxyapi.loadbalancer.server.port=8317"

      # Public API route. Management paths are handled by the higher-priority protected route below.
      - "traefik.http.routers.cliproxyapi-api.rule=Host(`${API_HOST:?set API_HOST}`)"
      - "traefik.http.routers.cliproxyapi-api.priority=10"
      - "traefik.http.routers.cliproxyapi-api.entrypoints=websecure"
      - "traefik.http.routers.cliproxyapi-api.tls=true"
      - "traefik.http.routers.cliproxyapi-api.tls.certresolver=le"
      - "traefik.http.routers.cliproxyapi-api.service=cliproxyapi"

      # Protect management paths even when reached through the API hostname.
      - "traefik.http.routers.cliproxyapi-api-management.rule=Host(`${API_HOST:?set API_HOST}`) && (PathPrefix(`/management`) || PathPrefix(`/v0/management`))"
      - "traefik.http.routers.cliproxyapi-api-management.priority=100"
      - "traefik.http.routers.cliproxyapi-api-management.entrypoints=websecure"
      - "traefik.http.routers.cliproxyapi-api-management.tls=true"
      - "traefik.http.routers.cliproxyapi-api-management.tls.certresolver=le"
      - "traefik.http.routers.cliproxyapi-api-management.middlewares=admin-auth"
      - "traefik.http.routers.cliproxyapi-api-management.service=cliproxyapi"

      # Admin hostname.
      - "traefik.http.routers.cliproxyapi-admin.rule=Host(`${ADMIN_HOST:?set ADMIN_HOST}`)"
      - "traefik.http.routers.cliproxyapi-admin.entrypoints=websecure"
      - "traefik.http.routers.cliproxyapi-admin.tls=true"
      - "traefik.http.routers.cliproxyapi-admin.tls.certresolver=le"
      - "traefik.http.routers.cliproxyapi-admin.middlewares=admin-auth"
      - "traefik.http.routers.cliproxyapi-admin.service=cliproxyapi"

      # BasicAuth for admin hostname and management paths on API hostname.
      - "traefik.http.middlewares.admin-auth.basicauth.users=${TRAEFIK_BASIC_AUTH_USERS:?set TRAEFIK_BASIC_AUTH_USERS}"

networks:
  proxy:
    name: proxy
```

- [ ] **Step 2: Run static compose validation**

Run:

```bash
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir" /tmp/cliproxy-compose.json' EXIT
cp docker-compose.yml "$tmpdir/docker-compose.yml"
cat > "$tmpdir/.env" <<'EOF'
ACME_EMAIL=ymviv@qq.com
API_HOST=cliproxy.x2r.store
ADMIN_HOST=cliproxy-admin.x2r.store
TRAEFIK_BASIC_AUTH_USERS=admin:$$2y$$05$$abcdefghijklmnopqrstuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuu
DEPLOY=
EOF
(cd "$tmpdir" && docker compose config --format json) > /tmp/cliproxy-compose.json
jq -e '.services.cliproxyapi.labels["traefik.http.routers.cliproxyapi-api-management.priority"] == "100"' /tmp/cliproxy-compose.json
jq -e '.services.cliproxyapi.labels["traefik.http.routers.cliproxyapi-api.priority"] == "10"' /tmp/cliproxy-compose.json
jq -e '.services.cliproxyapi.labels["traefik.http.routers.cliproxyapi-api-management.rule"] == "Host(`cliproxy.x2r.store`) && (PathPrefix(`/management`) || PathPrefix(`/v0/management`))"' /tmp/cliproxy-compose.json
jq -e '.services.cliproxyapi.labels["traefik.http.middlewares.admin-auth.basicauth.users"] | contains("$$2y$$05$$")' /tmp/cliproxy-compose.json
jq -e '.services.cliproxyapi.ports == null and (.services.cliproxyapi.expose == ["8317"])' /tmp/cliproxy-compose.json
```

Expected:

```text
true
true
true
true
true
```

`docker compose config --format json` preserves escaped `$$` in the rendered label. At container creation, Docker Compose writes a single `$` to the actual container label; the committed `.env.example` and README must continue to instruct operators to escape hash dollars as `$$`.

- [ ] **Step 3: Commit**

Run:

```bash
git add docker-compose.yml
git commit -m "feat: add cliproxy compose deployment"
```

---

### Task 4: Update Operator README

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Replace `README.md` with operator guide**

Replace `README.md` with exactly:

````markdown
# CLIProxyAPI Deploy

Docker Compose deployment for CLIProxyAPI with Traefik-managed HTTPS.

## Hostnames

Configure DNS A/AAAA records to point at the server:

```text
cliproxy.x2r.store
cliproxy-admin.x2r.store
```

Open ports `80` and `443` on the server firewall. Do not expose CLIProxyAPI port `8317` to the public internet.

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
curl -I https://cliproxy.x2r.store
```

Admin hostname without BasicAuth should return `401`:

```bash
curl -I https://cliproxy-admin.x2r.store/management.html
```

Management paths on the API hostname must also require BasicAuth or otherwise not pass through the unauthenticated API router:

```bash
curl -I https://cliproxy.x2r.store/management
curl -I https://cliproxy.x2r.store/management.html
curl -I https://cliproxy.x2r.store/v0/management/api-key-usage
```

With valid BasicAuth credentials, the management page should load:

```bash
curl -I -u 'admin:your-admin-password' https://cliproxy-admin.x2r.store/management.html
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
````

- [ ] **Step 2: Verify README mentions critical security checks**

Run:

```bash
for pattern in \
  'Do not expose CLIProxyAPI port `8317`' \
  'Every `\$` in the hash must be escaped' \
  'request-log: true' \
  'cliproxy.x2r.store/v0/management/api-key-usage' \
  'not an external Redis service'; do
  rg -q "$pattern" README.md
done
```

Expected: exit `0`.

- [ ] **Step 3: Commit**

Run:

```bash
git add README.md
git commit -m "docs: add cliproxy deploy runbook"
```

---

### Task 5: Final Static Verification

**Files:**
- Verify: `.gitignore`
- Verify: `.env.example`
- Verify: `config.yaml.template`
- Verify: `docker-compose.yml`
- Verify: `README.md`

- [ ] **Step 1: Check markdown and whitespace diff**

Run:

```bash
git diff --check HEAD
```

Expected: no output, exit `0`.

- [ ] **Step 2: Confirm ignored runtime files**

Run:

```bash
git check-ignore .worktrees/ .env config.yaml auths/ logs/ letsencrypt/
```

Expected:

```text
.worktrees/
.env
config.yaml
auths/
logs/
letsencrypt/
```

- [ ] **Step 3: Confirm no runtime secrets were committed**

Run:

```bash
git ls-files | rg '(^|/)(\.env|config\.yaml)$'
```

Expected: no output, exit `1`.

- [ ] **Step 4: Confirm placeholders remain in committed templates**

Run:

```bash
rg -q "replace-with-management-secret" config.yaml.template
rg -q "replace-with-client-api-key" config.yaml.template
rg -q "replace-with-escaped-htpasswd-hash" .env.example
```

Expected: exit `0`.

- [ ] **Step 5: Parse config template as YAML**

Run:

```bash
ruby -e 'require "yaml"; YAML.load_file("config.yaml.template"); puts "config.yaml.template: valid YAML"'
```

Expected:

```text
config.yaml.template: valid YAML
```

- [ ] **Step 6: Run full temporary Compose validation**

Run:

```bash
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir" /tmp/cliproxy-compose.json' EXIT
cp docker-compose.yml "$tmpdir/docker-compose.yml"
cat > "$tmpdir/.env" <<'EOF'
ACME_EMAIL=ymviv@qq.com
API_HOST=cliproxy.x2r.store
ADMIN_HOST=cliproxy-admin.x2r.store
TRAEFIK_BASIC_AUTH_USERS=admin:$$2y$$05$$abcdefghijklmnopqrstuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuuu
DEPLOY=
EOF
(cd "$tmpdir" && docker compose config --format json) > /tmp/cliproxy-compose.json
jq -e '([.services.traefik.ports[].target] == [80, 443]) and ([.services.traefik.ports[].published] == ["80", "443"])' /tmp/cliproxy-compose.json
jq -e '.services.cliproxyapi.ports == null and (.services.cliproxyapi.expose == ["8317"])' /tmp/cliproxy-compose.json
jq -e '.services.cliproxyapi.labels["traefik.http.routers.cliproxyapi-api-management.priority"] == "100"' /tmp/cliproxy-compose.json
jq -e '.services.cliproxyapi.labels["traefik.http.routers.cliproxyapi-api.priority"] == "10"' /tmp/cliproxy-compose.json
jq -e '.services.cliproxyapi.labels["traefik.http.routers.cliproxyapi-api-management.rule"] == "Host(`cliproxy.x2r.store`) && (PathPrefix(`/management`) || PathPrefix(`/v0/management`))"' /tmp/cliproxy-compose.json
jq -e '.services.cliproxyapi.labels["traefik.http.middlewares.admin-auth.basicauth.users"] | contains("$$2y$$05$$")' /tmp/cliproxy-compose.json
```

Expected:

```text
true
true
true
true
true
true
```

`docker compose config --format json` preserves escaped `$$` in the rendered BasicAuth label. That is expected for static validation; the actual container label resolves to single `$` at create time.

- [ ] **Step 7: Review final file list**

Run:

```bash
git ls-files
```

Expected committed deployment files include:

```text
.env.example
.gitignore
README.md
config.yaml.template
docker-compose.yml
docs/init-discussion.md
docs/superpowers/plans/2026-05-03-cliproxy-deploy-config.md
docs/superpowers/specs/2026-05-02-cliproxy-deploy-design.md
```

- [ ] **Step 8: Commit verification-only fixes if needed**

If any verification command fails because a committed file is wrong, fix the file and commit:

```bash
git add <fixed-files>
git commit -m "fix: align deploy config verification"
```

If all verification passes, do not create an empty commit.

---

## Runtime Deployment Commands For Operator

These commands are documented in `README.md`; implementation should not run them against production unless explicitly requested:

```bash
cp .env.example .env
cp config.yaml.template config.yaml
mkdir -p auths logs letsencrypt
touch letsencrypt/acme.json
chmod 600 letsencrypt/acme.json
docker compose config
docker compose up -d
docker compose ps
```

## Notes For Implementers

- Do not commit `.env`, `config.yaml`, `auths/`, `logs/`, or `letsencrypt/`.
- Do not add `ports: "8317:8317"` to CLIProxyAPI.
- Do not add a standalone Redis container.
- Do not remove BasicAuth from `cliproxy-admin.x2r.store`.
- Do not leave API-host management paths protected only by CLIProxyAPI's management secret; Traefik BasicAuth must also apply.
- Do not make `config.yaml` read-only; CLIProxyAPI may rewrite the management secret as a hash.
