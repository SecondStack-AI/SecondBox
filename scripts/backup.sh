#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
  cat <<'USAGE'
Usage: scripts/backup.sh

Packages one database-derived, quiescent SecondBox recovery point. Admission
and database mutations are fenced across all control-plane replicas by a PostgreSQL
table lock transaction for the duration of the recovery-point capture.

Required environment:
  SECONDBOX_BACKUP_DATABASE_URL
  SECONDBOX_BACKUP_DIR
  SECONDBOX_BACKUP_RECOVERY_POINT_ID
USAGE
}

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  usage
  exit 0
fi
if [[ "$#" -ne 0 ]]; then
  usage >&2
  exit 2
fi

: "${SECONDBOX_BACKUP_DATABASE_URL:?set SECONDBOX_BACKUP_DATABASE_URL}"
: "${SECONDBOX_BACKUP_DIR:?set SECONDBOX_BACKUP_DIR}"
: "${SECONDBOX_BACKUP_RECOVERY_POINT_ID:?set SECONDBOX_BACKUP_RECOVERY_POINT_ID}"

for required_command in pg_dump psql tar sha256sum jq flock date mktemp cmp sed; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox backup requires command: $required_command" >&2
    exit 2
  fi
done
if [[ ! "$SECONDBOX_BACKUP_RECOVERY_POINT_ID" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]]; then
  echo "SECONDBOX_BACKUP_RECOVERY_POINT_ID must be a bounded opaque identifier" >&2
  exit 2
fi
if [[ -L "$SECONDBOX_BACKUP_DIR" ]]; then
  echo "SECONDBOX_BACKUP_DIR must not be a symbolic link" >&2
  exit 2
fi
install -d -m 700 "$SECONDBOX_BACKUP_DIR"

exec 9>"$SECONDBOX_BACKUP_DIR/.backup.lock"
if ! flock -n 9; then
  echo "Another SecondBox backup is running" >&2
  exit 1
fi

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
created_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
stage="$(mktemp -d "$SECONDBOX_BACKUP_DIR/.stage-${timestamp}-XXXXXX")"
verify_stage="$(mktemp -d "$SECONDBOX_BACKUP_DIR/.verify-${timestamp}-XXXXXX")"
backup_fence_active=false
backup_fence_read_fd=""
backup_fence_write_fd=""
backup_fence_pid=""

release_backup_fence() {
  if [[ "$backup_fence_active" != true ]]; then
    return 0
  fi
  local release_status=0
  if ! printf '%s\n' 'COMMIT;' '\q' >&"$backup_fence_write_fd"; then
    echo "Failed to request SecondBox backup fence release" >&2
    release_status=1
  fi
  exec {backup_fence_write_fd}>&-
  while IFS= read -r _ <&"$backup_fence_read_fd"; do
    :
  done
  exec {backup_fence_read_fd}<&-
  if ! wait "$backup_fence_pid"; then
    echo "SecondBox backup fence transaction failed" >&2
    if [[ -s "$stage/database-fence.stderr" ]]; then
      sed -n '1,40p' "$stage/database-fence.stderr" >&2
    fi
    release_status=1
  fi
  backup_fence_active=false
  return "$release_status"
}

cleanup() {
  local status="$?"
  if ! release_backup_fence; then
    status=1
  fi
  if ! rm -rf -- "$stage" "$verify_stage"; then
    echo "Failed to remove SecondBox backup staging data" >&2
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT

database_state_sql="$(cat <<'SQL'
SELECT jsonb_build_object(
    'contractVersion','secondbox-backup-database-state/v3',
    'databaseRecoveryPosition',pg_current_wal_lsn()::text,
    'quiescence',jsonb_build_object(
        'activeSandboxes',(
            SELECT count(*) FROM secondbox.sandboxes
            WHERE deleted_at IS NULL
              AND (
                state NOT IN ('stopped','failed','deleted')
                OR (state='stopped' AND desired_state NOT IN ('stopped','deleted'))
              )
        ),
        'activeAssignments',(
            SELECT count(*) FROM secondbox.assignments
            WHERE state NOT IN ('released','failed','stopped','fenced','expired','superseded')
        ),
        'activeLifecycleEffects',(
            SELECT count(*) FROM secondbox.lifecycle_effects
            WHERE state NOT IN ('runner_succeeded','runner_failed')
        ),
        'activeDataPlaneSessions',(
            SELECT count(*) FROM secondbox.data_plane_sessions
            WHERE state IN ('pending','running','cancelling')
        )
    ),
    'fencing',jsonb_build_object(
        'activeInstances',(
            SELECT count(*) FROM secondbox.instances
            WHERE state NOT IN ('stopped','failed','fenced')
        ),
        'activeAssignments',(
            SELECT count(*) FROM secondbox.assignments
            WHERE state NOT IN ('released','failed','stopped','fenced','expired','superseded')
        )
    )
)
SQL
)"

