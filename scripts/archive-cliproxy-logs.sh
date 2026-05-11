#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$repo_root"

if [[ -f .env ]]; then
  set -a
  source .env
  set +a
fi

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "Missing required environment variable: ${name}" >&2
    exit 1
  fi
}

require_command() {
  local name="$1"
  if ! command -v "$name" >/dev/null 2>&1; then
    echo "Missing required command: ${name}" >&2
    exit 1
  fi
}

is_open_file() {
  local path="$1"
  if command -v lsof >/dev/null 2>&1 && lsof -- "$path" >/dev/null 2>&1; then
    return 0
  fi
  return 1
}

require_env R2_ACCOUNT_ID
require_env R2_BUCKET
require_env R2_ACCESS_KEY_ID
require_env R2_SECRET_ACCESS_KEY
require_command aws
require_command gzip

log_dir="${CLIPROXY_LOG_ARCHIVE_DIR:-logs}"
case "$log_dir" in
  /*) ;;
  *) log_dir="${repo_root}/${log_dir}" ;;
esac
log_dir="${log_dir%/}"

if [[ ! -d "$log_dir" ]]; then
  echo "CLIProxyAPI log directory does not exist: ${log_dir}" >&2
  exit 1
fi

lock_file="${CLIPROXY_LOG_ARCHIVE_LOCK_FILE:-${log_dir}/.archive-cliproxy-logs.lock}"
if command -v flock >/dev/null 2>&1; then
  exec 9>"$lock_file"
  if ! flock -n 9; then
    echo "Another CLIProxyAPI log archive run is active; exiting" >&2
    exit 0
  fi
fi

min_age_minutes="${CLIPROXY_LOG_ARCHIVE_MIN_AGE_MINUTES:-30}"
delete_after_days="${CLIPROXY_LOG_ARCHIVE_DELETE_AFTER_DAYS:-1}"
gzip_level="${CLIPROXY_LOG_ARCHIVE_GZIP_LEVEL:-1}"
nice_level="${CLIPROXY_LOG_ARCHIVE_NICE:-19}"
ionice_idle="${CLIPROXY_LOG_ARCHIVE_IONICE_IDLE:-true}"
cpu_limit_percent="${CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT:-}"
r2_prefix="${CLIPROXY_LOG_ARCHIVE_R2_PREFIX:-cliproxy-logs}"
r2_prefix="${r2_prefix#/}"
r2_prefix="${r2_prefix%/}"
r2_endpoint="${R2_ENDPOINT_URL:-https://${R2_ACCOUNT_ID}.r2.cloudflarestorage.com}"
aws_region="${R2_REGION:-auto}"

if ! [[ "$min_age_minutes" =~ ^[0-9]+$ ]]; then
  echo "CLIPROXY_LOG_ARCHIVE_MIN_AGE_MINUTES must be a non-negative integer" >&2
  exit 1
fi

if ! [[ "$delete_after_days" =~ ^[0-9]+$ ]]; then
  echo "CLIPROXY_LOG_ARCHIVE_DELETE_AFTER_DAYS must be a non-negative integer" >&2
  exit 1
fi

if ! [[ "$gzip_level" =~ ^[1-9]$ ]]; then
  echo "CLIPROXY_LOG_ARCHIVE_GZIP_LEVEL must be an integer from 1 to 9" >&2
  exit 1
fi

if ! [[ "$nice_level" =~ ^[0-9]+$ ]] || ((nice_level > 19)); then
  echo "CLIPROXY_LOG_ARCHIVE_NICE must be an integer from 0 to 19" >&2
  exit 1
fi

case "$ionice_idle" in
  true|false) ;;
  *)
    echo "CLIPROXY_LOG_ARCHIVE_IONICE_IDLE must be true or false" >&2
    exit 1
    ;;
esac

if [[ -n "$cpu_limit_percent" ]]; then
  if ! [[ "$cpu_limit_percent" =~ ^[1-9][0-9]*$ ]] || ((cpu_limit_percent > 100)); then
    echo "CLIPROXY_LOG_ARCHIVE_CPU_LIMIT_PERCENT must be an integer from 1 to 100" >&2
    exit 1
  fi
  require_command cpulimit
fi

delete_after_minutes=$((delete_after_days * 24 * 60))

object_uri_for_file() {
  local file="$1"
  local rel="${file#"$log_dir"/}"
  local key
  if [[ -n "$r2_prefix" ]]; then
    key="${r2_prefix}/${rel}"
  else
    key="$rel"
  fi
  printf 's3://%s/%s\n' "$R2_BUCKET" "$key"
}

gzip_file() {
  local file="$1"
  local -a cmd
  cmd=(gzip "-${gzip_level}n" -- "$file")

  if [[ -n "$cpu_limit_percent" ]]; then
    cmd=(cpulimit -l "$cpu_limit_percent" -- "${cmd[@]}")
  fi

  if [[ "$nice_level" != "0" ]]; then
    require_command nice
    cmd=(nice -n "$nice_level" "${cmd[@]}")
  fi

  if [[ "$ionice_idle" == "true" ]] && command -v ionice >/dev/null 2>&1; then
    cmd=(ionice -c 3 "${cmd[@]}")
  fi

  "${cmd[@]}"
}

compress_old_logs() {
  find "$log_dir" -type f \
    ! -name '*.gz' \
    ! -name '*.uploaded' \
    -mmin +"$min_age_minutes" \
    -size +0 \
    -print0 |
    while IFS= read -r -d '' file; do
      if is_open_file "$file"; then
        echo "Skipping open log file: ${file}" >&2
        continue
      fi
      if [[ ! -e "$file" ]]; then
        echo "Skipping disappeared log file: ${file}" >&2
        continue
      fi
      if ! gzip_file "$file"; then
        if [[ ! -e "$file" ]]; then
          echo "Skipping disappeared log file: ${file}" >&2
          continue
        fi
        return 1
      fi
    done
}

upload_compressed_logs() {
  find "$log_dir" -type f -name '*.gz' -print0 |
    while IFS= read -r -d '' file; do
      marker="${file}.uploaded"
      if [[ -e "$marker" ]]; then
        continue
      fi
      if is_open_file "$file"; then
        echo "Skipping open compressed log file: ${file}" >&2
        continue
      fi
      uri="$(object_uri_for_file "$file")"
      AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID" \
        AWS_SECRET_ACCESS_KEY="$R2_SECRET_ACCESS_KEY" \
        AWS_DEFAULT_REGION="$aws_region" \
        aws s3 cp "$file" "$uri" --endpoint-url "$r2_endpoint"
      printf '%s\n' "$uri" > "$marker"
    done
}

delete_uploaded_local_copies() {
  find "$log_dir" -type f -name '*.gz.uploaded' -mmin +"$delete_after_minutes" -print0 |
    while IFS= read -r -d '' marker; do
      file="${marker%.uploaded}"
      if [[ -e "$file" ]]; then
        rm -f -- "$file"
      fi
      rm -f -- "$marker"
    done
}

compress_old_logs
upload_compressed_logs
delete_uploaded_local_copies
