#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scenario_backend="${SECONDBOX_SCENARIO_COMPUTE_BACKEND:-firecracker}"
if [[ "$scenario_backend" != "firecracker" && "$scenario_backend" != "microsandbox" && "$scenario_backend" != "gvisor" ]]; then
  echo "SecondBox scenario prerequisite failed: SECONDBOX_SCENARIO_COMPUTE_BACKEND must be firecracker, microsandbox, or gvisor" >&2
  exit 1
fi
export SECONDBOX_SCENARIO_COMPUTE_BACKEND="$scenario_backend"
scenario_host_platform="${SECONDBOX_SCENARIO_HOST_PLATFORM:-linux}"
if [[ "$scenario_host_platform" != "linux" && "$scenario_host_platform" != "darwin" ]]; then
  echo "SecondBox scenario prerequisite failed: SECONDBOX_SCENARIO_HOST_PLATFORM must be linux or darwin" >&2
  exit 1
fi
if [[ "$scenario_backend" == "gvisor" && "$scenario_host_platform" != "linux" ]]; then
  echo "SecondBox scenario prerequisite failed: the gVisor scenario supports Linux only" >&2
  exit 1
fi
if [[ "$scenario_host_platform" == "darwin" && "$scenario_backend" != "microsandbox" ]]; then
  echo "SecondBox scenario prerequisite failed: Darwin supports only the Microsandbox scenario" >&2
  exit 1
fi
native_macos=false
if [[ "$scenario_host_platform" == "darwin" ]]; then
  native_macos=true
fi
# The gVisor runner also qualifies as a privileged Kubernetes pod. In that
# placement the runner lives outside Compose and a service-control script
# translates every runner lifecycle verb, exactly like the native macOS path.
runner_placement="${SECONDBOX_SCENARIO_RUNNER_PLACEMENT:-compose}"
if [[ "$runner_placement" != "compose" && "$runner_placement" != "pod" ]]; then
  echo "SecondBox scenario SECONDBOX_SCENARIO_RUNNER_PLACEMENT must be compose or pod" >&2
  exit 1
fi
if [[ "$runner_placement" == "pod" && "$scenario_backend" != "gvisor" ]]; then
  echo "SecondBox scenario pod placement is qualified only for the gvisor backend" >&2
  exit 1
fi
runner_external=false
if [[ "$native_macos" == "true" || "$runner_placement" == "pod" ]]; then
  runner_external=true
fi
export SECONDBOX_SCENARIO_HOST_PLATFORM="$scenario_host_platform"
qualification_evidence="$repo_root/.tmp/scenario-qualification-evidence.json"
if [[ "$scenario_backend" == "microsandbox" ]]; then
  qualification_evidence="$repo_root/.tmp/microsandbox-$scenario_host_platform-scenario-qualification-evidence.json"
elif [[ "$scenario_backend" == "gvisor" ]]; then
  qualification_evidence="$repo_root/.tmp/gvisor-$scenario_host_platform-scenario-qualification-evidence.json"
  if [[ "$runner_placement" == "pod" ]]; then
    qualification_evidence="$repo_root/.tmp/gvisor-pod-$scenario_host_platform-scenario-qualification-evidence.json"
  fi
fi
snapshot_resume_evidence="$repo_root/.tmp/2026-08-07-snapshot-resume-end-to-end.json"
microsandbox_cold_start_evidence="$repo_root/.tmp/2026-08-13-microsandbox-$scenario_host_platform-cold-starts.json"
if [[ "$scenario_backend" == "gvisor" ]]; then
  microsandbox_cold_start_evidence="$repo_root/.tmp/2026-08-25-gvisor-$scenario_host_platform-cold-starts.json"
  if [[ "$runner_placement" == "pod" ]]; then
    microsandbox_cold_start_evidence="$repo_root/.tmp/2026-08-25-gvisor-pod-$scenario_host_platform-cold-starts.json"
  fi
fi
rm -f -- "$qualification_evidence" "$snapshot_resume_evidence" "$microsandbox_cold_start_evidence"
compose_file="$repo_root/scripts/scenario-compose.yml"
compose_override_file=""
if [[ "$scenario_backend" == "microsandbox" && "$native_macos" != "true" ]]; then
  compose_override_file="$repo_root/scripts/scenario-microsandbox-compose.yml"
elif [[ "$scenario_backend" == "gvisor" ]]; then
  compose_override_file="$repo_root/scripts/scenario-gvisor-compose.yml"
  if [[ "$runner_placement" == "pod" ]]; then
    compose_override_file="$repo_root/scripts/scenario-gvisor-pod-compose.yml"
  fi
fi
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

sha256_stream() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum
  else
    shasum -a 256
  fi
}

if [[ "$scenario_mode" != "suite" && "$scenario_mode" != "stress" &&
      "$scenario_mode" != "lifecycle" ]]; then
  fail "SECONDBOX_SCENARIO_MODE must be suite, stress, or lifecycle"
fi

if [[ "${SECONDBOX_REQUIRE_QUALIFIED_SCENARIO:-}" != "1" ]]; then
  cat >&2 <<'PREREQUISITES'
SecondBox scenario qualification is mandatory and never skips.
Set SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1 and provide:
  the selected host hypervisor and native prerequisites
  the selected backend's explicit local artifacts and trust inputs
  SECONDBOX_RUNNER_WORKSPACE_ROOT on the platform-qualified reflink filesystem
PREREQUISITES
  exit 1
fi

