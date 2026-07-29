#!/usr/bin/env bash
set -euo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail_multirunner() {
  echo "SecondBox multi-Runner qualification failed: $*" >&2
  exit 1
}

required_environment=(
  SECONDBOX_MULTIRUNNER_QUALIFIED_DEPLOYMENT
  SECONDBOX_RELEASE_QUALIFICATION_CANDIDATE_DIRECTORY
  SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST
  SECONDBOX_RELEASE_QUALIFICATION_HOSTS_FILE
  SECONDBOX_RELEASE_QUALIFICATION_CONTROLLER_DIRECTORY
  SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY
  SECONDBOX_MULTIRUNNER_API_URL
  SECONDBOX_MULTIRUNNER_API_TOKEN
  SECONDBOX_MULTIRUNNER_API_CA_FILE
  SECONDBOX_MULTIRUNNER_DATABASE_URL
  SECONDBOX_MULTIRUNNER_PROFILE
  SECONDBOX_MULTIRUNNER_POOL
  SECONDBOX_MULTIRUNNER_RUNNER_A_ID
  SECONDBOX_MULTIRUNNER_RUNNER_B_ID
  SECONDBOX_MULTIRUNNER_CONTROLLER_HOST_ID
  SECONDBOX_MULTIRUNNER_HOST_A_ID
  SECONDBOX_MULTIRUNNER_HOST_B_ID
  SECONDBOX_MULTIRUNNER_HOST_A_SSH
  SECONDBOX_MULTIRUNNER_HOST_B_SSH
  SECONDBOX_MULTIRUNNER_HOST_A_SERVICE
  SECONDBOX_MULTIRUNNER_HOST_B_SERVICE
  SECONDBOX_MULTIRUNNER_HOST_A_RUNNER_BINARY
  SECONDBOX_MULTIRUNNER_HOST_B_RUNNER_BINARY
  SECONDBOX_MULTIRUNNER_HOST_A_ENVIRONMENT_FILE
  SECONDBOX_MULTIRUNNER_HOST_B_ENVIRONMENT_FILE
  SECONDBOX_MULTIRUNNER_HOST_SENTINEL_PATH
  SECONDBOX_MULTIRUNNER_SSH_IDENTITY_FILE
  SECONDBOX_MULTIRUNNER_SSH_KNOWN_HOSTS_FILE
  SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS
  SECONDBOX_MULTIRUNNER_PROBE_TIMEOUT_SECONDS
)
for environment_name in "${required_environment[@]}"; do
  [[ -n "${!environment_name-}" ]] ||
    fail_multirunner "set $environment_name explicitly"
done
[[ "$SECONDBOX_MULTIRUNNER_QUALIFIED_DEPLOYMENT" == "1" ]] ||
  fail_multirunner "SECONDBOX_MULTIRUNNER_QUALIFIED_DEPLOYMENT must be 1 only for a disposable qualification deployment"

for command_name in base64 curl git go jq node openssl psql realpath scp sha256sum ssh stat; do
  command -v "$command_name" >/dev/null 2>&1 ||
    fail_multirunner "required command is unavailable: $command_name"
done

canonical_file() {
  local file_path="$1"
  local label="$2"
  [[ ! -L "$file_path" && -f "$file_path" ]] ||
    fail_multirunner "$label must be a regular non-symbolic-link file"
  local canonical_path
  canonical_path="$(realpath -e -- "$file_path")"
  [[ "$canonical_path" == "$file_path" ]] ||
    fail_multirunner "$label must use its canonical absolute path"
}

canonical_directory() {
  local directory_path="$1"
  local label="$2"
  [[ ! -L "$directory_path" && -d "$directory_path" ]] ||
    fail_multirunner "$label must be a non-symbolic-link directory"
  local canonical_path
  canonical_path="$(realpath -e -- "$directory_path")"
  [[ "$canonical_path" == "$directory_path" ]] ||
    fail_multirunner "$label must use its canonical absolute path"
}

canonical_directory "$SECONDBOX_RELEASE_QUALIFICATION_CANDIDATE_DIRECTORY" "candidate directory"
canonical_file "$SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST" "release subject manifest"
canonical_file "$SECONDBOX_RELEASE_QUALIFICATION_HOSTS_FILE" "qualified host inventory"
canonical_directory "$SECONDBOX_RELEASE_QUALIFICATION_CONTROLLER_DIRECTORY" "scenario controller directory"
canonical_directory "$SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY" "qualification output directory"
canonical_file "$SECONDBOX_MULTIRUNNER_API_CA_FILE" "qualification API CA file"
canonical_file "$SECONDBOX_MULTIRUNNER_SSH_IDENTITY_FILE" "SSH identity"
canonical_file "$SECONDBOX_MULTIRUNNER_SSH_KNOWN_HOSTS_FILE" "SSH known-hosts file"
[[ "$(stat -c %a "$SECONDBOX_MULTIRUNNER_SSH_IDENTITY_FILE")" == "600" &&
   -s "$SECONDBOX_MULTIRUNNER_SSH_IDENTITY_FILE" ]] ||
  fail_multirunner "SSH identity must be a nonempty owner-only file"
[[ "$(stat -c %a "$SECONDBOX_MULTIRUNNER_SSH_KNOWN_HOSTS_FILE")" == "600" &&
   -s "$SECONDBOX_MULTIRUNNER_SSH_KNOWN_HOSTS_FILE" ]] ||
  fail_multirunner "SSH known-hosts file must be nonempty and owner-only"
[[ -s "$SECONDBOX_MULTIRUNNER_API_CA_FILE" ]] ||
  fail_multirunner "qualification API CA file must be nonempty"
[[ "$SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST" == \
   "$SECONDBOX_RELEASE_QUALIFICATION_CANDIDATE_DIRECTORY/release-subjects.json" ]] ||
  fail_multirunner "release subject manifest must be the canonical release-subjects.json inside the candidate directory"
