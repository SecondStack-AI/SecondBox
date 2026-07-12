#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

artifact_version="${AG_MICROVM_ARTIFACT_VERSION:-$(date -u +%Y%m%d%H%M%S)}"
out_dir="${AG_MICROVM_OUT_DIR:-$repo_root/releases/microvm/$artifact_version}"
rootfs_source_dir="${AG_MICROVM_ROOTFS_SOURCE_DIR:-}"
kernel_path="${AG_MICROVM_KERNEL_PATH:-}"
kernel_config="${AG_MICROVM_KERNEL_CONFIG:-}"
signing_key="${AG_MICROVM_SIGNING_KEY:-$repo_root/releases/microvm/signing.key}"
trusted_public_key="${AG_MICROVM_PUBLIC_KEY:-}"
trusted_public_key_sha="${AG_MICROVM_PUBLIC_KEY_SHA256:-}"
rootfs_size_mib="${AG_MICROVM_ROOTFS_SIZE_MIB:-8192}"
shared_format="${AG_MICROVM_SHARED_FORMAT:-auto}"
rootfs_uuid="${AG_MICROVM_ROOTFS_UUID:-11111111-2222-3333-4444-555555555555}"
build_kernel="${AG_MICROVM_BUILD_KERNEL:-false}"

usage() {
    cat >&2 <<'USAGE'
Usage: build.sh

Environment:
  AG_MICROVM_BUILD_KERNEL      Build the pinned kernel from kernel.lock when true.
  AG_MICROVM_KERNEL_PATH       Required path to the guest kernel image unless AG_MICROVM_BUILD_KERNEL=true.
  AG_MICROVM_KERNEL_CONFIG     Optional kernel .config to validate.
  AG_MICROVM_OUT_DIR           Output dir (default releases/microvm/<timestamp>).
  AG_MICROVM_ROOTFS_SOURCE_DIR Prepared guest rootfs directory to copy.
  AG_MICROVM_SIGNING_KEY       OpenSSL private key path for manifest signing.
  AG_MICROVM_PUBLIC_KEY        Optional trusted public key for verification.
  AG_MICROVM_PUBLIC_KEY_SHA256 Optional trusted public key DER SHA-256.
  AG_MICROVM_ROOTFS_SIZE_MIB   Rootfs image size, default 8192.
  AG_MICROVM_SHARED_FORMAT     auto|erofs|squashfs|ext4, default auto.
USAGE
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    usage
    exit 0
fi
case "$build_kernel" in
    1|true|TRUE|yes|YES)
        kernel_build_dir="$out_dir/kernel-build"
        kernel_path="$("$script_dir/build-kernel.sh" "$kernel_build_dir")"
        kernel_config="$kernel_build_dir/config"
        ;;
esac

if [ -z "$kernel_path" ] || [ ! -f "$kernel_path" ]; then
    echo "AG_MICROVM_KERNEL_PATH must point to a kernel image" >&2
    exit 2
fi

if [ -z "$rootfs_source_dir" ] || [ ! -d "$rootfs_source_dir" ]; then
    echo "AG_MICROVM_ROOTFS_SOURCE_DIR must point to a prepared guest rootfs directory" >&2
    exit 2
fi

for cmd in debugfs go sha256sum openssl mkfs.ext4 tar; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "missing required command: $cmd" >&2; exit 2; }
done

if [ -n "$kernel_config" ]; then
    "$script_dir/check-kernel-config.sh" "$kernel_config"
fi

work_dir="$(mktemp -d)"
cleanup() {
    rm -rf "$work_dir"
}
trap cleanup EXIT

root_dir="$work_dir/rootfs"
shared_dir="$work_dir/shared"
mkdir -p "$root_dir" "$shared_dir" "$out_dir"

