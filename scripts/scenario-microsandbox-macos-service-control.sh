#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

fail() {
  echo "SecondBox macOS scenario service control failed: $*" >&2
  exit 1
}

for name in \
  SECONDBOX_SCENARIO_COMPOSE_PROJECT \
  SECONDBOX_SCENARIO_COMPOSE_FILE \
  SECONDBOX_SCENARIO_NATIVE_RUNNER_BUILD \
  SECONDBOX_SCENARIO_NATIVE_ADVERTISED_ADDRESS \
  SECONDBOX_SCENARIO_IDENTITY_DIR \
  SECONDBOX_SCENARIO_STATE_DIR \
  SECONDBOX_SCENARIO_WORKSPACE_DIR \
  SECONDBOX_SCENARIO_RELOCATION_IDENTITY_DIR \
  SECONDBOX_SCENARIO_RELOCATION_STATE_DIR \
  SECONDBOX_SCENARIO_RELOCATION_WORKSPACE_DIR; do
  [[ -n "${!name:-}" ]] || fail "$name is required"
done

if docker compose version >/dev/null 2>&1; then
  compose=(docker compose)
elif command -v docker-compose >/dev/null 2>&1 && docker-compose version >/dev/null 2>&1; then
  compose=(docker-compose)
else
  fail "Docker Compose v2 is required"
fi
compose+=(
  --project-name "$SECONDBOX_SCENARIO_COMPOSE_PROJECT"
  --file "$SECONDBOX_SCENARIO_COMPOSE_FILE"
)

runner_binary="$SECONDBOX_SCENARIO_NATIVE_RUNNER_BUILD/runtime/bin/secondbox-runner"
helper_binary="$SECONDBOX_SCENARIO_NATIVE_RUNNER_BUILD/runtime/bin/secondbox-microsandbox-helper"
firmware="$SECONDBOX_SCENARIO_NATIVE_RUNNER_BUILD/runtime/lib/libkrunfw.5.dylib"
agentd="$SECONDBOX_SCENARIO_NATIVE_RUNNER_BUILD/runtime/bin/agentd"
flat_root="$SECONDBOX_SCENARIO_NATIVE_RUNNER_BUILD/rootfs"
for path in "$runner_binary" "$helper_binary" "$firmware" "$agentd"; do
  [[ -f "$path" && ! -L "$path" ]] || fail "native runtime input is invalid: $path"
done
[[ -x "$runner_binary" && -x "$helper_binary" && -x "$agentd" && -d "$flat_root" ]] ||
  fail "native runtime bundle is incomplete"

service_paths() {
  case "$1" in
    secondbox-runner)
      service_runner_id="$SECONDBOX_RUNNER_ID"
      service_identity="$SECONDBOX_SCENARIO_IDENTITY_DIR"
      service_state="$SECONDBOX_SCENARIO_STATE_DIR"
      service_workspace="$SECONDBOX_SCENARIO_WORKSPACE_DIR"
      service_port="$SECONDBOX_SCENARIO_RUNNER_DATA_PLANE_PORT"
      ;;
    secondbox-runner-relocation)
      service_runner_id="$SECONDBOX_SCENARIO_RELOCATION_RUNNER_ID"
      service_identity="$SECONDBOX_SCENARIO_RELOCATION_IDENTITY_DIR"
      service_state="$SECONDBOX_SCENARIO_RELOCATION_STATE_DIR"
      service_workspace="$SECONDBOX_SCENARIO_RELOCATION_WORKSPACE_DIR"
      service_port="$SECONDBOX_SCENARIO_RELOCATION_RUNNER_DATA_PLANE_PORT"
      ;;
    *) fail "unknown native runner service $1" ;;
  esac
  service_pid_file="$service_state/runner.pid"
  service_helper_pid_file="$service_state/helper-pids-at-stop"
  service_log_dir="$service_state/log"
  service_json_log="$service_log_dir/runner.jsonl"
  service_process_log="$service_log_dir/process.log"
}

protocol_environment() {
  printf '%s\0' \
    "PATH=/opt/homebrew/opt/e2fsprogs/bin:/opt/homebrew/opt/e2fsprogs/sbin:/usr/bin:/bin:/usr/sbin:/sbin" \
    "TMPDIR=/tmp" \
    "SECONDBOX_COMPUTE_BACKEND=microsandbox" \
    "SECONDBOX_RUNNER_ID=$service_runner_id" \
    "SECONDBOX_RUNNER_POOL_ID=$SECONDBOX_RUNNER_POOL_ID" \
    "SECONDBOX_RUNNER_SOFTWARE_VERSION=$SECONDBOX_SCENARIO_SOURCE_COMMIT" \
    "SECONDBOX_RUNNER_CONTROL_PLANE_ADDRESS=127.0.0.1:$SECONDBOX_SCENARIO_RUNNER_PORT" \
    "SECONDBOX_RUNNER_CONTROL_PLANE_SERVER_NAME=localhost" \
    "SECONDBOX_RUNNER_CREDENTIAL=$SECONDBOX_SCENARIO_RUNNER_CREDENTIAL" \
    "SECONDBOX_RUNNER_CLIENT_CERTIFICATE=$service_identity/runner.crt" \
    "SECONDBOX_RUNNER_CLIENT_KEY=$service_identity/runner.key" \
    "SECONDBOX_RUNNER_CONTROL_PLANE_CA=$service_identity/runner-ca.crt" \
    "SECONDBOX_RUNNER_ENABLED_FEATURES=exec-streaming,file-streaming,pty,evidence,local-workspace,port-proxy" \
    "SECONDBOX_RUNNER_HEARTBEAT_INTERVAL_MILLISECONDS=1000" \
    "SECONDBOX_RUNNER_DATA_PLANE_LISTEN_ADDRESS=0.0.0.0:$service_port" \
    "SECONDBOX_RUNNER_DATA_PLANE_ADVERTISED_ADDRESS=$SECONDBOX_SCENARIO_NATIVE_ADVERTISED_ADDRESS:$service_port"
}