[[ "$SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] &&
  (( SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS >= 60 &&
     SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS <= 3600 )) ||
  fail_multirunner "SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS must be from 60 through 3600"
[[ "$SECONDBOX_MULTIRUNNER_PROBE_TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] &&
  (( SECONDBOX_MULTIRUNNER_PROBE_TIMEOUT_SECONDS >= 5 &&
     SECONDBOX_MULTIRUNNER_PROBE_TIMEOUT_SECONDS <= 60 )) ||
  fail_multirunner "SECONDBOX_MULTIRUNNER_PROBE_TIMEOUT_SECONDS must be from 5 through 60"
[[ "$SECONDBOX_MULTIRUNNER_API_URL" =~ ^https://[A-Za-z0-9._:-]+$ ]] ||
  fail_multirunner "SECONDBOX_MULTIRUNNER_API_URL must be an origin-only HTTPS URL"
[[ "$SECONDBOX_MULTIRUNNER_PROFILE" =~ ^[a-z][a-z0-9-]{0,62}$ ]] ||
  fail_multirunner "SECONDBOX_MULTIRUNNER_PROFILE is invalid"
[[ "$SECONDBOX_MULTIRUNNER_POOL" =~ ^[a-z][a-z0-9-]{0,62}$ ]] ||
  fail_multirunner "SECONDBOX_MULTIRUNNER_POOL is invalid"

safe_identity='^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$'
safe_host='^[A-Za-z0-9][A-Za-z0-9@._:-]{0,255}$'
safe_unit='^[A-Za-z0-9][A-Za-z0-9@_.-]{0,127}$'
safe_path='^/[A-Za-z0-9._/-]+$'
for value in \
  "$SECONDBOX_MULTIRUNNER_RUNNER_A_ID" \
  "$SECONDBOX_MULTIRUNNER_RUNNER_B_ID" \
  "$SECONDBOX_MULTIRUNNER_CONTROLLER_HOST_ID" \
  "$SECONDBOX_MULTIRUNNER_HOST_A_ID" \
  "$SECONDBOX_MULTIRUNNER_HOST_B_ID"; do
  [[ "$value" =~ $safe_identity ]] || fail_multirunner "host or Runner identity is invalid: $value"
done
for value in "$SECONDBOX_MULTIRUNNER_HOST_A_SSH" "$SECONDBOX_MULTIRUNNER_HOST_B_SSH"; do
  [[ "$value" =~ $safe_host ]] || fail_multirunner "SSH destination is invalid: $value"
done
for value in "$SECONDBOX_MULTIRUNNER_HOST_A_SERVICE" "$SECONDBOX_MULTIRUNNER_HOST_B_SERVICE"; do
  [[ "$value" =~ $safe_unit ]] || fail_multirunner "systemd unit is invalid: $value"
done
for value in \
  "$SECONDBOX_MULTIRUNNER_HOST_A_RUNNER_BINARY" \
  "$SECONDBOX_MULTIRUNNER_HOST_B_RUNNER_BINARY" \
  "$SECONDBOX_MULTIRUNNER_HOST_A_ENVIRONMENT_FILE" \
  "$SECONDBOX_MULTIRUNNER_HOST_B_ENVIRONMENT_FILE" \
  "$SECONDBOX_MULTIRUNNER_HOST_SENTINEL_PATH"; do
  [[ "$value" =~ $safe_path ]] || fail_multirunner "remote path is invalid: $value"
done
[[ "$SECONDBOX_MULTIRUNNER_RUNNER_A_ID" != "$SECONDBOX_MULTIRUNNER_RUNNER_B_ID" ]] ||
  fail_multirunner "Runner identities must be distinct"
[[ "$SECONDBOX_MULTIRUNNER_HOST_A_ID" != "$SECONDBOX_MULTIRUNNER_HOST_B_ID" ]] ||
  fail_multirunner "qualified host identities must be distinct"
[[ "$SECONDBOX_MULTIRUNNER_HOST_A_SSH" != "$SECONDBOX_MULTIRUNNER_HOST_B_SSH" ]] ||
  fail_multirunner "qualified SSH destinations must be distinct"
[[ "$SECONDBOX_MULTIRUNNER_CONTROLLER_HOST_ID" != "$SECONDBOX_MULTIRUNNER_HOST_A_ID" &&
   "$SECONDBOX_MULTIRUNNER_CONTROLLER_HOST_ID" != "$SECONDBOX_MULTIRUNNER_HOST_B_ID" ]] ||
  fail_multirunner "controller host identity must be distinct from both Runner hosts"

scenario_directory="$SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY/qualification/multi-runner"
record_path="$SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY/qualification/multi-runner.json"
[[ ! -e "$scenario_directory" && ! -e "$record_path" ]] ||
  fail_multirunner "multi-Runner evidence already exists; refusing to overwrite"
install -d -m 0700 "$scenario_directory"
working_directory="$(mktemp -d)"
cleanup_multirunner() {
  local status="$?"
  trap - EXIT
  rm -rf -- "$working_directory"
  exit "$status"
}
trap cleanup_multirunner EXIT

source_commit="$(jq -er '.sourceCommit' "$SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST")"
release_version="$(jq -er '.releaseVersion' "$SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST")"
subject_manifest_sha256="$(sha256sum "$SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST" | awk '{print $1}')"
[[ "$source_commit" =~ ^[0-9a-f]{40}$ ]] ||
  fail_multirunner "release subject source commit is invalid"
[[ "$(git -C "$repo_root" rev-parse HEAD)" == "$source_commit" ]] ||
  fail_multirunner "controller checkout does not equal the packaged candidate commit"
[[ "$(jq -er '.status' "$SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST")" == "passed" ]] ||
  fail_multirunner "release subject manifest is not passing"
[[ "$(jq -er '.subjects | length' "$SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST")" == "13" ]] ||
  fail_multirunner "release subject manifest does not contain exactly 13 subjects"
runner_binary_sha256="$(
  jq -er '.subjects[] | select(.id == "secondbox-runner" and .status == "passed") | .digest.sha256' \
    "$SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST"
)"

node "$repo_root/scripts/verify-release-qualification-record.mjs" \
  "$SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST" \
  "$SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY/qualification/kvm.json" \
  kvm \
  "$SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY"
for host_id in "$SECONDBOX_MULTIRUNNER_HOST_A_ID" "$SECONDBOX_MULTIRUNNER_HOST_B_ID"; do
  jq --arg host_id "$host_id" -e \
    'any(.hosts[];
      .id == $host_id and .role == "runner" and .deploymentMode == "systemd" and
      .dedicated == true and .kvm == true)' \
    "$SECONDBOX_RELEASE_QUALIFICATION_HOSTS_FILE" >/dev/null ||
    fail_multirunner "qualified host inventory does not identify $host_id as a dedicated systemd KVM Runner"
  jq --arg host_id "$host_id" -e \
    'any(.hosts[]; .id == $host_id and .role == "runner" and .dedicated == true and .kvm == true)' \
    "$SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY/qualification/kvm.json" >/dev/null ||
    fail_multirunner "packaged KVM result does not qualify host $host_id"
done
jq --arg host_id "$SECONDBOX_MULTIRUNNER_CONTROLLER_HOST_ID" -e \
  'any(.hosts[]; .id == $host_id and .role == "controller")' \
  "$SECONDBOX_RELEASE_QUALIFICATION_HOSTS_FILE" >/dev/null ||
  fail_multirunner "qualified host inventory does not identify the explicit controller host"

runner_subject_locator="$(
  jq -er '.subjects[] | select(.id == "secondbox-runner") | .locator' \
    "$SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST"
)"
candidate_runner_path="$SECONDBOX_RELEASE_QUALIFICATION_CANDIDATE_DIRECTORY/$runner_subject_locator"
canonical_file "$candidate_runner_path" "packaged Runner subject"
[[ "$candidate_runner_path" == "$SECONDBOX_RELEASE_QUALIFICATION_CANDIDATE_DIRECTORY/"* ]] ||
  fail_multirunner "packaged Runner subject escapes the candidate directory"
[[ "$(sha256sum "$candidate_runner_path" | awk '{print $1}')" == "$runner_binary_sha256" ]] ||
  fail_multirunner "packaged Runner subject does not match its candidate digest"

database_name="$(
  psql "$SECONDBOX_MULTIRUNNER_DATABASE_URL" \
    --no-psqlrc --set ON_ERROR_STOP=1 --tuples-only --no-align \
    --command 'SELECT current_database()'
)"
[[ "$database_name" == *qualification* ]] ||
  fail_multirunner "database name must contain qualification; refusing possible production target"

ssh_options=(
  -o BatchMode=yes
  -o IdentitiesOnly=yes
  -o StrictHostKeyChecking=yes
  -o "UserKnownHostsFile=$SECONDBOX_MULTIRUNNER_SSH_KNOWN_HOSTS_FILE"
  -i "$SECONDBOX_MULTIRUNNER_SSH_IDENTITY_FILE"
)

remote_preflight() {
  local destination="$1"
  local service="$2"
  local runner_binary="$3"
  local environment_file="$4"
  ssh "${ssh_options[@]}" "$destination" sudo -n sh -s -- \
    "$source_commit" \
    "$SECONDBOX_MULTIRUNNER_HOST_SENTINEL_PATH" \
    "$service" \
    "$runner_binary" \
    "$environment_file" \
    "$runner_binary_sha256" <<'REMOTE_PREFLIGHT'
set -eu
source_commit="$1"
sentinel="$2"
service="$3"
runner_binary="$4"
environment_file="$5"
runner_sha256="$6"
test "$(id -u)" = 0
test -r /dev/kvm
test -w /dev/kvm
test -r /dev/net/tun
test -w /dev/net/tun
test -f "$sentinel"
test ! -L "$sentinel"
test "$(cat "$sentinel")" = "$source_commit"
test -x "$runner_binary"
test "$(sha256sum "$runner_binary" | awk '{print $1}')" = "$runner_sha256"
test -f "$environment_file"
test ! -L "$environment_file"
test "$(stat -c %a "$environment_file")" = 600
systemctl is-active --quiet "$service"
printf '{"machineId":"%s","bootId":"%s","runnerSHA256":"%s"}\n' \
  "$(cat /etc/machine-id)" \
  "$(cat /proc/sys/kernel/random/boot_id)" \
  "$runner_sha256"
REMOTE_PREFLIGHT
}

host_a_preflight="$(
  remote_preflight \
    "$SECONDBOX_MULTIRUNNER_HOST_A_SSH" \
    "$SECONDBOX_MULTIRUNNER_HOST_A_SERVICE" \
    "$SECONDBOX_MULTIRUNNER_HOST_A_RUNNER_BINARY" \
    "$SECONDBOX_MULTIRUNNER_HOST_A_ENVIRONMENT_FILE"
)"
host_b_preflight="$(
  remote_preflight \
    "$SECONDBOX_MULTIRUNNER_HOST_B_SSH" \
    "$SECONDBOX_MULTIRUNNER_HOST_B_SERVICE" \
    "$SECONDBOX_MULTIRUNNER_HOST_B_RUNNER_BINARY" \
    "$SECONDBOX_MULTIRUNNER_HOST_B_ENVIRONMENT_FILE"
)"
jq -e . <<<"$host_a_preflight" >/dev/null
jq -e . <<<"$host_b_preflight" >/dev/null
[[ "$(jq -r '.machineId' <<<"$host_a_preflight")" != "$(jq -r '.machineId' <<<"$host_b_preflight")" ]] ||
  fail_multirunner "Runner hosts report the same machine identity"

