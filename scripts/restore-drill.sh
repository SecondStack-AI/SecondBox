#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
  cat <<'USAGE'
Usage: scripts/restore-drill.sh [--keep-db]

Restores a SecondBox recovery bundle into isolated PostgreSQL and object-state
targets, compares restored database roots with restored object bytes, and then
requires detailed fresh-Runner portability evidence.

Required environment:
  SECONDBOX_RESTORE_DATABASE_URL
  SECONDBOX_RESTORE_BUNDLE
  SECONDBOX_RESTORE_STAGE_DIR
  SECONDBOX_RESTORE_OBJECT_TARGET
  SECONDBOX_RESTORE_CONTROL_PLANE_URL
  SECONDBOX_RESTORE_CONTROL_PLANE_TOKEN
  SECONDBOX_RESTORE_TENANT_REF
  SECONDBOX_RESTORE_SUBJECT_REF
  SECONDBOX_RESTORE_FRESH_RUNNER_RESULT
  SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_COMMAND
  SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS
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

: "${SECONDBOX_RESTORE_DATABASE_URL:?set SECONDBOX_RESTORE_DATABASE_URL}"
: "${SECONDBOX_RESTORE_BUNDLE:?set SECONDBOX_RESTORE_BUNDLE}"
: "${SECONDBOX_RESTORE_STAGE_DIR:?set SECONDBOX_RESTORE_STAGE_DIR}"
: "${SECONDBOX_RESTORE_OBJECT_TARGET:?set SECONDBOX_RESTORE_OBJECT_TARGET}"
: "${SECONDBOX_RESTORE_CONTROL_PLANE_URL:?set SECONDBOX_RESTORE_CONTROL_PLANE_URL}"
: "${SECONDBOX_RESTORE_CONTROL_PLANE_TOKEN:?set SECONDBOX_RESTORE_CONTROL_PLANE_TOKEN}"
: "${SECONDBOX_RESTORE_TENANT_REF:?set SECONDBOX_RESTORE_TENANT_REF}"
: "${SECONDBOX_RESTORE_SUBJECT_REF:?set SECONDBOX_RESTORE_SUBJECT_REF}"
: "${SECONDBOX_RESTORE_FRESH_RUNNER_RESULT:?set SECONDBOX_RESTORE_FRESH_RUNNER_RESULT}"
: "${SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_COMMAND:?set SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_COMMAND}"
: "${SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS:?set SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS}"

if [[ ! "$SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS" =~ ^[1-9][0-9]{0,2}$ ]] ||
   ((SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS > 600)); then
  echo "SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS must be between 1 and 600" >&2
  exit 2
fi

for required_command in createdb curl dropdb pg_restore psql sed tar sha256sum jq python3 mktemp cmp; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox restore drill requires command: $required_command" >&2
    exit 2
  fi
done
for file in "$SECONDBOX_RESTORE_BUNDLE" "$SECONDBOX_RESTORE_BUNDLE.sha256"; do
  if [[ -L "$file" || ! -f "$file" ]]; then
    echo "Restore input must be a regular non-symbolic-link file: $file" >&2
    exit 2
  fi
done
if [[ ! -x "$SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_COMMAND" ]]; then
  echo "SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_COMMAND must be an executable file" >&2
  exit 2
fi
if [[ -L "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT" ||
      -e "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT" ||
      ! -d "$(dirname "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT")" ]]; then
  echo "SECONDBOX_RESTORE_FRESH_RUNNER_RESULT must be a new file in an existing directory" >&2
  exit 2
fi
for directory in "$SECONDBOX_RESTORE_STAGE_DIR" "$(dirname "$SECONDBOX_RESTORE_OBJECT_TARGET")"; do
  if [[ -L "$directory" || ! -d "$directory" ]]; then
    echo "Restore parent must be an existing non-symbolic-link directory: $directory" >&2
    exit 2
  fi
