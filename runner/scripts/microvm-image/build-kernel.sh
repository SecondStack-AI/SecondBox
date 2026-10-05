#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

usage() {
    cat >&2 <<'USAGE'
Usage: build-kernel.sh <output-dir>

Builds the pinned Firecracker guest kernel described by kernel.lock. The output
directory receives the boot image (vmlinux on amd64, the arm64 Image on arm64),
config, System.map, and kernel-provenance.json.

Environment:
  SECONDBOX_RUNNER_MICROVM_ARCHITECTURE      Guest architecture: amd64 or arm64.
  SECONDBOX_RUNNER_MICROVM_KERNEL_LOCK       Lock file with KERNEL_VERSION/URL/SHA256.
  SECONDBOX_RUNNER_MICROVM_KERNEL_CACHE      Download cache directory.
  SECONDBOX_RUNNER_MICROVM_KERNEL_JOBS       Parallel make jobs.
  SECONDBOX_RUNNER_MICROVM_KERNEL_VERIFY_ONLY Verify the locked source tarball and exit.
  SECONDBOX_RUNNER_MICROVM_KERNEL_CROSS_COMPILE
                                             Toolchain prefix such as x86_64-linux-gnu-; required
                                             and may be empty to use the host compiler, which
                                             then must match the guest architecture.
USAGE
}

if [ "${1-}" = "-h" ] || [ "${1-}" = "--help" ]; then
    usage
    exit 0
fi
if [ "$#" -ne 1 ] || [ -z "$1" ]; then
    usage
    exit 2
fi
for required_name in \
    SECONDBOX_RUNNER_MICROVM_ARCHITECTURE \
    SECONDBOX_RUNNER_MICROVM_KERNEL_LOCK \
    SECONDBOX_RUNNER_MICROVM_KERNEL_CACHE \
    SECONDBOX_RUNNER_MICROVM_KERNEL_JOBS \
    SECONDBOX_RUNNER_MICROVM_KERNEL_VERIFY_ONLY
do
    if [ -z "${!required_name-}" ]; then
        echo "$required_name is required" >&2
        exit 2
    fi
done
if [ "${SECONDBOX_RUNNER_MICROVM_KERNEL_CROSS_COMPILE+x}" != x ]; then
    echo "SECONDBOX_RUNNER_MICROVM_KERNEL_CROSS_COMPILE is required and may be empty" >&2
    exit 2
fi
out_dir="$1"
lock_file="$SECONDBOX_RUNNER_MICROVM_KERNEL_LOCK"
cache_dir="$SECONDBOX_RUNNER_MICROVM_KERNEL_CACHE"
jobs="$SECONDBOX_RUNNER_MICROVM_KERNEL_JOBS"
verify_only="$SECONDBOX_RUNNER_MICROVM_KERNEL_VERIFY_ONLY"
architecture="$SECONDBOX_RUNNER_MICROVM_ARCHITECTURE"
cross_compile="$SECONDBOX_RUNNER_MICROVM_KERNEL_CROSS_COMPILE"
# Firecracker boots an uncompressed ELF vmlinux on x86_64 and the PE Image on aarch64.
case "$architecture" in
    amd64) kernel_arch=x86_64; guest_machine=x86_64; kernel_target=vmlinux; kernel_build_output=vmlinux ;;
    arm64) kernel_arch=arm64; guest_machine=aarch64; kernel_target=Image; kernel_build_output=arch/arm64/boot/Image ;;
    *) echo "SECONDBOX_RUNNER_MICROVM_ARCHITECTURE must be amd64 or arm64" >&2; exit 2 ;;
esac
if [ -z "$cross_compile" ] && [ "$(uname -m)" != "$guest_machine" ]; then
    echo "host $(uname -m) cannot build the $architecture guest kernel without SECONDBOX_RUNNER_MICROVM_KERNEL_CROSS_COMPILE" >&2
    exit 2
fi
if [ ! -f "$lock_file" ]; then
    echo "kernel lock file not found: $lock_file" >&2
    exit 2
fi

# shellcheck source=/dev/null
. "$lock_file"
: "${KERNEL_VERSION:?kernel lock missing KERNEL_VERSION}"
: "${KERNEL_URL:?kernel lock missing KERNEL_URL}"
: "${KERNEL_SHA256:?kernel lock missing KERNEL_SHA256}"

for cmd in curl make sha256sum tar xz "${cross_compile}gcc" "${cross_compile}ld"; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "missing required command: $cmd" >&2; exit 2; }
done

mkdir -p "$cache_dir" "$out_dir"
tarball="$cache_dir/linux-$KERNEL_VERSION.tar.xz"
if [ ! -f "$tarball" ]; then
    curl -fL "$KERNEL_URL" -o "$tarball"
fi
printf '%s  %s\n' "$KERNEL_SHA256" "$tarball" | sha256sum -c - >/dev/null
case "$verify_only" in
    1|true|TRUE|yes|YES)
        echo "$tarball"
        exit 0
        ;;
esac