go build -o "$working_directory/secondbox-multirunner-admin" \
  "$repo_root/cmd/secondbox-multirunner-admin"
(
  cd "$repo_root/runner"
  go build -o "$working_directory/secondbox-stale-runner-probe" \
    ./cmd/secondbox-stale-runner-probe
)

api_json() {
  local method="$1"
  local path="$2"
  local body="$3"
  shift 3
  local response_path="$working_directory/api-response.json"
  local status
  local curl_arguments=(
    --silent
    --show-error
    --proto '=https'
    --cacert "$SECONDBOX_MULTIRUNNER_API_CA_FILE"
    --max-time "$SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS"
    --request "$method"
    --header "Authorization: Bearer $SECONDBOX_MULTIRUNNER_API_TOKEN"
    --header 'Accept: application/json'
    "$@"
  )
  if [[ -n "$body" ]]; then
    curl_arguments+=(--data-binary "$body")
  fi
  status="$(
    curl "${curl_arguments[@]}" \
      --output "$response_path" \
      --write-out '%{http_code}' \
      "$SECONDBOX_MULTIRUNNER_API_URL$path"
  )"
  [[ "$status" =~ ^2[0-9][0-9]$ ]] ||
    fail_multirunner "API $method $path returned HTTP $status: $(tr '\n' ' ' <"$response_path")"
  jq -e . "$response_path" >/dev/null ||
    fail_multirunner "API $method $path returned non-JSON content"
  cat "$response_path"
}