done
if [[ -e "$SECONDBOX_RESTORE_OBJECT_TARGET" ]]; then
  echo "SECONDBOX_RESTORE_OBJECT_TARGET must not already exist" >&2
  exit 2
fi

python3 - "$SECONDBOX_RESTORE_BUNDLE.sha256" "$(basename "$SECONDBOX_RESTORE_BUNDLE")" <<'PY'
import pathlib
import re
import sys

sidecar = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
expected = rf"[0-9a-f]{{64}}  {re.escape(sys.argv[2])}\n"
if re.fullmatch(expected, sidecar) is None:
    raise SystemExit("Recovery bundle checksum sidecar is not a portable single-file manifest")
PY
(
  cd "$(dirname "$SECONDBOX_RESTORE_BUNDLE")"
  sha256sum --check "$(basename "$SECONDBOX_RESTORE_BUNDLE").sha256"
)
python3 - "$SECONDBOX_RESTORE_BUNDLE" <<'PY'
import pathlib
import sys
import tarfile

expected = {
    "manifest.json",
    "checksums.sha256",
    "secondbox.dump",
    "object-state.tar",
    "database-state.json",
    "publication-fence.json",
    "quiescence.json",
    "fencing.json",
    "checkpoint-reachability.json",
}
with tarfile.open(sys.argv[1], "r:") as archive:
    members = archive.getmembers()
    names = {member.name.removeprefix("./") for member in members}
    if names != expected:
        raise SystemExit("Recovery bundle contains unexpected or missing members")
    for member in members:
        path = pathlib.PurePosixPath(member.name)
        if path.is_absolute() or ".." in path.parts or not member.isfile():
            raise SystemExit(f"Unsafe recovery bundle member: {member.name}")
PY

stage="$(mktemp -d "$SECONDBOX_RESTORE_STAGE_DIR/secondbox-restore-XXXXXX")"
database_name="secondbox_restore_drill_$(date -u +%Y%m%dT%H%M%SZ)_$$"
fresh_runner_verifier_pid=""
fresh_runner_verifier_running=false

stop_fresh_runner_verifier() {
  if [[ "$fresh_runner_verifier_running" != true ]]; then
    return 0
  fi
  local verifier_status=0
  local verifier_watchdog_pid=""
  local verifier_forced_marker="$stage/fresh-runner-verifier-forced-stop"
  if kill -0 "$fresh_runner_verifier_pid" 2>/dev/null; then
    if ! kill -TERM "$fresh_runner_verifier_pid"; then
      echo "Failed to stop fresh-Runner restore verifier" >&2
      verifier_status=1
    fi
    (
      sleep "$SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS"
      if kill -0 "$fresh_runner_verifier_pid" 2>/dev/null; then
        : >"$verifier_forced_marker"
        kill -KILL "$fresh_runner_verifier_pid" 2>/dev/null || true
      fi
    ) &
    verifier_watchdog_pid="$!"
  fi
  if ! wait "$fresh_runner_verifier_pid"; then
    echo "Fresh-Runner restore verifier exited unsuccessfully" >&2
    verifier_status=1
  fi
  if [[ -n "$verifier_watchdog_pid" ]]; then
    kill -TERM "$verifier_watchdog_pid" 2>/dev/null || true
    wait "$verifier_watchdog_pid" 2>/dev/null || true
  fi
  if [[ -f "$verifier_forced_marker" ]]; then
    echo "Fresh-Runner restore verifier ignored its bounded termination request" >&2
    verifier_status=1
  fi
  fresh_runner_verifier_running=false
  return "$verifier_status"
}

