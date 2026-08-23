#!/usr/bin/env bash
set -euo pipefail

script_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
dotenv_reader="${script_repo_root}/scripts/read-dotenv.py"
manifest_tool="${script_repo_root}/scripts/deployment-backup-manifest.py"

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
if [[ ! -f "${deployment_dir}/docker-compose.yml" ]]; then
  echo "Missing required deployment file: ${deployment_dir}/docker-compose.yml" >&2
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

for required_path in \
  deployment-runtime.tgz \
  sub2api-postgres.dump \
  sub2api-redis-data; do
  if [[ ! -e "${backup_dir}/${required_path}" ]]; then
    echo "Missing required restore source: ${backup_dir}/${required_path}" >&2
    exit 1
  fi
done

if [[ -f "${backup_dir}/new-api-postgres.dump" ]]; then
  new_api_postgres_dump="${backup_dir}/new-api-postgres.dump"
elif [[ -f "${backup_dir}/newapi-postgres.dump" ]]; then
  new_api_postgres_dump="${backup_dir}/newapi-postgres.dump"
else
  echo "Missing required restore source: ${backup_dir}/new-api-postgres.dump" >&2
  exit 1
fi

if [[ -d "${backup_dir}/new-api-redis-data" ]]; then
  new_api_redis_data="${backup_dir}/new-api-redis-data"
elif [[ -d "${backup_dir}/redis-data" ]]; then
  new_api_redis_data="${backup_dir}/redis-data"
else
  echo "Missing required restore source: ${backup_dir}/new-api-redis-data" >&2
  exit 1
fi

verify_checksums() {
  if [[ ! -f "${backup_dir}/SHA256SUMS" ]]; then
    echo "Missing required restore source: ${backup_dir}/SHA256SUMS" >&2
    exit 1
  fi

  python3 "$manifest_tool" validate-checksums --root "$backup_dir"
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

if [[ -f "${backup_dir}/backup-manifest.json" ]]; then
  package_lark_state="$(
    python3 "$manifest_tool" validate \
      --root "$backup_dir" \
      --print-lark-state
  )"
else
  if [[ -e "${backup_dir}/lark-controller-data.tgz" || -e "${backup_dir}/lark-controller-data.absent" ]]; then
    echo "Lark backup state requires backup-manifest.json" >&2
    exit 1
  fi
  package_lark_state=legacy-absent
fi

if [[ "$package_lark_state" == enabled ]]; then
  validate_archive "${backup_dir}/lark-controller-data.tgz" "Lark Controller state"
  python3 "$manifest_tool" validate-archive-member \
    --archive "${backup_dir}/lark-controller-data.tgz" \
    --member controller.sqlite
fi
validate_archive "${backup_dir}/deployment-runtime.tgz" "deployment runtime"
tar -xzf "${backup_dir}/deployment-runtime.tgz" -C "$runtime_dir"
for required_path in .env letsencrypt; do
  if [[ ! -e "${runtime_dir}/${required_path}" ]]; then
    echo "Missing required runtime restore source: ${runtime_dir}/${required_path}" >&2
    exit 1
  fi
done

if [[ -d "${runtime_dir}/sub2api-data" ]]; then
  runtime_sub2api_data="${runtime_dir}/sub2api-data"
elif [[ -d "${runtime_dir}/data" ]]; then
  runtime_sub2api_data="${runtime_dir}/data"
else
  echo "Missing required runtime restore source: ${runtime_dir}/sub2api-data" >&2
  exit 1
fi

dotenv_value() {
  local env_file="$1"
  local key="$2"

  python3 "$dotenv_reader" "$env_file" "$key"
}

