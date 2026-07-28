#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

usage() {
    cat >&2 <<'USAGE'
Usage: build.sh

Environment:
  SECONDBOX_RUNNER_MICROVM_BUILD_KERNEL      Build the pinned kernel from kernel.lock when true.
  SECONDBOX_RUNNER_MICROVM_KERNEL_PATH       Required path to the guest kernel image unless SECONDBOX_RUNNER_MICROVM_BUILD_KERNEL=true.
  SECONDBOX_RUNNER_MICROVM_KERNEL_CONFIG     Optional kernel .config to validate.
  SECONDBOX_RUNNER_MICROVM_ARTIFACT_VERSION  Required immutable artifact version.
  SECONDBOX_RUNNER_MICROVM_OUT_DIR           Required output directory.
  SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR Prepared guest rootfs directory to copy.
  SECONDBOX_RUNNER_MICROVM_SIGNING_KEY       Required OpenSSL private key path for manifest signing.
  SECONDBOX_RUNNER_MICROVM_PUBLIC_KEY        Required independently provisioned verification key.
  SECONDBOX_RUNNER_MICROVM_PUBLIC_KEY_SHA256 Required trusted public key DER SHA-256.
  SECONDBOX_RUNNER_MICROVM_ROOTFS_SIZE_MIB   Required rootfs image size.
  SECONDBOX_RUNNER_MICROVM_SHARED_SIZE_MIB   Required ext4 shared-image size.
  SECONDBOX_RUNNER_MICROVM_SHARED_FORMAT     Required: auto|erofs|squashfs|ext4.
  SECONDBOX_RUNNER_MICROVM_ROOTFS_UUID       Required deterministic rootfs UUID.
USAGE
}

if [ "${1-}" = "-h" ] || [ "${1-}" = "--help" ]; then
    usage
    exit 0
fi
for required_name in \
    SECONDBOX_RUNNER_MICROVM_ARTIFACT_VERSION \
    SECONDBOX_RUNNER_MICROVM_OUT_DIR \
    SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR \
    SECONDBOX_RUNNER_MICROVM_SIGNING_KEY \
    SECONDBOX_RUNNER_MICROVM_PUBLIC_KEY \
    SECONDBOX_RUNNER_MICROVM_PUBLIC_KEY_SHA256 \
    SECONDBOX_RUNNER_MICROVM_ROOTFS_SIZE_MIB \
    SECONDBOX_RUNNER_MICROVM_SHARED_SIZE_MIB \
    SECONDBOX_RUNNER_MICROVM_SHARED_FORMAT \
    SECONDBOX_RUNNER_MICROVM_ROOTFS_UUID \
    SECONDBOX_RUNNER_MICROVM_BUILD_KERNEL
do
    if [ -z "${!required_name-}" ]; then
        echo "$required_name is required" >&2
        exit 2
    fi
done
if [ -z "${SECONDBOX_RUNNER_MICROVM_KERNEL_PATH+x}" ] ||
   [ -z "${SECONDBOX_RUNNER_MICROVM_KERNEL_CONFIG+x}" ]; then
    echo "SECONDBOX_RUNNER_MICROVM_KERNEL_PATH and SECONDBOX_RUNNER_MICROVM_KERNEL_CONFIG must be explicitly set" >&2
    exit 2
fi
artifact_version="$SECONDBOX_RUNNER_MICROVM_ARTIFACT_VERSION"
out_dir="$SECONDBOX_RUNNER_MICROVM_OUT_DIR"
rootfs_source_dir="$SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR"
kernel_path="$SECONDBOX_RUNNER_MICROVM_KERNEL_PATH"
kernel_config="$SECONDBOX_RUNNER_MICROVM_KERNEL_CONFIG"
signing_key="$SECONDBOX_RUNNER_MICROVM_SIGNING_KEY"
trusted_public_key="$SECONDBOX_RUNNER_MICROVM_PUBLIC_KEY"
trusted_public_key_sha="$SECONDBOX_RUNNER_MICROVM_PUBLIC_KEY_SHA256"
rootfs_size_mib="$SECONDBOX_RUNNER_MICROVM_ROOTFS_SIZE_MIB"
shared_size_mib="$SECONDBOX_RUNNER_MICROVM_SHARED_SIZE_MIB"
shared_format="$SECONDBOX_RUNNER_MICROVM_SHARED_FORMAT"
rootfs_uuid="$SECONDBOX_RUNNER_MICROVM_ROOTFS_UUID"
build_kernel="$SECONDBOX_RUNNER_MICROVM_BUILD_KERNEL"
case "$build_kernel" in
    1|true|TRUE|yes|YES)
        kernel_build_dir="$out_dir/kernel-build"
        kernel_path="$("$script_dir/build-kernel.sh" "$kernel_build_dir")"
        kernel_config="$kernel_build_dir/config"
        ;;
