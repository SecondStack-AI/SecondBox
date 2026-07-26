#!/usr/bin/env bash
set -euo pipefail

# Produce a prepared guest rootfs directory for scripts/microvm-image/build.sh
# (its AGENT_MANAGER_MICROVM_ROOTFS_SOURCE_DIR input). The default mode creates a fresh
# Debian rootfs with debootstrap, then applies the standard package set
# (apt-std.txt + requirements-std.txt + config/) in Docker. Set
# AGENT_MANAGER_MICROVM_ROOTFS_SOURCE_MODE=extend to preserve the legacy behavior of
# extending an existing signed rootfs.ext4.
#
# Requires: docker, tar, and debootstrap for the default mode. Legacy extend mode
# also requires debugfs (e2fsprogs) and an existing base rootfs.ext4.
# Network egress is needed for the apt/pip installs inside the docker build.

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../../.." && pwd)"

lock_file="$script_dir/debian-rootfs.lock"
if [ -f "$lock_file" ]; then
    # shellcheck source=/dev/null
    . "$lock_file"
fi

mode="${AGENT_MANAGER_MICROVM_ROOTFS_SOURCE_MODE:-debootstrap}"
base_rootfs="${AGENT_MANAGER_MICROVM_BASE_ROOTFS:-$repo_root/releases/microvm/latest/rootfs.ext4}"
out_dir="${AGENT_MANAGER_MICROVM_ROOTFS_SOURCE_DIR:-$repo_root/tmp/microvm-rootfs-src}"
base_tag="${AGENT_MANAGER_MICROVM_BASE_IMAGE:-agent-manager-rootfs:base}"
std_tag="${AGENT_MANAGER_MICROVM_STD_IMAGE:-agent-manager-rootfs:std}"
stage_dir="${AGENT_MANAGER_MICROVM_STAGE_DIR:-$repo_root/tmp/agent-manager-rootfs-stage}"
debian_suite="${AGENT_MANAGER_MICROVM_DEBIAN_SUITE:-${AGENT_MANAGER_MICROVM_DEBIAN_SUITE_DEFAULT:-bookworm}}"
debian_arch="${AGENT_MANAGER_MICROVM_DEBIAN_ARCH:-${AGENT_MANAGER_MICROVM_DEBIAN_ARCH_DEFAULT:-amd64}}"
debian_mirror="${AGENT_MANAGER_MICROVM_DEBIAN_MIRROR:-${AGENT_MANAGER_MICROVM_DEBIAN_MIRROR_DEFAULT:-http://deb.debian.org/debian}}"
debootstrap_include="${AGENT_MANAGER_MICROVM_DEBOOTSTRAP_INCLUDE:-${AGENT_MANAGER_MICROVM_DEBOOTSTRAP_INCLUDE_DEFAULT:-ca-certificates,bash,locales,tzdata,python3}}"
apt_check_valid_until="${AGENT_MANAGER_MICROVM_APT_CHECK_VALID_UNTIL:-${AGENT_MANAGER_MICROVM_APT_CHECK_VALID_UNTIL_DEFAULT:-true}}"

json_escape() {
    printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

sha256_or_empty() {
    if [ -f "$1" ]; then
        sha256sum "$1" | awk '{print $1}'
    fi
}

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1" >&2; exit 2; }
}

run_root() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    else
        require_cmd sudo
        sudo "$@"
    fi
}

write_source_manifest() {
    local base_sha=""
    if [ "$mode" = "extend" ]; then
        base_sha="$(sha256_or_empty "$base_rootfs")"
    fi

    if [ -f "$out_dir/var/lib/dpkg/status" ]; then
        awk '
            /^Package: / { pkg=$2 }
            /^Version: / && pkg != "" { print pkg "=" $2; pkg="" }
        ' "$out_dir/var/lib/dpkg/status" | sort > "$out_dir/rootfs-packages.dpkg.lock"
    fi

    docker run --rm "$std_tag" sh -c 'python3 -m pip freeze --all 2>/dev/null || true' \
        > "$out_dir/rootfs-python.freeze"

    cat > "$out_dir/rootfs-source-manifest.json" <<EOF
{
  "schemaVersion": 1,
  "mode": "$(json_escape "$mode")",
  "createdAt": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "debian": {
    "suite": "$(json_escape "$debian_suite")",
    "architecture": "$(json_escape "$debian_arch")",
    "mirror": "$(json_escape "$debian_mirror")",
    "debootstrapInclude": "$(json_escape "$debootstrap_include")",
    "aptCheckValidUntil": "$(json_escape "$apt_check_valid_until")"
  },
  "baseRootfs": {
    "path": "$(json_escape "$base_rootfs")",
    "sha256": "$(json_escape "$base_sha")"
  },
  "inputs": {
    "debianRootfsLockSha256": "$(sha256_or_empty "$lock_file")",
    "aptStdSha256": "$(sha256_or_empty "$script_dir/apt-std.txt")",
    "requirementsStdSha256": "$(sha256_or_empty "$script_dir/requirements-std.txt")"
  },
  "outputs": {
    "dpkgLock": "rootfs-packages.dpkg.lock",
    "pythonFreeze": "rootfs-python.freeze"
  },
  "builder": {
    "script": "scripts/microvm-image/rootfs/build-rootfs-source.sh",
    "gitCommit": "$(git -C "$repo_root" rev-parse HEAD 2>/dev/null || true)"
  }
}
EOF
}