optional_dotenv_value() {
  local env_file="$1"
  local key="$2"

  python3 "$dotenv_reader" --allow-missing "$env_file" "$key"
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

append_env_value() {
  local env_file="$1"
  local key="$2"
  local value="$3"

  if [[ -n "$value" ]]; then
    printf '%s=%s\n' "$key" "$(quote_env_value "$value")" >> "$env_file"
  fi
}

normalize_runtime_env() {
  local env_file="$1"
  local normalized_env="${env_file}.normalized"
  local edge_subnet
  local new_api_data_subnet
  local sub2api_data_subnet
  local backup_dir_value
  local sub2api_postgres_user
  local sub2api_postgres_db
  local sub2api_postgres_password
  local sub2api_redis_password
  local new_api_postgres_user
  local new_api_postgres_db
  local new_api_postgres_password
  local new_api_redis_password
  local lark_controller_mode
  local lark_oauth_public_enabled

  edge_subnet="${EDGE_SUBNET:-$(optional_dotenv_value "$env_file" EDGE_SUBNET)}"
  sub2api_data_subnet="${SUB2API_DATA_SUBNET:-$(optional_dotenv_value "$env_file" SUB2API_DATA_SUBNET)}"
  new_api_data_subnet="${NEW_API_DATA_SUBNET:-$(optional_dotenv_value "$env_file" NEW_API_DATA_SUBNET)}"
  backup_dir_value="$(optional_dotenv_value "$env_file" BACKUP_DIR)"
  if [[ "$backup_dir_value" == "/var/backups/sub2api" ]]; then
    backup_dir_value=/var/backups/new-api
  fi

  sub2api_postgres_user="$(optional_dotenv_value "$env_file" SUB2API_POSTGRES_USER)"
  sub2api_postgres_db="$(optional_dotenv_value "$env_file" SUB2API_POSTGRES_DB)"
  sub2api_postgres_password="$(optional_dotenv_value "$env_file" SUB2API_POSTGRES_PASSWORD)"
  sub2api_redis_password="$(optional_dotenv_value "$env_file" SUB2API_REDIS_PASSWORD)"
  new_api_postgres_user="$(optional_dotenv_value "$env_file" NEW_API_POSTGRES_USER)"
  new_api_postgres_db="$(optional_dotenv_value "$env_file" NEW_API_POSTGRES_DB)"
  new_api_postgres_password="$(optional_dotenv_value "$env_file" NEW_API_POSTGRES_PASSWORD)"
  new_api_redis_password="$(optional_dotenv_value "$env_file" NEW_API_REDIS_PASSWORD)"
  lark_controller_mode="$(optional_dotenv_value "$env_file" LARK_CONTROLLER_MODE)"
  lark_oauth_public_enabled="$(optional_dotenv_value "$env_file" LARK_OAUTH_PUBLIC_ENABLED)"

  sub2api_postgres_user="${sub2api_postgres_user:-$(optional_dotenv_value "$env_file" POSTGRES_USER)}"
  sub2api_postgres_db="${sub2api_postgres_db:-$(optional_dotenv_value "$env_file" POSTGRES_DB)}"
  sub2api_postgres_password="${sub2api_postgres_password:-$(optional_dotenv_value "$env_file" POSTGRES_PASSWORD)}"
  sub2api_redis_password="${sub2api_redis_password:-$(optional_dotenv_value "$env_file" REDIS_PASSWORD)}"
  new_api_postgres_user="${new_api_postgres_user:-$(optional_dotenv_value "$env_file" NEWAPI_POSTGRES_USER)}"
  new_api_postgres_db="${new_api_postgres_db:-$(optional_dotenv_value "$env_file" NEWAPI_POSTGRES_DB)}"
  new_api_postgres_password="${new_api_postgres_password:-$(optional_dotenv_value "$env_file" NEWAPI_POSTGRES_PASSWORD)}"
  new_api_redis_password="${new_api_redis_password:-$(optional_dotenv_value "$env_file" NEWAPI_REDIS_PASSWORD)}"

  if [[ "$package_lark_state" == enabled ]]; then
    lark_controller_mode=shadow
    lark_oauth_public_enabled=false
  fi

  awk '
    !/^[[:space:]]*(SUB2API_HOST|SUB2API_TEST_API_KEY|SUB2API_PROXY_SUBNET|SUB2API_BACKEND_SUBNET|POSTGRES_USER|POSTGRES_DB|POSTGRES_PASSWORD|REDIS_PASSWORD|NEWAPI_POSTGRES_USER|NEWAPI_POSTGRES_DB|NEWAPI_POSTGRES_PASSWORD|NEWAPI_REDIS_PASSWORD|SUB2API_POSTGRES_USER|SUB2API_POSTGRES_DB|SUB2API_POSTGRES_PASSWORD|SUB2API_REDIS_PASSWORD|NEW_API_POSTGRES_USER|NEW_API_POSTGRES_DB|NEW_API_POSTGRES_PASSWORD|NEW_API_REDIS_PASSWORD|EDGE_SUBNET|SUB2API_DATA_SUBNET|NEW_API_DATA_SUBNET|BACKUP_DIR|LARK_CONTROLLER_MODE|LARK_OAUTH_PUBLIC_ENABLED)[[:space:]]*=/
  ' "$env_file" > "$normalized_env"

  printf '\n' >> "$normalized_env"
  append_env_value "$normalized_env" SUB2API_POSTGRES_USER "$sub2api_postgres_user"
  append_env_value "$normalized_env" SUB2API_POSTGRES_DB "$sub2api_postgres_db"
  append_env_value "$normalized_env" SUB2API_POSTGRES_PASSWORD "$sub2api_postgres_password"
  append_env_value "$normalized_env" SUB2API_REDIS_PASSWORD "$sub2api_redis_password"
  append_env_value "$normalized_env" NEW_API_POSTGRES_USER "$new_api_postgres_user"
  append_env_value "$normalized_env" NEW_API_POSTGRES_DB "$new_api_postgres_db"
  append_env_value "$normalized_env" NEW_API_POSTGRES_PASSWORD "$new_api_postgres_password"
  append_env_value "$normalized_env" NEW_API_REDIS_PASSWORD "$new_api_redis_password"
  append_env_value "$normalized_env" EDGE_SUBNET "$edge_subnet"
  append_env_value "$normalized_env" SUB2API_DATA_SUBNET "$sub2api_data_subnet"
  append_env_value "$normalized_env" NEW_API_DATA_SUBNET "$new_api_data_subnet"
  append_env_value "$normalized_env" BACKUP_DIR "$backup_dir_value"
  append_env_value "$normalized_env" LARK_CONTROLLER_MODE "$lark_controller_mode"
  append_env_value "$normalized_env" LARK_OAUTH_PUBLIC_ENABLED "$lark_oauth_public_enabled"

  chmod 600 "$normalized_env"
  mv "$normalized_env" "$env_file"
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

compose_with_env_file() {
  local env_file="$1"
  shift
  local -a compose_command=(compose)

  if [[ -n "$env_file" ]]; then
    compose_command+=(--env-file "$env_file")
  fi
  (
    cd "$deployment_dir"
    docker_cli "${compose_command[@]}" "$@"
  )
}

runtime_env="${runtime_dir}/.env"
python3 "$dotenv_reader" --validate "$runtime_env"
normalize_runtime_env "$runtime_env"
python3 "$dotenv_reader" --validate "$runtime_env"
restored_lark_listener="$(optional_dotenv_value "$runtime_env" NEW_API_INTEGRATION_LISTEN_ADDR)"
case "$package_lark_state" in
  enabled)
    if [[ -z "$restored_lark_listener" ]]; then
      echo "Lark enabled backup is missing its integration listener configuration" >&2
      exit 1
    fi
    if [[ ! -f "${runtime_dir}/lark-runtime/policies/approval-bindings.json" ]]; then
      echo "Lark enabled backup is missing approval-bindings.json" >&2
      exit 1
    fi
    if ! compgen -G "${runtime_dir}/lark-runtime/policies/*.policy.json" >/dev/null; then
      echo "Lark enabled backup is missing versioned policy state" >&2
      exit 1
    fi
    ;;
  absent|legacy-absent)
    if [[ -n "$restored_lark_listener" ]]; then
      echo "Lark absent backup cannot enable the integration listener" >&2
      exit 1
    fi
    ;;
  *)
    echo "Unsupported Lark package state: $package_lark_state" >&2
    exit 1
    ;;
