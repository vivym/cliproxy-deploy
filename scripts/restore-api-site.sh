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
runtime_dir="${restore_tmp}/runtime"
cleanup_restore_tmp() {
  local status=$?
  rm -rf "$restore_tmp"
  exit "$status"
}
trap cleanup_restore_tmp EXIT
mkdir -p "$backup_dir"
mkdir -p "$runtime_dir"
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

require_env() {
  local name="$1"
  local source_file="$2"

  if [[ -z "${!name:-}" ]]; then
    echo "set ${name} in ${source_file}" >&2
    exit 1
  fi
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

clear_volume_dir() {
  local service="$1"
  local destination="$2"
  local volume_name

  volume_name="$(service_volume_name "$service" "$destination")"
  if [[ -z "$volume_name" ]]; then
    echo "Could not find ${destination} volume for ${service}" >&2
    exit 1
  fi

  docker run --rm \
    -v "${volume_name}:/target" \
    alpine sh -c 'rm -rf /target/* /target/.[!.]* /target/..?* 2>/dev/null || true'
}

extract_runtime_files() {
  local required_path

  tar -xzf "${backup_dir}/cliproxy-runtime.tgz" -C "$runtime_dir"
  for required_path in .env config.yaml auths letsencrypt; do
    if [[ ! -e "${runtime_dir}/${required_path}" ]]; then
      echo "Missing required runtime restore source: ${runtime_dir}/${required_path}" >&2
      exit 1
    fi
  done
}

restore_runtime_files() {
  rm -f "$repo_root/.env" "$repo_root/config.yaml"
  rm -rf "$repo_root/auths" "$repo_root/letsencrypt"
  cp -a "${runtime_dir}/.env" "$repo_root/.env"
  cp -a "${runtime_dir}/config.yaml" "$repo_root/config.yaml"
  cp -a "${runtime_dir}/auths" "$repo_root/auths"
  cp -a "${runtime_dir}/letsencrypt" "$repo_root/letsencrypt"
}

wait_for_postgres() {
  local attempt
  for ((attempt = 1; attempt <= 30; attempt++)); do
    if docker compose exec -T postgres pg_isready \
      -U "${POSTGRES_USER}" \
      -d "${POSTGRES_DB}" >/dev/null 2>&1; then
      return
    fi
    sleep 2
  done

  echo "Postgres did not become ready" >&2
  exit 1
}

verify_checksums

extract_runtime_files
docker compose --env-file "${runtime_dir}/.env" down
restore_runtime_files
chmod 600 letsencrypt/acme.json 2>/dev/null || true

set -a
# shellcheck source=/dev/null
source .env
set +a

require_env POSTGRES_USER "${repo_root}/.env"
require_env POSTGRES_DB "${repo_root}/.env"

docker compose create postgres >/dev/null
clear_volume_dir postgres /var/lib/postgresql/data

docker compose create redis >/dev/null
restore_volume_dir redis /data "${backup_dir}/redis-data"

if [[ -d "${backup_dir}/cpa-usage-keeper-data" ]]; then
  docker compose create cpa-usage-keeper >/dev/null
  restore_volume_dir cpa-usage-keeper /data "${backup_dir}/cpa-usage-keeper-data"
fi

docker compose up -d postgres
wait_for_postgres
docker compose exec -T postgres pg_restore \
  -U "${POSTGRES_USER}" \
  -d "${POSTGRES_DB}" \
  --clean \
  --if-exists \
  --no-owner \
  < "${backup_dir}/newapi-postgres.dump"

docker compose up -d

echo "Restore completed from ${backup_package}"
