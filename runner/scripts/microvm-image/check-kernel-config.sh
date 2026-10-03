#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat >&2 <<'USAGE'
Usage: check-kernel-config.sh <kernel-config> <architecture>

Validates that the kernel config used for the Firecracker guest has the minimum
features required by the standalone runner rootfs: virtio block/net/vsock, ext4, FUSE,
namespaces, user namespaces, and seccomp, plus the Firecracker platform devices
of the amd64 or arm64 guest architecture.
USAGE
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    usage
    exit 0
fi

if [ "$#" -ne 2 ]; then
    usage
    exit 2
fi
config="$1"
architecture="$2"
if [ ! -f "$config" ]; then
    usage
    exit 2
fi
case "$architecture" in
    amd64|arm64) ;;
    *) echo "guest architecture must be amd64 or arm64: $architecture" >&2; exit 2 ;;
esac
script_dir="$(dirname "$0")"

missing=0
while IFS= read -r line; do
    line="${line%%#*}"
    line="${line%"${line##*[![:space:]]}"}"
    [ -z "$line" ] && continue
    key="${line%%=*}"
    want="${line#*=}"
    if ! grep -Eq "^${key}=(${want}|m)$" "$config"; then
        echo "missing kernel option: $line" >&2
        missing=1
    fi
done < <(cat "$script_dir/kernel-required.config" "$script_dir/kernel-required-$architecture.config")

exit "$missing"