esac
if [[ "$package_lark_state" == enabled ]]; then
  compose_with_env_file "$runtime_env" --profile lark config --quiet
else
  compose_with_env_file "$runtime_env" config --quiet
fi

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
  rm -rf "$restore_tmp"
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
printf 'restore\n' > "${maintenance_lock}/mode"
chmod 600 "${maintenance_lock}/mode"
restore_mutated=false
restore_container=new-api-lark-restore-controller
restore_container_used=false
startup_handoff_active=false

recover_startup_handoff() {
  local recovery_failed=false
  local running_services

  if [[ "$maintenance_lock_owned" != true ]]; then
    if mkdir "$maintenance_lock"; then
      maintenance_lock_owned=true
      if ! printf 'restore\n' > "${maintenance_lock}/mode"; then
        echo "Could not write re-established deployment maintenance lock after startup failure: $maintenance_lock" >&2
        recovery_failed=true
      elif ! chmod 600 "${maintenance_lock}/mode"; then
        echo "Could not secure re-established deployment maintenance lock after startup failure: $maintenance_lock" >&2
        recovery_failed=true
      fi
    else
      echo "Could not re-establish deployment maintenance lock after startup failure: $maintenance_lock" >&2
      recovery_failed=true
    fi
  fi
  if ! compose down; then
    echo "Could not stop partially started services after restore startup failure" >&2
    recovery_failed=true
  fi
  if ! running_services="$(compose ps --services --filter status=running)"; then
    echo "Could not verify services stopped after restore startup failure" >&2
    recovery_failed=true
  elif [[ -n "$running_services" ]]; then
    echo "Services remain running after restore startup failure: $running_services" >&2
    recovery_failed=true
  fi
  [[ "$recovery_failed" == false ]]
}