echo "Copying prepared rootfs from $rootfs_source_dir" >&2
tar -C "$rootfs_source_dir" --one-file-system \
    --exclude='./dev/*' \
    --exclude='./proc/*' \
    --exclude='./sys/*' \
    --exclude='./tmp/*' \
    --exclude='./workspace/*' \
    --exclude='./runtime-private/*' \
    -cf - . | tar --no-same-owner -xf - -C "$root_dir"

install -d -m 0755 "$root_dir/dev" "$root_dir/proc" "$root_dir/sys" "$root_dir/tmp" "$root_dir/workspace" "$root_dir/runtime-private" "$root_dir/shared"
install -m 0755 "$script_dir/init" "$root_dir/init"

echo "Building guest supervisor" >&2
install -d -m 0755 "$root_dir/usr/local/bin"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$root_dir/usr/local/bin/agentcy-microvm-agent" "$repo_root/cmd/agentcy-microvm-agent"
# Tool-executor microVMs run the tool-exec server only (no in-VM agent runtime).
install -m 0755 "$repo_root/agent/tool-entrypoint.sh" "$root_dir/usr/local/bin/agentcy-microvm-entrypoint"

"$script_dir/rootfs/verify-standard-toolset.sh" --root-dir "$root_dir"

if [ -d "$root_dir/builtin-skills" ]; then
    mkdir -p "$shared_dir"
    cp -a "$root_dir/builtin-skills" "$shared_dir/builtin-skills"
fi
printf '%s\n' "$artifact_version" > "$shared_dir/agentcy-microvm-artifact-version"

"$script_dir/scan-no-secrets.sh" "$root_dir"

rootfs="$out_dir/rootfs.ext4"
truncate -s "${rootfs_size_mib}M" "$rootfs"
mkfs.ext4 -F -q -U "$rootfs_uuid" -d "$root_dir" "$rootfs"
"$script_dir/verify-browser-surface.sh" --rootfs "$rootfs"

shared="$out_dir/shared.img"
case "$shared_format" in
    auto)
        if command -v mkfs.erofs >/dev/null 2>&1; then
            shared_format="erofs"
        elif command -v mksquashfs >/dev/null 2>&1; then
            shared_format="squashfs"
        elif command -v mkfs.ext4 >/dev/null 2>&1; then
            shared_format="ext4"
        else
            echo "missing mkfs.erofs, mksquashfs, or mkfs.ext4 for shared image" >&2
            exit 2
        fi
        ;;
    erofs|squashfs|ext4) ;;
    *) echo "invalid AG_MICROVM_SHARED_FORMAT: $shared_format" >&2; exit 2 ;;
esac
if [ "$shared_format" = "erofs" ]; then
    mkfs.erofs "$shared" "$shared_dir" >/dev/null
elif [ "$shared_format" = "squashfs" ]; then
    mksquashfs "$shared_dir" "$shared" -noappend -all-root -quiet
else
    shared_size_mib="${AG_MICROVM_SHARED_SIZE_MIB:-128}"
    truncate -s "${shared_size_mib}M" "$shared"
    mkfs.ext4 -F -q -d "$shared_dir" "$shared"
fi
"$script_dir/verify-browser-surface.sh" --shared "$shared"

"$script_dir/rootfs/verify-standard-toolset.sh" --rootfs "$rootfs"

cp "$kernel_path" "$out_dir/kernel"
created_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
git_commit="$(git -C "$repo_root" rev-parse HEAD 2>/dev/null || true)"
if [ -f "$rootfs_source_dir/rootfs-source-manifest.json" ]; then
    cp "$rootfs_source_dir/rootfs-source-manifest.json" "$out_dir/rootfs-source-manifest.json"
else
    cat > "$out_dir/rootfs-source-manifest.json" <<EOF
{
  "schemaVersion": 1,
  "mode": "external",
  "createdAt": "$created_at",
  "source": {
    "path": "$rootfs_source_dir"
  },
  "builder": {
    "script": "scripts/microvm-image/build.sh",
    "gitCommit": "$git_commit"
  }
}
EOF
fi

