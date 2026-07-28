#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

fail_prerequisite() {
  echo "SecondBox Firecracker qualification prerequisite missing: $*" >&2
  echo "See docs/operations/qualification.md for the complete qualified-host contract." >&2
  exit 1
}

[[ "$(uname -s)" == "Linux" ]] || fail_prerequisite "Linux host"
[[ "$(id -u)" == "0" ]] || fail_prerequisite "run as root on the dedicated qualification host"
[[ "${SECONDBOX_FIRECRACKER_QUALIFIED_HOST-}" == "1" ]] ||
  fail_prerequisite "set SECONDBOX_FIRECRACKER_QUALIFIED_HOST=1 after dedicating the host"

for command in go ip iptables ip6tables nft mkfs.ext4 findmnt mountpoint sysctl timeout; do
  command -v "$command" >/dev/null 2>&1 || fail_prerequisite "command $command"
done

[[ -r /dev/kvm && -w /dev/kvm ]] || fail_prerequisite "read/write /dev/kvm"
[[ -c /dev/net/tun && -r /dev/net/tun && -w /dev/net/tun ]] ||
  fail_prerequisite "read/write /dev/net/tun"
mountpoint -q /sys/fs/cgroup || fail_prerequisite "mounted cgroup v2 filesystem"
[[ -f /sys/fs/cgroup/cgroup.controllers ]] || fail_prerequisite "cgroup v2 controllers"
for controller in cpu memory pids; do
  grep -qw "$controller" /sys/fs/cgroup/cgroup.controllers ||
    fail_prerequisite "cgroup v2 $controller controller"
done

: "${SECONDBOX_RUNNER_MICROVM_ARTIFACTS_DIR:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_MICROVM_ARTIFACTS_DIR}"
: "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY}"
: "${SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256}"
: "${SECONDBOX_RUNNER_FIRECRACKER_PATH:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_FIRECRACKER_PATH}"
: "${SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH}"
: "${SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH}"
: "${SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS}"
: "${SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL}"
: "${SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES}"
: "${SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS}"
: "${SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM}"
: "${SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT}"
: "${SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT}"
: "${SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT}"
: "${SECONDBOX_RUNNER_CHECKPOINT_RESTORE_SPOOL_DIR:?SecondBox Firecracker qualification prerequisite missing: SECONDBOX_RUNNER_CHECKPOINT_RESTORE_SPOOL_DIR}"

[[ -x "$SECONDBOX_RUNNER_FIRECRACKER_PATH" ]] || fail_prerequisite "executable SECONDBOX_RUNNER_FIRECRACKER_PATH"
[[ -x "$SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH" ]] || fail_prerequisite "executable SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH"
[[ -x "$SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH" ]] || fail_prerequisite "executable SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH"
[[ -d "$SECONDBOX_RUNNER_MICROVM_ARTIFACTS_DIR" ]] || fail_prerequisite "signed artifact directory"
[[ -r "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY" ]] || fail_prerequisite "artifact public key"

"$repo_root/runner/scripts/microvm-image/verify.sh" \
  "$SECONDBOX_RUNNER_MICROVM_ARTIFACTS_DIR" \
  "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY" \
  "$SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256"

export SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH="$SECONDBOX_RUNNER_MICROVM_ARTIFACTS_DIR/kernel"
export SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH="$SECONDBOX_RUNNER_MICROVM_ARTIFACTS_DIR/rootfs.ext4"
export SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH="$SECONDBOX_RUNNER_MICROVM_ARTIFACTS_DIR/shared.img"

"$repo_root/runner/scripts/microvm-stage-check.sh"
"$repo_root/runner/scripts/microvm-network-namespace-test.sh"

export SECONDBOX_RUNNER_QUALIFY_FIRECRACKER=1
export SECONDBOX_RUNNER_QUALIFY_GENERATED_IMAGE=1
export SECONDBOX_RUNNER_QUALIFY_TOOL_EXECUTOR=1
export SECONDBOX_RUNNER_QUALIFY_SNAPSHOT=1
export SECONDBOX_RUNNER_QUALIFY_THREAT=1
export SECONDBOX_RUNNER_QUALIFY_JAILED_NETWORK=1

(
  cd "$repo_root/runner"
  go test ./internal/firecracker \
    -run '^(TestSmokeBootFirecracker|TestSmokeGeneratedImageBootsControlAndRuntime|TestSmokeGeneratedToolExecutorImageReadiness|TestSmokeGoldenSnapshotCreateGeneratedImage|TestSmokeJailedTapGeneratedImage|TestThreatModelJailedGuestEscapeAndResourceExhaustion)$' \
    -count=1 \
    -v
)

echo "SecondBox Firecracker qualification passed for signed artifacts and the host isolation boundary"