runtime_environment() {
  protocol_environment
  printf '%s\0' \
    "SECONDBOX_RUNNER_LOG_DIR=$service_log_dir" \
    "SECONDBOX_RUNNER_LOG_PATH=$service_json_log" \
    "SECONDBOX_RUNNER_WORKSPACE_ROOT=$service_workspace" \
    "SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT=$SECONDBOX_SCENARIO_STORAGE_RECOVERY_PERCENT" \
    "SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT=$SECONDBOX_SCENARIO_STORAGE_WARNING_PERCENT" \
    "SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT=$SECONDBOX_SCENARIO_STORAGE_DENY_PERCENT" \
    "SECONDBOX_RUNNER_SANDBOX_MAX_VCPUS=$SECONDBOX_SCENARIO_SANDBOX_MAX_VCPUS" \
    "SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB=$SECONDBOX_SCENARIO_SANDBOX_MEMORY_MIB" \
    "SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB=$SECONDBOX_SCENARIO_SANDBOX_DISK_MIB" \
    "SECONDBOX_RUNNER_SANDBOX_MEMORY_BUDGET_MIB=$SECONDBOX_SCENARIO_MEMORY_BUDGET_MIB" \
    "SECONDBOX_RUNNER_MAX_CONCURRENT_PER_SANDBOX=$SECONDBOX_SCENARIO_MAX_CONCURRENT_PER_SANDBOX" \
    "SECONDBOX_RUNNER_MAX_CONCURRENT_GLOBAL=$SECONDBOX_SCENARIO_MAX_CONCURRENT_GLOBAL" \
    "SECONDBOX_RUNNER_MAX_CONCURRENT_STARTS=$SECONDBOX_SCENARIO_MAX_CONCURRENT_STARTS" \
    "SECONDBOX_RUNNER_MAX_CONCURRENT_WORKSPACE_CREATES=$SECONDBOX_SCENARIO_MAX_CONCURRENT_WORKSPACE_CREATES" \
    "SECONDBOX_RUNNER_MAX_CONCURRENT_OPERATIONS_GLOBAL=$SECONDBOX_SCENARIO_MAX_CONCURRENT_OPERATIONS_GLOBAL" \
    "SECONDBOX_RUNNER_FILE_TRANSFER_MAX_BYTES=$SECONDBOX_SCENARIO_FILE_TRANSFER_MAX_BYTES" \
    "SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL=$SECONDBOX_SCENARIO_RUNNER_GUEST_HEARTBEAT_INTERVAL" \
    "SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE=$helper_binary" \
    "SECONDBOX_MICROSANDBOX_LIBKRUNFW_PATH=$firmware" \
    "SECONDBOX_MICROSANDBOX_AGENTD_PATH=$agentd" \
    "SECONDBOX_MICROSANDBOX_FLAT_ROOT_PATH=$flat_root" \
    "SECONDBOX_MICROSANDBOX_MATERIALIZATION_PATH=$SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION" \
    "SECONDBOX_MICROSANDBOX_MATERIALIZATION_DIGEST=$SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION_DIGEST" \
    "SECONDBOX_MICROSANDBOX_MAXIMUM_VCPUS=$SECONDBOX_SCENARIO_MICROSANDBOX_MAXIMUM_VCPUS" \
    "SECONDBOX_MICROSANDBOX_MAXIMUM_MEMORY_BYTES=$SECONDBOX_SCENARIO_MICROSANDBOX_MAXIMUM_MEMORY_BYTES" \
    "SECONDBOX_MICROSANDBOX_MAXIMUM_DISK_BYTES=$SECONDBOX_SCENARIO_MICROSANDBOX_MAXIMUM_DISK_BYTES" \
    "SECONDBOX_MICROSANDBOX_MAXIMUM_INSTANCES=$SECONDBOX_SCENARIO_MAX_CONCURRENT_GLOBAL" \
    "SECONDBOX_MICROSANDBOX_MAXIMUM_OPERATIONS=$SECONDBOX_SCENARIO_MAX_CONCURRENT_OPERATIONS_GLOBAL" \
    "SECONDBOX_MICROSANDBOX_WORKSPACE_TEMPLATE_CAPACITY_BYTES=$SECONDBOX_SCENARIO_MICROSANDBOX_WORKSPACE_TEMPLATE_CAPACITY_BYTES"
}

