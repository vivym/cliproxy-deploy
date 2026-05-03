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

echo "Checking CLIProxyAPI public host is blocked: ${CLIPROXY_PUBLIC_HOST:?set CLIPROXY_PUBLIC_HOST}"
set +e
http_code="$(curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 5 "https://${CLIPROXY_PUBLIC_HOST}/v1/models")"
curl_exit=$?
set -e

if [[ "$http_code" != "000" ]]; then
  echo "CLIProxyAPI must not be publicly reachable: ${CLIPROXY_PUBLIC_HOST} returned HTTP ${http_code} (curl exit code ${curl_exit})" >&2
  exit 1
fi

case "$curl_exit" in
  7)
    # Expected: host resolves, but no service accepts the public connection.
    ;;
  28)
    # Expected: host resolves, but the public path times out without an HTTP response.
    ;;
  *)
    echo "CLIProxyAPI public exposure check did not reliably verify ${CLIPROXY_PUBLIC_HOST}: curl exit code ${curl_exit}, HTTP ${http_code}" >&2
    exit 1
    ;;
esac

echo "API-site verification checks completed"
