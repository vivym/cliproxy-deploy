#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

usage() {
  echo "Usage: scripts/restore-newapi-only.sh BACKUP_PACKAGE [SUB2API_DIR]" >&2
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

if [[ $# -lt 1 || $# -gt 2 ]]; then
  usage
  exit 1
fi

backup_package="$1"
sub2api_dir="${2:-${repo_root}/sub2api}"

if [[ ! -f "$backup_package" ]]; then
  echo "Backup package does not exist: $backup_package" >&2
  exit 1
fi
if [[ ! -d "$sub2api_dir" ]]; then
  echo "Sub2API directory does not exist: $sub2api_dir" >&2
  exit 1
fi
for required_path in .env docker-compose.yml docker-compose.newapi.yml; do
  if [[ ! -f "${sub2api_dir}/${required_path}" ]]; then
    echo "Missing required Sub2API file: ${sub2api_dir}/${required_path}" >&2
    exit 1
  fi
done

backup_package="$(cd "$(dirname "$backup_package")" && pwd -P)/$(basename "$backup_package")"
sub2api_dir="$(cd "$sub2api_dir" && pwd -P)"
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

for required_path in newapi-postgres.dump redis-data; do
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

set_env_if_missing_or_blank() {
  local key="$1"
  local value="$2"
  local env_file="${sub2api_dir}/.env"
  local escaped_value
  local tmp_env

  if [[ -z "$value" ]]; then
    return
  fi

  escaped_value="$(quote_env_value "$value")"

  if grep -Eq "^${key}=" "$env_file"; then
    if grep -Eq "^${key}=$" "$env_file"; then
      tmp_env="$(mktemp)"
      while IFS= read -r line || [[ -n "$line" ]]; do
        if [[ "$line" == "${key}=" ]]; then
          printf '%s=%s\n' "$key" "$escaped_value"
        else
          printf '%s\n' "$line"
        fi
      done < "$env_file" > "$tmp_env"
      mv "$tmp_env" "$env_file"
    fi
  else
    printf '%s=%s\n' "$key" "$escaped_value" >> "$env_file"
  fi
}

quote_env_value() {
  local value="$1"

  if [[ "$value" =~ ^[A-Za-z0-9_./:@%+=,-]+$ ]]; then
    printf '%s' "$value"
    return
  fi

  printf "'"
  while [[ "$value" == *"'"* ]]; do
    printf "%s'\\\\''" "${value%%\'*}"
    value="${value#*\'}"
  done
  printf "%s'" "$value"
}

seed_newapi_env_from_backup() {
  if [[ ! -f "${backup_dir}/cliproxy-runtime.tgz" ]]; then
    return
  fi

  tar -xzf "${backup_dir}/cliproxy-runtime.tgz" -C "$runtime_dir"
  if [[ ! -f "${runtime_dir}/.env" ]]; then
    return
  fi

  set -a
  # shellcheck source=/dev/null
  source "${runtime_dir}/.env"
  set +a

  set_env_if_missing_or_blank NEW_API_HOST "${AI_HOST:-}"
  set_env_if_missing_or_blank NEW_API_IMAGE_TAG "${NEW_API_IMAGE_TAG:-}"
  set_env_if_missing_or_blank NEWAPI_POSTGRES_USER "${POSTGRES_USER:-}"
  set_env_if_missing_or_blank NEWAPI_POSTGRES_DB "${POSTGRES_DB:-}"
  set_env_if_missing_or_blank NEWAPI_POSTGRES_PASSWORD "${POSTGRES_PASSWORD:-}"
  set_env_if_missing_or_blank NEWAPI_REDIS_PASSWORD "${REDIS_PASSWORD:-}"
  set_env_if_missing_or_blank NEW_API_SESSION_SECRET "${NEW_API_SESSION_SECRET:-}"
  set_env_if_missing_or_blank NEW_API_CRYPTO_SECRET "${NEW_API_CRYPTO_SECRET:-}"
}

load_newapi_env() {
  set -a
  # shellcheck source=/dev/null
  source "${sub2api_dir}/.env"
  set +a

  require_env NEW_API_HOST "${sub2api_dir}/.env"
  require_env NEW_API_IMAGE_TAG "${sub2api_dir}/.env"
  require_env NEWAPI_POSTGRES_USER "${sub2api_dir}/.env"
  require_env NEWAPI_POSTGRES_DB "${sub2api_dir}/.env"
  require_env NEWAPI_POSTGRES_PASSWORD "${sub2api_dir}/.env"
  require_env NEWAPI_REDIS_PASSWORD "${sub2api_dir}/.env"
  require_env NEW_API_SESSION_SECRET "${sub2api_dir}/.env"
  require_env NEW_API_CRYPTO_SECRET "${sub2api_dir}/.env"
}

compose() {
  (
    cd "$sub2api_dir"
    docker compose -f docker-compose.yml -f docker-compose.newapi.yml "$@"
  )
}

service_volume_name() {
  local service="$1"
  local destination="$2"
  local container_id

  container_id="$(compose ps -aq "$service")"
  if [[ -z "$container_id" ]]; then
    compose create "$service" >/dev/null
    container_id="$(compose ps -aq "$service")"
  fi

  docker inspect -f '{{range .Mounts}}{{if eq .Destination "'"$destination"'"}}{{.Name}}{{end}}{{end}}' "$container_id"
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
  for ((attempt = 1; attempt <= 30; attempt++)); do
    if compose exec -T newapi-postgres pg_isready \
      -U "${NEWAPI_POSTGRES_USER}" \
      -d "${NEWAPI_POSTGRES_DB}" >/dev/null 2>&1; then
      return
    fi
    sleep 2
  done

  echo "New API Postgres did not become ready" >&2
  exit 1
}

verify_checksums
seed_newapi_env_from_backup
load_newapi_env

compose stop new-api >/dev/null 2>&1 || true
compose stop newapi-postgres >/dev/null 2>&1 || true
compose stop newapi-redis >/dev/null 2>&1 || true

compose create newapi-postgres >/dev/null
clear_volume_dir newapi-postgres /var/lib/postgresql/data

compose create newapi-redis >/dev/null
restore_volume_dir newapi-redis /data "${backup_dir}/redis-data"

compose up -d newapi-postgres
wait_for_postgres
compose exec -T newapi-postgres pg_restore \
  -U "${NEWAPI_POSTGRES_USER}" \
  -d "${NEWAPI_POSTGRES_DB}" \
  --clean \
  --if-exists \
  --no-owner \
  < "${backup_dir}/newapi-postgres.dump"

compose up -d newapi-redis
compose up -d new-api

echo "New API-only restore completed from ${backup_package}"