cleanup_restore() {
  local status=$?
  local restore_ids
  trap - EXIT
  if [[ "$startup_handoff_active" == true ]]; then
    startup_handoff_active=false
    if ! recover_startup_handoff; then
      status=1
    fi
  fi
  if [[ "$restore_container_used" == true ]]; then
    docker_cli container rm -f "$restore_container" >/dev/null 2>&1 || true
    if ! restore_ids="$(
      docker_cli container ls -aq --filter "name=^/${restore_container}$"
    )" || [[ -n "$restore_ids" ]]; then
      echo "Could not confirm Controller restore container removal; maintenance lock retained: $maintenance_lock" >&2
      restore_mutated=true
      status=1
    fi
  fi
  rm -rf "$restore_tmp"
  if [[ "$maintenance_lock_owned" == true ]]; then
    if [[ "$restore_mutated" == true ]]; then
      echo "Restore did not complete; deployment maintenance lock retained: $maintenance_lock" >&2
      echo "Repair or rerun the full restore before removing this lock" >&2
    elif ! release_maintenance_lock; then
      status=1
    fi
  fi
  if [[ "$maintenance_session_owned" == true && "$maintenance_lock_owned" == false && "$restore_mutated" == false ]]; then
    if ! release_maintenance_session; then
      status=1
    fi
  fi
  exit "$status"
}
trap cleanup_restore EXIT

current_running_services="$(compose_with_env_file "" ps --services --filter status=running)"
for service in new-api-correction-endpoint lark-correction; do
  if printf '%s\n' "$current_running_services" | grep -qx "$service"; then
    echo "Refusing restore while Lark correction service is running: $service" >&2
    exit 1
  fi
done

SUB2API_POSTGRES_USER="$(dotenv_value "$runtime_env" SUB2API_POSTGRES_USER)"
SUB2API_POSTGRES_DB="$(dotenv_value "$runtime_env" SUB2API_POSTGRES_DB)"
NEW_API_POSTGRES_USER="$(dotenv_value "$runtime_env" NEW_API_POSTGRES_USER)"
NEW_API_POSTGRES_DB="$(dotenv_value "$runtime_env" NEW_API_POSTGRES_DB)"

require_env() {
  local name="$1"

  if [[ -z "${!name:-}" ]]; then
    echo "set ${name} in ${runtime_env}" >&2
    exit 1
  fi
}

for required_env in \
  SUB2API_POSTGRES_USER SUB2API_POSTGRES_DB \
  NEW_API_POSTGRES_USER NEW_API_POSTGRES_DB; do
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

restore_mutated=true
compose_with_env_file "$runtime_env" down

rm -f "${deployment_dir}/.env"
rm -rf \
  "${deployment_dir}/data" \
  "${deployment_dir}/sub2api-data" \
  "${deployment_dir}/letsencrypt"