esac

if [ -z "$kernel_path" ] || [ ! -f "$kernel_path" ]; then
    echo "SECONDBOX_RUNNER_MICROVM_KERNEL_PATH must point to a kernel image" >&2
    exit 2
fi

if [ -z "$rootfs_source_dir" ] || [ ! -d "$rootfs_source_dir" ]; then
    echo "SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR must point to a prepared guest rootfs directory" >&2
    exit 2
fi
if [ -z "$signing_key" ] || [ ! -f "$signing_key" ]; then
    echo "SECONDBOX_RUNNER_MICROVM_SIGNING_KEY must point to an existing private key" >&2
    exit 2
fi
if [ -z "$trusted_public_key" ] || [ ! -f "$trusted_public_key" ]; then
    echo "SECONDBOX_RUNNER_MICROVM_PUBLIC_KEY must point to an independently provisioned public key" >&2
    exit 2
fi
if ! [[ "$trusted_public_key_sha" =~ ^[0-9a-f]{64}$ ]]; then
    echo "SECONDBOX_RUNNER_MICROVM_PUBLIC_KEY_SHA256 must be 64 lowercase hex characters" >&2
    exit 2
fi

for cmd in debugfs file go sha256sum openssl mkfs.ext4 tar; do
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
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$root_dir/usr/local/bin/secondbox-guest-agent" "$repo_root/cmd/secondbox-guest-agent"
# Tool-executor microVMs run the tool-exec server only.
install -m 0755 "$script_dir/tool-entrypoint.sh" "$root_dir/usr/local/bin/secondbox-runner-guest-entrypoint"

"$script_dir/rootfs/verify-secondbox-rootfs.sh" --root-dir "$root_dir"

if [ -d "$root_dir/builtin-skills" ]; then
    mkdir -p "$shared_dir"
    cp -a "$root_dir/builtin-skills" "$shared_dir/builtin-skills"
fi
printf '%s\n' "$artifact_version" > "$shared_dir/secondbox-runner-guest-artifact-version"

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
    *) echo "invalid SECONDBOX_RUNNER_MICROVM_SHARED_FORMAT: $shared_format" >&2; exit 2 ;;
esac
if [ "$shared_format" = "erofs" ]; then
    mkfs.erofs "$shared" "$shared_dir" >/dev/null
elif [ "$shared_format" = "squashfs" ]; then
    mksquashfs "$shared_dir" "$shared" -noappend -all-root -quiet
else
    truncate -s "${shared_size_mib}M" "$shared"
    mkfs.ext4 -F -q -d "$shared_dir" "$shared"
fi
"$script_dir/verify-browser-surface.sh" --shared "$shared"

"$script_dir/rootfs/verify-secondbox-rootfs.sh" --rootfs "$rootfs"

cp "$kernel_path" "$out_dir/kernel"
created_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
git_commit="$(git -C "$repo_root" rev-parse HEAD)"
rootfs_provenance_dir="$rootfs_source_dir/usr/share/secondbox/image-provenance"
for provenance_file in \
    rootfs-source-manifest.json \
    rootfs-debian-packages.lock \
    rootfs-python.freeze \
    rootfs-debian-license-inventory.json \
    rootfs-python-license-inventory.json
do
    if [ ! -f "$rootfs_provenance_dir/$provenance_file" ]; then
        echo "prepared rootfs is missing required provenance: $provenance_file" >&2
        exit 2
    fi
    cp "$rootfs_provenance_dir/$provenance_file" "$out_dir/$provenance_file"
