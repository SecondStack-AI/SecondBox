#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'USAGE'
Usage: scripts/restore-drill.sh [--keep-db]

Restores a Sandbox backup into a throwaway PostgreSQL database and temporary
Host state directory, then verifies the schema and every bundled checksum.

Required environment:
  SANDBOX_RESTORE_DATABASE_URL
  SANDBOX_RESTORE_BUNDLE
  SANDBOX_RESTORE_STAGE_DIR
USAGE
}

keep_db=false
while (($#)); do
    case "$1" in
        --keep-db) keep_db=true ;;
        --help|-h) usage; exit 0 ;;
        *) usage >&2; exit 2 ;;
    esac
    shift
done

: "${SANDBOX_RESTORE_DATABASE_URL:?SANDBOX_RESTORE_DATABASE_URL is required}"
: "${SANDBOX_RESTORE_BUNDLE:?SANDBOX_RESTORE_BUNDLE is required}"
: "${SANDBOX_RESTORE_STAGE_DIR:?SANDBOX_RESTORE_STAGE_DIR is required}"

for command in createdb dropdb pg_restore psql sed tar sha256sum jq python3; do
    command -v "$command" >/dev/null 2>&1 || {
        echo "$command is required" >&2
        exit 2
    }
done
if [[ ! -f "$SANDBOX_RESTORE_BUNDLE" || -L "$SANDBOX_RESTORE_BUNDLE" ]]; then
    echo "SANDBOX_RESTORE_BUNDLE must be a regular non-symlink file" >&2
    exit 2
fi
if [[ ! -f "$SANDBOX_RESTORE_BUNDLE.sha256" ]]; then
    echo "Sandbox bundle checksum sidecar is required" >&2
    exit 2
fi
if [[ ! -d "$SANDBOX_RESTORE_STAGE_DIR" || -L "$SANDBOX_RESTORE_STAGE_DIR" ]]; then
    echo "SANDBOX_RESTORE_STAGE_DIR must be an existing non-symlink directory" >&2
    exit 2
fi

sha256sum --check "$SANDBOX_RESTORE_BUNDLE.sha256"
while IFS= read -r member; do
    case "$member" in
        /*|../*|*/../*|*/..) echo "unsafe bundle member: $member" >&2; exit 1 ;;
    esac
done < <(tar --list --file="$SANDBOX_RESTORE_BUNDLE")

stage="$(mktemp -d "$SANDBOX_RESTORE_STAGE_DIR/sandbox-restore-XXXXXX")"
db_name="sandbox_restore_drill_$(date -u +%Y%m%dT%H%M%SZ)_$$"
cleanup() {
    rm -rf -- "$stage"
    if [[ "$keep_db" != true && "${database_created:-false}" == true ]]; then
        dropdb --if-exists --maintenance-db="$SANDBOX_RESTORE_DATABASE_URL" "$db_name"
    fi
}
trap cleanup EXIT

tar --extract --no-same-owner --file="$SANDBOX_RESTORE_BUNDLE" --directory="$stage"
(
    cd "$stage"
    sha256sum --check checksums.sha256
)
jq -e '.contractVersion == "sandbox-backup.secondstack.ai/v1"' \
    "$stage/manifest.json" >/dev/null

createdb --maintenance-db="$SANDBOX_RESTORE_DATABASE_URL" "$db_name"
database_created=true
target_url="$(python3 - "$SANDBOX_RESTORE_DATABASE_URL" "$db_name" <<'PY'
import sys
from urllib.parse import urlsplit, urlunsplit

parts = urlsplit(sys.argv[1])
if parts.scheme not in ("postgres", "postgresql"):
    raise SystemExit("SANDBOX_RESTORE_DATABASE_URL must be a URL-style PostgreSQL DSN")
print(urlunsplit((parts.scheme, parts.netloc, "/" + sys.argv[2], parts.query, parts.fragment)))
PY
)"
pg_restore --no-owner --no-privileges --file=- "$stage/sandbox.dump" \
    | sed '/^SET transaction_timeout = /d' \
    | psql "$target_url" --set ON_ERROR_STOP=1
schema_exists="$(psql "$target_url" --tuples-only --no-align \
    --command="SELECT to_regnamespace('sandbox') IS NOT NULL")"
if [[ "$schema_exists" != "t" ]]; then
    echo "restored database does not contain the sandbox schema" >&2
    exit 1
fi

mkdir "$stage/host-state"
while IFS= read -r member; do
    case "$member" in
        /*|../*|*/../*|*/..) echo "unsafe Host state member: $member" >&2; exit 1 ;;
    esac
done < <(tar --list --file="$stage/host-state.tar")
tar --extract --no-same-owner --file="$stage/host-state.tar" \
    --directory="$stage/host-state"
printf 'Sandbox restore drill passed: database=%s host_state=%s\n' \
    "$db_name" "$stage/host-state"
if [[ "$keep_db" == true ]]; then
    printf 'Throwaway database retained: %s\n' "$db_name"
fi