cp -a "${runtime_dir}/.env" "${deployment_dir}/.env"
cp -a "$runtime_sub2api_data" "${deployment_dir}/sub2api-data"
cp -a "${runtime_dir}/letsencrypt" "${deployment_dir}/letsencrypt"
rm -rf "${deployment_dir}/lark-runtime/policies"
if [[ "$package_lark_state" == enabled ]]; then
  mkdir -p "${deployment_dir}/lark-runtime"
  cp -a \
    "${runtime_dir}/lark-runtime/policies" \
    "${deployment_dir}/lark-runtime/policies"
fi
chmod 600 "${deployment_dir}/.env"
chmod 600 "${deployment_dir}/letsencrypt/acme.json" 2>/dev/null || true

rm -rf \
  "${deployment_dir}/postgres_data" \
  "${deployment_dir}/redis_data" \
  "${deployment_dir}/sub2api-postgres-data" \
  "${deployment_dir}/sub2api-redis-data"
mkdir -p \
  "${deployment_dir}/sub2api-postgres-data" \
  "${deployment_dir}/sub2api-redis-data"
cp -a \
  "${backup_dir}/sub2api-redis-data/." \
  "${deployment_dir}/sub2api-redis-data/"

lark_controller_volumes="$(
  docker_cli volume ls --quiet --filter name=new-api-lark-controller-data
)"
if printf '%s\n' "$lark_controller_volumes" | grep -qx new-api-lark-controller-data; then
  docker_cli volume rm -f new-api-lark-controller-data >/dev/null
fi
lark_controller_volumes="$(
  docker_cli volume ls --quiet --filter name=new-api-lark-controller-data
)"
if printf '%s\n' "$lark_controller_volumes" | grep -qx new-api-lark-controller-data; then
  echo "Could not remove existing Controller state volume" >&2
  exit 1
fi
if [[ "$package_lark_state" == enabled ]]; then
  docker_cli volume create new-api-lark-controller-data >/dev/null
  restore_container_used=true
  docker_cli run \
    --name "$restore_container" \
    --rm \
    -v "new-api-lark-controller-data:/target" \
    -v "${backup_dir}/lark-controller-data.tgz:/backup/controller.tgz:ro" \
    alpine:3.22 \
    sh -ec \
    'rm -rf /target/* /target/.[!.]* /target/..?* 2>/dev/null || true; tar -xzf /backup/controller.tgz -C /target; test -f /target/controller.sqlite'
fi

compose create new-api-postgres new-api-redis >/dev/null
clear_service_volume new-api-postgres /var/lib/postgresql/data
replace_service_volume new-api-redis /data "$new_api_redis_data"

compose up -d sub2api-postgres new-api-postgres
wait_for_postgres sub2api-postgres "$SUB2API_POSTGRES_USER" "$SUB2API_POSTGRES_DB"
wait_for_postgres new-api-postgres "$NEW_API_POSTGRES_USER" "$NEW_API_POSTGRES_DB"

compose exec -T sub2api-postgres pg_restore \
  -U "$SUB2API_POSTGRES_USER" \
  -d "$SUB2API_POSTGRES_DB" \
  --clean \
  --if-exists \
  --no-owner \
  < "${backup_dir}/sub2api-postgres.dump"

compose exec -T new-api-postgres pg_restore \
  -U "$NEW_API_POSTGRES_USER" \
  -d "$NEW_API_POSTGRES_DB" \
  --clean \
  --if-exists \
  --no-owner \
  < "$new_api_postgres_dump"

if [[ "$restore_container_used" == true ]]; then
  restore_ids="$(
    docker_cli container ls -aq --filter "name=^/${restore_container}$"
  )"
  if [[ -n "$restore_ids" ]]; then
    echo "Controller restore container still exists; maintenance lock retained: $maintenance_lock" >&2
    exit 1
  fi
  restore_container_used=false
fi

if [[ "$package_lark_state" == enabled ]]; then
  start_command=(--profile lark up -d --wait --wait-timeout 120)
else
  start_command=(up -d --wait --wait-timeout 120)
fi
startup_handoff_active=true
release_maintenance_lock
compose "${start_command[@]}"
release_maintenance_session
startup_handoff_active=false

echo "Restore completed from ${backup_package}"
