#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# Snapshot resume requires the jailer. Firecracker opens every block device at
# the path the VM state recorded, during the load itself, so a restored Instance
# receives its own disks only when those paths are chroot-relative names it
# resolves inside its own jail. The jailer chroots, creates device nodes, chowns,
# and drops UID, none of which the unprivileged `just test-snapshot-resume` gate
# can do — so this qualification runs the compiled gate as root inside a
# privileged container with /dev/kvm, exactly as the scenario suite runs every
# other privileged gate.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
runner_image="secondbox-snapshot-resume-jailed-$$"
qualification_root=""
tools_root=""
container_id=""
container_name="secondbox-snapshot-resume-jailed-$$"
workspace_root=""
cgroup_parent="secondbox-resume-jailed-$$"
# The gate runs in its own network namespace, so the bridge, its subnet, and the
# TAP prefix are private to this run. A template is captured with a network
# device and every Instance resumes onto its own TAP, because the pinned VMM can
# rebind a snapshotted interface but cannot add one the capture did not record.
bridge_name="sbrj$(( $$ % 100000 ))"
tap_prefix="rj$(( $$ % 1000 ))"
bridge_cidr="198.18.$(( $$ % 200 + 40 )).1/24"

fail() {
  echo "SecondBox jailed snapshot-resume qualification prerequisite failed: $*" >&2
  exit 1
}

cleanup() {
  local status=$?
  if [[ -n "$container_id" ]]; then
    docker rm --force "$container_id" >/dev/null 2>&1 || true
  fi
  docker rm --force "$container_name" >/dev/null 2>&1 || true
  # The jailer creates one cgroup per Instance under the run's parent cgroup and
  # never removes it. Reclaim the tree from a privileged container, because the
  # qualification host has no passwordless sudo.
  if [[ -d "/sys/fs/cgroup/$cgroup_parent" ]]; then
    docker run --rm --privileged --cgroupns=host --network none \
      --volume /sys/fs/cgroup:/sys/fs/cgroup:rw \
      "$runner_image" \
      /bin/bash -c "find '/sys/fs/cgroup/$cgroup_parent' -depth -type d -exec rmdir {} + || true" \
      >/dev/null 2>&1 || true
  fi
  if [[ -n "$qualification_root" &&
        -n "$workspace_root" &&
        "$qualification_root" == "$workspace_root"/secondbox-resume-jailed.* ]]; then
    # Files staged inside a jail belong to per-Instance jailer UIDs, so reclaim
    # ownership from a privileged container before removing the tree.
    docker run --rm --privileged --network none \
      --volume "$qualification_root:/q" \
      "$runner_image" \
      /bin/bash -c "chown -R $(id -u):$(id -g) /q || true" >/dev/null 2>&1 || true
    rm -rf -- "$qualification_root"
  fi
  docker image rm "$runner_image" >/dev/null 2>&1 || true
  if [[ -n "$tools_root" && "$tools_root" == "$repo_root"/.tmp/snapshot-resume-jailed-tools.* ]]; then
    rm -rf -- "$tools_root"
  fi
  exit "$status"
}
trap cleanup EXIT

if [[ "${SECONDBOX_REQUIRE_QUALIFIED_SNAPSHOT_RESUME_JAILED:-}" != "1" ]]; then
  cat >&2 <<'PREREQUISITES'
SecondBox jailed snapshot-resume qualification is explicit and never skips.
Set SECONDBOX_REQUIRE_QUALIFIED_SNAPSHOT_RESUME_JAILED=1 and provide:
  /dev/kvm
  /dev/net/tun
  a cgroup v2 host
  SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY
  SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256
  SECONDBOX_RUNNER_WORKSPACE_ROOT on XFS or Btrfs
  SECONDBOX_SNAPSHOT_RESUME_JAILED_MEMORY_MIB
  SECONDBOX_SNAPSHOT_RESUME_JAILED_WORKSPACE_MIB
  SECONDBOX_SNAPSHOT_RESUME_JAILED_CONCURRENCY (ascending, comma-separated)
  SECONDBOX_SNAPSHOT_RESUME_JAILED_OUTPUT (absent absolute path)
PREREQUISITES
  exit 1
fi

for command in awk chmod docker findmnt git go id mktemp openssl realpath sha256sum uname; do
  command -v "$command" >/dev/null 2>&1 || fail "missing command: $command"
done
[[ -c /dev/kvm && -r /dev/kvm && -w /dev/kvm ]] ||
  fail "/dev/kvm must be a readable and writable character device"
[[ -c /dev/net/tun && -r /dev/net/tun && -w /dev/net/tun ]] ||
  fail "/dev/net/tun must be a readable and writable character device"
[[ -r /sys/fs/cgroup/cgroup.controllers ]] || fail "a cgroup v2 host is required"
[[ "$(go env GOARCH)" == "amd64" ]] || fail "qualification currently requires an amd64 host"

