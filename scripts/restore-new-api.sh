#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
dotenv_reader="${repo_root}/scripts/read-dotenv.py"
manifest_tool="${repo_root}/scripts/deployment-backup-manifest.py"

usage() {
  echo "Usage: scripts/restore-new-api.sh BACKUP_PACKAGE [DEPLOYMENT_DIR]" >&2
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
for required_path in .env docker-compose.yml; do
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
    if [[ "${ALLOW_UNVERIFIED_LEGACY_BACKUP:-false}" != "true" ]]; then
      echo "Missing required restore source: ${backup_dir}/SHA256SUMS" >&2
      echo "Set ALLOW_UNVERIFIED_LEGACY_BACKUP=true only for a separately verified historical package" >&2
      exit 1
    fi
    echo "WARNING: restoring an unverified historical package by explicit request" >&2
    return 0
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
  local env_file="$3"
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

seed_new_api_env_from_backup() {
  local target_env="$1"
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
    set_env_if_missing_or_blank NEW_API_HOST "$(dotenv_value "$runtime_env" NEW_API_HOST true)" "$target_env"
    set_env_if_missing_or_blank NEW_API_IMAGE_TAG "$(dotenv_value "$runtime_env" NEW_API_IMAGE_TAG true)" "$target_env"
    set_env_if_missing_or_blank NEW_API_POSTGRES_USER "$(dotenv_value "$runtime_env" NEW_API_POSTGRES_USER true)" "$target_env"
    set_env_if_missing_or_blank NEW_API_POSTGRES_DB "$(dotenv_value "$runtime_env" NEW_API_POSTGRES_DB true)" "$target_env"
    set_env_if_missing_or_blank NEW_API_POSTGRES_PASSWORD "$(dotenv_value "$runtime_env" NEW_API_POSTGRES_PASSWORD true)" "$target_env"
    set_env_if_missing_or_blank NEW_API_REDIS_PASSWORD "$(dotenv_value "$runtime_env" NEW_API_REDIS_PASSWORD true)" "$target_env"
    set_env_if_missing_or_blank NEW_API_POSTGRES_USER "$(dotenv_value "$runtime_env" NEWAPI_POSTGRES_USER true)" "$target_env"
    set_env_if_missing_or_blank NEW_API_POSTGRES_DB "$(dotenv_value "$runtime_env" NEWAPI_POSTGRES_DB true)" "$target_env"
    set_env_if_missing_or_blank NEW_API_POSTGRES_PASSWORD "$(dotenv_value "$runtime_env" NEWAPI_POSTGRES_PASSWORD true)" "$target_env"
    set_env_if_missing_or_blank NEW_API_REDIS_PASSWORD "$(dotenv_value "$runtime_env" NEWAPI_REDIS_PASSWORD true)" "$target_env"
    set_env_if_missing_or_blank NEW_API_SESSION_SECRET "$(dotenv_value "$runtime_env" NEW_API_SESSION_SECRET true)" "$target_env"
    set_env_if_missing_or_blank NEW_API_CRYPTO_SECRET "$(dotenv_value "$runtime_env" NEW_API_CRYPTO_SECRET true)" "$target_env"
    return
  fi

  set_env_if_missing_or_blank NEW_API_HOST "$(dotenv_value "$runtime_env" AI_HOST true)" "$target_env"
  set_env_if_missing_or_blank NEW_API_IMAGE_TAG "$(dotenv_value "$runtime_env" NEW_API_IMAGE_TAG true)" "$target_env"
  set_env_if_missing_or_blank NEW_API_POSTGRES_USER "$(dotenv_value "$runtime_env" POSTGRES_USER true)" "$target_env"
  set_env_if_missing_or_blank NEW_API_POSTGRES_DB "$(dotenv_value "$runtime_env" POSTGRES_DB true)" "$target_env"
  set_env_if_missing_or_blank NEW_API_POSTGRES_PASSWORD "$(dotenv_value "$runtime_env" POSTGRES_PASSWORD true)" "$target_env"
  set_env_if_missing_or_blank NEW_API_REDIS_PASSWORD "$(dotenv_value "$runtime_env" REDIS_PASSWORD true)" "$target_env"
  set_env_if_missing_or_blank NEW_API_SESSION_SECRET "$(dotenv_value "$runtime_env" NEW_API_SESSION_SECRET true)" "$target_env"
  set_env_if_missing_or_blank NEW_API_CRYPTO_SECRET "$(dotenv_value "$runtime_env" NEW_API_CRYPTO_SECRET true)" "$target_env"
}

load_new_api_env() {
  local env_file="$1"
  local required_name
  local required_value

  python3 "$dotenv_reader" --validate "$env_file"
  for required_name in \
    NEW_API_HOST NEW_API_IMAGE_TAG \
    NEW_API_POSTGRES_USER NEW_API_POSTGRES_DB \
    NEW_API_POSTGRES_PASSWORD NEW_API_REDIS_PASSWORD \
    NEW_API_SESSION_SECRET NEW_API_CRYPTO_SECRET; do
    required_value="$(dotenv_value "$env_file" "$required_name" true)"
    if [[ -z "$required_value" ]]; then
      echo "set ${required_name} in ${env_file}" >&2
      exit 1
    fi
  done

  NEW_API_POSTGRES_USER="$(dotenv_value "$env_file" NEW_API_POSTGRES_USER)"
  NEW_API_POSTGRES_DB="$(dotenv_value "$env_file" NEW_API_POSTGRES_DB)"
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
  (
    cd "$deployment_dir"
    docker_cli compose "$@"
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
    if compose exec -T new-api-postgres pg_isready \
      -U "${NEW_API_POSTGRES_USER}" \
      -d "${NEW_API_POSTGRES_DB}" >/dev/null 2>&1; then
      return
    fi
    sleep 2
  done

  echo "New API Postgres did not become ready" >&2
  exit 1
}

verify_checksums
if [[ -f "${backup_dir}/backup-manifest.json" ]]; then
  package_lark_state="$(
    python3 "$manifest_tool" validate \
      --root "$backup_dir" \
      --print-lark-state
  )"
  if [[ "$package_lark_state" == enabled ]]; then
    echo "Lark-enabled packages require scripts/restore-deployment.sh full restore" >&2
    exit 1
  fi
elif [[ -e "${backup_dir}/lark-controller-data.tgz" || -e "${backup_dir}/lark-controller-data.absent" ]]; then
  echo "Lark backup state requires backup-manifest.json and scripts/restore-deployment.sh full restore" >&2
  exit 1
fi

python3 "$dotenv_reader" --validate "${deployment_dir}/.env"
current_lark_listener="$(
  dotenv_value "${deployment_dir}/.env" NEW_API_INTEGRATION_LISTEN_ADDR true
)"
if [[ -n "$current_lark_listener" ]]; then
  echo "Lark-enabled targets require scripts/restore-deployment.sh full restore" >&2
  exit 1