read_database_state() {
  local output="$1"
  psql "$SECONDBOX_BACKUP_DATABASE_URL" \
    --quiet \
    --set ON_ERROR_STOP=1 \
    --tuples-only --no-align \
    --command "SET secondbox.backup_recovery_time='$created_at'; $database_state_sql" >"$output"
  jq -e '
    .contractVersion == "secondbox-backup-database-state/v3" and
    (.databaseRecoveryPosition | type == "string" and length > 0)
  ' "$output" >/dev/null
}

start_backup_fence() {
  coproc SECONDBOX_BACKUP_FENCE_PSQL {
    psql "$SECONDBOX_BACKUP_DATABASE_URL" \
      --no-psqlrc \
      --quiet \
      --tuples-only \
      --no-align \
      2>"$stage/database-fence.stderr"
  }
  backup_fence_pid="$SECONDBOX_BACKUP_FENCE_PSQL_PID"
  backup_fence_read_fd="${SECONDBOX_BACKUP_FENCE_PSQL[0]}"
  backup_fence_write_fd="${SECONDBOX_BACKUP_FENCE_PSQL[1]}"
  backup_fence_active=true

  if ! {
    printf '%s\n' \
      '\set ON_ERROR_STOP on' \
      'BEGIN;' \
      'DO $secondbox_backup$' \
      'DECLARE locked_table record;' \
      'BEGIN' \
      '  FOR locked_table IN' \
      '    SELECT schemaname,tablename' \
      '    FROM pg_tables' \
      "    WHERE schemaname='secondbox'" \
      '    ORDER BY schemaname,tablename' \
      '  LOOP' \
      "    EXECUTE format('LOCK TABLE %I.%I IN SHARE MODE', locked_table.schemaname, locked_table.tablename);" \
      '  END LOOP;' \
      'END' \
      '$secondbox_backup$;' \
      '\echo SECONDBOX_BACKUP_FENCE_READY'
  } >&"$backup_fence_write_fd"; then
    echo "Failed to initialize SecondBox backup fence" >&2
    return 1
  fi

  local fence_output
  while IFS= read -r fence_output <&"$backup_fence_read_fd"; do
    if [[ "$fence_output" == "SECONDBOX_BACKUP_FENCE_READY" ]]; then
      return 0
    fi
  done
  echo "SecondBox backup fence exited before acquiring all table locks" >&2
  if [[ -s "$stage/database-fence.stderr" ]]; then
    sed -n '1,40p' "$stage/database-fence.stderr" >&2
  fi
  return 1
}

start_backup_fence
jq -n \
  --arg recoveryPointId "$SECONDBOX_BACKUP_RECOVERY_POINT_ID" \
  '{
    contractVersion: "secondbox-backup-database-fence/v1",
    recoveryPointId: $recoveryPointId,
    state: "database-share-lock-held",
    schema: "secondbox",
    lockMode: "SHARE"
  }' >"$stage/database-fence.json"
read_database_state "$stage/database-state.json"
if ! jq -e '
  .quiescence == {
    activeSandboxes: 0,
    activeAssignments: 0,
    activeLifecycleEffects: 0,
    activeDataPlaneSessions: 0
  }
