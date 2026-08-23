#!/usr/bin/env bash
set -euo pipefail

script_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
dotenv_reader="${script_repo_root}/scripts/read-dotenv.py"
manifest_tool="${script_repo_root}/scripts/deployment-backup-manifest.py"

usage() {
  echo "Usage: scripts/backup-deployment.sh [DEPLOYMENT_DIR]" >&2
}

validate_deployment_files() {
  if [[ ! -f "${deployment_dir}/docker-compose.yml" ]]; then
    echo "Missing required deployment file: ${deployment_dir}/docker-compose.yml" >&2
    exit 1
  fi
}

dotenv_value() {
  local key="$1"
  local optional="${2:-false}"

  if [[ "$optional" == "true" ]]; then
    python3 "$dotenv_reader" --allow-missing .env "$key"
  else
    python3 "$dotenv_reader" .env "$key"
  fi
}

require_env() {
  local name="$1"

  if [[ -z "${!name:-}" ]]; then
    echo "set ${name} in ${deployment_dir}/.env" >&2
    exit 1
  fi
}

load_backup_credentials() {
  SUB2API_POSTGRES_USER="$(dotenv_value SUB2API_POSTGRES_USER)"
  SUB2API_POSTGRES_DB="$(dotenv_value SUB2API_POSTGRES_DB)"
  NEW_API_POSTGRES_USER="$(dotenv_value NEW_API_POSTGRES_USER)"
  NEW_API_POSTGRES_DB="$(dotenv_value NEW_API_POSTGRES_DB)"
  NEW_API_REDIS_PASSWORD="$(dotenv_value NEW_API_REDIS_PASSWORD)"
}

docker_cli() {
  local -a clean_env=(env -i "PATH=$PATH" "HOME=${HOME:-/nonexistent}")
  local docker_variable

  for docker_variable in DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG; do
    if [[ -n "${!docker_variable:-}" ]]; then
      clean_env+=("${docker_variable}=${!docker_variable}")
    fi
  done
  "${clean_env[@]}" docker "$@"
}

compose() {
  docker_cli compose "$@"
}

configure_layout() {
  sub2api_postgres_service=sub2api-postgres
  sub2api_redis_service=sub2api-redis
  new_api_postgres_service=new-api-postgres
  new_api_redis_service=new-api-redis
  sub2api_app_data_dir=sub2api-data
  sub2api_postgres_data_dir=sub2api-postgres-data
  sub2api_redis_data_dir=sub2api-redis-data
}

