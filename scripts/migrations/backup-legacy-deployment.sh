#!/usr/bin/env bash
set -euo pipefail

migration_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

# shellcheck disable=SC1091
source "${migration_script_dir}/../backup-deployment.sh"

usage() {
  echo "Usage: scripts/migrations/backup-legacy-deployment.sh DEPLOYMENT_DIR" >&2
}

# Variables assigned and consumed by the sourced backup implementation.
# shellcheck disable=SC2154
validate_deployment_files() {
  local required_file

  for required_file in docker-compose.yml docker-compose.newapi.yml; do
    if [[ ! -f "${deployment_dir}/${required_file}" ]]; then
      echo "Missing required legacy deployment file: ${deployment_dir}/${required_file}" >&2
      exit 1
    fi
  done
}

# shellcheck disable=SC2034
load_backup_credentials() {
  SUB2API_POSTGRES_USER="$(dotenv_value POSTGRES_USER)"
  SUB2API_POSTGRES_DB="$(dotenv_value POSTGRES_DB)"
  NEW_API_POSTGRES_USER="$(dotenv_value NEWAPI_POSTGRES_USER)"
  NEW_API_POSTGRES_DB="$(dotenv_value NEWAPI_POSTGRES_DB)"
  NEW_API_REDIS_PASSWORD="$(dotenv_value NEWAPI_REDIS_PASSWORD)"
}

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

# shellcheck disable=SC2034
configure_layout() {
  sub2api_postgres_service=postgres
  sub2api_redis_service=redis
  new_api_postgres_service=newapi-postgres
  new_api_redis_service=newapi-redis
  sub2api_app_data_dir=data
  sub2api_postgres_data_dir=postgres_data
  sub2api_redis_data_dir=redis_data
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi
if [[ $# -ne 1 ]]; then
  usage
  exit 1
fi

backup_main "$@"
