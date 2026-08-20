# Promote Sub2API runtime to the repository root

This runbook moves an existing server checkout from the historical `sub2api/`
layout to the root deployment layout. It does not change application data or
Compose project identity.

Do not combine this operation with the Lark rollout or an image upgrade.

## Invariants

- The Compose project remains `name: sub2api`.
- Existing New API named volumes are reused.
- `.env`, `data/`, `postgres_data/`, `redis_data/`, and `letsencrypt/` move
  together.
- A verified backup exists before the first production write or move.
- The previous Git commit and runtime layout remain available for rollback.

## 1. Record the current state

Run these commands from the currently deployed `sub2api/` directory and save
their output outside the repository:

```bash
docker compose -f docker-compose.yml -f docker-compose.newapi.yml ps
docker compose -f docker-compose.yml -f docker-compose.newapi.yml config
docker inspect sub2api sub2api-postgres sub2api-redis new-api newapi-postgres newapi-redis
docker volume ls --filter label=com.docker.compose.project=sub2api
git rev-parse HEAD
```

From the `docker inspect` receipt, record every mount source and destination.
Confirm the Sub2API bind mounts point into the current `sub2api/` directory and
record the exact New API volume names.

Stop if the running topology does not match the receipt. Do not infer volume
identity from names alone.

## 2. Create a pre-migration backup

Use a separate checkout of the new revision so the new script can inspect the
still-running historical deployment before its tracked files are replaced:

```bash
BACKUP_DIR=/var/backups/sub2api \
  /opt/cliproxy-deploy-next/scripts/backup-deployment.sh \
  /opt/cliproxy-deploy/sub2api
```

Extract the resulting package into a temporary directory and verify its
`SHA256SUMS`. Confirm it contains both Postgres dumps, both Redis snapshots,
`deployment-runtime.tgz`, and the checksum file.

## 3. Stop the historical layout

```bash
cd /opt/cliproxy-deploy/sub2api
docker compose -f docker-compose.yml -f docker-compose.newapi.yml down
```

Do not pass `--volumes` or `-v`.

## 4. Update tracked files and move runtime state

Update `/opt/cliproxy-deploy` to the reviewed migration commit. The ignored
runtime directories should remain under `sub2api/` after tracked files move to
the root.

From `/opt/cliproxy-deploy`:

```bash
set -euo pipefail
for path in .env data postgres_data redis_data letsencrypt; do
  if [[ -e "$path" || ! -e "sub2api/$path" ]]; then
    echo "Unsafe runtime move state: source=sub2api/$path destination=$path" >&2
    exit 1
  fi
done
for path in .env data postgres_data redis_data letsencrypt; do
  mv -n "sub2api/$path" "$path"
  if [[ -e "sub2api/$path" || ! -e "$path" ]]; then
    echo "Runtime move did not complete: $path" >&2
    exit 1
  fi
done
rmdir sub2api
chmod 600 letsencrypt/acme.json
```

Resolve the exact paths from the preflight receipt before running the loop. The
preflight rejects every missing source and existing destination before the
first move; `mv -n` and the postcondition prevent a concurrent destination from
being overwritten silently.

## 5. Validate identity before start

```bash
docker compose -f docker-compose.yml -f docker-compose.newapi.yml config
docker compose -f docker-compose.yml -f docker-compose.newapi.yml config --volumes
docker volume ls --filter label=com.docker.compose.project=sub2api
```

Confirm the rendered project is `sub2api`, the bind mount sources now point to
the repository root, and the New API volume names exactly match the preflight
receipt.

## 6. Start and verify

```bash
docker compose -f docker-compose.yml -f docker-compose.newapi.yml up -d
docker compose -f docker-compose.yml -f docker-compose.newapi.yml ps
scripts/verify-deployment.sh
```

Also verify representative user login, New API model listing, and one low-cost
request through the configured Sub2API channel before closing the maintenance
window.

## Rollback

If validation fails:

1. Stop the root deployment without removing volumes.
2. Return tracked files to the recorded pre-migration commit.
3. Move the five runtime paths back under `sub2api/`.
4. Render the historical Compose configuration and compare mounts with the
   preflight receipt.
5. Start the historical deployment and repeat its health checks.

If runtime state was modified after the new layout started, do not perform a
directory-only rollback. Run `scripts/restore-deployment.sh` with the verified
backup so Postgres, Redis, and runtime files return to one consistent snapshot.