backup_main() {
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
validate_deployment_files
cd "$deployment_dir"
configure_layout

if [[ ! -f .env ]]; then
  echo "Missing required backup source: .env" >&2
  exit 1
fi
python3 "$dotenv_reader" --validate .env

configured_lark_listener="$(dotenv_value NEW_API_INTEGRATION_LISTEN_ADDR true)"

env_backup_root="$(dotenv_value BACKUP_DIR true)"
backup_root="${BACKUP_DIR:-${env_backup_root:-/var/backups/new-api}}"
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

for required_path in \
  .env \
  "$sub2api_app_data_dir" \
  "$sub2api_postgres_data_dir" \
  "$sub2api_redis_data_dir" \
  letsencrypt; do
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

lock_root="${deployment_dir}/lark-runtime/ops"
maintenance_session="${lock_root}/maintenance.session"
maintenance_lock="${lock_root}/maintenance.lock"
maintenance_session_owned=false
maintenance_lock_owned=false

release_maintenance_lock() {
  rm -f "${maintenance_lock}/mode"
  if ! rmdir "$maintenance_lock"; then
    echo "Could not release deployment maintenance lock: $maintenance_lock" >&2
    return 1
  fi
  maintenance_lock_owned=false
}

release_maintenance_session() {
  if ! rmdir "$maintenance_session"; then
    echo "Could not release deployment maintenance session: $maintenance_session" >&2
    return 1
  fi
  maintenance_session_owned=false
}

cleanup_boundary_acquire() {
  local status=$?
  trap - EXIT
  if [[ "$maintenance_lock_owned" == true ]]; then
    release_maintenance_lock || status=1
  fi
  if [[ "$maintenance_session_owned" == true && "$maintenance_lock_owned" == false ]]; then
    release_maintenance_session || status=1
  fi
  release_lock
  exit "$status"
}

mkdir -p "$lock_root"
trap cleanup_boundary_acquire EXIT
if ! mkdir "$maintenance_session"; then
  echo "Another deployment maintenance session owns: $maintenance_session" >&2
  exit 1
fi
maintenance_session_owned=true
if ! mkdir "$maintenance_lock"; then
  echo "Another deployment maintenance session owns: $maintenance_lock" >&2
  exit 1
fi
maintenance_lock_owned=true
printf 'backup\n' > "${maintenance_lock}/mode"
chmod 600 "${maintenance_lock}/mode"

cleanup_early_exit() {
  local status=$?
  trap - EXIT
  if [[ "$maintenance_lock_owned" == true ]]; then
    release_maintenance_lock || status=1
  fi
  if [[ "$maintenance_session_owned" == true && "$maintenance_lock_owned" == false ]]; then
    release_maintenance_session || status=1
  fi
  release_lock
  exit "$status"
}
trap cleanup_early_exit EXIT

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

load_backup_credentials

for required_env in \
  SUB2API_POSTGRES_USER SUB2API_POSTGRES_DB \
  NEW_API_POSTGRES_USER NEW_API_POSTGRES_DB NEW_API_REDIS_PASSWORD; do
  require_env "$required_env"
done

checksum_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi
}

stopped_services=()
running_services="$(compose ps --services --filter status=running)"
snapshot_container=new-api-lark-backup-snapshot
snapshot_container_used=false

service_is_running() {
  local service="$1"
  printf '%s\n' "$running_services" | grep -qx "$service"
}

if service_is_running new-api; then
  effective_lark_listener="$(
    # Expand inside the container, not on the host.
    # shellcheck disable=SC2016
    compose exec -T new-api sh -c \
      'printf "%s" "${INTEGRATION_LISTEN_ADDR:-}"'
  )"
  if [[ -n "$effective_lark_listener" && -z "$configured_lark_listener" ]]; then
    echo "Running New API integration listener disagrees with the disabled deployment configuration" >&2
    exit 1
  fi
  if [[ -z "$effective_lark_listener" && -n "$configured_lark_listener" ]]; then
    echo "Running New API integration listener disagrees with the enabled deployment configuration" >&2
    exit 1
  fi
fi

lark_controller_volume_exists=false
if docker_cli volume inspect new-api-lark-controller-data >/dev/null 2>&1; then
  lark_controller_volume_exists=true
fi
if [[ -n "$configured_lark_listener" && "$lark_controller_volume_exists" != true ]]; then
  echo "Lark integration is enabled but Controller SQLite state is absent" >&2
  exit 1
fi
if [[ -z "$configured_lark_listener" && "$lark_controller_volume_exists" == true ]]; then
  echo "Controller SQLite state exists while the Lark integration is disabled" >&2
  exit 1
fi
if [[ -n "$configured_lark_listener" ]]; then
  if [[ ! -f lark-runtime/policies/approval-bindings.json ]]; then
    echo "Missing required backup source: lark-runtime/policies/approval-bindings.json" >&2
    exit 1
  fi
  if ! compgen -G 'lark-runtime/policies/*.policy.json' >/dev/null; then
    echo "Missing required backup source: lark-runtime/policies/*.policy.json" >&2
    exit 1
  fi
fi
for service in new-api-correction-endpoint lark-correction; do
  if service_is_running "$service"; then
    echo "Refusing backup while Lark correction service is running: $service" >&2
    exit 1
  fi
