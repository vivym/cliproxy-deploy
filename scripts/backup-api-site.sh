#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [[ -f .env ]]; then
  set -a
  source .env
  set +a
fi

backup_root="${BACKUP_DIR:-/var/backups/cliproxy-api-site}"
if [[ "$backup_root" != /* ]]; then
  echo "BACKUP_DIR must be an absolute path outside the repository: $backup_root" >&2
  exit 1
fi
backup_root="$(realpath -m "$backup_root")"
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
dest="${backup_root}/${timestamp}"

case "$backup_root" in
  "$repo_root"|"$repo_root"/*)
    echo "Refusing to write backups inside repository: $backup_root" >&2
    exit 1
    ;;
esac

mkdir -p "$dest"
chmod 700 "$dest"

for required_path in config.yaml auths; do
  if [[ ! -e "$required_path" ]]; then
    echo "Missing required backup source: $required_path" >&2
    exit 1
  fi
done

docker compose exec -T postgres pg_dump \
  -U "${POSTGRES_USER:?set POSTGRES_USER}" \
  -d "${POSTGRES_DB:?set POSTGRES_DB}" \
  --format=custom \
  > "${dest}/newapi-postgres.dump"

tar --warning=no-file-changed -czf "${dest}/cliproxy-runtime.tgz" \
  config.yaml \
  auths

if docker compose ps --services --filter status=running | grep -qx "cpa-usage-keeper"; then
  docker compose cp cpa-usage-keeper:/data "${dest}/cpa-usage-keeper-data"
fi

find "$dest" -type f ! -name SHA256SUMS -print0 \
  | sort -z \
  | xargs -0 sha256sum \
  > "${dest}/SHA256SUMS"

echo "Backup written to ${dest}"