if [[ "$scenario_mode" == "suite" ]]; then
  export SECONDBOX_RUNNER_ID=scenario-runner
  export SECONDBOX_COMPUTE_BACKEND="$scenario_backend"
  export SECONDBOX_RUNNER_POOL_ID=standard-amd64
  export SECONDBOX_SCENARIO_SUBJECT_MAX_ACTIVE_INSTANCES=10
  export SECONDBOX_SCENARIO_SUBJECT_MAX_CONCURRENT_OPERATIONS=20
  export SECONDBOX_SCENARIO_SUBJECT_MAX_VCPU_COUNT=100000
  export SECONDBOX_SCENARIO_SUBJECT_MAX_MEMORY_BYTES=107374182400
  export SECONDBOX_SCENARIO_SUBJECT_MAX_PORT_SESSIONS=100
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
  if [[ "$scenario_backend" == "microsandbox" ]]; then
    export SECONDBOX_SCENARIO_SANDBOX_DISK_MIB=64
    export SECONDBOX_SCENARIO_MICROSANDBOX_MAXIMUM_VCPUS=$SECONDBOX_SCENARIO_MAX_CONCURRENT_GLOBAL
    export SECONDBOX_SCENARIO_MICROSANDBOX_MAXIMUM_MEMORY_BYTES=$(( 2048 * 1024 * 1024 ))
    export SECONDBOX_SCENARIO_MICROSANDBOX_MAXIMUM_DISK_BYTES=$((
      SECONDBOX_SCENARIO_SANDBOX_DISK_MIB * 1024 * 1024 * SECONDBOX_SCENARIO_MAX_CONCURRENT_GLOBAL
    ))
    export SECONDBOX_SCENARIO_MICROSANDBOX_WORKSPACE_TEMPLATE_CAPACITY_BYTES=$(( 64 * 1024 * 1024 ))
  elif [[ "$scenario_backend" == "gvisor" ]]; then
    export SECONDBOX_SCENARIO_SANDBOX_DISK_MIB=64
    export SECONDBOX_SCENARIO_GVISOR_MAXIMUM_VCPUS=$SECONDBOX_SCENARIO_MAX_CONCURRENT_GLOBAL
    export SECONDBOX_SCENARIO_GVISOR_MAXIMUM_MEMORY_BYTES=$(( 2048 * 1024 * 1024 ))
    export SECONDBOX_SCENARIO_GVISOR_MAXIMUM_DISK_BYTES=$((
      SECONDBOX_SCENARIO_SANDBOX_DISK_MIB * 1024 * 1024 * SECONDBOX_SCENARIO_MAX_CONCURRENT_GLOBAL
    ))
    export SECONDBOX_SCENARIO_GVISOR_WORKSPACE_TEMPLATE_CAPACITY_BYTES=$(( 64 * 1024 * 1024 ))
  fi
else
  for variable in \
    SECONDBOX_STRESS_CONFIG \
    SECONDBOX_STRESS_OUTPUT \
    SECONDBOX_RUNNER_ID \
    SECONDBOX_RUNNER_POOL_ID \
    SECONDBOX_SCENARIO_SUBJECT_MAX_ACTIVE_INSTANCES \
    SECONDBOX_SCENARIO_SUBJECT_MAX_CONCURRENT_OPERATIONS \
    SECONDBOX_SCENARIO_SUBJECT_MAX_VCPU_COUNT \
    SECONDBOX_SCENARIO_SUBJECT_MAX_MEMORY_BYTES \
    SECONDBOX_SCENARIO_SUBJECT_MAX_PORT_SESSIONS \
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
  if [[ "$scenario_backend" == "microsandbox" ]]; then
    : "${SECONDBOX_SCENARIO_MICROSANDBOX_MAXIMUM_VCPUS:?Microsandbox stress mode requires SECONDBOX_SCENARIO_MICROSANDBOX_MAXIMUM_VCPUS}"
  fi
fi

# The snapshot-resume Profile shape. The template's compatibility key records
# it, so the same values must reach the template publisher and the resume
# scenario group; a disagreement is a cache miss, never a silent cold boot.
# Every Compose service is validated in all modes, so these are stated once here
# rather than defaulted in the Compose file.
export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_MEMORY_MIB=256
export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_WORKSPACE_MIB=64
export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_VCPUS=1
export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_ARRIVALS=10
export SECONDBOX_SCENARIO_SNAPSHOT_RESUME_EVIDENCE="$snapshot_resume_evidence"
export SECONDBOX_SCENARIO_MICROSANDBOX_COLD_START_EVIDENCE="$microsandbox_cold_start_evidence"

required_commands=(curl date docker git go jq openssl python3 seq)
if [[ "$native_macos" == "true" ]]; then
  required_commands+=(diskutil pgrep ps shasum sysctl)
else
  required_commands+=(findmnt ip mountpoint sha256sum)
fi
for command in "${required_commands[@]}"; do
  command -v "$command" >/dev/null 2>&1 ||
    fail "missing command: $command"
done
if docker compose version >/dev/null 2>&1; then
  compose_command=(docker compose)
elif command -v docker-compose >/dev/null 2>&1 && docker-compose version >/dev/null 2>&1; then
  compose_command=(docker-compose)
else
  fail "Docker Compose v2 is required"
fi

if [[ "$native_macos" == "true" ]]; then
  [[ "$(uname -s)" == "Darwin" && "$(uname -m)" == "arm64" ]] ||
    fail "the native macOS scenario requires Apple Silicon"
  [[ "$(sysctl -n kern.hv_support)" == "1" ]] ||
    fail "Hypervisor.framework support is required"
else
  if [[ "$scenario_backend" == "gvisor" ]]; then
    [[ ! -e /dev/kvm ]] ||
      fail "the gVisor scenario qualifies hosts without /dev/kvm"
  else
    required_devices=(/dev/kvm)
    if [[ "$scenario_backend" == "firecracker" ]]; then
      required_devices+=(/dev/net/tun)
    fi
    for device in "${required_devices[@]}"; do
      [[ -c "$device" && -r "$device" && -w "$device" ]] ||
        fail "$device must be a readable and writable character device"
    done
  fi
fi
if [[ "$scenario_backend" == "firecracker" ]]; then
  [[ -r /sys/fs/cgroup/cgroup.controllers ]] ||
    fail "a writable cgroup v2 host is required"
fi

: "${SECONDBOX_RUNNER_WORKSPACE_ROOT:?SecondBox scenario requires SECONDBOX_RUNNER_WORKSPACE_ROOT}"

