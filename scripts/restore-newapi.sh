#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
dotenv_reader="${repo_root}/scripts/read-dotenv.py"

usage() {
  echo "Usage: scripts/restore-newapi.sh BACKUP_PACKAGE [DEPLOYMENT_DIR]" >&2
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
deployment_dir="${2:-${repo_root}}"

if [[ ! -f "$backup_package" ]]; then
  echo "Backup package does not exist: $backup_package" >&2
  exit 1
fi
if [[ ! -d "$deployment_dir" ]]; then
  echo "Deployment directory does not exist: $deployment_dir" >&2
  exit 1
fi
for required_path in .env docker-compose.yml docker-compose.newapi.yml; do
  if [[ ! -f "${deployment_dir}/${required_path}" ]]; then
    echo "Missing required Deployment file: ${deployment_dir}/${required_path}" >&2
    exit 1
  fi
done

backup_package="$(cd "$(dirname "$backup_package")" && pwd -P)/$(basename "$backup_package")"
deployment_dir="$(cd "$deployment_dir" && pwd -P)"
if [[ "$deployment_dir" == "/" ]]; then
  echo "Refusing to restore with the filesystem root as DEPLOYMENT_DIR" >&2
  exit 1
fi
restore_tmp="$(mktemp -d)"
backup_dir="${restore_tmp}/backup"
runtime_dir="${restore_tmp}/runtime"
cleanup_restore_tmp() {
  local status=$?
  trap - EXIT
  rm -rf "$restore_tmp"
  exit "$status"
}
trap cleanup_restore_tmp EXIT
mkdir -p "$backup_dir" "$runtime_dir"

validate_archive() {
  local archive="$1"
  local label="$2"
  local member
  local normalized
  local listing

  listing="$(tar -tzf "$archive")"
  while IFS= read -r member; do
    normalized="${member#./}"
    case "$normalized" in
      ""|".") ;;
      /*|../*|*/../*|*/..)
        echo "Unsafe path in ${label}: ${member}" >&2
        exit 1
        ;;
    esac
  done <<< "$listing"

  while IFS= read -r member; do
    case "${member:0:1}" in
      -|d) ;;
      *)
        echo "Unsupported archive entry type in ${label}: ${member}" >&2
        exit 1
        ;;
    esac
  done < <(tar -tvzf "$archive")
}

validate_archive "$backup_package" "backup package"
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

dotenv_value() {
  local env_file="$1"
  local key="$2"
  local optional="${3:-false}"

  if [[ "$optional" == "true" ]]; then
    python3 "$dotenv_reader" --allow-missing "$env_file" "$key"
  else
    python3 "$dotenv_reader" "$env_file" "$key"
  fi
}

