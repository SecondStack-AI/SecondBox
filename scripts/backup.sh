#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'USAGE'
Usage: scripts/backup.sh

Creates a verified Sandbox recovery bundle containing the authoritative
PostgreSQL schema and opaque Sandbox Host workspace/checkpoint/artifact state.

Required environment:
  SANDBOX_BACKUP_DATABASE_URL
  SANDBOX_HOST_STATE_DIR
  SANDBOX_BACKUP_DIR
USAGE
}

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
    usage
    exit 0
fi
if [[ $# -ne 0 ]]; then
    usage >&2
    exit 2
fi

: "${SANDBOX_BACKUP_DATABASE_URL:?SANDBOX_BACKUP_DATABASE_URL is required}"
: "${SANDBOX_HOST_STATE_DIR:?SANDBOX_HOST_STATE_DIR is required}"
: "${SANDBOX_BACKUP_DIR:?SANDBOX_BACKUP_DIR is required}"

for command in pg_dump tar sha256sum jq flock; do
    command -v "$command" >/dev/null 2>&1 || {
        echo "$command is required" >&2
        exit 2
    }
done
if [[ ! -d "$SANDBOX_HOST_STATE_DIR" || -L "$SANDBOX_HOST_STATE_DIR" ]]; then
    echo "SANDBOX_HOST_STATE_DIR must be an existing non-symlink directory" >&2
    exit 2
fi
if [[ -L "$SANDBOX_BACKUP_DIR" ]]; then
    echo "SANDBOX_BACKUP_DIR must not be a symlink" >&2
    exit 2
fi
mkdir -p "$SANDBOX_BACKUP_DIR"
chmod 0700 "$SANDBOX_BACKUP_DIR"

exec 9>"$SANDBOX_BACKUP_DIR/.backup.lock"
flock -n 9 || {
    echo "another Sandbox backup is running" >&2
    exit 1
}

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
created_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
stage="$(mktemp -d "$SANDBOX_BACKUP_DIR/.stage-${timestamp}-XXXXXX")"
verify_stage="$(mktemp -d "$SANDBOX_BACKUP_DIR/.verify-${timestamp}-XXXXXX")"
trap 'rm -rf -- "$stage" "$verify_stage"' EXIT

pg_dump "$SANDBOX_BACKUP_DATABASE_URL" \
    --format=custom \
    --no-owner \
    --no-privileges \
    --schema=sandbox \
    --file="$stage/sandbox.dump"
tar --create --sparse --numeric-owner \
    --file="$stage/host-state.tar" \
    --directory="$SANDBOX_HOST_STATE_DIR" .

(
    cd "$stage"
    sha256sum sandbox.dump host-state.tar > checksums.sha256
)
database_sha256="$(awk '$2 == "sandbox.dump" {print $1}' "$stage/checksums.sha256")"
host_state_sha256="$(awk '$2 == "host-state.tar" {print $1}' "$stage/checksums.sha256")"
jq -n \
    --arg contractVersion "sandbox-backup.secondstack.ai/v1" \
    --arg createdAt "$created_at" \
    --arg databaseSHA256 "$database_sha256" \
    --arg hostStateSHA256 "$host_state_sha256" \
    '{
      contractVersion: $contractVersion,
      createdAt: $createdAt,
      database: {file: "sandbox.dump", schema: "sandbox", sha256: $databaseSHA256},
      hostState: {file: "host-state.tar", sha256: $hostStateSHA256}
    }' >"$stage/manifest.json"

bundle="$SANDBOX_BACKUP_DIR/sandbox-backup-${timestamp}.tar"
tar --create --numeric-owner --file="$bundle.tmp" \
    --directory="$stage" manifest.json checksums.sha256 sandbox.dump host-state.tar
mv "$bundle.tmp" "$bundle"
sha256sum "$bundle" >"$bundle.sha256"
chmod 0600 "$bundle" "$bundle.sha256"

tar --extract --file="$bundle" --directory="$verify_stage"
(
    cd "$verify_stage"
    sha256sum --check checksums.sha256
)
jq -e '.contractVersion == "sandbox-backup.secondstack.ai/v1"' \
    "$verify_stage/manifest.json" >/dev/null

printf 'Sandbox backup verified: %s\n' "$bundle"