done

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
rootfs_policy_sha="$(sha256sum "$script_dir/rootfs/verify-secondbox-rootfs.sh" | awk '{print $1}')"
cat > "$out_dir/secondbox-rootfs-contract.json" <<EOF
{
  "schemaVersion": 1,
  "contract": "secondbox-guest-rootfs",
  "state": "verified",
  "rootfsSha256": "$rootfs_sha",
  "policySha256": "$rootfs_policy_sha"
}
EOF
rootfs_contract_sha="$(sha256sum "$out_dir/secondbox-rootfs-contract.json" | awk '{print $1}')"
debian_packages_sha="$(sha256sum "$out_dir/rootfs-debian-packages.lock" | awk '{print $1}')"
python_freeze_sha="$(sha256sum "$out_dir/rootfs-python.freeze" | awk '{print $1}')"
debian_licenses_sha="$(sha256sum "$out_dir/rootfs-debian-license-inventory.json" | awk '{print $1}')"
python_licenses_sha="$(sha256sum "$out_dir/rootfs-python-license-inventory.json" | awk '{print $1}')"
cat > "$out_dir/runtime-manifest.json" <<EOF
{
  "artifactId": "${artifact_version}-runtime",
  "architecture": "amd64",
  "guestProtocol": {"minimum": 1, "maximum": 1},
  "kernelSha256": "$kernel_sha",
  "rootfsSha256": "$rootfs_sha",
  "rootfsContractSha256": "$rootfs_contract_sha"
}
EOF
cat > "$out_dir/toolchain-manifest.json" <<EOF
{
  "artifactId": "${artifact_version}-toolchain",
  "architecture": "amd64",
  "guestProtocol": {"minimum": 1, "maximum": 1},
  "sharedImageSha256": "$shared_sha",
  "debianPackagesSha256": "$debian_packages_sha",
  "pythonFreezeSha256": "$python_freeze_sha"
}
EOF
runtime_manifest_sha="$(sha256sum "$out_dir/runtime-manifest.json" | awk '{print $1}')"
toolchain_manifest_sha="$(sha256sum "$out_dir/toolchain-manifest.json" | awk '{print $1}')"
(cd "$out_dir" && sha256sum \
    kernel rootfs.ext4 shared.img kernel-provenance.json rootfs-source-manifest.json \
    secondbox-rootfs-contract.json rootfs-debian-packages.lock rootfs-python.freeze \
    rootfs-debian-license-inventory.json rootfs-python-license-inventory.json \
    runtime-manifest.json toolchain-manifest.json > SHA256SUMS)
cat > "$out_dir/manifest.json" <<EOF
{
  "artifactVersion": "$artifact_version",
  "architecture": "amd64",
  "guestProtocol": {"minimum": 1, "maximum": 1},
  "runtimeBundle": {
    "artifactId": "${artifact_version}-runtime",
    "path": "runtime-manifest.json",
    "manifestDigest": "sha256:$runtime_manifest_sha",
    "mandatoryGuestFeatures": []
  },
  "toolchainBundle": {
    "artifactId": "${artifact_version}-toolchain",
    "path": "toolchain-manifest.json",
    "manifestDigest": "sha256:$toolchain_manifest_sha",
    "mandatoryGuestFeatures": []
  },
  "createdAt": "$created_at",
  "kernel": {"path": "kernel", "sha256": "$kernel_sha"},
  "kernelProvenance": {"path": "kernel-provenance.json", "sha256": "$provenance_sha"},
  "rootfsSource": {"path": "rootfs-source-manifest.json", "sha256": "$rootfs_source_sha"},
  "rootfsContract": {"path": "secondbox-rootfs-contract.json", "sha256": "$rootfs_contract_sha", "state": "verified"},
  "rootfsProvenance": {
    "debianPackages": {"path": "rootfs-debian-packages.lock", "sha256": "$debian_packages_sha"},
    "pythonFreeze": {"path": "rootfs-python.freeze", "sha256": "$python_freeze_sha"},
    "debianLicenses": {"path": "rootfs-debian-license-inventory.json", "sha256": "$debian_licenses_sha"},
    "pythonLicenses": {"path": "rootfs-python-license-inventory.json", "sha256": "$python_licenses_sha"}
  },
  "rootfs": {"path": "rootfs.ext4", "sha256": "$rootfs_sha", "format": "ext4", "sizeMiB": $rootfs_size_mib},
  "shared": {"path": "shared.img", "sha256": "$shared_sha", "format": "$shared_format"},
  "entrypoint": "/init",
  "runtimeEntrypoint": "/usr/local/bin/secondbox-runner-guest-entrypoint"
}
EOF

openssl pkey -in "$signing_key" -pubout -out "$out_dir/signing.pub" >/dev/null 2>&1
openssl dgst -sha256 -sign "$signing_key" -out "$out_dir/manifest.sig" "$out_dir/manifest.json"
chmod 0644 "$out_dir/signing.pub" "$out_dir/manifest.sig"
openssl dgst -sha256 -verify "$out_dir/signing.pub" -signature "$out_dir/manifest.sig" "$out_dir/manifest.json" >/dev/null

"$script_dir/verify.sh" "$out_dir" "$trusted_public_key" "$trusted_public_key_sha"
out_dir_abs="$(cd "$out_dir" && pwd)"
ln -sfn "$out_dir_abs" "$repo_root/releases/microvm/latest"
echo "$out_dir"
