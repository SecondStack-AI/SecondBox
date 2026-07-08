#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat >&2 <<'USAGE'
Usage: check-kernel-config.sh <kernel-config> [required-config]

Validates that the kernel config used for the Firecracker guest has the minimum
features required by the Agentcy rootfs: virtio block/net/vsock, ext4, FUSE,
namespaces, user namespaces, and seccomp.
USAGE
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    usage
    exit 0
fi

config="${1:-}"
required="${2:-$(dirname "$0")/kernel-required.config}"
if [ -z "$config" ] || [ ! -f "$config" ]; then
    usage
    exit 2
fi
if [ ! -f "$required" ]; then
    echo "required config not found: $required" >&2
    exit 2
fi

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
done < "$required"

exit "$missing"
