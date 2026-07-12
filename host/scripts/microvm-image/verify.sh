#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat >&2 <<'USAGE'
Usage: verify.sh <artifact-dir> [public-key.pem]

Verifies the Firecracker artifact set emitted by build.sh: required files,
checksums, optional OpenSSL manifest signature, and manifest consistency.
USAGE
}

dir="${1:-}"
pubkey="${2:-${AG_MICROVM_PUBLIC_KEY:-}}"
if [ "${dir:-}" = "-h" ] || [ "${dir:-}" = "--help" ]; then
    usage
    exit 0
fi
if [ -z "$dir" ] || [ ! -d "$dir" ]; then
    usage
    exit 2
fi

for file in kernel rootfs.ext4 shared.img kernel-provenance.json rootfs-source-manifest.json manifest.json SHA256SUMS; do
    if [ ! -f "$dir/$file" ]; then
        echo "missing artifact: $file" >&2
        exit 1
    fi
done

(cd "$dir" && sha256sum -c SHA256SUMS)
"$script_dir/verify-browser-surface.sh" --rootfs "$dir/rootfs.ext4"
"$script_dir/verify-browser-surface.sh" --shared "$dir/shared.img"

for field in artifactVersion rootfs kernel kernelProvenance rootfsSource shared createdAt; do
    if ! grep -q "\"$field\"" "$dir/manifest.json"; then
        echo "manifest missing field: $field" >&2
        exit 1
    fi
done

provenance_sha="$(sha256sum "$dir/kernel-provenance.json" | awk '{print $1}')"
if ! grep -q "$provenance_sha" "$dir/manifest.json"; then
    echo "manifest kernelProvenance sha256 does not match kernel-provenance.json" >&2
    exit 1
fi
rootfs_source_sha="$(sha256sum "$dir/rootfs-source-manifest.json" | awk '{print $1}')"
if ! grep -q "$rootfs_source_sha" "$dir/manifest.json"; then
    echo "manifest rootfsSource sha256 does not match rootfs-source-manifest.json" >&2
    exit 1
fi
kernel_sha="$(sha256sum "$dir/kernel" | awk '{print $1}')"
if ! grep -q "$kernel_sha" "$dir/kernel-provenance.json"; then
    echo "kernel provenance does not describe kernel sha256" >&2
    exit 1
fi

if [ -n "$pubkey" ]; then
    if [ ! -f "$pubkey" ]; then
        echo "public key not found: $pubkey" >&2
        exit 1
    fi
    if [ ! -f "$dir/manifest.sig" ]; then
        echo "missing manifest signature" >&2
        exit 1
    fi
    openssl dgst -sha256 -verify "$pubkey" -signature "$dir/manifest.sig" "$dir/manifest.json" >/dev/null
else
    echo "manifest signature not verified: pass a trusted public key or set AG_MICROVM_PUBLIC_KEY" >&2
fi