work_dir="$(mktemp -d)"
cleanup() {
    rm -rf "$work_dir"
}
trap cleanup EXIT

tar -C "$work_dir" -xf "$tarball"
src_dir="$work_dir/linux-$KERNEL_VERSION"
build_dir="$work_dir/build"
mkdir -p "$build_dir"

export ARCH="$kernel_arch"
export CROSS_COMPILE="$cross_compile"
export KBUILD_BUILD_USER="secondbox-runner"
export KBUILD_BUILD_HOST="secondbox-runner-ci"
export KBUILD_BUILD_TIMESTAMP
KBUILD_BUILD_TIMESTAMP="$(date -u -d "@${KERNEL_SOURCE_DATE_EPOCH:?kernel lock missing KERNEL_SOURCE_DATE_EPOCH}" '+%Y-%m-%d %H:%M:%S')"
export SOURCE_DATE_EPOCH="$KERNEL_SOURCE_DATE_EPOCH"
export KCFLAGS="-Wno-error=date-time"

make -C "$src_dir" O="$build_dir" defconfig >/dev/null
config_tool="$src_dir/scripts/config"
config_file="$build_dir/.config"

"$config_tool" --file "$config_file" \
    --enable BLK_DEV_INITRD \
    --enable CGROUPS \
    --enable CGROUP_PIDS \
    --enable DEVTMPFS \
    --enable DEVTMPFS_MOUNT \
    --enable EXT4_FS \
    --enable EROFS_FS \
    --enable FUSE_FS \
    --enable IPC_NS \
    --enable MEMCG \
    --enable NAMESPACES \
    --enable NET \
    --enable NET_NS \
    --enable PID_NS \
    --enable SECCOMP \
    --enable SECCOMP_FILTER \
    --enable SERIAL_8250 \
    --enable SERIAL_8250_CONSOLE \
    --enable TTY \
    --enable UNIX \
    --enable USER_NS \
    --enable HW_RANDOM_VIRTIO \
    --enable IP_PNP \
    --enable UTS_NS \
    --enable VIRTIO \
    --enable VIRTIO_BLK \
    --enable VIRTIO_CONSOLE \
    --enable VIRTIO_MMIO \
    --enable VIRTIO_MMIO_CMDLINE_DEVICES \
    --enable VIRTIO_NET \
    --enable VIRTIO_VSOCKETS \
    --enable VSOCKETS \
    --set-str SYSTEM_TRUSTED_KEYS "" \
    --set-str SYSTEM_REVOCATION_KEYS ""
# Firecracker exposes a paravirtual clock on x86_64, and an FDT-described 8250
# UART and PL031 RTC on aarch64.
case "$architecture" in
    amd64)
        "$config_tool" --file "$config_file" --enable KVM_GUEST
        ;;
    arm64)
        "$config_tool" --file "$config_file" \
            --enable SERIAL_OF_PLATFORM \
            --enable RTC_CLASS \
            --enable RTC_DRV_PL031
        ;;
esac

make -C "$src_dir" O="$build_dir" olddefconfig >/dev/null
"$script_dir/check-kernel-config.sh" "$config_file" "$architecture"
make -C "$src_dir" O="$build_dir" -j"$jobs" "$kernel_target" >/dev/null

install -m 0644 "$build_dir/$kernel_build_output" "$out_dir/$kernel_target"
install -m 0644 "$config_file" "$out_dir/config"
install -m 0644 "$build_dir/System.map" "$out_dir/System.map"

kernel_sha="$(sha256sum "$out_dir/$kernel_target" | awk '{print $1}')"
config_sha="$(sha256sum "$out_dir/config" | awk '{print $1}')"
compiler_version="$(sed -n 's/^CONFIG_CC_VERSION_TEXT="\(.*\)"$/\1/p' "$out_dir/config")"
created_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
git_commit="$(git -C "$repo_root" rev-parse HEAD 2>/dev/null || true)"
cat > "$out_dir/kernel-provenance.json" <<EOF
{
  "mode": "pinned-source-build",
  "architecture": "$architecture",
  "createdAt": "$created_at",
  "source": {
    "version": "$KERNEL_VERSION",
    "url": "$KERNEL_URL",
    "sha256": "$KERNEL_SHA256"
  },
  "toolchain": {
    "crossCompile": "$CROSS_COMPILE",
    "compiler": "$compiler_version"
  },
  "reproducibility": {
    "sourceDateEpoch": "${SOURCE_DATE_EPOCH}",
    "kbuildBuildUser": "$KBUILD_BUILD_USER",
    "kbuildBuildHost": "$KBUILD_BUILD_HOST",
    "kbuildBuildTimestamp": "$KBUILD_BUILD_TIMESTAMP"
  },
  "outputs": {
    "kernel": {"path": "$kernel_target", "sha256": "$kernel_sha"},
    "config": {"path": "config", "sha256": "$config_sha"}
  },
  "builder": {
    "script": "scripts/microvm-image/build-kernel.sh",
    "gitCommit": "$git_commit"
  }
}
EOF

echo "$out_dir/$kernel_target"