kernel_sha="$(sha256sum "$out_dir/kernel" | awk '{print $1}')"
rootfs_sha="$(sha256sum "$rootfs" | awk '{print $1}')"
shared_sha="$(sha256sum "$shared" | awk '{print $1}')"
if [ -f "$out_dir/kernel-build/kernel-provenance.json" ]; then
    cp "$out_dir/kernel-build/kernel-provenance.json" "$out_dir/kernel-provenance.json"
else
    config_sha=""
    if [ -n "$kernel_config" ] && [ -f "$kernel_config" ]; then
        config_sha="$(sha256sum "$kernel_config" | awk '{print $1}')"
    fi
    cat > "$out_dir/kernel-provenance.json" <<EOF
{
  "mode": "supplied-kernel",
  "createdAt": "$created_at",
  "source": {
    "path": "$kernel_path",
    "configPath": "$kernel_config",
    "configSha256": "$config_sha"
  },
  "outputs": {
    "kernel": {"path": "kernel", "sha256": "$kernel_sha"}
  },
  "builder": {
    "script": "scripts/microvm-image/build.sh",
    "gitCommit": "$git_commit"
  }
}
EOF
fi
provenance_sha="$(sha256sum "$out_dir/kernel-provenance.json" | awk '{print $1}')"
rootfs_source_sha="$(sha256sum "$out_dir/rootfs-source-manifest.json" | awk '{print $1}')"
toolset_policy_sha="$(sha256sum "$script_dir/rootfs/verify-standard-toolset.sh" | awk '{print $1}')"
cat > "$out_dir/standard-toolset.json" <<EOF
{
  "schemaVersion": 1,
  "contract": "standard-tool-vm",
  "state": "verified",
  "rootfsSha256": "$rootfs_sha",
  "policySha256": "$toolset_policy_sha"
}
EOF
standard_toolset_sha="$(sha256sum "$out_dir/standard-toolset.json" | awk '{print $1}')"
(cd "$out_dir" && sha256sum kernel rootfs.ext4 shared.img kernel-provenance.json rootfs-source-manifest.json standard-toolset.json > SHA256SUMS)
cat > "$out_dir/manifest.json" <<EOF
{
  "artifactVersion": "$artifact_version",
  "createdAt": "$created_at",
  "kernel": {"path": "kernel", "sha256": "$kernel_sha"},
  "kernelProvenance": {"path": "kernel-provenance.json", "sha256": "$provenance_sha"},
  "rootfsSource": {"path": "rootfs-source-manifest.json", "sha256": "$rootfs_source_sha"},
  "standardToolset": {"path": "standard-toolset.json", "sha256": "$standard_toolset_sha", "state": "verified"},
  "rootfs": {"path": "rootfs.ext4", "sha256": "$rootfs_sha", "format": "ext4", "sizeMiB": $rootfs_size_mib},
  "shared": {"path": "shared.img", "sha256": "$shared_sha", "format": "$shared_format"},
  "entrypoint": "/init",
  "runtimeEntrypoint": "/usr/local/bin/agentcy-microvm-entrypoint"
}
EOF

mkdir -p "$(dirname "$signing_key")"
if [ ! -f "$signing_key" ]; then
    umask 077
    openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$signing_key" >/dev/null 2>&1
fi
openssl pkey -in "$signing_key" -pubout -out "$out_dir/signing.pub" >/dev/null 2>&1
openssl dgst -sha256 -sign "$signing_key" -out "$out_dir/manifest.sig" "$out_dir/manifest.json"
openssl dgst -sha256 -verify "$out_dir/signing.pub" -signature "$out_dir/manifest.sig" "$out_dir/manifest.json" >/dev/null

"$script_dir/verify.sh" "$out_dir" "$trusted_public_key" "$trusted_public_key_sha"
out_dir_abs="$(cd "$out_dir" && pwd)"
ln -sfn "$out_dir_abs" "$repo_root/releases/microvm/latest"
echo "$out_dir"