cleanup() {
  local status="$?"
  if ! stop_fresh_runner_verifier; then
    status=1
  fi
  if ! rm -rf -- "$stage"; then
    echo "Failed to remove SecondBox restore staging data: $stage" >&2
    status=1
  fi
  if [[ "$keep_db" != true && "${database_created:-false}" == true ]]; then
    if ! dropdb --if-exists --maintenance-db="$SECONDBOX_RESTORE_DATABASE_URL" "$database_name"; then
      echo "Failed to remove restore-drill database: $database_name" >&2
      status=1
    fi
  fi
  if [[ "${object_target_created:-false}" == true && "${restore_succeeded:-false}" != true ]]; then
    if ! rm -rf -- "$SECONDBOX_RESTORE_OBJECT_TARGET"; then
      echo "Failed to remove incomplete object restore: $SECONDBOX_RESTORE_OBJECT_TARGET" >&2
      status=1
    fi
  fi
  exit "$status"
}
trap cleanup EXIT

tar --extract --no-same-owner --file="$SECONDBOX_RESTORE_BUNDLE" --directory="$stage"
(
  cd "$stage"
  sha256sum --check checksums.sha256
)
recovery_point_id="$(
  jq -er '
    select(.contractVersion == "secondbox-backup/v2") |
    select(.database.schema == "secondbox") |
    select(.publicationFence.state == "database-share-lock-held") |
    select(.checkpointReachability.state == "database-verified") |
    select(.freshRunnerVerification.state == "required-after-restore") |
    .recoveryPointId
  ' "$stage/manifest.json"
)"
backup_created_at="$(jq -er '.createdAt' "$stage/manifest.json")"
if [[ ! "$backup_created_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]; then
  echo "Recovery manifest createdAt must be a UTC timestamp" >&2
  exit 1
fi
jq -e \
  --arg recoveryPointId "$recovery_point_id" \
  '.contractVersion == "secondbox-backup-publication-fence/v1" and
   .recoveryPointId == $recoveryPointId and
   .state == "database-share-lock-held" and
   .schema == "secondbox" and
   .lockMode == "SHARE"' \
  "$stage/publication-fence.json" >/dev/null
jq -e \
  --arg recoveryPointId "$recovery_point_id" \
  '.contractVersion == "secondbox-checkpoint-reachability/v1" and
   .recoveryPointId == $recoveryPointId and
   .state == "verified" and
   (.objects | type == "array")' \
  "$stage/checkpoint-reachability.json" >/dev/null
jq -e '
  .contractVersion == "secondbox-backup-database-state/v1" and
  (.objects | type == "array")
' "$stage/database-state.json" >/dev/null
if ! jq --sort-keys '.objects' "$stage/database-state.json" |
  cmp --silent - <(jq --sort-keys '.objects' "$stage/checkpoint-reachability.json"); then
  echo "Backup database state and checkpoint reachability roots differ" >&2
  exit 1
fi

createdb --maintenance-db="$SECONDBOX_RESTORE_DATABASE_URL" "$database_name"
database_created=true
target_url="$(python3 - "$SECONDBOX_RESTORE_DATABASE_URL" "$database_name" <<'PY'
import sys
from urllib.parse import urlsplit, urlunsplit

parts = urlsplit(sys.argv[1])
if parts.scheme not in ("postgres", "postgresql"):
    raise SystemExit("SECONDBOX_RESTORE_DATABASE_URL must be a URL-style PostgreSQL DSN")
print(urlunsplit((parts.scheme, parts.netloc, "/" + sys.argv[2], parts.query, parts.fragment)))
PY
)"
pg_restore --no-owner --no-privileges --file=- "$stage/secondbox.dump" \
  | sed '/^SET transaction_timeout = /d' \
  | psql "$target_url" --set ON_ERROR_STOP=1
schema_exists="$(psql "$target_url" --tuples-only --no-align \
  --command="SELECT to_regnamespace('secondbox') IS NOT NULL")"
if [[ "$schema_exists" != "t" ]]; then
  echo "Restored database does not contain the secondbox schema" >&2
  exit 1
fi

install -d -m 700 "$SECONDBOX_RESTORE_OBJECT_TARGET"
object_target_created=true
python3 - "$stage/object-state.tar" <<'PY'
import pathlib
import sys
import tarfile

with tarfile.open(sys.argv[1], "r:") as archive:
    for member in archive.getmembers():
        path = pathlib.PurePosixPath(member.name)
        if path.is_absolute() or ".." in path.parts:
            raise SystemExit(f"Unsafe object-state member: {member.name}")
        if not member.isfile() and not member.isdir():
            raise SystemExit(f"Object-state member is not a regular file or directory: {member.name}")
PY
tar --extract --no-same-owner \
  --file="$stage/object-state.tar" \
  --directory="$SECONDBOX_RESTORE_OBJECT_TARGET"

python3 - "$SECONDBOX_RESTORE_OBJECT_TARGET" "$stage/checkpoint-reachability.json" <<'PY'
import hashlib
import json
import os
import pathlib
import re
import stat
import sys

root = pathlib.Path(sys.argv[1])
with open(sys.argv[2], encoding="utf-8") as stream:
    document = json.load(stream)

expected = {}
for item in document["objects"]:
    if (
        item.get("kind") not in {"checkpoint", "artifact"}
        or not isinstance(item.get("id"), str)
        or not item["id"]
        or not isinstance(item.get("storageObjectId"), str)
        or not re.fullmatch(r"[0-9a-f]{64}", item.get("sha256", ""))
        or not isinstance(item.get("sizeBytes"), int)
        or item["sizeBytes"] < 0
    ):
        raise SystemExit("Restored object manifest contains an invalid entry")
    relative = pathlib.PurePosixPath(item["storageObjectId"])
    if relative.is_absolute() or ".." in relative.parts or str(relative) in {"", "."}:
        raise SystemExit(f"Unsafe restored storage object identifier: {relative}")
    key = relative.as_posix()
    if key in expected:
        raise SystemExit(f"Duplicate restored storage object identifier: {key}")
    expected[key] = item

actual = {}
for directory, directory_names, file_names in os.walk(root, followlinks=False):
    for name in directory_names:
        path = pathlib.Path(directory, name)
        if path.is_symlink():
            raise SystemExit(f"Restored object tree contains a symbolic link: {path}")
    for name in file_names:
        path = pathlib.Path(directory, name)
        metadata = path.lstat()
        if not stat.S_ISREG(metadata.st_mode):
            raise SystemExit(f"Restored object tree contains a non-regular file: {path}")
        actual[path.relative_to(root).as_posix()] = path

missing = sorted(set(expected) - set(actual))
unexpected = sorted(set(actual) - set(expected))
if missing or unexpected:
    raise SystemExit(
        f"Restored object tree differs from database roots: missing={missing} unexpected={unexpected}"
    )
for key, item in expected.items():
    path = actual[key]
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    if path.stat().st_size != item["sizeBytes"] or digest.hexdigest() != item["sha256"]:
        raise SystemExit(f"Restored object integrity does not match database root: {key}")
PY

restored_roots_sql="$(cat <<'SQL'
WITH
reachable_checkpoint_ids AS (
    SELECT workspace.current_checkpoint_id AS id
    FROM secondbox.workspaces AS workspace
    JOIN secondbox.sandboxes AS sandbox ON sandbox.id=workspace.sandbox_id
    WHERE sandbox.deleted_at IS NULL
      AND sandbox.state<>'deleted'
      AND workspace.current_checkpoint_id<>''
    UNION
    SELECT snapshot.checkpoint_id
    FROM secondbox.snapshots AS snapshot
    WHERE snapshot.state='published'
      AND snapshot.retention_ended_at IS NULL
      AND snapshot.retain_until>
          current_setting('secondbox.restore_recovery_time')::timestamptz
),
reachable_objects AS (
    SELECT 'checkpoint'::text AS kind,checkpoint.id,
           checkpoint.storage_key AS storage_object_id,
           checkpoint.sha256,checkpoint.size_bytes
    FROM secondbox.workspace_checkpoints AS checkpoint
    JOIN reachable_checkpoint_ids AS reachable ON reachable.id=checkpoint.id
    WHERE checkpoint.state='published'
      AND checkpoint.garbage_collected_at IS NULL
    UNION ALL
    SELECT 'artifact'::text,artifact.id,artifact.storage_key,
           artifact.sha256,artifact.size_bytes
    FROM secondbox.artifacts AS artifact
    WHERE artifact.state='published'
      AND artifact.garbage_collected_at IS NULL
)
SELECT jsonb_build_object(
    'contractVersion','secondbox-restore-database-roots/v1',
    'objects',COALESCE(
        jsonb_agg(
            jsonb_build_object(
                'kind',kind,
                'id',id,
                'storageObjectId',storage_object_id,
                'sha256',sha256,
                'sizeBytes',size_bytes
            )
            ORDER BY kind,id
        ),
        '[]'::jsonb
    )
)
FROM reachable_objects
SQL
)"
psql "$target_url" \
  --quiet \
  --set ON_ERROR_STOP=1 \
  --tuples-only --no-align \
  --command "SET secondbox.restore_recovery_time='$backup_created_at'; $restored_roots_sql" \
  >"$stage/restored-database-roots.json"
