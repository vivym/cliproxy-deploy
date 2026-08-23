# New API gateway deployment

Production Docker Compose deployment for the New API gateway. New API is the
only public user and SDK entry point. Sub2API is an internal model upstream
with a separate administrator route.

The repository root is the complete deployment interface. Every Compose and
operations command runs from this directory and uses the single
`docker-compose.yml` file. The Compose project is fixed as `name: new-api`.

## Topology

```text
Users and SDKs
      |
      v
Traefik :80/:443
      |
      +--> New API :3000 --> Sub2API :8080 --> model providers
      |
Operators only
      `--> Sub2API administrator UI :8080

New API --> dedicated Postgres + Redis
Sub2API  --> dedicated Postgres + Redis
```

Sub2API has no public OpenAI-compatible API route. Configure New API channels
to use `http://sub2api:8080/v1` on the shared Docker edge network.

## Files

```text
docker-compose.yml          Complete New API gateway stack
.env.example                Deployment configuration template
scripts/backup-deployment.sh
scripts/restore-deployment.sh
scripts/restore-new-api.sh
scripts/verify-deployment.sh
docs/architecture/          Current architecture documents
docs/runbooks/              Current operational procedures
tests/                      Deployment and operation contract tests
```

Local runtime state is ignored by Git:

```text
.env
sub2api-data/
sub2api-postgres-data/
sub2api-redis-data/
letsencrypt/
tmp/
```

`data/config.yaml` contains generated Sub2API configuration and secrets. Treat
it as production state.

## Requirements

- Linux with Docker Engine and Docker Compose v2
- Python 3, `curl`, and GNU-compatible `tar`
- DNS for `NEW_API_HOST` and `SUB2API_ADMIN_HOST`
- Reachable TCP ports `80` and `443`

## Initial setup

```bash
cp .env.example .env
openssl rand -hex 32
${EDITOR:-vi} .env
mkdir -p sub2api-data sub2api-postgres-data sub2api-redis-data letsencrypt
touch letsencrypt/acme.json
chmod 600 letsencrypt/acme.json
docker compose config >/dev/null
docker compose up -d
```

Set every required secret in `.env`. Pin `SUB2API_IMAGE_TAG` and
`NEW_API_IMAGE_TAG` to reviewed release tags or immutable digests in
production.

## Public routes

- `NEW_API_HOST`: user, administrator, and SDK entry point
- `SUB2API_ADMIN_HOST`: operator-only Sub2API administrator UI

Both DNS names must point to the host before the first ACME request. Database,
Redis, and the Sub2API OpenAI-compatible API are not published.

## Common operations

```bash
docker compose config
docker compose pull
docker compose up -d
docker compose ps
scripts/verify-deployment.sh
```

Create a consistent backup outside the repository:

```bash
BACKUP_DIR=/var/backups/new-api scripts/backup-deployment.sh
```

Restore every state domain from a backup produced by that command:

```bash
scripts/restore-deployment.sh /path/to/backup-package.tgz
```

Full restore is destructive. It replaces Sub2API runtime state, both Postgres
databases, and both Redis data domains after validating archive paths,
checksums, required files, and the restored Compose environment.

Restore only New API data from a compatible deployment or historical API-site
backup without modifying Sub2API state:

```bash
scripts/restore-new-api.sh /path/to/backup-package.tgz
```

The New API-only restore uses `new-api-postgres.dump` and
`new-api-redis-data/`. It also accepts the historical `newapi-postgres.dump`
and `redis-data/` names at the restore boundary. Historical runtime metadata
may seed missing New API environment values, but never replaces Sub2API state.
Packages without `SHA256SUMS` are rejected by default. After verifying a
historical package by an independent receipt, opt in for that one restore with
`ALLOW_UNVERIFIED_LEGACY_BACKUP=true`.

## Deployment identity

The current deployment owns these explicit identities:

- Compose project: `new-api`
- Edge network: `new-api-edge`
- Data networks: `new-api-data`, `new-api-sub2api-data`
- Named volumes: `new-api-postgres-data`, `new-api-redis-data`

Changing a project, network, or volume name creates a different Docker object.
Do not migrate from an older `sub2api` project by moving directories or
assuming similarly named volumes are reused. Follow
[`docs/runbooks/migrate-to-new-api-deploy.md`](docs/runbooks/migrate-to-new-api-deploy.md)
for a verified backup and full restore into the new identities.

## Security notes

- The Traefik Docker socket mount is inside the host Docker trust boundary even
  when mounted read-only.
- Keep `.env`, `data/config.yaml`, database state, backup packages, and ACME
  material outside Git.
- Operations scripts parse `.env` as strict one-line dotenv data and never
  execute it as shell. Variable interpolation is rejected; single-quote a
  literal `$` in a value.
- Keep the Sub2API upstream allowlist enabled and add specific required domains.
- Restrict `SUB2API_ADMIN_HOST` with an additional access control layer when
  the deployment environment supports one.

## Lark integration

The planned Lark login, wallet grant, and managed subscription integration is
specified in
[`docs/architecture/lark-entitlement-integration.md`](docs/architecture/lark-entitlement-integration.md).
The locally implemented, not-yet-deployed operator correction workflow is in
[`docs/runbooks/lark-entitlement-correction.md`](docs/runbooks/lark-entitlement-correction.md).
Its controller and policy bundle will extend the single root Compose deployment.