wait_operation() {
  local operation_id="$1"
  local deadline=$((SECONDS + SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    local operation
    operation="$(api_json GET "/v1/operations/$operation_id" "")"
    case "$(jq -r '.state' <<<"$operation")" in
      succeeded)
        printf '%s\n' "$operation"
        return
        ;;
      failed|cancelled)
        fail_multirunner "operation $operation_id ended in $(jq -r '.state' <<<"$operation")"
        ;;
    esac
    sleep 1
  done
  fail_multirunner "operation $operation_id did not complete before the qualification timeout"
}

wait_sandbox_state() {
  local sandbox_id="$1"
  local expected_state="$2"
  local deadline=$((SECONDS + SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    local sandbox
    sandbox="$(api_json GET "/v1/sandboxes/$sandbox_id" "")"
    if [[ "$(jq -r '.state' <<<"$sandbox")" == "$expected_state" ]]; then
      printf '%s\n' "$sandbox"
      return
    fi
    [[ "$(jq -r '.state' <<<"$sandbox")" != "failed" ]] ||
      fail_multirunner "Sandbox $sandbox_id entered failed state"
    sleep 1
  done
  fail_multirunner "Sandbox $sandbox_id did not reach $expected_state"
}

create_stopped_sandbox() {
  local suffix="$1"
  local request_body
  request_body="$(
    jq -cn \
      --arg profile "$SECONDBOX_MULTIRUNNER_PROFILE" \
      --arg scenario "$suffix" \
      '{profile: $profile, metadata: {qualificationScenario: $scenario}}'
  )"
  local accepted operation
  accepted="$(
    api_json POST /v1/sandboxes "$request_body" \
      --header 'Content-Type: application/json' \
      --header "Idempotency-Key: multirunner-create-$suffix-$source_commit" \
      --header "X-Request-ID: multirunner-create-$suffix"
  )"
  operation="$(wait_operation "$(jq -er '.id' <<<"$accepted")")"
  local sandbox_id
  sandbox_id="$(jq -er '.sandbox.id' <<<"$operation")"
  wait_sandbox_state "$sandbox_id" stopped >/dev/null
  printf '%s\n' "$sandbox_id"
}

start_sandbox() {
  local sandbox_id="$1"
  local suffix="$2"
  local sandbox accepted
  sandbox="$(api_json GET "/v1/sandboxes/$sandbox_id" "")"
  accepted="$(
    api_json POST "/v1/sandboxes/$sandbox_id:start" "" \
      --header "If-Match: \"revision-$(jq -er '.revision' <<<"$sandbox")\"" \
      --header "Idempotency-Key: multirunner-start-$suffix-$source_commit" \
      --header "X-Request-ID: multirunner-start-$suffix"
  )"
  wait_operation "$(jq -er '.id' <<<"$accepted")" >/dev/null
  wait_sandbox_state "$sandbox_id" ready
}

stop_sandbox() {
  local sandbox_id="$1"
  local suffix="$2"
  local sandbox accepted
  sandbox="$(api_json GET "/v1/sandboxes/$sandbox_id" "")"
  accepted="$(
    api_json POST "/v1/sandboxes/$sandbox_id:stop" "" \
      --header "If-Match: \"revision-$(jq -er '.revision' <<<"$sandbox")\"" \
      --header "Idempotency-Key: multirunner-stop-$suffix-$source_commit" \
      --header "X-Request-ID: multirunner-stop-$suffix"
  )"
  wait_operation "$(jq -er '.id' <<<"$accepted")" >/dev/null
  wait_sandbox_state "$sandbox_id" stopped
}

assignment_json() {
  local sandbox_id="$1"
  psql "$SECONDBOX_MULTIRUNNER_DATABASE_URL" \
    --no-psqlrc --set ON_ERROR_STOP=1 --tuples-only --no-align \
    --set sandbox_id="$sandbox_id" \
    --command "
      SELECT json_build_object(
        'assignmentId', assignment.id,
        'sandboxId', assignment.sandbox_id,
        'instanceId', assignment.instance_id,
        'runnerId', assignment.runner_id,
        'generation', assignment.generation,
        'state', assignment.state,
        'fencingTokenSHA256', encode(sha256(assignment.fencing_token), 'hex'),
        'fencingTokenBase64', encode(assignment.fencing_token, 'base64'),
        'materializationState', materialization.state,
        'sourceCheckpointId', materialization.source_checkpoint_id
      )
      FROM secondbox.assignments AS assignment
      JOIN secondbox.workspace_materializations AS materialization
        ON materialization.assignment_id=assignment.id
      WHERE assignment.sandbox_id=:'sandbox_id'
      ORDER BY assignment.generation DESC
      LIMIT 1"
}

