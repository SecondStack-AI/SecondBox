#!/usr/bin/env bash
set -euo pipefail
umask 077

: "${SECONDBOX_REQUIRE_QUALIFIED_STRESS:?SecondBox stress qualification requires SECONDBOX_REQUIRE_QUALIFIED_STRESS=1}"
if [[ "$SECONDBOX_REQUIRE_QUALIFIED_STRESS" != "1" ]]; then
  echo "SECONDBOX_REQUIRE_QUALIFIED_STRESS must be 1" >&2
  exit 1
fi

for variable in \
  SECONDBOX_STRESS_CONFIG \
  SECONDBOX_STRESS_OUTPUT \
  SECONDBOX_STRESS_API_PORT \
  SECONDBOX_STRESS_RUNNER_PORT \
  SECONDBOX_STRESS_POSTGRES_IMAGE \
  SECONDBOX_STRESS_OBJECT_STORE_IMAGE \
  SECONDBOX_STRESS_OBJECT_STORE_CLIENT_IMAGE \
  SECONDBOX_STRESS_CONTROL_PLANE_BASE_IMAGE \
  SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR \
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY \
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256 \
  SECONDBOX_RUNNER_WORKSPACE_ROOT; do
  if [[ -z "${!variable:-}" ]]; then
    echo "SecondBox stress qualification missing required variable: $variable" >&2
    exit 1
  fi
done

for command in awk chmod curl dirname docker findmnt git go grep id install ip jq mktemp openssl rm sha256sum ss xargs; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "SecondBox stress qualification prerequisite missing: $command" >&2
    exit 1
  fi
done
if ! docker compose version >/dev/null 2>&1; then
  echo "SecondBox stress qualification prerequisite missing: Docker Compose v2" >&2
  exit 1
fi
for device in /dev/kvm /dev/net/tun; do
  if [[ ! -c "$device" || ! -r "$device" || ! -w "$device" ]]; then
    echo "SecondBox stress qualification requires a readable and writable character device: $device" >&2
    exit 1
  fi
done