fi

candidate_env="${restore_tmp}/target.env"
cp -p "${deployment_dir}/.env" "$candidate_env"
seed_new_api_env_from_backup "$candidate_env"
load_new_api_env "$candidate_env"

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
target_env_tmp=""
startup_handoff_active=false

recover_startup_handoff() {
  local recovery_failed=false
  local running_services
  local service

  if [[ "$maintenance_lock_owned" != true ]]; then
    if mkdir "$maintenance_lock"; then
      maintenance_lock_owned=true
      if ! printf 'restore\n' > "${maintenance_lock}/mode"; then
        echo "Could not write re-established deployment maintenance lock after New API startup failure: $maintenance_lock" >&2
        recovery_failed=true
      elif ! chmod 600 "${maintenance_lock}/mode"; then
        echo "Could not secure re-established deployment maintenance lock after New API startup failure: $maintenance_lock" >&2
        recovery_failed=true
      fi
    else
      echo "Could not re-establish deployment maintenance lock after New API startup failure: $maintenance_lock" >&2
      recovery_failed=true
    fi
  fi
  for service in new-api new-api-postgres new-api-redis; do
    if ! compose stop "$service" >/dev/null; then
      echo "Could not stop service after New API restore startup failure: $service" >&2
      recovery_failed=true
    fi
  done
  if ! running_services="$(compose ps --services --filter status=running)"; then
    echo "Could not verify services stopped after New API restore startup failure" >&2
    recovery_failed=true
  else
    for service in new-api new-api-postgres new-api-redis; do
      if printf '%s\n' "$running_services" | grep -qx "$service"; then
        echo "Service remains running after New API restore startup failure: $service" >&2
        recovery_failed=true
      fi
    done
  fi
  [[ "$recovery_failed" == false ]]
}

