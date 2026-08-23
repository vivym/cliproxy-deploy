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

SUB2API_ADMIN_HOST="$(dotenv_value SUB2API_ADMIN_HOST)"
NEW_API_HOST="$(dotenv_value NEW_API_HOST)"
LARK_OAUTH_PUBLIC_ENABLED="$(dotenv_value LARK_OAUTH_PUBLIC_ENABLED true)"
env_new_api_test_key="$(dotenv_value NEW_API_TEST_API_KEY true)"
NEW_API_TEST_API_KEY="${NEW_API_TEST_API_KEY:-$env_new_api_test_key}"

sub2api_admin_url="https://${SUB2API_ADMIN_HOST:?set SUB2API_ADMIN_HOST}"
new_api_url="https://${NEW_API_HOST:?set NEW_API_HOST}"

compose() {
  local -a clean_env=(env -i "PATH=$PATH" "HOME=${HOME:-/nonexistent}")
  local docker_variable

  for docker_variable in DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG; do
    if [[ -n "${!docker_variable:-}" ]]; then
      clean_env+=("${docker_variable}=${!docker_variable}")
    fi
  done
  "${clean_env[@]}" docker compose "$@"
}

running_services="$(compose ps --services --filter status=running)"
for service in traefik sub2api-postgres sub2api-redis sub2api new-api-postgres new-api-redis new-api; do
  if ! printf '%s\n' "$running_services" | grep -qx "$service"; then
    echo "Required service is not running: $service" >&2
    exit 1
  fi
done

echo "Checking Sub2API admin route: ${sub2api_admin_url}/"
curl -fsS -o /dev/null "${sub2api_admin_url}/"

echo "Checking Sub2API /v1 is not publicly routed"
sub2api_v1_status="$(curl -sS -o /dev/null -w '%{http_code}' "${sub2api_admin_url}/v1/models")"
if [[ "$sub2api_v1_status" != "404" ]]; then
  echo "Sub2API /v1 must not be public; expected 404, got ${sub2api_v1_status}" >&2
  exit 1
fi

echo "Checking New API status: ${new_api_url}/api/status"
curl -fsS "${new_api_url}/api/status" >/dev/null

echo "Checking New API to Sub2API internal reachability"
compose exec -T new-api \
  wget -q -O /dev/null http://sub2api:8080/health

if printf '%s\n' "$running_services" | grep -qx "lark-quota-controller"; then
  echo "Checking Lark Controller readiness from New API"
  compose exec -T new-api \
    wget -q -O /dev/null http://lark-quota-controller:8080/readyz

  echo "Checking the New API integration route is absent from the public listener"
  public_integration_status="$(
    curl -sS -o /dev/null -w '%{http_code}' \
      "${new_api_url}/api/integrations/v1/principals"
  )"
  if [[ "$public_integration_status" != "404" ]]; then
    echo "New API integration route must not be public; expected 404, got ${public_integration_status}" >&2
    exit 1
  fi

  if [[ -z "$LARK_OAUTH_PUBLIC_ENABLED" || "$LARK_OAUTH_PUBLIC_ENABLED" == "false" ]]; then
    echo "Checking Lark OAuth routes are disabled during webhook-only shadow"
    oauth_status="$(
      curl -sS -o /dev/null -w '%{http_code}' \
        "${new_api_url}/integrations/lark/oauth/authorize"
    )"
    if [[ "$oauth_status" != "404" ]]; then
      echo "Lark OAuth routes are disabled but publicly reachable; expected 404, got ${oauth_status}" >&2
      exit 1
    fi
  else
    if [[ "$LARK_OAUTH_PUBLIC_ENABLED" != "true" ]]; then
      echo "LARK_OAUTH_PUBLIC_ENABLED must be true or false" >&2
      exit 1
    fi
    echo "Checking Lark OAuth authorize route is active"
    oauth_status="$(
      curl -sS -o /dev/null -w '%{http_code}' \
        "${new_api_url}/integrations/lark/oauth/authorize"
    )"
    if [[ "$oauth_status" != "400" ]]; then
      echo "Lark OAuth authorize route must reject an empty request; expected 400, got ${oauth_status}" >&2
      exit 1
    fi
  fi

  echo "Checking the internal New API listener rejects missing credentials"
  integration_headers="$(
    compose exec -T new-api \
      wget -S -O /dev/null -T 5 \
      http://127.0.0.1:3001/api/integrations/v1/principals 2>&1 || true
  )"
  if ! printf '%s\n' "$integration_headers" | grep -Eq 'HTTP/[0-9.]+ 401([[:space:]]|$)'; then
    echo "New API internal listener must reject missing credentials; expected 401" >&2
    exit 1
  fi
  echo "Lark integration verification checks completed"
else
  echo "Skipping Lark integration checks; lark-quota-controller is not running"
fi

if [[ -n "${NEW_API_TEST_API_KEY:-}" ]]; then
  echo "Checking New API /v1/models with NEW_API_TEST_API_KEY"
  curl -fsS \
    -H "Authorization: Bearer ${NEW_API_TEST_API_KEY}" \
    "${new_api_url}/v1/models" >/dev/null
else
  echo "Skipping New API credentialed check; set NEW_API_TEST_API_KEY to enable"
fi

echo "Deployment verification checks completed"
