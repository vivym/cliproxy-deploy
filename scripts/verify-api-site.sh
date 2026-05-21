#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [[ -f .env ]]; then
  set -a
  source .env
  set +a
fi

ai_host="${AI_HOST:-ai.x2r.store}"
base_url="https://${ai_host}"
cliproxy_host="${CLIPROXY_HOST:-cliproxy.x2r.store}"
cliproxy_url="https://${cliproxy_host}"
keeper_host="${CPA_USAGE_KEEPER_HOST:-keeper.x2r.store}"
keeper_url="https://${keeper_host}"

echo "Checking New API public endpoint: ${base_url}"
curl -fsS -I "${base_url}" >/dev/null

if [[ -n "${NEW_API_TEST_API_KEY:-}" ]]; then
  echo "Checking /v1/models with NEW_API_TEST_API_KEY"
  curl -fsS \
    -H "Authorization: Bearer ${NEW_API_TEST_API_KEY}" \
    "${base_url}/v1/models" >/dev/null
else
  echo "Skipping /v1/models credentialed check; set NEW_API_TEST_API_KEY to enable"
fi

if [[ -n "${CODEX_TEST_API_KEY:-}" ]]; then
  echo "Checking /v1/responses with CODEX_TEST_API_KEY"
  curl -fsS \
    -H "Authorization: Bearer ${CODEX_TEST_API_KEY}" \
    -H "Content-Type: application/json" \
    "${base_url}/v1/responses" \
    -d '{"model":"codex-cli","input":"Reply with ok.","store":false}' >/dev/null
else
  echo "Skipping /v1/responses check; set CODEX_TEST_API_KEY to enable"
fi

echo "Checking New API container can reach internal CLIProxyAPI"
docker compose exec -T new-api sh -lc \
  "wget -qO- --header='Authorization: Bearer ${CLIPROXY_INTERNAL_API_KEY:?set CLIPROXY_INTERNAL_API_KEY}' http://cliproxyapi:8317/v1/models >/dev/null"

echo "Checking CLIProxyAPI public endpoint: ${cliproxy_url}"
curl -fsS \
  -H "Authorization: Bearer ${CLIPROXY_INTERNAL_API_KEY}" \
  "${cliproxy_url}/v1/models" >/dev/null
curl -fsS "${cliproxy_url}/management.html" >/dev/null

echo "Checking CPA Usage Keeper public endpoint: ${keeper_url}"
curl -fsS "${keeper_url}/healthz" >/dev/null

echo "Checking CPA Usage Keeper requires authentication"
set +e
http_code="$(curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 5 "${keeper_url}/api/v1/usage/overview")"
curl_exit=$?
set -e

if [[ "$curl_exit" != "0" || "$http_code" != "401" ]]; then
  echo "CPA Usage Keeper must require authentication: ${keeper_url} returned HTTP ${http_code} (curl exit code ${curl_exit})" >&2
  exit 1
fi

echo "API-site verification checks completed"