prepare_debootstrap_base() {
    require_cmd docker
    require_cmd tar
    require_cmd debootstrap

    echo "[1/4] Creating Debian $debian_suite rootfs → $stage_dir (debootstrap from $debian_mirror)" >&2
    run_root rm -rf "$stage_dir"
    run_root mkdir -p "$stage_dir"
    run_root debootstrap \
        --arch="$debian_arch" \
        --variant=minbase \
        --include="$debootstrap_include" \
        "$debian_suite" \
        "$stage_dir" \
        "$debian_mirror"
    if [ "$apt_check_valid_until" = "false" ]; then
        run_root install -d -m 0755 "$stage_dir/etc/apt/apt.conf.d"
        printf 'Acquire::Check-Valid-Until "false";\n' | run_root tee "$stage_dir/etc/apt/apt.conf.d/99agent-manager-snapshot" >/dev/null
    fi
}

prepare_extended_base() {
    require_cmd docker
    require_cmd debugfs
    require_cmd tar
    [ -f "$base_rootfs" ] || { echo "base rootfs not found: $base_rootfs" >&2; exit 2; }

    echo "[1/4] Extracting current rootfs → $stage_dir (debugfs rdump; unprivileged, no loop device)" >&2
    rm -rf "$stage_dir"; mkdir -p "$stage_dir"
    debugfs -R "rdump / $stage_dir" "$base_rootfs"
    [ -x "$stage_dir/usr/bin/apt-get" ] || { echo "extraction incomplete: /usr/bin/apt-get missing in staged rootfs" >&2; exit 3; }
}

case "$mode" in
    debootstrap) prepare_debootstrap_base ;;
    extend) prepare_extended_base ;;
    *) echo "invalid AGENT_MANAGER_MICROVM_ROOTFS_SOURCE_MODE: $mode (expected debootstrap or extend)" >&2; exit 2 ;;
esac

echo "[2/4] Importing base → $base_tag (forcing root ownership)" >&2
docker rmi -f "$base_tag" >/dev/null 2>&1 || true
if [ "$mode" = "debootstrap" ]; then
    run_root tar -C "$stage_dir" --owner=0 --group=0 --numeric-owner -cf - . | docker import - "$base_tag"
    run_root rm -rf "$stage_dir"
else
    tar -C "$stage_dir" --owner=0 --group=0 --numeric-owner -cf - . | docker import - "$base_tag"
    rm -rf "$stage_dir"
fi

echo "[3/4] Building standard image → $std_tag (apt + pip + config bakes)" >&2
docker build \
    --build-arg "BASE_IMAGE=$base_tag" \
    --build-arg "APT_CHECK_VALID_UNTIL=$apt_check_valid_until" \
    -t "$std_tag" "$script_dir"

echo "[4/4] Exporting $std_tag → $out_dir" >&2
rm -rf "$out_dir"; mkdir -p "$out_dir"
cid="$(docker create "$std_tag" /bin/true)"
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true' EXIT
docker export "$cid" | tar -C "$out_dir" --numeric-owner -xf -
docker rm -f "$cid" >/dev/null 2>&1 || true
trap - EXIT

write_source_manifest

echo "Prepared rootfs source: $out_dir ($(du -sh "$out_dir" 2>/dev/null | cut -f1))" >&2
echo "Source manifest: $out_dir/rootfs-source-manifest.json" >&2
echo "Next: AGENT_MANAGER_MICROVM_ROOTFS_SOURCE_DIR=$out_dir AGENT_MANAGER_MICROVM_KERNEL_PATH=<kernel> scripts/microvm-image/build.sh" >&2