wait_runner_state() {
  local runner_id="$1"
  local expected_state="$2"
  local deadline=$((SECONDS + SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    local state
    state="$(
      psql "$SECONDBOX_MULTIRUNNER_DATABASE_URL" \
        --no-psqlrc --set ON_ERROR_STOP=1 --tuples-only --no-align \
        --set runner_id="$runner_id" \
        --command "SELECT state FROM secondbox.runners WHERE id=:'runner_id'"
    )"
    if [[ "$state" == "$expected_state" ]]; then
      return
    fi
    sleep 1
  done
  fail_multirunner "Runner $runner_id did not reach $expected_state"
}

wait_runner_software_version() {
  local runner_id="$1"
  local expected_version="$2"
  local deadline=$((SECONDS + SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS))
  while (( SECONDS < deadline )); do
    local observed_version
    observed_version="$(
      psql "$SECONDBOX_MULTIRUNNER_DATABASE_URL" \
        --no-psqlrc --set ON_ERROR_STOP=1 --tuples-only --no-align \
        --set runner_id="$runner_id" \
        --command "SELECT software_version FROM secondbox.runners WHERE id=:'runner_id'"
    )"
    if [[ "$observed_version" == "$expected_version" ]]; then
      return
    fi
    sleep 1
  done
  fail_multirunner "Runner $runner_id did not restore its packaged software connection"
}

runner_identity_json="$(
  psql "$SECONDBOX_MULTIRUNNER_DATABASE_URL" \
    --no-psqlrc --set ON_ERROR_STOP=1 --tuples-only --no-align \
    --set runner_a="$SECONDBOX_MULTIRUNNER_RUNNER_A_ID" \
    --set runner_b="$SECONDBOX_MULTIRUNNER_RUNNER_B_ID" \
    --set pool_name="$SECONDBOX_MULTIRUNNER_POOL" \
    --command "
      SELECT json_agg(row_to_json(evidence) ORDER BY evidence.runner_id)
      FROM (
        SELECT runner.id AS runner_id, runner.pool_name, runner.state,
               runner.software_version, runner.active_connection_id,
               credential.serial_number AS credential_serial,
               credential.certificate_fingerprint_sha256
        FROM secondbox.runners AS runner
        JOIN secondbox.runner_connections AS connection
          ON connection.id=runner.active_connection_id AND connection.state='active'
        JOIN secondbox.runner_credentials AS credential
          ON credential.serial_number=connection.credential_serial
         AND credential.state IN ('active','retiring')
        WHERE runner.id IN (:'runner_a', :'runner_b')
          AND runner.pool_name=:'pool_name'
          AND runner.state='ready'
      ) AS evidence"
)"
[[ "$(jq -er 'length' <<<"$runner_identity_json")" == "2" ]] ||
  fail_multirunner "both explicit Runners are not simultaneously ready in the expected pool"
[[ "$(jq -r '.[0].credential_serial' <<<"$runner_identity_json")" != \
   "$(jq -r '.[1].credential_serial' <<<"$runner_identity_json")" ]] ||
  fail_multirunner "Runner credential serials are not distinct"

profile_json="$(api_json GET "/v1/profiles/$SECONDBOX_MULTIRUNNER_PROFILE" "")"
jq --arg pool "$SECONDBOX_MULTIRUNNER_POOL" -e \
  '.currentRevision.spec.pool == $pool and
   .currentRevision.spec.backend == "firecracker" and
   .currentRevision.spec.lifecycle.initialState == "stopped" and
   .currentRevision.spec.lifecycle.leaseSeconds >= 300 and
   .currentRevision.spec.checkpoint.onStop == true' \
  <<<"$profile_json" >/dev/null ||
  fail_multirunner "qualification Profile must be explicit, stopped initially, Firecracker-backed, checkpoint on stop, and allow a 300-second Lease"

write_scenario_artifact() {
  local scenario_id="$1"
  local summary="$2"
  local evidence="$3"
  local output_path="$scenario_directory/$scenario_id.json"
  [[ ! -e "$output_path" ]] || fail_multirunner "scenario artifact already exists: $scenario_id"
  jq -n \
    --arg source_commit "$source_commit" \
    --arg subject_manifest_sha256 "$subject_manifest_sha256" \
    --arg scenario_id "$scenario_id" \
    --arg summary "$summary" \
    --argjson evidence "$evidence" \
    '{
      schemaVersion: 1,
      sourceCommit: $source_commit,
      subjectManifestSHA256: $subject_manifest_sha256,
      scenarioId: $scenario_id,
      status: "passed",
      summary: $summary,
      evidence: $evidence
    }' >"$output_path"
  chmod 600 "$output_path"
}

started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
write_scenario_artifact \
  independent-qualified-runners \
  "Two distinct KVM-qualified machines ran the exact packaged Runner binary." \
  "$(jq -cn \
      --argjson hostA "$host_a_preflight" \
      --argjson hostB "$host_b_preflight" \
      --arg kvmRecordSHA256 "$(sha256sum "$SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY/qualification/kvm.json" | awk '{print $1}')" \
      '{hostA: $hostA, hostB: $hostB, kvmRecordSHA256: $kvmRecordSHA256}')"
write_scenario_artifact \
  distinct-revocable-runner-identities \
  "Both Runner connections used distinct active or retiring mTLS credential serials." \
  "$runner_identity_json"

placement_one="$(create_stopped_sandbox placement-one)"
placement_two="$(create_stopped_sandbox placement-two)"
start_sandbox "$placement_one" placement-one >/dev/null
start_sandbox "$placement_two" placement-two >/dev/null
placement_one_assignment="$(assignment_json "$placement_one")"
placement_two_assignment="$(assignment_json "$placement_two")"
[[ "$(jq -r '.runnerId' <<<"$placement_one_assignment")" != \
   "$(jq -r '.runnerId' <<<"$placement_two_assignment")" ]] ||
  fail_multirunner "scheduler did not place concurrent Sandboxes on both qualified Runners"