jq -e '
  .contractVersion == "secondbox-restore-database-roots/v1" and
  (.objects | type == "array")
' "$stage/restored-database-roots.json" >/dev/null
if ! jq --sort-keys '.objects' "$stage/checkpoint-reachability.json" |
  cmp --silent - <(jq --sort-keys '.objects' "$stage/restored-database-roots.json"); then
  echo "Restored database roots differ from the recovery-point object manifest" >&2
  exit 1
fi

SECONDBOX_RESTORE_VERIFICATION_RECOVERY_POINT_ID="$recovery_point_id" \
SECONDBOX_RESTORE_VERIFICATION_DATABASE_URL="$target_url" \
SECONDBOX_RESTORE_VERIFICATION_OBJECT_STATE="$SECONDBOX_RESTORE_OBJECT_TARGET" \
SECONDBOX_RESTORE_VERIFICATION_OBJECT_MANIFEST="$stage/checkpoint-reachability.json" \
SECONDBOX_RESTORE_VERIFICATION_CONTROL_PLANE_URL="$SECONDBOX_RESTORE_CONTROL_PLANE_URL" \
  "$SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_COMMAND" \
  "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT" &
fresh_runner_verifier_pid="$!"
fresh_runner_verifier_running=true
for ((verifier_waited=0;
      verifier_waited<SECONDBOX_RESTORE_FRESH_RUNNER_VERIFY_TIMEOUT_SECONDS;
      verifier_waited++)); do
  if [[ -f "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT" ]]; then
    break
  fi
  if ! kill -0 "$fresh_runner_verifier_pid" 2>/dev/null; then
    if ! wait "$fresh_runner_verifier_pid"; then
      echo "Fresh-Runner restore verifier exited before publishing identity evidence" >&2
    else
      echo "Fresh-Runner restore verifier exited without publishing identity evidence" >&2
    fi
    fresh_runner_verifier_running=false
    exit 1
  fi
  sleep 1
