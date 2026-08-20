#!/usr/bin/env bash
set -euo pipefail

script_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
dotenv_reader="${script_repo_root}/scripts/read-dotenv.py"

usage() {
  echo "Usage: scripts/backup-deployment.sh [DEPLOYMENT_DIR]" >&2
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi
if [[ $# -gt 1 ]]; then
  usage
  exit 1
fi

deployment_dir="${1:-${script_repo_root}}"
if [[ ! -d "$deployment_dir" ]]; then
  echo "Deployment directory does not exist: $deployment_dir" >&2
  exit 1
fi
deployment_dir="$(cd "$deployment_dir" && pwd -P)"
for required_file in docker-compose.yml docker-compose.newapi.yml; do
  if [[ ! -f "${deployment_dir}/${required_file}" ]]; then
    echo "Missing required deployment file: ${deployment_dir}/${required_file}" >&2
    exit 1
  fi
done
cd "$deployment_dir"

if [[ ! -f .env ]]; then
  echo "Missing required backup source: .env" >&2
  exit 1
fi
python3 "$dotenv_reader" --validate .env

dotenv_value() {
  local key="$1"
  local optional="${2:-false}"

  if [[ "$optional" == "true" ]]; then
    python3 "$dotenv_reader" --allow-missing .env "$key"
  else
    python3 "$dotenv_reader" .env "$key"
  fi
}

env_backup_root="$(dotenv_value BACKUP_DIR true)"
backup_root="${BACKUP_DIR:-${env_backup_root:-/var/backups/sub2api}}"
if [[ "$backup_root" != /* ]]; then
  echo "BACKUP_DIR must be an absolute path outside the repository: $backup_root" >&2
  exit 1
fi
backup_root="${backup_root%/}"
if [[ -z "$backup_root" ]]; then
  backup_root="/"
fi

if [[ -e "$backup_root" ]]; then
  backup_root="$(cd "$backup_root" && pwd -P)"
else
  backup_parent="$(dirname "$backup_root")"
  backup_name="$(basename "$backup_root")"
  mkdir -p "$backup_parent"
  backup_parent="$(cd "$backup_parent" && pwd -P)"
  backup_root="${backup_parent}/${backup_name}"
fi

case "$backup_root" in
  "$script_repo_root"|"$script_repo_root"/*|"$deployment_dir"|"$deployment_dir"/*)
    echo "Refusing to write backups inside repository: $backup_root" >&2
    exit 1
    ;;
esac

for required_path in .env data postgres_data redis_data letsencrypt; do
  if [[ ! -e "$required_path" ]]; then
    echo "Missing required backup source: $required_path" >&2
    exit 1
  fi
done

mkdir -p "$backup_root"
lock_dir="${backup_root}/.backup.lock"
if ! mkdir "$lock_dir"; then
  echo "Another deployment backup is already running: $lock_dir" >&2
  exit 1
fi
release_lock() {
  rmdir "$lock_dir" 2>/dev/null || true
}
trap release_lock EXIT

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
dest="${backup_root}/${timestamp}"
partial_dest="${dest}.partial"
package="${dest}.tgz"
partial_package="${package}.partial"
checksum_tmp="${dest}.SHA256SUMS.partial"

if [[ -e "$partial_dest" || -e "$dest" || -e "$partial_package" || -e "$package" || -e "$checksum_tmp" ]]; then
  echo "Backup destination already exists: $package" >&2
  exit 1
fi

require_env() {
  local name="$1"

  if [[ -z "${!name:-}" ]]; then
    echo "set ${name} in ${deployment_dir}/.env" >&2
    exit 1
  fi
}

for required_env in \
  POSTGRES_USER POSTGRES_DB REDIS_PASSWORD \
  NEWAPI_POSTGRES_USER NEWAPI_POSTGRES_DB NEWAPI_REDIS_PASSWORD; do
  printf -v "$required_env" '%s' "$(dotenv_value "$required_env")"
  require_env "$required_env"
done

compose() {
  local -a clean_env=(env -i "PATH=$PATH" "HOME=${HOME:-/nonexistent}")
  local docker_variable

  for docker_variable in DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG; do
    if [[ -n "${!docker_variable:-}" ]]; then
      clean_env+=("${docker_variable}=${!docker_variable}")
    fi
  done
  "${clean_env[@]}" docker compose \
    -f docker-compose.yml \
    -f docker-compose.newapi.yml \
    "$@"
}

checksum_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi
}

stopped_services=()
running_services="$(compose ps --services --filter status=running)"

service_is_running() {
  local service="$1"
  printf '%s\n' "$running_services" | grep -qx "$service"
}

stop_running_service() {
  local service="$1"

  if service_is_running "$service"; then
    compose stop "$service" >/dev/null
    stopped_services+=("$service")
  fi
}

restart_stopped_services() {
  local best_effort="$1"
  local index

  for ((index = ${#stopped_services[@]} - 1; index >= 0; index--)); do
    if [[ "$best_effort" == "true" ]]; then
      compose start "${stopped_services[$index]}" >/dev/null || true
    else
      compose start "${stopped_services[$index]}" >/dev/null
    fi
  done
  stopped_services=()
}

cleanup_on_exit() {
  local status=$?
  trap - EXIT
  restart_stopped_services true
  if [[ "$status" -ne 0 ]]; then
    rm -rf "$partial_dest"
    rm -f "$partial_package" "$checksum_tmp"
  fi
  release_lock
  exit "$status"
}
trap cleanup_on_exit EXIT

mkdir "$partial_dest"
chmod 700 "$partial_dest"

stop_running_service traefik
stop_running_service new-api
stop_running_service sub2api

compose exec -T postgres pg_dump \
  -U "$POSTGRES_USER" \
  -d "$POSTGRES_DB" \
  --format=custom \
  > "${partial_dest}/sub2api-postgres.dump"

compose exec -T newapi-postgres pg_dump \
  -U "$NEWAPI_POSTGRES_USER" \
  -d "$NEWAPI_POSTGRES_DB" \
  --format=custom \
  > "${partial_dest}/newapi-postgres.dump"

compose exec -T redis redis-cli SAVE >/dev/null
stop_running_service redis
compose cp redis:/data "${partial_dest}/sub2api-redis-data"

compose exec -T newapi-redis redis-cli \
  -a "$NEWAPI_REDIS_PASSWORD" \
  SAVE >/dev/null
stop_running_service newapi-redis
compose cp newapi-redis:/data "${partial_dest}/redis-data"

tar -czf "${partial_dest}/deployment-runtime.tgz" \
  .env \
  data \
  letsencrypt

(
  cd "$partial_dest"
  find . -type f -print0 \
    | sort -z \
    | while IFS= read -r -d '' backup_file; do
        checksum_file "$backup_file"
      done \
    > "$checksum_tmp"
)
mv "$checksum_tmp" "${partial_dest}/SHA256SUMS"

(
  cd "$partial_dest"
  tar -czf "$partial_package" .
)
chmod 600 "$partial_package"
mv "$partial_package" "$package"
rm -rf "$partial_dest"

restart_stopped_services false
release_lock
trap - EXIT

echo "Backup package written to ${package}"
