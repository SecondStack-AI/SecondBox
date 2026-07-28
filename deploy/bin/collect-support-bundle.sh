#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ "$#" -ne 1 ]]; then
  echo "Usage: deploy/bin/collect-support-bundle.sh OUTPUT.tar.gz" >&2
  exit 2
fi

output_path="$1"
if [[ -e "$output_path" ]]; then
  echo "Refusing to overwrite support bundle: $output_path" >&2
  exit 1
fi

: "${SECONDBOX_SUPPORT_BASE_URL:?set SECONDBOX_SUPPORT_BASE_URL}"
: "${SECONDBOX_SUPPORT_CONTROL_PLANE_LOG:?set SECONDBOX_SUPPORT_CONTROL_PLANE_LOG}"
: "${SECONDBOX_SUPPORT_MAX_LOG_BYTES:?set SECONDBOX_SUPPORT_MAX_LOG_BYTES}"
: "${SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS:?set SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS}"

if [[ ! "$SECONDBOX_SUPPORT_MAX_LOG_BYTES" =~ ^[0-9]+$ ]] ||
   (( SECONDBOX_SUPPORT_MAX_LOG_BYTES < 1 || SECONDBOX_SUPPORT_MAX_LOG_BYTES > 104857600 )); then
  echo "SECONDBOX_SUPPORT_MAX_LOG_BYTES must be from 1 through 104857600" >&2
  exit 1
fi
if [[ ! "$SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] ||
   (( SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS < 1 || SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS > 60 )); then
  echo "SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS must be from 1 through 60" >&2
  exit 1
fi

working_directory="$(mktemp -d)"
trap 'rm -rf "$working_directory"' EXIT

probe_endpoint() {
  local endpoint="$1"
  local output_file="$2"
  local status_file="$3"
  local status_code

  if status_code="$(curl \
    --silent \
    --show-error \
    --max-time "$SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS" \
    --output "$output_file" \
    --write-out '%{http_code}' \
    "$SECONDBOX_SUPPORT_BASE_URL/$endpoint")"; then
    printf '%s\n' "$status_code" >"$status_file"
  else
    printf 'transport_error\n' >"$status_file"
  fi
}

probe_endpoint healthz "$working_directory/healthz.body" "$working_directory/healthz.status"
probe_endpoint readyz "$working_directory/readyz.body" "$working_directory/readyz.status"
probe_endpoint metrics "$working_directory/metrics.body" "$working_directory/metrics.status"

if [[ -f "$SECONDBOX_SUPPORT_CONTROL_PLANE_LOG" ]]; then
  tail -c "$SECONDBOX_SUPPORT_MAX_LOG_BYTES" \
    "$SECONDBOX_SUPPORT_CONTROL_PLANE_LOG" >"$working_directory/control-plane.log.tail"
else
  printf 'configured log file is unavailable\n' >"$working_directory/control-plane.log.status"
fi

(
  cd "$working_directory"
  find . -maxdepth 1 -type f ! -name SHA256SUMS -printf '%P\n' |
    sort |
    xargs -r sha256sum >SHA256SUMS
)
tar -C "$working_directory" -czf "$output_path" .
echo "Created bounded support bundle: $output_path"
