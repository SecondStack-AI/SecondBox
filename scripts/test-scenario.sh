#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
qualification_evidence="$repo_root/.tmp/scenario-qualification-evidence.json"
snapshot_resume_evidence="$repo_root/.tmp/2026-08-07-snapshot-resume-end-to-end.json"
rm -f -- "$qualification_evidence" "$snapshot_resume_evidence"
compose_file="$repo_root/scripts/scenario-compose.yml"
scenario_root="$repo_root/.tmp/scenario"
scenario_mode="${SECONDBOX_SCENARIO_MODE:-suite}"
project_name="secondbox-$scenario_mode-$$"
runner_image="$project_name-runner"
scenario_started_epoch="$(date +%s)"
scenario_source_commit="$(git -C "$repo_root" rev-parse HEAD)"
scenario_repository_dirty=false
scenario_pass_count=0
qualification_complete=false
[[ -z "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ]] ||
  scenario_repository_dirty=true
cd "$repo_root"

fail() {
  echo "SecondBox scenario prerequisite failed: $*" >&2
  exit 1
}

if [[ "$scenario_mode" != "suite" && "$scenario_mode" != "stress" &&
      "$scenario_mode" != "lifecycle" ]]; then
  fail "SECONDBOX_SCENARIO_MODE must be suite, stress, or lifecycle"
fi

if [[ "${SECONDBOX_REQUIRE_QUALIFIED_SCENARIO:-}" != "1" ]]; then
  cat >&2 <<'PREREQUISITES'
SecondBox scenario qualification is mandatory and never skips.
Set SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1 and provide:
  /dev/kvm
  /dev/net/tun
  SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256
  SECONDBOX_RUNNER_WORKSPACE_ROOT on XFS or Btrfs
PREREQUISITES
  exit 1
fi

if [[ "$scenario_mode" == "suite" ]]; then
  export SECONDBOX_RUNNER_ID=scenario-runner
  export SECONDBOX_RUNNER_POOL_ID=scenario-pool
  export SECONDBOX_SCENARIO_SUBJECT_MAX_ACTIVE_INSTANCES=10
  export SECONDBOX_SCENARIO_SUBJECT_MAX_ARTIFACTS=100
  export SECONDBOX_SCENARIO_SUBJECT_MAX_CONCURRENT_OPERATIONS=20
  export SECONDBOX_SCENARIO_SUBJECT_MAX_CPU_MILLIS=100000
  export SECONDBOX_SCENARIO_SUBJECT_MAX_MEMORY_BYTES=107374182400
  export SECONDBOX_SCENARIO_SUBJECT_MAX_PORT_SESSIONS=100
  export SECONDBOX_SCENARIO_SUBJECT_MAX_ARTIFACT_BYTES=1099511627776
  export SECONDBOX_SCENARIO_SUBJECT_MAX_SANDBOXES=100
  export SECONDBOX_SCENARIO_SUBJECT_MAX_SNAPSHOTS=100
  export SECONDBOX_SCENARIO_HTTP_TIMEOUT_SECONDS=65
  export SECONDBOX_SCENARIO_ASSIGNMENT_CLAIM_MILLISECONDS=5000
  export SECONDBOX_SCENARIO_ASSIGNMENT_DEADLINE_MILLISECONDS=30000
  export SECONDBOX_SCENARIO_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS=5000
  export SECONDBOX_SCENARIO_STORAGE_RECOVERY_PERCENT=70
  export SECONDBOX_SCENARIO_STORAGE_WARNING_PERCENT=80
  export SECONDBOX_SCENARIO_STORAGE_DENY_PERCENT=90
  export SECONDBOX_SCENARIO_SANDBOX_MAX_VCPUS=2
  export SECONDBOX_SCENARIO_SANDBOX_MEMORY_MIB=2048
  export SECONDBOX_SCENARIO_SANDBOX_DISK_MIB=10240
  export SECONDBOX_SCENARIO_MEMORY_BUDGET_MIB=8192
  export SECONDBOX_SCENARIO_MAX_CONCURRENT_PER_SANDBOX=2
  export SECONDBOX_SCENARIO_MAX_CONCURRENT_GLOBAL=8
  export SECONDBOX_SCENARIO_MAX_CONCURRENT_STARTS=4
  export SECONDBOX_SCENARIO_MAX_CONCURRENT_WORKSPACE_CREATES=4
  export SECONDBOX_SCENARIO_MAX_CONCURRENT_OPERATIONS_GLOBAL=32
  export SECONDBOX_SCENARIO_FILE_TRANSFER_MAX_BYTES=1048576
else
  for variable in \
    SECONDBOX_STRESS_CONFIG \
    SECONDBOX_STRESS_OUTPUT \
    SECONDBOX_RUNNER_ID \
    SECONDBOX_RUNNER_POOL_ID \
    SECONDBOX_SCENARIO_SUBJECT_MAX_ACTIVE_INSTANCES \
    SECONDBOX_SCENARIO_SUBJECT_MAX_ARTIFACTS \
    SECONDBOX_SCENARIO_SUBJECT_MAX_CONCURRENT_OPERATIONS \
    SECONDBOX_SCENARIO_SUBJECT_MAX_CPU_MILLIS \
    SECONDBOX_SCENARIO_SUBJECT_MAX_MEMORY_BYTES \
    SECONDBOX_SCENARIO_SUBJECT_MAX_PORT_SESSIONS \
    SECONDBOX_SCENARIO_SUBJECT_MAX_ARTIFACT_BYTES \
    SECONDBOX_SCENARIO_SUBJECT_MAX_SANDBOXES \
    SECONDBOX_SCENARIO_SUBJECT_MAX_SNAPSHOTS \
    SECONDBOX_SCENARIO_HTTP_TIMEOUT_SECONDS \
    SECONDBOX_SCENARIO_ASSIGNMENT_CLAIM_MILLISECONDS \
    SECONDBOX_SCENARIO_ASSIGNMENT_DEADLINE_MILLISECONDS \
    SECONDBOX_SCENARIO_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS \
    SECONDBOX_SCENARIO_STORAGE_RECOVERY_PERCENT \
    SECONDBOX_SCENARIO_STORAGE_WARNING_PERCENT \
    SECONDBOX_SCENARIO_STORAGE_DENY_PERCENT \
    SECONDBOX_SCENARIO_SANDBOX_MAX_VCPUS \
    SECONDBOX_SCENARIO_SANDBOX_MEMORY_MIB \
    SECONDBOX_SCENARIO_SANDBOX_DISK_MIB \
    SECONDBOX_SCENARIO_MEMORY_BUDGET_MIB \
    SECONDBOX_SCENARIO_MAX_CONCURRENT_PER_SANDBOX \
    SECONDBOX_SCENARIO_MAX_CONCURRENT_GLOBAL \
    SECONDBOX_SCENARIO_MAX_CONCURRENT_STARTS \
    SECONDBOX_SCENARIO_MAX_CONCURRENT_WORKSPACE_CREATES \
    SECONDBOX_SCENARIO_MAX_CONCURRENT_OPERATIONS_GLOBAL \
    SECONDBOX_SCENARIO_FILE_TRANSFER_MAX_BYTES; do
    [[ -n "${!variable:-}" ]] || fail "stress mode requires $variable"
  done
fi

# The snapshot-resume Profile shape. The template's compatibility key records
# it, so the same values must reach the template publisher and the resume
# scenario group; a disagreement is a cache miss, never a silent cold boot.
# Every Compose service is validated in all modes, so these are stated once here
# rather than defaulted in the Compose file.
export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_MEMORY_MIB=256
export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_WORKSPACE_MIB=64
export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_VCPUS=1
export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_PROCESS_LIMIT=128
export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_ARRIVALS=10
export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_EVIDENCE="$snapshot_resume_evidence"

for command in curl date docker findmnt git go ip jq mountpoint openssl python3 seq sha256sum; do
  command -v "$command" >/dev/null 2>&1 ||
    fail "missing command: $command"
done
docker compose version >/dev/null 2>&1 ||
  fail "Docker Compose v2 is required"

for device in /dev/kvm /dev/net/tun; do
  [[ -c "$device" && -r "$device" && -w "$device" ]] ||
    fail "$device must be a readable and writable character device"
done
[[ -r /sys/fs/cgroup/cgroup.controllers ]] ||
  fail "a writable cgroup v2 host is required"

: "${SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR:?SecondBox scenario requires SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR}"
: "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY:?SecondBox scenario requires SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY}"
: "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256:?SecondBox scenario requires SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256}"
: "${SECONDBOX_RUNNER_WORKSPACE_ROOT:?SecondBox scenario requires SECONDBOX_RUNNER_WORKSPACE_ROOT}"

artifacts_dir="$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR"
workspace_root="$SECONDBOX_RUNNER_WORKSPACE_ROOT"
public_key="$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"
for directory in "$artifacts_dir" "$workspace_root"; do
  [[ "$directory" = /* && "$(realpath -e "$directory")" == "$directory" && ! -L "$directory" && -d "$directory" ]] ||
    fail "directory must be an existing clean absolute non-symlink path: $directory"
done
[[ "$public_key" = /* && "$(realpath -e "$public_key")" == "$public_key" && ! -L "$public_key" && -f "$public_key" ]] ||
  fail "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY must be an existing clean absolute non-symlink file"
diagnostics_dir="${SECONDBOX_SCENARIO_DIAGNOSTICS_DIR:-}"
if [[ -n "$diagnostics_dir" ]]; then
  diagnostics_parent="$(dirname "$diagnostics_dir")"
  [[ "$diagnostics_dir" = /* && ! -e "$diagnostics_dir" && ! -L "$diagnostics_dir" ]] ||
    fail "SECONDBOX_SCENARIO_DIAGNOSTICS_DIR must be an absent absolute path"
  [[ ! -L "$diagnostics_parent" && -d "$diagnostics_parent" ]] ||
    fail "SECONDBOX_SCENARIO_DIAGNOSTICS_DIR parent must be an existing non-symlink directory"
fi

workspace_mount="$(findmnt -T "$workspace_root" -n -o TARGET,SOURCE,FSTYPE,OPTIONS)" ||
  fail "findmnt could not resolve SECONDBOX_RUNNER_WORKSPACE_ROOT"
workspace_fstype="$(findmnt -T "$workspace_root" -n -o FSTYPE)"
if [[ "$workspace_fstype" != "xfs" && "$workspace_fstype" != "btrfs" ]]; then
  fail "SECONDBOX_RUNNER_WORKSPACE_ROOT must be on XFS or Btrfs, got $workspace_fstype"
fi
workspace_device="$(stat -c %d "$workspace_root")"
artifacts_device="$(stat -c %d "$artifacts_dir")"
checkout_device="$(stat -c %d "$repo_root")"
[[ "$workspace_device" == "$artifacts_device" && "$workspace_device" == "$checkout_device" ]] ||
  fail "workspace root, microVM artifacts, and checkout must share one reflink filesystem"

required_artifacts=(
  SHA256SUMS
  kernel
  manifest.json
  manifest.sig
  rootfs.ext4
  runtime-manifest.json
  shared.img
  toolchain-manifest.json
)
for name in "${required_artifacts[@]}"; do
  [[ -f "$artifacts_dir/$name" && ! -L "$artifacts_dir/$name" ]] ||
    fail "artifact bundle is missing regular non-symlink file: $name"
done
(
  cd "$artifacts_dir"
  sha256sum --check --strict SHA256SUMS >/dev/null
) || fail "artifact bundle checksum verification failed"
openssl dgst -sha256 -verify "$public_key" \
  -signature "$artifacts_dir/manifest.sig" "$artifacts_dir/manifest.json" >/dev/null ||
  fail "artifact manifest signature verification failed"
actual_key_fingerprint="$(
  openssl pkey -pubin -in "$public_key" -outform DER 2>/dev/null |
    sha256sum |
    awk '{print $1}'
)"
[[ "$actual_key_fingerprint" == "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256" ]] ||
  fail "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256 does not match the parsed public key"

manifest_digest="sha256:$(sha256sum "$artifacts_dir/manifest.json" | awk '{print $1}')"
runtime_digest="$(jq -er '.runtimeBundle.manifestDigest' "$artifacts_dir/manifest.json")" ||
  fail "artifact manifest lacks runtimeBundle.manifestDigest"
toolchain_digest="$(jq -er '.toolchainBundle.manifestDigest' "$artifacts_dir/manifest.json")" ||
  fail "artifact manifest lacks toolchainBundle.manifestDigest"
runtime_artifact_id="$(jq -er '.runtimeBundle.artifactId' "$artifacts_dir/manifest.json")" ||
  fail "artifact manifest lacks runtimeBundle.artifactId"
toolchain_artifact_id="$(jq -er '.toolchainBundle.artifactId' "$artifacts_dir/manifest.json")" ||
  fail "artifact manifest lacks toolchainBundle.artifactId"
runtime_features="$(jq -ce '.runtimeBundle.mandatoryGuestFeatures' "$artifacts_dir/manifest.json")" ||
  fail "artifact manifest lacks runtimeBundle.mandatoryGuestFeatures"
toolchain_features="$(jq -ce '.toolchainBundle.mandatoryGuestFeatures' "$artifacts_dir/manifest.json")" ||
  fail "artifact manifest lacks toolchainBundle.mandatoryGuestFeatures"
guest_protocol_minimum="$(jq -er '.guestProtocol.minimum' "$artifacts_dir/manifest.json")" ||
  fail "artifact manifest lacks guestProtocol.minimum"
guest_protocol_maximum="$(jq -er '.guestProtocol.maximum' "$artifacts_dir/manifest.json")" ||
  fail "artifact manifest lacks guestProtocol.maximum"
if (( guest_protocol_minimum > 1 || guest_protocol_maximum < 1 )); then
  fail "artifact manifest does not support guest protocol generation 1"
fi
architecture="$(jq -er '.architecture' "$artifacts_dir/manifest.json")" ||
  fail "artifact manifest lacks architecture"
[[ "$architecture" == "amd64" ]] ||
  fail "scenario runner currently requires an amd64 artifact bundle"

mkdir -p "$scenario_root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -trimpath -buildvcs=false -o "$scenario_root/secondboxd" "$repo_root/cmd/secondboxd"
chmod 0755 "$scenario_root/secondboxd"
if [[ "$scenario_mode" == "suite" ]]; then
  # The snapshot-resume template publisher is compiled on the host, where the
  # module cache lives, and executed inside the privileged runner image. CGO is
  # disabled so the binary runs on the Debian-based image regardless of the
  # build host.
  (
    cd "$repo_root/runner"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c ./internal/firecracker \
      -o "$scenario_root/snapshot-template-publish.test"
  )
  chmod 0755 "$scenario_root/snapshot-template-publish.test"
fi
docker build --quiet --file "$repo_root/runner/Dockerfile" --tag "$runner_image" "$repo_root" >/dev/null

run_dir="$(mktemp -d "$scenario_root/run.XXXXXX")"
pki_dir="$run_dir/pki"
identity_dir="$run_dir/runner-identity"
state_dir="$run_dir/runner-state"
asset_catalog="$run_dir/signed-assets.json"
mkdir -p "$pki_dir" "$state_dir"
scenario_workspace_dir="$(mktemp -d "$workspace_root/secondbox-scenario.XXXXXX")"
mkdir -p "$scenario_workspace_dir/jailer-root"

reserve_port() {
  python3 -c \
    'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

export SECONDBOX_SCENARIO_API_PORT
SECONDBOX_SCENARIO_API_PORT="$(reserve_port)"
export SECONDBOX_SCENARIO_DATABASE_PORT
SECONDBOX_SCENARIO_DATABASE_PORT="$(reserve_port)"
export SECONDBOX_SCENARIO_RUNNER_PORT
SECONDBOX_SCENARIO_RUNNER_PORT="$(reserve_port)"
export SECONDBOX_SCENARIO_RUNNER_DATA_PLANE_PORT
SECONDBOX_SCENARIO_RUNNER_DATA_PLANE_PORT="$(reserve_port)"
[[ "$SECONDBOX_SCENARIO_API_PORT" != "$SECONDBOX_SCENARIO_DATABASE_PORT" &&
   "$SECONDBOX_SCENARIO_API_PORT" != "$SECONDBOX_SCENARIO_RUNNER_PORT" &&
   "$SECONDBOX_SCENARIO_API_PORT" != "$SECONDBOX_SCENARIO_RUNNER_DATA_PLANE_PORT" &&
   "$SECONDBOX_SCENARIO_DATABASE_PORT" != "$SECONDBOX_SCENARIO_RUNNER_PORT" &&
   "$SECONDBOX_SCENARIO_DATABASE_PORT" != "$SECONDBOX_SCENARIO_RUNNER_DATA_PLANE_PORT" &&
   "$SECONDBOX_SCENARIO_RUNNER_PORT" != "$SECONDBOX_SCENARIO_RUNNER_DATA_PLANE_PORT" ]] ||
  fail "failed to reserve distinct scenario ports"

openssl req -x509 -newkey rsa:3072 -nodes \
  -keyout "$pki_dir/runner-ca.key" \
  -out "$pki_dir/runner-ca.crt" \
  -days 2 \
  -subj "/CN=SecondBox Scenario Runner CA" >/dev/null 2>&1
openssl req -new -newkey rsa:3072 -nodes \
  -keyout "$pki_dir/server.key" \
  -out "$pki_dir/server.csr" \
  -subj "/CN=localhost" >/dev/null 2>&1
printf '%s\n' \
  "subjectAltName=DNS:localhost,IP:127.0.0.1" \
  "extendedKeyUsage=serverAuth" >"$pki_dir/server.ext"
openssl x509 -req \
  -in "$pki_dir/server.csr" \
  -CA "$pki_dir/runner-ca.crt" \
  -CAkey "$pki_dir/runner-ca.key" \
  -CAcreateserial \
  -out "$pki_dir/server.crt" \
  -days 2 \
  -extfile "$pki_dir/server.ext" >/dev/null 2>&1
chmod 0600 "$pki_dir/runner-ca.key" "$pki_dir/server.key"
chmod 0644 "$pki_dir/runner-ca.crt" "$pki_dir/server.crt"

export SECONDBOX_RUNNER_CA_CERTIFICATE="$pki_dir/runner-ca.crt"
export SECONDBOX_RUNNER_CA_PRIVATE_KEY="$pki_dir/runner-ca.key"
export SECONDBOX_RUNNER_CERTIFICATE_LIFETIME_DAYS=2
"$repo_root/scripts/issue-scenario-runner-identity.sh" "$identity_dir" >/dev/null

jq -n \
  --arg architecture "$architecture" \
  --arg runtime "$runtime_digest" \
  --arg runtimeArtifactID "$runtime_artifact_id" \
  --arg signatureKeyID "$actual_key_fingerprint" \
  --arg toolchain "$toolchain_digest" \
  --arg toolchainArtifactID "$toolchain_artifact_id" \
  --argjson runtimeFeatures "$runtime_features" \
  --argjson toolchainFeatures "$toolchain_features" \
  '{
    assets: [
      {
        artifactId: $runtimeArtifactID,
        manifestDigest: $runtime,
        signatureKeyId: $signatureKeyID,
        architecture: $architecture,
        guestProtocolGeneration: 1,
        mandatoryGuestFeatures: $runtimeFeatures
      },
      {
        artifactId: $toolchainArtifactID,
        manifestDigest: $toolchain,
        signatureKeyId: $signatureKeyID,
        architecture: $architecture,
        guestProtocolGeneration: 1,
        mandatoryGuestFeatures: $toolchainFeatures
      }
    ]
  }' >"$asset_catalog"

export SECONDBOX_SCENARIO_UID
SECONDBOX_SCENARIO_UID="$(id -u)"
export SECONDBOX_SCENARIO_GID
SECONDBOX_SCENARIO_GID="$(id -g)"
export SECONDBOX_SCENARIO_PKI_DIR="$pki_dir"
export SECONDBOX_SCENARIO_IDENTITY_DIR="$identity_dir"
export SECONDBOX_SCENARIO_STATE_DIR="$state_dir"
export SECONDBOX_SCENARIO_WORKSPACE_DIR="$scenario_workspace_dir"
export SECONDBOX_SCENARIO_ASSET_CATALOG="$asset_catalog"
export SECONDBOX_SCENARIO_RUNNER_IMAGE="$runner_image"
export SECONDBOX_SCENARIO_SOURCE_COMMIT
SECONDBOX_SCENARIO_SOURCE_COMMIT="$scenario_source_commit"
export SECONDBOX_SCENARIO_GO_VERSION
SECONDBOX_SCENARIO_GO_VERSION="$(go version)"
export SECONDBOX_SCENARIO_ARTIFACT_MANIFEST_DIGEST="$manifest_digest"
export SECONDBOX_SCENARIO_CGROUP_PARENT="secondbox-scenario-$$"
export SECONDBOX_SCENARIO_BRIDGE_NAME="sbxq$(( $$ % 100000 ))"
export SECONDBOX_SCENARIO_TAP_PREFIX="sq$(( $$ % 1000 ))"
scenario_network_found=false
for offset in $(seq 0 511); do
  scenario_network_index=$(( ($$ + offset) % 512 ))
  scenario_network_second_octet=$(( 18 + scenario_network_index / 256 ))
  scenario_network_third_octet=$(( scenario_network_index % 256 ))
  scenario_guest_cidr="198.${scenario_network_second_octet}.${scenario_network_third_octet}.0/24"
  if [[ -z "$(ip route show "$scenario_guest_cidr")" ]]; then
    scenario_network_found=true
    break
  fi
done
[[ "$scenario_network_found" == "true" ]] ||
  fail "no unused scenario guest /24 is available in 198.18.0.0/15"
export SECONDBOX_SCENARIO_GUEST_CIDR="$scenario_guest_cidr"
export SECONDBOX_SCENARIO_BRIDGE_ADDRESS="198.${scenario_network_second_octet}.${scenario_network_third_octet}.1"
export SECONDBOX_SCENARIO_BRIDGE_CIDR="$SECONDBOX_SCENARIO_BRIDGE_ADDRESS/24"
export SECONDBOX_SCENARIO_GUEST_IP="198.${scenario_network_second_octet}.${scenario_network_third_octet}.2"
export SECONDBOX_SCENARIO_RUNNER_CLIENT_CERTIFICATE=/opt/secondbox-runner-identity/runner.crt
export SECONDBOX_SCENARIO_RUNNER_CREDENTIAL=scenario-runner-credential-000000000000000000000000
export SECONDBOX_SCENARIO_RUNNER_GUEST_HEARTBEAT_INTERVAL=1s
export SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST="$runtime_digest"
export SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST="$toolchain_digest"
export SECONDBOX_SCENARIO_COMPOSE_FILE="$compose_file"
export SECONDBOX_SCENARIO_COMPOSE_PROJECT="$project_name"
export SECONDBOX_PLATFORM_TOKEN="scenario-platform-token-0000000000000000"
# The direct Port transport is granted per application authority, never to the
# platform token, so qualifying it requires one explicitly provisioned ingress.
# The deployed interval used by data-plane session cancellation and retention
# recovery sweeps during qualification.
export SECONDBOX_SCENARIO_DATA_PLANE_POLL_INTERVAL_MILLISECONDS="${SECONDBOX_SCENARIO_DATA_PLANE_POLL_INTERVAL_MILLISECONDS:-250}"
export SECONDBOX_SCENARIO_DIRECT_PORT_PROFILE="scenario-direct-port"
export SECONDBOX_SCENARIO_DIRECT_PORT_TOKEN="scenario-direct-port-ingress-token-000000"
export SECONDBOX_APPLICATION_AUTHORITIES_JSON
SECONDBOX_APPLICATION_AUTHORITIES_JSON="$(
  cat <<JSON
[
  {
    "id": "scenario-direct-port-ingress",
    "token": "$SECONDBOX_SCENARIO_DIRECT_PORT_TOKEN",
    "tenantRef": "scenario-tenant",
    "subjectRef": "scenario-subject",
    "scopes": ["sandbox:read", "sandbox:lifecycle", "sandbox:ports", "sandbox:ports:direct"],
    "profileGrants": ["$SECONDBOX_SCENARIO_DIRECT_PORT_PROFILE"]
  }
]
JSON
)"
export SECONDBOX_LIVE_BASE_URL="http://127.0.0.1:$SECONDBOX_SCENARIO_API_PORT"
export SECONDBOX_SCENARIO_DATABASE_URL="postgresql://secondbox:secondbox-scenario-password@127.0.0.1:$SECONDBOX_SCENARIO_DATABASE_PORT/secondbox_scenario?sslmode=disable"

compose() {
  docker compose --project-name "$project_name" --file "$compose_file" "$@"
}

remove_host_network() {
  [[ -f "$state_dir/network/host-network.state" ]] || return 0
  docker run --rm --privileged --network host \
    --entrypoint /usr/local/bin/microvm-host-network-setup \
    -e "SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME=$SECONDBOX_SCENARIO_BRIDGE_NAME" \
    -e "SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR=$SECONDBOX_SCENARIO_BRIDGE_CIDR" \
    -e "SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR=$SECONDBOX_SCENARIO_GUEST_CIDR" \
    -e "SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX=$SECONDBOX_SCENARIO_TAP_PREFIX" \
    -e "SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR=/var/lib/secondbox-runner/network" \
    -e "SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE=true" \
    -v "$state_dir:/var/lib/secondbox-runner:rshared" \
    "$runner_image" remove >/dev/null
}

remove_propagated_mounts() {
  local -a targets
  local target
  mapfile -t targets < <(
    findmnt --raw --noheadings --output TARGET |
      tac
  )
  for target in "${targets[@]}"; do
    if [[ "$target" != "$scenario_workspace_dir" &&
          "$target" != "$state_dir/"* ]]; then
      continue
    fi
    mountpoint --quiet "$target" || continue
    docker run --rm --privileged --pid host \
      --entrypoint /usr/bin/nsenter \
      "$runner_image" \
      --target 1 --mount --root --wd -- /usr/bin/umount "$target" >/dev/null
  done
}

collect_diagnostics() {
  [[ -n "$diagnostics_dir" ]] || return 0
  mkdir -m 0700 -- "$diagnostics_dir" ||
    return 1
  if ! compose exec --no-TTY secondbox-runner \
    /bin/sh -c \
    'test -f /var/lib/secondbox-runner/log/runner.jsonl &&
     cat /var/lib/secondbox-runner/log/runner.jsonl' \
    >"$diagnostics_dir/runner.jsonl"; then
    return 1
  fi
  if ! compose logs --no-color --timestamps \
    control-plane postgres object-store \
    >"$diagnostics_dir/compose.log"; then
    return 1
  fi
}

cleanup() {
  status="$?"
  trap - EXIT
  if ! collect_diagnostics; then
    echo "SecondBox scenario diagnostics collection failed: $diagnostics_dir" >&2
    status=1
  fi
  if [[ "$status" -ne 0 ]]; then
    if ! compose ps --all >&2; then
      echo "SecondBox scenario could not collect container state" >&2
    fi
    if ! compose exec --no-TTY secondbox-runner \
      /bin/sh -c \
      'test ! -f /var/lib/secondbox-runner/log/runner.jsonl ||
       tail -n 500 /var/lib/secondbox-runner/log/runner.jsonl' >&2; then
      echo "SecondBox scenario could not collect runner application logs" >&2
    fi
    if ! compose exec --no-TTY secondbox-runner \
      /bin/sh -c \
      'find /var/lib/secondbox-runner/firecracker-log -type f -exec tail -n 200 {} +' >&2; then
      echo "SecondBox scenario could not collect Firecracker logs" >&2
    fi
    if ! compose logs --tail 200 control-plane secondbox-runner postgres object-store >&2; then
      echo "SecondBox scenario could not collect failure logs" >&2
    fi
  fi
  if ! compose exec --no-TTY secondbox-runner \
    /usr/local/bin/microvm-host-network-setup remove >/dev/null 2>&1; then
    if ! remove_host_network; then
      echo "SecondBox scenario host-network cleanup failed" >&2
      status=1
    fi
  fi
  if ! compose stop secondbox-runner >/dev/null 2>&1; then
    echo "SecondBox scenario runner stop failed for $project_name" >&2
    status=1
  fi
  if ! compose down --volumes --remove-orphans >/dev/null 2>&1; then
    echo "SecondBox scenario Compose cleanup failed for $project_name" >&2
    status=1
  fi
  if ! remove_propagated_mounts; then
    echo "SecondBox scenario propagated-mount cleanup failed" >&2
    status=1
  fi
  for directory in "$state_dir" "$scenario_workspace_dir"; do
    if [[ -d "$directory" ]] &&
       ! docker run --rm \
         --entrypoint /bin/chown \
         -v "$directory:/cleanup" \
         "$runner_image" \
         -R "$SECONDBOX_SCENARIO_UID:$SECONDBOX_SCENARIO_GID" /cleanup >/dev/null; then
      echo "SecondBox scenario ownership cleanup failed: $directory" >&2
      status=1
    fi
  done
  if ! docker image rm "$runner_image" >/dev/null 2>&1; then
    echo "SecondBox scenario runner image cleanup failed: $runner_image" >&2
    status=1
  fi
  if [[ -d "$scenario_workspace_dir" ]] && ! rm -rf -- "$scenario_workspace_dir"; then
    echo "SecondBox scenario Workspace cleanup failed: $scenario_workspace_dir" >&2
    status=1
  fi
  if [[ -d "$run_dir" ]] && ! rm -rf -- "$run_dir"; then
    echo "SecondBox scenario run-directory cleanup failed: $run_dir" >&2
    status=1
  fi
  if [[ "$status" -eq 0 && "$qualification_complete" == "true" &&
        "$scenario_mode" == "suite" && -z "${SECONDBOX_SCENARIO_TEST_PATTERN:-}" ]]; then
    qualification_evidence_temporary="$qualification_evidence.tmp.$$"
    qualified_at="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
    wall_clock_seconds="$(( $(date +%s) - scenario_started_epoch ))"
    if ! jq -n \
      --arg schemaVersion "secondbox.release/qualification-evidence/v1" \
      --arg sourceCommit "$scenario_source_commit" \
      --argjson repositoryDirty "$scenario_repository_dirty" \
      --arg suite "test-scenario" \
      --argjson passCount "$scenario_pass_count" \
      --argjson wallClockSeconds "$wall_clock_seconds" \
      --arg workspaceMount "$workspace_mount" \
      --arg workspaceFilesystem "$workspace_fstype" \
      --arg qualifiedAt "$qualified_at" \
      '{
        schemaVersion: $schemaVersion,
        sourceCommit: $sourceCommit,
        repositoryDirty: $repositoryDirty,
        suite: $suite,
        passCount: $passCount,
        wallClockSeconds: $wallClockSeconds,
        host: {
          kvm: {path: "/dev/kvm", present: true, readable: true, writable: true},
          tun: {path: "/dev/net/tun", present: true, readable: true, writable: true},
          workspaceFilesystem: {mount: $workspaceMount, type: $workspaceFilesystem}
        },
        qualifiedAt: $qualifiedAt
      }' >"$qualification_evidence_temporary" ||
      ! mv -- "$qualification_evidence_temporary" "$qualification_evidence"; then
      rm -f -- "$qualification_evidence_temporary" "$qualification_evidence"
      echo "SecondBox scenario qualification evidence write failed: $qualification_evidence" >&2
      status=1
    fi
  fi
  exit "$status"
}
trap cleanup EXIT

echo "SecondBox scenario workspace mount: $workspace_mount"
echo "SecondBox scenario source commit: $SECONDBOX_SCENARIO_SOURCE_COMMIT"
echo "SecondBox scenario Go version: $SECONDBOX_SCENARIO_GO_VERSION"
echo "SecondBox scenario artifact manifest: $manifest_digest"
echo "SecondBox scenario guest network: $SECONDBOX_SCENARIO_GUEST_CIDR"

compose config --quiet
compose up --detach --wait --wait-timeout 240 \
  postgres object-store object-store-init control-plane

if [[ "$scenario_mode" == "stress" ]]; then
  go run ./tests/scenario/stress \
    --mode prepare \
    --config "$SECONDBOX_STRESS_CONFIG"
elif [[ "$scenario_mode" == "lifecycle" ]]; then
  go run ./tests/scenario/lifecycle \
    --mode prepare \
    --config "$SECONDBOX_STRESS_CONFIG"
fi

if [[ "$scenario_mode" == "suite" ]]; then
  # Publish a snapshot-resume template into the Runner's cache before the Runner
  # starts. Until a Runner builds its own, this is what makes it advertise the
  # snapshot-resume capability at all; without it every snapshot_resume Profile
  # would be refused at admission and the resume group would have nothing to
  # measure.
  echo "SecondBox scenario publishing a snapshot-resume template"
  compose run --rm --no-deps snapshot-template-publisher
  snapshot_template_report="$state_dir/snapshot-template-publish.json"
  [[ -f "$snapshot_template_report" ]] ||
    fail "the snapshot-resume template publisher produced no report"
  export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_TEMPLATE_ID
  SECONDBOX_SCENARIO_SNAPSHOT_RESUME_TEMPLATE_ID="$(
    jq -er '.templateId' "$snapshot_template_report"
  )" || fail "the snapshot-resume template report has no templateId"
  export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_TEMPLATE_BUILD_MS
  SECONDBOX_SCENARIO_SNAPSHOT_RESUME_TEMPLATE_BUILD_MS="$(
    jq -er '.templateBuildMilliseconds' "$snapshot_template_report"
  )"
  export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_TEMPLATE_ADMISSION_MS
  SECONDBOX_SCENARIO_SNAPSHOT_RESUME_TEMPLATE_ADMISSION_MS="$(
    jq -er '.cacheAdmissionMilliseconds' "$snapshot_template_report"
  )"
  echo "SecondBox scenario snapshot-resume template: $SECONDBOX_SCENARIO_SNAPSHOT_RESUME_TEMPLATE_ID"
fi

compose up --detach --wait --wait-timeout 300 secondbox-runner

if [[ "$scenario_mode" == "suite" ]]; then
  scenario_test_arguments=(-count=1 -tags=scenario_live -timeout=30m -v)
  if [[ -n "${SECONDBOX_SCENARIO_TEST_PATTERN:-}" ]]; then
    scenario_test_arguments+=(-run "$SECONDBOX_SCENARIO_TEST_PATTERN")
  fi
  scenario_test_output="$run_dir/scenario-test-output.log"
  go test "${scenario_test_arguments[@]}" ./tests/scenario 2>&1 |
    tee "$scenario_test_output"
  scenario_pass_count="$(awk '/^--- PASS: / { count++ } END { print count + 0 }' "$scenario_test_output")"
  [[ "$scenario_pass_count" -gt 0 ]] ||
    fail "scenario suite reported no passing top-level tests"
  qualification_complete=true
elif [[ "$scenario_mode" == "lifecycle" ]]; then
  go run ./tests/scenario/lifecycle \
    --mode run \
    --config "$SECONDBOX_STRESS_CONFIG" \
    --output "$SECONDBOX_STRESS_OUTPUT"
else
  go run ./tests/scenario/stress \
    --mode run \
    --config "$SECONDBOX_STRESS_CONFIG" \
    --output "$SECONDBOX_STRESS_OUTPUT"
fi

echo "SecondBox $scenario_mode qualification passed"