cleanup_restore() {
  local status=$?
  trap - EXIT
  if [[ "$startup_handoff_active" == true ]]; then
    startup_handoff_active=false
    if ! recover_startup_handoff; then
      status=1
    fi
  fi
  if [[ -n "$target_env_tmp" ]]; then
    rm -f "$target_env_tmp"
  fi
  rm -rf "$restore_tmp"
  if [[ "$maintenance_lock_owned" == true ]]; then
    if [[ "$restore_mutated" == true ]]; then
      echo "New API-only restore did not complete; maintenance lock retained: $maintenance_lock" >&2
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

current_running_services="$(compose ps --services --filter status=running)"
for service in lark-quota-controller new-api-correction-endpoint lark-correction; do
  if printf '%s\n' "$current_running_services" | grep -qx "$service"; then
    echo "Lark Controller or correction state requires scripts/restore-deployment.sh full restore: $service" >&2
    exit 1
  fi
done
if printf '%s\n' "$current_running_services" | grep -qx new-api; then
  effective_lark_listener="$(
    # Expand inside the container, not on the host.
    # shellcheck disable=SC2016
    compose exec -T new-api sh -c 'printf "%s" "${INTEGRATION_LISTEN_ADDR:-}"'
  )"
  if [[ -n "$effective_lark_listener" ]]; then
    echo "Running Lark-enabled New API requires scripts/restore-deployment.sh full restore" >&2
    exit 1
  fi
fi
if docker_cli volume inspect new-api-lark-controller-data >/dev/null 2>&1; then
  echo "Existing Lark Controller state requires scripts/restore-deployment.sh full restore" >&2
  exit 1
fi

restore_mutated=true
target_env_tmp="$(mktemp "${deployment_dir}/.env.restore.XXXXXX")"
cp "$candidate_env" "$target_env_tmp"
chmod 600 "$target_env_tmp"
mv "$target_env_tmp" "${deployment_dir}/.env"
target_env_tmp=""

for service in new-api new-api-postgres new-api-redis; do
  if ! compose stop "$service" >/dev/null; then
    echo "Failed to stop service before restore: ${service}" >&2
    exit 1
  fi
done

running_services="$(compose ps --services --filter status=running)"
for service in new-api new-api-postgres new-api-redis; do
  if printf '%s\n' "$running_services" | grep -qx "$service"; then
    echo "Service is still running; refusing to clear restore volumes: ${service}" >&2
    exit 1
  fi
done

compose create new-api-postgres >/dev/null
clear_volume_dir new-api-postgres /var/lib/postgresql/data

compose create new-api-redis >/dev/null
restore_volume_dir new-api-redis /data "$new_api_redis_data"

compose up -d new-api-postgres
wait_for_postgres
compose exec -T new-api-postgres pg_restore \
  -U "${NEW_API_POSTGRES_USER}" \
  -d "${NEW_API_POSTGRES_DB}" \
  --clean \
  --if-exists \
  --no-owner \
  < "$new_api_postgres_dump"

compose up -d new-api-redis
startup_handoff_active=true
release_maintenance_lock
compose up -d --wait --wait-timeout 120 new-api
release_maintenance_session
startup_handoff_active=false

echo "New API-only restore completed from ${backup_package}"
