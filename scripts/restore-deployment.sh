#!/usr/bin/env bash
set -euo pipefail

script_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
dotenv_reader="${script_repo_root}/scripts/read-dotenv.py"

usage() {
  echo "Usage: scripts/restore-deployment.sh BACKUP_PACKAGE [DEPLOYMENT_DIR]" >&2
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
deployment_dir="${2:-${script_repo_root}}"

if [[ ! -f "$backup_package" ]]; then
  echo "Backup package does not exist: $backup_package" >&2
  exit 1
fi
if [[ ! -d "$deployment_dir" ]]; then
  echo "Deployment directory does not exist: $deployment_dir" >&2
  exit 1
fi

backup_package="$(cd "$(dirname "$backup_package")" && pwd -P)/$(basename "$backup_package")"
deployment_dir="$(cd "$deployment_dir" && pwd -P)"
if [[ "$deployment_dir" == "/" ]]; then
  echo "Refusing to restore with the filesystem root as DEPLOYMENT_DIR" >&2
  exit 1
fi
for required_file in .env docker-compose.yml docker-compose.newapi.yml; do
  if [[ ! -f "${deployment_dir}/${required_file}" ]]; then
    echo "Missing required deployment file: ${deployment_dir}/${required_file}" >&2
    exit 1
  fi
done

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

for required_path in \
  deployment-runtime.tgz \
  sub2api-postgres.dump \
  newapi-postgres.dump \
  sub2api-redis-data \
  redis-data; do
  if [[ ! -e "${backup_dir}/${required_path}" ]]; then
    echo "Missing required restore source: ${backup_dir}/${required_path}" >&2
    exit 1
  fi
done

verify_checksums() {
  if [[ ! -f "${backup_dir}/SHA256SUMS" ]]; then
    echo "Missing required restore source: ${backup_dir}/SHA256SUMS" >&2
    exit 1
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

verify_checksums
validate_archive "${backup_dir}/deployment-runtime.tgz" "deployment runtime"
tar -xzf "${backup_dir}/deployment-runtime.tgz" -C "$runtime_dir"
for required_path in .env data letsencrypt; do
  if [[ ! -e "${runtime_dir}/${required_path}" ]]; then
    echo "Missing required runtime restore source: ${runtime_dir}/${required_path}" >&2
    exit 1
  fi
done

dotenv_value() {
  local env_file="$1"
  local key="$2"

  python3 "$dotenv_reader" "$env_file" "$key"
}

compose_with_env_file() {
  local env_file="$1"
  shift
  local -a clean_env=(env -i "PATH=$PATH" "HOME=${HOME:-/nonexistent}")
  local -a compose_command=(docker compose)
  local docker_variable

  for docker_variable in DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG; do
    if [[ -n "${!docker_variable:-}" ]]; then
      clean_env+=("${docker_variable}=${!docker_variable}")
    fi
  done
  if [[ -n "$env_file" ]]; then
    compose_command+=(--env-file "$env_file")
  fi
  compose_command+=(-f docker-compose.yml -f docker-compose.newapi.yml)
  (
    cd "$deployment_dir"
    "${clean_env[@]}" "${compose_command[@]}" "$@"
  )
}

runtime_env="${runtime_dir}/.env"
python3 "$dotenv_reader" --validate "$runtime_env"
compose_with_env_file "$runtime_env" config --quiet

POSTGRES_USER="$(dotenv_value "$runtime_env" POSTGRES_USER)"
POSTGRES_DB="$(dotenv_value "$runtime_env" POSTGRES_DB)"
NEWAPI_POSTGRES_USER="$(dotenv_value "$runtime_env" NEWAPI_POSTGRES_USER)"
NEWAPI_POSTGRES_DB="$(dotenv_value "$runtime_env" NEWAPI_POSTGRES_DB)"

require_env() {
  local name="$1"

  if [[ -z "${!name:-}" ]]; then
    echo "set ${name} in ${runtime_env}" >&2
    exit 1
  fi
}

for required_env in \
  POSTGRES_USER POSTGRES_DB \
  NEWAPI_POSTGRES_USER NEWAPI_POSTGRES_DB; do
  require_env "$required_env"
done

compose() {
  compose_with_env_file "" "$@"
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

replace_service_volume() {
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
    alpine:3.22 \
    sh -c 'rm -rf /target/* /target/.[!.]* /target/..?* 2>/dev/null || true; cp -a /backup/. /target/'
}

clear_service_volume() {
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
    alpine:3.22 \
    sh -c 'rm -rf /target/* /target/.[!.]* /target/..?* 2>/dev/null || true'
}

wait_for_postgres() {
  local service="$1"
  local user="$2"
  local database="$3"
  local attempt

  for ((attempt = 1; attempt <= 30; attempt++)); do
    if compose exec -T "$service" pg_isready -U "$user" -d "$database" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done

  echo "Postgres service did not become ready: $service" >&2
  exit 1
}

compose down

rm -f "${deployment_dir}/.env"
rm -rf "${deployment_dir}/data" "${deployment_dir}/letsencrypt"
cp -a "${runtime_dir}/.env" "${deployment_dir}/.env"
cp -a "${runtime_dir}/data" "${deployment_dir}/data"
cp -a "${runtime_dir}/letsencrypt" "${deployment_dir}/letsencrypt"
chmod 600 "${deployment_dir}/letsencrypt/acme.json" 2>/dev/null || true

rm -rf "${deployment_dir}/postgres_data" "${deployment_dir}/redis_data"
mkdir -p "${deployment_dir}/postgres_data" "${deployment_dir}/redis_data"
cp -a "${backup_dir}/sub2api-redis-data/." "${deployment_dir}/redis_data/"

compose create newapi-postgres newapi-redis >/dev/null
clear_service_volume newapi-postgres /var/lib/postgresql/data
replace_service_volume newapi-redis /data "${backup_dir}/redis-data"

compose up -d postgres newapi-postgres
wait_for_postgres postgres "$POSTGRES_USER" "$POSTGRES_DB"
wait_for_postgres newapi-postgres "$NEWAPI_POSTGRES_USER" "$NEWAPI_POSTGRES_DB"

compose exec -T postgres pg_restore \
  -U "$POSTGRES_USER" \
  -d "$POSTGRES_DB" \
  --clean \
  --if-exists \
  --no-owner \
  < "${backup_dir}/sub2api-postgres.dump"

compose exec -T newapi-postgres pg_restore \
  -U "$NEWAPI_POSTGRES_USER" \
  -d "$NEWAPI_POSTGRES_DB" \
  --clean \
  --if-exists \
  --no-owner \
  < "${backup_dir}/newapi-postgres.dump"

compose up -d

echo "Restore completed from ${backup_package}"