if [[ "$scenario_backend" == "firecracker" ]]; then
  : "${SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR:?SecondBox Firecracker scenario requires SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR}"
  : "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY:?SecondBox Firecracker scenario requires SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY}"
  : "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256:?SecondBox Firecracker scenario requires SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256}"
else
  : "${SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST:?SecondBox scenario requires SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST}"
  : "${SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST:?SecondBox scenario requires SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST}"
  if [[ "$scenario_backend" == "microsandbox" ]]; then
  : "${SECONDBOX_SCENARIO_MICROSANDBOX_BUILD:?SecondBox Microsandbox scenario requires SECONDBOX_SCENARIO_MICROSANDBOX_BUILD}"
  : "${SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION:?SecondBox Microsandbox scenario requires SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION}"
  : "${SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION_DIGEST:?SecondBox Microsandbox scenario requires SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION_DIGEST}"
  fi
  # The base Compose service contains ignored Firecracker mounts. Bind explicit
  # existing local Microsandbox inputs to those targets so Compose can validate
  # one shared topology without introducing a signature requirement.
  if [[ "$scenario_backend" == "gvisor" ]]; then
    export SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR="$SECONDBOX_SCENARIO_GVISOR_BUILD"
    export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY="$SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION"
    export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256="$SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION_DIGEST"
  else
    export SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR="$SECONDBOX_SCENARIO_MICROSANDBOX_BUILD"
    export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY="$SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION"
    export SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256="$SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION_DIGEST"
  fi
fi

