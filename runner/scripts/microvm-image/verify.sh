#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat >&2 <<'USAGE'
Usage: verify.sh <artifact-dir> <public-key.pem> <public-key-der-sha256>

Verifies the Firecracker artifact set emitted by build.sh: required files,
checksums, OpenSSL manifest signature, and manifest consistency. The trusted
public key and its canonical DER SHA-256 fingerprint are mandatory.
USAGE
}

dir="${1-}"
pubkey="${2-}"
expected_pubkey_sha="${3-}"
if [ "${dir:-}" = "-h" ] || [ "${dir:-}" = "--help" ]; then
    usage
    exit 0
fi
if [ -z "$dir" ] || [ ! -d "$dir" ]; then
    usage
    exit 2
fi

if [ -z "$pubkey" ]; then
    echo "trusted public key is required" >&2
    exit 1
fi
if [ -z "$expected_pubkey_sha" ]; then
    echo "trusted public key fingerprint is required" >&2
    exit 1
fi
if [ ! -f "$pubkey" ]; then
    echo "public key not found: $pubkey" >&2
    exit 1
fi
actual_pubkey_sha="$(openssl pkey -pubin -in "$pubkey" -outform DER 2>/dev/null | sha256sum | awk '{print $1}')" || {
    echo "invalid public key: $pubkey" >&2
    exit 1
}
if ! [[ "$expected_pubkey_sha" =~ ^[0-9a-f]{64}$ ]]; then
    echo "expected public key fingerprint must be 64 lowercase hex characters" >&2
    exit 1
fi
if [ "$actual_pubkey_sha" != "$expected_pubkey_sha" ]; then
    echo "public key fingerprint mismatch: expected $expected_pubkey_sha, got $actual_pubkey_sha" >&2
    exit 1
fi

for file in \
    kernel rootfs.ext4 shared.img kernel-provenance.json rootfs-source-manifest.json \
    secondbox-rootfs-contract.json rootfs-debian-packages.lock rootfs-python.freeze \
    rootfs-debian-license-inventory.json rootfs-python-license-inventory.json \
    runtime-manifest.json toolchain-manifest.json \
    manifest.json manifest.sig SHA256SUMS
do
    if [ ! -f "$dir/$file" ]; then
        echo "missing artifact: $file" >&2
        exit 1
    fi
done

(cd "$dir" && sha256sum -c SHA256SUMS)
"$script_dir/verify-browser-surface.sh" --rootfs "$dir/rootfs.ext4"
"$script_dir/verify-browser-surface.sh" --shared "$dir/shared.img"

for field in artifactVersion architecture guestProtocol runtimeBundle toolchainBundle rootfs kernel kernelProvenance rootfsSource rootfsContract shared createdAt; do
    if ! grep -q "\"$field\"" "$dir/manifest.json"; then
        echo "manifest missing field: $field" >&2
        exit 1
    fi
done
for component in runtime toolchain; do
    component_sha="$(sha256sum "$dir/${component}-manifest.json" | awk '{print $1}')"
    if ! grep -Eq "\"manifestDigest\"[[:space:]]*:[[:space:]]*\"sha256:$component_sha\"" "$dir/manifest.json"; then
        echo "manifest ${component}Bundle digest does not match ${component}-manifest.json" >&2
        exit 1
    fi
done
if ! grep -Eq '"architecture"[[:space:]]*:[[:space:]]*"amd64"' "$dir/manifest.json"; then
    echo "manifest architecture must be amd64" >&2
    exit 1
fi
if ! grep -Eq '"minimum"[[:space:]]*:[[:space:]]*1' "$dir/manifest.json" ||
   ! grep -Eq '"maximum"[[:space:]]*:[[:space:]]*1' "$dir/manifest.json"; then
    echo "manifest guest protocol range must be 1..1" >&2
    exit 1
fi

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
rootfs_contract_sha="$(sha256sum "$dir/secondbox-rootfs-contract.json" | awk '{print $1}')"
if ! grep -q "$rootfs_contract_sha" "$dir/manifest.json"; then
    echo "manifest rootfsContract sha256 does not match secondbox-rootfs-contract.json" >&2
    exit 1
fi
if ! grep -Eq '"state"[[:space:]]*:[[:space:]]*"verified"' "$dir/secondbox-rootfs-contract.json"; then
    echo "SecondBox rootfs contract is not verified" >&2
    exit 1
fi
for provenance_file in \
    rootfs-debian-packages.lock \
    rootfs-python.freeze \
    rootfs-debian-license-inventory.json \
    rootfs-python-license-inventory.json
do
    provenance_sha="$(sha256sum "$dir/$provenance_file" | awk '{print $1}')"
    if ! grep -q "$provenance_sha" "$dir/manifest.json"; then
        echo "manifest provenance sha256 does not match $provenance_file" >&2
        exit 1
    fi
done
kernel_sha="$(sha256sum "$dir/kernel" | awk '{print $1}')"
if ! grep -q "$kernel_sha" "$dir/kernel-provenance.json"; then
    echo "kernel provenance does not describe kernel sha256" >&2
    exit 1
fi

openssl dgst -sha256 -verify "$pubkey" -signature "$dir/manifest.sig" "$dir/manifest.json" >/dev/null
