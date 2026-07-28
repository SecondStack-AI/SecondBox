#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ "$#" -ne 1 ]]; then
  echo "Usage: deploy/bin/export-audit.sh OUTPUT.jsonl" >&2
  exit 2
fi

output_path="$1"
if [[ -e "$output_path" ]]; then
  echo "Refusing to overwrite audit export: $output_path" >&2
  exit 1
fi

: "${SECONDBOX_AUDIT_DATABASE_URL:?set SECONDBOX_AUDIT_DATABASE_URL}"
: "${SECONDBOX_AUDIT_LIMIT:?set SECONDBOX_AUDIT_LIMIT}"
: "${SECONDBOX_AUDIT_CONNECT_TIMEOUT_SECONDS:?set SECONDBOX_AUDIT_CONNECT_TIMEOUT_SECONDS}"

if [[ ! "$SECONDBOX_AUDIT_LIMIT" =~ ^[0-9]+$ ]] ||
   (( SECONDBOX_AUDIT_LIMIT < 1 || SECONDBOX_AUDIT_LIMIT > 10000 )); then
  echo "SECONDBOX_AUDIT_LIMIT must be from 1 through 10000" >&2
  exit 1
fi
if [[ ! "$SECONDBOX_AUDIT_CONNECT_TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] ||
   (( SECONDBOX_AUDIT_CONNECT_TIMEOUT_SECONDS < 1 || SECONDBOX_AUDIT_CONNECT_TIMEOUT_SECONDS > 60 )); then
  echo "SECONDBOX_AUDIT_CONNECT_TIMEOUT_SECONDS must be from 1 through 60" >&2
  exit 1
fi

temporary_path="$(mktemp "${output_path}.tmp.XXXXXX")"
trap 'rm -f "$temporary_path"' EXIT
PGCONNECT_TIMEOUT="$SECONDBOX_AUDIT_CONNECT_TIMEOUT_SECONDS" \
  psql "$SECONDBOX_AUDIT_DATABASE_URL" \
  --no-psqlrc \
  --set ON_ERROR_STOP=1 \
  --tuples-only \
  --no-align \
  --command "
    SELECT row_to_json(event)
    FROM (
      SELECT id, project_id, actor_kind, actor_id, action, resource_kind,
             resource_id, outcome, request_id, details_json, created_at
      FROM secondbox.audit_events
      ORDER BY created_at DESC, id DESC
      LIMIT $SECONDBOX_AUDIT_LIMIT
    ) AS event
    ORDER BY event.created_at, event.id
  " >"$temporary_path"
chmod 600 "$temporary_path"
mv "$temporary_path" "$output_path"
trap - EXIT
echo "Exported bounded audit evidence: $output_path"