done
if [[ -L "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT" ||
      ! -f "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT" ]]; then
  echo "Fresh-Runner verifier did not publish a regular identity result before its deadline" >&2
  exit 1
fi
chmod 600 "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT"
jq -e \
  --arg recoveryPointId "$recovery_point_id" \
  '.contractVersion == "secondbox-fresh-runner-identity/v1" and
   .recoveryPointId == $recoveryPointId and
   (.runner.id | type == "string" and length > 0) and
   (.runner.credentialSerial | type == "string" and length > 0) and
   (.restoration.sandboxId | type == "string" and length > 0) and
   (.restoration.workspaceId | type == "string" and length > 0) and
   (.restoration.assignmentId | type == "string" and length > 0) and
   (.restoration.materializationId | type == "string" and length > 0) and
   (.restoration.checkpointId | type == "string" and length > 0) and
   (.restoration.checkpointSHA256 | test("^[0-9a-f]{64}$")) and
   (.restoration.generation | type == "number" and . >= 2 and floor == .)' \
  "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT" >/dev/null

runner_id="$(jq -r '.runner.id' "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT")"
credential_serial="$(jq -r '.runner.credentialSerial' "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT")"
sandbox_id="$(jq -r '.restoration.sandboxId' "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT")"
workspace_id="$(jq -r '.restoration.workspaceId' "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT")"
assignment_id="$(jq -r '.restoration.assignmentId' "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT")"
materialization_id="$(jq -r '.restoration.materializationId' "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT")"
checkpoint_id="$(jq -r '.restoration.checkpointId' "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT")"
checkpoint_sha256="$(jq -r '.restoration.checkpointSHA256' "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT")"
restored_generation="$(jq -r '.restoration.generation' "$SECONDBOX_RESTORE_FRESH_RUNNER_RESULT")"
if ! jq -e --arg checkpointId "$checkpoint_id" --arg sha256 "$checkpoint_sha256" '
  any(.objects[];
      .kind == "checkpoint" and
      .id == $checkpointId and
      .sha256 == $sha256)
