#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

out_dir="${1:-${AGENT_MANAGER_MICROVM_KERNEL_OUT_DIR:-$repo_root/tmp/microvm-kernel}}"
lock_file="${AGENT_MANAGER_MICROVM_KERNEL_LOCK:-$script_dir/kernel.lock}"
cache_dir="${AGENT_MANAGER_MICROVM_KERNEL_CACHE:-$repo_root/tmp/microvm-kernel-cache}"
jobs="${AGENT_MANAGER_MICROVM_KERNEL_JOBS:-$(nproc)}"
verify_only="${AGENT_MANAGER_MICROVM_KERNEL_VERIFY_ONLY:-false}"

usage() {
    cat >&2 <<'USAGE'
Usage: build-kernel.sh [output-dir]

Builds the pinned Firecracker guest kernel described by kernel.lock. The output
directory receives vmlinux, config, System.map, and kernel-provenance.json.

Environment:
  AGENT_MANAGER_MICROVM_KERNEL_LOCK       Lock file with KERNEL_VERSION/URL/SHA256.
  AGENT_MANAGER_MICROVM_KERNEL_CACHE      Download cache directory.
  AGENT_MANAGER_MICROVM_KERNEL_JOBS       Parallel make jobs.
  AGENT_MANAGER_MICROVM_KERNEL_VERIFY_ONLY Verify the locked source tarball and exit.
  AGENT_MANAGER_MICROVM_KERNEL_OUT_DIR    Output directory when no argument is supplied.
USAGE
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    usage
    exit 0
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

for cmd in curl make sha256sum tar xz; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "missing required command: $cmd" >&2; exit 2; }
done

mkdir -p "$cache_dir" "$out_dir"
tarball="$cache_dir/linux-$KERNEL_VERSION.tar.xz"
if [ ! -f "$tarball" ]; then
    curl -fL "$KERNEL_URL" -o "$tarball"
fi
printf '%s  %s\n' "$KERNEL_SHA256" "$tarball" | sha256sum -c -
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

export ARCH=x86_64
export KBUILD_BUILD_USER="${KBUILD_BUILD_USER:-agent-manager}"
export KBUILD_BUILD_HOST="${KBUILD_BUILD_HOST:-agent-manager-ci}"
export KBUILD_BUILD_TIMESTAMP="${KBUILD_BUILD_TIMESTAMP:-$(date -u -d "@${KERNEL_SOURCE_DATE_EPOCH:-0}" '+%Y-%m-%d %H:%M:%S')}"
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-${KERNEL_SOURCE_DATE_EPOCH:-0}}"
export KCFLAGS="${KCFLAGS:--Wno-error=date-time}"

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
    --enable KVM_GUEST \
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

make -C "$src_dir" O="$build_dir" olddefconfig >/dev/null
"$script_dir/check-kernel-config.sh" "$config_file"
make -C "$src_dir" O="$build_dir" -j"$jobs" vmlinux >/dev/null

install -m 0644 "$build_dir/vmlinux" "$out_dir/vmlinux"
install -m 0644 "$config_file" "$out_dir/config"
install -m 0644 "$build_dir/System.map" "$out_dir/System.map"

kernel_sha="$(sha256sum "$out_dir/vmlinux" | awk '{print $1}')"
config_sha="$(sha256sum "$out_dir/config" | awk '{print $1}')"
created_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
git_commit="$(git -C "$repo_root" rev-parse HEAD 2>/dev/null || true)"
cat > "$out_dir/kernel-provenance.json" <<EOF
{
  "mode": "pinned-source-build",
  "createdAt": "$created_at",
  "source": {
    "version": "$KERNEL_VERSION",
    "url": "$KERNEL_URL",
    "sha256": "$KERNEL_SHA256"
  },
  "reproducibility": {
    "sourceDateEpoch": "${SOURCE_DATE_EPOCH}",
    "kbuildBuildUser": "$KBUILD_BUILD_USER",
    "kbuildBuildHost": "$KBUILD_BUILD_HOST",
    "kbuildBuildTimestamp": "$KBUILD_BUILD_TIMESTAMP"
  },
  "outputs": {
    "kernel": {"path": "vmlinux", "sha256": "$kernel_sha"},
    "config": {"path": "config", "sha256": "$config_sha"}
  },
  "builder": {
    "script": "scripts/microvm-image/build-kernel.sh",
    "gitCommit": "$git_commit"
  }
}
EOF

echo "$out_dir/vmlinux"
