#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Usage: scripts/verify-lark-secret-permissions.sh [--include-next] [--include-correction] [SECRETS_DIR] [OWNER_UID:OWNER_GID]" >&2
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi
include_correction=false
include_next=false
while [[ "${1:-}" == --* ]]; do
  case "$1" in
    --include-correction)
      include_correction=true
      ;;
    --include-next)
      include_next=true
      ;;
    *)
      usage
      exit 1
      ;;
  esac
  shift
done
if [[ $# -gt 2 ]]; then
  usage
  exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
secrets_dir="${1:-${repo_root}/lark-runtime/secrets}"
expected_owner="${2:-10001:10001}"

if [[ ! "$expected_owner" =~ ^[0-9]+:[0-9]+$ ]]; then
  echo "OWNER_UID:OWNER_GID must be numeric: $expected_owner" >&2
  exit 1
fi
if [[ ! -d "$secrets_dir" ]]; then
  echo "Lark secrets directory does not exist: $secrets_dir" >&2
  exit 1
fi
secrets_dir="$(cd "$secrets_dir" && pwd -P)"

file_mode() {
  local path="$1"

  if stat -c '%a' "$path" >/dev/null 2>&1; then
    stat -c '%a' "$path"
  else
    stat -f '%Lp' "$path"
  fi
}

file_owner() {
  local path="$1"

  if stat -c '%u:%g' "$path" >/dev/null 2>&1; then
    stat -c '%u:%g' "$path"
  else
    stat -f '%u:%g' "$path"
  fi
}

verify_path() {
  local path="$1"
  local expected_mode="$2"
  local actual_mode
  local actual_owner

  if [[ ! -e "$path" ]]; then
    echo "Missing required Lark secret path: $path" >&2
    return 1
  fi
  actual_mode="$(file_mode "$path")"
  actual_owner="$(file_owner "$path")"
  if [[ "$actual_mode" != "$expected_mode" ]]; then
    echo "Unsafe mode for $path: expected $expected_mode, got $actual_mode" >&2
    return 1
  fi
  if [[ "$actual_owner" != "$expected_owner" ]]; then
    echo "Unreadable owner for $path: expected $expected_owner, got $actual_owner" >&2
    return 1
  fi
}

verify_secret_format() {
  local path="$1"
  local label="$2"

  if [[ ! -r "$path" ]]; then
    echo "$label is not readable by the verifier; run as $expected_owner or root" >&2
    exit 1
  fi
  if ! python3 - "$path" <<'PY'
import pathlib
import sys

contents = pathlib.Path(sys.argv[1]).read_bytes()
if contents.endswith(b"\r\n"):
    token = contents[:-2]
elif contents.endswith(b"\n"):
    token = contents[:-1]
else:
    token = contents

if len(token) < 32 or any(byte < 0x21 or byte > 0x7E for byte in token):
    raise SystemExit(1)
PY
  then
    echo "$label must be one printable ASCII token of at least 32 bytes with at most one LF/CRLF terminator" >&2
    exit 1
  fi
}

verify_distinct_secret() {
  local first="$1"
  local second="$2"
  local label="$3"

  if ! python3 - "$first" "$second" <<'PY'
import pathlib
import sys

def effective_token(path):
    contents = pathlib.Path(path).read_bytes()
    if contents.endswith(b"\r\n"):
        return contents[:-2]
    if contents.endswith(b"\n"):
        return contents[:-1]
    return contents

raise SystemExit(0 if effective_token(sys.argv[1]) != effective_token(sys.argv[2]) else 1)
PY
  then
    echo "$label credentials must be independent" >&2
    exit 1
  fi
}

for directory in shared controller new-api; do
  verify_path "${secrets_dir}/${directory}" 700
done

for secret_file in \
  shared/lark_integration_secret \
  controller/lark_app_secret \
  controller/lark_verification_token \
  controller/lark_encrypt_key \
  controller/lark_grant_payload_keyring \
  controller/new_api_bridge_client_secret; do
  verify_path "${secrets_dir}/${secret_file}" 600
done

current_secret="${secrets_dir}/shared/lark_integration_secret"
next_secret="${secrets_dir}/shared/lark_integration_secret_next"
verify_secret_format "$current_secret" "Current integration secret"
if [[ "$include_next" == "true" || -e "$next_secret" ]]; then
  verify_path "$next_secret" 600
  verify_secret_format "$next_secret" "Next integration secret"
  verify_distinct_secret "$current_secret" "$next_secret" \
    "Current and next integration"
fi

if [[ "$include_correction" == "true" ]]; then
  correction_secret="${secrets_dir}/new-api/lark_correction_secret"
  verify_path "$correction_secret" 600
  verify_secret_format "$correction_secret" "Correction secret"
  verify_distinct_secret "$current_secret" "$correction_secret" \
    "Correction and current integration"
  if [[ -e "$next_secret" ]]; then
    verify_distinct_secret "$next_secret" "$correction_secret" \
      "Correction and next integration"
  fi
fi

echo "Lark secret ownership and modes verified"
