# CLIProxyAPI Single-Domain Deployment Plan

Date: 2026-05-03

## Goal

Keep this repository as the deployment root and run CLIProxyAPI behind Traefik on a single Cloudflare-proxied hostname:

```text
cliproxy.x2r.store
```

The deployment should avoid Traefik BasicAuth because CLIProxyAPI's Web UI and API calls use `Authorization: Bearer ...`; adding BasicAuth in front of those paths conflicts with the application authentication model.

## Desired Files

Committed files:

```text
.gitignore
.env.example
README.md
config.yaml.template
docker-compose.yml
docs/init-discussion.md
docs/superpowers/specs/2026-05-02-cliproxy-deploy-design.md
docs/superpowers/plans/2026-05-03-cliproxy-deploy-config.md
```

Local-only files:

```text
.env
config.yaml
auths/
logs/
letsencrypt/
```

## Compose Shape

`docker-compose.yml` should define two services:

- `traefik`
- `cliproxyapi`

Traefik publishes only:

```text
80:80
443:443
```

CLIProxyAPI must only expose internal port `8317`:

```yaml
expose:
  - "8317"
```

There should be one Traefik HTTP router for CLIProxyAPI:

```text
traefik.http.routers.cliproxyapi.rule=Host(`${API_HOST}`)
```

There should be no `ADMIN_HOST`, no Traefik BasicAuth middleware, no management redirect middleware, and no admin-specific routers.

## Runtime Auth Model

All paths use the same hostname:

```text
https://cliproxy.x2r.store/
https://cliproxy.x2r.store/v1/*
https://cliproxy.x2r.store/management.html
https://cliproxy.x2r.store/v0/management/*
```

Auth responsibility:

```text
/v1/*              -> CLIProxyAPI api-keys
/v0/management/*   -> CLIProxyAPI remote-management.secret-key
/management.html   -> public static UI shell
```

The management UI shell being publicly loadable is acceptable because sensitive actions require `remote-management.secret-key`.

## Environment Template

`.env.example` should contain:

```env
ACME_EMAIL=ymviv@qq.com
API_HOST=cliproxy.x2r.store
DEPLOY=
```

## Verification

Static checks:

```bash
git diff --check
git check-ignore .env config.yaml auths/ logs/ letsencrypt/
ruby -e 'require "yaml"; YAML.load_file("config.yaml.template"); puts "config.yaml.template: valid YAML"'
```

Compose validation:

```bash
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir" /tmp/cliproxy-compose.json' EXIT
cp docker-compose.yml "$tmpdir/docker-compose.yml"
cat > "$tmpdir/.env" <<'EOF'
ACME_EMAIL=ymviv@qq.com
API_HOST=cliproxy.x2r.store
DEPLOY=
EOF
(cd "$tmpdir" && docker compose config --format json) > /tmp/cliproxy-compose.json
jq -e '.services.cliproxyapi.ports == null and (.services.cliproxyapi.expose == ["8317"])' /tmp/cliproxy-compose.json
jq -e '.services.cliproxyapi.labels["traefik.http.routers.cliproxyapi.rule"] == "Host(`cliproxy.x2r.store`)"' /tmp/cliproxy-compose.json
jq -e '([.services.cliproxyapi.labels | keys[] | select(test("admin|basicauth|redirect|management"))] | length) == 0' /tmp/cliproxy-compose.json
```

Runtime verification after deployment:

```bash
curl -I https://cliproxy.x2r.store
curl -I https://cliproxy.x2r.store/management.html
curl -H "Authorization: Bearer $MANAGEMENT_SECRET" https://cliproxy.x2r.store/v0/management/config
curl -H "Authorization: Bearer $CLIENT_API_KEY" https://cliproxy.x2r.store/v1/models
```

Expected:

- No Traefik BasicAuth prompts.
- `/v0/management/*` accepts only the management secret.
- `/v1/*` accepts only client API keys.
- Cloudflare reports dynamic/bypass behavior, not cached API responses.
