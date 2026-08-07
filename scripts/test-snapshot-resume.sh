#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
runner_image="secondbox-snapshot-load-qualification-$$"
qualification_root=""
tools_root=""
container_id=""
workspace_root=""
run_alias=""

fail() {
  echo "SecondBox snapshot-load qualification prerequisite failed: $*" >&2
  exit 1
}

cleanup() {
  local status=$?
  if [[ -n "$container_id" ]]; then
    docker rm "$container_id" >/dev/null 2>&1 || true
  fi
  docker image rm "$runner_image" >/dev/null 2>&1 || true
  if [[ -n "$run_alias" && -L "$run_alias" &&
        "$(readlink "$run_alias")" == "$qualification_root/run" ]]; then
    rm -- "$run_alias"
  fi
  if [[ -n "$qualification_root" &&
        -n "$workspace_root" &&
        "$qualification_root" == "$workspace_root"/secondbox-snapshot-load.* ]]; then
    rm -r -- "$qualification_root"
  fi
  if [[ -n "$tools_root" && "$tools_root" == "$repo_root"/.tmp/snapshot-load-tools.* ]]; then
    rm -r -- "$tools_root"
  fi
  exit "$status"
}
trap cleanup EXIT

if [[ "${SECONDBOX_REQUIRE_QUALIFIED_SNAPSHOT_LOAD:-}" != "1" ]]; then
  cat >&2 <<'PREREQUISITES'
SecondBox snapshot-load qualification is explicit and never skips.
Set SECONDBOX_REQUIRE_QUALIFIED_SNAPSHOT_LOAD=1 and provide:
  /dev/kvm
  SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256
  SECONDBOX_RUNNER_WORKSPACE_ROOT on XFS or Btrfs
  SECONDBOX_SNAPSHOT_QUALIFICATION_MEMORY_MIB (comma-separated)
  SECONDBOX_SNAPSHOT_QUALIFICATION_WARM_ITERATIONS
  SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_MIB
  SECONDBOX_SNAPSHOT_QUALIFICATION_OUTPUT (absent absolute path)
  SECONDBOX_SNAPSHOT_RESUME_MEMORY_MIB
  SECONDBOX_SNAPSHOT_RESUME_WORKSPACE_MIB
  SECONDBOX_SNAPSHOT_RESUME_OUTPUT (absent absolute path)
PREREQUISITES
  exit 1
fi

for command in awk chmod docker findmnt git go ln mktemp openssl readlink realpath sha256sum uname; do
  command -v "$command" >/dev/null 2>&1 || fail "missing command: $command"
done
[[ -c /dev/kvm && -r /dev/kvm && -w /dev/kvm ]] ||
  fail "/dev/kvm must be a readable and writable character device"
[[ "$(go env GOARCH)" == "amd64" ]] || fail "qualification currently requires an amd64 host"

: "${SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR:?snapshot qualification requires SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR}"
: "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY:?snapshot qualification requires SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY}"
: "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256:?snapshot qualification requires SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256}"
: "${SECONDBOX_RUNNER_WORKSPACE_ROOT:?snapshot qualification requires SECONDBOX_RUNNER_WORKSPACE_ROOT}"
: "${SECONDBOX_SNAPSHOT_QUALIFICATION_MEMORY_MIB:?snapshot qualification requires SECONDBOX_SNAPSHOT_QUALIFICATION_MEMORY_MIB}"
: "${SECONDBOX_SNAPSHOT_QUALIFICATION_WARM_ITERATIONS:?snapshot qualification requires SECONDBOX_SNAPSHOT_QUALIFICATION_WARM_ITERATIONS}"
: "${SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_MIB:?snapshot qualification requires SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_MIB}"
: "${SECONDBOX_SNAPSHOT_QUALIFICATION_OUTPUT:?snapshot qualification requires SECONDBOX_SNAPSHOT_QUALIFICATION_OUTPUT}"
: "${SECONDBOX_SNAPSHOT_RESUME_MEMORY_MIB:?snapshot qualification requires SECONDBOX_SNAPSHOT_RESUME_MEMORY_MIB}"
: "${SECONDBOX_SNAPSHOT_RESUME_WORKSPACE_MIB:?snapshot qualification requires SECONDBOX_SNAPSHOT_RESUME_WORKSPACE_MIB}"
: "${SECONDBOX_SNAPSHOT_RESUME_OUTPUT:?snapshot qualification requires SECONDBOX_SNAPSHOT_RESUME_OUTPUT}"