' "$stage/database-state.json" >/dev/null; then
  echo "SecondBox database is not quiescent; stop admission and drain all mutations" >&2
  exit 1
fi
if ! jq -e '
  .fencing == {activeInstances: 0, activeAssignments: 0}
' "$stage/database-state.json" >/dev/null; then
  echo "SecondBox database contains unfenced Runner work" >&2
  exit 1
fi
jq -n \
  --arg recoveryPointId "$SECONDBOX_BACKUP_RECOVERY_POINT_ID" \
  --arg position "$(jq -r '.databaseRecoveryPosition' "$stage/database-state.json")" \
  '{
    contractVersion: "secondbox-backup-quiescence/v1",
    recoveryPointId: $recoveryPointId,
    state: "quiesced",
    databaseRecoveryPosition: $position
  }' >"$stage/quiescence.json"
jq -n \
  --arg recoveryPointId "$SECONDBOX_BACKUP_RECOVERY_POINT_ID" \
  --arg position "$(jq -r '.databaseRecoveryPosition' "$stage/database-state.json")" \
  '{
    contractVersion: "secondbox-backup-fencing/v1",
    recoveryPointId: $recoveryPointId,
    state: "fenced",
    databaseRecoveryPosition: $position
  }' >"$stage/fencing.json"
pg_dump "$SECONDBOX_BACKUP_DATABASE_URL" \
  --format=custom \
  --no-owner \
  --no-privileges \
  --schema=secondbox \
  --file="$stage/secondbox.dump"
read_database_state "$stage/database-state-after.json"
if ! jq --sort-keys 'del(.databaseRecoveryPosition)' "$stage/database-state.json" |
  cmp --silent - <(jq --sort-keys 'del(.databaseRecoveryPosition)' "$stage/database-state-after.json"); then
  echo "SecondBox database authority changed while the recovery point was captured" >&2
  exit 1
fi
release_backup_fence

(
  cd "$stage"
  sha256sum \
    secondbox.dump database-state.json quiescence.json fencing.json \
    database-fence.json >checksums.sha256
)
jq -n \
  --arg contractVersion "secondbox-backup/v4" \
  --arg createdAt "$created_at" \
  --arg recoveryPointId "$SECONDBOX_BACKUP_RECOVERY_POINT_ID" \
  --arg recoveryPosition "$(jq -r '.databaseRecoveryPosition' "$stage/database-state.json")" \
  '{
    contractVersion: $contractVersion,
    createdAt: $createdAt,
    recoveryPointId: $recoveryPointId,
    databaseRecoveryPosition: $recoveryPosition,
    database: {
      file: "secondbox.dump",
      stateFile: "database-state.json",
      schema: "secondbox"
    },
    databaseFence: {
      file: "database-fence.json",
      state: "database-share-lock-held"
    },
    quiescence: {file: "quiescence.json", state: "database-verified"},
    fencing: {file: "fencing.json", state: "database-verified"}
  }' >"$stage/manifest.json"

bundle="$SECONDBOX_BACKUP_DIR/secondbox-backup-${timestamp}-${SECONDBOX_BACKUP_RECOVERY_POINT_ID}.tar"
tar --create --numeric-owner --file="$bundle.tmp" --directory="$stage" \
  manifest.json checksums.sha256 secondbox.dump database-state.json \
  database-fence.json quiescence.json fencing.json
mv "$bundle.tmp" "$bundle"
(
  cd "$(dirname "$bundle")"
  sha256sum "$(basename "$bundle")" >"$(basename "$bundle").sha256"
)
chmod 600 "$bundle" "$bundle.sha256"

tar --extract --file="$bundle" --directory="$verify_stage"
(
  cd "$verify_stage"
  sha256sum --check checksums.sha256
)
jq -e \
  --arg recoveryPointId "$SECONDBOX_BACKUP_RECOVERY_POINT_ID" \
  '.contractVersion == "secondbox-backup/v4" and
   .recoveryPointId == $recoveryPointId and
   .databaseFence.state == "database-share-lock-held"' \
  "$verify_stage/manifest.json" >/dev/null

echo "Verified SecondBox recovery bundle: $bundle"