if [[ "$SECONDBOX_STRESS_CONFIG" != /* || -L "$SECONDBOX_STRESS_CONFIG" || ! -f "$SECONDBOX_STRESS_CONFIG" ]]; then
  echo "SECONDBOX_STRESS_CONFIG must be an absolute regular non-symbolic-link file" >&2
  exit 1
fi
if [[ "$SECONDBOX_STRESS_OUTPUT" != /* || -e "$SECONDBOX_STRESS_OUTPUT" || -L "$SECONDBOX_STRESS_OUTPUT" ]]; then
  echo "SECONDBOX_STRESS_OUTPUT must be an absent absolute path" >&2
  exit 1
fi
output_parent="$(dirname "$SECONDBOX_STRESS_OUTPUT")"
if [[ -L "$output_parent" || ! -d "$output_parent" ]]; then
  echo "SECONDBOX_STRESS_OUTPUT parent must be an existing non-symbolic-link directory" >&2
  exit 1
fi
if [[ "$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR" != /* ||
      -L "$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR" ||
      ! -d "$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR" ]]; then
  echo "SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR must be an absolute non-symbolic-link directory" >&2
  exit 1
fi
if [[ "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY" != /* ||
      -L "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY" ||
      ! -f "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY" ]]; then
  echo "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY must be an absolute regular non-symbolic-link file" >&2
  exit 1
fi
if [[ "$SECONDBOX_RUNNER_WORKSPACE_ROOT" != /* ||
      "$SECONDBOX_RUNNER_WORKSPACE_ROOT" == "/" ||
      -L "$SECONDBOX_RUNNER_WORKSPACE_ROOT" ||
      ! -d "$SECONDBOX_RUNNER_WORKSPACE_ROOT" ]]; then
  echo "SECONDBOX_RUNNER_WORKSPACE_ROOT must be an existing non-root absolute non-symbolic-link directory" >&2
  exit 1
fi
workspace_filesystem="$(findmnt --target "$SECONDBOX_RUNNER_WORKSPACE_ROOT" --noheadings --output FSTYPE | xargs)"
if [[ "$workspace_filesystem" != "xfs" && "$workspace_filesystem" != "btrfs" ]]; then
  echo "SECONDBOX_RUNNER_WORKSPACE_ROOT must use XFS or Btrfs, got $workspace_filesystem" >&2
  exit 1
fi

for port_variable in SECONDBOX_STRESS_API_PORT SECONDBOX_STRESS_RUNNER_PORT; do
  port="${!port_variable}"
  if [[ ! "$port" =~ ^[0-9]+$ ]] || (( port < 1024 || port > 65535 )); then
    echo "$port_variable must be an integer from 1024 through 65535" >&2
    exit 1
  fi
  if [[ -n "$(ss -Hln "sport = :$port")" ]]; then
    echo "$port_variable is already in use: $port" >&2
    exit 1
  fi
done
if [[ "$SECONDBOX_STRESS_API_PORT" == "$SECONDBOX_STRESS_RUNNER_PORT" ]]; then
  echo "SECONDBOX_STRESS_API_PORT and SECONDBOX_STRESS_RUNNER_PORT must differ" >&2
  exit 1
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose_file="$repo_root/scripts/scenario-compose.yml"
cd "$repo_root"

go run ./tests/scenario/stress --mode validate --config "$SECONDBOX_STRESS_CONFIG"
runner/scripts/microvm-image/verify.sh \
  "$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR" \
  "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY" \
  "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"

export SECONDBOX_STRESS_RUNNER_POOL_NAME
SECONDBOX_STRESS_RUNNER_POOL_NAME="$(jq -er '.runnerPoolName' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_FIRECRACKER_KERNEL_ARGS
SECONDBOX_STRESS_FIRECRACKER_KERNEL_ARGS="$(jq -er '.runner.firecrackerKernelArgs' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_FIRECRACKER_CPU_TEMPLATE
SECONDBOX_STRESS_FIRECRACKER_CPU_TEMPLATE="$(jq -er '.runner.firecrackerCpuTemplate' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_STORAGE_RECOVERY_PERCENT
SECONDBOX_STRESS_STORAGE_RECOVERY_PERCENT="$(jq -er '.runner.storagePressureRecoveryPercent' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_STORAGE_WARNING_PERCENT
SECONDBOX_STRESS_STORAGE_WARNING_PERCENT="$(jq -er '.runner.storagePressureWarningPercent' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_STORAGE_DENY_PERCENT
SECONDBOX_STRESS_STORAGE_DENY_PERCENT="$(jq -er '.runner.storagePressureAdmissionDenyPercent' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_SANDBOX_MAX_VCPUS
SECONDBOX_STRESS_SANDBOX_MAX_VCPUS="$(jq -er '.runner.sandboxMaxVcpus' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_SANDBOX_MEMORY_MIB
SECONDBOX_STRESS_SANDBOX_MEMORY_MIB="$(jq -er '.runner.sandboxMemoryMiB' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_SANDBOX_DISK_MIB
SECONDBOX_STRESS_SANDBOX_DISK_MIB="$(jq -er '.runner.sandboxDiskMiB' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_MEMORY_BUDGET_MIB
SECONDBOX_STRESS_MEMORY_BUDGET_MIB="$(jq -er '.runner.memoryBudgetMiB' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_MAX_CONCURRENT_PER_SANDBOX
SECONDBOX_STRESS_MAX_CONCURRENT_PER_SANDBOX="$(jq -er '.runner.maxConcurrentPerSandbox' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_MAX_CONCURRENT_GLOBAL
SECONDBOX_STRESS_MAX_CONCURRENT_GLOBAL="$(jq -er '.runner.maxConcurrentGlobal' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_FILE_TRANSFER_MAX_BYTES
SECONDBOX_STRESS_FILE_TRANSFER_MAX_BYTES="$(jq -er '.runner.fileTransferMaxBytes' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_BRIDGE_NAME
SECONDBOX_STRESS_BRIDGE_NAME="$(jq -er '.runner.bridgeName' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_BRIDGE_CIDR
SECONDBOX_STRESS_BRIDGE_CIDR="$(jq -er '.runner.bridgeCIDR' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_BRIDGE_ADDRESS="${SECONDBOX_STRESS_BRIDGE_CIDR%/*}"
export SECONDBOX_STRESS_GUEST_CIDR
SECONDBOX_STRESS_GUEST_CIDR="$(jq -er '.runner.guestCIDR' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_GUEST_IP
SECONDBOX_STRESS_GUEST_IP="$(jq -er '.runner.guestIP' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_TAP_PREFIX
SECONDBOX_STRESS_TAP_PREFIX="$(jq -er '.runner.tapPrefix' "$SECONDBOX_STRESS_CONFIG")"

if ip link show "$SECONDBOX_STRESS_BRIDGE_NAME" >/dev/null 2>&1; then
  echo "SecondBox stress bridge already exists; choose a fresh runner.bridgeName: $SECONDBOX_STRESS_BRIDGE_NAME" >&2
  exit 1
fi

export SECONDBOX_STRESS_SUBJECT_MAX_SANDBOXES
SECONDBOX_STRESS_SUBJECT_MAX_SANDBOXES="$(jq -er '.subjectMaxSandboxes' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_SUBJECT_MAX_ACTIVE_INSTANCES
SECONDBOX_STRESS_SUBJECT_MAX_ACTIVE_INSTANCES="$(jq -er '.subjectMaxActiveInstances' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_SUBJECT_MAX_CONCURRENT_OPERATIONS
SECONDBOX_STRESS_SUBJECT_MAX_CONCURRENT_OPERATIONS="$(jq -er '.subjectMaxConcurrentOperations' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_SUBJECT_MAX_SNAPSHOTS
SECONDBOX_STRESS_SUBJECT_MAX_SNAPSHOTS="$(jq -er '.subjectMaxSnapshots' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_SUBJECT_MAX_ARTIFACT_BYTES
SECONDBOX_STRESS_SUBJECT_MAX_ARTIFACT_BYTES="$(jq -er '.subjectMaxArtifactBytes' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_SUBJECT_MAX_ARTIFACTS
SECONDBOX_STRESS_SUBJECT_MAX_ARTIFACTS="$(jq -er '.subjectMaxArtifacts' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_SUBJECT_MAX_PORT_SESSIONS
SECONDBOX_STRESS_SUBJECT_MAX_PORT_SESSIONS="$(jq -er '.subjectMaxPortSessions' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_SUBJECT_MAX_CPU_MILLIS
SECONDBOX_STRESS_SUBJECT_MAX_CPU_MILLIS="$(jq -er '.subjectMaxCpuMillis' "$SECONDBOX_STRESS_CONFIG")"
export SECONDBOX_STRESS_SUBJECT_MAX_MEMORY_BYTES
SECONDBOX_STRESS_SUBJECT_MAX_MEMORY_BYTES="$(jq -er '.subjectMaxMemoryBytes' "$SECONDBOX_STRESS_CONFIG")"

export SECONDBOX_STRESS_RUNTIME_BUNDLE_DIGEST
SECONDBOX_STRESS_RUNTIME_BUNDLE_DIGEST="$(jq -er '.runtimeBundle.manifestDigest' "$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR/manifest.json")"
export SECONDBOX_STRESS_TOOLCHAIN_BUNDLE_DIGEST
SECONDBOX_STRESS_TOOLCHAIN_BUNDLE_DIGEST="$(jq -er '.toolchainBundle.manifestDigest' "$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR/manifest.json")"
export SECONDBOX_STRESS_ARTIFACT_MANIFEST_DIGEST
SECONDBOX_STRESS_ARTIFACT_MANIFEST_DIGEST="sha256:$(sha256sum "$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR/manifest.json" | awk '{print $1}')"
export SECONDBOX_STRESS_SOURCE_COMMIT
SECONDBOX_STRESS_SOURCE_COMMIT="$(git rev-parse HEAD)"
export SECONDBOX_STRESS_GO_VERSION
SECONDBOX_STRESS_GO_VERSION="$(go version)"

run_root="$(mktemp -d "$SECONDBOX_RUNNER_WORKSPACE_ROOT/secondbox-stress.XXXXXX")"
project_name="secondbox-stress-$$"
export SECONDBOX_STRESS_PKI_DIR="$run_root/pki"
export SECONDBOX_STRESS_RUNNER_IDENTITY_DIR="$run_root/runner-identity"
export SECONDBOX_STRESS_RUNNER_STATE_DIR="$run_root/runner-state"
export SECONDBOX_STRESS_RUNNER_WORKSPACE_DIR="$run_root/runner-workspaces"
export SECONDBOX_STRESS_CONTROL_PLANE_BINARY="$run_root/secondboxd"
export SECONDBOX_STRESS_ASSET_CATALOG="$run_root/signed-assets.json"
install -d -m 0700 \
  "$SECONDBOX_STRESS_PKI_DIR" \
  "$SECONDBOX_STRESS_RUNNER_STATE_DIR" \
  "$SECONDBOX_STRESS_RUNNER_WORKSPACE_DIR"

export SECONDBOX_STRESS_UID
SECONDBOX_STRESS_UID="$(id -u)"
export SECONDBOX_STRESS_GID
SECONDBOX_STRESS_GID="$(id -g)"
export SECONDBOX_STRESS_PLATFORM_TOKEN
SECONDBOX_STRESS_PLATFORM_TOKEN="$(openssl rand -hex 24)"
export SECONDBOX_STRESS_RUNNER_CREDENTIAL
SECONDBOX_STRESS_RUNNER_CREDENTIAL="$(openssl rand -hex 32)"
export SECONDBOX_STRESS_POSTGRES_PASSWORD
SECONDBOX_STRESS_POSTGRES_PASSWORD="$(openssl rand -hex 24)"
export SECONDBOX_STRESS_OBJECT_STORE_USER
SECONDBOX_STRESS_OBJECT_STORE_USER="stress$(openssl rand -hex 8)"
export SECONDBOX_STRESS_OBJECT_STORE_PASSWORD
SECONDBOX_STRESS_OBJECT_STORE_PASSWORD="$(openssl rand -hex 24)"

compose() {
  docker compose --project-name "$project_name" --file "$compose_file" "$@"
}

compose_started=0
cleanup() {
  status="$?"
  trap - EXIT
  if [[ "$status" -ne 0 && "$compose_started" -eq 1 ]]; then
    if ! compose logs control-plane secondbox-runner postgres object-store >&2; then
      echo "SecondBox stress qualification could not collect failure logs" >&2
    fi
  fi
  if [[ "$compose_started" -eq 1 ]]; then
    if compose ps --all --quiet secondbox-runner | grep -q .; then
      if compose ps --status running --quiet secondbox-runner | grep -q .; then
        if ! compose exec -T secondbox-runner systemctl stop secondbox-runner.service; then
          echo "SecondBox stress qualification could not stop the runner service cleanly" >&2
          status=1
        fi
      fi
      if ! compose stop --timeout 45 secondbox-runner >/dev/null; then
        echo "SecondBox stress qualification could not stop the runner container cleanly" >&2
        status=1
      fi
    fi
    if runner_image="$(compose images --quiet secondbox-runner)"; then
      if [[ -n "$runner_image" ]]; then
        if ! compose run --rm --no-deps \
          --entrypoint /usr/local/bin/microvm-host-network-setup secondbox-runner remove; then
          echo "SecondBox stress qualification could not remove its host network" >&2
          status=1
        fi
        if ! compose run --rm --no-deps --entrypoint /bin/chown secondbox-runner \
          -R "$SECONDBOX_STRESS_UID:$SECONDBOX_STRESS_GID" /var/lib/secondbox-runner >/dev/null; then
          echo "SecondBox stress qualification could not restore ownership of its run directory" >&2
          status=1
        fi
      fi
    else
      echo "SecondBox stress qualification could not inspect the runner image for cleanup" >&2
      status=1
    fi
    if ! compose down --volumes --remove-orphans >/dev/null 2>&1; then
      echo "SecondBox stress qualification Compose cleanup failed for $project_name" >&2
      status=1
    fi
  fi
  if [[ -d "$run_root" ]]; then
    if ! rm -r -- "$run_root"; then
      echo "SecondBox stress qualification run-directory cleanup failed: $run_root" >&2
      status=1
    fi
  fi
  exit "$status"
}
trap cleanup EXIT

openssl req -x509 -newkey rsa:3072 -nodes \
  -keyout "$SECONDBOX_STRESS_PKI_DIR/runner-ca.key" \
  -out "$SECONDBOX_STRESS_PKI_DIR/runner-ca.crt" \
  -days 2 \
  -subj "/CN=SecondBox Stress Runner CA" >/dev/null 2>&1
openssl req -new -newkey rsa:3072 -nodes \
  -keyout "$SECONDBOX_STRESS_PKI_DIR/server.key" \
  -out "$SECONDBOX_STRESS_PKI_DIR/server.csr" \
  -subj "/CN=localhost" >/dev/null 2>&1
printf '%s\n' \
  "subjectAltName=DNS:control-plane,DNS:localhost,IP:127.0.0.1" \
  "extendedKeyUsage=serverAuth" >"$SECONDBOX_STRESS_PKI_DIR/server.ext"
openssl x509 -req \
  -in "$SECONDBOX_STRESS_PKI_DIR/server.csr" \
  -CA "$SECONDBOX_STRESS_PKI_DIR/runner-ca.crt" \
  -CAkey "$SECONDBOX_STRESS_PKI_DIR/runner-ca.key" \
  -CAcreateserial \
  -out "$SECONDBOX_STRESS_PKI_DIR/server.crt" \
  -days 2 \
  -extfile "$SECONDBOX_STRESS_PKI_DIR/server.ext" >/dev/null 2>&1
chmod 0600 "$SECONDBOX_STRESS_PKI_DIR/runner-ca.key" "$SECONDBOX_STRESS_PKI_DIR/server.key"
chmod 0644 "$SECONDBOX_STRESS_PKI_DIR/runner-ca.crt" "$SECONDBOX_STRESS_PKI_DIR/server.crt"

export SECONDBOX_RUNNER_ID=secondbox-stress-runner
export SECONDBOX_RUNNER_CA_CERTIFICATE="$SECONDBOX_STRESS_PKI_DIR/runner-ca.crt"
export SECONDBOX_RUNNER_CA_PRIVATE_KEY="$SECONDBOX_STRESS_PKI_DIR/runner-ca.key"
export SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS=2
deploy/bin/bootstrap-runner-trust.sh "$SECONDBOX_STRESS_RUNNER_IDENTITY_DIR"

jq -n \
  --slurpfile manifest "$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR/manifest.json" \
  --arg signature_key_id "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256" \
  '{
    assets: [
      {
        artifactId: $manifest[0].runtimeBundle.artifactId,
        manifestDigest: $manifest[0].runtimeBundle.manifestDigest,
        signatureKeyId: $signature_key_id,
        architecture: $manifest[0].architecture,
        guestProtocolGeneration: $manifest[0].guestProtocol.minimum,
        mandatoryGuestFeatures: $manifest[0].runtimeBundle.mandatoryGuestFeatures
      },
      {
        artifactId: $manifest[0].toolchainBundle.artifactId,
        manifestDigest: $manifest[0].toolchainBundle.manifestDigest,
        signatureKeyId: $signature_key_id,
        architecture: $manifest[0].architecture,
        guestProtocolGeneration: $manifest[0].guestProtocol.minimum,
        mandatoryGuestFeatures: $manifest[0].toolchainBundle.mandatoryGuestFeatures
      }
    ]
  }' >"$SECONDBOX_STRESS_ASSET_CATALOG"

CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false \
  -o "$SECONDBOX_STRESS_CONTROL_PLANE_BINARY" ./cmd/secondboxd
chmod 0755 "$SECONDBOX_STRESS_CONTROL_PLANE_BINARY"

findmnt --target "$SECONDBOX_RUNNER_WORKSPACE_ROOT" --noheadings --output TARGET,SOURCE,FSTYPE,OPTIONS
echo "SecondBox stress source commit: $SECONDBOX_STRESS_SOURCE_COMMIT"
echo "SecondBox stress Go version: $SECONDBOX_STRESS_GO_VERSION"
echo "SecondBox stress artifact manifest: $SECONDBOX_STRESS_ARTIFACT_MANIFEST_DIGEST"

compose config --quiet
compose_started=1
compose up --detach --wait --wait-timeout 180 \
  postgres object-store object-store-init control-plane

export SECONDBOX_LIVE_BASE_URL="http://127.0.0.1:$SECONDBOX_STRESS_API_PORT"
export SECONDBOX_LIVE_PLATFORM_TOKEN="$SECONDBOX_STRESS_PLATFORM_TOKEN"
go run ./tests/scenario/stress \
  --mode prepare \
  --config "$SECONDBOX_STRESS_CONFIG"

compose up --detach --build --wait --wait-timeout 300 secondbox-runner
go run ./tests/scenario/stress \
  --mode run \
  --config "$SECONDBOX_STRESS_CONFIG" \
  --output "$SECONDBOX_STRESS_OUTPUT"

echo "SecondBox stress qualification passed for commit $SECONDBOX_STRESS_SOURCE_COMMIT"
