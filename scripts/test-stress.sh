#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
local_root="${SECONDBOX_STRESS_LOCAL_ROOT:-$repo_root/.secondbox/stress}"
cd "$repo_root"

export SECONDBOX_REQUIRE_QUALIFIED_STRESS="${SECONDBOX_REQUIRE_QUALIFIED_STRESS:-1}"
if [[ "$SECONDBOX_REQUIRE_QUALIFIED_STRESS" != "1" ]]; then
  echo "SECONDBOX_REQUIRE_QUALIFIED_STRESS must be 1" >&2
  exit 1
fi

uses_local_preparation=false
uses_local_public_key=false
if [[ -z "${SECONDBOX_STRESS_CONFIG:-}" ]]; then
  export SECONDBOX_STRESS_CONFIG="$local_root/stress.json"
  uses_local_preparation=true
fi
if [[ -z "${SECONDBOX_STRESS_OUTPUT:-}" ]]; then
  export SECONDBOX_STRESS_OUTPUT="$local_root/results/stress-$(date -u +%Y%m%dT%H%M%SZ)-$$.json"
  uses_local_preparation=true
fi
if [[ -z "${SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR:-}" ]]; then
  export SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR="$local_root/artifacts"
  uses_local_preparation=true
fi
if [[ -z "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY:-}" ]]; then
  export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY="$local_root/trust/manifest-public.pem"
  uses_local_preparation=true
  uses_local_public_key=true
fi
if [[ -z "${SECONDBOX_RUNNER_WORKSPACE_ROOT:-}" ]]; then
  export SECONDBOX_RUNNER_WORKSPACE_ROOT="$local_root/workspaces"
  uses_local_preparation=true
fi

