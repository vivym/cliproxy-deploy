#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Usage: scripts/run-lark-correction.sh LARK_CORRECTION_ARGS..." >&2
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi
if [[ $# -eq 0 ]]; then
  usage
  exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
lock_root="${repo_root}/lark-runtime/ops"
maintenance_session="${lock_root}/maintenance.session"
maintenance_lock="${lock_root}/maintenance.lock"
correction_container="new-api-lark-correction-ops"
readonly_container="new-api-lark-correction-readonly-ops"
maintenance_session_owned=false
maintenance_lock_owned=false
cd "$repo_root"

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

running_services() {
  compose --profile lark --profile lark-ops \
    ps --services --filter status=running
}

refuse_running_service() {
  local services="$1"
  local service="$2"

  if printf '%s\n' "$services" | grep -qx "$service"; then
    echo "Refusing Lark correction while service is running: $service" >&2
    exit 1
  fi
}

cleanup_boundary_acquire() {
  local status=$?
  trap - EXIT
  if [[ "$maintenance_lock_owned" == true ]]; then
    rm -f "${maintenance_lock}/mode" || status=1
    if ! rmdir "$maintenance_lock"; then
      echo "Could not release Lark correction maintenance lock: $maintenance_lock" >&2
      status=1
    else
      maintenance_lock_owned=false
    fi
  fi
  if [[ "$maintenance_session_owned" == true && "$maintenance_lock_owned" == false ]]; then
    if ! rmdir "$maintenance_session"; then
      echo "Could not release Lark correction maintenance session: $maintenance_session" >&2
      status=1
    else
      maintenance_session_owned=false
    fi
  fi
  exit "$status"
}

if [[ $# -eq 1 && "$1" == "--list-pending" ]]; then
  trap cleanup_boundary_acquire EXIT
  if ! mkdir "$maintenance_session"; then
    echo "Another Lark correction maintenance session owns: $maintenance_session" >&2
    exit 1
  fi
  maintenance_session_owned=true
  if ! mkdir "$maintenance_lock"; then
    echo "Another Lark correction maintenance session owns: $maintenance_lock" >&2
    exit 1
  fi
  maintenance_lock_owned=true
  chmod 755 "$maintenance_lock"
  printf 'readonly\n' > "${maintenance_lock}/mode"
  chmod 644 "${maintenance_lock}/mode"
  readonly_container_used=false

  # shellcheck disable=SC2329  # Invoked by the EXIT trap.
  cleanup_readonly() {
    local status=$?
    local readonly_ids
    trap - EXIT
    if [[ "$readonly_container_used" == true ]]; then
      docker_cli container rm -f "$readonly_container" >/dev/null 2>&1 || true
      if ! readonly_ids="$(
        docker_cli container ls -aq --filter "name=^/${readonly_container}$"
      )" || [[ -n "$readonly_ids" ]]; then
        echo "Could not confirm readonly correction container removal; maintenance boundary retained: $readonly_container" >&2
        exit 1
      fi
    fi
    rm -f "${maintenance_lock}/mode"
    if ! rmdir "$maintenance_lock"; then
      echo "Readonly correction lock could not be released: $maintenance_lock" >&2
      status=1
    elif ! rmdir "$maintenance_session"; then
      echo "Readonly correction session could not be released: $maintenance_session" >&2
      status=1
    fi
    exit "$status"
  }
  trap cleanup_readonly EXIT

  readonly_services="$(running_services)"
  for service in lark-quota-controller new-api-correction-endpoint lark-correction; do
    refuse_running_service "$readonly_services" "$service"
  done
  if ! docker_cli volume inspect new-api-lark-controller-data >/dev/null 2>&1; then
    echo "Controller state volume is unavailable: new-api-lark-controller-data" >&2
    exit 1
  fi
  readonly_container_used=true
  compose --profile lark-ops run --name "$readonly_container" --rm --no-deps lark-correction-readonly \
    --controller-db /var/lib/lark-controller/controller.sqlite \
    --list-pending
  exit
fi

"${repo_root}/scripts/verify-lark-secret-permissions.sh" --include-correction

services_before_lock="$(running_services)"
for service in new-api lark-quota-controller new-api-correction-endpoint lark-correction; do
  refuse_running_service "$services_before_lock" "$service"
done

trap cleanup_boundary_acquire EXIT
if ! mkdir "$maintenance_session"; then
  echo "Another Lark correction maintenance session owns: $maintenance_session" >&2
  exit 1
fi
maintenance_session_owned=true
if ! mkdir "$maintenance_lock"; then
  echo "Another Lark correction maintenance session owns: $maintenance_lock" >&2
  exit 1
fi
maintenance_lock_owned=true
chmod 755 "$maintenance_lock"
printf 'correction\n' > "${maintenance_lock}/mode"
chmod 644 "${maintenance_lock}/mode"

cleanup() {
  local status=$?
  local correction_ids
  local endpoint_ids
  trap - EXIT

  docker_cli container rm -f "$correction_container" >/dev/null 2>&1 || true
  compose --profile lark-ops rm -sf new-api-correction-endpoint >/dev/null 2>&1 || true
  if ! correction_ids="$(
    docker_cli container ls -aq --filter "name=^/${correction_container}$"
  )" || [[ -n "$correction_ids" ]]; then
    echo "Could not confirm correction CLI removal; maintenance lock retained: $maintenance_lock" >&2
    echo "After removing '$correction_container' and confirming its exact-name container query is empty, also confirm the endpoint is absent before running: sudo rm -f '${maintenance_lock}/mode' && sudo rmdir '$maintenance_lock' && sudo rmdir '$maintenance_session'" >&2
    exit 1
  fi
  if ! endpoint_ids="$(
    compose --profile lark-ops ps -aq new-api-correction-endpoint
  )" || [[ -n "$endpoint_ids" ]]; then
    echo "Could not confirm correction endpoint removal; maintenance lock retained: $maintenance_lock" >&2
    echo "After removing new-api-correction-endpoint and confirming 'docker compose --profile lark-ops ps -aq new-api-correction-endpoint' is empty, remove the lock with: sudo rm -f '${maintenance_lock}/mode' && sudo rmdir '$maintenance_lock' && sudo rmdir '$maintenance_session'" >&2
    exit 1
  fi
  rm -f "${maintenance_lock}/mode"
  if ! rmdir "$maintenance_lock"; then
    echo "Correction endpoint is gone, but maintenance lock could not be released: $maintenance_lock" >&2
    exit 1
  fi
  if ! rmdir "$maintenance_session"; then
    echo "Correction lock is gone, but maintenance session could not be released: $maintenance_session" >&2
    exit 1
  fi
  exit "$status"
}
trap cleanup EXIT

# Close the check/create race: a regular service that starts after lock creation
# sees the same marker in its entrypoint and refuses to execute.
services_after_lock="$(running_services)"
for service in new-api lark-quota-controller; do
  refuse_running_service "$services_after_lock" "$service"
done

compose --profile lark-ops rm -f new-api-correction-endpoint >/dev/null
compose --profile lark-ops up -d --wait --wait-timeout 60 new-api-correction-endpoint

services_after_endpoint="$(running_services)"
for service in new-api lark-quota-controller; do
  refuse_running_service "$services_after_endpoint" "$service"
done

compose --profile lark-ops run --name "$correction_container" --rm --no-deps lark-correction \
  --controller-db /var/lib/lark-controller/controller.sqlite \
  --new-api-base-url http://new-api-correction-endpoint:3001 \
  --correction-secret-file /run/secrets/lark-controller/new-api/lark_correction_secret \
  "$@"