: "${SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR:?jailed resume qualification requires SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR}"
: "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY:?jailed resume qualification requires SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY}"
: "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256:?jailed resume qualification requires SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256}"
: "${SECONDBOX_RUNNER_WORKSPACE_ROOT:?jailed resume qualification requires SECONDBOX_RUNNER_WORKSPACE_ROOT}"
: "${SECONDBOX_SNAPSHOT_RESUME_JAILED_MEMORY_MIB:?jailed resume qualification requires SECONDBOX_SNAPSHOT_RESUME_JAILED_MEMORY_MIB}"
: "${SECONDBOX_SNAPSHOT_RESUME_JAILED_WORKSPACE_MIB:?jailed resume qualification requires SECONDBOX_SNAPSHOT_RESUME_JAILED_WORKSPACE_MIB}"
: "${SECONDBOX_SNAPSHOT_RESUME_JAILED_CONCURRENCY:?jailed resume qualification requires SECONDBOX_SNAPSHOT_RESUME_JAILED_CONCURRENCY}"
: "${SECONDBOX_SNAPSHOT_RESUME_JAILED_OUTPUT:?jailed resume qualification requires SECONDBOX_SNAPSHOT_RESUME_JAILED_OUTPUT}"

artifacts_dir="$SECONDBOX_SCENARIO_MICROVM_ARTIFACTS_DIR"
workspace_root="$SECONDBOX_RUNNER_WORKSPACE_ROOT"
public_key="$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY"
output_path="$SECONDBOX_SNAPSHOT_RESUME_JAILED_OUTPUT"
for directory in "$artifacts_dir" "$workspace_root"; do
  [[ "$directory" = /* && "$(realpath -e "$directory")" == "$directory" && ! -L "$directory" && -d "$directory" ]] ||
    fail "directory must be an existing clean absolute non-symlink path: $directory"
done
[[ "$public_key" = /* && "$(realpath -e "$public_key")" == "$public_key" && ! -L "$public_key" && -f "$public_key" ]] ||
  fail "SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY must be an existing clean absolute non-symlink file"
[[ "$output_path" = /* && ! -e "$output_path" && ! -L "$output_path" ]] ||
  fail "SECONDBOX_SNAPSHOT_RESUME_JAILED_OUTPUT must be an absent absolute path"
output_parent="$(dirname "$output_path")"
[[ "$(realpath -e "$output_parent")" == "$output_parent" && ! -L "$output_parent" && -d "$output_parent" ]] ||
  fail "SECONDBOX_SNAPSHOT_RESUME_JAILED_OUTPUT parent must be an existing clean absolute non-symlink directory"
[[ "$SECONDBOX_SNAPSHOT_RESUME_JAILED_MEMORY_MIB" =~ ^[1-9][0-9]*$ ]] ||
  fail "SECONDBOX_SNAPSHOT_RESUME_JAILED_MEMORY_MIB must be a positive integer"
[[ "$SECONDBOX_SNAPSHOT_RESUME_JAILED_WORKSPACE_MIB" =~ ^[1-9][0-9]*$ ]] ||
  fail "SECONDBOX_SNAPSHOT_RESUME_JAILED_WORKSPACE_MIB must be a positive integer"
[[ "$SECONDBOX_SNAPSHOT_RESUME_JAILED_CONCURRENCY" =~ ^[1-9][0-9]*(,[1-9][0-9]*)*$ ]] ||
  fail "SECONDBOX_SNAPSHOT_RESUME_JAILED_CONCURRENCY must be comma-separated positive integers without spaces"

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

maximum_concurrency="$(
  awk -F, '{max=0; for (i=1; i<=NF; i++) if ($i+0 > max) max=$i+0; print max}' \
    <<<"$SECONDBOX_SNAPSHOT_RESUME_JAILED_CONCURRENCY"
)"

mkdir -p "$repo_root/.tmp"
tools_root="$(mktemp -d "$repo_root/.tmp/snapshot-resume-jailed-tools.XXXXXX")"
qualification_root="$(mktemp -d "$workspace_root/secondbox-resume-jailed.XXXXXX")"
chmod 0755 "$qualification_root"
mkdir -p "$qualification_root"/{run,j,tmp,out}

docker build --quiet --file "$repo_root/runner/Dockerfile" --tag "$runner_image" "$repo_root" >/dev/null

# The gate is compiled on the host, where the module cache lives, and executed
# inside the container. CGO is disabled so an Arch-built binary runs on the
# Debian-based runner image.
(
  cd "$repo_root/runner"
  CGO_ENABLED=0 go test -c ./internal/firecracker -o "$tools_root/snapshot-resume-jailed.test"
)
chmod 0755 "$tools_root" "$tools_root/snapshot-resume-jailed.test"

source_tree_dirty=false
if [[ -n "$(git -C "$repo_root" status --short --untracked-files=normal)" ]]; then
  source_tree_dirty=true
fi
host_cpu="$(awk -F ': ' '/^model name[[:space:]]*:/{print $2; exit}' /proc/cpuinfo)"
[[ -n "$host_cpu" ]] || host_cpu="unknown"

env_file="$qualification_root/gate.env"
cat >"$env_file" <<ENVIRONMENT
SECONDBOX_RUNNER_QUALIFY_SNAPSHOT_RESUME_JAILED=1
SECONDBOX_SNAPSHOT_RESUME_JAILED_MEMORY_MIB=$SECONDBOX_SNAPSHOT_RESUME_JAILED_MEMORY_MIB
SECONDBOX_SNAPSHOT_RESUME_JAILED_WORKSPACE_MIB=$SECONDBOX_SNAPSHOT_RESUME_JAILED_WORKSPACE_MIB
SECONDBOX_SNAPSHOT_RESUME_JAILED_CONCURRENCY=$SECONDBOX_SNAPSHOT_RESUME_JAILED_CONCURRENCY
SECONDBOX_SNAPSHOT_RESUME_JAILED_OUTPUT=/q/out/jailed-resume.json
SECONDBOX_RUNNER_FIRECRACKER_PATH=/usr/local/bin/firecracker
SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH=/usr/local/bin/jailer
SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT=/q/j
SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_START=10001
SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID=10001
SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION=2
SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT=$cgroup_parent
SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH=/opt/secondbox-artifacts/kernel
SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH=/opt/secondbox-artifacts/rootfs.ext4
SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH=/opt/secondbox-artifacts/shared.img
SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY=/opt/secondbox-artifact-public-key.pem
SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256=$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256
SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS=console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw quiet loglevel=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/init
SECONDBOX_RUNNER_FIRECRACKER_BRIDGE_NAME=$bridge_name
SECONDBOX_RUNNER_FIRECRACKER_BRIDGE_CIDR=$bridge_cidr
SECONDBOX_RUNNER_FIRECRACKER_TAP_PREFIX=$tap_prefix
SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH=/usr/sbin/nft
SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS=256
SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL=5m
SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM=1.1.1.1:53
SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT=1024
SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT=1025
SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL=1s
SECONDBOX_RUNNER_QUALIFICATION_TEMP_ROOT=/q/tmp
SECONDBOX_SNAPSHOT_QUALIFICATION_RUN_ROOT=/q/run
SECONDBOX_SNAPSHOT_QUALIFICATION_SOURCE_COMMIT=$(git -C "$repo_root" rev-parse HEAD)
SECONDBOX_SNAPSHOT_QUALIFICATION_SOURCE_TREE_DIRTY=$source_tree_dirty
SECONDBOX_SNAPSHOT_QUALIFICATION_HOST_KERNEL=$(uname -srmo)
SECONDBOX_SNAPSHOT_QUALIFICATION_HOST_CPU=$host_cpu
SECONDBOX_SNAPSHOT_QUALIFICATION_WORKSPACE_FILESYSTEM=$workspace_fstype
SECONDBOX_GATE_HOST_UID=$(id -u)
SECONDBOX_GATE_HOST_GID=$(id -g)
SECONDBOX_GATE_MAXIMUM_CONCURRENCY=$maximum_concurrency
TMPDIR=/q/tmp
ENVIRONMENT

cat >"$qualification_root/run-gate.sh" <<'GATE'
#!/usr/bin/env bash
set -Eeuo pipefail
status=0
/opt/secondbox-gate.test \
  -test.run '^TestSmokeJailedSnapshotResume$' \
  -test.count=1 \
  -test.timeout=120m \
  -test.v || status=$?
# Reclaim the jailer's cgroup tree and host ownership so the caller can clean up
# without root.
if [[ -d "/sys/fs/cgroup/$SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT" ]]; then
  find "/sys/fs/cgroup/$SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT" -depth -type d -exec rmdir {} + || true
fi
chown -R "$SECONDBOX_GATE_HOST_UID:$SECONDBOX_GATE_HOST_GID" /q || true
exit "$status"
GATE
chmod 0755 "$qualification_root/run-gate.sh"

docker run \
  --name "$container_name" \
  --rm \
  --privileged \
  --cgroupns=host \
  --network none \
  --device /dev/kvm:/dev/kvm \
  --device /dev/net/tun:/dev/net/tun \
  --env-file "$env_file" \
  --volume "$qualification_root:/q" \
  --volume "$artifacts_dir:/opt/secondbox-artifacts:ro" \
  --volume "$public_key:/opt/secondbox-artifact-public-key.pem:ro" \
  --volume "$tools_root/snapshot-resume-jailed.test:/opt/secondbox-gate.test:ro" \
  --volume /sys/fs/cgroup:/sys/fs/cgroup:rw \
  --entrypoint /bin/bash \
  "$runner_image" \
  /q/run-gate.sh

[[ -f "$qualification_root/out/jailed-resume.json" ]] ||
  fail "the jailed resume gate produced no evidence"
cp "$qualification_root/out/jailed-resume.json" "$output_path"
chmod 0644 "$output_path"

echo "Jailed snapshot-resume qualification passed; evidence: $output_path"