artifacts_dir="$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR"
workspace_root="$SECONDBOX_RUNNER_WORKSPACE_ROOT"
public_key="$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"
output_path="$SECONDBOX_SNAPSHOT_QUALIFICATION_OUTPUT"
for directory in "$artifacts_dir" "$workspace_root"; do
  [[ "$directory" = /* && "$(realpath -e "$directory")" == "$directory" && ! -L "$directory" && -d "$directory" ]] ||
    fail "directory must be an existing clean absolute non-symlink path: $directory"
done
[[ "$public_key" = /* && "$(realpath -e "$public_key")" == "$public_key" && ! -L "$public_key" && -f "$public_key" ]] ||
  fail "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY must be an existing clean absolute non-symlink file"
[[ "$output_path" = /* && ! -e "$output_path" && ! -L "$output_path" ]] ||
  fail "SECONDBOX_SNAPSHOT_QUALIFICATION_OUTPUT must be an absent absolute path"
output_parent="$(dirname "$output_path")"
[[ "$(realpath -e "$output_parent")" == "$output_parent" && ! -L "$output_parent" && -d "$output_parent" ]] ||
  fail "SECONDBOX_SNAPSHOT_QUALIFICATION_OUTPUT parent must be an existing clean absolute non-symlink directory"
resume_output_path="$SECONDBOX_SNAPSHOT_RESUME_OUTPUT"
[[ "$resume_output_path" = /* && ! -e "$resume_output_path" && ! -L "$resume_output_path" ]] ||
  fail "SECONDBOX_SNAPSHOT_RESUME_OUTPUT must be an absent absolute path"
resume_output_parent="$(dirname "$resume_output_path")"
[[ "$(realpath -e "$resume_output_parent")" == "$resume_output_parent" &&
   ! -L "$resume_output_parent" && -d "$resume_output_parent" ]] ||
  fail "SECONDBOX_SNAPSHOT_RESUME_OUTPUT parent must be an existing clean absolute non-symlink directory"
[[ "$SECONDBOX_SNAPSHOT_RESUME_MEMORY_MIB" =~ ^[1-9][0-9]*$ ]] ||
  fail "SECONDBOX_SNAPSHOT_RESUME_MEMORY_MIB must be a positive integer"
[[ "$SECONDBOX_SNAPSHOT_RESUME_WORKSPACE_MIB" =~ ^[1-9][0-9]*$ ]] ||
  fail "SECONDBOX_SNAPSHOT_RESUME_WORKSPACE_MIB must be a positive integer"
[[ "$SECONDBOX_SNAPSHOT_QUALIFICATION_MEMORY_MIB" =~ ^[1-9][0-9]*(,[1-9][0-9]*)*$ ]] ||
  fail "SECONDBOX_SNAPSHOT_QUALIFICATION_MEMORY_MIB must be comma-separated positive integers without spaces"
[[ "$SECONDBOX_SNAPSHOT_QUALIFICATION_WARM_ITERATIONS" =~ ^[1-9][0-9]*$ ]] ||
  fail "SECONDBOX_SNAPSHOT_QUALIFICATION_WARM_ITERATIONS must be a positive integer"
[[ "$SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_MIB" =~ ^[1-9][0-9]*$ ]] ||
  fail "SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_MIB must be a positive integer"

workspace_fstype="$(findmnt -T "$workspace_root" -n -o FSTYPE)"
if [[ "$workspace_fstype" != "xfs" && "$workspace_fstype" != "btrfs" ]]; then
  fail "SECONDBOX_RUNNER_WORKSPACE_ROOT must be on XFS or Btrfs, got $workspace_fstype"
fi

required_artifacts=(SHA256SUMS kernel manifest.json manifest.sig rootfs.ext4 shared.img)
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

mkdir -p "$repo_root/.tmp"
qualification_root="$(mktemp -d "$workspace_root/secondbox-snapshot-load.XXXXXX")"
tools_root="$(mktemp -d "$repo_root/.tmp/snapshot-load-tools.XXXXXX")"
mkdir -p "$qualification_root/run"
run_alias="/tmp/sq$$"
[[ ! -e "$run_alias" && ! -L "$run_alias" ]] || fail "short qualification run alias already exists: $run_alias"
ln -s "$qualification_root/run" "$run_alias"

docker build --quiet --file "$repo_root/runner/Dockerfile" --tag "$runner_image" "$repo_root" >/dev/null
container_id="$(docker create "$runner_image")"
docker cp "$container_id:/usr/local/bin/firecracker" "$tools_root/firecracker"
docker rm "$container_id" >/dev/null
container_id=""
chmod 0755 "$tools_root/firecracker"

source_tree_dirty=false
if [[ -n "$(git -C "$repo_root" status --short --untracked-files=normal)" ]]; then
  source_tree_dirty=true
fi
host_cpu="$(awk -F ': ' '/^model name[[:space:]]*:/{print $2; exit}' /proc/cpuinfo)"
[[ -n "$host_cpu" ]] || host_cpu="unknown"

cd "$repo_root/runner"
SECONDBOX_RUNNER_QUALIFY_SNAPSHOT_LOAD=1 \
SECONDBOX_RUNNER_QUALIFY_SNAPSHOT_RESUME=1 \
SECONDBOX_SNAPSHOT_RESUME_MEMORY_MIB="$SECONDBOX_SNAPSHOT_RESUME_MEMORY_MIB" \
SECONDBOX_SNAPSHOT_RESUME_WORKSPACE_MIB="$SECONDBOX_SNAPSHOT_RESUME_WORKSPACE_MIB" \
SECONDBOX_SNAPSHOT_RESUME_OUTPUT="$resume_output_path" \
SECONDBOX_RUNNER_FIRECRACKER_PATH="$tools_root/firecracker" \
SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH="$artifacts_dir/kernel" \
SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH="$artifacts_dir/rootfs.ext4" \
SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH="$artifacts_dir/shared.img" \
SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY="$public_key" \
SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256="$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256" \
SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS="console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw quiet loglevel=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init" \
SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT=1024 \
SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT=1025 \
SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL=1s \
SECONDBOX_RUNNER_QUALIFICATION_TEMP_ROOT="$qualification_root" \
SECONDBOX_SNAPSHOT_QUALIFICATION_RUN_ROOT="$run_alias" \
SECONDBOX_SNAPSHOT_QUALIFICATION_MEMORY_MIB="$SECONDBOX_SNAPSHOT_QUALIFICATION_MEMORY_MIB" \
SECONDBOX_SNAPSHOT_QUALIFICATION_WARM_ITERATIONS="$SECONDBOX_SNAPSHOT_QUALIFICATION_WARM_ITERATIONS" \
SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_MIB="$SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_MIB" \
SECONDBOX_SNAPSHOT_QUALIFICATION_OUTPUT="$output_path" \
SECONDBOX_SNAPSHOT_QUALIFICATION_SOURCE_COMMIT="$(git -C "$repo_root" rev-parse HEAD)" \
SECONDBOX_SNAPSHOT_QUALIFICATION_SOURCE_TREE_DIRTY="$source_tree_dirty" \
SECONDBOX_SNAPSHOT_QUALIFICATION_HOST_KERNEL="$(uname -srmo)" \
SECONDBOX_SNAPSHOT_QUALIFICATION_HOST_CPU="$host_cpu" \
SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_FILESYSTEM="$workspace_fstype" \
go test ./internal/firecracker \
  -run '^(TestSmokeSnapshotResumeLoadMeasurement|TestSmokeSnapshotResumeTemplateLifecycle)$' \
  -count=1 \
  -timeout=60m \
  -v

echo "Snapshot-load qualification passed; evidence: $output_path"
echo "Snapshot-resume template qualification passed; evidence: $resume_output_path"