set_env_if_missing_or_blank() {
  local key="$1"
  local value="$2"
  local env_file="${deployment_dir}/.env"
  local escaped_value
  local existing_value
  local tmp_env

  if [[ -z "$value" ]]; then
    return
  fi

  escaped_value="$(quote_env_value "$value")"
  existing_value="$(dotenv_value "$env_file" "$key" true)"

  if grep -Eq "^[[:space:]]*${key}[[:space:]]*=" "$env_file"; then
    if [[ -z "$existing_value" ]]; then
      tmp_env="$(mktemp)"
      while IFS= read -r line || [[ -n "$line" ]]; do
        if [[ "$line" =~ ^[[:space:]]*${key}[[:space:]]*= ]]; then
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
  local char
  local index

  if [[ "$value" =~ ^[A-Za-z0-9_./:@%+=,-]+$ ]]; then
    printf '%s' "$value"
    return
  fi

  printf "'"
  for ((index = 0; index < ${#value}; index++)); do
    char="${value:index:1}"
    case "$char" in
      "'"|"\\") printf '\\%s' "$char" ;;
      *) printf '%s' "$char" ;;
    esac
  done
  printf "'"
}

seed_newapi_env_from_backup() {
  local runtime_archive
  local runtime_format

  if [[ -f "${backup_dir}/deployment-runtime.tgz" ]]; then
    runtime_archive="${backup_dir}/deployment-runtime.tgz"
    runtime_format="current"
  elif [[ -f "${backup_dir}/cliproxy-runtime.tgz" ]]; then
    runtime_archive="${backup_dir}/cliproxy-runtime.tgz"
    runtime_format="legacy"
  else
    return 0
  fi

  validate_archive "$runtime_archive" "deployment runtime"
  tar -xzf "$runtime_archive" -C "$runtime_dir"
  if [[ ! -f "${runtime_dir}/.env" ]]; then
    return 0
  fi

  local runtime_env="${runtime_dir}/.env"
  python3 "$dotenv_reader" --validate "$runtime_env"

  if [[ "$runtime_format" == "current" ]]; then
    set_env_if_missing_or_blank NEW_API_HOST "$(dotenv_value "$runtime_env" NEW_API_HOST true)"
    set_env_if_missing_or_blank NEW_API_IMAGE_TAG "$(dotenv_value "$runtime_env" NEW_API_IMAGE_TAG true)"
    set_env_if_missing_or_blank NEWAPI_POSTGRES_USER "$(dotenv_value "$runtime_env" NEWAPI_POSTGRES_USER true)"
    set_env_if_missing_or_blank NEWAPI_POSTGRES_DB "$(dotenv_value "$runtime_env" NEWAPI_POSTGRES_DB true)"
    set_env_if_missing_or_blank NEWAPI_POSTGRES_PASSWORD "$(dotenv_value "$runtime_env" NEWAPI_POSTGRES_PASSWORD true)"
    set_env_if_missing_or_blank NEWAPI_REDIS_PASSWORD "$(dotenv_value "$runtime_env" NEWAPI_REDIS_PASSWORD true)"
    set_env_if_missing_or_blank NEW_API_SESSION_SECRET "$(dotenv_value "$runtime_env" NEW_API_SESSION_SECRET true)"
    set_env_if_missing_or_blank NEW_API_CRYPTO_SECRET "$(dotenv_value "$runtime_env" NEW_API_CRYPTO_SECRET true)"
    return
  fi

  set_env_if_missing_or_blank NEW_API_HOST "$(dotenv_value "$runtime_env" AI_HOST true)"
  set_env_if_missing_or_blank NEW_API_IMAGE_TAG "$(dotenv_value "$runtime_env" NEW_API_IMAGE_TAG true)"
  set_env_if_missing_or_blank NEWAPI_POSTGRES_USER "$(dotenv_value "$runtime_env" POSTGRES_USER true)"
  set_env_if_missing_or_blank NEWAPI_POSTGRES_DB "$(dotenv_value "$runtime_env" POSTGRES_DB true)"
  set_env_if_missing_or_blank NEWAPI_POSTGRES_PASSWORD "$(dotenv_value "$runtime_env" POSTGRES_PASSWORD true)"
  set_env_if_missing_or_blank NEWAPI_REDIS_PASSWORD "$(dotenv_value "$runtime_env" REDIS_PASSWORD true)"
  set_env_if_missing_or_blank NEW_API_SESSION_SECRET "$(dotenv_value "$runtime_env" NEW_API_SESSION_SECRET true)"
  set_env_if_missing_or_blank NEW_API_CRYPTO_SECRET "$(dotenv_value "$runtime_env" NEW_API_CRYPTO_SECRET true)"
}

load_newapi_env() {
  local env_file="${deployment_dir}/.env"
  local required_name
  local required_value

  python3 "$dotenv_reader" --validate "$env_file"
  for required_name in \
    NEW_API_HOST NEW_API_IMAGE_TAG \
    NEWAPI_POSTGRES_USER NEWAPI_POSTGRES_DB \
    NEWAPI_POSTGRES_PASSWORD NEWAPI_REDIS_PASSWORD \
    NEW_API_SESSION_SECRET NEW_API_CRYPTO_SECRET; do
    required_value="$(dotenv_value "$env_file" "$required_name" true)"
    if [[ -z "$required_value" ]]; then
      echo "set ${required_name} in ${env_file}" >&2
      exit 1
    fi
  done

  NEWAPI_POSTGRES_USER="$(dotenv_value "$env_file" NEWAPI_POSTGRES_USER)"
  NEWAPI_POSTGRES_DB="$(dotenv_value "$env_file" NEWAPI_POSTGRES_DB)"
}

compose() {
  local -a clean_env=(env -i "PATH=$PATH" "HOME=${HOME:-/nonexistent}")
  local docker_variable

  for docker_variable in DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG; do
    if [[ -n "${!docker_variable:-}" ]]; then
      clean_env+=("${docker_variable}=${!docker_variable}")
    fi
  done
  (
    cd "$deployment_dir"
    "${clean_env[@]}" docker compose \
      -f docker-compose.yml \
      -f docker-compose.newapi.yml \
      "$@"
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