write_scenario_artifact \
  placement \
  "The production scheduler placed two concurrent Sandboxes on distinct compatible Runners." \
  "$(jq -cn \
      --argjson first "$placement_one_assignment" \
      --argjson second "$placement_two_assignment" \
      '{first: $first, second: $second}')"

source_assignment="$placement_one_assignment"
source_generation="$(jq -r '.generation' <<<"$source_assignment")"
source_lease="$(
  api_json POST "/v1/sandboxes/$placement_one/leases" '{"durationSeconds":300}' \
    --header 'Content-Type: application/json' \
    --header "SecondBox-Generation: $source_generation" \
    --header "Idempotency-Key: multirunner-source-lease-$source_commit" |
    jq -er '.id'
)"
marker="secondbox-multirunner-$source_commit"
marker_digest="$(printf '%s' "$marker" | openssl dgst -sha256 -binary | base64 -w0)"
marker_write_path="$working_directory/marker-write.json"
marker_write_status="$(
  curl --silent --show-error --proto '=https' \
    --cacert "$SECONDBOX_MULTIRUNNER_API_CA_FILE" \
    --max-time "$SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS" \
    --request PUT \
    --header "Authorization: Bearer $SECONDBOX_MULTIRUNNER_API_TOKEN" \
    --header 'Content-Type: application/octet-stream' \
    --header "SecondBox-Generation: $source_generation" \
    --header "SecondBox-Lease-ID: $source_lease" \
    --header "Digest: sha-256=:$marker_digest=:" \
    --header "Idempotency-Key: multirunner-marker-$source_commit" \
    --data-binary "$marker" \
    --output "$marker_write_path" \
    --write-out '%{http_code}' \
    "$SECONDBOX_MULTIRUNNER_API_URL/v1/sandboxes/$placement_one/files?path=qualification-marker"
)"
[[ "$marker_write_status" == "200" ]] ||
  fail_multirunner "workspace marker write failed with HTTP $marker_write_status"

drained_runner="$(jq -r '.runnerId' <<<"$placement_one_assignment")"
other_runner="$(jq -r '.runnerId' <<<"$placement_two_assignment")"
# secondbox-multirunner-admin persists this through reconcile.RequestRunnerDrain.
SECONDBOX_MULTIRUNNER_DATABASE_URL="$SECONDBOX_MULTIRUNNER_DATABASE_URL" \
  "$working_directory/secondbox-multirunner-admin" request-runner-drain \
    --runner-id "$drained_runner" \
    --deadline-seconds "$SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS"
wait_runner_state "$drained_runner" draining
stop_sandbox "$placement_two" drain-capacity-release >/dev/null
drain_probe="$(create_stopped_sandbox drain-probe)"
start_sandbox "$drain_probe" drain-probe >/dev/null
drain_probe_assignment="$(assignment_json "$drain_probe")"
[[ "$(jq -r '.runnerId' <<<"$drain_probe_assignment")" == "$other_runner" ]] ||
  fail_multirunner "scheduler admitted new work to a draining Runner"
stop_sandbox "$placement_one" drain-complete >/dev/null
stopped_sandbox="$(api_json GET "/v1/sandboxes/$placement_one" "")"
checkpoint_id="$(jq -er '.workspace.currentCheckpointId' <<<"$stopped_sandbox")"
wait_runner_state "$drained_runner" drained
write_scenario_artifact \
  drain \
  "A durable bounded Runner drain rejected new placement while existing work stopped and reached drained." \
  "$(jq -cn \
      --arg drainedRunner "$drained_runner" \
      --arg replacementRunner "$other_runner" \
      --argjson probe "$drain_probe_assignment" \
      '{drainedRunner: $drainedRunner, replacementRunner: $replacementRunner, probe: $probe}')"

start_sandbox "$placement_one" relocation-target >/dev/null
relocated_assignment="$(assignment_json "$placement_one")"
[[ "$(jq -r '.runnerId' <<<"$relocated_assignment")" == "$other_runner" ]] ||
  fail_multirunner "stopped Sandbox did not relocate away from the drained Runner"
[[ "$(jq -r '.sourceCheckpointId' <<<"$relocated_assignment")" == "$checkpoint_id" ]] ||
  fail_multirunner "relocated Assignment did not materialize the published checkpoint"
new_generation="$(jq -r '.generation' <<<"$relocated_assignment")"
new_lease="$(
  api_json POST "/v1/sandboxes/$placement_one/leases" '{"durationSeconds":300}' \
    --header 'Content-Type: application/json' \
    --header "SecondBox-Generation: $new_generation" \
    --header "Idempotency-Key: multirunner-new-lease-$source_commit" |
    jq -er '.id'
)"
restored_marker="$working_directory/restored-marker"
restored_status="$(
  curl --silent --show-error --proto '=https' \
    --cacert "$SECONDBOX_MULTIRUNNER_API_CA_FILE" \
    --max-time "$SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS" \
    --request GET \
    --header "Authorization: Bearer $SECONDBOX_MULTIRUNNER_API_TOKEN" \
    --header "SecondBox-Generation: $new_generation" \
    --header "SecondBox-Lease-ID: $new_lease" \
    --output "$restored_marker" \
    --write-out '%{http_code}' \
    "$SECONDBOX_MULTIRUNNER_API_URL/v1/sandboxes/$placement_one/files?path=qualification-marker"
)"
[[ "$restored_status" == "200" && "$(cat "$restored_marker")" == "$marker" ]] ||
  fail_multirunner "cross-Runner restore did not preserve exact workspace bytes"
write_scenario_artifact \
  stopped-sandbox-relocation \
  "A stopped checkpointed Sandbox restored exact workspace bytes on the other Runner." \
  "$(jq -cn \
      --arg checkpointId "$checkpoint_id" \
      --arg markerSHA256 "$(sha256sum "$restored_marker" | awk '{print $1}')" \
      --argjson source "$source_assignment" \
      --argjson relocated "$relocated_assignment" \
      '{checkpointId: $checkpointId, markerSHA256: $markerSHA256, source: $source, relocated: $relocated}')"