artifacts_dir="$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR"
workspace_root="$SECONDBOX_RUNNER_WORKSPACE_ROOT"
public_key="$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"
for directory in "$artifacts_dir" "$workspace_root"; do
  [[ "$directory" = /* && "$(realpath "$directory")" == "$directory" && ! -L "$directory" && -d "$directory" ]] ||
    fail "directory must be an existing clean absolute non-symlink path: $directory"
done
[[ "$public_key" = /* && "$(realpath "$public_key")" == "$public_key" && ! -L "$public_key" && -f "$public_key" ]] ||
  fail "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY must be an existing clean absolute non-symlink file"
diagnostics_dir="${SECONDBOX_SCENARIO_DIAGNOSTICS_DIR:-}"
if [[ -n "$diagnostics_dir" ]]; then
  diagnostics_parent="$(dirname "$diagnostics_dir")"
  [[ "$diagnostics_dir" = /* && ! -e "$diagnostics_dir" && ! -L "$diagnostics_dir" ]] ||
    fail "SECONDBOX_SCENARIO_DIAGNOSTICS_DIR must be an absent absolute path"
  [[ ! -L "$diagnostics_parent" && -d "$diagnostics_parent" ]] ||
    fail "SECONDBOX_SCENARIO_DIAGNOSTICS_DIR parent must be an existing non-symlink directory"
fi

if [[ "$native_macos" == "true" ]]; then
  workspace_device_node="$(df -P "$workspace_root" | awk 'NR == 2 { print $1 }')"
  workspace_disk_info="$(diskutil info "$workspace_device_node")"
  grep -q 'File System Personality:.*APFS' <<<"$workspace_disk_info" ||
    fail "SECONDBOX_RUNNER_WORKSPACE_ROOT must be on APFS"
  workspace_fstype=apfs
  workspace_mount="$(df -P "$workspace_root" | awk 'NR == 2 { print $6 " " $1 " apfs" }')"
  workspace_device="$(stat -f %d "$workspace_root")"
else
  workspace_mount="$(findmnt -T "$workspace_root" -n -o TARGET,SOURCE,FSTYPE,OPTIONS)" ||
    fail "findmnt could not resolve SECONDBOX_RUNNER_WORKSPACE_ROOT"
  workspace_fstype="$(findmnt -T "$workspace_root" -n -o FSTYPE)"
  if [[ "$workspace_fstype" != "xfs" && "$workspace_fstype" != "btrfs" ]]; then
    fail "SECONDBOX_RUNNER_WORKSPACE_ROOT must be on XFS or Btrfs, got $workspace_fstype"
  fi
  workspace_device="$(stat -c %d "$workspace_root")"
fi
if [[ "$scenario_backend" == "firecracker" ]]; then
  artifacts_device="$(stat -c %d "$artifacts_dir")"
  checkout_device="$(stat -c %d "$repo_root")"
  [[ "$workspace_device" == "$artifacts_device" && "$workspace_device" == "$checkout_device" ]] ||
    fail "workspace root, microVM artifacts, and checkout must share one reflink filesystem"

  required_artifacts=(SHA256SUMS kernel manifest.json manifest.sig rootfs.ext4 runtime-manifest.json shared.img toolchain-manifest.json)
  for name in "${required_artifacts[@]}"; do
    [[ -f "$artifacts_dir/$name" && ! -L "$artifacts_dir/$name" ]] ||
      fail "artifact bundle is missing regular non-symlink file: $name"
  done
  (cd "$artifacts_dir" && sha256sum --check --strict SHA256SUMS >/dev/null) ||
    fail "artifact bundle checksum verification failed"
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
elif [[ "$scenario_backend" == "gvisor" ]]; then
  : "${SECONDBOX_SCENARIO_GVISOR_BUILD:?SecondBox gVisor scenario requires SECONDBOX_SCENARIO_GVISOR_BUILD}"
  : "${SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION:?SecondBox gVisor scenario requires SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION}"
  : "${SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION_DIGEST:?SecondBox gVisor scenario requires SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION_DIGEST}"
  materialization="$SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION"
  [[ "sha256:$(jq --compact-output --join-output . "$materialization" | sha256_stream | awk '{print $1}')" == "$SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION_DIGEST" ]] ||
    fail "gVisor materialization digest differs from its pinned identity"
  [[ "$(jq -er '.schemaVersion' "$materialization")" == "secondbox.runner/backend-materialization/v1" &&
     "$(jq -er '.key.backendKind' "$materialization")" == "gvisor" ]] ||
    fail "gVisor materialization schema or backend kind is invalid"
  runtime_digest="$SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST"
  toolchain_digest="$SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST"
  runtime_artifact_id="gvisor-runtime"
  toolchain_artifact_id="gvisor-toolchain"
  runtime_features='[]'
  toolchain_features='[]'
  guest_protocol_minimum=1
  guest_protocol_maximum=1
  architecture="$(jq -er '.key.guestArchitecture' "$materialization")" ||
    fail "gVisor materialization lacks key.guestArchitecture"
  manifest_digest="$SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION_DIGEST"
else
  materialization="$SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION"
  [[ "sha256:$(jq --compact-output --join-output . "$materialization" | sha256_stream | awk '{print $1}')" == "$SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION_DIGEST" ]] ||
    fail "Microsandbox materialization digest differs from its pinned identity"
  [[ "$(jq -er '.schemaVersion' "$materialization")" == "secondbox.runner/backend-materialization/v1" &&
     "$(jq -er '.key.backendKind' "$materialization")" == "microsandbox" ]] ||
    fail "Microsandbox materialization schema or backend kind is invalid"
  runtime_digest="$SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST"
  toolchain_digest="$SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST"
  runtime_artifact_id="microsandbox-runtime"
  toolchain_artifact_id="microsandbox-toolchain"
  runtime_features='[]'
  toolchain_features='[]'
  guest_protocol_minimum=6
  guest_protocol_maximum=6
  architecture="$(jq -er '.key.guestArchitecture' "$materialization")" ||
    fail "Microsandbox materialization lacks key.guestArchitecture"
  manifest_digest="$SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION_DIGEST"
fi
if [[ "$native_macos" == "true" ]]; then
  [[ "$architecture" == "arm64" ]] ||
    fail "native macOS scenario requires arm64 local artifacts"
else
  [[ "$architecture" == "amd64" ]] ||
    fail "Linux scenario requires amd64 local artifacts"
fi
export SECONDBOX_SCENARIO_ARCHITECTURE="$architecture"

mkdir -p "$scenario_root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -trimpath -buildvcs=false -o "$scenario_root/secondboxd" "$repo_root/cmd/secondboxd"
chmod 0755 "$scenario_root/secondboxd"
runner_dockerfile="$repo_root/runner/Dockerfile"
if [[ "$scenario_backend" != "firecracker" && "$native_macos" != "true" ]]; then
  (
    cd "$repo_root/runner"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -trimpath -buildvcs=false -o "$scenario_root/secondbox-runner" \
      ./cmd/secondbox-runner
  )
  chmod 0755 "$scenario_root/secondbox-runner"
  runner_dockerfile="$repo_root/runner/Dockerfile.microsandbox-scenario"
  if [[ "$scenario_backend" == "gvisor" ]]; then
    runner_dockerfile="$repo_root/runner/Dockerfile.gvisor-scenario"
  fi
fi
if [[ "$scenario_mode" == "suite" && "$scenario_backend" == "firecracker" ]]; then
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
if [[ "$native_macos" != "true" ]]; then
  runner_build_arguments=(--quiet --file "$runner_dockerfile" --tag "$runner_image")
  if [[ "$scenario_backend" != "firecracker" ]]; then
    runner_build_arguments+=(--build-context "scenario=$scenario_root")
  fi
  docker build "${runner_build_arguments[@]}" "$repo_root" >/dev/null
else
  : "${SECONDBOX_SCENARIO_NATIVE_RUNNER_BUILD:?native macOS scenario requires SECONDBOX_SCENARIO_NATIVE_RUNNER_BUILD}"
  [[ "$(cd "$SECONDBOX_SCENARIO_NATIVE_RUNNER_BUILD" && pwd -P)" == "$SECONDBOX_SCENARIO_NATIVE_RUNNER_BUILD" ]] ||
    fail "SECONDBOX_SCENARIO_NATIVE_RUNNER_BUILD must be a clean absolute path"
  export SECONDBOX_SCENARIO_SERVICE_CONTROL="$repo_root/scripts/scenario-microsandbox-macos-service-control.sh"
fi
if [[ "$runner_placement" == "pod" ]]; then
  export SECONDBOX_SCENARIO_SERVICE_CONTROL="$repo_root/scripts/scenario-gvisor-pod-service-control.sh"
fi

run_dir="$(mktemp -d "$scenario_root/run.XXXXXX")"
pki_dir="$run_dir/pki"
identity_dir="$run_dir/runner-identity"
state_dir="$run_dir/runner-state"
relocation_identity_dir="$run_dir/relocation-runner-identity"
relocation_state_dir="$run_dir/relocation-runner-state"
asset_catalog="$run_dir/signed-assets.json"
mkdir -p "$pki_dir" "$state_dir" "$relocation_state_dir"
scenario_workspace_dir="$(mktemp -d "$workspace_root/secondbox-scenario.XXXXXX")"
relocation_workspace_dir="$(mktemp -d "$workspace_root/secondbox-scenario-relocation.XXXXXX")"
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
export SECONDBOX_SCENARIO_RELOCATION_RUNNER_DATA_PLANE_PORT
SECONDBOX_SCENARIO_RELOCATION_RUNNER_DATA_PLANE_PORT="$(reserve_port)"
[[ "$SECONDBOX_SCENARIO_API_PORT" != "$SECONDBOX_SCENARIO_DATABASE_PORT" &&
   "$SECONDBOX_SCENARIO_API_PORT" != "$SECONDBOX_SCENARIO_RUNNER_PORT" &&
   "$SECONDBOX_SCENARIO_API_PORT" != "$SECONDBOX_SCENARIO_RUNNER_DATA_PLANE_PORT" &&
   "$SECONDBOX_SCENARIO_DATABASE_PORT" != "$SECONDBOX_SCENARIO_RUNNER_PORT" &&
   "$SECONDBOX_SCENARIO_DATABASE_PORT" != "$SECONDBOX_SCENARIO_RUNNER_DATA_PLANE_PORT" &&
   "$SECONDBOX_SCENARIO_RUNNER_PORT" != "$SECONDBOX_SCENARIO_RUNNER_DATA_PLANE_PORT" &&
   "$SECONDBOX_SCENARIO_RELOCATION_RUNNER_DATA_PLANE_PORT" != "$SECONDBOX_SCENARIO_API_PORT" &&
   "$SECONDBOX_SCENARIO_RELOCATION_RUNNER_DATA_PLANE_PORT" != "$SECONDBOX_SCENARIO_DATABASE_PORT" &&
   "$SECONDBOX_SCENARIO_RELOCATION_RUNNER_DATA_PLANE_PORT" != "$SECONDBOX_SCENARIO_RUNNER_PORT" &&
   "$SECONDBOX_SCENARIO_RELOCATION_RUNNER_DATA_PLANE_PORT" != "$SECONDBOX_SCENARIO_RUNNER_DATA_PLANE_PORT" ]] ||
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
if [[ "$scenario_backend" != "firecracker" ]]; then
  SECONDBOX_RUNNER_ID=scenario-runner-relocation \
    "$repo_root/scripts/issue-scenario-runner-identity.sh" "$relocation_identity_dir" >/dev/null
fi

jq -n \
  --arg architecture "$architecture" \
  --arg runtime "$runtime_digest" \
  --arg runtimeArtifactID "$runtime_artifact_id" \
  --arg toolchain "$toolchain_digest" \
  --arg toolchainArtifactID "$toolchain_artifact_id" \
  --argjson runtimeFeatures "$runtime_features" \
  --argjson toolchainFeatures "$toolchain_features" \
  --argjson guestProtocolGeneration "$guest_protocol_minimum" \
  '{
    assets: [
      {
        artifactId: $runtimeArtifactID,
        manifestDigest: $runtime,
        architecture: $architecture,
        guestProtocolGeneration: $guestProtocolGeneration,
        mandatoryGuestFeatures: $runtimeFeatures
      },
      {
        artifactId: $toolchainArtifactID,
        manifestDigest: $toolchain,
        architecture: $architecture,
        guestProtocolGeneration: $guestProtocolGeneration,
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
export SECONDBOX_SCENARIO_RELOCATION_IDENTITY_DIR="$relocation_identity_dir"
export SECONDBOX_SCENARIO_RELOCATION_STATE_DIR="$relocation_state_dir"
export SECONDBOX_SCENARIO_RELOCATION_WORKSPACE_DIR="$relocation_workspace_dir"
export SECONDBOX_SCENARIO_RELOCATION_RUNNER_ID=scenario-runner-relocation
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
  if [[ "$native_macos" == "true" ]] || [[ -z "$(ip route show "$scenario_guest_cidr")" ]]; then
    scenario_network_found=true
    break
  fi
done
[[ "$scenario_network_found" == "true" ]] ||
  fail "no unused scenario guest /24 is available in 198.18.0.0/15"
scenario_compose_network_found=false
for offset in $(seq 1 511); do
  scenario_compose_network_index=$(( (scenario_network_index + offset) % 512 ))
  scenario_compose_second_octet=$(( 18 + scenario_compose_network_index / 256 ))
  scenario_compose_third_octet=$(( scenario_compose_network_index % 256 ))
  scenario_compose_cidr="198.${scenario_compose_second_octet}.${scenario_compose_third_octet}.0/24"
  if [[ "$native_macos" == "true" ]] || [[ -z "$(ip route show "$scenario_compose_cidr")" ]]; then
    scenario_compose_network_found=true
    break
  fi
done
[[ "$scenario_compose_network_found" == "true" ]] ||
  fail "no unused scenario Compose /24 is available in 198.18.0.0/15"
export SECONDBOX_SCENARIO_GUEST_CIDR="$scenario_guest_cidr"
export SECONDBOX_SCENARIO_COMPOSE_CIDR="$scenario_compose_cidr"
export SECONDBOX_SCENARIO_BRIDGE_ADDRESS="198.${scenario_network_second_octet}.${scenario_network_third_octet}.1"
export SECONDBOX_SCENARIO_BRIDGE_CIDR="$SECONDBOX_SCENARIO_BRIDGE_ADDRESS/24"
export SECONDBOX_SCENARIO_GUEST_IP="198.${scenario_network_second_octet}.${scenario_network_third_octet}.2"
export SECONDBOX_SCENARIO_EGRESS_CONTEXT_CONFIG="$run_dir/egress-contexts.json"
jq -n --arg address "$SECONDBOX_SCENARIO_BRIDGE_ADDRESS" '{
  schemaVersion: "secondbox.runner-egress-contexts/v1",
  contexts: [
    {name: "scenario-primary", gateways: [
      {logicalName: "agent-gateway.secondbox.internal", address: $address},
      {logicalName: "platform-gateway.secondbox.internal", address: $address}
    ]},
    {name: "scenario-replacement", gateways: [
      {logicalName: "agent-gateway.secondbox.internal", address: $address},
      {logicalName: "platform-gateway.secondbox.internal", address: $address}
    ]}
  ]
}' >"$SECONDBOX_SCENARIO_EGRESS_CONTEXT_CONFIG"
chmod 0600 "$SECONDBOX_SCENARIO_EGRESS_CONTEXT_CONFIG"
export SECONDBOX_SCENARIO_RUNNER_CLIENT_CERTIFICATE=/opt/secondbox-runner-identity/runner.crt
export SECONDBOX_SCENARIO_RUNNER_CREDENTIAL=scenario-runner-credential-000000000000000000000000
export SECONDBOX_SCENARIO_RUNNER_GUEST_HEARTBEAT_INTERVAL=1s
export SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST="$runtime_digest"
export SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST="$toolchain_digest"
export SECONDBOX_SCENARIO_COMPOSE_FILE="$compose_file"
export SECONDBOX_SCENARIO_COMPOSE_OVERRIDE_FILE="$compose_override_file"
export SECONDBOX_SCENARIO_COMPOSE_PROJECT="$project_name"
export SECONDBOX_PLATFORM_TOKEN="scenario-platform-token-0000000000000000"
# The direct Port transport is granted per application authority, never to the
# platform token, so qualifying it requires one explicitly provisioned ingress.
# The deployed interval used by data-plane session cancellation and retention
# recovery sweeps during qualification.
export SECONDBOX_SCENARIO_DATA_PLANE_POLL_INTERVAL_MILLISECONDS="${SECONDBOX_SCENARIO_DATA_PLANE_POLL_INTERVAL_MILLISECONDS:-250}"
export SECONDBOX_SCENARIO_DIRECT_PORT_PROFILE="scenario-direct-port"
export SECONDBOX_LIVE_BASE_URL="http://127.0.0.1:$SECONDBOX_SCENARIO_API_PORT"
export SECONDBOX_SCENARIO_DATABASE_URL="postgresql://secondbox:secondbox-scenario-password@127.0.0.1:$SECONDBOX_SCENARIO_DATABASE_PORT/secondbox_scenario?sslmode=disable"

compose() {
  local -a compose_files=(--file "$compose_file")
  if [[ -n "$compose_override_file" ]]; then
    compose_files+=(--file "$compose_override_file")
  fi
  "${compose_command[@]}" --project-name "$project_name" "${compose_files[@]}" "$@"
}

sweep_host_orphans() {
  if [[ "$scenario_backend" != "firecracker" ]]; then
    return 0
  fi
  SECONDBOX_SCENARIO_SWEEP_IMAGE="$runner_image" \
    "$repo_root/scripts/scenario-sweep-host-orphans.sh"
}

# The Runner writes its host-network state as root into a 0700 directory, so the
# unprivileged suite cannot read it and must not gate the removal on it. The
# setup script reports an inactive host network itself.
remove_host_network() {
  if [[ "$scenario_backend" != "firecracker" ]]; then
    return 0
  fi
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
  if [[ "$scenario_backend" != "firecracker" ]]; then
    return 0
  fi
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
  if [[ "$runner_external" == "true" ]]; then
    "$SECONDBOX_SCENARIO_SERVICE_CONTROL" logs secondbox-runner \
      >"$diagnostics_dir/runner.jsonl" || return 1
  else
    compose exec --no-TTY secondbox-runner \
      /bin/sh -c \
      'test -f /var/lib/secondbox-runner/log/runner.jsonl &&
       cat /var/lib/secondbox-runner/log/runner.jsonl' \
      >"$diagnostics_dir/runner.jsonl" || return 1
  fi
  if ! compose logs --no-color --timestamps \
    control-plane postgres \
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
    if [[ "$runner_external" == "true" ]]; then
      "$SECONDBOX_SCENARIO_SERVICE_CONTROL" logs secondbox-runner >&2 ||
        echo "SecondBox scenario could not collect native runner application logs" >&2
    elif ! compose exec --no-TTY secondbox-runner \
      /bin/sh -c \
      'test ! -f /var/lib/secondbox-runner/log/runner.jsonl ||
       tail -n 500 /var/lib/secondbox-runner/log/runner.jsonl' >&2; then
      echo "SecondBox scenario could not collect runner application logs" >&2
    fi
    if [[ "$scenario_backend" == "firecracker" ]] && ! compose exec --no-TTY secondbox-runner \
      /bin/sh -c \
      'find /var/lib/secondbox-runner/firecracker-log -type f -exec tail -n 200 {} +' >&2; then
      echo "SecondBox scenario could not collect Firecracker logs" >&2
    fi
    if [[ "$runner_external" == "true" ]]; then
      failure_logs=("$SECONDBOX_SCENARIO_SERVICE_CONTROL" logs --tail 200 control-plane secondbox-runner postgres)
    else
      failure_logs=(compose logs --tail 200 control-plane secondbox-runner postgres)
    fi
    if ! "${failure_logs[@]}" >&2; then
      echo "SecondBox scenario could not collect failure logs" >&2
    fi
  fi
  # The Runner container restarts unless it is stopped and every start reapplies
  # host networking, so the bridge can only be removed once the container can no
  # longer come back and recreate it.
  if [[ "$runner_external" == "true" ]]; then
    runner_stop_command=("$SECONDBOX_SCENARIO_SERVICE_CONTROL" stop secondbox-runner)
    "$SECONDBOX_SCENARIO_SERVICE_CONTROL" stop secondbox-runner-relocation >/dev/null 2>&1 || true
  else
    runner_stop_command=(compose stop secondbox-runner)
  fi
  if ! "${runner_stop_command[@]}" >/dev/null 2>&1; then
    echo "SecondBox scenario runner stop failed for $project_name" >&2
    status=1
  fi
  if ! remove_host_network; then
    echo "SecondBox scenario host-network cleanup failed" >&2
    status=1
  fi
  if [[ "$scenario_backend" == "firecracker" ]] &&
     ip link show "$SECONDBOX_SCENARIO_BRIDGE_NAME" >/dev/null 2>&1; then
    echo "SecondBox scenario host-network cleanup left the bridge behind: $SECONDBOX_SCENARIO_BRIDGE_NAME" >&2
    status=1
  fi
  compose_down_arguments=(down --volumes --remove-orphans)
  if [[ "$scenario_backend" != "firecracker" && "$runner_external" != "true" ]]; then
    # Docker Compose excludes inactive profile services from `down`. The
    # relocation runner may be stopped but still retain the locally built
    # image, so activate its profile for complete topology cleanup.
    compose_down_arguments=(--profile relocation "${compose_down_arguments[@]}")
  fi
  if ! compose "${compose_down_arguments[@]}" >/dev/null 2>&1; then
    echo "SecondBox scenario Compose cleanup failed for $project_name" >&2
    status=1
  fi
  if ! remove_propagated_mounts; then
    echo "SecondBox scenario propagated-mount cleanup failed" >&2
    status=1
  fi
  # The jailer creates one cgroup per Instance under this run's parent and never
  # removes it, and a host-network removal that could not run leaves this run's
  # bridge behind. Now that this run's containers are gone both are orphans, so
  # the same sweep that clears earlier runs reclaims them.
  if ! sweep_host_orphans; then
    echo "SecondBox scenario orphan sweep failed" >&2
    status=1
  fi
  if [[ "$scenario_backend" == "firecracker" &&
        -d "/sys/fs/cgroup/$SECONDBOX_SCENARIO_CGROUP_PARENT" ]]; then
    echo "SecondBox scenario cgroup parent survived cleanup: $SECONDBOX_SCENARIO_CGROUP_PARENT" >&2
    status=1
  fi
  if [[ "$native_macos" != "true" ]]; then
    for directory in "$state_dir" "$relocation_state_dir" "$scenario_workspace_dir" "$relocation_workspace_dir"; do
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
  fi
  if [[ -d "$scenario_workspace_dir" ]] && ! rm -rf -- "$scenario_workspace_dir"; then
    echo "SecondBox scenario Workspace cleanup failed: $scenario_workspace_dir" >&2
    status=1
  fi
  if [[ -d "$relocation_workspace_dir" ]] && ! rm -rf -- "$relocation_workspace_dir"; then
    echo "SecondBox scenario relocation Workspace cleanup failed: $relocation_workspace_dir" >&2
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
    if [[ "$scenario_backend" == "firecracker" ]]; then
      evidence_suite="test-scenario"
    elif [[ "$scenario_backend" == "gvisor" ]]; then
      evidence_suite="test-scenario-gvisor"
      if [[ "$runner_placement" == "pod" ]]; then
        evidence_suite="test-scenario-gvisor-pod"
      fi
    else
      evidence_suite="test-scenario-microsandbox-$scenario_host_platform"
    fi
    if ! jq -n \
      --arg schemaVersion "secondbox.release/qualification-evidence/v2" \
      --arg sourceCommit "$scenario_source_commit" \
      --argjson repositoryDirty "$scenario_repository_dirty" \
      --arg suite "$evidence_suite" \
      --arg backend "$scenario_backend" \
      --arg hostPlatform "$scenario_host_platform" \
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
        host: ({workspaceFilesystem: {mount: $workspaceMount, type: $workspaceFilesystem}} +
        if $hostPlatform == "darwin" then {
          platform: "darwin-arm64",
          hypervisorFramework: {supported: true},
          kvm: {required: false},
          tun: {required: false}
        } else {
          platform: "linux-amd64",
          kvm: (if $backend == "gvisor" then {required: false, present: false}
            else {path: "/dev/kvm", present: true, readable: true, writable: true} end),
          tun: (if $backend == "firecracker" then
            {path: "/dev/net/tun", present: true, readable: true, writable: true}
          else {required: false} end)
        } end),
        qualifiedAt: $qualifiedAt
      } + if $backend != "firecracker" then {backend: $backend} else {} end' \
      >"$qualification_evidence_temporary" ||
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
echo "SecondBox scenario Compose network: $SECONDBOX_SCENARIO_COMPOSE_CIDR"

# A run killed with SIGKILL never reaches the EXIT trap, so its bridge and
# cgroup parent survive it. Reclaim those before this run claims host resources
# of its own; a live run's bridge and cgroup parent are never candidates.
sweep_host_orphans

compose config --quiet
compose up --detach --wait --wait-timeout 240 \
  postgres control-plane

if [[ "$scenario_mode" == "suite" ]]; then
  bootstrap_tenant="scenario-tenant"
  bootstrap_subject="scenario-subject"
  bootstrap_profile_grants='["agent-compartment-isolated","scenario-agent-compartment-network-enabled","scenario-concurrent-instance-isolation","scenario-control-restart","scenario-data-paths","scenario-direct-port","scenario-execution","scenario-lifecycle","scenario-microsandbox-cold-start-observation","scenario-microsandbox-relocation","scenario-microsandbox-snapshot-resume-rejected","scenario-network-allow","scenario-network-deny","scenario-no-capacity","scenario-over-capacity","scenario-port-lease","scenario-real-boot","scenario-runner-loss","scenario-snapshot-durability","scenario-snapshot-other-sandbox","scenario-snapshot-resume","scenario-snapshot-retention","scenario-touch-idle","scenario-uncached-materialization","scenario-unsupported-architecture"]'
else
  bootstrap_tenant="$(jq -er '.tenantRef' "$SECONDBOX_STRESS_CONFIG")"
  bootstrap_subject="$(jq -er '.subjectRef' "$SECONDBOX_STRESS_CONFIG")"
  bootstrap_profile_grants="$(jq -c '[.profileName]' "$SECONDBOX_STRESS_CONFIG")"
fi
bootstrap_expiry="$(python3 -c 'from datetime import datetime, timedelta, timezone; print((datetime.now(timezone.utc) + timedelta(hours=24)).isoformat().replace("+00:00", "Z"))')"

scenario_management_post() {
  local token="$1"
  local path="$2"
  local idempotency_key="$3"
  local body="$4"
  curl --fail-with-body --silent --show-error \
    --request POST \
    --header "Authorization: Bearer $token" \
    --header "Content-Type: application/json" \
    --header "Idempotency-Key: $idempotency_key" \
    --data "$body" \
    "$SECONDBOX_LIVE_BASE_URL$path"
}

scenario_management_post "$SECONDBOX_PLATFORM_TOKEN" /v1/tenants scenario-bootstrap-tenant "$(jq -cn \
  --arg ref "$bootstrap_tenant" \
  --arg egressContext "scenario-primary" \
  --argjson profileGrants "$bootstrap_profile_grants" \
  --argjson maxSandboxes "$SECONDBOX_SCENARIO_SUBJECT_MAX_SANDBOXES" \
  --argjson maxActiveInstances "$SECONDBOX_SCENARIO_SUBJECT_MAX_ACTIVE_INSTANCES" \
  --argjson maxVcpuCount "$SECONDBOX_SCENARIO_SUBJECT_MAX_VCPU_COUNT" \
  --argjson maxMemoryBytes "$SECONDBOX_SCENARIO_SUBJECT_MAX_MEMORY_BYTES" \
  --argjson maxSnapshots "$SECONDBOX_SCENARIO_SUBJECT_MAX_SNAPSHOTS" \
  --argjson maxPortSessions "$SECONDBOX_SCENARIO_SUBJECT_MAX_PORT_SESSIONS" \
  --argjson maxConcurrentOperations "$SECONDBOX_SCENARIO_SUBJECT_MAX_CONCURRENT_OPERATIONS" \
  '{ref:$ref,egressContext:$egressContext,allowedProfileGrants:$profileGrants,allowedApplicationScopes:["sandbox:read","sandbox:lifecycle","sandbox:exec","sandbox:files","sandbox:ports","sandbox:ports:direct"],aggregateQuota:{maxSandboxes:$maxSandboxes,maxActiveInstances:$maxActiveInstances,maxVcpuCount:$maxVcpuCount,maxMemoryBytes:$maxMemoryBytes,maxSnapshots:$maxSnapshots,maxPortSessions:$maxPortSessions,maxConcurrentOperations:$maxConcurrentOperations,maxActiveSubjects:2,maxApplicationAuthorities:2},expiryPolicy:{maximumSubjectLifetimeSeconds:86400,maximumAuthorityLifetimeSeconds:86400},metadata:{harness:"scenario"}}')" >/dev/null

controller_response="$(scenario_management_post "$SECONDBOX_PLATFORM_TOKEN" "/v1/tenants/$bootstrap_tenant/controller-authorities" scenario-bootstrap-controller "$(jq -cn --arg expiresAt "$bootstrap_expiry" '{expiresAt:$expiresAt,metadata:{harness:"scenario"}}')")"
controller_token="$(jq -er '.bearerToken' <<<"$controller_response")"

scenario_management_post "$controller_token" /v1/subjects scenario-bootstrap-subject "$(jq -cn \
  --arg ref "$bootstrap_subject" \
  --argjson maxSandboxes "$SECONDBOX_SCENARIO_SUBJECT_MAX_SANDBOXES" \
  --argjson maxActiveInstances "$SECONDBOX_SCENARIO_SUBJECT_MAX_ACTIVE_INSTANCES" \
  --argjson maxVcpuCount "$SECONDBOX_SCENARIO_SUBJECT_MAX_VCPU_COUNT" \
  --argjson maxMemoryBytes "$SECONDBOX_SCENARIO_SUBJECT_MAX_MEMORY_BYTES" \
  --argjson maxSnapshots "$SECONDBOX_SCENARIO_SUBJECT_MAX_SNAPSHOTS" \
  --argjson maxPortSessions "$SECONDBOX_SCENARIO_SUBJECT_MAX_PORT_SESSIONS" \
  --argjson maxConcurrentOperations "$SECONDBOX_SCENARIO_SUBJECT_MAX_CONCURRENT_OPERATIONS" \
  '{ref:$ref,quota:{maxSandboxes:$maxSandboxes,maxActiveInstances:$maxActiveInstances,maxVcpuCount:$maxVcpuCount,maxMemoryBytes:$maxMemoryBytes,maxSnapshots:$maxSnapshots,maxPortSessions:$maxPortSessions,maxConcurrentOperations:$maxConcurrentOperations},metadata:{harness:"scenario"}}')" >/dev/null

application_response="$(scenario_management_post "$controller_token" /v1/application-authorities scenario-bootstrap-application "$(jq -cn \
  --arg subjectRef "$bootstrap_subject" \
  --arg expiresAt "$bootstrap_expiry" \
  --argjson profileGrants "$bootstrap_profile_grants" \
  '{subjectRef:$subjectRef,scopes:["sandbox:read","sandbox:lifecycle","sandbox:exec","sandbox:files","sandbox:ports"],profileGrants:$profileGrants,expiresAt:$expiresAt,metadata:{harness:"scenario"}}')")"
export SECONDBOX_SCENARIO_APPLICATION_TOKEN
SECONDBOX_SCENARIO_APPLICATION_TOKEN="$(jq -er '.bearerToken' <<<"$application_response")"
export SECONDBOX_SCENARIO_TENANT_REF="$bootstrap_tenant"
export SECONDBOX_SCENARIO_SUBJECT_REF="$bootstrap_subject"

if [[ "$scenario_mode" == "suite" ]]; then
  direct_port_application_response="$(scenario_management_post "$controller_token" /v1/application-authorities scenario-bootstrap-direct-port-application "$(jq -cn \
    --arg subjectRef "$bootstrap_subject" \
    --arg expiresAt "$bootstrap_expiry" \
    --arg profileGrant "$SECONDBOX_SCENARIO_DIRECT_PORT_PROFILE" \
    '{subjectRef:$subjectRef,scopes:["sandbox:read","sandbox:lifecycle","sandbox:ports","sandbox:ports:direct"],profileGrants:[$profileGrant],expiresAt:$expiresAt,metadata:{harness:"scenario-direct-port"}}')")"
  export SECONDBOX_SCENARIO_DIRECT_PORT_TOKEN
  SECONDBOX_SCENARIO_DIRECT_PORT_TOKEN="$(jq -er '.bearerToken' <<<"$direct_port_application_response")"
fi

if [[ "$scenario_mode" == "stress" ]]; then
  go run ./tests/scenario/stress \
    --mode prepare \
    --config "$SECONDBOX_STRESS_CONFIG"
elif [[ "$scenario_mode" == "lifecycle" ]]; then
  go run ./tests/scenario/lifecycle \
    --mode prepare \
    --config "$SECONDBOX_STRESS_CONFIG"
fi

if [[ "$scenario_mode" == "suite" && "$scenario_backend" == "firecracker" ]]; then
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

if [[ "$runner_external" == "true" ]]; then
  "$SECONDBOX_SCENARIO_SERVICE_CONTROL" up --detach --wait --wait-timeout 300 secondbox-runner
else
  compose up --detach --wait --wait-timeout 300 secondbox-runner
fi

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
