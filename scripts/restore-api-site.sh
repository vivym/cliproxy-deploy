#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo_root"

usage() {
  echo "Usage: scripts/restore-api-site.sh BACKUP_PACKAGE" >&2
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

if [[ $# -ne 1 ]]; then
  usage
  exit 1
fi

backup_package="$1"
if [[ ! -f "$backup_package" ]]; then
  echo "Backup package does not exist: $backup_package" >&2
  exit 1
fi

backup_package="$(cd "$(dirname "$backup_package")" && pwd -P)/$(basename "$backup_package")"
restore_tmp="$(mktemp -d)"
backup_dir="${restore_tmp}/backup"
trap 'rm -rf "$restore_tmp"' EXIT
mkdir -p "$backup_dir"
tar -xzf "$backup_package" -C "$backup_dir"

for required_path in cliproxy-runtime.tgz newapi-postgres.dump redis-data; do
  if [[ ! -e "${backup_dir}/${required_path}" ]]; then
    echo "Missing required restore source: ${backup_dir}/${required_path}" >&2
    exit 1
  fi
done

verify_checksums() {
  if [[ ! -f "${backup_dir}/SHA256SUMS" ]]; then
    return
  fi

  (
    cd "$backup_dir"
    if command -v sha256sum >/dev/null 2>&1; then
      sha256sum -c SHA256SUMS
    else
      shasum -a 256 -c SHA256SUMS
    fi
  )
}

service_volume_name() {
  local service="$1"
  local destination="$2"
  local container_id

  container_id="$(docker compose ps -aq "$service")"
  if [[ -z "$container_id" ]]; then
    docker compose create "$service" >/dev/null
    container_id="$(docker compose ps -aq "$service")"
  fi

  docker inspect -f '{{range .Mounts}}{{if eq .Destination "'"$destination"'"}}{{.Name}}{{end}}{{end}}' "$container_id"
}

restore_volume_dir() {
  local service="$1"
  local destination="$2"
  local source_dir="$3"
  local volume_name

  volume_name="$(service_volume_name "$service" "$destination")"
  if [[ -z "$volume_name" ]]; then
    echo "Could not find ${destination} volume for ${service}" >&2
    exit 1
  fi

  docker run --rm \
    -v "${volume_name}:/target" \
    -v "${source_dir}:/backup:ro" \
    alpine sh -c 'rm -rf /target/* /target/.[!.]* /target/..?* 2>/dev/null || true; cp -a /backup/. /target/'
}

wait_for_postgres() {
  local attempt
  for attempt in $(seq 1 30); do
    if docker compose exec -T postgres pg_isready \
      -U "${POSTGRES_USER:?set POSTGRES_USER}" \
      -d "${POSTGRES_DB:?set POSTGRES_DB}" >/dev/null 2>&1; then
      return
    fi
    sleep 2
  done

  echo "Postgres did not become ready" >&2
  exit 1
}

verify_checksums

tar -xzf "${backup_dir}/cliproxy-runtime.tgz" -C "$repo_root"
chmod 600 letsencrypt/acme.json 2>/dev/null || true

set -a
source .env
set +a

docker compose down

docker compose create redis >/dev/null
restore_volume_dir redis /data "${backup_dir}/redis-data"

if [[ -d "${backup_dir}/cpa-usage-keeper-data" ]]; then
  docker compose create cpa-usage-keeper >/dev/null
  restore_volume_dir cpa-usage-keeper /data "${backup_dir}/cpa-usage-keeper-data"
fi

docker compose up -d postgres
wait_for_postgres
docker compose exec -T postgres pg_restore \
  -U "${POSTGRES_USER:?set POSTGRES_USER}" \
  -d "${POSTGRES_DB:?set POSTGRES_DB}" \
  --clean \
  --if-exists \
  --no-owner \
  < "${backup_dir}/newapi-postgres.dump"

docker compose up -d

echo "Restore completed from ${backup_package}"
