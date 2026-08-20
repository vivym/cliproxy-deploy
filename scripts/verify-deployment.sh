#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
dotenv_reader="${repo_root}/scripts/read-dotenv.py"
cd "$repo_root"

if [[ ! -f .env ]]; then
  echo "Missing required deployment file: ${repo_root}/.env" >&2
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

SUB2API_HOST="$(dotenv_value SUB2API_HOST)"
SUB2API_ADMIN_HOST="$(dotenv_value SUB2API_ADMIN_HOST)"
NEW_API_HOST="$(dotenv_value NEW_API_HOST)"
env_sub2api_test_key="$(dotenv_value SUB2API_TEST_API_KEY true)"
env_new_api_test_key="$(dotenv_value NEW_API_TEST_API_KEY true)"
SUB2API_TEST_API_KEY="${SUB2API_TEST_API_KEY:-$env_sub2api_test_key}"
NEW_API_TEST_API_KEY="${NEW_API_TEST_API_KEY:-$env_new_api_test_key}"

sub2api_url="https://${SUB2API_HOST:?set SUB2API_HOST}"
sub2api_admin_url="https://${SUB2API_ADMIN_HOST:?set SUB2API_ADMIN_HOST}"
newapi_url="https://${NEW_API_HOST:?set NEW_API_HOST}"

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

running_services="$(compose ps --services --filter status=running)"
for service in traefik postgres redis sub2api newapi-postgres newapi-redis new-api; do
  if ! printf '%s\n' "$running_services" | grep -qx "$service"; then
    echo "Required service is not running: $service" >&2
    exit 1
  fi
done

echo "Checking Sub2API health: ${sub2api_url}/health"
curl -fsS "${sub2api_url}/health" >/dev/null

echo "Checking Sub2API admin route: ${sub2api_admin_url}/"
curl -fsS -o /dev/null "${sub2api_admin_url}/"

echo "Checking New API status: ${newapi_url}/api/status"
curl -fsS "${newapi_url}/api/status" >/dev/null

echo "Checking New API to Sub2API internal reachability"
compose exec -T new-api \
  wget -q -O /dev/null http://sub2api:8080/health

if [[ -n "${SUB2API_TEST_API_KEY:-}" ]]; then
  echo "Checking Sub2API /v1/models with SUB2API_TEST_API_KEY"
  curl -fsS \
    -H "Authorization: Bearer ${SUB2API_TEST_API_KEY}" \
    "${sub2api_url}/v1/models" >/dev/null
else
  echo "Skipping Sub2API credentialed check; set SUB2API_TEST_API_KEY to enable"
fi

if [[ -n "${NEW_API_TEST_API_KEY:-}" ]]; then
  echo "Checking New API /v1/models with NEW_API_TEST_API_KEY"
  curl -fsS \
    -H "Authorization: Bearer ${NEW_API_TEST_API_KEY}" \
    "${newapi_url}/v1/models" >/dev/null
else
  echo "Skipping New API credentialed check; set NEW_API_TEST_API_KEY to enable"
fi

echo "Deployment verification checks completed"