if [[ "$uses_local_preparation" == "true" ]]; then
  if [[ "$local_root" != /* || -L "$local_root" || ! -d "$local_root" ||
        "$(realpath -e "$local_root")" != "$local_root" ]]; then
    echo "SecondBox local stress state is not prepared; run: just prepare-stress" >&2
    exit 1
  fi
fi

for command in date jq openssl realpath sha256sum; do
  command -v "$command" >/dev/null 2>&1 ||
  {
    echo "SecondBox stress qualification prerequisite missing: $command" >&2
    exit 1
  }
done

if [[ -z "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256:-}" &&
      "$uses_local_public_key" == "true" ]]; then
  fingerprint_file="$local_root/trust/manifest-public.sha256"
  if [[ ! -f "$fingerprint_file" || -L "$fingerprint_file" ]]; then
    echo "SecondBox stress trust fingerprint is missing; run: just prepare-stress" >&2
    exit 1
  fi
  export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256="$(<"$fingerprint_file")"
elif [[ -z "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256:-}" ]]; then
  echo "SecondBox stress qualification missing required variable: SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256" >&2
  exit 1
fi
[[ "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256" =~ ^[0-9a-f]{64}$ ]] ||
  {
    echo "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256 must be 64 lowercase hex characters" >&2
    exit 1
  }

if [[ "$SECONDBOX_STRESS_CONFIG" != /* ||
      -L "$SECONDBOX_STRESS_CONFIG" ||
      ! -f "$SECONDBOX_STRESS_CONFIG" ]]; then
  echo "SECONDBOX_STRESS_CONFIG must be an absolute regular non-symbolic-link file" >&2
  exit 1
fi
if [[ "$SECONDBOX_STRESS_OUTPUT" != /* ||
      -e "$SECONDBOX_STRESS_OUTPUT" ||
      -L "$SECONDBOX_STRESS_OUTPUT" ]]; then
  echo "SECONDBOX_STRESS_OUTPUT must be an absent absolute path" >&2
  exit 1
fi
output_parent="$(dirname "$SECONDBOX_STRESS_OUTPUT")"
if [[ -L "$output_parent" || ! -d "$output_parent" ]]; then
  echo "SECONDBOX_STRESS_OUTPUT parent must be an existing non-symbolic-link directory" >&2
  exit 1
fi

go run ./tests/scenario/stress \
  --mode validate \
  --config "$SECONDBOX_STRESS_CONFIG"

export SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1
export SECONDBOX_SCENARIO_MODE=stress
export SECONDBOX_RUNNER_ID=secondbox-stress-runner
export SECONDBOX_RUNNER_POOL_ID
SECONDBOX_RUNNER_POOL_ID="$(jq -er '.runnerPoolName' "$SECONDBOX_STRESS_CONFIG")"

export SECONDBOX_SCENARIO_SUBJECT_MAX_SANDBOXES
SECONDBOX_SCENARIO_SUBJECT_MAX_SANDBOXES="$(jq -er '.subjectMaxSandboxes' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_SUBJECT_MAX_ACTIVE_INSTANCES
SECONDBOX_SCENARIO_SUBJECT_MAX_ACTIVE_INSTANCES="$(jq -er '.subjectMaxActiveInstances' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_SUBJECT_MAX_CONCURRENT_OPERATIONS
SECONDBOX_SCENARIO_SUBJECT_MAX_CONCURRENT_OPERATIONS="$(jq -er '.subjectMaxConcurrentOperations' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_SUBJECT_MAX_SNAPSHOTS
SECONDBOX_SCENARIO_SUBJECT_MAX_SNAPSHOTS="$(jq -er '.subjectMaxSnapshots' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_SUBJECT_MAX_PORT_SESSIONS
SECONDBOX_SCENARIO_SUBJECT_MAX_PORT_SESSIONS="$(jq -er '.subjectMaxPortSessions' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_SUBJECT_MAX_CPU_MILLIS
SECONDBOX_SCENARIO_SUBJECT_MAX_CPU_MILLIS="$(jq -er '.subjectMaxCpuMillis' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_SUBJECT_MAX_MEMORY_BYTES
SECONDBOX_SCENARIO_SUBJECT_MAX_MEMORY_BYTES="$(jq -er '.subjectMaxMemoryBytes' "$SECONDBOX_STRESS_CONFIG")"

export SECONDBOX_SCENARIO_STORAGE_RECOVERY_PERCENT
SECONDBOX_SCENARIO_STORAGE_RECOVERY_PERCENT="$(jq -er '.runner.storagePressureRecoveryPercent' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_STORAGE_WARNING_PERCENT
SECONDBOX_SCENARIO_STORAGE_WARNING_PERCENT="$(jq -er '.runner.storagePressureWarningPercent' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_STORAGE_DENY_PERCENT
SECONDBOX_SCENARIO_STORAGE_DENY_PERCENT="$(jq -er '.runner.storagePressureAdmissionDenyPercent' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_SANDBOX_MAX_VCPUS
SECONDBOX_SCENARIO_SANDBOX_MAX_VCPUS="$(jq -er '.runner.sandboxMaxVcpus' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_SANDBOX_MEMORY_MIB
SECONDBOX_SCENARIO_SANDBOX_MEMORY_MIB="$(jq -er '.runner.sandboxMemoryMiB' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_SANDBOX_DISK_MIB
SECONDBOX_SCENARIO_SANDBOX_DISK_MIB="$(jq -er '.runner.sandboxDiskMiB' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_MEMORY_BUDGET_MIB
SECONDBOX_SCENARIO_MEMORY_BUDGET_MIB="$(jq -er '.runner.memoryBudgetMiB' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_MAX_CONCURRENT_PER_SANDBOX
SECONDBOX_SCENARIO_MAX_CONCURRENT_PER_SANDBOX="$(jq -er '.runner.maxConcurrentPerSandbox' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_MAX_CONCURRENT_GLOBAL
SECONDBOX_SCENARIO_MAX_CONCURRENT_GLOBAL="$(jq -er '.runner.maxConcurrentGlobal' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_MAX_CONCURRENT_STARTS
SECONDBOX_SCENARIO_MAX_CONCURRENT_STARTS="$(jq -er '.runner.maxConcurrentStarts' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_SCENARIO_MAX_CONCURRENT_WORKSPACE_CREATES
SECONDBOX_SCENARIO_MAX_CONCURRENT_WORKSPACE_CREATES="$(
  jq -er '.runner.maxConcurrentWorkspaceCreates' "$SECONDBOX_STRESS_CONFIG"
)"
export SECONDBOX_SCENARIO_MAX_CONCURRENT_OPERATIONS_GLOBAL
SECONDBOX_SCENARIO_MAX_CONCURRENT_OPERATIONS_GLOBAL="$(
  jq -er '.runner.maxConcurrentOperationsGlobal' "$SECONDBOX_STRESS_CONFIG"
)"
export SECONDBOX_SCENARIO_FILE_TRANSFER_MAX_BYTES
SECONDBOX_SCENARIO_FILE_TRANSFER_MAX_BYTES="$(jq -er '.runner.fileTransferMaxBytes' "$SECONDBOX_STRESS_CONFIG")"

export SECONDBOX_SCENARIO_HTTP_TIMEOUT_SECONDS=70
export SECONDBOX_SCENARIO_ASSIGNMENT_CLAIM_MILLISECONDS=30000
export SECONDBOX_SCENARIO_ASSIGNMENT_DEADLINE_MILLISECONDS=120000
export SECONDBOX_SCENARIO_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS=30000

exec "$repo_root/scripts/test-scenario.sh"
