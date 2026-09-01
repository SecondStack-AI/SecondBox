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
: "${SECONDBOX_SUPPORT_MAX_PROBE_BYTES:?set SECONDBOX_SUPPORT_MAX_PROBE_BYTES}"
: "${SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS:?set SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS}"
: "${SECONDBOX_SUPPORT_TIMING_WINDOW_SECONDS:?set SECONDBOX_SUPPORT_TIMING_WINDOW_SECONDS}"
: "${SECONDBOX_SUPPORT_PLATFORM_TOKEN:?set SECONDBOX_SUPPORT_PLATFORM_TOKEN}"

if [[ ! "$SECONDBOX_SUPPORT_MAX_LOG_BYTES" =~ ^[0-9]+$ ]] ||
   (( SECONDBOX_SUPPORT_MAX_LOG_BYTES < 1 || SECONDBOX_SUPPORT_MAX_LOG_BYTES > 104857600 )); then
  echo "SECONDBOX_SUPPORT_MAX_LOG_BYTES must be from 1 through 104857600" >&2
  exit 1
fi
if [[ ! "$SECONDBOX_SUPPORT_MAX_PROBE_BYTES" =~ ^[0-9]+$ ]] ||
   (( SECONDBOX_SUPPORT_MAX_PROBE_BYTES < 1 || SECONDBOX_SUPPORT_MAX_PROBE_BYTES > 10485760 )); then
  echo "SECONDBOX_SUPPORT_MAX_PROBE_BYTES must be from 1 through 10485760" >&2
  exit 1
fi
if [[ ! "$SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] ||
   (( SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS < 1 || SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS > 60 )); then
  echo "SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS must be from 1 through 60" >&2
  exit 1
fi
if [[ ! "$SECONDBOX_SUPPORT_TIMING_WINDOW_SECONDS" =~ ^[0-9]+$ ]] ||
   (( SECONDBOX_SUPPORT_TIMING_WINDOW_SECONDS < 60 || SECONDBOX_SUPPORT_TIMING_WINDOW_SECONDS > 3600 )); then
  echo "SECONDBOX_SUPPORT_TIMING_WINDOW_SECONDS must be from 60 through 3600" >&2
  exit 1
fi

working_directory="$(mktemp -d)"
trap 'rm -rf "$working_directory"' EXIT

probe_endpoint() {
  local endpoint="$1"
  local output_file="$2"
  local status_file="$3"
  local token="${4:-}"
  local status_code
  local -a authorization=()

  if [[ -n "$token" ]]; then
    authorization=(
      --header "Authorization: Bearer $token"
      --header "X-SecondBox-Tenant-Ref: secondbox"
      --header "X-SecondBox-Subject-Ref: secondbox-admin"
    )
  fi

  if status_code="$(curl \
    --silent \
    --show-error \
    --max-time "$SECONDBOX_SUPPORT_HTTP_TIMEOUT_SECONDS" \
    --max-filesize "$SECONDBOX_SUPPORT_MAX_PROBE_BYTES" \
    "${authorization[@]}" \
    --output "$output_file" \
    --write-out '%{http_code}' \
    "$SECONDBOX_SUPPORT_BASE_URL/$endpoint")"; then
    printf '%s\n' "$status_code" >"$status_file"
  else
    printf 'transport_or_bound_error\n' >"$status_file"
  fi
}

probe_endpoint healthz "$working_directory/healthz.body" "$working_directory/healthz.status"
probe_endpoint readyz "$working_directory/readyz.body" "$working_directory/readyz.status"
probe_endpoint metrics "$working_directory/metrics.body" "$working_directory/metrics.status"
probe_endpoint \
  "v1/timings?windowSeconds=$SECONDBOX_SUPPORT_TIMING_WINDOW_SECONDS" \
  "$working_directory/timing-summary.json" \
  "$working_directory/timing-summary.status" \
  "$SECONDBOX_SUPPORT_PLATFORM_TOKEN"
probe_endpoint \
  "v1/diagnostics/egress-contexts" \
  "$working_directory/egress-context-preflight.json" \
  "$working_directory/egress-context-preflight.status" \
  "$SECONDBOX_SUPPORT_PLATFORM_TOKEN"

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