stale_status="$(
  curl --silent --show-error --proto '=https' \
    --cacert "$SECONDBOX_MULTIRUNNER_API_CA_FILE" \
    --max-time "$SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS" \
    --request POST \
    --header "Authorization: Bearer $SECONDBOX_MULTIRUNNER_API_TOKEN" \
    --header "SecondBox-Generation: $source_generation" \
    --header "SecondBox-Lease-ID: $source_lease" \
    --header "Idempotency-Key: multirunner-stale-touch-$source_commit" \
    --output "$working_directory/stale-touch.json" \
    --write-out '%{http_code}' \
    "$SECONDBOX_MULTIRUNNER_API_URL/v1/sandboxes/$placement_one:touch"
)"
[[ "$stale_status" == "409" ]] ||
  fail_multirunner "old-generation Lease remained authorized after relocation"
exclusive_materializations="$(
  psql "$SECONDBOX_MULTIRUNNER_DATABASE_URL" \
    --no-psqlrc --set ON_ERROR_STOP=1 --tuples-only --no-align \
    --set sandbox_id="$placement_one" \
    --command "
      SELECT count(*)
      FROM secondbox.workspace_materializations
      WHERE sandbox_id=:'sandbox_id' AND state IN ('preparing','ready')"
)"
[[ "$exclusive_materializations" == "1" ]] ||
  fail_multirunner "relocated workspace has non-exclusive active materializations"
[[ "$new_generation" -gt "$source_generation" ]] ||
  fail_multirunner "relocation did not advance Sandbox generation"
[[ "$(jq -r '.fencingTokenSHA256' <<<"$source_assignment")" != \
   "$(jq -r '.fencingTokenSHA256' <<<"$relocated_assignment")" ]] ||
  fail_multirunner "relocation reused the old fencing token"
write_scenario_artifact \
  cross-runner-generation-fencing \
  "Relocation advanced generation and fence, fenced the old Lease, and retained one active materialization." \
  "$(jq -cn \
      --argjson oldGeneration "$source_generation" \
      --argjson newGeneration "$new_generation" \
      --arg staleLeaseHTTPStatus "$stale_status" \
      --argjson activeMaterializations "$exclusive_materializations" \
      --arg oldFenceSHA256 "$(jq -r '.fencingTokenSHA256' <<<"$source_assignment")" \
      --arg newFenceSHA256 "$(jq -r '.fencingTokenSHA256' <<<"$relocated_assignment")" \
      '{
        oldGeneration: $oldGeneration,
        newGeneration: $newGeneration,
        staleLeaseHTTPStatus: $staleLeaseHTTPStatus,
        activeMaterializations: $activeMaterializations,
        oldFenceSHA256: $oldFenceSHA256,
        newFenceSHA256: $newFenceSHA256
      }')"

host_for_runner() {
  local runner_id="$1"
  local field="$2"
  if [[ "$runner_id" == "$SECONDBOX_MULTIRUNNER_RUNNER_A_ID" ]]; then
    case "$field" in
      ssh) printf '%s\n' "$SECONDBOX_MULTIRUNNER_HOST_A_SSH" ;;
      service) printf '%s\n' "$SECONDBOX_MULTIRUNNER_HOST_A_SERVICE" ;;
      environment) printf '%s\n' "$SECONDBOX_MULTIRUNNER_HOST_A_ENVIRONMENT_FILE" ;;
    esac
  elif [[ "$runner_id" == "$SECONDBOX_MULTIRUNNER_RUNNER_B_ID" ]]; then
    case "$field" in
      ssh) printf '%s\n' "$SECONDBOX_MULTIRUNNER_HOST_B_SSH" ;;
      service) printf '%s\n' "$SECONDBOX_MULTIRUNNER_HOST_B_SERVICE" ;;
      environment) printf '%s\n' "$SECONDBOX_MULTIRUNNER_HOST_B_ENVIRONMENT_FILE" ;;
    esac
  else
    fail_multirunner "assignment names an unexpected Runner: $runner_id"
  fi
}

crash_sandbox="$drain_probe"
crash_assignment="$drain_probe_assignment"
crash_runner="$(jq -r '.runnerId' <<<"$crash_assignment")"
crash_ssh="$(host_for_runner "$crash_runner" ssh)"
crash_service="$(host_for_runner "$crash_runner" service)"
ssh "${ssh_options[@]}" "$crash_ssh" sudo -n sh -s -- "$crash_service" <<'REMOTE_CRASH'
set -eu
service="$1"
systemctl kill --kill-who=main --signal=KILL "$service"
systemctl stop "$service"
REMOTE_CRASH
wait_runner_state "$crash_runner" offline
crash_state="$(
  psql "$SECONDBOX_MULTIRUNNER_DATABASE_URL" \
    --no-psqlrc --set ON_ERROR_STOP=1 --tuples-only --no-align \
    --set assignment_id="$(jq -r '.assignmentId' <<<"$crash_assignment")" \
    --command "SELECT state FROM secondbox.assignments WHERE id=:'assignment_id'"
)"
[[ "$crash_state" == "uncertain" ]] ||
  fail_multirunner "crashed Runner Assignment did not become uncertain"
assignment_count_before="$(
  psql "$SECONDBOX_MULTIRUNNER_DATABASE_URL" --no-psqlrc --set ON_ERROR_STOP=1 \
    --tuples-only --no-align --set sandbox_id="$crash_sandbox" \
    --command "SELECT count(*) FROM secondbox.assignments WHERE sandbox_id=:'sandbox_id'"
)"
[[ "$assignment_count_before" == "1" ]] ||
  fail_multirunner "Runner crash authorized replacement before fencing proof"