read_environment() {
  local producer="$1"
  service_environment=()
  while IFS= read -r -d '' entry; do
    service_environment+=("$entry")
  done < <("$producer")
}

runner_start() {
  local service="$1"
  service_paths "$service"
  mkdir -p -- "$service_log_dir" "$service_workspace"
  if [[ -f "$service_pid_file" ]] && kill -0 "$(cat "$service_pid_file")" 2>/dev/null; then
    return 0
  fi
  rm -f -- "$service_pid_file" "$service_helper_pid_file"
  read_environment runtime_environment
  nohup env "${service_environment[@]}" "$runner_binary" \
    >>"$service_process_log" 2>&1 &
  runner_pid=$!
  printf '%s\n' "$runner_pid" >"$service_pid_file"
  for _ in $(seq 1 120); do
    if ! kill -0 "$runner_pid" 2>/dev/null; then
      tail -n 200 "$service_process_log" >&2 || true
      fail "$service exited before readiness"
    fi
    read_environment protocol_environment
    if env "${service_environment[@]}" "$runner_binary" \
      -healthcheck -healthcheck-timeout 2s \
      >>"$service_log_dir/healthcheck.log" 2>&1; then
      return 0
    fi
    if grep -Fq 'SecondBox runner Registration pool lookup: no rows in result set' \
      "$service_process_log"; then
      return 0
    fi
    if grep -Fq '"kind":"LOCAL_WORKSPACE_COMMAND_KIND_RECONCILE"' \
      "$service_process_log"; then
      return 0
    fi
    sleep 1
  done
  fail "$service did not pass authenticated health within 120 seconds"
}

runner_stop() {
  local service="$1"
  service_paths "$service"
  [[ -f "$service_pid_file" ]] || return 0
  runner_pid="$(cat "$service_pid_file")"
  if ! kill -0 "$runner_pid" 2>/dev/null; then
    rm -f -- "$service_pid_file"
    return 0
  fi
  pgrep -P "$runner_pid" >"$service_helper_pid_file" 2>/dev/null || true
  kill -TERM "$runner_pid"
  for _ in $(seq 1 45); do
    if ! kill -0 "$runner_pid" 2>/dev/null; then
      for helper_pid in $(cat "$service_helper_pid_file" 2>/dev/null || true); do
        kill -0 "$helper_pid" 2>/dev/null &&
          fail "$service left helper process $helper_pid alive after lifecycle EOF"
      done
      rm -f -- "$service_pid_file" "$service_helper_pid_file"
      return 0
    fi
    sleep 1
  done
  kill -KILL "$runner_pid" 2>/dev/null || true
  fail "$service did not stop within 45 seconds"
}

native_logs() {
  local service="$1"
  service_paths "$service"
  if [[ -f "$service_json_log" ]]; then
    cat "$service_json_log"
  elif [[ -f "$service_process_log" ]]; then
    cat "$service_process_log"
  fi
}

strip_profile=()
arguments=("$@")
index=0
while (( index < ${#arguments[@]} )); do
  if [[ "${arguments[$index]}" == "--profile" ]]; then
    (( index + 1 < ${#arguments[@]} )) || fail "--profile lacks a value"
    index=$(( index + 2 ))
    continue
  fi
  strip_profile+=("${arguments[$index]}")
  index=$(( index + 1 ))
done
arguments=("${strip_profile[@]}")
(( ${#arguments[@]} > 0 )) || fail "a command is required"

case "${arguments[0]}" in
  helper-rss-kib)
    (( ${#arguments[@]} == 2 )) || fail "helper-rss-kib requires one PID"
    ps -o rss= -p "${arguments[1]}" | awk '{$1=$1; print}'
    ;;
  start)
    runner_start "${arguments[1]:-}"
    ;;
  stop)
    service="${arguments[${#arguments[@]}-1]}"
    if [[ "$service" == secondbox-runner || "$service" == secondbox-runner-relocation ]]; then
      runner_stop "$service"
    else
      "${compose[@]}" "${arguments[@]}"
    fi
    ;;
  up)
    service="${arguments[${#arguments[@]}-1]}"
    if [[ "$service" == secondbox-runner || "$service" == secondbox-runner-relocation ]]; then
      runner_start "$service"
    else
      "${compose[@]}" "${arguments[@]}"
    fi
    ;;
  logs)
    compose_services=()
    for argument in "${arguments[@]:1}"; do
      case "$argument" in
        secondbox-runner|secondbox-runner-relocation) native_logs "$argument" ;;
        --no-color|--timestamps|--tail|[0-9]*) ;;
        -*) ;;
        *) compose_services+=("$argument") ;;
      esac
    done
    if (( ${#compose_services[@]} > 0 )); then
      "${compose[@]}" logs --no-color "${compose_services[@]}"
    fi
    ;;
  restart)
    "${compose[@]}" "${arguments[@]}"
    ;;
  *)
    "${compose[@]}" "${arguments[@]}"
    ;;
esac