' "$stage/checkpoint-reachability.json" >/dev/null; then
  echo "Fresh-Runner verifier restored a checkpoint outside the recovery-point roots" >&2
  exit 1
fi

fresh_runner_database_sql="$(cat <<'SQL'
SELECT jsonb_build_object(
    'contractVersion','secondbox-fresh-runner-database-verification/v1',
    'freshCredential',EXISTS(
        SELECT 1
        FROM secondbox.runner_connections AS connection
        JOIN secondbox.runners AS runner ON runner.id=connection.runner_id
        WHERE connection.credential_serial=:'credential_serial'
          AND connection.runner_id=:'runner_id'
          AND connection.state='active'
          AND runner.state IN ('ready','draining')
    ),
    'readyAssignment',EXISTS(
        SELECT 1
        FROM secondbox.assignments AS assignment
        JOIN secondbox.sandboxes AS sandbox
          ON sandbox.id=assignment.sandbox_id
          AND sandbox.current_instance_id=assignment.instance_id
        WHERE assignment.id=:'assignment_id'
          AND assignment.sandbox_id=:'sandbox_id'
          AND assignment.runner_id=:'runner_id'
          AND assignment.generation=:'restored_generation'::bigint
          AND assignment.state='ready'
          AND sandbox.generation=assignment.generation
    ),
    'readyMaterialization',EXISTS(
        SELECT 1
        FROM secondbox.workspace_materializations
        WHERE id=:'materialization_id'
          AND workspace_id=:'workspace_id'
          AND sandbox_id=:'sandbox_id'
          AND assignment_id=:'assignment_id'
          AND runner_id=:'runner_id'
          AND generation=:'restored_generation'::bigint
          AND source_checkpoint_id=:'checkpoint_id'
          AND state='ready'
    ),
    'authoritativeCheckpoint',EXISTS(
        SELECT 1
        FROM secondbox.sandboxes AS sandbox
        JOIN secondbox.workspaces AS workspace
          ON workspace.id=sandbox.workspace_id
        JOIN secondbox.workspace_checkpoints AS checkpoint
          ON checkpoint.id=workspace.current_checkpoint_id
        WHERE sandbox.id=:'sandbox_id'
          AND sandbox.workspace_id=:'workspace_id'
          AND sandbox.generation=:'restored_generation'::bigint
          AND checkpoint.id=:'checkpoint_id'
          AND checkpoint.sha256=:'checkpoint_sha256'
          AND checkpoint.state='published'
    )
)
SQL
)"
printf '%s\n' "$fresh_runner_database_sql" |
psql "$target_url" \
  --set ON_ERROR_STOP=1 \
  --set runner_id="$runner_id" \
  --set credential_serial="$credential_serial" \
  --set sandbox_id="$sandbox_id" \
  --set workspace_id="$workspace_id" \
  --set assignment_id="$assignment_id" \
  --set materialization_id="$materialization_id" \
  --set checkpoint_id="$checkpoint_id" \
  --set checkpoint_sha256="$checkpoint_sha256" \
  --set restored_generation="$restored_generation" \
  --tuples-only --no-align \
  >"$stage/fresh-runner-database-verification.json"
