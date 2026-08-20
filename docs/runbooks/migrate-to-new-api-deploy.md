# Migrate the legacy gateway to the New API deployment

This runbook migrates a legacy checkout such as
`/opt/cliproxy-deploy/sub2api` into `/opt/new-api-deploy`. The target has one
complete `docker-compose.yml`, uses `name: new-api`, exposes New API as the
only user and SDK entry point, and keeps Sub2API as an internal upstream.

This is a data migration, not a directory promotion. The Compose project and named-volume identities change, so the old volumes cannot be assumed to back
the new stack. Use a verified full backup and restore every state domain.

Do not combine this migration with the Lark rollout or an application image
upgrade.

## Invariants

- The source stack stays available for rollback until the target is accepted.
- The target migration script's explicit legacy adapter produces one verified
  source package.
- The target is a separate checkout at `/opt/new-api-deploy`.
- Source and target are never running simultaneously.
- The target renders as `name: new-api` before any restore mutation.
- No old Docker volume is attached to the target by name alone.

## 1. Record the legacy deployment

Run from the legacy deployment directory and save the output outside both
repositories:

```bash
cd /opt/cliproxy-deploy/sub2api
docker ps --filter name=sub2api --filter name=new-api --filter name=newapi
docker inspect sub2api sub2api-postgres sub2api-redis new-api newapi-postgres newapi-redis
docker volume ls --filter label=com.docker.compose.project=sub2api
docker network inspect sub2api-proxy sub2api-backend newapi-backend
git rev-parse HEAD
```

Record every bind mount and named volume from `docker inspect`. Stop if the
running topology differs from the rendered source configuration. Names alone
are not evidence that two volumes contain the same data.

## 2. Create and verify the source backup

Use the reviewed target migration adapter. It reads the source's historical
Compose interface and service names; the normal backup command has no legacy
mode:

```bash
BACKUP_DIR=/var/backups/new-api \
  /opt/new-api-deploy/scripts/migrations/backup-legacy-deployment.sh \
  /opt/cliproxy-deploy/sub2api
```

Extract the resulting package in a temporary directory and verify
`SHA256SUMS`. Confirm it contains:

```text
deployment-runtime.tgz
sub2api-postgres.dump
new-api-postgres.dump
sub2api-redis-data/
new-api-redis-data/
SHA256SUMS
```

Keep the package and its checksum receipt outside both checkouts.

## 3. Prepare the target without starting it

Check out the reviewed migration revision at `/opt/new-api-deploy`. Do not copy
the legacy runtime directories into it and do not run `docker compose up`.

Create a temporary target environment from `.env.example` only to validate the
target structure. Use reviewed non-production placeholders, then remove it:

```bash
cd /opt/new-api-deploy
cp .env.example .env.preflight
${EDITOR:-vi} .env.preflight
docker compose --env-file .env.preflight config --quiet
docker compose --env-file .env.preflight config --services
docker compose --env-file .env.preflight config --volumes
```

The rendered receipt must show `name: new-api`, all seven expected services,
`new-api-postgres-data`, and `new-api-redis-data`. It must not contain a public
Sub2API API router. Compare `EDGE_SUBNET`, `SUB2API_DATA_SUBNET`, and
`NEW_API_DATA_SUBNET` against every Docker network from the source inventory.
The target defaults use `172.31.20.0/24`, `172.31.21.0/24`, and
`172.31.22.0/24`; stop and select unused CIDRs if any pool overlaps.

## 4. Stop the source stack

Start the maintenance window and stop the source without deleting volumes:

```bash
cd /opt/cliproxy-deploy/sub2api
docker stop sub2api-traefik new-api sub2api newapi-redis sub2api-redis newapi-postgres sub2api-postgres
```

Confirm all seven legacy containers are stopped. Keep the containers, source
checkout, networks, and volumes unchanged for rollback.

## 5. Restore into the target identities

Run the target restore script against the verified package:

```bash
cd /opt/new-api-deploy
EDGE_SUBNET=172.31.20.0/24 \
SUB2API_DATA_SUBNET=172.31.21.0/24 \
NEW_API_DATA_SUBNET=172.31.22.0/24 \
  scripts/restore-deployment.sh /var/backups/new-api/<verified-package>.tgz
```

The restore validates the package, restores runtime files, creates the target
named volumes through the New API Compose project, restores both Postgres
dumps and both Redis data domains, then starts the complete target stack. Use
the inventory-approved CIDRs from preflight; the restore writes them into the
normalized target `.env` and never inherits the legacy network pools.

## 6. Verify and accept

```bash
cd /opt/new-api-deploy
docker compose config
docker compose ps
docker inspect new-api-new-api-1 new-api-sub2api-1
docker volume ls --filter label=com.docker.compose.project=new-api
scripts/verify-deployment.sh
```

Also verify a representative user login, New API model listing, one low-cost
request through the internal Sub2API channel, and Sub2API administrator login.
Save these receipts with the migration record.

Do not delete the legacy checkout or volumes during the acceptance window.

## Rollback

If target validation fails before it accepts production traffic:

1. Stop the target with `docker compose down` and do not pass `--volumes`.
2. Confirm no client DNS or route still points at the target.
3. Return to `/opt/cliproxy-deploy/sub2api`.
4. Compare the legacy container mounts to the preflight receipt.
5. Start the seven recorded legacy containers, then repeat their health,
   login, and low-cost request checks.

If the target accepted writes, do not start the source with its earlier data.
Quiesce the target, create a new consistent target backup, and perform a
reviewed reverse restore or reconcile the writes explicitly before rollback.
