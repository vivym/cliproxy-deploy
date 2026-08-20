# Sub2API + New API deployment

Production Docker Compose deployment for Sub2API and New API behind a shared
Traefik instance.

The repository root is the deployment interface. Run all Compose and operation
commands from this directory.

## Topology

```text
Internet
  |
  v
Traefik :80/:443
  |-- Sub2API API and admin UI :8080
  `-- New API :3000

Sub2API -> Postgres + Redis
New API -> dedicated Postgres + Redis
```

The base Compose file deploys Sub2API. The New API file is an overlay and must
be used together with the base file. The Compose project is explicitly fixed
as `name: sub2api` so moving the repository does not silently select different
New API named volumes.

## Files

```text
docker-compose.yml          Sub2API, Traefik, Postgres, and Redis
docker-compose.newapi.yml   New API and its dedicated Postgres and Redis
.env.example                Deployment configuration template
scripts/backup-deployment.sh
scripts/restore-deployment.sh
scripts/restore-newapi.sh
scripts/verify-deployment.sh
docs/architecture/          Current architecture documents
docs/runbooks/              Current operational procedures
tests/                      Deployment and operation contract tests
```

Local runtime state is ignored by Git:

```text
.env
data/
postgres_data/
redis_data/
letsencrypt/
tmp/
```

`data/config.yaml` contains generated Sub2API configuration and secrets. Treat
it as production state.

## Requirements

- Linux with Docker Engine and Docker Compose v2
- Python 3, `curl`, and GNU-compatible `tar`
- DNS for all public hosts and reachable TCP ports `80` and `443`

## Initial setup

```bash
cp .env.example .env
openssl rand -hex 32
${EDITOR:-vi} .env
mkdir -p data postgres_data redis_data letsencrypt
touch letsencrypt/acme.json
chmod 600 letsencrypt/acme.json
docker compose -f docker-compose.yml -f docker-compose.newapi.yml config >/dev/null
docker compose -f docker-compose.yml -f docker-compose.newapi.yml up -d
```

Set every required secret in `.env`. Pin `SUB2API_IMAGE_TAG` and
`NEW_API_IMAGE_TAG` to reviewed release tags or immutable digests for
production.

## Public routes

- `SUB2API_HOST`: OpenAI-compatible Sub2API route, normally `/v1`
- `SUB2API_ADMIN_HOST`: Sub2API administrator UI
- `NEW_API_HOST`: New API user, administrator, and SDK entry point

All three DNS names must point to the host before the first ACME request. Ports
`80` and `443` must be reachable. Database and Redis ports are not published.

New API should call Sub2API through `http://sub2api:8080/v1` on the Docker
network. The public Sub2API URL is also available when an externally routed
upstream is required.

## Common operations

Use both Compose files for every operation that includes New API:

```bash
docker compose -f docker-compose.yml -f docker-compose.newapi.yml config
docker compose -f docker-compose.yml -f docker-compose.newapi.yml pull
docker compose -f docker-compose.yml -f docker-compose.newapi.yml up -d
docker compose -f docker-compose.yml -f docker-compose.newapi.yml ps
```

Verify public routes and container health:

```bash
scripts/verify-deployment.sh
```

Create a consistent deployment backup outside the repository:

```bash
BACKUP_DIR=/var/backups/sub2api scripts/backup-deployment.sh
```

Restore the complete deployment from a backup produced by that command:

```bash
scripts/restore-deployment.sh /path/to/backup-package.tgz
```

Full restore is destructive: it replaces Sub2API runtime state, both Postgres
databases, and both Redis data domains. It validates archive paths, checksums,
and required sources before stopping the current deployment.

Restore only New API data from a compatible deployment or historical API-site
backup without modifying Sub2API state:

```bash
scripts/restore-newapi.sh /path/to/backup-package.tgz
```

The New API restore accepts the historical `newapi-postgres.dump` plus
`redis-data/` package contract. Historical runtime metadata may seed missing
New API environment values, but it never replaces Sub2API runtime data.

## Data movement warning

The base stack uses root-relative bind mounts for `data/`, `postgres_data/`,
`redis_data/`, and `letsencrypt/`. Moving the Compose files without moving these
directories points containers at empty paths.

Before changing the server checkout layout:

1. Record current container mounts and named volumes.
2. Create and verify a backup package.
3. Stop the current deployment.
4. Move the bind-mounted directories with the Compose files.
5. Confirm the Compose project remains `sub2api`.
6. Render the configuration and inspect every mount before starting.

Do not run the old and promoted Compose layouts simultaneously because they use
the same explicit container and network names.

## Security notes

- The Traefik Docker socket mount is effectively inside the host Docker trust
  boundary even though it is read-only.
- Keep `.env`, `data/config.yaml`, database state, backup packages, and ACME
  material outside Git.
- Operational scripts parse `.env` as strict one-line dotenv data and never
  execute it as shell. Variable interpolation is rejected; single-quote a
  literal `$` in a value.
- Keep the upstream allowlist enabled. Add required domains instead of broadly
  allowing private or arbitrary hosts.
- Pin images and review changes before production upgrades.

## Lark integration

The planned Lark login, wallet grant, and managed subscription integration is
specified in
[`docs/architecture/lark-entitlement-integration.md`](docs/architecture/lark-entitlement-integration.md).
Its controller and policy bundle will extend this root deployment rather than
introducing a second deployment directory.