ssh "${ssh_options[@]}" "$crash_ssh" sudo -n systemctl start "$crash_service"
wait_runner_state "$crash_runner" ready
write_scenario_artifact \
  runner-crash \
  "A killed Runner became offline and uncertain without creating a replacement before fence proof." \
  "$(jq -cn \
      --arg runnerId "$crash_runner" \
      --arg assignmentState "$crash_state" \
      --argjson assignmentCount "$assignment_count_before" \
      '{runnerId: $runnerId, assignmentState: $assignmentState, assignmentCountBeforeFenceProof: $assignmentCount}')"

stale_runner="$(jq -r '.runnerId' <<<"$source_assignment")"
stale_expected_version="$(
  jq -er --arg runner_id "$stale_runner" \
    '.[] | select(.runner_id == $runner_id) | .software_version' \
    <<<"$runner_identity_json"
)"
stale_ssh="$(host_for_runner "$stale_runner" ssh)"
stale_service="$(host_for_runner "$stale_runner" service)"
stale_environment="$(host_for_runner "$stale_runner" environment)"
probe_remote_path="/tmp/secondbox-stale-runner-probe-$source_commit"
probe_input_remote_path="/tmp/secondbox-stale-runner-input-$source_commit.json"
probe_input="$working_directory/stale-runner-input.json"
jq -n \
  --arg assignmentId "$(jq -r '.assignmentId' <<<"$source_assignment")" \
  --arg sandboxId "$(jq -r '.sandboxId' <<<"$source_assignment")" \
  --arg instanceId "$(jq -r '.instanceId' <<<"$source_assignment")" \
  --argjson generation "$(jq -r '.generation' <<<"$source_assignment")" \
  --arg fencingTokenBase64 "$(jq -r '.fencingTokenBase64' <<<"$source_assignment")" \
  --arg requestId "multirunner-stale-probe" \
  --arg operationId "multirunner-stale-operation" \
  '{
    assignmentId: $assignmentId,
    sandboxId: $sandboxId,
    instanceId: $instanceId,
    generation: $generation,
    fencingTokenBase64: $fencingTokenBase64,
    requestId: $requestId,
    operationId: $operationId
  }' >"$probe_input"
scp "${ssh_options[@]}" \
  "$working_directory/secondbox-stale-runner-probe" \
  "$stale_ssh:$probe_remote_path"
scp "${ssh_options[@]}" \
  "$probe_input" \
  "$stale_ssh:$probe_input_remote_path"
stale_probe_output="$(
  ssh "${ssh_options[@]}" "$stale_ssh" sudo -n sh -s -- \
    "$stale_service" "$stale_environment" "$probe_remote_path" "$probe_input_remote_path" \
    "$SECONDBOX_MULTIRUNNER_PROBE_TIMEOUT_SECONDS" <<'REMOTE_STALE_PROBE'
set -eu
service="$1"
environment_file="$2"
probe="$3"
input="$4"
timeout_seconds="$5"
cleanup() {
  status="$?"
  trap - EXIT
  rm -f -- "$probe" "$input"
  systemctl start "$service"
  exit "$status"
}
trap cleanup EXIT
systemctl stop "$service"
set -a
. "$environment_file"
set +a
chmod 700 "$probe"
"$probe" --input "$input" --timeout "${timeout_seconds}s"
REMOTE_STALE_PROBE
)"
[[ "$stale_probe_output" == *"generation-fenced rejection"* ]] ||
  fail_multirunner "stale Runner probe returned no fencing observation"
wait_runner_software_version "$stale_runner" "$stale_expected_version"
stale_runner_state="$(
  psql "$SECONDBOX_MULTIRUNNER_DATABASE_URL" \
    --no-psqlrc --set ON_ERROR_STOP=1 --tuples-only --no-align \
    --set runner_id="$stale_runner" \
    --command "SELECT state FROM secondbox.runners WHERE id=:'runner_id'"
)"
case "$stale_runner_state" in
  drained)
    ;;
  ready)
    SECONDBOX_MULTIRUNNER_DATABASE_URL="$SECONDBOX_MULTIRUNNER_DATABASE_URL" \
      "$working_directory/secondbox-multirunner-admin" request-runner-drain \
        --runner-id "$stale_runner" \
        --deadline-seconds "$SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS"
    wait_runner_state "$stale_runner" drained
    ;;
  *)
    fail_multirunner "packaged Runner restored in unexpected state $stale_runner_state after stale probe"
    ;;
esac
write_scenario_artifact \
  stale-runner-rejection \
  "The authenticated Runner protocol rejected an old-generation Assignment result with stale fencing." \
  "$(jq -cn \
      --arg runnerId "$stale_runner" \
      --arg assignmentId "$(jq -r '.assignmentId' <<<"$source_assignment")" \
      --arg oldFenceSHA256 "$(jq -r '.fencingTokenSHA256' <<<"$source_assignment")" \
      --arg probeObservation "$stale_probe_output" \
      '{
        runnerId: $runnerId,
        staleAssignmentId: $assignmentId,
        oldFenceSHA256: $oldFenceSHA256,
        probeObservation: $probeObservation
      }')"

completed_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
node "$repo_root/scripts/generate-multirunner-qualification-record.mjs" \
  "$SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST" \
  "$SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY" \
  "$record_path" \
  "$started_at" \
  "$completed_at" \
  "$SECONDBOX_RELEASE_QUALIFICATION_HOSTS_FILE" \
  "$SECONDBOX_MULTIRUNNER_HOST_A_ID" \
  "$SECONDBOX_MULTIRUNNER_HOST_B_ID" \
  "$SECONDBOX_MULTIRUNNER_CONTROLLER_HOST_ID"
node "$repo_root/scripts/verify-release-qualification-record.mjs" \
  "$SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST" \
  "$record_path" \
  multi-runner \
  "$SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY"

trap - EXIT
rm -rf -- "$working_directory"
echo "SecondBox multi-Runner qualification passed for $release_version ($source_commit)"