done

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
  local snapshot_ids
  local retain_maintenance_lock=false
  trap - EXIT
  if [[ "$snapshot_container_used" == true ]]; then
    docker_cli container rm -f "$snapshot_container" >/dev/null 2>&1 || true
    if ! snapshot_ids="$(
      docker_cli container ls -aq --filter "name=^/${snapshot_container}$"
    )" || [[ -n "$snapshot_ids" ]]; then
      echo "Could not confirm Controller snapshot container removal; maintenance lock retained: $maintenance_lock" >&2
      retain_maintenance_lock=true
      status=1
    fi
  fi
  if [[ "$status" -ne 0 ]]; then
    rm -rf "$partial_dest"
    rm -f "$partial_package" "$checksum_tmp"
  fi
  if [[ "$retain_maintenance_lock" == false && "$maintenance_lock_owned" == true ]]; then
    if release_maintenance_lock; then
      restart_stopped_services true
      release_maintenance_session || status=1
    else
      status=1
    fi
  fi
  release_lock
  exit "$status"
}
trap cleanup_on_exit EXIT

mkdir "$partial_dest"
chmod 700 "$partial_dest"

stop_running_service traefik
stop_running_service lark-quota-controller
stop_running_service new-api
stop_running_service sub2api

if [[ "$lark_controller_volume_exists" == true ]]; then
  snapshot_container_used=true
  docker_cli run \
    --name "$snapshot_container" \
    --rm \
    -v "new-api-lark-controller-data:/source:ro" \
    -v "${partial_dest}:/backup" \
    alpine:3.22 \
    sh -ec \
    'test -f /source/controller.sqlite; tar -czf /backup/lark-controller-data.tgz -C /source .; chmod 0644 /backup/lark-controller-data.tgz'
else
  printf 'new-api-lark-controller-data absent\n' > "${partial_dest}/lark-controller-data.absent"
fi

compose exec -T "$sub2api_postgres_service" pg_dump \
  -U "$SUB2API_POSTGRES_USER" \
  -d "$SUB2API_POSTGRES_DB" \
  --format=custom \
  > "${partial_dest}/sub2api-postgres.dump"

compose exec -T "$new_api_postgres_service" pg_dump \
  -U "$NEW_API_POSTGRES_USER" \
  -d "$NEW_API_POSTGRES_DB" \
  --format=custom \
  > "${partial_dest}/new-api-postgres.dump"

compose exec -T "$sub2api_redis_service" redis-cli SAVE >/dev/null
stop_running_service "$sub2api_redis_service"
compose cp "${sub2api_redis_service}:/data" "${partial_dest}/sub2api-redis-data"

compose exec -T "$new_api_redis_service" redis-cli \
  -a "$NEW_API_REDIS_PASSWORD" \
  SAVE >/dev/null
stop_running_service "$new_api_redis_service"
compose cp "${new_api_redis_service}:/data" "${partial_dest}/new-api-redis-data"

runtime_paths=(.env "$sub2api_app_data_dir" letsencrypt)
if [[ "$lark_controller_volume_exists" == true ]]; then
  runtime_paths+=(lark-runtime/policies)
fi
tar -czf "${partial_dest}/deployment-runtime.tgz" "${runtime_paths[@]}"

manifest_args=(
  create
  --root "$partial_dest"
  --created-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  --lark-state absent
)
if [[ "$lark_controller_volume_exists" == true ]]; then
  manifest_args=(
    create
    --root "$partial_dest"
    --created-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    --lark-state enabled
  )
fi
for service in "${stopped_services[@]}"; do
  manifest_args+=(--stopped-service "$service")
done
python3 "$manifest_tool" "${manifest_args[@]}"

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

if [[ "$snapshot_container_used" == true ]]; then
  snapshot_ids="$(
    docker_cli container ls -aq --filter "name=^/${snapshot_container}$"
  )"
  if [[ -n "$snapshot_ids" ]]; then
    echo "Controller snapshot container still exists; maintenance lock retained: $maintenance_lock" >&2
    exit 1
  fi
fi
mv "$partial_package" "$package"
rm -rf "$partial_dest"
release_maintenance_lock
restart_stopped_services false
release_maintenance_session
release_lock
trap - EXIT

echo "Backup package written to ${package}"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  backup_main "$@"
fi
