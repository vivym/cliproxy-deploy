#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
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
backup_root="${backup_root%/}"
if [[ -z "$backup_root" ]]; then
  backup_root="/"
fi

# Portable realpath behavior for a backup root that may not exist yet.
if [[ -e "$backup_root" ]]; then
  backup_root="$(cd "$backup_root" && pwd -P)"
else
  backup_parent="$(dirname "$backup_root")"
  backup_name="$(basename "$backup_root")"
  mkdir -p "$backup_parent"
  backup_parent="$(cd "$backup_parent" && pwd -P)"
  backup_root="${backup_parent}/${backup_name}"
fi
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
dest="${backup_root}/${timestamp}"
partial_dest="${dest}.partial"
package="${dest}.tgz"
partial_package="${package}.partial"

case "$backup_root" in
  "$repo_root"|"$repo_root"/*)
    echo "Refusing to write backups inside repository: $backup_root" >&2
    exit 1
    ;;
esac

if [[ -e "$partial_dest" || -e "$dest" || -e "$partial_package" || -e "$package" ]]; then
  echo "Backup destination already exists: $package" >&2
  exit 1
fi

checksum_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi
}

for required_path in .env config.yaml auths letsencrypt; do
  if [[ ! -e "$required_path" ]]; then
    echo "Missing required backup source: $required_path" >&2
    exit 1
  fi
done

mkdir -p "$partial_dest"
chmod 700 "$partial_dest"

docker compose exec -T postgres pg_dump \
  -U "${POSTGRES_USER:?set POSTGRES_USER}" \
  -d "${POSTGRES_DB:?set POSTGRES_DB}" \
  --format=custom \
  > "${partial_dest}/newapi-postgres.dump"

docker compose exec -T redis redis-cli \
  -a "${REDIS_PASSWORD:?set REDIS_PASSWORD}" \
  SAVE >/dev/null
docker compose cp redis:/data "${partial_dest}/redis-data"

tar -czf "${partial_dest}/cliproxy-runtime.tgz" \
  .env \
  config.yaml \
  auths \
  letsencrypt

running_services="$(docker compose ps --services --filter status=running)"
if printf '%s\n' "$running_services" | grep -qx "cpa-usage-keeper"; then
  docker compose cp cpa-usage-keeper:/data "${partial_dest}/cpa-usage-keeper-data"
fi

(
  cd "$partial_dest"
  find . -type f ! -name SHA256SUMS -print0 \
    | sort -z \
    | while IFS= read -r -d '' backup_file; do
        checksum_file "$backup_file"
      done \
    > SHA256SUMS
)

(
  cd "$partial_dest"
  tar -czf "$partial_package" .
)
chmod 600 "$partial_package"
mv "$partial_package" "$package"
rm -rf "$partial_dest"

echo "Backup package written to ${package}"