jq -e '
  .contractVersion == "secondbox-fresh-runner-database-verification/v1" and
  .freshCredential == true and
  .readyAssignment == true and
  .readyMaterialization == true and
  .authoritativeCheckpoint == true
' "$stage/fresh-runner-database-verification.json" >/dev/null

control_plane_url="$(python3 - "$SECONDBOX_RESTORE_CONTROL_PLANE_URL" <<'PY'
import sys
from urllib.parse import urlsplit, urlunsplit

parts = urlsplit(sys.argv[1])
if (
    parts.scheme not in {"http", "https"}
    or not parts.hostname
    or parts.username is not None
    or parts.password is not None
    or parts.query
    or parts.fragment
):
    raise SystemExit("SECONDBOX_RESTORE_CONTROL_PLANE_URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
print(urlunsplit((parts.scheme, parts.netloc, parts.path.rstrip("/"), "", "")))
PY
)"
sandbox_path="$(python3 - "$sandbox_id" <<'PY'
import sys
from urllib.parse import quote

print(quote(sys.argv[1], safe=""))
PY
)"
stale_generation="$((restored_generation - 1))"
current_status="$(
  curl --silent --show-error \
    --output "$stage/current-generation-ping.json" \
    --write-out '%{http_code}' \
    --request POST \
    --header "Authorization: Bearer $SECONDBOX_RESTORE_CONTROL_PLANE_TOKEN" \
    --header "X-SecondBox-Tenant-Ref: $SECONDBOX_RESTORE_TENANT_REF" \
    --header "X-SecondBox-Subject-Ref: $SECONDBOX_RESTORE_SUBJECT_REF" \
    --header "SecondBox-Generation: $restored_generation" \
    --header "X-Request-ID: restore-drill-current-generation" \
    "$control_plane_url/v1/sandboxes/$sandbox_path:ping"
)"
if [[ "$current_status" != "200" ]] ||
   ! jq -e --arg sandboxId "$sandbox_id" --argjson generation "$restored_generation" '
     .sandboxId == $sandboxId and .generation == $generation
   ' "$stage/current-generation-ping.json" >/dev/null; then
  echo "Restored control plane did not serve the verified fresh-Runner Sandbox generation: HTTP $current_status" >&2
  sed -n '1,12p' "$stage/current-generation-ping.json" >&2
  exit 1
fi
stale_status="$(
  curl --silent --show-error \
    --output "$stage/stale-generation-ping.json" \
    --write-out '%{http_code}' \
    --request POST \
    --header "Authorization: Bearer $SECONDBOX_RESTORE_CONTROL_PLANE_TOKEN" \
    --header "X-SecondBox-Tenant-Ref: $SECONDBOX_RESTORE_TENANT_REF" \
    --header "X-SecondBox-Subject-Ref: $SECONDBOX_RESTORE_SUBJECT_REF" \
    --header "SecondBox-Generation: $stale_generation" \
    --header "X-Request-ID: restore-drill-stale-generation" \
    "$control_plane_url/v1/sandboxes/$sandbox_path:ping"
)"
if [[ "$stale_status" != "409" ]] ||
   ! jq -e '.code == "generation_fenced" and .status == 409 and .retryable == false' \
     "$stage/stale-generation-ping.json" >/dev/null; then
  echo "Restored control plane did not reject the stale Sandbox generation: HTTP $stale_status" >&2
  sed -n '1,12p' "$stage/stale-generation-ping.json" >&2
  exit 1
fi
stop_fresh_runner_verifier

restore_succeeded=true
echo "SecondBox restore drill evidence passed: database=$database_name recovery_point=$recovery_point_id"
if [[ "$keep_db" == true ]]; then
  echo "Throwaway database retained: $database_name"
fi
